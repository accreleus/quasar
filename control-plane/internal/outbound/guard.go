package outbound

import (
	"context"
	"fmt"
	"net"
	"net/http"
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
// any non-public IP. dial, when non-nil, replaces the actual connect step (test
// seam) — the guard still runs in front of it.
func newGuardedHTTPClient(timeout time.Duration, lookup IPLookup, dial dialFunc) *http.Client {
	if lookup == nil {
		lookup = defaultLookupIP
	}
	return &http.Client{
		Timeout:       timeout,
		CheckRedirect: NoRedirect,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
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

// disallowedIP reports whether ip is one an outbound client must never connect
// to.
func disallowedIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified()
}
