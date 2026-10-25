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

// ErrRowChanged: SweepWrite found the record changed since the sweep read it.
var ErrRowChanged = errors.New("artwork: the record changed since the sweep read it")

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
	// LogoLost/IconLost are not stored: the named file is not known to be on disk,
	// so Save and SweepWrite do not serve it. A locked record keeps the name until a
	// repair fetches it again.
	LogoLost bool `json:"-"`
	IconLost bool `json:"-"`
	// LogoURL/IconURL are the app's served logo and icon URLs as read.
	LogoURL *string `json:"-"`
	IconURL *string `json:"-"`
}

// recordCols / recordFrom are what every Record read scans, in dests order.
const (
	recordCols = `w.app_id::text, w.source, w.provider, w.provider_ref, w.matched_name,
		       w.tile_asset, w.hero_asset, w.attribution, w.locked, w.updated_at,
		       w.logo_asset, w.icon_asset, w.logo_icon_checked, a.logo_url, a.icon_url`
	recordFrom = `app_artwork w JOIN apps a ON a.id = w.app_id`
)

func (r *Record) dests() []any {
	return []any{&r.AppID, &r.Source, &r.Provider, &r.ProviderRef, &r.MatchedName,
		&r.TileAsset, &r.HeroAsset, &r.Attribution, &r.Locked, &r.UpdatedAt,
		&r.LogoAsset, &r.IconAsset, &r.LogoIconChecked, &r.LogoURL, &r.IconURL}
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
	err := s.pool.QueryRow(ctx, `SELECT `+recordCols+` FROM `+recordFrom+` WHERE w.app_id::text = $1`, appID).
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

// SaveIfAbsent is Save for the FIRST-TIME path only (#739): the provenance row
// AND the app's render URLs are written in ONE transaction, but the insert is
// ON CONFLICT (app_id) DO NOTHING and the app's URLs are touched only when the
// row was actually inserted. Resolve's "no row yet" is a snapshot, not a
// guarantee: an admin pick or upload that lands while a resolve is fetching from the provider
// must win, so a resolve that loses the race discards its record and keeps
// its hands off the app row. Returns whether it inserted.
//
// Save (the upsert) stays for the admin paths, which mean to overwrite;
// SweepWrite keeps its own guarded update.
func (s *Store) SaveIfAbsent(ctx context.Context, r Record) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("artwork: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
		INSERT INTO app_artwork (app_id, source, provider, provider_ref, matched_name,
		                         tile_asset, hero_asset, attribution, locked, updated_at,
		                         logo_asset, icon_asset, logo_icon_checked)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, now(), $10, $11, $12)
		ON CONFLICT (app_id) DO NOTHING
	`, r.AppID, r.Source, r.Provider, r.ProviderRef, r.MatchedName,
		r.TileAsset, r.HeroAsset, r.Attribution, r.Locked,
		r.LogoAsset, r.IconAsset, r.LogoIconChecked)
	if err != nil {
		return false, fmt.Errorf("artwork: insert: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// A concurrent writer won. Their record stands; touching the app's URLs
		// here would overwrite it out from under that row.
		return false, tx.Commit(ctx)
	}

	if _, err := tx.Exec(ctx, `
		UPDATE apps SET cover_url = $2, hero_url = $3, logo_url = $4, icon_url = $5, updated_at = now()
		WHERE id::text = $1
	`, r.AppID, assetURLOrNil(r.TileAsset), assetURLOrNil(r.HeroAsset),
		servedURL(r.LogoAsset, r.LogoLost), servedURL(r.IconAsset, r.IconLost)); err != nil {
		return false, fmt.Errorf("artwork: update app urls: %w", err)
	}
	return true, tx.Commit(ctx)
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
	return s.records(ctx, `SELECT `+recordCols+` FROM `+recordFrom+`
		WHERE w.tile_asset <> '' OR w.hero_asset <> '' OR w.logo_asset <> '' OR w.icon_asset <> ''
		ORDER BY w.updated_at`)
}

// LogoIconPending lists unlocked provider records whose logo and icon were never
// fetched (amendment 38) — the sweep's backfill queue. Oldest updated_at first;
// the backfill's write bumps it, so a record that keeps failing moves to the back.
func (s *Store) LogoIconPending(ctx context.Context, limit int) ([]Record, error) {
	return s.records(ctx, `SELECT `+recordCols+` FROM `+recordFrom+`
		WHERE w.source = '`+SourceProvider+`' AND NOT w.locked AND NOT w.logo_icon_checked
		ORDER BY w.updated_at
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

// SweepWrite is the only write the sweep makes to a record it read: rec's crop
// names and logo_icon_checked, and the app's served URLs, applied only while
// app_artwork.updated_at still equals rec.UpdatedAt as read. Every admin write
// moves updated_at (Save) or deletes the row (Clear), so a sweep holding an older
// snapshot gets ErrRowChanged and writes nothing. Returns rec as now stored, with
// the new UpdatedAt, so a later write in the same pass chains from it. An apps row
// is touched only when a URL changes.
func (s *Store) SweepWrite(ctx context.Context, rec Record) (Record, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return rec, fmt.Errorf("artwork: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var next time.Time
	err = tx.QueryRow(ctx, `
		UPDATE app_artwork SET tile_asset = $3, hero_asset = $4, logo_asset = $5, icon_asset = $6,
		       logo_icon_checked = $7, updated_at = now()
		WHERE app_id::text = $1 AND updated_at = $2
		RETURNING updated_at`, rec.AppID, rec.UpdatedAt,
		rec.TileAsset, rec.HeroAsset, rec.LogoAsset, rec.IconAsset, rec.LogoIconChecked).Scan(&next)
	if errors.Is(err, pgx.ErrNoRows) {
		return rec, ErrRowChanged
	}
	if err != nil {
		return rec, fmt.Errorf("artwork: sweep write: %w", err)
	}
	logo, icon := servedURL(rec.LogoAsset, rec.LogoLost), servedURL(rec.IconAsset, rec.IconLost)
	if _, err := tx.Exec(ctx, `
		UPDATE apps SET cover_url = $2, hero_url = $3, logo_url = $4, icon_url = $5
		WHERE id::text = $1 AND (cover_url IS DISTINCT FROM $2 OR hero_url IS DISTINCT FROM $3
		      OR logo_url IS DISTINCT FROM $4 OR icon_url IS DISTINCT FROM $5)`,
		rec.AppID, assetURLOrNil(rec.TileAsset), assetURLOrNil(rec.HeroAsset), logo, icon); err != nil {
		return rec, fmt.Errorf("artwork: sweep write app urls: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return rec, fmt.Errorf("artwork: sweep write commit: %w", err)
	}
	rec.UpdatedAt, rec.LogoURL, rec.IconURL = next, servedPtr(rec.LogoAsset, rec.LogoLost), servedPtr(rec.IconAsset, rec.IconLost)
	return rec, nil
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

// servedURL is assetURLOrNil, except NULL for a name not known to be on disk.
func servedURL(asset string, lost bool) any {
	if p := servedPtr(asset, lost); p != nil {
		return *p
	}
	return nil
}

func servedPtr(asset string, lost bool) *string {
	if lost || asset == "" {
		return nil
	}
	u := AssetURL(asset)
	return &u
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
