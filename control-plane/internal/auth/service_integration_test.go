package auth

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestServiceAuthFlow(t *testing.T) {
	pool := testDB(t)
	svc := testService(t, pool)
	ctx := context.Background()

	// --- register --------------------------------------------------------
	user, err := svc.Register(ctx, "Ada@Example.com", "ada", "hunter2hunter2")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if user.ID == "" || user.Role != "user" {
		t.Fatalf("unexpected user: %+v", user)
	}
	if user.Email != "ada@example.com" {
		t.Fatalf("email not normalized to lowercase: %q", user.Email)
	}

	// --- duplicate email / username --------------------------------------
	if _, err := svc.Register(ctx, "ada@example.com", "ada2", "hunter2hunter2"); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate email: want ErrConflict, got %v", err)
	}
	if _, err := svc.Register(ctx, "other@example.com", "ada", "hunter2hunter2"); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate username: want ErrConflict, got %v", err)
	}

	// --- validation ------------------------------------------------------
	if _, err := svc.Register(ctx, "bad-email", "bob", "hunter2hunter2"); !isValidation(err) {
		t.Fatalf("bad email: want ErrValidation, got %v", err)
	}
	if _, err := svc.Register(ctx, "bob@example.com", "bob", "short"); !isValidation(err) {
		t.Fatalf("short password: want ErrValidation, got %v", err)
	}
	// #513: a common password (12+ chars, so length alone would not catch it)
	// is rejected by the embedded denylist.
	if _, err := svc.Register(ctx, "carol@example.com", "carol", "Password1234"); !isValidation(err) {
		t.Fatalf("common password: want ErrValidation, got %v", err)
	}
	// #513: a password built from the account's own username is rejected.
	if _, err := svc.Register(ctx, "dave@example.com", "davebuilder", "davebuilder-secure-42"); !isValidation(err) {
		t.Fatalf("password containing username: want ErrValidation, got %v", err)
	}

	// --- login: wrong password & unknown email both ErrInvalidCredentials
	if _, err := svc.Login(ctx, "ada@example.com", "wrongpassword", ""); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("bad password: want ErrInvalidCredentials, got %v", err)
	}
	if _, err := svc.Login(ctx, "nobody@example.com", "whatever123", ""); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("unknown email: want ErrInvalidCredentials, got %v", err)
	}

	// --- login: success --------------------------------------------------
	tok, err := svc.Login(ctx, "ADA@example.com", "hunter2hunter2", "test-agent")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if tok.Plaintext == "" {
		t.Fatal("empty access token")
	}
	if tok.User.ID != user.ID {
		t.Fatalf("token user mismatch: %q != %q", tok.User.ID, user.ID)
	}

	// --- authenticate: valid token --------------------------------------
	got, _, err := svc.Authenticate(ctx, tok.Plaintext)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if got.ID != user.ID {
		t.Fatalf("authenticated user mismatch")
	}

	// --- authenticate: garbage token ------------------------------------
	if _, _, err := svc.Authenticate(ctx, "not-a-real-token"); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("garbage token: want ErrUserNotFound, got %v", err)
	}

	// --- token-reuse rejection: revoke, then reuse must fail -------------
	if err := svc.Logout(ctx, tok.Plaintext); err != nil {
		t.Fatalf("logout: %v", err)
	}
	if _, _, err := svc.Authenticate(ctx, tok.Plaintext); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("revoked token reuse: want ErrUserNotFound, got %v", err)
	}
	// logout is idempotent
	if err := svc.Logout(ctx, tok.Plaintext); err != nil {
		t.Fatalf("second logout should be a no-op: %v", err)
	}
}

func TestServiceExpiredToken(t *testing.T) {
	pool := testDB(t)
	svc := testService(t, pool)
	ctx := context.Background()

	user, err := svc.Register(ctx, "exp@example.com", "exp", "hunter2hunter2")
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	// Insert a token that expired an hour ago, directly via the store.
	plaintext, hash, err := generateToken()
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	if err := svc.store.createToken(ctx, user.ID, hash, time.Now().Add(-time.Hour), "", ""); err != nil {
		t.Fatalf("create expired token: %v", err)
	}
	if _, _, err := svc.Authenticate(ctx, plaintext); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("expired token: want ErrUserNotFound, got %v", err)
	}
}

