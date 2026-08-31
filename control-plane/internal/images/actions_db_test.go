// actions_db_test.go — image-management P3 acceptance: the install / uninstall
// / pin / update route matrix through the REAL RequireAuth→RequireAdmin chain,
// the update-policy matrix, persisted sync state, and the #440 property that an
// ensure dispatched after an install carries the DIGEST form.
//
// TEST_DATABASE_URL-gated like every other DB test in this repo (make test-db
// provisions the database that makes them actually run).
package images

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
	"github.com/accreleus/quasar/control-plane/internal/audit"
	"github.com/accreleus/quasar/control-plane/internal/auth"
)

const (
	// imgDigest is the resolved digest form the catalog stores and every
	// dispatch must carry (#440) — never the mutable tag imgRef.
	imgDigest = "ghcr.io/accreleus/quasar-steam@sha256:ab12cd34ab12cd34ab12cd34ab12cd34ab12cd34ab12cd34ab12cd34ab12cd34"
	// imgDigest2 is what a NEWER catalog version resolves to.
	imgDigest2 = "ghcr.io/accreleus/quasar-steam@sha256:99887766998877669988776699887766998877669988776699887766998877aa"
	imgVer2    = "2026.08.09"
)

// actionsEnv is one wired P3 test environment: a Store with a real Ensurer over
// a fake fleet, served behind the real admin gate.
type actionsEnv struct {
	pool  *pgxpool.Pool
	store *Store
	fleet *fakeFleet
	ens   *Ensurer
	// do performs an authenticated admin request and returns status + body.
	do func(t *testing.T, method, path, body string) (int, []byte)
}

