package session

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/auth"
)

// PATCH /v1/sessions/{id}/display (session-display-update). The validation table
// is a pure unit test; the status-code contract is exercised over real HTTP with
// the real ownership middleware, because 404-before-403 is the load-bearing part
// and only the wired handler can prove it.

func i32(v int32) *int32 { return &v }

// TestValidateDisplayUpdate pins the 400 rules from control-api.md against a
// session whose pinned stream is 1280x720.
func TestValidateDisplayUpdate(t *testing.T) {
	sess := Session{Width: 1280, Height: 720}

	cases := []struct {
		name    string
		upd     DisplayUpdate
		wantErr bool
	}{
		{"scale only", DisplayUpdate{UIScale: f64(1.5)}, false},
		{"dims only", DisplayUpdate{RenderWidth: i32(640), RenderHeight: i32(360)}, false},
		{"dims + scale", DisplayUpdate{RenderWidth: i32(1280), RenderHeight: i32(720), UIScale: f64(1.0)}, false},
		{"scale at max", DisplayUpdate{UIScale: f64(3.0)}, false},
		{"dims at floor", DisplayUpdate{RenderWidth: i32(16), RenderHeight: i32(16)}, false},

		{"empty", DisplayUpdate{}, true},
		{"width without height", DisplayUpdate{RenderWidth: i32(640)}, true},
		{"height without width", DisplayUpdate{RenderHeight: i32(360)}, true},
		{"odd width", DisplayUpdate{RenderWidth: i32(641), RenderHeight: i32(360)}, true},
		{"odd height", DisplayUpdate{RenderWidth: i32(640), RenderHeight: i32(361)}, true},
		{"width below floor", DisplayUpdate{RenderWidth: i32(14), RenderHeight: i32(360)}, true},
		{"height below floor", DisplayUpdate{RenderWidth: i32(640), RenderHeight: i32(14)}, true},
		{"width above launch", DisplayUpdate{RenderWidth: i32(1920), RenderHeight: i32(720)}, true},
		{"height above launch", DisplayUpdate{RenderWidth: i32(1280), RenderHeight: i32(1080)}, true},
		{"scale below min", DisplayUpdate{UIScale: f64(0.5)}, true},
		{"scale above max", DisplayUpdate{UIScale: f64(3.5)}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Render dims are bounded by the session's pinned LAUNCH size,
			// independent of any external/stream state (2026-08-16 amendment).
			err := validateDisplayUpdate(tc.upd, sess)
			if tc.wantErr && err == nil {
				t.Fatalf("validateDisplayUpdate(%+v): want error, got nil", tc.upd)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("validateDisplayUpdate(%+v): unexpected error %v", tc.upd, err)
			}
		})
	}
}

// newDisplayServer wires the real session routes behind the real auth middleware,
// returning the fake dispatcher so the test can read the dispatched command.
func newDisplayServer(t *testing.T, pool *pgxpool.Pool, ackOK bool) (*httptest.Server, *auth.Service, *Store, *fakeDispatcher) {
	t.Helper()
	authSvc, err := auth.NewService(pool, auth.DefaultParams(), time.Hour)
	if err != nil {
		t.Fatalf("auth service: %v", err)
	}
	store := NewStore(pool)
	disp := newFakeDispatcher(ackOK)
	coord := newTestCoordinator(t, store, disp, testLogger())

	mux := http.NewServeMux()
	authHandler := auth.NewHandler(authSvc)
	authHandler.Register(mux)
	NewHandler(coord, store).Register(mux, authHandler.RequireAuth, authHandler.RequireAdmin)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, authSvc, store, disp
}

func errCode(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	return body.Error.Code
}

