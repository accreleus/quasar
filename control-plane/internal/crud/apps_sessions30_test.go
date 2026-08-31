package crud

// apps_sessions30_test.go — AdminApp.sessions_30d (UI v3 amendment §7).
// Requires Postgres: make test-db.
//
// Counted in the LIST query, not per row, and EVERY state counts: a launch that
// failed is still a launch someone attempted.

import (
	"context"
	"encoding/json"
	"testing"
)

func TestAdminAppsSessions30d(t *testing.T) {
	pool := testPool(t)
	s := &store{pool: pool}
	ctx := context.Background()
	ids := seedBasic(t, pool)

	// A second app with no sessions at all.
	var quietID string
	mustExec(t, pool.QueryRow(ctx, `INSERT INTO apps
		(name, default_width, default_height, default_fps, default_bitrate_kbps)
		VALUES ('quiet-app', 1280, 720, 60, 6000) RETURNING id::text`).Scan(&quietID))

	// Three recent sessions across three different states, plus one that ended 40
	// days ago and must fall outside the window.
	for _, state := range []string{"running", "stopped", "failed"} {
		insertSession(t, pool, ids, state)
	}
	old := insertSession(t, pool, ids, "stopped")
	if _, err := pool.Exec(ctx,
		`UPDATE sessions SET created_at = now() - interval '40 days' WHERE id::text = $1`, old); err != nil {
		t.Fatalf("age the old session: %v", err)
	}

	apps, _, err := s.listAllApps(ctx, ids.userID, "", 50)
	if err != nil {
		t.Fatalf("listAllApps: %v", err)
	}
	byID := map[string]App{}
	for _, a := range apps {
		byID[a.ID] = a
	}
	if got := byID[ids.appID].Sessions30d; got != 3 {
		t.Errorf("sessions_30d = %d, want 3 (every state within 30 days; the 40-day-old row excluded)", got)
	}
	if got := byID[quietID].Sessions30d; got != 0 {
		t.Errorf("an app with no sessions has sessions_30d = %d, want 0", got)
	}

	// Admin-only, and always serialized.
	raw, err := json.Marshal(appToAdminResp(byID[ids.appID]))
	if err != nil {
		t.Fatalf("marshal admin app: %v", err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if v, ok := decoded["sessions_30d"]; !ok || string(v) != "3" {
		t.Errorf("sessions_30d on the wire: got %s (present=%v), want 3", v, ok)
	}
	// The public shape must not carry it — it is fleet-wide across all users.
	// A FRESH map: json.Unmarshal merges into a non-empty one.
	if raw, err = json.Marshal(appToResp(byID[ids.appID])); err != nil {
		t.Fatalf("marshal app: %v", err)
	}
	publicShape := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &publicShape); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := publicShape["sessions_30d"]; ok {
		t.Error("sessions_30d leaked onto the public App shape")
	}
}
