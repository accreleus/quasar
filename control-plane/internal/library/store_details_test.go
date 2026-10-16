package library

import (
	"reflect"
	"testing"
)

func TestLookupOrderPutsNewTitlesThenBackfillAhead(t *testing.T) {
	publish := []string{"10", "20", "30", "40"}
	backfill := []string{"20", "50"}
	existing := map[string]bool{"10": true, "20": true}
	got := lookupOrder(publish, backfill, existing)
	want := []string{"30", "40", "20", "50", "10"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got := lookupOrder(nil, nil, nil); len(got) != 0 {
		t.Fatalf("nothing to look up, got %v", got)
	}
}
