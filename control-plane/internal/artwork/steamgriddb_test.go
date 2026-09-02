package artwork

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Every test here runs against a STUBBED HTTP server. Nothing in this package
// ever contacts the real SteamGridDB service — the client is exercised for
// request shape, auth header, envelope decoding, crop selection and error
// mapping, all of which are decidable without a live call or an API key.

type stubSGDB struct {
	t *testing.T
	// requests records the path+query of every call, in order.
	requests []string
	// responses maps a path prefix to (status, body).
	responses map[string]stubResponse
	authSeen  string
}

type stubResponse struct {
	status int
	body   string
}

func (s *stubSGDB) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.authSeen = r.Header.Get("Authorization")
		full := r.URL.RequestURI()
		s.requests = append(s.requests, full)
		for prefix, resp := range s.responses {
			if strings.HasPrefix(r.URL.Path, prefix) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(resp.status)
				_, _ = w.Write([]byte(resp.body))
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"success":false,"errors":["not found"]}`))
	})
}

func newStubProvider(t *testing.T, responses map[string]stubResponse) (*SteamGridDBClient, *stubSGDB) {
	t.Helper()
	stub := &stubSGDB{t: t, responses: responses}
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)
	c := NewSteamGridDBClient("test-key", srv.URL, &http.Client{Timeout: 5 * time.Second})
	// The production throttle spaces calls half a second apart, which would add
	// seconds to the suite for no coverage. The throttle itself is asserted
	// separately in TestThrottleSpacesCalls.
	c.lastCall = time.Now().Add(-time.Hour)
	return c, stub
}

// --- search -----------------------------------------------------------------

func TestSearchParsesEnvelope(t *testing.T) {
	c, stub := newStubProvider(t, map[string]stubResponse{
		"/search/autocomplete/": {http.StatusOK, `{"success":true,"data":[
			{"id":1234,"name":"Portal 2"},
			{"id":5678,"name":"Portal 2: Community Update"}
		]}`},
	})
	got, err := c.Search(context.Background(), "Portal 2")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("candidates: want 2, got %d", len(got))
	}
	if got[0].Ref != "1234" || got[0].Name != "Portal 2" {
		t.Fatalf("first candidate: got %+v", got[0])
	}
	if got[0].Attribution == "" {
		t.Fatal("candidates must carry the attribution line")
	}
	if stub.authSeen != "Bearer test-key" {
		t.Fatalf("Authorization: want %q, got %q", "Bearer test-key", stub.authSeen)
	}
}

// A title with characters that must be escaped must not corrupt the path.
func TestSearchEscapesQuery(t *testing.T) {
	c, stub := newStubProvider(t, map[string]stubResponse{
		"/search/autocomplete/": {http.StatusOK, `{"success":true,"data":[]}`},
	})
	if _, err := c.Search(context.Background(), "Tom Clancy's H.A.W.X 2/3"); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(stub.requests) != 1 {
		t.Fatalf("requests: want 1, got %d", len(stub.requests))
	}
	// The slash in the title must be percent-encoded, not become a path segment.
	if strings.Count(stub.requests[0], "/") != 3 { // /search/autocomplete/<term>
		t.Fatalf("query was not escaped into a single path segment: %q", stub.requests[0])
	}
}

// An empty query short-circuits without a request — no point asking a third
// party about "".
func TestSearchEmptyQueryMakesNoRequest(t *testing.T) {
	c, stub := newStubProvider(t, map[string]stubResponse{
		"/search/autocomplete/": {http.StatusOK, `{"success":true,"data":[]}`},
	})
	got, err := c.Search(context.Background(), "   ")
	if err != nil || got != nil {
		t.Fatalf("want (nil, nil), got (%v, %v)", got, err)
	}
	if len(stub.requests) != 0 {
		t.Fatalf("want no outbound request, got %v", stub.requests)
	}
}

// No match is a normal outcome, not an error — the whole desktop-app path
// depends on this.
func TestSearchNoMatchIsNotAnError(t *testing.T) {
	c, _ := newStubProvider(t, map[string]stubResponse{
		"/search/autocomplete/": {http.StatusOK, `{"success":true,"data":[]}`},
	})
	got, err := c.Search(context.Background(), "Blender")
	if err != nil {
		t.Fatalf("want no error for an unmatched title, got %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want no candidates, got %d", len(got))
	}
}

// --- art / the two crops ----------------------------------------------------

// The core "two crops" assertion: the tile comes from a PORTRAIT grid and the
// hero from a separate wide hero asset, and the two requests ask for different
// dimensions.
func TestArtRequestsTwoDistinctCrops(t *testing.T) {
	c, stub := newStubProvider(t, map[string]stubResponse{
		"/grids/game/": {http.StatusOK, `{"success":true,"data":[
			{"id":1,"score":10,"width":600,"height":900,"mime":"image/png",
			 "url":"https://cdn2.example.test/grid/wide.png","thumb":"https://cdn2.example.test/thumb/wide.png"}
		]}`},
		"/heroes/game/": {http.StatusOK, `{"success":true,"data":[
			{"id":2,"score":8,"width":1920,"height":620,"mime":"image/jpeg",
			 "url":"https://cdn2.example.test/hero/wide.jpg","thumb":"https://cdn2.example.test/thumb/hero.jpg"}
		]}`},
	})

	art, err := c.Art(context.Background(), "1234")
	if err != nil {
		t.Fatalf("Art: %v", err)
	}
	if art.TileURL != "https://cdn2.example.test/grid/wide.png" {
		t.Fatalf("tile url: got %q", art.TileURL)
	}
	if art.HeroURL != "https://cdn2.example.test/hero/wide.jpg" {
		t.Fatalf("hero url: got %q", art.HeroURL)
	}
	if art.TileURL == art.HeroURL {
		t.Fatal("tile and hero must be different assets, not one image reused")
	}
	if len(stub.requests) != 2 {
		t.Fatalf("want one grid request and one hero request, got %v", stub.requests)
	}

	// #385: the tile frame is 2:3, so the grid query asks for SteamGridDB's
	// PORTRAIT dimensions — 600x900 (exactly 2:3) first, then the two Steam
	// library-capsule sizes (~0.71, close enough to share a 2:3 frame under
	// object-fit: cover). The wide 920x430/460x215 grids and the square
	// 512x512/1024x1024 ones are deliberately never requested.
	gridQ := mustQuery(t, stub.requests[0])
	if got := gridQ.Get("dimensions"); got != "600x900,660x930,342x482" {
		t.Fatalf("grid must request the PORTRAIT dimensions for a 2:3 tile; got %q", got)
	}
	heroQ := mustQuery(t, stub.requests[1])
	if got := heroQ.Get("dimensions"); got != "1920x620,3840x1240,1600x650" {
		t.Fatalf("hero must request the wide banner dimensions; got %q", got)
	}
	for _, q := range []url.Values{gridQ, heroQ} {
		if q.Get("nsfw") != "false" || q.Get("humor") != "false" {
			t.Fatalf("both queries must filter nsfw/humor; got %v", q)
		}
		if q.Get("types") != "static" {
			t.Fatalf("both queries must ask for static images; got %q", q.Get("types"))
		}
	}
}

// A game with a grid but no hero yields half the art rather than failing —
// the hero panel then falls back, which is better than no art at all.
func TestArtToleratesAMissingCrop(t *testing.T) {
	c, _ := newStubProvider(t, map[string]stubResponse{
		"/grids/game/": {http.StatusOK, `{"success":true,"data":[
			{"id":1,"score":1,"width":600,"height":900,"url":"https://cdn2.example.test/g.png"}
		]}`},
		"/heroes/game/": {http.StatusOK, `{"success":true,"data":[]}`},
	})
	art, err := c.Art(context.Background(), "1")
	if err != nil {
		t.Fatalf("Art: %v", err)
	}
	if art.TileURL == "" {
		t.Fatal("tile should be present")
	}
	if art.HeroURL != "" {
		t.Fatalf("hero should be empty, got %q", art.HeroURL)
	}
}

// Highest community score wins; area breaks a tie. All three are on-target
// portrait here, so the aspect bucket is uniform and does not participate.
func TestPickBestOrdersByScoreThenArea(t *testing.T) {
	best, ok := pickBest([]sgdbImage{
		{Score: 3, Width: 600, Height: 900, URL: "low"},
		{Score: 9, Width: 600, Height: 900, URL: "winner"},
		{Score: 9, Width: 300, Height: 450, URL: "smaller-same-score"},
	}, TileAspect)
	if !ok || best.URL != "winner" {
		t.Fatalf("want the highest-scored, largest image; got %+v (ok=%v)", best, ok)
	}
}

// NSFW/humor assets are filtered client-side too — a query parameter is a
// request, not a guarantee about the response.
func TestPickBestFiltersNSFWAndHumorAndEmptyURLs(t *testing.T) {
	if _, ok := pickBest([]sgdbImage{
		{Score: 100, URL: "a", NSFW: true},
		{Score: 90, URL: "b", Humor: true},
		{Score: 80, URL: ""},
	}, TileAspect); ok {
		t.Fatal("every candidate was unusable; want ok=false")
	}
	best, ok := pickBest([]sgdbImage{
		{Score: 100, URL: "nsfw", NSFW: true},
		{Score: 1, URL: "clean"},
	}, TileAspect)
	if !ok || best.URL != "clean" {
		t.Fatalf("want the clean asset, got %+v", best)
	}
}

// --- #385 Phase 4: the defensive aspect preference --------------------------

// Aspect discipline used to live ENTIRELY in the query string's `dimensions=`,
// which assumes the provider honours the filter — an assumption nothing
// verified. If it returns landscape anyway, a wide asset must not win the 2:3
// tile frame just because the community scored it higher.
func TestPickBestPrefersTheTargetAspectOverAHigherScore(t *testing.T) {
	best, ok := pickBest([]sgdbImage{
		{Score: 500, Width: 920, Height: 430, URL: "wide-but-popular"},
		{Score: 1, Width: 600, Height: 900, URL: "portrait"},
	}, TileAspect)
	if !ok || best.URL != "portrait" {
		t.Fatalf("a portrait asset must win the 2:3 tile even at a lower score; got %+v", best)
	}
}

// A square grid (SteamGridDB offers 512x512 and 1024x1024) is not a tile.
func TestPickBestTreatsSquareArtAsOffTargetForTheTile(t *testing.T) {
	best, ok := pickBest([]sgdbImage{
		{Score: 99, Width: 1024, Height: 1024, URL: "square"},
		{Score: 2, Width: 342, Height: 482, URL: "capsule"},
	}, TileAspect)
	if !ok || best.URL != "capsule" {
		t.Fatalf("a square asset must not outrank a portrait capsule; got %+v", best)
	}
}

// PREFER, NEVER REJECT. A slightly-off crop under object-fit:cover beats a
// gradient fallback for an app that demonstrably has art, so an all-landscape
// response must still yield the best of what is there.
func TestPickBestNeverRejectsWhenNothingMatchesTheAspect(t *testing.T) {
	best, ok := pickBest([]sgdbImage{
		{Score: 1, Width: 460, Height: 215, URL: "small-wide"},
		{Score: 7, Width: 920, Height: 430, URL: "best-wide"},
	}, TileAspect)
	if !ok {
		t.Fatal("no art at all is worse than a slightly-off crop; want ok=true")
	}
	if best.URL != "best-wide" {
		t.Fatalf("with a uniform bucket the score comparator decides; got %+v", best)
	}
}

// The three dimensions the HERO query asks for all sit in one bucket, so the
// hero's selection order is unchanged by this addition — score, then area.
// (The hero query itself is untouched by #385; this pins that.)
func TestHeroAspectBucketHoldsEveryRequestedHeroDimension(t *testing.T) {
	for _, dim := range []struct{ w, h int }{{1920, 620}, {3840, 1240}, {1600, 650}} {
		if !onTargetAspect(sgdbImage{Width: dim.w, Height: dim.h}, HeroAspect) {
			t.Errorf("%dx%d must count as on-target for the hero", dim.w, dim.h)
		}
	}
	best, ok := pickBest([]sgdbImage{
		{Score: 2, Width: 1920, Height: 620, URL: "big-low-score"},
		{Score: 9, Width: 1600, Height: 650, URL: "smaller-high-score"},
	}, HeroAspect)
	if !ok || best.URL != "smaller-high-score" {
		t.Fatalf("hero ordering must still be score-first across the requested dimensions; got %+v", best)
	}
}

// Every dimension each query asks for must be on-target for its own frame, and
// off-target for the other. This is what keeps the two crops genuinely distinct.
func TestRequestedDimensionsBucketWithTheirOwnCrop(t *testing.T) {
	tiles := []struct{ w, h int }{{600, 900}, {660, 930}, {342, 482}}
	heroes := []struct{ w, h int }{{1920, 620}, {3840, 1240}, {1600, 650}}
	for _, d := range tiles {
		if !onTargetAspect(sgdbImage{Width: d.w, Height: d.h}, TileAspect) {
			t.Errorf("tile %dx%d must be on-target for the tile", d.w, d.h)
		}
		if onTargetAspect(sgdbImage{Width: d.w, Height: d.h}, HeroAspect) {
			t.Errorf("tile %dx%d must NOT be on-target for the hero", d.w, d.h)
		}
	}
	for _, d := range heroes {
		if !onTargetAspect(sgdbImage{Width: d.w, Height: d.h}, HeroAspect) {
			t.Errorf("hero %dx%d must be on-target for the hero", d.w, d.h)
		}
		if onTargetAspect(sgdbImage{Width: d.w, Height: d.h}, TileAspect) {
			t.Errorf("hero %dx%d must NOT be on-target for the tile", d.w, d.h)
		}
	}
}

// An asset with no dimensions has an UNKNOWN shape, which is not evidence of a
// good one — it must not be promoted, and must not divide by zero either.
func TestPickBestTreatsMissingDimensionsAsOffTarget(t *testing.T) {
	if onTargetAspect(sgdbImage{Width: 600, Height: 0}, TileAspect) {
		t.Error("a zero height must not count as on-target")
	}
	best, ok := pickBest([]sgdbImage{
		{Score: 50, URL: "no-dimensions"},
		{Score: 1, Width: 600, Height: 900, URL: "portrait"},
	}, TileAspect)
	if !ok || best.URL != "portrait" {
		t.Fatalf("a known-good shape must outrank an unknown one; got %+v", best)
	}
}

func TestArtRejectsEmptyRef(t *testing.T) {
	c, _ := newStubProvider(t, nil)
	if _, err := c.Art(context.Background(), " "); err == nil {
		t.Fatal("want an error for an empty ref")
	}
}

// --- error mapping ----------------------------------------------------------

func TestUnauthorizedIsDistinct(t *testing.T) {
	c, _ := newStubProvider(t, map[string]stubResponse{
		"/search/autocomplete/": {http.StatusUnauthorized, `{"success":false,"errors":["bad key"]}`},
	})
	_, err := c.Search(context.Background(), "anything")
	if err == nil || !strings.Contains(err.Error(), "API key") {
		t.Fatalf("a 401 must name the API key so an operator can act on it; got %v", err)
	}
}

// SteamGridDB publishes no rate limit, so a 429 is possible at any time and
// must be identifiable rather than looking like a generic outage.
func TestRateLimitIsDistinct(t *testing.T) {
	c, _ := newStubProvider(t, map[string]stubResponse{
		"/search/autocomplete/": {http.StatusTooManyRequests, `{"success":false}`},
	})
	_, err := c.Search(context.Background(), "anything")
	if err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("want a rate-limit error, got %v", err)
	}
}

// A 404 from the provider means "no such game" — a miss, not a failure.
func TestNotFoundIsAMiss(t *testing.T) {
	c, _ := newStubProvider(t, map[string]stubResponse{}) // stub 404s everything
	got, err := c.Search(context.Background(), "nothing here")
	if err != nil {
		t.Fatalf("a 404 must not be an error, got %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want no candidates, got %d", len(got))
	}
}

func TestServerErrorIsAnError(t *testing.T) {
	c, _ := newStubProvider(t, map[string]stubResponse{
		"/search/autocomplete/": {http.StatusInternalServerError, `{}`},
	})
	if _, err := c.Search(context.Background(), "x"); err == nil {
		t.Fatal("a 500 from the provider must surface as an error")
	}
}

func TestMalformedJSONIsAnError(t *testing.T) {
	c, _ := newStubProvider(t, map[string]stubResponse{
		"/search/autocomplete/": {http.StatusOK, `{"success":true,"data":[`},
	})
	if _, err := c.Search(context.Background(), "x"); err == nil {
		t.Fatal("a truncated body must surface as an error")
	}
}

// The throttle must actually delay a second call. Undocumented rate limits are
// the reason this exists, so it is worth asserting rather than assuming.
func TestThrottleSpacesCalls(t *testing.T) {
	c, _ := newStubProvider(t, map[string]stubResponse{
		"/search/autocomplete/": {http.StatusOK, `{"success":true,"data":[]}`},
	})
	ctx := context.Background()
	if _, err := c.Search(ctx, "one"); err != nil {
		t.Fatalf("first: %v", err)
	}
	start := time.Now()
	if _, err := c.Search(ctx, "two"); err != nil {
		t.Fatalf("second: %v", err)
	}
	if elapsed := time.Since(start); elapsed < sgdbMinInterval/2 {
		t.Fatalf("second call was not throttled (elapsed %v, min interval %v)", elapsed, sgdbMinInterval)
	}
}

func TestProviderName(t *testing.T) {
	c, _ := newStubProvider(t, nil)
	if c.Name() != "steamgriddb" {
		t.Fatalf("Name: got %q", c.Name())
	}
}

func mustQuery(t *testing.T, requestURI string) url.Values {
	t.Helper()
	u, err := url.ParseRequestURI(requestURI)
	if err != nil {
		t.Fatalf("parse %q: %v", requestURI, err)
	}
	return u.Query()
}

// #80: autocomplete carries no images, so Search must resolve previews itself
// — in ONE extra request (the throttle makes per-candidate lookups cost
// seconds while an admin watches the picker).
func TestSearchFillsThumbsInOneRequest(t *testing.T) {
	c, stub := newStubProvider(t, map[string]stubResponse{
		"/search/autocomplete/": {http.StatusOK, `{"success":true,"data":[
			{"id":1234,"name":"Portal 2"},
			{"id":5678,"name":"Portal 2: Community Update"}
		]}`},
		"/grids/game/": {http.StatusOK, `{"success":true,"data":[
			{"success":true,"data":[{"id":1,"width":600,"height":900,"score":5,"url":"https://cdn/full-a.png","thumb":"https://cdn/thumb-a.png"}]},
			{"success":true,"data":[{"id":2,"width":600,"height":900,"score":5,"url":"https://cdn/full-b.png","thumb":"https://cdn/thumb-b.png"}]}
		]}`},
	})
	got, err := c.Search(context.Background(), "Portal 2")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 2 || got[0].ThumbURL != "https://cdn/thumb-a.png" || got[1].ThumbURL != "https://cdn/thumb-b.png" {
		t.Fatalf("thumbs not filled from the multi-id grids response: %+v", got)
	}
	if len(stub.requests) != 2 {
		t.Fatalf("want exactly 2 requests (autocomplete + one grids), got %v", stub.requests)
	}
	if !strings.Contains(stub.requests[1], "/grids/game/1234,5678?") {
		t.Fatalf("grids request must carry every candidate id: %q", stub.requests[1])
	}
	if !strings.Contains(stub.requests[1], "dimensions=600x900") {
		t.Fatalf("grids request must reuse the portrait-tile filter: %q", stub.requests[1])
	}
}

// A single candidate gets SteamGridDB's FLAT single-game response shape, not
// the nested multi-id one.
func TestSearchSingleCandidateUsesFlatShape(t *testing.T) {
	c, _ := newStubProvider(t, map[string]stubResponse{
		"/search/autocomplete/": {http.StatusOK, `{"success":true,"data":[{"id":42,"name":"Blender"}]}`},
		"/grids/game/": {http.StatusOK, `{"success":true,"data":[
			{"id":1,"width":600,"height":900,"score":5,"url":"https://cdn/full.png","thumb":"https://cdn/thumb.png"}
		]}`},
	})
	got, err := c.Search(context.Background(), "Blender")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 || got[0].ThumbURL != "https://cdn/thumb.png" {
		t.Fatalf("single-candidate thumb not filled: %+v", got)
	}
}

// Previews are best-effort: a failing grids lookup must leave the search
// result intact (the picker's glyph fallback is the degraded path), never
// fail it.
func TestSearchSurvivesThumbLookupFailure(t *testing.T) {
	c, _ := newStubProvider(t, map[string]stubResponse{
		"/search/autocomplete/": {http.StatusOK, `{"success":true,"data":[
			{"id":1234,"name":"Portal 2"},
			{"id":5678,"name":"Portal 2: Community Update"}
		]}`},
		"/grids/game/": {http.StatusInternalServerError, `{"success":false,"errors":["boom"]}`},
	})
	got, err := c.Search(context.Background(), "Portal 2")
	if err != nil {
		t.Fatalf("a thumb failure must not fail the search: %v", err)
	}
	if len(got) != 2 || got[0].ThumbURL != "" || got[1].ThumbURL != "" {
		t.Fatalf("candidates must survive unfilled: %+v", got)
	}
}
