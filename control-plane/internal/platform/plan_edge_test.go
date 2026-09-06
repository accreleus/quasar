package platform

import (
	"testing"
	"time"
)

// The channel-switch rules (#111). The edge→stable direction is the one this
// ordering exists for: an instance that ran edge is on a build whose schema can
// be ahead of every tagged release, and no surface may offer a move backwards
// (ADR 0002).

func edgeRel(id, commit string, schema int, built time.Time) Release {
	r := rel(id, "", commit, schema, built, onEdge)
	r.Notes = ""
	r.Manifest = nil
	return r
}

func TestSwitchBackToStableNeverOffersALowerSchema(t *testing.T) {
	// Installed: the edge build at schema 76. Stored: stable 75 and stable 76.
	installed := cp(commitC, 76)
	rows := []Release{
		rel("stable-75", "0.7.5", commitA, 75, at(1)),
		rel("stable-76", "0.7.6", commitB, 76, at(2)),
		edgeRel("edge-76", commitC, 76, at(3)),
	}

	view := PlanRelease(PlanInputs{Channel: ChannelStable, ControlPlane: installed, Releases: rows})
	if len(view.Available) != 1 || view.Available[0].ID != "stable-76" {
		t.Fatalf("switching back to stable offered %v, want only stable-76", ids(view.Available))
	}

	// The same rows on edge list the edge row and nothing from stable.
	view = PlanRelease(PlanInputs{Channel: ChannelEdge, ControlPlane: installed, Releases: rows, UpdaterPresent: true, ControlPlaneInstallMode: str(InstallRegistry)})
	if len(view.Available) != 1 || view.Available[0].ID != "edge-76" {
		t.Fatalf("edge offered %v, want only edge-76", ids(view.Available))
	}
}

func TestEdgeOrderingAndUpToDate(t *testing.T) {
	installed := cp(commitC, 76)
	rows := []Release{
		edgeRel("older", commitA, 76, at(1)),
		edgeRel("installed", commitC, 76, at(2)),
		edgeRel("newest", commitB, 76, at(3)),
		edgeRel("below", commitA, 75, at(9)), // never listed, whatever its built_at
	}
	view := PlanRelease(PlanInputs{Channel: ChannelEdge, ControlPlane: installed, Releases: rows, UpdaterPresent: true, ControlPlaneInstallMode: str(InstallRegistry)})

	want := []string{"newest", "installed", "older"}
	if got := ids(view.Available); !equalStrings(got, want) {
		t.Fatalf("edge order = %v, want %v (built_at DESC at one schema_version)", got, want)
	}

	// Every listed edge row keeps the contract's edge shape.
	for _, r := range view.Available {
		if r.Version != nil {
			t.Errorf("%s: version = %v, want null on edge", r.ID, *r.Version)
		}
		if len(r.Manifest) != 0 {
			t.Errorf("%s: manifest is set, want null on edge", r.ID)
		}
	}

	// available[0] is not what the control plane is on, so it is eligible.
	if reason := targetReason(view, TargetControlPlane); reason != "" {
		t.Fatalf("control plane reason = %q, want eligible", reason)
	}

	// With the installed commit newest, the answer is up_to_date.
	rows = rows[:2]
	view = PlanRelease(PlanInputs{Channel: ChannelEdge, ControlPlane: installed, Releases: rows, UpdaterPresent: true, ControlPlaneInstallMode: str(InstallRegistry)})
	if view.Available[0].ID != "installed" {
		t.Fatalf("newest = %s, want installed", view.Available[0].ID)
	}
	if reason := targetReason(view, TargetControlPlane); reason != ReasonUpToDate {
		t.Fatalf("control plane reason = %q, want %q", reason, ReasonUpToDate)
	}
}

func ids(rows []Release) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// targetReason returns one target kind's reason, "" when eligible.
func targetReason(v View, kind string) string {
	for _, t := range v.Targets {
		if t.Kind == kind {
			if t.Reason == nil {
				return ""
			}
			return *t.Reason
		}
	}
	return "no such target"
}

// #136: switching from a newer tagged install to a lagging edge branch must not
// offer a downgrade in time. Keep the row visible, and guard apply independently
// because a fleet apply normally accepts an up_to_date control-plane target.
func TestOlderEdgeBuildIsListedButCannotBeApplied(t *testing.T) {
	for _, tc := range []struct {
		name      string
		channel   string
		schema    int
		built     time.Time
		installed *string
		blocked   bool
	}{
		{"older edge", ChannelEdge, 76, at(1), str(at(2).Format(time.RFC3339)), true},
		{"newer edge", ChannelEdge, 76, at(3), str(at(2).Format(time.RFC3339)), false},
		{"equal timestamps", ChannelEdge, 76, at(2), str(at(2).Format(time.RFC3339)), false},
		{"newer schema takes precedence", ChannelEdge, 77, at(1), str(at(2).Format(time.RFC3339)), false},
		{"stable unchanged", ChannelStable, 76, at(1), str(at(2).Format(time.RFC3339)), false},
		{"missing installed time", ChannelEdge, 76, at(1), nil, false},
		{"invalid installed time", ChannelEdge, 76, at(1), str("invalid"), false},
		{"missing candidate time", ChannelEdge, 76, time.Time{}, str(at(2).Format(time.RFC3339)), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			installed := cp(commitB, 76)
			installed.BuiltAt = tc.installed
			release := rel("candidate", "0.2.3", commitA, tc.schema, tc.built)
			release.Channel = tc.channel
			view := PlanRelease(PlanInputs{
				Channel: tc.channel, ControlPlane: installed, Releases: []Release{release},
				UpdaterPresent: true, ControlPlaneInstallMode: str(InstallRegistry),
			})
			if len(view.Available) != 1 {
				t.Fatal("candidate must remain visible")
			}
			wantReason := ""
			if tc.blocked {
				wantReason = ReasonUpToDate
			}
			if got := targetReason(view, TargetControlPlane); got != wantReason {
				t.Fatalf("reason = %q, want %q", got, wantReason)
			}
			if offered(view, release.ID) == tc.blocked {
				t.Fatalf("apply offered = %v, blocked = %v", offered(view, release.ID), tc.blocked)
			}
		})
	}
}
