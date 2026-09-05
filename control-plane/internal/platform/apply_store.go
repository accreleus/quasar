package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Data access for the apply half (migration 0075); it holds no decision.
//
// Single-flight is the database's partial unique indexes, so this file only
// TRANSLATES the violation (ErrAttemptInFlight) and must never pre-check for
// it — a pre-check races with itself.

// pgUniqueViolation is the SQLSTATE for a unique-constraint violation.
const pgUniqueViolation = "23505"

var (
	// ErrAttemptInFlight is the `409 attempt_in_flight` refusal, raised by
	// platform_apply_attempts_open_target_uk.
	ErrAttemptInFlight = errors.New("an apply is already in flight on this target")
	// ErrAttemptNotFound is a request id or attempt id no row matches.
	ErrAttemptNotFound = errors.New("attempt not found")
	// ErrReleaseNotFound is an unknown platform_releases id.
	ErrReleaseNotFound = errors.New("release not found")
	// ErrHostNotFound is an unknown hosts id.
	ErrHostNotFound = errors.New("host not found")
)

const attemptColumns = `a.id::text, a.run_id::text, a.kind, a.target, a.host_id::text,
	h.node_name, a.release_id::text, a.requested_digests, a.previous_digests,
	a.state, a.reason, a.sessions_remaining, a.force, a.output,
	a.requested_by::text, a.created_at, a.started_at, a.finished_at`

const attemptFrom = ` FROM platform_apply_attempts a LEFT JOIN hosts h ON h.id = a.host_id`

// terminalStatesSQL is the open/terminal split, in SQL. Go twin:
// TerminalAttemptState; pinned together by TestTerminalSplitMatchesSQL.
const terminalStatesSQL = `('succeeded','failed','cancelled')`

func scanAttempt(row pgx.Row) (Attempt, error) {
	var a Attempt
	var requested, previous []byte
	if err := row.Scan(&a.ID, &a.RunID, &a.Kind, &a.Target, &a.HostID, &a.NodeName,
		&a.ReleaseID, &requested, &previous, &a.State, &a.Reason, &a.SessionsRemaining,
		&a.Force, &a.Output, &a.RequestedBy, &a.CreatedAt, &a.StartedAt, &a.FinishedAt); err != nil {
		return Attempt{}, err
	}
	a.RequestedDigests = make([]ComponentDigest, 0)
	a.PreviousDigests = make([]PreviousDigest, 0)
	if len(requested) > 0 {
		if err := json.Unmarshal(requested, &a.RequestedDigests); err != nil {
			return Attempt{}, fmt.Errorf("decode requested_digests: %w", err)
		}
	}
	if len(previous) > 0 {
		if err := json.Unmarshal(previous, &a.PreviousDigests); err != nil {
			return Attempt{}, fmt.Errorf("decode previous_digests: %w", err)
		}
	}
	return a, nil
}

func (s *Store) queryAttempts(ctx context.Context, sql string, args ...any) ([]Attempt, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("query platform_apply_attempts: %w", err)
	}
	defer rows.Close()
	out := make([]Attempt, 0)
	for rows.Next() {
		a, err := scanAttempt(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// NewHostAttempt is everything an inserted host attempt needs. The digests are
// resolved by the caller (from the manifest, or from an earlier attempt for a
// revert) because they, not the release row, are the authority.
type NewHostAttempt struct {
	Kind string
	// The fleet run this attempt belongs to; nil for a standalone apply.
	RunID     *string
	HostID    string
	ReleaseID *string
	Requested []ComponentDigest
	Previous  []PreviousDigest
	Force     bool
	Actor     *string
}

// CreateHostAttempt inserts a `queued` attempt. A second open attempt for the
// same host raises the partial unique index and comes back as
// ErrAttemptInFlight — the refusal, unraced.
func (s *Store) CreateHostAttempt(ctx context.Context, in NewHostAttempt) (Attempt, error) {
	requested, err := json.Marshal(in.Requested)
	if err != nil {
		return Attempt{}, fmt.Errorf("encode requested_digests: %w", err)
	}
	previous, err := json.Marshal(in.Previous)
	if err != nil {
		return Attempt{}, fmt.Errorf("encode previous_digests: %w", err)
	}
	var id string
	err = s.pool.QueryRow(ctx, `
		INSERT INTO platform_apply_attempts
		    (run_id, kind, target, host_id, release_id, requested_digests, previous_digests,
		     state, force, requested_by)
		VALUES ($1::uuid, $2, 'host', $3::uuid, $4::uuid, $5::jsonb, $6::jsonb, 'queued', $7, $8::uuid)
		RETURNING id::text
	`, in.RunID, in.Kind, in.HostID, in.ReleaseID, requested, previous, in.Force, in.Actor).Scan(&id)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
			return Attempt{}, ErrAttemptInFlight
		}
		return Attempt{}, fmt.Errorf("insert platform_apply_attempt: %w", err)
	}
	return s.Attempt(ctx, id)
}

