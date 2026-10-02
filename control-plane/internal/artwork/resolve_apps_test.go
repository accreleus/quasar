package artwork

// ResolveApps is the library scan's artwork hook (#384): the apps a scan just
// added resolve straight away, under the sweep's own rules. DB-backed (reuses
// handler_test.go's harness and fakeProvider); no live third-party call.

import (
	"context"
	"errors"
	"testing"
)

// Exactly the named apps are resolved — one provider call each — and an app
// the caller did not name stays in the sweep's queue untouched.
func TestResolveAppsResolvesExactlyTheNamedApps(t *testing.T) {
	pool := testDB(t)
	fp := &fakeProvider{artByExternal: map[string]Candidate{}}
	h := newHarness(t, pool, fp)
	fp.artByExternal["steam:620"] = Candidate{TileURL: h.artSrv.URL + "/grid.png", HeroURL: h.artSrv.URL + "/hero.jpg"}
	// steam:400 is absent: the provider's 404, a stored "no match".

	portal2 := h.seedExternalApp(t, "Portal 2", "game", "steam", "620")
	portal := h.seedExternalApp(t, "Portal", "game", "steam", "400")
	other := h.seedExternalApp(t, "Half-Life 2", "game", "steam", "220")

	res := h.svc.ResolveApps(context.Background(), []string{portal2, portal})

	if fp.externalCalls != 2 {
		t.Fatalf("want exactly one ArtByExternalRef per named app (2), got %d", fp.externalCalls)
	}
	if fp.searches != 0 {
		t.Fatalf("appid-tagged apps must never search; got %d", fp.searches)
	}
	if !res.ProviderConfigured || res.AppsConsidered != 2 || res.ArtworkResolved != 1 || res.NoMatch != 1 {
		t.Fatalf("result = %+v, want configured, 2 considered, 1 resolved, 1 no-match", res)
	}
	if rec, ok, err := h.svc.store.Get(context.Background(), portal2); err != nil || !ok || rec.Source != SourceProvider {
		t.Fatalf("Portal 2: want a provider row, got %+v ok=%v err=%v", rec, ok, err)
	}
	if rec, ok, err := h.svc.store.Get(context.Background(), portal); err != nil || !ok || rec.Source != SourceNone {
		t.Fatalf("Portal: a 404 must be recorded as source=none, got %+v ok=%v err=%v", rec, ok, err)
	}
	if _, ok, err := h.svc.store.Get(context.Background(), other); err != nil || ok {
		t.Fatalf("an app the caller did not name must be left for the sweep; ok=%v err=%v", ok, err)
	}
}

// The cache holds: an app that already has a row is never asked about again,
// and an empty or unknown id list does nothing.
func TestResolveAppsUsesTheCache(t *testing.T) {
	pool := testDB(t)
	fp := &fakeProvider{artByExternal: map[string]Candidate{}}
	h := newHarness(t, pool, fp)

	appID := h.seedExternalApp(t, "Some Obscure Thing", "game", "steam", "999999")
	h.svc.ResolveApps(context.Background(), []string{appID})
	h.svc.ResolveApps(context.Background(), []string{appID})
	if fp.externalCalls != 1 {
		t.Fatalf("a resolved app must not be re-queried; got %d calls", fp.externalCalls)
	}

	h.svc.ResolveApps(context.Background(), nil)
	h.svc.ResolveApps(context.Background(), []string{"00000000-0000-0000-0000-000000000000"})
	if fp.externalCalls != 1 {
		t.Fatalf("no id / a vanished id must make no call; got %d", fp.externalCalls)
	}
}

// A manual (locked) choice is never overwritten.
func TestResolveAppsNeverOverwritesAManualChoice(t *testing.T) {
	pool := testDB(t)
	fp := &fakeProvider{artByExternal: map[string]Candidate{}}
	h := newHarness(t, pool, fp)
	fp.artByExternal["steam:620"] = Candidate{TileURL: h.artSrv.URL + "/grid.png"}

	appID := h.seedExternalApp(t, "Portal 2", "game", "steam", "620")
	if _, err := h.svc.Upload(context.Background(), appID, CropTile, "image/jpeg", onePixelJPEG); err != nil {
		t.Fatalf("upload: %v", err)
	}
	before, _, _ := h.svc.store.Get(context.Background(), appID)
	if before.Source != SourceManual || !before.Locked {
		t.Fatalf("fixture: want a manual+locked row, got %+v", before)
	}

	h.svc.ResolveApps(context.Background(), []string{appID})

	if fp.externalCalls != 0 || fp.searches != 0 {
		t.Fatalf("an app with a manual choice must make no provider call; external=%d searches=%d",
			fp.externalCalls, fp.searches)
	}
	after, _, _ := h.svc.store.Get(context.Background(), appID)
	if after.Source != SourceManual || !after.Locked || after.TileAsset != before.TileAsset {
		t.Fatalf("the manual choice was overwritten: %+v -> %+v", before, after)
	}
}

// A provider error (outage, rate limit) writes no row, so the regular sweep
// still picks the app up — the sweep stays the backstop.
func TestResolveAppsProviderErrorLeavesTheAppForTheSweep(t *testing.T) {
	pool := testDB(t)
	fp := &fakeProvider{
		artByExternal: map[string]Candidate{},
		externalErr:   errors.New("429 too many requests"),
	}
	h := newHarness(t, pool, fp)

	appID := h.seedExternalApp(t, "Portal 2", "game", "steam", "620")
	res := h.svc.ResolveApps(context.Background(), []string{appID})
	if res.ArtworkResolved != 0 || res.NoMatch != 0 {
		t.Fatalf("a provider error is neither a match nor a no-match: %+v", res)
	}
	if _, ok, err := h.svc.store.Get(context.Background(), appID); err != nil || ok {
		t.Fatalf("a provider error must write no row; ok=%v err=%v", ok, err)
	}

	fp.externalErr = nil
	fp.artByExternal["steam:620"] = Candidate{TileURL: h.artSrv.URL + "/grid.png"}
	sweep := h.svc.SweepOnce(context.Background())
	if sweep.AppsConsidered != 1 || sweep.ArtworkResolved != 1 {
		t.Fatalf("the sweep must pick the app up afterwards: %+v", sweep)
	}
}

// Ship-dark: with no provider, nothing changes — no row for a game, and none
// for a desktop app either (Resolve's kind short-circuit would write one
// without any provider call, so ResolveApps must check the provider first).
func TestResolveAppsWithNoProviderChangesNothing(t *testing.T) {
	pool := testDB(t)
	h := newHarness(t, pool, nil)

	game := h.seedExternalApp(t, "Portal 2", "game", "steam", "620")
	desktop := h.seedApp(t, "Desktop", "desktop")

	res := h.svc.ResolveApps(context.Background(), []string{game, desktop})
	if res != (SweepResult{}) {
		t.Fatalf("result = %+v, want the zero value", res)
	}
	for _, id := range []string{game, desktop} {
		if _, ok, err := h.svc.store.Get(context.Background(), id); err != nil || ok {
			t.Fatalf("app %s: no provider must mean no row; ok=%v err=%v", id, ok, err)
		}
		if cover, hero := h.appURLs(t, id); cover != nil || hero != nil {
			t.Fatalf("app %s: no provider must mean no urls; cover=%v hero=%v", id, cover, hero)
		}
	}
}
