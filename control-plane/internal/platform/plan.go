package platform

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/buildinfo"
	"github.com/accreleus/quasar/control-plane/internal/semver"
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
	// SourceRepo is the configured release repository ("owner/name"), passed
	// through to the view so a client composes GitHub links from the repo
	// detection reads. "" means detection is off.
	SourceRepo string

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
	// ActiveRun is the fleet run that owns the fleet right now, or nil: both
	// `active_apply.run` and the `run_active` eligibility reason.
	ActiveRun *ApplyRun
	// UpdaterPresent is whether an updater sits beside THIS control plane. A
	// host's is reported by its agent; this one is a local fact, so the caller
	// reads it and hands it in.
	UpdaterPresent bool
	// ControlPlaneInstallMode is how THIS control plane got its image, read
	// from its own updater. nil is "nobody could say" and is never treated as
	// registry: a source-built control plane offered a registry image is a
	// crash-loop with no console left to fix it from.
	ControlPlaneInstallMode *string

	// CheckedAt is when detection last SUCCEEDED. Orthogonal to LastError: a
	// stale CheckedAt with an error is the normal "failing since then".
	CheckedAt *time.Time
	LastError *string

	// Passed through untouched: the notification surface is config, not a
	// release decision.
	ReleaseWebhook *WebhookStatus
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
	fleet := fleetState{
		runActive:      in.ActiveRun != nil,
		updaterPresent: in.UpdaterPresent,
		installMode:    in.ControlPlaneInstallMode,
	}

	v := View{
		Channel:    channel,
		SourceRepo: in.SourceRepo,
		EdgeBranch: in.EdgeBranch,
		CheckedAt:  rfc3339OrNil(in.CheckedAt),
		LastError:  in.LastError,
		Installed: Installed{
			ControlPlane: in.ControlPlane,
			Hosts:        hosts,
		},
		Available: available,
		Targets:   targets(available, in.ControlPlane, hosts, open, fleet),
		// Faults are read off the rows the channel SELECTS, which on beta are
		// the stable channel's (rowChannel), and ordered the way `available`
		// orders them on that channel.
		Faults: faults(in.Releases, channel, in.ControlPlane, hosts),
		// Always serialized, `null` when nothing is in flight: null is the
		// answer, not the absence of one.
		ActiveApply:    activeApply(in.ActiveRun, in.OpenAttempts),
		ReleaseWebhook: in.ReleaseWebhook,
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

// fleetState is the two instance-wide facts the eligibility rule needs beyond
// the per-target ones.
type fleetState struct {
	runActive      bool
	updaterPresent bool
	// This control plane's own install mode; nil is unknown.
	installMode *string
}

// activeApply is the view's `active_apply`: null when nothing is in flight.
func activeApply(run *ApplyRun, attempts []Attempt) *ActiveApply {
	if run == nil && len(attempts) == 0 {
		return nil
	}
	if attempts == nil {
		attempts = []Attempt{}
	}
	return &ActiveApply{Run: run, Attempts: attempts}
}

// offerable applies the listing rules and the ordering: schema_version then
// built_at, both DESC (ADR 0002). The built_at tiebreak matters because edge
// produces many builds at one schema_version; id keeps a list stable across
// reads rather than dependent on the scan order.
//
// Beta inserts semver precedence between those two keys, and only beta: it is
// the only channel whose rows can arrive out of version order, because an rc is
// cut from `develop` while a patch is cut from `main`, so 0.3.0-rc.1 can be
// built before the 0.2.5 that orders below it. Edge builds have no version at
// all.
func offerable(rows []Release, channel string, cp buildinfo.Identity) []Release {
	source := rowChannel(channel)
	out := make([]Release, 0, len(rows))
	for _, r := range rows {
		if r.Channel != source {
			continue
		}
		// ADR 0002: a downgrade must be unrepresentable, not merely discouraged.
		if r.SchemaVersion < cp.SchemaVersion {
			continue
		}
		if belowInstalledVersion(r, cp) {
			continue
		}
		// Prereleases exist to be exercised; stable ignores them. Beta is the
		// channel that does not.
		if channel == ChannelStable && r.Prerelease {
			continue
		}
		// Nothing to pin it by (ADR 0001). Edge is exempt because it publishes
		// no manifest at all and resolves its digests at apply time.
		if channel != ChannelEdge && len(r.Manifest) == 0 {
			continue
		}
		out = append(out, r)
	}
	return sortOfferable(out, channel)
}

// ranked is one row with its precedence key computed ONCE, before the sort.
// Consulting semver only for pairs where BOTH versions parse is not a total
// order: with a parseable pair ordered by version and every mixed pair ordered
// by built_at, three rows can form a cycle, and the winner then depends on the
// scan order the rows arrived in (Store.Releases has no ORDER BY).
type ranked struct {
	release Release
	version semver.Full
	// parsed=false is "carries no version this build can order by", which ranks
	// strictly below every row that does rather than comparing pairwise.
	parsed bool
}

// sortOfferable is the ADR 0002 ordering: schema_version DESC, then built_at
// DESC, with id as the last tiebreak so a list is stable across reads. Beta
// inserts semver precedence between the first two keys — for beta only, so
// stable and edge order exactly as they did before the channel existed.
func sortOfferable(rows []Release, channel string) []Release {
	keyed := make([]ranked, len(rows))
	for i, r := range rows {
		k := ranked{release: r}
		if channel == ChannelBeta {
			k.version, k.parsed = parseVersion(r.Version)
		}
		keyed[i] = k
	}
	sort.SliceStable(keyed, func(i, j int) bool {
		a, b := keyed[i], keyed[j]
		if a.release.SchemaVersion != b.release.SchemaVersion {
			return a.release.SchemaVersion > b.release.SchemaVersion
		}
		// Every parseable row above every unparseable one, and built_at breaking
		// ties INSIDE each group: that is what makes the comparator transitive.
		if a.parsed != b.parsed {
			return a.parsed
		}
		if a.parsed && b.parsed {
			if c := semver.ComparePrecedence(a.version, b.version); c != 0 {
				return c > 0
			}
		}
		if !a.release.BuiltAt.Equal(b.release.BuiltAt) {
			return a.release.BuiltAt.After(b.release.BuiltAt)
		}
		return a.release.ID > b.release.ID
	})
	out := make([]Release, len(keyed))
	for i := range keyed {
		out[i] = keyed[i].release
	}
	return out
}

// comparePrecedence orders two release versions by SemVer 2.0.0 §11 precedence.
// ok=false when either is absent or does not parse, which is the caller's cue to
// fall back to built_at rather than to invent an order.
func comparePrecedence(a, b *string) (int, bool) {
	va, okA := parseVersion(a)
	vb, okB := parseVersion(b)
	if !okA || !okB {
		return 0, false
	}
	return semver.ComparePrecedence(va, vb), true
}

// parseVersion is the one place a row's version becomes an ordering key.
// ok=false covers absent, empty (edge rows) and unparseable alike, because all
// three mean the same thing to every caller: there is no version to order by.
func parseVersion(v *string) (semver.Full, bool) {
	if v == nil || strings.TrimSpace(*v) == "" {
		return semver.Full{}, false
	}
	return semver.ParseFull(*v)
}

// belowInstalledVersion is the switch-back rule: no channel offers a build that
// orders below the installed one, so leaving beta waits for stable to catch up
// rather than rolling the control plane back. Filtered here, not answered as an
// eligibility reason, so the downgrade is unrepresentable: it never reaches
// available[0] and apply_handler.offered reads the same list.
//
// Scoped to an installed PRERELEASE, which is the only way an install can be
// above what its channel lists — `make release` cuts stable versions
// monotonically from a clean `main` — so stable and edge see no change. Edge
// rows carry no version; edgeOlderThanInstalled covers them. Equal
// schema_version only: a newer schema still wins (ADR 0002).
func belowInstalledVersion(r Release, cp buildinfo.Identity) bool {
	if r.Version == nil || r.SchemaVersion != cp.SchemaVersion {
		return false
	}
	installed, ok := semver.ParseFull(cp.Version)
	if !ok || !installed.IsPrerelease() {
		return false
	}
	candidate, ok := semver.ParseFull(*r.Version)
	if !ok {
		return false
	}
	// Strictly below: the equal version is the one the instance is running, and
	// must stay listed for up_to_date to be evaluated against it.
	return semver.ComparePrecedence(candidate, installed) < 0
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
func targets(available []Release, cp buildinfo.Identity, hosts []HostIdentity, open map[string]bool, fleet fleetState) []Target {
	var newest *Release
	if len(available) > 0 {
		newest = &available[0]
	}

	out := make([]Target, 0, len(hosts)+1)
	out = append(out, target(TargetControlPlane, nil, nil, controlPlaneReason(newest, cp, open[""], fleet)))
	for i := range hosts {
		h := hosts[i]
		hostID, nodeName := h.HostID, h.NodeName
		out = append(out, target(TargetHost, &hostID, &nodeName, hostReason(newest, cp, h, open[hostID], fleet)))
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
func controlPlaneReason(newest *Release, cp buildinfo.Identity, attemptOpen bool, fleet fleetState) string {
	if newest == nil {
		return ReasonNoRelease
	}
	// An unstamped build has no commit to say "already on it" about.
	if cp.SourceCommit == nil {
		return ReasonIdentityUnknown
	}
	if commitsMatch(*cp.SourceCommit, newest.SourceCommit) || edgeOlderThanInstalled(*newest, cp) {
		return ReasonUpToDate
	}
	// Nothing beside this control plane could carry an apply out — and with no
	// updater there is nothing to learn the install mode from either, so this
	// answer comes first even though the contract's order lists it later.
	if !fleet.updaterPresent {
		return ReasonUpdaterAbsent
	}
	// Its image was never pulled, so there is nothing to re-pin, and the
	// registry image is a DIFFERENT build: replacing a source-built control
	// plane with it leaves a container that starts and then cannot write its
	// own state.
	if fleet.installMode == nil {
		return ReasonIdentityUnknown
	}
	if *fleet.installMode == InstallSource {
		return ReasonInstallModeSource
	}
	// The two most transient facts on the list, so they come last (amendment 2).
	if attemptOpen {
		return ReasonAttemptInFlight
	}
	if fleet.runActive {
		return ReasonRunActive
	}
	return ""
}

// edgeOlderThanInstalled refines ADR 0002 only within an equal schema version.
// The installed binary's timestamp is authoritative even after switching from
// stable: there may be no corresponding release row on the edge channel. Missing
// timestamps preserve legacy advisory behavior, and newer schemas still win.
func edgeOlderThanInstalled(release Release, cp buildinfo.Identity) bool {
	if release.Channel != ChannelEdge || release.SchemaVersion != cp.SchemaVersion || cp.BuiltAt == nil {
		return false
	}
	installedAt, err := time.Parse(time.RFC3339, *cp.BuiltAt)
	return err == nil && !release.BuiltAt.IsZero() && release.BuiltAt.Before(installedAt)
}

// hostReason: "" means eligible. The contract fixes the precedence as the order
// below, durable facts outranking transient ones: an offline source-built host
// reports install_mode_source, because reconnecting would not change it.
func hostReason(newest *Release, cp buildinfo.Identity, h HostIdentity, attemptOpen bool, fleet fleetState) string {
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
	// attempt_in_flight (9) then run_active (10) — the end of amendment 2's
	// precedence, because they are the most transient facts on it.
	if attemptOpen {
		return ReasonAttemptInFlight
	}
	if fleet.runActive {
		return ReasonRunActive
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
// `channel` is the instance's channel, not the platform_releases.channel value:
// the rows come from rowChannel(channel) — on beta those are not the same — and
// the channel itself is what decides the ordering the agent_ahead comparison
// uses, so that a fault says the same thing `available` does.
func faults(rows []Release, channel string, cp buildinfo.Identity, hosts []HostIdentity) []Fault {
	out := make([]Fault, 0)
	source := rowChannel(channel)

	// What "above the control plane" is measured against, when it is known.
	var cpRelease *Release
	if cp.SourceCommit != nil {
		for i := range rows {
			if rows[i].Channel == source && commitsMatch(rows[i].SourceCommit, *cp.SourceCommit) {
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
		hostRelease := matchRelease(rows, source, *h.SourceCommit)
		// Unordered is not ahead: a commit matching no known release raises nothing.
		if hostRelease == nil || !ordersAbove(*hostRelease, cpRelease, cp, channel) {
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

// matchRelease finds the row for a commit among those a channel reads,
// tolerating a short commit. `source` is a rowChannel value, never a raw channel.
func matchRelease(rows []Release, source, commit string) *Release {
	for i := range rows {
		if rows[i].Channel == source && commitsMatch(rows[i].SourceCommit, commit) {
			return &rows[i]
		}
	}
	return nil
}

// ordersAbove compares in the ordering `available` uses — including on beta,
// where that means semver precedence at an equal schema_version and NOT build
// time: an rc cut from `develop` can be built before the patch release it orders
// above, so comparing built_at would miss an agent that really is ahead. With no
// known row for the control plane there is no built_at to compare, so it falls
// back to schema_version, the key that always exists.
func ordersAbove(r Release, cpRelease *Release, cp buildinfo.Identity, channel string) bool {
	if cpRelease == nil {
		return r.SchemaVersion > cp.SchemaVersion
	}
	if r.SchemaVersion != cpRelease.SchemaVersion {
		return r.SchemaVersion > cpRelease.SchemaVersion
	}
	if channel == ChannelBeta {
		if c, ok := comparePrecedence(r.Version, cpRelease.Version); ok && c != 0 {
			return c > 0
		}
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
