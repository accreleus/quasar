package console

import "testing"

// fourK240Modes is a 42-mode list as a 4K 240 Hz DisplayPort monitor reports it
// over DRM: the native 3840x2160 timings first (preferred at 60 Hz), then the
// usual scaled and legacy modes down to 640x480.
func fourK240Modes() []DRMMode {
	type m struct {
		w, h      uint16
		mhz       uint32
		preferred bool
	}
	list := []m{
		{3840, 2160, 60000, true}, {3840, 2160, 239990, false}, {3840, 2160, 200000, false},
		{3840, 2160, 165000, false}, {3840, 2160, 144000, false}, {3840, 2160, 120000, false},
		{3840, 2160, 119880, false}, {3840, 2160, 100000, false}, {3840, 2160, 59940, false},
		{3840, 2160, 50000, false}, {3840, 2160, 30000, false}, {3840, 2160, 29970, false},
		{3840, 2160, 25000, false}, {3840, 2160, 24000, false}, {3840, 2160, 23976, false},
		{2560, 1440, 239970, false}, {2560, 1440, 165000, false}, {2560, 1440, 144000, false},
		{2560, 1440, 119998, false}, {2560, 1440, 59951, false},
		{1920, 1080, 240000, false}, {1920, 1080, 144001, false}, {1920, 1080, 120000, false},
		{1920, 1080, 119880, false}, {1920, 1080, 100000, false}, {1920, 1080, 60000, false},
		{1920, 1080, 59940, false}, {1920, 1080, 50000, false}, {1920, 1080, 30000, false},
		{1920, 1080, 24000, false},
		{1680, 1050, 59954, false}, {1600, 900, 60000, false}, {1440, 900, 59887, false},
		{1280, 1024, 75025, false}, {1280, 1024, 60020, false}, {1280, 800, 59810, false},
		{1280, 720, 60000, false}, {1280, 720, 59940, false}, {1024, 768, 75029, false},
		{1024, 768, 60004, false}, {800, 600, 60317, false}, {640, 480, 59940, false},
	}
	out := make([]DRMMode, 0, len(list))
	for _, x := range list {
		out = append(out, DRMMode{Width: x.w, Height: x.h, RefreshMillihz: x.mhz, Preferred: x.preferred})
	}
	return out
}

func strp(s string) *string { return &s }

func TestFourK240FixtureIs42Modes(t *testing.T) {
	if n := len(fourK240Modes()); n != 42 {
		t.Fatalf("fixture has %d modes, want 42", n)
	}
}