// newActionsEnv builds the environment. hosts are seeded into `hosts` and are
// all "connected" from the fake fleet's point of view.
func newActionsEnv(t *testing.T, hostNames ...string) (*actionsEnv, []string) {
	t.Helper()
	ctx := context.Background()
	pool := ensureDB(t)

	var hostIDs []string
	for _, n := range hostNames {
		hostIDs = append(hostIDs, seedHost(t, pool, n))
	}
	fleet := newFleet(hostIDs...)
	ens := newEnsurer(pool, fleet, testLog(), WithAckTimeout(2*time.Second), WithRetry(0, time.Millisecond))
	t.Cleanup(ens.Close)

	store := NewStoreWithFetcher(pool, fixtureFetcher{data: readFixture(t)})
	store.SetLogger(testLog())
	store.SetEnsurer(ens)

	authSvc, err := auth.NewService(pool, auth.DefaultParams(), time.Hour)
	mustT(t, err)
	authHandler := auth.NewHandler(authSvc)
	_, err = authSvc.Register(ctx, "p3-admin@t.local", "p3admin", "password12345")
	mustT(t, err)
	_, err = pool.Exec(ctx, `UPDATE users SET role='admin' WHERE email='p3-admin@t.local'`)
	mustT(t, err)
	tok, err := authSvc.Login(ctx, "p3-admin@t.local", "password12345", "test")
	mustT(t, err)

	mux := http.NewServeMux()
	// Real audit store: the action routes were built without one, and
	// images_audit_test.go reads the rows back out of admin_activity.
	NewHandler(store, audit.NewStore(pool)).Register(mux, func(next http.Handler) http.Handler {
		return authHandler.RequireAuth(authHandler.RequireAdmin(next))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	do := func(t *testing.T, method, path, body string) (int, []byte) {
		t.Helper()
		req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		mustT(t, err)
		req.Header.Set("Authorization", "Bearer "+tok.Plaintext)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req)
		mustT(t, err)
		defer resp.Body.Close()
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		return resp.StatusCode, buf.Bytes()
	}

	return &actionsEnv{pool: pool, store: store, fleet: fleet, ens: ens, do: do}, hostIDs
}

// seedCatalogDigest inserts/updates the catalog row at (version, digest).
func seedCatalogDigest(t *testing.T, pool *pgxpool.Pool, version, digest string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO image_catalog (id, manifest_version, display_name, kind, version, registry_ref, registry_digest, raw)
		VALUES ($1, 1, 'Steam', 'prebuilt', $2, $3, $4, '{}'::jsonb)
		ON CONFLICT (id) DO UPDATE SET version = EXCLUDED.version, registry_digest = EXCLUDED.registry_digest
	`, imgID, version, imgRef, digest); err != nil {
		t.Fatalf("seed image_catalog: %v", err)
	}
}

// errCode pulls the discriminator out of an error envelope body.
func errCode(t *testing.T, body []byte) string {
	t.Helper()
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode error envelope %q: %v", string(body), err)
	}
	return env.Error.Code
}

func decodeImage(t *testing.T, body []byte) CatalogImage {
	t.Helper()
	var img CatalogImage
	if err := json.Unmarshal(body, &img); err != nil {
		t.Fatalf("decode CatalogImage %q: %v", string(body), err)
	}
	return img
}

// adoptedRef reads what installed_images actually holds — the value every
// ensure dispatch carries.
func adoptedRef(t *testing.T, pool *pgxpool.Pool) (version, ref string, pinned, lazy bool) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`SELECT version, registry_ref, pinned, lazy FROM installed_images WHERE image_id = $1`, imgID).
		Scan(&version, &ref, &pinned, &lazy); err != nil {
		t.Fatalf("read installed_images: %v", err)
	}
	return version, ref, pinned, lazy
}

// --- install ------------------------------------------------------------------

// TestInstallAdoptsDigestAndEnsuresEverywhere — the P3 acceptance line AND the
// #440 property: the adoption stores the DIGEST form, and the ensure the
// install kicks carries that digest, not the mutable tag.
func TestInstallAdoptsDigestAndEnsuresEverywhere(t *testing.T) {
	env, hostIDs := newActionsEnv(t, "host-a")
	seedCatalogDigest(t, env.pool, imgVer, imgDigest)

	code, body := env.do(t, http.MethodPost, "/v1/admin/images/"+imgID+"/install", `{"lazy":false}`)
	if code != http.StatusCreated {
		t.Fatalf("install: status %d body %s, want 201", code, body)
	}
	img := decodeImage(t, body)
	if !img.Installed || img.InstalledVersion == nil || *img.InstalledVersion != imgVer {
		t.Fatalf("201 body adoption state: %+v", img)
	}
	if img.Pinned || img.Lazy {
		t.Fatalf("201 body: pinned=%v lazy=%v, want both false", img.Pinned, img.Lazy)
	}
	if img.RegistryDigest == nil || *img.RegistryDigest != imgDigest {
		t.Fatalf("201 body registry_digest: %v", img.RegistryDigest)
	}

	_, ref, _, _ := adoptedRef(t, env.pool)
	if ref != imgDigest {
		t.Fatalf("adopted registry_ref: got %q want the digest form %q (#440)", ref, imgDigest)
	}

	call := env.fleet.waitEnsure(t)
	if call.HostID != hostIDs[0] || call.ImageID != imgID {
		t.Fatalf("ensure dispatched to the wrong target: %+v", call)
	}
	if call.RegistryRef != imgDigest {
		t.Fatalf("ensure registry_ref: got %q want the digest form %q (#440 — a dispatch must never carry a mutable tag)", call.RegistryRef, imgDigest)
	}
	if call.Version != imgVer {
		t.Fatalf("ensure version: got %q want %q", call.Version, imgVer)
	}
}

// TestInstallLazyDispatchesNothing — a lazy install seeds the adoption row and
// dispatches nothing; hosts pull at first launch placement instead.
func TestInstallLazyDispatchesNothing(t *testing.T) {
	env, _ := newActionsEnv(t, "host-a")
	seedCatalogDigest(t, env.pool, imgVer, imgDigest)

	code, body := env.do(t, http.MethodPost, "/v1/admin/images/"+imgID+"/install", `{"lazy":true}`)
	if code != http.StatusCreated {
		t.Fatalf("lazy install: status %d body %s, want 201", code, body)
	}
	if img := decodeImage(t, body); !img.Lazy {
		t.Fatalf("201 body lazy: got false, want true")
	}
	if _, _, _, lazy := adoptedRef(t, env.pool); !lazy {
		t.Fatal("installed_images.lazy: got false, want true")
	}
	env.ens.Wait()
	env.fleet.noMoreEnsures(t, 200*time.Millisecond)
}

// TestInstallDefaultsToEagerWithNoBody — the request body is optional
// (openapi requestBody required:false); an absent body means lazy:false, not a
// 400.
func TestInstallDefaultsToEagerWithNoBody(t *testing.T) {
	env, _ := newActionsEnv(t, "host-a")
	seedCatalogDigest(t, env.pool, imgVer, imgDigest)

	code, body := env.do(t, http.MethodPost, "/v1/admin/images/"+imgID+"/install", "")
	if code != http.StatusCreated {
		t.Fatalf("install with no body: status %d body %s, want 201", code, body)
	}
	if img := decodeImage(t, body); img.Lazy {
		t.Fatal("201 body lazy: got true, want false (absent body ⇒ default eager)")
	}
	env.fleet.waitEnsure(t)
}

// TestInstallUnknownIdIs404 — 404 not_found for an id that is not in the
// catalog at all.
func TestInstallUnknownIdIs404(t *testing.T) {
	env, _ := newActionsEnv(t, "host-a")
	seedCatalogDigest(t, env.pool, imgVer, imgDigest)

	code, body := env.do(t, http.MethodPost, "/v1/admin/images/nope/install", `{}`)
	if code != http.StatusNotFound {
		t.Fatalf("install unknown id: status %d body %s, want 404", code, body)
	}
	if got := errCode(t, body); got != "not_found" {
		t.Fatalf("error code: got %q want not_found", got)
	}
}

// TestInstallTwiceIs409AlreadyInstalled — a second install is a 409 with its
// own discriminator, never a silent re-adopt (which would reset a pin).
func TestInstallTwiceIs409AlreadyInstalled(t *testing.T) {
	env, _ := newActionsEnv(t, "host-a")
	seedCatalogDigest(t, env.pool, imgVer, imgDigest)

	if code, body := env.do(t, http.MethodPost, "/v1/admin/images/"+imgID+"/install", `{"lazy":true}`); code != http.StatusCreated {
		t.Fatalf("first install: status %d body %s", code, body)
	}
	code, body := env.do(t, http.MethodPost, "/v1/admin/images/"+imgID+"/install", `{"lazy":true}`)
	if code != http.StatusConflict {
		t.Fatalf("second install: status %d body %s, want 409", code, body)
	}
	if got := errCode(t, body); got != "already_installed" {
		t.Fatalf("error code: got %q want already_installed", got)
	}
}

// TestInstallUnresolvedDigestIs409 — installing an image whose digest the last
// sync could not resolve is refused. Adopting the mutable tag instead is
// exactly the fleet split #440 exists to prevent.
func TestInstallUnresolvedDigestIs409(t *testing.T) {
	env, _ := newActionsEnv(t, "host-a")
	seedCatalogDigest(t, env.pool, imgVer, "")

	code, body := env.do(t, http.MethodPost, "/v1/admin/images/"+imgID+"/install", `{}`)
	if code != http.StatusConflict {
		t.Fatalf("install with an unresolved digest: status %d body %s, want 409", code, body)
	}
	if got := errCode(t, body); got != "digest_unresolved" {
		t.Fatalf("error code: got %q want digest_unresolved", got)
	}
	var n int
	if err := env.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM installed_images WHERE image_id = $1`, imgID).Scan(&n); err != nil {
		t.Fatalf("count installed_images: %v", err)
	}
	if n != 0 {
		t.Fatal("a refused install must not have written an adoption row")
	}
}

