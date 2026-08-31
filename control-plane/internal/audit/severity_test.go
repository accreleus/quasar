package audit

import "testing"

func TestSeverity(t *testing.T) {
	cases := map[string]string{
		"session.failed": "err", "image.sync.error": "err",
		"user.deleted": "warn", "invite.revoked": "warn", "user.disabled": "warn",
		"host.drain": "warn", "session.stop": "warn", "storage.home.tombstone": "warn",
		"app.artwork.set": "info", "user.role_changed": "info",
	}
	for action, want := range cases {
		if got := Severity(action); got != want {
			t.Errorf("%s: got %s want %s", action, got, want)
		}
	}
}

// TestSeverityBoundaries pins the cases the suffix rules can get wrong: the
// enable half of a disable pair must NOT read as destructive, `.delete` and
// `.deleted` are separate suffixes, and an unmatched action falls to info rather
// than to whatever the last branch happened to return.
func TestSeverityBoundaries(t *testing.T) {
	cases := map[string]string{
		"user.enabled":            SeverityInfo,
		"host.uncordon":           SeverityInfo,
		"app.delete":              SeverityWarn,
		"app.library.rule.delete": SeverityWarn,
		"session.launched":        SeverityInfo,
		"":                        SeverityInfo,
		"something.entirely.new":  SeverityInfo,
	}
	for action, want := range cases {
		if got := Severity(action); got != want {
			t.Errorf("%q: got %s want %s", action, got, want)
		}
	}
}
