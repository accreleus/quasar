package agentws

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/console"
)

// CM-09 item 3: the level-trigger presence check must key on the connector
// pinned via output_id. seedEligibleConsolePinnedHost mirrors
// seedEligibleConsoleHost (console_selfheal_test.go) with an output_id pin
// added.
func seedEligibleConsolePinnedHost(t *testing.T, h *Handler, pool *pgxpool.Pool, outputID string) string {
	t.Helper()
	hostID := seedHost(t, pool)
	cfg := map[string]any{
		"enabled":               true,
		"auto_start_on_display": true,
		"default_app":           seedConsoleApp(t, pool, true),
		"default_user":          "00000000-0000-0000-0000-0000000000bb",
		"output_id":             outputID,
	}
	if err := h.consoleStore.Upsert(context.Background(), hostID, cfg, nil); err != nil {
		t.Fatalf("seed pinned console config: %v", err)
	}
	return hostID
}

// A pinned connector present in the reported list launches, exactly like the
// "auto" behavior when any connector is present.
func TestConsoleAutoStartLaunchesWhenPinnedConnectorPresent(t *testing.T) {
	h, pool, ev := selfHealHandler(t)
	hostID := seedEligibleConsolePinnedHost(t, h, pool, "card0:DP-4")

	h.handleConsoleAutoStart(context.Background(), hostID, []string{"DP-3", "DP-4"})

	if got := ev.count(); got != 1 {
		t.Fatalf("launch count = %d, want 1 (pinned connector DP-4 present)", got)
	}
}

// A pinned connector NOT in the reported list must not launch — no silent
// fallback to a different connected monitor (fail-loud-by-omission, spec
// item 3 design point 2), even though other connectors are present.
func TestConsoleAutoStartSkipsWhenPinnedConnectorAbsent(t *testing.T) {
	h, pool, ev := selfHealHandler(t)
	hostID := seedEligibleConsolePinnedHost(t, h, pool, "card0:DP-4")

	h.handleConsoleAutoStart(context.Background(), hostID, []string{"DP-3", "HDMI-A-1"})

	if got := ev.count(); got != 0 {
		t.Fatalf("launch count = %d, want 0 (pinned connector DP-4 absent)", got)
	}
	if _, tracked := h.consoleAuto.sessions[hostID]; tracked {
		t.Fatal("no session should be tracked when the pinned connector never launched")
	}
}

// A running pinned-connector session must auto-stop the instant its specific
// connector drops out of the reported list, even while other connectors
// remain present — the auto-stop keys off the same pin as auto-start.
func TestConsoleAutoStopWhenPinnedConnectorGoesAbsent(t *testing.T) {
	h, pool, ev := selfHealHandler(t)
	h.WithConsoleDisconnectGrace(0)
	hostID := seedEligibleConsolePinnedHost(t, h, pool, "card0:DP-4")

	h.handleConsoleAutoStart(context.Background(), hostID, []string{"DP-4"})
	if got := ev.count(); got != 1 {
		t.Fatalf("launch count = %d, want 1", got)
	}

	stopEv := &teardownEvents{active: true}
	h.events = stopEv
	h.handleConsoleAutoStart(context.Background(), hostID, []string{"DP-3"})

	if len(stopEv.stopped) != 1 {
		t.Fatalf("stopped sessions = %v, want exactly one stop", stopEv.stopped)
	}
	if stopEv.stopReasons[0] != "console_display_disconnected" {
		t.Fatalf("stop reason = %q, want console_display_disconnected", stopEv.stopReasons[0])
	}
}

// Unset output_id keeps today's "auto" behavior: any reported connector is
// enough to launch.
func TestConsoleAutoStartUnpinnedKeepsAutoBehavior(t *testing.T) {
	h, pool, ev := selfHealHandler(t)
	hostID := seedEligibleConsoleHost(t, h, pool)

	h.handleConsoleAutoStart(context.Background(), hostID, []string{"HDMI-A-1"})

	if got := ev.count(); got != 1 {
		t.Fatalf("launch count = %d, want 1 (unpinned auto behavior)", got)
	}
}