// --- P4: template install/update -----------------------------------------------

// seedCatalogTemplate inserts/updates a kind=template catalog row at
// (version, context_sha) — the P4 analogue of seedCatalogDigest.
func seedCatalogTemplate(t *testing.T, pool *pgxpool.Pool, version, contextSHA string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO image_catalog (id, manifest_version, display_name, kind, version, dockerfile, context_sha, raw)
		VALUES ($1, 1, 'XFCE Desktop', 'template', $2, 'Dockerfile', $3, '{}'::jsonb)
		ON CONFLICT (id) DO UPDATE SET version = EXCLUDED.version, context_sha = EXCLUDED.context_sha
	`, tplID, version, contextSHA); err != nil {
		t.Fatalf("seed template image_catalog: %v", err)
	}
}

// adoptedTemplate reads what installed_images actually holds for the P4
// template row — the local_tag/registry_ref split every dispatch depends on.
func adoptedTemplate(t *testing.T, pool *pgxpool.Pool) (version, registryRef, localTag string, pinned, lazy bool) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`SELECT version, registry_ref, local_tag, pinned, lazy FROM installed_images WHERE image_id = $1`, tplID).
		Scan(&version, &registryRef, &localTag, &pinned, &lazy); err != nil {
		t.Fatalf("read installed_images: %v", err)
	}
	return version, registryRef, localTag, pinned, lazy
}

// TestInstallTemplateAdoptsLocalTagAndDispatchesBuild — the P4 acceptance
// line: a template install adopts the CP-assigned local_tag (registry_ref
// stays empty) and dispatches image_build, never image_ensure.
func TestInstallTemplateAdoptsLocalTagAndDispatchesBuild(t *testing.T) {
	env, hostIDs := newActionsEnv(t, "host-a")
	seedCatalogTemplate(t, env.pool, tplVer, tplContextSHA)

	code, body := env.do(t, http.MethodPost, "/v1/admin/images/"+tplID+"/install", `{"lazy":false}`)
	if code != http.StatusCreated {
		t.Fatalf("install: status %d body %s, want 201", code, body)
	}
	img := decodeImage(t, body)
	if !img.Installed || img.InstalledVersion == nil || *img.InstalledVersion != tplVer {
		t.Fatalf("201 body adoption state: %+v", img)
	}
	if img.LocalTag == nil || *img.LocalTag != tplLocalTag(tplVer) {
		t.Fatalf("201 body local_tag: got %v want %q", img.LocalTag, tplLocalTag(tplVer))
	}
	if img.RegistryRef != nil {
		t.Fatalf("201 body registry_ref: got %v want nil (a template never adopts a registry ref)", img.RegistryRef)
	}

	version, registryRef, localTag, _, _ := adoptedTemplate(t, env.pool)
	if version != tplVer || localTag != tplLocalTag(tplVer) || registryRef != "" {
		t.Fatalf("adopted row: version=%q registry_ref=%q local_tag=%q", version, registryRef, localTag)
	}

	call := env.fleet.waitBuild(t)
	if call.HostID != hostIDs[0] || call.ImageID != tplID || call.LocalTag != tplLocalTag(tplVer) {
		t.Fatalf("build dispatched: %+v", call)
	}
	env.fleet.noMoreEnsures(t, 200*time.Millisecond)
}

// TestInstallTemplateUnresolvedContextIs409 — the P4 analogue of
// TestInstallUnresolvedDigestIs409: refused, no adoption row written.
func TestInstallTemplateUnresolvedContextIs409(t *testing.T) {
	env, _ := newActionsEnv(t, "host-a")
	seedCatalogTemplate(t, env.pool, tplVer, "")

	code, body := env.do(t, http.MethodPost, "/v1/admin/images/"+tplID+"/install", `{}`)
	if code != http.StatusConflict {
		t.Fatalf("install with an unresolved context sha: status %d body %s, want 409", code, body)
	}
	if got := errCode(t, body); got != "context_unresolved" {
		t.Fatalf("error code: got %q want context_unresolved", got)
	}
	var n int
	if err := env.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM installed_images WHERE image_id = $1`, tplID).Scan(&n); err != nil {
		t.Fatalf("count installed_images: %v", err)
	}
	if n != 0 {
		t.Fatal("a refused install must not have written an adoption row")
	}
}

