package telemetry

import (
	"encoding/json"
	"testing"
	"time"
)

func sample(source string, ts int64) Sample {
	return Sample{Source: source, TsUnixMs: ts, Metrics: json.RawMessage(`{"fps":60}`)}
}

// The whole point of the module: a browser point moves onto the host clock, and
// an agent point — which IS the host clock — does not move at all. A shift
// applied to both would be a rotation of the timeline, not an alignment.
func TestAlignSeriesShiftsClientPointsOnlyByAddingTheOffset(t *testing.T) {
	clock := &Clock{ClientOffsetMs: 250, UncertaintyMs: 4, MeasuredAt: time.Now()}
	samples := []Sample{
		sample(SourceAgent, 1_000_000),
		sample(SourceBrowser, 1_000_000),
		sample(SourceNative, 1_000_000),
	}
	events := []Event{{Source: SourceBrowser, TsUnixMs: 2_000_000, Type: "playout.changed"}}

	got := AlignSeries(samples, events, clock)

	if !got.Applied {
		t.Fatal("Applied = false with a measured clock")
	}
	if got.Samples[0].TsUnixMs != 1_000_000 || got.Samples[0].Shifted {
		t.Errorf("agent sample moved: %+v — agent points ARE the host clock", got.Samples[0])
	}
	for i, want := range map[int]int64{1: 1_000_250, 2: 1_000_250} {
		if got.Samples[i].TsUnixMs != want {
			t.Errorf("client sample %d ts = %d, want %d (host_ts = client_ts + offset)",
				i, got.Samples[i].TsUnixMs, want)
		}
		if !got.Samples[i].Shifted {
			t.Errorf("client sample %d not marked Shifted", i)
		}
		if got.Samples[i].RawTsUnixMs != 1_000_000 {
			t.Errorf("client sample %d lost its reported timestamp", i)
		}
	}
	if got.Events[0].TsUnixMs != 2_000_250 {
		t.Errorf("browser event ts = %d, want 2000250", got.Events[0].TsUnixMs)
	}
}

// A negative offset means the client clock runs ahead of the host. It subtracts.
func TestAlignSeriesAppliesANegativeOffset(t *testing.T) {
	got := AlignSeries([]Sample{sample(SourceBrowser, 1_000_000)}, nil,
		&Clock{ClientOffsetMs: -40.6, MeasuredAt: time.Now()})
	if got.Samples[0].TsUnixMs != 999_959 {
		t.Errorf("ts = %d, want 999959 (1000000 + round(-40.6))", got.Samples[0].TsUnixMs)
	}
}

// Absence is not zero. An unmeasured clock must leave every point exactly where
// it was reported and say the offset was not applied — the downstream rules read
// Applied to decide whether a cross-source claim is even askable.
func TestAlignSeriesUnmeasuredShiftsNothing(t *testing.T) {
	got := AlignSeries([]Sample{sample(SourceBrowser, 1_000_000)}, nil, nil)
	if got.Applied {
		t.Error("Applied = true with no clock row")
	}
	if got.Samples[0].TsUnixMs != 1_000_000 || got.Samples[0].Shifted {
		t.Errorf("point moved with no measured offset: %+v", got.Samples[0])
	}
	if got.AgeMs != nil {
		t.Errorf("AgeMs = %v, want nil when unmeasured", *got.AgeMs)
	}
}

func TestAlignSeriesReportsClockAge(t *testing.T) {
	now := time.Unix(1_700_000, 0)
	got := alignAt(nil, nil, &Clock{MeasuredAt: now.Add(-90 * time.Second)}, now)
	if got.AgeMs == nil || *got.AgeMs != 90_000 {
		t.Fatalf("AgeMs = %v, want 90000", got.AgeMs)
	}
}

// The tolerance never drops below the reporting cadence, however good the offset
// is: two points inside one sampling window are indistinguishable in time.
func TestToleranceNeverTighterThanTheSamplingCadence(t *testing.T) {
	tight := AlignedSet{Applied: true, UncertaintyMs: 0.5}
	if tight.ToleranceMs() != CoincidenceWindowMs {
		t.Errorf("tolerance = %v, want the %v ms cadence floor", tight.ToleranceMs(), CoincidenceWindowMs)
	}
	loose := AlignedSet{Applied: true, UncertaintyMs: 4000}
	if loose.ToleranceMs() != 4000 {
		t.Errorf("tolerance = %v, want the offset's own error bar (4000)", loose.ToleranceMs())
	}
}

func TestHiddenFlagReadsBothEncodings(t *testing.T) {
	cases := []struct {
		raw           string
		hidden, found bool
	}{
		{`{"is_hidden":1}`, true, true},
		{`{"is_hidden":0}`, false, true},
		{`{"is_hidden":true}`, true, true},
		{`{"is_hidden":false}`, false, true},
		{`{"hidden":true}`, true, true},
		{`{"hidden":false}`, false, true},
		{`{"hidden":1}`, true, true},
		{`{"hidden":0}`, false, true},
		{`{"fps":60}`, false, false},
		{`{}`, false, false},
		{`not json`, false, false},
		{``, false, false},
	}
	for _, c := range cases {
		hidden, present := HiddenFlag(json.RawMessage(c.raw))
		if hidden != c.hidden || present != c.found {
			t.Errorf("HiddenFlag(%s) = (%v,%v), want (%v,%v)", c.raw, hidden, present, c.hidden, c.found)
		}
	}
}
