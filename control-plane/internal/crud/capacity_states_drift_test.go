package crud

// capacity_states_drift_test.go — ANTI-DRIFT GUARD for the one SQL fragment this
// package is forced to hand-copy.
//
// Host.capacity sums reservations held by active-state sessions. That state set
// is owned by internal/session (resource.go's activeStatesSQL, which the
// scheduler's availability check and GET /v1/hosts/{id}/gpus both derive from),
// and this package CANNOT import it: session imports crud, so the dependency
// only runs one way. The copy in store.go is the result.
//
// A silent divergence would not fail anything — it would make the roll-up and
// the per-GPU view disagree about how full a host is, and an operator would have
// to guess which one is lying. So the copy is compared against the original's
// SOURCE, which is the only handle a test on this side of the import edge has.
//
// Runs without a database: it reads a file.

import (
	"os"
	"regexp"
	"testing"
)

const sessionResourcePath = "../session/resource.go"

var activeStatesDecl = regexp.MustCompile(`(?m)^const activeStatesSQL = ` + "`" + `([^` + "`" + `]*)` + "`")

func TestCapacityActiveStatesMatchesSessionPackage(t *testing.T) {
	src, err := os.ReadFile(sessionResourcePath)
	if err != nil {
		t.Fatalf("read %s: %v — if activeStatesSQL moved, point this guard at its new home "+
			"rather than deleting it", sessionResourcePath, err)
	}
	m := activeStatesDecl.FindSubmatch(src)
	if m == nil {
		t.Fatalf("no `const activeStatesSQL = ...` in %s — the guard cannot find the original; "+
			"re-point it rather than deleting it", sessionResourcePath)
	}
	if got, want := capacityActiveStatesSQL, string(m[1]); got != want {
		t.Errorf("capacityActiveStatesSQL has drifted from internal/session's activeStatesSQL:\n"+
			"  crud    = %s\n  session = %s\n"+
			"Host.capacity and GET /v1/hosts/{id}/gpus must sum the SAME reservations.", got, want)
	}
}
