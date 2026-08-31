package artwork

// Steam library discovery, PHASE 1 (spec §12 / §12.1): artwork by external id,
// and the end of the unconditional candidates[0] guess.
//
// The DB-backed tests here reuse handler_test.go's harness and fakeProvider and
// therefore require Postgres (TEST_DATABASE_URL); the normalisation tests are
// pure and always run. No test in this file makes a live third-party call.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ============================================================================
// §12 — by-appid resolution
// ============================================================================

// THE NAMED ACCEPTANCE CRITERION: an app with external_source='steam' and a
// valid appid resolves art with NO Search call at all. The counter is the
// assertion — "it got art" alone would still pass if the fuzzy path had done it.
func TestExternalRefResolvesWithoutAnySearch(t *testing.T) {
	pool := testDB(t)
	fp := &fakeProvider{
		// Deliberately ALSO stocked with a title match. If the implementation
		// ever falls through to Search this test still finds art — so only the
		// searches counter can distinguish the two paths.
		results:       map[string][]Candidate{"Portal 2": {{Ref: "sgdb-1234", Name: "Portal 2"}}},
		art:           map[string]Candidate{"sgdb-1234": {TileURL: "http://unused/wrong.png"}},
		artByExternal: map[string]Candidate{},
	}
	h := newHarness(t, pool, fp)
	fp.artByExternal["steam:620"] = Candidate{
		Ref:         "620",
		TileURL:     h.artSrv.URL + "/grid.png",
		HeroURL:     h.artSrv.URL + "/hero.jpg",
		Attribution: "Artwork via Fake",
	}

	appID := h.seedExternalApp(t, "Portal 2", "game", "steam", "620")
	h.svc.SweepOnce(context.Background())

	if fp.searches != 0 {
		t.Fatalf("an appid-tagged app must never enter the fuzzy path; got %d searches", fp.searches)
	}
	if fp.artCalls != 0 {
		t.Fatalf("Art (the by-title crop call) must not be used either; got %d", fp.artCalls)
	}
	if fp.externalCalls != 1 {
		t.Fatalf("want exactly one ArtByExternalRef call, got %d", fp.externalCalls)
	}

	rec, ok, err := h.svc.store.Get(context.Background(), appID)
	if err != nil || !ok {
		t.Fatalf("want an artwork row: ok=%v err=%v", ok, err)
	}
	if rec.Source != SourceProvider {
		t.Fatalf("source: want %q, got %q", SourceProvider, rec.Source)
	}
	// provider_ref is the APPID (spec §12), not a provider-internal game id.
	if rec.ProviderRef != "620" {
		t.Fatalf("provider_ref: want the appid %q, got %q", "620", rec.ProviderRef)
	}
	cover, hero := h.appURLs(t, appID)
	if cover == nil || hero == nil {
		t.Fatalf("both crops should have been cached; cover=%v hero=%v", cover, hero)
	}
	if !strings.HasPrefix(*cover, "/v1/artwork/") {
		t.Fatalf("cover_url must be a local blob path, got %q", *cover)
	}
}

// A 404 from the provider is a DECISION ("this appid has no art"), so it writes
// a negative-cache row and is never asked again.
func TestExternalRefNotFoundIsCachedAsNone(t *testing.T) {
	pool := testDB(t)
	fp := &fakeProvider{artByExternal: map[string]Candidate{}} // every id 404s
	h := newHarness(t, pool, fp)

	appID := h.seedExternalApp(t, "Some Obscure Thing", "game", "steam", "999999")
	h.svc.SweepOnce(context.Background())
	if fp.externalCalls != 1 {
		t.Fatalf("want one ArtByExternalRef call, got %d", fp.externalCalls)
	}

	rec, ok, err := h.svc.store.Get(context.Background(), appID)
	if err != nil || !ok {
		t.Fatalf("a 404 must still write a row: ok=%v err=%v", ok, err)
	}
	if rec.Source != SourceNone {
		t.Fatalf("source: want %q, got %q", SourceNone, rec.Source)
	}
	cover, hero := h.appURLs(t, appID)
	if cover != nil || hero != nil {
		t.Fatalf("a 404 keeps the gradient tile; got cover=%v hero=%v", cover, hero)
	}

	// And the negative cache holds: no second question to the third party.
	h.svc.SweepOnce(context.Background())
	if fp.externalCalls != 1 {
		t.Fatalf("a 404 must not be re-queried; got %d calls", fp.externalCalls)
	}
}

