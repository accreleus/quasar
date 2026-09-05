package platform

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/buildinfo"
)

// The release plan is where every ordering, listing and eligibility rule of
// control-api.md §"Platform releases" lives, so this is a table over those
// rules rather than over the function's branches.

func at(day int) time.Time {
	return time.Date(2026, 9, day, 12, 0, 0, 0, time.UTC)
}

func str(s string) *string { return &s }
func boolp(b bool) *bool   { return &b }

var validManifest = json.RawMessage(`{"format_version":1}`)

// rel builds a stable, manifest-carrying release; the options mutate it.
func rel(id, version, commit string, schema int, built time.Time, opts ...func(*Release)) Release {
	r := Release{
		ID:            id,
		Channel:       ChannelStable,
		Version:       str(version),
		SourceCommit:  commit,
		BuiltAt:       built,
		SchemaVersion: schema,
		Manifest:      validManifest,
		DiscoveredAt:  built,
	}
	for _, o := range opts {
		o(&r)
	}
	return r
}

func prerelease(r *Release) { r.Prerelease = true }
func noManifest(r *Release) { r.Manifest = nil }
func onEdge(r *Release)     { r.Channel = ChannelEdge; r.Version = nil }
func cp(commit string, schema int) buildinfo.Identity {
	id := buildinfo.Identity{Version: "0.1.0", SchemaVersion: schema}
	if commit != "" {
		id.SourceCommit = str(commit)
	}
	return id
}

const (
	commitA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	commitB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	commitC = "cccccccccccccccccccccccccccccccccccccccc"
)

func TestOfferableListingAndOrdering(t *testing.T) {
	tests := []struct {
		name     string
		channel  string
		cp       buildinfo.Identity
		releases []Release
		wantIDs  []string
	}{
		{
			name:    "orders by schema_version DESC then built_at DESC",
			channel: ChannelStable,
			cp:      cp(commitA, 70),
			releases: []Release{
				rel("old", "0.1.0", commitA, 70, at(1)),
				rel("newest", "0.3.0", commitC, 74, at(2)),
				rel("mid", "0.2.0", commitB, 72, at(9)),
			},
			wantIDs: []string{"newest", "mid", "old"},
		},
		{
			name:    "built_at is the tiebreak at one schema_version",
			channel: ChannelEdge,
			cp:      cp(commitA, 74),
			releases: []Release{
				rel("older", "", commitA, 74, at(1), onEdge),
				rel("newer", "", commitB, 74, at(3), onEdge),
			},
			wantIDs: []string{"newer", "older"},
		},
		{
			name:    "a release below the control plane's schema is never listed (ADR 0002)",
			channel: ChannelStable,
			cp:      cp(commitA, 74),
			releases: []Release{
				rel("below", "0.1.0", commitB, 73, at(1)),
				rel("equal", "0.2.0", commitC, 74, at(2)),
			},
			wantIDs: []string{"equal"},
		},
		{
			name:    "a prerelease is never listed on stable",
			channel: ChannelStable,
			cp:      cp(commitA, 70),
			releases: []Release{
				rel("rc", "0.2.0-rc.1", commitB, 74, at(3), prerelease),
				rel("final", "0.1.0", commitC, 71, at(1)),
			},
			wantIDs: []string{"final"},
		},
		{
			name:    "a prerelease IS listed on edge, reported as found",
			channel: ChannelEdge,
			cp:      cp(commitA, 70),
			releases: []Release{
				rel("rc", "", commitB, 74, at(3), onEdge, prerelease, noManifest),
			},
			wantIDs: []string{"rc"},
		},
		{
			name:    "a stable release with no manifest is not listed (ADR 0001)",
			channel: ChannelStable,
			cp:      cp(commitA, 70),
			releases: []Release{
				rel("broken", "0.2.0", commitB, 74, at(3), noManifest),
			},
			wantIDs: nil,
		},
		{
			name:    "the other channel's rows are never mixed in",
			channel: ChannelStable,
			cp:      cp(commitA, 70),
			releases: []Release{
				rel("edge", "", commitB, 80, at(9), onEdge, noManifest),
				rel("stable", "0.2.0", commitC, 74, at(1)),
			},
			wantIDs: []string{"stable"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := PlanRelease(PlanInputs{Channel: tc.channel, ControlPlane: tc.cp, Releases: tc.releases})
			var got []string
			for _, r := range v.Available {
				got = append(got, r.ID)
			}
			if len(got) != len(tc.wantIDs) {
				t.Fatalf("available = %v, want %v", got, tc.wantIDs)
			}
			for i := range got {
				if got[i] != tc.wantIDs[i] {
					t.Fatalf("available = %v, want %v", got, tc.wantIDs)
				}
			}
		})
	}
}

