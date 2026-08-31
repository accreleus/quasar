package session

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
	"github.com/accreleus/quasar/control-plane/internal/migrate"
	"github.com/accreleus/quasar/control-plane/migrations"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// These tests exercise the lifecycle against a real Postgres (the state machine,
// the transactional reserve/release, the reap edge, and the coordinator
// orchestration). They are skipped without TEST_DATABASE_URL — the pure state
// machine in state_test.go runs unconditionally.

func testDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping DB integration test")
	}
	if err := migrate.Run(migrations.FS, dbURL); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		DELETE FROM session_metrics; DELETE FROM user_homes;
		DELETE FROM sessions; DELETE FROM gpus; DELETE FROM hosts;
		DELETE FROM apps; DELETE FROM runtime_presets;
		DELETE FROM auth_tokens; DELETE FROM users;
	`); err != nil {
		pool.Close()
		t.Fatalf("truncate: %v", err)
	}
	// Reset the stream_profile_policy singleton to its migration-0015 seed default
	// (no global default, overrides allowed). The shared test DB (-p 1) can have this
	// row wiped by the auth package's `TRUNCATE users ... CASCADE` (migration 0015 made
	// stream_profile_policy.updated_by a FK to users, so TRUNCATE CASCADE reaches it),
	// or mutated by a profile-policy handler test in another file. The launch path reads
	// this row (GetProfilePolicy) and the legacy/probe tests assume the clean default, so
	// force it back to the canonical seed here, independent of cross-package run order.
	if _, err := pool.Exec(ctx, `
		INSERT INTO stream_profile_policy (id, global_default_profile_id, user_overrides_allowed)
		VALUES (true, NULL, true)
		ON CONFLICT (id) DO UPDATE
		   SET global_default_profile_id = NULL,
		       user_overrides_allowed    = true,
		       updated_by                = NULL
	`); err != nil {
		pool.Close()
		t.Fatalf("seed profile policy: %v", err)
	}
	// Reset stream_profiles.codecs to NULL (the migration-0031 default, which reads
	// back as the in-code ship-dark default). The multi-codec tests mutate this
	// column via UPDATE (enableProfileCodecs / UpdateStreamProfile) and the shared
	// test DB (-p 1) never re-seeds stream_profiles otherwise, so without this a
	// prior test's persisted codec list leaks into a later test's NULL→default
	// assertion. Durable, table-wide reset (protects every test), independent of
	// cross-file run order.
	if _, err := pool.Exec(ctx, `UPDATE stream_profiles SET codecs = NULL`); err != nil {
		pool.Close()
		t.Fatalf("reset stream_profiles codecs: %v", err)
	}
	// Restore the post-0036 baseline: the launch profiles, the rung rows, and the
	// rung membership migration 0036's fan-out produced. UI-P4 tests create,
	// reorder and delete all three, and the shared test DB (-p 1) never re-seeds
	// them otherwise, so a prior test's edit would leak into a later test's
	// assertion. This regenerates them from the LEGACY rows (codec IS NULL), which
	// are exactly migration 0015's seed and are the same input the fan-out read —
	// so the baseline is derived, never a second hardcoded copy of the ladder.
	if _, err := pool.Exec(ctx, `
		DELETE FROM launch_profile_rungs;
		DELETE FROM launch_profiles;
		DELETE FROM stream_profiles WHERE codec IS NOT NULL;

		INSERT INTO stream_profiles (
		    id, display_name, width, height, fps, h264_profile,
		    nominal_bitrate_kbps, min_offer_bandwidth_kbps, recommended_offer_bandwidth_kbps,
		    headroom_factor, abr_floor_kbps, max_startup_rtt_ms, min_decode_height,
		    high_refresh_display, hardware_encoder_required, browser_client, playout0_ms,
		    visibility, sort_order, codec)
		SELECT p.id || '-h264', 'H.264 · ' || p.display_name, p.width, p.height, p.fps, p.h264_profile,
		       p.nominal_bitrate_kbps, p.min_offer_bandwidth_kbps, p.recommended_offer_bandwidth_kbps,
		       p.headroom_factor, p.abr_floor_kbps, p.max_startup_rtt_ms, p.min_decode_height,
		       p.high_refresh_display, p.hardware_encoder_required, p.browser_client, p.playout0_ms,
		       'internal', p.sort_order, 'h264'
		FROM stream_profiles p WHERE p.codec IS NULL;

		INSERT INTO launch_profiles (id, display_name, description, visibility, sort_order)
		SELECT p.id, p.display_name, '', p.visibility, p.sort_order
		FROM stream_profiles p WHERE p.codec IS NULL;

		INSERT INTO launch_profile_rungs (launch_profile_id, stream_profile_id, position)
		SELECT p.id, p.id || '-h264', 1 FROM stream_profiles p WHERE p.codec IS NULL;
	`); err != nil {
		pool.Close()
		t.Fatalf("restore post-0036 launch profile baseline: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

type seedIDs struct {
	userID, appID, hostID, gpuID string
}

// seed inserts a user, an enabled app, an online host and one GPU with the given
// encode-slot capacity (vram is generous). The app needs 1024 MB vram + 1 slot
// per launch (see launchParams).
func seed(t *testing.T, pool *pgxpool.Pool, encodeSlots int) seedIDs {
	return seedCap(t, pool, 16384, encodeSlots)
}

// seedCap is seed with an explicit per-GPU VRAM total, so a test can make VRAM
// (not encode slots) the binding constraint.
func seedCap(t *testing.T, pool *pgxpool.Pool, vramMBTotal, encodeSlots int) seedIDs {
	t.Helper()
	ctx := context.Background()
	var s seedIDs
	must(t, pool.QueryRow(ctx, `INSERT INTO users (email, username, password_hash)
		VALUES ('u@test.local','u','x') RETURNING id::text`).Scan(&s.userID))
	must(t, pool.QueryRow(ctx, `INSERT INTO apps
		(name, default_vram_mb, default_encode_slots, default_width, default_height, default_fps, default_bitrate_kbps)
		VALUES ('app', 1024, 1, 1280, 720, 60, 6000) RETURNING id::text`).Scan(&s.appID))
	must(t, pool.QueryRow(ctx, `INSERT INTO hosts (node_name, status, capacity_detection)
		VALUES ('host-1','online','ok') RETURNING id::text`).Scan(&s.hostID))
	must(t, pool.QueryRow(ctx, `INSERT INTO gpus (host_id, index, vram_mb_total, encode_slots_total)
		VALUES ($1, 0, $2, $3) RETURNING id::text`, s.hostID, vramMBTotal, encodeSlots).Scan(&s.gpuID))
	entitleAll(t, pool, s.appID)
	return s
}

// entitleAll grants the ('all') entitlement that migration 0043 backfills for
// every app that predates it and that POST /v1/apps writes for every app created
// after it (steam-library-discovery Phase 2, §6.4). Fixtures in this package
// INSERT INTO apps directly and so bypass both — an app with no entitlement is
// UNLAUNCHABLE BY DESIGN, and without this every launch fixture would be
// asserting against ErrNotEntitled instead of the thing it means to test.
//
// Deliberately NOT hidden inside a trigger or a schema default: the whole point
// of Phase 2 is that visibility is an explicit row, and a fixture that wants a
// launchable app should have to say so, exactly as the real create path does.
func entitleAll(t *testing.T, pool *pgxpool.Pool, appID string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO entitlements (subject_type, subject_id, app_id, granted_by)
		VALUES ('all', NULL, $1::uuid, 'migration')
		ON CONFLICT DO NOTHING`, appID); err != nil {
		t.Fatalf("entitle app %s: %v", appID, err)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func launchParams(s seedIDs) CreateParams {
	return CreateParams{
		UserID: s.userID, AppID: s.appID,
		Width: 1280, Height: 720, FPS: 60, BitrateKbps: 6000,
		H264Profile:     "constrained-baseline",
		NeedEncodeSlots: 1,
		TokenHash:       "", TokenExpires: time.Now().Add(time.Minute),
	}
}

// TestReserveCapacityRelease exercises the load-bearing reservation path: a GPU
// with one encode slot accepts one session, rejects a second (capacity exhausted,
// no row persisted), and accepts a third only after the first terminal transition
// releases the slot.
func TestReserveCapacityRelease(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 1) // one encode slot
	ctx := context.Background()

	first, err := store.ScheduleAndCreate(ctx, launchParams(s))
	if err != nil {
		t.Fatalf("first launch: %v", err)
	}
	if first.State != StateAssigned {
		t.Fatalf("first state: got %s want assigned", first.State)
	}

	// Second launch must fail — the only slot is reserved (totals fit, availability
	// does not ⇒ capacity_exhausted) — and persist no row.
	before := countSessions(t, pool)
	if _, err := store.ScheduleAndCreate(ctx, launchParams(s)); !errors.Is(err, ErrCapacityExhausted) {
		t.Fatalf("second launch: got %v want ErrCapacityExhausted", err)
	}
	if after := countSessions(t, pool); after != before {
		t.Fatalf("capacity_exhausted persisted a row: %d → %d", before, after)
	}

	// Release the slot by driving the first session terminal.
	if _, err := store.Transition(ctx, first.ID, StateStopped, nil, nil); err != nil {
		t.Fatalf("stop first: %v", err)
	}
	// Now a launch succeeds again.
	if _, err := store.ScheduleAndCreate(ctx, launchParams(s)); err != nil {
		t.Fatalf("third launch after release: %v", err)
	}
}

