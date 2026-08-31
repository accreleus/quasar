package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned by Capture when the requested capture has not landed
// yet. Callers translate it into their own 404 — for a capture that 404 is the
// POLL SIGNAL, not an error.
var ErrNotFound = errors.New("telemetry: not found")

// postgres is the production Store: plain Postgres, no extension, on the schema
// migrations 0003 (session_metrics) and 0016 (session_trace_events +
// session_trace_clock) already define.
type postgres struct {
	pool *pgxpool.Pool
}

// Postgres builds the production telemetry store over an existing pool.
func Postgres(pool *pgxpool.Pool) Store { return &postgres{pool: pool} }

// --- appends -----------------------------------------------------------------

func (s *postgres) Append(ctx context.Context, sessionID, source string, in SampleInput) error {
	_, err := s.pool.Exec(ctx, insertSampleSQL,
		sessionID, source, in.TsUnixMs, []byte(defaultJSON(in.Metrics)))
	if err != nil {
		return fmt.Errorf("insert metric: %w", err)
	}
	return nil
}

const insertSampleSQL = `
	INSERT INTO session_metrics (session_id, source, ts_unix_ms, metrics)
	VALUES ($1::uuid, $2, $3, $4)`

func (s *postgres) AppendBatch(ctx context.Context, sessionID, source string, samples []SampleInput) error {
	if len(samples) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, m := range samples {
		batch.Queue(insertSampleSQL, sessionID, source, m.TsUnixMs, []byte(defaultJSON(m.Metrics)))
	}
	return drain(ctx, s.pool, batch, len(samples), "insert metrics batch")
}

const insertEventSQL = `
	INSERT INTO session_trace_events (session_id, source, ts_unix_ms, type, payload)
	VALUES ($1::uuid, $2, $3, $4, $5)`

func (s *postgres) AppendEvent(ctx context.Context, sessionID, source string, e EventInput) error {
	_, err := s.pool.Exec(ctx, insertEventSQL,
		sessionID, source, e.TsUnixMs, e.Type, []byte(defaultJSON(e.Payload)))
	if err != nil {
		return fmt.Errorf("insert trace event: %w", err)
	}
	return nil
}

func (s *postgres) AppendEventReturningID(ctx context.Context, sessionID, source string, e EventInput) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, insertEventSQL+" RETURNING id::text",
		sessionID, source, e.TsUnixMs, e.Type, []byte(defaultJSON(e.Payload))).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("insert trace event returning id: %w", err)
	}
	return id, nil
}

func (s *postgres) AppendEvents(ctx context.Context, sessionID, source string, events []EventInput) error {
	if len(events) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, e := range events {
		batch.Queue(insertEventSQL, sessionID, source, e.TsUnixMs, e.Type, []byte(defaultJSON(e.Payload)))
	}
	return drain(ctx, s.pool, batch, len(events), "insert trace events batch")
}

// drain executes a pgx.Batch in one round-trip and reports which row failed.
func drain(ctx context.Context, pool *pgxpool.Pool, batch *pgx.Batch, n int, what string) error {
	br := pool.SendBatch(ctx, batch)
	defer br.Close()
	for i := 0; i < n; i++ {
		if _, err := br.Exec(); err != nil {
			return fmt.Errorf("%s (row %d/%d): %w", what, i+1, n, err)
		}
	}
	return nil
}

// --- clock -------------------------------------------------------------------

