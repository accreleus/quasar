package session

import (
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/telemetry"
)

// The taxonomy is DERIVED from the metric manifest. This test is what makes that
// claim checkable rather than a comment: the derived list and the manifest's
// taxonomy column must agree exactly, in both directions.
func TestTaxonomyV1IsDerivedFromManifest(t *testing.T) {
	want := map[string][2]string{}
	for _, e := range telemetry.Manifest().TaxonomyEntries() {
		want[e.Taxonomy] = [2]string{e.Source, e.Key}
	}
	got := map[string][2]string{}
	for _, m := range taxonomyV1 {
		if _, dup := got[m.name]; dup {
			t.Errorf("taxonomy name %q appears twice", m.name)
		}
		got[m.name] = [2]string{m.source, m.rawKey}
	}
	for name, src := range want {
		if got[name] != src {
			t.Errorf("taxonomy %q: manifest says %v, derived list says %v", name, src, got[name])
		}
	}
	for name := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("taxonomy %q is in the derived list but not the manifest", name)
		}
	}
}

// Every falsifier name a verdict can emit must be a real taxonomy series in the
// manifest. A falsifier naming a series that does not exist reports `n: 0`,
// `value: null` forever and reads as a measurement that simply never fired —
// the single most misleading thing this surface can do (trace-format.md §8).
//
// The list is PINNED rather than reflected out of verdict.go: a falsifier
// silently disappearing is as much a defect as one naming a bad series, and only
// a written-down expectation catches that.
func TestVerdictFalsifierNamesExistInManifest(t *testing.T) {
	pinned := []string{
		"client.is_hidden",
		"client.present_beat_fraction",
		"client.present_interval_sd_ms",
		"client.present_long_frames",
		"encoder.encode_ms",
		"encoder.fps",
		"transport.packets_lost",
		"transport.rtt_ms",
	}
	known := map[string]bool{}
	for _, e := range telemetry.Manifest().TaxonomyEntries() {
		known[e.Taxonomy] = true
	}
	for _, name := range pinned {
		if !known[name] {
			t.Errorf("falsifier series %q is not in the metric manifest", name)
		}
	}
}
