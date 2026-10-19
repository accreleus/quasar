// warm_steam.go — warm Steam (amendment 39, #759): parked sessions, the claim,
// park on exit, eviction and expiry. Semantics: control-api.md §Warm Steam.
package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
)

const (
	KindPlay   = "play"
	KindParked = "parked"

	// session_stop reasons that stop a parked session. Recorded in stop_reason
	// and never served (clientStopReason).
	StopReasonParkedExpired    = "parked_expired"
	StopReasonParkedEvicted    = "parked_evicted"
	StopReasonParkedSuperseded = "parked_superseded"
	// stopReasonRuntimeReleased marks a play row whose held runtime the control
	// plane released; the launch path reads it while the cleanup proof is owed.
	stopReasonRuntimeReleased = "runtime_released"
	// parkCooldown keeps a heartbeat from re-parking into room an eviction freed
	// for a refused launch's retry.
	parkCooldown = "5 minutes"

	rewarmNextSteamLaunch = "next_steam_launch"
	parkedSteamFlags      = "-silent"
	claimedDetail         = "claimed"
	steamDialogsEnvKey    = "QUASAR_STEAM_DIALOGS"
	steamDialogsDismiss   = "dismiss_safe"
	// parkedClaimRetryAfterSeconds: the parked Steam is stopping and holds the
	// home only for its stop window, and the web client already retries.
	parkedClaimRetryAfterSeconds = "2"
)

// errParkRefused: a rule refused a parked session. The heartbeat swallows it.
var errParkRefused = errors.New("no parked session allowed now")

// ParkedClaimRefusedError is a launch that must wait for a warm-Steam runtime:
// a parked session it could not adopt (being stopped, or still warming), or a
// released held runtime whose cleanup proof is owed. That runtime holds the
// home, so the launch answers capacity_exhausted with Retry-After: 2 rather than
// home_in_use naming a session the caller cannot see.
type ParkedClaimRefusedError struct {
	HolderID string
	// Warming: the parked session is not yet running and is left to finish.
	Warming bool
}

func (e *ParkedClaimRefusedError) Error() string {
	if e.Warming {
		return "parked session is still starting"
	}
	return "a warm-Steam runtime still holds the home; it is stopping"
}

func (e *ParkedClaimRefusedError) Unwrap() error { return ErrCapacityExhausted }

// warmSteam is the instance_settings half of the feature, read per request.
type warmSteam struct {
	Enabled     bool
	Rewarm      string
	IdleMinutes int
}

// readWarmSteam reads the three settings; a missing singleton reads as off.
func readWarmSteam(ctx context.Context, q rowQuerier) (warmSteam, error) {
	var w warmSteam
	err := q.QueryRow(ctx, `SELECT steam_warm_enabled, steam_warm_rewarm, steam_warm_idle_minutes
		FROM instance_settings WHERE id = true`).Scan(&w.Enabled, &w.Rewarm, &w.IdleMinutes)
	if errors.Is(err, pgx.ErrNoRows) {
		return warmSteam{Rewarm: "library_open", IdleMinutes: 10}, nil
	}
	if err != nil {
		return warmSteam{}, fmt.Errorf("read warm steam settings: %w", err)
	}
	return w, nil
}

// dispatcherParks is parked_sessions_v1 on one command epoch, or on the host's
// current connection when there is no epoch that can say.
func dispatcherParks(d Dispatcher, hostID string, epoch agentws.HomeCommandEpoch) bool {
	if e, ok := epoch.(interface{ SupportsParkedSessions() bool }); ok {
		return e.SupportsParkedSessions()
	}
	p, ok := d.(interface{ SupportsParkedSessions(string) bool })
	return ok && p.SupportsParkedSessions(hostID)
}

func (c *Coordinator) hostParks(hostID string) bool {
	return dispatcherParks(c.dispatcher, hostID, nil)
}

func (c *Coordinator) epochParks(hostID string, epoch agentws.HomeCommandEpoch) bool {
	return dispatcherParks(c.dispatcher, hostID, epoch)
}

// warmOn is steam_warm_enabled for a dispatch; a failed read is off.
func (c *Coordinator) warmOn() bool {
	w, err := readWarmSteam(c.ctx, c.store.pool)
	if err != nil {
		c.log.Warn("warm steam: settings read failed at dispatch; sending no kind", "err", err)
	}
	return err == nil && w.Enabled
}

