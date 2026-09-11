package platform

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/buildinfo"
)

// The wire shapes of openapi.yaml `PlatformReleaseView` and friends. Structs
// rather than maps so a renamed field is a compile error; every field
// serializes always, because a client must read `null` and never an absent key.

// Channels (schema.md instance_settings.release_channel).
const (
	ChannelStable = "stable"
	ChannelEdge   = "edge"

	// Beta offers stable's releases AND the prereleases among them. It stores
	// no rows of its own: detection already caches every published release,
	// prerelease or not, as a `stable` row carrying its `prerelease` flag, and
	// stable declines to list the flagged ones. `platform_releases.channel`
	// therefore stays CHECK IN ('stable','edge'), and switching to or from beta
	// re-detects and writes nothing.
	ChannelBeta = "beta"
)

// ValidChannel reports whether c is one of the three channels.
func ValidChannel(c string) bool {
	return c == ChannelStable || c == ChannelEdge || c == ChannelBeta
}

// rowChannel maps a channel to the `platform_releases.channel` value whose rows
// it selects: beta reads stable's, every other channel its own. Every
// channel-keyed read of a release row goes through here.
func rowChannel(channel string) string {
	if channel == ChannelBeta {
		return ChannelStable
	}
	return channel
}

// Release is one `platform_releases` row and the `PlatformRelease` wire shape.
type Release struct {
	ID            string    `json:"id"`
	Channel       string    `json:"channel"`
	Version       *string   `json:"version"`
	SourceCommit  string    `json:"source_commit"`
	BuiltAt       time.Time `json:"built_at"`
	SchemaVersion int       `json:"schema_version"`
	Prerelease    bool      `json:"prerelease"`
	Notes         string    `json:"notes"`
	CompareURL    *string   `json:"compare_url"`
	// Migrates is `schema_version` above the control plane's — i.e. applying
	// this release runs at least one migration here, which is the ONLY case in
	// which the fleet's control-plane step drains the instance (#153). DERIVED
	// AND SERVED, never left to a client to re-derive, for the same reason
	// HostIdentity.IdentityKnown is: the drain policy is the server's, and a
	// client twin of it would go on telling an operator what they are consenting
	// to after the policy moved. Set in `offerable`, which is where the control
	// plane's own schema version is in hand; a Release read straight from the
	// store and never served leaves it false.
	Migrates bool `json:"migrates"`
	// The asset verbatim: raw so a field this build does not read still reaches
	// a client. nil marshals to `null` — the answer on edge.
	Manifest     json.RawMessage `json:"manifest"`
	DiscoveredAt time.Time       `json:"discovered_at"`
}

// HostIdentity is one host's installed identity (`PlatformHostIdentity`).
// IdentityKnown is derived and served, never left to a client to re-derive.
type HostIdentity struct {
	HostID         string  `json:"host_id"`
	NodeName       string  `json:"node_name"`
	Status         string  `json:"status"`
	AgentVersion   *string `json:"agent_version"`
	SourceCommit   *string `json:"source_commit"`
	BuiltAt        *string `json:"built_at"`
	InstallMode    *string `json:"install_mode"`
	UpdaterPresent *bool   `json:"updater_present"`
	IdentityKnown  bool    `json:"identity_known"`
	// AgentConnected is whether this host's agent has a live socket to THIS
	// control-plane process, as the agent registry sees it right now.
	//
	// NOT SERIALIZED, deliberately: `PlatformHostIdentity` is a frozen shape and
	// this is an input to the eligibility decision, not a new field on the wire.
	//
	// It exists because `status` cannot answer the question. Nothing corrects an
	// idle host's status across a control-plane restart: `markOffline` runs only
	// from the connection goroutine's defer, so a control plane that exits never
	// runs it, and the stale sweep only visits hosts WITH ACTIVE SESSIONS. Every
	// fleet run contains a control-plane restart, so "the row says online but no
	// agent is there" is the normal shape of a run, not an edge case (#169).
	//
	// nil = unknown (no registry wired), and the column is trusted instead —
	// the same seam `UncordonHost` and the per-host apply runner already use.
	AgentConnected *bool `json:"-"`

	// The host's last stored readiness report (hosts.readiness, raw) and when
	// it changed: inputs to the preflight decision, not fields on the frozen
	// identity shape, hence unserialized like AgentConnected.
	Readiness           json.RawMessage `json:"-"`
	ReadinessReportedAt *time.Time      `json:"-"`
}

// Known is `identity_known`: all four fields present. A host with any of them
// absent is never eligible for an apply.
func (h HostIdentity) Known() bool {
	return h.SourceCommit != nil && h.BuiltAt != nil && h.InstallMode != nil && h.UpdaterPresent != nil
}

// `draining` is NOT offline: a cordon is the condition an apply wants.
const (
	HostOffline = "offline"
)

// Install modes (schema.md hosts.install_mode).
const (
	InstallRegistry = "registry"
	InstallSource   = "source"
)

// Target kinds.
const (
	TargetControlPlane = "control_plane"
	TargetHost         = "host"
)

