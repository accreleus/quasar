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
	"path/filepath"
	"reflect"
	"testing"
	"time"
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

// lockedPair resolves two apps with their own logos, locks both (an admin pick),
// deletes both logo files and limits each sweep to one provider repair.
func lockedPair(t *testing.T) (*harness, *fakeProvider, string, []string, map[string]Record) {
	h, fp, cdn := newLogoHarness(t)
	ctx := context.Background()
	ids := []string{appidApp(t, h, fp, cdn, "620"), appidApp(t, h, fp, cdn, "400")}
	fp.logoIcon["steam:400"] = [2]string{cdn + "/logo2.png", cdn + "/icon.png"}
	h.svc.SweepOnce(ctx)
	if _, err := h.pool.Exec(ctx, `UPDATE app_artwork SET source = 'manual', locked = true`); err != nil {
		t.Fatal(err)
	}
	before := map[string]Record{}
	for _, id := range ids {
		before[id] = h.record(t, id)
		if err := os.Remove(h.svc.blobs.pathFor(before[id].LogoAsset)); err != nil {
			t.Fatal(err)
		}
	}
	h.svc.sweepBatch = 1
	return h, fp, cdn, ids, before
}

// A locked record's lost logo stops being served at once but keeps its name, so
// every sweep retries it from the record's own reference until a fetch succeeds,
// as the tile and hero repair (#441) does; source and lock are kept.
func TestLockedRecordLostLogoIsRetriedUntilFetched(t *testing.T) {
	h, fp, _, ids, before := lockedPair(t)
	ctx := context.Background()
	restored := func() int {
		n := 0
		for _, id := range ids {
			rec := h.record(t, id)
			if rec.LogoAsset != before[id].LogoAsset || !rec.Locked || rec.Source != SourceManual {
				t.Fatalf("%s: name, lock or source changed: %+v", id, rec)
			}
			logo, icon := h.urls(t, id)
			if icon == "" {
				t.Fatalf("%s lost its icon url", id)
			}
			switch logo {
			case "":
				if h.cached(t, rec.LogoAsset) {
					t.Fatalf("%s: file back but url not served", id)
				}
			case AssetURL(rec.LogoAsset):
				if !h.cached(t, rec.LogoAsset) {
					t.Fatalf("%s serves a url to a missing file", id)
				}
				n++
			default:
				t.Fatalf("%s: logo url %q", id, logo)
			}
		}
		return n
	}

	// (a) Sweep 1, provider down: neither logo is served any more, the one past the
	// batch included, and both names are kept.
	fp.logoIconErr = errTransient
	h.svc.SweepOnce(ctx)
	if n := restored(); n != 0 {
		t.Fatalf("%d restored with the provider down", n)
	}
	// (b) Sweep 2, provider back: the batch's one record is fetched again.
	fp.logoIconErr = nil
	h.svc.SweepOnce(ctx)
	if n := restored(); n != 1 {
		t.Fatalf("after sweep 2: %d restored, want 1", n)
	}
	// (c) Sweep 3: the record that was past the batch is fetched too.
	h.svc.SweepOnce(ctx)
	if n := restored(); n != 2 {
		t.Fatalf("after sweep 3: %d restored, want 2", n)
	}
	// A kept name for a missing file was harmless to the prune.
	if _, err := h.svc.PruneOrphans(ctx); err != nil {
		t.Fatal(err)
	}
	if n := restored(); n != 2 {
		t.Fatalf("after prune: %d restored", n)
	}
}

// A re-save while the logo is still missing (an admin uploads a tile) must not
// serve the kept name again.
func TestResavingALockedRecordDoesNotServeAMissingLogo(t *testing.T) {
	h, fp, _, ids, _ := lockedPair(t)
	ctx := context.Background()
	fp.logoIconErr = errTransient
	h.svc.SweepOnce(ctx)
	if _, err := h.svc.Upload(ctx, ids[0], CropTile, "image/jpeg", onePixelJPEG); err != nil {
		t.Fatal(err)
	}
	if logo, _ := h.urls(t, ids[0]); logo != "" {
		t.Fatalf("an upload re-served the missing logo at %q", logo)
	}
}

