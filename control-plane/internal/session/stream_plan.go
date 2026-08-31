// stream_plan.go — the post-placement stream decision, as one pure function. A
// launch decides twice, across an I/O barrier that cannot be removed:
//
//	pre-placement  : app + user + params + catalogue + probe → CreateParams,
//	                 evaluated against the chain's TOP rung so admission sees the
//	                 worst case (launcher.go).
//	  ── ScheduleAndCreate ── the host and GPU are unknown until this returns ──
//	post-placement : chain + host caps + probe + failure history + certs →
//	                 the rung the session actually starts at. This file.
//
// The second decision's facts are gathered once into StreamInputs; planStream
// then decides with no I/O, logger or clock, and the caller logs, performs the
// single UpdateSessionStream, then mutates the session. Hoisting the reads is
// what stops one launch's two walks from seeing different LatestProbe rows, and
// makes every clamp reachable from a struct literal with no database.
//
// The cap needs no I/O because lowerProfileRung (cert_handler.go) is a
// compiled-in ladder, so its hop target is known before the walk runs.
package session

import (
	"time"

	"github.com/accreleus/quasar/control-plane/internal/profile"
)

// StreamInputs is every fact the post-placement decision reads, gathered once.
// Every zero value means "degrade, do not fail" — a nil Probe hard-gates
// HEVC/AV1 off, an empty HostCodecs is an h264-only host, an unset
// HostEncoder.Known skips clamp 5, a nil PixelRates clamp 6, a nil FailedRungs
// clamp 4, an empty LowerChain stops the cap hopping. A launch is never refused
// for an input that failed to load.
type StreamInputs struct {
	// The placed session. HostID/GPUIndex are nil only when placement produced no
	// host, in which case the cap cannot run.
	SessionID string
	HostID    *string
	GPUIndex  *int32

	// Decided pre-placement and carried forward unchanged: the H.264 profile (the
	// browser floor, or the native lift) and the codec the row was inserted with.
	H264Profile string
	Codec       string

	// The request as it was made.
	Params   LaunchParams
	Override StreamOverride
	Envelope ProbeEnvelope
	Source   string

	// Chain is the selected launch profile; LowerChain is the cap's hop target,
	// zero when the ladder names none or the read failed. LowerChainErr separates
	// a failed read from a chain that holds no rungs — they warn differently.
	Chain         profile.LaunchProfile
	LowerChain    profile.LaunchProfile
	LowerChainID  string
	LowerChainErr error

	// Placed-host encoder capability.
	HostCodecs  []string
	HostEncoder hostEncoderCaps

	// The launching account's latest decode probe, and the per-rung decode
	// failure history for each chain (keyed on the same device as the probe).
	Probe       *DeviceProbe
	FailedRungs map[string]bool
	LowerFailed map[string]bool

	// Certs is every fresh certification row for any rung of either chain, on
	// the placed host+GPU. Unranked — pickCert applies the selection rule.
	Certs      []EncoderCertRow
	CertMaxAge time.Duration

	// Now is the clock the staleness check reads, so a test can pin it.
	Now time.Time
}

// rungWalk records one resolution of one chain; there are two only when the cap
// hopped. Failed is the decode-failure set THIS walk resolved against, carried
// rather than re-derived because the two chains' sets differ and a log line
// pairing one walk's verdicts with the other's history contradicts itself.
type rungWalk struct {
	ChainID  string
	Decision rungDecision
	Failed   map[string]bool
	Err      error
}

// Cap outcomes recorded on a plan, so the caller can log why a cap did or did
// not happen without re-deriving it.
const (
	capNotEligible     = "not_eligible"     // admin, explicit override, or unplaced
	capNoCert          = "no_cert"          // no fresh cert for the resolved rung
	capCertOK          = "cert_ok"          // a cert exists and permits the rung
	capNoLower         = "no_lower"         // the ladder names no lower chain
	capLowerUnreadable = "lower_unreadable" // the lower chain failed to load
	capLowerEmpty      = "lower_empty"      // the lower chain loaded but holds no rungs
	capUnresolvable    = "unresolvable"     // the lower chain resolved to no rung
	capApplied         = "applied"
	capWalkFailed      = "walk_failed" // first walk failed; no cap decision reached
)

// StreamPlan is the decision: what to persist, and enough of the reasoning to
// log it and to assert on it in a test.
type StreamPlan struct {
	// Update is exactly what the caller writes. Nothing else is written.
	Update SessionStreamUpdate

	// ChainID / RungID are the chain and rung that won, after any cap hop.
	ChainID string
	RungID  string

	// CapOutcome is always set, including on the error return; CapTarget is the
	// chain the cap named, even when the hop was abandoned.
	Capped     bool
	CapOutcome string
	CapTarget  string

	// Decision is the winning walk's record; it is what gets persisted.
	Decision rungDecision

	// Walks is every resolution attempted, in order.
	Walks []rungWalk

	// The winning rung's bitrate after the override hatch and BEFORE the envelope,
	// so the caller can log the clamp against Update.BitrateKbps.
	RungBitrateKbps int32

	// Rung is the resolved rung itself, for callers that need more than the ids.
	Rung profile.Profile
}

