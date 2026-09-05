package platform

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/accreleus/quasar/control-plane/internal/buildinfo"
)

// Stable-channel detection: list the configured repository's releases, validate
// each new one's manifest, record it. The jobs.Definition that schedules this is
// in cmd/quasar-control/app.go; a test runs a pass without the dispatcher.
//
// Idempotent (the store keys on channel+source_commit) and non-destructive: an
// unreachable listing leaves every existing row as it was.

// DetectJobID is the jobs-framework id AND the id the release view reads run
// history under; the two must stay one name.
const DetectJobID = "platform.release_detect"

// NotesMaxBytes bounds a release body — untrusted upstream text.
const NotesMaxBytes = 64 << 10

// Report is one detection pass's outcome, and the job summary's contents.
type Report struct {
	Seen            int
	New             int
	Updated         int
	ManifestInvalid int
	// Per-release manifest failures. A pass with these and no listing failure
	// still SUCCEEDS: a broken publish must not hide a good release.
	Errors []string

	// The edge half (#111), filled only on that channel.
	EdgeBranch string
	EdgeTag    string
	EdgeCommit string
	EdgeSchema int
	// EdgeSkipped is why no edge row was written on a pass that succeeded.
	EdgeSkipped string
	// EdgeComponents is `image@digest` per component. The row cannot carry them
	// (schema.md: manifest is NULL on edge), so the run record does.
	EdgeComponents []string
}

// ChannelFunc reads the instance's channel and edge branch. Called per PASS:
// a channel switch must take effect on the next run with no restart.
type ChannelFunc func(ctx context.Context) (channel, edgeBranch string, err error)

// Detector runs one detection pass against one source.
type Detector struct {
	source ReleaseSource
	store  *Store
	log    *slog.Logger

	// Both nil leaves Detect stable-only.
	edge    EdgeResolver
	channel ChannelFunc
}

// NewDetector builds a Detector. log may be nil.
func NewDetector(source ReleaseSource, store *Store, log *slog.Logger) *Detector {
	if log == nil {
		log = slog.Default()
	}
	return &Detector{source: source, store: store, log: log}
}

// WithEdge enables the edge channel (#111): each pass reads the instance's
// channel and, on `edge`, also resolves the branch build.
func (d *Detector) WithEdge(edge EdgeResolver, channel ChannelFunc) *Detector {
	if edge == nil || channel == nil {
		return d
	}
	d.edge = edge
	d.channel = channel
	return d
}

// Detect runs one pass over the stable channel.
//
// A returned error is a FAILED run (the listing was unreachable); stored rows
// are untouched and the caller records it as last_error. An invalid manifest is
// NOT a failed run: it is counted and the release is not stored, because
// nothing pins it (ADR 0001) and its three NOT NULL identity columns would have
// to be invented.
func (d *Detector) Detect(ctx context.Context) (Report, error) {
	var rep Report

	listings, err := d.source.List(ctx)
	if err != nil {
		return rep, err
	}
	existing, err := d.store.Releases(ctx, ChannelStable)
	if err != nil {
		return rep, err
	}

	known := make(map[string]struct{}, len(existing))
	for _, r := range existing {
		if r.Version != nil {
			known[*r.Version] = struct{}{}
		}
	}

	fresh := make([]Release, 0, len(listings))
	for _, l := range listings {
		rep.Seen++
		if _, ok := known[l.Version]; ok {
			continue
		}
		rel, err := d.resolve(ctx, l)
		if err != nil {
			rep.ManifestInvalid++
			rep.Errors = append(rep.Errors, fmt.Sprintf("%s: %v", l.Tag, err))
			d.log.Warn("platform release manifest rejected", "tag", l.Tag, "err", err)
			continue
		}
		fresh = append(fresh, rel)
	}

	for _, rel := range fresh {
		inserted, err := d.store.UpsertRelease(ctx, rel)
		if err != nil {
			return rep, err
		}
		if inserted {
			rep.New++
		} else {
			rep.Updated++
		}
	}

	// The stable pass runs on both channels, so switching back to stable needs
	// no re-detection (schema.md); only the edge pass is conditional.
	if err := d.detectEdge(ctx, &rep); err != nil {
		return rep, err
	}
	return rep, nil
}

