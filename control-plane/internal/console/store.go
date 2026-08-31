package console

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store is the console_config / console_capabilities data-access layer.
type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// HostExists reports whether hostID has a hosts row (used for the handler's
// 404 semantics — a host with no console_config row is not the same as no
// host at all).
func (s *Store) HostExists(ctx context.Context, hostID string) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM hosts WHERE id::text = $1)`, hostID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check host exists: %w", err)
	}
	return exists, nil
}

// AppExists reports whether appID has an apps row (default_app FK check).
func (s *Store) AppExists(ctx context.Context, appID string) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM apps WHERE id::text = $1)`, appID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check app exists: %w", err)
	}
	return exists, nil
}

// UserExists reports whether userID has a users row (default_user FK check).
func (s *Store) UserExists(ctx context.Context, userID string) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM users WHERE id::text = $1)`, userID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check user exists: %w", err)
	}
	return exists, nil
}

// Get returns the host's sparse console-config override map (empty if no row
// exists). Absent keys resolve to Defaults() at read time — see Resolve.
func (s *Store) Get(ctx context.Context, hostID string) (map[string]any, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx,
		`SELECT config FROM console_config WHERE host_id::text = $1`, hostID).Scan(&raw)
	if err == pgx.ErrNoRows {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query console_config: %w", err)
	}
	out := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, fmt.Errorf("decode console_config: %w", err)
		}
	}
	return out, nil
}

// Upsert writes the full sparse config map for a host. updatedBy may be nil.
func (s *Store) Upsert(ctx context.Context, hostID string, config map[string]any, updatedBy *string) error {
	raw, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("encode console_config: %w", err)
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO console_config (host_id, config, updated_by, updated_at)
		VALUES ($1::uuid, $2, $3, now())
		ON CONFLICT (host_id) DO UPDATE
		    SET config     = EXCLUDED.config,
		        updated_by = EXCLUDED.updated_by,
		        updated_at = now()
	`, hostID, raw, updatedBy)
	if err != nil {
		return fmt.Errorf("upsert console_config: %w", err)
	}
	return nil
}

// GetCapabilities returns the host's latest reported console capabilities
// (empty arrays if the agent has never reported / is offline).
func (s *Store) GetCapabilities(ctx context.Context, hostID string) (Capabilities, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx,
		`SELECT capabilities FROM console_capabilities WHERE host_id::text = $1`, hostID).Scan(&raw)
	if err == pgx.ErrNoRows {
		return EmptyCapabilities(), nil
	}
	if err != nil {
		return Capabilities{}, fmt.Errorf("query console_capabilities: %w", err)
	}
	out := EmptyCapabilities()
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			return Capabilities{}, fmt.Errorf("decode console_capabilities: %w", err)
		}
	}
	return out, nil
}

// UpsertCapabilities replaces the host's latest reported console capabilities
// (agent-api.md capacity.console_capabilities) — written by the capacity
// handler, never by the admin PATCH.
func (s *Store) UpsertCapabilities(ctx context.Context, hostID string, caps Capabilities) error {
	raw, err := json.Marshal(caps)
	if err != nil {
		return fmt.Errorf("encode console_capabilities: %w", err)
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO console_capabilities (host_id, capabilities, updated_at)
		VALUES ($1::uuid, $2, now())
		ON CONFLICT (host_id) DO UPDATE
		    SET capabilities = EXCLUDED.capabilities,
		        updated_at   = now()
	`, hostID, raw)
	if err != nil {
		return fmt.Errorf("upsert console_capabilities: %w", err)
	}
	return nil
}
