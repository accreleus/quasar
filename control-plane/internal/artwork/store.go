package artwork

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrAppNotFound is returned when an artwork operation names an app that does
// not exist.
var ErrAppNotFound = errors.New("artwork: app not found")

// Record is the stored artwork provenance for one app, plus the render URLs
// currently on the app row.
type Record struct {
	AppID       string    `json:"app_id"`
	Source      string    `json:"source"`
	Provider    string    `json:"provider"`
	ProviderRef string    `json:"provider_ref"`
	MatchedName string    `json:"matched_name"`
	TileAsset   string    `json:"tile_asset"`
	HeroAsset   string    `json:"hero_asset"`
	Attribution string    `json:"attribution"`
	Locked      bool      `json:"locked"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Source values. 'none' is a negative cache, not an error (see migration 0039).
const (
	SourceProvider = "provider"
	SourceManual   = "manual"
	SourceNone     = "none"
)

// appRef is the minimum an artwork resolution needs about an app.
//
// ExternalSource/ExternalID (migration 0042) are what let a resolution skip the
// fuzzy title matcher entirely: an exact provider id beats any title match by
// construction (spec §12). They are read here rather than looked up later
// because Resolve's very first decision depends on them.
type appRef struct {
	ID             string
	Name           string
	Kind           string
	ExternalSource string
	ExternalID     string
}

// Store is the artwork data-access layer.
type Store struct{ pool *pgxpool.Pool }

// NewStore builds the store.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// App returns the id/name/kind/external-ref of an app, or ErrAppNotFound.
func (s *Store) App(ctx context.Context, appID string) (appRef, error) {
	var a appRef
	err := s.pool.QueryRow(ctx,
		`SELECT id::text, name, kind, external_source, external_id
		 FROM apps WHERE id::text = $1`, appID).
		Scan(&a.ID, &a.Name, &a.Kind, &a.ExternalSource, &a.ExternalID)
	if errors.Is(err, pgx.ErrNoRows) {
		return appRef{}, ErrAppNotFound
	}
	if err != nil {
		return appRef{}, fmt.Errorf("artwork: load app: %w", err)
	}
	return a, nil
}

// Get returns the artwork record for an app. A missing row is (Record{}, false,
// nil) — "no artwork yet" is an ordinary state, not an error.
func (s *Store) Get(ctx context.Context, appID string) (Record, bool, error) {
	var r Record
	err := s.pool.QueryRow(ctx, `
		SELECT app_id::text, source, provider, provider_ref, matched_name,
		       tile_asset, hero_asset, attribution, locked, updated_at
		FROM app_artwork WHERE app_id::text = $1
	`, appID).Scan(&r.AppID, &r.Source, &r.Provider, &r.ProviderRef, &r.MatchedName,
		&r.TileAsset, &r.HeroAsset, &r.Attribution, &r.Locked, &r.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, fmt.Errorf("artwork: get: %w", err)
	}
	return r, true, nil
}

// Save upserts the provenance row AND the app's render URLs in ONE transaction.
//
// The two must not drift: apps.cover_url pointing at a blob no app_artwork row
// references would survive an orphan prune and then 404 forever, and a
// provenance row whose URLs never reached the app row would show as "matched"
// in the admin UI while the library still rendered a gradient. A transaction is
// what makes "the library shows what the provenance says" true by construction
// rather than by convention.
func (s *Store) Save(ctx context.Context, r Record) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("artwork: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
		INSERT INTO app_artwork (app_id, source, provider, provider_ref, matched_name,
		                         tile_asset, hero_asset, attribution, locked, updated_at)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, now())
		ON CONFLICT (app_id) DO UPDATE SET
			source = EXCLUDED.source,
			provider = EXCLUDED.provider,
			provider_ref = EXCLUDED.provider_ref,
			matched_name = EXCLUDED.matched_name,
			tile_asset = EXCLUDED.tile_asset,
			hero_asset = EXCLUDED.hero_asset,
			attribution = EXCLUDED.attribution,
			locked = EXCLUDED.locked,
			updated_at = now()
	`, r.AppID, r.Source, r.Provider, r.ProviderRef, r.MatchedName,
		r.TileAsset, r.HeroAsset, r.Attribution, r.Locked); err != nil {
		return fmt.Errorf("artwork: upsert: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		UPDATE apps SET cover_url = $2, hero_url = $3, updated_at = now()
		WHERE id::text = $1
	`, r.AppID, assetURLOrNil(r.TileAsset), assetURLOrNil(r.HeroAsset)); err != nil {
		return fmt.Errorf("artwork: update app urls: %w", err)
	}
	return tx.Commit(ctx)
}

// Clear removes the provenance row and NULLs the app's render URLs, returning
// the app to the gradient tile. Deliberately does not delete the blobs — they
// are content-addressed and possibly shared; PruneOrphans reclaims them.
func (s *Store) Clear(ctx context.Context, appID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("artwork: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `DELETE FROM app_artwork WHERE app_id::text = $1`, appID); err != nil {
		return fmt.Errorf("artwork: delete: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE apps SET cover_url = NULL, hero_url = NULL, updated_at = now()
		WHERE id::text = $1
	`, appID); err != nil {
		return fmt.Errorf("artwork: clear app urls: %w", err)
	}
	return tx.Commit(ctx)
}

