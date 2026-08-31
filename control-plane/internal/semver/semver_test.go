package semver

import "testing"

func TestParse(t *testing.T) {
	cases := []struct {
		in         string
		wantOK     bool
		maj, mi, p int
	}{
		{"1.2.3", true, 1, 2, 3},
		{"v1.2.3", true, 1, 2, 3},
		{"0.0.0", true, 0, 0, 0},
		{"10.20.30", true, 10, 20, 30},
		{"", false, 0, 0, 0},
		{"1", false, 0, 0, 0},
		{"1.2", false, 0, 0, 0},
		{"1.2.3.4", false, 0, 0, 0},
		{"1.2.x", false, 0, 0, 0},
		{"1..3", false, 0, 0, 0},
		{"1.2.3-rc1", false, 0, 0, 0}, // pre-release unsupported → malformed
		{"1.2.3+meta", false, 0, 0, 0},
		{"-1.2.3", false, 0, 0, 0},
		{"1.+2.3", false, 0, 0, 0},
		{"  1.2.3", false, 0, 0, 0},
		{"99999999999999999999.0.0", false, 0, 0, 0}, // overflow → malformed (NOT accepted)
	}
	for _, c := range cases {
		got, ok := Parse(c.in)
		if ok != c.wantOK {
			t.Errorf("Parse(%q) ok = %v, want %v", c.in, ok, c.wantOK)
			continue
		}
		if ok && (got.Major != c.maj || got.Minor != c.mi || got.Patch != c.p) {
			t.Errorf("Parse(%q) = %+v, want {%d %d %d}", c.in, got, c.maj, c.mi, c.p)
		}
	}
}

func TestValid(t *testing.T) {
	for _, s := range []string{"1.2.3", "v0.1.0", "10.0.0"} {
		if !Valid(s) {
			t.Errorf("Valid(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "1.2", "1.2.3-rc1", "99999999999999999999.0.0", "x.y.z"} {
		if Valid(s) {
			t.Errorf("Valid(%q) = true, want false", s)
		}
	}
}

func TestCompare(t *testing.T) {
	mk := func(s string) Version { v, _ := Parse(s); return v }
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0},
		{"1.0.0", "1.0.1", -1},
		{"1.0.1", "1.0.0", 1},
		{"1.0.0", "1.1.0", -1},
		{"2.0.0", "1.9.9", 1},
		{"1.2.3", "1.2.3", 0},
		{"0.9.0", "1.0.0", -1},
	}
	for _, c := range cases {
		if got := Compare(mk(c.a), mk(c.b)); got != c.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}
