package httpx

import (
	"bufio"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/ratelimit"
)

// AccessLogLevel gates RequestLog independently of LOG_LEVEL, which also
// silences lifecycle lines an operator wants (#517). A single active session
// produced 60-120 access lines/min.
type AccessLogLevel string

const (
	// Zero-overhead: RequestLog returns next unwrapped, doing no per-request work.
	AccessLogOff AccessLogLevel = "off"
	// 5xx and hijacked upgrades only.
	AccessLogErrors AccessLogLevel = "errors"
	AccessLogAll    AccessLogLevel = "all"
)

// Place it OUTSIDE Compress, or bytes_out reports the uncompressed size.
// /health is dropped: the compose healthcheck polls it forever.
func RequestLog(next http.Handler, log *slog.Logger, level AccessLogLevel, trustedProxies []*net.IPNet) http.Handler {
	if level == AccessLogOff {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}

		start := time.Now()
		lw := &logWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(lw, r)

		// A hijacked connection has no meaningful status: the handler owns the
		// socket. Log the upgrade, not a fabricated 200.
		status := lw.status
		if lw.hijacked {
			status = http.StatusSwitchingProtocols
		}

		if level == AccessLogErrors && status < http.StatusInternalServerError {
			return
		}

		log.Info("http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", status,
			"dur_ms", time.Since(start).Milliseconds(),
			"bytes_out", lw.bytes,
			"enc", w.Header().Get("Content-Encoding"),
			"remote", ratelimit.ClientIP(r, trustedProxies),
		)
	})
}

type logWriter struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
	hijacked    bool
}

func (l *logWriter) WriteHeader(status int) {
	if l.wroteHeader {
		return
	}
	l.status = status
	l.wroteHeader = true
	l.ResponseWriter.WriteHeader(status)
}

func (l *logWriter) Write(p []byte) (int, error) {
	if !l.wroteHeader {
		// Mirror net/http: a bare Write implies 200.
		l.wroteHeader = true
	}
	n, err := l.ResponseWriter.Write(p)
	l.bytes += int64(n)
	return n, err
}

func (l *logWriter) Flush() {
	if f, ok := l.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Without this the agent WS and the signaling relay fail to upgrade.
func (l *logWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := l.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("httpx: underlying ResponseWriter is not an http.Hijacker")
	}
	l.hijacked = true
	return hj.Hijack()
}

func (l *logWriter) Unwrap() http.ResponseWriter { return l.ResponseWriter }
