package telemetry

import "time"

// Clock alignment. The sign convention, written down once
// (docs/session-trace/trace-format.md §4 points here; nothing else in the tree
// may restate it):
//
//	client_offset_ms = host_clock − client_clock
//	host_ts_unix_ms  = client_ts_unix_ms + client_offset_ms
//
// The producer/consumer pair is web/src/webrtc/telemetry.ts onPong and
// clockOffset.ts. The field name `client_offset_ms` reads as if client−host;
// it is not, and renaming would break the stored rows and the wire.
//
// An unmeasured clock is not offset 0 — absence is absence. AlignSeries shifts
// nothing when the clock is nil and marks the set Applied:false, so downstream
// coincidence claims are downgraded rather than made on two unaligned axes.

// CoincidenceWindowMs is the floor on the tolerance any cross-source coincidence
// claim is made with: the client and the agent each report on a ~1 s sampling
// cadence, so two events in the same sampling window are indistinguishable in
// time no matter how good the clock is. Sub-cadence skew must never be able to
// flip a claim; only skew larger than the sampling window can.
const CoincidenceWindowMs = 1000.0

// AlignedSample is one telemetry sample expressed on the HOST clock.
type AlignedSample struct {
	Sample
	// RawTsUnixMs is the timestamp exactly as the reporter sent it. Kept so a
	// reader can always recover what was actually reported.
	RawTsUnixMs int64
	// Shifted is true when TsUnixMs differs from RawTsUnixMs because the measured
	// offset was applied. Always false for agent samples (already host-clock).
	Shifted bool
}

// AlignedEvent is one trace event expressed on the HOST clock.
type AlignedEvent struct {
	Event
	RawTsUnixMs int64
	Shifted     bool
}

// AlignedSet is a session's telemetry with every point on one clock, plus the
// honesty that goes with it: whether the shift happened at all, and the error
// bar every cross-source comparison must be made with.
type AlignedSet struct {
	Samples []AlignedSample
	Events  []AlignedEvent

	// Applied is true when a measured clock existed and client-clock points were
	// shifted onto the host clock. False ⇒ the two axes are NOT comparable and a
	// cross-source coincidence claim must be downgraded, never quietly made.
	Applied bool
	// OffsetMs / UncertaintyMs are the measured clock; zero when Applied is false
	// (and meaningless then — read Applied first).
	OffsetMs      float64
	UncertaintyMs float64
	// AgeMs is now − measured_at (ms), nil when unmeasured. A stale offset is a
	// drifting one; the client re-posts while it changes, so age is the signal
	// that it stopped.
	AgeMs *int64
}

// ToleranceMs is the half-width any cross-source coincidence claim must be made
// with: never tighter than the reporting cadence, and never tighter than the
// offset's own error bar.
func (a AlignedSet) ToleranceMs() float64 {
	if a.UncertaintyMs > CoincidenceWindowMs {
		return a.UncertaintyMs
	}
	return CoincidenceWindowMs
}

// isClientSource reports whether a source's timestamps are on the CLIENT clock
// (Date.now() in a browser, or the native client's equivalent) and therefore
// need the offset applied. The agent is the host itself.
func isClientSource(source string) bool {
	return source == SourceBrowser || source == SourceNative
}

// AlignSeries puts every client-clock point on the host clock using the measured
// offset, and reports whether it could. Pure: it copies, it never mutates the
// input, and it makes no claim it cannot support.
//
// Agent points are never shifted — they are already host wall-clock, which is
// what the whole exercise aligns TO.
func AlignSeries(samples []Sample, events []Event, clock *Clock) AlignedSet {
	return alignAt(samples, events, clock, time.Now())
}

func alignAt(samples []Sample, events []Event, clock *Clock, now time.Time) AlignedSet {
	out := AlignedSet{
		Samples: make([]AlignedSample, 0, len(samples)),
		Events:  make([]AlignedEvent, 0, len(events)),
	}
	if clock != nil {
		out.Applied = true
		out.OffsetMs = clock.ClientOffsetMs
		out.UncertaintyMs = clock.UncertaintyMs
		if !clock.MeasuredAt.IsZero() {
			age := now.Sub(clock.MeasuredAt).Milliseconds()
			if age < 0 {
				age = 0
			}
			out.AgeMs = &age
		}
	}

	shift := func(source string, ts int64) (int64, bool) {
		if !out.Applied || !isClientSource(source) {
			return ts, false
		}
		return ts + int64(roundHalfAway(out.OffsetMs)), true
	}

	for _, s := range samples {
		ts, shifted := shift(s.Source, s.TsUnixMs)
		as := AlignedSample{Sample: s, RawTsUnixMs: s.TsUnixMs, Shifted: shifted}
		as.Sample.TsUnixMs = ts
		out.Samples = append(out.Samples, as)
	}
	for _, e := range events {
		ts, shifted := shift(e.Source, e.TsUnixMs)
		ae := AlignedEvent{Event: e, RawTsUnixMs: e.TsUnixMs, Shifted: shifted}
		ae.Event.TsUnixMs = ts
		out.Events = append(out.Events, ae)
	}
	return out
}

func roundHalfAway(f float64) float64 {
	if f < 0 {
		return -roundHalfAway(-f)
	}
	return float64(int64(f + 0.5))
}

// PlainSamples / PlainEvents hand back the aligned points in the plain shapes the
// rest of the tree already reads. The timestamps are the ALIGNED ones — that is
// the point of the type; RawTsUnixMs is where the reported value survives.
func (a AlignedSet) PlainSamples() []Sample {
	out := make([]Sample, 0, len(a.Samples))
	for _, s := range a.Samples {
		out = append(out, s.Sample)
	}
	return out
}

func (a AlignedSet) PlainEvents() []Event {
	out := make([]Event, 0, len(a.Events))
	for _, e := range a.Events {
		out = append(out, e.Event)
	}
	return out
}
