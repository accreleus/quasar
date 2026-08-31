// provenance_db_test.go — #548 manifest provenance. TEST_DATABASE_URL-gated
// like every other DB test here (testDB in store_db_test.go); make test-db is
// the sanctioned way to actually run them.
//
// The manifest is fetched unauthenticated at a MUTABLE ref, so a force-push
// silently changes what every host installs. The operator decision is
// VISIBILITY, not signing: these tests pin the three states an operator has to
// be able to tell apart — first sync, an unchanged resync, and a swap.
package images

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"
)

// mutableFetcher serves whatever bytes it currently holds, so one test can sync
// the same store twice with different manifests.
type mutableFetcher struct{ data []byte }

func (f *mutableFetcher) Fetch(_ context.Context, _ string) ([]byte, error) { return f.data, nil }

// changedManifest returns a valid manifest whose bytes differ from the
// fixture's — a swapped catalog, which is what a force-push to `stable` looks
// like from the control plane's side.
func changedManifest(t *testing.T) []byte {
	t.Helper()
	original := readFixture(t)
	swapped := bytes.Replace(original,
		[]byte(`"version": "sha-4afbf76"`), []byte(`"version": "sha-deadbee"`), 1)
	if bytes.Equal(swapped, original) {
		t.Fatal("changedManifest: the fixture no longer contains the version string to swap")
	}
	return swapped
}

func sha256Hex(b []byte) string { return fmt.Sprintf("%x", sha256.Sum256(b)) }

// TestProvenanceNilBeforeAnySync — an instance that has never synced reports no
// provenance at all (wire null), not an empty digest that would read as "the
// manifest is empty" rather than "nothing has been fetched".
func TestProvenanceNilBeforeAnySync(t *testing.T) {
	pool := testDB(t)
	env, err := NewStoreWithFetcher(pool, fixtureFetcher{data: readFixture(t)}).Envelope(context.Background())
	if err != nil {
		t.Fatalf("envelope: %v", err)
	}
	if env.ManifestProvenance != nil {
		t.Fatalf("manifest_provenance before any sync: got %+v want nil", env.ManifestProvenance)
	}
}

// TestProvenanceFirstSyncRecordsDigestAndIsNotAChange — the first sync records
// the digest/ref/url, and is explicitly NOT reported as a change: there is
// nothing it could have changed from, and flagging it would cry wolf on every
// fresh instance.
func TestProvenanceFirstSyncRecordsDigestAndIsNotAChange(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	data := readFixture(t)
	s := NewStoreWithFetcher(pool, fixtureFetcher{data: data})

	env, err := s.Sync(ctx)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	p := env.ManifestProvenance
	if p == nil {
		t.Fatal("manifest_provenance: got nil, want it recorded after a successful sync")
	}
	if p.SHA256 != sha256Hex(data) {
		t.Fatalf("sha256: got %q want %q", p.SHA256, sha256Hex(data))
	}
	if p.Changed {
		t.Fatal("changed: got true on the FIRST sync; there is no previous digest to differ from")
	}
	if p.PreviousSHA256 != nil {
		t.Fatalf("previous_sha256: got %q want nil on the first sync", *p.PreviousSHA256)
	}
	if p.ChangedAt == nil {
		t.Fatal("changed_at: got nil, want the time this digest was first observed")
	}
	if p.Ref != "stable" {
		t.Fatalf("ref: got %q want stable (the migration 0054 default)", p.Ref)
	}
	// The URL must name the actual fetch target, so an operator can go look at
	// what was served without reconstructing it from env vars.
	if p.URL != "https://raw.githubusercontent.com/accreleus/quasar-images/stable/quasar-manifest.json" {
		t.Fatalf("url: got %q", p.URL)
	}
	// The test store's context resolver is the noop one, so the commit sha is
	// legitimately unresolved — a supported state, never a sync failure.
	if p.CommitSHA != nil {
		t.Fatalf("commit_sha: got %q want nil with the noop context resolver", *p.CommitSHA)
	}

	// It must survive a restart (a fresh Store over the same database), which is
	// the whole reason it lives in instance_settings rather than in memory.
	env2, err := NewStoreWithFetcher(pool, fixtureFetcher{data: data}).Envelope(ctx)
	if err != nil {
		t.Fatalf("envelope after restart: %v", err)
	}
	if env2.ManifestProvenance == nil || env2.ManifestProvenance.SHA256 != p.SHA256 {
		t.Fatalf("provenance did not survive a restart: got %+v", env2.ManifestProvenance)
	}
}

