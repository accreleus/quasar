package platform

import (
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/buildinfo"
)

// The beta channel (#121) adds two rules to the pure plan — it lists the
// prereleases among the stable channel's rows, and it orders them by semver
// precedence rather than build time — plus one that applies everywhere: no
// channel offers a build below the installed one, which is what switching back
// to stable runs into. Everything here is a table over those rules.

// cpAt is cp() with a version, which the switch-back rule reads.
func cpAt(commit string, schema int, version string) buildinfo.Identity {
	id := cp(commit, schema)
	id.Version = version
	return id
}

const commitD = "dddddddddddddddddddddddddddddddddddddddd"

func TestRowChannel(t *testing.T) {
	for _, tc := range []struct{ channel, want string }{
		{ChannelStable, ChannelStable},
		{ChannelEdge, ChannelEdge},
		// The one that matters: beta stores no rows of its own.
		{ChannelBeta, ChannelStable},
	} {
		if got := rowChannel(tc.channel); got != tc.want {
			t.Errorf("rowChannel(%q) = %q, want %q", tc.channel, got, tc.want)
		}
	}
	if !ValidChannel(ChannelBeta) {
		t.Error("ValidChannel(beta) = false")
	}
	if ValidChannel("nightly") {
		t.Error("ValidChannel(nightly) = true")
	}
}

func TestBetaOfferableListingAndOrdering(t *testing.T) {
	tests := []struct {
		name     string
		cp       buildinfo.Identity
		releases []Release
		wantIDs  []string
	}{
		{
			name: "lists the prereleases stable hides, in semver precedence order",
			cp:   cpAt(commitA, 74, "0.1.0"),
			releases: []Release{
				rel("rc2", "0.2.0-rc.2", commitB, 74, at(5), prerelease),
				rel("final", "0.2.0", commitC, 74, at(6)),
				rel("next-rc", "0.2.1-rc.1", commitD, 74, at(7), prerelease),
			},
			wantIDs: []string{"next-rc", "final", "rc2"},
		},
		{
			// The reason beta cannot reuse the built_at ordering: an rc is cut
			// from develop while a patch is cut from main, so the newer BUILD
			// is the lower VERSION.
			name: "semver precedence beats build time when the two disagree",
			cp:   cpAt(commitA, 74, "0.1.0"),
			releases: []Release{
				rel("rc", "0.3.0-rc.1", commitB, 74, at(3), prerelease),
				rel("hotfix", "0.2.5", commitC, 74, at(9)),
			},
			wantIDs: []string{"rc", "hotfix"},
		},
		{
			name: "numeric prerelease identifiers are not string-ordered",
			cp:   cpAt(commitA, 74, "0.1.0"),
			releases: []Release{
				rel("rc9", "0.3.0-rc.9", commitB, 74, at(9), prerelease),
				rel("rc10", "0.3.0-rc.10", commitC, 74, at(1), prerelease),
			},
			wantIDs: []string{"rc10", "rc9"},
		},
		{
			name: "schema_version still outranks semver (ADR 0002)",
			cp:   cpAt(commitA, 74, "0.1.0"),
			releases: []Release{
				rel("high-schema", "0.2.5", commitB, 75, at(1)),
				rel("low-schema", "0.3.0-rc.1", commitC, 74, at(9), prerelease),
			},
			wantIDs: []string{"high-schema", "low-schema"},
		},
		{
			name: "a release below the control plane's schema is still never listed",
			cp:   cpAt(commitA, 74, "0.1.0"),
			releases: []Release{
				rel("below", "0.2.0-rc.1", commitB, 73, at(9), prerelease),
				rel("at", "0.2.0", commitC, 74, at(1)),
			},
			wantIDs: []string{"at"},
		},
		{
			name: "a prerelease with no manifest is not listed: nothing pins it (ADR 0001)",
			cp:   cpAt(commitA, 74, "0.1.0"),
			releases: []Release{
				rel("unpinned", "0.3.0-rc.1", commitB, 74, at(9), prerelease, noManifest),
				rel("pinned", "0.2.0", commitC, 74, at(1)),
			},
			wantIDs: []string{"pinned"},
		},
		{
			name: "edge rows are never mixed in: beta reads the stable rows",
			cp:   cpAt(commitA, 74, "0.1.0"),
			releases: []Release{
				rel("edge-build", "", commitB, 74, at(9), onEdge),
				rel("beta-build", "0.3.0-rc.1", commitC, 74, at(1), prerelease),
			},
			wantIDs: []string{"beta-build"},
		},
		{
			// Falls back rather than inventing an order.
			name: "an unparseable version falls back to built_at",
			cp:   cpAt(commitA, 74, "0.1.0"),
			releases: []Release{
				rel("older", "nightly-2", commitB, 74, at(1)),
				rel("newer", "nightly-9", commitC, 74, at(3)),
			},
			wantIDs: []string{"newer", "older"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := PlanRelease(PlanInputs{Channel: ChannelBeta, ControlPlane: tc.cp, Releases: tc.releases})
			if got.Channel != ChannelBeta {
				t.Errorf("view channel = %q, want beta", got.Channel)
			}
			assertIDs(t, got.Available, tc.wantIDs)
		})
	}
}

