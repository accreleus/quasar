package artwork

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode"
)

// ErrProviderNotConfigured is returned by the provider-backed operations when
// no API key is set. It is NOT an internal error: an unconfigured deployment is
// the DEFAULT and must behave exactly as it did before this feature existed.
var ErrProviderNotConfigured = errors.New("artwork: no artwork provider is configured")

// ErrInvalidRequest marks a caller mistake (an unknown crop, no URL supplied,
// a match with no usable asset). Separated from internal failures so the HTTP
// layer answers 400 for the first and 500 for the second, without either
// masquerading as the other.
var ErrInvalidRequest = errors.New("artwork: invalid request")

// ErrLocked marks a refusal to overwrite an admin's manual correction
// (migration 0039): a correction must never be silently re-broken, so both the
// single-app and bulk paths refuse and a caller that means it passes force.
var ErrLocked = errors.New("artwork: this app's artwork is locked")

// Crop names on the wire.
const (
	CropTile = "tile"
	CropHero = "hero"
)

// artworklessKinds are the apps.kind values a games artwork provider will
// never index; Resolve short-circuits them to source='none' — no provider
// call, no app name sent to a third party (§4.5.4). A set so a fourth kind has
// to answer the question here, once. The only read of apps.kind on any server
// path — see the short-circuit in Resolve.
var artworklessKinds = map[string]bool{
	"desktop":  true,
	"launcher": true,
}

// Service owns artwork resolution: match, fetch, cache, store, and the admin
// override paths. Ship-dark by construction: with no API key the provider is
// nil, no outbound request is ever made, and every app keeps the gradient
// tile; the local half (cache, upload, override, clear, serving) stays
// available. The provider is resolved per use, not captured at construction —
// see ProviderSource.
type Service struct {
	store   *Store
	blobs   *BlobStore
	fetcher *Fetcher
	source  ProviderSource
	log     *slog.Logger

	sweepInterval time.Duration
	sweepBatch    int
}

// Options configures the service.
type Options struct {
	// ProviderSource resolves the provider on every use. Preferred over
	// Provider; when both are nil the deployment has no provider at all.
	ProviderSource ProviderSource
	// Provider is a fixed provider, wrapped into a static ProviderSource. Kept
	// for callers (and tests) that already hold a constructed Provider. Ignored
	// when ProviderSource is set.
	Provider Provider
	// MaxImageBytes caps every stored image (fetched or uploaded).
	MaxImageBytes int64
	// FetchTimeout bounds one outbound image fetch.
	FetchTimeout time.Duration
	// SweepInterval is how often the background resolver looks for apps with no
	// artwork row. Zero disables the sweep entirely (used by tests, which drive
	// Resolve directly).
	SweepInterval time.Duration
	// SweepBatch caps how many apps one sweep resolves, so a large catalogue
	// spreads its third-party calls over many sweeps instead of one burst.
	SweepBatch int
}

// New builds the service. blobRoot is created if absent.
func New(store *Store, blobRoot string, opts Options, log *slog.Logger) (*Service, error) {
	blobs, err := NewBlobStore(blobRoot)
	if err != nil {
		return nil, err
	}
	if opts.MaxImageBytes <= 0 {
		opts.MaxImageBytes = DefaultMaxImageBytes
	}
	if opts.FetchTimeout <= 0 {
		opts.FetchTimeout = 30 * time.Second
	}
	if opts.SweepBatch <= 0 {
		opts.SweepBatch = 25
	}
	source := opts.ProviderSource
	if source == nil {
		source = StaticProviderSource(opts.Provider)
	}
	return &Service{
		store:         store,
		blobs:         blobs,
		fetcher:       NewFetcher(opts.FetchTimeout, opts.MaxImageBytes),
		source:        source,
		log:           log,
		sweepInterval: opts.SweepInterval,
		sweepBatch:    opts.SweepBatch,
	}, nil
}

// ProviderStatus reports whether a third-party provider is available right now,
// where its credential came from, and — when it is not available despite
// something being configured — why. The admin UI reads this to explain the
// absence of the search controls instead of showing a button that silently does
// nothing.
func (s *Service) ProviderStatus(ctx context.Context) ProviderInfo {
	_, info := s.source.Provider(ctx)
	return info
}