// A TRANSIENT provider failure must write NO row — otherwise one bad afternoon
// upstream permanently gradients every appid-tagged app.
func TestExternalRefTransientErrorIsNotCached(t *testing.T) {
	pool := testDB(t)
	fp := &fakeProvider{
		artByExternal: map[string]Candidate{},
		externalErr:   fmt.Errorf("upstream is down"),
	}
	h := newHarness(t, pool, fp)

	appID := h.seedExternalApp(t, "Portal 2", "game", "steam", "620")
	h.svc.SweepOnce(context.Background())

	if _, ok, err := h.svc.store.Get(context.Background(), appID); err != nil {
		t.Fatalf("get: %v", err)
	} else if ok {
		t.Fatal("a transient provider error must not write a row — it would be cached as 'no art' forever")
	}
	// The next sweep tries again, which is the whole point of not writing.
	h.svc.SweepOnce(context.Background())
	if fp.externalCalls != 2 {
		t.Fatalf("want a retry on the next sweep, got %d calls", fp.externalCalls)
	}
}

// A locked row (an admin correction) is untouched, exactly as on the fuzzy path:
// the early return in Resolve fires before any branch, appid or not.
func TestExternalRefNeverTouchesALockedRow(t *testing.T) {
	pool := testDB(t)
	fp := &fakeProvider{artByExternal: map[string]Candidate{}}
	h := newHarness(t, pool, fp)
	fp.artByExternal["steam:620"] = Candidate{TileURL: h.artSrv.URL + "/grid.png"}

	appID := h.seedExternalApp(t, "Portal 2", "game", "steam", "620")
	if resp, body := do(t, http.MethodPost,
		h.srv.URL+"/v1/admin/apps/"+appID+"/artwork/upload?crop=tile",
		h.adminToken, "image/jpeg", onePixelJPEG); resp.StatusCode != http.StatusOK {
		t.Fatalf("upload: want 200, got %d (%v)", resp.StatusCode, body)
	}
	before, _ := h.appURLs(t, appID)

	h.svc.SweepOnce(context.Background())
	if fp.externalCalls != 0 {
		t.Fatalf("an app that already has artwork must not be resolved at all; got %d calls", fp.externalCalls)
	}
	after, _ := h.appURLs(t, appID)
	if *before != *after {
		t.Fatalf("the sweep overwrote an admin override: %q → %q", *before, *after)
	}
	rec, ok, _ := h.svc.store.Get(context.Background(), appID)
	if !ok || !rec.Locked || rec.Source != SourceManual {
		t.Fatalf("the locked record must be untouched, got %+v (ok=%v)", rec, ok)
	}
}

// An app with NO external id behaves exactly as before — the branch is entered
// only when both halves of the ref are present, and a half-set pair is inert.
func TestNoExternalRefStillUsesTheSearchPath(t *testing.T) {
	pool := testDB(t)
	fp := &fakeProvider{
		results:       map[string][]Candidate{"Portal 2": {{Ref: "1234", Name: "Portal 2"}}},
		art:           map[string]Candidate{},
		artByExternal: map[string]Candidate{},
	}
	h := newHarness(t, pool, fp)
	fp.art["1234"] = Candidate{Name: "Portal 2", TileURL: h.artSrv.URL + "/grid.png"}

	// external_source set but no id: NOT a resolvable ref, so the fuzzy path.
	appID := h.seedExternalApp(t, "Portal 2", "game", "steam", "")
	h.svc.SweepOnce(context.Background())

	if fp.externalCalls != 0 {
		t.Fatalf("a half-set external ref must not take the by-appid path; got %d calls", fp.externalCalls)
	}
	if fp.searches != 1 {
		t.Fatalf("want exactly one search, got %d", fp.searches)
	}
	rec, ok, _ := h.svc.store.Get(context.Background(), appID)
	if !ok || rec.Source != SourceProvider || rec.MatchedName != "Portal 2" {
		t.Fatalf("want the ordinary by-title outcome, got %+v (ok=%v)", rec, ok)
	}
}