// wireAssign fits an assign to its connection. kind goes only where
// parked_sessions_v1 was advertised and only while warm Steam is on (with it
// off no runtime is held); adoption only where advertised. False means the
// assign cannot be sent: a parked session, or an adoption, to a connection
// that cannot take it, failing closed rather than orphaning the runtime.
func wireAssign(a agentws.SessionAssignCmd, parks, warmOn bool) (agentws.SessionAssignCmd, bool) {
	if !parks && a.AdoptSessionID != "" {
		return a, false
	}
	if a.Kind == KindParked && (!parks || !warmOn) {
		return a, false
	}
	switch {
	case !parks || !warmOn:
		a.Kind = ""
	case a.Kind == "":
		a.Kind = KindPlay
	}
	return a, true
}

// --- the library heartbeat ----------------------------------------------------

// SteamWarmHeartbeat is PUT /v1/me/steam-warm: start a parked session when
// every rule allows one, then extend the caller's parked_until. It reports
// nothing: the route always answers 204.
func (c *Coordinator) SteamWarmHeartbeat(ctx context.Context, userID string, isAdmin bool, deviceID string) {
	w, err := readWarmSteam(ctx, c.store.pool)
	if err != nil {
		c.log.Warn("warm steam: settings read failed", "err", err)
		return
	}
	if w.Enabled && deviceID != "" {
		appID, err := c.store.warmSteamApp(ctx, userID)
		switch {
		case err != nil:
			c.log.Warn("warm steam: eligibility read failed", "user_id", userID, "err", err)
		case appID != "":
			_, err := c.LaunchByProfile(ctx, userID, LaunchParams{
				AppID: appID, IsAdmin: isAdmin, DeviceID: deviceID, Mic: true, parked: true,
			})
			if err != nil {
				c.log.Debug("warm steam: no parked session", "user_id", userID, "err", err)
			}
		}
	}
	// Extend only a parked session that still qualifies; stop the rest.
	if _, err := c.stopParked(ctx, StopReasonParkedSuperseded, parkedFilter{UserID: userID, Unqualified: true}); err != nil {
		c.log.Warn("warm steam: stopping an unqualified parked session failed", "user_id", userID, "err", err)
	}
	if _, err := c.store.pool.Exec(ctx, `UPDATE sessions
		SET parked_until = now() + make_interval(mins => $2::int)
		WHERE user_id = $1::uuid AND kind = 'parked' AND state IN ('pending','assigned','starting','running')`,
		userID, w.IdleMinutes); err != nil {
		c.log.Warn("warm steam: parked_until extend failed", "user_id", userID, "err", err)
	}
}

// warmSteamApp is the Steam parent app a heartbeat would warm for this user,
// or "" when a rule outside the scheduling transaction refuses: not exactly
// one visible Steam parent, no managed home for it yet, a live session of any
// kind, suppression, or a zero quota. The transaction rechecks the racy ones.
// The visibility predicate is entitledSQL (internal/crud) for a parent app.
func (s *Store) warmSteamApp(ctx context.Context, userID string) (string, error) {
	if !isValidUUID(userID) {
		return "", nil
	}
	var (
		n                 int
		appID             *string
		hasHome, busy, ok bool
	)
	err := s.pool.QueryRow(ctx, `
		WITH steam AS (
			SELECT a.id FROM apps a
			WHERE a.enabled AND a.parent_app_id IS NULL AND a.library_provider = 'steam'
			  AND EXISTS (SELECT 1 FROM entitlements e WHERE e.app_id = a.id
			        AND (e.subject_type = 'all' OR (e.subject_type = 'user' AND e.subject_id = $1::uuid)))
		)
		SELECT (SELECT count(*) FROM steam), (SELECT id::text FROM steam LIMIT 1),
		       EXISTS (SELECT 1 FROM user_homes uh JOIN steam ON steam.id = uh.app_id
		               WHERE uh.user_id = u.id AND uh.gc_after IS NULL),
		       EXISTS (SELECT 1 FROM sessions s WHERE s.user_id = u.id AND s.state NOT IN ('stopped','failed')),
		       NOT u.steam_warm_suppressed AND u.max_concurrent_sessions > 0 AND u.disabled_at IS NULL
		FROM users u WHERE u.id = $1::uuid`, userID).Scan(&n, &appID, &hasHome, &busy, &ok)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read warm steam eligibility: %w", err)
	}
	if n != 1 || !hasHome || busy || !ok {
		return "", nil
	}
	return *appID, nil
}

