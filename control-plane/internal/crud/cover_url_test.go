package crud

// UI-P7 review fix: openapi.yaml's AppListItem.cover_url documents that
// cover_url is "written EXCLUSIVELY by the artwork service ...; a value set
// directly via AppWrite is honoured only while the app has no artwork
// record" — but nothing enforced that. These tests cover the guard added to
// store.updateApp (ErrCoverURLOwnedByArtwork) plus the scheme validation
// added to the direct-write path (validDirectCoverURL). Requires Postgres
// (TEST_DATABASE_URL); shares the DB with every other crud test (-p 1
// mandatory).

import (
	"context"
	"net/http"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/artwork"
)

// TestUpdateAppCoverURLRefusedOnceArtworkExists is the core of the fix: once
// an app has an app_artwork record, a direct cover_url write in PATCH
// /v1/apps/{id} must be refused (409 conflict), not silently ignored, and
// must not change the stored value — even when bundled with other, otherwise
// valid, field changes in the same request.
func TestUpdateAppCoverURLRefusedOnceArtworkExists(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	ctx := context.Background()
	bearer := adminBearer(t, ctx, pool, authSvc, "admin@coverurl-owned.test", "coverurlowned")

	resp, body := post(t, srv.URL+"/v1/apps", map[string]any{"name": "Has Artwork"}, bearer)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: want 201, got %d (%v)", resp.StatusCode, body)
	}
	appID := body["app"].(map[string]any)["id"].(string)

	// Give the app an artwork record, exactly as the artwork service would:
	// this is the ONLY thing that should ever be allowed to set cover_url
	// from here on.
	awStore := artwork.NewStore(pool)
	if err := awStore.Save(ctx, artwork.Record{
		AppID:     appID,
		Source:    artwork.SourceManual,
		TileAsset: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.png",
	}); err != nil {
		t.Fatalf("seed artwork record: %v", err)
	}
	ownedCoverURL := artwork.AssetURL("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.png")

	// A direct write, alone.
	resp, body = patch(t, srv.URL+"/v1/apps/"+appID, map[string]any{
		"cover_url": "https://evil.example/steal.png",
	}, bearer)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("direct cover_url write with an artwork record: got %d (%v), want 409", resp.StatusCode, body)
	}
	if errObj, ok := body["error"].(map[string]any); !ok || errObj["code"] != "conflict" {
		t.Fatalf("error envelope: got %v, want code=conflict", body)
	}

	// A direct write bundled with an otherwise-valid field change must refuse
	// the WHOLE request — never a partial update.
	resp, body = patch(t, srv.URL+"/v1/apps/"+appID, map[string]any{
		"cover_url":   "https://evil.example/steal.png",
		"description": "should not apply either",
	}, bearer)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("bundled direct cover_url write: got %d (%v), want 409", resp.StatusCode, body)
	}

	// The stored value must be untouched by either attempt.
	resp, body = getReq(t, srv.URL+"/v1/apps/"+appID, bearer)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get app: want 200, got %d (%v)", resp.StatusCode, body)
	}
	app := body["app"].(map[string]any)
	if got := app["cover_url"]; got != ownedCoverURL {
		t.Fatalf("cover_url after refused direct writes: got %v, want %q (the artwork service's own value)", got, ownedCoverURL)
	}
	if got := app["description"]; got != "" {
		t.Fatalf("description after a refused bundled patch: got %q, want unchanged (\"\")", got)
	}
}

// TestUpdateAppCoverURLAllowedWithoutArtwork is the documented allowance this
// fix must NOT regress: an app with no app_artwork record still honours a
// direct cover_url write.
func TestUpdateAppCoverURLAllowedWithoutArtwork(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	ctx := context.Background()
	bearer := adminBearer(t, ctx, pool, authSvc, "admin@coverurl-none.test", "coverurlnone")

	resp, body := post(t, srv.URL+"/v1/apps", map[string]any{"name": "No Artwork Yet"}, bearer)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: want 201, got %d (%v)", resp.StatusCode, body)
	}
	appID := body["app"].(map[string]any)["id"].(string)

	resp, body = patch(t, srv.URL+"/v1/apps/"+appID, map[string]any{
		"cover_url": "https://cdn.example/my-own-art.png",
	}, bearer)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("direct cover_url write with no artwork record: got %d (%v), want 200", resp.StatusCode, body)
	}
	if got := body["app"].(map[string]any)["cover_url"]; got != "https://cdn.example/my-own-art.png" {
		t.Fatalf("cover_url: got %v, want the written value", got)
	}

	// Also allowed at create time.
	resp, body = post(t, srv.URL+"/v1/apps", map[string]any{
		"name":      "Set At Birth",
		"cover_url": "https://cdn.example/at-birth.png",
	}, bearer)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create with cover_url: want 201, got %d (%v)", resp.StatusCode, body)
	}
	if got := body["app"].(map[string]any)["cover_url"]; got != "https://cdn.example/at-birth.png" {
		t.Fatalf("cover_url on create: got %v, want the written value", got)
	}
}

