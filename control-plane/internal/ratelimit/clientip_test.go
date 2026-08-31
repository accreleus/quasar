package ratelimit

import (
	"net"
	"net/http/httptest"
	"strings"
	"testing"
)

func mustCIDRs(t *testing.T, cidrs ...string) []*net.IPNet {
	t.Helper()
	var out []*net.IPNet
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			t.Fatalf("ParseCIDR(%q): %v", c, err)
		}
		out = append(out, n)
	}
	return out
}

// TestClientIP is the whole #438 policy in one table. The load-bearing rows are
// the two failure modes the policy exists to sit between: an untrusted peer
// must never be able to pick its own limiter key, and a trusted proxy must not
// collapse every client behind it onto one key.
func TestClientIP(t *testing.T) {
	proxy := mustCIDRs(t, "172.18.0.0/16")
	proxyV6 := mustCIDRs(t, "fd00::/8")
	chain := mustCIDRs(t, "172.18.0.0/16", "10.9.0.0/16")

	cases := []struct {
		name    string
		remote  string
		xff     []string
		trusted []*net.IPNet
		want    string
	}{
		{
			name:    "no trusted proxies configured ignores the header (today's behaviour)",
			remote:  "192.0.2.10:4567",
			xff:     []string{"203.0.113.9"},
			trusted: nil,
			want:    "192.0.2.10",
		},
		{
			name:    "no trusted proxies configured and no header",
			remote:  "192.0.2.10:4567",
			trusted: nil,
			want:    "192.0.2.10",
		},
		{
			name:    "untrusted peer cannot spoof its key even when proxies are configured",
			remote:  "192.0.2.10:4567",
			xff:     []string{"203.0.113.9"},
			trusted: proxy,
			want:    "192.0.2.10",
		},
		{
			name:    "trusted peer with a clean single-hop header",
			remote:  "172.18.0.5:33000",
			xff:     []string{"203.0.113.9"},
			trusted: proxy,
			want:    "203.0.113.9",
		},
		{
			name:    "two clients behind the same trusted proxy resolve differently (A)",
			remote:  "172.18.0.5:33000",
			xff:     []string{"203.0.113.9"},
			trusted: proxy,
			want:    "203.0.113.9",
		},
		{
			name:    "two clients behind the same trusted proxy resolve differently (B)",
			remote:  "172.18.0.5:33001",
			xff:     []string{"198.51.100.4"},
			trusted: proxy,
			want:    "198.51.100.4",
		},
		{
			name:   "client-injected hops to the left are discarded",
			remote: "172.18.0.5:33000",
			// The client sent "1.2.3.4"; the proxy appended the real peer.
			xff:     []string{"1.2.3.4, 203.0.113.9"},
			trusted: proxy,
			want:    "203.0.113.9",
		},
		{
			name:    "spoofed multi-hop walks right-to-left past trusted hops only",
			remote:  "172.18.0.5:33000",
			xff:     []string{"9.9.9.9, 203.0.113.9, 10.9.0.7"},
			trusted: chain,
			want:    "203.0.113.9",
		},
		{
			name:    "repeated header instances keep chain order",
			remote:  "172.18.0.5:33000",
			xff:     []string{"9.9.9.9", "203.0.113.9, 10.9.0.7"},
			trusted: chain,
			want:    "203.0.113.9",
		},
		{
			name:    "a chain of only trusted proxies falls back to the peer",
			remote:  "172.18.0.5:33000",
			xff:     []string{"10.9.0.7, 172.18.0.9"},
			trusted: chain,
			want:    "172.18.0.5",
		},
		{
			name:    "trusted peer with no header falls back to the peer",
			remote:  "172.18.0.5:33000",
			trusted: proxy,
			want:    "172.18.0.5",
		},
		{
			name:    "trusted peer with an empty header falls back to the peer",
			remote:  "172.18.0.5:33000",
			xff:     []string{""},
			trusted: proxy,
			want:    "172.18.0.5",
		},
		{
			// GATING (review): under an APPENDING proxy every element except the
			// last is attacker-supplied, so an injected leading empty element
			// must not be able to force the peer fallback — that would collapse
			// every client back onto the proxy's single budget, which is the
			// lockout DoS #438 exists to close. The decision point is the last
			// hop and it is clean.
			name:    "a client-injected leading empty element cannot force peer fallback",
			remote:  "172.18.0.5:33000",
			xff:     []string{", 203.0.113.9"},
			trusted: proxy,
			want:    "203.0.113.9",
		},
		{
			name:    "a bare comma from the client cannot force peer fallback",
			remote:  "172.18.0.5:33000",
			xff:     []string{",", "203.0.113.9"},
			trusted: proxy,
			want:    "203.0.113.9",
		},
		{
			name:    "junk left of the decision point is ignored",
			remote:  "172.18.0.5:33000",
			xff:     []string{"junk, 203.0.113.9"},
			trusted: proxy,
			want:    "203.0.113.9",
		},
		{
			name:    "junk AT the decision point falls back to the peer",
			remote:  "172.18.0.5:33000",
			xff:     []string{"junk,"},
			trusted: proxy,
			want:    "172.18.0.5",
		},
		{
			name:    "junk at the decision point with a clean hop to its left",
			remote:  "172.18.0.5:33000",
			xff:     []string{"203.0.113.9,junk"},
			trusted: proxy,
			want:    "172.18.0.5",
		},
		{
			name:    "junk left of a trusted hop that is skipped over",
			remote:  "172.18.0.5:33000",
			xff:     []string{"junk, 203.0.113.9, 10.9.0.7"},
			trusted: chain,
			want:    "203.0.113.9",
		},
		{
			name:    "malformed rightmost hop falls back to the peer",
			remote:  "172.18.0.5:33000",
			xff:     []string{"203.0.113.9, not-an-ip"},
			trusted: proxy,
			want:    "172.18.0.5",
		},
		{
			name:    "a hop carrying a port is malformed and falls back to the peer",
			remote:  "172.18.0.5:33000",
			xff:     []string{"203.0.113.9:8080"},
			trusted: proxy,
			want:    "172.18.0.5",
		},
		{
			// An empty element LEFT of the decision point is attacker noise and
			// is ignored, for the same reason as the leading-empty case above.
			name:    "an empty element mid-chain does not reach the decision",
			remote:  "172.18.0.5:33000",
			xff:     []string{"203.0.113.9, , 198.51.100.4"},
			trusted: proxy,
			want:    "198.51.100.4",
		},
		{
			name:    "IPv6 peer and IPv6 forwarded client",
			remote:  "[fd00::5]:33000",
			xff:     []string{"2001:db8::1234"},
			trusted: proxyV6,
			want:    "2001:db8::1234",
		},
		{
			name:    "IPv6 forwarded client is canonicalised so it cannot be respelled",
			remote:  "[fd00::5]:33000",
			xff:     []string{"2001:0DB8:0000:0000:0000:0000:0000:1234"},
			trusted: proxyV6,
			want:    "2001:db8::1234",
		},
		{
			name:    "IPv4-mapped IPv6 forwarded client canonicalises to dotted quad",
			remote:  "172.18.0.5:33000",
			xff:     []string{"::ffff:203.0.113.9"},
			trusted: proxy,
			want:    "203.0.113.9",
		},
		{
			name:    "an untrusted IPv6 peer is not widened by an IPv4 trusted CIDR",
			remote:  "[2001:db8::9]:33000",
			xff:     []string{"203.0.113.9"},
			trusted: proxy,
			want:    "2001:db8::9",
		},
		{
			name:    "RemoteAddr without a port stays an opaque key",
			remote:  "unix-socket",
			xff:     []string{"203.0.113.9"},
			trusted: proxy,
			want:    "unix-socket",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "http://example.test", nil)
			r.RemoteAddr = tc.remote
			r.Header.Del("X-Forwarded-For")
			for _, v := range tc.xff {
				r.Header.Add("X-Forwarded-For", v)
			}
			if got := ClientIP(r, tc.trusted); got != tc.want {
				t.Fatalf("ClientIP = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestClientIPBoundsTheForwardedChain pins the hop cap: an attacker padding
// X-Forwarded-For controls per-request parsing work, so beyond maxForwardedHops
// the header is discarded entirely. The fallback is the peer — the same
// conservative answer as a malformed chain, so padding can only lose the
// benefit of forwarding, never steer the key.
func TestClientIPBoundsTheForwardedChain(t *testing.T) {
	proxy := mustCIDRs(t, "172.18.0.0/16")

	build := func(n int) string {
		hops := make([]string, 0, n)
		for i := 0; i < n-1; i++ {
			hops = append(hops, "192.0.2.1")
		}
		return strings.Join(append(hops, "203.0.113.9"), ", ")
	}

	r := httptest.NewRequest("GET", "http://example.test", nil)
	r.RemoteAddr = "172.18.0.5:33000"
	r.Header.Set("X-Forwarded-For", build(maxForwardedHops))
	if got := ClientIP(r, proxy); got != "203.0.113.9" {
		t.Fatalf("at the cap: ClientIP = %q, want the forwarded client", got)
	}

	r.Header.Set("X-Forwarded-For", build(maxForwardedHops+1))
	if got := ClientIP(r, proxy); got != "172.18.0.5" {
		t.Fatalf("over the cap: ClientIP = %q, want the peer", got)
	}
}

// TestClientIPNeverReadsXRealIP pins the documented decision: the hardened
// overlay does not send X-Real-IP, so consulting it would only add a spoofable
// input with no legitimate producer.
func TestClientIPNeverReadsXRealIP(t *testing.T) {
	r := httptest.NewRequest("GET", "http://example.test", nil)
	r.RemoteAddr = "172.18.0.5:33000"
	r.Header.Set("X-Real-IP", "203.0.113.9")
	if got := ClientIP(r, mustCIDRs(t, "172.18.0.0/16")); got != "172.18.0.5" {
		t.Fatalf("ClientIP = %q, want the peer (X-Real-IP must be ignored)", got)
	}
}