// Attempt reads one row by id.
func (s *Store) Attempt(ctx context.Context, id string) (Attempt, error) {
	a, err := scanAttempt(s.pool.QueryRow(ctx,
		`SELECT `+attemptColumns+attemptFrom+` WHERE a.id = $1::uuid`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Attempt{}, ErrAttemptNotFound
	}
	if err != nil {
		return Attempt{}, fmt.Errorf("read platform_apply_attempt: %w", err)
	}
	return a, nil
}

// ListAttempts is the history read: newest first, optionally one host's. An
// unknown host_id yields an empty list, never a 404 — "this host has no
// history" and "this host is gone" are the same answer here.
func (s *Store) ListAttempts(ctx context.Context, hostID string, limit int) ([]Attempt, error) {
	return s.queryAttempts(ctx, `
		SELECT `+attemptColumns+attemptFrom+`
		WHERE ($1::uuid IS NULL OR a.host_id = $1::uuid)
		ORDER BY a.created_at DESC, a.id DESC
		LIMIT $2
	`, nilIfEmpty(hostID), limit)
}

// OpenAttempts is every non-terminal attempt on the instance: `active_apply`,
// the plan's `attempt_in_flight`, and what boot re-adoption resumes.
func (s *Store) OpenAttempts(ctx context.Context) ([]Attempt, error) {
	return s.queryAttempts(ctx, `
		SELECT `+attemptColumns+attemptFrom+`
		WHERE a.state NOT IN `+terminalStatesSQL+`
		ORDER BY a.created_at DESC, a.id DESC`)
}

// ActiveRunExists reports whether a fleet run owns the fleet right now, which
// is the `409 run_active` refusal. This build creates no run; the read is here
// so a standalone apply is refused the moment #117 can create one.
func (s *Store) ActiveRunExists(ctx context.Context) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM platform_apply_runs WHERE state IN ('pending','running'))
	`).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("query platform_apply_runs: %w", err)
	}
	return exists, nil
}

// SetWaitingSessions records the last observed non-terminal session count while
// the attempt drains. Advisory: the drain decision is made against the live
// sessions table, never against this column.
func (s *Store) SetWaitingSessions(ctx context.Context, attemptID string, remaining int) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE platform_apply_attempts
		   SET state = 'waiting_sessions', sessions_remaining = $2
		 WHERE id = $1::uuid AND state NOT IN `+terminalStatesSQL, attemptID, remaining)
	if err != nil {
		return fmt.Errorf("update waiting_sessions: %w", err)
	}
	return nil
}

// MintRequestID persists the apply's identity and marks it sent, BEFORE the
// command goes out (agent-api.md: the agent that receives it is normally
// destroyed by carrying it out). Minted by the database so this package needs
// no uuid dependency. Returns ErrAttemptNotFound when the attempt resolved
// first — a cancel or a restart-adopted terminal state.
func (s *Store) MintRequestID(ctx context.Context, attemptID string) (string, error) {
	var reqID string
	err := s.pool.QueryRow(ctx, `
		UPDATE platform_apply_attempts
		   SET updater_request_id = gen_random_uuid(),
		       state = 'pending',
		       sessions_remaining = NULL,
		       started_at = COALESCE(started_at, now())
		 WHERE id = $1::uuid AND state NOT IN `+terminalStatesSQL+`
		RETURNING updater_request_id::text
	`, attemptID).Scan(&reqID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrAttemptNotFound
	}
	if err != nil {
		return "", fmt.Errorf("mint updater_request_id: %w", err)
	}
	return reqID, nil
}

