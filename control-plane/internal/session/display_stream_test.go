package session

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
	"github.com/accreleus/quasar/control-plane/internal/auth"
)

// Adaptive external resolution (spec D4/D5): stream_width/stream_height on
// PATCH /v1/sessions/{id}/display, the rung ladder, and the ephemeral
// external-size cache that feeds both validation and stream.external_*.

func bp(v bool) *bool { return &v }

// TestValidateDisplayUpdateStream pins the stream-pair rules, and the
// independent-axes rule (2026-08-16 amendment), against a session LAUNCHED at
// 1920x1080 (rungs 1080/900/720). Render and stream are validated purely
// against the LAUNCH size here — cached/current external state is a
// coordinator-level concern (see TestDisplayRenderIndependentOfExternal),
// validateDisplayUpdate itself never sees it.
func TestValidateDisplayUpdateStream(t *testing.T) {
	sess := Session{Width: 1920, Height: 1080}

	cases := []struct {
		name         string
		upd          DisplayUpdate
		wantErr      bool
		wantErrMatch string
	}{
		{name: "stream to a lower rung", upd: DisplayUpdate{StreamWidth: i32(1280), StreamHeight: i32(720)}},
		{name: "stream to the middle rung", upd: DisplayUpdate{StreamWidth: i32(1600), StreamHeight: i32(900)}},
		{name: "stream back up to the launch rung", upd: DisplayUpdate{StreamWidth: i32(1920), StreamHeight: i32(1080)}},

		// The rung ladder is derived from the LAUNCH size, not from any current
		// external size — stepping back UP is exactly the point.
		{name: "stream above the launch size", upd: DisplayUpdate{StreamWidth: i32(2560), StreamHeight: i32(1440)},
			wantErr: true, wantErrMatch: "rungs"},
		{name: "stream off the ladder", upd: DisplayUpdate{StreamWidth: i32(1366), StreamHeight: i32(768)},
			wantErr: true, wantErrMatch: "rungs"},
		{name: "stream from another family", upd: DisplayUpdate{StreamWidth: i32(1440), StreamHeight: i32(900)},
			wantErr: true, wantErrMatch: "rungs"},
		{name: "stream width without height", upd: DisplayUpdate{StreamWidth: i32(1280)},
			wantErr: true, wantErrMatch: "together"},
		{name: "stream height without width", upd: DisplayUpdate{StreamHeight: i32(720)},
			wantErr: true, wantErrMatch: "together"},

		// Render is bounded ONLY by the session's LAUNCH size — independent of
		// stream/external state entirely (2026-08-16 amendment).
		{name: "render at the launch size", upd: DisplayUpdate{RenderWidth: i32(1920), RenderHeight: i32(1080)}},
		{name: "render above the launch size", upd: DisplayUpdate{RenderWidth: i32(2560), RenderHeight: i32(1440)},
			wantErr: true, wantErrMatch: "exceeds"},
		{name: "render well below launch", upd: DisplayUpdate{RenderWidth: i32(640), RenderHeight: i32(360)}},
		{name: "render + stream raised together", upd: DisplayUpdate{
			RenderWidth: i32(1920), RenderHeight: i32(1080), StreamWidth: i32(1920), StreamHeight: i32(1080)}},
		// Render and stream are independent: a stream step DOWN while render stays
		// at (or near) the launch size is allowed outright — the encoder downsamples
		// the compositor framebuffer, the app never sees a mode change.
		{name: "stream step below render accepted", upd: DisplayUpdate{
			RenderWidth: i32(1920), RenderHeight: i32(1080), StreamWidth: i32(1280), StreamHeight: i32(720)}},
		{name: "stream dropped, render untouched", upd: DisplayUpdate{StreamWidth: i32(1280), StreamHeight: i32(720)}},

		{name: "stream + ui_scale", upd: DisplayUpdate{StreamWidth: i32(1280), StreamHeight: i32(720), UIScale: f64(1.5)}},
		{name: "empty is still empty", upd: DisplayUpdate{}, wantErr: true, wantErrMatch: "at least one of"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateDisplayUpdate(tc.upd, sess)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("validateDisplayUpdate(%+v): want error, got nil", tc.upd)
				}
				if tc.wantErrMatch != "" && !strings.Contains(err.Error(), tc.wantErrMatch) {
					t.Fatalf("error %q does not mention %q", err, tc.wantErrMatch)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateDisplayUpdate(%+v): unexpected error %v", tc.upd, err)
			}
		})
	}
}

