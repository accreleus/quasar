// health_evaluator.go — stream and client health evaluation.
package session

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// healthEvaluator owns the server- and client-health run tracking and the
// health-state transitions they drive.
type healthEvaluator struct {
	store       *Store
	log         *slog.Logger
	failSession func(sessionID, reason string, detail *string) // = Coordinator.failSessionWithDetail

	mu         sync.Mutex
	healthRuns map[string]time.Time
	clientRuns map[string]*clientHealthRun
}

func newHealthEvaluator(store *Store, log *slog.Logger, failSession func(string, string, *string)) *healthEvaluator {
	return &healthEvaluator{store: store, log: log, failSession: failSession,
		healthRuns: make(map[string]time.Time), clientRuns: make(map[string]*clientHealthRun)}
}

// forget deletes a session's health-run tracking from both maps. Call it at
// EVERY terminal transition: the evaluate paths delete only on their own
// below-floor/recovery edges, so a session ending any other way (reap, fail,
// normal stop) leaks its entries for the life of the process.
func (h *healthEvaluator) forget(sessionID string) {
	h.mu.Lock()
	delete(h.healthRuns, sessionID)
	delete(h.clientRuns, sessionID)
	h.mu.Unlock()
}

// ClientHealthSample is the server's view of the browser's own client-health
// classification. Fields are additive on the stats POST, so an older browser
// yields the zero value (Class ""), meaning "no client signal".
type ClientHealthSample struct {
	// Class is the browser-reported client_health (clientHealth.ts); "" ⇒ no signal.
	Class string
	// Reason is the browser-reported client_health_reason.
	Reason string
	// DeviceKey is the client's localStorage device key; "" ⇒ coarse per-user key.
	DeviceKey string
	// IsHidden: a hidden tab is never a failure.
	IsHidden bool
}

// evaluateHealth computes stream health from the latest ABR setpoint against the
// rung's floor, persists a change, and fails a session unsustainable too long.
// The state machine is Evaluate; this does the I/O, in its own goroutine off the
// metrics hot path, logging errors rather than propagating them.
func (h *healthEvaluator) evaluateHealth(ctx context.Context, sessionID string, setpointKbps float64) {
	sess, err := h.store.Get(ctx, sessionID)
	if err != nil {
		return
	}
	// The floor comes from the RESOLVED RUNG, read from the database: an in-code
	// catalog was a second source of truth that cannot follow an admin tuning a
	// per-rung floor, and a stale floor fails healthy sessions or spares unhealthy
	// ones. No resolved rung, a read error or a zero floor all skip.
	if sess.StreamProfileID == nil {
		return
	}
	floorKbps, err := h.store.RungABRFloor(ctx, *sess.StreamProfileID)
	if err != nil {
		h.log.Warn("read rung abr floor failed, skipping health evaluation",
			"session_id", sessionID, "stream_profile_id", *sess.StreamProfileID, "err", err)
		return
	}
	if floorKbps <= 0 {
		return
	}

	h.mu.Lock()
	since := h.healthRuns[sessionID]
	res := Evaluate(HealthSample{
		Current:         sess.HealthState,
		BelowFloorSince: since,
		SetpointKbps:    setpointKbps,
		HasSetpoint:     true,
		FloorKbps:       floorKbps,
		Now:             time.Now(),
	})
	if res.BelowFloorSince.IsZero() {
		delete(h.healthRuns, sessionID)
	} else {
		h.healthRuns[sessionID] = res.BelowFloorSince
	}
	h.mu.Unlock()

	// Persist only on an actual change, so health_state_changed_at does not churn
	// on every steady-state sample.
	//
	// Two-writer guard: EvaluateClientHealth also writes health_state. This path
	// returns HealthHealthy for any above-floor sample, which would clobber a live
	// client_* banner on the next agent metric. Suppress only that no-op overwrite
	// — the client evaluator owns clearing its own state. Network
	// degradation/unsustainable still override a client_* state below.
	if res.State == HealthHealthy && isClientState(sess.HealthState) {
		return
	}
	if res.State != sess.HealthState {
		var reason *string
		if res.Reason != "" {
			r := res.Reason
			reason = &r
		}
		if err := h.store.UpdateHealthState(ctx, sessionID, res.State, reason); err != nil {
			h.log.Warn("update session health failed", "session_id", sessionID, "err", err)
		} else {
			h.log.Info("session health changed",
				"session_id", sessionID, "health_state", res.State, "reason", res.Reason)
		}
	}

	// The only action health takes: fail an unsustainable session. Resolution and
	// fps are never silently switched or renegotiated; the web client prompts the
	// user to relaunch at a lower profile.
	if res.ShouldFail {
		h.mu.Lock()
		delete(h.healthRuns, sessionID)
		h.mu.Unlock()
		// state_detail carries the reason too (mirroring host_lost) so the client
		// banner can read it, not just error_message.
		reason := "unsustainable: " + res.Reason
		h.failSession(sessionID, reason, &reason)
	}
}