// host builds a fully-identified, online, registry-installed host.
func host(id, name, commit string, opts ...func(*HostIdentity)) HostIdentity {
	h := HostIdentity{
		HostID:         id,
		NodeName:       name,
		Status:         "online",
		AgentVersion:   str("0.1.0"),
		SourceCommit:   str(commit),
		BuiltAt:        str("2026-09-01T00:00:00Z"),
		InstallMode:    str(InstallRegistry),
		UpdaterPresent: boolp(true),
	}
	for _, o := range opts {
		o(&h)
	}
	return h
}

func sourceInstall(h *HostIdentity) { h.InstallMode = str(InstallSource) }
func noUpdater(h *HostIdentity)     { h.UpdaterPresent = boolp(false) }
func offline(h *HostIdentity)       { h.Status = HostOffline }
func draining(h *HostIdentity)      { h.Status = "draining" }
func unknownIdentity(h *HostIdentity) {
	h.SourceCommit, h.BuiltAt, h.InstallMode, h.UpdaterPresent = nil, nil, nil, nil
}

// NULL IS NOT false (openapi.yaml PlatformHostIdentity.updater_present): nobody
// has said, which is an old agent, not a real gap.
func updaterUnreported(h *HostIdentity) { h.UpdaterPresent = nil }
func noInstallMode(h *HostIdentity)     { h.InstallMode = nil }