func TestServiceDisabledUser(t *testing.T) {
	pool := testDB(t)
	svc := testService(t, pool)
	ctx := context.Background()

	user, err := svc.Register(ctx, "dis@example.com", "dis", "hunter2hunter2")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	// Mint a valid token first, then disable the account.
	tok, err := svc.Login(ctx, "dis@example.com", "hunter2hunter2", "")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET disabled_at = now() WHERE id = $1`, user.ID); err != nil {
		t.Fatalf("disable: %v", err)
	}

	// Disabled account: login fails as invalid credentials (no distinction).
	if _, err := svc.Login(ctx, "dis@example.com", "hunter2hunter2", ""); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("disabled login: want ErrInvalidCredentials, got %v", err)
	}
	// Existing token for a now-disabled user must stop authenticating.
	if _, _, err := svc.Authenticate(ctx, tok.Plaintext); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("disabled user token: want ErrUserNotFound, got %v", err)
	}
}

func isValidation(err error) bool {
	var v ErrValidation
	return errors.As(err, &v)
}

// TestServiceChangePassword (CP-01): correct-current rotates the hash (new
// authenticates, old fails) and revokes ALL active tokens (log out everywhere,
// force re-authentication). wrong-current → ErrInvalidCredentials; weak/short
// new → ErrValidation.
func TestServiceChangePassword(t *testing.T) {
	pool := testDB(t)
	svc := testService(t, pool)
	ctx := context.Background()

	user, err := svc.Register(ctx, "cp@example.com", "cp", "old-password-1")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	// Mint two tokens before the change (simulates two devices / two sessions).
	tok1, err := svc.Login(ctx, "cp@example.com", "old-password-1", "device-1")
	if err != nil {
		t.Fatalf("login tok1: %v", err)
	}
	tok2, err := svc.Login(ctx, "cp@example.com", "old-password-1", "device-2")
	if err != nil {
		t.Fatalf("login tok2: %v", err)
	}

	// --- weak new password → ErrValidation (checked before current verify) ---
	if err := svc.ChangePassword(ctx, user.ID, "old-password-1", "short"); !isValidation(err) {
		t.Fatalf("weak new password: want ErrValidation, got %v", err)
	}
	// --- wrong current password → ErrInvalidCredentials ----------------------
	if err := svc.ChangePassword(ctx, user.ID, "wrong-password", "new-password-2"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("wrong current: want ErrInvalidCredentials, got %v", err)
	}

	// --- correct rotate → success --------------------------------------------
	if err := svc.ChangePassword(ctx, user.ID, "old-password-1", "new-password-2"); err != nil {
		t.Fatalf("change password: %v", err)
	}
	// New password authenticates.
	if _, err := svc.Login(ctx, "cp@example.com", "new-password-2", ""); err != nil {
		t.Fatalf("login with new password: %v", err)
	}
	// Old password no longer works.
	if _, err := svc.Login(ctx, "cp@example.com", "old-password-1", ""); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("login with old password: want ErrInvalidCredentials, got %v", err)
	}
	// All pre-change tokens must be revoked — both devices must re-authenticate.
	if _, _, err := svc.Authenticate(ctx, tok1.Plaintext); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("tok1 after change: want ErrUserNotFound (revoked), got %v", err)
	}
	if _, _, err := svc.Authenticate(ctx, tok2.Plaintext); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("tok2 after change: want ErrUserNotFound (revoked), got %v", err)
	}
}

// TestServiceChangePasswordRejectsCommonAndIdentityPasswords pins #513 on the
// self-service change-password path: a common password is rejected without
// even reaching the current-password check (no DB read for the account
// beyond the one ChangePassword always needs), and a password built from the
// account's own username is rejected once the account's identifiers are
// known.
func TestServiceChangePasswordRejectsCommonAndIdentityPasswords(t *testing.T) {
	pool := testDB(t)
	svc := testService(t, pool)
	ctx := context.Background()

	user, err := svc.Register(ctx, "clarke@example.com", "clarkemission", "orbit-mechanics-2")
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	if err := svc.ChangePassword(ctx, user.ID, "orbit-mechanics-2", "Password1234"); !isValidation(err) {
		t.Fatalf("common new password: want ErrValidation, got %v", err)
	}
	if err := svc.ChangePassword(ctx, user.ID, "orbit-mechanics-2", "clarkemission-2026!"); !isValidation(err) {
		t.Fatalf("new password containing username: want ErrValidation, got %v", err)
	}
	// The rejected attempts must not have rotated the hash — the original
	// password still authenticates.
	if _, err := svc.Login(ctx, "clarke@example.com", "orbit-mechanics-2", ""); err != nil {
		t.Fatalf("login with original password after rejected changes: %v", err)
	}
}

// TestUpdateUserLastAdminRace (#148): two concurrent demotions of the only two
// admins must not both succeed — the last-admin guard's check+update is
// serialized by an advisory lock. Exactly one demotion wins; one admin remains.
func TestUpdateUserLastAdminRace(t *testing.T) {
	pool := testDB(t)
	svc := testService(t, pool)
	ctx := context.Background()

	mk := func(email, uname string) string {
		u, err := svc.Register(ctx, email, uname, "password-123")
		if err != nil {
			t.Fatalf("register %s: %v", email, err)
		}
		if _, err := pool.Exec(ctx, `UPDATE users SET role='admin' WHERE id::text=$1`, u.ID); err != nil {
			t.Fatalf("promote %s: %v", email, err)
		}
		return u.ID
	}
	a := mk("racea@test.local", "racea")
	b := mk("raceb@test.local", "raceb")

	role := RoleUser
	errs := make(chan error, 2)
	for _, id := range []string{a, b} {
		id := id
		go func() {
			_, err := svc.UpdateUser(ctx, id, &role, nil, nil)
			errs <- err
		}()
	}
	var lastAdminErrs, oks int
	for i := 0; i < 2; i++ {
		switch err := <-errs; {
		case err == nil:
			oks++
		case errors.Is(err, ErrLastAdmin):
			lastAdminErrs++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if oks != 1 || lastAdminErrs != 1 {
		t.Errorf("want exactly one demotion to succeed and one ErrLastAdmin; got ok=%d lastAdmin=%d", oks, lastAdminErrs)
	}
	var admins int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM users WHERE role='admin'`).Scan(&admins); err != nil {
		t.Fatalf("count admins: %v", err)
	}
	if admins != 1 {
		t.Errorf("admins remaining = %d, want 1 (zero admins = locked-out deployment)", admins)
	}
}

