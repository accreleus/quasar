package session

import (
	"fmt"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/telemetry"
)

// The session-side half of clock alignment. telemetry.AlignSeries puts every
// client-clock point on the host clock; this file turns that into the two things
// the classifier needs: a tolerance, so a cross-source coincidence carries an
// error bar, and a downgrade, so an unmeasured clock yields a weaker labelled
// claim instead of a confident one across two unaligned axes.
//
// The sign convention lives only in telemetry/align.go. Do not restate it here.

// alignment is what a classification rule is allowed to know about the clock.
type alignment struct {
	// Applied ⇒ client points sit on the host clock and a cross-source coincidence
	// is verifiable. Not applied ⇒ every such claim must be downgraded.
	Applied bool
	// ToleranceMs is the half-width every cross-source coincidence must allow.
	ToleranceMs   float64
	OffsetMs      float64
	UncertaintyMs float64
	AgeMs         *int64
}

func alignmentOf(a telemetry.AlignedSet) alignment {
	return alignment{
		Applied:       a.Applied,
		ToleranceMs:   a.ToleranceMs(),
		OffsetMs:      a.OffsetMs,
		UncertaintyMs: a.UncertaintyMs,
		AgeMs:         a.AgeMs,
	}
}

// crossSourceNote is the sentence a falsifier or evidence line carries when it
// spans host and client. It must name the tolerance actually used, and say so
// when the clock is unmeasured rather than imply a timing check happened.
func (al alignment) crossSourceNote() string {
	if !al.Applied {
		return "cross-source timing unverified (clock unmeasured); this leg rests on " +
			"whole-window aggregates, not on events lining up in time"
	}
	return fmt.Sprintf("cross-source: coincidence assessed on the aligned clock with a ±%.0f ms tolerance",
		al.ToleranceMs)
}

// coincides reports whether an instant lands inside a window widened by the
// tolerance on both sides.
func coincides(atMs, fromMs, toMs int64, tolMs float64) bool {
	tol := int64(tolMs)
	return atMs >= fromMs-tol && atMs <= toMs+tol
}

// Warm-up exclusion. A session's first seconds spike present-interval σ and sag
// encoder fps while the pipeline fills, which then sat in a 300 s window as a
// permanent falsifier for the session's life. The excluded amount is reported as
// window.warmup_excluded_ms, so it is visible rather than a silent trim.

// warmupSensitiveSeries are the only series the exclusion touches. Keep it
// short: an exclusion applied everywhere would hide a real fault that started early.
var warmupSensitiveSeries = map[string]bool{
	"client.present_interval_sd_ms": true,
	"encoder.fps":                   true,
}

type warmupExclusion struct {
	// UntilMs is the host-clock instant warm-up ends (running-at + warmupExcludeS).
	// Zero when the session never reached running: no anchor, so exclude nothing.
	UntilMs int64
	// ExcludedMs is how much of THIS read window fell inside warm-up.
	ExcludedMs int64
}

func warmupFor(runningAt *time.Time, fromMs, toMs int64) warmupExclusion {
	if runningAt == nil || runningAt.IsZero() {
		return warmupExclusion{}
	}
	until := runningAt.UnixMilli() + int64(warmupExcludeS*1000)
	w := warmupExclusion{UntilMs: until}
	// Warm-up ∩ this window, anchored on running-at rather than the window start:
	// for a session that started mid-window, the minutes before it was running are
	// not warm-up.
	start := runningAt.UnixMilli()
	if start < fromMs {
		start = fromMs
	}
	end := until
	if end > toMs {
		end = toMs
	}
	if end > start {
		w.ExcludedMs = end - start
	}
	return w
}

// assessed returns the series map the rules read: the input with the warm-up
// head dropped from warm-up-sensitive series. The bundle still serves the full
// map, so a reader can see the samples the classifier declined to judge.
func (w warmupExclusion) assessed(series map[string][]seriesPoint) map[string][]seriesPoint {
	if w.UntilMs == 0 || w.ExcludedMs == 0 {
		return series
	}
	out := make(map[string][]seriesPoint, len(series))
	for name, pts := range series {
		if !warmupSensitiveSeries[name] {
			out[name] = pts
			continue
		}
		kept := make([]seriesPoint, 0, len(pts))
		for _, p := range pts {
			if p.TsUnixMs >= w.UntilMs {
				kept = append(kept, p)
			}
		}
		out[name] = kept
	}
	return out
}

type fpsDip struct {
	FromMs int64
	ToMs   int64
	Fps    float64
}

// hostFpsDips are the agent samples where the encoder fell below the steady
// floor. They answer one cross-source question a whole-window p10 cannot: did
// the client's hitch happen while the host was dipping?
func hostFpsDips(series map[string][]seriesPoint) []fpsDip {
	var out []fpsDip
	for _, p := range series["encoder.fps"] {
		if p.V < classifierMinHostFps {
			out = append(out, fpsDip{FromMs: p.TsUnixMs, ToMs: p.TsUnixMs, Fps: p.V})
		}
	}
	return out
}

// hitchCoincidesWithHostDip is the claim the client-presentation verdict must
// rule out: a hitch landing on a host fps dip is the host's, not the client's
// present path. Ask it only when the clock was applied — otherwise the two
// timestamp axes are not comparable and the caller must downgrade to the
// whole-window aggregate guard.
func hitchCoincidesWithHostDip(hitches []hitchWindow, dips []fpsDip, tolMs float64) bool {
	for _, h := range hitches {
		for _, d := range dips {
			if coincides(h.FromMs, d.FromMs, d.ToMs, tolMs) {
				return true
			}
		}
	}
	return false
}

// abrDownshiftInCongestion asks whether the governor's downshift lands on the
// congestion window. The downshift is an agent event and the window derives from
// browser series, so this is only meaningful on an aligned clock.
func abrDownshiftInCongestion(downshifts []abrDownshiftWindow, w congestionWindow, tolMs float64) bool {
	for _, d := range downshifts {
		if coincides(d.TsUnixMs, w.FromMs, w.ToMs, tolMs) {
			return true
		}
	}
	return false
}
