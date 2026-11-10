// swapper.go — the launcher/game app-swap lifecycle.
package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
)

// swapper owns the app-swap lifecycle and its in-flight state.
type swapper struct {
	store       *Store
	dispatcher  Dispatcher
	log         *slog.Logger
	resolveSpec func(ctx context.Context, app LaunchApp, sess Session) ([]byte, error) // = Coordinator.swapSpec
	// supersede stops a parked warm-Steam session that holds the home a swap
	// target needs, as a launch does (= Coordinator.supersedeParkedHolder).
	supersede func(ctx context.Context, userID, parkedID, cause string)

	mu           sync.Mutex
	pendingSwaps map[string]string
	// pendingShared is the Steam home each in-flight swap's target mounts, ""
	// when it shares nothing. It is the one thing the agent's "swap complete"
	// cannot tell us, and the commit needs it to record (and release) the
	// session's shared home. pendingSharedHold is that Steam claim's hold when
	// THIS swap took it — the only case a proven pre-handoff failure may release
	// it in; the own hold's Created flag says nothing about it.
	pendingShared     map[string]string
	pendingSharedHold map[string]*HomeHoldDecision
	// swapSince is when this process last began writing each session's durable
	// `swapping` guard; see unresolved. Kept until the session ends (forget): a
	// rejected or rolled-back swap clears its pending entry before the row leaves
	// `swapping`, and must not read as unresolved in between.
	swapSince map[string]time.Time
}

func newSwapper(store *Store, dispatcher Dispatcher, log *slog.Logger, resolveSpec func(context.Context, LaunchApp, Session) ([]byte, error),
	supersede func(context.Context, string, string, string)) *swapper {
	return &swapper{store: store, dispatcher: dispatcher, log: log, resolveSpec: resolveSpec, supersede: supersede,
		pendingSwaps: make(map[string]string), pendingShared: make(map[string]string),
		pendingSharedHold: make(map[string]*HomeHoldDecision), swapSince: make(map[string]time.Time)}
}

// noteSwapStart must run before the durable `swapping` guard is written, or a
// sweep between the two reads a healthy swap as unresolved.
func (s *swapper) noteSwapStart(sessionID string) {
	s.mu.Lock()
	s.swapSince[sessionID] = time.Now()
	s.mu.Unlock()
}

// unresolved reports whether a row still in the durable `swapping` guard has no
// swap this process can finish: none on record (a restart lost pendingSwaps, so
// the agent's "swap complete" can no longer commit app_id) or one older than
// swapUnresolvedAfter (its commit failed or its callback never came). The row's
// app_id is then not known to be what the agent runs.
func (s *swapper) unresolved(sessionID string) bool {
	s.mu.Lock()
	since, ok := s.swapSince[sessionID]
	s.mu.Unlock()
	return !ok || time.Since(since) > swapUnresolvedAfter
}