// ProviderUnavailableError carries the reason no provider is in effect as
// data; it wraps ErrProviderNotConfigured so errors.Is callers are unchanged.
// A type rather than a formatted message so the HTTP layer never recovers the
// reason by string surgery on err.Error() — one future `Problem: err.Error()`
// and that surgery would ship an arbitrary internal error to a client.
type ProviderUnavailableError struct {
	// Info is the provider status that produced this error. Info.Problem is the
	// only operator-facing text in it.
	Info ProviderInfo
}

func (e *ProviderUnavailableError) Error() string {
	if e.Info.Problem == "" {
		return ErrProviderNotConfigured.Error()
	}
	return ErrProviderNotConfigured.Error() + ": " + e.Info.Problem
}

// Unwrap keeps errors.Is(err, ErrProviderNotConfigured) true.
func (e *ProviderUnavailableError) Unwrap() error { return ErrProviderNotConfigured }

// providerNow resolves the provider for one operation. Returns a
// *ProviderUnavailableError (never a nil Provider with a nil error) so no caller
// can forget the check.
func (s *Service) providerNow(ctx context.Context) (Provider, error) {
	p, info := s.source.Provider(ctx)
	if p == nil {
		return nil, &ProviderUnavailableError{Info: info}
	}
	return p, nil
}

// Blobs exposes the cache (used by the asset handler).
func (s *Service) Blobs() *BlobStore { return s.blobs }

// Get returns an app's artwork record, or ok=false when it has none.
func (s *Service) Get(ctx context.Context, appID string) (Record, bool, error) {
	if _, err := s.store.App(ctx, appID); err != nil {
		return Record{}, false, err
	}
	return s.store.Get(ctx, appID)
}

