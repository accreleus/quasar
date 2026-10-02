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
//
// The stored JSONB carries two control-plane-owned bookkeeping keys beside
// the agent's report (schema.md amendment 18): `_handled_restored_request_id`
// (ResetEnabledOnRestoredAccess's once-per-request_id guard) and
// `_placement_hold_pending` (the PATCH-triggered placement hold,
// admission_query.go consoleAccessGate). Both survive an ordinary capacity
// resend untouched — an agent resends the same report on every reconnect and
// every hotplug, and losing either key on a report that changes nothing would
// re-arm a request_id the admin already retried, or drop a hold the report did
// not settle. `_placement_hold_pending` is the exception: when this report
// carries no `access` at all, amendment 18 says the host is "never held", so
// it is dropped rather than preserved.
func (s *Store) UpsertCapabilities(ctx context.Context, hostID string, caps Capabilities) error {
	raw, err := json.Marshal(caps)
	if err != nil {
		return fmt.Errorf("encode console_capabilities: %w", err)
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO console_capabilities (host_id, capabilities, updated_at)
		VALUES ($1::uuid, $2, now())
		ON CONFLICT (host_id) DO UPDATE
		    SET capabilities = EXCLUDED.capabilities
		            || CASE WHEN console_capabilities.capabilities ? '_handled_restored_request_id'
		                    THEN jsonb_build_object('_handled_restored_request_id',
		                             console_capabilities.capabilities->'_handled_restored_request_id')
		                    ELSE '{}'::jsonb END
		            || CASE WHEN EXCLUDED.capabilities ? 'access'
		                     AND console_capabilities.capabilities ? '_placement_hold_pending'
		                    THEN jsonb_build_object('_placement_hold_pending',
		                             console_capabilities.capabilities->'_placement_hold_pending')
		                    ELSE '{}'::jsonb END,
		        updated_at = now()
	`, hostID, raw)
	if err != nil {
		return fmt.Errorf("upsert console_capabilities: %w", err)
	}
	return nil
}

// ClearAccess removes any stored console-access report for hostID, leaving
// every other reported capability field and the handled-restored-request-id
// bookkeeping untouched — amendment 18's rule for a capacity report with no
// `console_capabilities` at all (an agent that predates the amendment, or a
// Compose/source install). It also drops the placement hold: "a stored access
// is cleared by any capacity without it ... so a host whose agent stops
// reporting it is never held" (control-api.md). No-op if there is no row yet.
func (s *Store) ClearAccess(ctx context.Context, hostID string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE console_capabilities
		   SET capabilities = (capabilities - 'access') - '_placement_hold_pending',
		       updated_at   = now()
		 WHERE host_id::text = $1
	`, hostID)
	if err != nil {
		return fmt.Errorf("clear console access: %w", err)
	}
	return nil
}

