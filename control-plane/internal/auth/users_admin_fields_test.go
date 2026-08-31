package auth

// users_admin_fields_test.go — AdminUser.last_seen_at + active_session_count
// (UI v3 amendment §5). Requires Postgres: make test-db.
//
// Both are aggregated in the LIST query. A users page that costs one extra call
// per row is the thing this field exists to prevent, so the list path is what is
// tested, not a per-user helper.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func seedDevice(t *testing.T, pool *pgxpool.Pool, userID, key string, lastSeen time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO user_devices (user_id, device_key, name, last_seen_at)
		VALUES ($1::uuid, $2, 'test device', $3)`, userID, key, lastSeen); err != nil {
		t.Fatalf("seed device: %v", err)
	}
}

// seedSessionFor inserts a session in the given state. It needs an app and a
// host, which this package does not otherwise create.
func seedSessionFor(t *testing.T, pool *pgxpool.Pool, userID, state string) {
	t.Helper()
	ctx := context.Background()
	var appID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO apps (name, default_width, default_height, default_fps, default_bitrate_kbps)
		VALUES ('audit-app', 1280, 720, 60, 6000) RETURNING id::text`).Scan(&appID); err != nil {
		t.Fatalf("seed app: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO sessions (user_id, app_id, state, width, height, fps, bitrate_kbps)
		VALUES ($1::uuid, $2::uuid, $3, 1280, 720, 60, 6000)`, userID, appID, state); err != nil {
		t.Fatalf("seed session (%s): %v", state, err)
	}
}

func TestAdminUserActivityFields(t *testing.T) {
	pool := testDB(t)
	svc := testService(t, pool)
	ctx := context.Background()

	busy, err := svc.Register(ctx, "busy@fields.test", "busyuser", "quasar-fixture-pw-08")
	if err != nil {
		t.Fatalf("register busy: %v", err)
	}
	quiet, err := svc.Register(ctx, "quiet@fields.test", "quietuser", "quasar-fixture-pw-08")
	if err != nil {
		t.Fatalf("register quiet: %v", err)
	}

	// Two devices: last_seen_at is the MAX, not the newest row or the first.
	older := time.Now().Add(-72 * time.Hour).UTC().Truncate(time.Second)
	newest := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Second)
	seedDevice(t, pool, busy.ID, "dev-newest", newest)
	seedDevice(t, pool, busy.ID, "dev-older", older)

	// Two non-terminal sessions and two terminal ones.
	for _, st := range []string{"running", "pending", "stopped", "failed"} {
		seedSessionFor(t, pool, busy.ID, st)
	}
	// The quiet user's terminal session must not count either.
	seedSessionFor(t, pool, quiet.ID, "stopped")

	users, _, err := svc.ListUsers(ctx, "", 50)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	byID := map[string]AdminUser{}
	for _, u := range users {
		byID[u.ID] = u
	}

	got := byID[busy.ID]
	if got.LastSeenAt == nil {
		t.Fatalf("busy user last_seen_at is nil; they have two devices")
	}
	if !got.LastSeenAt.UTC().Truncate(time.Second).Equal(newest) {
		t.Errorf("last_seen_at = %v, want the MAX across devices (%v)", got.LastSeenAt.UTC(), newest)
	}
	if got.ActiveSessionCount != 2 {
		t.Errorf("active_session_count = %d, want 2 (running + pending; terminal rows excluded)",
			got.ActiveSessionCount)
	}

	q := byID[quiet.ID]
	if q.LastSeenAt != nil {
		t.Errorf("a user with no devices has last_seen_at = %v, want nil", q.LastSeenAt)
	}
	if q.ActiveSessionCount != 0 {
		t.Errorf("quiet user active_session_count = %d, want 0", q.ActiveSessionCount)
	}
}

// TestAdminUserFieldsAlwaysSerialized: both keys are required on the wire, so a
// client can tell "no devices" from "an older server that does not send it".
func TestAdminUserFieldsAlwaysSerialized(t *testing.T) {
	pool := testDB(t)
	svc := testService(t, pool)
	ctx := context.Background()
	u, err := svc.Register(ctx, "wire@fields.test", "wireuser", "quasar-fixture-pw-08")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET role='admin' WHERE id::text=$1`, u.ID); err != nil {
		t.Fatalf("promote: %v", err)
	}

	users, _, err := svc.ListUsers(ctx, "", 50)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	raw, err := json.Marshal(toAdminUserResp(users[0]))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if v, ok := decoded["last_seen_at"]; !ok || string(v) != "null" {
		t.Errorf("last_seen_at on the wire: got %s (present=%v), want null", v, ok)
	}
	if v, ok := decoded["active_session_count"]; !ok || string(v) != "0" {
		t.Errorf("active_session_count on the wire: got %s (present=%v), want 0", v, ok)
	}
}

// TestPatchUserReturnsActivityFields: the PATCH response reuses AdminUser, so it
// must carry the same derived values — a page that patches and re-renders from
// the response must not blank the columns it just showed.
func TestPatchUserReturnsActivityFields(t *testing.T) {
	srv, pool, tok, _, subjectID := newAuditedUserServer(t)
	seedDevice(t, pool, subjectID, "dev-patch", time.Now().Add(-time.Hour))
	seedSessionFor(t, pool, subjectID, "running")

	resp, body := do(t, req(t, http.MethodPatch, srv.URL+"/v1/users/"+subjectID,
		map[string]any{"max_concurrent_sessions": 4}, tok))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH: got %d, want 200", resp.StatusCode)
	}
	user, ok := body["user"].(map[string]any)
	if !ok {
		t.Fatalf("PATCH body has no user: %v", body)
	}
	if user["last_seen_at"] == nil {
		t.Error("PATCH response last_seen_at is null; the user has a device")
	}
	if user["active_session_count"] != float64(1) {
		t.Errorf("PATCH response active_session_count = %v, want 1", user["active_session_count"])
	}
}
