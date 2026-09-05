package platform

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/buildinfo"
)

// The edge pass against a real database (#111): one row per branch build,
// idempotent on (edge, source_commit), and a channel switch changing which
// rows the view serves without re-detecting the other channel.

// fakeEdge is a branch whose build the test moves.
type fakeEdge struct {
	build EdgeBuild
	err   error
	calls int
}

func (f *fakeEdge) Resolve(context.Context, string) (EdgeBuild, error) {
	f.calls++
	if f.err != nil {
		return EdgeBuild{}, f.err
	}
	return f.build, nil
}

func edgeBuild(commit string, schema int, built time.Time) EdgeBuild {
	return EdgeBuild{
		Tag:           "develop",
		SourceCommit:  commit,
		BuiltAt:       built,
		SchemaVersion: schema,
		Components: []ManifestComponent{
			{Name: "control-plane", Image: "ghcr.io/accreleus/quasar/quasar-control-plane", Digest: "sha256:" + hex64},
			{Name: "node-agent", Image: "ghcr.io/accreleus/quasar/quasar-node-agent", Digest: "sha256:" + hex64},
		},
	}
}

// channelIs returns a ChannelFunc over a variable the test can flip.
func channelIs(channel *string) ChannelFunc {
	return func(context.Context) (string, string, error) { return *channel, "develop", nil }
}

func TestDetectEdgeUpsertsOneRowAndIsIdempotent(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	store := NewStore(pool)

	channel := ChannelEdge
	edge := &fakeEdge{build: edgeBuild(commitC, 76, at(4))}
	det := NewDetector(twoReleaseSource(), store, nil).WithEdge(edge, channelIs(&channel))

	rep, err := det.Detect(ctx)
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if rep.EdgeCommit != commitC || rep.EdgeSchema != 76 || rep.EdgeSkipped != "" {
		t.Fatalf("report = %+v", rep)
	}
	if len(rep.EdgeComponents) != 2 {
		t.Fatalf("edge components = %v, want both digests in the run record", rep.EdgeComponents)
	}

	rows, err := store.Releases(ctx, ChannelEdge)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("stored %d edge rows, want 1", len(rows))
	}
	got := rows[0]
	if got.Version != nil {
		t.Errorf("version = %v, want NULL on edge", *got.Version)
	}
	if got.Notes != "" || len(got.Manifest) != 0 {
		t.Errorf("edge row carries notes %q / manifest %s, want neither", got.Notes, got.Manifest)
	}
	if got.SourceCommit != commitC || got.SchemaVersion != 76 {
		t.Errorf("identity = %s/%d", got.SourceCommit, got.SchemaVersion)
	}
	// compare_url is set exactly when this build knows its own commit.
	if buildinfo.Get().SourceCommit == nil {
		if got.CompareURL != nil {
			t.Errorf("compare_url = %v, want null with an unstamped control plane", *got.CompareURL)
		}
	} else if got.CompareURL == nil {
		t.Error("compare_url must stand in for the notes an edge build has none of")
	}

	// The branch has not moved: the same row, no duplicate.
	rep2, err := det.Detect(ctx)
	if err != nil {
		t.Fatalf("second detect: %v", err)
	}
	if rep2.New != 0 {
		t.Fatalf("second report = %+v, want nothing new", rep2)
	}
	rows2, _ := store.Releases(ctx, ChannelEdge)
	if len(rows2) != 1 || rows2[0].ID != got.ID {
		t.Fatalf("re-running detection changed the edge rows: %+v", rows2)
	}

	// The branch moves: a second build, not a rewritten one.
	edge.build = edgeBuild(commitB, 76, at(5))
	if _, err := det.Detect(ctx); err != nil {
		t.Fatalf("third detect: %v", err)
	}
	rows3, _ := store.Releases(ctx, ChannelEdge)
	if len(rows3) != 2 {
		t.Fatalf("stored %d edge rows, want 2", len(rows3))
	}
}

func TestDetectChannelSwitchChangesWhatIsServed(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	store := NewStore(pool)

	channel := ChannelEdge
	edge := &fakeEdge{build: edgeBuild(commitC, 74, at(6))}
	det := NewDetector(twoReleaseSource(), store, nil).WithEdge(edge, channelIs(&channel))
	if _, err := det.Detect(ctx); err != nil {
		t.Fatalf("detect: %v", err)
	}

	// Both channels were recorded in one pass, so a switch needs no re-detection.
	view := viewFor(t, ctx, store, ChannelEdge)
	if len(view.Available) != 1 || view.Available[0].Channel != ChannelEdge {
		t.Fatalf("edge view = %+v", ids(view.Available))
	}
	view = viewFor(t, ctx, store, ChannelStable)
	for _, r := range view.Available {
		if r.Channel != ChannelStable {
			t.Fatalf("stable view served a %s row", r.Channel)
		}
	}

	// On stable the edge pass does not run at all.
	channel = ChannelStable
	before := edge.calls
	if _, err := det.Detect(ctx); err != nil {
		t.Fatalf("stable detect: %v", err)
	}
	if edge.calls != before {
		t.Fatalf("the edge source was consulted %d times on the stable channel", edge.calls-before)
	}
}

func TestDetectEdgeDisagreementFailsTheRun(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	store := NewStore(pool)

	channel := ChannelEdge
	edge := &fakeEdge{err: ErrEdgeComponentsDisagree}
	det := NewDetector(twoReleaseSource(), store, nil).WithEdge(edge, channelIs(&channel))

	_, err := det.Detect(ctx)
	if err == nil || !errors.Is(err, ErrEdgeComponentsDisagree) {
		t.Fatalf("err = %v, want the disagreement to fail the run (it becomes last_error)", err)
	}
	rows, _ := store.Releases(ctx, ChannelEdge)
	if len(rows) != 0 {
		t.Fatalf("stored %d edge rows, want none from a disagreeing pair", len(rows))
	}
	// The stable half of the same pass still landed.
	stable, _ := store.Releases(ctx, ChannelStable)
	if len(stable) != 2 {
		t.Fatalf("stable rows = %d, want the stable pass to have completed", len(stable))
	}
}

func TestDetectEdgeSkipsAnUnorderableBuild(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	store := NewStore(pool)

	channel := ChannelEdge
	edge := &fakeEdge{err: ErrEdgeSchemaUnknown}
	det := NewDetector(twoReleaseSource(), store, nil).WithEdge(edge, channelIs(&channel))

	rep, err := det.Detect(ctx)
	if err != nil {
		t.Fatalf("an image without the schema label must not fail the run: %v", err)
	}
	if rep.EdgeSkipped == "" {
		t.Fatal("the skip must be recorded in the run summary")
	}
	if _, ok := rep.Summary()["edge_skipped"]; !ok {
		t.Fatalf("summary = %v, want edge_skipped", rep.Summary())
	}
	rows, _ := store.Releases(ctx, ChannelEdge)
	if len(rows) != 0 {
		t.Fatalf("stored %d edge rows, want none for an unorderable build", len(rows))
	}
}

func viewFor(t *testing.T, ctx context.Context, store *Store, channel string) View {
	t.Helper()
	rows, err := store.Releases(ctx, channel)
	if err != nil {
		t.Fatalf("read %s releases: %v", channel, err)
	}
	return PlanRelease(PlanInputs{
		Channel:      channel,
		EdgeBranch:   "develop",
		ControlPlane: buildinfo.Identity{Version: "dev", SchemaVersion: 1},
		Releases:     rows,
	})
}