// SetPlacementHoldPending records or lifts the amendment 18 placement hold
// the console-config PATCH sets when it accepts a change of `enabled` on a
// host that reports access (control-api.md §Console mode "Placement"). Read
// at candidate-selection time by admission_query.go consoleAccessGate — no
// Go-side query needed, since the gate is pure SQL over this same JSONB. A
// no-op if there is no console_capabilities row yet: nothing to hold on a
// host that has never reported.
func (s *Store) SetPlacementHoldPending(ctx context.Context, hostID string, pending bool) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE console_capabilities
		   SET capabilities = jsonb_set(capabilities, '{_placement_hold_pending}', to_jsonb($2::bool), true),
		       updated_at   = now()
		 WHERE host_id::text = $1
	`, hostID, pending)
	if err != nil {
		return fmt.Errorf("set console placement hold: %w", err)
	}
	return nil
}

// ResetEnabledOnRestoredAccess applies amendment 18's restored-attempt reset
// (control-api.md §Console mode "Restored attempt"): when access.State is
// "restored", its target equals the stored `enabled`, and its request_id has
// not already been handled for this host, it flips `enabled` to the access
// the host actually kept (!target), stamps updated_by NULL (the system, not
// an admin), and records the request_id as handled — inside one transaction,
// serialized per host by an advisory lock, so a replayed report (every
// capacity resend carries the current access) cannot act twice on the same
// request_id, and a genuinely new "try again" PATCH is unaffected (it changes
// `enabled` again, so a later restored report's target no longer matches).
//
// For every other state (or a restored report with no request_id/target) this
// is a read-only no-op that still returns the resolved config, so the caller
// can decide whether this report settles the placement hold without a second
// query.
func (s *Store) ResetEnabledOnRestoredAccess(ctx context.Context, hostID string, access Access) (applied bool, resolved ConsoleConfig, err error) {
	if access.State != "restored" || access.RequestID == nil || access.Target == nil {
		sparse, err := s.Get(ctx, hostID)
		if err != nil {
			return false, ConsoleConfig{}, err
		}
		resolved, err := Resolve(sparse)
		return false, resolved, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, ConsoleConfig{}, fmt.Errorf("begin restored-reset tx: %w", err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, hostID); err != nil {
		return false, ConsoleConfig{}, fmt.Errorf("lock host for restored-reset: %w", err)
	}

	var capsRaw []byte
	err = tx.QueryRow(ctx, `SELECT capabilities FROM console_capabilities WHERE host_id::text = $1`, hostID).Scan(&capsRaw)
	if err != nil && err != pgx.ErrNoRows {
		return false, ConsoleConfig{}, fmt.Errorf("query console_capabilities: %w", err)
	}
	if len(capsRaw) > 0 {
		var rec struct {
			HandledRestoredRequestID *string `json:"_handled_restored_request_id"`
		}
		if err := json.Unmarshal(capsRaw, &rec); err != nil {
			return false, ConsoleConfig{}, fmt.Errorf("decode console_capabilities: %w", err)
		}
		if rec.HandledRestoredRequestID != nil && *rec.HandledRestoredRequestID == *access.RequestID {
			sparse := map[string]any{}
			if cfgRaw, err := s.rawConfig(ctx, tx, hostID); err != nil {
				return false, ConsoleConfig{}, err
			} else if len(cfgRaw) > 0 {
				if err := json.Unmarshal(cfgRaw, &sparse); err != nil {
					return false, ConsoleConfig{}, fmt.Errorf("decode console_config: %w", err)
				}
			}
			resolved, err := Resolve(sparse)
			return false, resolved, err
		}
	}

	cfgRaw, err := s.rawConfig(ctx, tx, hostID)
	if err != nil {
		return false, ConsoleConfig{}, err
	}
	sparse := map[string]any{}
	if len(cfgRaw) > 0 {
		if err := json.Unmarshal(cfgRaw, &sparse); err != nil {
			return false, ConsoleConfig{}, fmt.Errorf("decode console_config: %w", err)
		}
	}
	resolved, err = Resolve(sparse)
	if err != nil {
		return false, ConsoleConfig{}, err
	}
	if resolved.Enabled != *access.Target {
		// Not our restore: the stored `enabled` has moved on since the attempt
		// this report describes (e.g. an admin's later PATCH already changed
		// it again).
		return false, resolved, nil
	}

	newEnabled := !*access.Target
	sparse["enabled"] = newEnabled
	rawCfg, err := json.Marshal(sparse)
	if err != nil {
		return false, ConsoleConfig{}, fmt.Errorf("encode console_config: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO console_config (host_id, config, updated_by, updated_at)
		VALUES ($1::uuid, $2, NULL, now())
		ON CONFLICT (host_id) DO UPDATE
		    SET config     = EXCLUDED.config,
		        updated_by = NULL,
		        updated_at = now()
	`, hostID, rawCfg); err != nil {
		return false, ConsoleConfig{}, fmt.Errorf("upsert console_config: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO console_capabilities (host_id, capabilities, updated_at)
		VALUES ($1::uuid, jsonb_build_object('_handled_restored_request_id', $2::text), now())
		ON CONFLICT (host_id) DO UPDATE
		    SET capabilities = jsonb_set(console_capabilities.capabilities, '{_handled_restored_request_id}', to_jsonb($2::text), true),
		        updated_at   = now()
	`, hostID, *access.RequestID); err != nil {
		return false, ConsoleConfig{}, fmt.Errorf("mark restored request_id handled: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return false, ConsoleConfig{}, fmt.Errorf("commit restored-reset: %w", err)
	}
	resolved.Enabled = newEnabled
	return true, resolved, nil
}

// rawConfig reads the host's sparse console_config JSON inside tx, locking the
// row (or noting its absence) so ResetEnabledOnRestoredAccess's read and its
// later write are consistent within one transaction.
func (s *Store) rawConfig(ctx context.Context, tx pgx.Tx, hostID string) ([]byte, error) {
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT config FROM console_config WHERE host_id::text = $1 FOR UPDATE`, hostID).Scan(&raw)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query console_config: %w", err)
	}
	return raw, nil
}
