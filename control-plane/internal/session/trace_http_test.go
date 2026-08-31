package session

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/telemetry"
)

// ST-04 HTTP-level tests for the browser trace-event ingest endpoint:
// POST /v1/sessions/{id}/trace/events. Like the stats-POST tests, these exercise
// the real auth middleware and run the full handler path. DB-gated (go-test-db).

// TestPostTraceEventsOwnerGate: owner posts → 202 + known-type events stored as
// source='browser'; unknown types dropped; non-owner → 403; unknown session → 404.
func TestPostTraceEventsOwnerGate(t *testing.T) {
	pool := testDB(t)
	srv, authSvc, store := newMetricsServer(t, pool)
	ctx := context.Background()

	_ = seed(t, pool, 4)
	owner, err := authSvc.Register(ctx, "owner@test.local", "owner", "quasar-fixture-pw-03")
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}
	if _, err := authSvc.Register(ctx, "other@test.local", "other", "quasar-fixture-pw-02"); err != nil {
		t.Fatalf("register other: %v", err)
	}
	s := currentSeed(t, pool)
	sid := sessionForUser(t, store, s, owner.ID)

	ownerTok := loginTok(t, authSvc, "owner@test.local", "quasar-fixture-pw-03")
	otherTok := loginTok(t, authSvc, "other@test.local", "quasar-fixture-pw-02")

	eventsBody := map[string]any{
		"client": "browser",
		"events": []map[string]any{
			{
				"ts_unix_ms": nowMs() + 100,
				"type":       "playout.changed",
				"payload":    map[string]any{"from_ms": 100, "to_ms": 67, "reason": "degrade"},
			},
			{
				"ts_unix_ms": nowMs() + 250,
				"type":       "client.visibility_changed",
				"payload":    map[string]any{"hidden": true},
			},
			{
				"ts_unix_ms": nowMs() + 300,
				"type":       "unknown.evil_type", // must be dropped
				"payload":    map[string]any{"bad": "data"},
			},
		},
	}

	// Owner → 202 + rows written; unknown type dropped.
	resp := doJSON(t, http.MethodPost, srv.URL+"/v1/sessions/"+sid+"/trace/events", ownerTok, eventsBody)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("owner POST: got %d want 202", resp.StatusCode)
	}
	resp.Body.Close()

	// 2 known-type events stored; the unknown type was dropped.
	if n := countTraceEvents(t, pool, sid); n != 2 {
		t.Fatalf("trace event rows: got %d want 2 (unknown type should be dropped)", n)
	}

	// All stored events must be source='browser'.
	events, err := store.Telemetry().Events(ctx, sid, telemetry.Range{FromMs: 0, ToMs: 0}, telemetry.Filter{Types: nil, Limit: 10})
	if err != nil {
		t.Fatalf("query events: %v", err)
	}
	for _, e := range events {
		if e.Source != telemetry.SourceBrowser {
			t.Errorf("event source: got %q want %q", e.Source, telemetry.SourceBrowser)
		}
	}

	// Verify playout.changed payload was stored verbatim.
	var found bool
	for _, e := range events {
		if e.Type == "playout.changed" {
			found = true
			var p map[string]any
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				t.Fatalf("playout.changed payload json: %v", err)
			}
			if p["reason"] != "degrade" {
				t.Errorf("playout.changed reason: got %v", p["reason"])
			}
		}
	}
	if !found {
		t.Error("playout.changed event not stored")
	}

	// Non-owner → 403.
	resp = doJSON(t, http.MethodPost, srv.URL+"/v1/sessions/"+sid+"/trace/events", otherTok, eventsBody)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-owner POST: got %d want 403", resp.StatusCode)
	}
	resp.Body.Close()

	// Unknown session → 404.
	resp = doJSON(t, http.MethodPost,
		srv.URL+"/v1/sessions/00000000-0000-0000-0000-000000000000/trace/events", ownerTok, eventsBody)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown session POST: got %d want 404", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestPostTraceEventsAllUnknownTypesDropped: a batch containing ONLY unknown event
// types results in 202 with zero rows stored.
func TestPostTraceEventsAllUnknownTypesDropped(t *testing.T) {
	pool := testDB(t)
	srv, authSvc, store := newMetricsServer(t, pool)
	ctx := context.Background()

	_ = seed(t, pool, 4)
	owner, _ := authSvc.Register(ctx, "owner@test.local", "owner", "quasar-fixture-pw-03")
	s := currentSeed(t, pool)
	sid := sessionForUser(t, store, s, owner.ID)
	ownerTok := loginTok(t, authSvc, "owner@test.local", "quasar-fixture-pw-03")

	eventsBody := map[string]any{
		"events": []map[string]any{
			{"ts_unix_ms": nowMs(), "type": "evil.type", "payload": map[string]any{"x": 1}},
			{"ts_unix_ms": nowMs() + 1, "type": "another.unknown", "payload": map[string]any{}},
		},
	}

	resp := doJSON(t, http.MethodPost, srv.URL+"/v1/sessions/"+sid+"/trace/events", ownerTok, eventsBody)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("all-unknown-types POST: got %d want 202", resp.StatusCode)
	}
	resp.Body.Close()

	if n := countTraceEvents(t, pool, sid); n != 0 {
		t.Fatalf("unknown types stored: got %d want 0", n)
	}
}

