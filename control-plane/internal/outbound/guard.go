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
// address is the proxy's, so the check would never see the target. dial, when non-nil, replaces the actual connect step (test
// seam) — the guard still runs in front of it.
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

// guardedDialContext refuses loopback/private/link-local/multicast/unspecified
// addresses on every connection (DNS-rebind guard): resolves the host itself
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
			if disallowedIP(ip) {
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

// nonPublic is the IANA special-purpose address registries plus multicast,
// minus anything globally reachable. The 6to4, Teredo and NAT64 prefixes are
// refused whole, so an IPv4 target embedded in them needs no decoding.
var nonPublic = func() []netip.Prefix {
	var ps []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
		"172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16",
		"198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
		"::/96", "::1/128", "64:ff9b::/96", "64:ff9b:1::/48", "100::/64", "2001::/23",
		"2001:db8::/32", "2002::/16", "3fff::/20", "5f00::/16", "fc00::/7", "fe80::/10", "ff00::/8",
	} {
		ps = append(ps, netip.MustParsePrefix(s))
	}
	return ps
}()

// DisallowedIP reports whether ip is one an outbound client must never connect
// to: anything that is not global unicast. An unparseable address is refused.
func DisallowedIP(ip net.IP) bool { return disallowedIP(ip) }

func disallowedIP(ip net.IP) bool {
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}
	a = a.Unmap().WithZone("")
	for _, p := range nonPublic {
		if p.Contains(a) {
			return true
		}
	}
	return false
}