// Search asks the provider for candidate matches. Returns
// ErrProviderNotConfigured when the feature is off — never a fake result.
func (s *Service) Search(ctx context.Context, appID, query string) ([]Candidate, error) {
	app, err := s.store.App(ctx, appID)
	if err != nil {
		return nil, err
	}
	provider, err := s.providerNow(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(query) == "" {
		query = app.Name
	}
	cands, err := provider.Search(ctx, query)
	if err != nil {
		return nil, err
	}
	// The picker renders these; the CSP (img-src 'self' data:) will never let
	// a browser hotlink the provider's CDN, so the previews ship inlined
	// (thumbs.go). Best-effort: a failed inline is a glyph, not an error.
	inlineSearchThumbs(ctx, s.fetcher, cands)
	return cands, nil
}

// Resolve is the automatic path: identify an app's art and cache the result.
// An app carrying a provider external id (migration 0042) resolves by id and
// never searches — exact beats fuzzy; everything else accepts only an exact
// normalised title match (§12.1), a near miss is `source='none'`, not a guess.
// It writes a row for every decision (the row stops the next sweep asking
// again) and no row on a provider error — a transient outage must not be
// cached as "no art". Never touches an app that already has any row.
func (s *Service) Resolve(ctx context.Context, app appRef) error {
	if existing, ok, err := s.store.Get(ctx, app.ID); err != nil {
		return err
	} else if ok {
		s.log.Debug("artwork: already resolved", "app_id", app.ID, "source", existing.Source)
		return nil
	}

	// An exact id beats any title match (spec §12), and sits ahead of both the
	// kind short-circuit and Search — no app name is ever sent to a third
	// party for these apps.
	if app.ExternalSource == ExternalSourceSteam && app.ExternalID != "" {
		return s.resolveByExternalRef(ctx, app)
	}

	// Desktop/launcher apps are a first-class case, not an error path: a games
	// database has no entry for Blender, and it mis-matches the Steam client
	// confidently ("Steam (Dev)" → "Steam Dev Days"), so the gradient tile is
	// the better outcome (§4.5.4); an admin uploads art instead. This narrow
	// presentation decision is the one permitted read of apps.kind — nothing
	// in scheduling, admission, placement or discovery may branch on kind
	// (spec §4.5.3).
	if artworklessKinds[app.Kind] {
		return s.store.Save(ctx, Record{
			AppID:       app.ID,
			Source:      SourceNone,
			MatchedName: "",
		})
	}

	provider, err := s.providerNow(ctx)
	if err != nil {
		return err
	}

	candidates, err := provider.Search(ctx, app.Name)
	if err != nil {
		return fmt.Errorf("artwork: search %q: %w", app.Name, err)
	}
	if len(candidates) == 0 {
		return s.store.Save(ctx, Record{AppID: app.ID, Source: SourceNone})
	}

	// Only an exact normalised title match is accepted (spec §12.1): no
	// scorer, no threshold, no ranking. Taking candidates[0] was wrong 7 times
	// out of 7 on the live catalogue, so a near miss is `source='none'` and
	// the admin picker (Service.Search) shows the options this loop declined.
	best, ok := pickExactTitleMatch(app.Name, candidates)
	if !ok {
		// No ProviderRef and no MatchedName — nothing was matched, and a
		// stored MatchedName would read in the admin UI as "we matched this
		// title", the false claim §12.1 exists to stop.
		return s.store.Save(ctx, Record{AppID: app.ID, Source: SourceNone})
	}
	art, err := provider.Art(ctx, best.Ref)
	if err != nil {
		return fmt.Errorf("artwork: resolve art for %q: %w", best.Name, err)
	}
	if art.TileURL == "" && art.HeroURL == "" {
		// A matched title with no usable asset is still a decision: nothing to
		// fetch now, and asking again next sweep would not change that.
		return s.store.Save(ctx, Record{
			AppID:       app.ID,
			Source:      SourceNone,
			Provider:    provider.Name(),
			ProviderRef: best.Ref,
			MatchedName: best.Name,
		})
	}

	tile, hero, err := s.cacheCrops(ctx, art.TileURL, art.HeroURL)
	if err != nil {
		return err
	}
	return s.store.Save(ctx, Record{
		AppID:       app.ID,
		Source:      SourceProvider,
		Provider:    provider.Name(),
		ProviderRef: best.Ref,
		MatchedName: best.Name,
		TileAsset:   tile,
		HeroAsset:   hero,
		Attribution: firstNonEmpty(art.Attribution, best.Attribution),
		Locked:      false,
	})
}

// resolveByExternalRef is the by-appid arm of Resolve (spec §12): Search is
// never called. Row-writing rule as in Resolve, resting on ErrArtNotFound:
// no entry (404) is a decision → `source='none'`; anything else → no row,
// retried next sweep.
func (s *Service) resolveByExternalRef(ctx context.Context, app appRef) error {
	provider, err := s.providerNow(ctx)
	if err != nil {
		return err
	}

	art, err := provider.ArtByExternalRef(ctx, app.ExternalSource, app.ExternalID)
	if err != nil && !errors.Is(err, ErrArtNotFound) {
		return fmt.Errorf("artwork: resolve art for %s:%s: %w", app.ExternalSource, app.ExternalID, err)
	}

	// provider_ref is the external id (spec §12) — the identifier that
	// actually resolved the art. matched_name is whatever the provider
	// volunteered; "" is honest, nothing was matched by title.
	if err != nil || (art.TileURL == "" && art.HeroURL == "") {
		return s.store.Save(ctx, Record{
			AppID:       app.ID,
			Source:      SourceNone,
			Provider:    provider.Name(),
			ProviderRef: app.ExternalID,
			MatchedName: art.Name,
		})
	}

	tile, hero, err := s.cacheCrops(ctx, art.TileURL, art.HeroURL)
	if err != nil {
		return err
	}
	return s.store.Save(ctx, Record{
		AppID:       app.ID,
		Source:      SourceProvider,
		Provider:    provider.Name(),
		ProviderRef: app.ExternalID,
		MatchedName: art.Name,
		TileAsset:   tile,
		HeroAsset:   hero,
		Attribution: art.Attribution,
		Locked:      false,
	})
}

// pickExactTitleMatch returns the first candidate whose normalised title
// equals the app's normalised name (provider order is the only tie-break).
// ok=false is a `source='none'` outcome, never a fallback to the nearest
// thing. A name normalising to "" matches nothing, rather than matching every
// candidate that also normalises to "".
func pickExactTitleMatch(appName string, candidates []Candidate) (Candidate, bool) {
	want := normaliseTitle(appName)
	if want == "" {
		return Candidate{}, false
	}
	for _, c := range candidates {
		if normaliseTitle(c.Name) == want {
			return c, true
		}
	}
	return Candidate{}, false
}

// editionSuffixes are trailing re-release labels, stripped from the end only
// ("Sunset Remastered" is "Sunset"; "Remastered Sunset" is not). Longest-first
// — scanned in order, "game of the year" would shadow its "edition" form.
// Must stay short: every entry is another way to declare two different games
// the same (§12.1); a title needing more is a job for the admin picker.
var editionSuffixes = []string{
	"game of the year edition",
	"game of the year",
	"definitive edition",
	"complete edition",
	"enhanced edition",
	"remastered edition",
	"goty edition",
	"remastered",
	"goty",
}

// normaliseTitle folds a title to the form two titles must share to count as
// the same game: lower-cased, non-alphanumerics turned into spaces, whitespace
// collapsed, trailing edition label removed. Punctuation becomes a space, not
// deleted — deleting would fold "Steam (Dev)" to "steamdev" (or, dropping the
// bracket group, to "steam"), quietly widening what can collide.
func normaliseTitle(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToLower(s) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		default:
			b.WriteRune(' ')
		}
	}
	out := strings.Join(strings.Fields(b.String()), " ")
	for _, suffix := range editionSuffixes {
		if trimmed, cut := strings.CutSuffix(out, " "+suffix); cut {
			// Only when something is left: a game actually called "GOTY" keeps
			// its name (the " "+suffix form already guarantees this, and the
			// check documents the intent).
			if trimmed != "" {
				out = trimmed
			}
			break
		}
	}
	return out
}

