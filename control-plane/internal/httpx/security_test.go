package httpx

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSecurityHeaders(t *testing.T) {
	rr := httptest.NewRecorder()
	SecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))

	for _, name := range []string{"Content-Security-Policy", "X-Content-Type-Options", "X-Frame-Options", "Referrer-Policy", "Permissions-Policy"} {
		if rr.Header().Get(name) == "" {
			t.Fatalf("missing %s", name)
		}
	}
	if csp := rr.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "font-src 'self' data:") {
		t.Fatalf("CSP must allow the SPA's bundled data fonts: %q", csp)
	}
	directives := map[string]string{}
	for _, d := range strings.Split(rr.Header().Get("Content-Security-Policy"), ";") {
		name, value, _ := strings.Cut(strings.TrimSpace(d), " ")
		directives[name] = value
	}
	// #1147: a WebTransport session dials the agent's https: origin. Nothing else moves.
	for name, want := range map[string]string{
		"connect-src":     "'self' ws: wss: https:",
		"default-src":     "'self'",
		"frame-ancestors": "'none'",
		"object-src":      "'none'",
		"base-uri":        "'none'",
	} {
		if got := directives[name]; got != want {
			t.Errorf("CSP %s = %q, want %q", name, got, want)
		}
	}
	if got := directives["script-src"]; strings.Contains(got, "https:") || strings.Contains(got, "'unsafe-inline'") || !strings.HasPrefix(got, "'self' 'sha256-") {
		t.Errorf("CSP script-src must stay 'self' plus hashes: %q", got)
	}
	// Microphone-capture amendment (2026-08-02): gUM needs this origin allowed,
	// but never a third-party one.
	if pp := rr.Header().Get("Permissions-Policy"); !strings.Contains(pp, "microphone=(self)") {
		t.Fatalf("Permissions-Policy must allow microphone=(self): %q", pp)
	}
}
