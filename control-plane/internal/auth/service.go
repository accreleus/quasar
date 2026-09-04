package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrInvalidCredentials is returned by Login for a bad email/password or a
// disabled account — the three are indistinguishable to the caller by design
// (no user enumeration).
var ErrInvalidCredentials = errors.New("invalid credentials")

// ErrRegistrationClosed is returned by RegisterWithInvite when registration_mode is
// 'closed' — the invitation system is off (LP-SEC-01 §B.1). Handler → 403.
var ErrRegistrationClosed = errors.New("registration is closed")

// ErrInvalidInvite is the single non-enumerating invite failure (decision D2): it covers
// missing / unknown / expired / exhausted / revoked, indistinguishably. Handler → 400.
var ErrInvalidInvite = errors.New("invalid invite")

// Registration modes (must match settings.RegistrationMode* / schema.md). Kept as local
// literals so auth does not import the settings package (that would cycle: the settings
// handler imports auth for its admin identity).
const (
	regClosed     = "closed"
	regInviteOnly = "invite_only"
	regOpen       = "open"
)

// Registration is the injected registration policy (LP-SEC-01 SEC-03): the persisted
// registration_mode plus atomic invite redemption. Implemented by an adapter in the main
// package that wraps the settings + invites stores, so auth imports neither (avoids an
// import cycle). RedeemInvite runs inside the register transaction so the invite consume
// and the account insert are atomic (decision D5).
type Registration interface {
	// Mode returns the current registration_mode (closed|invite_only|open).
	Mode(ctx context.Context) (string, error)
	// RedeemInvite atomically consumes one use of code within tx, returning the role the
	// account must be created with. ok=false means the invite was invalid/exhausted/
	// expired/revoked (a normal outcome, not an error); err is reserved for real failures.
	RedeemInvite(ctx context.Context, tx pgx.Tx, code string) (role string, ok bool, err error)
}

// Option customizes a Service at construction.
type Option func(*Service)

// WithRegistration injects the registration gate. When unset, RegisterWithInvite behaves
// as 'open' (legacy) — production always wires the gate; the nil default keeps existing
// tests, which construct a bare Service, working.
func WithRegistration(r Registration) Option {
	return func(s *Service) { s.registration = r }
}

// ErrValidation is returned for malformed input (bad email, short password…).
type ErrValidation struct{ Msg string }

func (e ErrValidation) Error() string { return e.Msg }

// HomeReaper is nudged with the hosts holding a just-deleted user's homes, so
// each reaps the backing store now rather than at its next scheduled sweep
// (#92). Implemented in the main package over the jobs dispatcher; nil means no
// nudge, which costs latency only — the homes are tombstoned and orphaned
// either way, and the host's own home.gc interval still collects them.
type HomeReaper interface {
	// ReapHomesOn asks each host to run its home-GC pass soon. Best-effort:
	// implementations log and swallow their own failures.
	ReapHomesOn(ctx context.Context, hostIDs []string)
}

// Service holds the auth business logic. Construct with NewService.
type Service struct {
	store        *store
	params       Params
	tokenTTL     time.Duration
	dummyHash    string       // a real argon2id hash, verified against on unknown users to equalize timing
	registration Registration // injected registration gate (LP-SEC-01); nil ⇒ legacy 'open'

	// Guards homeReaper alone: SetHomeReaper runs during wiring, DeleteUser /
	// ReapEphemeral read it from request and job goroutines.
	reaperMu   sync.RWMutex
	homeReaper HomeReaper
}

// SetHomeReaper wires the post-delete home reap nudge. A setter, not an Option,
// only because the jobs dispatcher is built after this Service in app.go.
func (s *Service) SetHomeReaper(r HomeReaper) {
	s.reaperMu.Lock()
	s.homeReaper = r
	s.reaperMu.Unlock()
}

func (s *Service) reapHomesOn(ctx context.Context, hostIDs []string) {
	if len(hostIDs) == 0 {
		return
	}
	s.reaperMu.RLock()
	r := s.homeReaper
	s.reaperMu.RUnlock()
	if r != nil {
		r.ReapHomesOn(ctx, hostIDs)
	}
}

// NewService wires the auth service to the pool. tokenTTL is how long an issued
// bearer token stays valid; params are the argon2id cost parameters.
func NewService(pool *pgxpool.Pool, params Params, tokenTTL time.Duration, opts ...Option) (*Service, error) {
	// Precompute a hash to verify against when an email is unknown, so login
	// timing does not reveal whether an account exists.
	dummy, err := HashPassword("quasar-timing-equalizer", params)
	if err != nil {
		return nil, fmt.Errorf("init dummy hash: %w", err)
	}
	s := &Service{
		store:     &store{pool: pool},
		params:    params,
		tokenTTL:  tokenTTL,
		dummyHash: dummy,
	}
	for _, o := range opts {
		o(s)
	}
	return s, nil
}