// parkAllowed is the heartbeat's recheck under the user lock: the switch, no
// live session of any kind, no suppression. Returns the idle minutes.
func parkAllowed(ctx context.Context, tx pgx.Tx, userID string) (int32, error) {
	w, err := readWarmSteam(ctx, tx)
	if err != nil {
		return 0, err
	}
	var busy, suppressed, evicted bool
	if err := tx.QueryRow(ctx, `SELECT
		EXISTS (SELECT 1 FROM sessions WHERE user_id = $1::uuid AND state NOT IN ('stopped','failed')),
		steam_warm_suppressed,
		EXISTS (SELECT 1 FROM sessions WHERE user_id = $1::uuid AND kind = 'parked'
		        AND stop_reason = 'parked_evicted' AND COALESCE(ended_at, now()) > now() - $2::interval)
		FROM users WHERE id = $1::uuid`, userID, parkCooldown).Scan(&busy, &suppressed, &evicted); err != nil {
		return 0, fmt.Errorf("recheck warm steam: %w", err)
	}
	if !w.Enabled || busy || suppressed || evicted {
		return 0, errParkRefused
	}
	return int32(w.IdleMinutes), nil
}

// parkedSpec is the dispatch spec of a parked session: its Steam parent's,
// with Steam started -silent.
func (c *Coordinator) parkedSpec(ctx context.Context, app LaunchApp, userID, hostID string) ([]byte, error) {
	spec, err := c.resolveHomeSpec(ctx, app, userID, hostID)
	if err != nil {
		return nil, err
	}
	return injectSteamFlags(spec, parkedSteamFlags)
}

// applySteamDialogs sets QUASAR_STEAM_DIALOGS=dismiss_safe on a Steam-family
// spec when the user chose it, and removes the key otherwise. Only the fixed
// value ever reaches the app. A spec without the key and with `show` is left
// byte-identical.
func (c *Coordinator) applySteamDialogs(ctx context.Context, spec []byte, userID string) ([]byte, error) {
	dialogs, err := c.store.GetSteamDialogs(ctx, userID)
	if err != nil {
		return nil, err
	}
	if dialogs == steamDialogsDismiss {
		v := steamDialogsDismiss
		return setSpecEnv(spec, steamDialogsEnvKey, &v)
	}
	var probe struct {
		Env map[string]json.RawMessage `json:"env"`
	}
	if json.Unmarshal(spec, &probe) != nil || probe.Env[steamDialogsEnvKey] == nil {
		return spec, nil
	}
	return setSpecEnv(spec, steamDialogsEnvKey, nil)
}

// --- the claim ----------------------------------------------------------------

// parkedRef is the caller's live parked session, as a launch first reads it.
type parkedRef struct {
	ID       string
	AppID    string
	HostID   string
	GPUIndex int32
}

// liveParked is the user's non-terminal parked session, zero when none.
func (s *Store) liveParked(ctx context.Context, userID string) (parkedRef, error) {
	var k parkedRef
	var host *string
	var idx *int32
	err := s.pool.QueryRow(ctx, `SELECT s.id::text, s.app_id::text, s.host_id::text, g.index
		FROM sessions s LEFT JOIN gpus g ON g.id = s.gpu_id
		WHERE s.user_id = $1::uuid AND s.kind = 'parked'
		  AND s.state IN ('pending','assigned','starting','running','stopping')`, userID).
		Scan(&k.ID, &k.AppID, &host, &idx)
	if errors.Is(err, pgx.ErrNoRows) {
		return parkedRef{}, nil
	}
	if err != nil {
		return parkedRef{}, fmt.Errorf("read parked session: %w", err)
	}
	k.HostID = deref(host)
	if idx != nil {
		k.GPUIndex = *idx
	}
	return k, nil
}

type claimedParked struct {
	id       string
	hostID   string
	gpuIndex int32
}

