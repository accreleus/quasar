package devauth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/auth"
	"github.com/accreleus/quasar/control-plane/internal/httpx"
)

// Minter is the identity seam, implemented by *auth.Service; an interface so
// the key check, validation and response shape are testable without a
// database. Reaping/cascade/token death are covered by the TEST_DATABASE_URL
// tests.
type Minter interface {
	// MintEphemeral creates a throwaway user with the given role and issues a
	// token whose expiry equals the identity's.
	MintEphemeral(ctx context.Context, role string, ttl time.Duration) (auth.Token, error)
	// ReapEphemeral deletes every expired throwaway identity it is allowed to
	// delete, reporting what it did.
	ReapEphemeral(ctx context.Context) (auth.ReapReport, error)
}

// Service serves POST /v1/dev/agent-session and owns the reaper.
type Service struct {
	minter Minter
	secret string
	log    *slog.Logger

	// secretDigest: both sides are SHA-256'd before comparing so the comparison
	// is fixed-width — subtle.ConstantTimeCompare on raw strings returns early
	// on a length mismatch.
	secretDigest [sha256.Size]byte
}

// NewService builds the dev-auth service. secret is the per-boot key.
func NewService(minter Minter, secret string, log *slog.Logger) *Service {
	return &Service{
		minter:       minter,
		secret:       secret,
		log:          log,
		secretDigest: sha256.Sum256([]byte(secret)),
	}
}

// Register wires the dev route only when cfg.Enabled; off means nothing is
// registered and the path 404s from the mux itself. Called from
// cmd/quasar-control/main.go, never Services.RegisterRoutes — that route table
// is the production surface TestOpenAPIDrift records, and this endpoint must
// not be in it under any configuration.
func Register(mux httpx.Router, cfg Config, svc *Service, log *slog.Logger) {
	if !cfg.Enabled {
		return
	}
	// Gate 4: a WARN banner every enabled boot — visible without knowing to look.
	log.Warn("DEV-ONLY AGENT AUTH IS ENABLED — POST /v1/dev/agent-session mints throwaway identities",
		"knob", EnvFlag, "disable", EnvFlag+"=0 (or unset) then restart")
	mux.HandleFunc(Route, svc.Handle)
}

// ReapOnce deletes expired throwaway identities in one pass; the devauth.reaper
// job (wired in cmd/quasar-control/app.go) records its counts. The job is
// wired unconditionally, even with QUASAR_DEV_AGENT_AUTH off: "flag on → mint
// an 8h identity → flag off → restart" must not strand that row (and its home)
// forever. A package function because Minter is the only state.
func ReapOnce(ctx context.Context, minter Minter, log *slog.Logger) (auth.ReapReport, error) {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	rep, err := minter.ReapEphemeral(cctx)
	if err != nil {
		// Per-row failures; the sweep already continued past them.
		log.Warn("dev agent reaper: some identities could not be deleted",
			"failed", rep.Failed, "err", err)
	}
	if rep.Deleted > 0 {
		log.Info("dev agent reaper", "deleted", rep.Deleted)
	}
	if rep.InSession > 0 {
		// Not a problem: an expired identity still holding a live session is
		// deliberately left alone until that session goes terminal.
		log.Info("dev agent reaper: expired identities still in session, retrying next sweep",
			"in_session", rep.InSession)
	}
	return rep, err
}

type request struct {
	Role string `json:"role"`
	// TTLSeconds is a pointer so "absent" is distinguishable from an explicit 0
	// (which is out of range and must be rejected, not defaulted).
	TTLSeconds *int `json:"ttl_seconds"`
}

type response struct {
	AccessToken string            `json:"access_token"`
	TokenType   string            `json:"token_type"`
	ExpiresAt   string            `json:"expires_at"`
	User        auth.User         `json:"user"`
	StorageKeys map[string]string `json:"storage_keys"`
}

// localStorage keys the web SPA rehydrates from (web/src/auth/storage.ts). Kept
// here verbatim so browser-automation tooling can inject a session without
// knowing anything about the SPA's internals.
const (
	storageKeyToken     = "quasar.auth.token"
	storageKeyExpiresAt = "quasar.auth.expires_at"
	storageKeyUser      = "quasar.auth.user"
)

