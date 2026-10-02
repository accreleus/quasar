package audit

import "testing"

// control-api.md severity table (amendment 11): setting a readiness override
// makes a host launch against evidence that says it should not, so it is warn;
// clearing and lapsing restore the default and stay info.
func TestSeverityReadinessOverride(t *testing.T) {
	for action, want := range map[string]string{
		"host.readiness_override.set":     SeverityWarn,
		"host.readiness_override.cleared": SeverityInfo,
		"host.readiness_override.lapsed":  SeverityInfo,
	} {
		if got := Severity(action); got != want {
			t.Errorf("Severity(%q) = %q, want %q", action, got, want)
		}
	}
}
