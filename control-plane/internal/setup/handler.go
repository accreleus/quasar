package setup

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/auth"
	"github.com/accreleus/quasar/control-plane/internal/httpx"
	"github.com/accreleus/quasar/control-plane/internal/ratelimit"
)

// errorCodeSetupAlreadyComplete: its own code, not generic `conflict` — the
// SPA renders "already set up, sign in instead" and may not parse messages.
// protocol/openapi.yaml (POST /v1/setup/claim 409).
const errorCodeSetupAlreadyComplete = "setup_already_complete"

// Claimer is the identity seam, implemented by *auth.Service; an interface so
// the token check and response shape are testable without a database (the
// advisory-locked admin-exists gate is covered by the TEST_DATABASE_URL tests).
type Claimer interface {
	// AdminExists reports whether any admin account exists (status routing).
	AdminExists(ctx context.Context) (bool, error)
	// ClaimSetup creates the first admin and returns a login-shaped token.
	// deviceKey ("" when the client sent none) device-binds the token so the
	// founding admin's session is per-device revocable like any login.
	ClaimSetup(ctx context.Context, email, username, password, userAgent, deviceKey string) (auth.Token, error)
}

// StateReader exposes the persisted wizard state, implemented by *settings.Store.
type StateReader interface {
	// SetupCompleted reports whether the wizard finished or was skipped.
	SetupCompleted(ctx context.Context) (bool, error)
	// MarkSetupComplete stamps setup_completed_at if NULL and reports the
	// resulting completion state. Idempotent — a second call must not move the
	// original timestamp.
	MarkSetupComplete(ctx context.Context) (bool, error)
}

// Service serves GET /v1/setup/status and POST /v1/setup/claim.
type Service struct {
	claimer Claimer
	state   StateReader
	log     *slog.Logger

	// secret is the per-boot setup token. Empty means "not offered this boot"
	// (an admin already existed); tokenOK then fails closed and every claim 401s.
	secret string
	// secretDigest: both sides are SHA-256'd so the compare is fixed-width —
	// ConstantTimeCompare on raw strings returns early on a length mismatch.
	secretDigest [sha256.Size]byte

	// failures bounds repeated rejected claims per source IP (finding 6). See
	// handleClaim for why this cannot become a wrong-vs-missing oracle.
	failures *ratelimit.FailureLimiter

	// trustedProxies is the #438 trusted-proxy policy. nil means "key on the
	// direct peer" — correct for a direct-LAN deployment; the hardened overlay
	// opts in via WithTrustedProxies.
	trustedProxies []*net.IPNet
}

// WithTrustedProxies configures which direct peers' X-Forwarded-For may be
// believed (#438). Without it, every client behind the hardened Caddy overlay
// shares one claim budget and an attacker can lock the operator out of setup.
func (s *Service) WithTrustedProxies(nets []*net.IPNet) *Service {
	s.trustedProxies = nets
	return s
}

// NewService builds the setup service. secret is the per-boot setup token, or ""
// when no token was minted this boot (an admin already exists) — in which case
// claim fails closed with 401 and only status is useful.
func NewService(claimer Claimer, state StateReader, secret string, log *slog.Logger) *Service {
	return &Service{
		claimer:      claimer,
		state:        state,
		log:          log,
		secret:       secret,
		secretDigest: sha256.Sum256([]byte(secret)),
		failures:     ratelimit.NewFailureLimiter(claimFailureLimit, claimFailureTTL, claimFailureMaxIPs),
	}
}

// Register wires the setup routes unconditionally (spec "Not registered
// conditionally"): claim self-disables through its 409/401 gates once an admin
// exists, and running inside Services.RegisterRoutes keeps every route in
// TestOpenAPIDrift's recorded surface.
//
// The auth split is the point: status + claim are unauthenticated (they must
// work before any bearer token can exist; claim carries its own per-boot-token
// gate), while complete goes through the same RequireAuth → RequireAdmin chain
// as every admin route — completion is instance state only an admin may declare.
func (s *Service) Register(mux httpx.Router, admin func(http.Handler) http.Handler) {
	mux.HandleFunc(RouteStatus, s.handleStatus)
	mux.HandleFunc(RouteClaim, s.handleClaim)
	mux.Handle(RouteComplete, admin(http.HandlerFunc(s.handleComplete)))
}

type statusResponse struct {
	AdminExists    bool `json:"admin_exists"`
	SetupCompleted bool `json:"setup_completed"`
}