// TestValidateDisplayUpdateStreamNamesTheRungs: the 400 has to be actionable —
// a client that guessed wrong must be told what the legal values are.
func TestValidateDisplayUpdateStreamNamesTheRungs(t *testing.T) {
	sess := Session{Width: 1920, Height: 1080}
	err := validateDisplayUpdate(DisplayUpdate{StreamWidth: i32(1366), StreamHeight: i32(768)}, sess)
	if err == nil {
		t.Fatal("want error")
	}
	for _, want := range []string{"1920x1080", "1600x900", "1280x720"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not name rung %s", err, want)
		}
	}
}

// TestDisplayStateCache pins the cache's authority rules: an optimistic set is
// overwritten by a metrics sample; an ABSENT stream pair on a display-aware
// sample means "back at launch"; a sample from an agent that knows nothing about
// external resize changes nothing; forget clears everything.
func TestDisplayStateCache(t *testing.T) {
	d := newDisplayState()

	if _, ok := d.get("s1"); ok {
		t.Fatal("empty cache reported an entry")
	}
	if w, h := d.externalSizeOf("s1", 1920, 1080); w != 1920 || h != 1080 {
		t.Fatalf("unknown session: got %dx%d want the launch size", w, h)
	}

	d.setSize("s1", 1280, 720, 1920, 1080)
	if w, h := d.externalSizeOf("s1", 1920, 1080); w != 1280 || h != 720 {
		t.Fatalf("after optimistic set: got %dx%d want 1280x720", w, h)
	}
	// Review round 1: a manual PATCH ack to a NON-launch size is a PIN, visible
	// immediately — not just from the next metrics sample.
	if st, _ := d.get("s1"); st.Owner != "pinned" {
		t.Fatalf("optimistic set to a non-launch size did not pin: got %q", st.Owner)
	}

	// An old agent's sample (nothing display-aware in it) must not clobber the
	// optimistic value.
	d.observe("s1", nil, nil, nil, "")
	if w, h := d.externalSizeOf("s1", 1920, 1080); w != 1280 || h != 720 {
		t.Fatalf("non-display-aware sample clobbered the cache: %dx%d", w, h)
	}

	// The authoritative readback wins, even when it disagrees.
	d.observe("s1", i32(1600), i32(900), bp(true), "auto")
	if w, h := d.externalSizeOf("s1", 1920, 1080); w != 1600 || h != 900 {
		t.Fatalf("metrics did not override the optimistic value: %dx%d", w, h)
	}
	st, ok := d.get("s1")
	if !ok || st.Supported == nil || !*st.Supported {
		t.Fatalf("supported not recorded: %+v ok=%v", st, ok)
	}
	if st.Owner != "auto" {
		t.Fatalf("owner not recorded: got %q want auto", st.Owner)
	}

	// A later sample can flip owner to "pinned" (a manual PATCH took over).
	d.observe("s1", i32(1600), i32(900), bp(true), "pinned")
	if st, _ := d.get("s1"); st.Owner != "pinned" {
		t.Fatalf("owner did not flip to pinned: got %q", st.Owner)
	}

	// Display-aware sample WITHOUT the pair ⇒ back at launch size. Without this
	// a session stepped 1080 → 720 → 1080 would report 720 forever. Owner clears
	// in lockstep — there is no meaningful owner of the launch size.
	d.observe("s1", nil, nil, bp(true), "")
	if w, h := d.externalSizeOf("s1", 1920, 1080); w != 1920 || h != 1080 {
		t.Fatalf("absent pair did not reset to launch: %dx%d", w, h)
	}
	if st, _ := d.get("s1"); st.HasSize {
		t.Fatal("HasSize still set after an absent pair")
	}
	if st, _ := d.get("s1"); st.Owner != "" {
		t.Fatalf("owner not cleared at launch size: got %q", st.Owner)
	}

	d.forget("s1")
	if _, ok := d.get("s1"); ok {
		t.Fatal("forget left an entry behind")
	}
}