// TestPostTraceEventsAdminCanPost: an admin can post to any session (owner-or-admin).
func TestPostTraceEventsAdminCanPost(t *testing.T) {
	pool := testDB(t)
	srv, authSvc, store := newMetricsServer(t, pool)
	ctx := context.Background()

	_ = seed(t, pool, 4)
	owner, _ := authSvc.Register(ctx, "owner@test.local", "owner", "quasar-fixture-pw-03")
	if _, err := authSvc.Register(ctx, "admin@test.local", "admin", "quasar-fixture-pw-01"); err != nil {
		t.Fatalf("register admin: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET role='admin' WHERE email='admin@test.local'`); err != nil {
		t.Fatalf("promote admin: %v", err)
	}
	s := currentSeed(t, pool)
	sid := sessionForUser(t, store, s, owner.ID)
	adminTok := loginTok(t, authSvc, "admin@test.local", "quasar-fixture-pw-01")

	eventsBody := map[string]any{
		"events": []map[string]any{
			{"ts_unix_ms": nowMs(), "type": "playout.changed",
				"payload": map[string]any{"from_ms": 50, "to_ms": 75}},
		},
	}

	resp := doJSON(t, http.MethodPost, srv.URL+"/v1/sessions/"+sid+"/trace/events", adminTok, eventsBody)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("admin POST: got %d want 202", resp.StatusCode)
	}
	resp.Body.Close()

	if n := countTraceEvents(t, pool, sid); n != 1 {
		t.Fatalf("admin POST rows: got %d want 1", n)
	}
}

// TestPostTraceEventsBodyLimits: >64 events → 400; oversize body → 400.
func TestPostTraceEventsBodyLimits(t *testing.T) {
	pool := testDB(t)
	srv, authSvc, store := newMetricsServer(t, pool)
	ctx := context.Background()
	_ = seed(t, pool, 4)
	owner, _ := authSvc.Register(ctx, "owner@test.local", "owner", "quasar-fixture-pw-03")
	s := currentSeed(t, pool)
	sid := sessionForUser(t, store, s, owner.ID)
	ownerTok := loginTok(t, authSvc, "owner@test.local", "quasar-fixture-pw-03")

	// > 64 events → 400.
	many := make([]map[string]any, 65)
	for i := range many {
		many[i] = map[string]any{
			"ts_unix_ms": nowMs() + int64(i),
			"type":       "playout.changed",
			"payload":    map[string]any{},
		}
	}
	resp := doJSON(t, http.MethodPost, srv.URL+"/v1/sessions/"+sid+"/trace/events", ownerTok,
		map[string]any{"events": many})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf(">64 events: got %d want 400", resp.StatusCode)
	}
	resp.Body.Close()

	// Oversize body (> 32 KB) → 400.
	big := `{"events":[{"ts_unix_ms":1,"type":"playout.changed","payload":{"x":"` +
		strings.Repeat("a", 40000) + `"}}]}`
	resp = doJSON(t, http.MethodPost, srv.URL+"/v1/sessions/"+sid+"/trace/events", ownerTok, big)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("oversize body: got %d want 400", resp.StatusCode)
	}
	resp.Body.Close()

	// Unauthenticated → 401.
	resp = doJSON(t, http.MethodPost, srv.URL+"/v1/sessions/"+sid+"/trace/events", "", eventsBodySmall())
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: got %d want 401", resp.StatusCode)
	}
	resp.Body.Close()
	_ = ctx
}

// TestPostStatsStillWorks: the existing POST /v1/sessions/{id}/stats endpoint is
// unaffected by the ST-04 changes — it still returns 202 and still drops unknown
// metric keys. This is a regression guard, not a net-new test.
func TestPostStatsStillWorks(t *testing.T) {
	pool := testDB(t)
	srv, authSvc, store := newMetricsServer(t, pool)
	ctx := context.Background()

	_ = seed(t, pool, 4)
	owner, _ := authSvc.Register(ctx, "owner@test.local", "owner", "quasar-fixture-pw-03")
	s := currentSeed(t, pool)
	sid := sessionForUser(t, store, s, owner.ID)
	ownerTok := loginTok(t, authSvc, "owner@test.local", "quasar-fixture-pw-03")

	statsBody := map[string]any{"samples": []map[string]any{
		{"ts_unix_ms": nowMs(), "metrics": map[string]any{
			"fps": 59.6, "rtt_ms": 12, "evil_key": "drop",
		}},
	}}

	resp := doJSON(t, http.MethodPost, srv.URL+"/v1/sessions/"+sid+"/stats", ownerTok, statsBody)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("stats POST: got %d want 202", resp.StatusCode)
	}
	resp.Body.Close()

	samples, _, _ := store.Telemetry().Recent(ctx, sid, 10, nil, "")
	if len(samples) != 1 {
		t.Fatalf("stats POST rows: got %d want 1", len(samples))
	}
	var m map[string]any
	_ = json.Unmarshal(samples[0].Metrics, &m)
	if _, ok := m["evil_key"]; ok {
		t.Fatalf("unknown key stored: %v", m)
	}
	if m["fps"] == nil {
		t.Fatalf("fps missing: %v", m)
	}
}

func eventsBodySmall() map[string]any {
	return map[string]any{
		"events": []map[string]any{
			{"ts_unix_ms": nowMs(), "type": "playout.changed", "payload": map[string]any{}},
		},
	}
}

// nowMs is the fixture clock for ingest tests. Client-reported timestamps are now
// validated against server-now (telemetry.PlausibleTsUnixMs), so a fixed epoch
// literal is no longer a valid stand-in for a real client stamp — it is exactly
// the "stored where no read window reaches it" case the gate rejects.
func nowMs() int64 { return time.Now().UnixMilli() }