// PendingApps lists apps with NO artwork row at all — the fetcher's work queue.
//
// "No row" rather than "no art" is the load-bearing predicate: an app that
// resolved to `source='none'` HAS a row, so it is never re-queried, and an
// admin override HAS a row, so the sweep can never overwrite it. That is what
// makes the cache survive a redeploy instead of re-fetching at browse time.
func (s *Store) PendingApps(ctx context.Context, limit int) ([]appRef, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT a.id::text, a.name, a.kind, a.external_source, a.external_id
		FROM apps a
		LEFT JOIN app_artwork w ON w.app_id = a.id
		WHERE w.app_id IS NULL
		ORDER BY a.created_at DESC
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("artwork: pending: %w", err)
	}
	defer rows.Close()
	var out []appRef
	for rows.Next() {
		var a appRef
		if err := rows.Scan(&a.ID, &a.Name, &a.Kind, &a.ExternalSource, &a.ExternalID); err != nil {
			return nil, fmt.Errorf("artwork: scan pending: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AllApps lists every app, newest first — the bulk re-resolve's work queue.
//
// Deliberately NOT PendingApps: that one's whole job is to exclude apps that
// already have a row, which for a re-resolve is exactly the set that needs
// doing. Unbounded on purpose too — a re-resolve is an explicit operator action
// over the whole catalogue, and silently doing 25 of them would be a worse
// surprise than taking a few seconds (the provider throttle is 500 ms/call).
func (s *Store) AllApps(ctx context.Context) ([]appRef, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, name, kind, external_source, external_id
		FROM apps ORDER BY created_at DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("artwork: all apps: %w", err)
	}
	defer rows.Close()
	var out []appRef
	for rows.Next() {
		var a appRef
		if err := rows.Scan(&a.ID, &a.Name, &a.Kind, &a.ExternalSource, &a.ExternalID); err != nil {
			return nil, fmt.Errorf("artwork: scan app: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ReferencedAssets returns every blob name still named by an artwork row —
// the keep-set for PruneOrphans.
func (s *Store) ReferencedAssets(ctx context.Context) (map[string]struct{}, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT tile_asset FROM app_artwork WHERE tile_asset <> ''
		UNION
		SELECT hero_asset FROM app_artwork WHERE hero_asset <> ''
	`)
	if err != nil {
		return nil, fmt.Errorf("artwork: referenced: %w", err)
	}
	defer rows.Close()
	out := make(map[string]struct{})
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("artwork: scan referenced: %w", err)
		}
		out[name] = struct{}{}
	}
	return out, rows.Err()
}

// assetURLOrNil maps a blob name to the served URL, or NULL for "no crop".
// NULL (not "") is what the library's `cover_url ? <img> : gradient` check
// needs to fall back cleanly.
func assetURLOrNil(asset string) any {
	if asset == "" {
		return nil
	}
	return AssetURL(asset)
}

// AssetURL is the public path a cached blob is served at. The blob name IS a
// SHA-256, so the URL is content-addressed: it changes whenever the bytes
// change, which is what lets the handler mark it immutable.
func AssetURL(asset string) string { return "/v1/artwork/" + asset }
