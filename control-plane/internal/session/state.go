// Package session implements the session lifecycle: the seam between the control
// plane (scheduling and authoritative state) and the node agent (which runs the
// pipeline and reports progress). The state machine here is the single source of
// truth named by schema.md, agent-api.md and control-api.md — the control plane
// owns state and writes the row, and every agent report is validated against
// this machine before it is persisted.
package session

// State is a session lifecycle state. The permitted values are exactly the
// schema.md CHECK set; adding one needs a migration + sign-off.
type State string

const (
	// Row created, not yet placed. No resources reserved.
	StatePending State = "pending"
	// Scheduler chose host+GPU and reserved slots; assign sent.
	StateAssigned State = "assigned"
	// Agent acked assign and is bringing the pipeline up.
	StateStarting State = "starting"
	// Pipeline live, signaling offer available.
	StateRunning State = "running"
	// Teardown requested, pipeline coming down.
	StateStopping State = "stopping"
	// Terminated normally; reservation released.
	StateStopped State = "stopped"
	// Terminated abnormally; error_message set; reservation released.
	StateFailed State = "failed"
)

// IsTerminal states are final: no transition out of them is permitted.
func (s State) IsTerminal() bool {
	return s == StateStopped || s == StateFailed
}

// HoldsReservation is exactly the schema.md availability-sum filter, so a
// transition OUT of this set IS the reservation release; there is no separate
// release step. `stopping` must stay in the set (schema.md §Failure &
// reservation-release invariants #2): releasing there was the #489 half-open
// door, freeing a teardown-in-flight NVENC session's encode slot before the
// encoder was destroyed and admitting a replacement launch into the driver's
// use-after-free window.
func (s State) HoldsReservation() bool {
	return s == StateAssigned || s == StateStarting || s == StateRunning || s == StateStopping
}

// CoerceReport handles the teardown race: an operator DELETE moves the row to
// `stopping` BEFORE the agent tears down, the transport close drives the SCTP
// association into an error state, and the agent reports `failed` for that
// noise. An already-`stopping` session is a clean stop, so that report becomes
// `stopped` with no error_message (the non-SCTP detail is still kept in
// state_detail; see Transition). A `failed` report from any active state stays
// `failed`.
func CoerceReport(from, to State) State {
	if from == StateStopping && to == StateFailed {
		return StateStopped
	}
	return to
}

// CanTransition reports whether from → to is a permitted transition.
//
// The rules encode schema.md's machine:
//   - forward progress: pending → assigned → starting → running
//   - stop: any non-terminal → stopping → stopped (and a fast agent may report
//     stopped directly from an active state)
//   - failure: EVERY non-terminal state → failed (invariant #1: a stuck session
//     is a bug, not a designed state)
//   - terminal states are final
//   - a same-state report is an idempotent no-op (agents may re-report)
func CanTransition(from, to State) bool {
	if from == to {
		return true // idempotent re-report
	}
	if from.IsTerminal() {
		return false // terminal is final
	}
	switch to {
	case StateFailed:
		return true // invariant #1: reachable from every non-terminal state
	case StateStopping:
		return true
	case StateStopped:
		// From teardown, or directly from an active state: the agent's callback
		// is authoritative for progress.
		return from == StateStopping || from.HoldsReservation()
	case StateAssigned:
		return from == StatePending
	case StateStarting:
		return from == StateAssigned
	case StateRunning:
		// assigned→running is allowed for a fast agent that reports running before
		// its starting callback is observed.
		return from == StateAssigned || from == StateStarting
	default:
		return false
	}
}