// TestTransitionForwardAndStamps walks the happy path and checks the timestamps
// the store stamps (started_at on running, ended_at on terminal).
func TestTransitionForwardAndStamps(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	ctx := context.Background()

	sess, err := store.ScheduleAndCreate(ctx, launchParams(s))
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	for _, to := range []State{StateStarting, StateRunning, StateStopping, StateStopped} {
		sess, err = store.Transition(ctx, sess.ID, to, nil, nil)
		if err != nil {
			t.Fatalf("transition → %s: %v", to, err)
		}
		if sess.State != to {
			t.Fatalf("state: got %s want %s", sess.State, to)
		}
	}
	if sess.StartedAt == nil {
		t.Error("started_at not stamped")
	}
	if sess.EndedAt == nil {
		t.Error("ended_at not stamped")
	}
}

// TestTransitionStoppingFailedIsCleanStop covers the SCTP teardown race: an
// operator-terminated session (already `stopping`) that then gets a `failed`
// report (transport error during teardown) must land `stopped`, with the error
// discarded as expected teardown noise and no error_message stamped.
func TestTransitionStoppingFailedIsCleanStop(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	ctx := context.Background()

	sess, err := store.ScheduleAndCreate(ctx, launchParams(s))
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	for _, to := range []State{StateStarting, StateRunning, StateStopping} {
		if sess, err = store.Transition(ctx, sess.ID, to, nil, nil); err != nil {
			t.Fatalf("transition → %s: %v", to, err)
		}
	}
	// Agent reports `failed` with an SCTP teardown error while already stopping.
	sctpErr := "encode pipeline error: SCTP association went into error state"
	sess, err = store.Transition(ctx, sess.ID, StateFailed, nil, &sctpErr)
	if err != nil {
		t.Fatalf("stopping → failed report: %v", err)
	}
	if sess.State != StateStopped {
		t.Fatalf("state: got %s want stopped (clean teardown, not a failure)", sess.State)
	}
	if sess.ErrorMessage != nil {
		t.Errorf("error_message should be unset on a clean stop, got %q", *sess.ErrorMessage)
	}
	if sess.StateDetail != nil {
		t.Errorf("state_detail should suppress expected SCTP teardown noise, got %v", sess.StateDetail)
	}
	if sess.EndedAt == nil {
		t.Error("ended_at not stamped on the coerced stopped transition")
	}
}