// The closed `EligibilityReason` vocabulary, in the contract's fixed precedence
// order. The server never sends the sentence; a client maps these to text.
const (
	ReasonNoRelease                = "no_release"
	ReasonIdentityUnknown          = "identity_unknown"
	ReasonUpToDate                 = "up_to_date"
	ReasonInstallModeSource        = "install_mode_source"
	ReasonUpdaterAbsent            = "updater_absent"
	ReasonHostOffline              = "host_offline"
	ReasonReleaseAboveControlPlane = "release_above_control_plane"
	ReasonControlPlaneNotFirst     = "control_plane_not_first"

	// Before the two transient reasons: a stack shape is a durable fact.
	// Produced only by a preflight whose state is `blocked`; `unknown` never
	// blocks (preflight.go).
	ReasonPreflightBlocked = "preflight_blocked"

	// Amendment 2 appends these two at the END of the order. They need apply
	// state this build has no table for; #116 evaluates them.
	ReasonAttemptInFlight = "attempt_in_flight"
	ReasonRunActive       = "run_active"
)

// Target is one target's eligibility, evaluated against available[0] only.
type Target struct {
	Kind     string  `json:"kind"`
	HostID   *string `json:"host_id"`
	NodeName *string `json:"node_name"`
	Eligible bool    `json:"eligible"`
	Reason   *string `json:"reason"`
	// Preflight: CONTEXT.md. Answered beside Eligible so the card can name the fix.
	Preflight Preflight `json:"preflight"`
}

// The closed `PlatformReleaseFaultKind` vocabulary. A fault gates nothing; it
// is reported so a wrong state is visible instead of silent.
const (
	FaultAgentAhead      = "agent_ahead_of_control_plane"
	FaultIdentityUnknown = "identity_unknown"
	FaultManifestInvalid = "manifest_invalid"
)

// Fault is one `PlatformReleaseFault`. host_id/node_name are null on an
// instance-scoped fault; detail is operator prose and is never parsed.
type Fault struct {
	Kind     string  `json:"kind"`
	HostID   *string `json:"host_id"`
	NodeName *string `json:"node_name"`
	Detail   string  `json:"detail"`
}

// Installed is the `installed` object of the view.
type Installed struct {
	ControlPlane buildinfo.Identity `json:"control_plane"`
	Hosts        []HostIdentity     `json:"hosts"`
}

// View is the whole `GET /v1/admin/platform/releases` body. `active_apply` is
// ALWAYS serialized now that the apply half is served (#116): `null` is the
// answer, not the absence of one.
type View struct {
	Channel string `json:"channel"`
	// SourceRepo is the configured release repository as "owner/name", so the
	// console composes the release / commit / issue links from the repository
	// detection actually reads instead of hard-coding one. "" when detection is
	// switched off — the client renders no links rather than the default.
	SourceRepo string    `json:"source_repo"`
	EdgeBranch string    `json:"edge_branch"`
	CheckedAt  *string   `json:"checked_at"`
	LastError  *string   `json:"last_error"`
	Installed  Installed `json:"installed"`
	Available  []Release `json:"available"`
	Targets    []Target  `json:"targets"`
	Faults     []Fault   `json:"faults"`
	// Every open attempt on the instance, plus the active fleet run (#117).
	// A client joins an attempt to a target by host_id.
	ActiveApply *ActiveApply `json:"active_apply"`
	// Outbound notification config + last delivery (#123). Null on a build with
	// no notification store wired.
	ReleaseWebhook *WebhookStatus `json:"release_webhook"`
}

// UpdateAvailable is the newest listed release when it is a step FORWARD from
// the installed control plane, and false otherwise.
//
// `available` alone is not the answer: a current instance still lists the
// release it is running, so that `up_to_date` can be evaluated against it.
// Client twin: web/src/pages/admin/fleet/releasesCopy.ts hasUpdate.
func (v View) UpdateAvailable() (*Release, bool) {
	if len(v.Available) == 0 {
		return nil, false
	}
	newest := v.Available[0]
	cp := v.Installed.ControlPlane
	if edgeOlderThanInstalled(newest, cp) {
		return nil, false
	}
	// An unstamped build has no commit to be "already on it" about, so the
	// listed release is news.
	if cp.SourceCommit == nil {
		return &newest, true
	}
	if commitsMatch(*cp.SourceCommit, newest.SourceCommit) {
		return nil, false
	}
	return &newest, true
}

// An agent reports 7-40 hex (agent-api.md) while a manifest carries the full
// 40, so "same commit" must be a prefix match or every short-stamped agent
// reads as perpetually out of date. Empty never matches.
// Client twin: web/src/pages/admin/fleet/releasesCopy.ts commitsMatch.
func commitsMatch(a, b string) bool {
	a, b = strings.ToLower(strings.TrimSpace(a)), strings.ToLower(strings.TrimSpace(b))
	if a == "" || b == "" {
		return false
	}
	if len(a) > len(b) {
		a, b = b, a
	}
	return strings.HasPrefix(b, a)
}