// TestDisplayStateCacheSetSizeOwnership (review round 1): setSize — the
// OPTIMISTIC write on a manual PATCH ack — must set Owner itself, not leave it
// to the next metrics sample. Before the fix, pinning after a ladder-owned
// step left `Owner="auto"` visible on a GET until the next sample landed.
func TestDisplayStateCacheSetSizeOwnership(t *testing.T) {
	d := newDisplayState()

	// The ladder had already stepped this session down and reported it.
	d.observe("s1", i32(1600), i32(900), bp(true), "auto")
	if st, _ := d.get("s1"); st.Owner != "auto" {
		t.Fatalf("setup: want auto, got %q", st.Owner)
	}

	// A manual PATCH ack to a DIFFERENT non-launch size must flip to pinned
	// IMMEDIATELY — this is the optimistic write, no metrics sample involved.
	d.setSize("s1", 1280, 720, 1920, 1080)
	if st, _ := d.get("s1"); st.Owner != "pinned" {
		t.Fatalf("manual ack over a ladder-owned size did not pin: got %q", st.Owner)
	}

	// A manual PATCH ack back to the LAUNCH size is the release — Owner clears
	// to "" immediately, same as an absent metrics sample would.
	d.setSize("s1", 1920, 1080, 1920, 1080)
	if st, _ := d.get("s1"); st.Owner != "" {
		t.Fatalf("manual ack to the launch size did not release: got %q", st.Owner)
	}
	if w, h := d.externalSizeOf("s1", 1920, 1080); w != 1920 || h != 1080 {
		t.Fatalf("released size: got %dx%d want the launch size", w, h)
	}
}

// TestBuildAgentMetricsStreamPassthrough: the three new keys reach the metrics
// JSONB verbatim, and are omitted (not zeroed) when the agent does not send them.
func TestBuildAgentMetricsStreamPassthrough(t *testing.T) {
	raw := buildAgentMetrics(agentws.SessionMetricsMsg{
		SessionID:               "s1",
		StreamWidth:             i32(1280),
		StreamHeight:            i32(720),
		ExternalResizeSupported: bp(true),
	})
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("unmarshal metrics: %v", err)
	}
	if obj["stream_width"] != float64(1280) || obj["stream_height"] != float64(720) {
		t.Fatalf("stream dims not passed through: %v", obj)
	}
	if obj["external_resize_supported"] != true {
		t.Fatalf("external_resize_supported not passed through: %v", obj)
	}

	// supported=false must survive as an explicit false, not be dropped as a zero.
	raw = buildAgentMetrics(agentws.SessionMetricsMsg{SessionID: "s1", ExternalResizeSupported: bp(false)})
	obj = nil
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("unmarshal metrics: %v", err)
	}
	if v, ok := obj["external_resize_supported"]; !ok || v != false {
		t.Fatalf("explicit false dropped: %v", obj)
	}
	if _, ok := obj["stream_width"]; ok {
		t.Fatalf("absent stream_width was emitted: %v", obj)
	}

	// An agent that knows nothing about any of this emits none of the keys.
	raw = buildAgentMetrics(agentws.SessionMetricsMsg{SessionID: "s1", FPS: f64(60)})
	obj = nil
	_ = json.Unmarshal(raw, &obj)
	for _, k := range []string{"stream_width", "stream_height", "external_resize_supported"} {
		if _, ok := obj[k]; ok {
			t.Fatalf("key %s emitted for a pre-amendment sample: %v", k, obj)
		}
	}
}

