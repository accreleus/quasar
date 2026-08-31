package session

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
	"github.com/accreleus/quasar/control-plane/internal/auth"

	"github.com/accreleus/quasar/control-plane/internal/telemetry"
)

// session-capture, at the HTTP level: the real auth middleware, the real routes,
// a real database, and a fake agent whose ack the test chooses. DB-gated like the
// rest of the package (go-test-db / make test-db).

func newCaptureServer(t *testing.T, pool *pgxpool.Pool, disp Dispatcher) (*httptest.Server, *auth.Service, *Store) {
	t.Helper()
	authSvc, err := auth.NewService(pool, auth.DefaultParams(), time.Hour)
	if err != nil {
		t.Fatalf("auth service: %v", err)
	}
	store := NewStore(pool)
	coord := newTestCoordinator(t, store, disp, testLogger())
	mux := http.NewServeMux()
	authHandler := auth.NewHandler(authSvc)
	authHandler.Register(mux)
	NewHandler(coord, store).Register(mux, authHandler.RequireAuth, authHandler.RequireAdmin)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, authSvc, store
}

func captureAdminToken(t *testing.T, pool *pgxpool.Pool, authSvc *auth.Service) string {
	t.Helper()
	ctx := context.Background()
	if _, err := authSvc.Register(ctx, "capadmin@test.local", "capadmin", "unrelated-pw-10"); err != nil {
		t.Fatalf("register admin: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE users SET role='admin' WHERE email='capadmin@test.local'`); err != nil {
		t.Fatalf("promote admin: %v", err)
	}
	return loginTok(t, authSvc, "capadmin@test.local", "unrelated-pw-10")
}

// runningSessionFor builds a running, placed session — the only state in which
// there is anything to observe.
func runningSessionFor(t *testing.T, pool *pgxpool.Pool, store *Store) string {
	t.Helper()
	s := seed(t, pool, 4)
	return runningSession(t, store, s).ID
}

// TestArmCaptureAccepted: the happy path. 202 with the minted join key, and the
// command that reached the agent carries that same id plus a bounded budget.
func TestArmCaptureAccepted(t *testing.T) {
	pool := testDB(t)
	disp := newFakeDispatcher(true)
	srv, authSvc, store := newCaptureServer(t, pool, disp)
	sid := runningSessionFor(t, pool, store)
	tok := captureAdminToken(t, pool, authSvc)

	resp := doJSON(t, http.MethodPost, srv.URL+"/v1/admin/sessions/"+sid+"/capture", tok,
		map[string]any{"kind": "burst_stats", "params": map[string]any{"windows": 999, "window_ms": 9999}})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("arm: got %d want 202", resp.StatusCode)
	}
	var body struct {
		CaptureID string `json:"capture_id"`
		Kind      string `json:"kind"`
		SessionID string `json:"session_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	if body.CaptureID == "" || body.Kind != "burst_stats" || body.SessionID != sid {
		t.Fatalf("202 body: %+v", body)
	}

	disp.mu.Lock()
	cmd := disp.lastCapture
	disp.mu.Unlock()
	if cmd == nil {
		t.Fatal("no session_capture reached the agent")
	}
	if cmd.CaptureID != body.CaptureID {
		t.Fatalf("the 202's id and the wire's id differ: %s vs %s", body.CaptureID, cmd.CaptureID)
	}
	if cmd.Budget.MaxBytes != captureMaxBytes || cmd.Budget.MaxMs != captureMaxMs {
		t.Fatalf("budget not sent: %+v", cmd.Budget)
	}
	// An out-of-range window plan was clamped INTO the wall-clock budget before
	// dispatch — the agent should never be handed a plan it cannot finish.
	if cmd.Params == nil || cmd.Params.Windows*cmd.Params.WindowMs > captureMaxMs {
		t.Fatalf("params not clamped into the budget: %+v", cmd.Params)
	}
}

// TestArmCaptureAckFailures: each documented nack, and the silence that means an
// old agent, becomes its own status code.
func TestArmCaptureAckFailures(t *testing.T) {
	cases := []struct {
		name    string
		ackErr  string
		sendErr error
		want    int
	}{
		{"busy", "busy", nil, http.StatusConflict},
		{"unknown kind", "unknown_kind", nil, http.StatusUnprocessableEntity},
		{"unsupported here", "unsupported", nil, http.StatusUnprocessableEntity},
		{"agent not connected", "", agentws.ErrAgentNotConnected, http.StatusServiceUnavailable},
		{"agent predates captures", "", context.DeadlineExceeded, http.StatusNotImplemented},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool := testDB(t)
			disp := newFakeDispatcher(false)
			disp.ackErr = tc.ackErr
			disp.ackSendErr = tc.sendErr
			srv, authSvc, store := newCaptureServer(t, pool, disp)
			sid := runningSessionFor(t, pool, store)
			tok := captureAdminToken(t, pool, authSvc)

			resp := doJSON(t, http.MethodPost, srv.URL+"/v1/admin/sessions/"+sid+"/capture", tok,
				map[string]any{"kind": "pipeline_dot"})
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("got %d want %d", resp.StatusCode, tc.want)
			}
			var env struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&env)
			if env.Error.Code == "" || env.Error.Message == "" {
				t.Fatalf("refusal carried no code/message: %+v", env)
			}
		})
	}
}

// TestArmCaptureNotRunning / unknown session / non-admin — the three refusals that
// never reach the agent.
func TestArmCaptureGates(t *testing.T) {
	pool := testDB(t)
	disp := newFakeDispatcher(true)
	srv, authSvc, store := newCaptureServer(t, pool, disp)
	ctx := context.Background()
	s := seed(t, pool, 4)
	tok := captureAdminToken(t, pool, authSvc)

	// A pending (never-started) session has no pipeline to observe.
	sess, err := store.ScheduleAndCreate(ctx, launchParams(s))
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	resp := doJSON(t, http.MethodPost, srv.URL+"/v1/admin/sessions/"+sess.ID+"/capture", tok,
		map[string]any{"kind": "pipeline_dot"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("not-running arm: got %d want 409", resp.StatusCode)
	}
	resp.Body.Close()

	// Unknown session → 404 (to an admin).
	resp = doJSON(t, http.MethodPost,
		srv.URL+"/v1/admin/sessions/00000000-0000-0000-0000-000000000000/capture", tok,
		map[string]any{"kind": "pipeline_dot"})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown session: got %d want 404", resp.StatusCode)
	}
	resp.Body.Close()

	// A non-admin bearer is 403 BEFORE the lookup — the gate is the server's, and
	// it must not leak whether the session exists.
	if _, err := authSvc.Register(ctx, "plain@test.local", "plain", "unrelated-pw-16"); err != nil {
		t.Fatalf("register user: %v", err)
	}
	userTok := loginTok(t, authSvc, "plain@test.local", "unrelated-pw-16")
	resp = doJSON(t, http.MethodPost, srv.URL+"/v1/admin/sessions/"+sess.ID+"/capture", userTok,
		map[string]any{"kind": "pipeline_dot"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin arm: got %d want 403", resp.StatusCode)
	}
	resp.Body.Close()

	if len(disp.types()) != 0 {
		t.Fatalf("a refused arm still dispatched: %v", disp.types())
	}
}

// TestReadCapture404ThenPresent: the poll protocol. 404 while the capture is in
// flight — that is the signal, not an error — then 200 with the agent's payload
// verbatim once the diag.* event lands. The bundle carries it too.
func TestReadCapture404ThenPresent(t *testing.T) {
	pool := testDB(t)
	disp := newFakeDispatcher(true)
	srv, authSvc, store := newCaptureServer(t, pool, disp)
	sid := runningSessionFor(t, pool, store)
	tok := captureAdminToken(t, pool, authSvc)
	ctx := context.Background()

	resp := doJSON(t, http.MethodPost, srv.URL+"/v1/admin/sessions/"+sid+"/capture", tok,
		map[string]any{"kind": "encoder_props"})
	var armed struct {
		CaptureID string `json:"capture_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&armed)
	resp.Body.Close()

	url := srv.URL + "/v1/admin/sessions/" + sid + "/captures/" + armed.CaptureID
	resp = doJSON(t, http.MethodGet, url, tok, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("before the result lands: got %d want 404", resp.StatusCode)
	}
	resp.Body.Close()

	// The agent reports. (The ingest path that routes this synchronously is tested
	// in internal/agentws; here we assert what a reader sees.)
	payload := `{"capture_id":"` + armed.CaptureID + `","kind":"encoder_props","encoding":"json",` +
		`"content_type":"application/json","json":{"encoder_factory":"vulkanh264enc"},` +
		`"bytes":42,"compressed_bytes":42,"truncated":false,"duration_ms":3}`
	if err := store.Telemetry().AppendEvent(ctx, sid, telemetry.SourceAgent, telemetry.EventInput{
		TsUnixMs: 1735689600000, Type: "diag.encoder_props", Payload: json.RawMessage(payload)}); err != nil {
		t.Fatalf("insert diag event: %v", err)
	}

	resp = doJSON(t, http.MethodGet, url, tok, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("after the result lands: got %d want 200", resp.StatusCode)
	}
	var got map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if got["capture_id"] != armed.CaptureID || got["kind"] != "encoder_props" {
		t.Fatalf("capture body: %+v", got)
	}
	if got["ts_unix_ms"] == nil {
		t.Fatalf("capture body carries no ts_unix_ms: %+v", got)
	}
	// The payload is stored and served VERBATIM: a new capture kind must not need
	// a control-plane release to be readable.
	inner, _ := got["json"].(map[string]any)
	if inner == nil || inner["encoder_factory"] != "vulkanh264enc" {
		t.Fatalf("agent payload not served verbatim: %+v", got)
	}

	// A capture id belonging to nothing is a 404, not a 500.
	resp = doJSON(t, http.MethodGet, srv.URL+"/v1/admin/sessions/"+sid+"/captures/nope", tok, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown capture id: got %d want 404", resp.StatusCode)
	}
	resp.Body.Close()

	// The bundle carries the capture, even though the event's ts_unix_ms (2025) is
	// nowhere near the bundle's default 5-minute window. That is the point.
	resp = doJSON(t, http.MethodGet, srv.URL+"/v1/admin/sessions/"+sid+"/diagnostic-bundle", tok, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bundle: got %d want 200", resp.StatusCode)
	}
	var bundle struct {
		Captures []map[string]any `json:"captures"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&bundle)
	resp.Body.Close()
	if len(bundle.Captures) != 1 || bundle.Captures[0]["capture_id"] != armed.CaptureID {
		t.Fatalf("bundle captures: %+v", bundle.Captures)
	}
}

// TestBundleCapturesAlwaysAnArray: a session with no captures still carries the
// key as [], so a consumer can iterate without a presence check.
func TestBundleCapturesAlwaysAnArray(t *testing.T) {
	pool := testDB(t)
	srv, authSvc, store := newCaptureServer(t, pool, newFakeDispatcher(true))
	sid := runningSessionFor(t, pool, store)
	tok := captureAdminToken(t, pool, authSvc)

	resp := doJSON(t, http.MethodGet, srv.URL+"/v1/admin/sessions/"+sid+"/diagnostic-bundle", tok, nil)
	defer resp.Body.Close()
	var raw map[string]json.RawMessage
	_ = json.NewDecoder(resp.Body).Decode(&raw)
	if string(raw["captures"]) != "[]" {
		t.Fatalf("captures should be []: %s", raw["captures"])
	}
}
