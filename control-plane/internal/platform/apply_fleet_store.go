package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Data access for the fleet half: `platform_apply_runs`, and the control-plane
// attempt (host_id NULL). It holds no decision — single-flight is
// platform_apply_runs_active_uk, so this file only TRANSLATES the violation.

var (
	// ErrRunActive is the `409 run_active` refusal, raised by
	// platform_apply_runs_active_uk.
	ErrRunActive = errors.New("a fleet apply is already running")
	// ErrRunNotFound is a run id no row matches.
	ErrRunNotFound = errors.New("run not found")
	// ErrRunNotActive is the `409 run_not_active` cancel refusal.
	ErrRunNotActive = errors.New("this run is already terminal")
)

// terminalRunStatesSQL is TerminalRunState in SQL; pinned to its Go twin by
// TestTerminalRunSplitMatchesSQL.
const terminalRunStatesSQL = `('succeeded','failed','cancelled')`

const runColumns = `id::text, release_id::text, state, force, requested_by::text,
	cancel_requested, cancel_requested_at, current_target, current_host_id::text,
	error, created_at, started_at, finished_at`

func scanRun(row pgx.Row) (ApplyRun, error) {
	var r ApplyRun
	var errText string
	if err := row.Scan(&r.ID, &r.ReleaseID, &r.State, &r.Force, &r.RequestedBy,
		&r.CancelRequested, &r.CancelRequestedAt, &r.CurrentTarget, &r.CurrentHostID,
		&errText, &r.CreatedAt, &r.StartedAt, &r.FinishedAt); err != nil {
		return ApplyRun{}, err
	}
	if errText != "" {
		r.Error = &errText
	}
	r.Skipped = make([]RunSkip, 0)
	r.Attempts = make([]Attempt, 0)
	return r, nil
}

// CreateRun inserts a `pending` run. A second active run raises the partial
// unique index and comes back as ErrRunActive — the refusal, unraced.
func (s *Store) CreateRun(ctx context.Context, releaseID string, force bool, actor *string) (ApplyRun, error) {
	var id string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO platform_apply_runs (release_id, state, force, requested_by)
		VALUES ($1::uuid, 'pending', $2, $3::uuid)
		RETURNING id::text
	`, releaseID, force, actor).Scan(&id)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
			return ApplyRun{}, ErrRunActive
		}
		return ApplyRun{}, fmt.Errorf("insert platform_apply_run: %w", err)
	}
	return s.Run(ctx, id)
}

// Run reads one run by id, without its attempts.
func (s *Store) Run(ctx context.Context, id string) (ApplyRun, error) {
	r, err := scanRun(s.pool.QueryRow(ctx,
		`SELECT `+runColumns+` FROM platform_apply_runs WHERE id = $1::uuid`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return ApplyRun{}, ErrRunNotFound
	}
	if err != nil {
		return ApplyRun{}, fmt.Errorf("read platform_apply_run: %w", err)
	}
	return r, nil
}

// ActiveRun is the run that owns the fleet right now, or nil.
func (s *Store) ActiveRun(ctx context.Context) (*ApplyRun, error) {
	r, err := scanRun(s.pool.QueryRow(ctx,
		`SELECT `+runColumns+` FROM platform_apply_runs
		  WHERE state NOT IN `+terminalRunStatesSQL+`
		  ORDER BY created_at DESC LIMIT 1`))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read active platform_apply_run: %w", err)
	}
	return &r, nil
}

// ListRuns reads the run history, newest first.
func (s *Store) ListRuns(ctx context.Context, limit int) ([]ApplyRun, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+runColumns+` FROM platform_apply_runs
		 ORDER BY created_at DESC, id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("query platform_apply_runs: %w", err)
	}
	defer rows.Close()
	out := make([]ApplyRun, 0)
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RunAttempts reads one run's attempts in the order the run reached them.
func (s *Store) RunAttempts(ctx context.Context, runID string) ([]Attempt, error) {
	return s.queryAttempts(ctx, `
		SELECT `+attemptColumns+attemptFrom+`
		 WHERE a.run_id = $1::uuid
		 ORDER BY a.created_at ASC, a.id ASC`, runID)
}

// SetRunTarget records which target the run is on and marks it running. The
// denormalized current_target is what a resuming control plane reads first,
// before it has loaded any attempt row.
func (s *Store) SetRunTarget(ctx context.Context, runID, target string, hostID *string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE platform_apply_runs
		   SET state = 'running', current_target = $2, current_host_id = $3::uuid,
		       started_at = COALESCE(started_at, now())
		 WHERE id = $1::uuid AND state NOT IN `+terminalRunStatesSQL, runID, target, hostID)
	if err != nil {
		return fmt.Errorf("set run target: %w", err)
	}
	return nil
}

// FinishRun resolves a run. Idempotent by the state guard: a cancel and a
// failing target race, and the first one wins.
func (s *Store) FinishRun(ctx context.Context, runID, state, errText string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE platform_apply_runs
		   SET state = $2, current_target = NULL, current_host_id = NULL,
		       error = CASE WHEN $3 = '' THEN error ELSE left($3, 4096) END,
		       finished_at = now()
		 WHERE id = $1::uuid AND state NOT IN `+terminalRunStatesSQL, runID, state, errText)
	if err != nil {
		return fmt.Errorf("finish platform_apply_run: %w", err)
	}
	return nil
}

