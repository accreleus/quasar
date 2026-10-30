package auth

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The password routes are refused over plain HTTP (#604). The handlers are
// driven with a body that fails to decode, so no database is needed: a served
// request answers 400 (or 401 with no user, for the password change), a refused
// one 403 before the body is read.
func TestCredentialRoutesRefusePlainHTTP(t *testing.T) {
	_, proxy, _ := net.ParseCIDR("192.0.2.0/24")
	routes := []struct {
		name   string
		handle func(*Handler) http.HandlerFunc
		served int
	}{
		{"POST /v1/auth/login", func(h *Handler) http.HandlerFunc { return h.handleLogin }, http.StatusBadRequest},
		{"POST /v1/auth/register", func(h *Handler) http.HandlerFunc { return h.handleRegister }, http.StatusBadRequest},
		// No user in the context: served means it reached the auth check.
		{"POST /v1/me/password", func(h *Handler) http.HandlerFunc { return h.handleChangePassword }, http.StatusUnauthorized},
	}
	cases := []struct {
		name    string
		peer    string
		trusted []*net.IPNet
		proto   []string
		tls     bool
		refused bool
	}{
		{"plain http", "192.0.2.7:4000", nil, nil, false, true},
		{"loopback peer", "127.0.0.1:4000", nil, nil, false, true},
		{"untrusted peer claims https", "192.0.2.7:4000", nil, []string{"https"}, false, true},
		{"trusted proxy reports http", "192.0.2.7:4000", []*net.IPNet{proxy}, []string{"http"}, false, true},
		{"trusted proxy appends to a client's http", "192.0.2.7:4000", []*net.IPNet{proxy}, []string{"http, https"}, false, true},
		{"trusted proxy sends no header", "192.0.2.7:4000", []*net.IPNet{proxy}, nil, false, true},
		{"trusted proxy reports https", "192.0.2.7:4000", []*net.IPNet{proxy}, []string{"https"}, false, false},
		{"https listener", "192.0.2.7:4000", nil, nil, true, false},
		{"https listener, untrusted peer claims https", "192.0.2.7:4000", nil, []string{"https"}, true, false},
	}
	for _, rt := range routes {
		for _, c := range cases {
			t.Run(rt.name+"/"+c.name, func(t *testing.T) {
				var logged bytes.Buffer
				h := NewHandler(nil).WithTrustedProxies(c.trusted).
					WithLogger(slog.New(slog.NewTextHandler(&logged, nil)))
				req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("not json"))
				req.RemoteAddr = c.peer
				for _, p := range c.proto {
					req.Header.Add("X-Forwarded-Proto", p)
				}
				if c.tls {
					req.TLS = &tls.ConnectionState{}
				}
				rr := httptest.NewRecorder()
				rt.handle(h)(rr, req)

				if !c.refused {
					if rr.Code != rt.served || logged.Len() != 0 {
						t.Fatalf("status = %d (want %d), log %q: a secure request must be served", rr.Code, rt.served, logged.String())
					}
					return
				}
				var body struct {
					Error struct{ Code, Message string }
				}
				if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
					t.Fatalf("body: %v: %q", err, rr.Body.String())
				}
				if rr.Code != http.StatusForbidden || body.Error.Code != "forbidden" || !strings.Contains(body.Error.Message, "HTTPS") {
					t.Fatalf("got %d %q %q, want 403 forbidden naming HTTPS", rr.Code, body.Error.Code, body.Error.Message)
				}
				if !strings.Contains(logged.String(), "credential-route-plaintext") ||
					!strings.Contains(logged.String(), "route=\""+rt.name+"\"") ||
					!strings.Contains(logged.String(), "remote_addr="+strings.Split(c.peer, ":")[0]) {
					t.Fatalf("WARN must carry the token, route and source: %q", logged.String())
				}
			})
		}
	}
}

// A flood from one source logs a bounded number of lines, and every request is
// still refused.
func TestCredentialRouteRefusalLogIsBoundedPerSource(t *testing.T) {
	var logged bytes.Buffer
	h := NewHandler(nil).WithLogger(slog.New(slog.NewTextHandler(&logged, nil)))
	for i := 0; i < 100; i++ {
		rr := httptest.NewRecorder()
		h.handleLogin(rr, httptest.NewRequest(http.MethodPost, "/", strings.NewReader("x")))
		if rr.Code != http.StatusForbidden {
			t.Fatalf("request %d: status %d, want 403", i, rr.Code)
		}
	}
	if got := strings.Count(logged.String(), "credential-route-plaintext"); got != 10 {
		t.Fatalf("WARN lines = %d under a flood, want the bound 10", got)
	}
}