// ============================================================================
// §12.1 — no more unconditional candidates[0]
// ============================================================================

// THE LIVE DEFECT, as a test. The app "Steam (Dev)" matched "Steam Dev Days" on
// the real catalogue; it was 1 of 7 wrong matches out of 7. It must now record
// `source='none'` with NO provider_ref and NO matched_name — and the candidate
// the matcher declined must still be retrievable by the admin picker, which is
// what makes "do not guess" cost nothing.
func TestNonExactMatchWritesNoneAndLeavesCandidatesRetrievable(t *testing.T) {
	pool := testDB(t)
	fp := &fakeProvider{
		results: map[string][]Candidate{
			"Steam (Dev)": {{Ref: "sdd", Name: "Steam Dev Days"}},
		},
		art:           map[string]Candidate{"sdd": {TileURL: "http://unused/wrong.png"}},
		artByExternal: map[string]Candidate{},
	}
	h := newHarness(t, pool, fp)

	appID := h.seedApp(t, "Steam (Dev)", "game")
	h.svc.SweepOnce(context.Background())

	if fp.artCalls != 0 {
		t.Fatalf("a declined candidate must not have its crops fetched; got %d Art calls", fp.artCalls)
	}
	rec, ok, err := h.svc.store.Get(context.Background(), appID)
	if err != nil || !ok {
		t.Fatalf("a non-match is still a decision and must write a row: ok=%v err=%v", ok, err)
	}
	if rec.Source != SourceNone {
		t.Fatalf("source: want %q, got %q", SourceNone, rec.Source)
	}
	if rec.ProviderRef != "" || rec.MatchedName != "" {
		t.Fatalf("nothing was matched, so nothing may be recorded as matched: ref=%q name=%q",
			rec.ProviderRef, rec.MatchedName)
	}
	cover, hero := h.appURLs(t, appID)
	if cover != nil || hero != nil {
		t.Fatalf("a non-match renders the gradient; got cover=%v hero=%v", cover, hero)
	}

	// The admin picker still sees what the automatic path refused to pick.
	resp, body := do(t, http.MethodPost, h.srv.URL+"/v1/admin/apps/"+appID+"/artwork/search",
		h.adminToken, "application/json", jsonBody(t, map[string]any{}))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("search: want 200, got %d (%v)", resp.StatusCode, body)
	}
	cands, _ := body["candidates"].([]any)
	if len(cands) != 1 {
		t.Fatalf("the declined candidate must still be offered to an admin, got %v", body["candidates"])
	}
}

// An exact match further down the list is taken; provider order only breaks ties
// AMONG exact matches, it never promotes a near miss.
func TestExactMatchIsPreferredOverProviderOrder(t *testing.T) {
	pool := testDB(t)
	fp := &fakeProvider{
		results: map[string][]Candidate{
			"Portal": {
				{Ref: "knights", Name: "Portal Knights"},
				{Ref: "reloaded", Name: "Portal Reloaded"},
				{Ref: "portal", Name: "Portal"},
			},
		},
		art:           map[string]Candidate{},
		artByExternal: map[string]Candidate{},
	}
	h := newHarness(t, pool, fp)
	fp.art["portal"] = Candidate{Name: "Portal", TileURL: h.artSrv.URL + "/grid.png"}

	appID := h.seedApp(t, "Portal", "game")
	h.svc.SweepOnce(context.Background())

	rec, ok, _ := h.svc.store.Get(context.Background(), appID)
	if !ok || rec.Source != SourceProvider || rec.ProviderRef != "portal" {
		t.Fatalf("want the exact match at position 3, got %+v (ok=%v)", rec, ok)
	}
}

// ============================================================================
// Title normalisation (pure — no database)
// ============================================================================

