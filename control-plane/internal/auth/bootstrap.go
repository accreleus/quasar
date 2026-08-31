package auth

import (
	"context"
	"fmt"
	"strings"
)

// BootstrapResult reports what EnsureBootstrapAdmin did, for startup logging.
type BootstrapResult int

const (
	// BootstrapSkipped: bootstrap was not configured, or an admin already
	// exists, so nothing changed.
	BootstrapSkipped BootstrapResult = iota
	// BootstrapCreated: a new admin account was inserted.
	BootstrapCreated
	// BootstrapPromoted: an existing account with the configured email was
	// promoted to admin.
	BootstrapPromoted
)

func (r BootstrapResult) String() string {
	switch r {
	case BootstrapCreated:
		return "created"
	case BootstrapPromoted:
		return "promoted"
	default:
		return "skipped"
	}
}

// EnsureBootstrapAdmin provisions the first admin from operator-supplied
// configuration (control-api.md §Authorization, first-admin bootstrap). It is
// the deliberate alternative to "first registered user is admin": on a publicly
// reachable fresh instance the latter lets whoever races to /auth/register seize
// admin, whereas this binds the first admin to whoever controls the deployment
// (the env). Register semantics are therefore unchanged — every /auth/register
// account is role=user.
//
// Behaviour (idempotent, safe to call on every boot):
//   - all of email/username/password empty ⇒ not configured ⇒ BootstrapSkipped;
//   - an admin already exists ⇒ BootstrapSkipped (never a second admin);
//   - the configured email already has an account ⇒ promote it (BootstrapPromoted);
//   - otherwise create the admin account (BootstrapCreated).
//
// A partially-specified configuration is an operator error and returns
// ErrValidation. Returns ErrConflict if the configured username collides with a
// different existing account.
func (s *Service) EnsureBootstrapAdmin(ctx context.Context, email, username, password string) (BootstrapResult, error) {
	email = strings.TrimSpace(strings.ToLower(email))
	username = strings.TrimSpace(username)

	if email == "" && username == "" && password == "" {
		return BootstrapSkipped, nil // not configured
	}
	if email == "" || username == "" || password == "" {
		return BootstrapSkipped, ErrValidation{Msg: "bootstrap admin requires email, username, and password"}
	}
	if err := validateRegistration(email, username, password); err != nil {
		return BootstrapSkipped, err
	}

	hash, err := HashPassword(password, s.params)
	if err != nil {
		return BootstrapSkipped, fmt.Errorf("hash bootstrap password: %w", err)
	}
	return s.store.ensureBootstrapAdmin(ctx, email, username, hash)
}
