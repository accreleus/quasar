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
}

func newSwapper(store *Store, dispatcher Dispatcher, log *slog.Logger, resolveHome func(context.Context, LaunchApp, string, string) ([]byte, error)) *swapper {
	return &swapper{store: store, dispatcher: dispatcher, log: log, resolveHome: resolveHome, pendingSwaps: make(map[string]string)}
}

// Swap validates that a running session is swappable and the new app fits its
// held reservation, marks it running+swapping, and dispatches session_swap_app.
// The swap then proceeds asynchronously via AgentState callbacks.
//
// Errors leave the session untouched: ErrNotFound (404),
// ErrSessionNotSwappable (409), ErrSwapExceedsReservation (409).
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
	if app.ManagedHome {
		conflictID, err := s.store.HasLiveUserAppSession(ctx, sess.UserID, homeAppID(app), sessionID)
		if err != nil {
			return Session{}, fmt.Errorf("home in use check: %w", err)
		}
		if conflictID != "" {
			return Session{}, &HomeInUseError{SessionID: conflictID}
		}
	}

	// A managed-home swap target gets its home injected exactly like a launch.
	// Resolved BEFORE the swapping detail is set, so a failure leaves the session
	// untouched.
	dispatchSpec, err := s.resolveHome(ctx, app, sess.UserID, *sess.HostID)
	if err != nil {
		return Session{}, fmt.Errorf("home mount: %w", err)
	}

	// Mark swapping + remember the target; app_id stays the OLD app until commit.
	if err := s.store.SetStateDetail(ctx, sessionID, swapDetailInProgress); err != nil {
		return Session{}, err
	}
	s.mu.Lock()
	s.pendingSwaps[sessionID] = newAppID
	s.mu.Unlock()

	go s.dispatchSwap(*sess.HostID, sessionID, dispatchSpec)

	sess.StateDetail = strptr(swapDetailInProgress)
	return sess, nil
}

// dispatchSwap sends session_swap_app and waits for the ack. A rejected or
// undeliverable swap is a no-op: clear the pending swap, revert the detail, and
// the session keeps running its previous app — a rejected swap must never fail
// the session (agent-api.md). On accept, progress arrives via AgentState.
func (s *swapper) dispatchSwap(hostID, sessionID string, runtimeSpec []byte) {
	app := runtimeSpec
	if len(app) == 0 {
		app = []byte("{}")
	}
	cmd := agentws.SessionSwapAppCmd{Type: "session_swap_app", ID: newCmdID(), SessionID: sessionID, App: app}
	ctx, cancel := context.WithTimeout(context.Background(), swapAckTimeout)
	defer cancel()
	res, err := s.dispatcher.SendWithAck(ctx, hostID, cmd.ID, cmd)
	if err != nil || !res.OK {
		reason := "agent unreachable"
		if err == nil {
			reason = res.Error
		}
		s.log.Warn("swap rejected/undeliverable", "session_id", sessionID, "reason", reason)
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
	s.mu.Unlock()
}

func (s *swapper) clearPendingSwap(sessionID string) {
	s.mu.Lock()
	delete(s.pendingSwaps, sessionID)
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
		s.clearPendingSwap(m.SessionID)
		if err := s.store.CommitSwappedApp(ctx, m.SessionID, newAppID, m.Detail); err != nil {
			s.log.Error("commit swapped app failed", "session_id", m.SessionID, "err", err)
		} else {
			s.log.Info("swap committed", "session_id", m.SessionID, "app_id", newAppID)
		}
	default:
		// Any other running detail while pending: record it, never touch app_id.
		_ = s.store.SetStateDetail(ctx, m.SessionID, m.Detail)
	}
	return true
}
