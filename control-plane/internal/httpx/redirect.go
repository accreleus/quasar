package httpx

import (
	"net"
	"net/http"
	"strings"
)

// RedirectToHTTPS pushes browsers off the plaintext listener: a plain-HTTP page
// is not a secure context, so Keyboard Lock and Gamepad silently fail.
// Agent-facing routes must keep working over HTTP (/agent/ws enrollment,
// /v1/agent/*, and the compose healthcheck on /health).
//
// port must be the EXTERNAL https port for the Location header, not the
// in-container one, since a host may publish it remapped.
//
// X-Forwarded-Proto: https is served normally — redirecting a request that
// already crossed a TLS-terminating proxy loops. The header is spoofable, but
// this is a UX aid, not access control: both listeners serve the same handler.
//
// Browsers never follow a redirect on a WebSocket upgrade, so those get 426.
func RedirectToHTTPS(next http.Handler, port string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if agentFacing(r.URL.Path) || r.Header.Get("X-Forwarded-Proto") == "https" {
			next.ServeHTTP(w, r)
			return
		}

		if isWebSocketUpgrade(r) {
			w.Header().Set("Connection", "close")
			http.Error(w, "plain-HTTP WebSocket not served here: reconnect with wss:// on the HTTPS port "+port, http.StatusUpgradeRequired)
			return
		}

		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		target := "https://" + host
		if port != "443" {
			target += ":" + port
		}
		target += r.URL.RequestURI()
		// 308, not 301: it preserves method and body, so a POST from a stale
		// http:// bookmark still works.
		http.Redirect(w, r, target, http.StatusPermanentRedirect)
	})
}

// The node-agent / infrastructure surface that must stay reachable over HTTP.
func agentFacing(path string) bool {
	return path == "/health" ||
		path == "/agent/ws" ||
		strings.HasPrefix(path, "/v1/agent/")
}

func isWebSocketUpgrade(r *http.Request) bool {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return false
	}
	for _, part := range strings.Split(r.Header.Get("Connection"), ",") {
		if strings.EqualFold(strings.TrimSpace(part), "upgrade") {
			return true
		}
	}
	return false
}