// detectEdge records the branch build while the instance is on edge. A returned
// error fails the run, which is how it reaches the view's `last_error`.
func (d *Detector) detectEdge(ctx context.Context, rep *Report) error {
	if d.edge == nil || d.channel == nil {
		return nil
	}
	channel, branch, err := d.channel(ctx)
	if err != nil {
		return fmt.Errorf("read the release channel: %w", err)
	}
	if channel != ChannelEdge {
		return nil
	}
	rep.EdgeBranch = branch

	build, err := d.edge.Resolve(ctx, branch)
	switch {
	case errors.Is(err, ErrEdgeSchemaUnknown):
		// Not a failure. schema_version is the ADR 0002 ordering key, so a
		// guessed one could offer a downgrade; skip instead.
		rep.EdgeSkipped = err.Error()
		d.log.Warn("edge build skipped", "branch", branch, "err", err)
		return nil
	case err != nil:
		// Includes ErrEdgeComponentsDisagree, which is not a
		// PlatformReleaseFault (closed vocabulary — edge.go).
		return fmt.Errorf("edge branch %q: %w", branch, err)
	}

	rep.EdgeTag = build.Tag
	rep.EdgeCommit = build.SourceCommit
	rep.EdgeSchema = build.SchemaVersion
	for _, c := range build.Components {
		rep.EdgeComponents = append(rep.EdgeComponents, c.Image+"@"+c.Digest)
	}

	rel := Release{
		Channel:       ChannelEdge,
		Version:       nil, // an edge build is a commit, never a version
		SourceCommit:  build.SourceCommit,
		BuiltAt:       build.BuiltAt,
		SchemaVersion: build.SchemaVersion,
		Prerelease:    false,
		Notes:         "", // no notes on a branch; compare_url stands in
		// Manifest stays nil: NULL on edge (schema.md / openapi.yaml).
	}
	// Null on an unstamped build: nothing to compare from.
	if cp := buildinfo.Get().SourceCommit; cp != nil {
		if u := d.source.CompareURL(*cp, build.SourceCommit); u != "" {
			rel.CompareURL = &u
		}
	}

	// Idempotent on (edge, source_commit).
	inserted, err := d.store.UpsertRelease(ctx, rel)
	if err != nil {
		return err
	}
	rep.Seen++
	if inserted {
		rep.New++
	} else {
		rep.Updated++
	}
	return nil
}

// resolve turns one listing into its row. The manifest is the identity: a tag
// can be moved, so every identity field comes from the asset.
func (d *Detector) resolve(ctx context.Context, l Listing) (Release, error) {
	raw, err := d.source.FetchManifest(ctx, l.ManifestURL)
	if err != nil {
		return Release{}, err
	}
	m, err := ParseManifest(raw)
	if err != nil {
		return Release{}, err
	}
	// Notes and manifest come from one tag; a disagreement means neither can
	// be trusted.
	if l.Version != "" && m.Version != l.Version {
		return Release{}, fmt.Errorf("manifest version %q does not match tag %q", m.Version, l.Tag)
	}
	if m.Prerelease != l.Prerelease {
		return Release{}, fmt.Errorf("manifest prerelease=%v does not match the tag's %v", m.Prerelease, l.Prerelease)
	}

	version := m.Version
	return Release{
		Channel:       ChannelStable,
		Version:       &version,
		SourceCommit:  m.SourceCommit,
		BuiltAt:       m.BuiltAtTime(),
		SchemaVersion: m.SchemaVersion,
		Prerelease:    m.Prerelease,
		Notes:         boundNotes(l.Body),
		Manifest:      raw,
		// compare_url stays NULL on stable: the notes ARE the diff
		// (control-api.md §Platform releases). It is the edge channel's field,
		// and #111 is what fills it.
	}, nil
}

// Truncates on a rune boundary: a cut mid-rune is not valid UTF-8 and will not
// store.
func boundNotes(body string) string {
	if len(body) <= NotesMaxBytes {
		return body
	}
	cut := NotesMaxBytes
	for cut > 0 && !utf8.RuneStart(body[cut]) {
		cut--
	}
	return strings.TrimRight(body[:cut], "\x00") + "\n\n…(truncated)"
}

// Summary renders the report as the job's summary map.
func (r Report) Summary() map[string]any {
	s := map[string]any{
		"releases_seen":    r.Seen,
		"releases_new":     r.New,
		"releases_updated": r.Updated,
		"manifest_invalid": r.ManifestInvalid,
	}
	if r.EdgeBranch != "" {
		s["edge_branch"] = r.EdgeBranch
		if r.EdgeTag != "" {
			s["edge_tag"] = r.EdgeTag
		}
		if r.EdgeCommit != "" {
			s["edge_commit"] = r.EdgeCommit
			s["edge_schema_version"] = r.EdgeSchema
		}
		if r.EdgeSkipped != "" {
			s["edge_skipped"] = r.EdgeSkipped
		}
		if len(r.EdgeComponents) > 0 {
			s["edge_components"] = r.EdgeComponents
		}
	}
	if len(r.Errors) > 0 {
		// Bounded: the summary column has a 4096-byte ceiling.
		errs := r.Errors
		if len(errs) > 5 {
			errs = errs[:5]
		}
		s["manifest_errors"] = errs
	}
	return s
}

// DetectionStatus is the pair the view reports as checked_at / last_error.
type DetectionStatus struct {
	CheckedAt *time.Time
	LastError *string
}
