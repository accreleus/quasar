package library

// #384: a library scan resolves artwork for exactly the tiles it created, after it commits,
// without blocking the scan, and not at all when no artwork provider is configured.

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/artwork"
)

// recordingArtwork is an ArtworkResolver that records each call and, when block is set, holds
// the call until it is closed.
type recordingArtwork struct {
	mu    sync.Mutex
	calls [][]string
	block chan struct{}
	done  chan struct{}
}

func (r *recordingArtwork) ResolveApps(_ context.Context, ids []string) artwork.SweepResult {
	if r.block != nil {
		<-r.block
	}
	r.mu.Lock()
	r.calls = append(r.calls, append([]string(nil), ids...))
	r.mu.Unlock()
	if r.done != nil {
		close(r.done)
	}
	return artwork.SweepResult{ProviderConfigured: true, AppsConsidered: len(ids)}
}

func (r *recordingArtwork) snapshot() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]string(nil), r.calls...)
}

// newArtworkScanServer is newTestServer with an artwork resolver wired in. inline=true runs the
// post-commit pass inline, so a test can assert on it the moment the report is answered.
func newArtworkScanServer(t *testing.T, f fixture, art ArtworkResolver, inline bool) *httptest.Server {
	t.Helper()
	set := &fakeSettings{enabled: true}
	h := NewHandler(f.store, testStorageManager(f, set), set, NewAppDetails(false, quietLogger()),
		newTestResolver(set), quietLogger())
	h.SetArtwork(art)
	if inline {
		h.spawn = func(fn func()) { fn() }
	}
	mux := http.NewServeMux()
	h.Register(mux, func(next http.Handler) http.Handler { return next })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func (f fixture) reportScan(t *testing.T, srv *httptest.Server, entries []ReportEntry) {
	t.Helper()
	scanID := f.claimedScan(t, f.user)
	resp := agentReq(t, "POST", srv.URL+"/v1/agent/library/scan-report", f.nodeName, f.secret,
		ScanReport{ScanID: scanID, OK: true, Entries: entries})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("scan-report = %d, want 200", resp.StatusCode)
	}
}

func (f fixture) tileIDs(t *testing.T) []string {
	t.Helper()
	rows, err := f.pool.Query(context.Background(),
		`SELECT id::text FROM apps WHERE parent_app_id = $1::uuid`, f.parent)
	must(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		must(t, rows.Scan(&id))
		out = append(out, id)
	}
	must(t, rows.Err())
	sort.Strings(out)
	return out
}

// A scan that adds apps resolves each new app exactly once; a re-scan of the same (now
// known) apps resolves none.
func TestScanResolvesArtworkOncePerNewAppAndNotOnRescan(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)
	art := &recordingArtwork{}
	srv := newArtworkScanServer(t, f, art, true)

	f.reportScan(t, srv, observedEntries())
	tiles := f.tileIDs(t)
	if len(tiles) != 4 {
		t.Fatalf("fixture: want 4 published tiles, got %d", len(tiles))
	}
	calls := art.snapshot()
	if len(calls) != 1 {
		t.Fatalf("want one artwork pass for the scan, got %d", len(calls))
	}
	got := append([]string(nil), calls[0]...)
	sort.Strings(got)
	if len(got) != len(tiles) {
		t.Fatalf("want exactly the %d new apps resolved, got %v", len(tiles), got)
	}
	for i := range tiles {
		if got[i] != tiles[i] {
			t.Fatalf("resolved %v, want exactly the new tiles %v (each once)", got, tiles)
		}
	}

	// Re-scan: nothing new, so nothing to resolve.
	f.reportScan(t, srv, observedEntries())
	if n := len(art.snapshot()); n != 1 {
		t.Fatalf("a re-scan of known apps must trigger no artwork resolve; got %d passes", n)
	}

	// A later scan adding ONE game resolves only that one.
	more := append(observedEntries(), ReportEntry{ExternalID: "620", Name: "Portal 2"})
	f.reportScan(t, srv, more)
	calls = art.snapshot()
	if len(calls) != 2 || len(calls[1]) != 1 {
		t.Fatalf("want a second pass with exactly the one new app, got %v", calls)
	}
	if id, _, ok := f.tile(t, "620"); !ok || calls[1][0] != id {
		t.Fatalf("second pass resolved %v, want the new Portal 2 tile %q", calls[1], id)
	}
}

