package session

import "testing"

// These tests are pure (no database) so the state machine — the load-bearing
// contract — is verified on every `go test`, with or without TEST_DATABASE_URL.

func TestForwardPath(t *testing.T) {
	path := []State{StatePending, StateAssigned, StateStarting, StateRunning, StateStopping, StateStopped}
	for i := 0; i+1 < len(path); i++ {
		if !CanTransition(path[i], path[i+1]) {
			t.Errorf("forward %s → %s should be allowed", path[i], path[i+1])
		}
	}
}

func TestFailedReachableFromEveryNonTerminal(t *testing.T) {
	// Invariant #1: failed is reachable from every non-terminal state.
	for _, s := range []State{StatePending, StateAssigned, StateStarting, StateRunning, StateStopping} {
		if !CanTransition(s, StateFailed) {
			t.Errorf("%s → failed must be allowed (invariant #1)", s)
		}
	}
}

func TestTerminalIsFinal(t *testing.T) {
	for _, term := range []State{StateStopped, StateFailed} {
		for _, to := range []State{StatePending, StateAssigned, StateStarting, StateRunning, StateStopping, StateStopped, StateFailed} {
			want := term == to // only the idempotent no-op is allowed
			if got := CanTransition(term, to); got != want {
				t.Errorf("terminal %s → %s: got %v want %v", term, to, got, want)
			}
		}
	}
}

func TestStopFromAnyNonTerminal(t *testing.T) {
	for _, s := range []State{StatePending, StateAssigned, StateStarting, StateRunning} {
		if !CanTransition(s, StateStopping) {
			t.Errorf("%s → stopping should be allowed", s)
		}
	}
}

func TestIllegalBackwardTransitions(t *testing.T) {
	illegal := [][2]State{
		{StateRunning, StateStarting},
		{StateRunning, StateAssigned},
		{StateRunning, StatePending},
		{StateStarting, StateAssigned},
		{StateStarting, StatePending},
		{StateAssigned, StatePending},
		{StateStopping, StateRunning},
		{StatePending, StateStarting}, // must go through assigned
		{StatePending, StateRunning},
	}
	for _, tc := range illegal {
		if CanTransition(tc[0], tc[1]) {
			t.Errorf("%s → %s should be rejected", tc[0], tc[1])
		}
	}
}

func TestIdempotentSameState(t *testing.T) {
	for _, s := range []State{StatePending, StateAssigned, StateStarting, StateRunning, StateStopping, StateStopped, StateFailed} {
		if !CanTransition(s, s) {
			t.Errorf("%s → %s (same) should be an allowed no-op", s, s)
		}
	}
}

func TestReservationHeldExactlyForActiveStates(t *testing.T) {
	// Must match the schema.md availability-sum filter precisely.
	want := map[State]bool{
		StatePending:  false,
		StateAssigned: true,
		StateStarting: true,
		StateRunning:  true,
		// schema.md: `stopping` holds until the terminal callback ("released on
		// terminal") — the early release was the #489 half-open door (a launch
		// admitted against a slot whose NVENC encoder was still being destroyed).
		StateStopping: true,
		StateStopped:  false,
		StateFailed:   false,
	}
	for s, w := range want {
		if got := s.HoldsReservation(); got != w {
			t.Errorf("%s.HoldsReservation(): got %v want %v", s, got, w)
		}
	}
}

func TestCoerceReportStoppingFailedIsCleanStop(t *testing.T) {
	// The teardown race: an operator-terminated (stopping) session that then sees
	// a transport error (SCTP association error on pipeline teardown) must land
	// stopped, not failed.
	if got := CoerceReport(StateStopping, StateFailed); got != StateStopped {
		t.Errorf("CoerceReport(stopping, failed): got %s want %s", got, StateStopped)
	}
}

func TestCoerceReportLeavesGenuineFailuresUntouched(t *testing.T) {
	// A failed report from any ACTIVE (non-stopping) state is a real failure and
	// must NOT be coerced — do not weaken failure detection for running sessions.
	for _, from := range []State{StatePending, StateAssigned, StateStarting, StateRunning} {
		if got := CoerceReport(from, StateFailed); got != StateFailed {
			t.Errorf("CoerceReport(%s, failed): got %s want failed (genuine failure)", from, got)
		}
	}
	// Non-failed reports are never coerced, including in the stopping window.
	for _, to := range []State{StateStopped, StateStopping, StateRunning} {
		if got := CoerceReport(StateStopping, to); got != to {
			t.Errorf("CoerceReport(stopping, %s): got %s want %s (unchanged)", to, got, to)
		}
	}
}

func TestTerminalClassification(t *testing.T) {
	for s, want := range map[State]bool{
		StatePending: false, StateAssigned: false, StateStarting: false,
		StateRunning: false, StateStopping: false, StateStopped: true, StateFailed: true,
	} {
		if got := s.IsTerminal(); got != want {
			t.Errorf("%s.IsTerminal(): got %v want %v", s, got, want)
		}
	}
}