// claimParked locks the parked row and runs claim checks 1–4; on success it
// moves the row to stopped "claimed", releasing its reservation, so check 5 is
// the pinned placement that follows in the same transaction. The checks do not
// read steam_warm_enabled: with the switch off a claim still adopts, and the
// expiry job stops whatever is left within 60 s.
func claimParked(ctx context.Context, tx pgx.Tx, p CreateParams) (*claimedParked, error) {
	k := claimedParked{id: p.claim.id}
	var state State
	var others, inDeadline bool
	err := tx.QueryRow(ctx, `
		SELECT k.host_id::text, g.index, k.state, k.parked_until > now(),
		       k.user_id = $2::uuid AND k.app_id = $3::uuid AND k.kind = 'parked'
		       AND h.status = 'online' AND h.agent_disconnected_at IS NULL
		       AND NOT EXISTS (SELECT 1 FROM host_admission_restrictions ar WHERE ar.host_id = h.id)
		       AND k.codec = COALESCE(NULLIF($4, ''), 'h264') AND k.fps = $5 AND k.bitrate_kbps = $6
		       AND k.h264_profile = $7 AND k.mic = $8 AND k.width >= $9 AND k.height >= $10
		FROM sessions k JOIN gpus g ON g.id = k.gpu_id JOIN hosts h ON h.id = k.host_id
		WHERE k.id = $1::uuid
		FOR UPDATE OF k`,
		k.id, p.UserID, p.homeAppID(), p.Codec, p.FPS, p.BitrateKbps, p.H264Profile, p.Mic, p.Width, p.Height).
		Scan(&k.hostID, &k.gpuIndex, &state, &inDeadline, &others)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, &ParkedClaimRefusedError{HolderID: k.id}
	}
	if err != nil {
		return nil, fmt.Errorf("lock parked session: %w", err)
	}
	others = others && p.claim.hostParks
	// Only check 2 failing on a session still warming leaves it to finish.
	if others && inDeadline && (state == StatePending || state == StateAssigned || state == StateStarting) {
		return nil, &ParkedClaimRefusedError{HolderID: k.id, Warming: true}
	}
	if !(others && inDeadline && state == StateRunning) {
		return nil, &ParkedClaimRefusedError{HolderID: k.id}
	}
	if _, err := tx.Exec(ctx, `UPDATE sessions SET state = 'stopped', state_detail = $2,
		ended_at = now(), launch_progress = NULL WHERE id = $1::uuid`, k.id, claimedDetail); err != nil {
		return nil, fmt.Errorf("claim parked session: %w", err)
	}
	return &k, nil
}

