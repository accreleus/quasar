package actorsocket

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
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

// The fixtures cover every shape, request kind and state, every rejection
// reason with a rejection and every failure reason with a failed result (never
// the other way round), and both restored outcomes of a failure.
func TestFixturesCoverTheVocabulary(t *testing.T) {
	shapes, kinds, states := map[string]bool{}, map[string]bool{}, map[string]bool{}
	rejections, failures := map[string]bool{}, map[string]bool{}
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
			rejections[string(r.Reason)] = true
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
		states[string(r.State)] = true
		if (r.Reason != nil) != (r.State == StateFailed) {
			t.Errorf("result %s: reason must be set exactly when failed", r.State)
		}
		if r.Reason != nil {
			failures[string(*r.Reason)] = true
			restored[r.Restored] = true
		}
		if (r.FinishedAt != nil) != (r.State == StateSucceeded || r.State == StateFailed) {
			t.Errorf("result %s: finished_at must be set exactly when terminal", r.State)
		}
	}
	exactly := func(what string, got map[string]bool, all ...string) {
		t.Helper()
		want := map[string]bool{}
		for _, a := range all {
			want[a] = true
		}
		if !reflect.DeepEqual(got, want) {
			keys := func(m map[string]bool) []string {
				out := []string{}
				for k := range m {
					out = append(out, k)
				}
				sort.Strings(out)
				return out
			}
			t.Errorf("%s: fixtures cover %v, want exactly %v", what, keys(got), keys(want))
		}
	}
	strs := func(rs []Reason) []string {
		out := []string{}
		for _, r := range rs {
			out = append(out, string(r))
		}
		return out
	}
	exactly("shapes", shapes, "request", "accepted", "rejection", "status", "result")
	exactly("request kinds", kinds, string(KindReplace), string(KindRestore), string(KindRemove))
	exactly("states", states, string(StatePending), string(StatePulling), string(StateRecreating),
		string(StateVerifying), string(StateSucceeded), string(StateFailed))
	exactly("rejection reasons", rejections, strs(RejectionReasons)...)
	exactly("failure reasons", failures, strs(FailureReasons)...)
	if !restored[true] || !restored[false] {
		t.Errorf("fixtures must cover a failure both restored and not: %v", restored)
	}
}

// reasonBlock returns the string values of the const block in file that
// declares ident, so a reason added beside it is seen here.
func reasonBlock(t *testing.T, file, ident string) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		var values []string
		declares := false
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, name := range vs.Names {
				if name.Name == ident {
					declares = true
				}
				if i < len(vs.Values) {
					if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						v, _ := strconv.Unquote(lit.Value)
						values = append(values, v)
					}
				}
			}
		}
		if declares {
			return values
		}
	}
	t.Fatalf("%s declares no %s", file, ident)
	return nil
}

// The actor's reasons must include every reason the Go updater emits and every
// failure reason the platform package records, apart from the four the
// platform observes about an updater rather than receives from one. Guards
// drift until the ActorClient adapter unifies the vocabularies.
func TestReasonsIncludeTheUpdaterAndPlatformVocabularies(t *testing.T) {
	known := map[string]bool{}
	for _, r := range KnownReasons {
		known[string(r)] = true
	}
	observedByOthers := map[string]bool{"updater_absent": true, "updater_unreachable": true, "timeout": true, "unsupported": true}

	updater := append(reasonBlock(t, "../updater/plan.go", "ReasonInvalid"),
		reasonBlock(t, "../updater/signature.go", "ReasonSignatureMissing")...)
	for _, r := range updater {
		if !known[r] {
			t.Errorf("updater reason %q is missing from actorsocket.KnownReasons", r)
		}
	}
	platform := reasonBlock(t, "../platform/apply.go", "ReasonUpdaterAbsentFailure")
	inPlatform := map[string]bool{}
	for _, r := range platform {
		inPlatform[r] = true
		if !known[r] && !observedByOthers[r] {
			t.Errorf("platform reason %q is missing from actorsocket.KnownReasons", r)
		}
	}
	for r := range observedByOthers {
		if !inPlatform[r] || known[r] {
			t.Errorf("%q must be a platform reason the actor never emits", r)
		}
	}
	if len(updater) < 10 || len(platform) < 14 {
		t.Fatalf("parsed %d updater and %d platform reasons: the const blocks moved", len(updater), len(platform))
	}
}