// planStream resolves the rung a placed session starts at, applies the cert cap,
// and produces the single stream update recording both. Ordering is fixed by
// migration 0041: a cert is keyed on the RUNG, so the cap cannot be asked before
// the walk names one.
//
//  1. resolve the rung over the selected chain. The walk reads no cert input, so
//     the order cannot change which rung a chain produces.
//  2. cap on THAT rung, at the bitrate it would dispatch at — from rungStream,
//     the same function step 3 uses, so cap and dispatch cannot disagree.
//  3. if the cap fires, hop to the lower CHAIN and re-resolve, exactly once; the
//     lower chain is not re-capped.
//  4. one update: the final rung, its envelope-clamped bitrate, the winning
//     walk's decision record.
//
// Re-applying the envelope in step 3 is not optional: applyEnvelopeToBitrate
// only lowers, so twice is safe, but skipping it lets a fall-through restore an
// unclamped bitrate.
//
// An error returns only when the FIRST walk fails (ErrRungCodecNotAvailable 400,
// ErrCodecUnsupportedByHost 409). Every later failure degrades to the uncapped
// resolution: turning a downgrade into a dead session is worse.
func planStream(in StreamInputs) (StreamPlan, error) {
	rung, decision, err := resolveRung(
		in.Chain.Rungs, in.HostCodecs, in.HostEncoder, in.Probe, in.FailedRungs, in.Override,
	)
	plan := StreamPlan{Walks: []rungWalk{{
		ChainID: in.Chain.ID, Decision: decision, Failed: in.FailedRungs, Err: err,
	}}}
	if err != nil {
		plan.CapOutcome = capWalkFailed
		return plan, err
	}

	chain := in.Chain
	stream := rungStream(rung, in.Params, in.Override, in.Envelope)
	plan.CapOutcome = capNotEligible

	// Default path only: admin and explicit override both mean the operator is
	// forcing this, and an unplaced session has no host to consult a cert for.
	if in.capEligible() {
		chain, rung, decision, stream = in.applyCertCap(&plan, chain, rung, decision, stream)
	}

	codec, ok := catalogToWire(rung.Codec)
	if !ok {
		codec = wireCodecH264
	}

	plan.ChainID, plan.RungID, plan.Rung = chain.ID, rung.ID, rung
	plan.Decision = decision
	plan.RungBitrateKbps = pick(in.Override.BitrateKbps, rung.NominalBitrateKbps)
	plan.Update = SessionStreamUpdate{
		Width:       stream.width,
		Height:      stream.height,
		FPS:         stream.fps,
		BitrateKbps: stream.bitrateKbps,
		// The H.264 profile follows the session, not the rung: the row's value
		// already encodes the browser floor and the native lift, and neither is
		// revisited post-placement.
		H264Profile:     in.H264Profile,
		Codec:           codec,
		Playout0Ms:      stream.playout0Ms,
		ProfileID:       chain.ID,
		StreamProfileID: rung.ID,
		// The decision rides on THIS write because it describes the values this
		// write persists; splitting them lets record and row disagree on a failure.
		CodecDecision: marshalCodecDecision(decision),
	}
	return plan, nil
}

// capEligible reports whether the certification cap may run at all.
func (in StreamInputs) capEligible() bool {
	return !in.Params.IsAdmin && !in.Override.any() && in.HostID != nil && in.GPUIndex != nil
}

// applyCertCap consults the host's certification for the resolved rung and, when
// unsafe, hops once to the lower chain. It records the outcome on plan and
// returns the originals unchanged whenever the hop does not complete.
func (in StreamInputs) applyCertCap(
	plan *StreamPlan,
	chain profile.LaunchProfile,
	rung profile.Profile,
	decision rungDecision,
	stream resolvedStream,
) (profile.LaunchProfile, profile.Profile, rungDecision, resolvedStream) {
	cert := pickCert(in.Certs, rung.ID, stream.bitrateKbps, in.Now, in.CertMaxAge)
	if cert == nil {
		// No row, or all stale. Missing is not "unsafe": the table starts empty and
		// an uncertified host proceeds optimistically.
		plan.CapOutcome = capNoCert
		return chain, rung, decision, stream
	}
	if !certShouldCap(*cert) {
		plan.CapOutcome = capCertOK
		return chain, rung, decision, stream
	}

	plan.CapTarget = in.LowerChainID
	switch {
	case in.LowerChainID == "":
		// Bottom of the ladder: nothing to cap to, so allow it.
		plan.CapOutcome = capNoLower
		return chain, rung, decision, stream
	case in.LowerChainErr != nil || in.LowerChain.ID == "":
		plan.CapOutcome = capLowerUnreadable
		return chain, rung, decision, stream
	case len(in.LowerChain.Rungs) == 0:
		plan.CapOutcome = capLowerEmpty
		return chain, rung, decision, stream
	}

	lowerRung, lowerDecision, err := resolveRung(
		in.LowerChain.Rungs, in.HostCodecs, in.HostEncoder, in.Probe, in.LowerFailed, in.Override,
	)
	plan.Walks = append(plan.Walks, rungWalk{
		ChainID: in.LowerChain.ID, Decision: lowerDecision, Failed: in.LowerFailed, Err: err,
	})
	if err != nil {
		// Unreachable today (no override, and the floor always yields a rung), but
		// keep the uncapped resolution rather than failing the launch if it ever is.
		plan.CapOutcome = capUnresolvable
		return chain, rung, decision, stream
	}

	plan.Capped = true
	plan.CapOutcome = capApplied
	return in.LowerChain, lowerRung, lowerDecision, rungStream(lowerRung, in.Params, in.Override, in.Envelope)
}

