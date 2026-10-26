package httpx

import (
	"bytes"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWarnUnlistedProxy(t *testing.T) {
	_, listed, _ := net.ParseCIDR("192.0.2.0/24")
	var logged bytes.Buffer
	h := WarnUnlistedProxy(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}), slog.New(slog.NewTextHandler(&logged, nil)), []*net.IPNet{listed})

	send := func(remote, header string) int {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = remote
		if header != "" {
			req.Header.Set(header, "203.0.113.9")
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr.Code
	}
	warns := func() int { return strings.Count(logged.String(), "forwarded-from-unlisted-peer") }

	send("192.0.2.7:1", "X-Forwarded-For") // listed peer
	send("198.51.100.1:1", "")             // unlisted, no forwarded header
	send("198.51.100.1:1", "X-Real-IP")    // not X-Forwarded-*
	if warns() != 0 {
		t.Fatalf("warned without cause: %q", logged.String())
	}
	for i := 0; i < 50; i++ {
		if code := send("198.51.100.1:1", "X-Forwarded-Proto"); code != http.StatusTeapot {
			t.Fatalf("status = %d: the request must be served unchanged", code)
		}
	}
	if warns() != 1 || !strings.Contains(logged.String(), "peer=198.51.100.1") {
		t.Fatalf("want one WARN naming the peer, got %q", logged.String())
	}
	send("198.51.100.2:1", "X-Forwarded-For")
	if warns() != 2 {
		t.Fatalf("a second peer gets its own WARN: %q", logged.String())
	}

	// IPv6: one WARN per /64, naming the first address, not the /64.
	send("[2001:db8:1:2::5]:1", "X-Forwarded-For")
	send("[2001:db8:1:2:aaaa::9]:1", "X-Forwarded-For")
	if warns() != 3 || !strings.Contains(logged.String(), "peer=2001:db8:1:2::5") || strings.Contains(logged.String(), "/64") {
		t.Fatalf("want one WARN for the /64 naming the first address: %q", logged.String())
	}

	// Header names are matched canonically.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "198.51.100.3:1"
	req.Header.Set("x-forwarded-for", "203.0.113.9")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if warns() != 4 {
		t.Fatalf("a lower-case x-forwarded-for must warn: %q", logged.String())
	}
}
