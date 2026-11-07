package agentws

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

const (
	// ownerCacheMax bounds each cache; it is cleared when full. One agent runs a handful of
	// sessions, so a full cache means a flood of forged ids.
	ownerCacheMax = 1024
	// foreignTTL is how long a refusal is remembered, so a forging agent costs one query
	// per id per window, not one per frame.
	foreignTTL = 5 * time.Second
	// foreignLogEvery spaces the per-connection drop log.
	foreignLogEvery = 10 * time.Second
)

// sessionPlacedOn reports whether sessions.host_id of sessionID is hostID.
func (s *agentStore) sessionPlacedOn(ctx context.Context, sessionID, hostID string) (bool, error) {
	var sid, hid pgtype.UUID
	if sid.Scan(sessionID) != nil || hid.Scan(hostID) != nil {
		return false, nil
	}
	var ok bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM sessions WHERE id = $1 AND host_id = $2)`, sid, hid).Scan(&ok)
	return ok, err
}

// signalingOwners decides whether one agent connection may relay signaling for a session id.
// Placement is the sessions.host_id the coordinator wrote when it inserted the row, before it
// sent session_assign; nothing updates it afterwards, so a positive answer is cached for the
// life of the connection. Read-loop only: no locking.
type signalingOwners struct {
	store   *agentStore
	log     *slog.Logger
	hostID  string
	owned   map[string]struct{}
	foreign map[string]time.Time
	lastLog time.Time
	dropped int
	now     func() time.Time // test seam
}

func newSignalingOwners(store *agentStore, log *slog.Logger, hostID string) *signalingOwners {
	return &signalingOwners{store: store, log: log, hostID: hostID,
		owned: map[string]struct{}{}, foreign: map[string]time.Time{}, now: time.Now}
}

// allows fails closed: a lookup error drops the frame, uncached, and the next frame retries.
func (o *signalingOwners) allows(ctx context.Context, sessionID string) bool {
	if _, ok := o.owned[sessionID]; ok {
		return true
	}
	now := o.now()
	if until, ok := o.foreign[sessionID]; ok && now.Before(until) {
		o.drop(now)
		return false
	}
	qctx, cancel := context.WithTimeout(ctx, agentDBCallTimeout)
	ok, err := o.store.sessionPlacedOn(qctx, sessionID, o.hostID)
	cancel()
	if err != nil {
		o.log.Error("agent signaling placement lookup failed; frame dropped", "host_id", o.hostID, "err", err)
		return false
	}
	if ok {
		if len(o.owned) >= ownerCacheMax {
			clear(o.owned)
		}
		o.owned[sessionID] = struct{}{}
		return true
	}
	if len(o.foreign) >= ownerCacheMax {
		clear(o.foreign)
	}
	o.foreign[sessionID] = now.Add(foreignTTL)
	o.drop(now)
	return false
}

// drop logs without the session id or message body: both are agent-supplied.
func (o *signalingOwners) drop(now time.Time) {
	o.dropped++
	if now.Sub(o.lastLog) < foreignLogEvery {
		return
	}
	o.log.Warn("agent signaling for a session not placed on this host; frame dropped",
		"token", "agent-signaling-foreign-session", "host_id", o.hostID, "dropped", o.dropped)
	o.lastLog, o.dropped = now, 0
}