// ResolveByID is the explicit "match now" action: clear and re-resolve
// immediately — wanted right after a key is first configured, when apps carry
// `source='none'` rows from the unconfigured era. A locked row is refused
// unless force (ErrLocked). Clear-then-resolve means a provider failure leaves
// no row, which is recoverable: "no row" is Store.PendingApps' predicate, so
// the sweeper picks the app up again.
func (s *Service) ResolveByID(ctx context.Context, appID string, force bool) (Record, error) {
	app, err := s.store.App(ctx, appID)
	if err != nil {
		return Record{}, err
	}
	if _, err := s.providerNow(ctx); err != nil {
		return Record{}, err
	}
	if !force {
		if prev, ok, err := s.store.Get(ctx, appID); err != nil {
			return Record{}, err
		} else if ok && prev.Locked {
			return Record{}, ErrLocked
		}
	}
	if err := s.store.Clear(ctx, appID); err != nil {
		return Record{}, err
	}
	if err := s.Resolve(ctx, app); err != nil {
		return Record{}, err
	}
	return s.reload(ctx, appID)
}

// ReresolveResult reports one bulk re-resolve sweep.
type ReresolveResult struct {
	// Total is how many apps were considered.
	Total int
	// Resolved is how many were cleared and re-resolved.
	Resolved int
	// SkippedLocked is how many carried an admin correction and were left alone.
	SkippedLocked int
	// Failed is how many errored (provider miss, fetch failure). Each is logged
	// and skipped — one unmatchable app must never stall the queue behind it.
	Failed int
}

// ReresolveAll is the admin-triggered bulk refetch (#385 changed the tile crop
// and Resolve returns early for any existing row, so old art would persist
// forever). Explicit, never automatic — a boot-time refetch would spend the
// deployment's third-party budget unasked. Locked rows are skipped unless
// force. Orphaned blobs need no work here: PruneOrphans at boot deletes every
// blob no artwork row names.
func (s *Service) ReresolveAll(ctx context.Context, force bool) (ReresolveResult, error) {
	var out ReresolveResult
	// Resolve the provider ONCE up front so an unconfigured deployment gets one
	// clean 409 instead of N identical per-app failures.
	if _, err := s.providerNow(ctx); err != nil {
		return out, err
	}
	apps, err := s.store.AllApps(ctx)
	if err != nil {
		return out, err
	}
	out.Total = len(apps)
	for _, app := range apps {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		if _, err := s.ResolveByID(ctx, app.ID, force); err != nil {
			switch {
			case errors.Is(err, ErrLocked):
				out.SkippedLocked++
			default:
				out.Failed++
				s.log.Info("artwork: could not re-resolve", "app_id", app.ID, "name", app.Name, "err", err)
			}
			continue
		}
		out.Resolved++
	}
	s.log.Info("artwork: bulk re-resolve complete",
		"total", out.Total, "resolved", out.Resolved,
		"skipped_locked", out.SkippedLocked, "failed", out.Failed, "force", force)
	return out, nil
}

