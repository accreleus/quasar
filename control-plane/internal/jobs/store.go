package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// lockNamespaceJob is this package's pg_advisory_xact_lock class.
//
// The session scheduler already owns classes 1 (per-user) and 2 (per-GPU)
// (internal/session/scheduler.go). Advisory-lock keys are scoped by the class,
// so a job-id hash colliding with a user-id hash as an int still takes a
// distinct lock. A transaction in this package takes AT MOST ONE lock, in one
// class, so it cannot participate in a lock-order cycle with the scheduler's
// two-lock path.
const lockNamespaceJob = 3

// zeroUUID is the "no host" stand-in used by the job_runs_open_per_target unique
// index. It must match the index expression in migration 0066 exactly: a plain
// partial index on the nullable host_id would not collide instance-scoped rows
// with each other, because NULL is distinct from NULL.
const zeroUUID = "00000000-0000-0000-0000-000000000000"

// Store is the persistence layer for jobs and their runs.
type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// jobColumns is the one SELECT list for a `jobs` row. window_start/window_end are
// rendered as text because Postgres TIME has no natural Go counterpart and the
// wire form ("02:00:00") is what both the API and an operator already speak.
const jobColumns = `
	id, name, description, plane, scope, managed, enabled, schedule_kind,
	interval_secs,
	to_char(window_start, 'HH24:MI:SS'), to_char(window_end, 'HH24:MI:SS'),
	window_days, timezone, history_limit, created_at, updated_at`

func scanJob(row pgx.Row) (Job, error) {
	var (
		j        Job
		interval *int32
		wsRaw    *string
		weRaw    *string
		days     []int16
	)
	err := row.Scan(&j.ID, &j.Name, &j.Description, &j.Plane, &j.Scope, &j.Managed,
		&j.Enabled, &j.Schedule.Kind, &interval, &wsRaw, &weRaw, &days,
		&j.Schedule.Timezone, &j.HistoryLimit, &j.CreatedAt, &j.UpdatedAt)
	if err != nil {
		return Job{}, err
	}
	if interval != nil {
		j.Schedule.IntervalSecs = *interval
	}
	if wsRaw != nil {
		t, perr := ParseTimeOfDay(*wsRaw)
		if perr != nil {
			return Job{}, fmt.Errorf("job %s window_start: %w", j.ID, perr)
		}
		j.Schedule.WindowStart = &t
	}
	if weRaw != nil {
		t, perr := ParseTimeOfDay(*weRaw)
		if perr != nil {
			return Job{}, fmt.Errorf("job %s window_end: %w", j.ID, perr)
		}
		j.Schedule.WindowEnd = &t
	}
	for _, d := range days {
		j.Schedule.WindowDays = append(j.Schedule.WindowDays, int(d))
	}
	return j, nil
}

