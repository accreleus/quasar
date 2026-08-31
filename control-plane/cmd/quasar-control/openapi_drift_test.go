package main

import (
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/access"
	"github.com/accreleus/quasar/control-plane/internal/agentws"
	"github.com/accreleus/quasar/control-plane/internal/artwork"
	"github.com/accreleus/quasar/control-plane/internal/auth"
	"github.com/accreleus/quasar/control-plane/internal/config"
	"github.com/accreleus/quasar/control-plane/internal/console"
	"github.com/accreleus/quasar/control-plane/internal/crud"
	"github.com/accreleus/quasar/control-plane/internal/devices"
	"github.com/accreleus/quasar/control-plane/internal/hostcfg"
	"github.com/accreleus/quasar/control-plane/internal/images"
	"github.com/accreleus/quasar/control-plane/internal/invites"
	"github.com/accreleus/quasar/control-plane/internal/jobs"
	"github.com/accreleus/quasar/control-plane/internal/origins"
	"github.com/accreleus/quasar/control-plane/internal/secrets"
	"github.com/accreleus/quasar/control-plane/internal/session"
	"github.com/accreleus/quasar/control-plane/internal/settings"
	"github.com/accreleus/quasar/control-plane/internal/setup"
	signalpkg "github.com/accreleus/quasar/control-plane/internal/signal"
	"github.com/accreleus/quasar/control-plane/internal/storage"
	"gopkg.in/yaml.v3"
)

// recordingRouter implements httpx.Router by capturing every registered
// "<METHOD> <pattern>" without executing any handler. Go's http.ServeMux does not
// expose its registered patterns, so this is how the drift test learns the real
// route surface. See internal/httpx/router.go.
type recordingRouter struct{ patterns []string }

func (r *recordingRouter) Handle(pattern string, _ http.Handler) {
	r.patterns = append(r.patterns, pattern)
}
func (r *recordingRouter) HandleFunc(pattern string, _ func(http.ResponseWriter, *http.Request)) {
	r.patterns = append(r.patterns, pattern)
}

// nilDepServices builds a Services with nil-dependency handlers (every
// NewHandler is a pure field-assignment; Register only takes method values and
// never dereferences deps), so RegisterRoutes can be run for its route surface
// alone with no database and no live agent.
func nilDepServices(t *testing.T) *Services {
	t.Helper()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return &Services{
		cfg:             &config.Config{}, // empty WebRoot → SPA catch-all not registered
		log:             log,
		authHandler:     auth.NewHandler(nil),
		crudHandler:     crud.NewHandler(nil),
		sessionHandler:  session.NewHandler(nil, nil),
		deviceHandler:   devices.NewHandler(nil, nil),
		signalHandler:   signalpkg.NewHandler(nil, nil, nil, log, origins.NewResolver("", false, nil, log)),
		agentHandler:    agentws.NewHandler(nil, "", log, nil, nil, nil, nil, nil),
		storageHandler:  storage.NewHandler(nil),
		cfgHandler:      hostcfg.NewHandler(nil, nil, nil),
		settingsHandler: settings.NewHandler(nil),
		invitesHandler:  invites.NewHandler(nil, ""),
		consoleHandler:  console.NewHandler(nil, nil),
		// nil service/store: Register only needs the handler to exist. Every
		// request path checks for it and answers 503, so no route can 500 here.
		artworkHandler: artwork.NewHandler(nil, log),
		secretsHandler: secrets.NewHandler(nil, log),
		imagesHandler:  images.NewHandler(nil),
		// setup.Service.Register only takes method values (handleStatus /
		// handleClaim / handleComplete) — deps are never dereferenced at
		// registration — so nil claimer/state and an empty token are fine for the
		// route recorder.
		setupHandler: setup.NewService(nil, nil, "", log),
		// nil manager/resolver: Register only takes method values. Every request
		// path checks for the manager and answers 503 ("TLS is not enabled on this
		// control plane"), so no route can 500 here.
		accessHandler: access.NewService(nil, nil, log),
		// jobs.Handler.Register only takes method values; nil store/registry/
		// dispatcher/auditor are never dereferenced at registration.
		jobsHandler: jobs.NewHandler(nil, nil, nil, log, nil),
	}
}