// adoptedByLive reports whether a non-terminal row adopts id's runtime. Until
// it ends, id is neither an orphan nor a hold to reconcile.
func (s *Store) adoptedByLive(ctx context.Context, id string) bool {
	var ok bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM sessions WHERE adopt_session_id = $1::uuid
		AND state NOT IN ('stopped','failed'))`, id).Scan(&ok)
	return err == nil && ok
}

// adoptRuntime records that row `to` adopts the runtime of `from` and copies
// its managed-home binding, in the caller's transaction. The hold stays on
// `from` until `to`'s first agent report (transition) or `from`'s cleanup
// proof (clearQualifiedHomeHolds): an assign alone adopts nothing.
func adoptRuntime(ctx context.Context, tx pgx.Tx, from, to string) error {
	if _, err := tx.Exec(ctx, `UPDATE sessions t SET adopt_session_id = f.id,
		    managed_home_id = f.managed_home_id, managed_home_mount_sha256 = f.managed_home_mount_sha256
		FROM sessions f WHERE f.id = $1::uuid AND t.id = $2::uuid`, from, to); err != nil {
		return fmt.Errorf("record adoption: %w", err)
	}
	return nil
}

// --- stopping parked sessions -------------------------------------------------

type parkedStop struct{ id, hostID, userID string }

// parkedFilter narrows stopParked; an empty field matches every row.
type parkedFilter struct {
	ID, HostID, UserID string
	ExceptApp          string // keep this Steam family
	// Expired: parked_until has passed, or the Steam parent is disabled.
	Expired bool
	// Unqualified: the parent is no longer enabled and visible to the user, or
	// the user's quota is 0.
	Unqualified bool
}

// orNil turns "" into SQL NULL, the "no filter" of stopParked.
func orNil(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// stopParkedWhere moves the matching live parked rows to `stopping` with
// reason, in one statement.
func (s *Store) stopParkedWhere(ctx context.Context, reason string, f parkedFilter) ([]parkedStop, error) {
	rows, err := s.pool.Query(ctx, `UPDATE sessions SET state = 'stopping', state_detail = 'stop requested',
		    stop_reason = COALESCE(stop_reason, $1), launch_progress = NULL
		WHERE kind = 'parked' AND state IN ('pending','assigned','starting','running')
		  AND ($2::uuid IS NULL OR id = $2::uuid)
		  AND ($3::uuid IS NULL OR host_id = $3::uuid)
		  AND ($4::uuid IS NULL OR user_id = $4::uuid)
		  AND ($5::uuid IS NULL OR app_id <> $5::uuid)
		  AND (NOT $6::boolean OR parked_until <= now()
		       OR NOT EXISTS (SELECT 1 FROM apps a WHERE a.id = sessions.app_id AND a.enabled))
		  AND (NOT $7::boolean OR NOT EXISTS (
		        SELECT 1 FROM apps a JOIN users u ON u.id = sessions.user_id
		        WHERE a.id = sessions.app_id AND a.enabled AND a.library_provider = 'steam'
		          AND u.max_concurrent_sessions > 0
		          AND EXISTS (SELECT 1 FROM entitlements e WHERE e.app_id = a.id
		                AND (e.subject_type = 'all' OR (e.subject_type = 'user' AND e.subject_id = sessions.user_id)))))
		RETURNING id::text, COALESCE(host_id::text, ''), user_id::text`,
		reason, orNil(f.ID), orNil(f.HostID), orNil(f.UserID), orNil(f.ExceptApp), f.Expired, f.Unqualified)
	if err != nil {
		return nil, fmt.Errorf("stop parked sessions: %w", err)
	}
	var out []parkedStop
	for rows.Next() {
		var k parkedStop
		if err := rows.Scan(&k.id, &k.hostID, &k.userID); err != nil {
			rows.Close()
			return nil, fmt.Errorf("stop parked sessions: %w", err)
		}
		out = append(out, k)
	}
	rows.Close()
	return out, rows.Err()
}

// sendParkedStops tells each agent to tear down. Send, not SendWithAck: some
// callers run on an agent read loop, and AgentHeartbeat re-sends a lost stop.
func (c *Coordinator) sendParkedStops(stops []parkedStop, reason string) {
	for _, k := range stops {
		c.noteStopRequested(k.id)
		if k.hostID == "" {
			continue
		}
		cmd := agentws.SessionStopCmd{Type: "session_stop", ID: newCmdID(), SessionID: k.id, Reason: reason}
		if err := c.dispatcher.Send(k.hostID, cmd); err != nil {
			c.log.Warn("parked session stop dispatch failed", "session_id", k.id, "err", err)
		}
	}
}

func (c *Coordinator) stopParked(ctx context.Context, reason string, f parkedFilter) (int, error) {
	stops, err := c.store.stopParkedWhere(ctx, reason, f)
	if err != nil {
		return 0, err
	}
	c.sendParkedStops(stops, reason)
	return len(stops), nil
}

// ExpireParked is the session.parked_expire job: stop every parked session
// past parked_until or whose Steam parent is disabled, and every one while the
// switch is off.
func (c *Coordinator) ExpireParked(ctx context.Context) (int, error) {
	w, err := readWarmSteam(ctx, c.store.pool)
	if err != nil {
		return 0, err
	}
	return c.stopParked(ctx, StopReasonParkedExpired, parkedFilter{Expired: w.Enabled})
}

// StopParkedOnHost stops a host's parked sessions as a drain or an apply
// cordons it: they are nobody's session to wait for.
func (c *Coordinator) StopParkedOnHost(ctx context.Context, hostID string) error {
	if !isValidUUID(hostID) {
		return nil
	}
	_, err := c.stopParked(ctx, "host_draining", parkedFilter{HostID: hostID})
	return err
}

// supersedeParked stops the user's parked session unless it belongs to the
// Steam family keepHomeApp (a Steam parent's id; "" keeps none), and suppresses
// warm starts when steam_warm_rewarm says so. onlyID narrows it to one row.
func (c *Coordinator) supersedeParked(ctx context.Context, userID, keepHomeApp, onlyID string) {
	if !isValidUUID(userID) {
		return
	}
	stops, err := c.store.stopParkedWhere(ctx, StopReasonParkedSuperseded,
		parkedFilter{UserID: userID, ExceptApp: keepHomeApp, ID: onlyID})
	if err != nil {
		c.log.Warn("warm steam: supersede failed", "user_id", userID, "err", err)
		return
	}
	if len(stops) == 0 {
		return
	}
	c.sendParkedStops(stops, StopReasonParkedSuperseded)
	w, err := readWarmSteam(ctx, c.store.pool)
	if err == nil && w.Rewarm == rewarmNextSteamLaunch {
		_, err = c.store.pool.Exec(ctx, `UPDATE users SET steam_warm_suppressed = true WHERE id = $1::uuid`, userID)
	}
	if err != nil {
		c.log.Warn("warm steam: suppression write failed", "user_id", userID, "err", err)
	}
}

// afterLaunchAdmitted applies the two warm-Steam side effects of an admitted
// launch into app: a Steam launch clears suppression, and anything outside the
// parked session's family supersedes it. Always run: the launch's earlier
// parked-session read is outside the user lock and can be stale.
func (c *Coordinator) afterLaunchAdmitted(ctx context.Context, userID string, app LaunchApp) {
	keep := ""
	if app.SteamFamily {
		keep = homeAppID(app)
		if _, err := c.store.pool.Exec(ctx, `UPDATE users SET steam_warm_suppressed = false
			WHERE id = $1::uuid AND steam_warm_suppressed`, userID); err != nil {
			c.log.Warn("warm steam: suppression clear failed", "user_id", userID, "err", err)
		}
	}
	c.supersedeParked(ctx, userID, keep, "")
}

// --- eviction -----------------------------------------------------------------

// evictParkedFor stops the oldest parked session whose released reservation
// would let p fit. The caller still refuses its own attempt.
func (c *Coordinator) evictParkedFor(ctx context.Context, p CreateParams) {
	if p.ManagedHome && p.PinHostID == "" {
		if owner, err := c.store.HomeHostForApp(ctx, p.UserID, p.homeAppID()); err == nil && owner != "" {
			p.PinHostID = owner
		}
	}
	cand := candidacy{p: p, readiness: c.store.readiness}
	q, args := cand.evictionQuery()
	var id, owner string
	err := c.store.pool.QueryRow(ctx, q, args...).Scan(&id, &owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return
	}
	if err != nil {
		c.log.Warn("warm steam: eviction query failed", "err", err)
		return
	}
	if owner == p.UserID {
		c.supersedeParked(ctx, p.UserID, "", id)
		return
	}
	if _, err := c.stopParked(ctx, StopReasonParkedEvicted, parkedFilter{ID: id}); err != nil {
		c.log.Warn("warm steam: eviction failed", "session_id", id, "err", err)
	}
}

// --- park on exit ---------------------------------------------------------------

// endHeldRuntime handles a `stopped` report with runtime_held: it ends the play
// session and, in the same transaction, parks its runtime or releases it. A
// release leaves the hold on the ended row; only the agent's later qualified
// `stopped` clears it. Runs on the agent's read loop, so it sends without
// waiting for acks.
func (c *Coordinator) endHeldRuntime(ctx context.Context, hostID string, m agentws.SessionStateMsg) {
	var detail *string
	if m.Detail != "" {
		detail = &m.Detail
	}
	ended, parked, err := c.store.endHeldPlay(ctx, hostID, m.SessionID, detail, c.hostParks(hostID))
	if errors.Is(err, ErrNotFound) {
		c.log.Warn("ignoring session state from non-owner host", "session_id", m.SessionID)
		return
	}
	if err != nil {
		c.log.Error("apply held-runtime stop failed", "session_id", m.SessionID, "err", err)
	}
	if ended != nil {
		c.afterTerminal(*ended, "agent")
	}
	if parked == nil {
		// "user_requested": the user's game exit ended it; the reason is informational.
		cmd := agentws.SessionStopCmd{Type: "session_stop", ID: newCmdID(), SessionID: m.SessionID, Reason: "user_requested"}
		if err := c.dispatcher.Send(hostID, cmd); err != nil {
			c.log.Warn("held runtime release dispatch failed", "session_id", m.SessionID, "err", err)
		}
		return
	}
	c.log.Info("session parked", "session_id", parked.ID, "adopts", m.SessionID, "host_id", hostID)
	parked.AdoptSessionID = m.SessionID
	go c.dispatchParked(*parked)
}

// dispatchParked sends a parked row its assign and start.
func (c *Coordinator) dispatchParked(k Session) {
	app, err := c.store.GetLaunchApp(c.ctx, k.AppID)
	if err != nil || k.HostID == nil {
		c.failBeforeAgent(k.ID, k.AdoptSessionID, k.HostID, fmt.Sprintf("parked session app: %v", err))
		return
	}
	spec, err := c.parkedSpec(c.ctx, app, k.UserID, *k.HostID)
	if err != nil {
		c.failBeforeAgent(k.ID, k.AdoptSessionID, k.HostID, fmt.Sprintf("home mount: %v", err))
		return
	}
	expected := expectedHomeDispatch(app)
	c.dispatchAssignStart(k, spec, &expected)
}

// endHeldPlay stores the play session's `stopped` and decides the park in one
// transaction: a parked row for the Steam parent on the same GPU, taking the
// ended session's reservation and home binding (the hold follows at its first
// report, adoptRuntime). ended is nil when the row was already terminal;
// parked is nil for a release.
func (s *Store) endHeldPlay(ctx context.Context, hostID, sessionID string, detail *string, hostParks bool) (ended, parked *Session, err error) {
	if !isValidUUID(sessionID) {
		return nil, nil, ErrNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("begin held stop: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// User lock first, as scheduleAttempt takes it: a heartbeat cannot then
	// create a second parked session beside this one.
	var userID string
	if err := tx.QueryRow(ctx, `SELECT user_id::text FROM sessions WHERE id = $1::uuid`, sessionID).Scan(&userID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, ErrNotFound
		}
		return nil, nil, fmt.Errorf("read held session: %w", err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1, hashtext($2::text))`, lockNamespaceUser, userID); err != nil {
		return nil, nil, fmt.Errorf("lock user: %w", err)
	}
	var cur State
	var kind string
	var host *string
	if err := tx.QueryRow(ctx, `SELECT state, kind, host_id::text FROM sessions WHERE id = $1::uuid FOR UPDATE`,
		sessionID).Scan(&cur, &kind, &host); err != nil {
		return nil, nil, fmt.Errorf("lock held session: %w", err)
	}
	if host == nil || *host != hostID {
		return nil, nil, ErrNotFound
	}
	if cur.IsTerminal() {
		return nil, nil, nil
	}
	end, err := scanSession(tx.QueryRow(ctx, `UPDATE sessions SET state = 'stopped',
		    state_detail = COALESCE($2, state_detail), launch_progress = NULL,
		    ended_at = COALESCE(ended_at, now())
		WHERE id = $1::uuid RETURNING `+sessionCols, sessionID, detail))
	if err != nil {
		return nil, nil, fmt.Errorf("stop held session: %w", err)
	}

	w, err := readWarmSteam(ctx, tx)
	if err != nil {
		return nil, nil, err
	}
	// A stopping row was stopped by the control plane: never parked. The agent
	// sends no stopping before a held stop (agent-api.md §session_state).
	if !(w.Enabled && hostParks && cur == StateRunning && kind == KindPlay) {
		if err := releaseHeld(ctx, tx, sessionID); err != nil {
			return nil, nil, err
		}
		return &end, nil, nil
	}
	// A savepoint, so a failed park still stores the stop. The parent must be a
	// visible Steam parent, the user must hold nothing else live, and the host
	// must be online and uncordoned.
	sp, err := tx.Begin(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("begin park: %w", err)
	}
	k, err := scanSession(sp.QueryRow(ctx, `
		INSERT INTO sessions (
		    user_id, app_id, host_id, gpu_id, state,
		    width, height, fps, bitrate_kbps, h264_profile, codec,
		    profile_id, stream_profile_id, playout0_ms, reserved_encode_slots,
		    mic, device_id, assigned_at, kind, parked_until)
		SELECT p.user_id, root.id, p.host_id, p.gpu_id, 'assigned',
		       p.width, p.height, p.fps, p.bitrate_kbps, p.h264_profile, p.codec,
		       p.profile_id, p.stream_profile_id, p.playout0_ms, p.reserved_encode_slots,
		       p.mic, p.device_id, now(), 'parked', now() + make_interval(mins => $2::int)
		FROM sessions p
		JOIN apps a ON a.id = p.app_id
		JOIN apps root ON root.id = COALESCE(a.parent_app_id, a.id)
		JOIN hosts h ON h.id = p.host_id
		WHERE p.id = $1::uuid
		  AND root.enabled AND root.library_provider = 'steam'
		  AND EXISTS (SELECT 1 FROM entitlements e WHERE e.app_id = root.id
		        AND (e.subject_type = 'all' OR (e.subject_type = 'user' AND e.subject_id = p.user_id))
		        FOR SHARE)
		  AND NOT EXISTS (SELECT 1 FROM sessions o WHERE o.user_id = p.user_id AND o.id <> p.id
		        AND o.state NOT IN ('stopped','failed'))
		  AND h.status = 'online' AND h.agent_disconnected_at IS NULL
		  AND NOT EXISTS (SELECT 1 FROM host_admission_restrictions ar WHERE ar.host_id = h.id)
		RETURNING `+sessionCols, sessionID, w.IdleMinutes))
	if err == nil {
		err = adoptRuntime(ctx, sp, sessionID, k.ID)
	}
	if err == nil {
		err = sp.QueryRow(ctx, `SELECT index FROM gpus WHERE id = $1::uuid`, deref(k.GPUID)).Scan(&k.GPUIndex)
	}
	if err == nil {
		err = sp.Commit(ctx)
	}
	if err == nil {
		if err := tx.Commit(ctx); err != nil {
			return nil, nil, fmt.Errorf("commit held stop: %w", err)
		}
		return &end, &k, nil
	}
	if !errors.Is(err, ErrNotFound) {
		_ = sp.Rollback(ctx)
		slog.Warn("park on exit failed; releasing the runtime", "session_id", sessionID, "err", err)
	}
	if err := releaseHeld(ctx, tx, sessionID); err != nil {
		return nil, nil, err
	}
	return &end, nil, nil
}

