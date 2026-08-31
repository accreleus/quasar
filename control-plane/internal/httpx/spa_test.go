package httpx_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/httpx"
)

// buildWebRoot creates a minimal fake web/dist tree for testing:
//
//	index.html
//	assets/main-abc123.js
func buildWebRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	must(t, os.WriteFile(filepath.Join(root, "index.html"), []byte("<!doctype html><html/>"), 0o644))
	must(t, os.MkdirAll(filepath.Join(root, "assets"), 0o755))
	must(t, os.WriteFile(filepath.Join(root, "assets", "main-abc123.js"), []byte("console.log(1)"), 0o644))
	must(t, os.WriteFile(filepath.Join(root, "favicon.ico"), []byte("ico"), 0o644))
	// #435: the installable-web-app files. They live at the web root (vite
	// copies public/* there), NOT under /assets/ — so they are content-addressed
	// by nothing and must not pick up the immutable cache header.
	must(t, os.WriteFile(filepath.Join(root, "manifest.webmanifest"), []byte(`{"name":"Quasar"}`), 0o644))
	must(t, os.MkdirAll(filepath.Join(root, "icons"), 0o755))
	must(t, os.WriteFile(filepath.Join(root, "icons", "icon-192.png"), []byte("\x89PNG"), 0o644))
	return root
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func do(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestSPAHandler_AssetHit(t *testing.T) {
	h := httpx.SPAHandler(buildWebRoot(t))
	rr := do(t, h, "/assets/main-abc123.js")
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
	cc := rr.Header().Get("Cache-Control")
	if cc != "public, max-age=31536000, immutable" {
		t.Fatalf("want immutable cache header, got %q", cc)
	}
}

func TestSPAHandler_AssetMiss(t *testing.T) {
	h := httpx.SPAHandler(buildWebRoot(t))
	rr := do(t, h, "/assets/nonexistent-xyz.js")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rr.Code)
	}
	// Response body must NOT be the SPA index.html (the MIME-block trap)
	body := rr.Body.String()
	if body == "<!doctype html><html/>" {
		t.Fatal("asset miss returned SPA HTML — MIME-block trap active")
	}
}

func TestSPAHandler_IndexRoot(t *testing.T) {
	h := httpx.SPAHandler(buildWebRoot(t))
	rr := do(t, h, "/")
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
	cc := rr.Header().Get("Cache-Control")
	if cc != "no-cache" {
		t.Fatalf("want no-cache on /, got %q", cc)
	}
}

func TestSPAHandler_SPARouteApp(t *testing.T) {
	h := httpx.SPAHandler(buildWebRoot(t))
	rr := do(t, h, "/app/games")
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200 for SPA route /app/games, got %d", rr.Code)
	}
	cc := rr.Header().Get("Cache-Control")
	if cc != "no-cache" {
		t.Fatalf("want no-cache on SPA route, got %q", cc)
	}
}

func TestSPAHandler_SPARouteAdmin(t *testing.T) {
	h := httpx.SPAHandler(buildWebRoot(t))
	rr := do(t, h, "/admin/users")
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200 for SPA route /admin/users, got %d", rr.Code)
	}
	cc := rr.Header().Get("Cache-Control")
	if cc != "no-cache" {
		t.Fatalf("want no-cache on admin SPA route, got %q", cc)
	}
}

func TestSPAHandler_UnmatchedAPIPath404(t *testing.T) {
	h := httpx.SPAHandler(buildWebRoot(t))
	// An unregistered /v1/* path (e.g. a method/route the mux never matched)
	// must 404, NOT fall through to the SPA index.html with 200 — otherwise a
	// call to a nonexistent endpoint looks like it succeeded.
	rr := do(t, h, "/v1/admin/sessions/some-id")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("want 404 for unmatched API path, got %d", rr.Code)
	}
	if body := rr.Body.String(); body == "<!doctype html><html/>" {
		t.Fatal("unmatched API path returned SPA HTML — masks the missing route")
	}
}

// #435 serves the manifest and its icons as ordinary static files through the
// existing "real file" branch — no new routing, no new cache policy. The trap
// this guards is the SPA fallback: a path that does not exist on disk returns
// index.html with 200, so a mis-copied manifest would look like it was served
// while actually handing the browser HTML.
func TestSPAHandler_ManifestAndIcons(t *testing.T) {
	h := httpx.SPAHandler(buildWebRoot(t))
	for _, tc := range []struct{ path, want string }{
		{"/manifest.webmanifest", `{"name":"Quasar"}`},
		{"/icons/icon-192.png", "\x89PNG"},
	} {
		rr := do(t, h, tc.path)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: want 200, got %d", tc.path, rr.Code)
		}
		if body := rr.Body.String(); body != tc.want {
			t.Fatalf("%s: served %q, not the file on disk (SPA fallback?)", tc.path, body)
		}
		if cc := rr.Header().Get("Cache-Control"); cc != "" {
			t.Fatalf("%s: want no explicit cache header, got %q", tc.path, cc)
		}
	}
}

func TestSPAHandler_RealStaticFile(t *testing.T) {
	h := httpx.SPAHandler(buildWebRoot(t))
	rr := do(t, h, "/favicon.ico")
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200 for favicon.ico, got %d", rr.Code)
	}
}