// --- DB-backed HTTP tests ----------------------------------------------------

// newStreamDisplayServer is newDisplayServer plus the coordinator, which these
// tests need in order to feed metrics samples in (the cache's real input path).
func newStreamDisplayServer(t *testing.T, pool *pgxpool.Pool, ackOK bool) (*httptest.Server, *auth.Service, *Store, *fakeDispatcher, *Coordinator) {
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
	return srv, authSvc, store, disp, coord
}

// session1080ForUser creates a RUNNING session launched at 1920x1080, so the rung
// ladder has something to step down. (The shared helper's 1280x720 is its own
// floor — a 720p session has exactly one rung.)
func session1080ForUser(t *testing.T, store *Store, s seedIDs, userID string) string {
	t.Helper()
	ctx := context.Background()
	p := launchParams(s)
	p.UserID = userID
	p.Width, p.Height = 1920, 1080
	sess, err := store.ScheduleAndCreate(ctx, p)
	if err != nil {
		t.Fatalf("create 1080p session: %v", err)
	}
	if _, err := store.Transition(ctx, sess.ID, StateStarting, nil, nil); err != nil {
		t.Fatalf("transition starting: %v", err)
	}
	if _, err := store.Transition(ctx, sess.ID, StateRunning, nil, nil); err != nil {
		t.Fatalf("transition running: %v", err)
	}
	return sess.ID
}

func hostOf(t *testing.T, store *Store, sid string) string {
	t.Helper()
	sess, err := store.Get(context.Background(), sid)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if sess.HostID == nil {
		t.Fatal("session has no host")
	}
	return *sess.HostID
}

// TestDisplayUpdateStreamAccepted: a rung step is relayed with the contract's
// field names, answered 202, and persists nothing — the row keeps the LAUNCH
// size, which is what the ladder is derived from.
func TestDisplayUpdateStreamAccepted(t *testing.T) {
	pool := testDB(t)
	srv, authSvc, store, disp, _ := newStreamDisplayServer(t, pool, true)
	ctx := context.Background()

	_ = seed(t, pool, 4)
	owner, err := authSvc.Register(ctx, "strmowner@test.local", "strmowner", "unrelated-pw-03")
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}
	tok := loginTok(t, authSvc, "strmowner@test.local", "unrelated-pw-03")
	sid := session1080ForUser(t, store, currentSeed(t, pool), owner.ID)

	resp := doJSON(t, http.MethodPatch, srv.URL+"/v1/sessions/"+sid+"/display", tok,
		map[string]any{"stream_width": 1280, "stream_height": 720})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("PATCH stream: got %d want 202 (code=%s)", resp.StatusCode, errCode(t, resp))
	}
	got := decodeSessionResp(t, resp)
	if got.Stream.Width != 1920 || got.Stream.Height != 1080 {
		t.Fatalf("stream width/height moved: got %dx%d want the launch 1920x1080",
			got.Stream.Width, got.Stream.Height)
	}
	// The optimistic cache is already reflected in the 202 body.
	if got.Stream.ExternalWidth == nil || *got.Stream.ExternalWidth != 1280 ||
		got.Stream.ExternalHeight == nil || *got.Stream.ExternalHeight != 720 {
		t.Fatalf("external_* not reflected after the ack: %+v", got.Stream)
	}

	cmd := disp.lastDisplay
	if cmd == nil {
		t.Fatal("no session_display_update dispatched")
	}
	if cmd.StreamWidth == nil || *cmd.StreamWidth != 1280 ||
		cmd.StreamHeight == nil || *cmd.StreamHeight != 720 {
		t.Fatalf("dispatched stream dims: %+v", cmd)
	}
	if cmd.RenderWidth != nil || cmd.RenderHeight != nil || cmd.UIScale != nil {
		t.Fatalf("stream-only request carried render/scale fields: %+v", cmd)
	}

	after, err := store.Get(ctx, sid)
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if after.Width != 1920 || after.Height != 1080 || after.State != StateRunning {
		t.Fatalf("stream update mutated the row: %dx%d state=%s", after.Width, after.Height, after.State)
	}
}

