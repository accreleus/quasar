package session

// session_audit_test.go — session.launched and session.failed (UI v3 amendment
// §3). Requires Postgres: make test-db.
//
// These are the two audit rows whose actor is not an admin. A launch is recorded
// against the launching user; a FAILURE has no actor at all, which is what
// admin_activity.actor_user_id's nullability has always been for.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
	"github.com/accreleus/quasar/control-plane/internal/audit"
	"github.com/accreleus/quasar/control-plane/internal/auth"
)

type sessionAuditRow struct {
	Actor      *string
	Action     string
	TargetType string
	Target     string
	Details    map[string]any
}

func sessionAuditRows(t *testing.T, pool *pgxpool.Pool) []sessionAuditRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT actor_user_id::text, action, target_type, COALESCE(target_id, ''), details
		FROM admin_activity ORDER BY id`)
	if err != nil {
		t.Fatalf("read activity: %v", err)
	}
	defer rows.Close()
	var out []sessionAuditRow
	for rows.Next() {
		var r sessionAuditRow
		var raw []byte
		if err := rows.Scan(&r.Actor, &r.Action, &r.TargetType, &r.Target, &raw); err != nil {
			t.Fatalf("scan activity: %v", err)
		}
		if err := json.Unmarshal(raw, &r.Details); err != nil {
			t.Fatalf("unmarshal details: %v", err)
		}
		out = append(out, r)
	}
	return out
}

func clearSessionActivity(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `DELETE FROM admin_activity`); err != nil {
		t.Fatalf("truncate activity: %v", err)
	}
}

// TestLaunchIsAudited drives the real HTTP route: the actor is the bearer
// identity, and only the handler has it.
func TestLaunchIsAudited(t *testing.T) {
	pool := testDB(t)
	s := seed(t, pool, 4)
	ctx := context.Background()

	authSvc, err := auth.NewService(pool, auth.DefaultParams(), time.Hour)
	if err != nil {
		t.Fatalf("auth service: %v", err)
	}
	u, err := authSvc.Register(ctx, "launcher@audit.test", "launcher", "quasar-fixture-pw-08")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	// seed() grants the ('all') entitlement, so any account may launch the app.
	tok, err := authSvc.Login(ctx, "launcher@audit.test", "quasar-fixture-pw-08", "")
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	store := NewStore(pool)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	mux := http.NewServeMux()
	authHandler := auth.NewHandler(authSvc)
	authHandler.Register(mux)
	NewHandler(coord, store, audit.NewStore(pool)).
		Register(mux, authHandler.RequireAuth, authHandler.RequireAdmin)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	clearSessionActivity(t, pool)

	resp := doJSON(t, "POST", srv.URL+"/v1/sessions", tok.Plaintext, map[string]any{"app_id": s.appID})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /v1/sessions: got %d, want 201", resp.StatusCode)
	}
	var body struct {
		Session struct {
			ID string `json:"id"`
		} `json:"session"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode launch: %v", err)
	}

	rows := sessionAuditRows(t, pool)
	if len(rows) != 1 || rows[0].Action != "session.launched" {
		t.Fatalf("recorded %+v, want one session.launched", rows)
	}
	r := rows[0]
	if r.TargetType != "session" || r.Target != body.Session.ID {
		t.Errorf("target = %s/%s, want session/%s", r.TargetType, r.Target, body.Session.ID)
	}
	if r.Actor == nil || *r.Actor != u.ID {
		t.Errorf("actor = %v, want the launching user %s", r.Actor, u.ID)
	}
	if r.Details["app_id"] != s.appID {
		t.Errorf("details.app_id = %v, want %s", r.Details["app_id"], s.appID)
	}
	if r.Details["host_id"] != s.hostID {
		t.Errorf("details.host_id = %v, want %s", r.Details["host_id"], s.hostID)
	}
}