// Logo fetched, icon fails: the logo is named and served at once; only the icon
// stays pending, and the next sweep fetches it.
func TestLogoFetchedIconFailedKeepsTheLogo(t *testing.T) {
	h, fp, cdn := newLogoHarness(t)
	ctx := context.Background()
	appID := appidApp(t, h, fp, cdn, "620")
	h.svc.SweepOnce(ctx)
	if _, err := h.pool.Exec(ctx, `UPDATE app_artwork SET source = 'manual', locked = true`); err != nil {
		t.Fatal(err)
	}
	before := h.record(t, appID)
	for _, a := range []string{before.LogoAsset, before.IconAsset} {
		if err := os.Remove(h.svc.blobs.pathFor(a)); err != nil {
			t.Fatal(err)
		}
	}
	fp.logoIcon["steam:620"] = [2]string{cdn + "/logo.png", cdn + "/gone.png"} // the icon download 404s

	h.svc.SweepOnce(ctx)
	if _, err := h.svc.PruneOrphans(ctx); err != nil {
		t.Fatal(err)
	}
	rec := h.record(t, appID)
	logo, icon := h.urls(t, appID)
	if rec.LogoAsset != before.LogoAsset || !h.cached(t, rec.LogoAsset) || logo != AssetURL(rec.LogoAsset) {
		t.Fatalf("logo not kept: %+v, url %q", rec, logo)
	}
	if rec.IconAsset != before.IconAsset || icon != "" {
		t.Fatalf("icon: name %q (want kept), url %q (want none)", rec.IconAsset, icon)
	}

	fp.logoIcon["steam:620"] = [2]string{cdn + "/logo.png", cdn + "/icon.png"}
	h.svc.SweepOnce(ctx)
	if _, icon := h.urls(t, appID); icon != AssetURL(before.IconAsset) || !h.cached(t, before.IconAsset) {
		t.Fatalf("icon not fetched on the next sweep: %q", icon)
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

// adminActions are the admin writes a sweep can race: each must win.
func adminActions(h *harness, fp *fakeProvider, cdn string) map[string]func(t *testing.T, appID string) {
	return map[string]func(t *testing.T, appID string){
		"pick": func(t *testing.T, appID string) {
			fp.art["88"] = Candidate{Ref: "88", Name: "Other", TileURL: cdn + "/tile2.png"}
			fp.logoIcon[":88"] = [2]string{cdn + "/logo2.png", ""}
			if _, err := h.svc.ApplyCandidate(context.Background(), appID, "88"); err != nil {
				t.Fatal(err)
			}
		},
		"upload": func(t *testing.T, appID string) {
			if _, err := h.svc.Upload(context.Background(), appID, CropTile, "image/jpeg", onePixelJPEG); err != nil {
				t.Fatal(err)
			}
		},
	}
}

// A sweep holding a snapshot older than an admin write writes nothing for that
// row: the guard every sweep write goes through.
func TestSweepWriteRefusesARowAnAdminChanged(t *testing.T) {
	for _, name := range []string{"pick", "upload"} {
		t.Run(name, func(t *testing.T) {
			h, fp, cdn := newLogoHarness(t)
			ctx := context.Background()
			appID := appidApp(t, h, fp, cdn, "620")
			h.svc.SweepOnce(ctx)
			seen := h.record(t, appID) // what the sweep listed

			adminActions(h, fp, cdn)[name](t, appID)
			admin := h.record(t, appID)
			logo, icon := h.urls(t, appID)

			want := seen
			want.LogoAsset, want.LogoLost, want.LogoIconChecked = "", true, false
			if _, ok := h.svc.sweepWrite(ctx, seen, want); ok {
				t.Fatal("a write from a stale snapshot was accepted")
			}
			if got := h.record(t, appID); !reflect.DeepEqual(got, admin) {
				t.Fatalf("admin's row changed:\n got %+v\nwant %+v", got, admin)
			}
			if l, i := h.urls(t, appID); l != logo || i != icon {
				t.Fatalf("urls %q %q, want %q %q", l, i, logo, icon)
			}
		})
	}
}

// An admin action landing while the sweep fetches (between its list and its
// repair or backfill write) leaves the admin's row as the admin wrote it.
func TestAdminActionDuringASweepFetchWins(t *testing.T) {
	for _, pass := range []string{"repair", "backfill"} {
		for _, name := range []string{"pick", "upload"} {
			t.Run(pass+"/"+name, func(t *testing.T) {
				h, fp, cdn := newLogoHarness(t)
				ctx := context.Background()
				appID := appidApp(t, h, fp, cdn, "620")
				if pass == "backfill" {
					fp.logoIconErr = errTransient // resolved with no logo: on the backfill queue
				}
				h.svc.SweepOnce(ctx)
				fp.logoIconErr = nil
				if pass == "repair" {
					if err := os.Remove(h.svc.blobs.pathFor(h.record(t, appID).LogoAsset)); err != nil {
						t.Fatal(err)
					}
				}
				var admin Record
				var logo, icon string
				fp.onLogoIcon = func() {
					fp.onLogoIcon = nil
					adminActions(h, fp, cdn)[name](t, appID)
					admin = h.record(t, appID)
					logo, icon = h.urls(t, appID)
				}

				h.svc.SweepOnce(ctx)
				if admin.AppID == "" {
					t.Fatal("the sweep never fetched")
				}
				if got := h.record(t, appID); !reflect.DeepEqual(got, admin) {
					t.Fatalf("the sweep overwrote the admin's row:\n got %+v\nwant %+v", got, admin)
				}
				if l, i := h.urls(t, appID); l != logo || i != icon {
					t.Fatalf("urls %q %q, want %q %q", l, i, logo, icon)
				}
			})
		}
	}
}

// A logo or icon is served only when Has says (true, nil). A stat error (here
// ENOTDIR) unserves it and changes nothing else: no unname, no fetch. When the
// file can be found again, the next sweep serves it with no provider call.
func TestLogoThatCannotBeCheckedIsOnlyUnservedThenServedAgain(t *testing.T) {
	h, fp, cdn := newLogoHarness(t)
	ctx := context.Background()
	appID := appidApp(t, h, fp, cdn, "620")
	h.svc.SweepOnce(ctx)
	before := h.record(t, appID)
	logoURL, iconURL := h.urls(t, appID)
	shard := filepath.Join(h.svc.blobs.Root(), before.LogoAsset[:2])
	for _, a := range []string{before.TileAsset, before.HeroAsset, before.IconAsset} {
		if a[:2] == before.LogoAsset[:2] {
			t.Fatalf("fixture: %s shares the logo's shard", a)
		}
	}
	data, err := os.ReadFile(h.svc.blobs.pathFor(before.LogoAsset))
	if err != nil {
		t.Fatal(err)
	}
	// The shard path as a file: stat of the logo fails with ENOTDIR.
	if err := os.RemoveAll(shard); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shard, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	calls := fp.logoIconCalls

	h.svc.SweepOnce(ctx)
	after := h.record(t, appID)
	if l, i := h.urls(t, appID); l != "" || i != iconURL {
		t.Fatalf("urls %q %q: want the logo unserved and the icon %q kept", l, i, iconURL)
	}
	if after.LogoAsset != before.LogoAsset || after.LogoIconChecked != before.LogoIconChecked || fp.logoIconCalls != calls {
		t.Fatalf("a stat error changed more than the url: %+v, %d LogoIcon calls", after, fp.logoIconCalls-calls)
	}

	// The cache comes back (a late mount): served again, nothing fetched.
	if err := os.Remove(shard); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(shard, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.svc.blobs.pathFor(before.LogoAsset), data, 0o600); err != nil {
		t.Fatal(err)
	}
	h.svc.SweepOnce(ctx)
	if l, _ := h.urls(t, appID); l != logoURL || fp.logoIconCalls != calls {
		t.Fatalf("logo url %q (want %q), %d LogoIcon calls", l, logoURL, fp.logoIconCalls-calls)
	}
}

func (h *harness) updatedAts(t *testing.T, appID string) (artwork, app time.Time) {
	t.Helper()
	if err := h.pool.QueryRow(context.Background(), `
		SELECT w.updated_at, a.updated_at FROM app_artwork w JOIN apps a ON a.id = w.app_id
		WHERE w.app_id::text = $1`, appID).Scan(&artwork, &app); err != nil {
		t.Fatal(err)
	}
	return artwork, app
}

// A locked record whose logo stays unfetchable is written once, not every sweep:
// neither its row nor its app is touched while nothing changes. The boot prune
// is untroubled by the kept name of the missing file.
func TestUnfetchableLockedLogoIsWrittenOnceAndSurvivesThePrune(t *testing.T) {
	h, fp, _, ids, before := lockedPair(t)
	ctx := context.Background()
	fp.logoIconErr = errTransient
	h.svc.sweepBatch = 25
	h.svc.SweepOnce(ctx)
	w1, a1 := h.updatedAts(t, ids[0])

	h.svc.SweepOnce(ctx)
	if w2, a2 := h.updatedAts(t, ids[0]); !w2.Equal(w1) || !a2.Equal(a1) {
		t.Fatalf("a sweep with nothing to change wrote: artwork %v -> %v, app %v -> %v", w1, w2, a1, a2)
	}

	if _, err := h.svc.PruneOrphans(ctx); err != nil {
		t.Fatalf("prune with a kept name for a missing file: %v", err)
	}
	rec := h.record(t, ids[0])
	if rec.LogoAsset != before[ids[0]].LogoAsset || !h.cached(t, rec.IconAsset) || !h.cached(t, rec.TileAsset) || !h.cached(t, rec.HeroAsset) {
		t.Fatalf("after prune: %+v", rec)
	}
	if l, _ := h.urls(t, ids[0]); l != "" {
		t.Fatalf("logo served at %q after prune", l)
	}
}

// Unlocked: logo fetched, icon fails. The logo is named and served, the icon is
// forgotten and the record stays on the backfill queue, which fetches it later.
func TestUnlockedLogoFetchedIconFailed(t *testing.T) {
	h, fp, cdn := newLogoHarness(t)
	ctx := context.Background()
	appID := appidApp(t, h, fp, cdn, "620")
	h.svc.SweepOnce(ctx)
	before := h.record(t, appID)
	for _, a := range []string{before.LogoAsset, before.IconAsset} {
		if err := os.Remove(h.svc.blobs.pathFor(a)); err != nil {
			t.Fatal(err)
		}
	}
	fp.logoIcon["steam:620"] = [2]string{cdn + "/logo.png", cdn + "/gone.png"}

	h.svc.SweepOnce(ctx)
	if _, err := h.svc.PruneOrphans(ctx); err != nil {
		t.Fatal(err)
	}
	rec := h.record(t, appID)
	logo, icon := h.urls(t, appID)
	if rec.LogoAsset != before.LogoAsset || !h.cached(t, rec.LogoAsset) || logo != AssetURL(rec.LogoAsset) {
		t.Fatalf("logo not kept: %+v, url %q", rec, logo)
	}
	if rec.IconAsset != "" || icon != "" || rec.LogoIconChecked {
		t.Fatalf("icon: name %q url %q checked %v; want forgotten and queued", rec.IconAsset, icon, rec.LogoIconChecked)
	}

	fp.logoIcon["steam:620"] = [2]string{cdn + "/logo.png", cdn + "/icon.png"}
	h.svc.SweepOnce(ctx)
	if rec := h.record(t, appID); rec.IconAsset != before.IconAsset || !rec.LogoIconChecked {
		t.Fatalf("the backfill did not fetch the icon: %+v", rec)
	}
}