// releaseHeld marks the ended row runtime_released, so a relaunch onto its
// still-held home answers 503 until the cleanup proof, then commits.
func releaseHeld(ctx context.Context, tx pgx.Tx, sessionID string) error {
	if err := markReleased(ctx, tx, sessionID); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit held stop: %w", err)
	}
	return nil
}

// markReleased writes runtime_released over any earlier reason but the served
// entitlement_revoked: a user stop racing the game exit must not lose the marker
// (schema.md, sessions.stop_reason).
func markReleased(ctx context.Context, tx pgx.Tx, sessionID string) error {
	if _, err := tx.Exec(ctx, `UPDATE sessions SET stop_reason = CASE
		    WHEN stop_reason = $3 THEN stop_reason ELSE $2 END
		WHERE id = $1::uuid`, sessionID, stopReasonRuntimeReleased, StopReasonEntitlementRevoked); err != nil {
		return fmt.Errorf("mark released runtime: %w", err)
	}
	return nil
}

// failBeforeAgent fails a session before the agent adopted anything for it.
// The hold is still on the runtime it was to adopt, which only that runtime's
// own cleanup proof clears; the stop here is a best-effort hurry, and the
// heartbeat and ReconcilePendingHomes repeat it once no live row adopts it.
func (c *Coordinator) failBeforeAgent(sessionID, adoptID string, hostID *string, reason string) {
	c.failSession(sessionID, reason)
	if adoptID == "" || hostID == nil {
		return
	}
	cmd := agentws.SessionStopCmd{Type: "session_stop", ID: newCmdID(), SessionID: adoptID, Reason: "error"}
	if err := c.dispatcher.Send(*hostID, cmd); err != nil {
		c.log.Warn("stopping an unadopted runtime failed", "session_id", adoptID, "err", err)
	}
}

