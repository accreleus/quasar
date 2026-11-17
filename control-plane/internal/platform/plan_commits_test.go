package platform

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/buildinfo"
)

// previewRel is a preview row whose manifest lists the given commits, newest first.
func previewRel(commits ...string) Release {
	type entry struct {
		Commit  string `json:"commit"`
		Subject string `json:"subject"`
	}
	list := make([]entry, len(commits))
	for i, c := range commits {
		list[i] = entry{c, fmt.Sprintf("#%d change", i)}
	}
	doc := map[string]any{"format_version": 2, "version": nil, "commits": list}
	if commits == nil {
		delete(doc, "commits")
	}
	raw, _ := json.Marshal(doc)
	r := rel("p", "", commitA, 76, at(5))
	r.Channel, r.Version, r.Manifest = ChannelPreview, nil, raw
	return r
}

// new_commit_count (amendment 48): the entries before the installed commit, or
// all of them when the installed commit is not on the list; null without a list
// and on every channel but preview.
func TestNewCommitCount(t *testing.T) {
	list := []string{commitA, commitB, commitC}
	absent := previewRel()
	empty := previewRel()
	empty.Manifest = json.RawMessage(`{"commits":[]}`)
	stable := rel("s", "0.2.0", commitA, 76, at(5))
	stable.Manifest = previewRel(list...).Manifest // a stable row never counts

	for _, tc := range []struct {
		name      string
		installed string
		release   Release
		channel   string
		want      *int
	}{
		{"installed is the newest", commitA, previewRel(list...), ChannelPreview, intp(0)},
		{"installed is mid-list", commitB, previewRel(list...), ChannelPreview, intp(1)},
		{"installed is the oldest", commitC, previewRel(list...), ChannelPreview, intp(2)},
		{"installed is not on the list", commitD, previewRel(list...), ChannelPreview, intp(3)},
		{"installed commit unknown", "", previewRel(list...), ChannelPreview, intp(3)},
		{"empty list", commitD, empty, ChannelPreview, intp(0)},
		{"manifest without commits", commitB, absent, ChannelPreview, nil},
		{"stable row", commitB, stable, ChannelStable, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view := PlanRelease(PlanInputs{Channel: tc.channel, ControlPlane: cp(tc.installed, 76), Releases: []Release{tc.release}})
			if len(view.Available) != 1 {
				t.Fatalf("available = %v, want the one row", ids(view.Available))
			}
			got := view.Available[0].NewCommitCount
			if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
				t.Errorf("new_commit_count = %v, want %v", derefInt(got), derefInt(tc.want))
			}
			b, _ := json.Marshal(view.Available[0])
			var wire map[string]json.RawMessage
			if err := json.Unmarshal(b, &wire); err != nil {
				t.Fatal(err)
			}
			if _, ok := wire["new_commit_count"]; !ok {
				t.Error("new_commit_count must always be serialized, null when it has no answer")
			}
		})
	}
}

func intp(n int) *int { return &n }

// Amendment 83: an installed control plane NEWER than the release by the edge
// ordering rule (same schema_version, later built_at) runs every commit the
// release carries, so new_commit_count is 0 — not the whole list, which is what
// "installed commit is not on the list" would answer.
func TestNewCommitCountIsZeroWhenInstalledIsNewer(t *testing.T) {
	// The preview row was built at(5); installed commit is mid-list, so the
	// ordinary answer would be 1.
	row := previewRel(commitA, commitB, commitC)
	installedNewer := cp(commitB, 76)
	installedNewer.BuiltAt = str(at(6).Format(time.RFC3339))
	installedOlder := cp(commitB, 76)
	installedOlder.BuiltAt = str(at(4).Format(time.RFC3339))

	for _, tc := range []struct {
		name     string
		cp       buildinfo.Identity
		wantZero bool
	}{
		{"installed built later at the same schema", installedNewer, true},
		{"installed built earlier", installedOlder, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view := PlanRelease(PlanInputs{Channel: ChannelPreview, ControlPlane: tc.cp, Releases: []Release{row}})
			if len(view.Available) != 1 {
				t.Fatalf("available = %v, want the one row", ids(view.Available))
			}
			got := view.Available[0].NewCommitCount
			if got == nil {
				t.Fatal("new_commit_count = null, want a count on a preview row with a commit list")
			}
			if (got != nil && *got == 0) != tc.wantZero {
				t.Fatalf("new_commit_count = %d, want zero=%v", *got, tc.wantZero)
			}
		})
	}
}

func derefInt(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}