func (s *postgres) UpsertClock(ctx context.Context, sessionID string, clientOffsetMs, uncertaintyMs float64) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO session_trace_clock (session_id, client_offset_ms, uncertainty_ms, measured_at, updated_at)
		VALUES ($1::uuid, $2, $3, now(), now())
		ON CONFLICT (session_id) DO UPDATE
		   SET client_offset_ms = EXCLUDED.client_offset_ms,
		       uncertainty_ms   = EXCLUDED.uncertainty_ms,
		       measured_at      = EXCLUDED.measured_at,
		       updated_at       = now()
	`, sessionID, clientOffsetMs, uncertaintyMs)
	if err != nil {
		return fmt.Errorf("upsert trace clock: %w", err)
	}
	return nil
}

func (s *postgres) Clock(ctx context.Context, sessionID string) (*Clock, error) {
	var c Clock
	err := s.pool.QueryRow(ctx, `
		SELECT client_offset_ms, uncertainty_ms, measured_at, updated_at
		FROM session_trace_clock
		WHERE session_id = $1::uuid
	`, sessionID).Scan(&c.ClientOffsetMs, &c.UncertaintyMs, &c.MeasuredAt, &c.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil // UNMEASURED — never a synthesized offset-0 default
	}
	if err != nil {
		return nil, fmt.Errorf("get trace clock: %w", err)
	}
	return &c, nil
}

// --- reads -------------------------------------------------------------------

// nullableBound renders an open (zero) window bound as SQL NULL, so every
// windowed read stays a single statement rather than four query variants.
func nullableBound(v int64) any {
	if v > 0 {
		return v
	}
	return nil
}

func (s *postgres) Window(ctx context.Context, sessionID string, r Range, f Filter) (Slice, error) {
	var out Slice

	samples, err := s.samplesInWindow(ctx, sessionID, r, f.Limit)
	if err != nil {
		return Slice{}, err
	}
	out.Samples = samples

	events, err := s.Events(ctx, sessionID, r, f)
	if err != nil {
		return Slice{}, err
	}
	out.Events = events

	clock, err := s.Clock(ctx, sessionID)
	if err != nil {
		return Slice{}, err
	}
	out.Clock = clock

	return out, nil
}

func (s *postgres) samplesInWindow(ctx context.Context, sessionID string, r Range, limit int32) ([]Sample, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT source, ts_unix_ms, metrics
		FROM session_metrics
		WHERE session_id = $1::uuid
		  AND ($2::bigint IS NULL OR ts_unix_ms >= $2::bigint)
		  AND ($3::bigint IS NULL OR ts_unix_ms <= $3::bigint)
		ORDER BY ts_unix_ms DESC, created_at DESC
		LIMIT $4
	`, sessionID, nullableBound(r.FromMs), nullableBound(r.ToMs), clampLimit(limit))
	if err != nil {
		return nil, fmt.Errorf("query metrics window: %w", err)
	}
	return scanSamples(rows, "metric window")
}

func (s *postgres) Events(ctx context.Context, sessionID string, r Range, f Filter) ([]Event, error) {
	// types is a nullable text[] so the type filter stays a single statement.
	var typesArg any
	if len(f.Types) > 0 {
		typesArg = f.Types
	}
	rows, err := s.pool.Query(ctx, `
		SELECT source, ts_unix_ms, type, payload
		FROM session_trace_events
		WHERE session_id = $1::uuid
		  AND ($2::bigint IS NULL OR ts_unix_ms >= $2::bigint)
		  AND ($3::bigint IS NULL OR ts_unix_ms <= $3::bigint)
		  AND ($4::text[] IS NULL OR type = ANY($4::text[]))
		ORDER BY ts_unix_ms DESC, created_at DESC
		LIMIT $5
	`, sessionID, nullableBound(r.FromMs), nullableBound(r.ToMs), typesArg, clampLimit(f.Limit))
	if err != nil {
		return nil, fmt.Errorf("query trace events: %w", err)
	}
	return scanEvents(rows, "trace event")
}

func (s *postgres) Recent(ctx context.Context, sessionID string, limit int32, source *string, cursor string) ([]Sample, string, error) {
	limit = clampLimit(limit)
	var offset int64
	if cursor != "" {
		fmt.Sscanf(cursor, "%d", &offset)
	}
	// The source filter is a nullable parameter so the plan stays one statement.
	var srcArg any
	if source != nil {
		srcArg = *source
	}
	rows, err := s.pool.Query(ctx, `
		SELECT source, ts_unix_ms, metrics
		FROM session_metrics
		WHERE session_id = $1::uuid
		  AND ($2::text IS NULL OR source = $2::text)
		ORDER BY ts_unix_ms DESC, created_at DESC
		LIMIT $3 OFFSET $4
	`, sessionID, srcArg, limit+1, offset)
	if err != nil {
		return nil, "", fmt.Errorf("query metrics: %w", err)
	}
	out, err := scanSamples(rows, "metric")
	if err != nil {
		return nil, "", err
	}
	var next string
	if int32(len(out)) > limit {
		out = out[:limit]
		next = fmt.Sprintf("%d", offset+int64(limit))
	}
	return out, next, nil
}