// TestArtworkServiceCoverURLWriteUnaffectedByGuard confirms the artwork
// service's own write path (internal/artwork Store.Save/Clear) is a
// different code path from crud.updateApp and is not touched by the new
// guard: it must keep working exactly as before, both to set AND to clear
// cover_url, regardless of any prior direct write.
func TestArtworkServiceCoverURLWriteUnaffectedByGuard(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	ctx := context.Background()
	bearer := adminBearer(t, ctx, pool, authSvc, "admin@coverurl-artwork.test", "coverurlartwork")

	resp, body := post(t, srv.URL+"/v1/apps", map[string]any{
		"name":      "Artwork Service Owns Me",
		"cover_url": "https://cdn.example/manual-before-artwork.png",
	}, bearer)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: want 201, got %d (%v)", resp.StatusCode, body)
	}
	appID := body["app"].(map[string]any)["id"].(string)

	awStore := artwork.NewStore(pool)
	const tileAsset = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb.png"
	if err := awStore.Save(ctx, artwork.Record{
		AppID:     appID,
		Source:    artwork.SourceProvider,
		TileAsset: tileAsset,
	}); err != nil {
		t.Fatalf("artwork service save: %v", err)
	}

	resp, body = getReq(t, srv.URL+"/v1/apps/"+appID, bearer)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get app: want 200, got %d (%v)", resp.StatusCode, body)
	}
	if got, want := body["app"].(map[string]any)["cover_url"], artwork.AssetURL(tileAsset); got != want {
		t.Fatalf("cover_url after artwork service save: got %v, want %q", got, want)
	}

	// Clearing (the artwork DELETE path) must also still work.
	if err := awStore.Clear(ctx, appID); err != nil {
		t.Fatalf("artwork service clear: %v", err)
	}
	resp, body = getReq(t, srv.URL+"/v1/apps/"+appID, bearer)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get app: want 200, got %d (%v)", resp.StatusCode, body)
	}
	if got := body["app"].(map[string]any)["cover_url"]; got != nil {
		t.Fatalf("cover_url after artwork service clear: got %v, want null", got)
	}

	// And with the artwork record gone again, the documented direct-write
	// allowance is back in effect.
	resp, body = patch(t, srv.URL+"/v1/apps/"+appID, map[string]any{
		"cover_url": "https://cdn.example/manual-after-clear.png",
	}, bearer)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("direct write after artwork clear: got %d (%v), want 200", resp.StatusCode, body)
	}
	if got := body["app"].(map[string]any)["cover_url"]; got != "https://cdn.example/manual-after-clear.png" {
		t.Fatalf("cover_url after clear + direct write: got %v", got)
	}
}

// TestDirectCoverURLSchemeValidation covers the additional hardening this fix
// adds: even the documented direct-write allowance (no artwork record) must
// not accept an arbitrary scheme, since the value is rendered straight into
// an <img src> for every user browsing the library (UI-P7's "never hotlink"
// goal — control-api.md §"The app write shape and cover_url").
func TestDirectCoverURLSchemeValidation(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	ctx := context.Background()
	bearer := adminBearer(t, ctx, pool, authSvc, "admin@coverurl-scheme.test", "coverurlscheme")

	cases := []struct {
		name      string
		coverURL  string
		wantValid bool
	}{
		{"https", "https://cdn.example/art.png", true},
		{"http", "http://cdn.example/art.png", true},
		{"relative path", "/static/art.png", true},
		{"javascript scheme", "javascript:alert(1)", false},
		{"data uri", "data:image/png;base64,AAAA", false},
		{"file scheme", "file:///etc/passwd", false},
	}

	for _, c := range cases {
		t.Run("create/"+c.name, func(t *testing.T) {
			resp, body := post(t, srv.URL+"/v1/apps", map[string]any{
				"name":      "Scheme " + c.name,
				"cover_url": c.coverURL,
			}, bearer)
			if c.wantValid && resp.StatusCode != http.StatusCreated {
				t.Fatalf("create with %q: got %d (%v), want 201", c.coverURL, resp.StatusCode, body)
			}
			if !c.wantValid && resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("create with %q: got %d (%v), want 400", c.coverURL, resp.StatusCode, body)
			}
		})
	}

	// And on the patch path, against a fresh app with no artwork record so
	// the ownership guard is not what is being exercised here.
	resp, body := post(t, srv.URL+"/v1/apps", map[string]any{"name": "Scheme Patch Target"}, bearer)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: want 201, got %d (%v)", resp.StatusCode, body)
	}
	appID := body["app"].(map[string]any)["id"].(string)

	for _, c := range cases {
		t.Run("patch/"+c.name, func(t *testing.T) {
			resp, body := patch(t, srv.URL+"/v1/apps/"+appID, map[string]any{
				"cover_url": c.coverURL,
			}, bearer)
			if c.wantValid && resp.StatusCode != http.StatusOK {
				t.Fatalf("patch with %q: got %d (%v), want 200", c.coverURL, resp.StatusCode, body)
			}
			if !c.wantValid && resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("patch with %q: got %d (%v), want 400", c.coverURL, resp.StatusCode, body)
			}
		})
	}
}