// TestDisplayUpdateStreamOffLadder: a size that is not a rung is a 400 that names
// the ladder, and nothing is dispatched.
func TestDisplayUpdateStreamOffLadder(t *testing.T) {
	pool := testDB(t)
	srv, authSvc, store, disp, _ := newStreamDisplayServer(t, pool, true)
	ctx := context.Background()

	_ = seed(t, pool, 4)
	owner, err := authSvc.Register(ctx, "strmbad@test.local", "strmbad", "unrelated-pw-04")
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}
	tok := loginTok(t, authSvc, "strmbad@test.local", "unrelated-pw-04")
	sid := session1080ForUser(t, store, currentSeed(t, pool), owner.ID)

	resp := doJSON(t, http.MethodPatch, srv.URL+"/v1/sessions/"+sid+"/display", tok,
		map[string]any{"stream_width": 1366, "stream_height": 768})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("off-ladder stream: got %d want 400", resp.StatusCode)
	}
	if code := errCode(t, resp); code != "validation_failed" {
		t.Fatalf("off-ladder code: got %s want validation_failed", code)
	}
	if disp.lastDisplay != nil {
		t.Fatalf("an invalid request was dispatched: %+v", disp.lastDisplay)
	}
}

// TestSessionRespRungsAndExternal: GET always carries the ladder, and reports
// external_* only once a metrics sample says the encoded size has moved.
func TestSessionRespRungsAndExternal(t *testing.T) {
	pool := testDB(t)
	srv, authSvc, store, _, coord := newStreamDisplayServer(t, pool, true)
	ctx := context.Background()

	_ = seed(t, pool, 4)
	owner, err := authSvc.Register(ctx, "rungs@test.local", "rungsuser", "unrelated-pw-05")
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}
	tok := loginTok(t, authSvc, "rungs@test.local", "unrelated-pw-05")
	sid := session1080ForUser(t, store, currentSeed(t, pool), owner.ID)
	host := hostOf(t, store, sid)

	get := func() streamResp {
		resp := doJSON(t, http.MethodGet, srv.URL+"/v1/sessions/"+sid, tok, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET session: %d", resp.StatusCode)
		}
		return decodeSessionResp(t, resp).Stream
	}

	st := get()
	want := [][2]int32{{1920, 1080}, {1600, 900}, {1280, 720}}
	if len(st.Rungs) != len(want) {
		t.Fatalf("rungs: got %v want %v", st.Rungs, want)
	}
	for i := range want {
		if st.Rungs[i] != want[i] {
			t.Fatalf("rungs: got %v want %v", st.Rungs, want)
		}
	}
	if st.ExternalWidth != nil || st.ExternalHeight != nil {
		t.Fatalf("external_* present before any step: %+v", st)
	}
	if st.ExternalResizeSupported != nil {
		t.Fatalf("external_resize_supported present before any sample: %+v", st)
	}

	if st.ExternalOwner != "" {
		t.Fatalf("external_owner present before any step: %+v", st)
	}

	// A display-aware sample reporting a stepped size, ladder-owned.
	coord.AgentMetrics(ctx, host, agentws.SessionMetricsMsg{
		Type: "session_metrics", SessionID: sid, TsUnixMs: time.Now().UnixMilli(),
		StreamWidth: i32(1280), StreamHeight: i32(720), ExternalResizeSupported: bp(true),
		ExternalOwner: "auto",
	})
	st = get()
	if st.ExternalWidth == nil || *st.ExternalWidth != 1280 ||
		st.ExternalHeight == nil || *st.ExternalHeight != 720 {
		t.Fatalf("external_* after a stepped sample: %+v", st)
	}
	if st.ExternalResizeSupported == nil || !*st.ExternalResizeSupported {
		t.Fatalf("external_resize_supported after a sample: %+v", st)
	}
	if st.Width != 1920 || st.Height != 1080 {
		t.Fatalf("width/height moved: %dx%d", st.Width, st.Height)
	}
	if st.ExternalOwner != "auto" {
		t.Fatalf("external_owner after a ladder-owned sample: got %q want auto", st.ExternalOwner)
	}

	// A later sample reports the same size but manually pinned — the resource
	// must flip too.
	coord.AgentMetrics(ctx, host, agentws.SessionMetricsMsg{
		Type: "session_metrics", SessionID: sid, TsUnixMs: time.Now().UnixMilli(),
		StreamWidth: i32(1280), StreamHeight: i32(720), ExternalResizeSupported: bp(true),
		ExternalOwner: "pinned",
	})
	st = get()
	if st.ExternalOwner != "pinned" {
		t.Fatalf("external_owner did not flip to pinned: got %q", st.ExternalOwner)
	}

	// Back at the launch size: the pair is omitted from the SAMPLE (that is the
	// wire's convention) but the resource still reports it — as the launch size.
	// Absence on the resource means "unknown", so a client that holds its
	// last-acked value has to be able to see the revert here. external_owner has
	// no meaning at the launch size and must go absent too.
	coord.AgentMetrics(ctx, host, agentws.SessionMetricsMsg{
		Type: "session_metrics", SessionID: sid, TsUnixMs: time.Now().UnixMilli(),
		ExternalResizeSupported: bp(true),
	})
	st = get()
	if st.ExternalWidth == nil || *st.ExternalWidth != 1920 ||
		st.ExternalHeight == nil || *st.ExternalHeight != 1080 {
		t.Fatalf("external_* after returning to launch: %+v want 1920x1080", st)
	}
	if st.ExternalOwner != "" {
		t.Fatalf("external_owner not cleared at launch size: got %q", st.ExternalOwner)
	}
}

