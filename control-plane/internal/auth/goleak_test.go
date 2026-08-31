package auth

import (
	"testing"

	"go.uber.org/goleak"

	"github.com/accreleus/quasar/control-plane/internal/leakcheck"
)

// TestMain fails the package's tests if any goroutine outlives them (#391 D-1).
// auth owns the token janitor (Service.StartTokenJanitor), a ctx-scoped ticker
// loop — the shape most likely to be started in a test and never cancelled.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m, leakcheck.Options()...)
}
