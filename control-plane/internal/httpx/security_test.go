package httpx

import (
	"crypto/tls"
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
	// Microphone-capture amendment (2026-08-02): gUM needs this origin allowed,
	// but never a third-party one.
	if pp := rr.Header().Get("Permissions-Policy"); !strings.Contains(pp, "microphone=(self)") {
		t.Fatalf("Permissions-Policy must allow microphone=(self): %q", pp)
	}
}

// HSTS rides the HTTPS listener only (#820): r.TLS is set there and nowhere else.
func TestSecurityHeadersHSTSOnlyOnTLS(t *testing.T) {
	h := SecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))

	plain := httptest.NewRecorder()
	h.ServeHTTP(plain, httptest.NewRequest(http.MethodGet, "/", nil))
	if got := plain.Header().Get("Strict-Transport-Security"); got != "" {
		t.Fatalf("plain HTTP listener sent HSTS: %q", got)
	}

	secure := httptest.NewRequest(http.MethodGet, "/", nil)
	secure.TLS = &tls.ConnectionState{}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, secure)
	if got := rr.Header().Get("Strict-Transport-Security"); got != "max-age=31536000" {
		t.Fatalf("HTTPS listener HSTS = %q, want max-age=31536000", got)
	}
}
