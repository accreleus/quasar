package compat

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/audit"
)

var (
	ErrNotFound = errors.New("compat tool not found")
	// ErrNotUsable: the tool is disabled or marked (control-api.md §Usable).
	ErrNotUsable = errors.New("compat tool is not usable")
	// ErrLimit, ErrPinConflict: an enable refused (PATCH /v1/admin/compat-tools/{id}).
	ErrLimit       = errors.New("compat tool limit reached")
	ErrPinConflict = errors.New("compat tool pin conflict")
	// ErrMappingsFull: the table holds MaxMappings and the appid is not one of them.
	ErrMappingsFull = errors.New("compat tool mappings are full")
	// ErrExists: a tool with this tag (or this pin) exists (POST /v1/admin/compat-tools).
	ErrExists = errors.New("compat tool exists")
)

// MaxMappings is the contract's cap on per-game mappings (control-api.md §Compatibility tools).
const MaxMappings = 1000

// mappingCapLock serializes the writers that can add a row, so the cap holds under concurrency.
const mappingCapLock = 0x71_636d // "qcm"

// InUseError refuses a disable while a mapping or the default names the tool.
type InUseError struct {
	Mappings  int
	IsDefault bool
}

func (e *InUseError) Error() string {
	return fmt.Sprintf("%d per-game mapping(s) name the tool; fleet default: %v", e.Mappings, e.IsDefault)
}

