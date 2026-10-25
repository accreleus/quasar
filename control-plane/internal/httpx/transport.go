package httpx

import (
	"net"
	"net/http"
	"strings"

	"github.com/accreleus/quasar/control-plane/internal/ratelimit"
)

const insecureTransportMessage = "refused: this request reached the control plane over plain HTTP. " +
	"Use its HTTPS port, or name your reverse proxy in QUASAR_TRUSTED_PROXIES and have it send X-Forwarded-Proto: https. " +
	"A token or key sent this way crossed the network in clear and stays valid until a restart: restart the control plane to rotate it."

// RefuseInsecure answers 401 and reports true unless the request's credential
// stayed off the network in clear (#593). Routes whose credential is a per-boot
// secret call it first: before the secret is compared, the body read, or
// anything logged or counted. Both listeners share one router and
// RedirectToHTTPS is not access control, so the route has to decide for itself.
//
// loopbackOK also accepts a loopback peer. Only for a route production never
// serves: behind a same-host proxy every client is a loopback peer.
func RefuseInsecure(w http.ResponseWriter, r *http.Request, trusted []*net.IPNet, loopbackOK bool) bool {
	if SecureTransport(r, trusted, loopbackOK) {
		return false
	}
	WriteError(w, http.StatusUnauthorized, CodeUnauthorized, insecureTransportMessage)
	return true
}

// SecureTransport is the predicate behind RefuseInsecure, for a route that only
// warns.
func SecureTransport(r *http.Request, trusted []*net.IPNet, loopbackOK bool) bool {
	peer, isProxy := ratelimit.DirectPeer(r, trusted)
	if isProxy {
		// What a trusted proxy says about the client's hop outranks our own
		// transport: TLS to us is no proof it did not serve the client in clear.
		if protos := r.Header.Values("X-Forwarded-Proto"); len(protos) > 0 {
			return allHTTPS(protos)
		}
	}
	if r.TLS != nil {
		return true
	}
	return loopbackOK && peer != nil && peer.IsLoopback()
}

// Every value on every header line must be https: a proxy that appends leaves
// the client's own value first, so no single position can be believed.
func allHTTPS(headerLines []string) bool {
	for _, line := range headerLines {
		for _, proto := range strings.Split(line, ",") {
			if !strings.EqualFold(strings.TrimSpace(proto), "https") {
				return false
			}
		}
	}
	return true
}
