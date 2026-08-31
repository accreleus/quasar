package audit

import "strings"

// Severity buckets an action for display.
const (
	SeverityInfo = "info"
	SeverityWarn = "warn"
	SeverityErr  = "err"
)

// destructiveActions are the destructive actions whose names carry no suffix to
// match on. Each takes something away from a user.
var destructiveActions = map[string]bool{
	"host.drain":             true,
	"session.stop":           true,
	"storage.home.tombstone": true,
}

// Severity classifies an action. DERIVED at read time, never stored and never
// client-supplied: one rule governs every consumer, and a stored value could
// contradict its own action. An unmatched action is info, so a new action needs
// no change here. semantics: control-api.md §UI v3 console
func Severity(action string) string {
	switch {
	case strings.HasSuffix(action, ".failed"), strings.Contains(action, "error"):
		return SeverityErr
	case strings.HasSuffix(action, ".deleted"),
		strings.HasSuffix(action, ".delete"),
		strings.HasSuffix(action, ".revoked"),
		strings.HasSuffix(action, ".disabled"),
		destructiveActions[action]:
		return SeverityWarn
	}
	return SeverityInfo
}
