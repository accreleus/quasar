package telemetry

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// docsManifestPath is docs/session-trace/metrics.json — the SOURCE. The copy
// beside manifest.go exists only because go:embed cannot reach outside the
// module.
func docsManifestPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "..",
		"docs", "session-trace", "metrics.json")
}

// The embedded copy must be byte-identical to the docs original. If this fails
// the fix is `make docs-metrics-sync`, never editing the copy.
func TestMetricsManifestIsInSync(t *testing.T) {
	want, err := os.ReadFile(docsManifestPath(t))
	if err != nil {
		t.Fatalf("read docs/session-trace/metrics.json: %v", err)
	}
	if !bytes.Equal(want, manifestJSON) {
		t.Fatalf("control-plane/internal/telemetry/metrics.json is stale.\n"+
			"docs source is %d bytes, embedded copy is %d bytes.\n"+
			"Run: make docs-metrics-sync", len(want), len(manifestJSON))
	}
}

// The docs original must pass the SAME validator the embedded copy passes —
// otherwise the sync target could promote a manifest that only fails later.
func TestMetricsManifestDocsSourceParses(t *testing.T) {
	data, err := os.ReadFile(docsManifestPath(t))
	if err != nil {
		t.Fatalf("read docs manifest: %v", err)
	}
	if _, err := ParseMetricManifest(data); err != nil {
		t.Fatalf("docs/session-trace/metrics.json is invalid: %v", err)
	}
}

func TestMetricsManifestParsesAndIsWellFormed(t *testing.T) {
	m := Manifest()
	if len(m.Metrics) == 0 {
		t.Fatal("manifest is empty")
	}
	// Duplicate (source,key), duplicate taxonomy names, unknown vocabulary
	// values and dangling n_key/deprecated_for references are all rejected by
	// ParseMetricManifest, which ran at init. Assert the invariants that are
	// cheap to restate and expensive to lose.
	seen := map[string]bool{}
	for _, e := range m.Metrics {
		if e.Taxonomy == "" {
			continue
		}
		if seen[e.Taxonomy] {
			t.Errorf("duplicate taxonomy name %q", e.Taxonomy)
		}
		seen[e.Taxonomy] = true
	}
}

// The manifest is the field dictionary; a key that is not stored must never
// reach the ingest allow-list, and every key that IS on the allow-list must be
// a client key.
func TestBrowserAllowListIsDerivedFromManifest(t *testing.T) {
	allow := Manifest().BrowserAllowList()
	if len(allow) == 0 {
		t.Fatal("empty browser allow-list")
	}
	for _, e := range Manifest().Metrics {
		on := allow[e.Key]
		clientKey := e.Source == "browser" || e.Source == "native"
		switch {
		case clientKey && e.IsStored() && !on:
			t.Errorf("%s is a stored client key but not allow-listed", e.Key)
		case clientKey && !e.IsStored() && on && !storedTwin(e.Key):
			t.Errorf("%s is marked stored:false but is allow-listed", e.Key)
		}
	}
	// The live contract, restated: FilterBrowserMetrics keeps what the manifest
	// says it keeps and nothing else.
	got := FilterBrowserMetrics([]byte(`{"fps":60,"input_msg_per_sec":120,"nonsense":1}`))
	if string(got) != `{"fps":60}` {
		t.Fatalf("FilterBrowserMetrics = %s, want {\"fps\":60}", got)
	}
}

// A key can exist for two sources (frames_dropped does). storedTwin says whether
// some OTHER source's entry for the same key is stored, which legitimately puts
// the bare key on a key-only allow-list.
func storedTwin(key string) bool {
	for _, e := range Manifest().Metrics {
		if e.Key == key && e.IsStored() && (e.Source == "browser" || e.Source == "native") {
			return true
		}
	}
	return false
}
