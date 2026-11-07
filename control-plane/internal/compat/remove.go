package compat

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	// ErrNotDeletable: the conditional DELETE removed nothing (DELETE /v1/admin/compat-tools/{id}).
	ErrNotDeletable = errors.New("compat tool cannot be deleted")
	// ErrValve: Quasar installs no Valve build, so there is nothing to force-remove.
	ErrValve = errors.New("a Valve build is not installed by Quasar")
)

// liveHolder is true for a live session created at or before the tool's force_remove_pending_at
// that mounts the tool's Steam name (t is the compat_tools row, s a sessions row).
const liveHolder = `t.steam_name IS NOT NULL AND s.compat_tool_names @> ARRAY[t.steam_name]
	AND s.state NOT IN ('stopped','failed')`

// Delete is DELETE /v1/admin/compat-tools/{id}: a never-enabled GE row, or a stuck pin_conflict
// nothing uses, removed by one conditional statement after the row lock (control-api.md
// §Compatibility tools). A removed pin_conflict row is replaced by the shipped entry in the same
// transaction. It returns the removed row's tag and pin.
func (s *Store) Delete(ctx context.Context, id string) (tag, sha string, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := tx.QueryRow(ctx, `SELECT 1 FROM compat_tools WHERE id = $1::uuid FOR UPDATE`, id).Scan(new(int)); errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrNotFound
	} else if err != nil {
		return "", "", err
	}
	var wasPin bool
	err = tx.QueryRow(ctx, `
		DELETE FROM compat_tools t
		WHERE t.id = $1::uuid AND t.family = 'ge' AND NOT t.enabled AND (
		    (t.first_enabled_at IS NULL AND NOT t.recommended)
		 OR (t.pin_conflict
		     AND NOT EXISTS (SELECT 1 FROM compat_tool_mappings m WHERE m.tool_id = t.id)
		     AND NOT EXISTS (SELECT 1 FROM instance_settings i WHERE i.compat_tool_default_id = t.id)
		     AND NOT EXISTS (SELECT 1 FROM host_compat_tools h WHERE h.sha512 = t.asset_sha512)
		     AND NOT EXISTS (SELECT 1 FROM sessions s WHERE `+liveHolder+`)))
		RETURNING t.tag, t.asset_sha512, t.pin_conflict`, id).Scan(&tag, &sha, &wasPin)
	var pgErr *pgconn.PgError
	switch {
	case errors.Is(err, pgx.ErrNoRows), errors.As(err, &pgErr) && pgErr.Code == "23503":
		return "", "", ErrNotDeletable
	case err != nil:
		return "", "", err
	}
	if wasPin {
		for _, r := range recommended {
			if r.Tag != tag {
				continue
			}
			if _, err := tx.Exec(ctx, `INSERT INTO compat_tools (family, source, tag, steam_name, display_name, recommended,
				asset_name, asset_size_bytes, asset_sha512) VALUES ('ge', 'recommended', $1, $2, $2, true, $3, $4, $5)
				ON CONFLICT DO NOTHING`, r.Tag, r.SteamName, r.Asset, r.SizeBytes, r.SHA512); err != nil {
				return "", "", fmt.Errorf("insert the shipped entry: %w", err)
			}
		}
	}
	return tag, sha, tx.Commit(ctx)
}

// Removed is what an accepted force-remove changed, for the response and its audit row.
type Removed struct {
	Tag, SHA512     *string
	MappingsCleared int
	DefaultCleared  bool
	SessionsStopped int
	HostIDs         []string
}

