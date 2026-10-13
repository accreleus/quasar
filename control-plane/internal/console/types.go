// Package console implements the CM-01 admin per-host console-config surface:
// whether a host runs a console session, a session whose desktop drives the
// host's own display directly (ADR 0009).
// Storage: schema.md `console_config` / `console_capabilities`. Delivery to the
// agent: agent-api.md `config_update.console_config` + capability enumeration
// in `capacity.console_capabilities`. Mirrors the internal/hostcfg package's
// store/resolve/handler shape, but is a distinct structured surface (lists,
// nested selectors) rather than a flat scalar knob catalog, so it carries a
// typed ConsoleConfig for the resolved API response.
package console

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
)

// ConsoleConfig is the resolved (every field has a value) console-mode
// configuration, with the exact json tags of protocol/openapi.yaml
// ConsoleConfig. OutputID / DefaultApp /
// DefaultUser are nullable — nil is a meaningful value ("automatic output" /
// "no app" / "no auto-launch owner"), not "unset".
type ConsoleConfig struct {
	Enabled bool `json:"enabled"`
	// OutputID is the output pick: a card-scoped DRM output id
	// (`cardN:CONNECTOR`) meaning "this card, launch when this connector has
	// a monitor"; nil is automatic (any connected output).
	OutputID *string `json:"output_id"`
	// InputDevices is the allowlist of input nodes passed into the console
	// container; "auto" passes every one.
	InputDevices       InputDevices `json:"input_devices"`
	AutoStartOnDisplay bool         `json:"auto_start_on_display"`
	DefaultApp         *string      `json:"default_app"`
	// DefaultUser is the admin-set owner (users.id) for auto-started console
	// sessions (CM-06 Decision 2, 2026-07-11). auto_start_on_display requires
	// this set — the node-agent does not consume it; control-plane uses it
	// for session ownership only.
	DefaultUser *string `json:"default_user"`
}

// ModeSelection is a physical display mode as the DRM mode names it: the shape
// of a console session's reported mode (agent-api.md session_metrics.console_mode).
type ModeSelection struct {
	Width          uint16 `json:"width"`
	Height         uint16 `json:"height"`
	RefreshMillihz uint32 `json:"refresh_millihz"`
}

// ConsoleVideoTopology is a console session's only output plan: its desktop
// drives the display and is never streamed (agent-api.md session_assign).
const ConsoleVideoTopology = "local_only"

// PinnedConnector returns the connector the level-trigger presence check
// (agentws connectorPresent) keys on (CM-09 item 3), derived from the
// already-validated `output_id` (`cardN:CONNECTOR`, resolve.go
// ValidatePatch). No `output_id` means "auto" (any connector present).
func (c ConsoleConfig) PinnedConnector() string {
	if c.OutputID == nil {
		return "auto"
	}
	_, connector, ok := strings.Cut(*c.OutputID, ":")
	if !ok || connector == "" {
		// output_id is validated card-scoped before storage, so a malformed value
		// here is a pre-validation write or an upstream bug — warn, don't
		// silently disable the pin.
		slog.Warn("console: output_id is set but not card-scoped (cardN:CONNECTOR); falling back to auto", "output_id", *c.OutputID)
		return "auto"
	}
	return connector
}

// InputDevices is "auto" (enumerate connected) or an explicit list of
// /dev/input/event* paths (protocol/openapi.yaml ConsoleConfig.input_devices,
// a oneOf[string enum[auto], array[string]]).
type InputDevices struct {
	Auto  bool
	Paths []string
}

func (d InputDevices) MarshalJSON() ([]byte, error) {
	if d.Auto || d.Paths == nil {
		return json.Marshal("auto")
	}
	return json.Marshal(d.Paths)
}

func (d *InputDevices) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		if s != "auto" {
			return fmt.Errorf("input_devices string value must be %q", "auto")
		}
		d.Auto = true
		d.Paths = nil
		return nil
	}
	var arr []string
	if err := json.Unmarshal(b, &arr); err != nil {
		return fmt.Errorf("input_devices must be %q or an array of strings", "auto")
	}
	d.Auto = false
	d.Paths = arr
	return nil
}

// Capabilities is what the host can do in console mode (agent-api.md
// `capacity.console_capabilities`). Empty arrays if the agent has not reported.
// An older agent's `audio_sinks` is dropped here, never stored or served.
type Capabilities struct {
	Connectors   []string          `json:"connectors"`
	Outputs      []DRMOutput       `json:"outputs,omitempty"`
	InputDevices []InputDevicePath `json:"input_devices"`
	// Access (amendment 18, agent-api.md `capacity.console_capabilities.access`)
	// is the agent's latest console-access report on an owned host. Nil when
	// the agent reports none (a Compose/source install, or a pre-amendment
	// agent) — matches openapi.yaml ConsoleCapabilities.access.
	Access *Access `json:"access,omitempty"`
}

// Access mirrors protocol/openapi.yaml ConsoleAccess (amendment 18) exactly —
// passed through from the agent's report to the admin GET response verbatim.
type Access struct {
	State      string  `json:"state"`
	Target     *bool   `json:"target"`
	RequestID  *string `json:"request_id"`
	Reason     *string `json:"reason"`
	StartedAt  *string `json:"started_at"`
	FinishedAt *string `json:"finished_at"`
	Summary    string  `json:"summary"`
}

// HasAccess is amendment 18's definition (agent-api.md): the host currently
// has console access when the agent is verified `on`, or a failed attempt
// left it `restored` with `target` false (the attempt was trying to turn
// access off and failed, so it still has it). Every other state, and a nil
// report, means no access.
func (a *Access) HasAccess() bool {
	if a == nil {
		return false
	}
	if a.State == "on" {
		return true
	}
	return a.State == "restored" && a.Target != nil && !*a.Target
}

type DRMOutput struct {
	ID         string    `json:"id"`
	Card       string    `json:"card"`
	RenderNode *string   `json:"render_node"`
	Connector  string    `json:"connector"`
	Connected  bool      `json:"connected"`
	ActiveMode *DRMMode  `json:"active_mode"`
	Modes      []DRMMode `json:"modes"`
}

type DRMMode struct {
	Name           string `json:"name"`
	Width          uint16 `json:"width"`
	Height         uint16 `json:"height"`
	RefreshMillihz uint32 `json:"refresh_millihz"`
	Preferred      bool   `json:"preferred"`
	Interlaced     bool   `json:"interlaced"`
	ClockKHz       uint32 `json:"clock_khz"`
	HTotal         uint16 `json:"htotal"`
	VTotal         uint16 `json:"vtotal"`
}

// InputDevicePath is one reported physical input device.
type InputDevicePath struct {
	Path  string `json:"path"`
	Label string `json:"label"`
}

// EmptyCapabilities returns the zero-value capabilities report (empty arrays,
// never nil, so it serializes as `[]` not `null`) — used when a host has no
// console_capabilities row yet (older/offline agent).
func EmptyCapabilities() Capabilities {
	return Capabilities{
		Connectors:   []string{},
		Outputs:      []DRMOutput{},
		InputDevices: []InputDevicePath{},
	}
}
