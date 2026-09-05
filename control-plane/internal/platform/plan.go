package platform

import (
	"fmt"
	"sort"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/buildinfo"
)

// Everything the release view decides, decided in one pure function.
// PlanRelease does no I/O: the caller gathers every read and hands it in, so
// the ADR 0002 ordering, the eligibility precedence and the fault rules stay a
// table test. Same shape as internal/session/stream_plan.go.

// PlanInputs is every fact the view is computed from.
type PlanInputs struct {
	// EdgeBranch is reported on both channels so a UI needs no second read.
	Channel    string
	EdgeBranch string

	ControlPlane buildinfo.Identity

	// In the order the host list uses: `targets` and `installed.hosts` must
	// agree with GET /v1/hosts.
	Hosts []HostIdentity

	// Every row the caller read; the other channel's are filtered out here
	// rather than in SQL, beside the rule that orders them.
	Releases []Release

	// OpenAttempts is every non-terminal platform_apply_attempts row on the
	// instance (amendment 2). It feeds both `active_apply` and the
	// `attempt_in_flight` eligibility reason, from one read.
	OpenAttempts []Attempt

	// CheckedAt is when detection last SUCCEEDED. Orthogonal to LastError: a
	// stale CheckedAt with an error is the normal "failing since then".
	CheckedAt *time.Time
	LastError *string
}

// PlanRelease computes the whole view.
func PlanRelease(in PlanInputs) View {
	channel := in.Channel
	if !ValidChannel(channel) {
		channel = ChannelStable // unreachable past the column CHECK; stable is the safe read
	}

	available := offerable(in.Releases, channel, in.ControlPlane)
	hosts := withDerivedIdentity(in.Hosts)
	open := openTargets(in.OpenAttempts)

	v := View{
		Channel:    channel,
		EdgeBranch: in.EdgeBranch,
		CheckedAt:  rfc3339OrNil(in.CheckedAt),
		LastError:  in.LastError,
		Installed: Installed{
			ControlPlane: in.ControlPlane,
			Hosts:        hosts,
		},
		Available: available,
		Targets:   targets(available, in.ControlPlane, hosts, open),
		Faults:    faults(in.Releases, channel, in.ControlPlane, hosts),
		// Always serialized, `null` when nothing is in flight: null is the
		// answer, not the absence of one.
		ActiveApply: activeApply(in.OpenAttempts),
	}
	return v
}

// openTargets keys the open attempts the way `targets` reads them: by host id,
// with "" standing for the control-plane target — the same collapse the
// database's partial unique index makes with the zero uuid.
func openTargets(attempts []Attempt) map[string]bool {
	out := make(map[string]bool, len(attempts))
	for _, a := range attempts {
		if a.HostID == nil {
			out[""] = true
			continue
		}
		out[*a.HostID] = true
	}
	return out
}

// activeApply is the view's `active_apply`. `run` stays nil here: no fleet run
// can exist until #117 creates one, and this build creates none.
func activeApply(attempts []Attempt) *ActiveApply {
	if len(attempts) == 0 {
		return nil
	}
	return &ActiveApply{Attempts: attempts}
}

// offerable applies the three listing rules and the ordering: schema_version
// then built_at, both DESC (ADR 0002). The built_at tiebreak matters because
// edge produces many builds at one schema_version; id keeps a list stable
// across reads rather than dependent on the scan order.
func offerable(rows []Release, channel string, cp buildinfo.Identity) []Release {
	out := make([]Release, 0, len(rows))
	for _, r := range rows {
		if r.Channel != channel {
			continue
		}
		// ADR 0002: a downgrade must be unrepresentable, not merely discouraged.
		if r.SchemaVersion < cp.SchemaVersion {
			continue
		}
		if channel == ChannelStable {
			// Prereleases exist to be exercised; stable ignores them.
			if r.Prerelease {
				continue
			}
			// Nothing to pin it by (ADR 0001).
			if len(r.Manifest) == 0 {
				continue
			}
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.SchemaVersion != b.SchemaVersion {
			return a.SchemaVersion > b.SchemaVersion
		}
		if !a.BuiltAt.Equal(b.BuiltAt) {
			return a.BuiltAt.After(b.BuiltAt)
		}
		return a.ID > b.ID
	})
	return out
}

// withDerivedIdentity fills identity_known, which is served, never re-derived.
func withDerivedIdentity(hosts []HostIdentity) []HostIdentity {
	out := make([]HostIdentity, 0, len(hosts))
	for _, h := range hosts {
		h.IdentityKnown = h.Known()
		out = append(out, h)
	}
	return out
}

// targets evaluates every target against available[0] and nothing else: this
// surface carries no per-release eligibility matrix.
func targets(available []Release, cp buildinfo.Identity, hosts []HostIdentity, open map[string]bool) []Target {
	var newest *Release
	if len(available) > 0 {
		newest = &available[0]
	}

	out := make([]Target, 0, len(hosts)+1)
	out = append(out, target(TargetControlPlane, nil, nil, controlPlaneReason(newest, cp, open[""])))
	for i := range hosts {
		h := hosts[i]
		hostID, nodeName := h.HostID, h.NodeName
		out = append(out, target(TargetHost, &hostID, &nodeName, hostReason(newest, cp, h, open[hostID])))
	}
	return out
}

func target(kind string, hostID, nodeName *string, reason string) Target {
	t := Target{Kind: kind, HostID: hostID, NodeName: nodeName}
	if reason == "" {
		t.Eligible = true
		return t
	}
	r := reason
	t.Reason = &r
	return t
}

