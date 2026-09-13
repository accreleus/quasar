package semver

import "testing"

// The precedence half is the ordering key of the beta release channel (#121),
// so the SemVer 2.0.0 §11 examples are tested verbatim rather than paraphrased:
// getting "0.2.0-rc.2 < 0.2.0 < 0.2.1-rc.1" wrong offers an operator a downgrade.

func TestParseFull(t *testing.T) {
	tests := []struct {
		in      string
		wantOK  bool
		major   int
		minor   int
		patch   int
		pre     []string
		wantPre bool
	}{
		{in: "0.2.0", wantOK: true, minor: 2},
		{in: "v0.2.0", wantOK: true, minor: 2},
		{in: "1.2.3", wantOK: true, major: 1, minor: 2, patch: 3},
		{in: "0.2.0-rc.1", wantOK: true, minor: 2, pre: []string{"rc", "1"}, wantPre: true},
		{in: "0.2.0-rc1", wantOK: true, minor: 2, pre: []string{"rc1"}, wantPre: true},
		{in: "0.2.0-0.3.7", wantOK: true, minor: 2, pre: []string{"0", "3", "7"}, wantPre: true},
		{in: "0.2.0-x-y-z.-", wantOK: true, minor: 2, pre: []string{"x-y-z", "-"}, wantPre: true},
		// Build metadata is accepted and discarded (§10).
		{in: "0.2.0+build.5", wantOK: true, minor: 2},
		{in: "0.2.0-rc.1+build.5", wantOK: true, minor: 2, pre: []string{"rc", "1"}, wantPre: true},
		// A "-" inside build metadata must not be read as the prerelease cut.
		{in: "0.2.0+build-5", wantOK: true, minor: 2},

		{in: "", wantOK: false},
		{in: "0.2", wantOK: false},
		{in: "0.2.0.1", wantOK: false},
		{in: "0.2.x", wantOK: false},
		{in: "0.2.0-", wantOK: false},
		{in: "0.2.0-rc..1", wantOK: false},
		{in: "0.2.0-rc.1+", wantOK: false},
		{in: "0.2.0-rc_1", wantOK: false},
		{in: "dev", wantOK: false},
	}
	for _, tc := range tests {
		got, ok := ParseFull(tc.in)
		if ok != tc.wantOK {
			t.Errorf("ParseFull(%q) ok = %v, want %v", tc.in, ok, tc.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if got.Major != tc.major || got.Minor != tc.minor || got.Patch != tc.patch {
			t.Errorf("ParseFull(%q) core = %d.%d.%d, want %d.%d.%d",
				tc.in, got.Major, got.Minor, got.Patch, tc.major, tc.minor, tc.patch)
		}
		if got.IsPrerelease() != tc.wantPre {
			t.Errorf("ParseFull(%q).IsPrerelease() = %v, want %v", tc.in, got.IsPrerelease(), tc.wantPre)
		}
		if len(got.Pre) != len(tc.pre) {
			t.Fatalf("ParseFull(%q).Pre = %v, want %v", tc.in, got.Pre, tc.pre)
		}
		for i := range tc.pre {
			if got.Pre[i] != tc.pre[i] {
				t.Errorf("ParseFull(%q).Pre = %v, want %v", tc.in, got.Pre, tc.pre)
				break
			}
		}
	}
}

// TestParseIsUnchangedByPrecedence pins the split: the strict grammar the client
// handshake gates on must still refuse everything it refused before.
func TestParseIsUnchangedByPrecedence(t *testing.T) {
	for _, s := range []string{"0.2.0-rc.1", "0.2.0+build", "1.2.0-rc1"} {
		if Valid(s) {
			t.Errorf("Valid(%q) = true; the strict grammar must not have grown a prerelease", s)
		}
	}
	if !Valid("1.2.0") {
		t.Error("Valid(\"1.2.0\") = false; the strict grammar must still accept a plain version")
	}
}

func TestComparePrecedence(t *testing.T) {
	// Each list is in ASCENDING precedence order; every pair is checked both
	// ways, so an asymmetric comparator cannot pass.
	ladders := [][]string{
		// SemVer §11.2 / §11.3, verbatim.
		{"1.0.0", "2.0.0", "2.1.0", "2.1.1"},
		{"1.0.0-alpha", "1.0.0"},
		// §11.4, verbatim.
		{
			"1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta",
			"1.0.0-beta.2", "1.0.0-beta.11", "1.0.0-rc.1", "1.0.0",
		},
		// The ordering the beta channel exists for.
		{"0.2.0-rc.1", "0.2.0-rc.2", "0.2.0", "0.2.1-rc.1", "0.2.1", "0.3.0-rc.1"},
		// Numeric identifiers are NOT string-ordered: rc.9 < rc.10 < rc.100.
		{"0.3.0-rc.9", "0.3.0-rc.10", "0.3.0-rc.100"},
		// A numeric identifier orders below an alphanumeric one (§11.4.3).
		{"1.0.0-1", "1.0.0-alpha"},
		// A larger set of identifiers wins when every preceding one is equal.
		{"1.0.0-rc.1", "1.0.0-rc.1.1"},
	}
	for _, ladder := range ladders {
		for i := 0; i < len(ladder); i++ {
			for j := 0; j < len(ladder); j++ {
				a, okA := ParseFull(ladder[i])
				b, okB := ParseFull(ladder[j])
				if !okA || !okB {
					t.Fatalf("ladder entry did not parse: %q (%v) / %q (%v)", ladder[i], okA, ladder[j], okB)
				}
				want := 0
				switch {
				case i < j:
					want = -1
				case i > j:
					want = 1
				}
				if got := ComparePrecedence(a, b); got != want {
					t.Errorf("ComparePrecedence(%q, %q) = %d, want %d", ladder[i], ladder[j], got, want)
				}
			}
		}
	}
}

// Build metadata takes no part in precedence (§10), so two versions differing
// only in it compare EQUAL rather than one winning arbitrarily.
func TestBuildMetadataIsIgnored(t *testing.T) {
	a, _ := ParseFull("0.2.0+aaa")
	b, _ := ParseFull("0.2.0+zzz")
	if got := ComparePrecedence(a, b); got != 0 {
		t.Errorf("ComparePrecedence with differing build metadata = %d, want 0", got)
	}
}

// Leading zeros are read numerically rather than rejected: the strict Parse this
// builds on already tolerates them, and two parsers that disagreed about "01"
// would be worse than one that is lenient in a documented way.
func TestLeadingZerosCompareNumerically(t *testing.T) {
	a, okA := ParseFull("0.2.0-rc.02")
	b, okB := ParseFull("0.2.0-rc.10")
	if !okA || !okB {
		t.Fatalf("leading-zero versions did not parse: %v / %v", okA, okB)
	}
	if got := ComparePrecedence(a, b); got != -1 {
		t.Errorf("ComparePrecedence(rc.02, rc.10) = %d, want -1", got)
	}
}
