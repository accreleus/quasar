package tier

import (
	"testing"
)

// TestLadderOrdering verifies the Ladder slice is ordered highest-quality first
// (1080p60-lan → 1080p60 → 720p60 → 720p30) so Select returns the best
// eligible tier, not an arbitrary one.
func TestLadderOrdering(t *testing.T) {
	want := []string{"1080p60-lan", "1080p60", "720p60", "720p30"}
	if len(Ladder) != len(want) {
		t.Fatalf("Ladder has %d entries, want %d", len(Ladder), len(want))
	}
	for i, name := range want {
		if Ladder[i].Name != name {
			t.Errorf("Ladder[%d].Name = %q, want %q", i, Ladder[i].Name, name)
		}
	}
}

// TestFloorAlwaysEligible verifies the 720p30 floor tier is eligible for any
// probe, including a zero-value probe (no measurements at all) and a probe
// indicating very poor connectivity.
func TestFloorAlwaysEligible(t *testing.T) {
	cases := []struct {
		name  string
		probe Probe
	}{
		{"zero probe", Probe{}},
		{"very low bandwidth", Probe{BandwidthKbps: 100, RTTMs: 500}},
		{"tiny decode height", Probe{BandwidthKbps: 500, MaxDecodeHeight: 240}},
	}
	floor := Ladder[len(Ladder)-1]
	if floor.Name != "720p30" {
		t.Fatalf("expected last rung to be 720p30, got %q", floor.Name)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !eligible(tc.probe, floor) {
				t.Errorf("floor tier not eligible for probe %+v", tc.probe)
			}
		})
	}
}

