// Package semver is the single strict MAJOR.MINOR.PATCH grammar shared by the
// control plane — the native-client version handshake (P9-08 / #236) uses it
// both to validate the operator-configured floor at startup (config.Load) and
// to compare a client's reported version at login (internal/auth). Keeping one
// parser means the startup validator and the runtime gate cannot disagree (an
// earlier "all digits" startup check accepted an overflowing component that the
// runtime parser rejected, silently failing the gate OPEN).
package semver

import (
	"strconv"
	"strings"
)

// Version is a parsed strict MAJOR.MINOR.PATCH version.
type Version struct {
	Major, Minor, Patch int
}

// Parse parses a strict "MAJOR.MINOR.PATCH" string, tolerating a single leading
// "v" (e.g. "v1.2.0"). Pre-release / build metadata (e.g. "1.2.0-rc1",
// "1.2.0+meta") is NOT supported and reports ok=false. Non-numeric, signed, or
// out-of-int-range components are malformed (out-of-range is what makes this the
// authoritative grammar — a component like "99999999999999999999" is rejected,
// not silently accepted).
func Parse(s string) (Version, bool) {
	s = strings.TrimPrefix(s, "v")
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return Version{}, false
	}
	var v Version
	dst := []*int{&v.Major, &v.Minor, &v.Patch}
	for i, p := range parts {
		if p == "" || !isDigits(p) {
			return Version{}, false
		}
		n, err := strconv.Atoi(p)
		if err != nil { // overflow etc.
			return Version{}, false
		}
		*dst[i] = n
	}
	return v, true
}

// Valid reports whether s is a well-formed strict version per Parse.
func Valid(s string) bool {
	_, ok := Parse(s)
	return ok
}

// Compare returns -1 if a<b, 0 if a==b, +1 if a>b.
func Compare(a, b Version) int {
	for _, d := range [][2]int{{a.Major, b.Major}, {a.Minor, b.Minor}, {a.Patch, b.Patch}} {
		switch {
		case d[0] < d[1]:
			return -1
		case d[0] > d[1]:
			return 1
		}
	}
	return 0
}

// isDigits reports whether s is non-empty and all ASCII digits (so strconv.Atoi
// cannot slip in a leading '+'/'-' sign).
func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}
