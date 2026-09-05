package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/migrate"
	"github.com/accreleus/quasar/control-plane/migrations"
)

// TEST_DATABASE_URL-gated, like every other DB-backed suite here: `make test-db`
// provisions it. -p 1 is mandatory — these truncate.
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
	if _, err := pool.Exec(ctx, `TRUNCATE platform_releases, hosts, instance_settings, users CASCADE`); err != nil {
		pool.Close()
		t.Fatalf("truncate: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// fakeSource stands in for GitHub. listErr fails the whole pass; a listing whose
// manifest is missing from manifests fails only that release.
type fakeSource struct {
	listings  []Listing
	manifests map[string]string
	listErr   error
	listCalls int
}

func (f *fakeSource) List(context.Context) ([]Listing, error) {
	f.listCalls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.listings, nil
}

func (f *fakeSource) FetchManifest(_ context.Context, url string) ([]byte, error) {
	raw, ok := f.manifests[url]
	if !ok {
		return nil, fmt.Errorf("no asset at %s", url)
	}
	return []byte(raw), nil
}

func (f *fakeSource) CompareURL(from, to string) string {
	return "https://github.com/accreleus/quasar/compare/" + from + "..." + to
}

func manifestFor(version, commit string, schema int, built string) string {
	return fmt.Sprintf(`{
	  "format_version": 1, "version": %q, "prerelease": false,
	  "source_commit": %q, "built_at": %q, "schema_version": %d,
	  "components": [
	    { "name": "control-plane", "image": "ghcr.io/accreleus/quasar/quasar-control-plane", "digest": "sha256:%s" },
	    { "name": "node-agent",    "image": "ghcr.io/accreleus/quasar/quasar-node-agent",    "digest": "sha256:%s" }
	  ]
	}`, version, commit, built, schema, hex64, hex64)
}

func twoReleaseSource() *fakeSource {
	return &fakeSource{
		listings: []Listing{
			{Tag: "v0.2.0", Version: "0.2.0", Body: "notes for 0.2.0", ManifestURL: "u/0.2.0"},
			{Tag: "v0.3.0", Version: "0.3.0", Body: "notes for 0.3.0", ManifestURL: "u/0.3.0"},
		},
		manifests: map[string]string{
			"u/0.2.0": manifestFor("0.2.0", commitA, 73, "2026-09-01T12:00:00Z"),
			"u/0.3.0": manifestFor("0.3.0", commitB, 74, "2026-09-04T12:00:00Z"),
		},
	}
}

func TestDetectRecordsNewReleasesAndIsIdempotent(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	store := NewStore(pool)
	src := twoReleaseSource()
	det := NewDetector(src, store, nil)

	rep, err := det.Detect(ctx)
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if rep.Seen != 2 || rep.New != 2 || rep.ManifestInvalid != 0 {
		t.Fatalf("report = %+v, want 2 seen / 2 new", rep)
	}

	rows, err := store.Releases(ctx, ChannelStable)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("stored %d rows, want 2", len(rows))
	}
	byVersion := map[string]Release{}
	for _, r := range rows {
		byVersion[*r.Version] = r
	}
	got := byVersion["0.3.0"]
	if got.SourceCommit != commitB || got.SchemaVersion != 74 {
		t.Errorf("identity came from the tag, not the manifest: %+v", got)
	}
	if got.Notes != "notes for 0.3.0" {
		t.Errorf("notes = %q, want the release body verbatim", got.Notes)
	}
	if len(got.Manifest) == 0 {
		t.Error("the manifest must be stored verbatim, not dropped")
	}
	var manifest map[string]any
	if err := json.Unmarshal(got.Manifest, &manifest); err != nil {
		t.Errorf("stored manifest is not the asset: %v", err)
	}
	// compare_url links a release to the one below it in the ordering; the
	// oldest release has nothing to compare from.
	if got.CompareURL == nil || *got.CompareURL != src.CompareURL(commitA, commitB) {
		t.Errorf("compare_url = %v, want the diff from 0.2.0", got.CompareURL)
	}
	if byVersion["0.2.0"].CompareURL != nil {
		t.Errorf("the oldest release has nothing to compare from, got %v", *byVersion["0.2.0"].CompareURL)
	}

	// A second pass re-observes the same commits and must not accumulate
	// duplicates — nor re-download a manifest it already has.
	rep2, err := det.Detect(ctx)
	if err != nil {
		t.Fatalf("second detect: %v", err)
	}
	if rep2.New != 0 || rep2.Seen != 2 {
		t.Fatalf("second report = %+v, want 2 seen / 0 new", rep2)
	}
	rows2, _ := store.Releases(ctx, ChannelStable)
	if len(rows2) != 2 {
		t.Fatalf("second pass stored %d rows, want the same 2", len(rows2))
	}
	for _, r := range rows2 {
		if r.ID != byVersion[*r.Version].ID {
			t.Errorf("release id changed across detections: %s", *r.Version)
		}
	}
}

func TestDetectKeepsExistingRowsWhenTheSourceIsUnreachable(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	store := NewStore(pool)
	src := twoReleaseSource()

	if _, err := NewDetector(src, store, nil).Detect(ctx); err != nil {
		t.Fatalf("seed detect: %v", err)
	}

	src.listErr = errors.New("api.github.com answered 503")
	_, err := NewDetector(src, store, nil).Detect(ctx)
	if err == nil {
		t.Fatal("an unreachable source must fail the run, which is what becomes last_error")
	}
	rows, _ := store.Releases(ctx, ChannelStable)
	if len(rows) != 2 {
		t.Fatalf("a failed pass destroyed rows: %d left, want 2", len(rows))
	}
}

func TestDetectCountsAnInvalidManifestAndStillRecordsTheGoodRelease(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	store := NewStore(pool)

	src := twoReleaseSource()
	src.manifests["u/0.3.0"] = `{"format_version": 99}`

	rep, err := NewDetector(src, store, nil).Detect(ctx)
	if err != nil {
		t.Fatalf("a broken publish must not fail the pass: %v", err)
	}
	if rep.New != 1 || rep.ManifestInvalid != 1 || len(rep.Errors) != 1 {
		t.Fatalf("report = %+v, want 1 new and 1 invalid", rep)
	}
	rows, _ := store.Releases(ctx, ChannelStable)
	if len(rows) != 1 || *rows[0].Version != "0.2.0" {
		t.Fatalf("rows = %+v, want only the release whose manifest validated", rows)
	}
	if _, ok := rep.Summary()["manifest_errors"]; !ok {
		t.Error("the summary must name the broken publish")
	}
}

func TestDetectRefusesAManifestDisagreeingWithItsTag(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	store := NewStore(pool)

	src := twoReleaseSource()
	src.manifests["u/0.3.0"] = manifestFor("9.9.9", commitB, 74, "2026-09-04T12:00:00Z")

	rep, err := NewDetector(src, store, nil).Detect(ctx)
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if rep.ManifestInvalid != 1 {
		t.Fatalf("report = %+v, want the mismatched manifest refused", rep)
	}
}

func TestStoreHostsProjectsIdentityAndDerivesKnown(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	store := NewStore(pool)

	mustExec(t, pool, `INSERT INTO hosts (node_name, status, node_secret_hash, source_commit, built_at, install_mode, updater_present)
		VALUES ('gpu-known', 'online', 'x', $1, now(), 'registry', true)`, commitA)
	mustExec(t, pool, `INSERT INTO hosts (node_name, status, node_secret_hash)
		VALUES ('gpu-unknown', 'offline', 'y')`)

	hosts, err := store.Hosts(ctx)
	if err != nil {
		t.Fatalf("hosts: %v", err)
	}
	if len(hosts) != 2 {
		t.Fatalf("hosts = %d, want 2", len(hosts))
	}
	byName := map[string]HostIdentity{}
	for _, h := range hosts {
		byName[h.NodeName] = h
	}
	if !byName["gpu-known"].IdentityKnown {
		t.Error("a host reporting all four identity fields must read as known")
	}
	if byName["gpu-unknown"].IdentityKnown || byName["gpu-unknown"].SourceCommit != nil {
		t.Error("a host that never reported identity must read as unknown")
	}
}

func mustExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec: %v", err)
	}
}
