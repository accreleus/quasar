package artwork

// Integration tests for UI-P7. Require Postgres: run via `scripts/dev/dev.sh
// go-test-db` (which sets TEST_DATABASE_URL), never against a shared long-lived
// database — setup deletes rows.
//
// EVERY test here uses a FAKE provider or none at all. No test in this package
// makes a live third-party call; what a live call would prove is called out
// explicitly in the phase report.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/auth"
	"github.com/accreleus/quasar/control-plane/internal/migrate"
	migrations "github.com/accreleus/quasar/control-plane/migrations"
)

// --- fake provider ----------------------------------------------------------

// fakeProvider is the Provider stand-in. It counts calls so a test can assert
// that a desktop app or an already-resolved app produced NO third-party traffic
// — which is the actual behaviour under test, not an implementation detail.
type fakeProvider struct {
	searches int
	artCalls int
	results  map[string][]Candidate
	art      map[string]Candidate
	// externalCalls counts ArtByExternalRef, keyed nowhere: the acceptance
	// criterion is "resolved by appid AND searches == 0", so both counters are
	// asserted together.
	externalCalls int
	// artByExternal maps "<source>:<id>" to the art an appid resolves to. A key
	// that is absent models the provider's 404 — ErrArtNotFound, which is a
	// NORMAL no-art outcome and must not look like an outage.
	artByExternal map[string]Candidate
	searchErr     error
	artErr        error
	externalErr   error
}

func (f *fakeProvider) Name() string { return "fake" }

func (f *fakeProvider) Search(_ context.Context, query string) ([]Candidate, error) {
	f.searches++
	if f.searchErr != nil {
		return nil, f.searchErr
	}
	return f.results[query], nil
}

func (f *fakeProvider) Art(_ context.Context, ref string) (Candidate, error) {
	f.artCalls++
	if f.artErr != nil {
		return Candidate{}, f.artErr
	}
	c, ok := f.art[ref]
	if !ok {
		return Candidate{}, fmt.Errorf("fake: no art for %q", ref)
	}
	return c, nil
}

func (f *fakeProvider) ArtByExternalRef(_ context.Context, source, id string) (Candidate, error) {
	f.externalCalls++
	if source != ExternalSourceSteam {
		return Candidate{}, fmt.Errorf("%w: fake only knows steam", ErrUnsupportedExternalSource)
	}
	if f.externalErr != nil {
		return Candidate{}, f.externalErr
	}
	c, ok := f.artByExternal[source+":"+id]
	if !ok {
		return Candidate{}, ErrArtNotFound
	}
	return c, nil
}

// --- harness ----------------------------------------------------------------

func testDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set")
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
	if _, err := pool.Exec(ctx, `
		DELETE FROM sessions;
		DELETE FROM app_artwork;
		DELETE FROM user_app_favourites;
		DELETE FROM apps;
		DELETE FROM auth_tokens;
		DELETE FROM users;
	`); err != nil {
		pool.Close()
		t.Fatalf("truncate: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

type harness struct {
	pool     *pgxpool.Pool
	svc      *Service
	srv      *httptest.Server
	provider *fakeProvider
	// artSrv serves the fake "CDN" the service fetches crops from.
	artSrv     *httptest.Server
	adminToken string
	userToken  string
}

// newHarness builds the service + an httptest server with the real auth
// middleware, so the admin gate under test is the ACTUAL server-side gate.
// provider == nil models an unconfigured deployment.
func newHarness(t *testing.T, pool *pgxpool.Pool, provider Provider) *harness {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	svc, err := New(NewStore(pool), t.TempDir(), Options{
		Provider:      provider,
		MaxImageBytes: 64 << 10,
		SweepInterval: 0, // no background goroutine: tests drive SweepOnce
	}, log)
	if err != nil {
		t.Fatalf("artwork.New: %v", err)
	}
	// Loopback is refused by the production dialer by design (see
	// TestFetcherBlocksLoopback). The httptest "CDN" below is on loopback, so
	// the service under test gets an unguarded fetcher; the guard itself is
	// covered in fetch_test.go against the real constructor.
	svc.fetcher = unguardedFetcher(t, 64<<10)

	authSvc, err := auth.NewService(pool, auth.DefaultParams(), time.Hour)
	if err != nil {
		t.Fatalf("auth service: %v", err)
	}
	mux := http.NewServeMux()
	authHandler := auth.NewHandler(authSvc)
	authHandler.Register(mux)
	NewHandler(svc, log).Register(mux, authHandler.RequireAuth, authHandler.RequireAdmin)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	artSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, ".png"):
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(onePixelPNG)
		case strings.HasSuffix(r.URL.Path, ".jpg"):
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(onePixelJPEG)
		case strings.HasSuffix(r.URL.Path, ".html"):
			// A "crop URL" that actually serves markup — the mislabelled-content
			// case, exercised end to end.
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("<html><script>alert(1)</script></html>"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(artSrv.Close)

	ctx := context.Background()
	if _, err := authSvc.Register(ctx, "admin@test.local", "admin", "quasar-fixture-pw-01"); err != nil {
		t.Fatalf("register admin: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET role = 'admin' WHERE email = $1`, "admin@test.local"); err != nil {
		t.Fatalf("promote: %v", err)
	}
	adminTok, err := authSvc.Login(ctx, "admin@test.local", "quasar-fixture-pw-01", "")
	if err != nil {
		t.Fatalf("login admin: %v", err)
	}
	if _, err := authSvc.Register(ctx, "user@test.local", "user", "quasar-fixture-pw-05"); err != nil {
		t.Fatalf("register user: %v", err)
	}
	userTok, err := authSvc.Login(ctx, "user@test.local", "quasar-fixture-pw-05", "")
	if err != nil {
		t.Fatalf("login user: %v", err)
	}

	fp, _ := provider.(*fakeProvider)
	return &harness{
		pool: pool, svc: svc, srv: srv, provider: fp, artSrv: artSrv,
		adminToken: adminTok.Plaintext, userToken: userTok.Plaintext,
	}
}

func (h *harness) seedApp(t *testing.T, name, kind string) string {
	t.Helper()
	var id string
	err := h.pool.QueryRow(context.Background(),
		`INSERT INTO apps (name, description, kind, runtime_spec, enabled)
		 VALUES ($1, '', $2, '{}'::jsonb, true) RETURNING id::text`, name, kind).Scan(&id)
	if err != nil {
		t.Fatalf("seed app: %v", err)
	}
	return id
}

// seedExternalApp seeds an app tagged with a provider external ref (migration
// 0042) — the shape the by-appid resolver takes.
func (h *harness) seedExternalApp(t *testing.T, name, kind, source, externalID string) string {
	t.Helper()
	var id string
	err := h.pool.QueryRow(context.Background(),
		`INSERT INTO apps (name, description, kind, runtime_spec, enabled, external_source, external_id)
		 VALUES ($1, '', $2, '{}'::jsonb, true, $3, $4) RETURNING id::text`,
		name, kind, source, externalID).Scan(&id)
	if err != nil {
		t.Fatalf("seed external app: %v", err)
	}
	return id
}

func (h *harness) appURLs(t *testing.T, appID string) (cover, hero *string) {
	t.Helper()
	err := h.pool.QueryRow(context.Background(),
		`SELECT cover_url, hero_url FROM apps WHERE id::text = $1`, appID).Scan(&cover, &hero)
	if err != nil {
		t.Fatalf("read app urls: %v", err)
	}
	return cover, hero
}

func (h *harness) appRef(t *testing.T, appID string) appRef {
	t.Helper()
	a, err := h.svc.store.App(context.Background(), appID)
	if err != nil {
		t.Fatalf("appRef: %v", err)
	}
	return a
}

func do(t *testing.T, method, url, token, contentType string, body []byte) (*http.Response, map[string]any) {
	t.Helper()
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var parsed map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &parsed)
	}
	return resp, parsed
}

func jsonBody(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// ============================================================================
// ACCEPTANCE: a deployment with the feature disabled behaves exactly as today
// ============================================================================

// With no provider configured, a sweep must write nothing for a GAME and leave
// cover_url NULL — the gradient tile, byte-for-byte the pre-UI-P7 behaviour.
func TestUnconfiguredDeploymentChangesNothingForGames(t *testing.T) {
	pool := testDB(t)
	h := newHarness(t, pool, nil) // no provider

	appID := h.seedApp(t, "Portal 2", "game")
	res := h.svc.SweepOnce(context.Background())
	// ProviderConfigured=false is the artwork.sweep job's Skipped("no artwork
	// provider configured") outcome (WP2 §8.1) — the ship-dark distinction the
	// jobs viewer exists to surface.
	if res.ProviderConfigured {
		t.Fatal("SweepOnce: ProviderConfigured=true with no provider configured")
	}
	if res.AppsConsidered != 0 || res.ArtworkResolved != 0 || res.NoMatch != 0 {
		t.Fatalf("SweepOnce result = %+v, want the zero value (no query, no work)", res)
	}

	cover, hero := h.appURLs(t, appID)
	if cover != nil || hero != nil {
		t.Fatalf("an unconfigured deployment must not write artwork urls; got cover=%v hero=%v", cover, hero)
	}
	if _, ok, err := h.svc.store.Get(context.Background(), appID); err != nil {
		t.Fatalf("get: %v", err)
	} else if ok {
		t.Fatal("an unconfigured deployment must not write an artwork row for a game")
	}
}

// The provider-backed admin actions answer a clean 409 rather than 500 — an
// unconfigured deployment is the documented default, not a fault.
func TestUnconfiguredProviderRejectsSearchCleanly(t *testing.T) {
	pool := testDB(t)
	h := newHarness(t, pool, nil)
	appID := h.seedApp(t, "Portal 2", "game")

	resp, body := do(t, http.MethodPost, h.srv.URL+"/v1/admin/apps/"+appID+"/artwork/search",
		h.adminToken, "application/json", jsonBody(t, map[string]any{}))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("search with no provider: want 409, got %d (%v)", resp.StatusCode, body)
	}

	// GET still works and reports the state, so the UI can explain itself.
	resp, body = do(t, http.MethodGet, h.srv.URL+"/v1/admin/apps/"+appID+"/artwork", h.adminToken, "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get artwork: want 200, got %d", resp.StatusCode)
	}
	if body["provider_configured"] != false {
		t.Fatalf("provider_configured: want false, got %v", body["provider_configured"])
	}
	if body["artwork"] != nil {
		t.Fatalf("artwork: want null, got %v", body["artwork"])
	}
}

// TestProviderUnavailableReasonComesFromProviderInfo pins the boundary: the
// client-visible 409 message is read from the typed error's ProviderInfo, never
// recovered by slicing err.Error().
//
// The old implementation cut the sentinel's text off the front of the error
// string, which meant (a) any future ProviderInfo.Problem built from an internal
// error would have been handed straight to an HTTP client, and (b) any caller
// that added context to the error silently lost the explanation. Both are
// covered below. No database: this is pure error plumbing.
func TestProviderUnavailableReasonComesFromProviderInfo(t *testing.T) {
	h := NewHandler(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	const problem = "An artwork API key is stored but the configured master key does not match it."
	const generic = "no artwork provider is configured on this deployment"

	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"a reason is surfaced verbatim",
			&ProviderUnavailableError{Info: ProviderInfo{Origin: OriginNone, Problem: problem}}, problem},
		{"no reason falls back to the generic sentence",
			&ProviderUnavailableError{Info: ProviderInfo{Origin: OriginNone}}, generic},
		{"the reason survives an outer wrap",
			fmt.Errorf("sweep: %w", &ProviderUnavailableError{Info: ProviderInfo{Problem: problem}}), problem},
		{"the bare sentinel still answers 409", ErrProviderNotConfigured, generic},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.writeErr(rec, tc.err, "could not do the thing")
			if rec.Code != http.StatusConflict {
				t.Fatalf("status %d, want 409 (%s)", rec.Code, rec.Body.String())
			}
			var body struct {
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if body.Error.Message != tc.want {
				t.Fatalf("message = %q, want %q", body.Error.Message, tc.want)
			}
		})
	}

	// And the sentinel relationship the rest of the package relies on.
	err := error(&ProviderUnavailableError{Info: ProviderInfo{Problem: problem}})
	if !errors.Is(err, ErrProviderNotConfigured) {
		t.Fatal("ProviderUnavailableError must satisfy errors.Is(ErrProviderNotConfigured)")
	}
}