type Store struct {
	pool *pgxpool.Pool
	// Auditor records the reconcile's pin_conflict marks (actor null); nil records nothing.
	Auditor audit.Recorder
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Tool is the CompatTool read shape.
type Tool struct {
	ID           string     `json:"id"`
	Kind         string     `json:"kind"`
	Family       string     `json:"family"`
	Source       string     `json:"source"`
	Tag          *string    `json:"tag"`
	SteamName    *string    `json:"steam_name"`
	DisplayName  string     `json:"display_name"`
	Recommended  bool       `json:"recommended"`
	Enabled      bool       `json:"enabled"`
	Asset        *Asset     `json:"asset"`
	Hosts        *HostCount `json:"hosts"`
	Mappings     int        `json:"mappings"`
	IsDefault    bool       `json:"is_default"`
	NameConflict bool       `json:"name_conflict"`
	PinConflict  bool       `json:"pin_conflict"`
	CreatedAt    time.Time  `json:"created_at"`
}

type Asset struct {
	Name      string `json:"name"`
	URL       string `json:"url"`
	SizeBytes int64  `json:"size_bytes"`
	SHA512    string `json:"sha512"`
}

type HostCount struct {
	Installed   int `json:"installed"`
	Installing  int `json:"installing"`
	Waiting     int `json:"waiting"`
	Failed      int `json:"failed"`
	Unsupported int `json:"unsupported"`
}

// Reconcile inserts the code-owned rows and sets recommended from the shipped list. It never
// deletes a row or changes enabled. A tag row pinned to another SHA-512 than the shipped entry is
// never re-pinned: it is marked pin_conflict, and the mark clears when the shipped pin matches again.
func (s *Store) Reconcile(ctx context.Context) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("compat reconcile: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	for _, v := range valveTools {
		if _, err := tx.Exec(ctx, `
			INSERT INTO compat_tools (family, source, steam_name, display_name, enabled, first_enabled_at)
			VALUES ('valve', 'steam', $1, $2, true, now())
			ON CONFLICT (steam_name) DO NOTHING`, v.SteamName, v.Display); err != nil {
			return fmt.Errorf("compat reconcile valve %s: %w", v.SteamName, err)
		}
	}
	tags := make([]string, 0, len(recommended))
	type held struct{ id, tag, sha, name, shipped string }
	var helds []held
	for _, r := range recommended {
		tags = append(tags, r.Tag)
		// No arbiter: a row holding this entry's tag, Steam name or pin skips it rather than
		// aborting the boot. Only a row holding the name or the pin under another tag is a fault.
		var inserted bool
		err := tx.QueryRow(ctx, `
			WITH ins AS (
				INSERT INTO compat_tools (family, source, tag, steam_name, display_name, recommended,
				                          asset_name, asset_size_bytes, asset_sha512)
				VALUES ('ge', 'recommended', $1, $2, $2, true, $3, $4, $5)
				ON CONFLICT DO NOTHING RETURNING id)
			SELECT EXISTS (SELECT 1 FROM ins) OR EXISTS (SELECT 1 FROM compat_tools WHERE tag = $1)`,
			r.Tag, r.SteamName, r.Asset, r.SizeBytes, r.SHA512).Scan(&inserted)
		if err != nil {
			return fmt.Errorf("compat reconcile %s: %w", r.Tag, err)
		}
		if inserted {
			continue
		}
		h := held{name: r.SteamName, shipped: r.Tag}
		err = tx.QueryRow(ctx, `UPDATE compat_tools SET name_conflict = true
			WHERE steam_name = $1 AND NOT name_conflict RETURNING id::text, COALESCE(tag, ''), COALESCE(asset_sha512, '')`,
			r.SteamName).Scan(&h.id, &h.tag, &h.sha)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			slog.Warn("compat tools: a recommended entry is held by another row and was not inserted",
				"tag", r.Tag, "steam_name", r.SteamName)
		case err != nil:
			return fmt.Errorf("compat reconcile %s holder: %w", r.Tag, err)
		default:
			slog.Warn("compat tools: another row holds a recommended entry's Steam name; marked name_conflict",
				"tag", r.Tag, "steam_name", r.SteamName, "holder_tag", h.tag)
			helds = append(helds, h)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE compat_tools SET recommended = (tag = ANY($1::text[]))
		WHERE family = 'ge' AND recommended IS DISTINCT FROM (tag = ANY($1::text[]))`, tags); err != nil {
		return fmt.Errorf("compat reconcile recommended: %w", err)
	}
	type marked struct{ id, tag, sha, shipped string }
	var marks []marked
	for _, r := range recommended {
		var m marked
		err := tx.QueryRow(ctx, `UPDATE compat_tools SET pin_conflict = (asset_sha512 <> $2)
			WHERE tag = $1 AND pin_conflict IS DISTINCT FROM (asset_sha512 <> $2)
			RETURNING id::text, asset_sha512, pin_conflict`, r.Tag, r.SHA512).Scan(&m.id, &m.sha, new(bool))
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return fmt.Errorf("compat reconcile pin %s: %w", r.Tag, err)
		case m.sha != r.SHA512:
			m.tag, m.shipped = r.Tag, r.SHA512
			marks = append(marks, m)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	for _, m := range marks {
		audit.TryRecord(ctx, s.Auditor, "", "compat_tool.pin_conflict", "compat_tool", m.id,
			map[string]any{"tag": m.tag, "sha512": m.sha, "shipped_sha512": m.shipped})
	}
	for _, h := range helds {
		audit.TryRecord(ctx, s.Auditor, "", "compat_tool.name_conflict", "compat_tool", h.id,
			map[string]any{"tag": h.tag, "sha512": h.sha, "name": h.name, "shipped_tag": h.shipped})
	}
	return nil
}

// HasTag reports whether a catalogue row holds tag, so add-by-tag refuses before reading github.com.
func (s *Store) HasTag(ctx context.Context, tag string) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM compat_tools WHERE tag = $1)`, tag).Scan(&ok)
	return ok, err
}

