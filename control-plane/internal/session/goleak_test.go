package session

import (
	"testing"

	"go.uber.org/goleak"

	"github.com/accreleus/quasar/control-plane/internal/leakcheck"
)

// TestMain fails the package's tests if any goroutine outlives them (#391 D-1).
// The coordinator fans out best-effort goroutines (metric prune, home touch,
// health evaluation, drain stops) that nothing joins; this catches one that
// stops being able to exit.
//
// NO IGNORES (#406). The stuck-start watchdog used to need one, because no test
// called coord.Close() and so the watchdogs armed by dispatchAssignStart stayed
// parked at teardown. Tests now build coordinators through newTestCoordinator,
// which registers Close as a t.Cleanup — keep it that way rather than adding an
// ignore back.
//
// Worth stating for the soak: a session that reaches `running` in two seconds
// still leaves its watchdog parked for the whole startToRunningTimeout, so the
// steady-state watchdog population in production is launch_rate x timeout.
// Bounded, but real, and it will show in a goroutine profile taken under churn —
// do not mistake it for a leak.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m, leakcheck.Options()...)
}
