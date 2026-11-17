package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"

	"gopkg.in/yaml.v3"
)

// singleRowEnvelope pins one documented single-row response against the handler
// that writes it. Both ends are asserted, so a one-sided change fails here: the
// spec reverting to the bare shape, or the handler dropping the wrapper. That is
// exactly the drift TestOpenAPIDrift cannot see — it compares ROUTES, not bodies
// (#1293: GET /v1/hosts/{id} answered {"host": ...} while the spec said bare Host).
type singleRowEnvelope struct {
	route    string // normalized "METHOD /path" (normalizeRoute form: {param} → {})
	file     string // handler source, relative to control-plane/
	function string // the Go function that writes the success body
	key      string // the envelope key the body is wrapped in
	schema   string // the components/schemas name the key holds
}

// Amendment 77 (protocol@316ffa9, accreleus/quasar#1293) states these envelopes.
// Adding a row is a reviewed decision: it makes that route's body shape a gate.
var singleRowEnvelopes = []singleRowEnvelope{
	{"GET /v1/hosts/{}", "internal/crud/handler.go", "handleGetHost", "host", "Host"},
	{"POST /v1/hosts/{}/drain", "internal/session/handler.go", "writeHostLifecycleResult", "host", "Host"},
	{"POST /v1/hosts/{}/uncordon", "internal/session/handler.go", "writeHostLifecycleResult", "host", "Host"},
	{"PATCH /v1/users/{}", "internal/auth/handler.go", "handleUpdateUser", "user", "AdminUser"},
}

// TestSingleRowEnvelopeMatchesHandler fails when a single-row envelope in the
// handler and protocol/openapi.yaml disagree.
func TestSingleRowEnvelopeMatchesHandler(t *testing.T) {
	for _, e := range singleRowEnvelopes {
		t.Run(e.route, func(t *testing.T) {
			op := specOperation(t, e.route)

			// --- the spec end -------------------------------------------------
			var doc struct {
				Responses map[string]struct {
					Content map[string]struct {
						Schema struct {
							Type       string `yaml:"type"`
							Required   []string
							Properties map[string]struct {
								Ref string `yaml:"$ref"`
							}
						}
					}
				}
			}
			if err := op.Decode(&doc); err != nil {
				t.Fatalf("decode operation %s: %v", e.route, err)
			}
			ok, has := doc.Responses["200"]
			if !has {
				t.Fatalf("%s documents no 200 response", e.route)
			}
			schema, has := ok.Content["application/json"]
			if !has {
				t.Fatalf("%s documents no application/json 200 body", e.route)
			}
			if schema.Schema.Type != "object" {
				t.Fatalf("%s: 200 body is not the {%s} envelope (amendment 77): the spec declares "+
					"type %q here, and a bare $ref is the drift this test exists for (#1293)",
					e.route, e.key, schema.Schema.Type)
			}
			if len(schema.Schema.Required) != 1 || schema.Schema.Required[0] != e.key {
				t.Fatalf("%s: 200 body required=%v, want [%s] (amendment 77)",
					e.route, schema.Schema.Required, e.key)
			}
			if len(schema.Schema.Properties) != 1 {
				t.Fatalf("%s: 200 body has %d properties, want only %q (amendment 77)",
					e.route, len(schema.Schema.Properties), e.key)
			}
			want := "#/components/schemas/" + e.schema
			if got := schema.Schema.Properties[e.key].Ref; got != want {
				t.Fatalf("%s: %q holds %q, want %q", e.route, e.key, got, want)
			}

			// --- the handler end ---------------------------------------------
			keys := handlerSuccessBodyKeys(t, e.file, e.function)
			if len(keys) != 1 {
				t.Fatalf("%s %s writes %d success bodies, want 1", e.file, e.function, len(keys))
			}
			if keys[0] != e.key {
				t.Fatalf("%s %s answers %s but %s documents {%s: %s} (amendment 77) — "+
					"change the handler and the spec together, or neither",
					e.file, e.function, describeBody(keys[0]), "protocol/openapi.yaml", e.key, e.schema)
			}
		})
	}
}

func describeBody(key string) string {
	if key == "" {
		return "a bare body"
	}
	return "{" + key + ": ...}"
}

// specOperation finds the documented operation for a normalized route.
func specOperation(t *testing.T, route string) *yaml.Node {
	t.Helper()
	data, err := os.ReadFile(openAPIPath(t))
	if err != nil {
		t.Fatalf("read openapi.yaml: %v (author protocol/openapi.yaml)", err)
	}
	var doc struct {
		Paths map[string]map[string]yaml.Node `yaml:"paths"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse openapi.yaml: %v", err)
	}
	for path, item := range doc.Paths {
		for key, op := range item {
			if normalizeRoute(key, path) == route {
				op := op
				return &op
			}
		}
	}
	t.Fatalf("%s is not documented in protocol/openapi.yaml", route)
	return nil
}

// handlerSuccessBodyKeys returns, for every successful (2xx) JSON body the named
// function writes, the envelope key it wraps the row in — "" for a bare body. It
// reads the source rather than driving the handler because these routes need a
// database, a live host and an admin token; the shape being asserted is a literal
// in the function, so the source is the fact.
func handlerSuccessBodyKeys(t *testing.T, file, function string) []string {
	t.Helper()
	path := filepath.Join(controlPlaneRoot(t), file)
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	var fn *ast.FuncDecl
	for _, decl := range parsed.Decls {
		if d, ok := decl.(*ast.FuncDecl); ok && d.Name.Name == function {
			fn = d
			break
		}
	}
	if fn == nil {
		t.Fatalf("%s does not declare %s", file, function)
	}

	var keys []string
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); !ok || sel.Sel.Name != "WriteJSON" {
			return true
		}
		if len(call.Args) != 3 || !isSuccessStatus(call.Args[1]) {
			return true
		}
		keys = append(keys, envelopeKey(call.Args[2]))
		return true
	})
	if len(keys) == 0 {
		t.Fatalf("%s %s writes no httpx.WriteJSON success body the drift check can read", file, function)
	}
	return keys
}

// isSuccessStatus reports whether e is http.StatusOK / StatusCreated / StatusAccepted.
func isSuccessStatus(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	switch sel.Sel.Name {
	case "StatusOK", "StatusCreated", "StatusAccepted":
		return true
	}
	return false
}

// envelopeKey returns the single key of a `map[string]any{"key": value}` literal,
// or "" when the body is anything else (a bare struct, a variable, an error map).
func envelopeKey(e ast.Expr) string {
	lit, ok := e.(*ast.CompositeLit)
	if !ok {
		return ""
	}
	if mt, ok := lit.Type.(*ast.MapType); !ok {
		return ""
	} else if id, ok := mt.Key.(*ast.Ident); !ok || id.Name != "string" {
		return ""
	}
	if len(lit.Elts) != 1 {
		return "" // a multi-key body is a page or an error, never a single-row envelope
	}
	kv, ok := lit.Elts[0].(*ast.KeyValueExpr)
	if !ok {
		return ""
	}
	basic, ok := kv.Key.(*ast.BasicLit)
	if !ok || basic.Kind != token.STRING {
		return ""
	}
	key, err := strconv.Unquote(basic.Value)
	if err != nil {
		return ""
	}
	return key
}

// controlPlaneRoot locates control-plane/ relative to this test file.
func controlPlaneRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..")
}
