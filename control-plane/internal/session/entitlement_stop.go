// entitlement_stop.go — ending sessions whose owner lost access to their app.
package session

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// StopReasonEntitlementRevoked is the session_stop reason (agent-api.md §session_stop).
const StopReasonEntitlementRevoked = "entitlement_revoked"

// entitlementSweepInterval bounds how long a session outlives its owner's
// entitlement when nothing swept at the moment it was lost (control-api.md
// amendment 23).
const entitlementSweepInterval = 30 * time.Second

// RunEntitlementSweep stops unentitled sessions every entitlementSweepInterval
// until ctx is cancelled. It is what makes the rule continuous: a swap that
// commits into a revoked app, a library sync's revoke, a route sweep that hit a
// DB error and a control-plane restart mid-revoke are all caught by a later tick.
//
// Its own ticker, not RunStaleSweep's: that one is switched off by a zero grace.
func (c *Coordinator) RunEntitlementSweep(ctx context.Context) {
	t := time.NewTicker(entitlementSweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := c.StopUnentitledSessions(ctx, ""); err != nil {
				c.log.Warn("entitlement sweep failed; the next tick retries", "err", err)
			}
		}
	}
}

// StopUnentitledSessions stops every live session whose owner IsEntitled no
// longer covers for the app it runs, and returns their ids (control-api.md
// amendment 23, #503). appID narrows it to that app and the tiles derived from
// it, for the admin routes that just removed access; "" checks every session.
//
// A launch racing an entitlement write holds FOR SHARE on the rows
// (scheduleAttempt), so its session exists by the time a sweep after the write
// lists. The stop applies only while the session still runs the app that was
// checked: one that swapped away in between is left for the next tick.
//
// Each returned session is `stopping` on return. The agent's ack is not awaited,
// so an admin's request never waits stopAckTimeout per session.
//
// ponytail: one IsEntitled query per live session per tick; batch it if a fleet
// ever holds hundreds of sessions.
func (c *Coordinator) StopUnentitledSessions(ctx context.Context, appID string) ([]string, error) {
	live, err := c.store.liveSessions(ctx, appID)
	if err != nil {
		return nil, err
	}
	var stopped []string
	var errs error
	for _, s := range live {
		entitled, err := c.store.IsEntitled(ctx, s.userID, s.appID)
		if err != nil {
			errs = errors.Join(errs, err)
			continue
		}
		if entitled {
			continue
		}
		sess, err := c.stop(ctx, s.id, s.appID, StopReasonEntitlementRevoked, false)
		if errors.Is(err, errAppChanged) {
			continue
		}
		if err != nil {
			errs = errors.Join(errs, fmt.Errorf("stop session %s: %w", s.id, err))
			continue
		}
		if sess.State != StateStopping {
			continue // went terminal on its own since the list
		}
		c.log.Info("session stopped: owner no longer entitled", "session_id", s.id, "app_id", s.appID)
		stopped = append(stopped, s.id)
	}
	return stopped, errs
}

type liveSession struct{ id, userID, appID string }

// liveSessions lists the sessions not yet stopping or terminal; a non-empty
// appID keeps those running it or a tile derived from it.
func (s *Store) liveSessions(ctx context.Context, appID string) ([]liveSession, error) {
	var filter *string
	if appID != "" {
		if !isValidUUID(appID) {
			return nil, nil
		}
		filter = &appID
	}
	rows, err := s.pool.Query(ctx, `
		SELECT s.id::text, s.user_id::text, s.app_id::text
		FROM sessions s JOIN apps a ON a.id = s.app_id
		WHERE s.state NOT IN ('stopping','stopped','failed')
		  AND ($1::uuid IS NULL OR a.id = $1::uuid OR a.parent_app_id = $1::uuid)
	`, filter)
	if err != nil {
		return nil, fmt.Errorf("list live sessions: %w", err)
	}
	defer rows.Close()
	var out []liveSession
	for rows.Next() {
		var ls liveSession
		if err := rows.Scan(&ls.id, &ls.userID, &ls.appID); err != nil {
			return nil, fmt.Errorf("scan live session: %w", err)
		}
		out = append(out, ls)
	}
	return out, rows.Err()
}