// TestUpdateUserDemoteWithOtherFields (#148): demotion plus other field changes
// in one call — the role applies via the locked path, the rest via the generic
// UPDATE; both must land.
func TestUpdateUserDemoteWithOtherFields(t *testing.T) {
	pool := testDB(t)
	svc := testService(t, pool)
	ctx := context.Background()

	// Two admins so demotion of one is allowed.
	var ids []string
	for i, e := range []string{"dmf1@test.local", "dmf2@test.local"} {
		u, err := svc.Register(ctx, e, fmt.Sprintf("dmf%d", i+1), "password-123")
		if err != nil {
			t.Fatalf("register: %v", err)
		}
		if _, err := pool.Exec(ctx, `UPDATE users SET role='admin' WHERE id::text=$1`, u.ID); err != nil {
			t.Fatalf("promote: %v", err)
		}
		ids = append(ids, u.ID)
	}

	role := RoleUser
	max := int32(7)
	got, err := svc.UpdateUser(ctx, ids[0], &role, nil, &max)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if got.Role != RoleUser {
		t.Errorf("role = %q, want %q", got.Role, RoleUser)
	}
	if got.MaxConcurrentSessions != 7 {
		t.Errorf("max_concurrent_sessions = %d, want 7", got.MaxConcurrentSessions)
	}
}

