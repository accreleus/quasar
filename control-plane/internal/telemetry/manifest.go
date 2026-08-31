package telemetry

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
)

// The metric manifest — the one dictionary of every metric key on either
// telemetry wire, with the four things a number needs before it can be read:
// unit, clock, window, estimator (plus the key carrying its sample count).
//
// The SOURCE of truth is `docs/session-trace/metrics.json`, beside
// `thresholds.json`, for the same reason: a spec a human edits, next to the
// prose that explains it. Go's embed cannot reach outside the module, so
// `make docs-metrics-sync` copies it here and `TestMetricsManifestIsInSync`
// fails if this copy goes stale. Same pattern, same guarantee as the golden
// threshold file.
//
// Everything derived from it is derived MECHANICALLY, at init, from this one
// parse: the browser ingest allow-list below, `taxonomyV1` in
// internal/session/classifier.go, and its `rvfcQualifiedRawKeys`. Adding a key
// to the manifest is how a key is added; there is no second list to remember.

//go:embed metrics.json
var manifestJSON []byte

// MetricEntry is one row of the manifest. The JSON tags ARE the schema; see the
// `_readme` block in metrics.json for the vocabulary of each field.
type MetricEntry struct {
	Key      string `json:"key"`
	Source   string `json:"source"`
	Taxonomy string `json:"taxonomy"` // "" when the key is outside the diagnostic lens
	Unit     string `json:"unit"`
	Clock    string `json:"clock"`
	Window   string `json:"window"`
	// Estimator is part of the CLAIM, not decoration: "present sigma was 19 ms"
	// means different things as a mean, a p95 and a max.
	Estimator string `json:"estimator"`
	// NKey is the key carrying this one's sample count, or "".
	NKey string `json:"n_key"`
	// DeprecatedFor names the replacement key, or "". A deprecated key is still
	// posted and still stored — deprecation here is about what to READ.
	DeprecatedFor string `json:"deprecated_for"`
	Since         string `json:"since"`
	Why           string `json:"why"`

	// Stored is false ONLY for a key that travels on the wire and reaches no
	// stored sample. Absent in JSON means true, which is why it is a *bool.
	Stored *bool `json:"stored"`
	// RVFCQualified marks a value that exists only while the client's RVFC
	// captureTime is available.
	RVFCQualified bool `json:"rvfc_qualified"`
}

// IsStored reports whether a sample of this key ever reaches storage.
func (e MetricEntry) IsStored() bool { return e.Stored == nil || *e.Stored }

// MetricManifest is the parsed manifest file.
type MetricManifest struct {
	Version string        `json:"version"`
	Metrics []MetricEntry `json:"metrics"`

	byKey map[[2]string]MetricEntry
}

var manifest = mustParseManifest(manifestJSON)

func mustParseManifest(data []byte) *MetricManifest {
	m, err := ParseMetricManifest(data)
	if err != nil {
		// The manifest is embedded at build time and validated by a unit test;
		// a failure here is a broken binary, not a runtime condition.
		panic("telemetry: embedded metrics manifest is invalid: " + err.Error())
	}
	return m
}

// Known vocabularies. A value outside them is a typo, and a typo in a units
// column is worse than a missing column — it reads as authoritative.
var (
	metricSources    = map[string]bool{"agent": true, "browser": true, "native": true, "bench": true}
	metricUnits      = map[string]bool{"fps": true, "ms": true, "kbps": true, "count": true, "bool": true, "fraction": true, "px": true, "string": true}
	metricClocks     = map[string]bool{"host_monotonic": true, "host_wall": true, "gst_pts": true, "rtp": true, "client_performance": true, "client_wall": true, "none": true}
	metricWindows    = map[string]bool{"heartbeat(~5s)": true, "1s": true, "rolling_600": true, "cumulative": true, "snapshot": true, "event": true}
	metricEstimators = map[string]bool{"mean": true, "median": true, "p50": true, "p95": true, "p10": true, "max": true, "min": true, "sum": true, "count": true, "delta": true, "last": true, "raw": true}
)

