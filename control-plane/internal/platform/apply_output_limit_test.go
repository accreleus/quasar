package platform

// The 8192 that bounds an attempt's `output` is written out in four places: the
// CHECK in migration 0075, agentws.maxReleaseOutputLen (the wire hop's cut),
// platform.applyOutputLimit (every writer's cut) and updater.OutputTailBytes
// (the tail the updater keeps in the first place). Nothing made them agree, and
// the failure mode of disagreement is silent: Postgres REFUSES an oversized
// output rather than truncating it, so a terminal write is simply lost and the
// attempt hangs until its deadline.
//
// This is the TestTerminalSplitMatchesSQL / TestCertForRungMatchesPickCert
// pattern, and needs no database: the migrations are embedded.

import (
	"regexp"
	"strconv"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/updater"
	"github.com/accreleus/quasar/control-plane/migrations"
)

// outputCheckSQL is the column's CHECK, read out of the migration itself. A
// hand-copied predicate would go on passing after the migration was edited,
// which is the whole thing this test exists to notice.
var outputCheckSQL = regexp.MustCompile(`CHECK \(octet_length\(output\) <= (\d+)\)`)

// sqlOutputLimit is the bound migration 0075 puts on
// platform_apply_attempts.output.
func sqlOutputLimit(t *testing.T) int {
	t.Helper()
	raw, err := migrations.FS.ReadFile("0075_platform_apply.up.sql")
	if err != nil {
		t.Fatalf("read migration 0075: %v", err)
	}
	m := outputCheckSQL.FindAllStringSubmatch(string(raw), -1)
	if len(m) != 1 {
		t.Fatalf("migration 0075 has %d output-length CHECKs, want exactly 1", len(m))
	}
	n, err := strconv.Atoi(m[0][1])
	if err != nil {
		t.Fatalf("output CHECK bound %q is not a number: %v", m[0][1], err)
	}
	return n
}

func TestApplyOutputLimitMatchesSQL(t *testing.T) {
	want := sqlOutputLimit(t)
	if applyOutputLimit != want {
		t.Errorf("applyOutputLimit = %d, migration 0075's CHECK = %d: a write the CHECK refuses is a "+
			"terminal state that never lands", applyOutputLimit, want)
	}
	// The updater's own tail is the same number for the same reason: what it
	// keeps has to fit in the column the relay eventually writes it to.
	if updater.OutputTailBytes != want {
		t.Errorf("updater.OutputTailBytes = %d, migration 0075's CHECK = %d", updater.OutputTailBytes, want)
	}
}
