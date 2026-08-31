package ratelimit

import (
	"net"
	"net/http"
	"strings"
)

// ClientIP resolves the address a rate limiter should key on, under the #438
// trusted-proxy policy. RemoteAddr alone is catastrophic behind a proxy (every
// caller shares one budget — a pre-auth lockout DoS on the setup claim); a
// client-supplied header alone is the opposite failure (rotate the header, get
// unbounded budgets). Safe in both worlds: consume a forwarded address only
// when the direct peer is in QUASAR_TRUSTED_PROXIES (empty by default —
// headers never consulted), then walk X-Forwarded-For right-to-left to the
// first non-trusted address; everything left of it is attacker-controlled.
// X-Real-IP is never consulted — the hardened Caddy overlay does not send it,
// so it would only be a second, unset, spoofable input.
func ClientIP(r *http.Request, trusted []*net.IPNet) string {
	peer := peerHost(r.RemoteAddr)
	if len(trusted) == 0 {
		return peer
	}
	peerIP := net.ParseIP(peer)
	if peerIP == nil || !containsIP(trusted, peerIP) {
		// The direct peer is a real client (or unparseable). Its own address is
		// the only trustworthy fact about this request.
		return peer
	}
	return rightmostUntrusted(r.Header.Values("X-Forwarded-For"), trusted, peer)
}

// Bounds the flattened chain (a legitimate one is one or two hops; parsing an
// unbounded list is attacker-controlled work). Over the cap the whole header
// is discarded — the same fallback as a malformed chain, so exceeding it
// cannot steer the key.
const maxForwardedHops = 32

// rightmostUntrusted walks the chain from the end (the one entry the trusted
// proxy itself wrote), skipping trusted addresses, and returns the first that
// is not. Nothing left of that decision point may influence the result: under
// an appending proxy every earlier element is attacker-supplied, and a draft
// that rejected the chain on ANY empty element handed `X-Forwarded-For: ,` as
// a way to collapse every client onto one budget — the exact lockout DoS this
// exists to close. An empty/unparseable hop ends the walk at the peer only
// when it IS the decision point: garbage from a proxy and garbage from a
// client are indistinguishable, and one shared budget is the conservative
// answer there.
func rightmostUntrusted(values []string, trusted []*net.IPNet, peer string) string {
	// Each header instance may itself be a comma-separated list; net/http keeps
	// repeated headers in arrival order, so flattening preserves chain order.
	// Elements are kept VERBATIM, empties included — dropping them would silently
	// renumber the chain and move the decision point.
	var hops []string
	for _, v := range values {
		for _, hop := range strings.Split(v, ",") {
			hops = append(hops, strings.TrimSpace(hop))
			if len(hops) > maxForwardedHops {
				return peer
			}
		}
	}
	for i := len(hops) - 1; i >= 0; i-- {
		if hops[i] == "" {
			return peer
		}
		ip := net.ParseIP(hops[i])
		if ip == nil {
			return peer
		}
		if containsIP(trusted, ip) {
			continue
		}
		// Canonical form, so an attacker cannot mint extra budgets by spelling
		// the same address several ways (::ffff:203.0.113.7 vs 203.0.113.7,
		// upper vs lower case hex).
		return ip.String()
	}
	// Empty header, or a chain consisting only of trusted proxies.
	return peer
}

// peerHost strips the port from RemoteAddr, normalising the address when it
// parses. Returns RemoteAddr verbatim when it is not host:port (httptest and
// unix sockets do this), which keeps it usable as an opaque limiter key.
func peerHost(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.String()
	}
	return host
}

func containsIP(nets []*net.IPNet, ip net.IP) bool {
	for _, n := range nets {
		if n != nil && n.Contains(ip) {
			return true
		}
	}
	return false
}
