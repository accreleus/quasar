package actorsocket

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// The shared control-socket fixtures. The Rust twin runs the same files in
// node-agent/crates/quasar-recovery/tests/socket_fixtures.rs.
var fixtureDir = filepath.Join("..", "..", "..", "testdata", "recovery", "socket")

type fixture struct {
	Shape string          `json:"shape"`
	About string          `json:"about"`
	Body  json.RawMessage `json:"body"`
}

func loadFixtures(t *testing.T) map[string]fixture {
	t.Helper()
	entries, err := os.ReadDir(fixtureDir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]fixture{}
	for _, e := range entries {
		if e.Name() == "README.md" {
			continue
		}
		if !strings.HasSuffix(e.Name(), ".json") {
			t.Fatalf("%s: only .json fixtures and README.md belong in %s", e.Name(), fixtureDir)
		}
		raw, err := os.ReadFile(filepath.Join(fixtureDir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var f fixture
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&f); err != nil {
			t.Fatalf("%s: %v", e.Name(), err)
		}
		out[e.Name()] = f
	}
	if len(out) == 0 {
		t.Fatal("no fixtures: the test must never pass vacuously")
	}
	return out
}

// target is the Go type a shape decodes into.
func target(t *testing.T, shape string) any {
	t.Helper()
	switch shape {
	case "request":
		return &Request{}
	case "accepted":
		return &Accepted{}
	case "rejection":
		return &Rejection{}
	case "status":
		return &Status{}
	case "result":
		return &Result{}
	}
	t.Fatalf("unknown shape %q (both sides must know every shape)", shape)
	return nil
}

func canonical(t *testing.T, raw []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// Every fixture decodes strictly into its Go type and re-encodes to the same
// JSON value: no field the Go type lacks, none it adds, and the same
// omitempty/null choices.
func TestEveryFixtureRoundTripsThroughTheGoTypes(t *testing.T) {
	for name, f := range loadFixtures(t) {
		v := target(t, f.Shape)
		dec := json.NewDecoder(bytes.NewReader(f.Body))
		dec.DisallowUnknownFields()
		if err := dec.Decode(v); err != nil {
			t.Errorf("%s: decode: %v", name, err)
			continue
		}
		out, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if want, got := canonical(t, f.Body), canonical(t, out); !reflect.DeepEqual(want, got) {
			t.Errorf("%s: re-encoded differently\n want %s\n  got %s", name, f.Body, out)
		}
	}
}

// The fixtures cover every shape, every request kind, every state, every
// reason, and both restored outcomes of a failure.
func TestFixturesCoverTheVocabulary(t *testing.T) {
	shapes, kinds, states, reasons := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	restored := map[bool]bool{}
	results := []Result{}
	for _, f := range loadFixtures(t) {
		shapes[f.Shape] = true
		switch f.Shape {
		case "request":
			var r Request
			_ = json.Unmarshal(f.Body, &r)
			kinds[string(r.Kind)] = true
		case "rejection":
			var r Rejection
			_ = json.Unmarshal(f.Body, &r)
			reasons[r.Reason] = true
		case "result":
			var r Result
			_ = json.Unmarshal(f.Body, &r)
			results = append(results, r)
		case "status":
			var s Status
			_ = json.Unmarshal(f.Body, &s)
			if s.Result != nil {
				results = append(results, *s.Result)
			}
		}
	}
	for _, r := range results {
		states[r.State] = true
		if (r.Reason != nil) != (r.State == StateFailed) {
			t.Errorf("result %s: reason must be set exactly when failed", r.State)
		}
		if r.Reason != nil {
			reasons[*r.Reason] = true
			restored[r.Restored] = true
		}
		if (r.FinishedAt != nil) != (r.State == StateSucceeded || r.State == StateFailed) {
			t.Errorf("result %s: finished_at must be set exactly when terminal", r.State)
		}
	}
	want := func(what string, got map[string]bool, all ...string) {
		t.Helper()
		var missing []string
		for _, a := range all {
			if !got[a] {
				missing = append(missing, a)
			}
		}
		sort.Strings(missing)
		if len(missing) > 0 {
			t.Errorf("no fixture covers %s %v", what, missing)
		}
	}
	want("shape", shapes, "request", "accepted", "rejection", "status", "result")
	want("kind", kinds, string(KindReplace), string(KindRestore), string(KindRemove))
	want("state", states, StatePending, StatePulling, StateRecreating, StateVerifying, StateSucceeded, StateFailed)
	want("reason", reasons, KnownReasons...)
	if !restored[true] || !restored[false] {
		t.Errorf("fixtures must cover a failure both restored and not: %v", restored)
	}
}
