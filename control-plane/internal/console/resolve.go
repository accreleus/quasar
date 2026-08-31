package console

import (
	"encoding/json"
	"fmt"
)

// Defaults returns the default for every console-config field (mirrors
// hostcfg.Defaults()): off, local-only. The nil defaults (audio_output,
// default_app, default_user) are meaningful values, not omissions.
func Defaults() map[string]any {
	return map[string]any{
		"enabled":                 false,
		"connector":               "auto",
		"output_id":               nil,
		"mode":                    nil,
		"compositor":              "weston",
		"audio_output":            nil,
		"stream":                  false,
		"stream_audio":            false,
		"input_devices":           "auto",
		"grab":                    true,
		"auto_start_on_display":   false,
		"auto_connect_controller": false,
		"default_app":             nil,
		"default_user":            nil,
		"fullscreen":              true,
	}
}

// knownKeys is the console-config field set (PATCH validation rejects any
// other key).
var knownKeys = map[string]bool{
	"enabled":                 true,
	"connector":               true,
	"output_id":               true,
	"mode":                    true,
	"compositor":              true,
	"audio_output":            true,
	"stream":                  true,
	"stream_audio":            true,
	"input_devices":           true,
	"grab":                    true,
	"auto_start_on_display":   true,
	"auto_connect_controller": true,
	"default_app":             true,
	"default_user":            true,
	"fullscreen":              true,
}

// Resolve merges a sparse override map onto Defaults() and decodes the result
// into a typed ConsoleConfig for the API response (GET / PATCH 200 body).
// Callers must ValidatePatch the sparse map's origin first.
func Resolve(sparse map[string]any) (ConsoleConfig, error) {
	merged := Defaults()
	for k, v := range sparse {
		merged[k] = v
	}
	// Connector/compositor/fullscreen remain fixed until their runtime support
	// lands. Stream flags are now effective and must not be overwritten here.
	merged["connector"] = "auto"
	merged["compositor"] = "weston"
	merged["fullscreen"] = true
	raw, err := json.Marshal(merged)
	if err != nil {
		return ConsoleConfig{}, fmt.Errorf("encode resolved console_config: %w", err)
	}
	var out ConsoleConfig
	if err := json.Unmarshal(raw, &out); err != nil {
		return ConsoleConfig{}, fmt.Errorf("decode resolved console_config: %w", err)
	}
	return out, nil
}

// ValidatePatch validates a PATCH request body (partial console-config) against
// the known field set, per-field type/enum, and the host's reported
// capabilities (`caps`) for connector/audio_output/input_devices — unless the
// value is "auto" or null, which always validate. `appExists` FK-checks
// `default_app`; `userExists` FK-checks `default_user`. A null value is
// always allowed (it clears the override, reverting that key to its
// Defaults() value on the next Resolve).
func ValidatePatch(patch map[string]any, caps Capabilities, appExists func(appID string) (bool, error), userExists func(userID string) (bool, error)) error {
	for k, v := range patch {
		if !knownKeys[k] {
			return fmt.Errorf("unknown console-config key %q", k)
		}
		if v == nil {
			continue // null always means "clear"; always allowed in patch context
		}
		if err := validateField(k, v, caps, appExists, userExists); err != nil {
			return err
		}
	}
	return nil
}

