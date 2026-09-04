package platform

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/buildinfo"
)

func TestIdentityServesThePlatformIdentityEnvelope(t *testing.T) {
	rec := httptest.NewRecorder()
	NewHandler().handleIdentity(rec, httptest.NewRequest(http.MethodGet, "/v1/admin/platform/identity", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	// Decoded into a map, not the struct, so a renamed or dropped JSON key
	// fails here instead of round-tripping through its own type.
	var body struct {
		Identity map[string]any `json:"identity"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (body %s)", err, rec.Body.String())
	}
	for _, key := range []string{"version", "source_commit", "built_at", "schema_version"} {
		if _, ok := body.Identity[key]; !ok {
			t.Errorf("identity is missing required key %q: %s", key, rec.Body.String())
		}
	}
	if body.Identity["version"] != buildinfo.UnknownVersion {
		t.Errorf("version = %v, want %q on an unstamped test binary",
			body.Identity["version"], buildinfo.UnknownVersion)
	}
	if body.Identity["source_commit"] != nil || body.Identity["built_at"] != nil {
		t.Errorf("unstamped build must serve null commit/built_at, got %v / %v",
			body.Identity["source_commit"], body.Identity["built_at"])
	}
	// schema_version is ALWAYS known — that is the whole reason it is the
	// ordering key rather than semver.
	if n, ok := body.Identity["schema_version"].(float64); !ok || n <= 0 {
		t.Errorf("schema_version = %v, want a positive integer", body.Identity["schema_version"])
	}
}

// The gate is the middleware, wired at registration — the handler itself never
// checks a role. This records that the route is registered THROUGH the admin
// wrapper, so a future refactor that drops the wrapper fails here.
func TestRegisterWiresIdentityThroughTheAdminMiddleware(t *testing.T) {
	wrapped := false
	admin := func(next http.Handler) http.Handler {
		wrapped = true
		return next
	}
	mux := http.NewServeMux()
	NewHandler().Register(mux, admin)

	if !wrapped {
		t.Fatal("GET /v1/admin/platform/identity was registered without the admin middleware")
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/admin/platform/identity", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("route not reachable: status %d", rec.Code)
	}
}
