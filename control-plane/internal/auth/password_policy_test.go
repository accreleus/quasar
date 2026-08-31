package auth

import "testing"

// TestValidatePasswordFormat covers the length + common-password rules in
// isolation (#513) — no identity/DB dependency, exactly the pure check
// ChangePassword runs before touching the database.
func TestValidatePasswordFormat(t *testing.T) {
	cases := []struct {
		name     string
		password string
		wantErr  bool
		wantMsg  string // substring; empty = don't check
	}{
		{
			name:     "11 chars rejected — below the 12 floor",
			password: "eleven-char", // 11 runes
			wantErr:  true,
			wantMsg:  "at least 12",
		},
		{
			name:     "12 chars accepted — exactly the floor",
			password: "twelve-chars", // 12 runes
			wantErr:  false,
		},
		{
			name:     "128 chars accepted — exactly the ceiling",
			password: makeRunes('a', 128),
			wantErr:  false,
		},
		{
			name:     "129 chars rejected — over the ceiling",
			password: makeRunes('a', 129),
			wantErr:  true,
			wantMsg:  "at most 128",
		},
		{
			name:     "common password rejected with its own message",
			password: "Password1234", // 12 chars, exact-match (case-insensitively) against the embedded list
			wantErr:  true,
			wantMsg:  "too common",
		},
		{
			name:     "common password padded differently is not flagged (exact match only, not substring)",
			password: "xxpassword1234xx", // contains a common entry but is not equal to it
			wantErr:  false,
		},
		{
			name:     "unicode password counted in runes, not bytes: 12 multi-byte runes accepted",
			password: "pâsswörd-œ12", // 12 runes, >12 UTF-8 bytes
			wantErr:  false,
		},
		{
			name:     "unicode password counted in runes, not bytes: 11 multi-byte runes rejected",
			password: "pâsswörd-œ1", // 11 runes
			wantErr:  true,
			wantMsg:  "at least 12",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validatePasswordFormat(tc.password)
			if tc.wantErr && err == nil {
				t.Fatalf("validatePasswordFormat(%q): want error, got nil", tc.password)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("validatePasswordFormat(%q): want no error, got %v", tc.password, err)
			}
			if tc.wantErr && tc.wantMsg != "" {
				if !containsFold(err.Error(), tc.wantMsg) {
					t.Fatalf("validatePasswordFormat(%q): error %q does not contain %q", tc.password, err.Error(), tc.wantMsg)
				}
			}
		})
	}
}

// TestCheckPasswordIdentity covers the username/email-local-part containment
// rule (#513) in isolation.
func TestCheckPasswordIdentity(t *testing.T) {
	cases := []struct {
		name        string
		password    string
		identifiers []string
		wantErr     bool
	}{
		{
			name:        "password equal to username rejected",
			password:    "quasaradmin",
			identifiers: []string{"quasaradmin", "owner"},
			wantErr:     true,
		},
		{
			name:        "password containing username rejected",
			password:    "quasaradmin-2026!",
			identifiers: []string{"quasaradmin", "owner"},
			wantErr:     true,
		},
		{
			name:        "password containing email local-part rejected",
			password:    "letme-inowner-please",
			identifiers: []string{"someone", "inowner"},
			wantErr:     true,
		},
		{
			name:        "match is case-insensitive",
			password:    "QuasarAdmin-2026",
			identifiers: []string{"quasaradmin"},
			wantErr:     true,
		},
		{
			name:        "unrelated password accepted",
			password:    "correct-horse-battery",
			identifiers: []string{"owner", "ownermail"},
			wantErr:     false,
		},
		{
			name:        "short identifiers (below minIdentifierLen) are skipped, not compared",
			password:    "any-old-password",
			identifiers: []string{"o", "an"}, // would trivially match by chance otherwise
			wantErr:     false,
		},
		{
			name:        "empty identifiers are skipped",
			password:    "any-old-password",
			identifiers: []string{"", "   "},
			wantErr:     false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkPasswordIdentity(tc.password, tc.identifiers...)
			if tc.wantErr && err == nil {
				t.Fatalf("checkPasswordIdentity(%q, %v): want error, got nil", tc.password, tc.identifiers)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("checkPasswordIdentity(%q, %v): want no error, got %v", tc.password, tc.identifiers, err)
			}
		})
	}
}

func TestEmailLocalPart(t *testing.T) {
	cases := map[string]string{
		"ada@example.com": "ada",
		"no-at-sign":      "no-at-sign",
		"a@b@c":           "a",
	}
	for in, want := range cases {
		if got := emailLocalPart(in); got != want {
			t.Errorf("emailLocalPart(%q) = %q, want %q", in, got, want)
		}
	}
}

// makeRunes builds a rune-counted string of length n from a repeated
// character — used to hit the exact boundary values without a byte/rune
// mismatch (all-ASCII, so bytes == runes, isolating the boundary test from
// the separate unicode-counting test above).
func makeRunes(r rune, n int) string {
	out := make([]rune, n)
	for i := range out {
		out[i] = r
	}
	return string(out)
}

func containsFold(s, substr string) bool {
	sl, subl := []rune(s), []rune(substr)
	toLower := func(rs []rune) []rune {
		out := make([]rune, len(rs))
		for i, r := range rs {
			if r >= 'A' && r <= 'Z' {
				r += 'a' - 'A'
			}
			out[i] = r
		}
		return out
	}
	sl, subl = toLower(sl), toLower(subl)
	for i := 0; i+len(subl) <= len(sl); i++ {
		if string(sl[i:i+len(subl)]) == string(subl) {
			return true
		}
	}
	return false
}