// ParseMetricManifest parses and validates a manifest document. Exported so the
// drift test can parse the docs/ original with the same code that parses the
// embedded copy — a validator only one of two copies passes is not a validator.
func ParseMetricManifest(data []byte) (*MetricManifest, error) {
	var m MetricManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if m.Version == "" {
		return nil, fmt.Errorf("manifest has no version")
	}
	m.byKey = make(map[[2]string]MetricEntry, len(m.Metrics))
	taxonomies := map[string]string{}
	for _, e := range m.Metrics {
		id := [2]string{e.Source, e.Key}
		if _, dup := m.byKey[id]; dup {
			return nil, fmt.Errorf("duplicate (source,key): %s/%s", e.Source, e.Key)
		}
		if !metricSources[e.Source] {
			return nil, fmt.Errorf("%s/%s: unknown source %q", e.Source, e.Key, e.Source)
		}
		if !metricUnits[e.Unit] {
			return nil, fmt.Errorf("%s/%s: unknown unit %q", e.Source, e.Key, e.Unit)
		}
		if !metricClocks[e.Clock] {
			return nil, fmt.Errorf("%s/%s: unknown clock %q", e.Source, e.Key, e.Clock)
		}
		if !metricWindows[e.Window] {
			return nil, fmt.Errorf("%s/%s: unknown window %q", e.Source, e.Key, e.Window)
		}
		if !metricEstimators[e.Estimator] {
			return nil, fmt.Errorf("%s/%s: unknown estimator %q", e.Source, e.Key, e.Estimator)
		}
		if e.Why == "" {
			return nil, fmt.Errorf("%s/%s: empty why", e.Source, e.Key)
		}
		if e.Taxonomy != "" {
			if prev, dup := taxonomies[e.Taxonomy]; dup {
				return nil, fmt.Errorf("duplicate taxonomy name %q (%s and %s/%s)", e.Taxonomy, prev, e.Source, e.Key)
			}
			taxonomies[e.Taxonomy] = e.Source + "/" + e.Key
		}
		m.byKey[id] = e
	}
	// Every n_key and deprecated_for must name a key that exists for the same
	// source. A dangling pointer in a dictionary is how a dictionary rots.
	for _, e := range m.Metrics {
		for label, ref := range map[string]string{"n_key": e.NKey, "deprecated_for": e.DeprecatedFor} {
			if ref == "" {
				continue
			}
			if _, ok := m.byKey[[2]string{e.Source, ref}]; !ok {
				return nil, fmt.Errorf("%s/%s: %s references unknown key %q", e.Source, e.Key, label, ref)
			}
		}
	}
	return &m, nil
}

// Manifest returns the embedded manifest.
func Manifest() *MetricManifest { return manifest }

// Lookup returns the entry for a (source, key) pair.
func (m *MetricManifest) Lookup(source, key string) (MetricEntry, bool) {
	e, ok := m.byKey[[2]string{source, key}]
	return e, ok
}

// TaxonomyEntries returns every entry that carries a taxonomy name, sorted by
// that name so the derived table is stable across runs.
func (m *MetricManifest) TaxonomyEntries() []MetricEntry {
	out := make([]MetricEntry, 0, len(m.Metrics))
	for _, e := range m.Metrics {
		if e.Taxonomy != "" {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Taxonomy < out[j].Taxonomy })
	return out
}

// BrowserAllowList is the set of raw metric keys the browser/native ingest path
// keeps. A key is on it when the manifest says a client reports it AND that a
// stored sample can contain it — the two facts that used to live only in
// filter.go's hand-maintained map.
func (m *MetricManifest) BrowserAllowList() map[string]bool {
	out := make(map[string]bool, len(m.Metrics))
	for _, e := range m.Metrics {
		if (e.Source == "browser" || e.Source == "native") && e.IsStored() {
			out[e.Key] = true
		}
	}
	return out
}

// RVFCQualifiedKeys is the set of raw client keys whose value exists only while
// RVFC captureTime is available.
func (m *MetricManifest) RVFCQualifiedKeys() map[string]bool {
	out := map[string]bool{}
	for _, e := range m.Metrics {
		if e.RVFCQualified {
			out[e.Key] = true
		}
	}
	return out
}