func TestResolveSessionMode(t *testing.T) {
	active240 := &DRMMode{Width: 3840, Height: 2160, RefreshMillihz: 239990}
	active1199 := &DRMMode{Width: 2560, Height: 1440, RefreshMillihz: 119879}
	fourK := DRMOutput{ID: "card0:DP-4", Connector: "DP-4", Connected: true, ActiveMode: active240, Modes: fourK240Modes()}
	fourKIdle := fourK
	fourKIdle.ActiveMode = nil
	qhd := DRMOutput{ID: "card0:DP-5", Connector: "DP-5", Connected: true, ActiveMode: active1199, Modes: []DRMMode{
		{Width: 2560, Height: 1440, RefreshMillihz: 119879},
		{Width: 1920, Height: 1080, RefreshMillihz: 60000},
	}}
	unplugged := DRMOutput{ID: "card0:DP-1", Connector: "DP-1", Connected: false, Modes: []DRMMode{{Width: 1280, Height: 720, RefreshMillihz: 60000, Preferred: true}}}
	noModes := DRMOutput{ID: "card0:HDMI-A-1", Connector: "HDMI-A-1", Connected: true}

	caps := func(outs ...DRMOutput) Capabilities { return Capabilities{Outputs: outs} }

	tests := []struct {
		name string
		cfg  ConsoleConfig
		caps Capabilities
		want SessionMode
	}{
		{
			name: "configured mode wins over the physical one",
			cfg:  ConsoleConfig{OutputID: strp("card0:DP-4"), Mode: &ModeSelection{Width: 2560, Height: 1440, RefreshMillihz: 119998}},
			caps: caps(fourK),
			want: SessionMode{Width: 2560, Height: 1440, FPS: 120, Source: ModeSourceConfigured},
		},
		{
			name: "configured mode still wins when streaming",
			cfg:  ConsoleConfig{Stream: true, OutputID: strp("card0:DP-4"), Mode: &ModeSelection{Width: 3840, Height: 2160, RefreshMillihz: 239990}},
			caps: caps(fourK),
			want: SessionMode{Width: 3840, Height: 2160, FPS: 240, Source: ModeSourceConfigured},
		},
		{
			name: "automatic local-only follows the preferred mode weston lights",
			cfg:  ConsoleConfig{},
			caps: caps(unplugged, fourK, qhd),
			want: SessionMode{Width: 3840, Height: 2160, FPS: 60, Source: ModeSourcePreferred},
		},
		{
			name: "automatic falls back to the active mode with no preferred flag",
			cfg:  ConsoleConfig{},
			caps: caps(qhd),
			want: SessionMode{Width: 2560, Height: 1440, FPS: 120, Source: ModeSourceActive},
		},
		{
			name: "pinned output with no mode uses its active mode",
			cfg:  ConsoleConfig{OutputID: strp("card0:DP-4")},
			caps: caps(qhd, fourK),
			want: SessionMode{Width: 3840, Height: 2160, FPS: 240, Source: ModeSourceActive},
		},
		{
			name: "pinned output rounds 119879 mHz to 120 fps",
			cfg:  ConsoleConfig{OutputID: strp("card0:DP-5")},
			caps: caps(fourK, qhd),
			want: SessionMode{Width: 2560, Height: 1440, FPS: 120, Source: ModeSourceActive},
		},
		{
			name: "pinned idle output uses its preferred mode",
			cfg:  ConsoleConfig{OutputID: strp("card0:DP-4")},
			caps: caps(fourKIdle),
			want: SessionMode{Width: 3840, Height: 2160, FPS: 60, Source: ModeSourcePreferred},
		},
		{
			name: "pinned output with neither active nor preferred uses its first mode",
			cfg:  ConsoleConfig{OutputID: strp("card0:DP-5")},
			caps: caps(DRMOutput{ID: "card0:DP-5", Connector: "DP-5", Connected: true, Modes: qhd.Modes}),
			want: SessionMode{Width: 2560, Height: 1440, FPS: 120, Source: ModeSourceFirst},
		},
		{
			name: "pinned output absent from the report keeps the app default",
			cfg:  ConsoleConfig{OutputID: strp("card1:DP-1")},
			caps: caps(fourK),
			want: SessionMode{Source: ModeSourceAppDefault},
		},
		{
			name: "streaming with no configured mode keeps the app default",
			cfg:  ConsoleConfig{Stream: true},
			caps: caps(fourK),
			want: SessionMode{Source: ModeSourceAppDefault},
		},
		{
			name: "no capabilities keeps the app default",
			cfg:  ConsoleConfig{},
			caps: Capabilities{},
			want: SessionMode{Source: ModeSourceAppDefault},
		},
		{
			name: "only disconnected outputs keeps the app default",
			cfg:  ConsoleConfig{},
			caps: caps(unplugged),
			want: SessionMode{Source: ModeSourceAppDefault},
		},
		{
			name: "connected output with no modes keeps the app default",
			cfg:  ConsoleConfig{},
			caps: caps(noModes),
			want: SessionMode{Source: ModeSourceAppDefault},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveSessionMode(tc.cfg, tc.caps); got != tc.want {
				t.Fatalf("ResolveSessionMode = %+v, want %+v", got, tc.want)
			}
		})
	}
}