// Add stores a verified tag pin, disabled, with source "tag" and no Steam name yet: the name is
// bound from the first verified install (Converger.Closed).
func (s *Store) Add(ctx context.Context, p Pin, actorID string) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO compat_tools (family, source, tag, display_name, asset_name, asset_size_bytes, asset_sha512, created_by)
		VALUES ('ge', 'tag', $1, $1, $2, $3, $4, NULLIF($5, '')::uuid)
		RETURNING id::text`, p.Tag, p.AssetName, p.SizeBytes, p.SHA512, actorID).Scan(&id)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return "", ErrExists
	}
	return id, err
}

// List returns the catalogue and the fleet default's id: Valve builds first, then GE newest tag first.
func (s *Store) List(ctx context.Context) ([]Tool, *string, error) {
	var def *string
	if err := s.pool.QueryRow(ctx,
		`SELECT compat_tool_default_id::text FROM instance_settings WHERE id = true`).Scan(&def); err != nil &&
		!errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, fmt.Errorf("read compat default: %w", err)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT t.id::text, t.kind, t.family, t.source, t.tag, t.steam_name, t.display_name, t.recommended,
		       t.enabled, t.asset_name, t.asset_size_bytes, t.asset_sha512, t.name_conflict, t.pin_conflict,
		       t.created_at, (SELECT count(*) FROM compat_tool_mappings m WHERE m.tool_id = t.id)
		FROM compat_tools t
		ORDER BY (t.family = 'valve') DESC,
		         CASE t.steam_name WHEN 'proton_experimental' THEN 0 WHEN 'proton_hotfix' THEN 1 ELSE 2 END,
		         substring(t.steam_name from '^proton_([0-9]+)$')::int DESC NULLS LAST,
		         t.created_at DESC, t.tag DESC`)
	if err != nil {
		return nil, nil, fmt.Errorf("list compat tools: %w", err)
	}
	defer rows.Close()
	out := []Tool{}
	for rows.Next() {
		var t Tool
		var assetName, sha *string
		var size *int64
		if err := rows.Scan(&t.ID, &t.Kind, &t.Family, &t.Source, &t.Tag, &t.SteamName, &t.DisplayName,
			&t.Recommended, &t.Enabled, &assetName, &size, &sha, &t.NameConflict, &t.PinConflict,
			&t.CreatedAt, &t.Mappings); err != nil {
			return nil, nil, err
		}
		if t.Family == "ge" && assetName != nil && size != nil && sha != nil && t.Tag != nil {
			t.Asset = &Asset{Name: *assetName, SizeBytes: *size, SHA512: *sha,
				URL: "https://github.com/GloriousEggroll/proton-ge-custom/releases/download/" + *t.Tag + "/" + *assetName}
			t.Hosts = &HostCount{} // filled by Converger.counts
		}
		t.IsDefault = def != nil && *def == t.ID
		out = append(out, t)
	}
	return out, def, rows.Err()
}

// SetDefault sets or clears the fleet default and returns the previous one. It locks
// instance_settings, then the tool row FOR SHARE (control-api.md §Usable, "Lock order").
func (s *Store) SetDefault(ctx context.Context, toolID *string) (prev *string, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `INSERT INTO instance_settings (id) VALUES (true) ON CONFLICT DO NOTHING`); err != nil {
		return nil, fmt.Errorf("ensure instance settings: %w", err)
	}
	if err := tx.QueryRow(ctx,
		`SELECT compat_tool_default_id::text FROM instance_settings WHERE id = true FOR UPDATE`).Scan(&prev); err != nil {
		return nil, fmt.Errorf("lock instance settings: %w", err)
	}
	if toolID != nil {
		var usable bool
		err := tx.QueryRow(ctx, `SELECT enabled AND NOT name_conflict AND NOT pin_conflict
			FROM compat_tools WHERE id = $1::uuid FOR SHARE`, *toolID).Scan(&usable)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		if err != nil {
			return nil, err
		}
		if !usable {
			return nil, ErrNotUsable
		}
	}
	if _, err := tx.Exec(ctx,
		`UPDATE instance_settings SET compat_tool_default_id = $1::uuid WHERE id = true`, toolID); err != nil {
		return nil, err
	}
	return prev, tx.Commit(ctx)
}

// Toggled is what an accepted enable or disable changed, for its audit row.
type Toggled struct {
	Changed     bool
	Tag, SHA512 *string
	HostIDs     []string
}

