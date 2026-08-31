package hostcfg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store is the host_settings data-access layer.
type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Get returns the host's sparse override map (empty if no row exists).
func (s *Store) Get(ctx context.Context, hostID string) (map[string]any, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx,
		`SELECT overrides FROM host_settings WHERE host_id::text = $1`, hostID).Scan(&raw)
	if err == pgx.ErrNoRows {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query host_settings: %w", err)
	}
	out := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, fmt.Errorf("decode overrides: %w", err)
		}
	}
	return out, nil
}

// GetEffective returns the host's last agent-reported effective settings
// (host-observability, agent-api.md `capacity.effective_settings`) — the true
// env←overrides overlay the agent process is running with. Returns nil when the
// host has never reported one (column NULL, including pre-amendment agents) or
// the host row doesn't exist.
func (s *Store) GetEffective(ctx context.Context, hostID string) (map[string]string, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx,
		`SELECT effective_settings FROM hosts WHERE id::text = $1`, hostID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query effective_settings: %w", err)
	}
	if len(raw) == 0 {
		return nil, nil
	}
	var out map[string]string
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode effective_settings: %w", err)
	}
	return out, nil
}

// HomeRoot resolves a host's managed-home root, first non-empty wins:
// stored override → agent-reported effective home_root → envFallback (the
// control plane's own QUASAR_HOME_ROOT) → "". Empty is a loud launch failure
// for managed-home apps, never a silent fall back to the volume driver
// (internal/storage.Manager.resolveDriver). Hosts may resolve differently.
func (s *Store) HomeRoot(ctx context.Context, hostID, envFallback string) (string, error) {
	overrides, err := s.Get(ctx, hostID)
	if err != nil {
		return "", err
	}
	if v, ok := overrides["home_root"]; ok {
		if str, ok := v.(string); ok && str != "" {
			return str, nil
		}
	}
	eff, err := s.GetEffective(ctx, hostID)
	if err != nil {
		return "", err
	}
	if eff != nil {
		if str := eff["home_root"]; str != "" {
			return str, nil
		}
	}
	return envFallback, nil
}

// GetCodecs returns the host's last-reported wire codec set (hosts.codecs, from
// `capacity.codecs`) for the read-only `codecs` field (control-api.md, §S5).
// nil means "never reported"; never normalise it to ["h264"] the way
// session.Store.HostCodecs does — the operator surface must distinguish
// "H.264-only" from "never told us"; only the launch path may collapse the two.
// Malformed JSON is an error, not a silent nil: this read path has no launch
// to protect.
func (s *Store) GetCodecs(ctx context.Context, hostID string) ([]string, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx,
		`SELECT codecs FROM hosts WHERE id::text = $1`, hostID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query host codecs: %w", err)
	}
	if len(raw) == 0 {
		return nil, nil
	}
	var codecs []string
	if err := json.Unmarshal(raw, &codecs); err != nil {
		return nil, fmt.Errorf("decode host codecs: %w", err)
	}
	if len(codecs) == 0 {
		return nil, nil
	}
	return codecs, nil
}

// HostStatus returns a host's current status (online|offline|draining) and
// whether the host row exists at all (host-observability-2, backs the
// POST /v1/admin/hosts/{id}/restart 404/409-offline checks).
func (s *Store) HostStatus(ctx context.Context, hostID string) (status string, found bool, err error) {
	err = s.pool.QueryRow(ctx, `SELECT status FROM hosts WHERE id::text = $1`, hostID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("query host status: %w", err)
	}
	return status, true, nil
}

// GetPendingRestart returns whether a restart-class change is pending; false
// when the host row doesn't exist.
func (s *Store) GetPendingRestart(ctx context.Context, hostID string) (bool, error) {
	var pending bool
	err := s.pool.QueryRow(ctx, `SELECT pending_restart FROM hosts WHERE id::text = $1`, hostID).Scan(&pending)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("query pending_restart: %w", err)
	}
	return pending, nil
}

// SetPendingRestart sets or clears the host's pending_restart flag. Set true
// when a restart command is sent (PATCH .../settings or POST .../restart);
// cleared on the agent's next register (agentws.enrollHost/reconnectHost).
func (s *Store) SetPendingRestart(ctx context.Context, hostID string, pending bool) error {
	_, err := s.pool.Exec(ctx, `UPDATE hosts SET pending_restart=$2 WHERE id::text=$1`, hostID, pending)
	if err != nil {
		return fmt.Errorf("update pending_restart: %w", err)
	}
	return nil
}

// Upsert writes the full override map for a host. updatedBy may be nil.
func (s *Store) Upsert(ctx context.Context, hostID string, overrides map[string]any, updatedBy *string) error {
	raw, err := json.Marshal(overrides)
	if err != nil {
		return fmt.Errorf("encode overrides: %w", err)
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO host_settings (host_id, overrides, updated_by, updated_at)
		VALUES ($1::uuid, $2, $3, now())
		ON CONFLICT (host_id) DO UPDATE
		    SET overrides = EXCLUDED.overrides,
		        updated_by = EXCLUDED.updated_by,
		        updated_at = now()
	`, hostID, raw, updatedBy)
	if err != nil {
		return fmt.Errorf("upsert host_settings: %w", err)
	}
	return nil
}