// EvaluateClientHealth maps the browser's reported class into the health
// machine's client_* states (network states win) and records or clears the
// profile-certification history feeding eligibility's HistoricalFailures.
//
// It must never touch ABR: its only writes are health_state and the
// user_device_profile_history row. A backgrounded tab is the main
// false-positive guard — treated as no signal, clearing the run with no state
// flip and no cert failure.
func (h *healthEvaluator) EvaluateClientHealth(ctx context.Context, sessionID string, sample ClientHealthSample) {
	if sample.Class == "" {
		return // older browser: no client signal
	}

	sess, err := h.store.Get(ctx, sessionID)
	if err != nil {
		return
	}
	// Only a running session with a launched profile can certify one.
	if sess.State != StateRunning || sess.ProfileID == nil {
		return
	}

	// Fold a hidden tab into the backgrounded class so the state machine clears
	// the run rather than ever failing.
	class := sample.Class
	if sample.IsHidden {
		class = ClientHealthBackgrounded
	}

	h.mu.Lock()
	run := h.clientRuns[sessionID]
	dec := evaluateClientHealthSample(sess.HealthState, run, class, time.Now())
	if dec.Run == nil {
		delete(h.clientRuns, sessionID)
	} else {
		h.clientRuns[sessionID] = dec.Run
	}
	h.mu.Unlock()

	if dec.SetState != "" && dec.SetState != sess.HealthState {
		reason := dec.SetReason
		if sample.Reason != "" {
			reason = sample.Reason
		}
		if err := h.store.UpdateHealthState(ctx, sessionID, dec.SetState, &reason); err != nil {
			h.log.Warn("update client health state failed", "session_id", sessionID, "err", err)
		} else {
			h.log.Info("session client health changed",
				"session_id", sessionID, "health_state", dec.SetState, "reason", reason)
		}
	}
	// Sustained recovery: reset a client-degraded session back to healthy.
	if dec.ClearToHealthy && sess.HealthState != HealthHealthy {
		if err := h.store.UpdateHealthState(ctx, sessionID, HealthHealthy, nil); err != nil {
			h.log.Warn("clear client health state failed", "session_id", sessionID, "err", err)
		}
	}

	// Record or clear profile-certification history, latest-outcome-wins, keyed by
	// the device that posted the sample ("" falls back to a coarse per-user key).
	// Both fails and passes carry the session's wire codec (migration 0032), so a
	// codec-specific decode failure never blanks a profile that works on another.
	//
	// Grain split (§4.4): a DECODE-side fail is written against the resolved RUNG
	// id, since decode failure is resolution-dependent too and a 4K AV1 failure
	// must not ban the 1080p AV1 rung of the same chain. A presentation-side fail
	// and every pass keep the launch-profile-level row, which is what feeds
	// ProfileFailures. No resolved rung (legacy/console) falls back to that row.
	launchProfileID := *sess.ProfileID
	deviceKey := sample.DeviceKey
	if dec.RecordFail != "" {
		fr := dec.RecordFail
		codec := certFailCodec(dec.RecordFail, sess.Codec)
		historyID := launchProfileID
		if codec != "" && sess.StreamProfileID != nil {
			historyID = *sess.StreamProfileID
		}
		if err := h.store.RecordProfileOutcome(ctx, sess.UserID, deviceKey, historyID, codec, outcomeFail, &fr); err != nil {
			h.log.Warn("record profile fail failed", "session_id", sessionID, "err", err)
		}
	} else if dec.RecordPass {
		if err := h.store.RecordProfileOutcome(ctx, sess.UserID, deviceKey, launchProfileID, sess.Codec, outcomePass, nil); err != nil {
			h.log.Warn("record profile pass failed", "session_id", sessionID, "err", err)
		}
	}
}

// logGPUUtilization makes the chosen GPU's accounting observable in the logs.
//
// VRAM is the LIVE free/total from telemetry (#383 §5), not the reserved sum:
// declared VRAM left admission and the reserved column is permanently 0 for new
// sessions, so it would print "0/16384" forever. Encode slots, the actual
// reservation, still log reserved/total. An unsampled GPU logs "unknown" rather
// than a misleading zero. A read error is logged, never fatal to the launch.
func (h *healthEvaluator) logGPUUtilization(ctx context.Context, hostID, gpuID string) {
	if hostID == "" || gpuID == "" {
		return
	}
	avs, err := h.store.GPUAvailability(ctx, hostID)
	if err != nil {
		h.log.Warn("read gpu utilization", "host_id", hostID, "err", err)
		return
	}
	for _, a := range avs {
		if a.GPUID == gpuID {
			free := "unknown"
			if a.VramMBFree != nil {
				free = fmt.Sprintf("%d", *a.VramMBFree)
			}
			h.log.Info("gpu utilization",
				"gpu_index", a.GPUIndex,
				"vram_mb_free", fmt.Sprintf("%s/%d", free, a.VramMBTotal),
				"encode_slots", fmt.Sprintf("%d/%d", a.SlotsReserved, a.SlotsTotal),
				"active_sessions", a.ActiveSessions)
			return
		}
	}
}