func TestTargetEligibilityReasons(t *testing.T) {
	newest := rel("newest", "0.3.0", commitC, 74, at(3))

	tests := []struct {
		name       string
		releases   []Release
		cp         buildinfo.Identity
		host       HostIdentity
		wantCPRsn  string // "" = eligible
		wantHostRs string
	}{
		{
			name:       "no release detected: every target is no_release",
			releases:   nil,
			cp:         cp(commitA, 74),
			host:       host("h1", "gpu-01", commitA),
			wantCPRsn:  ReasonNoRelease,
			wantHostRs: ReasonNoRelease,
		},
		{
			name:       "an unstamped control plane cannot be compared at all",
			releases:   []Release{newest},
			cp:         cp("", 74),
			host:       host("h1", "gpu-01", commitC),
			wantCPRsn:  ReasonIdentityUnknown,
			wantHostRs: ReasonUpToDate,
		},
		{
			name:       "a host with any identity field null is identity_unknown",
			releases:   []Release{newest},
			cp:         cp(commitC, 74),
			host:       host("h1", "gpu-01", commitA, unknownIdentity),
			wantCPRsn:  ReasonUpToDate,
			wantHostRs: ReasonIdentityUnknown,
		},
		{
			name:       "a target already on the newest release is up_to_date",
			releases:   []Release{newest},
			cp:         cp(commitC, 74),
			host:       host("h1", "gpu-01", commitC),
			wantCPRsn:  ReasonUpToDate,
			wantHostRs: ReasonUpToDate,
		},
		{
			name:       "a short agent commit still matches the release's full one",
			releases:   []Release{newest},
			cp:         cp(commitC, 74),
			host:       host("h1", "gpu-01", commitC[:7]),
			wantCPRsn:  ReasonUpToDate,
			wantHostRs: ReasonUpToDate,
		},
		{
			name:       "a source-built host is told, never offered",
			releases:   []Release{newest},
			cp:         cp(commitC, 74),
			host:       host("h1", "gpu-01", commitA, sourceInstall),
			wantCPRsn:  ReasonUpToDate,
			wantHostRs: ReasonInstallModeSource,
		},
		{
			name:       "durable outranks transient: an offline source host still reports install_mode_source",
			releases:   []Release{newest},
			cp:         cp(commitC, 74),
			host:       host("h1", "gpu-01", commitA, sourceInstall, offline),
			wantCPRsn:  ReasonUpToDate,
			wantHostRs: ReasonInstallModeSource,
		},
		{
			name:       "updater_present NULL is nobody-has-said, not a gap: identity_unknown",
			releases:   []Release{newest},
			cp:         cp(commitC, 74),
			host:       host("h1", "gpu-01", commitA, updaterUnreported),
			wantCPRsn:  ReasonUpToDate,
			wantHostRs: ReasonIdentityUnknown,
		},
		{
			name:       "install_mode NULL with every other field reported is still identity_unknown",
			releases:   []Release{newest},
			cp:         cp(commitC, 74),
			host:       host("h1", "gpu-01", commitA, noInstallMode),
			wantCPRsn:  ReasonUpToDate,
			wantHostRs: ReasonIdentityUnknown,
		},
		{
			name:       "a registry host whose agent found no updater",
			releases:   []Release{newest},
			cp:         cp(commitC, 74),
			host:       host("h1", "gpu-01", commitA, noUpdater),
			wantCPRsn:  ReasonUpToDate,
			wantHostRs: ReasonUpdaterAbsent,
		},
		{
			name:       "an offline host has nobody to tell",
			releases:   []Release{newest},
			cp:         cp(commitC, 74),
			host:       host("h1", "gpu-01", commitA, offline),
			wantCPRsn:  ReasonUpToDate,
			wantHostRs: ReasonHostOffline,
		},
		{
			name:       "a draining host is NOT offline — a cordon is what an apply wants",
			releases:   []Release{newest},
			cp:         cp(commitC, 74),
			host:       host("h1", "gpu-01", commitA, draining),
			wantCPRsn:  ReasonUpToDate,
			wantHostRs: "",
		},
		{
			name:       "a release above the control plane is a ceiling for hosts",
			releases:   []Release{newest},
			cp:         cp(commitA, 70),
			host:       host("h1", "gpu-01", commitA),
			wantCPRsn:  "",
			wantHostRs: ReasonReleaseAboveControlPlane,
		},
		{
			name:       "equal schema, control plane not yet on it: ordering, not a ceiling",
			releases:   []Release{newest},
			cp:         cp(commitA, 74),
			host:       host("h1", "gpu-01", commitB),
			wantCPRsn:  "",
			wantHostRs: ReasonControlPlaneNotFirst,
		},
		{
			name:       "control plane on the release, host behind: the host is eligible",
			releases:   []Release{newest},
			cp:         cp(commitC, 74),
			host:       host("h1", "gpu-01", commitA),
			wantCPRsn:  ReasonUpToDate,
			wantHostRs: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := PlanRelease(PlanInputs{
				Channel:        ChannelStable,
				ControlPlane:   tc.cp,
				Hosts:          []HostIdentity{tc.host},
				Releases:       tc.releases,
				UpdaterPresent: true,
			})
			if len(v.Targets) != 2 {
				t.Fatalf("targets = %d, want the control plane then one host", len(v.Targets))
			}
			assertTarget(t, "control plane", v.Targets[0], TargetControlPlane, tc.wantCPRsn)
			assertTarget(t, "host", v.Targets[1], TargetHost, tc.wantHostRs)
		})
	}
}