func TestTransitionStoppingNonSCTPFailurePreservesDetail(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	ctx := context.Background()

	sess, err := store.ScheduleAndCreate(ctx, launchParams(s))
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	for _, to := range []State{StateStarting, StateRunning, StateStopping} {
		if sess, err = store.Transition(ctx, sess.ID, to, nil, nil); err != nil {
			t.Fatalf("transition -> %s: %v", to, err)
		}
	}
	reason := "encode pipeline error: encoder device was lost"
	sess, err = store.Transition(ctx, sess.ID, StateFailed, nil, &reason)
	if err != nil {
		t.Fatalf("stopping -> failed report: %v", err)
	}
	if sess.State != StateStopped || sess.StateDetail == nil || *sess.StateDetail != reason {
		t.Fatalf("non-SCTP teardown evidence not preserved: state=%s detail=%v", sess.State, sess.StateDetail)
	}
}

// TestTransitionRunningFailedStillFails guards against weakening genuine failure
// detection: a `failed` report from an ACTIVE (running) session is a real failure.
func TestTransitionRunningFailedStillFails(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	ctx := context.Background()

	sess, err := store.ScheduleAndCreate(ctx, launchParams(s))
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	for _, to := range []State{StateStarting, StateRunning} {
		if sess, err = store.Transition(ctx, sess.ID, to, nil, nil); err != nil {
			t.Fatalf("transition → %s: %v", to, err)
		}
	}
	realErr := "encode pipeline error: mid-session SCTP failure"
	sess, err = store.Transition(ctx, sess.ID, StateFailed, nil, &realErr)
	if err != nil {
		t.Fatalf("running → failed: %v", err)
	}
	if sess.State != StateFailed {
		t.Fatalf("state: got %s want failed (genuine mid-session failure)", sess.State)
	}
	if sess.ErrorMessage == nil || *sess.ErrorMessage != realErr {
		t.Errorf("error_message should be stamped on a real failure, got %v", sess.ErrorMessage)
	}
}

