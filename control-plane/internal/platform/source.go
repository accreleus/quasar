package platform

import (
	"context"
	"time"
)

// ReleaseSource is where detection learns what has been published. GitHub
// Releases is the one implementation (github.go); the interface exists so the
// job runs against a fake instead of the live internet, and so #111's edge
// source needs no change to the detector.
type ReleaseSource interface {
	// Any order. A listing failure fails the job and leaves existing rows.
	List(ctx context.Context) ([]Listing, error)

	// Refuses a URL whose host is off the egress allowlist: the asset host is
	// remote-supplied and must pass the containment the listing did.
	FetchManifest(ctx context.Context, url string) ([]byte, error)

	// CompareURL is the human-readable diff between two commits, "" when the
	// source has no such concept.
	CompareURL(fromCommit, toCommit string) string
}

// Listing is one published release before its manifest is read. The MANIFEST,
// not this, is the authority for identity: a tag can be moved and
// target_commitish is often a branch name (ADR 0001).
type Listing struct {
	Tag         string
	Version     string // Tag without a leading "v"
	Prerelease  bool
	Body        string
	PublishedAt time.Time
	ManifestURL string // "" when the release published no manifest asset
}