// recordRoutes runs the real RegisterRoutes against a recorder and returns every
// raw pattern it registered. Shared by the OpenAPI drift test (which filters to
// /v1) and the PROF-01 debug-surface test (which asserts the absence of /debug).
func recordRoutes(t *testing.T) []string {
	t.Helper()
	rec := &recordingRouter{}
	nilDepServices(t).RegisterRoutes(rec)
	return rec.patterns
}

// registeredV1Routes narrows recordRoutes to the normalized "METHOD /path" keys
// of the /v1 surface the running server actually serves.
func registeredV1Routes(t *testing.T) map[string]struct{} {
	t.Helper()
	out := make(map[string]struct{})
	for _, p := range recordRoutes(t) {
		method, path, ok := splitMethodPath(p)
		if !ok || !strings.HasPrefix(path, "/v1/") {
			continue // /health, "/", and any non-method pattern are out of scope
		}
		out[normalizeRoute(method, path)] = struct{}{}
	}
	return out
}

var pathParam = regexp.MustCompile(`\{[^}]*\}`)

// normalizeRoute produces a comparison key immune to path-parameter naming
// differences ({id} vs {sessionId}) and query strings: uppercase method + path
// with every {param} collapsed to {}.
func normalizeRoute(method, path string) string {
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	path = pathParam.ReplaceAllString(path, "{}")
	return strings.ToUpper(method) + " " + path
}

// splitMethodPath parses a Go 1.22 ServeMux pattern "METHOD /path". Patterns
// without a leading method (host-only or bare paths) return ok=false.
func splitMethodPath(pattern string) (method, path string, ok bool) {
	fields := strings.Fields(pattern)
	if len(fields) != 2 {
		return "", "", false
	}
	m := fields[0]
	switch m {
	case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS":
		return m, fields[1], true
	}
	return "", "", false
}

// specV1Routes parses protocol/openapi.yaml and returns the normalized set of
// "METHOD /path" keys it documents under the /v1 surface.
func specV1Routes(t *testing.T) map[string]struct{} {
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
	out := make(map[string]struct{})
	httpMethods := map[string]struct{}{
		"get": {}, "post": {}, "put": {}, "patch": {}, "delete": {}, "head": {}, "options": {},
	}
	for path, item := range doc.Paths {
		if !strings.HasPrefix(path, "/v1/") {
			continue
		}
		for key, op := range item {
			if _, isMethod := httpMethods[strings.ToLower(key)]; !isMethod {
				continue // skip parameters, summary, x-* keys
			}
			// An operation marked `x-dev-only: true` is registered by a flag-gated
			// path outside Services.RegisterRoutes (today: POST /v1/dev/agent-session,
			// #399, wired from main.go only when QUASAR_DEV_AGENT_AUTH=1). It is
			// documented so tooling can generate a client, but it is BY DESIGN absent
			// from the recorded production route table — so drift-checking it would
			// permanently report a phantom. Its registration semantics (absent when
			// the flag is off, present when on) are asserted by dedicated tests
			// instead: TestDevAgentSessionRouteAbsentFromRegisterRoutes here, and
			// TestRegister* in internal/devauth.
			//
			// Matched on the extension, never on the path string: the next dev-only
			// endpoint must not require editing this test.
			if devOnlyOperation(t, op) {
				continue
			}
			// An operation marked `x-unimplemented: true` documents an agreed shape
			// the server does NOT serve yet (today: GET /v1/sessions/{id}/events —
			// the session-events SSE amendment whose implementation was parked and
			// reverted, its contract text deliberately kept for a future
			// resurrection). Without this skip the drift check reports it as a
			// phantom and fails every branch that pins protocol main. The marker is
			// removed in the same change that registers the route, at which point
			// drift-checking resumes automatically.
			//
			// The skip is NOT open-ended: only routes in allowedUnimplemented may
			// carry the marker. Anything else marked x-unimplemented FAILS the test
			// — otherwise any future operation could opt out of the bidirectional
			// route check merely by carrying the extension.
			if unimplementedOperation(t, op) {
				route := normalizeRoute(key, path)
				if _, ok := allowedUnimplemented[route]; !ok {
					t.Errorf("operation %s carries x-unimplemented but is NOT in allowedUnimplemented — "+
						"either implement and register the route, or get the exception reviewed and added to the allowlist", route)
				}
				continue
			}
			out[normalizeRoute(key, path)] = struct{}{}
		}
	}
	return out
}