// The LOCAL half must work with no provider at all — this is what makes a
// desktop app a solved case rather than a permanent gradient.
func TestUploadWorksWithNoProvider(t *testing.T) {
	pool := testDB(t)
	h := newHarness(t, pool, nil)
	appID := h.seedApp(t, "Blender", "desktop")

	resp, body := do(t, http.MethodPost,
		h.srv.URL+"/v1/admin/apps/"+appID+"/artwork/upload?crop=tile",
		h.adminToken, "image/png", onePixelPNG)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("upload: want 200, got %d (%v)", resp.StatusCode, body)
	}
	cover, _ := h.appURLs(t, appID)
	if cover == nil || !strings.HasPrefix(*cover, "/v1/artwork/") {
		t.Fatalf("cover_url: want a local /v1/artwork path, got %v", cover)
	}
}

// ============================================================================
// The automatic path
// ============================================================================

func TestResolveMatchesGameAndCachesBothCrops(t *testing.T) {
	pool := testDB(t)
	fp := &fakeProvider{results: map[string][]Candidate{}, art: map[string]Candidate{}}
	h := newHarness(t, pool, fp)
	fp.results["Portal 2"] = []Candidate{{Ref: "1234", Name: "Portal 2"}}
	fp.art["1234"] = Candidate{
		Ref: "1234", Name: "Portal 2",
		TileURL:     h.artSrv.URL + "/grid.png",
		HeroURL:     h.artSrv.URL + "/hero.jpg",
		Attribution: "Artwork via Fake",
	}

	appID := h.seedApp(t, "Portal 2", "game")
	res := h.svc.SweepOnce(context.Background())
	// The counts the artwork.sweep job (internal/jobs) records as its run
	// summary (WP2 §8.1) — this is what finally distinguishes "resolved" from
	// "configured but nothing matched" in the admin viewer.
	if !res.ProviderConfigured {
		t.Fatal("SweepOnce: ProviderConfigured=false with a fake provider configured")
	}
	if res.AppsConsidered != 1 || res.ArtworkResolved != 1 || res.NoMatch != 0 {
		t.Fatalf("SweepOnce result = %+v, want {ProviderConfigured:true AppsConsidered:1 ArtworkResolved:1 NoMatch:0}", res)
	}

	cover, hero := h.appURLs(t, appID)
	if cover == nil || hero == nil {
		t.Fatalf("both crops must be written; got cover=%v hero=%v", cover, hero)
	}
	if *cover == *hero {
		t.Fatal("tile and hero must be different cached assets, not one image reused")
	}
	// Locally cached, not hotlinked: the stored URL must be ours.
	for _, u := range []string{*cover, *hero} {
		if !strings.HasPrefix(u, "/v1/artwork/") {
			t.Fatalf("artwork must be served locally, got %q", u)
		}
		if strings.Contains(u, h.artSrv.URL) {
			t.Fatalf("artwork must never hotlink the source, got %q", u)
		}
	}

	rec, ok, err := h.svc.store.Get(context.Background(), appID)
	if err != nil || !ok {
		t.Fatalf("record: ok=%v err=%v", ok, err)
	}
	if rec.Source != SourceProvider || rec.MatchedName != "Portal 2" || rec.Locked {
		t.Fatalf("record: %+v", rec)
	}
	if rec.Attribution != "Artwork via Fake" {
		t.Fatalf("attribution must be stored, got %q", rec.Attribution)
	}
}

// ART SURVIVES A REDEPLOY: a second sweep must make no third-party call, and
// the blobs must still be on disk and servable.
func TestResolvedArtIsNotRefetched(t *testing.T) {
	pool := testDB(t)
	fp := &fakeProvider{
		results: map[string][]Candidate{"Portal 2": {{Ref: "1234", Name: "Portal 2"}}},
		art:     map[string]Candidate{},
	}
	h := newHarness(t, pool, fp)
	fp.art["1234"] = Candidate{TileURL: h.artSrv.URL + "/grid.png", HeroURL: h.artSrv.URL + "/hero.jpg"}

	appID := h.seedApp(t, "Portal 2", "game")
	h.svc.SweepOnce(context.Background())
	afterFirst := fp.searches

	// Simulate a redeploy: the process restarts and sweeps again.
	h.svc.SweepOnce(context.Background())
	h.svc.SweepOnce(context.Background())
	if fp.searches != afterFirst {
		t.Fatalf("art was re-fetched on a later sweep: %d searches after first, %d now",
			afterFirst, fp.searches)
	}

	cover, _ := h.appURLs(t, appID)
	resp, _ := do(t, http.MethodGet, h.srv.URL+*cover, "", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cached asset must still serve after a re-sweep, got %d", resp.StatusCode)
	}
}

// A DESKTOP app must never reach the provider — Blender is not in a games
// database, and asking leaks the app name for a guaranteed miss.
func TestDesktopAppIsNeverQueried(t *testing.T) {
	pool := testDB(t)
	fp := &fakeProvider{}
	h := newHarness(t, pool, fp)

	appID := h.seedApp(t, "Blender", "desktop")
	h.svc.SweepOnce(context.Background())

	if fp.searches != 0 {
		t.Fatalf("a desktop app must produce no provider call, got %d", fp.searches)
	}
	rec, ok, err := h.svc.store.Get(context.Background(), appID)
	if err != nil || !ok {
		t.Fatalf("a desktop app must record a 'none' row: ok=%v err=%v", ok, err)
	}
	if rec.Source != SourceNone {
		t.Fatalf("source: want %q, got %q", SourceNone, rec.Source)
	}
	cover, hero := h.appURLs(t, appID)
	if cover != nil || hero != nil {
		t.Fatalf("a desktop app keeps the gradient tile; got cover=%v hero=%v", cover, hero)
	}
}

// An unmatched game is a NEGATIVE CACHE, not a retry loop.
func TestUnmatchedGameIsCachedAsNone(t *testing.T) {
	pool := testDB(t)
	fp := &fakeProvider{results: map[string][]Candidate{}}
	h := newHarness(t, pool, fp)

	appID := h.seedApp(t, "Some Internal Tool", "game")
	res := h.svc.SweepOnce(context.Background())
	if fp.searches != 1 {
		t.Fatalf("want exactly one search, got %d", fp.searches)
	}
	if res.AppsConsidered != 1 || res.ArtworkResolved != 0 || res.NoMatch != 1 {
		t.Fatalf("SweepOnce result = %+v, want {AppsConsidered:1 ArtworkResolved:0 NoMatch:1}", res)
	}
	h.svc.SweepOnce(context.Background())
	if fp.searches != 1 {
		t.Fatalf("an unmatched app must not be re-queried; got %d searches", fp.searches)
	}
	rec, ok, _ := h.svc.store.Get(context.Background(), appID)
	if !ok || rec.Source != SourceNone {
		t.Fatalf("want a cached 'none' row, got ok=%v rec=%+v", ok, rec)
	}
}