// connectorPresent unit coverage (pure function, no DB): pinned match/no-match
// and the auto passthrough.
func TestConnectorPresentPinned(t *testing.T) {
	cases := []struct {
		name       string
		connectors []string
		configured string
		want       bool
	}{
		{"auto with connectors", []string{"DP-4"}, "auto", true},
		{"auto with none", nil, "auto", false},
		{"pinned present", []string{"DP-3", "DP-4"}, "DP-4", true},
		{"pinned absent", []string{"DP-3", "HDMI-A-1"}, "DP-4", false},
		{"pinned empty list", nil, "DP-4", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := connectorPresent(tc.connectors, tc.configured); got != tc.want {
				t.Fatalf("connectorPresent(%v, %q) = %v, want %v", tc.connectors, tc.configured, got, tc.want)
			}
		})
	}
}

// console.ConsoleConfig.PinnedConnector plumbing sanity: Resolve() preserves
// output_id (and ignores the retired `connector` key a stale row may still
// carry), and PinnedConnector derives the connector from it.
func TestResolvedConfigPinnedConnectorFromOutputID(t *testing.T) {
	cfg, err := console.Resolve(map[string]any{"output_id": "card0:DP-4", "connector": "HDMI-A-1"})
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.PinnedConnector(); got != "DP-4" {
		t.Fatalf("PinnedConnector() = %q, want DP-4", got)
	}

	unpinned, err := console.Resolve(map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if got := unpinned.PinnedConnector(); got != "auto" {
		t.Fatalf("PinnedConnector() with no output_id = %q, want auto", got)
	}
}

// Without a configured grace an absent connector never stops the session: a monitor
// that drops its link on power-off reads as unplugged, and the desktop handles its
// return.
func TestConsoleAbsentConnectorKeepsTheSessionByDefault(t *testing.T) {
	h, pool, ev := selfHealHandler(t)
	hostID := seedEligibleConsolePinnedHost(t, h, pool, "card0:DP-4")
	h.handleConsoleAutoStart(context.Background(), hostID, []string{"DP-4"})
	if got := ev.count(); got != 1 {
		t.Fatalf("launch count = %d, want 1", got)
	}
	stopEv := &teardownEvents{active: true}
	h.events = stopEv
	h.handleConsoleAutoStart(context.Background(), hostID, []string{})
	h.handleConsoleAutoStart(context.Background(), hostID, []string{})
	if n := len(stopEv.reasons()); n != 0 {
		t.Fatalf("stops = %d, want 0", n)
	}
	h.consoleAuto.mu.Lock()
	_, tracked := h.consoleAuto.sessions[hostID]
	_, noted := h.consoleAuto.absent[hostID]
	h.consoleAuto.mu.Unlock()
	if !tracked || !noted {
		t.Fatalf("tracked=%v noted=%v, want the session kept and the absence noted", tracked, noted)
	}
}

// With a grace, a connector that drops and comes back within it keeps the session
// and disarms the timer; one that stays absent past the grace stops it from the
// timer the first absence armed. Disabling console mode mid-grace stops the session
// for that reason alone.
func TestConsoleAutoStopWaitsOutTheDisconnectGrace(t *testing.T) {
	const grace = 1500 * time.Millisecond
	h, pool, ev := selfHealHandler(t)
	h.WithConsoleDisconnectGrace(grace)
	hostID := seedEligibleConsolePinnedHost(t, h, pool, "card0:DP-4")
	h.handleConsoleAutoStart(context.Background(), hostID, []string{"DP-4"})
	if got := ev.count(); got != 1 {
		t.Fatalf("launch count = %d, want 1", got)
	}
	stopEv := &teardownEvents{active: true}
	h.events = stopEv

	h.handleConsoleAutoStart(context.Background(), hostID, []string{})
	h.handleConsoleAutoStart(context.Background(), hostID, []string{"DP-4"})
	h.consoleAuto.mu.Lock()
	_, tracked := h.consoleAuto.sessions[hostID]
	_, noted := h.consoleAuto.absent[hostID]
	h.consoleAuto.mu.Unlock()
	if n := len(stopEv.reasons()); n != 0 || !tracked || noted {
		t.Fatalf("stops=%d tracked=%v absence=%v, want 0/true/false after the display came back", n, tracked, noted)
	}

	h.handleConsoleAutoStart(context.Background(), hostID, []string{})
	deadline := time.Now().Add(grace + 3*time.Second)
	for len(stopEv.reasons()) == 0 && time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
	}
	if got := stopEv.reasons(); len(got) != 1 || got[0] != "console_display_disconnected" {
		t.Fatalf("stops = %v, want one console_display_disconnected after the grace", got)
	}
	h.consoleAuto.mu.Lock()
	_, noted = h.consoleAuto.absent[hostID]
	h.consoleAuto.mu.Unlock()
	if noted {
		t.Fatal("the absence must be forgotten once the session is stopped")
	}
}