// The resolve runs after the commit and off the request: a resolver that has not returned
// does not hold up the scan report, and it sees the scan already reported.
func TestScanArtworkResolveDoesNotBlockTheScan(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)
	art := &recordingArtwork{block: make(chan struct{}), done: make(chan struct{})}
	srv := newArtworkScanServer(t, f, art, false) // production spawn: a goroutine

	scanID := f.claimedScan(t, f.user)
	answered := make(chan int, 1)
	go func() {
		resp := agentReq(t, "POST", srv.URL+"/v1/agent/library/scan-report", f.nodeName, f.secret,
			ScanReport{ScanID: scanID, OK: true, Entries: observedEntries()})
		resp.Body.Close()
		answered <- resp.StatusCode
	}()
	select {
	case code := <-answered:
		if code != http.StatusOK {
			t.Fatalf("scan-report = %d, want 200", code)
		}
	case <-time.After(10 * time.Second):
		close(art.block)
		t.Fatal("the scan report waited on the artwork resolve")
	}
	var state string
	must(t, pool.QueryRow(context.Background(),
		`SELECT state FROM library_scans WHERE id::text = $1`, scanID).Scan(&state))
	if state != "reported" {
		t.Fatalf("scan state = %q, want reported before the artwork pass finished", state)
	}

	close(art.block)
	select {
	case <-art.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the artwork pass never ran")
	}
	if calls := art.snapshot(); len(calls) != 1 || len(calls[0]) != 4 {
		t.Fatalf("want one pass over the 4 new apps, got %v", calls)
	}
}

// countingProvider is an artwork.Provider that counts every call — the only way a test can
// see "one outbound request per new app".
type countingProvider struct {
	mu       sync.Mutex
	external []string
	searches int
}

func (p *countingProvider) Name() string { return "counting" }
func (p *countingProvider) Search(context.Context, string) ([]artwork.Candidate, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.searches++
	return nil, nil
}
func (p *countingProvider) Art(context.Context, string) (artwork.Candidate, error) {
	return artwork.Candidate{}, artwork.ErrArtNotFound
}
func (p *countingProvider) ArtByExternalRef(_ context.Context, source, id string) (artwork.Candidate, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.external = append(p.external, source+":"+id)
	return artwork.Candidate{}, artwork.ErrArtNotFound // a stored "no match"
}

func newArtworkService(t *testing.T, pool *pgxpool.Pool, src artwork.ProviderSource) *artwork.Service {
	t.Helper()
	svc, err := artwork.New(artwork.NewStore(pool), t.TempDir(),
		artwork.Options{ProviderSource: src}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	must(t, err)
	return svc
}

// End to end through the real artwork service: one provider request per new app, by Steam
// appid, a recorded decision for each, and no request at all on a re-scan.
func TestScanArtworkUsesTheSweepResolverEndToEnd(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)
	prov := &countingProvider{}
	svc := newArtworkService(t, pool, artwork.StaticProviderSource(prov))
	srv := newArtworkScanServer(t, f, svc, true)

	f.reportScan(t, srv, observedEntries())
	if len(prov.external) != 4 || prov.searches != 0 {
		t.Fatalf("want 4 by-appid requests and no search, got %v / %d searches", prov.external, prov.searches)
	}
	if n := countT(t, pool, `SELECT count(*) FROM app_artwork w JOIN apps a ON a.id = w.app_id
		WHERE a.parent_app_id = $1::uuid AND w.source = 'none'`, f.parent); n != 4 {
		t.Fatalf("want a recorded no-match for each of the 4 new apps, got %d", n)
	}

	f.reportScan(t, srv, observedEntries())
	if len(prov.external) != 4 {
		t.Fatalf("a re-scan must make no artwork request; got %v", prov.external)
	}
}

// Ship-dark: with the shipped provider source and no key configured, a scan makes no artwork
// request and writes no artwork row.
func TestScanWithNoArtworkProviderMakesNoRequest(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)
	svc := newArtworkService(t, pool, artwork.NewSecretProviderSource(nil, "", false, quietLogger()))
	srv := newArtworkScanServer(t, f, svc, true)

	f.reportScan(t, srv, observedEntries())
	if n := len(f.tileIDs(t)); n != 4 {
		t.Fatalf("the scan itself must still publish; got %d tiles", n)
	}
	if n := countT(t, pool, `SELECT count(*) FROM app_artwork`); n != 0 {
		t.Fatalf("no provider must mean no artwork row; got %d", n)
	}
	if n := countT(t, pool, `SELECT count(*) FROM apps WHERE cover_url IS NOT NULL OR hero_url IS NOT NULL`); n != 0 {
		t.Fatalf("no provider must mean no artwork urls; got %d", n)
	}
}
