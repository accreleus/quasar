package agentws

import (
	"testing"

	"go.uber.org/goleak"

	"github.com/accreleus/quasar/control-plane/internal/leakcheck"
)

// TestMain fails the package's tests if any goroutine outlives them (#391 D-1).
// agentws is the densest concurrency in the control plane — a per-connection
// writer goroutine, two drain queues, and the console crash-loop backoff timers
// — so a leak here is exactly the class this guard exists to stop returning.
//
// NO IGNORES (#401/#406). Both drain queues can now be stopped and every test
// that builds a Handler stops it, so the per-connection writer, the console
// backoff timers and both queues are all live under this guard. Adding an
// ignore back for one of our own goroutines is never the fix — an owner that
// cannot stop its goroutine is the defect this guard exists to catch.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m, leakcheck.Options()...)
}