// ApplyCandidate is the admin override for a WRONG MATCH: fetch and cache the
// crops for a provider candidate the operator picked from the search results.
// The resulting row is locked, so no later sweep re-breaks the correction.
func (s *Service) ApplyCandidate(ctx context.Context, appID, ref string) (Record, error) {
	if _, err := s.store.App(ctx, appID); err != nil {
		return Record{}, err
	}
	provider, err := s.providerNow(ctx)
	if err != nil {
		return Record{}, err
	}
	art, err := provider.Art(ctx, ref)
	if err != nil {
		return Record{}, err
	}
	if art.TileURL == "" && art.HeroURL == "" {
		return Record{}, fmt.Errorf("%w: that match has no usable artwork", ErrInvalidRequest)
	}
	tile, hero, err := s.cacheCrops(ctx, art.TileURL, art.HeroURL)
	if err != nil {
		return Record{}, err
	}
	rec := Record{
		AppID:       appID,
		Source:      SourceManual,
		Provider:    provider.Name(),
		ProviderRef: ref,
		MatchedName: art.Name,
		TileAsset:   tile,
		HeroAsset:   hero,
		Attribution: art.Attribution,
		Locked:      true,
	}
	if err := s.store.Save(ctx, rec); err != nil {
		return Record{}, err
	}
	return s.reload(ctx, appID)
}

// ApplyURLs is the admin override for art the provider does not have. The
// URLs are operator-supplied and therefore attacker-adjacent: they go through
// the same SSRF-guarded, size- and type-capped fetcher as a provider URL, with
// no "it came from an admin" exception. An empty crop URL leaves that crop
// unchanged, so an operator can replace just the hero.
func (s *Service) ApplyURLs(ctx context.Context, appID, tileURL, heroURL string) (Record, error) {
	if _, err := s.store.App(ctx, appID); err != nil {
		return Record{}, err
	}
	if strings.TrimSpace(tileURL) == "" && strings.TrimSpace(heroURL) == "" {
		return Record{}, fmt.Errorf("%w: supply a tile url, a hero url, or both", ErrInvalidRequest)
	}
	prev, _, err := s.store.Get(ctx, appID)
	if err != nil {
		return Record{}, err
	}
	tile, hero, err := s.cacheCrops(ctx, tileURL, heroURL)
	if err != nil {
		return Record{}, err
	}
	rec := Record{
		AppID:       appID,
		Source:      SourceManual,
		TileAsset:   firstNonEmpty(tile, prev.TileAsset),
		HeroAsset:   firstNonEmpty(hero, prev.HeroAsset),
		Attribution: prev.Attribution,
		Locked:      true,
	}
	if err := s.store.Save(ctx, rec); err != nil {
		return Record{}, err
	}
	return s.reload(ctx, appID)
}

// Upload is the admin override with no network at all: raw image bytes for
// one crop — the fallback that always works. declaredType is verified against
// the sniffed bytes in BlobStore.Put, so an "image/png" that is really HTML is
// rejected before it is stored, let alone served back.
func (s *Service) Upload(ctx context.Context, appID, crop, declaredType string, data []byte) (Record, error) {
	if _, err := s.store.App(ctx, appID); err != nil {
		return Record{}, err
	}
	if crop != CropTile && crop != CropHero {
		return Record{}, fmt.Errorf("%w: crop must be %q or %q", ErrInvalidRequest, CropTile, CropHero)
	}
	if int64(len(data)) > s.fetcher.maxBytes {
		return Record{}, fmt.Errorf("%w: image exceeds the %d byte limit", ErrInvalidRequest, s.fetcher.maxBytes)
	}
	name, err := s.blobs.Put(declaredType, data)
	if err != nil {
		return Record{}, err
	}
	prev, _, err := s.store.Get(ctx, appID)
	if err != nil {
		return Record{}, err
	}
	rec := Record{
		AppID:       appID,
		Source:      SourceManual,
		TileAsset:   prev.TileAsset,
		HeroAsset:   prev.HeroAsset,
		Attribution: prev.Attribution,
		Locked:      true,
	}
	if crop == CropTile {
		rec.TileAsset = name
	} else {
		rec.HeroAsset = name
	}
	if err := s.store.Save(ctx, rec); err != nil {
		return Record{}, err
	}
	return s.reload(ctx, appID)
}

