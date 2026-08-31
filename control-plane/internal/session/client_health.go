package session

import "time"

// Control-plane consumption of the browser-classified client health (#207). The
// browser is the single classifier (web/src/lib/clientHealth.ts); the server maps
// its reported class into the health machine's client_* states.
//
// Invariants:
//   - Client health NEVER drives server ABR. It feeds only the health state,
//     admin diagnostics and the profile-certification history — there is no path
//     from here to the encoder bitrate or the ABR governor.
//   - Network states win. A client_* state is set only when the session is not
//     already network-degraded or unsustainable.
//   - A hidden or backgrounded tab is NEVER a failure and never flips state: the
//     server treats it as no client signal and clears the run.
//
// The sustained run mirrors the below-floor run: a single degraded sample is not
// acted on, and a sustained smooth run certifies a pass (latest-outcome-wins).

// The browser-reported classes; must match clientHealth.ts.
const (
	ClientHealthSmooth       = "smooth"
	ClientHealthDecode       = "decode_degrading"
	ClientHealthPresentation = "presentation_degrading"
	ClientHealthBackgrounded = "backgrounded_or_hidden"
	ClientHealthUnsupported  = "client_unsupported"
)

// Sustained-run thresholds. A browser posts on a ~5 s cadence (telemetry.ts), so
// these windows span several samples and a transient hiccup never flips state or
// records a cert failure.
const (
	clientDegradeSustainFor = 20 * time.Second // degraded ⇒ flip state + record a fail
	clientSmoothSustainFor  = 30 * time.Second // smooth ⇒ certify a pass
)

// clientHealthRun is one session's in-memory degradation/recovery run; since
// marks the first sample of the current class.
type clientHealthRun struct {
	class string
	since time.Time
}

// clientNetworkDegraded marks the states that take precedence over any client
// signal.
func clientNetworkDegraded(s HealthState) bool {
	switch s {
	case HealthNetworkDegrading, HealthABRAtFloor, HealthUnsustainable, HealthFailed:
		return true
	}
	return false
}

// clientHealthToState maps a degraded browser class to its health state.
// decode and unsupported both map to client_decode_degrading; there is no
// separate unsupported state. Returns ("", false) for smooth and backgrounded.
func clientHealthToState(class string) (HealthState, bool) {
	switch class {
	case ClientHealthDecode, ClientHealthUnsupported:
		return HealthClientDecodeDegrading, true
	case ClientHealthPresentation:
		return HealthClientPresentationDegrading, true
	}
	return "", false
}

// certFailCodec returns the codec a certification fail is recorded against
// (migration 0032). Decode-side classes indict the codec the session streamed,
// so the launch resolver skips that codec instead of eligibility hiding the
// whole profile. presentation_degrading is codec-independent and stays a
// profile-level (empty-codec) verdict. A codec='h264' fail still blocks the
// profile in ProfileFailures, because h264 is the guaranteed floor.
func certFailCodec(class, sessionCodec string) string {
	switch class {
	case ClientHealthUnsupported, ClientHealthDecode:
		return sessionCodec
	}
	return ""
}

// clientDecision is what the coordinator applies after evaluating a sample.
type clientDecision struct {
	// Run is the sustained-run state for the next sample; nil clears it.
	Run *clientHealthRun
	// SetState, when non-empty, is a client_* health state to persist.
	SetState  HealthState
	SetReason string
	// ClearToHealthy: a sustained smooth run resets a client-degraded session.
	ClearToHealthy bool
	// RecordFail, when non-empty, is the failure class to write to cert history.
	RecordFail string
	// RecordPass: a sustained smooth run certifies a pass, clearing any prior
	// cert failure for this profile.
	RecordPass bool
}

// evaluateClientHealthSample is the pure state machine for one browser sample.
//
//   - A network/unsustainable state wins: no client transition, run cleared.
//   - backgrounded_or_hidden is no signal: run cleared, state left alone. This is
//     the hidden guard — never a fail, never a flip.
//   - smooth: once sustained, certify a pass and clear a client_* state.
//   - a degraded class: once sustained, set the mapped state and record a fail.
func evaluateClientHealthSample(current HealthState, run *clientHealthRun, class string, now time.Time) clientDecision {
	if clientNetworkDegraded(current) {
		return clientDecision{Run: nil}
	}
	if class == ClientHealthBackgrounded {
		return clientDecision{Run: nil}
	}

	if run == nil || run.class != class {
		run = &clientHealthRun{class: class, since: now}
	}
	sustainedFor := now.Sub(run.since)

	if class == ClientHealthSmooth {
		d := clientDecision{Run: run}
		if sustainedFor >= clientSmoothSustainFor {
			d.RecordPass = true
			if current == HealthClientDecodeDegrading || current == HealthClientPresentationDegrading {
				d.ClearToHealthy = true
			}
		}
		return d
	}

	state, ok := clientHealthToState(class)
	if !ok {
		return clientDecision{Run: nil} // unknown class from a future browser
	}
	d := clientDecision{Run: run}
	if sustainedFor >= clientDegradeSustainFor {
		d.SetState = state
		d.SetReason = "client " + class
		d.RecordFail = class
	}
	return d
}