func assertTarget(t *testing.T, label string, got Target, wantKind, wantReason string) {
	t.Helper()
	if got.Kind != wantKind {
		t.Fatalf("%s target kind = %q, want %q", label, got.Kind, wantKind)
	}
	// eligible:true always carries reason null; eligible:false always carries
	// exactly one non-null reason.
	if wantReason == "" {
		if !got.Eligible || got.Reason != nil {
			t.Fatalf("%s: want eligible with a null reason, got eligible=%v reason=%v",
				label, got.Eligible, deref(got.Reason))
		}
		return
	}
	if got.Eligible || got.Reason == nil || *got.Reason != wantReason {
		t.Fatalf("%s: want ineligible with reason %q, got eligible=%v reason=%v",
			label, wantReason, got.Eligible, deref(got.Reason))
	}
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

func TestControlPlaneTargetComesFirstAndHostsKeepTheirOrder(t *testing.T) {
	v := PlanRelease(PlanInputs{
		Channel:      ChannelStable,
		ControlPlane: cp(commitA, 74),
		Hosts: []HostIdentity{
			host("h1", "gpu-01", commitA),
			host("h2", "gpu-02", commitA),
		},
		Releases: []Release{rel("r", "0.2.0", commitA, 74, at(1))},
	})
	if v.Targets[0].Kind != TargetControlPlane || v.Targets[0].HostID != nil {
		t.Fatalf("first target must be the control plane with a null host_id: %+v", v.Targets[0])
	}
	if *v.Targets[1].NodeName != "gpu-01" || *v.Targets[2].NodeName != "gpu-02" {
		t.Fatalf("host targets lost the caller's order: %+v", v.Targets)
	}
}

func TestFaults(t *testing.T) {
	// The host is on a KNOWN release that orders above the control plane's.
	ahead := rel("ahead", "0.3.0", commitB, 75, at(5))
	installed := rel("installed", "0.2.0", commitA, 74, at(1))

	t.Run("an agent above the control plane is a fault", func(t *testing.T) {
		v := PlanRelease(PlanInputs{
			Channel:      ChannelStable,
			ControlPlane: cp(commitA, 74),
			Hosts:        []HostIdentity{host("h1", "gpu-01", commitB)},
			Releases:     []Release{installed, ahead},
		})
		if len(v.Faults) != 1 || v.Faults[0].Kind != FaultAgentAhead {
			t.Fatalf("faults = %+v, want one agent_ahead_of_control_plane", v.Faults)
		}
		if v.Faults[0].HostID == nil || *v.Faults[0].HostID != "h1" {
			t.Fatalf("fault must name the host: %+v", v.Faults[0])
		}
	})

	t.Run("a commit matching no known release raises nothing — unrecognized is not ahead", func(t *testing.T) {
		v := PlanRelease(PlanInputs{
			Channel:      ChannelStable,
			ControlPlane: cp(commitA, 74),
			Hosts:        []HostIdentity{host("h1", "gpu-01", commitC)},
			Releases:     []Release{installed},
		})
		if len(v.Faults) != 0 {
			t.Fatalf("faults = %+v, want none", v.Faults)
		}
	})

	t.Run("identity_unknown is both an ineligibility and a fault", func(t *testing.T) {
		v := PlanRelease(PlanInputs{
			Channel:      ChannelStable,
			ControlPlane: cp(commitA, 74),
			Hosts:        []HostIdentity{host("h1", "gpu-01", commitA, unknownIdentity)},
			Releases:     []Release{installed},
		})
		if len(v.Faults) != 1 || v.Faults[0].Kind != FaultIdentityUnknown {
			t.Fatalf("faults = %+v, want one identity_unknown", v.Faults)
		}
		if v.Targets[1].Reason == nil || *v.Targets[1].Reason != ReasonIdentityUnknown {
			t.Fatalf("the same host must also be ineligible: %+v", v.Targets[1])
		}
		if v.Installed.Hosts[0].IdentityKnown {
			t.Fatal("identity_known must be served false for that host")
		}
	})
}

func TestViewSerializesEveryRequiredKeyIncludingActiveApply(t *testing.T) {
	checked := at(4)
	v := PlanRelease(PlanInputs{
		Channel:      ChannelStable,
		EdgeBranch:   "develop",
		ControlPlane: cp(commitA, 74),
		CheckedAt:    &checked,
		LastError:    str("api.github.com answered 503"),
	})
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"channel", "edge_branch", "checked_at", "last_error",
		"installed", "available", "targets", "faults", "active_apply"} {
		if _, ok := body[key]; !ok {
			t.Errorf("view is missing required key %q: %s", key, raw)
		}
	}
	// #116 serves the apply half, so active_apply is ALWAYS serialized: null is
	// the answer, not the absence of one.
	if got := string(body["active_apply"]); got != "null" {
		t.Errorf("active_apply = %s, want null when nothing is in flight", got)
	}
	if string(body["available"]) != "[]" || string(body["targets"]) == "null" || string(body["faults"]) != "[]" {
		t.Errorf("empty lists must serialize as [], never null: %s", raw)
	}
	if string(body["checked_at"]) != `"2026-09-04T12:00:00Z"` {
		t.Errorf("checked_at = %s, want RFC3339 UTC", body["checked_at"])
	}
}

