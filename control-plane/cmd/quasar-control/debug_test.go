package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestDebugSurfaceIsNotOnTheApplicationMux is the load-bearing PROF-01
// assertion: the debug listener must be a separate server, so NOTHING with a
// /debug prefix may ever appear on the application router. If someone
// "simplifies" this later by moving pprof onto the main mux, the loopback
// guarantee silently becomes an internet-reachable profiler.
func TestDebugSurfaceIsNotOnTheApplicationMux(t *testing.T) {
	for _, pattern := range recordRoutes(t) {
		_, path, ok := splitMethodPath(pattern)
		if !ok {
			path = pattern
		}
		if strings.HasPrefix(path, "/debug") {
			t.Errorf("RegisterRoutes registered %q on the application mux — "+
				"the debug surface belongs on the separate loopback listener (newDebugServer)", pattern)
		}
	}
}

// TestApplicationMuxDoesNotServePprof is the same guarantee checked through a
// real ServeMux rather than the recorder, so a catch-all or a middleware could
// not smuggle the surface back in.
func TestApplicationMuxDoesNotServePprof(t *testing.T) {
	s := nilDepServices(t)
	mux := http.NewServeMux()
	s.RegisterRoutes(mux)

	for _, path := range []string{"/debug/pprof/", "/debug/pprof/heap", "/debug/quasar/pool"} {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusNotFound {
			t.Errorf("main mux GET %s = %d, want 404", path, rr.Code)
		}
	}
}

// TestNewDebugServerIsADistinctServer asserts the wiring shape: its own
// *http.Server, its own handler, and — critically — no WriteTimeout, because the
// 10s WriteTimeout the application servers use would truncate every
// /debug/pprof/profile?seconds=30 and every execution trace.
func TestNewDebugServerIsADistinctServer(t *testing.T) {
	srv := newDebugServer("127.0.0.1:0", nil, discardLogger())
	if srv == nil {
		t.Fatal("newDebugServer returned nil")
	}
	if srv.Addr != "127.0.0.1:0" {
		t.Errorf("Addr = %q, want 127.0.0.1:0", srv.Addr)
	}
	if srv.Handler == nil {
		t.Fatal("debug server has no handler of its own")
	}
	if srv.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %v, want 0 — a non-zero write timeout truncates "+
			"/debug/pprof/profile?seconds=N, which is the reason this listener exists", srv.WriteTimeout)
	}
	if srv.ReadHeaderTimeout == 0 {
		t.Error("ReadHeaderTimeout = 0: a stuck client could pin a connection forever")
	}

	// Two calls must not share a handler — i.e. this is not the DefaultServeMux
	// that net/http/pprof's init() decorates.
	if srv.Handler == http.DefaultServeMux {
		t.Fatal("debug server serves http.DefaultServeMux; it must own a private mux")
	}
}

