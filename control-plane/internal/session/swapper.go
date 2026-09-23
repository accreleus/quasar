// swapper.go — the launcher/game app-swap lifecycle.
package session

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
)

// swapper owns the app-swap lifecycle and its in-flight state.
type swapper struct {
	store       *Store
	dispatcher  Dispatcher
	log         *slog.Logger
	resolveHome func(ctx context.Context, app LaunchApp, userID, hostID string) ([]byte, error) // = Coordinator.resolveHomeSpec

	mu           sync.Mutex
	pendingSwaps map[string]string
	pendingHome  map[string]bool
}

func newSwapper(store *Store, dispatcher Dispatcher, log *slog.Logger, resolveHome func(context.Context, LaunchApp, string, string) ([]byte, error)) *swapper {
	return &swapper{store: store, dispatcher: dispatcher, log: log, resolveHome: resolveHome,
		pendingSwaps: make(map[string]string), pendingHome: make(map[string]bool)}
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
	// Accepted residual: a plain read with no FOR SHARE and no enclosing
	// transaction, so a revoke committing before the dispatch is not serialized
	// against. Every step to dispatchSwap is a separate statement and a revoke
	// does not terminate a running session either. Closing it means making the
	// whole swap transactional; do not fix it here in isolation.
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
		conflictID, err := s.store.HasLiveUserAppSession(ctx, sess.UserID, homeAppID(app), sessionID)
		if err != nil {
			return Session{}, fmt.Errorf("home in use check: %w", err)
		}
		if conflictID != "" {
			return Session{}, &HomeInUseError{SessionID: conflictID}
		}
		hold, err := s.store.GuardHomeForSwapWithHold(ctx, sessionID, sess.UserID, app,
			*sess.HostID, epoch != nil && epoch.SupportsHomeCleanup())
		if err != nil {
			return Session{}, fmt.Errorf("swap home location: %w", err)
		}
		homeEpoch = epoch
		homeHold = hold
	}

	// A managed-home swap target gets its home injected exactly like a launch.
	// Its durable swapping detail was set with the claim before this resolution.
	dispatchSpec, err := s.resolveHome(ctx, app, sess.UserID, *sess.HostID)
	if err != nil {
		if app.ManagedHome {
			_ = s.store.ClearNewHomeHold(ctx, homeHold)
			// No target payload has left the control plane. Clearing here is
			// safe; a failed clear retains conservative GC protection.
			_ = s.store.SetStateDetail(ctx, sessionID, swapDetailRejected)
		}
		return Session{}, fmt.Errorf("home mount: %w", err)
	}

	// Mark swapping + remember the target; app_id stays the OLD app until commit.
	if !app.ManagedHome {
		if err := s.store.SetStateDetail(ctx, sessionID, swapDetailInProgress); err != nil {
			return Session{}, err
		}
	}
	s.mu.Lock()
	s.pendingSwaps[sessionID] = newAppID
	s.pendingHome[sessionID] = app.ManagedHome
	s.mu.Unlock()

	go s.dispatchSwap(*sess.HostID, sessionID, dispatchSpec, app.ManagedHome,
		sess.UserID, homeAppID(app), homeEpoch, homeHold)

	sess.StateDetail = strptr(swapDetailInProgress)
	return sess, nil
}

// dispatchSwap sends session_swap_app and waits for the ack. An explicit agent
// rejection clears the pending target. A managed-home transport error leaves
// the guard in place: the agent may have accepted a command whose ack was lost.
// On accept, progress arrives via AgentState.
func (s *swapper) dispatchSwap(hostID, sessionID string, runtimeSpec []byte, managedHome bool,
	userID, canonicalAppID string, epoch agentws.HomeCommandEpoch, hold *HomeHoldDecision) {
	app := runtimeSpec
	if len(app) == 0 {
		app = []byte("{}")
	}
	cmd := agentws.SessionSwapAppCmd{Type: "session_swap_app", ID: newCmdID(), SessionID: sessionID, App: app}
	var res agentws.AckResult
	var err error
	var queued bool
	for attempt := 0; attempt < 3; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), swapAckTimeout)
		if epoch != nil {
			res, queued, err = epoch.SendWithAck(ctx, cmd.ID, cmd)
		} else {
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
			userID, canonicalAppID, hostID, hold, next.SupportsHomeCleanup())
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
		if err != nil && managedHome && (epoch == nil || queued) {
			// A timeout/lost ack does not prove that the agent never accepted
			// the swap. Retain the hold after queue handoff or when a legacy
			// dispatcher cannot prove the frame stayed out of its queue.
			return
		}
		if err == nil && !res.OK || err != nil && !queued {
			if clearErr := s.store.ClearNewHomeHold(context.Background(), hold); clearErr != nil {
				s.log.Warn("unstarted swap home hold release failed", "err", clearErr)
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
	delete(s.pendingHome, sessionID)
	s.mu.Unlock()
}

func (s *swapper) clearPendingSwap(sessionID string) {
	s.mu.Lock()
	delete(s.pendingSwaps, sessionID)
	delete(s.pendingHome, sessionID)
	s.mu.Unlock()
}

// handleSwapCallback processes an agent session_state callback for a session
// with a swap in flight, returning true if it consumed it. The swap rides within
// `running`: "swapping" is in progress, "swap complete" commits app_id, a
// "rolled back" detail keeps it. A `failed` state clears the pending swap and
// falls through to the normal terminal path, releasing the reservation.
func (s *swapper) handleSwapCallback(ctx context.Context, m agentws.SessionStateMsg) bool {
	s.mu.Lock()
	newAppID, pending := s.pendingSwaps[m.SessionID]
	managedHome := s.pendingHome[m.SessionID]
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
		if err := s.store.CommitSwappedApp(ctx, m.SessionID, newAppID, m.Detail); err != nil {
			s.log.Error("commit swapped app failed", "session_id", m.SessionID, "err", err)
		} else {
			s.clearPendingSwap(m.SessionID)
			s.log.Info("swap committed", "session_id", m.SessionID, "app_id", newAppID)
		}
	default:
		// Any other running detail while pending: record it, never touch app_id.
		if !managedHome {
			_ = s.store.SetStateDetail(ctx, m.SessionID, m.Detail)
		}
	}
	return true
}
