package agentws

// maxReleaseOutputLen is the wire hop's cut, and it must be the bound the
// column's CHECK enforces (migration 0075): cut shorter and a verdict loses its
// tail for nothing, cut longer and Postgres REFUSES the write, leaving the
// attempt non-terminal until its deadline. Go twin in internal/platform:
// TestApplyOutputLimitMatchesSQL, which pins the same literal from the other
// side.

import (
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/accreleus/quasar/control-plane/migrations"
)

func TestMaxReleaseOutputLenMatchesSQL(t *testing.T) {
	raw, err := migrations.FS.ReadFile("0075_platform_apply.up.sql")
	if err != nil {
		t.Fatalf("read migration 0075: %v", err)
	}
	m := regexp.MustCompile(`CHECK \(octet_length\(output\) <= (\d+)\)`).FindAllStringSubmatch(string(raw), -1)
	if len(m) != 1 {
		t.Fatalf("migration 0075 has %d output-length CHECKs, want exactly 1", len(m))
	}
	want, err := strconv.Atoi(m[0][1])
	if err != nil {
		t.Fatalf("output CHECK bound %q is not a number: %v", m[0][1], err)
	}
	if maxReleaseOutputLen != want {
		t.Errorf("maxReleaseOutputLen = %d, migration 0075's CHECK = %d", maxReleaseOutputLen, want)
	}
}

// The cut is from the front, keeps the end, and lands inside the bound.
func TestValidateReleaseStateBoundsTheOutput(t *testing.T) {
	m := ReleaseStateMsg{
		RequestID: "r", State: "failed",
		Output: strings.Repeat("a", maxReleaseOutputLen) + "the error",
	}
	if !validateReleaseState(&m) {
		t.Fatal("a bounded message was dropped")
	}
	if len(m.Output) != maxReleaseOutputLen {
		t.Errorf("output length = %d, want %d", len(m.Output), maxReleaseOutputLen)
	}
	if !strings.HasSuffix(m.Output, "the error") {
		t.Error("the cut kept the head; the error is at the end")
	}
}
