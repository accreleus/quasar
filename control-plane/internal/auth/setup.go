package auth

import (
	"context"
	"fmt"
	"strings"
)

// AdminExists reports whether any admin account exists. It backs the
// admin_exists boolean of GET /v1/setup/status (first-run wizard, Spec B W1) —
// an unauthenticated routing read, deliberately exposing nothing else.
func (s *Service) AdminExists(ctx context.Context) (bool, error) {
	return s.store.anyAdminExists(ctx)
}

// ClaimSetup creates the FIRST admin from an unauthenticated, setup-token-gated
// request (POST /v1/setup/claim) and returns a login-shaped Token so the caller
// is signed in as their new admin. The token check itself lives in the setup
// handler; this method assumes it already passed.
//
// It reuses the exact registration validation, the argon2id hashing and the
// token primitives the login/register paths use — never a duplicate. The
// password is validated and hashed BEFORE the transaction opens, so a weak
// password is a cheap 400 and the expensive KDF never holds the advisory lock.
// The admin-exists gate and the insert run inside claimFirstAdmin under the same
// bootstrapAdvisoryLock as env-bootstrap, so the two paths can never both create
// an admin: a second caller (or a racing env bootstrap) gets ErrSetupAlreadyComplete.
//
// deviceKey, when non-empty, binds the minted token to the caller's device
// (upserted inside the claim transaction) so the founding admin's token is
// revocable from Account → Devices like any other login token (LP-SEC-01
// §B.5). Empty deviceKey mints an unbound token, exactly as Login without a
// device_key does.
//
// Errors: ErrValidation (bad email/username/weak password → 400),
// ErrSetupAlreadyComplete (an admin already exists → 409), ErrConflict (the
// email/username collides with a non-admin account created in a race → 409).
func (s *Service) ClaimSetup(ctx context.Context, email, username, password, userAgent, deviceKey string) (Token, error) {
	email = strings.TrimSpace(strings.ToLower(email))
	username = strings.TrimSpace(username)
	if err := validateRegistration(email, username, password); err != nil {
		return Token{}, err
	}

	hash, err := HashPassword(password, s.params)
	if err != nil {
		return Token{}, fmt.Errorf("hash claim password: %w", err)
	}
	return s.store.claimFirstAdmin(ctx, email, username, hash, s.tokenTTL, userAgent, deviceKey)
}