// TestTransitionRejectsIllegal confirms the store enforces the machine.
func TestTransitionRejectsIllegal(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	ctx := context.Background()

	sess, _ := store.ScheduleAndCreate(ctx, launchParams(s))
	if _, err := store.Transition(ctx, sess.ID, StateStopped, nil, nil); err != nil {
		t.Fatalf("→ stopped: %v", err)
	}
	// stopped is terminal: any further transition is rejected.
	if _, err := store.Transition(ctx, sess.ID, StateRunning, nil, nil); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("stopped → running: got %v want ErrInvalidTransition", err)
	}
}

// TestReapHostFailsNonTerminal covers schema invariant #3: a host losing its
// agent fails all its non-terminal sessions (and releases their reservations).
func TestReapHostFailsNonTerminal(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	ctx := context.Background()

	sess, _ := store.ScheduleAndCreate(ctx, launchParams(s))
	sess, _ = store.Transition(ctx, sess.ID, StateStarting, nil, nil)
	sess, _ = store.Transition(ctx, sess.ID, StateRunning, nil, nil)

	n, err := store.ReapHost(ctx, s.hostID, "agent lost")
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if n != 1 {
		t.Fatalf("reaped: got %d want 1", n)
	}
	got, _ := store.Get(ctx, sess.ID)
	if got.State != StateFailed {
		t.Fatalf("reaped session state: got %s want failed", got.State)
	}
	if got.ErrorMessage == nil || *got.ErrorMessage != "agent lost" {
		t.Fatalf("reaped session error_message: %v", got.ErrorMessage)
	}
}

// --- coordinator orchestration (with a fake agent dispatcher) ----------------

type fakeDispatcher struct {
	mu         sync.Mutex
	sent       []string // command types, in order
	lastAssign *agentws.SessionAssignCmd
	// lastDisplay records the most recent session_display_update
	// (session-display-update) so a test can assert the wire fields.
	lastDisplay *agentws.SessionDisplayUpdateCmd
	// lastCapture records the most recent session_capture (session-capture) so a
	// test can assert the minted capture_id and the clamped budget/params.
	lastCapture *agentws.SessionCaptureCmd
	ackOK       bool
	ackErr      string
	// ackSendErr, when set, makes SendWithAck fail outright (agent unreachable /
	// no ack within the command timeout) instead of returning a nack. That is a
	// distinct branch from ackOK=false for every caller that treats the two the
	// same, and it is the ONLY way to reach it — the default fake never errors.
	ackSendErr error
	sawAssign  chan struct{}
}

func newFakeDispatcher(ackOK bool) *fakeDispatcher {
	return &fakeDispatcher{ackOK: ackOK, sawAssign: make(chan struct{}, 8)}
}

func (f *fakeDispatcher) Send(string, any) error { return nil }

func (f *fakeDispatcher) SendWithAck(_ context.Context, _ string, _ string, v any) (agentws.AckResult, error) {
	f.mu.Lock()
	switch c := v.(type) {
	case agentws.SessionAssignCmd:
		f.sent = append(f.sent, "assign")
		assign := c
		f.lastAssign = &assign
	case agentws.SessionStartCmd:
		f.sent = append(f.sent, "start")
	case agentws.SessionStopCmd:
		f.sent = append(f.sent, "stop")
	case agentws.SessionSwapAppCmd:
		f.sent = append(f.sent, "swap")
	case agentws.SessionDisplayUpdateCmd:
		f.sent = append(f.sent, "display")
		display := c
		f.lastDisplay = &display
	case agentws.SessionCaptureCmd:
		f.sent = append(f.sent, "capture")
		capture := c
		f.lastCapture = &capture
	}
	sendErr := f.ackSendErr
	f.mu.Unlock()
	f.sawAssign <- struct{}{}
	if sendErr != nil {
		return agentws.AckResult{}, sendErr
	}
	return agentws.AckResult{OK: f.ackOK, Error: f.ackErr}, nil
}