// A provider OUTAGE must NOT be cached as "no art" — otherwise a transient
// blip permanently gradients the whole catalogue.
func TestProviderErrorIsNotCached(t *testing.T) {
	pool := testDB(t)
	fp := &fakeProvider{searchErr: fmt.Errorf("upstream is down")}
	h := newHarness(t, pool, fp)

	appID := h.seedApp(t, "Portal 2", "game")
	h.svc.SweepOnce(context.Background())

	if _, ok, err := h.svc.store.Get(context.Background(), appID); err != nil {
		t.Fatalf("get: %v", err)
	} else if ok {
		t.Fatal("a provider outage must not write a row — it would be cached as 'no art' forever")
	}
	// And the next sweep tries again.
	h.svc.SweepOnce(context.Background())
	if fp.searches != 2 {
		t.Fatalf("want a retry on the next sweep, got %d searches", fp.searches)
	}
}

// A locked (admin-overridden) row must survive every later sweep.
func TestSweepNeverOverwritesAnAdminOverride(t *testing.T) {
	pool := testDB(t)
	fp := &fakeProvider{
		results: map[string][]Candidate{"Portal 2": {{Ref: "wrong", Name: "Portal 2 Fan Remake"}}},
		art:     map[string]Candidate{},
	}
	h := newHarness(t, pool, fp)
	fp.art["wrong"] = Candidate{TileURL: h.artSrv.URL + "/grid.png"}

	appID := h.seedApp(t, "Portal 2", "game")
	resp, body := do(t, http.MethodPost,
		h.srv.URL+"/v1/admin/apps/"+appID+"/artwork/upload?crop=tile",
		h.adminToken, "image/jpeg", onePixelJPEG)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("upload: want 200, got %d (%v)", resp.StatusCode, body)
	}
	coverBefore, _ := h.appURLs(t, appID)

	h.svc.SweepOnce(context.Background())
	if fp.searches != 0 {
		t.Fatalf("an app with artwork must not be swept, got %d searches", fp.searches)
	}
	coverAfter, _ := h.appURLs(t, appID)
	if *coverBefore != *coverAfter {
		t.Fatalf("the sweep overwrote an admin override: %q → %q", *coverBefore, *coverAfter)
	}
}

// ============================================================================
// The admin override
// ============================================================================

func TestAdminCanOverrideAWrongMatch(t *testing.T) {
	pool := testDB(t)
	// §12.1 CHANGED WHAT A "WRONG MATCH" CAN BE, and this setup moves with it.
	// The automatic path no longer accepts a candidate whose title merely looks
	// close ("Portal Knights" for "Portal"), so it can no longer produce that
	// state at all — a test that kept asserting it would be asserting a
	// behaviour the code deliberately removed.
	//
	// A wrong match is still perfectly reachable, and this is the shape it now
	// takes: two provider entries whose titles BOTH normalise to the app's name
	// ("Portal™" and "Portal" — the trademark symbol is punctuation), so the
	// exactness rule is satisfied by both and the provider's own order decides.
	// The first one is the wrong game's art, and only an operator can know that.
	fp := &fakeProvider{
		results: map[string][]Candidate{
			"Portal": {{Ref: "wrong", Name: "Portal™"}, {Ref: "right", Name: "Portal"}},
		},
		art: map[string]Candidate{},
	}
	h := newHarness(t, pool, fp)
	fp.art["wrong"] = Candidate{Name: "Portal™", TileURL: h.artSrv.URL + "/wrong.png"}
	fp.art["right"] = Candidate{Name: "Portal", TileURL: h.artSrv.URL + "/right.jpg", HeroURL: h.artSrv.URL + "/hero.jpg"}

	appID := h.seedApp(t, "Portal", "game")
	h.svc.SweepOnce(context.Background()) // lands on the WRONG match
	rec, _, _ := h.svc.store.Get(context.Background(), appID)
	if rec.MatchedName != "Portal™" {
		t.Fatalf("setup: want the wrong match first, got %q", rec.MatchedName)
	}
	wrongCover, _ := h.appURLs(t, appID)

	// The operator searches, sees both, and picks the right one.
	resp, body := do(t, http.MethodPost, h.srv.URL+"/v1/admin/apps/"+appID+"/artwork/search",
		h.adminToken, "application/json", jsonBody(t, map[string]string{"query": "Portal"}))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("search: want 200, got %d (%v)", resp.StatusCode, body)
	}
	cands, _ := body["candidates"].([]any)
	if len(cands) != 2 {
		t.Fatalf("candidates: want 2, got %v", body["candidates"])
	}

	resp, body = do(t, http.MethodPut, h.srv.URL+"/v1/admin/apps/"+appID+"/artwork",
		h.adminToken, "application/json", jsonBody(t, map[string]string{"provider_ref": "right"}))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("apply: want 200, got %d (%v)", resp.StatusCode, body)
	}

	rec, _, _ = h.svc.store.Get(context.Background(), appID)
	if rec.Source != SourceManual || !rec.Locked || rec.MatchedName != "Portal" {
		t.Fatalf("after override: %+v", rec)
	}
	rightCover, rightHero := h.appURLs(t, appID)
	if *rightCover == *wrongCover {
		t.Fatal("the cover did not change after the override")
	}
	if rightHero == nil {
		t.Fatal("the override must also apply the hero crop")
	}
}

// The operator-supplied-URL override — the path for art the provider does not
// have. It must go through the SAME fetch guards.
func TestAdminCanApplyExplicitURLs(t *testing.T) {
	pool := testDB(t)
	h := newHarness(t, pool, nil)
	appID := h.seedApp(t, "Blender", "desktop")

	resp, body := do(t, http.MethodPut, h.srv.URL+"/v1/admin/apps/"+appID+"/artwork",
		h.adminToken, "application/json", jsonBody(t, map[string]string{
			"tile_url": h.artSrv.URL + "/tile.png",
			"hero_url": h.artSrv.URL + "/hero.jpg",
		}))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("apply urls: want 200, got %d (%v)", resp.StatusCode, body)
	}
	cover, hero := h.appURLs(t, appID)
	if cover == nil || hero == nil {
		t.Fatalf("both crops must be set; got cover=%v hero=%v", cover, hero)
	}
	rec, _, _ := h.svc.store.Get(context.Background(), appID)
	if rec.Source != SourceManual || !rec.Locked {
		t.Fatalf("an operator-supplied override must be manual+locked, got %+v", rec)
	}
}

