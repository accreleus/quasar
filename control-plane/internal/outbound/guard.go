package outbound

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"time"
)

// IPLookup resolves a host to its IPs. Injectable so the DNS-rebind guard can
// be tested with a resolver returning a private address for an allowed host.
type IPLookup func(ctx context.Context, host string) ([]net.IP, error)

// dialFunc is the shape of http.Transport.DialContext.
type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

func defaultLookupIP(ctx context.Context, host string) ([]net.IP, error) {
	return net.DefaultResolver.LookupIP(ctx, "ip", host)
}

// NoRedirect is the CheckRedirect every outbound client uses: a remote response
// is untrusted, and a 3xx Location could point the next request past the
// allowlist and the dial guard at an internal address.
func NoRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// newGuardedHTTPClient: per-request timeout, no redirects, DialContext refuses
// any non-public IP. Proxy is nil on purpose: through a proxy the dialled
// address is the proxy's, so the check would never see the target. dial, when
// non-nil, replaces the actual connect step (test seam); the guard still runs
// in front of it.
func newGuardedHTTPClient(timeout time.Duration, lookup IPLookup, dial dialFunc) *http.Client {
	if lookup == nil {
		lookup = defaultLookupIP
	}
	return &http.Client{
		Timeout:       timeout,
		CheckRedirect: NoRedirect,
		Transport: &http.Transport{
			DialContext:           guardedDialContext(lookup, dial),
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          10,
			IdleConnTimeout:       30 * time.Second,
			TLSHandshakeTimeout:   5 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}
}

// guardedDialContext refuses every address DisallowedIP refuses, on every
// connection (DNS-rebind guard): resolves the host itself
// and dials the resolved IP directly, so a name resolving public-then-private
// across two lookups can't slip past. TLS ServerName still derives from the
// request URL, unaffected.
func guardedDialContext(lookup IPLookup, dial dialFunc) dialFunc {
	if dial == nil {
		d := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
		dial = d.DialContext
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("split dial address %q: %w", addr, err)
		}
		ips, err := lookup(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("resolve %q: %w", host, err)
		}
		for _, ip := range ips {
			if DisallowedIP(ip) {
				return nil, fmt.Errorf("refusing connection to non-public address %s (host %q)", ip, host)
			}
		}
		var lastErr error
		for _, ip := range ips {
			conn, err := dial(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
			lastErr = err
		}
		if lastErr == nil {
			lastErr = fmt.Errorf("no addresses for %q", host)
		}
		return nil, lastErr
	}
}

// IANA special-purpose IPv4 registry plus multicast; everything else in IPv4
// is public unicast.
var specialV4 = mustPrefixes(
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
	"172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16",
	"198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
)

// IPv6 is an allowlist: only global unicast (2000::/3) can pass, minus these
// special-purpose ranges inside it. 6to4 and Teredo are refused whole rather
// than decoded.
var (
	globalUnicastV6 = netip.MustParsePrefix("2000::/3")
	specialV6       = mustPrefixes("2001::/23", "2001:db8::/32", "2002::/16", "3fff::/20")
	// DNS64 maps IPv4-only hosts into this prefix; the embedded IPv4 decides.
	nat64 = netip.MustParsePrefix("64:ff9b::/96")
)

func mustPrefixes(ss ...string) []netip.Prefix {
	ps := make([]netip.Prefix, len(ss))
	for i, s := range ss {
		ps[i] = netip.MustParsePrefix(s)
	}
	return ps
}

func inAny(ps []netip.Prefix, a netip.Addr) bool {
	for _, p := range ps {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// DisallowedIP reports whether ip is one an outbound client must never connect
// to: anything that is not public unicast. An unparseable address is refused.
func DisallowedIP(ip net.IP) bool {
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}
	a = a.Unmap() // IPv4-mapped ::ffff:a.b.c.d is judged as IPv4
	if a.Is4() {
		return inAny(specialV4, a)
	}
	if nat64.Contains(a) {
		b := a.As16()
		return DisallowedIP(net.IP(b[12:]))
	}
	return !globalUnicastV6.Contains(a) || inAny(specialV6, a)
}