func TestConsoleDisableMidGraceStopsOnceForDisable(t *testing.T) {
	h, pool, ev := selfHealHandler(t)
	h.WithConsoleDisconnectGrace(400 * time.Millisecond)
	hostID := seedEligibleConsolePinnedHost(t, h, pool, "card0:DP-4")
	h.handleConsoleAutoStart(context.Background(), hostID, []string{"DP-4"})
	if got := ev.count(); got != 1 {
		t.Fatalf("launch count = %d, want 1", got)
	}
	stopEv := &teardownEvents{active: true}
	h.events = stopEv
	h.handleConsoleAutoStart(context.Background(), hostID, []string{})
	if err := h.consoleStore.Upsert(context.Background(), hostID, map[string]any{"enabled": false}, nil); err != nil {
		t.Fatalf("disable console: %v", err)
	}
	h.handleConsoleAutoStart(context.Background(), hostID, []string{})
	time.Sleep(700 * time.Millisecond)
	if got := stopEv.reasons(); len(got) != 1 || got[0] != "console_disabled" {
		t.Fatalf("stops = %v, want exactly one console_disabled (the grace timer must be disarmed)", got)
	}
}

// consoleReport decodes a console_capabilities object in the agent's wire shape and feeds
// it through the capacity path's console entry.
func consoleReport(t *testing.T, h *Handler, hostID, raw string) {
	t.Helper()
	var caps console.Capabilities
	if err := json.Unmarshal([]byte(raw), &caps); err != nil {
		t.Fatalf("decode console_capabilities: %v", err)
	}
	h.handleConsoleReport(context.Background(), hostID, caps, true)
}