// TestBetaOrderingIsATotalOrder is the guard on the comparator itself. When a
// parseable version and an unparseable one mix, ordering the pair by built_at
// while ordering parseable pairs by precedence is not transitive: 0.3.0 beats
// 0.2.0 by version, the unparseable row beats 0.3.0 by build time, and 0.2.0
// beats the unparseable row by build time — a cycle whose winner is whatever the
// SQL scan happened to return first (Store.Releases has no ORDER BY). So the
// answer must not depend on the input order at all.
func TestBetaOrderingIsATotalOrder(t *testing.T) {
	high := rel("high", "0.3.0", commitB, 74, at(1))
	odd := rel("odd", "nightly-2", commitC, 74, at(2))
	low := rel("low", "0.2.0", commitD, 74, at(3))

	// Every parseable row above every unparseable one, precedence inside the
	// parseable group: the highest version leads in all six permutations.
	want := []string{"high", "low", "odd"}
	for _, in := range [][]Release{
		{high, odd, low},
		{high, low, odd},
		{odd, high, low},
		{odd, low, high},
		{low, odd, high},
		{low, high, odd},
	} {
		order := make([]string, len(in))
		for i, r := range in {
			order[i] = r.ID
		}
		t.Run("input "+order[0]+order[1]+order[2], func(t *testing.T) {
			got := PlanRelease(PlanInputs{
				Channel:      ChannelBeta,
				ControlPlane: cpAt(commitA, 74, "0.1.0"),
				Releases:     in,
			})
			assertIDs(t, got.Available, want)
		})
	}
}

// TestStableOrderingIsUnchangedByBeta is the guard on "beta changes nothing
// else": the same rows, read on stable, keep the ordering and the listing rules
// they had before the channel existed.
func TestStableOrderingIsUnchangedByBeta(t *testing.T) {
	releases := []Release{
		rel("rc", "0.3.0-rc.1", commitB, 74, at(3), prerelease),
		rel("hotfix", "0.2.5", commitC, 74, at(9)),
		rel("older", "0.2.4", commitD, 74, at(1)),
	}
	got := PlanRelease(PlanInputs{
		Channel:      ChannelStable,
		ControlPlane: cpAt(commitA, 74, "0.1.0"),
		Releases:     releases,
	})
	// The prerelease is gone, and built_at — not semver — orders the rest, so
	// the later-built 0.2.5 still leads.
	assertIDs(t, got.Available, []string{"hotfix", "older"})
}

// TestSwitchBackFromBetaNeverDowngrades is the switch-back rule. An instance
// that took a prerelease from beta and then moved to stable waits for stable to
// pass it; it is never offered the older stable release, on any channel.
func TestSwitchBackFromBetaNeverDowngrades(t *testing.T) {
	installed := cpAt(commitB, 74, "0.3.0-rc.1")

	t.Run("stable has not caught up: nothing is offered", func(t *testing.T) {
		got := PlanRelease(PlanInputs{
			Channel:      ChannelStable,
			ControlPlane: installed,
			Releases:     []Release{rel("hotfix", "0.2.5", commitC, 74, at(9))},
			Hosts:        []HostIdentity{knownHost("h1", commitB)},
		})
		assertIDs(t, got.Available, nil)
		assertReason(t, got, TargetControlPlane, ReasonNoRelease)
		assertReason(t, got, TargetHost, ReasonNoRelease)
	})

	t.Run("stable has caught up: the final release is offered", func(t *testing.T) {
		got := PlanRelease(PlanInputs{
			Channel:      ChannelStable,
			ControlPlane: installed,
			Releases: []Release{
				rel("hotfix", "0.2.5", commitC, 74, at(9)),
				rel("final", "0.3.0", commitD, 74, at(10)),
			},
			UpdaterPresent:          true,
			ControlPlaneInstallMode: str(InstallRegistry),
		})
		assertIDs(t, got.Available, []string{"final"})
		assertEligible(t, got, TargetControlPlane)
	})

	t.Run("on beta the installed prerelease stays listed and reads up_to_date", func(t *testing.T) {
		got := PlanRelease(PlanInputs{
			Channel:      ChannelBeta,
			ControlPlane: installed,
			Releases: []Release{
				rel("hotfix", "0.2.5", commitC, 74, at(9)),
				rel("installed", "0.3.0-rc.1", commitB, 74, at(3), prerelease),
			},
		})
		assertIDs(t, got.Available, []string{"installed"})
		assertReason(t, got, TargetControlPlane, ReasonUpToDate)
	})
}

// The switch-back rule is scoped to an installed PRERELEASE, so a stable install
// sees exactly what it saw before — including the ordering quirk where a
// later-built patch outranks a higher version.
func TestBelowInstalledVersionDoesNotFireOnAReleaseInstall(t *testing.T) {
	got := PlanRelease(PlanInputs{
		Channel:      ChannelStable,
		ControlPlane: cpAt(commitB, 74, "0.3.0"),
		Releases:     []Release{rel("hotfix", "0.2.5", commitC, 74, at(9))},
	})
	assertIDs(t, got.Available, []string{"hotfix"})
}