// --- shader progress ------------------------------------------------------------

// validLaunchProgress accepts exactly {"step":"shaders","percent":<0–100>}.
func validLaunchProgress(raw json.RawMessage) (json.RawMessage, bool) {
	var keys map[string]json.RawMessage
	if json.Unmarshal(raw, &keys) != nil || len(keys) != 2 {
		return nil, false
	}
	var step string
	var percent *int // a pointer, so null is refused rather than read as 0
	if json.Unmarshal(keys["step"], &step) != nil || step != "shaders" ||
		json.Unmarshal(keys["percent"], &percent) != nil || percent == nil || *percent < 0 || *percent > 100 {
		return nil, false
	}
	out, _ := json.Marshal(map[string]any{"step": step, "percent": *percent})
	return out, true
}

// SetLaunchProgress stores accepted shader progress while the session is still
// booting on hostID; any other row is left alone.
func (s *Store) SetLaunchProgress(ctx context.Context, id, hostID string, progress json.RawMessage) error {
	if !isValidUUID(id) || !isValidUUID(hostID) {
		return nil
	}
	_, err := s.pool.Exec(ctx, `UPDATE sessions SET launch_progress = $3
		WHERE id = $1::uuid AND host_id = $2::uuid AND state = 'running' AND state_detail = $4`,
		id, hostID, []byte(progress), appDetailBooting)
	if err != nil {
		return fmt.Errorf("set launch progress: %w", err)
	}
	return nil
}