// With no disconnect grace configured, a monitor the agent read as powered off (its
// output still connected, its connector gone from `connectors`) stops the session once
// powerOffGrace has passed. An unplugged connector (output disconnected) is still kept.
func TestConsolePoweredOffMonitorStopsTheSessionByDefault(t *testing.T) {
	const grace = 1500 * time.Millisecond
	const on = `{"connectors":["DP-4"],"input_devices":[],
		"outputs":[{"id":"card0:DP-4","card":"card0","render_node":null,"connector":"DP-4","connected":true,"active_mode":null,"modes":[]}]}`
	const off = `{"connectors":[],"input_devices":[],
		"outputs":[{"id":"card0:DP-4","card":"card0","render_node":null,"connector":"DP-4","connected":true,"active_mode":null,"modes":[]}]}`
	const unplugged = `{"connectors":[],"input_devices":[],
		"outputs":[{"id":"card0:DP-4","card":"card0","render_node":null,"connector":"DP-4","connected":false,"active_mode":null,"modes":[]}]}`

	for _, pin := range []string{"card0:DP-4", ""} {
		h, pool, ev := selfHealHandler(t)
		h.consoleAuto.powerOffGrace = grace
		var hostID string
		if pin == "" {
			hostID = seedEligibleConsoleHost(t, h, pool)
		} else {
			hostID = seedEligibleConsolePinnedHost(t, h, pool, pin)
		}
		consoleReport(t, h, hostID, on)
		if got := ev.count(); got != 1 {
			t.Fatalf("pin %q: launch count = %d, want 1", pin, got)
		}
		stopEv := &teardownEvents{active: true}
		h.events = stopEv

		consoleReport(t, h, hostID, unplugged)
		time.Sleep(grace + 500*time.Millisecond)
		if got := stopEv.reasons(); len(got) != 0 {
			t.Fatalf("pin %q: stops = %v, want none for an unplugged connector", pin, got)
		}
		consoleReport(t, h, hostID, on)

		consoleReport(t, h, hostID, off)
		time.Sleep(grace / 2)
		if got := stopEv.reasons(); len(got) != 0 {
			t.Fatalf("pin %q: stops = %v before the grace ended", pin, got)
		}
		deadline := time.Now().Add(grace + 3*time.Second)
		for len(stopEv.reasons()) == 0 && time.Now().Before(deadline) {
			time.Sleep(25 * time.Millisecond)
		}
		if got := stopEv.reasons(); len(got) != 1 || got[0] != "console_display_disconnected" {
			t.Fatalf("pin %q: stops = %v, want one console_display_disconnected after the grace", pin, got)
		}
	}
}

// A monitor read off and then on again within the grace (the user is back) keeps the
// session and disarms the timer.
func TestConsolePoweredOffMonitorBackWithinTheGraceKeepsTheSession(t *testing.T) {
	const grace = 800 * time.Millisecond
	h, pool, ev := selfHealHandler(t)
	h.consoleAuto.powerOffGrace = grace
	hostID := seedEligibleConsolePinnedHost(t, h, pool, "card0:DP-4")
	output := `"outputs":[{"id":"card0:DP-4","card":"card0","render_node":null,"connector":"DP-4","connected":true,"active_mode":null,"modes":[]}]`
	consoleReport(t, h, hostID, `{"connectors":["DP-4"],"input_devices":[],`+output+`}`)
	if got := ev.count(); got != 1 {
		t.Fatalf("launch count = %d, want 1", got)
	}
	stopEv := &teardownEvents{active: true}
	h.events = stopEv
	consoleReport(t, h, hostID, `{"connectors":[],"input_devices":[],`+output+`}`)
	consoleReport(t, h, hostID, `{"connectors":["DP-4"],"input_devices":[],`+output+`}`)
	time.Sleep(grace + 500*time.Millisecond)
	if got := stopEv.reasons(); len(got) != 0 {
		t.Fatalf("stops = %v, want none once the monitor is back on", got)
	}
}

// seedLiveConsoleSession inserts a running console session (play, no encode slot, no
// signaling token) on hostID, as one that outlived a control-plane restart leaves it.
func seedLiveConsoleSession(t *testing.T, h *Handler, pool *pgxpool.Pool, hostID string) string {
	t.Helper()
	return seedPlaySession(t, h, pool, hostID, "running", "", "", 0)
}

