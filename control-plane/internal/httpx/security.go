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
		h.Set("Content-Security-Policy", "default-src 'self'; base-uri 'none'; frame-ancestors 'none'; object-src 'none'; img-src 'self' data: blob:; font-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self' 'sha256-JyTIELzOXtUuYYDzvKo5xumSZoFM9u+g1oWa3B+7nGo=' 'sha256-EcGgvKpbPwFbv8Ad/fJ8ZDUJx//FEfHU+grt4Hmvxjs='; connect-src 'self' ws: wss:; media-src 'self' blob:")
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