// Clear removes an app's artwork, returning it to the gradient tile.
func (s *Service) Clear(ctx context.Context, appID string) error {
	if _, err := s.store.App(ctx, appID); err != nil {
		return err
	}
	return s.store.Clear(ctx, appID)
}

// cacheCrops fetches whichever crop URLs are non-empty and stores them locally.
// A crop that fails is a hard error rather than a silent skip: an operator who
// asked for art and got half of it with no message would have no way to tell a
// missing hero from a rejected one.
func (s *Service) cacheCrops(ctx context.Context, tileURL, heroURL string) (tile, hero string, err error) {
	if strings.TrimSpace(tileURL) != "" {
		data, ct, err := s.fetcher.Get(ctx, tileURL)
		if err != nil {
			return "", "", fmt.Errorf("tile: %w", err)
		}
		if tile, err = s.blobs.Put(ct, data); err != nil {
			return "", "", fmt.Errorf("tile: %w", err)
		}
	}
	if strings.TrimSpace(heroURL) != "" {
		data, ct, err := s.fetcher.Get(ctx, heroURL)
		if err != nil {
			return "", "", fmt.Errorf("hero: %w", err)
		}
		if hero, err = s.blobs.Put(ct, data); err != nil {
			return "", "", fmt.Errorf("hero: %w", err)
		}
	}
	return tile, hero, nil
}

func (s *Service) reload(ctx context.Context, appID string) (Record, error) {
	rec, ok, err := s.store.Get(ctx, appID)
	if err != nil {
		return Record{}, err
	}
	if !ok {
		return Record{}, fmt.Errorf("artwork: record vanished after write")
	}
	return rec, nil
}

// SweepResult is one sweep pass's counts, recorded by the artwork.sweep job.
// ProviderConfigured=false is the ship-dark case — nothing else in the result
// means anything — and is what makes "configured but finding nothing" and
// "not configured at all" distinguishable.
type SweepResult struct {
	ProviderConfigured bool
	AppsConsidered     int
	ArtworkResolved    int
	NoMatch            int
}

// SweepOnce resolves up to sweepBatch apps that have no artwork row. Exported
// so a test or the artwork.sweep job's RunFunc can drive one deterministic
// pass. Per-app failures are logged and skipped — one unmatchable app must
// never stall the queue behind it. It runs even with no provider configured
// (the key can be set from the admin UI at any moment); ship-dark is preserved
// because the provider is resolved first and the pass returns immediately
// without one.
func (s *Service) SweepOnce(ctx context.Context) SweepResult {
	var res SweepResult
	// Deliberately not short-circuited on secrets.Store.Available(): that would
	// throw away the "a key is stored but this control plane holds no master
	// key" diagnostic, worth far more than one indexed lookup per interval.
	if _, err := s.providerNow(ctx); err != nil {
		return res
	}
	res.ProviderConfigured = true
	apps, err := s.store.PendingApps(ctx, s.sweepBatch)
	if err != nil {
		s.log.Warn("artwork: could not list apps needing artwork", "err", err)
		return res
	}
	for _, app := range apps {
		if ctx.Err() != nil {
			return res
		}
		res.AppsConsidered++
		if err := s.Resolve(ctx, app); err != nil {
			s.log.Info("artwork: could not resolve", "app_id", app.ID, "name", app.Name, "err", err)
			continue
		}
		if rec, ok, gerr := s.store.Get(ctx, app.ID); gerr == nil && ok && rec.Source != SourceNone {
			res.ArtworkResolved++
		} else {
			res.NoMatch++
		}
	}
	return res
}

// PruneOrphans deletes cached blobs no artwork row references.
func (s *Service) PruneOrphans(ctx context.Context) (int, error) {
	keep, err := s.store.ReferencedAssets(ctx)
	if err != nil {
		return 0, err
	}
	return s.blobs.PruneOrphans(keep)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
