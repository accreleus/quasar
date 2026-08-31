package agentws

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/console"
)

// selfHealEvents is the Events fake for the CM-09 item-2 tests: it records
// LaunchConsoleSession calls. Guarded by its own mutex — once the backoff
// retry timer is involved, LaunchConsoleSession can be called from a
// time.AfterFunc goroutine concurrently with the test goroutine's assertions.
type selfHealEvents struct {
	noopEvents
	mu          sync.Mutex
	launchCount int
	launchErr   error
}

func (e *selfHealEvents) LaunchConsoleSession(context.Context, string, string, string, string, int32, int32, int32) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.launchErr != nil {
		return "", e.launchErr
	}
	e.launchCount++
	return fmt.Sprintf("sess-%d", e.launchCount), nil
}

func (e *selfHealEvents) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.launchCount
}

// ConsoleSessionActive is never consulted by the paths under test here (the
// terminal hook already knows its session ended; the stale-entry check in
// reevalConsole only applies to the capacity path re-checking a session it
// still believes is tracked) but is wired for completeness.
func (e *selfHealEvents) ConsoleSessionActive(context.Context, string) bool { return true }

func selfHealHandler(t *testing.T) (*Handler, *pgxpool.Pool, *selfHealEvents) {
	t.Helper()
	pool := testPool(t)
	ev := &selfHealEvents{}
	h := &Handler{
		log:          slog.Default(),
		events:       ev,
		consoleStore: console.NewStore(pool),
		consoleAuto:  newConsoleAutoState(),
	}
	// Stop any pending backoff retry timer left running by a test so it can't
	// fire after the test (and its pool) has gone away.
	t.Cleanup(func() {
		h.consoleAuto.mu.Lock()
		for _, bo := range h.consoleAuto.backoff {
			if bo.pendingTimer != nil {
				bo.pendingTimer.Stop()
			}
		}
		h.consoleAuto.mu.Unlock()
	})
	return h, pool, ev
}

// seedEligibleConsoleHost seeds a host row and a console_config eligible for
// auto-start (enabled + auto_start_on_display + default_app + default_user
// all set); default_app/default_user are opaque strings inside the JSONB
// config, not FK-checked by console.Store.Upsert, so any UUID-shaped string
// works.
func seedEligibleConsoleHost(t *testing.T, h *Handler, pool *pgxpool.Pool) string {
	t.Helper()
	hostID := seedHost(t, pool)
	cfg := map[string]any{
		"enabled":               true,
		"auto_start_on_display": true,
		"default_app":           "00000000-0000-0000-0000-0000000000aa",
		"default_user":          "00000000-0000-0000-0000-0000000000bb",
	}
	if err := h.consoleStore.Upsert(context.Background(), hostID, cfg, nil); err != nil {
		t.Fatalf("seed console config: %v", err)
	}
	return hostID
}

func trackedSession(h *Handler, hostID string) string {
	h.consoleAuto.mu.Lock()
	defer h.consoleAuto.mu.Unlock()
	return h.consoleAuto.sessions[hostID]
}

// TestConsoleSelfHealRelaunchesTrackedSessionOnTerminal is the spec's primary
// case: a tracked console session reaches terminal state with the display
// still present → relaunch fires exactly once, without waiting for a fresh
// capacity report.
func TestConsoleSelfHealRelaunchesTrackedSessionOnTerminal(t *testing.T) {
	h, pool, ev := selfHealHandler(t)
	hostID := seedEligibleConsoleHost(t, h, pool)
	connectors := []string{"DP-3"}

	h.consoleAuto.mu.Lock()
	h.consoleAuto.lastConnectors[hostID] = connectors
	h.consoleAuto.sessions[hostID] = "sess-crashed"
	h.consoleAuto.mu.Unlock()

	h.ConsoleSessionTerminated(context.Background(), hostID, "sess-crashed")

	if got := ev.count(); got != 1 {
		t.Fatalf("launch count = %d, want 1", got)
	}
	newID := trackedSession(h, hostID)
	if newID == "" {
		t.Fatal("no relaunch tracked after terminal hook")
	}
	if newID == "sess-crashed" {
		t.Fatal("tracked session id unchanged — relaunch did not happen")
	}
}