func TestNormaliseTitle(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"Portal 2", "portal 2"},
		{"PORTAL 2", "portal 2"},
		{"Portal 2™", "portal 2"},
		{"Portal 2®", "portal 2"},
		{"  Portal   2  ", "portal 2"},
		{"Half-Life: Alyx", "half life alyx"},
		{"Steam (Dev)", "steam dev"},
		{"Steam Dev Days", "steam dev days"},
		{"Portal 2 Game of the Year Edition", "portal 2"},
		{"Portal 2 - Definitive Edition", "portal 2"},
		{"Portal 2 GOTY", "portal 2"},
		{"Portal 2 Remastered", "portal 2"},
		// A game genuinely called by an edition word keeps its name — the suffix
		// rule only ever strips something that FOLLOWS a title.
		{"GOTY", "goty"},
		{"Remastered", "remastered"},
		// Nothing survivable in, nothing out (never a wildcard "" match).
		{"!!!", ""},
		{"", ""},
	} {
		if got := normaliseTitle(tc.in); got != tc.want {
			t.Errorf("normaliseTitle(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The exactness rule, at the level it is actually applied — including the live
// failing pair, which is the one case that must never match again.
func TestPickExactTitleMatch(t *testing.T) {
	for _, tc := range []struct {
		name      string
		appName   string
		cands     []Candidate
		wantRef   string
		wantMatch bool
	}{
		{
			name:    "the live 7-for-7 defect: a near miss is NOT a match",
			appName: "Steam (Dev)",
			cands:   []Candidate{{Ref: "sdd", Name: "Steam Dev Days"}},
		},
		{
			name:      "an exact title matches",
			appName:   "Portal 2",
			cands:     []Candidate{{Ref: "p2", Name: "Portal 2"}},
			wantRef:   "p2",
			wantMatch: true,
		},
		{
			name:      "punctuation and case are not differences",
			appName:   "half-life: ALYX",
			cands:     []Candidate{{Ref: "hla", Name: "Half-Life™: Alyx"}},
			wantRef:   "hla",
			wantMatch: true,
		},
		{
			name:      "an edition suffix on the provider side is not a difference",
			appName:   "Portal 2",
			cands:     []Candidate{{Ref: "goty", Name: "Portal 2 Game of the Year Edition"}},
			wantRef:   "goty",
			wantMatch: true,
		},
		{
			name:      "the first exact match wins, in provider order",
			appName:   "Portal",
			cands:     []Candidate{{Ref: "near", Name: "Portal Knights"}, {Ref: "a", Name: "Portal"}, {Ref: "b", Name: "Portal™"}},
			wantRef:   "a",
			wantMatch: true,
		},
		{
			name:    "a prefix is not a match",
			appName: "Portal",
			cands:   []Candidate{{Ref: "k", Name: "Portal Knights"}, {Ref: "r", Name: "Portal Reloaded"}},
		},
		{
			name:    "a superstring app name is not a match either",
			appName: "Portal Knights",
			cands:   []Candidate{{Ref: "p", Name: "Portal"}},
		},
		{
			name:    "an app whose name normalises to nothing matches nothing",
			appName: "!!!",
			cands:   []Candidate{{Ref: "x", Name: "???"}},
		},
		{
			name:    "no candidates at all",
			appName: "Portal",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := pickExactTitleMatch(tc.appName, tc.cands)
			if ok != tc.wantMatch {
				t.Fatalf("matched = %v, want %v (picked %+v)", ok, tc.wantMatch, got)
			}
			if ok && got.Ref != tc.wantRef {
				t.Fatalf("ref = %q, want %q", got.Ref, tc.wantRef)
			}
		})
	}
}

// ============================================================================
// The SteamGridDB client's by-appid path (stub server, never the real service)
// ============================================================================

// The by-appid endpoints are /grids/steam/<appid> and /heroes/steam/<appid> —
// the platform-id form, not the /grids/game/<id> form Search feeds.
func TestSteamGridDBArtByExternalRefUsesTheSteamEndpoints(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/grids/"):
			fmt.Fprint(w, `{"success":true,"data":[{"id":1,"score":9,"width":600,"height":900,"url":"https://cdn/tile.png","thumb":"https://cdn/t.png"}]}`)
		default:
			fmt.Fprint(w, `{"success":true,"data":[{"id":2,"score":9,"width":1920,"height":620,"url":"https://cdn/hero.png"}]}`)
		}
	}))
	defer srv.Close()

	c := NewSteamGridDBClient("k", srv.URL, srv.Client())
	art, err := c.ArtByExternalRef(context.Background(), ExternalSourceSteam, "620")
	if err != nil {
		t.Fatalf("ArtByExternalRef: %v", err)
	}
	if art.Ref != "620" {
		t.Errorf("Ref: want the appid %q, got %q", "620", art.Ref)
	}
	if art.TileURL != "https://cdn/tile.png" || art.HeroURL != "https://cdn/hero.png" {
		t.Errorf("crops: %+v", art)
	}
	if art.Attribution != sgdbAttribution {
		t.Errorf("attribution: want %q, got %q", sgdbAttribution, art.Attribution)
	}
	want := []string{"/grids/steam/620", "/heroes/steam/620"}
	if len(paths) != 2 || paths[0] != want[0] || paths[1] != want[1] {
		t.Fatalf("paths = %v, want %v", paths, want)
	}
}