// TestDeleteUser (#152 quick win): allow + the three deny gates.
func TestDeleteUser(t *testing.T) {
	pool := testDB(t)
	svc := testService(t, pool)
	ctx := context.Background()

	reg := func(email, uname string) string {
		u, err := svc.Register(ctx, email, uname, "password-123")
		if err != nil {
			t.Fatalf("register %s: %v", email, err)
		}
		return u.ID
	}

	t.Run("deletes a plain user and cascades history", func(t *testing.T) {
		id := reg("del1@test.local", "del1")
		if err := svc.DeleteUser(ctx, id); err != nil {
			t.Fatalf("delete: %v", err)
		}
		var n int
		if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM users WHERE id::text=$1`, id).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != 0 {
			t.Errorf("user row still present")
		}
	})

	t.Run("unknown id → ErrUserNotFound", func(t *testing.T) {
		err := svc.DeleteUser(ctx, "00000000-0000-0000-0000-000000000000")
		if !errors.Is(err, ErrUserNotFound) {
			t.Errorf("got %v, want ErrUserNotFound", err)
		}
	})

	t.Run("last admin refused", func(t *testing.T) {
		id := reg("del-admin@test.local", "deladmin")
		if _, err := pool.Exec(ctx, `UPDATE users SET role='admin' WHERE id::text=$1`, id); err != nil {
			t.Fatalf("promote: %v", err)
		}
		// This is the only admin in the truncated test DB.
		if err := svc.DeleteUser(ctx, id); !errors.Is(err, ErrLastAdmin) {
			t.Errorf("got %v, want ErrLastAdmin", err)
		}
	})

	t.Run("active session refused", func(t *testing.T) {
		id := reg("del-active@test.local", "delactive")
		var appID string
		if err := pool.QueryRow(ctx, `INSERT INTO apps (name, default_vram_mb, default_encode_slots)
			VALUES ('del-app', 256, 1) RETURNING id::text`).Scan(&appID); err != nil {
			t.Fatalf("seed app: %v", err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO sessions (user_id, app_id, state, width, height, fps, bitrate_kbps)
			VALUES ($1::uuid, $2::uuid, 'running', 1280, 720, 30, 2000)`, id, appID); err != nil {
			t.Fatalf("seed session: %v", err)
		}
		if err := svc.DeleteUser(ctx, id); !errors.Is(err, ErrUserHasActiveSessions) {
			t.Errorf("got %v, want ErrUserHasActiveSessions", err)
		}
		// Stop the session → delete succeeds and history cascades.
		if _, err := pool.Exec(ctx, `UPDATE sessions SET state='stopped' WHERE user_id::text=$1`, id); err != nil {
			t.Fatalf("stop session: %v", err)
		}
		if err := svc.DeleteUser(ctx, id); err != nil {
			t.Fatalf("delete after stop: %v", err)
		}
		var n int
		if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM sessions WHERE user_id::text=$1`, id).Scan(&n); err != nil {
			t.Fatalf("count sessions: %v", err)
		}
		if n != 0 {
			t.Errorf("session history did not cascade")
		}
	})

	// Regression (0016): the singleton stream_profile_policy row records the
	// admin who last updated it in updated_by. The FK must SET NULL on delete,
	// otherwise deleting that admin fails with a foreign-key violation and the
	// policy row would block the delete.
	t.Run("policy updated_by admin deletes, policy survives with NULL", func(t *testing.T) {
		// Two admins so the last-admin gate does not refuse the delete.
		keepID := reg("policy-keep@test.local", "policykeep")
		if _, err := pool.Exec(ctx, `UPDATE users SET role='admin' WHERE id::text=$1`, keepID); err != nil {
			t.Fatalf("promote keep admin: %v", err)
		}
		delID := reg("policy-del@test.local", "policydel")
		if _, err := pool.Exec(ctx, `UPDATE users SET role='admin' WHERE id::text=$1`, delID); err != nil {
			t.Fatalf("promote del admin: %v", err)
		}

		// The singleton policy row is wiped by the TRUNCATE users ... CASCADE in
		// setup (it FK-references users), so re-seed it pointing updated_by at the
		// admin we will delete.
		if _, err := pool.Exec(ctx,
			`INSERT INTO stream_profile_policy (id, updated_by) VALUES (true, $1::uuid)
			 ON CONFLICT (id) DO UPDATE SET updated_by = EXCLUDED.updated_by`, delID); err != nil {
			t.Fatalf("seed policy updated_by: %v", err)
		}

		if err := svc.DeleteUser(ctx, delID); err != nil {
			t.Fatalf("delete admin referenced by policy: %v", err)
		}

		// The policy singleton survives with updated_by reset to NULL.
		var updatedBy *string
		if err := pool.QueryRow(ctx,
			`SELECT updated_by::text FROM stream_profile_policy WHERE id=true`).Scan(&updatedBy); err != nil {
			t.Fatalf("read policy: %v", err)
		}
		if updatedBy != nil {
			t.Errorf("policy updated_by = %q, want NULL", *updatedBy)
		}
	})
}

// TestAuthenticateThrottlesLastUsedAt (#421): the per-request UPDATE of
// auth_tokens.last_used_at is throttled to once per lastUsedAtThrottle window —
// it was previously a synchronous WAL write on every single authenticated
// request (13.95ms mean, dominant DB cost). Two calls inside the window must
// produce exactly one write; a call once the window has elapsed must produce a
// second.
func TestAuthenticateThrottlesLastUsedAt(t *testing.T) {
	pool := testDB(t)
	svc := testService(t, pool)
	ctx := context.Background()

	user, err := svc.Register(ctx, "throttle@example.com", "throttle", "hunter2hunter2")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	tok, err := svc.Login(ctx, user.Email, "hunter2hunter2", "test-agent")
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	readLastUsedAt := func() *time.Time {
		t.Helper()
		var lu *time.Time
		if err := pool.QueryRow(ctx,
			`SELECT last_used_at FROM auth_tokens WHERE token_hash = $1`,
			hashToken(tok.Plaintext)).Scan(&lu); err != nil {
			t.Fatalf("read last_used_at: %v", err)
		}
		return lu
	}

	// Login's own re-read (service.go Login → store.authenticate) already sets
	// last_used_at once. Capture that baseline.
	before := readLastUsedAt()
	if before == nil {
		t.Fatal("last_used_at should be set after login, got NULL")
	}

	// A second Authenticate call inside the throttle window must NOT change
	// last_used_at.
	if _, _, err := svc.Authenticate(ctx, tok.Plaintext); err != nil {
		t.Fatalf("authenticate (within window): %v", err)
	}
	afterWithinWindow := readLastUsedAt()
	if !afterWithinWindow.Equal(*before) {
		t.Fatalf("last_used_at changed within throttle window: before=%v after=%v", before, afterWithinWindow)
	}

	// Push last_used_at back beyond the throttle window and confirm the next
	// Authenticate call touches it again.
	backdated := time.Now().Add(-lastUsedAtThrottle - time.Second)
	if _, err := pool.Exec(ctx,
		`UPDATE auth_tokens SET last_used_at = $1 WHERE token_hash = $2`,
		backdated, hashToken(tok.Plaintext)); err != nil {
		t.Fatalf("backdate last_used_at: %v", err)
	}

	if _, _, err := svc.Authenticate(ctx, tok.Plaintext); err != nil {
		t.Fatalf("authenticate (after window): %v", err)
	}
	afterWindowElapsed := readLastUsedAt()
	if afterWindowElapsed == nil || !afterWindowElapsed.After(backdated) {
		t.Fatalf("last_used_at not refreshed after throttle window elapsed: backdated=%v got=%v", backdated, afterWindowElapsed)
	}
}