// TestDisplayUpdateStreamManualPinOverridesLadderImmediately (review round 1):
// a ladder-owned ("auto") session that then gets a manual PATCH must show
// "pinned" on the VERY NEXT GET — the 202's optimistic cache write, not a wait
// for the next metrics sample. And a manual PATCH back to the launch size must
// show external_owner absent immediately, the same way.
func TestDisplayUpdateStreamManualPinOverridesLadderImmediately(t *testing.T) {
	pool := testDB(t)
	srv, authSvc, store, _, coord := newStreamDisplayServer(t, pool, true)
	ctx := context.Background()

	_ = seed(t, pool, 4)
	owner, err := authSvc.Register(ctx, "pinnow@test.local", "pinnowuser", "unrelated-pw-06")
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}
	tok := loginTok(t, authSvc, "pinnow@test.local", "unrelated-pw-06")
	sid := session1080ForUser(t, store, currentSeed(t, pool), owner.ID)
	host := hostOf(t, store, sid)

	get := func() streamResp {
		resp := doJSON(t, http.MethodGet, srv.URL+"/v1/sessions/"+sid, tok, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET session: %d", resp.StatusCode)
		}
		return decodeSessionResp(t, resp).Stream
	}

	// The ladder stepped the session down and reported ownership.
	coord.AgentMetrics(ctx, host, agentws.SessionMetricsMsg{
		Type: "session_metrics", SessionID: sid, TsUnixMs: time.Now().UnixMilli(),
		StreamWidth: i32(1280), StreamHeight: i32(720), ExternalResizeSupported: bp(true),
		ExternalOwner: "auto",
	})
	if st := get(); st.ExternalOwner != "auto" {
		t.Fatalf("setup: want auto, got %q", st.ExternalOwner)
	}

	// A human PATCHes to a different rung. NO metrics sample follows this —
	// the very next GET must already read "pinned" off the 202's optimistic
	// write, not the stale "auto" from the last sample.
	resp := doJSON(t, http.MethodPatch, srv.URL+"/v1/sessions/"+sid+"/display", tok,
		map[string]any{"stream_width": 1600, "stream_height": 900})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("PATCH stream: got %d want 202 (code=%s)", resp.StatusCode, errCode(t, resp))
	}
	resp.Body.Close()
	st := get()
	if st.ExternalOwner != "pinned" {
		t.Fatalf("manual PATCH over a ladder-owned session did not pin immediately: got %q", st.ExternalOwner)
	}
	if st.ExternalWidth == nil || *st.ExternalWidth != 1600 || st.ExternalHeight == nil || *st.ExternalHeight != 900 {
		t.Fatalf("external_* after the manual pin: %+v", st)
	}

	// PATCHing back to the LAUNCH size is the release (control-api.md §Pin /
	// release semantics) — external_owner must go ABSENT on the very next GET,
	// again with no metrics sample in between.
	resp = doJSON(t, http.MethodPatch, srv.URL+"/v1/sessions/"+sid+"/display", tok,
		map[string]any{"stream_width": 1920, "stream_height": 1080})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("PATCH stream (release): got %d want 202 (code=%s)", resp.StatusCode, errCode(t, resp))
	}
	resp.Body.Close()
	st = get()
	if st.ExternalOwner != "" {
		t.Fatalf("manual PATCH to the launch size did not release immediately: got %q", st.ExternalOwner)
	}
	if st.ExternalWidth == nil || *st.ExternalWidth != 1920 || st.ExternalHeight == nil || *st.ExternalHeight != 1080 {
		t.Fatalf("external_* after the release: %+v", st)
	}
}

