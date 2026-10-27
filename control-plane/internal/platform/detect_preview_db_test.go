package platform

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/images"
)

// The preview pass (amendment 48) against a real database: it reads the preview
// tag only on `preview`, keeps one row per source_commit, and tells "not
// published", "invalid" and "unreachable" apart.

type fakePreview struct {
	raw   string
	err   error
	calls int
}

func (f *fakePreview) FetchPreview(context.Context) ([]byte, error) {
	f.calls++
	return []byte(f.raw), f.err
}

func previewDoc(t *testing.T, commit string) string {
	t.Helper()
	return strings.Replace(previewFixture(t), `"source_commit": "`+commitC+`"`, `"source_commit": "`+commit+`"`, 1)
}

func TestDetectPreviewRecordsTheBuildOnlyOnPreview(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	store := NewStore(pool)

	channel := ChannelStable
	preview := &fakePreview{raw: previewDoc(t, commitC)}
	src := twoReleaseSource()
	det := NewDetector(src, store, nil).WithEdge(&fakeEdge{}, channelIs(&channel)).WithPreview(preview)

	for _, c := range []string{ChannelStable, ChannelBeta, ChannelEdge} {
		channel = c
		if _, err := det.Detect(ctx); err != nil {
			t.Fatalf("detect on %s: %v", c, err)
		}
	}
	if preview.calls != 0 {
		t.Fatalf("the preview tag was read %d times off the preview channel", preview.calls)
	}

	channel = ChannelPreview
	rep, err := det.Detect(ctx)
	if err != nil {
		t.Fatalf("detect on preview: %v", err)
	}
	if rep.PreviewCommit != commitC || rep.ManifestInvalid != 0 || rep.Summary()["preview_commit"] != commitC {
		t.Fatalf("report = %+v", rep)
	}
	rows, err := store.Releases(ctx, ChannelPreview)
	if err != nil || len(rows) != 1 {
		t.Fatalf("preview rows = %d, %v; want 1", len(rows), err)
	}
	got := rows[0]
	if got.Version != nil || got.Prerelease || got.Notes != "" || got.SchemaVersion != 107 ||
		string(got.Manifest) == "" {
		t.Fatalf("preview row = %+v", got)
	}
	if _, err := ParsePreviewManifest(got.Manifest); err != nil {
		t.Fatalf("the stored manifest is not the preview document: %v", err)
	}

	// The tag has not moved: no second row. It moves: a second build.
	if rep, _ := det.Detect(ctx); rep.New != 0 {
		t.Fatalf("an unmoved tag added a row: %+v", rep)
	}
	preview.raw = previewDoc(t, commitB)
	if _, err := det.Detect(ctx); err != nil {
		t.Fatal(err)
	}
	if rows, _ := store.Releases(ctx, ChannelPreview); len(rows) != 2 {
		t.Fatalf("preview rows = %d after the tag moved, want 2", len(rows))
	}

	// The preview view serves preview rows only; the stable view none of them.
	view := viewFor(t, ctx, store, ChannelPreview)
	for _, r := range view.Available {
		if r.Channel != ChannelPreview {
			t.Fatalf("the preview view served a %s row", r.Channel)
		}
	}
	if len(view.Available) != 2 {
		t.Fatalf("preview view = %v", ids(view.Available))
	}
	for _, r := range viewFor(t, ctx, store, ChannelStable).Available {
		if r.Channel == ChannelPreview {
			t.Fatal("the stable view served a preview row")
		}
	}
	// The stable pass still runs on every channel (one GitHub listing per pass).
	if src.listCalls != 6 {
		t.Fatalf("stable listings = %d over 6 passes", src.listCalls)
	}
}

func TestDetectPreviewFailures(t *testing.T) {
	notFound := fmt.Errorf("get tag: status 404: %w", images.ErrRegistryNotFound)
	shape := fmt.Errorf("%w: two layers", images.ErrArtifactShape)
	for _, tc := range []struct {
		name     string
		preview  fakePreview
		failsRun bool
		invalid  int
		skipped  bool
	}{
		{"tag not published", fakePreview{err: notFound}, false, 0, true},
		{"unreachable registry", fakePreview{err: errors.New("dial tcp: i/o timeout")}, true, 0, false},
		{"not a single-layer artifact", fakePreview{err: shape}, false, 1, false},
		{"invalid document", fakePreview{raw: strings.Replace(previewFixture(t), `"version": null`, `"version": "0.5.1"`, 1)}, false, 1, false},
		{"not JSON", fakePreview{raw: "<html>"}, false, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := testDB(t)
			ctx := context.Background()
			store := NewStore(pool)
			channel := ChannelPreview
			p := tc.preview
			rep, err := NewDetector(twoReleaseSource(), store, nil).
				WithEdge(&fakeEdge{}, channelIs(&channel)).WithPreview(&p).Detect(ctx)
			if (err != nil) != tc.failsRun {
				t.Fatalf("err = %v, want failed run = %v", err, tc.failsRun)
			}
			if rep.ManifestInvalid != tc.invalid || (rep.PreviewSkipped != "") != tc.skipped {
				t.Fatalf("report = %+v", rep)
			}
			if tc.invalid > 0 && rep.Summary()["manifest_invalid"] != tc.invalid {
				t.Fatalf("summary = %v, want the invalid document counted", rep.Summary())
			}
			if rows, _ := store.Releases(ctx, ChannelPreview); len(rows) != 0 {
				t.Fatalf("a %s stored %d preview rows", tc.name, len(rows))
			}
		})
	}
}