// ForceRemove is POST /v1/admin/compat-tools/{id}/force-remove. One transaction, in the lock order
// of control-api.md §Usable: instance_settings, the tool row, the mappings, then the parked
// sessions. The stop step runs after commit; a failure there is retried by StopHolders.
func (s *Store) ForceRemove(ctx context.Context, id string) (Removed, error) {
	var out Removed
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `INSERT INTO instance_settings (id) VALUES (true) ON CONFLICT DO NOTHING`); err != nil {
		return out, err
	}
	var def *string
	if err := tx.QueryRow(ctx, `SELECT compat_tool_default_id::text FROM instance_settings WHERE id = true FOR UPDATE`).Scan(&def); err != nil {
		return out, fmt.Errorf("lock instance settings: %w", err)
	}
	var family string
	err = tx.QueryRow(ctx, `SELECT family, tag, asset_sha512 FROM compat_tools WHERE id = $1::uuid FOR UPDATE`, id).
		Scan(&family, &out.Tag, &out.SHA512)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	if family != "ge" {
		return out, ErrValve
	}
	del, err := tx.Exec(ctx, `DELETE FROM compat_tool_mappings WHERE tool_id = $1::uuid`, id)
	if err != nil {
		return out, err
	}
	out.MappingsCleared = int(del.RowsAffected())
	if out.DefaultCleared = def != nil && *def == id; out.DefaultCleared {
		if _, err := tx.Exec(ctx, `UPDATE instance_settings SET compat_tool_default_id = NULL WHERE id = true`); err != nil {
			return out, err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE compat_tools SET enabled = false, name_conflict = false WHERE id = $1::uuid`, id); err != nil {
		return out, err
	}
	after, err := s.supersede(ctx, tx)
	if err != nil {
		return out, err
	}
	// Last, so a session created before the mark is one a launch committed before this lock.
	if _, err := tx.Exec(ctx, `UPDATE compat_tools SET force_remove_pending_at = clock_timestamp() WHERE id = $1::uuid`, id); err != nil {
		return out, err
	}
	rows, err := tx.Query(ctx, `SELECT id::text FROM hosts ORDER BY id LIMIT 101`)
	if err != nil {
		return out, err
	}
	if out.HostIDs, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		return out, err
	}
	if err := tx.Commit(ctx); err != nil {
		return out, err
	}
	after()
	if out.SessionsStopped, err = s.stopTool(ctx, id); err != nil {
		slog.Warn("compat tools: force-remove stop step incomplete; it is retried", "tool_id", id, "err", err)
	}
	return out, nil
}

// stopTool ends the live sessions that mounted the tool before its mark, then clears the mark once
// none remain. A session already stopping is waited on, not stopped again.
func (s *Store) stopTool(ctx context.Context, id string) (int, error) {
	if s.Stopper == nil {
		return 0, errors.New("no session stopper wired")
	}
	rows, err := s.pool.Query(ctx, `SELECT s.id::text, s.state FROM sessions s
		JOIN compat_tools t ON t.id = $1::uuid AND t.force_remove_pending_at IS NOT NULL
		WHERE `+liveHolder+` AND s.created_at <= t.force_remove_pending_at`, id)
	if err != nil {
		return 0, err
	}
	type held struct{ id, state string }
	live, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (h held, err error) { return h, r.Scan(&h.id, &h.state) })
	if err != nil {
		return 0, err
	}
	var stopped int
	var errs error
	for _, h := range live {
		if h.state == "stopping" {
			continue
		}
		if err := s.Stopper(ctx, h.id); err != nil {
			errs = errors.Join(errs, fmt.Errorf("stop session %s: %w", h.id, err))
			continue
		}
		stopped++
	}
	if errs != nil {
		return stopped, errs
	}
	_, err = s.pool.Exec(ctx, `UPDATE compat_tools t SET force_remove_pending_at = NULL
		WHERE t.id = $1::uuid AND t.force_remove_pending_at IS NOT NULL
		  AND NOT EXISTS (SELECT 1 FROM sessions s WHERE `+liveHolder+` AND s.created_at <= t.force_remove_pending_at)`, id)
	return stopped, err
}

// StopHolders re-runs the stop step for every tool with a force-remove pending.
func (s *Store) StopHolders(ctx context.Context) error {
	rows, err := s.pool.Query(ctx, `SELECT id::text FROM compat_tools WHERE force_remove_pending_at IS NOT NULL`)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	var errs error
	for _, id := range ids {
		if _, err := s.stopTool(ctx, id); err != nil {
			errs = errors.Join(errs, err)
		}
	}
	return errs
}

// RunStopLoop retries the stop step at startup and every interval until ctx ends, so a crash
// between the force-remove commit and the stops loses nothing.
func (s *Store) RunStopLoop(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := s.StopHolders(ctx); err != nil {
			slog.Warn("compat tools: force-remove stop step failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
