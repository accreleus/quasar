package library

import (
	"context"
	"reflect"
	"testing"
)

type storeDetailsRow struct {
	Description       string
	Genres            []string
	Developer         *string
	Publisher         *string
	ReleaseDate       *string
	ControllerSupport *string
	Written           bool
}

func readStoreDetails(t *testing.T, f fixture, appID string) storeDetailsRow {
	t.Helper()
	var r storeDetailsRow
	must(t, f.pool.QueryRow(context.Background(), `
		SELECT description, genres, developer, publisher, release_date, controller_support,
		       store_details_at IS NOT NULL
		  FROM apps WHERE id::text = $1`, appID).
		Scan(&r.Description, &r.Genres, &r.Developer, &r.Publisher, &r.ReleaseDate, &r.ControllerSupport, &r.Written))
	return r
}

func strp(s string) *string { return &s }

// Amendment 38: a tile that already has a description still gets its store details, keyed on
// store_details_at, and they are written once.
func TestReconcileBackfillsStoreDetailsOnceWhateverTheDescription(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)
	ctx := context.Background()

	tileID := f.seedDiscoveredTile(t, "620", "An operator wrote this.")
	first := AppDetail{IsGame: true, ShortDescription: "Valve's answer.",
		Genres: []string{"Action", "Adventure"}, Developer: "Valve", Publisher: "Valve",
		ReleaseDate: "18 Apr, 2011", ControllerSupport: "full"}
	res, err := f.store.Reconcile(ctx, f.claimedScan(t, f.user), f.host,
		[]ReportEntry{{ExternalID: "620", Name: "Portal 2"}}, map[string]AppDetail{"620": first}, testInterval)
	must(t, err)
	if res.Backfilled != 1 {
		t.Fatalf("Backfilled = %d, want 1", res.Backfilled)
	}
	want := storeDetailsRow{Description: "An operator wrote this.", Genres: []string{"Action", "Adventure"},
		Developer: strp("Valve"), Publisher: strp("Valve"), ReleaseDate: strp("18 Apr, 2011"),
		ControllerSupport: strp("full"), Written: true}
	if got := readStoreDetails(t, f, tileID); !reflect.DeepEqual(got, want) {
		t.Fatalf("after first scan:\n got %+v\nwant %+v", got, want)
	}

	// The next answer differs; nothing is rewritten and nothing is counted.
	second := AppDetail{IsGame: true, Genres: []string{"Puzzle"}, Developer: "Someone else"}
	res, err = f.store.Reconcile(ctx, f.claimedScan(t, f.user), f.host,
		[]ReportEntry{{ExternalID: "620", Name: "Portal 2"}}, map[string]AppDetail{"620": second}, testInterval)
	must(t, err)
	if res.Backfilled != 0 {
		t.Fatalf("second scan Backfilled = %d, want 0", res.Backfilled)
	}
	if got := readStoreDetails(t, f, tileID); !reflect.DeepEqual(got, want) {
		t.Fatalf("details rewritten:\n got %+v\nwant %+v", got, want)
	}
}

// An answer with no genres or credits still marks the tile, so it is not asked about again.
func TestReconcileMarksATitleWithNoStoreDetails(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)
	ctx := context.Background()

	tileID := f.seedDiscoveredTile(t, "1007", "Has one.")
	_, err := f.store.Reconcile(ctx, f.claimedScan(t, f.user), f.host,
		[]ReportEntry{{ExternalID: "1007", Name: "A tool"}}, map[string]AppDetail{"1007": {IsGame: true}}, testInterval)
	must(t, err)
	got := readStoreDetails(t, f, tileID)
	if !got.Written || got.Genres == nil || len(got.Genres) != 0 || got.Developer != nil || got.ControllerSupport != nil {
		t.Fatalf("%+v", got)
	}
	cands, err := f.store.BackfillCandidates(ctx, f.parent, []ReportEntry{{ExternalID: "1007"}})
	must(t, err)
	if len(cands) != 0 {
		t.Fatalf("a marked tile with a description is no longer a candidate: %v", cands)
	}
}

// Not consulted (no answer) leaves the marker unset, so the next scan asks again.
func TestReconcileWithoutAnAnswerLeavesStoreDetailsUnwritten(t *testing.T) {
	pool := testDB(t)
	f := newFixture(t, pool)
	ctx := context.Background()

	tileID := f.seedDiscoveredTile(t, "620", "Has one.")
	_, err := f.store.Reconcile(ctx, f.claimedScan(t, f.user), f.host,
		[]ReportEntry{{ExternalID: "620", Name: "Portal 2"}}, nil, testInterval)
	must(t, err)
	if readStoreDetails(t, f, tileID).Written {
		t.Fatal("marked without a store answer")
	}
	cands, err := f.store.BackfillCandidates(ctx, f.parent, []ReportEntry{{ExternalID: "620"}})
	must(t, err)
	if !reflect.DeepEqual(cands, []string{"620"}) {
		t.Fatalf("candidates %v, want the unmarked tile", cands)
	}
	existing, err := f.store.ExistingTileIDs(ctx, f.parent, []string{"620", "999"})
	must(t, err)
	if !reflect.DeepEqual(existing, map[string]bool{"620": true}) {
		t.Fatalf("existing %v", existing)
	}
}