func (s *postgres) LatestPerSession(ctx context.Context, sessionIDs []string) (map[string]Latest, error) {
	out := make(map[string]Latest)
	if len(sessionIDs) == 0 {
		return out, nil
	}
	// DISTINCT ON picks the newest row per (session, source) in one pass — no
	// N+1. No created_at tiebreak (#148): it is not in the index, so including
	// it forced a full re-sort per admin list load; ts_unix_ms is the
	// authoritative sample clock.
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT ON (session_id, source)
		       session_id::text, source, ts_unix_ms, metrics
		FROM session_metrics
		WHERE session_id = ANY($1::uuid[])
		ORDER BY session_id, source, ts_unix_ms DESC
	`, sessionIDs)
	if err != nil {
		return nil, fmt.Errorf("query latest metrics: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var sid string
		var m Sample
		var raw []byte
		if err := rows.Scan(&sid, &m.Source, &m.TsUnixMs, &raw); err != nil {
			return nil, fmt.Errorf("scan latest metric: %w", err)
		}
		m.Metrics = json.RawMessage(raw)
		l := out[sid]
		sample := m
		switch m.Source {
		case SourceAgent:
			l.Agent = &sample
		case SourceBrowser:
			l.Browser = &sample
		}
		out[sid] = l
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate latest metrics: %w", err)
	}
	return out, nil
}

// --- captures ----------------------------------------------------------------

// maxCapturesPerSession bounds the capture read, which is not window-bounded
// like other trace reads: captures are sparse, byte-capped and
// retention-exempt, and the bundle's 5-minute window would reliably hide the
// artifact whose purpose is to be looked at later.
const maxCapturesPerSession = 200

func (s *postgres) Captures(ctx context.Context, sessionID string) ([]Event, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT source, ts_unix_ms, type, payload
		FROM session_trace_events
		WHERE session_id = $1::uuid AND type LIKE $2
		ORDER BY ts_unix_ms DESC, created_at DESC
		LIMIT $3
	`, sessionID, capturePattern, maxCapturesPerSession)
	if err != nil {
		return nil, fmt.Errorf("query session captures: %w", err)
	}
	return scanEvents(rows, "session capture")
}

func (s *postgres) Capture(ctx context.Context, sessionID, captureID string) (Event, error) {
	var e Event
	var raw []byte
	err := s.pool.QueryRow(ctx, `
		SELECT source, ts_unix_ms, type, payload
		FROM session_trace_events
		WHERE session_id = $1::uuid AND type LIKE $2
		  AND payload->>'capture_id' = $3
		ORDER BY ts_unix_ms DESC, created_at DESC
		LIMIT 1
	`, sessionID, capturePattern, captureID).Scan(&e.Source, &e.TsUnixMs, &e.Type, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return Event{}, ErrNotFound
	}
	if err != nil {
		return Event{}, fmt.Errorf("query capture: %w", err)
	}
	e.Payload = json.RawMessage(raw)
	return e, nil
}

// --- retention ---------------------------------------------------------------

// Retain applies Policy across every session in one pass — the only thing in
// this system that deletes telemetry. Every DELETE is `WHERE ctid IN
// (SELECT … LIMIT $batch)` looped until empty, so no statement holds a long
// lock on a table the ingest path writes. There is deliberately no
// created_at index: the rolling trim drives from the small live-session set,
// the post-mortem sweep carries a redundant created_at predicate so the
// planner discards cheaply, and the tables are bounded by this very policy.
// If a fleet makes this hot, the fix is one index migration.
func (s *postgres) Retain(ctx context.Context, p Policy) (Report, error) {
	p = p.normalized()
	started := time.Now()
	var rep Report

	rollingCutoff := started.Add(-p.Rolling)
	postMortemCutoff := started.Add(-p.PostMortem)

	steps := []struct {
		sql   string
		args  []any
		count *int64
	}{
		{rollingSamplesSQL, []any{rollingCutoff, p.Batch}, &rep.RollingSamples},
		{rollingEventsSQL, []any{rollingCutoff, capturePattern, p.Batch}, &rep.RollingEvents},
		{postMortemSamplesSQL, []any{postMortemCutoff, p.Batch}, &rep.PostMortemSamples},
		{postMortemEventsSQL, []any{postMortemCutoff, capturePattern, p.Batch}, &rep.PostMortemEvents},
		{postMortemClocksSQL, []any{postMortemCutoff, p.Batch}, &rep.PostMortemClocks},
	}
	for _, step := range steps {
		n, truncated, err := s.deleteInBatches(ctx, step.sql, step.args)
		*step.count += n
		rep.Truncated = rep.Truncated || truncated
		if err != nil {
			rep.Duration = time.Since(started)
			return rep, err
		}
	}
	rep.Duration = time.Since(started)
	return rep, nil
}