// TestEligibilityPerRung checks each rung's requirements in isolation.
func TestEligibilityPerRung(t *testing.T) {
	tierByName := func(name string) Tier {
		for _, t := range Ladder {
			if t.Name == name {
				return t
			}
		}
		panic("unknown tier: " + name)
	}

	cases := []struct {
		name     string
		tier     string
		probe    Probe
		wantElig bool
	}{
		// --- 1080p60-lan ---
		// Requires bandwidth ≥ 25000 AND rtt ≤ 10; both must be present.
		{
			name:     "1080p60-lan: qualified LAN probe",
			tier:     "1080p60-lan",
			probe:    Probe{BandwidthKbps: 50000, RTTMs: 5},
			wantElig: true,
		},
		{
			name: "1080p60-lan: bandwidth just enough, rtt ok",
			tier: "1080p60-lan",
			// headroom: 25000 × 1.5 = 37500 required; 38000 is enough
			probe:    Probe{BandwidthKbps: 38000, RTTMs: 8},
			wantElig: true,
		},
		{
			name:     "1080p60-lan: rtt too high",
			tier:     "1080p60-lan",
			probe:    Probe{BandwidthKbps: 50000, RTTMs: 15},
			wantElig: false,
		},
		{
			name:     "1080p60-lan: bandwidth too low",
			tier:     "1080p60-lan",
			probe:    Probe{BandwidthKbps: 20000, RTTMs: 5},
			wantElig: false,
		},
		{
			name:     "1080p60-lan: rtt not measured → ineligible (LAN requires both)",
			tier:     "1080p60-lan",
			probe:    Probe{BandwidthKbps: 50000},
			wantElig: false,
		},
		{
			name:     "1080p60-lan: bandwidth not measured → ineligible (LAN requires both)",
			tier:     "1080p60-lan",
			probe:    Probe{RTTMs: 5},
			wantElig: false,
		},

		// --- 1080p60 ---
		{
			name: "1080p60: good probe",
			tier: "1080p60",
			// headroom: 8000 × 1.5 = 12000; probe has 15000 ≥ 12000
			probe:    Probe{BandwidthKbps: 15000, MaxDecodeHeight: 1080},
			wantElig: true,
		},
		{
			name: "1080p60: bandwidth exactly at headroom",
			tier: "1080p60",
			// 8000 × 1.5 = 12000
			probe:    Probe{BandwidthKbps: 12000, MaxDecodeHeight: 1080},
			wantElig: true,
		},
		{
			name: "1080p60: bandwidth below headroom threshold",
			tier: "1080p60",
			// 8000 × 1.5 = 12000; 11999 < 12000
			probe:    Probe{BandwidthKbps: 11999, MaxDecodeHeight: 1080},
			wantElig: false,
		},
		{
			name:     "1080p60: bandwidth below 12000 requirement",
			tier:     "1080p60",
			probe:    Probe{BandwidthKbps: 10000, MaxDecodeHeight: 1080},
			wantElig: false,
		},
		{
			name:     "1080p60: decode height too low",
			tier:     "1080p60",
			probe:    Probe{BandwidthKbps: 15000, MaxDecodeHeight: 720},
			wantElig: false,
		},
		{
			name:     "1080p60: zero probe (no measurements) → eligible (unknown → allow)",
			tier:     "1080p60",
			probe:    Probe{},
			wantElig: true,
		},

		// --- 720p60 ---
		{
			name: "720p60: good probe",
			tier: "720p60",
			// headroom: 5000 × 1.5 = 7500; probe has 10000 ≥ 7500; also ≥ 8000
			probe:    Probe{BandwidthKbps: 10000},
			wantElig: true,
		},
		{
			name: "720p60: bandwidth exactly at headroom (7500)",
			tier: "720p60",
			// 5000 × 1.5 = 7500; also ≥ 8000? No — 7500 < 8000, so ineligible
			probe:    Probe{BandwidthKbps: 7500},
			wantElig: false,
		},
		{
			name:     "720p60: bandwidth at 8000",
			tier:     "720p60",
			probe:    Probe{BandwidthKbps: 8000},
			wantElig: true,
		},
		{
			name:     "720p60: bandwidth below 8000",
			tier:     "720p60",
			probe:    Probe{BandwidthKbps: 7000},
			wantElig: false,
		},
		{
			name:     "720p60: zero bandwidth → eligible",
			tier:     "720p60",
			probe:    Probe{},
			wantElig: true,
		},

		// --- 720p30 (floor) ---
		{
			name:     "720p30: always eligible with good probe",
			tier:     "720p30",
			probe:    Probe{BandwidthKbps: 50000, RTTMs: 2},
			wantElig: true,
		},
		{
			name:     "720p30: always eligible with bad probe",
			tier:     "720p30",
			probe:    Probe{BandwidthKbps: 100, RTTMs: 1000},
			wantElig: true,
		},
		{
			name:     "720p30: always eligible with zero probe",
			tier:     "720p30",
			probe:    Probe{},
			wantElig: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := tierByName(tc.tier)
			got := eligible(tc.probe, tr)
			if got != tc.wantElig {
				t.Errorf("eligible(%+v, %q) = %v, want %v", tc.probe, tc.tier, got, tc.wantElig)
			}
		})
	}
}

// TestHeadroomRule specifically exercises the 1.5× headroom rule across all
// tiers that have a bandwidth requirement.
func TestHeadroomRule(t *testing.T) {
	cases := []struct {
		tierName string
		bitrate  int32 // the tier's BitrateKbps
		probeBW  int32 // probe bandwidth to test
		wantElig bool
	}{
		// 1080p60-lan: bitrate=12000, headroom=12000×1.5=18000; also needs bw≥25000 & rtt≤10
		// Headroom check: bw≥18000 (passes at 18000), but bw<25000 → ineligible.
		{"1080p60-lan", 12000, 18000, false}, // passes headroom but below 25000
		// At 37499: passes headroom (≥18000) AND passes bw≥25000 → eligible.
		{"1080p60-lan", 12000, 37499, true},
		{"1080p60-lan", 12000, 37500, true}, // well above all thresholds

		// 1080p60: bitrate=8000, headroom=12000
		{"1080p60", 8000, 11999, false},
		{"1080p60", 8000, 12000, true},
		{"1080p60", 8000, 12001, true},

		// 720p60: bitrate=5000, headroom=7500; but also requires bw≥8000
		{"720p60", 5000, 7499, false},
		{"720p60", 5000, 7500, false}, // passes headroom but fails bw≥8000
		{"720p60", 5000, 8000, true},  // passes both

		// 720p30: no headroom check (floor)
		{"720p30", 3000, 1, true},
	}

	for _, tc := range cases {
		t.Run(tc.tierName+"_bw"+itoa(tc.probeBW), func(t *testing.T) {
			var p Probe
			p.BandwidthKbps = tc.probeBW
			if tc.tierName == "1080p60-lan" {
				// supply rtt to make only bandwidth the variable
				p.RTTMs = 5
			}
			// find the tier
			var tr Tier
			for _, l := range Ladder {
				if l.Name == tc.tierName {
					tr = l
					break
				}
			}
			got := eligible(p, tr)
			if got != tc.wantElig {
				t.Errorf("eligible(%+v, %q) = %v, want %v", p, tc.tierName, got, tc.wantElig)
			}
		})
	}
}

