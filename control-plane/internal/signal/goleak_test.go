package signal

import (
	"testing"

	"go.uber.org/goleak"

	"github.com/accreleus/quasar/control-plane/internal/leakcheck"
)

// TestMain fails the package's tests if any goroutine outlives them (#391 D-1).
// The browser signaling handler runs a per-connection reader goroutine that is
// unblocked only by the `done` channel (#148); this is the guard that keeps
// that edge honest.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m, leakcheck.Options()...)
}
