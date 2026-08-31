package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func redirectTestHandler() (http.Handler, *int) {
	served := 0
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served++
		w.WriteHeader(http.StatusOK)
	})
	return RedirectToHTTPS(next, "18443"), &served
}

func TestRedirectBrowserRoutes(t *testing.T) {
	h, served := redirectTestHandler()

	cases := []struct {
		method, path string
		wantLocation string
	}{
		{"GET", "/", "https://host-a:18443/"},
		{"GET", "/app/library", "https://host-a:18443/app/library"},
		{"POST", "/v1/auth/login", "https://host-a:18443/v1/auth/login"},
		{"GET", "/v1/sessions?limit=5", "https://host-a:18443/v1/sessions?limit=5"},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, c.path, nil)
		req.Host = "host-a:18080"
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusPermanentRedirect {
			t.Errorf("%s %s: code = %d, want 308", c.method, c.path, rr.Code)
		}
		if loc := rr.Header().Get("Location"); loc != c.wantLocation {
			t.Errorf("%s %s: Location = %q, want %q", c.method, c.path, loc, c.wantLocation)
		}
	}
	if *served != 0 {
		t.Errorf("next handler served %d browser requests, want 0", *served)
	}
}

func TestRedirectDefaultHTTPSPortOmitted(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h := RedirectToHTTPS(next, "443")
	req := httptest.NewRequest("GET", "/app", nil)
	req.Host = "play.example.com:8080"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if loc := rr.Header().Get("Location"); loc != "https://play.example.com/app" {
		t.Errorf("Location = %q, want no :443 suffix", loc)
	}
}

func TestAgentRoutesServedPlainHTTP(t *testing.T) {
	h, served := redirectTestHandler()

	for _, path := range []string{"/health", "/agent/ws", "/v1/agent/storage/gc-pending"} {
		req := httptest.NewRequest("GET", path, nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Errorf("GET %s: code = %d, want 200 (agent-facing must not redirect)", path, rr.Code)
		}
	}
	if *served != 3 {
		t.Errorf("next handler served %d, want 3", *served)
	}
}

func TestWebSocketUpgradeGets426(t *testing.T) {
	h, served := redirectTestHandler()

	req := httptest.NewRequest("GET", "/v1/signal", nil)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "keep-alive, Upgrade")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUpgradeRequired {
		t.Errorf("code = %d, want 426", rr.Code)
	}
	if *served != 0 {
		t.Errorf("next handler served a plain-HTTP WS upgrade")
	}
}

func TestAgentWSUpgradeStillServed(t *testing.T) {
	// The agent's own WS handshake must not be caught by the 426 branch.
	h, served := redirectTestHandler()
	req := httptest.NewRequest("GET", "/agent/ws", nil)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if *served != 1 {
		t.Errorf("agent WS handshake was not passed through (code %d)", rr.Code)
	}
}

func TestForwardedProtoHTTPSServesNormally(t *testing.T) {
	// Behind the hardened Caddy overlay the proxy speaks plain HTTP to us but
	// the client leg is TLS; redirecting would loop forever.
	h, served := redirectTestHandler()
	req := httptest.NewRequest("GET", "/app/library", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || *served != 1 {
		t.Errorf("X-Forwarded-Proto: https request redirected (code %d)", rr.Code)
	}
}
