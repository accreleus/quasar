// Package console implements the CM-01 admin per-host console-config surface:
// local display + local audio + local input ("use the host like a console").
// Storage: schema.md `console_config` / `console_capabilities`. Delivery to the
// agent: agent-api.md `config_update.console_config` (additive) + capability
// enumeration in `capacity.console_capabilities` (additive). Mirrors the
// internal/hostcfg package's store/resolve/handler shape, but is a distinct
// structured surface (lists, nested selectors) rather than a flat scalar knob
// catalog, so it carries a typed ConsoleConfig for the resolved API response.
package console

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
)

// ConsoleConfig is the resolved (every field has a value) console-mode
// configuration, with the exact json tags of protocol/openapi.yaml
// ConsoleConfig. AudioOutput / DefaultApp / DefaultUser are nullable — nil is
// a meaningful value ("no local audio yet" / "no auto-launch app" / "no
// auto-launch owner"), not "unset".
type ConsoleConfig struct {
	Enabled               bool           `json:"enabled"`
	Connector             string         `json:"connector"`
	OutputID              *string        `json:"output_id"`
	Mode                  *ModeSelection `json:"mode"`
	Compositor            string         `json:"compositor"`
	AudioOutput           *string        `json:"audio_output"`
	Stream                bool           `json:"stream"`
	StreamAudio           bool           `json:"stream_audio"`
	InputDevices          InputDevices   `json:"input_devices"`
	Grab                  bool           `json:"grab"`
	AutoStartOnDisplay    bool           `json:"auto_start_on_display"`
	AutoConnectController bool           `json:"auto_connect_controller"`
	DefaultApp            *string        `json:"default_app"`
	// DefaultUser is the admin-set owner (users.id) for auto-started console
	// sessions (CM-06 Decision 2, 2026-07-11). auto_start_on_display requires
	// this set — the node-agent does not consume it; control-plane uses it
	// for session ownership only.
	DefaultUser *string `json:"default_user"`
	Fullscreen  bool    `json:"fullscreen"`
}

type ModeSelection struct {
	Width          uint16 `json:"width"`
	Height         uint16 `json:"height"`
	RefreshMillihz uint32 `json:"refresh_millihz"`
}

// PinnedConnector returns the connector the level-trigger presence check
// (agentws connectorPresent) should key on (CM-09 item 3). `connector` itself
// stays validation-locked to "auto" (resolve.go), so the pin — when one
// exists — is derived from the already-validated `output_id`
// (`cardN:CONNECTOR`, resolve.go ValidatePatch/ValidateOutputSelection). No
// `output_id` means "auto" (today's any-connector-present behavior).
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

// VideoTopology returns the per-session output plan represented by the
// backwards-compatible stream flag. Console mode always has local output;
// stream=true adds WebRTC as a second output.
func (c ConsoleConfig) VideoTopology() string {
	if c.Stream {
		return "dual_output"
	}
	return "local_only"
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
type Capabilities struct {
	Connectors   []string          `json:"connectors"`
	Outputs      []DRMOutput       `json:"outputs,omitempty"`
	AudioSinks   []AudioSink       `json:"audio_sinks"`
	InputDevices []InputDevicePath `json:"input_devices"`
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

// AudioSink is one reported local host audio sink.
type AudioSink struct {
	ID    string `json:"id"`
	Label string `json:"label"`
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
		AudioSinks:   []AudioSink{},
		InputDevices: []InputDevicePath{},
	}
}