// TestPolicyAutoRebuildsUnpinnedTemplate — the update-policy matrix, P4
// generalization of TestPolicyAutoReAdoptsUnpinnedOnly: auto re-adopts a
// drifted template's NEW local_tag and re-dispatches image_build, not
// image_ensure.
func TestPolicyAutoRebuildsUnpinnedTemplate(t *testing.T) {
	env, hostIDs := newActionsEnv(t, "host-a")
	seedCatalogTemplate(t, env.pool, tplVer, tplContextSHA)
	if code, _ := env.do(t, http.MethodPost, "/v1/admin/images/"+tplID+"/install", `{}`); code != http.StatusCreated {
		t.Fatal("install failed")
	}
	env.fleet.waitBuild(t)
	setPolicy(t, env.pool, "auto")

	const tplVer2 = "2026.08.09"
	const tplSHA2 = "0123456789abcdef0123456789abcdef01234567"
	seedCatalogTemplate(t, env.pool, tplVer2, tplSHA2)
	env.store.applyUpdatePolicy(context.Background())

	version, _, localTag, _, _ := adoptedTemplate(t, env.pool)
	if version != tplVer2 || localTag != tplLocalTag(tplVer2) {
		t.Fatalf("auto policy did not re-adopt the template: got (%q,%q) want (%q,%q)",
			version, localTag, tplVer2, tplLocalTag(tplVer2))
	}
	call := env.fleet.waitBuild(t)
	if call.HostID != hostIDs[0] || call.LocalTag != tplLocalTag(tplVer2) || call.Version != tplVer2 {
		t.Fatalf("auto policy build dispatch: %+v", call)
	}
}

// --- uninstall ----------------------------------------------------------------

// TestUninstallDispatchesRemoveAndCleansRows — P2's untested half: uninstall
// sends image_remove to every connected host that has the image, deletes that
// image's host_images rows, and drops the adoption row.
func TestUninstallDispatchesRemoveAndCleansRows(t *testing.T) {
	env, hostIDs := newActionsEnv(t, "host-a")
	seedCatalogDigest(t, env.pool, imgVer, imgDigest)

	if code, body := env.do(t, http.MethodPost, "/v1/admin/images/"+imgID+"/install", `{"lazy":true}`); code != http.StatusCreated {
		t.Fatalf("install: status %d body %s", code, body)
	}
	// The host reports it has the image — this is what makes it a remove target.
	env.ens.AgentImageState(context.Background(), hostIDs[0], agentws.ImageStateMsg{
		Type: "image_state", ImageID: imgID, Version: imgVer, State: "ready",
	})
	if _, ok := hostStateOf(t, env.pool, hostIDs[0]); !ok {
		t.Fatal("host_images row was not created by the image_state report")
	}

	code, body := env.do(t, http.MethodDelete, "/v1/admin/images/"+imgID+"/install", "")
	if code != http.StatusNoContent {
		t.Fatalf("uninstall: status %d body %s, want 204", code, body)
	}

	rm := env.fleet.waitRemove(t)
	if rm.HostID != hostIDs[0] || rm.ImageID != imgID {
		t.Fatalf("image_remove dispatched to the wrong target: %+v", rm)
	}

	var installed, hostRows int
	if err := env.pool.QueryRow(context.Background(),
		`SELECT (SELECT count(*) FROM installed_images WHERE image_id = $1),
		        (SELECT count(*) FROM host_images WHERE image_id = $1)`, imgID).Scan(&installed, &hostRows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if installed != 0 || hostRows != 0 {
		t.Fatalf("after uninstall: installed_images=%d host_images=%d, want 0/0", installed, hostRows)
	}

	// Second uninstall: nothing to uninstall.
	code2, body2 := env.do(t, http.MethodDelete, "/v1/admin/images/"+imgID+"/install", "")
	if code2 != http.StatusNotFound {
		t.Fatalf("second uninstall: status %d body %s, want 404", code2, body2)
	}
	if got := errCode(t, body2); got != "not_installed" {
		t.Fatalf("error code: got %q want not_installed", got)
	}
}

// --- pin / unpin --------------------------------------------------------------

// TestPinIsIdempotentAndBlocksUpdate — the pin contract end to end: 204 twice,
// update refused with 409 conflict while pinned, unpin 204 twice, update then
// applies.
func TestPinIsIdempotentAndBlocksUpdate(t *testing.T) {
	env, _ := newActionsEnv(t, "host-a")
	seedCatalogDigest(t, env.pool, imgVer, imgDigest)
	if code, body := env.do(t, http.MethodPost, "/v1/admin/images/"+imgID+"/install", `{"lazy":true}`); code != http.StatusCreated {
		t.Fatalf("install: status %d body %s", code, body)
	}

	for i := 0; i < 2; i++ {
		if code, body := env.do(t, http.MethodPost, "/v1/admin/images/"+imgID+"/pin", ""); code != http.StatusNoContent {
			t.Fatalf("pin #%d: status %d body %s, want 204 (idempotent)", i+1, code, body)
		}
	}
	if _, _, pinned, _ := adoptedRef(t, env.pool); !pinned {
		t.Fatal("installed_images.pinned: got false after pin")
	}

	// The catalog moves forward; the pin must hold.
	seedCatalogDigest(t, env.pool, imgVer2, imgDigest2)
	code, body := env.do(t, http.MethodPost, "/v1/admin/images/"+imgID+"/update", "")
	if code != http.StatusConflict {
		t.Fatalf("update while pinned: status %d body %s, want 409", code, body)
	}
	if got := errCode(t, body); got != "conflict" {
		t.Fatalf("error code: got %q want conflict", got)
	}

	for i := 0; i < 2; i++ {
		if code, body := env.do(t, http.MethodDelete, "/v1/admin/images/"+imgID+"/pin", ""); code != http.StatusNoContent {
			t.Fatalf("unpin #%d: status %d body %s, want 204 (idempotent)", i+1, code, body)
		}
	}
	code, body = env.do(t, http.MethodPost, "/v1/admin/images/"+imgID+"/update", "")
	if code != http.StatusOK {
		t.Fatalf("update after unpin: status %d body %s, want 200", code, body)
	}
	var res struct {
		Applied bool         `json:"applied"`
		Image   CatalogImage `json:"image"`
	}
	mustT(t, json.Unmarshal(body, &res))
	if !res.Applied {
		t.Fatal("update after unpin: applied=false, want true")
	}
	if v, ref, _, _ := adoptedRef(t, env.pool); v != imgVer2 || ref != imgDigest2 {
		t.Fatalf("re-adoption: got (%q,%q) want (%q,%q)", v, ref, imgVer2, imgDigest2)
	}
}

// TestPinNotInstalledIs404 — pin/unpin of an image with no adoption row.
func TestPinNotInstalledIs404(t *testing.T) {
	env, _ := newActionsEnv(t, "host-a")
	seedCatalogDigest(t, env.pool, imgVer, imgDigest)

	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		code, body := env.do(t, method, "/v1/admin/images/"+imgID+"/pin", "")
		if code != http.StatusNotFound {
			t.Fatalf("%s pin without an adoption: status %d body %s, want 404", method, code, body)
		}
		if got := errCode(t, body); got != "not_installed" {
			t.Fatalf("%s pin error code: got %q want not_installed", method, got)
		}
	}
}