// FailAttempt resolves an attempt as failed. Idempotent by the state guard: a
// deadline and a late terminal report race, and the first one wins.
func (s *Store) FailAttempt(ctx context.Context, attemptID, reason, output string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE platform_apply_attempts
		   SET state = 'failed', reason = $2, sessions_remaining = NULL,
		       output = CASE WHEN $3 = '' THEN output ELSE $3 END,
		       finished_at = now()
		 WHERE id = $1::uuid AND state NOT IN `+terminalStatesSQL, attemptID, reason, output)
	if err != nil {
		return fmt.Errorf("fail platform_apply_attempt: %w", err)
	}
	return nil
}

// SucceedAttempt resolves an attempt as succeeded and reports whether this call
// was the one that did it — false means it was already terminal, which is what
// makes a late `release_state{succeeded}` a no-op rather than a conflict.
func (s *Store) SucceedAttempt(ctx context.Context, attemptID string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE platform_apply_attempts
		   SET state = 'succeeded', reason = NULL, sessions_remaining = NULL,
		       finished_at = now()
		 WHERE id = $1::uuid AND state NOT IN `+terminalStatesSQL, attemptID)
	if err != nil {
		return false, fmt.Errorf("succeed platform_apply_attempt: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// AttemptByRequestID resolves a `release_state`'s correlation key. A request id
// no row matches is the caller's cue to DROP the message, not store it.
func (s *Store) AttemptByRequestID(ctx context.Context, requestID string) (Attempt, error) {
	a, err := scanAttempt(s.pool.QueryRow(ctx,
		`SELECT `+attemptColumns+attemptFrom+` WHERE a.updater_request_id = $1::uuid`, requestID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Attempt{}, ErrAttemptNotFound
	}
	if err != nil {
		return Attempt{}, fmt.Errorf("read attempt by request id: %w", err)
	}
	return a, nil
}

// RecordReleaseState writes one relayed non-terminal state onto an open
// attempt: the state itself, the previous digests (present in EVERY message,
// which is what makes them recorded before a restore needs them) and the
// bounded output. Terminal states go through Fail/SucceedAttempt.
func (s *Store) RecordReleaseState(ctx context.Context, attemptID, state string, previous []PreviousDigest, output string) error {
	prev, err := json.Marshal(previous)
	if err != nil {
		return fmt.Errorf("encode previous_digests: %w", err)
	}
	_, err = s.pool.Exec(ctx, `
		UPDATE platform_apply_attempts
		   SET state = $2,
		       previous_digests = CASE WHEN $3::jsonb = '[]'::jsonb THEN previous_digests ELSE $3::jsonb END,
		       output = CASE WHEN $4 = '' THEN output ELSE $4 END
		 WHERE id = $1::uuid AND state NOT IN `+terminalStatesSQL, attemptID, state, prev, output)
	if err != nil {
		return fmt.Errorf("record release_state: %w", err)
	}
	return nil
}

// SetPreviousDigests records the previous digests alone — used when a terminal
// report carries them, since the terminal write itself must not clobber the
// state guard's ordering.
func (s *Store) SetPreviousDigests(ctx context.Context, attemptID string, previous []PreviousDigest) error {
	if len(previous) == 0 {
		return nil
	}
	prev, err := json.Marshal(previous)
	if err != nil {
		return fmt.Errorf("encode previous_digests: %w", err)
	}
	_, err = s.pool.Exec(ctx, `
		UPDATE platform_apply_attempts SET previous_digests = $2::jsonb WHERE id = $1::uuid
	`, attemptID, prev)
	if err != nil {
		return fmt.Errorf("set previous_digests: %w", err)
	}
	return nil
}

// OpenHostAttempt returns a host's open attempt and the source_commit of the
// release it is moving to, which is what a post-apply `register` is matched
// against. The commit is empty when the attempt names no release row.
func (s *Store) OpenHostAttempt(ctx context.Context, hostID string) (Attempt, string, error) {
	var commit *string
	var a Attempt
	var requested, previous []byte
	err := s.pool.QueryRow(ctx, `
		SELECT `+attemptColumns+`, r.source_commit
		  FROM platform_apply_attempts a
		  LEFT JOIN hosts h ON h.id = a.host_id
		  LEFT JOIN platform_releases r ON r.id = a.release_id
		 WHERE a.host_id = $1::uuid AND a.state NOT IN `+terminalStatesSQL+`
		 ORDER BY a.created_at DESC LIMIT 1
	`, hostID).Scan(&a.ID, &a.RunID, &a.Kind, &a.Target, &a.HostID, &a.NodeName,
		&a.ReleaseID, &requested, &previous, &a.State, &a.Reason, &a.SessionsRemaining,
		&a.Force, &a.Output, &a.RequestedBy, &a.CreatedAt, &a.StartedAt, &a.FinishedAt, &commit)
	if errors.Is(err, pgx.ErrNoRows) {
		return Attempt{}, "", ErrAttemptNotFound
	}
	if err != nil {
		return Attempt{}, "", fmt.Errorf("read open host attempt: %w", err)
	}
	a.RequestedDigests = make([]ComponentDigest, 0)
	a.PreviousDigests = make([]PreviousDigest, 0)
	if len(requested) > 0 {
		_ = json.Unmarshal(requested, &a.RequestedDigests)
	}
	if len(previous) > 0 {
		_ = json.Unmarshal(previous, &a.PreviousDigests)
	}
	if commit == nil {
		return a, "", nil
	}
	return a, *commit, nil
}

// LastSucceededDigests is what a host is demonstrably on: the digests its last
// succeeded attempt asked for. Empty when it has never succeeded one, which is
// the honest "nobody looked" rather than a guess.
func (s *Store) LastSucceededDigests(ctx context.Context, hostID string) ([]ComponentDigest, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx, `
		SELECT requested_digests FROM platform_apply_attempts
		 WHERE host_id = $1::uuid AND state = 'succeeded'
		 ORDER BY created_at DESC, id DESC LIMIT 1
	`, hostID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read last succeeded attempt: %w", err)
	}
	out := make([]ComponentDigest, 0)
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode requested_digests: %w", err)
	}
	return out, nil
}

// NonTerminalSessions counts what an apply would end on a host. SQL twin:
// session.Store.NonTerminalSessionIDsOnHost — the same state predicate, because
// "what a force-drain stops" and "what this apply is waiting for" must be the
// same set.
func (s *Store) NonTerminalSessions(ctx context.Context, hostID string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM sessions
		 WHERE host_id = $1::uuid AND state NOT IN ('stopped','failed')
	`, hostID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count host sessions: %w", err)
	}
	return n, nil
}

// HostStatus reads one host's scheduling status, and is how the apply learns
// whether it found the host already cordoned.
func (s *Store) HostStatus(ctx context.Context, hostID string) (string, error) {
	var status string
	err := s.pool.QueryRow(ctx, `SELECT status FROM hosts WHERE id = $1::uuid`, hostID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrHostNotFound
	}
	if err != nil {
		return "", fmt.Errorf("read host status: %w", err)
	}
	return status, nil
}

// Release reads one release row by id.
func (s *Store) Release(ctx context.Context, id string) (Release, error) {
	var r Release
	var manifest []byte
	err := s.pool.QueryRow(ctx,
		`SELECT `+releaseColumns+` FROM platform_releases WHERE id = $1::uuid`, id).
		Scan(&r.ID, &r.Channel, &r.Version, &r.SourceCommit, &r.BuiltAt, &r.SchemaVersion,
			&r.Prerelease, &r.Notes, &r.CompareURL, &manifest, &r.DiscoveredAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Release{}, ErrReleaseNotFound
	}
	if err != nil {
		return Release{}, fmt.Errorf("read platform_release: %w", err)
	}
	if len(manifest) > 0 {
		r.Manifest = manifest
	}
	return r, nil
}

func nilIfEmpty(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}
