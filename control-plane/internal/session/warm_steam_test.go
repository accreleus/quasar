package session

import (
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
)

// kind goes only to a parked_sessions_v1 connection and only while warm Steam
// is on; a parked session or an adoption to a connection that cannot take it
// fails closed.
func TestWireAssign(t *testing.T) {
	for _, c := range []struct {
		name          string
		parks, warmOn bool
		kind, adopt   string
		wantKind      string
		wantAdopt     string
		wantOK        bool
	}{
		{"capable, on: kind defaults to play", true, true, "", "", KindPlay, "", true},
		{"capable, on: parked", true, true, KindParked, "", KindParked, "", true},
		{"capable, on: claim", true, true, "", "k", KindPlay, "k", true},
		{"capable, off: no kind", true, false, "", "", "", "", true},
		{"capable, off: a claim still adopts, with no kind", true, false, "", "k", "", "k", true},
		{"capable, off: no parked session", true, false, KindParked, "", "", "", false},
		{"older agent: no kind", false, true, "", "", "", "", true},
		{"older agent: no parked session", false, true, KindParked, "", "", "", false},
		{"older agent: an adoption fails closed", false, true, "", "k", "", "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, ok := wireAssign(agentws.SessionAssignCmd{Kind: c.kind, AdoptSessionID: c.adopt}, c.parks, c.warmOn)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if ok && (got.Kind != c.wantKind || got.AdoptSessionID != c.wantAdopt) {
				t.Errorf("kind=%q adopt=%q, want %q / %q", got.Kind, got.AdoptSessionID, c.wantKind, c.wantAdopt)
			}
		})
	}
}

func TestValidLaunchProgress(t *testing.T) {
	for raw, want := range map[string]bool{
		`{"step":"shaders","percent":0}`:       true,
		`{"step":"shaders","percent":100}`:     true,
		`{"step":"shaders","percent":101}`:     false,
		`{"step":"shaders","percent":-1}`:      false,
		`{"step":"shaders","percent":4.5}`:     false,
		`{"step":"shaders","percent":null}`:    false,
		`{"step":"compile","percent":5}`:       false,
		`{"step":"shaders"}`:                   false,
		`{"step":"shaders","percent":5,"x":1}`: false,
		`"shaders"`:                            false,
		`{"step":"shaders","percent":"5"}`:     false,
		`{"percent":7,"step":"shaders"}`:       true,
	} {
		if _, ok := validLaunchProgress([]byte(raw)); ok != want {
			t.Errorf("validLaunchProgress(%s) = %v, want %v", raw, ok, want)
		}
	}
}