// SyncDefinitions reconciles the code-owned registry into the table. See
// Registry.Sync for the ownership rules this implements; they are the reason
// this is two statements and not one blanket upsert.
func (s *Store) SyncDefinitions(ctx context.Context, defs []Definition, defaultTimezone string, defaultHistoryLimit int) (SyncResult, error) {
	var res SyncResult
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return res, fmt.Errorf("jobs sync: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if defaultTimezone == "" {
		defaultTimezone = "UTC"
	}
	if defaultHistoryLimit < 1 || defaultHistoryLimit > 500 {
		defaultHistoryLimit = 50
	}
	ids := make([]string, 0, len(defs))
	for _, d := range defs {
		ids = append(ids, d.ID)

		tz := d.Default.Timezone
		if tz == "" {
			tz = defaultTimezone
		}
		var interval *int32
		if d.Default.Kind == KindInterval {
			v := d.Default.IntervalSecs
			interval = &v
		}
		var ws, we *string
		if d.Default.WindowStart != nil {
			a, b := d.Default.WindowStart.String(), d.Default.WindowEnd.String()
			ws, we = &a, &b
		}
		days := make([]int16, 0, len(d.Default.WindowDays))
		for _, w := range d.Default.WindowDays {
			days = append(days, int16(w))
		}

		// ON CONFLICT updates ONLY the identity columns. Every column an admin owns
		// — enabled, schedule_kind, interval_secs, window_*, timezone,
		// history_limit — is absent from the SET list on purpose, so those values
		// are seeded on INSERT and never touched again. See Registry.Sync.
		//
		// The WHERE on the DO UPDATE is what makes the boot line honest: an
		// unchanged deploy reports "added=0 updated=0 removed=0" instead of
		// claiming it rewrote every row. (xmax = 0 is the standard "this tuple was
		// inserted, not updated" test; no rows returned means no change at all.)
		var inserted bool
		err := tx.QueryRow(ctx, `
			INSERT INTO jobs (id, name, description, plane, scope, managed,
			                  schedule_kind, interval_secs, window_start, window_end,
			                  window_days, timezone, history_limit)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::time, $10::time, $11, $12, $13)
			ON CONFLICT (id) DO UPDATE SET
				name        = EXCLUDED.name,
				description = EXCLUDED.description,
				plane       = EXCLUDED.plane,
				scope       = EXCLUDED.scope,
				managed     = EXCLUDED.managed
			WHERE jobs.name        IS DISTINCT FROM EXCLUDED.name
			   OR jobs.description IS DISTINCT FROM EXCLUDED.description
			   OR jobs.plane       IS DISTINCT FROM EXCLUDED.plane
			   OR jobs.scope       IS DISTINCT FROM EXCLUDED.scope
			   OR jobs.managed     IS DISTINCT FROM EXCLUDED.managed
			RETURNING (xmax = 0)
		`, d.ID, d.Name, d.Description, string(d.Plane), string(d.Scope), d.Managed,
			string(d.Default.Kind), interval, ws, we, days, tz, defaultHistoryLimit).Scan(&inserted)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			// Identical row already present: nothing to count.
		case err != nil:
			return res, fmt.Errorf("jobs sync: upsert %s: %w", d.ID, err)
		case inserted:
			res.Added++
		default:
			res.Updated++
		}
	}

	// Rows whose id is no longer registered. CASCADE takes their history with
	// them: a job that no longer exists in code has no meaningful history, and a
	// lingering invisible row is exactly what this framework exists to remove.
	tag, err := tx.Exec(ctx, `DELETE FROM jobs WHERE NOT (id = ANY($1::text[]))`, ids)
	if err != nil {
		return res, fmt.Errorf("jobs sync: prune: %w", err)
	}
	res.Removed = int(tag.RowsAffected())

	if err := tx.Commit(ctx); err != nil {
		return res, fmt.Errorf("jobs sync: commit: %w", err)
	}
	return res, nil
}