// openAPIPath locates protocol/openapi.yaml relative to this test file
// (control-plane/cmd/quasar-control → ../../../protocol/openapi.yaml).
// devOnlyOperation reports whether an OpenAPI operation carries `x-dev-only: true`.
func devOnlyOperation(t *testing.T, op yaml.Node) bool {
	t.Helper()
	var ext struct {
		DevOnly bool `yaml:"x-dev-only"`
	}
	if err := op.Decode(&ext); err != nil {
		// A non-mapping operation node is not something this test can classify;
		// treat it as normal surface so the drift check still fires.
		return false
	}
	return ext.DevOnly
}

// allowedUnimplemented is the explicit, reviewed allowlist of operations
// permitted to carry `x-unimplemented: true` (normalizeRoute key form). Adding
// an entry here is a REVIEWED decision, not a convenience: every entry is a
// hole in the bidirectional drift gate. Remove the entry in the same change
// that registers the route.
var allowedUnimplemented = map[string]struct{}{
	"GET /v1/sessions/{}/events": {}, // parked session-events SSE amendment
}

// unimplementedOperation reports whether an OpenAPI operation carries
// `x-unimplemented: true` — a documented shape the server does not yet serve.
func unimplementedOperation(t *testing.T, op yaml.Node) bool {
	t.Helper()
	var ext struct {
		Unimplemented bool `yaml:"x-unimplemented"`
	}
	if err := op.Decode(&ext); err != nil {
		// A non-mapping operation node is not something this test can classify;
		// treat it as normal surface so the drift check still fires.
		return false
	}
	return ext.Unimplemented
}

// TestDevAgentSessionRouteAbsentFromRegisterRoutes pins the #399 invariant: the
// dev-only endpoint is NEVER part of the production route table, under any
// configuration. RegisterRoutes takes no flag — if this route ever appears here,
// the gate has been moved to a runtime check (a 403 guard), which the spec
// explicitly rejects.
func TestDevAgentSessionRouteAbsentFromRegisterRoutes(t *testing.T) {
	for _, p := range recordRoutes(t) {
		if strings.Contains(p, "/v1/dev/") {
			t.Fatalf("dev-only route %q must never be registered by Services.RegisterRoutes "+
				"(it is wired from main.go behind QUASAR_DEV_AGENT_AUTH=1)", p)
		}
	}
}

func openAPIPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "protocol", "openapi.yaml")
}

// TestOpenAPIDrift is the load-bearing anti-drift guard: the OpenAPI schema must
// document exactly the /v1 routes the server registers — no more, no less. When it
// fails, the message lists both drift directions so the fix is obvious.
func TestOpenAPIDrift(t *testing.T) {
	registered := registeredV1Routes(t)
	documented := specV1Routes(t)

	var missingFromSpec, extraInSpec []string
	for r := range registered {
		if _, ok := documented[r]; !ok {
			missingFromSpec = append(missingFromSpec, r)
		}
	}
	for r := range documented {
		if _, ok := registered[r]; !ok {
			extraInSpec = append(extraInSpec, r)
		}
	}
	sort.Strings(missingFromSpec)
	sort.Strings(extraInSpec)

	if len(missingFromSpec) > 0 {
		t.Errorf("%d route(s) registered by the server but MISSING from protocol/openapi.yaml:\n  %s",
			len(missingFromSpec), strings.Join(missingFromSpec, "\n  "))
	}
	if len(extraInSpec) > 0 {
		t.Errorf("%d route(s) in protocol/openapi.yaml but NOT registered by the server (phantom/renamed):\n  %s",
			len(extraInSpec), strings.Join(extraInSpec, "\n  "))
	}
}

// TestDumpRegisteredRoutes is an authoring aid: `go test -run TestDumpRegisteredRoutes -v`
// prints the authoritative registered /v1 surface to transcribe into openapi.yaml.
func TestDumpRegisteredRoutes(t *testing.T) {
	routes := registeredV1Routes(t)
	list := make([]string, 0, len(routes))
	for r := range routes {
		list = append(list, r)
	}
	sort.Strings(list)
	t.Logf("registered /v1 routes (%d):\n%s", len(list), strings.Join(list, "\n"))
}
