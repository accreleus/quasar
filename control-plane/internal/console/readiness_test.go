package console

import (
	"strings"
	"testing"
)

func TestDefaultAppCheck(t *testing.T) {
	appID := "6f1c0000-0000-0000-0000-000000000001"
	tests := []struct {
		name        string
		defaultApp  *string
		facts       DefaultAppFacts
		wantStatus  string
		wantSummary []string // substrings the operator must read
	}{
		{
			name:       "no default app is not applicable",
			wantStatus: "skip",
		},
		{
			name:        "a desktop that declares direct display passes",
			defaultApp:  &appID,
			facts:       DefaultAppFacts{Found: true, Name: "KDE Plasma", Enabled: true, Kind: "desktop", Direct: true},
			wantStatus:  "pass",
			wantSummary: []string{"KDE Plasma"},
		},
		{
			name:        "a launcher that declares direct display passes",
			defaultApp:  &appID,
			facts:       DefaultAppFacts{Found: true, Name: "Steam", Enabled: true, Kind: "launcher", Direct: true},
			wantStatus:  "pass",
			wantSummary: []string{"Steam"},
		},
		{
			name:        "an app without the key cannot run direct",
			defaultApp:  &appID,
			facts:       DefaultAppFacts{Found: true, Name: "Old Desktop", Enabled: true, Kind: "desktop"},
			wantStatus:  "fail",
			wantSummary: []string{"Old Desktop", "cannot run direct", "direct_display"},
		},
		{
			name:        "a disabled app fails, naming it",
			defaultApp:  &appID,
			facts:       DefaultAppFacts{Found: true, Name: "KDE Plasma", Kind: "desktop", Direct: true},
			wantStatus:  "fail",
			wantSummary: []string{"KDE Plasma", "disabled"},
		},
		{
			name:        "a game tile fails, naming its kind",
			defaultApp:  &appID,
			facts:       DefaultAppFacts{Found: true, Name: "Hades", Enabled: true, Kind: "game", ParentAppID: "6f1c0000-0000-0000-0000-000000000099", Direct: true},
			wantStatus:  "fail",
			wantSummary: []string{"Hades", "game", "desktop or launcher"},
		},
		{
			name:        "a game with no parent still fails on kind",
			defaultApp:  &appID,
			facts:       DefaultAppFacts{Found: true, Name: "Some Game", Enabled: true, Kind: "game", Direct: true},
			wantStatus:  "fail",
			wantSummary: []string{"Some Game", "game", "desktop or launcher"},
		},
		{
			name:        "a deleted app fails",
			defaultApp:  &appID,
			facts:       DefaultAppFacts{},
			wantStatus:  "fail",
			wantSummary: []string{"no longer exists"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := DefaultAppCheck(tc.defaultApp, tc.facts)
			if got.ID != DefaultAppCheckID || got.Source != "operator" || got.Blocks != nil {
				t.Fatalf("check identity = %+v, want id %q, source operator, no blocks", got, DefaultAppCheckID)
			}
			if got.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q (%+v)", got.Status, tc.wantStatus, got)
			}
			if got.Summary == "" {
				t.Fatal("every check carries a summary")
			}
			for _, want := range tc.wantSummary {
				if !strings.Contains(got.Summary, want) {
					t.Fatalf("summary %q does not say %q", got.Summary, want)
				}
			}
			if (got.Status == "fail") != (got.Remediation != "") {
				t.Fatalf("remediation %q: a fail asks the operator for something, nothing else does", got.Remediation)
			}
		})
	}
}

func TestRuntimeSpecDirect(t *testing.T) {
	for _, tc := range []struct {
		spec string
		want bool
	}{
		{`{"image":"x","direct_display":true}`, true},
		{`{"image":"x","direct_display":false}`, false},
		{`{"image":"x"}`, false},
		{`{"direct_display":"true"}`, false},
		{`{}`, false},
		{``, false},
		{`not json`, false},
	} {
		if got := RuntimeSpecDirect([]byte(tc.spec)); got != tc.want {
			t.Errorf("RuntimeSpecDirect(%s) = %v, want %v", tc.spec, got, tc.want)
		}
	}
}
