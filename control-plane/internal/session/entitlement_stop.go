// entitlement_stop.go — ending the sessions an entitlement removal left unentitled.
package session

import (
	"context"
	"errors"
	"fmt"
)

// StopReasonEntitlementRevoked is the session_stop reason (agent-api.md §session_stop).
const StopReasonEntitlementRevoked = "entitlement_revoked"

// StopUnentitledSessions stops every live session running appID, or a tile
// derived from it, whose owner IsEntitled no longer covers, and returns their ids
// (control-api.md amendment 23, #503).
//
// Call it after the entitlement write commits. A launch racing that write holds
// FOR SHARE on the rows (scheduleAttempt), so its session exists by the time this
// lists; a swap racing it is caught at commit (stopIfSwapLeftUnentitled).
//
// Each returned session is `stopping` on return. The agent's ack is not awaited,
// so the admin's request never waits stopAckTimeout per session.
func (c *Coordinator) StopUnentitledSessions(ctx context.Context, appID string) ([]string, error) {
	live, err := c.store.liveSessionsOfApp(ctx, appID)
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
		if _, err := c.stop(ctx, s.id, StopReasonEntitlementRevoked, false); err != nil {
			errs = errors.Join(errs, fmt.Errorf("stop session %s: %w", s.id, err))
			continue
		}
		c.log.Info("session stopped: owner no longer entitled", "session_id", s.id, "app_id", s.appID)
		stopped = append(stopped, s.id)
	}
	return stopped, errs
}

// stopIfSwapLeftUnentitled closes the swap side of the revoke race: Swap checks
// the target's entitlement before dispatch, and app_id only moves at commit, so a
// revoke landing in between lists the session under its old app. Runs on the
// agent read loop, so the stop must not await an ack.
func (c *Coordinator) stopIfSwapLeftUnentitled(ctx context.Context, sessionID string) {
	sess, err := c.store.Get(ctx, sessionID)
	if err != nil || sess.State.IsTerminal() || sess.State == StateStopping {
		return
	}
	entitled, err := c.store.IsEntitled(ctx, sess.UserID, sess.AppID)
	if err != nil {
		c.log.Error("swap commit entitlement re-check failed", "session_id", sessionID, "err", err)
		return
	}
	if entitled {
		return
	}
	if _, err := c.stop(ctx, sessionID, StopReasonEntitlementRevoked, false); err != nil {
		c.log.Error("stop session swapped into a revoked app failed", "session_id", sessionID, "err", err)
		return
	}
	c.log.Info("session stopped: swapped into an app its owner lost", "session_id", sessionID, "app_id", sess.AppID)
}

type liveSession struct{ id, userID, appID string }

// liveSessionsOfApp lists the sessions not yet stopping or terminal that run
// appID or a tile derived from it.
func (s *Store) liveSessionsOfApp(ctx context.Context, appID string) ([]liveSession, error) {
	if !isValidUUID(appID) {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT s.id::text, s.user_id::text, s.app_id::text
		FROM sessions s JOIN apps a ON a.id = s.app_id
		WHERE (a.id = $1::uuid OR a.parent_app_id = $1::uuid)
		  AND s.state NOT IN ('stopping','stopped','failed')
	`, appID)
	if err != nil {
		return nil, fmt.Errorf("list app sessions: %w", err)
	}
	defer rows.Close()
	var out []liveSession
	for rows.Next() {
		var ls liveSession
		if err := rows.Scan(&ls.id, &ls.userID, &ls.appID); err != nil {
			return nil, fmt.Errorf("scan app session: %w", err)
		}
		out = append(out, ls)
	}
	return out, rows.Err()
}