// Get returns one job row.
func (s *Store) Get(ctx context.Context, id string) (Job, error) {
	j, err := scanJob(s.pool.QueryRow(ctx, `SELECT`+jobColumns+` FROM jobs WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	if err != nil {
		return Job{}, fmt.Errorf("get job %s: %w", id, err)
	}
	return j, nil
}

// List returns every job row, ordered by id.
func (s *Store) List(ctx context.Context) ([]Job, error) {
	rows, err := s.pool.Query(ctx, `SELECT`+jobColumns+` FROM jobs ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("list jobs: %w", err)
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// ScheduleUpdate is the already-validated column set an admin PATCH applies
// (WP3, design §3.6). All-pointer/flag so "leave this column alone" is
// distinguishable from "set it" — mirroring the repo's other admin PATCH
// structs (internal/session's streamProfileReq, launchProfileReq).
//
// Validation (interval floor, schedule_kind compatibility, IANA zone,
// window-days range, the env-lock check) is the HANDLER's job, not this
// method's: the handler holds the Registry (for EnvOverride) that this
// storage-only type deliberately does not carry. This method only ever
// applies columns a caller already decided are legal.
type ScheduleUpdate struct {
	Enabled      *bool
	IntervalSecs *int32
	// SetWindow distinguishes "the window was not touched" (false) from "the
	// window was touched" (true). When true, a nil WindowStart/WindowEnd CLEARS
	// the window (both columns to NULL, matching the migration's paired CHECK);
	// a non-nil pair sets it.
	SetWindow   bool
	WindowStart *TimeOfDay
	WindowEnd   *TimeOfDay
	// WindowDays is a pointer so an explicitly-empty array ("every day") is
	// distinguishable from "not touched". A nil *[]int leaves the column alone.
	WindowDays   *[]int
	Timezone     *string
	HistoryLimit *int32
}

// UpdateSchedule applies an admin's PATCH to the admin-owned half of a job row
// and returns the row as it now stands. An empty update (every field nil,
// SetWindow false) is a legal no-op that just re-reads the row — a PATCH with
// an empty body is not an error.
func (s *Store) UpdateSchedule(ctx context.Context, jobID string, u ScheduleUpdate) (Job, error) {
	var sets []string
	var args []any
	n := 1
	add := func(clause string, val any) {
		sets = append(sets, fmt.Sprintf(clause, n))
		args = append(args, val)
		n++
	}
	if u.Enabled != nil {
		add("enabled = $%d", *u.Enabled)
	}
	if u.IntervalSecs != nil {
		add("interval_secs = $%d", *u.IntervalSecs)
	}
	if u.SetWindow {
		if u.WindowStart != nil && u.WindowEnd != nil {
			add("window_start = $%d::time", u.WindowStart.String())
			add("window_end = $%d::time", u.WindowEnd.String())
		} else {
			add("window_start = $%d::time", nil)
			add("window_end = $%d::time", nil)
		}
	}
	if u.WindowDays != nil {
		days := make([]int16, 0, len(*u.WindowDays))
		for _, d := range *u.WindowDays {
			days = append(days, int16(d))
		}
		add("window_days = $%d", days)
	}
	if u.Timezone != nil {
		add("timezone = $%d", *u.Timezone)
	}
	if u.HistoryLimit != nil {
		add("history_limit = $%d", *u.HistoryLimit)
	}
	if len(sets) == 0 {
		return s.Get(ctx, jobID)
	}
	sets = append(sets, "updated_at = now()")
	args = append(args, jobID)
	query := fmt.Sprintf(`UPDATE jobs SET %s WHERE id = $%d RETURNING`+jobColumns,
		strings.Join(sets, ", "), n)
	j, err := scanJob(s.pool.QueryRow(ctx, query, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	if err != nil {
		return Job{}, fmt.Errorf("update schedule %s: %w", jobID, err)
	}
	return j, nil
}

// HostIDs enumerates the targets of a host-scoped job.
//
// EVERY host, not just the online ones. A pending row for an offline host is not
// waste: it is claimed the moment the agent reconnects, which is precisely the
// durability the in-memory schedulers this replaces could not offer.
func (s *Store) HostIDs(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT id::text FROM hosts ORDER BY node_name`)
	if err != nil {
		return nil, fmt.Errorf("list hosts: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

const runColumns = `
	id::text, job_id, COALESCE(host_id::text, ''), state, trigger,
	COALESCE(actor_user_id::text, ''), attempt, scheduled_for, claimed_at,
	started_at, finished_at, params, summary, COALESCE(error, ''), created_at`

func scanRun(row pgx.Row) (Run, error) {
	var r Run
	err := row.Scan(&r.ID, &r.JobID, &r.HostID, &r.State, &r.Trigger, &r.ActorUserID,
		&r.Attempt, &r.ScheduledFor, &r.ClaimedAt, &r.StartedAt, &r.FinishedAt,
		&r.Params, &r.Summary, &r.Error, &r.CreatedAt)
	return r, err
}

// MaterializeParams describes the run to create.
type MaterializeParams struct {
	JobID        string
	HostID       string // "" for an instance-scoped job
	Trigger      Trigger
	ActorUserID  string
	ScheduledFor time.Time
	Attempt      int
	Params       any
}

// Materialization is which of the three things Materialize did with the
// (job, target) single-flight slot. Callers log on it; nothing branches on it
// for correctness.
type Materialization string

const (
	RunCreated Materialization = "created"
	// An open pending run's scheduled_for moved earlier.
	RunPulledForward Materialization = "pulled_forward"
	// An open run was already due, or already running.
	RunCoalesced Materialization = "coalesced"
)

// Materialize creates the next pending run for (job, target), or returns the
// run already open (check its State to tell queued from in-progress).
//
// Single-flight is a storage invariant: the partial unique index
// job_runs_open_per_target makes a second open run impossible, and the
// advisory lock makes the ordinary read-then-insert avoid the violation rather
// than recover from it — either alone is insufficient. A manual OR an event
// trigger pulls an open pending row forward (both mean "now", not "twice"); a
// schedule trigger never moves a row it just found.
func (s *Store) Materialize(ctx context.Context, p MaterializeParams) (Run, Materialization, error) {
	if p.Attempt < 1 {
		p.Attempt = 1
	}
	params, err := marshalBounded(p.Params, "params")
	if err != nil {
		return Run{}, "", err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Run{}, "", fmt.Errorf("materialize: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1, hashtext($2))`,
		lockNamespaceJob, p.JobID); err != nil {
		return Run{}, "", fmt.Errorf("materialize: lock %s: %w", p.JobID, err)
	}

	job, err := scanJob(tx.QueryRow(ctx, `SELECT`+jobColumns+` FROM jobs WHERE id = $1`, p.JobID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, "", ErrNotFound
	}
	if err != nil {
		return Run{}, "", fmt.Errorf("materialize: read job %s: %w", p.JobID, err)
	}
	// The jobs.scope <-> job_runs.host_id agreement. No row-level CHECK can span
	// two tables, so it is enforced here, on the one path every insert takes.
	if job.Scope == ScopeHost && p.HostID == "" {
		return Run{}, "", ErrHostRequired
	}
	if job.Scope == ScopeInstance && p.HostID != "" {
		return Run{}, "", ErrHostNotAllowed
	}
	if !job.Managed {
		return Run{}, "", ErrUnmanaged
	}
	if !job.Enabled {
		// A disabled job never runs, not even manually. Disabling is the operator's
		// kill switch and it has to mean it.
		return Run{}, "", ErrDisabled
	}

	existing, found, err := openRunTx(ctx, tx, p.JobID, p.HostID)
	if err != nil {
		return Run{}, "", err
	}
	if found {
		// attempt and params are never rewritten here. Keeping attempt is what
		// stops a trigger that pulls a deferral retry forward from also resetting
		// the backoff ladder: it skips one wait, it does not start over.
		earlier := existing.ScheduledFor.After(p.ScheduledFor)
		if existing.State == StatePending && p.Trigger == TriggerManual {
			pulled, err := scanRun(tx.QueryRow(ctx, `
				UPDATE job_runs
				SET scheduled_for = LEAST(scheduled_for, $2),
				    trigger       = 'manual',
				    actor_user_id = NULLIF($3, '')::uuid
				WHERE id = $1::uuid
				RETURNING`+runColumns, existing.ID, p.ScheduledFor, p.ActorUserID))
			if err != nil {
				return Run{}, "", fmt.Errorf("materialize: pull forward: %w", err)
			}
			if err := tx.Commit(ctx); err != nil {
				return Run{}, "", fmt.Errorf("materialize: commit: %w", err)
			}
			m := RunCoalesced
			if earlier {
				m = RunPulledForward
			}
			return pulled, m, nil
		}
		// The event trigger's half (#92): coalescing an event onto a run the
		// schedule dated hours out made the trigger a no-op — the reap waited for
		// the interval it was meant to skip. A pending manual run keeps its
		// trigger and actor: the event moved the clock, not the reason.
		if existing.State == StatePending && p.Trigger == TriggerEvent && earlier {
			pulled, err := scanRun(tx.QueryRow(ctx, `
				UPDATE job_runs
				SET scheduled_for = $2,
				    trigger = CASE WHEN trigger = 'manual' THEN trigger ELSE 'event' END
				WHERE id = $1::uuid
				RETURNING`+runColumns, existing.ID, p.ScheduledFor))
			if err != nil {
				return Run{}, "", fmt.Errorf("materialize: pull forward: %w", err)
			}
			if err := tx.Commit(ctx); err != nil {
				return Run{}, "", fmt.Errorf("materialize: commit: %w", err)
			}
			return pulled, RunPulledForward, nil
		}
		if err := tx.Commit(ctx); err != nil {
			return Run{}, "", fmt.Errorf("materialize: commit: %w", err)
		}
		return existing, RunCoalesced, nil
	}

	run, err := scanRun(tx.QueryRow(ctx, `
		INSERT INTO job_runs (job_id, host_id, state, trigger, actor_user_id,
		                      attempt, scheduled_for, params)
		VALUES ($1, NULLIF($2, '')::uuid, 'pending', $3, NULLIF($4, '')::uuid, $5, $6, $7)
		RETURNING`+runColumns,
		p.JobID, p.HostID, string(p.Trigger), p.ActorUserID, p.Attempt, p.ScheduledFor, params))
	if err != nil {
		// A violation means something outside this function's lock opened a run
		// first — a legitimate outcome, not a failure.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			_ = tx.Rollback(ctx)
			cur, found, rerr := s.OpenRun(ctx, p.JobID, p.HostID)
			if rerr != nil {
				return Run{}, "", rerr
			}
			if found {
				return cur, RunCoalesced, nil
			}
		}
		return Run{}, "", fmt.Errorf("materialize: insert: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Run{}, "", fmt.Errorf("materialize: commit: %w", err)
	}
	return run, RunCreated, nil
}

func openRunTx(ctx context.Context, tx pgx.Tx, jobID, hostID string) (Run, bool, error) {
	r, err := scanRun(tx.QueryRow(ctx, `
		SELECT`+runColumns+` FROM job_runs
		WHERE job_id = $1
		  AND COALESCE(host_id, '`+zeroUUID+`'::uuid) = COALESCE(NULLIF($2, '')::uuid, '`+zeroUUID+`'::uuid)
		  AND state IN ('pending', 'running')
		FOR UPDATE`, jobID, hostID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, false, nil
	}
	if err != nil {
		return Run{}, false, fmt.Errorf("open run %s: %w", jobID, err)
	}
	return r, true, nil
}

// OpenRun returns the pending-or-running run for (job, target), if any.
func (s *Store) OpenRun(ctx context.Context, jobID, hostID string) (Run, bool, error) {
	r, err := scanRun(s.pool.QueryRow(ctx, `
		SELECT`+runColumns+` FROM job_runs
		WHERE job_id = $1
		  AND COALESCE(host_id, '`+zeroUUID+`'::uuid) = COALESCE(NULLIF($2, '')::uuid, '`+zeroUUID+`'::uuid)
		  AND state IN ('pending', 'running')`, jobID, hostID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, false, nil
	}
	if err != nil {
		return Run{}, false, fmt.Errorf("open run %s: %w", jobID, err)
	}
	return r, true, nil
}

// HostNames enumerates every registered host as id -> node_name, for the admin
// viewer's per-host target rows on a host-scoped job (WP3). A sibling of
// HostIDs (used by the dispatcher, which only ever needs the ids) rather than
// a change to it, so the dispatcher's hot path stays a one-column query.
func (s *Store) HostNames(ctx context.Context) (map[string]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT id::text, node_name FROM hosts ORDER BY node_name`)
	if err != nil {
		return nil, fmt.Errorf("list host names: %w", err)
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = name
	}
	return out, rows.Err()
}

// GetRun returns one run by id. The agent report endpoint needs it before
// writing: the claiming-host identity check cannot fold into the UPDATE
// without making "not your run" (401) and "already finished" (200)
// indistinguishable.
func (s *Store) GetRun(ctx context.Context, runID string) (Run, error) {
	r, err := scanRun(s.pool.QueryRow(ctx,
		`SELECT`+runColumns+` FROM job_runs WHERE id = $1::uuid`, runID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	if err != nil {
		// A malformed uuid is a 22P02 from the cast, not a missing row. It is still
		// "no such run" from the caller's point of view, and reporting it as
		// anything else would let a caller distinguish a well-formed id it does not
		// own from a garbage one.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "22P02" {
			return Run{}, ErrNotFound
		}
		return Run{}, fmt.Errorf("get run %s: %w", runID, err)
	}
	return r, nil
}

// LastTerminalRun returns the most recently FINISHED run for (job, target). It is
// what the interval is measured from — the END of the previous pass.
func (s *Store) LastTerminalRun(ctx context.Context, jobID, hostID string) (Run, bool, error) {
	r, err := scanRun(s.pool.QueryRow(ctx, `
		SELECT`+runColumns+` FROM job_runs
		WHERE job_id = $1
		  AND COALESCE(host_id, '`+zeroUUID+`'::uuid) = COALESCE(NULLIF($2, '')::uuid, '`+zeroUUID+`'::uuid)
		  AND state NOT IN ('pending', 'running')
		ORDER BY finished_at DESC NULLS LAST, created_at DESC
		LIMIT 1`, jobID, hostID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, false, nil
	}
	if err != nil {
		return Run{}, false, fmt.Errorf("last run %s: %w", jobID, err)
	}
	return r, true, nil
}

// LastRunInState returns the most recently FINISHED run for (job, target) that
// ended in state. The release view reads it twice — the last SUCCEEDED run is
// checked_at, the last FAILED one is last_error — which "the last run" alone
// cannot answer, the two being independent facts.
func (s *Store) LastRunInState(ctx context.Context, jobID, hostID string, state State) (Run, bool, error) {
	r, err := scanRun(s.pool.QueryRow(ctx, `
		SELECT`+runColumns+` FROM job_runs
		WHERE job_id = $1
		  AND COALESCE(host_id, '`+zeroUUID+`'::uuid) = COALESCE(NULLIF($2, '')::uuid, '`+zeroUUID+`'::uuid)
		  AND state = $3
		ORDER BY finished_at DESC NULLS LAST, created_at DESC
		LIMIT 1`, jobID, hostID, string(state)))
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, false, nil
	}
	if err != nil {
		return Run{}, false, fmt.Errorf("last %s run %s: %w", state, jobID, err)
	}
	return r, true, nil
}

// ListRuns returns a job's run history, newest first.
func (s *Store) ListRuns(ctx context.Context, jobID, hostID string, limit int) ([]Run, error) {
	if limit < 1 || limit > 500 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
		SELECT`+runColumns+` FROM job_runs
		WHERE job_id = $1
		  AND ($2 = '' OR host_id = $2::uuid)
		ORDER BY created_at DESC
		LIMIT $3`, jobID, hostID, limit)
	if err != nil {
		return nil, fmt.Errorf("list runs %s: %w", jobID, err)
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListRunsPage is ListRuns with offset-cursor pagination for GET
// /v1/admin/jobs/{id}/runs. The cursor is an opaque decimal offset string,
// the session.Store.ListAll convention.
func (s *Store) ListRunsPage(ctx context.Context, jobID, hostID, cursor string, limit int) ([]Run, string, error) {
	if limit < 1 || limit > 500 {
		limit = 50
	}
	var offset int64
	if cursor != "" {
		fmt.Sscanf(cursor, "%d", &offset)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT`+runColumns+` FROM job_runs
		WHERE job_id = $1
		  AND ($2 = '' OR host_id = $2::uuid)
		ORDER BY created_at DESC
		LIMIT $3 OFFSET $4`, jobID, hostID, limit+1, offset)
	if err != nil {
		return nil, "", fmt.Errorf("list runs page %s: %w", jobID, err)
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	var next string
	if len(out) > limit {
		out = out[:limit]
		next = fmt.Sprintf("%d", offset+int64(limit))
	}
	return out, next, nil
}

// ClaimOptions selects which due runs to claim.
type ClaimOptions struct {
	// Plane restricts the claim to one side of the split. A control-plane
	// dispatcher must never claim a run only a host can execute, and vice versa.
	Plane Plane
	// HostID restricts an agent claim to that host's runs. Required for
	// PlaneAgent; ignored for PlaneControl.
	HostID string
	Now    time.Time
	Limit  int
}

// ClaimDue atomically moves due pending runs to `running` and returns them.
//
// FOR UPDATE ... SKIP LOCKED is what makes two dispatcher instances (or two
// agent polls arriving together) claim DISJOINT sets rather than fighting: a row
// another transaction is already claiming is skipped, not waited on. It is the
// same mechanism the session scheduler uses, for the same reason.
func (s *Store) ClaimDue(ctx context.Context, o ClaimOptions) ([]Run, error) {
	if o.Limit < 1 {
		o.Limit = 5
	}
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	if o.Plane == PlaneAgent && o.HostID == "" {
		return nil, ErrHostRequired
	}
	rows, err := s.pool.Query(ctx, `
		WITH due AS (
			SELECT r.id
			FROM job_runs r
			JOIN jobs j ON j.id = r.job_id
			WHERE r.state = 'pending'
			  AND r.scheduled_for <= $1
			  AND j.enabled AND j.managed
			  AND j.plane = $2
			  AND ($3 = '' OR r.host_id = $3::uuid)
			ORDER BY r.scheduled_for
			LIMIT $4
			FOR UPDATE OF r SKIP LOCKED
		)
		UPDATE job_runs SET state = 'running', claimed_at = now(), started_at = now()
		WHERE id IN (SELECT id FROM due)
		RETURNING`+runColumns, o.Now, string(o.Plane), o.HostID, o.Limit)
	if err != nil {
		return nil, fmt.Errorf("claim due runs: %w", err)
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Report closes a running run.
//
// A report for an ALREADY-TERMINAL run is a no-op that returns applied=false and
// no error, so an agent retrying after a network blip is safe: the alternative —
// a 409 the agent cannot act on — would turn a successful run into a permanent
// error in the operator's face.
func (s *Store) Report(ctx context.Context, runID string, state State, summary any, errText string) (Run, bool, error) {
	if !state.Terminal() {
		return Run{}, false, fmt.Errorf("jobs: %q is not a terminal state", state)
	}
	sum, err := marshalBounded(summary, "summary")
	if err != nil {
		return Run{}, false, err
	}
	r, err := scanRun(s.pool.QueryRow(ctx, `
		UPDATE job_runs
		SET state = $2, finished_at = now(), summary = $3, error = NULLIF($4, ''),
		    started_at = COALESCE(started_at, claimed_at, now())
		WHERE id = $1::uuid AND state = 'running'
		RETURNING`+runColumns, runID, string(state), sum, errText))
	if err == nil {
		return r, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Run{}, false, fmt.Errorf("report run %s: %w", runID, err)
	}
	cur, err := scanRun(s.pool.QueryRow(ctx, `SELECT`+runColumns+` FROM job_runs WHERE id = $1::uuid`, runID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, false, ErrNotFound
	}
	if err != nil {
		return Run{}, false, fmt.Errorf("report run %s: %w", runID, err)
	}
	if cur.State.Terminal() {
		return cur, false, nil
	}
	// Pending: a report for a run nobody claimed. Refuse rather than close it —
	// something is confused about which run it is holding.
	return cur, false, fmt.Errorf("jobs: run %s is %s, not running", runID, cur.State)
}

// ReapAbandoned aborts runs claimed longer than timeout ago with no report, and
// returns them so the caller can re-materialize.
//
// This is the library janitor's "returned abandoned scans to pending" behaviour,
// generalized. Without it an agent that dies mid-run leaves a `running` row
// forever, and the single-flight index then blocks that job on that host for
// good — the failure mode is not "one lost run", it is "this job never runs
// again here".
func (s *Store) ReapAbandoned(ctx context.Context, timeout time.Duration) ([]Run, error) {
	if timeout <= 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `
		UPDATE job_runs
		SET state = 'aborted', finished_at = now(),
		    error = 'claim timed out with no report'
		WHERE state = 'running'
		  AND claimed_at IS NOT NULL
		  AND claimed_at < now() - make_interval(secs => $1)
		RETURNING`+runColumns, timeout.Seconds())
	if err != nil {
		return nil, fmt.Errorf("reap abandoned runs: %w", err)
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DefaultReclaimReason is the fallback error text when the caller supplied
// none. session.Coordinator.AgentReconnected passes its own jobReclaimReason
// with the same words — a matched pair, kept as separate literals because the
// JobReclaimer seam exists so session does not import this package.
const DefaultReclaimReason = "agent restarted"

// ReclaimHostRuns aborts every running agent-plane run claimed by hostID.
//
// A node-agent rebuilds its world on every connection, so a run it was
// executing when its process died has no live executor — stale by definition
// at re-register. Waiting out QUASAR_JOBS_CLAIM_TIMEOUT_SECS instead leaves
// job_runs_open_per_target holding the single-flight slot: a 409 on every
// "Run now" and no scheduled run on that host for an hour (#492).
//
// plane='agent' is the load-bearing predicate: a host-scoped control-plane run
// executes in this process and aborting it would kill a live run. pending rows
// are untouched — the reconnecting agent claims them on its next poll. A bulk
// UPDATE like ReapAbandoned: the framework's verdict on a claim nobody can
// report.
func (s *Store) ReclaimHostRuns(ctx context.Context, hostID, reason string) ([]Run, error) {
	if strings.TrimSpace(hostID) == "" {
		return nil, ErrHostRequired
	}
	if strings.TrimSpace(reason) == "" {
		reason = DefaultReclaimReason
	}
	rows, err := s.pool.Query(ctx, `
		UPDATE job_runs
		SET state = 'aborted', finished_at = now(), error = $2
		WHERE state = 'running'
		  AND host_id = $1::uuid
		  AND job_id IN (SELECT id FROM jobs WHERE plane = 'agent')
		RETURNING`+runColumns, hostID, reason)
	if err != nil {
		return nil, fmt.Errorf("reclaim host runs %s: %w", hostID, err)
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PruneRuns applies both retention rules: the per-(job, target) row cap from
// jobs.history_limit, and an age cap in days (0 disables the age rule).
//
// Deliberately NOT the prune-inline-per-write pattern PruneSessionMetrics uses:
// job runs are low-volume (tens per day, fleet-wide), so a periodic prune keeps
// the write path trivial. Only terminal rows are eligible — pruning a pending
// row would delete the next run.
func (s *Store) PruneRuns(ctx context.Context, maxAgeDays int) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		WITH ranked AS (
			SELECT r.id,
			       row_number() OVER (
			           PARTITION BY r.job_id, COALESCE(r.host_id, '`+zeroUUID+`'::uuid)
			           ORDER BY r.created_at DESC, r.id DESC
			       ) AS rn,
			       j.history_limit,
			       r.created_at
			FROM job_runs r
			JOIN jobs j ON j.id = r.job_id
			WHERE r.state NOT IN ('pending', 'running')
		)
		DELETE FROM job_runs
		WHERE id IN (
			SELECT id FROM ranked
			WHERE rn > history_limit
			   OR ($1 > 0 AND created_at < now() - make_interval(days => $1))
		)`, maxAgeDays)
	if err != nil {
		return 0, fmt.Errorf("prune job runs: %w", err)
	}
	return tag.RowsAffected(), nil
}

// summaryReason digs the operator-readable phrase back out of a stored summary,
// for the deferred/skipped log lines.
func summaryReason(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	if v, ok := m["reason"].(string); ok {
		return v
	}
	return ""
}
