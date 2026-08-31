// store_db_test.go — TEST_DATABASE_URL-gated like every other DB test in this
// repo: without it these SKIP (see internal/settings/settings_test.go's
// testDB for the identical pattern). make test-db / scripts/dev/dev.sh go-test-db
// provision the database that makes them actually run.
package images

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/migrate"
	"github.com/accreleus/quasar/control-plane/migrations"
)

func testDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping DB integration test")
	}
	if err := migrate.Run(migrations.FS, dbURL); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE image_catalog, instance_settings, users CASCADE`); err != nil {
		pool.Close()
		t.Fatalf("truncate: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// fixtureFetcher serves the real manifest fixture, so the DB tests exercise
// Store.Sync's full path (fetch → validate → upsert) without ever making a
// live network call.
type fixtureFetcher struct{ data []byte }

func (f fixtureFetcher) Fetch(_ context.Context, _ string) ([]byte, error) {
	return f.data, nil
}

type failingFetcher struct{ err error }

func (f failingFetcher) Fetch(_ context.Context, _ string) ([]byte, error) {
	return nil, f.err
}

// TestCatalogRefDefaultsMainWhenUnseeded — the column default an unseeded
// instance must read (migration 0054).
func TestCatalogRefDefaultsMainWhenUnseeded(t *testing.T) {
	pool := testDB(t)
	s := NewStore(pool)
	ref, err := s.CatalogRef(context.Background())
	if err != nil {
		t.Fatalf("catalog ref: %v", err)
	}
	if ref != "stable" {
		t.Fatalf("unseeded catalog ref: got %q want main", ref)
	}
}

// TestSyncFromFixturePopulatesCatalog — acceptance: syncing from the fixture
// populates image_catalog, and GET (via Envelope) returns the steam entry
// with kind/version/registry_ref intact and hosts=[] (P1 has no host_images
// source yet).
func TestSyncFromFixturePopulatesCatalog(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	data := readFixture(t)
	s := NewStoreWithFetcher(pool, fixtureFetcher{data: data})

	env, err := s.Sync(ctx)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if env.SyncError != nil {
		t.Fatalf("sync_error: got %q want nil", *env.SyncError)
	}
	if len(env.Images) != 1 {
		t.Fatalf("images: got %d want 1", len(env.Images))
	}
	steam := env.Images[0]
	if steam.ID != "steam" {
		t.Fatalf("id: got %q want steam", steam.ID)
	}
	if steam.Kind != "prebuilt" {
		t.Fatalf("kind: got %q want prebuilt", steam.Kind)
	}
	if steam.Version != "sha-4afbf76" {
		t.Fatalf("version: got %q want sha-4afbf76", steam.Version)
	}
	if steam.RegistryRef == nil || *steam.RegistryRef != "ghcr.io/accreleus/quasar-steam:sha-4afbf76" {
		t.Fatalf("registry_ref: got %v", steam.RegistryRef)
	}
	if steam.Hosts == nil || len(steam.Hosts) != 0 {
		t.Fatalf("hosts: got %v want empty non-nil slice", steam.Hosts)
	}
	if steam.Installed {
		t.Fatal("installed: got true want false (no install path in P1)")
	}

	// GET (Envelope) independently must return the same picture — this is
	// the surface GET /v1/admin/images serves.
	env2, err := s.Envelope(ctx)
	if err != nil {
		t.Fatalf("envelope: %v", err)
	}
	if len(env2.Images) != 1 || env2.Images[0].ID != "steam" {
		t.Fatalf("envelope after sync: got %+v", env2.Images)
	}
	if env2.FetchedAt == nil {
		t.Fatal("fetched_at: got nil, want populated after a successful sync")
	}
}

// TestSyncWithFailingFetcherServesCachedCatalog — acceptance: a sync that
// cannot fetch (or whose manifest fails validation) must return the LAST
// cached catalog with sync_error set, and crucially the HTTP posture is 200
// not 500 — asserted at the store level here (the handler layer just wraps
// this in WriteJSON 200 unconditionally when err == nil).
func TestSyncWithFailingFetcherServesCachedCatalog(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()

	// Seed a good catalog first via a successful sync.
	good := NewStoreWithFetcher(pool, fixtureFetcher{data: readFixture(t)})
	if _, err := good.Sync(ctx); err != nil {
		t.Fatalf("seed sync: %v", err)
	}

	// Now sync again with a fetcher that always fails — same store instance
	// so the in-memory sync_error is observable, and the same table so the
	// previously-synced row is still there to be served.
	failing := NewStoreWithFetcher(pool, failingFetcher{err: errors.New("github: connection refused")})
	env, err := failing.Sync(ctx)
	if err != nil {
		// A failing fetch must never surface as a Go error to the HTTP layer
		// (that would become a 500) — Sync itself must swallow it into
		// sync_error and still return the cached envelope.
		t.Fatalf("sync with failing fetcher returned an error (would 500 the request): %v", err)
	}
	if env.SyncError == nil {
		t.Fatal("sync_error: got nil, want the fetch failure recorded")
	}
	if len(env.Images) != 1 || env.Images[0].ID != "steam" {
		t.Fatalf("cached catalog not served after failed sync: got %+v", env.Images)
	}
}

// TestSyncWithInvalidManifestServesCachedCatalog — same posture, but the
// failure is a validation refusal (unknown manifest_version) rather than a
// transport error.
//
// The refused manifest carries TWO valid-looking entries (not an empty
// images array) specifically so this test can prove REFUSAL rather than
// "applied zero rows": if Validate's early return were ever bypassed, this
// manifest would upsert "other-image" into the catalog, which the assertion
// below checks for and fails on.
func TestSyncWithInvalidManifestServesCachedCatalog(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()

	good := NewStoreWithFetcher(pool, fixtureFetcher{data: readFixture(t)})
	if _, err := good.Sync(ctx); err != nil {
		t.Fatalf("seed sync: %v", err)
	}

	badManifest := `{
		"manifest_version": 2,
		"images": [
			{"id":"steam","display_name":"Steam v2","kind":"prebuilt","version":"9.9.9","registry_ref":"ghcr.io/x/steam:9.9.9"},
			{"id":"other-image","display_name":"Other","kind":"prebuilt","version":"1.0.0","registry_ref":"ghcr.io/x/other:1.0.0"}
		]
	}`
	bad := NewStoreWithFetcher(pool, fixtureFetcher{data: []byte(badManifest)})
	env, err := bad.Sync(ctx)
	if err != nil {
		t.Fatalf("sync with invalid manifest returned an error (would 500 the request): %v", err)
	}
	if env.SyncError == nil {
		t.Fatal("sync_error: got nil, want the validation refusal recorded")
	}
	// The catalog must be EXACTLY what the good sync left it as: still one
	// entry, still steam's ORIGINAL version — not "Steam v2" / 9.9.9 from
	// the refused manifest, and no "other-image" row at all.
	if len(env.Images) != 1 || env.Images[0].ID != "steam" {
		t.Fatalf("cached catalog not served after invalid manifest: got %+v", env.Images)
	}
	if env.Images[0].Version != "sha-4afbf76" {
		t.Fatalf("catalog was partially applied: steam version = %q, want the ORIGINAL sha-4afbf76 (refused manifest must change nothing)", env.Images[0].Version)
	}
	for _, img := range env.Images {
		if img.ID == "other-image" {
			t.Fatal("refused manifest was partially applied: other-image is present in the catalog")
		}
	}
}

// TestSyncReconcilesWithdrawnImages — MINOR 3: a successful sync must
// remove a catalog row whose id the manifest no longer lists ("cached
// upstream offer" — not "everything ever seen").
func TestSyncReconcilesWithdrawnImages(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()

	twoImages := `{
		"manifest_version": 1,
		"images": [
			{"id":"steam","display_name":"Steam","kind":"prebuilt","version":"1.0.0","registry_ref":"ghcr.io/x/steam:1.0.0"},
			{"id":"withdrawn","display_name":"Withdrawn App","kind":"prebuilt","version":"1.0.0","registry_ref":"ghcr.io/x/withdrawn:1.0.0"}
		]
	}`
	s := NewStoreWithFetcher(pool, fixtureFetcher{data: []byte(twoImages)})
	env, err := s.Sync(ctx)
	if err != nil {
		t.Fatalf("seed sync: %v", err)
	}
	if len(env.Images) != 2 {
		t.Fatalf("seed images: got %d want 2", len(env.Images))
	}

	// The next manifest at the SAME ref no longer lists "withdrawn".
	oneImage := `{
		"manifest_version": 1,
		"images": [
			{"id":"steam","display_name":"Steam","kind":"prebuilt","version":"1.0.1","registry_ref":"ghcr.io/x/steam:1.0.1"}
		]
	}`
	s2 := NewStoreWithFetcher(pool, fixtureFetcher{data: []byte(oneImage)})
	env2, err := s2.Sync(ctx)
	if err != nil {
		t.Fatalf("reconciling sync: %v", err)
	}
	if env2.SyncError != nil {
		t.Fatalf("sync_error: got %q want nil", *env2.SyncError)
	}
	if len(env2.Images) != 1 {
		t.Fatalf("images after reconciling sync: got %d want 1 (withdrawn image must be gone): %+v", len(env2.Images), env2.Images)
	}
	if env2.Images[0].ID != "steam" {
		t.Fatalf("surviving image: got %q want steam", env2.Images[0].ID)
	}
}

// TestFailedSyncNeverDeletes — MINOR 3's other half: a sync that fails
// (fetch error or validation refusal) must not touch the row count at all,
// not even to reconcile. Deletion only ever happens inside upsert, which
// Sync's early-return paths never reach — this pins that at the
// integration level, on top of the unit-level "same transaction" comment.
func TestFailedSyncNeverDeletes(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()

	twoImages := `{
		"manifest_version": 1,
		"images": [
			{"id":"steam","display_name":"Steam","kind":"prebuilt","version":"1.0.0","registry_ref":"ghcr.io/x/steam:1.0.0"},
			{"id":"other","display_name":"Other","kind":"prebuilt","version":"1.0.0","registry_ref":"ghcr.io/x/other:1.0.0"}
		]
	}`
	s := NewStoreWithFetcher(pool, fixtureFetcher{data: []byte(twoImages)})
	if _, err := s.Sync(ctx); err != nil {
		t.Fatalf("seed sync: %v", err)
	}

	// A fetch failure.
	failing := NewStoreWithFetcher(pool, failingFetcher{err: errors.New("connection refused")})
	env, err := failing.Sync(ctx)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if env.SyncError == nil || len(env.Images) != 2 {
		t.Fatalf("fetch failure must not delete anything: sync_error=%v images=%d want 2", env.SyncError, len(env.Images))
	}

	// A validation refusal (only one of the two ids present — if this
	// deleted anything, "other" would vanish here).
	oneImageBadVersion := `{"manifest_version":2,"images":[{"id":"steam","display_name":"Steam","kind":"prebuilt","version":"2.0.0","registry_ref":"ghcr.io/x/steam:2.0.0"}]}`
	bad := NewStoreWithFetcher(pool, fixtureFetcher{data: []byte(oneImageBadVersion)})
	env2, err := bad.Sync(ctx)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if env2.SyncError == nil || len(env2.Images) != 2 {
		t.Fatalf("validation refusal must not delete anything: sync_error=%v images=%d want 2", env2.SyncError, len(env2.Images))
	}
}

// TestSyncPreservesUnknownFieldsInRaw — MAJOR 1: a manifest field this
// build's ManifestImage struct does not declare ("notes", which the real
// fixture carries) must survive a sync round-trip into image_catalog.raw
// verbatim, matching migration 0054's documented purpose for the column.
func TestSyncPreservesUnknownFieldsInRaw(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	s := NewStoreWithFetcher(pool, fixtureFetcher{data: readFixture(t)})

	if _, err := s.Sync(ctx); err != nil {
		t.Fatalf("sync: %v", err)
	}

	var raw []byte
	if err := pool.QueryRow(ctx, `SELECT raw FROM image_catalog WHERE id = 'steam'`).Scan(&raw); err != nil {
		t.Fatalf("read raw column: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode raw column: %v", err)
	}
	notes, ok := decoded["notes"].(string)
	if !ok || notes == "" {
		t.Fatalf("raw column lost the \"notes\" field the fixture carries (a field ManifestImage does not declare): decoded=%+v", decoded)
	}
	if want := "no_new_privileges MUST be false"; len(notes) < len(want) || notes[:len(want)] != want {
		t.Fatalf("notes content mismatch: got %q", notes)
	}
}

// TestSyncUpsertIsIdempotent — syncing the same fixture twice must not
// duplicate rows (ON CONFLICT (id) DO UPDATE), and the second sync clears
// any previously-recorded sync_error.
func TestSyncUpsertIsIdempotent(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	s := NewStoreWithFetcher(pool, fixtureFetcher{data: readFixture(t)})

	if _, err := s.Sync(ctx); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	env, err := s.Sync(ctx)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if len(env.Images) != 1 {
		t.Fatalf("images after two syncs: got %d want 1 (no duplication)", len(env.Images))
	}
	if env.SyncError != nil {
		t.Fatalf("sync_error after a clean sync: got %q want nil", *env.SyncError)
	}
}