// TestConsoleSelfHealNonTrackedSessionIsNoOp is the spec's regression case: a
// session terminating that this handler did not auto-start (or a stale
// sessionID no longer matching the tracked one) must not trigger a launch or
// disturb the tracker.
func TestConsoleSelfHealNonTrackedSessionIsNoOp(t *testing.T) {
	h, pool, ev := selfHealHandler(t)
	hostID := seedEligibleConsoleHost(t, h, pool)

	h.consoleAuto.mu.Lock()
	h.consoleAuto.sessions[hostID] = "sess-real"
	h.consoleAuto.mu.Unlock()

	h.ConsoleSessionTerminated(context.Background(), hostID, "sess-other")

	if got := ev.count(); got != 0 {
		t.Fatalf("launch count = %d, want 0 (non-tracked session)", got)
	}
	h.consoleAuto.mu.Lock()
	recordedID, tracked := h.consoleAuto.sessions[hostID]
	h.consoleAuto.mu.Unlock()
	if !tracked || recordedID != "sess-real" {
		t.Fatalf("tracker entry disturbed by a mismatched session id: %q, tracked=%v", recordedID, tracked)
	}
}

// TestConsoleSelfHealBackoffWindowBlocksImmediateRetry: once a fast
// termination has set a backoff window, neither the terminal hook's own
// re-eval nor an immediately-following capacity report may bypass it — a
// hotplug re-send during a crash-loop must not relaunch faster than the
// schedule allows.
func TestConsoleSelfHealBackoffWindowBlocksImmediateRetry(t *testing.T) {
	h, pool, ev := selfHealHandler(t)
	hostID := seedEligibleConsoleHost(t, h, pool)
	connectors := []string{"DP-3"}

	// Initial clean launch (no prior lastLaunchAt recorded — not a crash-loop).
	h.handleConsoleAutoStart(context.Background(), hostID, connectors)
	if got := ev.count(); got != 1 {
		t.Fatalf("initial launch count = %d, want 1", got)
	}
	sessionID := trackedSession(h, hostID)

	// The session crashes immediately (well within the stability window) —
	// this arms a backoff window (and a retry timer) before any relaunch is
	// permitted.
	h.ConsoleSessionTerminated(context.Background(), hostID, sessionID)
	if got := ev.count(); got != 1 {
		t.Fatalf("launch count after fast crash = %d, want still 1 (own re-eval must respect its own new backoff window)", got)
	}

	// A capacity re-send arriving inside the backoff window must not bypass it.
	h.handleConsoleAutoStart(context.Background(), hostID, connectors)
	if got := ev.count(); got != 1 {
		t.Fatalf("capacity re-send bypassed backoff window: launch count = %d", got)
	}
}

// TestConsoleSelfHealCrashLoopGuardStopsAfterMaxRetries drives
// consoleBackoffMaxRetries consecutive fast terminations (each time forcing
// the backoff window open, standing in for "enough real time has passed for
// the next hotplug re-send" — the delay schedule itself, and the timer that
// actually advances it with zero capacity reports, are covered by
// TestConsoleBackoffDelaySchedule and
// TestConsoleSelfHealBackoffTimerAdvancesWithoutCapacityReport) and verifies
// relaunching stops once the guard gives up, then resumes on a fresh capacity
// report.
func TestConsoleSelfHealCrashLoopGuardStopsAfterMaxRetries(t *testing.T) {
	h, pool, ev := selfHealHandler(t)
	hostID := seedEligibleConsoleHost(t, h, pool)
	connectors := []string{"DP-3"}

	for i := 1; i <= consoleBackoffMaxRetries; i++ {
		h.consoleAuto.mu.Lock()
		h.consoleAuto.backoffFor(hostID).nextEligibleAt = time.Time{}
		h.consoleAuto.mu.Unlock()

		h.handleConsoleAutoStart(context.Background(), hostID, connectors)

		sessionID := trackedSession(h, hostID)
		if sessionID == "" {
			t.Fatalf("iteration %d: expected a relaunch to be tracked", i)
		}

		// Crashes immediately — well within the stability window.
		h.ConsoleSessionTerminated(context.Background(), hostID, sessionID)
	}

	if got := ev.count(); got != consoleBackoffMaxRetries {
		t.Fatalf("launch count after crash loop = %d, want %d", got, consoleBackoffMaxRetries)
	}
	h.consoleAuto.mu.Lock()
	gaveUp := h.consoleAuto.backoffFor(hostID).gaveUp
	h.consoleAuto.mu.Unlock()
	if !gaveUp {
		t.Fatalf("expected crash-loop guard to give up after %d consecutive fast terminations", consoleBackoffMaxRetries)
	}

	// A fresh capacity report re-primes the host (with the window forced open,
	// standing in for the passage of time) — the very next relaunch succeeds.
	h.consoleAuto.mu.Lock()
	h.consoleAuto.backoffFor(hostID).nextEligibleAt = time.Time{}
	h.consoleAuto.mu.Unlock()
	h.handleConsoleAutoStart(context.Background(), hostID, connectors)
	if got := ev.count(); got != consoleBackoffMaxRetries+1 {
		t.Fatalf("expected re-prime to relaunch: launch count = %d, want %d", got, consoleBackoffMaxRetries+1)
	}
}