// controlPlaneReason: "" means eligible. Only the reasons that apply to every
// target kind can appear here; the four host-only ones describe an install this
// process does not have.
func controlPlaneReason(newest *Release, cp buildinfo.Identity, attemptOpen bool) string {
	if newest == nil {
		return ReasonNoRelease
	}
	// An unstamped build has no commit to say "already on it" about.
	if cp.SourceCommit == nil {
		return ReasonIdentityUnknown
	}
	if commitsMatch(*cp.SourceCommit, newest.SourceCommit) {
		return ReasonUpToDate
	}
	// The most transient fact on the list, so it comes last (amendment 2).
	if attemptOpen {
		return ReasonAttemptInFlight
	}
	return ""
}

// hostReason: "" means eligible. The contract fixes the precedence as the order
// below, durable facts outranking transient ones: an offline source-built host
// reports install_mode_source, because reconnecting would not change it.
func hostReason(newest *Release, cp buildinfo.Identity, h HostIdentity, attemptOpen bool) string {
	if newest == nil {
		return ReasonNoRelease
	}
	if !h.Known() {
		return ReasonIdentityUnknown
	}
	if commitsMatch(*h.SourceCommit, newest.SourceCommit) {
		return ReasonUpToDate
	}
	// Its images were never pulled, so there is nothing to re-pin.
	if *h.InstallMode == InstallSource {
		return ReasonInstallModeSource
	}
	// Known() makes this the real "an agent looked and found none", never
	// "nobody has said".
	if !*h.UpdaterPresent {
		return ReasonUpdaterAbsent
	}
	if h.Status == HostOffline {
		return ReasonHostOffline
	}
	// A ceiling, not a queue: an agent is never moved past the control plane
	// (ADR 0002), and this stands until the control plane moves.
	if newest.SchemaVersion > cp.SchemaVersion {
		return ReasonReleaseAboveControlPlane
	}
	// Ordering, not a ceiling: equal schema, different commit. Apply the
	// control plane and this clears.
	if cp.SourceCommit == nil || !commitsMatch(*cp.SourceCommit, newest.SourceCommit) {
		return ReasonControlPlaneNotFirst
	}
	// attempt_in_flight (9) — amendment 2. run_active (10) belongs after it,
	// and is #117's: no fleet run can exist until it creates one.
	if attemptOpen {
		return ReasonAttemptInFlight
	}
	return ""
}

// faults reports everything wrong that is not an ineligibility; a fault gates
// nothing.
//
// manifest_invalid is not raised here: a manifest that fails validation carries
// no trustworthy commit, built_at or schema_version, all three NOT NULL, so the
// release is never stored and the detector reports the broken publish in its own
// run record instead of inventing an identity (detect.go).
func faults(rows []Release, channel string, cp buildinfo.Identity, hosts []HostIdentity) []Fault {
	out := make([]Fault, 0)

	// What "above the control plane" is measured against, when it is known.
	var cpRelease *Release
	if cp.SourceCommit != nil {
		for i := range rows {
			if rows[i].Channel == channel && commitsMatch(rows[i].SourceCommit, *cp.SourceCommit) {
				cpRelease = &rows[i]
				break
			}
		}
	}

	for _, h := range hosts {
		if !h.Known() {
			hostID, nodeName := h.HostID, h.NodeName
			out = append(out, Fault{
				Kind:     FaultIdentityUnknown,
				HostID:   &hostID,
				NodeName: &nodeName,
				Detail: "the agent has not reported its build identity — it predates the " +
					"platform-release amendment, or could not determine its own install",
			})
			continue
		}
		hostRelease := matchRelease(rows, channel, *h.SourceCommit)
		// Unordered is not ahead: a commit matching no known release raises nothing.
		if hostRelease == nil || !ordersAbove(*hostRelease, cpRelease, cp) {
			continue
		}
		hostID, nodeName := h.HostID, h.NodeName
		out = append(out, Fault{
			Kind:     FaultAgentAhead,
			HostID:   &hostID,
			NodeName: &nodeName,
			Detail: fmt.Sprintf("the agent is on release %s (schema %d), which is ahead of this control plane (schema %d); "+
				"ADR 0002 applies the control plane first",
				releaseLabel(*hostRelease), hostRelease.SchemaVersion, cp.SchemaVersion),
		})
	}
	return out
}

// matchRelease finds the channel's row for a commit, tolerating a short one.
func matchRelease(rows []Release, channel, commit string) *Release {
	for i := range rows {
		if rows[i].Channel == channel && commitsMatch(rows[i].SourceCommit, commit) {
			return &rows[i]
		}
	}
	return nil
}

// ordersAbove compares in the ordering `available` uses. With no known row for
// the control plane there is no built_at to compare, so it falls back to
// schema_version, the key that always exists.
func ordersAbove(r Release, cpRelease *Release, cp buildinfo.Identity) bool {
	if cpRelease == nil {
		return r.SchemaVersion > cp.SchemaVersion
	}
	if r.SchemaVersion != cpRelease.SchemaVersion {
		return r.SchemaVersion > cpRelease.SchemaVersion
	}
	return r.BuiltAt.After(cpRelease.BuiltAt)
}

// releaseLabel names a release for operator prose: a version on stable, a short
// commit on edge, which has none by design.
func releaseLabel(r Release) string {
	if r.Version != nil && *r.Version != "" {
		return *r.Version
	}
	c := r.SourceCommit
	if len(c) > 12 {
		c = c[:12]
	}
	return c
}

func rfc3339OrNil(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
}