// SweepTokens deletes expired/revoked auth_tokens rows in one pass (#148 —
// without it the table grows without bound; the 7-day grace keeps recent rows
// for debugging). Triggered by the auth.token_janitor job (internal/jobs); the
// deleted count is the run summary.
func (s *Service) SweepTokens(ctx context.Context, log *slog.Logger) (int64, error) {
	const sweep = `DELETE FROM auth_tokens
		WHERE expires_at < now() - interval '7 days' OR revoked_at IS NOT NULL`
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	tag, err := s.store.pool.Exec(cctx, sweep)
	if err != nil {
		log.Warn("token janitor sweep failed", "err", err)
		return 0, err
	}
	if n := tag.RowsAffected(); n > 0 {
		log.Info("token janitor", "deleted", n)
	}
	return tag.RowsAffected(), nil
}

// Register creates an account. The password is hashed with argon2id; the
// plaintext never leaves this call. Returns ErrValidation, ErrConflict, or the
// created user.
func (s *Service) Register(ctx context.Context, email, username, password string) (User, error) {
	email = strings.TrimSpace(strings.ToLower(email))
	username = strings.TrimSpace(username)
	if err := validateRegistration(email, username, password); err != nil {
		return User{}, err
	}

	hash, err := HashPassword(password, s.params)
	if err != nil {
		return User{}, fmt.Errorf("hash password: %w", err)
	}
	return s.store.createUser(ctx, email, username, hash)
}

// RegisterWithInvite is the gated registration path used by the HTTP handler
// (LP-SEC-01 SEC-03). It consults the persisted registration_mode:
//   - closed      → ErrRegistrationClosed (the invitation system is off).
//   - open        → creates a role=user account, inviteCode ignored.
//   - invite_only → requires a valid inviteCode; redeems it and creates the account
//     with the INVITE'S role, atomically (redeem + insert in one tx; a duplicate-
//     email/username 409 rolls the tx back and un-consumes the invite — decision D5).
//
// The password is validated and hashed BEFORE any transaction is opened, so a weak
// password never holds a tx and the expensive argon2 work is outside the DB critical
// section. When no gate is wired (tests), behaviour is 'open'.
func (s *Service) RegisterWithInvite(ctx context.Context, email, username, password, inviteCode string) (User, error) {
	email = strings.TrimSpace(strings.ToLower(email))
	username = strings.TrimSpace(username)
	if err := validateRegistration(email, username, password); err != nil {
		return User{}, err
	}

	mode := regOpen
	if s.registration != nil {
		m, err := s.registration.Mode(ctx)
		if err != nil {
			return User{}, fmt.Errorf("read registration mode: %w", err)
		}
		mode = m
	}

	switch mode {
	case regClosed:
		return User{}, ErrRegistrationClosed
	case regOpen:
		hash, err := HashPassword(password, s.params)
		if err != nil {
			return User{}, fmt.Errorf("hash password: %w", err)
		}
		return s.store.createUser(ctx, email, username, hash)
	case regInviteOnly:
		// no gate but mode came back invite_only is impossible (mode is 'open' when
		// gate is nil); guard defensively so a misconfig can never bypass the invite.
		if s.registration == nil {
			return User{}, ErrRegistrationClosed
		}
		hash, err := HashPassword(password, s.params)
		if err != nil {
			return User{}, fmt.Errorf("hash password: %w", err)
		}
		return s.registerViaInvite(ctx, email, username, hash, inviteCode)
	default:
		return User{}, fmt.Errorf("unknown registration mode %q", mode)
	}
}

