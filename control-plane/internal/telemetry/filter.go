package telemetry

import "encoding/json"

// The browser/native ingest allow-list is DERIVED from the metric manifest
// (manifest.go) — it is every key the manifest says a client reports and that a
// stored sample may contain. It used to be a hand-maintained map beside a
// hand-maintained taxonomy beside a hand-maintained openapi schema, which is how
// `input_*` came to be posted every second by every client and dropped here
// without a word: nothing listed the two sets side by side.
//
// To allow a new client key: add it to docs/session-trace/metrics.json with
// source `browser` and `make docs-metrics-sync`. There is no second list.
var browserMetricKeys = Manifest().BrowserAllowList()

func FilterBrowserMetrics(raw json.RawMessage) json.RawMessage {
	out := json.RawMessage("{}")
	if len(raw) == 0 {
		return out
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return out
	}
	kept := make(map[string]json.RawMessage, len(m))
	for k, v := range m {
		if browserMetricKeys[k] {
			kept[k] = v
		}
	}
	b, err := json.Marshal(kept)
	if err != nil {
		return out
	}
	return b
}