// Swap validates that a running session is swappable and the new app fits its
// held reservation, marks it running+swapping, and dispatches session_swap_app.
// The swap then proceeds asynchronously via AgentState callbacks.
//
// Validation errors leave the session untouched. A managed-home mount error
// occurs after the durable claim/guard; the guard is cleared only because no
// target command has left the control plane, while the claim remains reserved.
func (s *swapper) Swap(ctx context.Context, sessionID, newAppID string) (Session, error) {
	sess, err := s.store.Get(ctx, sessionID)
	if err != nil {
		return Session{}, err
	}
	sessionID = sess.ID // canonical spelling: pendingSwaps and swapSince are keyed on it (#1100)
	// Swappable: top-level running, no swap already in progress.
	if sess.State != StateRunning {
		return Session{}, ErrSessionNotSwappable
	}
	if sess.StateDetail != nil && *sess.StateDetail == swapDetailInProgress {
		return Session{}, ErrSessionNotSwappable
	}
	if sess.HostID == nil {
		return Session{}, ErrSessionNotSwappable
	}

	app, err := s.store.GetLaunchApp(ctx, newAppID)
	if err != nil {
		return Session{}, err // ErrNotFound: unknown or disabled app
	}

	// Entitlement gate on the SWAP TARGET (§6.3 by extension): a swap is a launch
	// of a different app into a live session, so ungated it defeats the launch
	// check in two requests. Against the session's OWNER, with no role bypass.
	//
	// A plain read with no FOR SHARE and no enclosing transaction, so a revoke
	// committing before the dispatch is not serialized against. A swap that
	// loses that race completes and RunEntitlementSweep stops it.
	entitled, err := s.store.IsEntitled(ctx, sess.UserID, app.ID)
	if err != nil {
		return Session{}, err
	}
	if !entitled {
		return Session{}, ErrNotEntitled
	}
	// The swap must fit the held reservation; there is no resize. Slots only
	// (#383): sessions now reserve 0 MB, so comparing declared VRAM would reject
	// every swap into an app with any default_vram_mb at all.
	if app.DefaultEncodeSlots > sess.ReservedSlots {
		return Session{}, ErrSwapExceedsReservation
	}

	// Single-writer guard for managed-home swap targets (P5-04). The swapping
	// session excludes itself: a same-app swap of the only session stays allowed.
	//
	// Keyed on homeAppID(app), never app.ID. Otherwise the launch-path guard is
	// defeated in two requests: launch the launcher tile, then swap into a derived
	// tile of the same parent — the launch guard never sees the second app, and
	// this one would compare two different app ids and pass.
	//
	// app.ManagedHome is the PARENT's for a tile (GetLaunchApp resolved it), so
	// the gate fires; the tile's own column is false by CHECK.
	var homeEpoch agentws.HomeCommandEpoch
	var homeHold *HomeHoldDecision
	var sharedHome string
	var sharedHold *HomeHoldDecision
	// A swap target shares the user's Steam library exactly as a launch does
	// (control-api.md §Storage, "A shared Steam library"): the same single-Steam
	// choice, and — inside the guard transaction below — the same claim, the same
	// one-Steam-at-a-time check and the same hold, so the decision, the check and
	// the claim are one step.
	sharedApp, sharedNotice, err := s.store.sharedHomeChoice(ctx, app, sess.UserID, false)
	if err != nil {
		return Session{}, err
	}
	if app.ManagedHome {
		var epoch agentws.HomeCommandEpoch
		if provider, ok := s.dispatcher.(interface {
			CurrentHomeCommandEpoch(string) (agentws.HomeCommandEpoch, bool)
		}); ok {
			var connected bool
			epoch, connected = provider.CurrentHomeCommandEpoch(*sess.HostID)
			if !connected {
				return Session{}, ErrSessionNotSwappable
			}
		}
		conflict, err := s.store.HasLiveUserAppSession(ctx, sess.UserID, homeAppID(app), sessionID)
		if err != nil {
			return Session{}, fmt.Errorf("home in use check: %w", err)
		}
		if conflict != nil {
			return Session{}, conflict
		}
		s.noteSwapStart(sessionID)
		guard, err := s.store.GuardHomeForSwapWithHold(ctx, sessionID, sess.UserID, app,
			*sess.HostID, epoch != nil && epoch.SupportsHomeCleanup(), sharedApp)
		if err != nil {
			// The target's own home is only ever held by a named session (the
			// pre-check above catches it first), so an UNNAMED holder here is the
			// Steam claim the target's shared home needed: answered as a launch
			// answers a parked Steam.
			var inUse *HomeInUseError
			if sharedApp != "" && errors.As(err, &inUse) && (inUse.parkedID != "" || inUse.releasedID != "") {
				return Session{}, s.sharedHomeBusy(ctx, sess.UserID, inUse)
			}
			return Session{}, fmt.Errorf("swap home location: %w", err)
		}
		homeEpoch = epoch
		homeHold = guard.hold
		sharedHome = guard.sharedID
		sharedHold = guard.sharedHold
		if sharedApp != "" && sharedHome == "" {
			sharedNotice = guard.notice
		}
	}
	if sharedNotice != "" && sharedHome == "" {
		// A swap response carries no notice, so this is where "the library is on
		// another host" or "no Steam app to share" is recorded.
		s.log.Info("swap target runs without the Steam library", "session_id", sessionID, "notice", sharedNotice)
	}

	// Two tiles of one Steam parent can switch games inside the running Steam
	// (control-api.md §Warm Steam, "Quick switch"); the agent decides whether it
	// can, and the connection whether to ask.
	inPlace := false
	if app.IsDerived() && app.SteamFamily && app.ID != sess.AppID {
		cur, err := s.store.GetLaunchApp(ctx, sess.AppID)
		inPlace = err == nil && cur.ParentAppID == app.ParentAppID
	}

	// A swap target gets its home, its shared home and its region values exactly
	// like a launch. Its durable swapping detail was set with the claim before
	// this resolution. The shared home rides on the spec only: the session row
	// records it at the commit, so a rejection or a rollback never claims one.
	specSess := sess
	specSess.SharedHomeAppID = nil
	if sharedHome != "" {
		specSess.SharedHomeAppID = &sharedHome
	}
	dispatchSpec, err := s.resolveSpec(ctx, app, specSess)
	if err != nil {
		if app.ManagedHome {
			_ = s.store.ClearNewHomeHold(ctx, homeHold)
			// The Steam hold and the Steam claim this swap took go too: no
			// target payload has left the control plane, so the home the claim
			// pins this host to was never mounted. A failed release retains
			// conservative GC protection.
			_ = s.store.ClearNewSharedHomeHold(ctx, sharedHold)
			if relErr := s.store.releaseUnusedSwapSharedClaim(ctx, sessionID, sharedHome); relErr != nil {
				s.log.Warn("releasing an unused swap shared home claim failed", "session_id", sessionID, "err", relErr)
			}
			_ = s.store.SetStateDetail(ctx, sessionID, swapDetailRejected)
		}
		return Session{}, fmt.Errorf("home mount: %w", err)
	}

	// Mark swapping + remember the target; app_id stays the OLD app until commit.
	if !app.ManagedHome {
		s.noteSwapStart(sessionID)
		if err := s.store.GuardPlacementForSwap(ctx, sessionID, homeAppID(app), *sess.HostID); err != nil {
			return Session{}, err
		}
	}
	s.mu.Lock()
	s.pendingSwaps[sessionID] = newAppID
	s.pendingShared[sessionID] = sharedHome
	s.pendingSharedHold[sessionID] = sharedHold
	s.mu.Unlock()

	go s.dispatchSwap(*sess.HostID, sessionID, dispatchSpec, app.ManagedHome, inPlace,
		sess.UserID, homeAppID(app), homeEpoch, homeHold)

	sess.StateDetail = strptr(swapDetailInProgress)
	return sess, nil
}

