package origins

import (
	"context"
	"strings"
	"sync"
	"testing"
)

func TestNormalize(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
		ok   bool
	}{
		{"https://quasar.example.com", "https://quasar.example.com", true},
		{"https://QUASAR.example.com:8443", "https://quasar.example.com:8443", true},
		{"http://192.0.2.10:8080", "http://192.0.2.10:8080", true},
		{"  https://spaced.example  ", "https://spaced.example", true},

		// An Origin header carries none of these. Accepting them is what would let
		// a merely host-shaped, attacker-controlled URI be treated as same-origin.
		{"https://evil.example/@trusted.host", "", false},
		{"https://evil.example/", "", false},
		{"https://user:pw@evil.example", "", false},
		{"https://evil.example?a=b", "", false},
		{"https://evil.example#frag", "", false},
		{"ftp://quasar.example.com", "", false},
		{"file:///etc/passwd", "", false},
		{"quasar.example.com", "", false},
		{"", "", false},
		{"*", "", false},
		{"https://" + strings.Repeat("a", MaxEntryLength), "", false},
	} {
		got, ok := Normalize(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("Normalize(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// TestValidateListRejectsWildcard is the §S6e rule. "*" is the value an operator
// is most likely to reach for and the one that discards the layer entirely, so
// it must be refused rather than quietly stored as an unmatchable literal.
func TestValidateListRejectsWildcard(t *testing.T) {
	_, err := ValidateList([]string{"https://ok.example", "*"})
	if err == nil {
		t.Fatal("\"*\" was accepted")
	}
	if !strings.Contains(err.Error(), "wildcard") {
		t.Errorf("message = %q, want it to explain what a wildcard would do", err)
	}
	if !strings.Contains(err.Error(), "entry 2") {
		t.Errorf("message = %q, want the offending position named", err)
	}
}

func TestValidateListNormalizesDedupesAndDropsBlanks(t *testing.T) {
	got, err := ValidateList([]string{
		"https://A.example:8443",
		"",
		"   ",
		"https://a.example:8443", // duplicate after normalization
		"http://b.example",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"https://a.example:8443", "http://b.example"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestValidateListNamesTheBadEntryPosition(t *testing.T) {
	_, err := ValidateList([]string{"https://ok.example", "https://bad.example/path"})
	if err == nil || !strings.Contains(err.Error(), "entry 2") {
		t.Fatalf("err = %v, want the position named so an operator can find it", err)
	}
}

func TestValidateListBoundsLength(t *testing.T) {
	many := make([]string, MaxEntries+1)
	for i := range many {
		many[i] = "https://h" + string(rune('a'+i%26)) + string(rune('a'+i/26)) + ".example"
	}
	if _, err := ValidateList(many); err == nil {
		t.Fatal("an unbounded allow-list was accepted")
	}
}

func TestValidateListEmptyIsLegal(t *testing.T) {
	// An explicitly-sent [] means "clear the list", which is a legitimate request
	// and must not be an error.
	got, err := ValidateList(nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("ValidateList(nil) = (%v, %v), want an empty list and no error", got, err)
	}
}

// TestNormalizeCanonicalisesIPv6Literals — round 3. The same IPv6 address has
// many spellings; a browser sends the compressed form while an operator often
// pastes whatever their tooling printed. Comparing the raw strings would never
// match, and would fail as silently as the default-port bug did.
func TestNormalizeCanonicalisesIPv6Literals(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"https://[2001:0db8:0000:0000:0000:0000:0000:0001]:8443", "https://[2001:db8::1]:8443"},
		{"https://[2001:db8::1]:8443", "https://[2001:db8::1]:8443"},
		{"https://[2001:DB8::1]", "https://[2001:db8::1]"},
		{"https://[2001:db8::1]:443", "https://[2001:db8::1]"}, // default port still dropped
		{"http://[::1]:80", "http://[::1]"},
		// IPv4 literals are canonical already, but must not be damaged.
		{"https://192.0.2.10:8443", "https://192.0.2.10"[:len("https://192.0.2.10")] + ":8443"},
	} {
		got, ok := Normalize(tc.in)
		if !ok {
			t.Errorf("Normalize(%q) rejected a valid origin", tc.in)
			continue
		}
		if got != tc.want {
			t.Errorf("Normalize(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestSameOriginExemptionCanonicalisesIPv6 — the exemption compares the request
// Host against the origin authority, so it needs the same treatment or a
// literal-addressed instance silently loses it.
func TestSameOriginExemptionCanonicalisesIPv6(t *testing.T) {
	r := NewResolver("", false, nil, nil)
	for _, tc := range []struct {
		origin, host string
		want         bool
	}{
		{"https://[2001:db8::1]:8443", "[2001:0db8:0:0:0:0:0:1]:8443", true},
		{"https://[2001:0db8::0001]:8443", "[2001:db8::1]:8443", true},
		{"https://[2001:db8::1]", "[2001:db8::1]:443", true},
		{"https://[2001:db8::1]:8443", "[2001:db8::2]:8443", false},
	} {
		if got := r.Decide(context.Background(), tc.origin, tc.host); got.SameOrigin != tc.want {
			t.Errorf("Decide(%q, %q).SameOrigin = %v, want %v", tc.origin, tc.host, got.SameOrigin, tc.want)
		}
	}
}

// TestAllowListMatchesAcrossIPv6Spellings — an entry saved in one spelling must
// match a browser sending another.
func TestAllowListMatchesAcrossIPv6Spellings(t *testing.T) {
	list, err := ValidateList([]string{"https://[2001:0db8:0000::0001]:8443"})
	if err != nil {
		t.Fatalf("ValidateList: %v", err)
	}
	r := NewResolver("", false, stubStore{list: list}, nil)
	d := r.Decide(context.Background(), "https://[2001:db8::1]:8443", "internal.lan")
	if !d.Listed || !d.Allowed {
		t.Fatalf("an allow-list entry did not match the same address in compressed form (stored %v)", list)
	}
}

type stubStore struct{ list []string }

func (s stubStore) AllowedOrigins(context.Context) ([]string, error) { return s.list, nil }

// TestNormalizeRejectsOutOfRangePorts — round 5. url.Parse accepts any digits,
// so ":65536" would be stored as an ordinary-looking allow-list entry that no
// browser can ever send: silently doing nothing, the same failure mode as the
// default-port and IPv6-spelling bugs.
func TestNormalizeRejectsOutOfRangePorts(t *testing.T) {
	for _, bad := range []string{
		"https://example.com:65536",
		"https://example.com:99999",
		"https://example.com:0",
		"https://example.com:-1",
		"https://[2001:db8::1]:65536",
	} {
		if got, ok := Normalize(bad); ok {
			t.Errorf("Normalize(%q) = %q, accepted an unreachable port", bad, got)
		}
	}
	for _, good := range []string{
		"https://example.com:1",
		"https://example.com:65535",
		"https://example.com:8443",
	} {
		if _, ok := Normalize(good); !ok {
			t.Errorf("Normalize(%q) rejected a valid port", good)
		}
	}
}

// TestValidateListRejectsOutOfRangePortWithItsPosition — the operator has to be
// able to find the entry they mistyped.
func TestValidateListRejectsOutOfRangePortWithItsPosition(t *testing.T) {
	_, err := ValidateList([]string{"https://ok.example", "https://bad.example:65536"})
	if err == nil {
		t.Fatal("an out-of-range port was accepted into the allow-list")
	}
	if !strings.Contains(err.Error(), "entry 2") {
		t.Errorf("message = %q, want the offending position named", err)
	}
}

// TestNormalizeRejectsNonASCIIAuthority — round 6. A browser sends the punycode
// A-label, so a U-label entry could never match. Rejecting is the interim
// behaviour: strictly better than storing something unmatchable, and it fails
// with an actionable message instead of silently.
func TestNormalizeRejectsNonASCIIAuthority(t *testing.T) {
	for _, u := range []string{
		"https://bücher.example",
		"https://münchen.example:8443",
		"https://例え.テスト",
	} {
		if got, ok := Normalize(u); ok {
			t.Errorf("Normalize(%q) = %q, accepted an entry no browser can ever send", u, got)
		}
	}
	// The punycode form a browser actually sends is accepted and canonicalised.
	got, ok := Normalize("https://XN--BCHER-KVA.example:443")
	if !ok || got != "https://xn--bcher-kva.example" {
		t.Fatalf("Normalize(punycode) = (%q, %v), want the lowercased, port-stripped A-label", got, ok)
	}
}

// TestValidateListExplainsPunycode — the refusal has to teach, or the operator
// just retypes the same thing.
func TestValidateListExplainsPunycode(t *testing.T) {
	_, err := ValidateList([]string{"https://ok.example", "https://bücher.example"})
	if err == nil {
		t.Fatal("a non-ASCII entry was accepted")
	}
	if !strings.Contains(err.Error(), "punycode") || !strings.Contains(err.Error(), "entry 2") {
		t.Errorf("message = %q, want the position AND the punycode hint", err)
	}
}

// TestAnOlderFillCannotOverwriteANewerSnapshot — round 6. Concurrent misses
// could let an OLDER read land last and pin a stale allow-list for a full TTL:
// a removed origin keeps working, or one the admin just added keeps failing,
// with nothing to indicate why.
//
// Deterministic by construction, not by timing: the first read BLOCKS until the
// test releases it, so "the older fill returns last" is guaranteed rather than
// raced.
func TestAnOlderFillCannotOverwriteANewerSnapshot(t *testing.T) {
	store := &gatedStore{
		entered: make(chan struct{}),
		release: make(chan struct{}),
		first:   []string{"https://old.example"},
		later:   []string{"https://new.example"},
	}
	r := NewResolver("", false, store, nil)

	done := make(chan []string, 1)
	go func() {
		list, _ := r.Resolve(context.Background()) // fill A — blocks in the store
		done <- list
	}()
	<-store.entered // A is inside the read and holds no lock

	// Fill B starts strictly after A and completes strictly before it.
	r.Invalidate()
	if list, _ := r.Resolve(context.Background()); len(list) != 1 || list[0] != "https://new.example" {
		t.Fatalf("second fill = %v, want the newer snapshot", list)
	}

	close(store.release) // now let the OLDER fill finish
	<-done

	cached, _ := r.Resolve(context.Background())
	if len(cached) != 1 || cached[0] != "https://new.example" {
		t.Fatalf("cached = %v, want the newer snapshot; an older fill overwrote it and would pin a stale "+
			"allow-list for a full TTL after the admin's edit", cached)
	}
}

// gatedStore blocks its FIRST read until released; every later read returns
// immediately. That makes fill ordering explicit instead of timing-dependent.
type gatedStore struct {
	mu           sync.Mutex
	calls        int
	entered      chan struct{}
	release      chan struct{}
	first, later []string
}

func (g *gatedStore) AllowedOrigins(context.Context) ([]string, error) {
	g.mu.Lock()
	g.calls++
	n := g.calls
	g.mu.Unlock()
	if n == 1 {
		close(g.entered)
		<-g.release
		return append([]string(nil), g.first...), nil
	}
	return append([]string(nil), g.later...), nil
}
