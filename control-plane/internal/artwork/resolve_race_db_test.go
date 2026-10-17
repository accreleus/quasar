package artwork

// #739: a first-time Resolve is insert-only. An admin pick or upload that lands
// between Resolve's initial Get ("no row yet") and its save wins: the resolve's
// result is discarded, the concurrent writer's record stands, and the app's
// render URLs stay pointing at the manual assets. The race is made deterministic
// by landing the competing write inside the fake provider's resolve call —
// exactly the gap Resolve spends fetching from the provider. DB-backed (reuses
// handler_test.go's harness and fakeProvider); no live third-party call.

import (
	"context"
	"testing"
)

// TestFirstTimeResolveLosesToAConcurrentAdminWrite drives the title path
// (Search → Art): an admin Upload landing inside provider.Art must stand, and
// the resolve's result must be discarded without changing anything.
func TestFirstTimeResolveLosesToAConcurrentAdminWrite(t *testing.T) {
	pool := testDB(t)
	fp := &fakeProvider{
		results: map[string][]Candidate{"Portal 2": {{Ref: "1234", Name: "Portal 2"}}},
		art:     map[string]Candidate{},
	}
	h := newHarness(t, pool, fp)
	appID := h.seedApp(t, "Portal 2", "game")
	fp.art["1234"] = Candidate{
		Ref: "1234", Name: "Portal 2",
		TileURL: h.artSrv.URL + "/grid.png",
		HeroURL: h.artSrv.URL + "/hero.jpg",
	}

	manualAsset := ""
	fp.onArt = func() {
		rec, err := h.svc.Upload(context.Background(), appID, CropTile, "image/png", onePixelPNG)
		if err != nil {
			t.Fatalf("concurrent admin upload: %v", err)
		}
		manualAsset = rec.TileAsset
		if manualAsset == "" {
			t.Fatal("manual upload stored no tile asset")
		}
		fp.onArt = nil
	}

	res := h.svc.ResolveApps(context.Background(), []string{appID})
	if !res.ProviderConfigured {
		t.Fatal("ProviderConfigured=false with a provider configured")
	}

	// The race really ran: the provider was asked, and the manual write landed
	// while it was being asked.
	if fp.searches != 1 || fp.artCalls != 1 {
		t.Fatalf("searches=%d artCalls=%d, want 1/1 — the hook race did not run", fp.searches, fp.artCalls)
	}
	if manualAsset == "" {
		t.Fatal("the concurrent admin write never landed")
	}

	// The resolve still reports success — a lost race is not an error.
	if res.AppsConsidered != 1 || res.ArtworkResolved != 1 || res.NoMatch != 0 {
		t.Fatalf("result = %+v, want considered=1 resolved=1 (the manual record is what counts)", res)
	}

	// The manual record stands: a locked admin pick, not the resolve's.
	rec, ok, err := h.svc.store.Get(context.Background(), appID)
	if err != nil || !ok {
		t.Fatalf("record: ok=%v err=%v", ok, err)
	}
	if rec.Source != SourceManual || !rec.Locked {
		t.Fatalf("record = %+v; the concurrent admin write must survive the resolve", rec)
	}
	if rec.TileAsset != manualAsset {
		t.Fatalf("tile asset = %q, want the manual upload %q", rec.TileAsset, manualAsset)
	}

	// ...and the app row was not touched back to the resolve's crops. The admin
	// upload set only the tile, so the hero URL must still be NULL — exactly what
	// the manual write left it at.
	cover, hero := h.appURLs(t, appID)
	if cover == nil || *cover != AssetURL(manualAsset) {
		t.Fatalf("cover_url = %v, want %s", cover, AssetURL(manualAsset))
	}
	if hero != nil {
		t.Fatalf("hero_url = %v, want NULL (the manual write set only the tile)", *hero)
	}
}

// TestResolveByExternalRefLosesToAConcurrentAdminWrite drives the by-appid arm
// (ArtByExternalRef — Search is never called for a steam-tagged app), whose save
// is the same first-time path and must hold the same promise.
func TestResolveByExternalRefLosesToAConcurrentAdminWrite(t *testing.T) {
	pool := testDB(t)
	fp := &fakeProvider{artByExternal: map[string]Candidate{}}
	h := newHarness(t, pool, fp)
	appID := h.seedExternalApp(t, "Portal 2", "game", "steam", "620")
	fp.artByExternal["steam:620"] = Candidate{
		TileURL: h.artSrv.URL + "/grid.png",
		HeroURL: h.artSrv.URL + "/hero.jpg",
	}

	manualAsset := ""
	fp.onExternal = func() {
		rec, err := h.svc.Upload(context.Background(), appID, CropTile, "image/png", onePixelPNG)
		if err != nil {
			t.Fatalf("concurrent admin upload: %v", err)
		}
		manualAsset = rec.TileAsset
		if manualAsset == "" {
			t.Fatal("manual upload stored no tile asset")
		}
		fp.onExternal = nil
	}

	h.svc.ResolveApps(context.Background(), []string{appID})

	if fp.externalCalls != 1 {
		t.Fatalf("externalCalls=%d, want exactly one ArtByExternalRef", fp.externalCalls)
	}
	if fp.searches != 0 {
		t.Fatalf("appid-tagged apps must never search; got %d searches", fp.searches)
	}
	if manualAsset == "" {
		t.Fatal("the concurrent admin write never landed")
	}

	rec, ok, err := h.svc.store.Get(context.Background(), appID)
	if err != nil || !ok {
		t.Fatalf("record: ok=%v err=%v", ok, err)
	}
	if rec.Source != SourceManual || !rec.Locked || rec.TileAsset != manualAsset {
		t.Fatalf("record = %+v; the concurrent admin write must survive the resolve", rec)
	}
}