// sharedHomeBusy turns a shared-home conflict into the swap's answer. An
// unnamed holder is the caller's own warm-Steam runtime: it is superseded, as a
// launch supersedes it, and the swap answers the retryable home_in_use ("still
// shutting down") so the client retries once it has stopped. A named one is a
// live Steam-family session, refused with reason steam_game_running.
func (s *swapper) sharedHomeBusy(ctx context.Context, userID string, conflict *HomeInUseError) error {
	if conflict.parkedID != "" || conflict.releasedID != "" {
		if conflict.parkedID != "" && s.supersede != nil {
			s.supersede(ctx, userID, conflict.parkedID, "")
		}
		return &HomeInUseError{Stopping: true}
	}
	conflict.forShared = true
	return conflict
}

// dispatchSwap sends session_swap_app and waits for the ack. An explicit agent
// rejection clears the pending target, and so does a frame proven never to have
// left the control plane. Any other transport error leaves the pending target
// and the `swapping` marker in place, for a managed and an unmanaged target
// alike: the agent may have accepted a command whose ack was lost, and its
// "swap complete" must still be able to commit app_id. A swap it never ran is
// ended by the sweep (swapUnresolvedAfter). On accept, progress arrives via
// AgentState.
func (s *swapper) dispatchSwap(hostID, sessionID string, runtimeSpec []byte, managedHome, inPlace bool,
	userID, canonicalAppID string, epoch agentws.HomeCommandEpoch, hold *HomeHoldDecision) {
	app := runtimeSpec
	if len(app) == 0 {
		app = []byte("{}")
	}
	cmd := agentws.SessionSwapAppCmd{Type: "session_swap_app", ID: newCmdID(), SessionID: sessionID, App: app}
	// Only an epoch reports whether the frame was queued. Swap takes one for a
	// managed home; an unmanaged target gets its own here. provable stays false
	// for a dispatcher with no epochs, where no error proves non-delivery.
	provable := epoch != nil
	if epoch == nil {
		if provider, ok := s.dispatcher.(interface {
			CurrentHomeCommandEpoch(string) (agentws.HomeCommandEpoch, bool)
		}); ok {
			provable = true
			epoch, _ = provider.CurrentHomeCommandEpoch(hostID)
		}
	}
	var res agentws.AckResult
	var err error
	var queued bool
	for attempt := 0; attempt < 3; attempt++ {
		cmd.InPlace = inPlace && dispatcherParks(s.dispatcher, hostID, epoch)
		ctx, cancel := context.WithTimeout(context.Background(), swapAckTimeout)
		switch {
		case epoch != nil:
			res, queued, err = epoch.SendWithAck(ctx, cmd.ID, cmd)
		case provable:
			err = agentws.ErrAgentNotConnected // no connection to hand the frame to
		default:
			res, err = s.dispatcher.SendWithAck(ctx, hostID, cmd.ID, cmd)
			queued = err == nil
		}
		cancel()
		if !managedHome || epoch == nil || err == nil || queued {
			break
		}
		provider, ok := s.dispatcher.(interface {
			CurrentHomeCommandEpoch(string) (agentws.HomeCommandEpoch, bool)
		})
		if !ok {
			break
		}
		next, connected := provider.CurrentHomeCommandEpoch(hostID)
		if !connected {
			break
		}
		refreshed, refreshErr := s.store.RefreshSwapHomeHold(context.Background(), sessionID,
			userID, canonicalAppID, hostID, hold, next.SupportsHomeCleanup(), s.pendingSharedHoldFor(sessionID))
		if refreshErr != nil {
			s.log.Warn("swap home epoch refresh failed", "err", refreshErr)
			break
		}
		hold, epoch = refreshed, next
	}
	if err != nil || !res.OK {
		reason := "agent unreachable"
		if err == nil {
			reason = res.Error
		}
		s.log.Warn("swap rejected/undeliverable", "session_id", sessionID, "reason", reason)
		if err != nil && (!provable || queued) {
			// A timeout/lost ack does not prove that the agent never accepted
			// the swap. Retain the pending target, the marker and any home hold
			// after queue handoff, or when a legacy dispatcher cannot prove the
			// frame stayed out of its queue.
			return
		}
		if err == nil && !res.OK || err != nil && !queued {
			if clearErr := s.store.ClearNewHomeHold(context.Background(), hold); clearErr != nil {
				s.log.Warn("unstarted swap home hold release failed", "err", clearErr)
			}
			// The Steam hold this swap newly took, and the Steam claim it would
			// have mounted: the target never ran, so neither may outlive it. A
			// hold the session already carried stays — it belongs to the app the
			// session still runs.
			if clearErr := s.store.ClearNewSharedHomeHold(context.Background(), s.pendingSharedHoldFor(sessionID)); clearErr != nil {
				s.log.Warn("unstarted swap shared home hold release failed", "err", clearErr)
			}
			if relErr := s.store.releaseUnusedSwapSharedClaim(context.Background(), sessionID, s.pendingSharedFor(sessionID)); relErr != nil {
				s.log.Warn("releasing an unused swap shared home claim failed", "session_id", sessionID, "err", relErr)
			}
		}
		s.clearPendingSwap(sessionID)
		if e := s.store.SetStateDetail(context.Background(), sessionID, swapDetailRejected); e != nil {
			s.log.Warn("revert swap detail failed", "session_id", sessionID, "err", e)
		}
		return
	}
	s.log.Info("swap accepted by agent", "session_id", sessionID)
}

