package artwork

// Amendment 38: the logo and icon crops. DB-backed; reuses handler_test.go's
// harness and fakeProvider.

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

// logoCDN serves a logo and an icon whose bytes differ from each other and from
// the harness CDN's one-pixel crops, so each gets its own content-addressed blob.
func logoCDN(t *testing.T) string {
	t.Helper()
	encode := func(w, h int) []byte {
		var b bytes.Buffer
		if err := png.Encode(&b, image.NewNRGBA(image.Rect(0, 0, w, h))); err != nil {
			t.Fatal(err)
		}
		return b.Bytes()
	}
	files := map[string][]byte{"/logo.png": encode(4, 1), "/icon.png": encode(2, 2),
		"/logo2.png": encode(5, 1), "/tile2.png": encode(2, 3), "/hero2.png": encode(6, 2)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, ok := files[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(data)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func (h *harness) logoIconURLs(t *testing.T, appID string) (logo, icon *string) {
	t.Helper()
	if err := h.pool.QueryRow(context.Background(),
		`SELECT logo_url, icon_url FROM apps WHERE id::text = $1`, appID).Scan(&logo, &icon); err != nil {
		t.Fatalf("read logo/icon urls: %v", err)
	}
	return logo, icon
}

func (h *harness) cached(t *testing.T, asset string) bool {
	t.Helper()
	ok, err := h.svc.blobs.Has(asset)
	if err != nil {
		t.Fatalf("has %s: %v", asset, err)
	}
	return ok
}

func (h *harness) record(t *testing.T, appID string) Record {
	t.Helper()
	rec, ok, err := h.svc.store.Get(context.Background(), appID)
	if err != nil || !ok {
		t.Fatalf("get %s: ok=%v err=%v", appID, ok, err)
	}
	return rec
}

// appidApp seeds a Steam-tagged app the fake resolves with all four crops.
func appidApp(t *testing.T, h *harness, fp *fakeProvider, cdn, appid string) string {
	t.Helper()
	fp.artByExternal["steam:"+appid] = Candidate{Name: "Portal 2", TileURL: h.artSrv.URL + "/grid.png", HeroURL: h.artSrv.URL + "/hero.jpg"}
	fp.logoIcon["steam:"+appid] = [2]string{cdn + "/logo.png", cdn + "/icon.png"}
	return h.seedExternalApp(t, "Portal 2", "game", "steam", appid)
}

func newLogoHarness(t *testing.T) (*harness, *fakeProvider, string) {
	pool := testDB(t)
	fp := &fakeProvider{artByExternal: map[string]Candidate{}, art: map[string]Candidate{},
		results: map[string][]Candidate{}, logoIcon: map[string][2]string{}}
	return newHarness(t, pool, fp), fp, logoCDN(t)
}

func TestResolveFetchesLogoAndIconWithTheCrops(t *testing.T) {
	h, fp, cdn := newLogoHarness(t)
	byAppid := appidApp(t, h, fp, cdn, "620")
	fp.results["Hades"] = []Candidate{{Ref: "77", Name: "Hades"}}
	fp.art["77"] = Candidate{Ref: "77", Name: "Hades", TileURL: h.artSrv.URL + "/grid.png"}
	fp.logoIcon[":77"] = [2]string{cdn + "/logo.png", ""}
	byTitle := h.seedApp(t, "Hades", "game")

	h.svc.SweepOnce(context.Background())

	rec := h.record(t, byAppid)
	if rec.LogoAsset == "" || rec.IconAsset == "" || rec.LogoAsset == rec.IconAsset || !rec.LogoIconChecked {
		t.Fatalf("by appid: %+v", rec)
	}
	logo, icon := h.logoIconURLs(t, byAppid)
	if logo == nil || *logo != AssetURL(rec.LogoAsset) || icon == nil || *icon != AssetURL(rec.IconAsset) {
		t.Fatalf("urls %v %v", logo, icon)
	}
	if resp, _ := do(t, http.MethodGet, h.srv.URL+*logo, "", "", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("logo not served: %d", resp.StatusCode)
	}
	rec = h.record(t, byTitle)
	if rec.LogoAsset == "" || rec.IconAsset != "" || !rec.LogoIconChecked {
		t.Fatalf("by title: %+v", rec)
	}
	if fp.logoIconCalls != 2 {
		t.Fatalf("LogoIcon calls %d, want 2", fp.logoIconCalls)
	}
}

func TestNoTileOrHeroMeansNoLogoRequest(t *testing.T) {
	h, fp, _ := newLogoHarness(t)
	appID := h.seedExternalApp(t, "Nothing", "game", "steam", "999") // provider 404s it
	h.svc.SweepOnce(context.Background())
	if rec := h.record(t, appID); rec.Source != SourceNone || fp.logoIconCalls != 0 {
		t.Fatalf("%+v, %d LogoIcon calls", rec, fp.logoIconCalls)
	}
	h.svc.SweepOnce(context.Background())
	if fp.logoIconCalls != 0 {
		t.Fatal("the backfill asked about a none record")
	}
}

func TestLogoIconErrorDoesNotHoldBackTheCropsAndIsRetried(t *testing.T) {
	h, fp, cdn := newLogoHarness(t)
	appID := appidApp(t, h, fp, cdn, "620")
	fp.logoIconErr = errTransient

	h.svc.SweepOnce(context.Background())
	if cover, _ := h.appURLs(t, appID); cover == nil {
		t.Fatal("a logo outage held back the tile")
	}
	if rec := h.record(t, appID); rec.LogoIconChecked || rec.LogoAsset != "" {
		t.Fatalf("%+v", rec)
	}

	fp.logoIconErr = nil
	res := h.svc.SweepOnce(context.Background())
	if res.LogoIconFilled != 1 || res.AppsConsidered != 0 || fp.externalCalls != 1 {
		t.Fatalf("result %+v, %d ArtByExternalRef calls: want a backfill, no re-match", res, fp.externalCalls)
	}
	if logo, icon := h.logoIconURLs(t, appID); logo == nil || icon == nil {
		t.Fatal("backfill did not fill the urls")
	}
	// Sweep 1 asked twice (its resolve, then its backfill), sweep 2 once.
	h.svc.SweepOnce(context.Background())
	if fp.logoIconCalls != 3 {
		t.Fatalf("an answered record was asked again: %d calls", fp.logoIconCalls)
	}
}

// A record made before amendment 38 is filled by the sweep from its own reference;
// a locked record is never touched.
func TestBackfillFillsOldProviderRecordsOnlyAndKeepsTheRest(t *testing.T) {
	h, fp, cdn := newLogoHarness(t)
	ctx := context.Background()
	old := appidApp(t, h, fp, cdn, "620")
	locked := appidApp(t, h, fp, cdn, "400")
	// Resolved with no logo or icon answer: the state of every record before 0104.
	fp.logoIconErr = errTransient
	h.svc.SweepOnce(ctx)
	fp.logoIconErr, fp.logoIconCalls = nil, 0
	if _, err := h.pool.Exec(ctx, `UPDATE app_artwork SET source = 'manual', locked = true WHERE app_id::text = $1`, locked); err != nil {
		t.Fatal(err)
	}
	before := h.record(t, old)

	res := h.svc.SweepOnce(ctx)
	if res.LogoIconFilled != 1 || fp.logoIconCalls != 1 {
		t.Fatalf("result %+v, %d calls: want only the unlocked provider record", res, fp.logoIconCalls)
	}
	after := h.record(t, old)
	if after.LogoAsset == "" || after.IconAsset == "" || !after.LogoIconChecked ||
		after.Source != before.Source || after.ProviderRef != before.ProviderRef || after.TileAsset != before.TileAsset {
		t.Fatalf("before %+v\nafter %+v", before, after)
	}
	if rec := h.record(t, locked); rec.LogoAsset != "" || rec.LogoIconChecked {
		t.Fatalf("locked record touched: %+v", rec)
	}
	if logo, _ := h.logoIconURLs(t, locked); logo != nil {
		t.Fatal("locked app got a logo url")
	}
}

// The orphan prune (run at boot) must count logos and icons as referenced.
func TestOrphanPruneKeepsCachedLogoAndIcon(t *testing.T) {
	h, fp, cdn := newLogoHarness(t)
	ctx := context.Background()
	appID := appidApp(t, h, fp, cdn, "620")
	h.svc.SweepOnce(ctx)
	rec := h.record(t, appID)

	if _, err := h.svc.PruneOrphans(ctx); err != nil {
		t.Fatal(err)
	}
	h.svc.SweepOnce(ctx)
	if !h.cached(t, rec.LogoAsset) || !h.cached(t, rec.IconAsset) {
		t.Fatal("the prune deleted a referenced logo or icon")
	}

	if err := h.svc.Clear(ctx, appID); err != nil {
		t.Fatal(err)
	}
	if logo, icon := h.logoIconURLs(t, appID); logo != nil || icon != nil {
		t.Fatal("clear left logo or icon urls")
	}
	if n, err := h.svc.PruneOrphans(ctx); err != nil || n < 2 || h.cached(t, rec.LogoAsset) {
		t.Fatalf("pruned %d (err %v); an unreferenced logo must go", n, err)
	}
}

func TestOverridesKeepOrReplaceLogoAndIcon(t *testing.T) {
	h, fp, cdn := newLogoHarness(t)
	ctx := context.Background()
	appID := appidApp(t, h, fp, cdn, "620")
	h.svc.SweepOnce(ctx)
	want := h.record(t, appID)

	if _, err := h.svc.Upload(ctx, appID, CropTile, "image/jpeg", onePixelJPEG); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.ApplyURLs(ctx, appID, "", h.artSrv.URL+"/hero.jpg"); err != nil {
		t.Fatal(err)
	}
	if got := h.record(t, appID); got.LogoAsset != want.LogoAsset || got.IconAsset != want.IconAsset {
		t.Fatalf("an upload or URL override dropped the logo or icon: %+v", got)
	}

	fp.art["88"] = Candidate{Ref: "88", Name: "Other", TileURL: h.artSrv.URL + "/grid.png"}
	if _, err := h.svc.ApplyCandidate(ctx, appID, "88"); err != nil {
		t.Fatal(err)
	}
	if got := h.record(t, appID); got.LogoAsset != "" || got.IconAsset != "" || !got.LogoIconChecked {
		t.Fatalf("a picked candidate with no logo kept the old one: %+v", got)
	}
}

func (h *harness) urls(t *testing.T, appID string) (logo, icon string) {
	t.Helper()
	l, i := h.logoIconURLs(t, appID)
	if l != nil {
		logo = *l
	}
	if i != nil {
		icon = *i
	}
	return logo, icon
}

// lostLogoSetup resolves two apps with all four crops, makes the second an older
// unchecked record ahead of the first in the backfill queue, deletes only the
// first's logo file, and limits each sweep to one record.
func lostLogoSetup(t *testing.T) (*harness, *fakeProvider, string, string, Record) {
	h, fp, cdn := newLogoHarness(t)
	ctx := context.Background()
	first := appidApp(t, h, fp, cdn, "620")
	older := appidApp(t, h, fp, cdn, "400")
	fp.logoIcon["steam:400"] = [2]string{cdn + "/logo2.png", cdn + "/icon.png"} // its own logo file
	h.svc.SweepOnce(ctx)
	if _, err := h.pool.Exec(ctx, `UPDATE app_artwork SET logo_icon_checked = false, updated_at = now() - interval '1 hour'
		WHERE app_id::text = $1`, older); err != nil {
		t.Fatal(err)
	}
	rec := h.record(t, first)
	if err := os.Remove(h.svc.blobs.pathFor(rec.LogoAsset)); err != nil {
		t.Fatal(err)
	}
	h.svc.sweepBatch = 1
	return h, fp, first, older, rec
}

// Only the lost crop goes, and it comes back in the same sweep from the record's
// own reference, ahead of an older record waiting in the backfill queue.
func TestLostLogoIsFetchedAgainAndTheIconKept(t *testing.T) {
	h, _, first, _, before := lostLogoSetup(t)
	ctx := context.Background()
	logoURL, iconURL := h.urls(t, first)

	res := h.svc.SweepOnce(ctx)
	after := h.record(t, first)
	if res.ArtworkRepaired != 1 || after.IconAsset != before.IconAsset || after.LogoAsset != before.LogoAsset ||
		!after.LogoIconChecked || !h.cached(t, after.LogoAsset) {
		t.Fatalf("result %+v\nbefore %+v\nafter %+v", res, before, after)
	}
	if l, i := h.urls(t, first); l != logoURL || i != iconURL {
		t.Fatalf("urls %q %q, want %q %q", l, i, logoURL, iconURL)
	}
	if _, err := h.svc.PruneOrphans(ctx); err != nil {
		t.Fatal(err)
	}
	if !h.cached(t, before.IconAsset) || !h.cached(t, before.LogoAsset) {
		t.Fatal("the prune deleted a referenced logo or icon")
	}
}

// With the provider down the lost logo's URL is dropped at once (never a URL to a
// missing file), the icon is kept, and a later sweep fetches the logo again.
func TestLostLogoThatCannotBeFetchedIsDroppedThenRefetched(t *testing.T) {
	h, fp, first, _, before := lostLogoSetup(t)
	ctx := context.Background()
	_, iconURL := h.urls(t, first)
	fp.logoIconErr = errTransient

	h.svc.SweepOnce(ctx)
	if l, i := h.urls(t, first); l != "" || i != iconURL {
		t.Fatalf("urls %q %q: want the logo dropped and the icon %q kept", l, i, iconURL)
	}
	if rec := h.record(t, first); rec.LogoAsset != "" || rec.IconAsset != before.IconAsset || !h.cached(t, rec.IconAsset) {
		t.Fatalf("%+v", rec)
	}

	fp.logoIconErr = nil
	for range 3 { // two records in the queue, one per sweep
		h.svc.SweepOnce(ctx)
		if rec := h.record(t, first); rec.LogoAsset == before.LogoAsset && rec.IconAsset == before.IconAsset {
			return
		}
	}
	t.Fatalf("the dropped logo was not fetched again: %+v", h.record(t, first))
}

// Dropping costs no provider call, so it is not held back by the sweep's batch:
// no record keeps a URL to a missing file past one sweep.
func TestLostLogosAreDroppedBeyondTheSweepBatch(t *testing.T) {
	h, fp, cdn := newLogoHarness(t)
	ctx := context.Background()
	a := appidApp(t, h, fp, cdn, "620")
	b := appidApp(t, h, fp, cdn, "400") // same logo bytes: one file backs both
	h.svc.SweepOnce(ctx)
	if err := os.Remove(h.svc.blobs.pathFor(h.record(t, a).LogoAsset)); err != nil {
		t.Fatal(err)
	}
	h.svc.sweepBatch = 1
	fp.logoIconErr = errTransient

	h.svc.SweepOnce(ctx)
	for _, id := range []string{a, b} {
		if l, i := h.urls(t, id); l != "" || i == "" {
			t.Fatalf("%s: logo %q (want dropped), icon %q (want kept)", id, l, i)
		}
	}
}

// A locked record's lost logo is dropped and fetched again by its own reference,
// as the tile and hero repair does; it stays locked.
func TestLockedRecordLostLogoIsDroppedAndFetchedAgain(t *testing.T) {
	h, fp, cdn := newLogoHarness(t)
	ctx := context.Background()
	appID := appidApp(t, h, fp, cdn, "620")
	h.svc.SweepOnce(ctx)
	if _, err := h.pool.Exec(ctx, `UPDATE app_artwork SET source = 'manual', locked = true WHERE app_id::text = $1`, appID); err != nil {
		t.Fatal(err)
	}
	before := h.record(t, appID)
	lose := func() {
		if err := os.Remove(h.svc.blobs.pathFor(before.LogoAsset)); err != nil {
			t.Fatal(err)
		}
	}

	lose()
	h.svc.SweepOnce(ctx)
	after := h.record(t, appID)
	if after.LogoAsset != before.LogoAsset || !h.cached(t, after.LogoAsset) || !after.Locked || after.Source != SourceManual {
		t.Fatalf("before %+v\nafter %+v", before, after)
	}

	lose()
	fp.logoIconErr = errTransient
	h.svc.SweepOnce(ctx)
	if l, i := h.urls(t, appID); l != "" || i == "" {
		t.Fatalf("locked record serves logo %q (want none) icon %q", l, i)
	}
}

func TestRematchReplacesAllFourCrops(t *testing.T) {
	h, fp, cdn := newLogoHarness(t)
	ctx := context.Background()
	appID := appidApp(t, h, fp, cdn, "620")
	h.svc.SweepOnce(ctx)
	before := h.record(t, appID)

	fp.artByExternal["steam:620"] = Candidate{Name: "Portal 2", TileURL: cdn + "/tile2.png", HeroURL: cdn + "/hero2.png"}
	fp.logoIcon["steam:620"] = [2]string{cdn + "/logo2.png", ""}
	after, err := h.svc.ResolveByID(ctx, appID, false)
	if err != nil {
		t.Fatal(err)
	}
	if after.TileAsset == before.TileAsset || after.HeroAsset == before.HeroAsset ||
		after.LogoAsset == before.LogoAsset || after.LogoAsset == "" || after.IconAsset != "" {
		t.Fatalf("before %+v\nafter %+v", before, after)
	}
	if l, i := h.urls(t, appID); l != AssetURL(after.LogoAsset) || i != "" {
		t.Fatalf("urls %q %q", l, i)
	}
}