// TestAgentReportedFailureIsAudited: the agent's terminal failure edge, with no
// actor and the machine-readable classification in details.
func TestAgentReportedFailureIsAudited(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	disp := newFakeDispatcher(true)
	coord := newTestCoordinator(t, store, disp, testLogger(), WithAuditor(audit.NewStore(pool)))
	ctx := context.Background()

	res, err := coord.Launch(ctx, s.userID, s.appID, StreamOverride{})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	waitFor(t, func() bool { return len(disp.types()) == 2 })
	clearSessionActivity(t, pool)

	code := "app_exited_early"
	detail := "game exited"
	coord.AgentState(ctx, s.hostID, agentws.SessionStateMsg{
		SessionID: res.Session.ID, State: "failed", Detail: detail, ReasonCode: &code,
	})

	rows := sessionAuditRows(t, pool)
	if len(rows) != 1 || rows[0].Action != "session.failed" {
		t.Fatalf("recorded %+v, want one session.failed", rows)
	}
	r := rows[0]
	if r.Actor != nil {
		t.Errorf("actor = %q; a failure has no acting admin", *r.Actor)
	}
	if r.TargetType != "session" || r.Target != res.Session.ID {
		t.Errorf("target = %s/%s, want session/%s", r.TargetType, r.Target, res.Session.ID)
	}
	if r.Details["failure_code"] != code {
		t.Errorf("details.failure_code = %v, want %q", r.Details["failure_code"], code)
	}
	if r.Details["state_detail"] != detail {
		t.Errorf("details.state_detail = %v, want %q", r.Details["state_detail"], detail)
	}
	if r.Details["reason_source"] != "agent" {
		t.Errorf("details.reason_source = %v, want agent", r.Details["reason_source"])
	}
	if r.Details["host_id"] != s.hostID {
		t.Errorf("details.host_id = %v, want %s", r.Details["host_id"], s.hostID)
	}
	// error_message is free agent text and stays on the session row.
	if _, ok := r.Details["error_message"]; ok {
		t.Errorf("details carries the free-text error_message: %v", r.Details)
	}
}

// TestControlPlaneFailureIsAudited: the other half of the edge — a fault the
// control plane itself declares (a rejected command), which reaches the same
// terminal state by a different path and must produce the same row.
// TestOversizedFailureDetailStillRecords — the two free-text fields on this row
// are chosen by the agent (or by a wrapped Go error), and admin_activity.details
// has a 4096-byte CHECK. Without truncation an over-long state_detail turns the
// audit write into a constraint violation and the failure vanishes from the feed
// — exactly the row an operator most needs.
func TestOversizedFailureDetailStillRecords(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	disp := newFakeDispatcher(true)
	coord := newTestCoordinator(t, store, disp, testLogger(), WithAuditor(audit.NewStore(pool)))
	ctx := context.Background()

	res, err := coord.Launch(ctx, s.userID, s.appID, StreamOverride{})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	waitFor(t, func() bool { return len(disp.types()) == 2 })
	clearSessionActivity(t, pool)

	// Both far past the column's whole budget, and the reason past its own.
	hugeDetail := strings.Repeat("x", 9000)
	hugeReason := strings.Repeat("y", 9000)
	coord.failSessionWithDetail(res.Session.ID, hugeReason, &hugeDetail)

	rows := sessionAuditRows(t, pool)
	if len(rows) != 1 || rows[0].Action != "session.failed" {
		t.Fatalf("recorded %+v, want one session.failed — an oversized detail must not "+
			"lose the row", rows)
	}
	detail, _ := rows[0].Details["state_detail"].(string)
	if len(detail) != maxAuditStateDetail {
		t.Errorf("state_detail is %d bytes, want it truncated to %d", len(detail), maxAuditStateDetail)
	}
	reason, _ := rows[0].Details["reason"].(string)
	if len(reason) != maxAuditReason {
		t.Errorf("reason is %d bytes, want it truncated to %d", len(reason), maxAuditReason)
	}
}

// TestControlPlaneFailureCarriesItsReason: on this path failure_code does not
// exist and the session row's error_message is the only other copy, so the
// bounded reason is what makes the feed row actionable.
func TestControlPlaneFailureIsAudited(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	disp := newFakeDispatcher(true)
	coord := newTestCoordinator(t, store, disp, testLogger(), WithAuditor(audit.NewStore(pool)))
	ctx := context.Background()

	res, err := coord.Launch(ctx, s.userID, s.appID, StreamOverride{})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	waitFor(t, func() bool { return len(disp.types()) == 2 })
	clearSessionActivity(t, pool)

	coord.failSession(res.Session.ID, "agent rejected session_start: no encoder")

	rows := sessionAuditRows(t, pool)
	if len(rows) != 1 || rows[0].Action != "session.failed" {
		t.Fatalf("recorded %+v, want one session.failed", rows)
	}
	if rows[0].Details["reason_source"] != "control_plane" {
		t.Errorf("details.reason_source = %v, want control_plane", rows[0].Details["reason_source"])
	}
	if _, ok := rows[0].Details["failure_code"]; ok {
		t.Errorf("a control-plane fault has no agent failure_code: %v", rows[0].Details)
	}
	// The reason IS carried here (bounded), because nothing else on this path
	// explains the failure: there is no agent failure_code to read instead.
	reason, _ := rows[0].Details["reason"].(string)
	if !strings.Contains(reason, "no encoder") {
		t.Errorf("details.reason = %q, want the control-plane fault's reason", reason)
	}
	if len(reason) > maxAuditReason {
		t.Errorf("details.reason is %d bytes, over the %d cap", len(reason), maxAuditReason)
	}
}
