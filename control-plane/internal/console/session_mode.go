package console

// ModeSource says where a console session's initial display mode came from. It
// is a log field, not a wire value.
type ModeSource string

const (
	// ModeSourceActive: the output's active mode (what its CRTC runs now).
	ModeSourceActive ModeSource = "physical_active"
	// ModeSourcePreferred: the output's DRM-preferred mode.
	ModeSourcePreferred ModeSource = "physical_preferred"
	// ModeSourceFirst: the output's first listed mode (no active, no preferred).
	ModeSourceFirst ModeSource = "physical_first"
	// ModeSourceAppDefault: nothing to follow; zero size and rate, so the launch
	// falls back to the app's defaults as it always did.
	ModeSourceAppDefault ModeSource = "app_default"
)

// SessionMode is the initial size and rate of a console session's app display.
// Zero Width/Height/FPS means "use the app's defaults".
type SessionMode struct {
	Width, Height, FPS int32
	Source             ModeSource
}

// ResolveSessionMode picks the console session's initial mode (#422) from the
// resolved console config and the host's last capability report. Pure; caps may
// be empty. Since amendment 19 the desktop drives the display and picks its own
// mode, so this is only the launch size and rate recorded on the session row;
// there is no configured mode and no streamed console to protect.
//
//   - A pinned output_id runs at that output's active mode, else its preferred
//     mode, else its first mode.
//   - Automatic takes the first connected output's preferred mode, else its
//     active mode, else its first mode.
//   - Nothing to follow keeps the app's defaults.
//
// Fps is the refresh rate rounded to the nearest Hz (119879 mHz -> 120).
func ResolveSessionMode(cfg ConsoleConfig, caps Capabilities) SessionMode {
	if cfg.OutputID != nil {
		out := outputByID(caps.Outputs, *cfg.OutputID)
		if out == nil || !out.Connected {
			return SessionMode{Source: ModeSourceAppDefault}
		}
		if m, src, ok := physicalMode(*out, true); ok {
			return fromDRM(m, src)
		}
		return SessionMode{Source: ModeSourceAppDefault}
	}
	for _, out := range caps.Outputs {
		if !out.Connected {
			continue
		}
		if m, src, ok := physicalMode(out, false); ok {
			return fromDRM(m, src)
		}
	}
	return SessionMode{Source: ModeSourceAppDefault}
}

// physicalMode returns the output's active, preferred or first mode, in that
// order when activeFirst, else preferred before active.
func physicalMode(out DRMOutput, activeFirst bool) (DRMMode, ModeSource, bool) {
	var preferred *DRMMode
	for i := range out.Modes {
		if out.Modes[i].Preferred {
			preferred = &out.Modes[i]
			break
		}
	}
	order := []ModeSource{ModeSourcePreferred, ModeSourceActive}
	if activeFirst {
		order = []ModeSource{ModeSourceActive, ModeSourcePreferred}
	}
	for _, src := range order {
		switch {
		case src == ModeSourceActive && out.ActiveMode != nil:
			return *out.ActiveMode, src, true
		case src == ModeSourcePreferred && preferred != nil:
			return *preferred, src, true
		}
	}
	if len(out.Modes) > 0 {
		return out.Modes[0], ModeSourceFirst, true
	}
	return DRMMode{}, ModeSourceAppDefault, false
}

func fromDRM(m DRMMode, src ModeSource) SessionMode {
	return SessionMode{Width: int32(m.Width), Height: int32(m.Height), FPS: roundHz(m.RefreshMillihz), Source: src}
}

func roundHz(millihz uint32) int32 {
	return int32((millihz + 500) / 1000)
}