func TestCheckedAtNilBeforeTheFirstSuccess(t *testing.T) {
	v := PlanRelease(PlanInputs{Channel: ChannelStable, ControlPlane: cp(commitA, 74)})
	if v.CheckedAt != nil {
		t.Fatalf("checked_at = %v, want nil before detection has ever succeeded", *v.CheckedAt)
	}
}

// TestEveryContractIdentifierIsEvaluatedOrReserved walks the two CLOSED
// vocabularies of control-api.md §"Platform releases" — EligibilityReason and
// PlatformReleaseFaultKind — and asserts that this build either EVALUATES each
// identifier (a scenario here produces it) or RESERVES it with the ticket that
// will. A value the contract lists and this test does not name fails: silently
// unimplemented is the one outcome a closed vocabulary must not allow.
func TestEveryContractIdentifierIsEvaluatedOrReserved(t *testing.T) {
	newest := rel("newest", "0.3.0", commitC, 74, at(3))
	above := rel("above", "0.4.0", commitC, 80, at(4))

	// Each evaluated reason with a plan that produces it, and the target it
	// lands on: 0 = the control plane, 1 = the single host.
	evaluated := []struct {
		reason string
		in     PlanInputs
		target int
	}{
		{ReasonNoRelease, PlanInputs{Channel: ChannelStable, ControlPlane: cp(commitA, 74),
			Hosts: []HostIdentity{host("h1", "gpu-01", commitA)}}, 0},
		{ReasonIdentityUnknown, PlanInputs{Channel: ChannelStable, ControlPlane: cp("", 74),
			Hosts: []HostIdentity{host("h1", "gpu-01", commitA, unknownIdentity)}, Releases: []Release{newest}}, 1},
		{ReasonUpToDate, PlanInputs{Channel: ChannelStable, ControlPlane: cp(commitC, 74),
			Hosts: []HostIdentity{host("h1", "gpu-01", commitC)}, Releases: []Release{newest}}, 1},
		{ReasonInstallModeSource, PlanInputs{Channel: ChannelStable, ControlPlane: cp(commitC, 74),
			Hosts: []HostIdentity{host("h1", "gpu-01", commitA, sourceInstall)}, Releases: []Release{newest}}, 1},
		{ReasonUpdaterAbsent, PlanInputs{Channel: ChannelStable, ControlPlane: cp(commitC, 74),
			Hosts: []HostIdentity{host("h1", "gpu-01", commitA, noUpdater)}, Releases: []Release{newest}}, 1},
		{ReasonHostOffline, PlanInputs{Channel: ChannelStable, ControlPlane: cp(commitC, 74),
			Hosts: []HostIdentity{host("h1", "gpu-01", commitA, offline)}, Releases: []Release{newest}}, 1},
		{ReasonReleaseAboveControlPlane, PlanInputs{Channel: ChannelStable, ControlPlane: cp(commitA, 74),
			Hosts: []HostIdentity{host("h1", "gpu-01", commitA)}, Releases: []Release{above}}, 1},
		{ReasonControlPlaneNotFirst, PlanInputs{Channel: ChannelStable, ControlPlane: cp(commitA, 74),
			Hosts: []HostIdentity{host("h1", "gpu-01", commitB)}, Releases: []Release{newest}}, 1},
	}

	// Amendment 2 appends these two at the END of the precedence. They need
	// apply state this build has no table for; they must not appear until then.
	reserved := map[string]string{
		ReasonAttemptInFlight: "#116",
		ReasonRunActive:       "#117",
	}

	seen := map[string]bool{}
	for _, e := range evaluated {
		t.Run(e.reason, func(t *testing.T) {
			got := PlanRelease(e.in).Targets[e.target]
			if got.Eligible || got.Reason == nil || *got.Reason != e.reason {
				t.Fatalf("want reason %q on target %d, got eligible=%v reason=%v",
					e.reason, e.target, got.Eligible, deref(got.Reason))
			}
		})
		seen[e.reason] = true
	}

	// The contract's enumeration, verbatim and in its fixed precedence order.
	for _, reason := range []string{
		"no_release", "identity_unknown", "up_to_date", "install_mode_source",
		"updater_absent", "host_offline", "release_above_control_plane",
		"control_plane_not_first", "attempt_in_flight", "run_active",
	} {
		if seen[reason] {
			continue
		}
		if _, ok := reserved[reason]; !ok {
			t.Fatalf("EligibilityReason %q is neither evaluated nor reserved with a ticket", reason)
		}
	}

	// A reserved reason must not leak out of any scenario above.
	for _, e := range evaluated {
		for _, target := range PlanRelease(e.in).Targets {
			if target.Reason == nil {
				continue
			}
			if ticket, isReserved := reserved[*target.Reason]; isReserved {
				t.Fatalf("reason %q is reserved for %s but this build emitted it", *target.Reason, ticket)
			}
		}
	}

	t.Run("fault kinds", func(t *testing.T) {
		// agent_ahead_of_control_plane and identity_unknown are raised here;
		// manifest_invalid belongs to detection (detect.go), which is where a
		// release with an unusable manifest is met — the plan never sees one,
		// so it must never raise it either.
		v := PlanRelease(PlanInputs{
			Channel:      ChannelStable,
			ControlPlane: cp(commitA, 74),
			Hosts: []HostIdentity{
				host("h1", "gpu-01", commitC),                  // ahead: on `above`
				host("h2", "gpu-02", commitA, unknownIdentity), // identity unknown
			},
			Releases: []Release{rel("installed", "0.2.0", commitA, 74, at(1)), above},
		})
		kinds := map[string]bool{}
		for _, f := range v.Faults {
			kinds[f.Kind] = true
		}
		if !kinds[FaultAgentAhead] || !kinds[FaultIdentityUnknown] {
			t.Fatalf("faults = %+v, want both agent_ahead_of_control_plane and identity_unknown", v.Faults)
		}
		if kinds[FaultManifestInvalid] {
			t.Fatalf("manifest_invalid is detection's fault to raise, not the plan's: %+v", v.Faults)
		}
	})
}

// A host ahead of the control plane is a fault AND still a target row: the row
// explains the absent button, the fault is what a "needs attention" count reads.
func TestAgentAheadIsAFaultAndStillATargetRow(t *testing.T) {
	above := rel("above", "0.4.0", commitC, 80, at(4))
	v := PlanRelease(PlanInputs{
		Channel:      ChannelStable,
		ControlPlane: cp(commitA, 74),
		Hosts:        []HostIdentity{host("h1", "gpu-01", commitC)},
		Releases:     []Release{rel("installed", "0.2.0", commitA, 74, at(1)), above},
	})
	if len(v.Faults) != 1 || v.Faults[0].Kind != FaultAgentAhead {
		t.Fatalf("faults = %+v, want one agent_ahead_of_control_plane", v.Faults)
	}
	if v.Faults[0].Detail == "" {
		t.Fatal("a fault must carry operator prose")
	}
	// The host is ON the newest listed release, so its row reads up_to_date
	// while the fault says it should never have got there: the two surfaces say
	// different things about the same host on purpose — a fault gates nothing.
	if v.Targets[1].Reason == nil || *v.Targets[1].Reason != ReasonUpToDate {
		t.Fatalf("host target = %+v, want up_to_date", v.Targets[1])
	}
}
