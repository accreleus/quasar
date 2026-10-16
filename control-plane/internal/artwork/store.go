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
	// Amendment 38. Not on the admin artwork read. LogoIconChecked: the provider
	// answered for this record's logo and icon (migration 0104).
	LogoAsset       string `json:"-"`
	IconAsset       string `json:"-"`
	LogoIconChecked bool   `json:"-"`
	// LogoLost/IconLost are not stored: the named file is gone, so Save does not
	// serve it. A locked record keeps the name until a repair fetches it again.
	LogoLost bool `json:"-"`
	IconLost bool `json:"-"`
}

// recordCols is the column list every Record read scans, in recordDests order.
const recordCols = `app_id::text, source, provider, provider_ref, matched_name,
		       tile_asset, hero_asset, attribution, locked, updated_at,
		       logo_asset, icon_asset, logo_icon_checked`

func (r *Record) dests() []any {
	return []any{&r.AppID, &r.Source, &r.Provider, &r.ProviderRef, &r.MatchedName,
		&r.TileAsset, &r.HeroAsset, &r.Attribution, &r.Locked, &r.UpdatedAt,
		&r.LogoAsset, &r.IconAsset, &r.LogoIconChecked}
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
	err := s.pool.QueryRow(ctx, `SELECT `+recordCols+` FROM app_artwork WHERE app_id::text = $1`, appID).
		Scan(r.dests()...)
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
		                         tile_asset, hero_asset, attribution, locked, updated_at,
		                         logo_asset, icon_asset, logo_icon_checked)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, now(), $10, $11, $12)
		ON CONFLICT (app_id) DO UPDATE SET
			source = EXCLUDED.source,
			provider = EXCLUDED.provider,
			provider_ref = EXCLUDED.provider_ref,
			matched_name = EXCLUDED.matched_name,
			tile_asset = EXCLUDED.tile_asset,
			hero_asset = EXCLUDED.hero_asset,
			attribution = EXCLUDED.attribution,
			locked = EXCLUDED.locked,
			updated_at = now(),
			logo_asset = EXCLUDED.logo_asset,
			icon_asset = EXCLUDED.icon_asset,
			logo_icon_checked = EXCLUDED.logo_icon_checked
	`, r.AppID, r.Source, r.Provider, r.ProviderRef, r.MatchedName,
		r.TileAsset, r.HeroAsset, r.Attribution, r.Locked,
		r.LogoAsset, r.IconAsset, r.LogoIconChecked); err != nil {
		return fmt.Errorf("artwork: upsert: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		UPDATE apps SET cover_url = $2, hero_url = $3, logo_url = $4, icon_url = $5, updated_at = now()
		WHERE id::text = $1
	`, r.AppID, assetURLOrNil(r.TileAsset), assetURLOrNil(r.HeroAsset),
		servedURL(r.LogoAsset, r.LogoLost), servedURL(r.IconAsset, r.IconLost)); err != nil {
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
		UPDATE apps SET cover_url = NULL, hero_url = NULL, logo_url = NULL, icon_url = NULL, updated_at = now()
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

// RecordsWithAssets lists every artwork row that names a cached image — the
// sweep's check that each named file still exists (#441).
func (s *Store) RecordsWithAssets(ctx context.Context) ([]Record, error) {
	return s.records(ctx, `SELECT `+recordCols+`
		FROM app_artwork WHERE tile_asset <> '' OR hero_asset <> '' OR logo_asset <> '' OR icon_asset <> ''
		ORDER BY updated_at`)
}

// LogoIconPending lists unlocked provider records whose logo and icon were never
// fetched (amendment 38) — the sweep's backfill queue. Oldest updated_at first;
// SaveLogoIcon bumps it, so a record that keeps failing moves to the back.
func (s *Store) LogoIconPending(ctx context.Context, limit int) ([]Record, error) {
	return s.records(ctx, `SELECT `+recordCols+`
		FROM app_artwork
		WHERE source = '`+SourceProvider+`' AND NOT locked AND NOT logo_icon_checked
		ORDER BY updated_at
		LIMIT $1`, limit)
}

func (s *Store) records(ctx context.Context, query string, args ...any) ([]Record, error) {
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("artwork: list records: %w", err)
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		var r Record
		if err := rows.Scan(r.dests()...); err != nil {
			return nil, fmt.Errorf("artwork: scan record: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SaveLogoIcon writes a backfilled logo and icon onto rec's row and the app's
// URLs, only while the row is still the unlocked provider match that was read: an
// admin override that landed meanwhile wins. It always bumps updated_at.
func (s *Store) SaveLogoIcon(ctx context.Context, rec Record) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("artwork: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
		UPDATE app_artwork SET logo_asset = $3, icon_asset = $4, logo_icon_checked = $5, updated_at = now()
		WHERE app_id::text = $1 AND provider_ref = $2 AND source = '`+SourceProvider+`' AND NOT locked`,
		rec.AppID, rec.ProviderRef, rec.LogoAsset, rec.IconAsset, rec.LogoIconChecked)
	if err != nil {
		return fmt.Errorf("artwork: save logo and icon: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE apps SET logo_url = $2, icon_url = $3, updated_at = now() WHERE id::text = $1`,
		rec.AppID, assetURLOrNil(rec.LogoAsset), assetURLOrNil(rec.IconAsset)); err != nil {
		return fmt.Errorf("artwork: update app logo and icon urls: %w", err)
	}
	return tx.Commit(ctx)
}

// DropLogoIcon stops serving lostLogo / lostIcon ("" = that crop is fine), the
// names of files that are gone, and with unname also clears the names and
// re-queues the record for the backfill. A locked record keeps the names, so
// every sweep's repair retries them (#441). Each crop is touched only while the
// row still names that file, so an admin re-pick in between is never wiped.
// updated_at is left alone: a re-queued record keeps its place in
// LogoIconPending's oldest-first order.
func (s *Store) DropLogoIcon(ctx context.Context, appID, lostLogo, lostIcon string, unname bool) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("artwork: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if unname {
		if _, err := tx.Exec(ctx, `
			UPDATE app_artwork SET
			       logo_asset = CASE WHEN $2 <> '' AND logo_asset = $2 THEN '' ELSE logo_asset END,
			       icon_asset = CASE WHEN $3 <> '' AND icon_asset = $3 THEN '' ELSE icon_asset END,
			       logo_icon_checked = CASE WHEN ($2 <> '' AND logo_asset = $2) OR ($3 <> '' AND icon_asset = $3)
			                                THEN false ELSE logo_icon_checked END
			WHERE app_id::text = $1`, appID, lostLogo, lostIcon); err != nil {
			return fmt.Errorf("artwork: drop logo or icon: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE apps SET logo_url = CASE WHEN $2 <> '' AND logo_url = $4 THEN NULL ELSE logo_url END,
		       icon_url = CASE WHEN $3 <> '' AND icon_url = $5 THEN NULL ELSE icon_url END
		WHERE id::text = $1`, appID, lostLogo, lostIcon, AssetURL(lostLogo), AssetURL(lostIcon)); err != nil {
		return fmt.Errorf("artwork: stop serving logo or icon: %w", err)
	}
	return tx.Commit(ctx)
}

// Crop columns RepairCrop may write; a fixed set, never caller text.
var logoIconCols = map[string][2]string{
	"logo": {"logo_asset", "logo_url"},
	"icon": {"icon_asset", "icon_url"},
}

// RepairCrop names a re-fetched logo or icon ("" = the provider has none now) and
// serves it, locked record or not, only while the row still names stale ("" for a
// dropped crop) under ref: an admin change in between wins. checked also marks the
// record's logo and icon answered.
func (s *Store) RepairCrop(ctx context.Context, appID, ref, crop, stale, fresh string, checked bool) error {
	cols, ok := logoIconCols[crop]
	if !ok {
		return fmt.Errorf("artwork: no crop %q", crop)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("artwork: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `
		UPDATE app_artwork SET `+cols[0]+` = $4, logo_icon_checked = logo_icon_checked OR $5, updated_at = now()
		WHERE app_id::text = $1 AND provider_ref = $2 AND `+cols[0]+` = $3`, appID, ref, stale, fresh, checked)
	if err != nil {
		return fmt.Errorf("artwork: repair %s: %w", crop, err)
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE apps SET `+cols[1]+` = $2 WHERE id::text = $1`, appID, assetURLOrNil(fresh)); err != nil {
		return fmt.Errorf("artwork: serve repaired %s: %w", crop, err)
	}
	return tx.Commit(ctx)
}

// ReferencedAssets returns every blob name still named by an artwork row —
// the keep-set for PruneOrphans.
func (s *Store) ReferencedAssets(ctx context.Context) (map[string]struct{}, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT tile_asset FROM app_artwork WHERE tile_asset <> ''
		UNION
		SELECT hero_asset FROM app_artwork WHERE hero_asset <> ''
		UNION
		SELECT logo_asset FROM app_artwork WHERE logo_asset <> ''
		UNION
		SELECT icon_asset FROM app_artwork WHERE icon_asset <> ''
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

// servedURL is assetURLOrNil, except NULL for a name whose file is gone.
func servedURL(asset string, lost bool) any {
	if lost {
		return nil
	}
	return assetURLOrNil(asset)
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
