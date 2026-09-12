package platform

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/buildinfo"
)

func TestIdentityServesThePlatformIdentityEnvelope(t *testing.T) {
	rec := httptest.NewRecorder()
	NewHandler(nil, nil).handleIdentity(rec, httptest.NewRequest(http.MethodGet, "/v1/admin/platform/identity", nil))

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
	NewHandler(nil, nil).Register(mux, admin)

	if !wrapped {
		t.Fatal("GET /v1/admin/platform/identity was registered without the admin middleware")
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/admin/platform/identity", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("route not reachable: status %d", rec.Code)
	}
}

// #184 through the view: a control plane whose socket volume is not mounted
// reads Blocked on its own target, with the recreate named — the diagnosis the
// bare "not installed" used to hide.
func TestReleaseViewNamesTheUnmountedSocketVolume(t *testing.T) {
	h := NewHandler(&Deps{
		Channel:  func(context.Context) (string, string, error) { return ChannelStable, "develop", nil },
		Hosts:    func(context.Context) ([]HostIdentity, error) { return nil, nil },
		Releases: func(context.Context, string) ([]Release, error) { return nil, nil },
		Detection: func(context.Context) (DetectionStatus, error) {
			return DetectionStatus{}, nil
		},
		UpdaterPresent:        func() bool { return false },
		ControlPlanePreflight: func(context.Context) PreflightFacts { return PreflightFacts{Socket: &SocketState{}} },
	}, nil)
	v, err := h.ReleaseView(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cp := v.Targets[0]
	if cp.Kind != TargetControlPlane || cp.Preflight.State != PreflightBlocked {
		t.Fatalf("control-plane target = %+v, want a blocked preflight", cp)
	}
	sock := cp.Preflight.Checks[0]
	if sock.ID != CheckUpdaterSocket || sock.Status != CheckFail || !strings.Contains(sock.Detail, "--force-recreate --no-deps quasar-control-plane") {
		t.Fatalf("updater_socket = %+v, want the recreate named", sock)
	}
	// With no release listed the image check is unknown, not a fault.
	if img := cp.Preflight.Checks[len(cp.Preflight.Checks)-1]; img.ID != CheckImageResolvable || img.Status != CheckUnknown {
		t.Fatalf("image check = %+v", img)
	}
}