// An unstamped control plane reports version "dev", which parses as nothing; the
// rule must fall open rather than hide every release from it.
func TestBelowInstalledVersionIgnoresAnUnstampedControlPlane(t *testing.T) {
	got := PlanRelease(PlanInputs{
		Channel:      ChannelBeta,
		ControlPlane: cpAt(commitB, 74, buildinfo.UnknownVersion),
		Releases:     []Release{rel("rc", "0.3.0-rc.1", commitC, 74, at(9), prerelease)},
	})
	assertIDs(t, got.Available, []string{"rc"})
}

// Faults are read off the rows the channel selects, so agent_ahead still fires
// on beta even though the rows are stored under `stable`.
func TestBetaFaultsReadTheStableRows(t *testing.T) {
	got := PlanRelease(PlanInputs{
		Channel:      ChannelBeta,
		ControlPlane: cpAt(commitA, 74, "0.2.0"),
		Releases: []Release{
			rel("installed", "0.2.0", commitA, 74, at(1)),
			rel("ahead", "0.3.0-rc.1", commitC, 75, at(9), prerelease),
		},
		Hosts: []HostIdentity{knownHost("h1", commitC)},
	})
	if len(got.Faults) != 1 || got.Faults[0].Kind != FaultAgentAhead {
		t.Fatalf("faults = %+v, want one agent_ahead_of_control_plane", got.Faults)
	}
}

// A control plane moved BACKWARDS by hand — redeployed to an older patch release
// while the fleet stayed on a prerelease — leaves its agents ahead of it, and the
// fault must say so. On beta that comparison is semver precedence, not build
// time: the rc was cut from `develop` before the patch was cut from `main`, so
// built_at reads the wrong way round.
func TestBetaAgentAheadUsesSemverPrecedenceNotBuildTime(t *testing.T) {
	releases := []Release{
		rel("rc", "0.3.0-rc.1", commitB, 74, at(3), prerelease),
		// The build the control plane was manually put back onto: LOWER version,
		// LATER build.
		rel("patch", "0.2.5", commitC, 74, at(9)),
	}
	installed := cpAt(commitC, 74, "0.2.5")
	hosts := []HostIdentity{knownHost("h1", commitB)}

	got := PlanRelease(PlanInputs{
		Channel: ChannelBeta, ControlPlane: installed, Releases: releases, Hosts: hosts,
	})
	if len(got.Faults) != 1 || got.Faults[0].Kind != FaultAgentAhead {
		t.Fatalf("faults = %+v, want one agent_ahead_of_control_plane", got.Faults)
	}
	// The ordering the fault used is the one `available` used.
	assertIDs(t, got.Available, []string{"rc", "patch"})

	// Scoped to beta: on stable the rc is not even listed, and built_at — the
	// ordering that channel really uses — puts the control plane's row on top.
	onStable := PlanRelease(PlanInputs{
		Channel: ChannelStable, ControlPlane: installed, Releases: releases, Hosts: hosts,
	})
	if len(onStable.Faults) != 0 {
		t.Errorf("faults on stable = %+v, want none: built_at is that channel's ordering", onStable.Faults)
	}
}

// ─── helpers ────────────────────────────────────────────────────────────────

func knownHost(id, commit string) HostIdentity {
	built := at(1).Format(time.RFC3339)
	return HostIdentity{
		HostID: id, NodeName: id, Status: "online",
		SourceCommit: str(commit), BuiltAt: &built,
		InstallMode: str(InstallRegistry), UpdaterPresent: boolp(true),
	}
}

func assertIDs(t *testing.T, got []Release, want []string) {
	t.Helper()
	if len(got) != len(want) {
		ids := make([]string, len(got))
		for i, r := range got {
			ids[i] = r.ID
		}
		t.Fatalf("available = %v, want %v", ids, want)
	}
	for i := range want {
		if got[i].ID != want[i] {
			ids := make([]string, len(got))
			for j, r := range got {
				ids[j] = r.ID
			}
			t.Fatalf("available = %v, want %v", ids, want)
		}
	}
}

func assertReason(t *testing.T, v View, kind, want string) {
	t.Helper()
	for _, tgt := range v.Targets {
		if tgt.Kind != kind {
			continue
		}
		if tgt.Reason == nil {
			t.Fatalf("%s target is eligible, want reason %q", kind, want)
		}
		if *tgt.Reason != want {
			t.Fatalf("%s target reason = %q, want %q", kind, *tgt.Reason, want)
		}
		return
	}
	t.Fatalf("no %s target in the view", kind)
}

func assertEligible(t *testing.T, v View, kind string) {
	t.Helper()
	for _, tgt := range v.Targets {
		if tgt.Kind != kind {
			continue
		}
		if !tgt.Eligible {
			t.Fatalf("%s target ineligible: %v", kind, *tgt.Reason)
		}
		return
	}
	t.Fatalf("no %s target in the view", kind)
}
