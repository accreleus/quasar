package compat

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound = errors.New("compat tool not found")
	// ErrNotUsable: the tool is disabled or marked (control-api.md §Usable).
	ErrNotUsable = errors.New("compat tool is not usable")
)

type Store struct{ pool *pgxpool.Pool }

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
// deletes a row or changes enabled. A tag row with another pin cannot exist until add-by-tag does.
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
	for _, r := range recommended {
		tags = append(tags, r.Tag)
		if _, err := tx.Exec(ctx, `
			INSERT INTO compat_tools (family, source, tag, steam_name, display_name, recommended,
			                          asset_name, asset_size_bytes, asset_sha512)
			VALUES ('ge', 'recommended', $1, $2, $2, true, $3, $4, $5)
			ON CONFLICT (tag) DO NOTHING`, r.Tag, r.SteamName, r.Asset, r.SizeBytes, r.SHA512); err != nil {
			return fmt.Errorf("compat reconcile %s: %w", r.Tag, err)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE compat_tools SET recommended = (tag = ANY($1::text[]))
		WHERE family = 'ge' AND recommended IS DISTINCT FROM (tag = ANY($1::text[]))`, tags); err != nil {
		return fmt.Errorf("compat reconcile recommended: %w", err)
	}
	return tx.Commit(ctx)
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
			t.Hosts = &HostCount{} // nothing installs yet, so no host has a state to count
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

// Entry is one mapping sent to the agent: appid "0" is the fleet default.
type Entry struct {
	AppID  string
	Name   string
	Forced bool
}

// Entries is the fleet default as appid 0, then every per-game mapping, filtered by the usable
// predicate. A GE tool is never usable here: no host holds one yet (mount is always empty).
func (s *Store) Entries(ctx context.Context) ([]Entry, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT '0', t.steam_name, false
		FROM instance_settings i JOIN compat_tools t ON t.id = i.compat_tool_default_id
		WHERE i.id = true AND t.family = 'valve' AND t.enabled AND NOT t.name_conflict AND NOT t.pin_conflict
		UNION ALL
		SELECT m.external_id, t.steam_name, m.forced
		FROM compat_tool_mappings m JOIN compat_tools t ON t.id = m.tool_id
		WHERE m.provider = 'steam' AND t.family = 'valve' AND t.enabled AND NOT t.name_conflict AND NOT t.pin_conflict
		ORDER BY 1`)
	if err != nil {
		return nil, fmt.Errorf("read compat entries: %w", err)
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.AppID, &e.Name, &e.Forced); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
