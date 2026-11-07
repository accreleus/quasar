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
	// ownerLogEvery spaces each per-connection drop and lookup-error log.
	ownerLogEvery = 10 * time.Second
	// ownerLookupTimeout bounds each of the two lookup attempts; the read loop waits on them.
	ownerLookupTimeout = 2 * time.Second
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

// throttledLog emits at most once per ownerLogEvery, carrying the count since the last line.
type throttledLog struct {
	last  time.Time
	count int
}

func (t *throttledLog) emit(now time.Time, log *slog.Logger, level slog.Level, msg string, args ...any) {
	t.count++
	if now.Sub(t.last) < ownerLogEvery {
		return
	}
	log.Log(context.Background(), level, msg, append(args, "count", t.count)...)
	t.last, t.count = now, 0
}

// signalingOwners decides whether one agent connection may relay signaling for a session id.
// Placement is the sessions.host_id the coordinator wrote when it inserted the row, before it
// sent session_assign; nothing updates it afterwards, so a positive answer is cached for the
// life of the connection. Read-loop only: no locking.
type signalingOwners struct {
	lookup  func(ctx context.Context, sessionID string) (bool, error)
	log     *slog.Logger
	hostID  string
	owned   map[string]struct{}
	foreign map[string]time.Time
	drops   throttledLog
	errs    throttledLog
	now     func() time.Time // test seam
}

func newSignalingOwners(store *agentStore, log *slog.Logger, hostID string) *signalingOwners {
	return &signalingOwners{
		lookup: func(ctx context.Context, sid string) (bool, error) { return store.sessionPlacedOn(ctx, sid, hostID) },
		log:    log, hostID: hostID,
		owned: map[string]struct{}{}, foreign: map[string]time.Time{}, now: time.Now,
	}
}

// allows reports whether the frame may be relayed. It answers true only for a canonically
// spelled id placed on this host: the relay keys its buffers on the raw string, and a ::uuid cast
// accepts other spellings of an owned id. A non-nil error means the lookup failed twice and the
// frame was not judged; the caller must report it, not drop it silently.
func (o *signalingOwners) allows(ctx context.Context, sessionID string) (bool, error) {
	now := o.now()
	if !canonicalPolicyUUID(sessionID) {
		o.drops.emit(now, o.log, slog.LevelWarn, "agent signaling for a malformed or non-canonical session id; frame dropped",
			"token", "agent-signaling-foreign-session", "host_id", o.hostID)
		return false, nil
	}
	if _, ok := o.owned[sessionID]; ok {
		return true, nil
	}
	if until, ok := o.foreign[sessionID]; ok && now.Before(until) {
		o.dropForeign(now)
		return false, nil
	}
	ok, err := o.query(ctx, sessionID)
	if err != nil {
		ok, err = o.query(ctx, sessionID)
	}
	if err != nil {
		o.errs.emit(now, o.log, slog.LevelError, "agent signaling placement lookup failed; session reported failed",
			"host_id", o.hostID, "err", err)
		return false, err
	}
	if ok {
		if len(o.owned) >= ownerCacheMax {
			clear(o.owned)
		}
		o.owned[sessionID] = struct{}{}
		return true, nil
	}
	if len(o.foreign) >= ownerCacheMax {
		clear(o.foreign)
	}
	o.foreign[sessionID] = now.Add(foreignTTL)
	o.dropForeign(now)
	return false, nil
}

func (o *signalingOwners) query(ctx context.Context, sessionID string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, ownerLookupTimeout)
	defer cancel()
	return o.lookup(ctx, sessionID)
}

// dropForeign logs without the session id or message body: both are agent-supplied.
func (o *signalingOwners) dropForeign(now time.Time) {
	o.drops.emit(now, o.log, slog.LevelWarn, "agent signaling for a session not placed on this host; frame dropped",
		"token", "agent-signaling-foreign-session", "host_id", o.hostID)
}