// A 404 on BOTH endpoints is ErrArtNotFound — the sentinel the service reads to
// decide "write a none row" rather than "retry next sweep".
func TestSteamGridDBArtByExternalRefReports404AsNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := NewSteamGridDBClient("k", srv.URL, srv.Client())
	if _, err := c.ArtByExternalRef(context.Background(), ExternalSourceSteam, "1"); !errors.Is(err, ErrArtNotFound) {
		t.Fatalf("want ErrArtNotFound, got %v", err)
	}
}

// A 5xx is transient and must NOT be reported as "not found" — the two lead to
// opposite decisions in the service.
func TestSteamGridDBArtByExternalRefKeeps5xxTransient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	c := NewSteamGridDBClient("k", srv.URL, srv.Client())
	_, err := c.ArtByExternalRef(context.Background(), ExternalSourceSteam, "1")
	if err == nil {
		t.Fatal("a 502 must be an error")
	}
	if errors.Is(err, ErrArtNotFound) {
		t.Fatalf("a 502 must not masquerade as a 404: %v", err)
	}
}

// An unknown source is an ERROR, never a silent miss — a silent miss would be
// cached as `source='none'` and the misconfiguration would be invisible.
func TestSteamGridDBArtByExternalRefRejectsAnUnknownSource(t *testing.T) {
	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer srv.Close()

	c := NewSteamGridDBClient("k", srv.URL, srv.Client())
	_, err := c.ArtByExternalRef(context.Background(), "epic", "620")
	if !errors.Is(err, ErrUnsupportedExternalSource) {
		t.Fatalf("want ErrUnsupportedExternalSource, got %v", err)
	}
	if called {
		t.Fatal("an unsupported source must not reach the network")
	}
}

// The appid grammar is enforced before the value can become part of a URL path.
func TestSteamGridDBArtByExternalRefRejectsAMalformedAppID(t *testing.T) {
	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer srv.Close()

	c := NewSteamGridDBClient("k", srv.URL, srv.Client())
	for _, id := range []string{"", "0", "007", "620 -foo", "1; rm -rf /", "99999999999", "-620"} {
		if _, err := c.ArtByExternalRef(context.Background(), ExternalSourceSteam, id); err == nil {
			t.Errorf("appid %q must be rejected", id)
		}
	}
	if called {
		t.Fatal("a malformed appid must not reach the network")
	}
}

// The by-TITLE path's contract is unchanged by the sentinel: a double 404 there
// is still an empty Candidate with a nil error, which is what ApplyCandidate's
// "that match has no usable artwork" 400 rests on.
func TestSteamGridDBArtStillSwallows404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := NewSteamGridDBClient("k", srv.URL, srv.Client())
	art, err := c.Art(context.Background(), "1234")
	if err != nil {
		t.Fatalf("Art must absorb a 404 as it always has, got %v", err)
	}
	if art.TileURL != "" || art.HeroURL != "" {
		t.Fatalf("want no crops, got %+v", art)
	}
}