// Replacing only the hero must leave the tile alone.
func TestApplyURLsLeavesTheOtherCropAlone(t *testing.T) {
	pool := testDB(t)
	h := newHarness(t, pool, nil)
	appID := h.seedApp(t, "Thing", "desktop")

	if resp, body := do(t, http.MethodPost,
		h.srv.URL+"/v1/admin/apps/"+appID+"/artwork/upload?crop=tile",
		h.adminToken, "image/png", onePixelPNG); resp.StatusCode != http.StatusOK {
		t.Fatalf("seed tile: %d (%v)", resp.StatusCode, body)
	}
	tileBefore, _ := h.appURLs(t, appID)

	if resp, body := do(t, http.MethodPut, h.srv.URL+"/v1/admin/apps/"+appID+"/artwork",
		h.adminToken, "application/json", jsonBody(t, map[string]string{
			"hero_url": h.artSrv.URL + "/hero.jpg",
		})); resp.StatusCode != http.StatusOK {
		t.Fatalf("apply hero: %d (%v)", resp.StatusCode, body)
	}
	tileAfter, heroAfter := h.appURLs(t, appID)
	if tileAfter == nil || *tileAfter != *tileBefore {
		t.Fatalf("tile changed: %v → %v", tileBefore, tileAfter)
	}
	if heroAfter == nil {
		t.Fatal("hero was not applied")
	}
}

// Clearing returns an app to the gradient tile.
func TestAdminCanClearArtwork(t *testing.T) {
	pool := testDB(t)
	h := newHarness(t, pool, nil)
	appID := h.seedApp(t, "Thing", "desktop")

	do(t, http.MethodPost, h.srv.URL+"/v1/admin/apps/"+appID+"/artwork/upload?crop=tile",
		h.adminToken, "image/png", onePixelPNG)

	resp, _ := do(t, http.MethodDelete, h.srv.URL+"/v1/admin/apps/"+appID+"/artwork", h.adminToken, "", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("clear: want 204, got %d", resp.StatusCode)
	}
	cover, hero := h.appURLs(t, appID)
	if cover != nil || hero != nil {
		t.Fatalf("after clear the app must render the gradient tile; got cover=%v hero=%v", cover, hero)
	}
	if _, ok, _ := h.svc.store.Get(context.Background(), appID); ok {
		t.Fatal("the provenance row must be gone after a clear")
	}
}

