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
const terminalRunStatesSQL = `('succeeded','succeeded_partial','failed','cancelled')`

const runColumns = `id::text, release_id::text, state, force, unattended, requested_by::text,
	cancel_requested, cancel_requested_at, current_target, current_host_id::text,
	error, created_at, started_at, finished_at, retry_of::text, skipped`

func scanRun(row pgx.Row) (ApplyRun, error) {
	var r ApplyRun
	var errText string
	var skipped []byte
	if err := row.Scan(&r.ID, &r.ReleaseID, &r.State, &r.Force, &r.Unattended, &r.RequestedBy,
		&r.CancelRequested, &r.CancelRequestedAt, &r.CurrentTarget, &r.CurrentHostID,
		&errText, &r.CreatedAt, &r.StartedAt, &r.FinishedAt, &r.RetryOf, &skipped); err != nil {
		return ApplyRun{}, err
	}
	if errText != "" {
		r.Error = &errText
	}
	r.Skipped = make([]RunSkip, 0)
	if len(skipped) > 0 {
		if err := json.Unmarshal(skipped, &r.Skipped); err != nil {
			return ApplyRun{}, fmt.Errorf("decode platform_apply_runs.skipped: %w", err)
		}
	}
	r.Attempts = make([]Attempt, 0)
	return r, nil
}

// CreateRun inserts a `pending` run. A second active run raises the partial
// unique index and comes back as ErrRunActive — the refusal, unraced. retryOf
// is the succeeded_partial run this one finishes, or nil (amendment 9).
func (s *Store) CreateRun(ctx context.Context, releaseID string, force bool, actor, retryOf *string) (ApplyRun, error) {
	var id string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO platform_apply_runs (release_id, state, force, requested_by, retry_of)
		VALUES ($1::uuid, 'pending', $2, $3::uuid, $4::uuid)
		RETURNING id::text
	`, releaseID, force, actor, retryOf).Scan(&id)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
			return ApplyRun{}, ErrRunActive
		}
		return ApplyRun{}, fmt.Errorf("insert platform_apply_run: %w", err)
	}
	return s.Run(ctx, id)
}

// CreateUnattendedRun is CreateRun for a run nobody clicked (#122): force is
// false and there is no requesting admin.
//
// A separate method rather than two more arguments on CreateRun, so `force` is
// not expressible on this path at all. `force` means an operator agreeing to end
// N live sessions, and an unattended pass has no operator to agree; since #153 it
// additionally STOPS sessions on a migrating release, which this path is never
// allowed to reach. A bool argument would make the wrong call one typo away.
func (s *Store) CreateUnattendedRun(ctx context.Context, releaseID string) (ApplyRun, error) {
	var id string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO platform_apply_runs (release_id, state, force, requested_by, unattended)
		VALUES ($1::uuid, 'pending', false, NULL, true)
		RETURNING id::text
	`, releaseID).Scan(&id)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
			return ApplyRun{}, ErrRunActive
		}
		return ApplyRun{}, fmt.Errorf("insert unattended platform_apply_run: %w", err)
	}
	return s.Run(ctx, id)
}

