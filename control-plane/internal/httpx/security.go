package httpx

import "net/http"

// SecurityHeaders applies browser hardening to every API and SPA response.
// TLS/HSTS is terminated by the hardened reverse-proxy deployment profile.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		// The two script-src sha256 hashes pin web/index.html's anti-FOUC inline
		// scripts and MUST be recomputed when either script's text changes, or the
		// browser CSP-blocks it silently. TestSecurityHeaders checks only that the
		// directive is present, never that the hashes are current.
		//
		// connect-src allows https: because a WebTransport session dials the agent, a
		// different origin (ADR 0010): its own listener or the admin's public URL, either
		// of which can change after the page loaded (#1147). WebTransport is governed by
		// connect-src, so without https: Chrome refuses every such session. An injected
		// script gains no capability it lacked: ws:/wss: already reached any host. And
		// script-src stays pinned to 'self' and two hashes, so injection itself is the bar.
		h.Set("Content-Security-Policy", "default-src 'self'; base-uri 'none'; frame-ancestors 'none'; object-src 'none'; img-src 'self' data: blob:; font-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self' 'sha256-JyTIELzOXtUuYYDzvKo5xumSZoFM9u+g1oWa3B+7nGo=' 'sha256-EcGgvKpbPwFbv8Ad/fJ8ZDUJx//FEfHU+grt4Hmvxjs='; connect-src 'self' ws: wss: https:; media-src 'self' blob:")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		// microphone=(self), not (): an empty allowlist makes every getUserMedia
		// audio call reject NotAllowedError regardless of user consent.
		h.Set("Permissions-Policy", "camera=(), geolocation=(), microphone=(self), payment=(), usb=()")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}
