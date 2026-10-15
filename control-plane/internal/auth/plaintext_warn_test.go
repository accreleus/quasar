package auth

import (
	"bytes"
	"crypto/tls"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The password routes are served over plain HTTP in this release but warn once
// per source (#604). The handlers are driven with a body that fails to decode,
// so no database is needed and the response must not depend on the transport.
func TestCredentialRoutesWarnOverPlainHTTP(t *testing.T) {
	_, proxy, _ := net.ParseCIDR("192.0.2.0/24")
	routes := []struct {
		name   string
		handle func(*Handler) http.HandlerFunc
	}{
		{"POST /v1/auth/login", func(h *Handler) http.HandlerFunc { return h.handleLogin }},
		{"POST /v1/auth/register", func(h *Handler) http.HandlerFunc { return h.handleRegister }},
		{"POST /v1/me/password", func(h *Handler) http.HandlerFunc { return h.handleChangePassword }},
	}
	cases := []struct {
		name    string
		trusted []*net.IPNet
		proto   []string
		tls     bool
		warns   int
	}{
		{"plain http", nil, nil, false, 1},
		{"untrusted peer claims https", nil, []string{"https"}, false, 1},
		{"trusted proxy reports http", []*net.IPNet{proxy}, []string{"http"}, false, 1},
		{"trusted proxy reports https", []*net.IPNet{proxy}, []string{"https"}, false, 0},
		{"https listener", nil, nil, true, 0},
	}
	for _, rt := range routes {
		for _, c := range cases {
			t.Run(rt.name+"/"+c.name, func(t *testing.T) {
				var logged bytes.Buffer
				h := NewHandler(nil).WithTrustedProxies(c.trusted).
					WithLogger(slog.New(slog.NewTextHandler(&logged, nil)))
				req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("not json"))
				req.RemoteAddr = "192.0.2.7:4000"
				for _, p := range c.proto {
					req.Header.Add("X-Forwarded-Proto", p)
				}
				if c.tls {
					req.TLS = &tls.ConnectionState{}
				}
				rr := httptest.NewRecorder()
				rt.handle(h)(rr, req)

				if got := strings.Count(logged.String(), "credential-route-plaintext"); got != c.warns {
					t.Fatalf("WARN lines = %d, want %d: %q", got, c.warns, logged.String())
				}
				if c.warns == 1 && (!strings.Contains(logged.String(), "route=\""+rt.name+"\"") || !strings.Contains(logged.String(), "remote_addr=192.0.2.7")) {
					t.Fatalf("WARN must carry the route and source: %q", logged.String())
				}
				want := http.StatusBadRequest
				if rt.name == "POST /v1/me/password" {
					want = http.StatusUnauthorized // no user in the context; before the body is read
				}
				if rr.Code != want {
					t.Fatalf("status = %d, want %d: the response must not depend on the transport", rr.Code, want)
				}
			})
		}
	}
}

// A flood from one source logs a bounded number of lines.
func TestCredentialRouteWarnIsBoundedPerSource(t *testing.T) {
	var logged bytes.Buffer
	h := NewHandler(nil).WithLogger(slog.New(slog.NewTextHandler(&logged, nil)))
	for i := 0; i < 100; i++ {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("x"))
		h.warnPlaintext(req, "POST /v1/auth/login")
	}
	if got := strings.Count(logged.String(), "credential-route-plaintext"); got != 10 {
		t.Fatalf("WARN lines = %d under a flood, want the bound 10", got)
	}
}
