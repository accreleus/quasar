package httpx_test

import (
	"bytes"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/httpx"
)

func logTo(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// logInt pulls a numeric slog attribute out of a rendered text-handler line.
func logInt(t *testing.T, line, key string) int {
	t.Helper()
	_, rest, found := strings.Cut(line, key+"=")
	if !found {
		t.Fatalf("log line has no %s: %s", key, line)
	}
	val, _, _ := strings.Cut(strings.TrimSpace(rest), " ")
	n, err := strconv.Atoi(val)
	if err != nil {
		t.Fatalf("%s=%q is not an integer: %v", key, val, err)
	}
	return n
}

func TestRequestLogRecordsStatusAndBytes(t *testing.T) {
	var buf bytes.Buffer
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("hello world"))
	})

	r := httptest.NewRequest(http.MethodPost, "/v1/apps", nil)
	r.RemoteAddr = "192.0.2.56:55038"
	httpx.RequestLog(h, logTo(&buf), httpx.AccessLogAll, nil).ServeHTTP(httptest.NewRecorder(), r)

	line := buf.String()
	for _, want := range []string{
		`method=POST`,
		`path=/v1/apps`,
		`status=201`,
		`bytes_out=11`,
		`remote=192.0.2.56`,
		`dur_ms=`,
	} {
		if !strings.Contains(line, want) {
			t.Errorf("log line missing %q\ngot: %s", want, line)
		}
	}
}

// An implicit 200 (handler writes without calling WriteHeader) must not be
// logged as 0.
func TestRequestLogImplicitOK(t *testing.T) {
	var buf bytes.Buffer
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	httpx.RequestLog(h, logTo(&buf), httpx.AccessLogAll, nil).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if !strings.Contains(buf.String(), "status=200") {
		t.Errorf("want status=200, got: %s", buf.String())
	}
}

// The compose healthcheck hits /health every few seconds forever; logging it
// buries every other line.
func TestRequestLogSkipsHealth(t *testing.T) {
	var buf bytes.Buffer
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })
	httpx.RequestLog(h, logTo(&buf), httpx.AccessLogAll, nil).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/health", nil))
	if buf.Len() != 0 {
		t.Errorf("/health must not be logged, got: %s", buf.String())
	}
}

// The access log now uses the SAME trusted-proxy policy as the rate limiters
// (#438) instead of blindly taking the left-most X-Forwarded-For entry. That
// entry is attacker-supplied, so a "diagnostic, never an access control"
// justification still bought an operator a log full of addresses of the
// attacker's choosing. With proxies configured, the resolved address is the one
// the proxy actually saw.
func TestRequestLogUsesTrustedProxyPolicy(t *testing.T) {
	_, proxyNet, err := net.ParseCIDR("172.18.0.0/16")
	if err != nil {
		t.Fatal(err)
	}
	newReq := func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
		r.RemoteAddr = "172.18.0.5:4000"
		// "1.2.3.4" is what the client injected; the proxy appended the peer.
		r.Header.Set("X-Forwarded-For", "1.2.3.4, 203.0.113.9")
		return r
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })

	var trusted bytes.Buffer
	httpx.RequestLog(h, logTo(&trusted), httpx.AccessLogAll, []*net.IPNet{proxyNet}).
		ServeHTTP(httptest.NewRecorder(), newReq())
	if !strings.Contains(trusted.String(), "remote=203.0.113.9") {
		t.Errorf("want the right-most untrusted XFF entry, got: %s", trusted.String())
	}

	var unconfigured bytes.Buffer
	httpx.RequestLog(h, logTo(&unconfigured), httpx.AccessLogAll, nil).
		ServeHTTP(httptest.NewRecorder(), newReq())
	if !strings.Contains(unconfigured.String(), "remote=172.18.0.5") {
		t.Errorf("with no trusted proxies the header must be ignored, got: %s", unconfigured.String())
	}
}