// certShouldCap is the SPT-06 verdict rule. `capped` with live_write_stable
// false counts as unsafe: the encoder cannot take live bitrate updates, so ABR
// cannot keep it in budget. `ok` always passes.
func certShouldCap(cert EncoderCertRow) bool {
	return cert.Verdict == VerdictUnsafe ||
		(cert.Verdict == VerdictCapped && !cert.LiveWriteStable)
}

// pickCert selects the cert row for a rung at a bitrate: measured closest to it,
// ties to the most recent. It is Store.CertForRung's `ORDER BY ABS(bitrate_kbps
// - $n) ASC, measured_at DESC LIMIT 1` restated in Go so the launch can read
// certs for rungs it has not chosen yet; the two must not drift, guarded by
// TestCertForRungMatchesPickCert.
//
// maxAge is applied again even though CertsForRungs filters on it: the staleness
// rule belongs to the decision, so a stale row handed over cannot be acted on.
func pickCert(certs []EncoderCertRow, rungID string, bitrateKbps int32, now time.Time, maxAge time.Duration) *EncoderCertRow {
	var best *EncoderCertRow
	var bestDelta int32
	for i := range certs {
		c := &certs[i]
		if c.StreamProfileID != rungID {
			continue
		}
		if maxAge > 0 && !now.IsZero() && now.Sub(c.MeasuredAt) > maxAge {
			continue
		}
		delta := int32(c.BitrateKbps) - bitrateKbps
		if delta < 0 {
			delta = -delta
		}
		switch {
		case best == nil, delta < bestDelta, delta == bestDelta && c.MeasuredAt.After(best.MeasuredAt):
			best, bestDelta = c, delta
		}
	}
	return best
}

// applyTo copies a persisted plan onto the in-memory session. Call it ONLY after
// the write succeeds: on a failed write the session must keep the pre-schedule
// top-rung values it was admitted against and the h264 floor it was inserted with.
func (p StreamPlan) applyTo(sess *Session) {
	sess.Width = p.Update.Width
	sess.Height = p.Update.Height
	sess.FPS = p.Update.FPS
	sess.BitrateKbps = p.Update.BitrateKbps
	sess.Playout0Ms = p.Update.Playout0Ms
	sess.Codec = p.Update.Codec
	chainID, rungID := p.ChainID, p.RungID
	sess.ProfileID = &chainID
	sess.StreamProfileID = &rungID
	sess.CodecDecision = p.Update.CodecDecision
}

// dispatchDims is the size a rung is actually dispatched at: its own
// width/height/fps, beaten field by field by an explicit override. The ONE place
// those three numbers are derived — rungStream builds the session update from
// them and clamp 6 measures against them, so deriving them twice would let clamp
// 6 judge a codec on a size the session never runs at. The envelope is not
// applied here: it moves bitrate and playout only.
func dispatchDims(rung profile.Profile, ov StreamOverride) (w, h, fps int32) {
	return pick(ov.Width, rung.Width), pick(ov.Height, rung.Height), pick(ov.FPS, rung.FPS)
}

// resolvedStream is one rung's dispatch values after the override hatch and the
// probe envelope.
type resolvedStream struct {
	width       int32
	height      int32
	fps         int32
	bitrateKbps int32
	playout0Ms  int32
}

// rungStream is what a resolved rung would be dispatched at: its own values,
// beaten field by field by an override, then the probe envelope. The ONE place
// that bitrate is produced — the cap asks at the bitrate the session starts at
// and the write persists the same one, so deriving them separately would let the
// cap consult a bench cell the launch never uses.
func rungStream(rung profile.Profile, lp LaunchParams, ov StreamOverride, env ProbeEnvelope) resolvedStream {
	w, h, fps := dispatchDims(rung, ov)
	s := resolvedStream{
		width:       w,
		height:      h,
		fps:         fps,
		bitrateKbps: pick(ov.BitrateKbps, rung.NominalBitrateKbps),
		playout0Ms:  rung.Playout0Ms,
	}
	if lp.IsAdmin || ov.any() {
		return s
	}
	if env.SafeCeilingKbps > 0 {
		if clamped := applyEnvelopeToBitrate(s.bitrateKbps, env); clamped < s.bitrateKbps {
			s.bitrateKbps = clamped
		}
	}
	if env.Playout0BumpMs > 0 {
		s.playout0Ms = applyEnvelopeToPlayout0(s.playout0Ms, env)
	}
	return s
}