// TestProvenanceUnchangedResyncIsNotAChange — re-syncing the same bytes must
// not raise the change flag or move changed_at. A sync that reports a change
// every time reports nothing.
func TestProvenanceUnchangedResyncIsNotAChange(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	data := readFixture(t)
	s := NewStoreWithFetcher(pool, fixtureFetcher{data: data})

	first, err := s.Sync(ctx)
	if err != nil {
		t.Fatalf("first sync: %v", err)
	}
	second, err := s.Sync(ctx)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	p := second.ManifestProvenance
	if p == nil {
		t.Fatal("manifest_provenance: got nil after a resync")
	}
	if p.Changed {
		t.Fatal("changed: got true resyncing identical manifest bytes")
	}
	if p.PreviousSHA256 != nil {
		t.Fatalf("previous_sha256: got %q want nil (nothing has ever changed)", *p.PreviousSHA256)
	}
	if p.SHA256 != sha256Hex(data) {
		t.Fatalf("sha256: got %q want %q", p.SHA256, sha256Hex(data))
	}
	// changed_at names when THIS digest was first observed, so an unchanged
	// resync must not move it forward — otherwise "unchanged since" always
	// reads as "just now".
	if !p.ChangedAt.Equal(*first.ManifestProvenance.ChangedAt) {
		t.Fatalf("changed_at moved on an unchanged resync: %v -> %v",
			first.ManifestProvenance.ChangedAt, p.ChangedAt)
	}
}

// TestProvenanceChangedDigestIsFlaggedAndKeepsPrevious — the acceptance case
// for #548: the manifest at the same ref is swapped, and the sync that picks it
// up flags the change and keeps the digest it replaced.
func TestProvenanceChangedDigestIsFlaggedAndKeepsPrevious(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	original, swapped := readFixture(t), changedManifest(t)
	f := &mutableFetcher{data: original}
	s := NewStoreWithFetcher(pool, f)

	first, err := s.Sync(ctx)
	if err != nil {
		t.Fatalf("first sync: %v", err)
	}

	// The force-push.
	f.data = swapped
	env, err := s.Sync(ctx)
	if err != nil {
		t.Fatalf("sync after the swap: %v", err)
	}
	p := env.ManifestProvenance
	if p == nil {
		t.Fatal("manifest_provenance: got nil after the swap")
	}
	if !p.Changed {
		t.Fatal("changed: got false after the manifest bytes at the same ref were swapped — the whole point of #548")
	}
	if p.SHA256 != sha256Hex(swapped) {
		t.Fatalf("sha256: got %q want the swapped manifest's %q", p.SHA256, sha256Hex(swapped))
	}
	if p.PreviousSHA256 == nil || *p.PreviousSHA256 != sha256Hex(original) {
		t.Fatalf("previous_sha256: got %v want %q", p.PreviousSHA256, sha256Hex(original))
	}
	if p.ChangedAt == nil || !p.ChangedAt.After(*first.ManifestProvenance.ChangedAt) {
		t.Fatalf("changed_at: got %v, want a time after the first sync's %v",
			p.ChangedAt, first.ManifestProvenance.ChangedAt)
	}

	// A later unchanged sync clears the acute flag but keeps the durable
	// record, so the swap stays auditable after the operator has seen it.
	after, err := s.Sync(ctx)
	if err != nil {
		t.Fatalf("third sync: %v", err)
	}
	q := after.ManifestProvenance
	if q.Changed {
		t.Fatal("changed: still true after an unchanged sync; the flag is about the LAST sync")
	}
	if q.PreviousSHA256 == nil || *q.PreviousSHA256 != sha256Hex(original) {
		t.Fatalf("previous_sha256 lost once the flag cleared: got %v", q.PreviousSHA256)
	}
	if !q.ChangedAt.Equal(*p.ChangedAt) {
		t.Fatalf("changed_at moved on an unchanged sync: %v -> %v", p.ChangedAt, q.ChangedAt)
	}
}

// TestProvenanceSurvivesAFailedSync — a failed sync must leave the provenance
// describing the catalog that is still being SERVED. Overwriting it would make
// the admin page claim an origin for rows that came from somewhere else.
func TestProvenanceSurvivesAFailedSync(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	data := readFixture(t)
	if _, err := NewStoreWithFetcher(pool, fixtureFetcher{data: data}).Sync(ctx); err != nil {
		t.Fatalf("seed sync: %v", err)
	}

	failing := NewStoreWithFetcher(pool, failingFetcher{err: errors.New("github: connection refused")})
	env, err := failing.Sync(ctx)
	if err != nil {
		t.Fatalf("failed sync returned an error: %v", err)
	}
	if env.SyncError == nil {
		t.Fatal("sync_error: got nil, want the fetch failure recorded")
	}
	p := env.ManifestProvenance
	if p == nil || p.SHA256 != sha256Hex(data) {
		t.Fatalf("provenance after a failed sync: got %+v want the served catalog's digest %q", p, sha256Hex(data))
	}
	if p.Changed {
		t.Fatal("changed: got true after a sync that never replaced the catalog")
	}
}