// bytes_out must be the COMPRESSED size, which is what the client actually
// waits for. This pins the middleware order: RequestLog outside Compress.
func TestRequestLogReportsCompressedBytes(t *testing.T) {
	var buf bytes.Buffer
	payload := strings.Repeat("export const a = 1;\n", 5000) // ~100 KB, highly compressible
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		_, _ = w.Write([]byte(payload))
	})

	r := httptest.NewRequest(http.MethodGet, "/assets/index.js", nil)
	r.Header.Set("Accept-Encoding", "gzip")
	httpx.RequestLog(httpx.Compress(h), logTo(&buf), httpx.AccessLogAll, nil).ServeHTTP(httptest.NewRecorder(), r)

	line := buf.String()
	if !strings.Contains(line, "enc=gzip") {
		t.Errorf("want enc=gzip, got: %s", line)
	}
	// The plain body is ~100 KB; the logged size must be a small fraction of it.
	n := logInt(t, line, "bytes_out")
	if n == 0 || n > 20_000 {
		t.Errorf("bytes_out = %d, want a compressed size well under 20000 (plain body is ~100 KB)", n)
	}
}

// Compress must still be able to hijack through the log wrapper, otherwise the
// agent WS and signaling relay break.
func TestRequestLogForwardsHijack(t *testing.T) {
	var buf bytes.Buffer
	var hijacked bool
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("lost http.Hijacker through RequestLog")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		hijacked = true
		_ = conn.Close()
	})

	r := httptest.NewRequest(http.MethodGet, "/agent/ws", nil)
	r.Header.Set("Upgrade", "websocket")
	r.Header.Set("Connection", "Upgrade")
	rec := &hijackRecorder{ResponseRecorder: httptest.NewRecorder()}
	httpx.RequestLog(httpx.Compress(h), logTo(&buf), httpx.AccessLogAll, nil).ServeHTTP(rec, r)

	if !hijacked {
		t.Fatal("handler never hijacked")
	}
	if !strings.Contains(buf.String(), "status=101") {
		t.Errorf("a hijacked upgrade should log 101, got: %s", buf.String())
	}
}

// #517: AccessLogOff must suppress the routine line entirely.
func TestRequestLogOffSuppressesRoutineLine(t *testing.T) {
	var buf bytes.Buffer
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	httpx.RequestLog(h, logTo(&buf), httpx.AccessLogOff, nil).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/apps", nil))
	if buf.Len() != 0 {
		t.Errorf("AccessLogOff must not log a 200, got: %s", buf.String())
	}
}

// #517: AccessLogOff must also suppress a 5xx — "off" means off, full stop.
// AccessLogErrors is the level that keeps errors visible; that split is what
// makes "off" safe to recommend without losing the #386 forensic value.
func TestRequestLogOffSuppressesErrorsToo(t *testing.T) {
	var buf bytes.Buffer
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
	httpx.RequestLog(h, logTo(&buf), httpx.AccessLogOff, nil).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/apps", nil))
	if buf.Len() != 0 {
		t.Errorf("AccessLogOff must not log a 500 either, got: %s", buf.String())
	}
}

// #517: AccessLogErrors is the default-shaped level — it must stay silent on
// routine 2xx traffic that would otherwise dominate docker logs.
func TestRequestLogErrorsLevelSuppressesRoutineLine(t *testing.T) {
	var buf bytes.Buffer
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	httpx.RequestLog(h, logTo(&buf), httpx.AccessLogErrors, nil).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/apps", nil))
	if buf.Len() != 0 {
		t.Errorf("AccessLogErrors must not log a 200, got: %s", buf.String())
	}
}

// #517: AccessLogErrors must still surface a 5xx — the forensic signal #386
// added the log for must survive turning the routine noise off.
func TestRequestLogErrorsLevelKeeps5xx(t *testing.T) {
	var buf bytes.Buffer
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadGateway) })
	httpx.RequestLog(h, logTo(&buf), httpx.AccessLogErrors, nil).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/apps", nil))
	if !strings.Contains(buf.String(), "status=502") {
		t.Errorf("AccessLogErrors must log a 502, got: %s", buf.String())
	}
}

// #517: AccessLogErrors must not log a 4xx — those are routine (bad client
// requests, auth failures), not server-side incidents.
func TestRequestLogErrorsLevelSuppresses4xx(t *testing.T) {
	var buf bytes.Buffer
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
	httpx.RequestLog(h, logTo(&buf), httpx.AccessLogErrors, nil).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/apps", nil))
	if buf.Len() != 0 {
		t.Errorf("AccessLogErrors must not log a 404, got: %s", buf.String())
	}
}