// TestConsoleSelfHealBackoffTimerAdvancesWithoutCapacityReport is the CM-09
// item-2 review's required regression: drives the crash loop using ONLY the
// session-terminal hook (ConsoleSessionTerminated) — zero capacity reports —
// and asserts the backoff schedule still advances and eventually gives up.
// This proves the retry timer (armed by ConsoleSessionTerminated, fired by
// retryConsoleAfterBackoff), not just the capacity-report path, is what makes
// the schedule reachable: a static display never sends another capacity
// report on its own, so without the timer nextEligibleAt would sit in the
// future forever and consecutiveFailures would never advance past 1. The
// backoff schedule is scaled down to millisecond range for the test — the
// real 2s..60s schedule is pinned separately by TestConsoleBackoffDelaySchedule.
func TestConsoleSelfHealBackoffTimerAdvancesWithoutCapacityReport(t *testing.T) {
	origBase, origMax := consoleBackoffBase, consoleBackoffMax
	consoleBackoffBase, consoleBackoffMax = 5*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { consoleBackoffBase, consoleBackoffMax = origBase, origMax })

	h, pool, ev := selfHealHandler(t)
	hostID := seedEligibleConsoleHost(t, h, pool)
	connectors := []string{"DP-3"}

	// Initial launch is a clean start (no prior lastLaunchAt) — not counted as
	// a crash-loop failure.
	h.handleConsoleAutoStart(context.Background(), hostID, connectors)
	if got := ev.count(); got != 1 {
		t.Fatalf("initial launch count = %d, want 1", got)
	}
	sessionID := trackedSession(h, hostID)

	waitForLaunchCount := func(want int) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if ev.count() >= want {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatalf("timed out waiting for launch count %d (stuck at %d) — the backoff-timer retry never fired", want, ev.count())
	}

	// Drive consoleBackoffMaxRetries consecutive fast crashes using ONLY the
	// terminal hook. Each relaunch after the first must come from the retry
	// timer armed by the previous termination — no capacity report is ever
	// sent in this loop.
	for i := 1; i <= consoleBackoffMaxRetries; i++ {
		h.ConsoleSessionTerminated(context.Background(), hostID, sessionID)

		if i == consoleBackoffMaxRetries {
			break // this termination gives up; no further relaunch to wait for
		}
		waitForLaunchCount(i + 1)
		sessionID = trackedSession(h, hostID)
		if sessionID == "" {
			t.Fatalf("iteration %d: timer relaunch did not track a session", i)
		}
	}

	h.consoleAuto.mu.Lock()
	bo := h.consoleAuto.backoffFor(hostID)
	gaveUp := bo.gaveUp
	failures := bo.consecutiveFailures
	h.consoleAuto.mu.Unlock()
	if !gaveUp {
		t.Fatalf("expected give-up after %d consecutive fast terminations (failures=%d)", consoleBackoffMaxRetries, failures)
	}
	if got := ev.count(); got != consoleBackoffMaxRetries {
		t.Fatalf("launch count = %d, want %d", got, consoleBackoffMaxRetries)
	}

	// No further relaunch happens on its own once given up (no timer is armed
	// for a gave-up host).
	time.Sleep(50 * time.Millisecond)
	if got := ev.count(); got != consoleBackoffMaxRetries {
		t.Fatalf("unexpected relaunch after give-up: launch count = %d", got)
	}
}

// TestConsoleBackoffDelaySchedule pins the exponential schedule: 2s, 4s, 8s,
// 16s, 32s, capped at 60s.
func TestConsoleBackoffDelaySchedule(t *testing.T) {
	cases := []struct {
		failures int
		want     time.Duration
	}{
		{1, 2 * time.Second},
		{2, 4 * time.Second},
		{3, 8 * time.Second},
		{4, 16 * time.Second},
		{5, 32 * time.Second},
		{6, 60 * time.Second},
		{7, 60 * time.Second},
	}
	for _, tc := range cases {
		if got := consoleBackoffDelay(tc.failures); got != tc.want {
			t.Errorf("consoleBackoffDelay(%d) = %v, want %v", tc.failures, got, tc.want)
		}
	}
}
