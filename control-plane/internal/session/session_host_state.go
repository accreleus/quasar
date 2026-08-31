package session

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// The telemetry trust boundary lives here, not internal/telemetry: that package
// owns storage/retention, this one owns the sessions table and the question
// "may this reporter write this session's telemetry at all?".

// SessionHostState is the minimal session view the ingestion trust boundary and
// the trace relay need: who owns it (host), and whether it is live.
type SessionHostState struct {
	HostID *string
	State  State
}

// GetSessionHostState is the trust boundary for agent metric ingestion (agent
// must own the session's host) and the running-precondition check for the
// deep-trace relay. Returns ErrNotFound if the session is gone.
func (s *Store) GetSessionHostState(ctx context.Context, sessionID string) (SessionHostState, error) {
	if !isValidUUID(sessionID) {
		return SessionHostState{}, ErrNotFound
	}
	var st SessionHostState
	var state string
	err := s.pool.QueryRow(ctx, `
		SELECT host_id::text, state FROM sessions WHERE id = $1::uuid
	`, sessionID).Scan(&st.HostID, &state)
	if errors.Is(err, pgx.ErrNoRows) {
		return SessionHostState{}, ErrNotFound
	}
	if err != nil {
		return SessionHostState{}, fmt.Errorf("get session host/state: %w", err)
	}
	st.State = State(state)
	return st, nil
}

// AgentTraceEventAllowed: an agent may store a trace event only for a session
// placed on the reporting host and currently running there (same posture as
// AgentMetrics); otherwise the event is dropped, never fatal to the WS.
// Returns (false, nil) to drop, and drops on a real DB error too.
func (s *Store) AgentTraceEventAllowed(ctx context.Context, hostID, sessionID string) (bool, error) {
	hs, err := s.GetSessionHostState(ctx, sessionID)
	if errors.Is(err, ErrNotFound) {
		return false, nil // unknown session ⇒ drop
	}
	if err != nil {
		return false, err
	}
	if hs.HostID == nil || *hs.HostID != hostID {
		return false, nil // cross-host (or unplaced) ⇒ drop
	}
	if hs.State != StateRunning {
		return false, nil // not running here ⇒ drop (events never resurrect a session)
	}
	return true, nil
}
