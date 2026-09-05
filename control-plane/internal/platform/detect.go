package platform

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"
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
}

// Detector runs one detection pass against one source.
type Detector struct {
	source ReleaseSource
	store  *Store
	log    *slog.Logger
}

// NewDetector builds a Detector. log may be nil.
func NewDetector(source ReleaseSource, store *Store, log *slog.Logger) *Detector {
	if log == nil {
		log = slog.Default()
	}
	return &Detector{source: source, store: store, log: log}
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
	return rep, nil
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
