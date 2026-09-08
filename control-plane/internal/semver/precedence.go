package semver

import (
	"strings"
)

// The full SemVer 2.0.0 grammar, added for the beta release channel (#121).
//
// Parse/Valid/Compare above are the STRICT MAJOR.MINOR.PATCH grammar the native
// client handshake gates on, and they stay strict: a client that reports
// "1.2.0-rc.1" is still not a version that grammar accepts. This half exists for
// the one place a prerelease is a first-class value — ordering platform releases
// on a channel that offers them — and it lives in the same package so there is
// still exactly one version parser in the control plane.

// Full is a parsed SemVer 2.0.0 version: the MAJOR.MINOR.PATCH core plus the
// dot-separated prerelease identifiers. Build metadata is parsed and DISCARDED —
// SemVer §10 excludes it from precedence, so keeping it would only invite a
// comparison that used it.
type Full struct {
	Version
	// Pre is nil on a release version and non-empty on a prerelease. An empty
	// non-nil slice is unrepresentable: "1.0.0-" does not parse.
	Pre []string
}

// IsPrerelease reports whether the version carries a prerelease part, which is
// what makes it order BELOW the same core version.
func (f Full) IsPrerelease() bool { return len(f.Pre) > 0 }

// ParseFull parses "MAJOR.MINOR.PATCH[-prerelease][+build]", tolerating a single
// leading "v". The core is the same grammar Parse accepts (which means a leading
// zero such as "01" reads as 1 rather than being rejected — one lenience, in one
// place, rather than two parsers that disagree). Every prerelease identifier
// must be non-empty and drawn from [0-9A-Za-z-]; anything else is ok=false, and
// a caller is expected to have a defined fallback for that answer.
func ParseFull(s string) (Full, bool) {
	s = strings.TrimSpace(s)
	// Build metadata comes off first: it may itself contain "-", so cutting the
	// prerelease off before it would swallow the wrong bytes.
	if core, build, ok := strings.Cut(s, "+"); ok {
		if !validIdentifierList(build) {
			return Full{}, false
		}
		s = core
	}
	core, pre, hasPre := strings.Cut(s, "-")
	v, ok := Parse(core)
	if !ok {
		return Full{}, false
	}
	if !hasPre {
		return Full{Version: v}, true
	}
	if !validIdentifierList(pre) {
		return Full{}, false
	}
	return Full{Version: v, Pre: strings.Split(pre, ".")}, true
}

// ComparePrecedence returns -1 if a orders below b, 0 if they are equal in
// precedence, +1 if a orders above b — SemVer 2.0.0 §11.
func ComparePrecedence(a, b Full) int {
	if c := Compare(a.Version, b.Version); c != 0 {
		return c
	}
	// §11.3: a version WITH a prerelease has lower precedence than the same
	// core version without one. This is the rule that puts 0.2.0-rc.2 below
	// 0.2.0 and 0.2.0 below 0.2.1-rc.1.
	switch {
	case !a.IsPrerelease() && !b.IsPrerelease():
		return 0
	case !a.IsPrerelease():
		return 1
	case !b.IsPrerelease():
		return -1
	}
	return comparePre(a.Pre, b.Pre)
}

// comparePre is §11.4: compare identifiers left to right, and a larger SET of
// identifiers wins when every preceding one is equal ("rc.1" < "rc.1.1").
func comparePre(a, b []string) int {
	n := min(len(a), len(b))
	for i := 0; i < n; i++ {
		if c := compareIdentifier(a[i], b[i]); c != 0 {
			return c
		}
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	}
	return 0
}

// compareIdentifier is §11.4.1–3: numeric identifiers compare NUMERICALLY (so
// rc.10 orders above rc.9, which string order gets backwards), alphanumeric ones
// compare in ASCII order, and a numeric identifier always orders below an
// alphanumeric one.
func compareIdentifier(a, b string) int {
	na, nb := isDigits(a), isDigits(b)
	switch {
	case na && !nb:
		return -1
	case !na && nb:
		return 1
	case !na && !nb:
		return strings.Compare(a, b)
	}
	// Both numeric. Compared as digit strings rather than parsed integers:
	// exact at any length, with no overflow to get wrong.
	x, y := strings.TrimLeft(a, "0"), strings.TrimLeft(b, "0")
	if len(x) != len(y) {
		if len(x) < len(y) {
			return -1
		}
		return 1
	}
	return strings.Compare(x, y)
}

// validIdentifierList checks a dot-separated run of SemVer identifiers: at least
// one, each non-empty and drawn from [0-9A-Za-z-].
func validIdentifierList(s string) bool {
	if s == "" {
		return false
	}
	for _, id := range strings.Split(s, ".") {
		if id == "" {
			return false
		}
		for _, r := range id {
			switch {
			case r >= '0' && r <= '9',
				r >= 'A' && r <= 'Z',
				r >= 'a' && r <= 'z',
				r == '-':
			default:
				return false
			}
		}
	}
	return true
}