func (f *fakeDispatcher) types() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent...)
}

// lastAssignCmd returns a copy of the most recent SessionAssignCmd seen, or nil.
func (f *fakeDispatcher) lastAssignCmd() *agentws.SessionAssignCmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastAssign
}

// TestCoordinatorHappyPath: launch reserves+assigns, the agent handshake is
// dispatched, and agent callbacks drive running → stopped.
func TestCoordinatorHappyPath(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	disp := newFakeDispatcher(true)
	coord := newTestCoordinator(t, store, disp, testLogger())
	ctx := context.Background()

	res, err := coord.Launch(ctx, s.userID, s.appID, StreamOverride{})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	if res.Session.State != StateAssigned {
		t.Fatalf("launch state: got %s want assigned", res.Session.State)
	}
	if res.SignalingToken == "" {
		t.Fatal("launch returned no signaling token")
	}

	// The assign→start handshake is dispatched asynchronously.
	waitFor(t, func() bool { return len(disp.types()) == 2 })
	if got := disp.types(); got[0] != "assign" || got[1] != "start" {
		t.Fatalf("dispatch order: got %v want [assign start]", got)
	}

	// Agent reports progress.
	coord.AgentState(ctx, s.hostID, agentws.SessionStateMsg{SessionID: res.Session.ID, State: "starting"})
	coord.AgentState(ctx, s.hostID, agentws.SessionStateMsg{SessionID: res.Session.ID, State: "running"})
	got, _ := store.Get(ctx, res.Session.ID)
	if got.State != StateRunning {
		t.Fatalf("after running callback: got %s want running", got.State)
	}

	// Stop → stopping; agent confirms stopped.
	if _, err := coord.Stop(ctx, res.Session.ID, "user_requested"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	coord.AgentState(ctx, s.hostID, agentws.SessionStateMsg{SessionID: res.Session.ID, State: "stopped"})
	got, _ = store.Get(ctx, res.Session.ID)
	if got.State != StateStopped {
		t.Fatalf("after stopped callback: got %s want stopped", got.State)
	}
}

// TestCoordinatorAssignRejected: the agent rejecting assign fails the session
// (and releases its reservation).
func TestCoordinatorAssignRejected(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	disp := newFakeDispatcher(false) // ack ok:false
	disp.ackErr = "cannot satisfy"
	coord := newTestCoordinator(t, store, disp, testLogger())
	ctx := context.Background()

	res, err := coord.Launch(ctx, s.userID, s.appID, StreamOverride{})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	// The async handshake should drive the session to failed.
	waitFor(t, func() bool {
		got, _ := store.Get(ctx, res.Session.ID)
		return got.State == StateFailed
	})
}

// TestCoordinatorHostDisconnected: a running session is reaped to failed.
func TestCoordinatorHostDisconnected(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	disp := newFakeDispatcher(true)
	coord := newTestCoordinator(t, store, disp, testLogger())
	ctx := context.Background()

	res, _ := coord.Launch(ctx, s.userID, s.appID, StreamOverride{})
	waitFor(t, func() bool { return len(disp.types()) == 2 })
	coord.AgentState(ctx, s.hostID, agentws.SessionStateMsg{SessionID: res.Session.ID, State: "running"})

	coord.HostDisconnected(ctx, s.hostID)
	got, _ := store.Get(ctx, res.Session.ID)
	if got.State != StateFailed {
		t.Fatalf("after host disconnect: got %s want failed", got.State)
	}
}

// TestCoordinatorH264ProfileOverride: a per-launch h264_profile override is
// persisted on the session (and reaches session_assign via sess.H264Profile);
// no override falls back to the constrained-baseline floor (P1-11).
func TestCoordinatorH264ProfileOverride(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	disp := newFakeDispatcher(true)
	coord := newTestCoordinator(t, store, disp, testLogger())
	ctx := context.Background()

	// Explicit override → persisted verbatim.
	main := "main"
	res, err := coord.Launch(ctx, s.userID, s.appID, StreamOverride{H264Profile: &main})
	if err != nil {
		t.Fatalf("launch with override: %v", err)
	}
	if res.Session.H264Profile != "main" {
		t.Fatalf("override profile: got %q want main", res.Session.H264Profile)
	}
	got, _ := store.Get(ctx, res.Session.ID)
	if got.H264Profile != "main" {
		t.Fatalf("persisted profile: got %q want main", got.H264Profile)
	}

	// No override → the constrained-baseline floor.
	res2, err := coord.Launch(ctx, s.userID, s.appID, StreamOverride{})
	if err != nil {
		t.Fatalf("launch without override: %v", err)
	}
	if res2.Session.H264Profile != "constrained-baseline" {
		t.Fatalf("default profile: got %q want constrained-baseline", res2.Session.H264Profile)
	}
}

// sctpAssociationError is the verbatim error text three Tower sessions reported on
// 2026-07-25 (48756bd4, 86e7d95d, f5a51f36) when the browser's DataChannel/SCTP
// association died — twice during the first real remote-user WAN test over VPN.
const sctpAssociationError = "encode pipeline error: Could not write to resource. " +
	"(gstsctpenc.c(898): on_sctp_association_state_changed: SCTP association went into error state)"

// TestCoordinatorPeerDisconnectStopsCleanly: the node agent now classifies an SCTP
// association error as a peer disconnect and reports `stopped` with a human
// state_detail (runner.rs PEER_DISCONNECT_DETAIL) instead of `failed` with the raw
// gst text. The row must land terminal-stopped, carry the detail, and stamp NO
// error_message.
func TestCoordinatorPeerDisconnectStopsCleanly(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	disp := newFakeDispatcher(true)
	coord := newTestCoordinator(t, store, disp, testLogger())
	ctx := context.Background()

	res, err := coord.Launch(ctx, s.userID, s.appID, StreamOverride{})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	waitFor(t, func() bool { return len(disp.types()) == 2 })
	coord.AgentState(ctx, s.hostID, agentws.SessionStateMsg{SessionID: res.Session.ID, State: "running"})

	// The agent's peer-disconnect report: stopped + detail, no error.
	coord.AgentState(ctx, s.hostID, agentws.SessionStateMsg{
		SessionID: res.Session.ID,
		State:     "stopped",
		Detail:    "peer disconnected: WebRTC data channel closed",
	})

	got, err := store.Get(ctx, res.Session.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State != StateStopped {
		t.Fatalf("after peer disconnect: got %s want stopped", got.State)
	}
	if got.ErrorMessage != nil && *got.ErrorMessage != "" {
		t.Fatalf("peer disconnect stamped an error_message: %q", *got.ErrorMessage)
	}
	if got.StateDetail == nil || *got.StateDetail != "peer disconnected: WebRTC data channel closed" {
		t.Fatalf("peer disconnect detail: got %v", got.StateDetail)
	}
}

// TestAgentFailedWritesNoProfileHistory: a session that the agent fails — for any
// reason, including the pre-fix SCTP text — must NEVER write a codec/profile
// certification verdict. user_device_profile_history is fed exclusively by browser
// client-health telemetry (EvaluateClientHealth); a pipeline/transport fault is not
// evidence that this device cannot decode this profile or codec, and a stray fail
// row would silently blank the profile (ProfileFailures) or skip the codec
// (CodecFailures) on the user's next launch.
func TestAgentFailedWritesNoProfileHistory(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	disp := newFakeDispatcher(true)
	coord := newTestCoordinator(t, store, disp, testLogger())
	ctx := context.Background()

	res, err := coord.Launch(ctx, s.userID, s.appID, StreamOverride{})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	waitFor(t, func() bool { return len(disp.types()) == 2 })
	coord.AgentState(ctx, s.hostID, agentws.SessionStateMsg{SessionID: res.Session.ID, State: "running"})

	errText := sctpAssociationError
	coord.AgentState(ctx, s.hostID, agentws.SessionStateMsg{
		SessionID: res.Session.ID,
		State:     "failed",
		Error:     &errText,
	})

	got, err := store.Get(ctx, res.Session.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State != StateFailed {
		t.Fatalf("after agent failure: got %s want failed", got.State)
	}
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM user_device_profile_history WHERE user_id = $1::uuid`,
		s.userID).Scan(&n); err != nil {
		t.Fatalf("count history: %v", err)
	}
	if n != 0 {
		t.Fatalf("agent-reported failure wrote %d user_device_profile_history row(s); want 0", n)
	}
}

// --- helpers -----------------------------------------------------------------

func countSessions(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM sessions`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}
