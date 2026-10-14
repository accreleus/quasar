package devauth

import (
	"bytes"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// unreadBody fails the test if the handler touches the request body.
type unreadBody struct{ t *testing.T }

func (b unreadBody) Read([]byte) (int, error) {
	b.t.Error("a refused request must not read the body")
	return 0, http.ErrBodyReadAfterClose
}

// A request on the plaintext listener is refused before the key is compared:
// no mint, no body read, no log line, the same answer for a right and a wrong key.
// The spoofed X-Forwarded-Proto is what used to carry it past the redirect (#594).
func TestPlainHTTPRequestIsRefusedBeforeTheKeyIsCompared(t *testing.T) {
	var logged bytes.Buffer
	m := okMinter()
	svc := NewService(m, testSecret, slog.New(slog.NewTextHandler(&logged, nil)))

	plain := func(key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/dev/agent-session", unreadBody{t})
		req.Header.Set("X-Forwarded-Proto", "https")
		req.Header.Set(KeyHeader, key)
		w := httptest.NewRecorder()
		svc.Handle(w, req)
		return w
	}

	right, wrong := plain(testSecret), plain("not-the-key")
	if right.Code != http.StatusUnauthorized {
		t.Fatalf("plaintext request with the right key: status = %d, want 401 (%s)", right.Code, right.Body)
	}
	if right.Body.String() != wrong.Body.String() {
		t.Fatalf("refusal depends on the key:\n right=%s\n wrong=%s", right.Body, wrong.Body)
	}
	if !strings.Contains(right.Body.String(), "QUASAR_TRUSTED_PROXIES") || !strings.Contains(right.Body.String(), "HTTPS") {
		t.Fatalf("the refusal must say what to do; got %s", right.Body)
	}
	if m.calls != 0 {
		t.Fatalf("a plaintext request reached the minter; calls=%d", m.calls)
	}
	if logged.Len() != 0 {
		t.Fatalf("a refused request must not log; got %s", logged.String())
	}
}

// Which transports the route serves. The gate itself is tabled in
// internal/httpx; this pins the route's own choices: loopback is accepted, a
// published port's bridge-gateway peer is not, and the knob lifts the gate.
func TestWhichTransportsTheRouteServes(t *testing.T) {
	_, edge, _ := net.ParseCIDR("172.18.0.0/16")
	cases := []struct {
		name         string
		peer         string
		proto        string
		insecureHTTP bool
		want         int
	}{
		{name: "loopback: a caller inside the container", peer: "127.0.0.1:40000", want: http.StatusOK},
		{name: "bridge gateway: the machine's own tooling through a published port", peer: "172.17.0.1:40000", want: http.StatusUnauthorized},
		{name: "bridge gateway, stack declares its HTTP port loopback-only", peer: "172.17.0.1:40000", insecureHTTP: true, want: http.StatusOK},
		{name: "another machine", peer: "203.0.113.7:40000", want: http.StatusUnauthorized},
		{name: "trusted proxy forwards https", peer: "172.18.0.5:40000", proto: "https", want: http.StatusOK},
		{name: "trusted proxy forwards http", peer: "172.18.0.5:40000", proto: "http", want: http.StatusUnauthorized},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := okMinter()
			svc := NewService(m, testSecret, testLogger()).WithTransport([]*net.IPNet{edge}, c.insecureHTTP)
			req := httptest.NewRequest(http.MethodPost, "/v1/dev/agent-session", nil)
			req.RemoteAddr = c.peer
			req.Header.Set(KeyHeader, testSecret)
			if c.proto != "" {
				req.Header.Set("X-Forwarded-Proto", c.proto)
			}
			w := httptest.NewRecorder()
			svc.Handle(w, req)
			if w.Code != c.want {
				t.Fatalf("status = %d, want %d (%s)", w.Code, c.want, w.Body)
			}
			if minted := c.want == http.StatusOK; (m.calls == 1) != minted {
				t.Fatalf("minter calls = %d with status %d", m.calls, w.Code)
			}
		})
	}
}

func TestInsecureHTTPNeedsExactlyOne(t *testing.T) {
	for value, want := range map[string]bool{"1": true, "": false, "true": false, "0": false} {
		t.Setenv(EnvInsecureHTTP, value)
		if got := LoadConfig().InsecureHTTP; got != want {
			t.Fatalf("%s=%q: InsecureHTTP = %v, want %v", EnvInsecureHTTP, value, got, want)
		}
	}
}
