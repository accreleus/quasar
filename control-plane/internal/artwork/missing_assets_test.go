package artwork

// #441: a recorded asset whose cached file is gone (a database restored into a
// fresh install, a lost volume, a hand-cleaned cache) is fetched again by the
// sweep from the provider reference the row already holds. DB-backed; reuses
// handler_test.go's harness and fakeProvider.

import (
	"context"
	"errors"
	"net/http"
	"os"
	"testing"
)

var errTransient = errors.New("fake: provider unavailable")

// emptyCache deletes every cached blob, as a fresh install restored from a
// dump has none.
func (h *harness) emptyCache(t *testing.T) {
	t.Helper()
	root := h.svc.blobs.Root()
	if err := os.RemoveAll(root); err != nil {
		t.Fatalf("empty cache: %v", err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("recreate cache: %v", err)
	}
}

func (h *harness) assetsPresent(t *testing.T, appID string) Record {
	t.Helper()
	rec, ok, err := h.svc.store.Get(context.Background(), appID)
	if err != nil || !ok {
		t.Fatalf("get %s: ok=%v err=%v", appID, ok, err)
	}
	for _, a := range []string{rec.TileAsset, rec.HeroAsset} {
		if a != "" && !h.svc.blobs.Has(a) {
			t.Fatalf("asset %s of %s is still missing", a, appID)
		}
	}
	return rec
}

// An appid-resolved row is fetched again by its appid; nothing is searched and
// the row keeps everything but its (identical, content-addressed) asset names.
func TestSweepRefetchesMissingFilesOfAnAppidMatch(t *testing.T) {
	pool := testDB(t)
	fp := &fakeProvider{artByExternal: map[string]Candidate{}}
	h := newHarness(t, pool, fp)
	fp.artByExternal["steam:620"] = Candidate{Name: "Portal 2", TileURL: h.artSrv.URL + "/grid.png", HeroURL: h.artSrv.URL + "/hero.jpg"}
	appID := h.seedExternalApp(t, "Portal 2", "game", "steam", "620")

	h.svc.SweepOnce(context.Background())
	before := h.assetsPresent(t, appID)
	h.emptyCache(t)

	res := h.svc.SweepOnce(context.Background())
	if res.ArtworkRepaired != 1 || res.AppsConsidered != 0 {
		t.Fatalf("result = %+v, want 1 repaired and no new app considered", res)
	}
	if fp.externalCalls != 2 || fp.searches != 0 {
		t.Fatalf("want one more ArtByExternalRef and no search, got external=%d searches=%d", fp.externalCalls, fp.searches)
	}
	after := h.assetsPresent(t, appID)
	if after.TileAsset != before.TileAsset || after.HeroAsset != before.HeroAsset ||
		after.Source != before.Source || after.ProviderRef != before.ProviderRef {
		t.Fatalf("row changed: before %+v, after %+v", before, after)
	}
	cover, _ := h.appURLs(t, appID)
	if cover == nil {
		t.Fatal("the app lost its cover URL")
	}
	if resp, _ := do(t, http.MethodGet, h.srv.URL+*cover, h.userToken, "", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("the repaired cover is not served: %d", resp.StatusCode)
	}

	// Present files are never fetched again.
	h.svc.SweepOnce(context.Background())
	if fp.externalCalls != 2 {
		t.Fatalf("a row whose files exist must not be re-fetched, got %d calls", fp.externalCalls)
	}
}

// A title-matched row is fetched again by its stored provider ref, never by a
// new search: the match was already decided.
func TestSweepRefetchesMissingFilesOfATitleMatchByItsRef(t *testing.T) {
	pool := testDB(t)
	fp := &fakeProvider{
		results: map[string][]Candidate{"Hades": {{Ref: "h1", Name: "Hades"}}},
		art:     map[string]Candidate{},
	}
	h := newHarness(t, pool, fp)
	fp.art["h1"] = Candidate{Name: "Hades", TileURL: h.artSrv.URL + "/grid.png"}
	appID := h.seedApp(t, "Hades", "game")

	h.svc.SweepOnce(context.Background())
	h.emptyCache(t)
	res := h.svc.SweepOnce(context.Background())

	if res.ArtworkRepaired != 1 {
		t.Fatalf("result = %+v, want 1 repaired", res)
	}
	if fp.searches != 1 || fp.artCalls != 2 {
		t.Fatalf("want no new search and one more Art call, got searches=%d art=%d", fp.searches, fp.artCalls)
	}
	h.assetsPresent(t, appID)
}

// An admin's locked choice is fetched again by its ref and stays locked and
// manual: repairing a file is not re-matching.
func TestSweepRefetchesALockedChoiceAndKeepsItLocked(t *testing.T) {
	pool := testDB(t)
	fp := &fakeProvider{art: map[string]Candidate{}}
	h := newHarness(t, pool, fp)
	fp.art["pick"] = Candidate{Name: "Portal", TileURL: h.artSrv.URL + "/grid.png", HeroURL: h.artSrv.URL + "/hero.jpg"}
	appID := h.seedApp(t, "Portal", "game")
	if _, err := h.svc.ApplyCandidate(context.Background(), appID, "pick"); err != nil {
		t.Fatalf("apply candidate: %v", err)
	}
	h.emptyCache(t)

	res := h.svc.SweepOnce(context.Background())
	if res.ArtworkRepaired != 1 || fp.searches != 0 {
		t.Fatalf("result = %+v searches=%d, want 1 repaired and no search", res, fp.searches)
	}
	rec := h.assetsPresent(t, appID)
	if !rec.Locked || rec.Source != SourceManual || rec.ProviderRef != "pick" {
		t.Fatalf("the admin's choice was not kept: %+v", rec)
	}
}

// An upload has no provider reference: nothing can fetch it again, so the row
// is left exactly as it is and nothing is asked of the provider.
func TestSweepLeavesAMissingUploadAlone(t *testing.T) {
	pool := testDB(t)
	fp := &fakeProvider{}
	h := newHarness(t, pool, fp)
	appID := h.seedApp(t, "Blender", "desktop")
	if resp, body := do(t, http.MethodPost,
		h.srv.URL+"/v1/admin/apps/"+appID+"/artwork/upload?crop=tile",
		h.adminToken, "image/png", onePixelPNG); resp.StatusCode != http.StatusOK {
		t.Fatalf("upload: %d (%v)", resp.StatusCode, body)
	}
	before, _, _ := h.svc.store.Get(context.Background(), appID)
	h.emptyCache(t)

	res := h.svc.SweepOnce(context.Background())
	if res.ArtworkRepaired != 0 || fp.searches+fp.artCalls+fp.externalCalls != 0 {
		t.Fatalf("result = %+v, provider calls %d/%d/%d; want nothing", res, fp.searches, fp.artCalls, fp.externalCalls)
	}
	after, _, _ := h.svc.store.Get(context.Background(), appID)
	if after.TileAsset != before.TileAsset || after.Source != before.Source {
		t.Fatalf("the upload's row changed: %+v -> %+v", before, after)
	}
}

// A provider outage while repairing changes nothing: the row and its names stay,
// and the next sweep tries again.
func TestSweepRepairIsRetriedAfterAProviderError(t *testing.T) {
	pool := testDB(t)
	fp := &fakeProvider{artByExternal: map[string]Candidate{}}
	h := newHarness(t, pool, fp)
	fp.artByExternal["steam:620"] = Candidate{TileURL: h.artSrv.URL + "/grid.png"}
	appID := h.seedExternalApp(t, "Portal 2", "game", "steam", "620")
	h.svc.SweepOnce(context.Background())
	before, _, _ := h.svc.store.Get(context.Background(), appID)
	h.emptyCache(t)

	fp.externalErr = errTransient
	if res := h.svc.SweepOnce(context.Background()); res.ArtworkRepaired != 0 {
		t.Fatalf("result = %+v, want nothing repaired during the outage", res)
	}
	if rec, _, _ := h.svc.store.Get(context.Background(), appID); rec.TileAsset != before.TileAsset {
		t.Fatalf("an outage changed the row: %+v", rec)
	}
	fp.externalErr = nil
	if res := h.svc.SweepOnce(context.Background()); res.ArtworkRepaired != 1 {
		t.Fatalf("result = %+v, want the repair on the next sweep", res)
	}
	h.assetsPresent(t, appID)
}
