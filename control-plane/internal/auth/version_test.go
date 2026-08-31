package auth

import (
	"fmt"
	"testing"
)

func TestDecideClientVersion(t *testing.T) {
	cases := []struct {
		name          string
		clientVersion string
		minVersion    string
		want          versionDecision
	}{
		{"no floor, no version — legacy permissive", "", "", versionProceed},
		{"no floor, native version — permissive", "1.0.0", "", versionProceed},
		{"floor set, legacy client (no version) — additive baseline", "", "1.0.0", versionProceed},
		{"below floor — gated", "0.9.0", "1.0.0", versionGate},
		{"at floor — proceeds", "1.0.0", "1.0.0", versionProceed},
		{"above floor (latest soft-warn is client-side) — proceeds", "1.2.0", "1.0.0", versionProceed},
		{"patch below floor — gated", "1.0.0", "1.0.1", versionGate},
		{"malformed client version with floor — gated (unprovable)", "not-a-version", "1.0.0", versionGate},
		{"overflowing client version with floor — gated (unparseable)", "99999999999999999999.0.0", "1.0.0", versionGate},
		{"malformed floor — fails open", "1.0.0", "garbage", versionProceed},
		{"malformed floor + malformed client — fails open", "x", "y", versionProceed},
	}
	for _, c := range cases {
		if got := decideClientVersion(c.clientVersion, c.minVersion); got != c.want {
			t.Errorf("%s: decideClientVersion(%q, %q) = %d, want %d",
				c.name, c.clientVersion, c.minVersion, got, c.want)
		}
	}
}

// TestDecideClientVersionHeader is the bearer-path (#380) twin of the login gate.
// It must agree with decideClientVersion on every case EXCEPT a malformed
// version, which the header path treats as absent (see decideClientVersionHeader
// for why).
func TestDecideClientVersionHeader(t *testing.T) {
	cases := []struct {
		name          string
		clientVersion string
		minVersion    string
		want          versionDecision
	}{
		{"no floor, no header — legacy permissive", "", "", versionProceed},
		{"no floor, header present — permissive", "1.0.0", "", versionProceed},
		{"floor set, absent header (web/legacy) — no gate", "", "1.0.0", versionProceed},
		{"below floor — gated", "0.9.0", "1.0.0", versionGate},
		{"at floor — proceeds", "1.0.0", "1.0.0", versionProceed},
		{"above floor — proceeds", "1.2.0", "1.0.0", versionProceed},
		{"patch below floor — gated", "1.0.0", "1.0.1", versionGate},
		{"v-prefixed below floor — gated (same grammar as login)", "v0.9.0", "1.0.0", versionGate},
		{"malformed header with floor — treated as ABSENT, not gated", "not-a-version", "1.0.0", versionProceed},
		{"pre-release suffix (outside the grammar) — treated as absent", "1.0.0-rc1", "2.0.0", versionProceed},
		{"overflowing header with floor — treated as absent", "99999999999999999999.0.0", "1.0.0", versionProceed},
		{"malformed floor — fails open", "1.0.0", "garbage", versionProceed},
	}
	for _, c := range cases {
		if got := decideClientVersionHeader(c.clientVersion, c.minVersion); got != c.want {
			t.Errorf("%s: decideClientVersionHeader(%q, %q) = %d, want %d",
				c.name, c.clientVersion, c.minVersion, got, c.want)
		}
	}
}

// TestMalformedClientVersionWarnIsDeduped guards the log-flood defence: the
// header rides every request, so one misconfigured client must not warn once per
// call, and a caller varying the garbage must not grow the dedupe set without
// bound.
func TestMalformedClientVersionWarnIsDeduped(t *testing.T) {
	malformedVersionWarned.Lock()
	malformedVersionWarned.seen = nil
	malformedVersionWarned.Unlock()

	for i := 0; i < 5; i++ {
		warnMalformedClientVersion("junk")
	}
	malformedVersionWarned.Lock()
	n := len(malformedVersionWarned.seen)
	malformedVersionWarned.Unlock()
	if n != 1 {
		t.Fatalf("same malformed value warned %d times, want 1", n)
	}

	for i := 0; i < malformedVersionWarnLimit*2; i++ {
		warnMalformedClientVersion(fmt.Sprintf("junk-%d", i))
	}
	malformedVersionWarned.Lock()
	n = len(malformedVersionWarned.seen)
	malformedVersionWarned.Unlock()
	if n > malformedVersionWarnLimit {
		t.Fatalf("dedupe set grew to %d, want <= %d", n, malformedVersionWarnLimit)
	}
}