// TestSelectTopDownOrdering verifies Select returns the highest eligible tier,
// not a lower one, when multiple tiers are eligible.
func TestSelectTopDownOrdering(t *testing.T) {
	cases := []struct {
		name     string
		probe    Probe
		wantTier string
	}{
		{
			name:     "excellent LAN probe → 1080p60-lan",
			probe:    Probe{BandwidthKbps: 50000, RTTMs: 5, MaxDecodeHeight: 2160},
			wantTier: "1080p60-lan",
		},
		{
			name:     "good WAN probe → 1080p60",
			probe:    Probe{BandwidthKbps: 20000, RTTMs: 30, MaxDecodeHeight: 1080},
			wantTier: "1080p60",
		},
		{
			name:     "mid probe (720p capable) → 720p60",
			probe:    Probe{BandwidthKbps: 9000, RTTMs: 50, MaxDecodeHeight: 720},
			wantTier: "720p60",
		},
		{
			name:     "poor probe → 720p30 floor",
			probe:    Probe{BandwidthKbps: 2000, RTTMs: 200, MaxDecodeHeight: 480},
			wantTier: "720p30",
		},
		{
			name:     "zero probe → 1080p60 (default, no measurements)",
			probe:    Probe{},
			wantTier: "1080p60",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Select(tc.probe)
			if got.Name != tc.wantTier {
				t.Errorf("Select(%+v).Name = %q, want %q", tc.probe, got.Name, tc.wantTier)
			}
		})
	}
}

// TestSelectNeverReturnsZero verifies Select always returns a valid Tier (the
// floor guarantee).
func TestSelectNeverReturnsZero(t *testing.T) {
	probes := []Probe{
		{},
		{BandwidthKbps: 0},
		{BandwidthKbps: 1, RTTMs: 9999},
	}
	for _, p := range probes {
		got := Select(p)
		if got.Name == "" {
			t.Errorf("Select(%+v) returned zero-value Tier", p)
		}
		if got.BitrateKbps == 0 {
			t.Errorf("Select(%+v) returned tier with zero BitrateKbps", p)
		}
	}
}

// TestDefaultMatchesExpected verifies Default() returns the 1080p60 tier so
// AS-02 "no probe → today's behavior" is exact.
func TestDefaultMatchesExpected(t *testing.T) {
	d := Default()
	if d.Name != "1080p60" {
		t.Errorf("Default().Name = %q, want %q", d.Name, "1080p60")
	}
	if d.Width != 1920 || d.Height != 1080 {
		t.Errorf("Default() res = %dx%d, want 1920x1080", d.Width, d.Height)
	}
	if d.FPS != 60 {
		t.Errorf("Default().FPS = %d, want 60", d.FPS)
	}
	if d.BitrateKbps != 8000 {
		t.Errorf("Default().BitrateKbps = %d, want 8000", d.BitrateKbps)
	}
	if d.Playout0Ms != 50 {
		t.Errorf("Default().Playout0Ms = %d, want 50", d.Playout0Ms)
	}
}

// itoa is a simple int32→string helper for test names.
func itoa(n int32) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	b := make([]byte, 0, 10)
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}