// Handle serves POST /v1/dev/agent-session.
func (s *Service) Handle(w http.ResponseWriter, r *http.Request) {
	// Gate 5: key check first, before the body is read; every failure produces
	// the identical envelope (no missing-vs-wrong distinction, no length leak —
	// see secretDigest).
	if !s.keyOK(r.Header.Get(KeyHeader)) {
		// #438 exception: bare RemoteAddr, no trusted-proxy policy. Boot hard-fails
		// if QUASAR_DEV_AGENT_AUTH=1 reaches a hardened (#399, QUASAR_ENV=production)
		// stack — the overlay that introduces the proxy — so RemoteAddr here is
		// always the real peer, and it is diagnostic only.
		s.log.Warn("dev agent auth: rejected key", "remote_addr", r.RemoteAddr)
		httpx.WriteError(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "unauthorized")
		return
	}

	req, ok := decodeRequest(w, r)
	if !ok {
		return
	}

	role := auth.RoleUser
	if req.Role != "" {
		role = req.Role
	}
	if role != auth.RoleUser && role != auth.RoleAdmin {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed,
			`role must be "user" or "admin"`)
		return
	}

	ttl, err := resolveTTL(req.TTLSeconds)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, err.Error())
		return
	}

	if role == auth.RoleAdmin {
		// The one call that hands out real admin; never silent.
		s.log.Warn("dev agent auth minted an ADMIN identity",
			"remote_addr", r.RemoteAddr, "ttl", ttl.String())
	}

	tok, err := s.minter.MintEphemeral(r.Context(), role, ttl)
	if err != nil {
		var v auth.ErrValidation
		if errors.As(err, &v) {
			httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, v.Error())
			return
		}
		s.log.Error("dev agent auth mint failed", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not mint agent identity")
		return
	}

	expiresAt := tok.ExpiresAt.UTC().Format(time.RFC3339)
	userJSON, err := json.Marshal(tok.User)
	if err != nil {
		s.log.Error("dev agent auth: serialize user", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not mint agent identity")
		return
	}

	s.log.Info("dev agent auth minted an identity",
		"user_id", tok.User.ID, "role", tok.User.Role, "expires_at", expiresAt)

	httpx.WriteJSON(w, http.StatusOK, response{
		AccessToken: tok.Plaintext,
		TokenType:   "Bearer",
		ExpiresAt:   expiresAt,
		User:        tok.User,
		StorageKeys: map[string]string{
			storageKeyToken:     tok.Plaintext,
			storageKeyExpiresAt: expiresAt,
			storageKeyUser:      string(userJSON),
		},
	})
}

// keyOK compares the presented key against the per-boot secret in constant time.
func (s *Service) keyOK(presented string) bool {
	got := sha256.Sum256([]byte(presented))
	// An empty secret would otherwise make an empty header valid; the service is
	// never constructed that way, but fail closed rather than rely on it.
	if s.secret == "" {
		return false
	}
	return subtle.ConstantTimeCompare(got[:], s.secretDigest[:]) == 1
}

// maxBody bounds the request body — the shape is two small fields.
const maxBody = 4 << 10

// decodeRequest reads the optional JSON body. An absent/empty body is valid and
// yields defaults; malformed JSON or unknown fields are 400 validation_failed,
// matching the envelope conventions of internal/auth's handlers.
func decodeRequest(w http.ResponseWriter, r *http.Request) (request, bool) {
	var req request
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "could not read request body")
		return request{}, false
	}
	if len(body) == 0 {
		return req, true // body is optional: role=user, ttl=default
	}
	if err := json.Unmarshal(body, &req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "invalid JSON body")
		return request{}, false
	}
	return req, true
}

// resolveTTL applies the TTL policy: default 1800s, min 60s, hard cap 28800s.
// Out-of-range is rejected, not clamped: silently handing an 8h token to a
// caller who asked for 24h turns into a mid-run 401 in an unattended harness.
func resolveTTL(ttlSeconds *int) (time.Duration, error) {
	if ttlSeconds == nil {
		return DefaultTTL, nil
	}
	d := time.Duration(*ttlSeconds) * time.Second
	if d < MinTTL || d > MaxTTL {
		return 0, errTTLRange
	}
	return d, nil
}

var errTTLRange = errors.New("ttl_seconds must be between 60 and 28800")