// seedPlaySession inserts a play session on hostID reserving no encode slot, running the
// host's console default app, created age ago. token non-empty mints it a signaling
// token, as every streamed launch does.
func seedPlaySession(t *testing.T, h *Handler, pool *pgxpool.Pool, hostID, state, token, stopReason string, age time.Duration) string {
	t.Helper()
	ctx := context.Background()
	cfg, err := h.consoleStore.Get(ctx, hostID)
	if err != nil {
		t.Fatalf("read console config: %v", err)
	}
	var userID, sessionID string
	if err := pool.QueryRow(ctx, `INSERT INTO users(email,username,password_hash)
		VALUES (gen_random_uuid()::text || '@test', gen_random_uuid()::text, 'x') RETURNING id::text`).Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO sessions(user_id,app_id,host_id,state,width,height,fps,bitrate_kbps,
		    reserved_encode_slots,signaling_token_hash,stop_reason,created_at)
		VALUES ($1,$2,$3,$4,1920,1080,60,0,0,NULLIF($5,''),NULLIF($6,''),now() - make_interval(secs => $7))
		RETURNING id::text`,
		userID, cfg["default_app"], hostID, state, token, stopReason, age.Seconds()).Scan(&sessionID); err != nil {
		t.Fatalf("seed play session: %v", err)
	}
	return sessionID
}

// Adoption takes only a console session: not a streamed session that reserves 0 slots
// (one launched before migration 0106) even when it is newer, and not another host's.
// A monitor read off then stops the console session alone.
func TestConsoleAdoptsOnlyAConsoleSession(t *testing.T) {
	const grace = 500 * time.Millisecond
	h, pool, ev := selfHealHandler(t)
	h.consoleAuto.powerOffGrace = grace
	hostID := seedEligibleConsolePinnedHost(t, h, pool, "card0:DP-4")
	var otherHost string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO hosts (node_name, status) VALUES ('h2', 'online') RETURNING id::text`).Scan(&otherHost); err != nil {
		t.Fatalf("seed other host: %v", err)
	}
	cfg, err := h.consoleStore.Get(context.Background(), hostID)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.consoleStore.Upsert(context.Background(), otherHost, cfg, nil); err != nil {
		t.Fatal(err)
	}
	consoleID := seedPlaySession(t, h, pool, hostID, "running", "", "", 2*time.Minute)
	streamID := seedPlaySession(t, h, pool, hostID, "running", "tok-"+hostID, "", time.Minute)
	seedPlaySession(t, h, pool, otherHost, "running", "", "", 0)

	consoleReport(t, h, hostID, dp4On)
	if got := ev.count(); got != 0 {
		t.Fatalf("launch count = %d, want 0 beside a live console session", got)
	}
	if got := trackedSession(h, hostID); got != consoleID {
		t.Fatalf("tracked session = %q, want the console session %q (stream %q)", got, consoleID, streamID)
	}

	stopEv := &teardownEvents{active: true}
	h.events = stopEv
	consoleReport(t, h, hostID, dp4Off)
	deadline := time.Now().Add(grace + 3*time.Second)
	for len(stopEv.reasons()) == 0 && time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
	}
	stopEv.mu.Lock()
	defer stopEv.mu.Unlock()
	if len(stopEv.stopped) != 1 || stopEv.stopped[0] != consoleID {
		t.Fatalf("stopped = %v, want only the console session %s", stopEv.stopped, consoleID)
	}
}

