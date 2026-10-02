// Package readinessgate is the DB-side half of evidence-gated readiness
// (control-api.md "Readiness override"): storing a report, setting or clearing
// an admin override, and recomputing the scheduling verdict all share one
// transaction shape — lock the host row, re-read the stored report, write,
// recompute — so an override and a report can never leave the derived columns
// describing a state older than either write.
//
// Lock order, everywhere in this package: the host row first (SELECT ... FOR
// UPDATE, or an UPDATE that takes the same lock), then host_readiness_overrides,
// then gpus. Every entry point takes that same first lock before touching
// anything else, so any two of them serialize on it instead of deadlocking.
//
// Imports only internal/readiness (the pure decision) and pgx — never agentws,
// crud or session, which is what makes it reachable from both.
package readinessgate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/audit"
	"github.com/accreleus/quasar/control-plane/internal/readiness"
)

// ErrHostNotFound is returned by SetOverride/ClearOverride for an id no host
// row matches.
var ErrHostNotFound = errors.New("host not found")

// NotOverridableError is the 409: the host's current stored report has no
// check with this id carrying `blocks` and status `fail`, or that check is
// enforced by the agent itself. Reason says which.
type NotOverridableError struct{ Reason string }

func (e *NotOverridableError) Error() string { return e.Reason }

// Override is one stored admin decision, and the wire shape of
// PUT/DELETE .../readiness-overrides/{check_id} and the host body's
// `readiness_overrides` entries (openapi.yaml ReadinessOverride).
type Override struct {
	CheckID string `json:"check_id"`
	// CreatedBy/CreatedByUsername are resolved at read time (LEFT JOIN users),
	// both nil once the author's account is gone.
	CreatedBy         *string   `json:"created_by"`
	CreatedByUsername *string   `json:"created_by_username"`
	CreatedAt         time.Time `json:"created_at"`
	// Inert: the host's current report no longer has a check with this id.
	Inert bool `json:"inert"`
}

// Gate is the write/read seam for readiness reports and overrides.
type Gate struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Gate { return &Gate{pool: pool} }

