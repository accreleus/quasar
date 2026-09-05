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
)

// ValidChannel reports whether c is one of the two channels.
func ValidChannel(c string) bool { return c == ChannelStable || c == ChannelEdge }

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
// absent rather than null: the contract keeps it optional so a server without
// the apply half (#116/#117/#118) stays conformant.
type View struct {
	Channel    string    `json:"channel"`
	EdgeBranch string    `json:"edge_branch"`
	CheckedAt  *string   `json:"checked_at"`
	LastError  *string   `json:"last_error"`
	Installed  Installed `json:"installed"`
	Available  []Release `json:"available"`
	Targets    []Target  `json:"targets"`
	Faults     []Fault   `json:"faults"`
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