// TestDisplayUpdateAccepted: a valid update on a running session is relayed with
// the contract's field names and answered 202 with the (unchanged) session.
func TestDisplayUpdateAccepted(t *testing.T) {
	pool := testDB(t)
	srv, authSvc, store, disp := newDisplayServer(t, pool, true)
	ctx := context.Background()

	_ = seed(t, pool, 4)
	owner, err := authSvc.Register(ctx, "dispowner@test.local", "dispowner", "unrelated-pw-11")
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}
	tok := loginTok(t, authSvc, "dispowner@test.local", "unrelated-pw-11")
	sid := sessionForUser(t, store, currentSeed(t, pool), owner.ID) // stream 1280x720

	resp := doJSON(t, http.MethodPatch, srv.URL+"/v1/sessions/"+sid+"/display", tok,
		map[string]any{"render_width": 640, "render_height": 360, "ui_scale": 1.5})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("PATCH display: got %d want 202 (code=%s)", resp.StatusCode, errCode(t, resp))
	}
	got := decodeSessionResp(t, resp)
	if got.ID != sid {
		t.Fatalf("202 body session id: got %s want %s", got.ID, sid)
	}
	// The Session shape is unchanged by this endpoint: the STREAM size is still
	// the launched one, not the new render size.
	if got.Stream.Width != 1280 || got.Stream.Height != 720 {
		t.Fatalf("stream size changed: got %dx%d want 1280x720", got.Stream.Width, got.Stream.Height)
	}

	cmd := disp.lastDisplay
	if cmd == nil {
		t.Fatal("no session_display_update dispatched")
	}
	if cmd.Type != "session_display_update" || cmd.SessionID != sid || cmd.ID == "" {
		t.Fatalf("dispatched cmd envelope: %+v", cmd)
	}
	if cmd.RenderWidth == nil || *cmd.RenderWidth != 640 ||
		cmd.RenderHeight == nil || *cmd.RenderHeight != 360 ||
		cmd.UIScale == nil || *cmd.UIScale != 1.5 {
		t.Fatalf("dispatched cmd values: %+v", cmd)
	}

	// Nothing is persisted: render size / UI scale are ephemeral, agent-held.
	after, err := store.Get(ctx, sid)
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if after.Width != 1280 || after.Height != 720 || after.State != StateRunning {
		t.Fatalf("display update mutated the row: %dx%d state=%s", after.Width, after.Height, after.State)
	}
	if after.StateDetail != nil {
		t.Fatalf("display update set a state_detail: %q", *after.StateDetail)
	}
}

// TestDisplayUpdateRejectedByAgent: ack{ok:false} ⇒ 409 display_update_rejected,
// and the session is left running exactly as it was.
func TestDisplayUpdateRejectedByAgent(t *testing.T) {
	pool := testDB(t)
	srv, authSvc, store, disp := newDisplayServer(t, pool, false)
	disp.ackErr = "display_update_rejected: ring not resizable"
	ctx := context.Background()

	_ = seed(t, pool, 4)
	owner, err := authSvc.Register(ctx, "disprej@test.local", "disprej", "unrelated-pw-12")
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}
	tok := loginTok(t, authSvc, "disprej@test.local", "unrelated-pw-12")
	sid := sessionForUser(t, store, currentSeed(t, pool), owner.ID)

	resp := doJSON(t, http.MethodPatch, srv.URL+"/v1/sessions/"+sid+"/display", tok,
		map[string]any{"ui_scale": 2.0})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("rejected update: got %d want 409", resp.StatusCode)
	}
	if code := errCode(t, resp); code != "display_update_rejected" {
		t.Fatalf("rejected update code: got %s want display_update_rejected", code)
	}
	after, _ := store.Get(ctx, sid)
	if after.State != StateRunning {
		t.Fatalf("rejected update changed state: %s want running", after.State)
	}
	if after.StateDetail != nil {
		t.Fatalf("rejected update set a state_detail: %q", *after.StateDetail)
	}
}

// TestDisplayUpdateUndeliverable covers the OTHER half of the rejection branch:
// SendWithAck returning an error (agent unreachable, or no ack within the
// command timeout) rather than a nack. The contract treats both identically —
// 409 display_update_rejected, session untouched — and before ackSendErr existed
// the fake dispatcher could not reach this path at all.
func TestDisplayUpdateUndeliverable(t *testing.T) {
	pool := testDB(t)
	srv, authSvc, store, disp := newDisplayServer(t, pool, true) // ackOK true: the SEND fails, not the ack
	disp.ackSendErr = errors.New("agent not connected")
	ctx := context.Background()

	_ = seed(t, pool, 4)
	owner, err := authSvc.Register(ctx, "dispundel@test.local", "dispundel", "unrelated-pw-13")
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}
	tok := loginTok(t, authSvc, "dispundel@test.local", "unrelated-pw-13")
	sid := sessionForUser(t, store, currentSeed(t, pool), owner.ID)

	resp := doJSON(t, http.MethodPatch, srv.URL+"/v1/sessions/"+sid+"/display", tok,
		map[string]any{"render_width": 640, "render_height": 360})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("undeliverable update: got %d want 409", resp.StatusCode)
	}
	if code := errCode(t, resp); code != "display_update_rejected" {
		t.Fatalf("undeliverable update code: got %s want display_update_rejected", code)
	}
	after, err := store.Get(ctx, sid)
	if err != nil {
		t.Fatalf("get after undeliverable: %v", err)
	}
	if after.State != StateRunning || after.Width != 1280 || after.Height != 720 {
		t.Fatalf("undeliverable update mutated the row: %dx%d state=%s",
			after.Width, after.Height, after.State)
	}
	if after.StateDetail != nil {
		t.Fatalf("undeliverable update set a state_detail: %q", *after.StateDetail)
	}
}