// --- update -------------------------------------------------------------------

// TestUpdateNoopWhenAlreadyCurrent — applied:false with a 200, not an error: a
// UI button must not have to branch on status code to tell "worked" from "was
// already current".
func TestUpdateNoopWhenAlreadyCurrent(t *testing.T) {
	env, _ := newActionsEnv(t, "host-a")
	seedCatalogDigest(t, env.pool, imgVer, imgDigest)
	if code, _ := env.do(t, http.MethodPost, "/v1/admin/images/"+imgID+"/install", `{"lazy":true}`); code != http.StatusCreated {
		t.Fatal("install failed")
	}

	code, body := env.do(t, http.MethodPost, "/v1/admin/images/"+imgID+"/update", "")
	if code != http.StatusOK {
		t.Fatalf("update at the current version: status %d body %s, want 200", code, body)
	}
	var res struct {
		Applied bool         `json:"applied"`
		Image   CatalogImage `json:"image"`
	}
	mustT(t, json.Unmarshal(body, &res))
	if res.Applied {
		t.Fatal("applied: got true, want false for a no-op update")
	}
	if res.Image.ID != imgID {
		t.Fatalf("no-op update body image: %+v", res.Image)
	}
}

// TestUpdateReEnsuresWithTheNewDigest — an update re-adopts and re-ensures, and
// the dispatch carries the NEW digest.
func TestUpdateReEnsuresWithTheNewDigest(t *testing.T) {
	env, hostIDs := newActionsEnv(t, "host-a")
	seedCatalogDigest(t, env.pool, imgVer, imgDigest)
	if code, _ := env.do(t, http.MethodPost, "/v1/admin/images/"+imgID+"/install", `{}`); code != http.StatusCreated {
		t.Fatal("install failed")
	}
	first := env.fleet.waitEnsure(t)
	if first.RegistryRef != imgDigest {
		t.Fatalf("install ensure ref: got %q", first.RegistryRef)
	}

	seedCatalogDigest(t, env.pool, imgVer2, imgDigest2)
	if code, body := env.do(t, http.MethodPost, "/v1/admin/images/"+imgID+"/update", ""); code != http.StatusOK {
		t.Fatalf("update: status %d body %s", code, body)
	}
	second := env.fleet.waitEnsure(t)
	if second.HostID != hostIDs[0] || second.RegistryRef != imgDigest2 || second.Version != imgVer2 {
		t.Fatalf("update ensure: %+v, want host=%s ref=%s version=%s", second, hostIDs[0], imgDigest2, imgVer2)
	}
}

// TestUpdateNotInstalledIs404 and TestUpdateUnresolvedDigestIs409.
func TestUpdateErrorMatrix(t *testing.T) {
	env, _ := newActionsEnv(t, "host-a")
	seedCatalogDigest(t, env.pool, imgVer, imgDigest)

	code, body := env.do(t, http.MethodPost, "/v1/admin/images/"+imgID+"/update", "")
	if code != http.StatusNotFound {
		t.Fatalf("update without an adoption: status %d body %s, want 404", code, body)
	}
	if got := errCode(t, body); got != "not_installed" {
		t.Fatalf("error code: got %q want not_installed", got)
	}

	if code, _ := env.do(t, http.MethodPost, "/v1/admin/images/"+imgID+"/install", `{"lazy":true}`); code != http.StatusCreated {
		t.Fatal("install failed")
	}
	// The catalog moves forward but the new version's digest is unresolved.
	seedCatalogDigest(t, env.pool, imgVer2, "")
	code, body = env.do(t, http.MethodPost, "/v1/admin/images/"+imgID+"/update", "")
	if code != http.StatusConflict {
		t.Fatalf("update with an unresolved digest: status %d body %s, want 409", code, body)
	}
	if got := errCode(t, body); got != "digest_unresolved" {
		t.Fatalf("error code: got %q want digest_unresolved", got)
	}
	if v, _, _, _ := adoptedRef(t, env.pool); v != imgVer {
		t.Fatalf("a refused update must leave the adoption at %q, got %q", imgVer, v)
	}
}

// --- update policy ------------------------------------------------------------

