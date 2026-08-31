package session

import "time"

// HealthState is a session's computed stream-health classification.
//
// Health is observational: derived from telemetry the agent already reports
// (abr_setpoint_kbps) against the rung's ABR floor. It must NEVER change the
// stream — the only action it takes is failing a session that has been
// unsustainable too long (see Evaluate). Resolution, fps and profile are
// preserved; health is a signal to the user, not a reconfiguration.
type HealthState string

const (
	// The stream is sustainable, or has no floor to enforce. Default at launch.
	HealthHealthy HealthState = "healthy"
	// ABR has held at/below the floor for a sustained window.
	HealthNetworkDegrading HealthState = "network_degrading"
	// ABR pinned at the floor: quality cannot drop further within the profile.
	HealthABRAtFloor HealthState = "abr_at_floor"
	// The client cannot decode at the offered rate; set from browser telemetry.
	HealthClientDecodeDegrading HealthState = "client_decode_degrading"
	// The client cannot present smoothly; set from browser telemetry.
	HealthClientPresentationDegrading HealthState = "client_presentation_degrading"
	// Below floor so long the stream is judged unsustainable: the session is
	// failed and the user prompted to relaunch at a lower profile.
	HealthUnsustainable HealthState = "unsustainable"
	// The session has terminated.
	HealthFailed HealthState = "failed"
)

// validHealthStates must stay in lockstep with the CHECK constraint in
// migration 0012_stream_health.
var validHealthStates = map[HealthState]bool{
	HealthHealthy:                     true,
	HealthNetworkDegrading:            true,
	HealthABRAtFloor:                  true,
	HealthClientDecodeDegrading:       true,
	HealthClientPresentationDegrading: true,
	HealthUnsustainable:               true,
	HealthFailed:                      true,
}

func ValidHealthState(s HealthState) bool { return validHealthStates[s] }

// isClientState marks the client-owned health states. The network evaluator must
// NOT clobber one with HealthHealthy — evaluateClientHealthSample owns clearing
// it when its own signal recovers. Network degradation and unsustainable still
// override; only the healthy no-op overwrite is suppressed.
func isClientState(s HealthState) bool {
	switch s {
	case HealthClientDecodeDegrading, HealthClientPresentationDegrading:
		return true
	}
	return false
}

// Below-floor escalation thresholds. They measure SUSTAINED time below the ABR
// floor, accumulated from the first below-floor sample, not time in the current
// state; a single above-floor sample resets the clock (see Evaluate).
const (
	healthNetworkDegradingAfter = 30 * time.Second
	healthABRAtFloorAfter       = 90 * time.Second
	healthUnsustainableAfter    = 180 * time.Second
)

// HealthSample is the telemetry and context for one health evaluation.
type HealthSample struct {
	// Current is the session's currently-persisted health state.
	Current HealthState
	// BelowFloorSince is the FIRST sample of the current below-floor run. Zero
	// when the last observation was above floor.
	BelowFloorSince time.Time
	// SetpointKbps is the latest abr_setpoint_kbps; HasSetpoint is false when the
	// sample carried none (ABR disarmed, static CBR).
	SetpointKbps float64
	HasSetpoint  bool
	// FloorKbps is the rung's ABR floor. Zero means no floor to enforce.
	FloorKbps int32
	// Now is the evaluation time, passed in for testability.
	Now time.Time
}

// HealthResult is one Evaluate outcome.
type HealthResult struct {
	State HealthState
	// BelowFloorSince is the run start to carry into the next sample; zero clears it.
	BelowFloorSince time.Time
	Reason          string
	// ShouldFail: the session crossed the unsustainable threshold and must be
	// driven to failed via failSession.
	ShouldFail bool
}

// Evaluate is the pure stream-health state machine. No I/O.
//
//   - No floor: nothing to enforce, healthy, run cleared.
//   - No setpoint: not an observation of the floor, so state and run are left
//     untouched.
//   - Above floor: healthy, run cleared, from any non-terminal state.
//   - At/below floor: start or continue the run and escalate by SUSTAINED
//     duration — >=180s unsustainable (fail), >=90s abr_at_floor, >=30s
//     network_degrading, otherwise healthy with the run recorded.
func Evaluate(s HealthSample) HealthResult {
	if s.FloorKbps <= 0 {
		return HealthResult{State: HealthHealthy}
	}
	if !s.HasSetpoint {
		return HealthResult{State: s.Current, BelowFloorSince: s.BelowFloorSince}
	}

	floor := float64(s.FloorKbps)

	if s.SetpointKbps > floor {
		return HealthResult{State: HealthHealthy}
	}

	since := s.BelowFloorSince
	if since.IsZero() {
		since = s.Now
	}
	belowFor := s.Now.Sub(since)

	switch {
	case belowFor >= healthUnsustainableAfter:
		return HealthResult{
			State:           HealthUnsustainable,
			BelowFloorSince: since,
			Reason:          "below ABR floor for over 3m — network cannot sustain this profile",
			ShouldFail:      true,
		}
	case belowFor >= healthABRAtFloorAfter:
		return HealthResult{
			State:           HealthABRAtFloor,
			BelowFloorSince: since,
			Reason:          "ABR pinned at floor — quality cannot drop further within this profile",
		}
	case belowFor >= healthNetworkDegradingAfter:
		return HealthResult{
			State:           HealthNetworkDegrading,
			BelowFloorSince: since,
			Reason:          "ABR at/below floor — network is degrading",
		}
	default:
		return HealthResult{State: HealthHealthy, BelowFloorSince: since}
	}
}