// forget drops a session's pending-swap entry at a TERMINAL transition (#405),
// the analogue of healthEvaluator.forget at the same four sites. clearPendingSwap
// covers the swap protocol's own edges; every other way a session ends falls
// through handleSwapCallback's non-running arm and would orphan the entry for
// the life of the process. Kept separate because that one is a protocol step and
// this is lifecycle hygiene.
func (s *swapper) forget(sessionID string) {
	s.mu.Lock()
	delete(s.pendingSwaps, sessionID)
	delete(s.pendingShared, sessionID)
	delete(s.pendingSharedHold, sessionID)
	delete(s.swapSince, sessionID)
	s.mu.Unlock()
}

func (s *swapper) clearPendingSwap(sessionID string) {
	s.mu.Lock()
	delete(s.pendingSwaps, sessionID)
	delete(s.pendingShared, sessionID)
	delete(s.pendingSharedHold, sessionID)
	s.mu.Unlock()
}

// pendingSharedFor is the Steam home this session's in-flight swap mounts, ""
// when none (or no swap is pending).
func (s *swapper) pendingSharedFor(sessionID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pendingShared[sessionID]
}

// pendingSharedHoldFor is the hold this session's in-flight swap newly took on
// its shared Steam claim, nil when it took none.
func (s *swapper) pendingSharedHoldFor(sessionID string) *HomeHoldDecision {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pendingSharedHold[sessionID]
}