// TestDisplayUpdateStreamUnsupported: once the agent has reported the encoder
// cannot resize live, a stream request is refused 409 external_resize_unsupported
// BEFORE dispatch — while render/ui_scale on the same session still work.
func TestDisplayUpdateStreamUnsupported(t *testing.T) {
	pool := testDB(t)
	srv, authSvc, store, disp, coord := newStreamDisplayServer(t, pool, true)
	ctx := context.Background()

	_ = seed(t, pool, 4)
	owner, err := authSvc.Register(ctx, "nostretch@test.local", "nostretch", "unrelated-pw-07")
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}
	tok := loginTok(t, authSvc, "nostretch@test.local", "unrelated-pw-07")
	sid := session1080ForUser(t, store, currentSeed(t, pool), owner.ID)
	host := hostOf(t, store, sid)

	// Unknown support is PERMISSIVE: before any sample, the request goes through.
	resp := doJSON(t, http.MethodPatch, srv.URL+"/v1/sessions/"+sid+"/display", tok,
		map[string]any{"stream_width": 1600, "stream_height": 900})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("unknown support: got %d want 202 (code=%s)", resp.StatusCode, errCode(t, resp))
	}
	resp.Body.Close()

	coord.AgentMetrics(ctx, host, agentws.SessionMetricsMsg{
		Type: "session_metrics", SessionID: sid, TsUnixMs: time.Now().UnixMilli(),
		ExternalResizeSupported: bp(false),
	})

	disp.lastDisplay = nil
	resp = doJSON(t, http.MethodPatch, srv.URL+"/v1/sessions/"+sid+"/display", tok,
		map[string]any{"stream_width": 1280, "stream_height": 720})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("unsupported stream resize: got %d want 409", resp.StatusCode)
	}
	if code := errCode(t, resp); code != "external_resize_unsupported" {
		t.Fatalf("unsupported code: got %s want external_resize_unsupported", code)
	}
	if disp.lastDisplay != nil {
		t.Fatalf("a refused request was dispatched anyway: %+v", disp.lastDisplay)
	}

	// The refusal is scoped to the ENCODED size; the internal render size is a
	// different mechanism and stays available.
	resp = doJSON(t, http.MethodPatch, srv.URL+"/v1/sessions/"+sid+"/display", tok,
		map[string]any{"render_width": 1280, "render_height": 720})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("render update on an unsupported host: got %d want 202 (code=%s)",
			resp.StatusCode, errCode(t, resp))
	}
	resp.Body.Close()
}