// RequestCancel sets the run's persisted cancel flag and cancels every attempt
// the cancel caught BEFORE it was sent. It never touches a sent attempt:
// interrupting a recreate is how a stack is left with no container at all.
// Idempotent; ErrRunNotActive when the run is already terminal.
func (s *Store) RequestCancel(ctx context.Context, runID string) (ApplyRun, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE platform_apply_runs
		   SET cancel_requested = true,
		       cancel_requested_at = COALESCE(cancel_requested_at, now())
		 WHERE id = $1::uuid AND state NOT IN `+terminalRunStatesSQL, runID)
	if err != nil {
		return ApplyRun{}, fmt.Errorf("request cancel: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Either no such run or a terminal one; the read tells them apart.
		if _, err := s.Run(ctx, runID); err != nil {
			return ApplyRun{}, err
		}
		return ApplyRun{}, ErrRunNotActive
	}
	if _, err := s.pool.Exec(ctx, `
		UPDATE platform_apply_attempts
		   SET state = 'cancelled', sessions_remaining = NULL, finished_at = now()
		 WHERE run_id = $1::uuid AND state IN ('queued','waiting_sessions')
	`, runID); err != nil {
		return ApplyRun{}, fmt.Errorf("cancel unsent attempts: %w", err)
	}
	return s.Run(ctx, runID)
}

// NewControlPlaneAttempt is everything an inserted control-plane attempt needs.
// host_id is always NULL; the zero uuid in the single-flight index is what
// makes two open control-plane attempts impossible.
type NewControlPlaneAttempt struct {
	RunID     *string
	ReleaseID *string
	Requested []ComponentDigest
	Previous  []PreviousDigest
	Actor     *string
}

// CreateControlPlaneAttempt inserts a `queued` control-plane attempt.
func (s *Store) CreateControlPlaneAttempt(ctx context.Context, in NewControlPlaneAttempt) (Attempt, error) {
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
		    (run_id, kind, target, release_id, requested_digests, previous_digests,
		     state, requested_by)
		VALUES ($1::uuid, 'apply', 'control_plane', $2::uuid, $3::jsonb, $4::jsonb, 'queued', $5::uuid)
		RETURNING id::text
	`, in.RunID, in.ReleaseID, requested, previous, in.Actor).Scan(&id)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
			return Attempt{}, ErrAttemptInFlight
		}
		return Attempt{}, fmt.Errorf("insert control-plane attempt: %w", err)
	}
	return s.Attempt(ctx, id)
}

// OpenControlPlaneAttempt returns the open control-plane attempt and the
// source_commit of the release it is moving to — what this binary's own
// identity is compared against on boot. The commit is empty when the attempt
// names no release row.
func (s *Store) OpenControlPlaneAttempt(ctx context.Context) (Attempt, string, error) {
	var commit *string
	var a Attempt
	var requested, previous []byte
	err := s.pool.QueryRow(ctx, `
		SELECT `+attemptColumns+`, r.source_commit
		  FROM platform_apply_attempts a
		  LEFT JOIN hosts h ON h.id = a.host_id
		  LEFT JOIN platform_releases r ON r.id = a.release_id
		 WHERE a.target = 'control_plane' AND a.state NOT IN `+terminalStatesSQL+`
		 ORDER BY a.created_at DESC LIMIT 1
	`).Scan(&a.ID, &a.RunID, &a.Kind, &a.Target, &a.HostID, &a.NodeName,
		&a.ReleaseID, &requested, &previous, &a.State, &a.Reason, &a.SessionsRemaining,
		&a.Force, &a.Output, &a.RequestedBy, &a.CreatedAt, &a.StartedAt, &a.FinishedAt, &commit)
	if errors.Is(err, pgx.ErrNoRows) {
		return Attempt{}, "", ErrAttemptNotFound
	}
	if err != nil {
		return Attempt{}, "", fmt.Errorf("read open control-plane attempt: %w", err)
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

// AttemptRequestID is the id the control plane minted before calling the
// updater. "" while the attempt is queued; it is what a boot polls the result
// file on.
func (s *Store) AttemptRequestID(ctx context.Context, attemptID string) (string, error) {
	var id *string
	err := s.pool.QueryRow(ctx,
		`SELECT updater_request_id::text FROM platform_apply_attempts WHERE id = $1::uuid`, attemptID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrAttemptNotFound
	}
	if err != nil {
		return "", fmt.Errorf("read updater_request_id: %w", err)
	}
	if id == nil {
		return "", nil
	}
	return *id, nil
}

// LastSucceededControlPlaneDigests is what this control plane is demonstrably
// on: the digests of its last succeeded attempt. Empty when it has never
// applied one, which is the honest "nobody looked".
func (s *Store) LastSucceededControlPlaneDigests(ctx context.Context) ([]ComponentDigest, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx, `
		SELECT requested_digests FROM platform_apply_attempts
		 WHERE target = 'control_plane' AND state = 'succeeded'
		 ORDER BY created_at DESC, id DESC LIMIT 1
	`).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read last succeeded control-plane attempt: %w", err)
	}
	out := make([]ComponentDigest, 0)
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode requested_digests: %w", err)
	}
	return out, nil
}