// handleStatus serves GET /v1/setup/status: unauthenticated, and deliberately
// only the two routing booleans — nothing an attacker can use. Never the token.
func (s *Service) handleStatus(w http.ResponseWriter, r *http.Request) {
	adminExists, err := s.claimer.AdminExists(r.Context())
	if err != nil {
		s.log.Error("setup status: admin-exists read failed", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not read setup status")
		return
	}
	completed, err := s.state.SetupCompleted(r.Context())
	if err != nil {
		s.log.Error("setup status: setup-completed read failed", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not read setup status")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, statusResponse{AdminExists: adminExists, SetupCompleted: completed})
}

type claimRequest struct {
	Email    string `json:"email"`
	Username string `json:"username"`
	Password string `json:"password"`
	// DeviceKey is optional (mirrors LoginRequest.device_key): when present the
	// minted token is device-bound so the founding admin's token is revocable;
	// absent means an unbound token. Contract: openapi.yaml SetupClaimRequest.
	DeviceKey string `json:"device_key"`
}

// userBrief mirrors the login response user shape (control-api.md LoginResponse:
// id/email/username/role, created_at omitted from the login user).
type userBrief struct {
	ID       string `json:"id"`
	Email    string `json:"email"`
	Username string `json:"username"`
	Role     string `json:"role"`
}

type claimResponse struct {
	AccessToken string    `json:"access_token"`
	TokenType   string    `json:"token_type"`
	ExpiresAt   time.Time `json:"expires_at"`
	User        userBrief `json:"user"`
}

// handleClaim serves POST /v1/setup/claim. Fail-closed, in order:
//  1. token check FIRST, before the body is even read; every rejection produces
//     the identical 401 envelope with no missing-vs-wrong distinction, and the
//     rejection is logged at WARN with the source address.
//  2. body decode / password strength → 400 validation_failed.
//  3. admin-exists gate (inside the advisory-locked claim tx) → 409.
func (s *Service) handleClaim(w http.ResponseWriter, r *http.Request) {
	// Rate gate (finding 6), before the token compare and any logging. Reserve
	// (not Allow) makes admission and in-flight accounting one atomic operation,
	// same pattern as the signaling and enrollment limiters. Not a
	// wrong-vs-missing oracle: the decision reads only this IP's failure counts,
	// never the header, so missing and wrong tokens stay byte-identical (#399);
	// a 429 only tells the caller about their own request history.
	clientIP := ratelimit.ClientIP(r, s.trustedProxies)
	if !s.failures.Reserve(clientIP, claimMaxInFlightIP, claimMaxInFlight) {
		httpx.WriteError(w, http.StatusTooManyRequests, httpx.CodeRateLimited,
			"too many failed setup claims; try again later")
		return
	}
	defer s.failures.Release(clientIP)

	// Token check before the body is read; every failure produces the identical
	// envelope (see secretDigest). The WARN is bounded by the limiter above.
	if !s.tokenOK(r.Header.Get(TokenHeader)) {
		s.failures.Failure(clientIP)
		s.log.Warn("setup claim: rejected token", "remote_addr", clientIP)
		httpx.WriteError(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "unauthorized")
		return
	}
	// A correct token clears the penalty — an operator who fumbled the paste a
	// few times is never left rate-limited.
	s.failures.Forget(clientIP)

	var req claimRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	tok, err := s.claimer.ClaimSetup(r.Context(), req.Email, req.Username, req.Password, r.UserAgent(), req.DeviceKey)
	switch {
	case errors.As(err, &auth.ErrValidation{}):
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, err.Error())
		return
	case errors.Is(err, auth.ErrSetupAlreadyComplete):
		// The self-disable: an admin already exists (env-bootstrap or a race).
		s.log.Warn("setup claim: refused, admin already exists", "remote_addr", clientIP)
		httpx.WriteError(w, http.StatusConflict, errorCodeSetupAlreadyComplete, "setup already complete")
		return
	case errors.Is(err, auth.ErrConflict):
		httpx.WriteError(w, http.StatusConflict, httpx.CodeConflict, "email or username already in use")
		return
	case err != nil:
		s.log.Error("setup claim: mint failed", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not create the first admin")
		return
	}

	s.log.Warn("setup claim: first admin created",
		"remote_addr", clientIP, "user_id", tok.User.ID, "username", tok.User.Username)

	httpx.WriteJSON(w, http.StatusCreated, claimResponse{
		AccessToken: tok.Plaintext,
		TokenType:   "Bearer",
		ExpiresAt:   tok.ExpiresAt,
		User: userBrief{
			ID:       tok.User.ID,
			Email:    tok.User.Email,
			Username: tok.User.Username,
			Role:     tok.User.Role,
		},
	})
}

// handleComplete serves POST /v1/setup/complete: stamps
// instance_settings.setup_completed_at and returns the SetupStatus body.
// Admin-gated by the caller's middleware (x-required-role: admin). Idempotent:
// the store's COALESCE keeps an already-set timestamp. Completion is instance
// state, permanent for every admin on every device; setup_state is not
// written — furthest-step resume stays client-side.
func (s *Service) handleComplete(w http.ResponseWriter, r *http.Request) {
	completed, err := s.state.MarkSetupComplete(r.Context())
	if err != nil {
		s.log.Error("setup complete: write failed", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not mark setup complete")
		return
	}
	// Re-read rather than inferred from "an admin called this": the response
	// must report real state.
	adminExists, err := s.claimer.AdminExists(r.Context())
	if err != nil {
		s.log.Error("setup complete: admin-exists read failed", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not mark setup complete")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, statusResponse{AdminExists: adminExists, SetupCompleted: completed})
}

// tokenOK compares the presented token against the per-boot secret in constant
// time. Fails closed when no token was minted this boot (secret == "").
func (s *Service) tokenOK(presented string) bool {
	if s.secret == "" {
		return false
	}
	got := sha256.Sum256([]byte(presented))
	return subtle.ConstantTimeCompare(got[:], s.secretDigest[:]) == 1
}