// TestUpdateDisplayWrapsInvalid pins the sentinel wrapping the handler's 400 arm
// matches on. The handler answers 400 on errors.Is(ErrDisplayInvalid) and 500 on
// anything else, so if UpdateDisplay ever stops wrapping, validation failures
// silently become 500s — and, worse, the reverse regression (a catch-all 400)
// would serve raw store/driver error text to clients.
func TestUpdateDisplayWrapsInvalid(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	ctx := context.Background()

	sess := runningSession(t, store, s)
	_, err := coord.UpdateDisplay(ctx, sess.ID, DisplayUpdate{
		RenderWidth: i32(641), RenderHeight: i32(360)}) // odd width
	if !errors.Is(err, ErrDisplayInvalid) {
		t.Fatalf("odd width: got %v want wrapped ErrDisplayInvalid", err)
	}
	// A validation failure must NOT masquerade as any other sentinel.
	if errors.Is(err, ErrDisplayRejected) || errors.Is(err, ErrDisplayNotRunning) {
		t.Fatalf("validation error matched another sentinel: %v", err)
	}
}

// TestDisplayUpdateStatusContract covers the remaining status codes on one
// fixture: 400 (odd width), 403 (non-owner), 404 (unknown id), 409
// session_not_running.
func TestDisplayUpdateStatusContract(t *testing.T) {
	pool := testDB(t)
	srv, authSvc, store, _ := newDisplayServer(t, pool, true)
	ctx := context.Background()

	_ = seed(t, pool, 4)
	owner, err := authSvc.Register(ctx, "dispown2@test.local", "dispown2", "quasar-fixture-pw-09")
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}
	if _, err := authSvc.Register(ctx, "dispother@test.local", "dispother", "unrelated-pw-14"); err != nil {
		t.Fatalf("register other: %v", err)
	}
	admin, err := authSvc.Register(ctx, "dispadmin@test.local", "dispadmin", "unrelated-pw-15")
	if err != nil {
		t.Fatalf("register admin: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET role='admin' WHERE id=$1`, admin.ID); err != nil {
		t.Fatalf("promote admin: %v", err)
	}

	ownerTok := loginTok(t, authSvc, "dispown2@test.local", "quasar-fixture-pw-09")
	otherTok := loginTok(t, authSvc, "dispother@test.local", "unrelated-pw-14")
	adminTok := loginTok(t, authSvc, "dispadmin@test.local", "unrelated-pw-15")
	s := currentSeed(t, pool)
	sid := sessionForUser(t, store, s, owner.ID)
	url := srv.URL + "/v1/sessions/" + sid + "/display"

	// 400 — odd width (the agent would reject it too; we reject it first).
	resp := doJSON(t, http.MethodPatch, url, ownerTok,
		map[string]any{"render_width": 641, "render_height": 360})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("odd width: got %d want 400", resp.StatusCode)
	}
	if code := errCode(t, resp); code != "validation_failed" {
		t.Fatalf("odd width code: got %s want validation_failed", code)
	}

	// 400 — empty body: at least one field is required.
	resp = doJSON(t, http.MethodPatch, url, ownerTok, map[string]any{})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty body: got %d want 400", resp.StatusCode)
	}
	resp.Body.Close()

	// 403 — a non-owner, non-admin caller on an existing session.
	resp = doJSON(t, http.MethodPatch, url, otherTok, map[string]any{"ui_scale": 1.5})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-owner: got %d want 403", resp.StatusCode)
	}
	if code := errCode(t, resp); code != "forbidden" {
		t.Fatalf("non-owner code: got %s want forbidden", code)
	}

	// 404 — unknown id, for the OWNER-less caller too: the lookup precedes the
	// ownership check so a stranger cannot probe which ids exist.
	absent := srv.URL + "/v1/sessions/00000000-0000-0000-0000-000000000000/display"
	for name, tok := range map[string]string{"owner": ownerTok, "other": otherTok} {
		resp = doJSON(t, http.MethodPatch, absent, tok, map[string]any{"ui_scale": 1.5})
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("unknown id (%s): got %d want 404", name, resp.StatusCode)
		}
		resp.Body.Close()
	}

	// Admin may act on someone else's session (owner-or-admin).
	resp = doJSON(t, http.MethodPatch, url, adminTok, map[string]any{"ui_scale": 1.5})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("admin on other's session: got %d want 202", resp.StatusCode)
	}
	resp.Body.Close()

	// 409 session_not_running — same session, now stopped.
	if _, err := store.Transition(ctx, sid, StateStopping, nil, nil); err != nil {
		t.Fatalf("→ stopping: %v", err)
	}
	resp = doJSON(t, http.MethodPatch, url, ownerTok, map[string]any{"ui_scale": 1.5})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("not running: got %d want 409", resp.StatusCode)
	}
	if code := errCode(t, resp); code != "session_not_running" {
		t.Fatalf("not running code: got %s want session_not_running", code)
	}
}