// handleSwapCallback processes an agent session_state callback for a session
// with a swap in flight, returning true if it consumed it. The swap rides within
// `running`: "swapping" is in progress, "swap complete" commits app_id, a
// "rolled back" detail keeps it. A `failed` state clears the pending swap and
// falls through to the normal terminal path, releasing the reservation.
func (s *swapper) handleSwapCallback(ctx context.Context, m agentws.SessionStateMsg) bool {
	s.mu.Lock()
	newAppID, pending := s.pendingSwaps[m.SessionID]
	newShared := s.pendingShared[m.SessionID]
	s.mu.Unlock()
	if !pending {
		return false
	}

	to := State(m.State)
	if to == StateFailed {
		s.clearPendingSwap(m.SessionID)
		return false
	}
	if to != StateRunning {
		return false
	}

	switch {
	case m.Detail == swapDetailInProgress:
		_ = s.store.SetStateDetail(ctx, m.SessionID, swapDetailInProgress)
	case strings.Contains(m.Detail, swapDetailRolledBack):
		s.clearPendingSwap(m.SessionID)
		_ = s.store.SetStateDetail(ctx, m.SessionID, m.Detail) // keep app_id
		s.log.Warn("swap rolled back", "session_id", m.SessionID, "detail", m.Detail)
	case m.Detail == swapDetailComplete:
		if err := s.store.CommitSwappedApp(ctx, m.SessionID, newAppID, newShared, m.Detail); err != nil {
			s.log.Error("commit swapped app failed", "session_id", m.SessionID, "err", err)
		} else {
			s.clearPendingSwap(m.SessionID)
			s.log.Info("swap committed", "session_id", m.SessionID, "app_id", newAppID, "shared_home_app_id", newShared)
		}
	default:
		// Any other running detail while pending (app booting, app presented) is
		// consumed and not recorded. The durable `swapping` marker must outlive it
		// and clear only on a commit or a rollback: while it is set app_id names
		// the old app, and the entitlement sweep, the home GC and Store.transition
		// all read it to know that.
	}
	return true
}