// UnattendedFailedReleaseIDs is the failure suppression (#122): the releases
// whose MOST RECENT run was a failed unattended one.
//
// "Most recent", not "any", and that is the whole implementation of the
// operator's rule that an admin applying the release themselves clears the
// suppression. A `DISTINCT release_id WHERE unattended AND state='failed'`
// suppresses for ever — the admin's own successful run sits alongside the old
// failure and changes nothing, so a host left behind by one bad pass never
// updates again until a newer release appears. Ordering by `created_at DESC` per
// release makes an admin run of ANY outcome reset it, and a second unattended
// failure re-suppress it, which is what the contract says.
//
// Per RELEASE and not global, deliberately: a genuinely bad release must not be
// re-attempted once a week for ever, but one flaky host must not end automatic
// updates for the whole instance either.
//
// `unattended` is what makes this answerable at all: requested_by is NULL for an
// unattended run AND for a run whose requesting admin has since been deleted
// (ON DELETE SET NULL), so it cannot stand in.
func (s *Store) UnattendedFailedReleaseIDs(ctx context.Context) (map[string]bool, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT release_id::text FROM (
		    SELECT DISTINCT ON (release_id) release_id, unattended, state
		    FROM platform_apply_runs
		    ORDER BY release_id, created_at DESC, id DESC
		) last
		WHERE last.unattended AND last.state = 'failed'
	`)
	if err != nil {
		return nil, fmt.Errorf("read unattended failures: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("read unattended failures: %w", err)
		}
		out[id] = true
	}
	return out, rows.Err()
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

// RecordSkip appends one host the run passed over to its persisted `skipped`
// list (migration 0083). Idempotent per host: a re-adopted run re-walks its
// host list, and a host it already recorded must not appear twice.
func (s *Store) RecordSkip(ctx context.Context, runID string, skip RunSkip) error {
	entry, err := json.Marshal([]RunSkip{skip})
	if err != nil {
		return fmt.Errorf("encode skip: %w", err)
	}
	_, err = s.pool.Exec(ctx, `
		UPDATE platform_apply_runs
		   SET skipped = skipped || $2::jsonb
		 WHERE id = $1::uuid
		   AND NOT EXISTS (
		       SELECT 1 FROM jsonb_array_elements(skipped) e
		        WHERE e->>'host_id' = $3)`, runID, entry, skip.HostID)
	if err != nil {
		return fmt.Errorf("record skip: %w", err)
	}
	return nil
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

// SetCordonedHosts records what the run found before it cordoned. Persisted,
// not held in memory: the run's first target restarts this process (migration
// 0076).
func (s *Store) SetCordonedHosts(ctx context.Context, runID string, states []HostCordon) error {
	raw, err := json.Marshal(states)
	if err != nil {
		return fmt.Errorf("encode cordoned_hosts: %w", err)
	}
	_, err = s.pool.Exec(ctx,
		`UPDATE platform_apply_runs SET cordoned_hosts = $2::jsonb WHERE id = $1::uuid`, runID, raw)
	if err != nil {
		return fmt.Errorf("set cordoned_hosts: %w", err)
	}
	return nil
}

// CordonedHosts reads that record back.
func (s *Store) CordonedHosts(ctx context.Context, runID string) ([]HostCordon, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx,
		`SELECT cordoned_hosts FROM platform_apply_runs WHERE id = $1::uuid`, runID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrRunNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read cordoned_hosts: %w", err)
	}
	out := make([]HostCordon, 0)
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, fmt.Errorf("decode cordoned_hosts: %w", err)
		}
	}
	return out, nil
}

// FleetNonTerminalSessions counts every session on the instance, not one
// host's — a control-plane recreate is instance-wide. Same state predicate as
// NonTerminalSessions, without the host filter. This is the count a MIGRATING
// control-plane step drains to zero and reports as `sessions_remaining`; since
// #128 a recreate on its own no longer ends any of them (#153).
func (s *Store) FleetNonTerminalSessions(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM sessions WHERE state NOT IN ('stopped','failed')
	`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count fleet sessions: %w", err)
	}
	return n, nil
}

// FleetInFlightSessions counts the sessions a control-plane recreate still
// ends: everything non-terminal EXCEPT `running`.
//
// The predicate is deliberately character-for-character the one in
// session.Store.ReapHostExceptRunning (#128). A `running` row survives a
// recreate because the agent holds the session and the heartbeat re-adopts the
// row; a row that is `pending`, `assigned`, `starting` or `stopping` was mid
// flight in a goroutine that died with the old connection, so the reconnecting
// agent's first act is to fail it. The fleet-wide wait used to make that set
// provably empty at the recreate; a non-migrating step no longer drains, so it
// waits on THIS count instead (#153) — a user who pressed Play two seconds
// earlier should not have their launch reaped by an update.
func (s *Store) FleetInFlightSessions(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM sessions WHERE state NOT IN ('stopped','failed','running')
	`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count fleet in-flight sessions: %w", err)
	}
	return n, nil
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