// A console session its user is stopping is adopted and not relaunched while it ends;
// once it has ended the user's stop holds auto-start back.
func TestConsoleAdoptedStoppingSessionHoldsAfterTheUserStop(t *testing.T) {
	h, pool, ev := selfHealHandler(t)
	hostID := seedEligibleConsolePinnedHost(t, h, pool, "card0:DP-4")
	id := seedPlaySession(t, h, pool, hostID, "stopping", "", "user_requested", 0)

	consoleReport(t, h, hostID, dp4On)
	if got := ev.count(); got != 0 {
		t.Fatalf("launch count = %d, want 0 while the stopping session is live", got)
	}
	if got := trackedSession(h, hostID); got != id {
		t.Fatalf("tracked session = %q, want the stopping console session %q", got, id)
	}

	if _, err := pool.Exec(context.Background(), `UPDATE sessions SET state='stopped', ended_at=now() WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	ev.mu.Lock()
	ev.userStopped, ev.endedAt, ev.inactive = true, time.Now(), true
	ev.mu.Unlock()
	h.ConsoleSessionTerminated(context.Background(), hostID, id)
	consoleReport(t, h, hostID, dp4On)
	h.consoleAuto.mu.Lock()
	held := h.consoleAuto.userStopped[hostID]
	h.consoleAuto.mu.Unlock()
	if !held || ev.count() != 0 {
		t.Fatalf("held=%v launches=%d, want the user's stop held and no relaunch", held, ev.count())
	}
}

// A console session that outlived a control-plane restart is adopted from the database
// on the first report: a monitor read off then stops it after the grace.
func TestConsolePoweredOffStopsASessionFromBeforeARestart(t *testing.T) {
	const grace = 800 * time.Millisecond
	h, pool, _ := selfHealHandler(t)
	h.consoleAuto.powerOffGrace = grace
	stopEv := &teardownEvents{active: true}
	h.events = stopEv
	hostID := seedEligibleConsolePinnedHost(t, h, pool, "card0:DP-4")
	sessionID := seedLiveConsoleSession(t, h, pool, hostID)

	consoleReport(t, h, hostID, `{"connectors":[],"input_devices":[],
		"outputs":[{"id":"card0:DP-4","card":"card0","render_node":null,"connector":"DP-4","connected":true,"active_mode":null,"modes":[]}]}`)
	deadline := time.Now().Add(grace + 3*time.Second)
	for len(stopEv.reasons()) == 0 && time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
	}
	stopEv.mu.Lock()
	defer stopEv.mu.Unlock()
	if len(stopEv.stopped) != 1 || stopEv.stopped[0] != sessionID || stopEv.stopReasons[0] != "console_display_disconnected" {
		t.Fatalf("stops = %v %v, want %s stopped once with console_display_disconnected", stopEv.stopped, stopEv.stopReasons, sessionID)
	}
}

// With the monitor on, a console session from before a restart is tracked, not
// launched beside.
func TestConsoleReportAdoptsASessionFromBeforeARestart(t *testing.T) {
	h, pool, ev := selfHealHandler(t)
	hostID := seedEligibleConsolePinnedHost(t, h, pool, "card0:DP-4")
	sessionID := seedLiveConsoleSession(t, h, pool, hostID)

	consoleReport(t, h, hostID, `{"connectors":["DP-4"],"input_devices":[],
		"outputs":[{"id":"card0:DP-4","card":"card0","render_node":null,"connector":"DP-4","connected":true,"active_mode":null,"modes":[]}]}`)
	if got := ev.count(); got != 0 {
		t.Fatalf("launch count = %d, want 0 beside a live console session", got)
	}
	if got := trackedSession(h, hostID); got != sessionID {
		t.Fatalf("tracked session = %q, want the live one %q", got, sessionID)
	}
}

// countingEvents counts reevaluations of a tracked session (each asks whether it is
// still active).
type countingEvents struct {
	teardownEvents
	actives atomic.Int32
}

func (e *countingEvents) ConsoleSessionActive(context.Context, string) bool {
	e.actives.Add(1)
	return true
}

// A monitor read off and then unplugged keeps the session when the grace ends (an
// unplug is not a stop), with no timer left and no hot loop of re-evaluations.
func TestConsolePoweredOffThenUnpluggedLeavesNoTimerAndNoHotLoop(t *testing.T) {
	h, pool, ev := selfHealHandler(t)
	hostID := seedEligibleConsolePinnedHost(t, h, pool, "card0:DP-4")
	output := func(connected bool) string {
		return fmt.Sprintf(`"outputs":[{"id":"card0:DP-4","card":"card0","render_node":null,"connector":"DP-4","connected":%t,"active_mode":null,"modes":[]}]`, connected)
	}
	consoleReport(t, h, hostID, `{"connectors":["DP-4"],"input_devices":[],`+output(true)+`}`)
	if got := ev.count(); got != 1 {
		t.Fatalf("launch count = %d, want 1", got)
	}
	cev := &countingEvents{}
	h.events = cev
	t.Cleanup(func() {
		h.consoleAuto.mu.Lock()
		h.clearConsoleAbsenceLocked(hostID)
		h.consoleAuto.mu.Unlock()
	})

	consoleReport(t, h, hostID, `{"connectors":[],"input_devices":[],`+output(true)+`}`)
	consoleReport(t, h, hostID, `{"connectors":[],"input_devices":[],`+output(false)+`}`)
	before := cev.actives.Load()
	h.consoleGraceEnded(hostID) // the power-off grace has ended
	time.Sleep(300 * time.Millisecond)
	if got := cev.actives.Load() - before; got > 2 {
		t.Fatalf("%d reevaluations in 300 ms after the grace: the timer re-arms in a hot loop", got)
	}
	if got := cev.reasons(); len(got) != 0 {
		t.Fatalf("stops = %v, want none for an unplugged connector", got)
	}
	h.consoleAuto.mu.Lock()
	var timer *time.Timer
	if a := h.consoleAuto.absent[hostID]; a != nil {
		timer = a.timer
	}
	h.consoleAuto.mu.Unlock()
	if timer != nil && timer.Stop() {
		t.Fatal("a kept unplugged connector left a grace timer armed")
	}
}

const (
	dp4On        = `{"connectors":["DP-4"],"input_devices":[],"outputs":[{"id":"card0:DP-4","card":"card0","render_node":null,"connector":"DP-4","connected":true,"active_mode":null,"modes":[]}]}`
	dp4Off       = `{"connectors":[],"input_devices":[],"outputs":[{"id":"card0:DP-4","card":"card0","render_node":null,"connector":"DP-4","connected":true,"active_mode":null,"modes":[]}]}`
	dp4Unplugged = `{"connectors":[],"input_devices":[],"outputs":[{"id":"card0:DP-4","card":"card0","render_node":null,"connector":"DP-4","connected":false,"active_mode":null,"modes":[]}]}`
)

// Unplugged time kept with no disconnect grace is not power-off time: a monitor that
// reads off after a long unplug gets the whole powerOffGrace from that report.
func TestConsoleUnpluggedTimeDoesNotCountTowardThePowerOffGrace(t *testing.T) {
	const grace = 800 * time.Millisecond
	h, pool, ev := selfHealHandler(t)
	h.consoleAuto.powerOffGrace = grace
	hostID := seedEligibleConsolePinnedHost(t, h, pool, "card0:DP-4")
	consoleReport(t, h, hostID, dp4On)
	if got := ev.count(); got != 1 {
		t.Fatalf("launch count = %d, want 1", got)
	}
	stopEv := &teardownEvents{active: true}
	h.events = stopEv

	consoleReport(t, h, hostID, dp4Unplugged)
	time.Sleep(grace + 300*time.Millisecond)
	consoleReport(t, h, hostID, dp4Off)
	if got := stopEv.reasons(); len(got) != 0 {
		t.Fatalf("stops = %v on the first off report after an unplug longer than the grace", got)
	}
	time.Sleep(grace / 2)
	if got := stopEv.reasons(); len(got) != 0 {
		t.Fatalf("stops = %v before a fresh grace ended", got)
	}
	deadline := time.Now().Add(grace + 3*time.Second)
	for len(stopEv.reasons()) == 0 && time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
	}
	if got := stopEv.reasons(); len(got) != 1 || got[0] != "console_display_disconnected" {
		t.Fatalf("stops = %v, want one console_display_disconnected after a fresh grace", got)
	}
}

// processConsoleCapacity feeds a console_capabilities object in the agent's wire shape
// through processCapacity on ac, as the agent's connection sends it.
func processConsoleCapacity(t *testing.T, h *Handler, ac *conn, raw string) {
	t.Helper()
	var caps console.Capabilities
	if err := json.Unmarshal([]byte(raw), &caps); err != nil {
		t.Fatal(err)
	}
	if err := h.processCapacity(context.Background(), ac, capacityRaw(t, CapacityMsg{ConsoleCapabilities: &caps})); err != nil {
		t.Fatalf("process capacity: %v", err)
	}
}

// A connection's first report can precede the agent's console configuration, when it
// lists no connectors with the output connected. That never reads as a monitor off; the
// report the agent re-sends after config_update does. Fed in the agent's connect order
// (register, capacity, then capacity again after config_update) through processCapacity.
func TestConsoleFirstReportOfAConnectionIsNeverAMonitorOff(t *testing.T) {
	const grace = 500 * time.Millisecond
	h, pool, ev := selfHealHandler(t)
	h.consoleAuto.powerOffGrace = grace
	h.registry = NewRegistry(slog.Default())
	h.store = &agentStore{pool: pool, isAgentConnected: h.registry.IsConnected}
	hostID := seedEligibleConsolePinnedHost(t, h, pool, "card0:DP-4")
	capacity := func(ac *conn, raw string) { processConsoleCapacity(t, h, ac, raw) }
	first := &conn{hostID: hostID}
	capacity(first, dp4On)
	capacity(first, dp4On)
	if got := ev.count(); got != 1 {
		t.Fatalf("launch count = %d, want 1", got)
	}
	stopEv := &teardownEvents{active: true}
	h.events = stopEv

	// The agent restarted: its first report on the new connection is pre-config.
	second := &conn{hostID: hostID}
	capacity(second, dp4Off)
	time.Sleep(grace + 400*time.Millisecond)
	if got := stopEv.reasons(); len(got) != 0 {
		t.Fatalf("stops = %v, want none from a report made before the agent had its config", got)
	}
	capacity(second, dp4Off)
	deadline := time.Now().Add(grace + 3*time.Second)
	for len(stopEv.reasons()) == 0 && time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
	}
	if got := stopEv.reasons(); len(got) != 1 || got[0] != "console_display_disconnected" {
		t.Fatalf("stops = %v, want one console_display_disconnected once the agent has its config", got)
	}
}

// A reconnect inside a running power-off grace neither stops the session on the new
// connection's first report nor restarts the grace: the timer the off read armed still
// ends it on time.
func TestConsoleReconnectInsideThePowerOffGraceKeepsItsClock(t *testing.T) {
	const grace = 1200 * time.Millisecond
	h, pool, ev := selfHealHandler(t)
	h.consoleAuto.powerOffGrace = grace
	h.registry = NewRegistry(slog.Default())
	h.store = &agentStore{pool: pool, isAgentConnected: h.registry.IsConnected}
	hostID := seedEligibleConsolePinnedHost(t, h, pool, "card0:DP-4")
	first := &conn{hostID: hostID}
	processConsoleCapacity(t, h, first, dp4On)
	processConsoleCapacity(t, h, first, dp4On)
	if got := ev.count(); got != 1 {
		t.Fatalf("launch count = %d, want 1", got)
	}
	stopEv := &teardownEvents{active: true}
	h.events = stopEv
	processConsoleCapacity(t, h, first, dp4Off)
	since := func() (time.Time, bool) {
		h.consoleAuto.mu.Lock()
		defer h.consoleAuto.mu.Unlock()
		a := h.consoleAuto.absent[hostID]
		if a == nil {
			return time.Time{}, false
		}
		return a.since, a.timer != nil
	}
	armedAt, _ := since()
	time.Sleep(grace * 3 / 4)

	second := &conn{hostID: hostID}
	processConsoleCapacity(t, h, second, dp4Off) // first report: before config_update
	if got, armed := since(); got != armedAt || !armed || len(stopEv.reasons()) != 0 {
		t.Fatalf("since=%v armed=%v stops=%v, want the clock from %v still armed and no stop", got, armed, stopEv.reasons(), armedAt)
	}
	processConsoleCapacity(t, h, second, dp4Off)
	if got, _ := since(); got != armedAt {
		t.Fatalf("since = %v after the configured report, want %v kept", got, armedAt)
	}
	deadline := armedAt.Add(grace + 300*time.Millisecond)
	for len(stopEv.reasons()) == 0 && time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
	}
	if got := stopEv.reasons(); len(got) != 1 || got[0] != "console_display_disconnected" {
		t.Fatalf("stops = %v by the original grace's end, want one console_display_disconnected", got)
	}
}