// registerViaInvite runs the atomic invite-redeem + account-create transaction. The role
// comes from the invite (never the request). A returned-zero redeem is ErrInvalidInvite;
// a duplicate account is ErrConflict — both roll the tx back, leaving the invite unspent.
func (s *Service) registerViaInvite(ctx context.Context, email, username, hash, inviteCode string) (User, error) {
	tx, err := s.store.begin(ctx)
	if err != nil {
		return User{}, fmt.Errorf("begin register tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck — no-op after commit

	role, ok, err := s.registration.RedeemInvite(ctx, tx, inviteCode)
	if err != nil {
		return User{}, fmt.Errorf("redeem invite: %w", err)
	}
	if !ok {
		return User{}, ErrInvalidInvite
	}

	user, err := s.store.createUserTx(ctx, tx, email, username, hash, role)
	if err != nil {
		return User{}, err // ErrConflict / other; deferred Rollback un-consumes the invite
	}

	if err := tx.Commit(ctx); err != nil {
		return User{}, fmt.Errorf("commit register tx: %w", err)
	}
	return user, nil
}

// Token is the result of a successful Login: the plaintext bearer token (shown
// once) plus its expiry and the user.
type Token struct {
	Plaintext string
	ExpiresAt time.Time
	User      User
}

// Login verifies credentials and issues a bearer token. Any failure path
// (unknown email, wrong password, disabled account) returns ErrInvalidCredentials
// and runs an argon2 verification regardless, so the response time does not leak
// whether the account exists.
func (s *Service) Login(ctx context.Context, email, password, userAgent string) (Token, error) {
	return s.LoginWithDevice(ctx, email, password, userAgent, "")
}

// LoginWithDevice verifies credentials and issues a bearer token bound to the caller's
// device (LP-SEC-01 §B.5). When deviceKey is non-empty the (user_id, device_key)
// user_devices row is upserted and the minted token's device_id is stamped with it — the
// binding that makes per-device revocation real. When empty, behaviour is exactly as
// before (token minted with device_id = NULL). Every failure path runs an argon2 verify
// so response time never reveals whether the account exists.
func (s *Service) LoginWithDevice(ctx context.Context, email, password, userAgent, deviceKey string) (Token, error) {
	email = strings.TrimSpace(strings.ToLower(email))

	creds, err := s.store.getCredentialsByEmail(ctx, email)
	if errors.Is(err, ErrUserNotFound) {
		// Equalize timing: verify against the dummy hash, then fail.
		_, _ = VerifyPassword(password, s.dummyHash)
		return Token{}, ErrInvalidCredentials
	}
	if err != nil {
		return Token{}, err
	}

	ok, err := VerifyPassword(password, creds.passwordHash)
	if err != nil {
		return Token{}, fmt.Errorf("verify password: %w", err)
	}
	if !ok || creds.disabled {
		return Token{}, ErrInvalidCredentials
	}

	// Bind the token to a device when the client declared one. The upsert is
	// owner-scoped (userID from verified credentials, never the request body).
	deviceID := ""
	if deviceKey != "" {
		deviceID, err = s.store.upsertLoginDevice(ctx, creds.userID, deviceKey, userAgent)
		if err != nil {
			return Token{}, err
		}
	}

	plaintext, hash, err := generateToken()
	if err != nil {
		return Token{}, err
	}
	expiresAt := time.Now().Add(s.tokenTTL)
	if err := s.store.createToken(ctx, creds.userID, hash, expiresAt, userAgent, deviceID); err != nil {
		return Token{}, err
	}

	// Re-read the user for the response body (id/email/username/role).
	user, _, err := s.store.authenticate(ctx, hash)
	if err != nil {
		return Token{}, fmt.Errorf("load issued token user: %w", err)
	}
	return Token{Plaintext: plaintext, ExpiresAt: expiresAt, User: user}, nil
}

// Authenticate validates a bearer token and returns the owning user plus the device id
// the token is bound to ("" when unbound — a pre-0020 or no-device_key login). Returns
// ErrUserNotFound (→ 401) for any invalid/expired/revoked token.
func (s *Service) Authenticate(ctx context.Context, plaintextToken string) (User, string, error) {
	if plaintextToken == "" {
		return User{}, "", ErrUserNotFound
	}
	return s.store.authenticate(ctx, hashToken(plaintextToken))
}

// Logout revokes a bearer token. Idempotent and never errors on an unknown or
// already-revoked token.
func (s *Service) Logout(ctx context.Context, plaintextToken string) error {
	if plaintextToken == "" {
		return nil
	}
	return s.store.revokeToken(ctx, hashToken(plaintextToken))
}

// ChangePassword rotates a user's password after verifying the current one
// (CP-01), then revokes all tokens (log out everywhere). Check order is
// cheapest-first on purpose: format (no DB) → fetch → identity (#513) → the
// expensive argon2 verify. Returns ErrInvalidCredentials on a wrong current
// password, ErrValidation on a weak new one.
func (s *Service) ChangePassword(ctx context.Context, userID, currentPassword, newPassword string) error {
	if err := validatePasswordFormat(newPassword); err != nil {
		return err
	}

	creds, err := s.store.getCredentialsByID(ctx, userID)
	if err != nil {
		// A valid bearer token implies the user exists; treat any lookup miss as
		// an auth failure rather than leaking a distinct not-found path.
		if errors.Is(err, ErrUserNotFound) {
			return ErrInvalidCredentials
		}
		return err
	}

	if err := checkPasswordIdentity(newPassword, creds.username, emailLocalPart(creds.email)); err != nil {
		return err
	}

	ok, err := VerifyPassword(currentPassword, creds.passwordHash)
	if err != nil {
		return fmt.Errorf("verify password: %w", err)
	}
	if !ok {
		return ErrInvalidCredentials
	}

	hash, err := HashPassword(newPassword, s.params)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}
	if err := s.store.updatePasswordHash(ctx, userID, hash); err != nil {
		return err
	}
	// Revoke all active tokens so every device must re-authenticate with the new
	// password. This is the correct security posture for a password change.
	return s.store.revokeAllUserTokens(ctx, userID)
}

// ListUsers returns all users paginated. Admin use only (enforced at the handler level).
func (s *Service) ListUsers(ctx context.Context, cursor string, limit int32) ([]AdminUser, string, error) {
	return s.store.listUsers(ctx, cursor, limit)
}

// UpdateUser applies partial updates to a user. Admin use only.
func (s *Service) UpdateUser(ctx context.Context, id string, role *string, disabled *bool, maxSessions *int32) (AdminUser, error) {
	return s.store.updateUser(ctx, id, role, disabled, maxSessions)
}

// DeleteUser hard-deletes an account (admin only). Terminal session history
// cascades; see store.deleteUser for the guard set. The user's homes are
// tombstoned and orphaned by that call; the nudge here only shortens how long
// the bytes survive their owner (#92).
func (s *Service) DeleteUser(ctx context.Context, id string) error {
	hosts, err := s.store.deleteUser(ctx, id)
	if err != nil {
		return err
	}
	s.reapHomesOn(ctx, hosts)
	return nil
}

// --- validation --------------------------------------------------------------

const (
	// NIST SP 800-63B (#513): length over composition rules — no forced
	// symbols/digits, which push predictable substitutions. Gates NEW
	// passwords only; a pre-existing shorter password keeps logging in.
	minPasswordLen = 12
	// Bounds argon2 input cost (hashing-cost DoS) while covering any
	// realistic passphrase.
	maxPasswordLen = 128
	maxUsernameLen = 64
	maxEmailLen    = 320
)

func validateRegistration(email, username, password string) error {
	if email == "" || len(email) > maxEmailLen || !strings.Contains(email, "@") {
		return ErrValidation{Msg: "valid email is required"}
	}
	if username == "" || len(username) > maxUsernameLen {
		return ErrValidation{Msg: "username must be 1–64 characters"}
	}
	return validatePassword(password, username, emailLocalPart(email))
}

// validatePasswordFormat applies the length + common-password rules — the
// checks that need no knowledge of who the account belongs to, so callers
// that must reject a bad password before touching the database (CP-01: don't
// pay for a DB round trip on an obviously-too-short new password) can run
// this first. Length is counted in runes, not bytes: a password built from
// multi-byte UTF-8 characters must not be penalized (or advantaged) by its
// byte length.
func validatePasswordFormat(password string) error {
	n := utf8.RuneCountInString(password)
	if n < minPasswordLen {
		return ErrValidation{Msg: fmt.Sprintf("password must be at least %d characters", minPasswordLen)}
	}
	if n > maxPasswordLen {
		return ErrValidation{Msg: fmt.Sprintf("password must be at most %d characters", maxPasswordLen)}
	}
	if _, common := commonPasswords[strings.ToLower(password)]; common {
		return ErrValidation{Msg: "password is too common; choose something less predictable"}
	}
	return nil
}

// minIdentifierLen is the shortest identifier the containment check acts on.
// A 1-2 character username/local-part (test fixtures like "o", "cp", plus
// real accounts on a small self-hosted instance) is contained in nearly any
// password by chance — checking it rejects almost everything and catches
// nothing meaningful, so those identifiers are skipped rather than compared.
const minIdentifierLen = 3

// checkPasswordIdentity rejects a password that equals or contains one of the
// account's own identifiers (username, email local-part), case-insensitively
// (#513) — "myusername2024" is not a meaningfully stronger secret than
// "myusername". Identifiers shorter than minIdentifierLen, or empty (a caller
// that hasn't loaded one yet), are skipped.
func checkPasswordIdentity(password string, identifiers ...string) error {
	lower := strings.ToLower(password)
	for _, id := range identifiers {
		id = strings.ToLower(strings.TrimSpace(id))
		if len(id) < minIdentifierLen {
			continue
		}
		if strings.Contains(lower, id) {
			return ErrValidation{Msg: "password must not contain your username or email"}
		}
	}
	return nil
}

// emailLocalPart returns the part of an email before '@', lowercased by the
// caller as needed. Used only for the identity-containment check — the
// domain half is not a secret an attacker gains by guessing.
func emailLocalPart(email string) string {
	if i := strings.IndexByte(email, '@'); i >= 0 {
		return email[:i]
	}
	return email
}

// validatePassword applies the full password policy (format + identity) in
// one call. Used by paths that already have every identifier on hand before
// validating (registration, setup-claim, bootstrap admin).
func validatePassword(password string, identifiers ...string) error {
	if err := validatePasswordFormat(password); err != nil {
		return err
	}
	return checkPasswordIdentity(password, identifiers...)
}