func TestDebugServerSurface(t *testing.T) {
	srv := newDebugServer("127.0.0.1:0", nil, discardLogger())
	ts := httptest.NewServer(srv.Handler)
	t.Cleanup(ts.Close)

	get := func(t *testing.T, path string) (int, string) {
		t.Helper()
		resp, err := ts.Client().Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	post := func(t *testing.T, path string) (int, string) {
		t.Helper()
		resp, err := ts.Client().Post(ts.URL+path, "", nil)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	t.Run("pprof index", func(t *testing.T) {
		code, body := get(t, "/debug/pprof/")
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200", code)
		}
		if !strings.Contains(body, "goroutine") || !strings.Contains(body, "heap") {
			t.Errorf("index does not list the standard profiles:\n%s", body)
		}
	})

	t.Run("named profile", func(t *testing.T) {
		// ?debug=1 keeps it textual and cheap; the point is that pprof.Index
		// dispatches the named profiles under the same prefix.
		code, body := get(t, "/debug/pprof/goroutine?debug=1")
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200", code)
		}
		if !strings.Contains(body, "goroutine profile") {
			t.Errorf("unexpected body:\n%s", body)
		}
	})

	t.Run("pool stats degrade to 503 without a pool", func(t *testing.T) {
		// Never a panic: the drift/unit path constructs the debug server with a
		// nil pool, and so does a control plane whose database wiring failed.
		code, _ := get(t, "/debug/quasar/pool")
		if code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", code)
		}
	})

	t.Run("runtime stats", func(t *testing.T) {
		code, body := get(t, "/debug/quasar/runtime")
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", code, body)
		}
		var rs runtimeStats
		if err := json.Unmarshal([]byte(body), &rs); err != nil {
			t.Fatalf("decode: %v (%s)", err, body)
		}
		if rs.Goroutines <= 0 {
			t.Errorf("goroutines = %d, want > 0", rs.Goroutines)
		}
		if rs.GOMAXPROCS <= 0 {
			t.Errorf("gomaxprocs = %d, want > 0", rs.GOMAXPROCS)
		}
	})

	t.Run("mutex profiling arms and disarms", func(t *testing.T) {
		t.Cleanup(func() { runtime.SetMutexProfileFraction(0) })

		code, body := post(t, "/debug/quasar/mutexprofile?fraction=5")
		if code != http.StatusOK {
			t.Fatalf("arm: status = %d, want 200: %s", code, body)
		}
		if got := runtime.SetMutexProfileFraction(-1); got != 5 {
			t.Fatalf("runtime fraction = %d, want 5", got)
		}

		code, body = post(t, "/debug/quasar/mutexprofile?fraction=0")
		if code != http.StatusOK {
			t.Fatalf("disarm: status = %d, want 200: %s", code, body)
		}
		var got map[string]int
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatalf("decode: %v (%s)", err, body)
		}
		if got["previous"] != 5 {
			t.Errorf("previous = %d, want 5", got["previous"])
		}
		if now := runtime.SetMutexProfileFraction(-1); now != 0 {
			t.Errorf("runtime fraction = %d after disarm, want 0", now)
		}
	})

	t.Run("block profiling arms and disarms", func(t *testing.T) {
		t.Cleanup(func() { runtime.SetBlockProfileRate(0) })
		if code, body := post(t, "/debug/quasar/blockprofile?rate=1000"); code != http.StatusOK {
			t.Fatalf("arm: status = %d, want 200: %s", code, body)
		}
		if code, body := post(t, "/debug/quasar/blockprofile?rate=0"); code != http.StatusOK {
			t.Fatalf("disarm: status = %d, want 200: %s", code, body)
		}
	})

	t.Run("toggles reject bad input", func(t *testing.T) {
		cases := []string{
			"/debug/quasar/mutexprofile",           // missing fraction
			"/debug/quasar/mutexprofile?fraction=", // blank fraction
			"/debug/quasar/mutexprofile?fraction=x",
			// -1 means "read without changing" to the runtime, which would make
			// a typo look like a successful arm.
			"/debug/quasar/mutexprofile?fraction=-1",
			"/debug/quasar/blockprofile?rate=-1",
			"/debug/quasar/blockprofile",
		}
		for _, path := range cases {
			if code, _ := post(t, path); code != http.StatusBadRequest {
				t.Errorf("POST %s = %d, want 400", path, code)
			}
		}
		if got := runtime.SetMutexProfileFraction(-1); got != 0 {
			t.Errorf("a rejected request changed the mutex fraction to %d", got)
			runtime.SetMutexProfileFraction(0)
		}
	})

	t.Run("toggles are POST-only", func(t *testing.T) {
		for _, path := range []string{"/debug/quasar/mutexprofile?fraction=1", "/debug/quasar/blockprofile?rate=1"} {
			if code, _ := get(t, path); code != http.StatusMethodNotAllowed {
				t.Errorf("GET %s = %d, want 405", path, code)
			}
		}
		if got := runtime.SetMutexProfileFraction(-1); got != 0 {
			t.Errorf("a GET armed mutex profiling (fraction=%d)", got)
			runtime.SetMutexProfileFraction(0)
		}
	})
}

// TestNoComposeFilePublishesTheDebugPort enforces the other half of the loopback
// guarantee. The bind address keeps the listener on 127.0.0.1 inside the network
// namespace; this keeps a `ports:` entry from forwarding the host straight past
// it. Access is `docker exec`, always.
func TestNoComposeFilePublishesTheDebugPort(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(repoRoot(t), "deploy", "docker-compose*.yml"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no compose files found — this guard would silently pass forever")
	}

	var seen int
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		var doc any
		if err := yaml.Unmarshal(data, &doc); err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		for _, mapping := range publishedPorts(doc) {
			seen++
			if strings.Contains(mapping, "6060") {
				t.Errorf("%s publishes %q — the debug listener must never be reachable "+
					"off-container; use `docker exec` instead", filepath.Base(f), mapping)
			}
		}
	}
	// Without this the guard would pass just as happily if publishedPorts stopped
	// finding anything at all — the compose files do publish 8080/8443/5432.
	if seen == 0 {
		t.Fatal("found no published ports in any compose file: the walker is broken, not the deployment clean")
	}
	t.Logf("checked %d published port mappings across %d compose files", seen, len(files))
}

// publishedPorts walks an arbitrary decoded compose document and returns every
// entry of every `ports:` list, in whatever form it was written (short string
// syntax or the long mapping form).
func publishedPorts(node any) []string {
	var out []string
	switch v := node.(type) {
	case map[string]any:
		for k, val := range v {
			if k == "ports" {
				if list, ok := val.([]any); ok {
					for _, item := range list {
						switch e := item.(type) {
						case string:
							out = append(out, e)
						case int:
							out = append(out, strings.TrimSpace(yamlScalar(e)))
						case map[string]any:
							// long form: {target: 6060, published: 6060, ...}
							for _, field := range []string{"published", "target"} {
								if p, ok := e[field]; ok {
									out = append(out, yamlScalar(p))
								}
							}
						}
					}
					continue
				}
			}
			out = append(out, publishedPorts(val)...)
		}
	case []any:
		for _, item := range v {
			out = append(out, publishedPorts(item)...)
		}
	}
	return out
}

func yamlScalar(v any) string {
	b, err := yaml.Marshal(v)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// repoRoot resolves the repository root from this test file
// (control-plane/cmd/quasar-control → ../../..).
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
}
