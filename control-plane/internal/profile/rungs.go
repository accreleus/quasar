package profile

// External-resolution rungs (adaptive-external-resolution, spec D4/D5).
//
// A "rung" is a legal ENCODED (external) frame size a running session may be
// stepped to live. The ladder is derived from the session's LAUNCH size: the
// reduced aspect ratio of the launch WxH selects a family, and the available
// rungs are that family filtered to entries no larger than the launch size in
// BOTH dimensions, descending, with the launch size itself always first (a
// profile may legitimately be launched at a size that is not a canonical rung —
// it is still, trivially, a rung of itself).
//
// The CONTROL PLANE is the validator: `PATCH /v1/sessions/{id}/display` rejects
// a `stream_width`/`stream_height` pair that is not on this ladder before the
// command ever reaches a host. The node agent mirrors this table only to
// advertise a `wl_output` mode ladder to the guest app (so a desktop's display
// settings can offer the same rungs). The agent ALSO co-validates `is_rung`
// against its mirror today (defence in depth; strictly more restrictive, never
// less) on top of the structural checks (even, >= 16 px, <= launch). Keep the two
// tables equal — both sides carry a unit test pinning the same values. The CP
// remains the authority: when custom rungs land (spec D4), the agent's `is_rung`
// relaxes to structural-only (see node-agent/src/session/rungs.rs) so a
// CP-accepted custom rung is not refused by a stale mirror.
type Rung struct {
	Width  int32
	Height int32
}

// Canonical families, each DESCENDING. Membership is by reduced aspect ratio of
// the launch size, so a 16:9 profile of any size draws from rungs16x9.
var (
	rungs16x9 = []Rung{
		{3840, 2160}, {2560, 1440}, {1920, 1080}, {1600, 900}, {1280, 720},
	}
	rungs16x10 = []Rung{
		{2560, 1600}, {1920, 1200}, {1680, 1050}, {1440, 900}, {1280, 800},
	}
	rungs21x9 = []Rung{
		{3440, 1440}, {2560, 1080},
	}
	rungs4x3 = []Rung{
		{1600, 1200}, {1280, 960}, {1024, 768},
	}
)

// Family returns the canonical rung family for a size's reduced aspect ratio,
// descending. An aspect ratio outside the four known families (an ultrawide
// oddity, a portrait profile, a bespoke operator rung) has no ladder at all: the
// family is the size itself, so the only "step" available is a no-op. That is
// deliberate — inventing rungs for an unknown ratio would hand the guest app
// sizes the operator never certified.
//
// The returned slice is freshly allocated; callers may retain or sort it.
func Family(w, h int32) []Rung {
	self := []Rung{{w, h}}
	if w <= 0 || h <= 0 {
		return self
	}
	r := newRatio(w, h)
	for _, f := range families {
		for _, accept := range f.ratios {
			if accept == r {
				return append([]Rung(nil), f.rungs...)
			}
		}
	}
	return self
}

type ratio struct{ w, h int32 }

func newRatio(w, h int32) ratio { rw, rh := reduce(w, h); return ratio{rw, rh} }

// families maps a set of ACCEPTED reduced ratios onto a rung table. Membership is
// a set, not a single ratio, because the "21:9" family is a marketing label, not
// an arithmetic one: 3440x1440 reduces to 43:18 and 2560x1080 to 64:27, so a
// single reduced ratio would put the family's own rungs in different families.
// 16:10 has the same trap in reverse — every 16:10 size reduces to 8:5.
var families = []struct {
	ratios []ratio
	rungs  []Rung
}{
	{[]ratio{{16, 9}}, rungs16x9},
	{[]ratio{{8, 5}}, rungs16x10},
	// 43:18 = 3440x1440, 64:27 = 2560x1080, 7:3 = a nominal 21:9 size.
	{[]ratio{{43, 18}, {64, 27}, {7, 3}}, rungs21x9},
	{[]ratio{{4, 3}}, rungs4x3},
}

// AvailableRungs is the ladder a session launched at w x h may be stepped along:
// its family filtered to entries <= w AND <= h, descending, with {w,h} itself
// guaranteed to be the first entry.
//
// The launch size is prepended rather than assumed present because a profile can
// carry a non-canonical size (2000x1125 is exactly 16:9 but is not a rung); the
// session must always be able to return to what it launched at. When the launch
// size IS canonical it is already the largest surviving entry, so no duplicate
// is produced.
func AvailableRungs(w, h int32) []Rung {
	out := []Rung{{w, h}}
	for _, r := range Family(w, h) {
		if r.Width > w || r.Height > h {
			continue
		}
		if r.Width == w && r.Height == h {
			continue
		}
		out = append(out, r)
	}
	return out
}

// IsRung reports whether w x h is a legal external size for a session launched
// at launchW x launchH.
func IsRung(w, h, launchW, launchH int32) bool {
	for _, r := range AvailableRungs(launchW, launchH) {
		if r.Width == w && r.Height == h {
			return true
		}
	}
	return false
}

// reduce divides a size by its greatest common divisor, yielding the aspect
// ratio in lowest terms.
func reduce(w, h int32) (int32, int32) {
	g := gcd(w, h)
	if g == 0 {
		return w, h
	}
	return w / g, h / g
}

func gcd(a, b int32) int32 {
	for b != 0 {
		a, b = b, a%b
	}
	if a < 0 {
		return -a
	}
	return a
}