// deleteInBatches runs one batched DELETE until it removes no more rows, the
// iteration cap is hit, or the context is done. The last argument of sql must be
// the batch limit.
func (s *postgres) deleteInBatches(ctx context.Context, sql string, args []any) (int64, bool, error) {
	var total int64
	for i := 0; i < maxBatchIterations; i++ {
		if err := ctx.Err(); err != nil {
			return total, true, err
		}
		tag, err := s.pool.Exec(ctx, sql, args...)
		if err != nil {
			return total, false, fmt.Errorf("telemetry retain: %w", err)
		}
		n := tag.RowsAffected()
		total += n
		if n == 0 {
			return total, false, nil
		}
	}
	// Still deleting when the cap hit: a backlog exists, the next pass continues.
	return total, true, nil
}

// terminalStates is the sessions.state set that freezes a session's telemetry.
// It matches session.State.IsTerminal(); the two are kept in step by
// TestTerminalStatesMatchSessionPackage in the session package.
const terminalStates = `('stopped','failed')`

// terminalAt is a terminal session's clock: ended_at when the lifecycle stamped
// one, updated_at otherwise (a session forced terminal without an ended_at must
// still age out rather than live forever).
const terminalAt = `COALESCE(s.ended_at, s.updated_at)`

const rollingSamplesSQL = `
	DELETE FROM session_metrics
	WHERE ctid IN (
	    SELECT m.ctid
	    FROM session_metrics m
	    JOIN sessions s ON s.id = m.session_id
	    WHERE s.state NOT IN ` + terminalStates + `
	      AND m.created_at < $1
	    LIMIT $2
	)`

const rollingEventsSQL = `
	DELETE FROM session_trace_events
	WHERE ctid IN (
	    SELECT e.ctid
	    FROM session_trace_events e
	    JOIN sessions s ON s.id = e.session_id
	    WHERE s.state NOT IN ` + terminalStates + `
	      AND e.created_at < $1
	      AND e.type NOT LIKE $2
	    LIMIT $3
	)`

const postMortemSamplesSQL = `
	DELETE FROM session_metrics
	WHERE ctid IN (
	    SELECT m.ctid
	    FROM session_metrics m
	    JOIN sessions s ON s.id = m.session_id
	    WHERE s.state IN ` + terminalStates + `
	      AND ` + terminalAt + ` < $1
	      AND m.created_at < $1
	    LIMIT $2
	)`

const postMortemEventsSQL = `
	DELETE FROM session_trace_events
	WHERE ctid IN (
	    SELECT e.ctid
	    FROM session_trace_events e
	    JOIN sessions s ON s.id = e.session_id
	    WHERE s.state IN ` + terminalStates + `
	      AND ` + terminalAt + ` < $1
	      AND e.created_at < $1
	      AND e.type NOT LIKE $2
	    LIMIT $3
	)`

// The clock row goes with the post-mortem sweep, not with the session row's
// cascade: sessions rows are never deleted, so before this janitor every
// terminal session left its clock row behind forever.
const postMortemClocksSQL = `
	DELETE FROM session_trace_clock
	WHERE ctid IN (
	    SELECT c.ctid
	    FROM session_trace_clock c
	    JOIN sessions s ON s.id = c.session_id
	    WHERE s.state IN ` + terminalStates + `
	      AND ` + terminalAt + ` < $1
	    LIMIT $2
	)`

// --- scan helpers -------------------------------------------------------------

func scanSamples(rows pgx.Rows, what string) ([]Sample, error) {
	defer rows.Close()
	var out []Sample
	for rows.Next() {
		var m Sample
		var raw []byte
		if err := rows.Scan(&m.Source, &m.TsUnixMs, &raw); err != nil {
			return nil, fmt.Errorf("scan %s: %w", what, err)
		}
		m.Metrics = json.RawMessage(raw)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate %s: %w", what, err)
	}
	return out, nil
}

func scanEvents(rows pgx.Rows, what string) ([]Event, error) {
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var raw []byte
		if err := rows.Scan(&e.Source, &e.TsUnixMs, &e.Type, &raw); err != nil {
			return nil, fmt.Errorf("scan %s: %w", what, err)
		}
		e.Payload = json.RawMessage(raw)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate %s: %w", what, err)
	}
	return out, nil
}