// TestDisplayRenderIndependentOfExternal (2026-08-16 amendment): render and
// external/stream size are independent axes. After the stream has been stepped
// down, a render request ABOVE the cached external size — even up to the full
// launch size — is still accepted: the render bound is the session's LAUNCH
// size only, never the current/cached external size. This replaces the old
// "internal ≤ external" clamp.
func TestDisplayRenderIndependentOfExternal(t *testing.T) {
	pool := testDB(t)
	srv, authSvc, store, _, coord := newStreamDisplayServer(t, pool, true)
	ctx := context.Background()

	_ = seed(t, pool, 4)
	owner, err := authSvc.Register(ctx, "bound@test.local", "bounduser", "unrelated-pw-08")
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}
	tok := loginTok(t, authSvc, "bound@test.local", "unrelated-pw-08")
	sid := session1080ForUser(t, store, currentSeed(t, pool), owner.ID)
	host := hostOf(t, store, sid)

	// Full-size render is fine while the session is encoding at its launch size.
	resp := doJSON(t, http.MethodPatch, srv.URL+"/v1/sessions/"+sid+"/display", tok,
		map[string]any{"render_width": 1920, "render_height": 1080})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("render at launch size: got %d want 202 (code=%s)", resp.StatusCode, errCode(t, resp))
	}
	resp.Body.Close()

	// Step the cached EXTERNAL size down to 720p (e.g. congestion).
	coord.AgentMetrics(ctx, host, agentws.SessionMetricsMsg{
		Type: "session_metrics", SessionID: sid, TsUnixMs: time.Now().UnixMilli(),
		StreamWidth: i32(1280), StreamHeight: i32(720), ExternalResizeSupported: bp(true),
	})

	// Render UP to 1600x900 — between the cached external (720p) and the launch
	// size (1080p) — is accepted: render is never bounded by external.
	resp = doJSON(t, http.MethodPatch, srv.URL+"/v1/sessions/"+sid+"/display", tok,
		map[string]any{"render_width": 1600, "render_height": 900})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("render between cached external and launch: got %d want 202 (code=%s)",
			resp.StatusCode, errCode(t, resp))
	}
	resp.Body.Close()

	// Render at the full launch size, still with a 720p cached external, is ALSO
	// accepted — this is the case the old clamp forbade.
	resp = doJSON(t, http.MethodPatch, srv.URL+"/v1/sessions/"+sid+"/display", tok,
		map[string]any{"render_width": 1920, "render_height": 1080})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("render at launch size while external is lower: got %d want 202 (code=%s)",
			resp.StatusCode, errCode(t, resp))
	}
	resp.Body.Close()

	// A stream step BELOW the current render size (now 1080p) is accepted too —
	// the encoder downsamples the render framebuffer; the app never sees a
	// mode change.
	resp = doJSON(t, http.MethodPatch, srv.URL+"/v1/sessions/"+sid+"/display", tok,
		map[string]any{"stream_width": 1280, "stream_height": 720})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("stream step below render: got %d want 202 (code=%s)", resp.StatusCode, errCode(t, resp))
	}
	resp.Body.Close()

	// …and raising both together in one call is still accepted.
	resp = doJSON(t, http.MethodPatch, srv.URL+"/v1/sessions/"+sid+"/display", tok,
		map[string]any{"render_width": 1920, "render_height": 1080, "stream_width": 1920, "stream_height": 1080})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("render+stream raised together: got %d want 202 (code=%s)", resp.StatusCode, errCode(t, resp))
	}
	resp.Body.Close()
}
