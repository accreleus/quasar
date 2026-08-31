package profile

import "testing"

func rungsEqual(a, b []Rung) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestFamily(t *testing.T) {
	cases := []struct {
		name string
		w, h int32
		want []Rung
	}{
		{"16:9 at 1080p", 1920, 1080, []Rung{{3840, 2160}, {2560, 1440}, {1920, 1080}, {1600, 900}, {1280, 720}}},
		{"16:9 at 4k", 3840, 2160, []Rung{{3840, 2160}, {2560, 1440}, {1920, 1080}, {1600, 900}, {1280, 720}}},
		{"16:9 non-canonical size still selects the family", 2000, 1125, []Rung{{3840, 2160}, {2560, 1440}, {1920, 1080}, {1600, 900}, {1280, 720}}},
		{"16:10", 1920, 1200, []Rung{{2560, 1600}, {1920, 1200}, {1680, 1050}, {1440, 900}, {1280, 800}}},
		{"21:9", 3440, 1440, []Rung{{3440, 1440}, {2560, 1080}}},
		{"21:9 at 2560x1080", 2560, 1080, []Rung{{3440, 1440}, {2560, 1080}}},
		{"4:3", 1280, 960, []Rung{{1600, 1200}, {1280, 960}, {1024, 768}}},
		// Unknown ratios get no ladder at all — only themselves.
		{"unknown 5:4", 1280, 1024, []Rung{{1280, 1024}}},
		{"unknown portrait", 1080, 1920, []Rung{{1080, 1920}}},
		{"unknown 32:9", 3840, 1080, []Rung{{3840, 1080}}},
		{"zero", 0, 0, []Rung{{0, 0}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Family(tc.w, tc.h)
			if !rungsEqual(got, tc.want) {
				t.Fatalf("Family(%d,%d) = %v, want %v", tc.w, tc.h, got, tc.want)
			}
		})
	}
}

func TestFamilyReturnsAFreshSlice(t *testing.T) {
	a := Family(1920, 1080)
	a[0] = Rung{1, 1}
	b := Family(1920, 1080)
	if b[0] != (Rung{3840, 2160}) {
		t.Fatalf("Family leaked the package-level table: %v", b)
	}
}

func TestAvailableRungs(t *testing.T) {
	cases := []struct {
		name string
		w, h int32
		want []Rung
	}{
		{"1080p", 1920, 1080, []Rung{{1920, 1080}, {1600, 900}, {1280, 720}}},
		{"1440p", 2560, 1440, []Rung{{2560, 1440}, {1920, 1080}, {1600, 900}, {1280, 720}}},
		{"4k", 3840, 2160, []Rung{{3840, 2160}, {2560, 1440}, {1920, 1080}, {1600, 900}, {1280, 720}}},
		{"720p is its own floor", 1280, 720, []Rung{{1280, 720}}},
		{"16:10 1920x1200", 1920, 1200, []Rung{{1920, 1200}, {1680, 1050}, {1440, 900}, {1280, 800}}},
		{"21:9 3440x1440", 3440, 1440, []Rung{{3440, 1440}, {2560, 1080}}},
		{"4:3 1600x1200", 1600, 1200, []Rung{{1600, 1200}, {1280, 960}, {1024, 768}}},
		// Non-canonical 16:9 launch: itself first, then the family below it.
		{"non-canonical 16:9", 2000, 1125, []Rung{{2000, 1125}, {1920, 1080}, {1600, 900}, {1280, 720}}},
		// Below every canonical rung of its family ⇒ only itself.
		{"tiny 16:9", 640, 360, []Rung{{640, 360}}},
		{"unknown ratio", 1280, 1024, []Rung{{1280, 1024}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := AvailableRungs(tc.w, tc.h)
			if !rungsEqual(got, tc.want) {
				t.Fatalf("AvailableRungs(%d,%d) = %v, want %v", tc.w, tc.h, got, tc.want)
			}
			if got[0] != (Rung{tc.w, tc.h}) {
				t.Fatalf("AvailableRungs(%d,%d)[0] = %v, want the launch size first", tc.w, tc.h, got[0])
			}
		})
	}
}

func TestAvailableRungsAreDescendingAndUnique(t *testing.T) {
	for _, s := range []Rung{{3840, 2160}, {2560, 1440}, {1920, 1080}, {2000, 1125}, {1920, 1200}, {3440, 1440}, {1600, 1200}} {
		got := AvailableRungs(s.Width, s.Height)
		seen := map[Rung]bool{}
		for i, r := range got {
			if seen[r] {
				t.Fatalf("AvailableRungs(%v) has a duplicate %v: %v", s, r, got)
			}
			seen[r] = true
			if i > 0 && (r.Width >= got[i-1].Width || r.Height >= got[i-1].Height) {
				t.Fatalf("AvailableRungs(%v) is not strictly descending at %d: %v", s, i, got)
			}
		}
	}
}

func TestIsRung(t *testing.T) {
	cases := []struct {
		name    string
		w, h    int32
		launchW int32
		launchH int32
		want    bool
	}{
		{"launch size itself", 1920, 1080, 1920, 1080, true},
		{"one step down", 1600, 900, 1920, 1080, true},
		{"two steps down", 1280, 720, 1920, 1080, true},
		{"above the launch size", 2560, 1440, 1920, 1080, false},
		{"not on the ladder", 1366, 768, 1920, 1080, false},
		{"other family's rung", 1440, 900, 1920, 1080, false},
		{"16:10 rung of a 16:10 launch", 1440, 900, 1920, 1200, true},
		{"non-canonical launch is a rung of itself", 2000, 1125, 2000, 1125, true},
		{"unknown-ratio launch has no other rung", 1280, 720, 1280, 1024, false},
		{"unknown-ratio launch is a rung of itself", 1280, 1024, 1280, 1024, true},
		{"transposed dims", 1080, 1920, 1920, 1080, false},
		{"zero", 0, 0, 1920, 1080, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsRung(tc.w, tc.h, tc.launchW, tc.launchH); got != tc.want {
				t.Fatalf("IsRung(%d,%d,%d,%d) = %v, want %v", tc.w, tc.h, tc.launchW, tc.launchH, got, tc.want)
			}
		})
	}
}