func TestArtworkCascadesOnAppDelete(t *testing.T) {
	pool := testDB(t)
	h := newHarness(t, pool, nil)
	appID := h.seedApp(t, "Thing", "desktop")
	do(t, http.MethodPost, h.srv.URL+"/v1/admin/apps/"+appID+"/artwork/upload?crop=tile",
		h.adminToken, "image/png", onePixelPNG)

	if _, err := pool.Exec(context.Background(), `DELETE FROM apps WHERE id::text = $1`, appID); err != nil {
		t.Fatalf("delete app: %v", err)
	}
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM app_artwork WHERE app_id::text = $1`, appID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("artwork must cascade with its app; %d rows remain", n)
	}
}

// ============================================================================
// SECURITY
// ============================================================================

// Server-enforced admin gating on EVERY mutating route (CLAUDE.md invariant #6).
// Hiding the Artwork panel in the UI is not the access control.
func TestMutatingRoutesRejectNonAdminServerSide(t *testing.T) {
	pool := testDB(t)
	h := newHarness(t, pool, nil)
	appID := h.seedApp(t, "Thing", "game")
	base := h.srv.URL + "/v1/admin/apps/" + appID + "/artwork"

	cases := []struct {
		method, url, ct string
		body            []byte
	}{
		{http.MethodGet, base, "", nil},
		{http.MethodPut, base, "application/json", jsonBody(t, map[string]string{"tile_url": "https://example.test/a.png"})},
		{http.MethodDelete, base, "", nil},
		{http.MethodPost, base + "/search", "application/json", jsonBody(t, map[string]any{})},
		{http.MethodPost, base + "/upload?crop=tile", "image/png", onePixelPNG},
		// #385 Phase 4: the catalogue-wide re-resolve is admin-gated by the SAME
		// middleware, not by being hard to find in the UI.
		{http.MethodPost, h.srv.URL + "/v1/admin/artwork/reresolve", "application/json", jsonBody(t, map[string]any{})},
	}
	for _, c := range cases {
		// A valid NON-ADMIN token must be 403.
		resp, _ := do(t, c.method, c.url, h.userToken, c.ct, c.body)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s %s with a user token: want 403, got %d", c.method, c.url, resp.StatusCode)
		}
		// No token at all must be 401.
		resp, _ = do(t, c.method, c.url, "", c.ct, c.body)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s with no token: want 401, got %d", c.method, c.url, resp.StatusCode)
		}
	}

	// Nothing was written by any of the rejected calls.
	if cover, hero := h.appURLs(t, appID); cover != nil || hero != nil {
		t.Fatalf("a rejected request wrote artwork: cover=%v hero=%v", cover, hero)
	}
}

// The asset route must 404 (never 500, never a file) for anything that is not a
// valid content-addressed name.
func TestAssetRouteRejectsTraversal(t *testing.T) {
	pool := testDB(t)
	h := newHarness(t, pool, nil)

	for _, name := range []string{
		url.PathEscape("../../../etc/passwd"),
		"..%2F..%2Fetc%2Fpasswd",
		"secret.txt",
		"AAAA.png",
		strings.Repeat("a", 64) + ".svg",
	} {
		resp, _ := do(t, http.MethodGet, h.srv.URL+"/v1/artwork/"+name, "", "", nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET /v1/artwork/%s: want 404, got %d", name, resp.StatusCode)
		}
	}
}

// A crop URL that serves HTML while claiming image/png must be rejected, and
// nothing must be written.
func TestMislabelledRemoteContentIsRejected(t *testing.T) {
	pool := testDB(t)
	h := newHarness(t, pool, nil)
	appID := h.seedApp(t, "Thing", "desktop")

	resp, body := do(t, http.MethodPut, h.srv.URL+"/v1/admin/apps/"+appID+"/artwork",
		h.adminToken, "application/json", jsonBody(t, map[string]string{
			"tile_url": h.artSrv.URL + "/evil.html",
		}))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400 for mislabelled content, got %d (%v)", resp.StatusCode, body)
	}
	if cover, _ := h.appURLs(t, appID); cover != nil {
		t.Fatalf("mislabelled content must not be stored, got %v", cover)
	}
}

// An operator-supplied URL pointing INTO the deployment's network is refused.
// This is the SSRF guard reached through the real HTTP surface: the handler's
// service uses the unguarded fetcher for loopback (so tests can serve art), so
// this asserts against a genuinely private RFC1918 address instead.
func TestOperatorURLCannotReachAPrivateAddress(t *testing.T) {
	pool := testDB(t)
	h := newHarness(t, pool, nil)
	// Restore the production fetcher for this test only.
	h.svc.fetcher = NewFetcher(5*time.Second, 64<<10)
	appID := h.seedApp(t, "Thing", "desktop")

	for _, target := range []string{
		"http://192.0.2.10/art.png",
		"http://169.254.169.254/latest/meta-data/",
		"http://127.0.0.1:5432/art.png",
		"file:///etc/passwd",
	} {
		resp, body := do(t, http.MethodPut, h.srv.URL+"/v1/admin/apps/"+appID+"/artwork",
			h.adminToken, "application/json", jsonBody(t, map[string]string{"tile_url": target}))
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("tile_url=%s: want 400, got %d (%v)", target, resp.StatusCode, body)
		}
	}
	if cover, _ := h.appURLs(t, appID); cover != nil {
		t.Fatalf("nothing must be stored from a blocked fetch, got %v", cover)
	}
}

// An oversized upload is refused by the server's own reader, not by trusting
// what the client declared.
func TestUploadEnforcesSizeCap(t *testing.T) {
	pool := testDB(t)
	h := newHarness(t, pool, nil)
	appID := h.seedApp(t, "Thing", "desktop")

	huge := make([]byte, (64<<10)+4096) // cap is 64 KiB in the harness
	copy(huge, onePixelPNG)
	resp, _ := do(t, http.MethodPost, h.srv.URL+"/v1/admin/apps/"+appID+"/artwork/upload?crop=tile",
		h.adminToken, "image/png", huge)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("want 413 for an oversized upload, got %d", resp.StatusCode)
	}
	if cover, _ := h.appURLs(t, appID); cover != nil {
		t.Fatalf("an oversized upload must store nothing, got %v", cover)
	}
}

func TestUploadRejectsNonImage(t *testing.T) {
	pool := testDB(t)
	h := newHarness(t, pool, nil)
	appID := h.seedApp(t, "Thing", "desktop")

	resp, _ := do(t, http.MethodPost, h.srv.URL+"/v1/admin/apps/"+appID+"/artwork/upload?crop=tile",
		h.adminToken, "image/png", []byte("<html><script>alert(1)</script></html>"))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400 for a non-image upload, got %d", resp.StatusCode)
	}
}

func TestUploadRejectsUnknownCrop(t *testing.T) {
	pool := testDB(t)
	h := newHarness(t, pool, nil)
	appID := h.seedApp(t, "Thing", "desktop")

	resp, _ := do(t, http.MethodPost, h.srv.URL+"/v1/admin/apps/"+appID+"/artwork/upload?crop=banner",
		h.adminToken, "image/png", onePixelPNG)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400 for an unknown crop, got %d", resp.StatusCode)
	}
}

// The served asset must carry the headers that stop a browser reinterpreting it.
func TestAssetResponseHeaders(t *testing.T) {
	pool := testDB(t)
	h := newHarness(t, pool, nil)
	appID := h.seedApp(t, "Thing", "desktop")
	do(t, http.MethodPost, h.srv.URL+"/v1/admin/apps/"+appID+"/artwork/upload?crop=tile",
		h.adminToken, "image/png", onePixelPNG)
	cover, _ := h.appURLs(t, appID)

	req, _ := http.NewRequest(http.MethodGet, h.srv.URL+*cover, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get asset: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("asset: want 200, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options: want nosniff, got %q", got)
	}
	if got := resp.Header.Get("Content-Type"); got != "image/png" {
		t.Errorf("Content-Type: want image/png, got %q", got)
	}
	if !strings.Contains(resp.Header.Get("Cache-Control"), "immutable") {
		t.Errorf("Cache-Control must be immutable, got %q", resp.Header.Get("Cache-Control"))
	}
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(body, onePixelPNG) {
		t.Error("served bytes differ from what was uploaded")
	}
}

// 404 for an app that does not exist, on every route.
func TestUnknownAppIs404(t *testing.T) {
	pool := testDB(t)
	h := newHarness(t, pool, nil)
	missing := "00000000-0000-0000-0000-000000000000"
	base := h.srv.URL + "/v1/admin/apps/" + missing + "/artwork"

	resp, _ := do(t, http.MethodGet, base, h.adminToken, "", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET: want 404, got %d", resp.StatusCode)
	}
	resp, _ = do(t, http.MethodDelete, base, h.adminToken, "", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("DELETE: want 404, got %d", resp.StatusCode)
	}
	resp, _ = do(t, http.MethodPost, base+"/upload?crop=tile", h.adminToken, "image/png", onePixelPNG)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("upload: want 404, got %d", resp.StatusCode)
	}
}

// An apply with no usable intent is a 400, not a silent no-op.
func TestApplyRequiresAnIntent(t *testing.T) {
	pool := testDB(t)
	h := newHarness(t, pool, nil)
	appID := h.seedApp(t, "Thing", "game")

	resp, _ := do(t, http.MethodPut, h.srv.URL+"/v1/admin/apps/"+appID+"/artwork",
		h.adminToken, "application/json", jsonBody(t, map[string]any{}))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
}

// --- orphan pruning against real rows ---------------------------------------

func TestPruneOrphansKeepsReferencedBlobs(t *testing.T) {
	pool := testDB(t)
	h := newHarness(t, pool, nil)
	keptApp := h.seedApp(t, "Kept", "desktop")
	droppedApp := h.seedApp(t, "Dropped", "desktop")

	do(t, http.MethodPost, h.srv.URL+"/v1/admin/apps/"+keptApp+"/artwork/upload?crop=tile",
		h.adminToken, "image/png", onePixelPNG)
	do(t, http.MethodPost, h.srv.URL+"/v1/admin/apps/"+droppedApp+"/artwork/upload?crop=tile",
		h.adminToken, "image/jpeg", onePixelJPEG)

	keptCover, _ := h.appURLs(t, keptApp)
	droppedCover, _ := h.appURLs(t, droppedApp)

	// Clearing must NOT delete the blob (it may be shared) — the prune does.
	if resp, _ := do(t, http.MethodDelete, h.srv.URL+"/v1/admin/apps/"+droppedApp+"/artwork",
		h.adminToken, "", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("clear: %d", resp.StatusCode)
	}
	if resp, _ := do(t, http.MethodGet, h.srv.URL+*droppedCover, "", "", nil); resp.StatusCode != http.StatusOK {
		t.Fatal("clear must not delete the blob — it may be shared with another app")
	}

	n, err := h.svc.PruneOrphans(context.Background())
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 1 {
		t.Fatalf("pruned: want 1, got %d", n)
	}
	if resp, _ := do(t, http.MethodGet, h.srv.URL+*keptCover, "", "", nil); resp.StatusCode != http.StatusOK {
		t.Fatal("a referenced blob was pruned")
	}
	if resp, _ := do(t, http.MethodGet, h.srv.URL+*droppedCover, "", "", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatal("the orphaned blob should be gone")
	}
}

// ============================================================================
// #385 Phase 4 — re-resolving existing artwork after the query changed
// ============================================================================

// reresolveHarness seeds three games that all resolved under the OLD (landscape)
// query, then flips the fake provider to serve DIFFERENT art — which is exactly
// the situation the portrait change creates: same apps, same refs, new bytes.
func reresolveHarness(t *testing.T) (*harness, map[string]string) {
	t.Helper()
	pool := testDB(t)
	fp := &fakeProvider{
		results: map[string][]Candidate{
			"Alpha": {{Ref: "alpha", Name: "Alpha"}},
			"Beta":  {{Ref: "beta", Name: "Beta"}},
			"Gamma": {{Ref: "gamma", Name: "Gamma"}},
		},
		art: map[string]Candidate{},
	}
	h := newHarness(t, pool, fp)
	// Round one: the "landscape" bytes (the PNG the fake CDN serves).
	for _, ref := range []string{"alpha", "beta", "gamma"} {
		fp.art[ref] = Candidate{Name: strings.ToUpper(ref[:1]) + ref[1:], TileURL: h.artSrv.URL + "/old.png"}
	}
	ids := map[string]string{}
	for _, name := range []string{"Alpha", "Beta", "Gamma"} {
		ids[name] = h.seedApp(t, name, "game")
	}
	h.svc.SweepOnce(context.Background())
	for name, id := range ids {
		if cover, _ := h.appURLs(t, id); cover == nil {
			t.Fatalf("setup: %s should have art after the first sweep", name)
		}
	}
	// Round two: the "portrait" bytes. Different content ⇒ a different
	// content-addressed blob, so a changed cover_url proves a real refetch.
	for _, ref := range []string{"alpha", "beta", "gamma"} {
		c := fp.art[ref]
		c.TileURL = h.artSrv.URL + "/new.jpg"
		fp.art[ref] = c
	}
	return h, ids
}

// The headline behaviour: a bulk re-resolve replaces art fetched under the old
// query for unlocked apps, and leaves a locked (admin-corrected) app alone.
func TestBulkReresolveReplacesArtAndSkipsLockedRows(t *testing.T) {
	h, ids := reresolveHarness(t)

	// The operator has corrected Gamma by hand; that must survive.
	if resp, body := do(t, http.MethodPost,
		h.srv.URL+"/v1/admin/apps/"+ids["Gamma"]+"/artwork/upload?crop=tile",
		h.adminToken, "image/png", onePixelPNG); resp.StatusCode != http.StatusOK {
		t.Fatalf("lock Gamma: %d (%v)", resp.StatusCode, body)
	}
	before := map[string]string{}
	for name, id := range ids {
		cover, _ := h.appURLs(t, id)
		before[name] = *cover
	}

	resp, body := do(t, http.MethodPost, h.srv.URL+"/v1/admin/artwork/reresolve",
		h.adminToken, "application/json", jsonBody(t, map[string]any{}))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reresolve: want 200, got %d (%v)", resp.StatusCode, body)
	}
	if got := body["resolved"]; got != float64(2) {
		t.Errorf("resolved: want 2, got %v (body %v)", got, body)
	}
	if got := body["skipped_locked"]; got != float64(1) {
		t.Errorf("skipped_locked: want 1, got %v (body %v)", got, body)
	}
	if got := body["failed"]; got != float64(0) {
		t.Errorf("failed: want 0, got %v (body %v)", got, body)
	}
	if got := body["total"]; got != float64(3) {
		t.Errorf("total: want 3, got %v (body %v)", got, body)
	}

	for _, name := range []string{"Alpha", "Beta"} {
		cover, _ := h.appURLs(t, ids[name])
		if cover == nil || *cover == before[name] {
			t.Errorf("%s: an unlocked app must pick up the new art; cover is still %v", name, cover)
		}
	}
	gammaCover, _ := h.appURLs(t, ids["Gamma"])
	if gammaCover == nil || *gammaCover != before["Gamma"] {
		t.Fatalf("a locked app's manual correction was overwritten: %q → %v", before["Gamma"], gammaCover)
	}
	rec, ok, err := h.svc.store.Get(context.Background(), ids["Gamma"])
	if err != nil || !ok || !rec.Locked || rec.Source != SourceManual {
		t.Fatalf("the locked record must be untouched, got %+v (ok=%v err=%v)", rec, ok, err)
	}
}

// force is the deliberate override — and the only way past a correction.
func TestBulkReresolveForceReplacesLockedRows(t *testing.T) {
	h, ids := reresolveHarness(t)
	if resp, _ := do(t, http.MethodPost,
		h.srv.URL+"/v1/admin/apps/"+ids["Gamma"]+"/artwork/upload?crop=tile",
		h.adminToken, "image/png", onePixelPNG); resp.StatusCode != http.StatusOK {
		t.Fatalf("lock Gamma: %d", resp.StatusCode)
	}
	beforeCover, _ := h.appURLs(t, ids["Gamma"])

	resp, body := do(t, http.MethodPost, h.srv.URL+"/v1/admin/artwork/reresolve",
		h.adminToken, "application/json", jsonBody(t, map[string]any{"force": true}))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("forced reresolve: want 200, got %d (%v)", resp.StatusCode, body)
	}
	if got := body["skipped_locked"]; got != float64(0) {
		t.Errorf("force must skip nothing, got skipped_locked=%v", got)
	}
	if got := body["resolved"]; got != float64(3) {
		t.Errorf("resolved: want 3 with force, got %v", got)
	}
	afterCover, _ := h.appURLs(t, ids["Gamma"])
	if afterCover == nil || *afterCover == *beforeCover {
		t.Fatal("force must replace the locked app's art too")
	}
}

// THE BUG THIS PHASE FIXES: single-app `rematch` called store.Clear
// unconditionally, so it silently discarded an admin's correction — the one
// thing `locked` exists to prevent. It must now refuse, and take force.
func TestSingleAppRematchRefusesALockedRecordUnlessForced(t *testing.T) {
	h, ids := reresolveHarness(t)
	appID := ids["Alpha"]
	if resp, _ := do(t, http.MethodPost,
		h.srv.URL+"/v1/admin/apps/"+appID+"/artwork/upload?crop=tile",
		h.adminToken, "image/png", onePixelPNG); resp.StatusCode != http.StatusOK {
		t.Fatalf("lock Alpha: %d", resp.StatusCode)
	}
	locked, _ := h.appURLs(t, appID)

	resp, body := do(t, http.MethodPut, h.srv.URL+"/v1/admin/apps/"+appID+"/artwork",
		h.adminToken, "application/json", jsonBody(t, map[string]any{"rematch": true}))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("rematch on a locked record: want 409, got %d (%v)", resp.StatusCode, body)
	}
	// And it must not have cleared anything on the way to refusing.
	stillThere, _ := h.appURLs(t, appID)
	if stillThere == nil || *stillThere != *locked {
		t.Fatalf("a refused rematch still mutated the record: %q → %v", *locked, stillThere)
	}
	rec, ok, _ := h.svc.store.Get(context.Background(), appID)
	if !ok || !rec.Locked {
		t.Fatalf("the record must survive a refused rematch, got %+v (ok=%v)", rec, ok)
	}

	resp, body = do(t, http.MethodPut, h.srv.URL+"/v1/admin/apps/"+appID+"/artwork",
		h.adminToken, "application/json", jsonBody(t, map[string]any{"rematch": true, "force": true}))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("forced rematch: want 200, got %d (%v)", resp.StatusCode, body)
	}
	forced, _ := h.appURLs(t, appID)
	if forced == nil || *forced == *locked {
		t.Fatal("a forced rematch must actually replace the art")
	}
}

// The spec's claim that orphaned landscape blobs need no new code — PruneOrphans
// already runs at control-plane boot — asserted rather than assumed. After a
// re-resolve the OLD blob is unreferenced and the existing prune reclaims it.
func TestReresolveOrphansAreReclaimedByTheExistingPrune(t *testing.T) {
	h, ids := reresolveHarness(t)
	oldCover, _ := h.appURLs(t, ids["Alpha"])

	if resp, body := do(t, http.MethodPost, h.srv.URL+"/v1/admin/artwork/reresolve",
		h.adminToken, "application/json", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("reresolve: %d (%v)", resp.StatusCode, body)
	}
	newCover, _ := h.appURLs(t, ids["Alpha"])
	if newCover == nil || *newCover == *oldCover {
		t.Fatalf("setup: the cover should have changed, got %v", newCover)
	}
	// Still on disk right after the re-resolve — Save does not delete blobs.
	if resp, _ := do(t, http.MethodGet, h.srv.URL+*oldCover, "", "", nil); resp.StatusCode != http.StatusOK {
		t.Fatal("the superseded blob should still be on disk until the prune runs")
	}

	n, err := h.svc.PruneOrphans(context.Background())
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n < 1 {
		t.Fatalf("the prune must reclaim the superseded landscape blob; pruned %d", n)
	}
	if resp, _ := do(t, http.MethodGet, h.srv.URL+*oldCover, "", "", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatal("the superseded blob survived the prune")
	}
	if resp, _ := do(t, http.MethodGet, h.srv.URL+*newCover, "", "", nil); resp.StatusCode != http.StatusOK {
		t.Fatal("the prune deleted the blob the app still references")
	}
}

// With no provider the whole sweep is one clean 409 — nothing is broken, the
// deployment simply has not opted in — and it must not clear anybody's art on
// the way to saying so.
func TestBulkReresolveWithNoProviderIsAConflictAndChangesNothing(t *testing.T) {
	pool := testDB(t)
	h := newHarness(t, pool, nil)
	appID := h.seedApp(t, "Blender", "desktop")
	do(t, http.MethodPost, h.srv.URL+"/v1/admin/apps/"+appID+"/artwork/upload?crop=tile",
		h.adminToken, "image/png", onePixelPNG)
	before, _ := h.appURLs(t, appID)

	resp, _ := do(t, http.MethodPost, h.srv.URL+"/v1/admin/artwork/reresolve",
		h.adminToken, "application/json", nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("reresolve with no provider: want 409, got %d", resp.StatusCode)
	}
	after, _ := h.appURLs(t, appID)
	if after == nil || *after != *before {
		t.Fatalf("a refused sweep must not touch existing art: %q → %v", *before, after)
	}
}

// Two apps matching the SAME art share one blob — content addressing — and
// clearing one must not break the other.
func TestSharedBlobSurvivesTheOtherAppsClear(t *testing.T) {
	pool := testDB(t)
	h := newHarness(t, pool, nil)
	a := h.seedApp(t, "A", "desktop")
	b := h.seedApp(t, "B", "desktop")

	do(t, http.MethodPost, h.srv.URL+"/v1/admin/apps/"+a+"/artwork/upload?crop=tile",
		h.adminToken, "image/png", onePixelPNG)
	do(t, http.MethodPost, h.srv.URL+"/v1/admin/apps/"+b+"/artwork/upload?crop=tile",
		h.adminToken, "image/png", onePixelPNG)

	coverA, _ := h.appURLs(t, a)
	coverB, _ := h.appURLs(t, b)
	if *coverA != *coverB {
		t.Fatalf("identical bytes must share one blob: %q vs %q", *coverA, *coverB)
	}

	do(t, http.MethodDelete, h.srv.URL+"/v1/admin/apps/"+a+"/artwork", h.adminToken, "", nil)
	if _, err := h.svc.PruneOrphans(context.Background()); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if resp, _ := do(t, http.MethodGet, h.srv.URL+*coverB, "", "", nil); resp.StatusCode != http.StatusOK {
		t.Fatal("the shared blob was deleted while another app still referenced it")
	}
}