func setPolicy(t *testing.T, pool *pgxpool.Pool, policy string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO instance_settings (id, image_update_policy) VALUES (true, $1)
		ON CONFLICT (id) DO UPDATE SET image_update_policy = EXCLUDED.image_update_policy
	`, policy); err != nil {
		t.Fatalf("set image_update_policy=%s: %v", policy, err)
	}
}

// driftSync runs one sync whose fetch/parse is stubbed out — the policy is
// applied against a catalog the test moved forward itself, which is the exact
// situation "a sync brought a newer version" produces without needing a second
// manifest fixture.
func (e *actionsEnv) driftSync(t *testing.T) {
	t.Helper()
	seedCatalogDigest(t, e.pool, imgVer2, imgDigest2)
	e.store.applyUpdatePolicy(context.Background())
}

// TestPolicyManualAndNotifyDoNotUpdate — both policies leave the adoption
// alone; update_available is reported and nothing else happens.
func TestPolicyManualAndNotifyDoNotUpdate(t *testing.T) {
	for _, policy := range []string{"manual", "notify"} {
		t.Run(policy, func(t *testing.T) {
			env, _ := newActionsEnv(t, "host-a")
			seedCatalogDigest(t, env.pool, imgVer, imgDigest)
			if code, _ := env.do(t, http.MethodPost, "/v1/admin/images/"+imgID+"/install", `{}`); code != http.StatusCreated {
				t.Fatal("install failed")
			}
			env.fleet.waitEnsure(t)
			setPolicy(t, env.pool, policy)

			env.driftSync(t)

			if v, ref, _, _ := adoptedRef(t, env.pool); v != imgVer || ref != imgDigest {
				t.Fatalf("policy %s re-adopted: got (%q,%q), want the ORIGINAL (%q,%q)", policy, v, ref, imgVer, imgDigest)
			}
			env.ens.Wait()
			env.fleet.noMoreEnsures(t, 200*time.Millisecond)

			img, err := env.store.ImageByID(context.Background(), imgID)
			mustT(t, err)
			if !img.UpdateAvailable {
				t.Fatalf("policy %s: update_available must still be reported", policy)
			}
		})
	}
}

// TestPolicyAutoReAdoptsUnpinnedOnly — auto re-adopts and re-ensures the
// unpinned drifted image; a pinned one is skipped regardless of policy.
func TestPolicyAutoReAdoptsUnpinnedOnly(t *testing.T) {
	env, hostIDs := newActionsEnv(t, "host-a")
	seedCatalogDigest(t, env.pool, imgVer, imgDigest)
	if code, _ := env.do(t, http.MethodPost, "/v1/admin/images/"+imgID+"/install", `{}`); code != http.StatusCreated {
		t.Fatal("install failed")
	}
	env.fleet.waitEnsure(t)
	setPolicy(t, env.pool, "auto")

	// Pinned first: auto must skip it entirely.
	if code, _ := env.do(t, http.MethodPost, "/v1/admin/images/"+imgID+"/pin", ""); code != http.StatusNoContent {
		t.Fatal("pin failed")
	}
	env.driftSync(t)
	if v, ref, _, _ := adoptedRef(t, env.pool); v != imgVer || ref != imgDigest {
		t.Fatalf("auto policy updated a PINNED image: got (%q,%q)", v, ref)
	}
	env.ens.Wait()
	env.fleet.noMoreEnsures(t, 200*time.Millisecond)

	// Unpin: the same sync now re-adopts and re-ensures.
	if code, _ := env.do(t, http.MethodDelete, "/v1/admin/images/"+imgID+"/pin", ""); code != http.StatusNoContent {
		t.Fatal("unpin failed")
	}
	env.driftSync(t)
	if v, ref, _, _ := adoptedRef(t, env.pool); v != imgVer2 || ref != imgDigest2 {
		t.Fatalf("auto policy did not re-adopt: got (%q,%q) want (%q,%q)", v, ref, imgVer2, imgDigest2)
	}
	call := env.fleet.waitEnsure(t)
	if call.HostID != hostIDs[0] || call.RegistryRef != imgDigest2 || call.Version != imgVer2 {
		t.Fatalf("auto policy ensure: %+v", call)
	}
}

// TestPolicyAutoSkipsUnresolvedDigest — auto must not adopt a mutable tag when
// the registry could not be reached; the fleet stays on the last known-good
// digest.
func TestPolicyAutoSkipsUnresolvedDigest(t *testing.T) {
	env, _ := newActionsEnv(t, "host-a")
	seedCatalogDigest(t, env.pool, imgVer, imgDigest)
	if code, _ := env.do(t, http.MethodPost, "/v1/admin/images/"+imgID+"/install", `{"lazy":true}`); code != http.StatusCreated {
		t.Fatal("install failed")
	}
	setPolicy(t, env.pool, "auto")

	seedCatalogDigest(t, env.pool, imgVer2, "")
	env.store.applyUpdatePolicy(context.Background())

	if v, ref, _, _ := adoptedRef(t, env.pool); v != imgVer || ref != imgDigest {
		t.Fatalf("auto policy adopted an unresolved version: got (%q,%q)", v, ref)
	}
}

// --- persisted sync state -----------------------------------------------------

// TestSyncStateSurvivesANewStore — the invariant-#5 cleanup: sync_error and
// fetched_at are read back by a DIFFERENT Store instance, which is what a
// control-plane restart is.
func TestSyncStateSurvivesANewStore(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()

	failing := NewStoreWithFetcher(pool, failingFetcher{err: errAlwaysFails})
	failing.SetLogger(testLog())
	env, err := failing.Sync(ctx)
	mustT(t, err)
	if env.SyncError == nil {
		t.Fatal("sync_error after a failed sync: got nil")
	}
	stored := *env.SyncError

	// A brand-new Store — no shared memory with the one that synced.
	fresh := NewStoreWithFetcher(pool, fixtureFetcher{data: readFixture(t)})
	fresh.SetLogger(testLog())
	env2, err := fresh.Envelope(ctx)
	mustT(t, err)
	if env2.SyncError == nil || *env2.SyncError != stored {
		t.Fatalf("sync_error after 'restart': got %v want %q (it must come from instance_settings, not process memory)", env2.SyncError, stored)
	}
	if env2.FetchedAt != nil {
		t.Fatalf("fetched_at with no successful sync yet: got %v want nil", env2.FetchedAt)
	}

	// A successful sync clears the error and stamps the timestamp — again read
	// back by a third Store.
	if _, err := fresh.Sync(ctx); err != nil {
		t.Fatalf("successful sync: %v", err)
	}
	third := NewStoreWithFetcher(pool, failingFetcher{err: errAlwaysFails})
	third.SetLogger(testLog())
	env3, err := third.Envelope(ctx)
	mustT(t, err)
	if env3.SyncError != nil {
		t.Fatalf("sync_error after a successful sync: got %q want nil", *env3.SyncError)
	}
	if env3.FetchedAt == nil {
		t.Fatal("fetched_at after a successful sync: got nil")
	}

	// And a LATER failure keeps the previous fetched_at: it names when the
	// catalog being served was fetched, which the failed attempt did not change.
	if _, err := third.Sync(ctx); err != nil {
		t.Fatalf("failing sync: %v", err)
	}
	env4, err := third.Envelope(ctx)
	mustT(t, err)
	if env4.SyncError == nil {
		t.Fatal("sync_error after a later failure: got nil")
	}
	if env4.FetchedAt == nil || !env4.FetchedAt.Equal(*env3.FetchedAt) {
		t.Fatalf("fetched_at after a later failure: got %v want the earlier %v", env4.FetchedAt, env3.FetchedAt)
	}
}

// TestSyncResolvesDigestsIntoTheCatalog — the resolver is wired into Sync: the
// fixture's entry comes back with a registry_digest on the envelope.
func TestSyncResolvesDigestsIntoTheCatalog(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	reg := newFakeRegistry(t)

	s := NewStoreWithFetcher(pool, fixtureFetcher{data: readFixture(t)})
	s.SetLogger(testLog())
	s.SetResolver(reg.resolver())

	env, err := s.Sync(ctx)
	mustT(t, err)
	if len(env.Images) != 1 {
		t.Fatalf("images: %+v", env.Images)
	}
	img := env.Images[0]
	if img.RegistryDigest == nil {
		t.Fatal("registry_digest after a sync with a working registry: got nil")
	}
	if !strings.Contains(*img.RegistryDigest, "@sha256:") {
		t.Fatalf("registry_digest: got %q, want the digest form", *img.RegistryDigest)
	}
}

// TestSyncDigestFailureNeverFailsTheSync — the contract's hard rule: a registry
// that will not answer leaves an empty digest and a SUCCESSFUL sync.
func TestSyncDigestFailureNeverFailsTheSync(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	reg := newFakeRegistry(t)
	reg.notFound = true

	s := NewStoreWithFetcher(pool, fixtureFetcher{data: readFixture(t)})
	s.SetLogger(testLog())
	s.SetResolver(reg.resolver())

	env, err := s.Sync(ctx)
	mustT(t, err)
	if env.SyncError != nil {
		t.Fatalf("sync_error: got %q, want nil (digest resolution failure must NEVER fail a sync)", *env.SyncError)
	}
	if len(env.Images) != 1 {
		t.Fatalf("images: %+v", env.Images)
	}
	if env.Images[0].RegistryDigest != nil {
		t.Fatalf("registry_digest: got %v want nil", env.Images[0].RegistryDigest)
	}
}

// templateManifest is a one-entry kind=template manifest — the fixture the P4
// context-resolution sync tests build against (the real fixture has no
// template entry).
const templateManifest = `{
	"manifest_version": 1,
	"images": [
		{"id":"xfce-desktop","display_name":"XFCE Desktop","kind":"template","version":"2026.08.08","dockerfile":"xfce-desktop/Dockerfile"}
	]
}`

// TestSyncResolvesTemplateContextIntoTheCatalog — the P4 analogue of
// TestSyncResolvesDigestsIntoTheCatalog: the context resolver is wired into
// Sync, and a kind=template entry comes back with a resolved context_sha.
func TestSyncResolvesTemplateContextIntoTheCatalog(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	gh := newFakeGitHub(t)

	s := NewStoreWithFetcher(pool, fixtureFetcher{data: []byte(templateManifest)})
	s.SetLogger(testLog())
	s.SetContextResolver(gh.resolver())

	env, err := s.Sync(ctx)
	mustT(t, err)
	if len(env.Images) != 1 {
		t.Fatalf("images: %+v", env.Images)
	}
	img := env.Images[0]
	if img.ContextSHA == nil || *img.ContextSHA != testSHA {
		t.Fatalf("context_sha: got %v want %q", img.ContextSHA, testSHA)
	}
	if img.RegistryDigest != nil {
		t.Fatalf("registry_digest for a template entry: got %v want nil", img.RegistryDigest)
	}
}

// TestSyncTemplateContextFailureNeverFailsTheSync — the contract's hard rule,
// P4 side: a GitHub API that will not answer leaves an empty context_sha and
// a SUCCESSFUL sync.
func TestSyncTemplateContextFailureNeverFailsTheSync(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	gh := newFakeGitHub(t)
	gh.notFound = true

	s := NewStoreWithFetcher(pool, fixtureFetcher{data: []byte(templateManifest)})
	s.SetLogger(testLog())
	s.SetContextResolver(gh.resolver())

	env, err := s.Sync(ctx)
	mustT(t, err)
	if env.SyncError != nil {
		t.Fatalf("sync_error: got %q, want nil (context sha resolution failure must NEVER fail a sync)", *env.SyncError)
	}
	if len(env.Images) != 1 {
		t.Fatalf("images: %+v", env.Images)
	}
	if env.Images[0].ContextSHA != nil {
		t.Fatalf("context_sha: got %v want nil", env.Images[0].ContextSHA)
	}
}

// refRecordingFetcher records every ref it is asked to fetch — the seam the
// resolve-then-fetch test uses to prove the manifest is fetched AT the resolved
// commit sha, not the mutable ref.
type refRecordingFetcher struct {
	data []byte
	refs []string
}

func (f *refRecordingFetcher) Fetch(_ context.Context, ref string) ([]byte, error) {
	f.refs = append(f.refs, ref)
	return f.data, nil
}

// TestSyncResolvesRefBeforeFetch — P4 fix #2 (resolve-then-fetch): the catalog
// ref is resolved to a commit sha FIRST and the manifest is fetched AT that
// sha, so the manifest bytes and every template's context_sha come from ONE
// commit. Fetching from the mutable ref and resolving it separately let a
// branch advancing in between pair a manifest from commit A with a context_sha
// from commit B.
func TestSyncResolvesRefBeforeFetch(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	gh := newFakeGitHub(t) // resolves any ref to testSHA

	fetch := &refRecordingFetcher{data: []byte(templateManifest)}
	s := NewStoreWithFetcher(pool, fetch)
	s.SetLogger(testLog())
	s.SetContextResolver(gh.resolver())

	env, err := s.Sync(ctx)
	mustT(t, err)

	if len(fetch.refs) != 1 || fetch.refs[0] != testSHA {
		t.Fatalf("fetch refs: got %v want exactly [%s] (the manifest must be fetched AT the resolved sha, not the mutable ref)", fetch.refs, testSHA)
	}
	if len(env.Images) != 1 || env.Images[0].ContextSHA == nil || *env.Images[0].ContextSHA != testSHA {
		t.Fatalf("context_sha: got %+v want %q (the SAME commit the manifest was fetched at)", env.Images, testSHA)
	}
}

// TestBuildUsesFrozenInputsNotMovedCatalog — P4 CRITICAL #1 (the fleet-split
// twin): after a template is adopted, a later catalog sync that moves
// context_sha / dockerfile / build_args must NOT change what an already-adopted
// template builds. Every build input is frozen into installed_images at
// adoption; dispatch reads the frozen row, never the live catalog. Getting this
// wrong rebuilds DIFFERENT bits under the adopted version (or blanks
// context_sha and makes the template un-dispatchable) — silently splitting the
// fleet.
func TestBuildUsesFrozenInputsNotMovedCatalog(t *testing.T) {
	env, _ := newActionsEnv(t, "host-a")
	seedCatalogTemplate(t, env.pool, tplVer, tplContextSHA) // dockerfile='Dockerfile', build_args='{}'

	if code, body := env.do(t, http.MethodPost, "/v1/admin/images/"+tplID+"/install", `{}`); code != http.StatusCreated {
		t.Fatalf("install: status %d body %s", code, body)
	}
	wantFrozenURL := "https://codeload.github.com/" + ConfiguredCatalogRepo() + "/tar.gz/" + tplContextSHA
	first := env.fleet.waitBuild(t)
	if first.ContextURL != wantFrozenURL {
		t.Fatalf("install build context_url: got %q want %q", first.ContextURL, wantFrozenURL)
	}

	// A later sync moves ALL of the catalog's build-defining inputs WITHOUT
	// touching the adoption row — exactly what Store.upsert does (it only ever
	// writes image_catalog).
	const movedSHA = "0000000000000000000000000000000000000000"
	if _, err := env.pool.Exec(context.Background(), `
		UPDATE image_catalog
		   SET context_sha = $2, dockerfile = 'evil/Dockerfile', build_args = '{"BASE":"evil"}'::jsonb
		 WHERE id = $1
	`, tplID, movedSHA); err != nil {
		t.Fatalf("simulate catalog sync moving build inputs: %v", err)
	}

	// Re-ensure (a host reconnect, another install trigger, an auto-policy pass):
	// the dispatch must still carry the FROZEN inputs, not the moved catalog ones.
	env.ens.EnsureImage(context.Background(), tplID)
	second := env.fleet.waitBuild(t)
	if second.ContextURL != wantFrozenURL {
		t.Fatalf("re-ensure build context_url: got %q want the FROZEN %q (catalog moved context_sha to %q)", second.ContextURL, wantFrozenURL, movedSHA)
	}
	if second.ContextSubdir != "." || second.Dockerfile != "Dockerfile" {
		t.Fatalf("re-ensure dockerfile: subdir=%q dockerfile=%q, want the FROZEN '.'/'Dockerfile' (catalog moved it to evil/Dockerfile)", second.ContextSubdir, second.Dockerfile)
	}
	if second.BuildArgs["BASE"] == "evil" {
		t.Fatalf("re-ensure build_args carried the MOVED catalog value: %+v (must be the frozen '{}')", second.BuildArgs)
	}
}