// StoreReport writes hosts.readiness and recomputes the verdict in one
// transaction. raw must already be a validated, non-nil report — the
// keep-if-absent and malformed-payload guards are the caller's job
// (agentws.upsertHostReadiness), because this package has no notion of the
// agent wire format's optionality.
func (g *Gate) StoreReport(ctx context.Context, hostID string, raw json.RawMessage) error {
	tx, err := g.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// Takes the host row lock; Recompute below re-acquires it in the same tx
	// (a no-op) before reading overrides — see the package doc's lock order.
	if _, err := tx.Exec(ctx,
		`UPDATE hosts SET readiness = $2, readiness_reported_at = now() WHERE id = $1`,
		hostID, []byte(raw)); err != nil {
		return fmt.Errorf("update host readiness: %w", err)
	}
	if err := g.Recompute(ctx, tx, hostID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Recompute re-derives one host's scheduling columns from its stored readiness
// report and its overrides, inside the caller's transaction. Also lapses:
// deletes any override whose check now passes and audits it with a null actor
// (the system, not an admin, withdrew it). Call this after every write of
// hosts.readiness and after every GPU-set write, or the derived columns
// describe a state older than the report. The caller's transaction must take
// the host row lock before it touches gpus, as the capacity write does:
// admission's recheck locks in the other order.
func (g *Gate) Recompute(ctx context.Context, tx pgx.Tx, hostID string) error {
	var nodeName string
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT node_name, readiness FROM hosts WHERE id = $1 FOR UPDATE`, hostID).
		Scan(&nodeName, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // the host was deleted under us; nothing to derive
	}
	if err != nil {
		return fmt.Errorf("lock host for readiness verdict: %w", err)
	}

	rows, err := tx.Query(ctx, `SELECT check_id FROM host_readiness_overrides WHERE host_id = $1`, hostID)
	if err != nil {
		return fmt.Errorf("read readiness overrides: %w", err)
	}
	var overrides []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("scan readiness override: %w", err)
		}
		overrides = append(overrides, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate readiness overrides: %w", err)
	}

	v := readiness.Evaluate(raw, overrides)

	// Lapse before writing the derived columns: v was computed from the
	// pre-lapse override list, but a passing check never blocks either way, so
	// the columns below already describe the post-lapse state.
	for _, id := range v.Lapsed {
		tag, err := tx.Exec(ctx,
			`DELETE FROM host_readiness_overrides WHERE host_id = $1 AND check_id = $2`, hostID, id)
		if err != nil {
			return fmt.Errorf("delete lapsed override %s: %w", id, err)
		}
		if tag.RowsAffected() == 0 {
			continue
		}
		if err := audit.RecordTx(ctx, tx, "", "host.readiness_override.lapsed", "host", hostID,
			map[string]any{"node_name": nodeName, "check_id": id}); err != nil {
			return fmt.Errorf("audit lapsed override %s: %w", id, err)
		}
	}

	if _, err := tx.Exec(ctx,
		`UPDATE hosts SET readiness_block_host = $2, readiness_block_homes = $3 WHERE id = $1`,
		hostID, v.BlockHost, v.BlockHomes); err != nil {
		return fmt.Errorf("write readiness host verdict: %w", err)
	}

	// Never nil: pgx sends a nil slice as SQL NULL, and `index = ANY(NULL)` is
	// NULL, which would leave every row's IS DISTINCT FROM unresolvable.
	blocked := v.BlockedGPUs
	if blocked == nil {
		blocked = []int{}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE gpus SET readiness_blocked = (index = ANY($2::int[]))
		WHERE host_id = $1 AND readiness_blocked IS DISTINCT FROM (index = ANY($2::int[]))
	`, hostID, blocked); err != nil {
		return fmt.Errorf("write readiness gpu verdict: %w", err)
	}
	return nil
}

// SetOverride records an admin's decision to launch on hostID despite checkID
// currently failing. Idempotent: a repeat returns the existing row (created =
// false) with its original CreatedAt, and audits nothing.
func (g *Gate) SetOverride(ctx context.Context, hostID, checkID, actorUserID string) (Override, bool, error) {
	tx, err := g.pool.Begin(ctx)
	if err != nil {
		return Override{}, false, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var nodeName string
	var raw []byte
	// id::text: hostID is caller-supplied over HTTP and need not be a well-formed
	// uuid, which a bare uuid-column comparison would reject with an error
	// instead of the "no such host" this path means to report.
	err = tx.QueryRow(ctx, `SELECT node_name, readiness FROM hosts WHERE id::text = $1 FOR UPDATE`, hostID).
		Scan(&nodeName, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return Override{}, false, ErrHostNotFound
	}
	if err != nil {
		return Override{}, false, fmt.Errorf("lock host: %w", err)
	}

	// The precondition holds for every PUT, a repeat included: an override on a
	// check that stopped failing, or became agent-enforced, excludes nothing, and
	// answering 200 for it would say otherwise. The stored row is left as it is.
	b, ok := readiness.FindBlocking(raw, checkID)
	if !ok {
		return Override{}, false, &NotOverridableError{
			Reason: fmt.Sprintf("no failing check %q currently blocks this host", checkID),
		}
	}
	if b.EnforcedBy == "agent" {
		return Override{}, false, &NotOverridableError{
			Reason: fmt.Sprintf("check %q is enforced by the agent itself; no override lifts it", checkID),
		}
	}

	if existing, ok, err := queryOverride(ctx, tx, hostID, checkID); err != nil {
		return Override{}, false, err
	} else if ok {
		if err := tx.Commit(ctx); err != nil {
			return Override{}, false, fmt.Errorf("commit: %w", err)
		}
		return existing, false, nil
	}

	var o Override
	err = tx.QueryRow(ctx, `
		WITH ins AS (
			INSERT INTO host_readiness_overrides (host_id, check_id, created_by)
			VALUES ($1, $2, NULLIF($3, '')::uuid)
			RETURNING check_id, created_by, created_at
		)
		SELECT ins.check_id, ins.created_by::text, ins.created_at, u.username
		FROM ins LEFT JOIN users u ON u.id = ins.created_by
	`, hostID, checkID, actorUserID).Scan(&o.CheckID, &o.CreatedBy, &o.CreatedAt, &o.CreatedByUsername)
	if err != nil {
		return Override{}, false, fmt.Errorf("insert override: %w", err)
	}

	if err := audit.RecordTx(ctx, tx, actorUserID, "host.readiness_override.set", "host", hostID,
		map[string]any{"node_name": nodeName, "check_id": checkID}); err != nil {
		return Override{}, false, fmt.Errorf("audit set override: %w", err)
	}
	if err := g.Recompute(ctx, tx, hostID); err != nil {
		return Override{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Override{}, false, fmt.Errorf("commit: %w", err)
	}
	return o, true, nil
}

// ClearOverride withdraws an admin override. Idempotent (removed = false on a
// repeat) and works on an inert override just as well as a live one.
func (g *Gate) ClearOverride(ctx context.Context, hostID, checkID, actorUserID string) (bool, error) {
	tx, err := g.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var nodeName string
	err = tx.QueryRow(ctx, `SELECT node_name FROM hosts WHERE id::text = $1 FOR UPDATE`, hostID).Scan(&nodeName)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrHostNotFound
	}
	if err != nil {
		return false, fmt.Errorf("lock host: %w", err)
	}

	tag, err := tx.Exec(ctx,
		`DELETE FROM host_readiness_overrides WHERE host_id = $1 AND check_id = $2`, hostID, checkID)
	if err != nil {
		return false, fmt.Errorf("delete override: %w", err)
	}
	if tag.RowsAffected() == 0 {
		if err := tx.Commit(ctx); err != nil {
			return false, fmt.Errorf("commit: %w", err)
		}
		return false, nil
	}

	if err := audit.RecordTx(ctx, tx, actorUserID, "host.readiness_override.cleared", "host", hostID,
		map[string]any{"node_name": nodeName, "check_id": checkID}); err != nil {
		return false, fmt.Errorf("audit cleared override: %w", err)
	}
	if err := g.Recompute(ctx, tx, hostID); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}
	return true, nil
}

// queryOverride reads one stored override row, if any, resolving the
// author's username at read time (nil once the account is gone).
func queryOverride(ctx context.Context, tx pgx.Tx, hostID, checkID string) (Override, bool, error) {
	var o Override
	o.CheckID = checkID
	err := tx.QueryRow(ctx, `
		SELECT o.created_by::text, o.created_at, u.username
		FROM host_readiness_overrides o LEFT JOIN users u ON u.id = o.created_by
		WHERE o.host_id = $1 AND o.check_id = $2
	`, hostID, checkID).Scan(&o.CreatedBy, &o.CreatedAt, &o.CreatedByUsername)
	if errors.Is(err, pgx.ErrNoRows) {
		return Override{}, false, nil
	}
	if err != nil {
		return Override{}, false, fmt.Errorf("query override: %w", err)
	}
	return o, true, nil
}

// Overrides returns every stored override for the given hosts, ordered by
// check_id within each host. Inert is derived from each host's current
// readiness report — a check id the report no longer names.
func (g *Gate) Overrides(ctx context.Context, hostIDs []string) (map[string][]Override, error) {
	out := map[string][]Override{}
	if len(hostIDs) == 0 {
		return out, nil
	}
	rows, err := g.pool.Query(ctx, `
		SELECT o.host_id::text, o.check_id, o.created_by::text, o.created_at, u.username, h.readiness
		FROM host_readiness_overrides o
		JOIN hosts h ON h.id = o.host_id
		LEFT JOIN users u ON u.id = o.created_by
		WHERE o.host_id::text = ANY($1)
		ORDER BY o.host_id, o.check_id
	`, hostIDs)
	if err != nil {
		return nil, fmt.Errorf("query readiness overrides: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var hostID string
		var o Override
		var raw []byte
		if err := rows.Scan(&hostID, &o.CheckID, &o.CreatedBy, &o.CreatedAt, &o.CreatedByUsername, &raw); err != nil {
			return nil, fmt.Errorf("scan readiness override: %w", err)
		}
		o.Inert = !checkPresent(raw, o.CheckID)
		out[hostID] = append(out[hostID], o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate readiness overrides: %w", err)
	}
	return out, nil
}

// checkPresent reports whether id names any entry in report, blocking or not
// — Overrides' Inert must reflect the id being renamed away entirely, not
// merely no longer blocking (e.g. it now passes, which is a lapse, not inert).
func checkPresent(report json.RawMessage, id string) bool {
	inert := readiness.Evaluate(report, []string{id}).Inert
	return len(inert) == 0
}
