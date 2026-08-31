package artwork

import (
	"context"
	"errors"
)

const userAgent = "quasar-control-plane/artwork"

// ExternalSourceSteam is the only external source that exists today. It is the
// value stored in apps.external_source (migration 0042) AND the value passed to
// ArtByExternalRef — one constant so a typo cannot make the two disagree
// silently, which would present as "the appid is set but art never resolves".
const ExternalSourceSteam = "steam"

// ErrArtNotFound means the provider does not know this reference — a 404, not
// a failure. The distinction is irreversible in the wrong direction: "no
// entry" writes a `source='none'` row (never re-asked), a transient failure
// writes no row (retried next sweep). Collapsing them would let one bad
// afternoon upstream permanently gradient the catalogue.
var ErrArtNotFound = errors.New("artwork: the provider has no entry for that reference")

// ErrUnsupportedExternalSource means the provider cannot answer for that
// source at all. An error, never a silent miss — a silent miss would cache as
// `source='none'` and the app would keep a gradient forever with no why.
var ErrUnsupportedExternalSource = errors.New("artwork: unsupported external source")

// Candidate is one possible artwork match: a provider-side title plus the two
// crops that back it. Two crops, not one image scaled — the 2:3 tile (#385)
// and the ~3:1 hero are different source assets; scaling one into the other
// reads as a stretched thumbnail (design notes §15).
type Candidate struct {
	// Ref is the provider's opaque id for the matched title.
	Ref string
	// Name is the provider-side title, shown to an admin so a wrong fuzzy match
	// is visible before it is accepted.
	Name string
	// TileURL is the portrait crop for the 2:3 library tile. May be empty if the
	// provider has no suitable asset — the tile then falls back to the gradient.
	TileURL string
	// HeroURL is the much wider banner crop for the detail/hero panels. May be
	// empty independently of TileURL.
	HeroURL string
	// ThumbURL is a small preview for the admin picker only. NEVER stored and
	// never rendered to a user — the admin picker is the one place a remote URL
	// is loaded directly, and only for an admin who has explicitly opened the
	// picker (see handler.go's note on the hotlinking rule).
	ThumbURL string
	// Attribution is the credit line the provider's terms require, if any.
	Attribution string
}

// Provider is the pluggable artwork source. SteamGridDB is implementation #1;
// the interface exists so a second source (or a purely local one) is a new type,
// not a rewrite. Nothing outside this package knows which provider is in use.
//
// A Provider is only ever constructed when its credentials are configured — a
// deployment with no API key resolves to a nil Provider and the whole fetch
// path is inert (see Service.providerNow).
type Provider interface {
	// Name identifies the provider in stored provenance ("steamgriddb").
	Name() string
	// Search returns candidate matches for a title, best first. An empty slice
	// with a nil error means "no match", which is a NORMAL outcome, not a
	// failure: desktop apps are not in a games database.
	Search(ctx context.Context, query string) ([]Candidate, error)
	// Art resolves the two crops for a chosen candidate ref. Split from Search
	// so the admin picker can list titles cheaply and only resolve full-size
	// assets for the one an operator actually picks.
	Art(ctx context.Context, ref string) (Candidate, error)
	// ArtByExternalRef resolves the two crops for a provider-native external id
	// (a Steam appid), skipping Search entirely. An exact id beats any fuzzy title
	// match by construction.
	//
	// Returns ErrArtNotFound when the provider simply has no entry for that id
	// (a normal "no art" outcome), and ErrUnsupportedExternalSource when it
	// cannot answer for that source at all. Every other error is transient.
	ArtByExternalRef(ctx context.Context, source, id string) (Candidate, error)
}

// Where the provider credential in effect came from. These are the values of
// `provider_origin` on the wire; they mirror internal/secrets' origins, plus
// "static" for a Provider handed in directly at construction (embedded and test
// wiring — never produced by the shipped server).
const (
	OriginNone        = "none"
	OriginDatabase    = "database"
	OriginEnvironment = "environment"
	OriginStatic      = "static"
)

// ProviderInfo describes the provider in effect WITHOUT exposing any
// credential. It is what the admin UI renders and what the log line records.
type ProviderInfo struct {
	// Configured is true when a usable credential resolved and a Provider exists.
	Configured bool
	// Name is the provider's name ("steamgriddb"), or "".
	Name string
	// Origin is where the credential came from: "database" (an admin set it),
	// "environment" (the legacy env var) or "none". Surfaced so an operator is
	// never guessing which of two sources the server actually used.
	Origin string
	// Problem is a short operator-facing reason the provider is unavailable
	// despite something being configured (e.g. the master key does not match the
	// stored key). "" when there is nothing to explain.
	Problem string
}

// ProviderSource yields the Provider in effect right now. A function, not a
// field: a key baked in at boot made an admin-UI save appear to work and then
// do nothing until a restart. Resolving per use is what keeps the UI honest.
type ProviderSource interface {
	// Provider returns the live provider, or nil with an explanatory
	// ProviderInfo when none is configured. Implementations are expected to be
	// cheap and to reuse the underlying client while the credential is unchanged
	// (the throttle state lives on the client).
	Provider(ctx context.Context) (Provider, ProviderInfo)
}

// staticSource is a fixed provider. Used by tests and by any caller that
// already holds a constructed Provider.
type staticSource struct{ p Provider }

// StaticProviderSource wraps an already-constructed Provider (or nil for "no
// provider") as a ProviderSource.
func StaticProviderSource(p Provider) ProviderSource { return staticSource{p: p} }

func (s staticSource) Provider(context.Context) (Provider, ProviderInfo) {
	if s.p == nil {
		return nil, ProviderInfo{Origin: OriginNone}
	}
	return s.p, ProviderInfo{Configured: true, Name: s.p.Name(), Origin: OriginStatic}
}