func validateField(k string, v any, caps Capabilities, appExists func(appID string) (bool, error), userExists func(userID string) (bool, error)) error {
	switch k {
	case "enabled", "grab", "auto_start_on_display", "auto_connect_controller":
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("%q must be a boolean", k)
		}
	case "stream", "stream_audio":
		_, ok := v.(bool)
		if !ok {
			return fmt.Errorf("%q must be a boolean", k)
		}
	case "fullscreen":
		b, ok := v.(bool)
		if !ok {
			return fmt.Errorf("%q must be a boolean", k)
		}
		if !b {
			return fmt.Errorf("%q=false is not supported; console sessions currently use dual-output WebRTC with fullscreen local display", k)
		}
	case "compositor":
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("%q must be a string", k)
		}
		if s != "weston" {
			return fmt.Errorf("%q=%q is not supported; only the proven weston DRM backend is available", k, s)
		}
	case "connector":
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("%q must be a string", k)
		}
		if s != "auto" {
			return fmt.Errorf("%q=%q is not supported; the runtime currently selects the display connector automatically", k, s)
		}
	case "output_id":
		s, ok := v.(string)
		if !ok || s == "" {
			return fmt.Errorf("%q must be a non-empty string or null", k)
		}
		if outputByID(caps.Outputs, s) == nil {
			return fmt.Errorf("%q is not a reported DRM output", k)
		}
	case "mode":
		m, ok := decodeModeSelection(v)
		if !ok {
			return fmt.Errorf("%q must contain integer width, height, and refresh_millihz", k)
		}
		// A mode is valid if any currently reported output supports its exact
		// timing identity. Cross-field output_id validation runs below.
		if !modeKnown(caps.Outputs, m) {
			return fmt.Errorf("%q is not a reported DRM mode", k)
		}
	case "audio_output":
		// null is a meaningful value (no local audio / quiet) — allow it.
		if v == nil {
			break
		}
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("%q must be a string or null", k)
		}
		if s != "auto" && len(caps.AudioSinks) > 0 && !audioSinkKnown(caps.AudioSinks, s) {
			return fmt.Errorf("%q is not a reported audio sink", k)
		}
	case "input_devices":
		switch vv := v.(type) {
		case string:
			if vv != "auto" {
				return fmt.Errorf("%q string value must be %q", k, "auto")
			}
		case []any:
			for _, item := range vv {
				s, ok := item.(string)
				if !ok {
					return fmt.Errorf("%q entries must be strings", k)
				}
				if len(caps.InputDevices) > 0 && !inputDeviceKnown(caps.InputDevices, s) {
					return fmt.Errorf("%q contains an unreported device %q", k, s)
				}
			}
		default:
			return fmt.Errorf("%q must be %q or an array of strings", k, "auto")
		}
	case "default_app":
		// null clears the auto-launch app — allow it.
		if v == nil {
			break
		}
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("%q must be a string uuid or null", k)
		}
		if appExists == nil {
			return fmt.Errorf("%q cannot be validated", k)
		}
		exists, err := appExists(s)
		if err != nil {
			return fmt.Errorf("check default_app: %w", err)
		}
		if !exists {
			return fmt.Errorf("%q references an unknown app", k)
		}
	case "default_user":
		// null clears the auto-launch owner — allow it.
		if v == nil {
			break
		}
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("%q must be a string uuid or null", k)
		}
		if userExists == nil {
			return fmt.Errorf("%q cannot be validated", k)
		}
		exists, err := userExists(s)
		if err != nil {
			return fmt.Errorf("check default_user: %w", err)
		}
		if !exists {
			return fmt.Errorf("%q references an unknown user", k)
		}
	}
	return nil
}

func outputByID(outputs []DRMOutput, id string) *DRMOutput {
	for i := range outputs {
		if outputs[i].ID == id {
			return &outputs[i]
		}
	}
	return nil
}

func decodeModeSelection(v any) (ModeSelection, bool) {
	m, ok := v.(map[string]any)
	if !ok {
		return ModeSelection{}, false
	}
	num := func(key string) (uint32, bool) {
		f, ok := m[key].(float64)
		return uint32(f), ok && f > 0 && f == float64(uint32(f))
	}
	w, wok := num("width")
	h, hok := num("height")
	r, rok := num("refresh_millihz")
	if !wok || !hok || !rok || w > 65535 || h > 65535 {
		return ModeSelection{}, false
	}
	return ModeSelection{Width: uint16(w), Height: uint16(h), RefreshMillihz: r}, true
}

func modeKnown(outputs []DRMOutput, wanted ModeSelection) bool {
	for _, output := range outputs {
		for _, mode := range output.Modes {
			if mode.Width == wanted.Width && mode.Height == wanted.Height && mode.RefreshMillihz == wanted.RefreshMillihz {
				return true
			}
		}
	}
	return false
}

// ValidateOutputSelection enforces the cross-field invariant after a partial
// PATCH has been merged with stored sparse config.
func ValidateOutputSelection(config map[string]any, caps Capabilities) error {
	id, hasID := config["output_id"].(string)
	modeValue, hasMode := config["mode"]
	if !hasID && !hasMode {
		return nil
	}
	if !hasID || !hasMode {
		return fmt.Errorf("output_id and mode must be configured or cleared together")
	}
	output := outputByID(caps.Outputs, id)
	if output == nil || !output.Connected {
		return fmt.Errorf("output_id must reference a connected DRM output")
	}
	mode, ok := decodeModeSelection(modeValue)
	if !ok {
		return fmt.Errorf("mode must contain integer width, height, and refresh_millihz")
	}
	for _, candidate := range output.Modes {
		if candidate.Width == mode.Width && candidate.Height == mode.Height && candidate.RefreshMillihz == mode.RefreshMillihz {
			return nil
		}
	}
	return fmt.Errorf("mode is not supported by selected output_id")
}

func audioSinkKnown(sinks []AudioSink, id string) bool {
	for _, s := range sinks {
		if s.ID == id {
			return true
		}
	}
	return false
}

func inputDeviceKnown(devices []InputDevicePath, path string) bool {
	for _, d := range devices {
		if d.Path == path {
			return true
		}
	}
	return false
}