// evictionQuery finds the oldest live parked session on a GPU this launch could
// use whose released reservation would make room. The candidacy gates minus
// the live-VRAM veto, which the parked session's own memory would satisfy.
func (c candidacy) evictionQuery() (string, []any) {
	a := &argset{}
	slotsIdx := a.add(c.p.NeedEncodeSlots)
	pin := c.pinGate(a)
	image := c.imageGate(a, "\n\t\t  AND ")
	gate := c.readinessGate(a, "\n\t\t  AND ")
	codec := c.codecGate(a, "\n\t\t  AND ")
	placement := c.placementGate(a)

	return `
		SELECT k.id::text, k.user_id::text
		FROM sessions k
		JOIN gpus g ON g.id = k.gpu_id
		JOIN hosts h ON h.id = g.host_id
		WHERE k.kind = 'parked' AND k.state IN ('assigned','starting','running')
		  AND h.status = 'online' AND h.capacity_detection = 'ok' AND h.config_policy_gate_connection IS NULL AND g.reported` + unrestrictedHostSQL + liveHostSQL + consoleAccessHoldSQL + schedulableBindingSQL + pin + image + gate + codec + placement + `
		  AND g.encode_slots_total + k.reserved_encode_slots
		        - COALESCE((SELECT SUM(x.reserved_encode_slots) FROM sessions x
		                    WHERE x.gpu_id = g.id AND x.state IN ` + activeStatesSQL + `), 0) >= $` + fmt.Sprint(slotsIdx) + `
		ORDER BY k.created_at, k.id
		LIMIT 1
	`, a.args()
}