// SetEnabled is PATCH /v1/admin/compat-tools/{id}. It locks instance_settings FOR UPDATE, then the
// tool row (control-api.md §Usable, "Lock order"); an enable counts the enabled GE tools under that
// lock. A disable clears name_conflict (§Every state has an exit). A repeat is a no-op.
func (s *Store) SetEnabled(ctx context.Context, id string, enabled bool) (Toggled, error) {
	var out Toggled
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `INSERT INTO instance_settings (id) VALUES (true) ON CONFLICT DO NOTHING`); err != nil {
		return out, err
	}
	var max int
	var def *string
	if err := tx.QueryRow(ctx, `SELECT compat_tools_max_enabled, compat_tool_default_id::text FROM instance_settings
		WHERE id = true FOR UPDATE`).Scan(&max, &def); err != nil {
		return out, fmt.Errorf("lock instance settings: %w", err)
	}
	var family string
	var cur, pin bool
	err = tx.QueryRow(ctx, `SELECT family, enabled, pin_conflict, tag, asset_sha512 FROM compat_tools
		WHERE id = $1::uuid FOR UPDATE`, id).Scan(&family, &cur, &pin, &out.Tag, &out.SHA512)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	if cur == enabled {
		return out, tx.Commit(ctx)
	}
	if enabled {
		if pin {
			return out, ErrPinConflict
		}
		if family == "ge" {
			var n int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM compat_tools WHERE family = 'ge' AND enabled`).Scan(&n); err != nil {
				return out, err
			}
			if n >= max {
				return out, ErrLimit
			}
		}
	} else {
		inUse := &InUseError{IsDefault: def != nil && *def == id}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM compat_tool_mappings WHERE tool_id = $1::uuid`, id).Scan(&inUse.Mappings); err != nil {
			return out, err
		}
		if inUse.Mappings > 0 || inUse.IsDefault {
			return out, inUse
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE compat_tools SET enabled = $2,
		first_enabled_at = CASE WHEN $2 THEN COALESCE(first_enabled_at, now()) ELSE first_enabled_at END,
		name_conflict = name_conflict AND $2
		WHERE id = $1::uuid`, id, enabled); err != nil {
		return out, err
	}
	if family == "ge" {
		rows, err := tx.Query(ctx, `SELECT id::text FROM hosts ORDER BY id LIMIT 101`)
		if err != nil {
			return out, err
		}
		if out.HostIDs, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
			return out, err
		}
	}
	out.Changed = true
	return out, tx.Commit(ctx)
}

// Entry is one mapping sent to the agent: appid "0" is the fleet default.
type Entry struct {
	AppID  string
	Name   string
	Forced bool
}

// Entries is the fleet default as appid 0, then every per-game mapping, filtered by the usable
// predicate on hostID, and the mount: the GE tools usable there (the host holds the tool's
// SHA-512 under its bound Steam name). The caller drops both for a connection without
// compat_tools_v1.
func (s *Store) Entries(ctx context.Context, hostID string) ([]Entry, []string, error) {
	mount, err := s.mountable(ctx, hostID)
	if err != nil {
		return nil, nil, err
	}
	rows, err := s.pool.Query(ctx, `
		WITH usable AS (
			SELECT t.id, t.steam_name FROM compat_tools t
			WHERE t.enabled AND NOT t.name_conflict AND NOT t.pin_conflict
			  AND (t.family = 'valve' OR t.steam_name = ANY($1::text[])))
		SELECT '0', u.steam_name, false
		FROM instance_settings i JOIN usable u ON u.id = i.compat_tool_default_id
		WHERE i.id = true
		UNION ALL
		SELECT m.external_id, u.steam_name, m.forced
		FROM compat_tool_mappings m JOIN usable u ON u.id = m.tool_id
		WHERE m.provider = 'steam'
		ORDER BY 1`, mount)
	if err != nil {
		return nil, nil, fmt.Errorf("read compat entries: %w", err)
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.AppID, &e.Name, &e.Forced); err != nil {
			return nil, nil, err
		}
		out = append(out, e)
	}
	return out, mount, rows.Err()
}

func (s *Store) mountable(ctx context.Context, hostID string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT t.steam_name FROM compat_tools t
		JOIN host_compat_tools h ON h.host_id = $1::uuid AND h.sha512 = t.asset_sha512 AND h.name = t.steam_name
		WHERE t.family = 'ge' AND t.enabled AND NOT t.name_conflict AND NOT t.pin_conflict
		ORDER BY t.created_at DESC, t.tag DESC LIMIT 32`, hostID)
	if err != nil {
		return nil, fmt.Errorf("read mountable compat tools: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// Mapping is the CompatToolMapping read shape.
type Mapping struct {
	Provider   string    `json:"provider"`
	ExternalID string    `json:"external_id"`
	ToolID     string    `json:"tool_id"`
	Forced     bool      `json:"forced"`
	AppIDs     []string  `json:"app_ids"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// ToolRef names a tool in an audit row; Tag and SHA512 are set for a GE tool only.
type ToolRef struct{ ID, Tag, SHA512 string }

// Mappings lists the per-game mappings by provider, then appid numerically. app_ids are the apps
// that are this game (the derived tiles); a mapping can precede discovery, so it may be empty.
func (s *Store) Mappings(ctx context.Context) ([]Mapping, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT m.provider, m.external_id, m.tool_id::text, m.forced, m.updated_at,
		       COALESCE((SELECT array_agg(a.id::text ORDER BY a.id) FROM apps a
		                 WHERE a.external_source = m.provider AND a.external_id = m.external_id), '{}')
		FROM compat_tool_mappings m
		ORDER BY m.provider, m.external_id::bigint`)
	if err != nil {
		return nil, fmt.Errorf("list compat mappings: %w", err)
	}
	defer rows.Close()
	out := []Mapping{}
	for rows.Next() {
		var m Mapping
		if err := rows.Scan(&m.Provider, &m.ExternalID, &m.ToolID, &m.Forced, &m.UpdatedAt, &m.AppIDs); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SetMapping replaces the Steam game's mapping and returns it with the one it replaced (nil if none).
// It takes FOR SHARE on the tool row first and checks usable after the lock (control-api.md §Usable).
func (s *Store) SetMapping(ctx context.Context, appID, toolID string, forced bool, actorID string) (m Mapping, prev *Mapping, tool, prevTool ToolRef, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var usable bool
	var tag, sha *string
	err = tx.QueryRow(ctx, `SELECT enabled AND NOT name_conflict AND NOT pin_conflict, tag, asset_sha512
		FROM compat_tools WHERE id = $1::uuid FOR SHARE`, toolID).Scan(&usable, &tag, &sha)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
		return
	}
	if err != nil {
		return
	}
	if !usable {
		err = ErrNotUsable
		return
	}
	tool = refOf(toolID, tag, sha)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, mappingCapLock); err != nil {
		return
	}
	var p Mapping
	switch e := tx.QueryRow(ctx, `SELECT tool_id::text, forced FROM compat_tool_mappings
		WHERE provider = 'steam' AND external_id = $1 FOR UPDATE`, appID).Scan(&p.ToolID, &p.Forced); {
	case errors.Is(e, pgx.ErrNoRows):
		var n int
		if err = tx.QueryRow(ctx, `SELECT count(*) FROM compat_tool_mappings`).Scan(&n); err != nil {
			return
		}
		if n >= MaxMappings {
			err = ErrMappingsFull
			return
		}
	case e != nil:
		err = e
		return
	default:
		prev = &p
		if err = tx.QueryRow(ctx, `SELECT tag, asset_sha512 FROM compat_tools WHERE id = $1::uuid`, p.ToolID).Scan(&tag, &sha); err != nil {
			return
		}
		prevTool = refOf(p.ToolID, tag, sha)
	}
	if err = tx.QueryRow(ctx, `
		INSERT INTO compat_tool_mappings (provider, external_id, tool_id, forced, updated_by)
		VALUES ('steam', $1, $2::uuid, $3, $4::uuid)
		ON CONFLICT (provider, external_id) DO UPDATE
		   SET tool_id = EXCLUDED.tool_id, forced = EXCLUDED.forced, updated_by = EXCLUDED.updated_by, updated_at = now()
		RETURNING provider, external_id, tool_id::text, forced, updated_at`, appID, toolID, forced, actorID).
		Scan(&m.Provider, &m.ExternalID, &m.ToolID, &m.Forced, &m.UpdatedAt); err != nil {
		return
	}
	err = tx.QueryRow(ctx, `SELECT COALESCE(array_agg(id::text ORDER BY id), '{}') FROM apps
		WHERE external_source = 'steam' AND external_id = $1`, appID).Scan(&m.AppIDs)
	if err == nil {
		err = tx.Commit(ctx)
	}
	return
}

func refOf(id string, tag, sha *string) ToolRef {
	r := ToolRef{ID: id}
	if tag != nil && sha != nil {
		r.Tag, r.SHA512 = *tag, *sha
	}
	return r
}

// ClearMapping removes the Steam game's mapping and returns it (nil when none existed).
func (s *Store) ClearMapping(ctx context.Context, appID string) (prev *Mapping, prevTool ToolRef, err error) {
	var p Mapping
	var tag, sha *string
	err = s.pool.QueryRow(ctx, `
		WITH d AS (DELETE FROM compat_tool_mappings WHERE provider = 'steam' AND external_id = $1
		           RETURNING tool_id, forced)
		SELECT d.tool_id::text, d.forced, t.tag, t.asset_sha512 FROM d JOIN compat_tools t ON t.id = d.tool_id`, appID).
		Scan(&p.ToolID, &p.Forced, &tag, &sha)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ToolRef{}, nil
	}
	if err != nil {
		return nil, ToolRef{}, err
	}
	return &p, refOf(p.ToolID, tag, sha), nil
}
