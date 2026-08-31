package telemetry

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

var _ Store = (*Fake)(nil)

func TestFakeReadsAreNewestFirstAndClamped(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	for i := 0; i < 5; i++ {
		if err := f.Append(ctx, "s1", SourceAgent, SampleInput{TsUnixMs: int64(1000 + i)}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	got, next, err := f.Recent(ctx, "s1", 3, nil, "")
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	if len(got) != 3 || got[0].TsUnixMs != 1004 || got[2].TsUnixMs != 1002 {
		t.Fatalf("not newest-first page of 3: %+v", got)
	}
	if next != "3" {
		t.Fatalf("next cursor = %q, want 3", next)
	}
	page2, next2, err := f.Recent(ctx, "s1", 3, nil, next)
	if err != nil {
		t.Fatalf("recent page 2: %v", err)
	}
	if len(page2) != 2 || next2 != "" {
		t.Fatalf("page 2 = %d rows, next %q; want 2 rows and no cursor", len(page2), next2)
	}
}

func TestFakeLatestIsSourceScoped(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	_ = f.Append(ctx, "s1", SourceAgent, SampleInput{TsUnixMs: 10, Metrics: json.RawMessage(`{"frames_dropped":1}`)})
	_ = f.Append(ctx, "s1", SourceBrowser, SampleInput{TsUnixMs: 20, Metrics: json.RawMessage(`{"frames_dropped":9}`)})

	got, err := f.LatestPerSession(ctx, []string{"s1", "absent"})
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if _, ok := got["absent"]; ok {
		t.Fatal("a session with no telemetry must be absent from the map, not present-and-empty")
	}
	l := got["s1"]
	if l.Agent == nil || l.Browser == nil {
		t.Fatalf("both sources must survive: %+v", l)
	}
	// The whole reason Latest is a pair: the newer browser sample must not
	// overlay the agent's frames_dropped.
	if string(l.Agent.Metrics) != `{"frames_dropped":1}` {
		t.Fatalf("agent sample was overlaid: %s", l.Agent.Metrics)
	}
}

func TestFakeClockAbsenceIsUnmeasured(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	c, err := f.Clock(ctx, "s1")
	if err != nil || c != nil {
		t.Fatalf("unmeasured must be (nil, nil), got (%+v, %v)", c, err)
	}
	if err := f.UpsertClock(ctx, "s1", -12.5, 3); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	c, err = f.Clock(ctx, "s1")
	if err != nil || c == nil || c.ClientOffsetMs != -12.5 {
		t.Fatalf("clock round-trip: (%+v, %v)", c, err)
	}
}

func TestFakeWindowFiltersEventTypes(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	_ = f.AppendEvents(ctx, "s1", SourceAgent, []EventInput{
		{TsUnixMs: 100, Type: "abr.retarget"},
		{TsUnixMs: 200, Type: "encoder.drop_detected"},
	})
	_ = f.Append(ctx, "s1", SourceAgent, SampleInput{TsUnixMs: 150})

	sl, err := f.Window(ctx, "s1", Range{FromMs: 120, ToMs: 250}, Filter{Types: []string{"encoder.drop_detected"}})
	if err != nil {
		t.Fatalf("window: %v", err)
	}
	if len(sl.Events) != 1 || sl.Events[0].Type != "encoder.drop_detected" {
		t.Fatalf("event type filter: %+v", sl.Events)
	}
	if len(sl.Samples) != 1 || sl.Samples[0].TsUnixMs != 150 {
		t.Fatalf("samples must be window-bounded but NOT type-filtered: %+v", sl.Samples)
	}
}

func TestFakeCaptureLookup(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	_ = f.AppendEvent(ctx, "s1", SourceAgent, EventInput{
		TsUnixMs: 1, Type: "diag.pipeline_graph",
		Payload: json.RawMessage(`{"capture_id":"abc"}`),
	})
	if _, err := f.Capture(ctx, "s1", "nope"); err != ErrNotFound {
		t.Fatalf("a capture that has not landed must be ErrNotFound, got %v", err)
	}
	e, err := f.Capture(ctx, "s1", "abc")
	if err != nil || e.Type != "diag.pipeline_graph" {
		t.Fatalf("capture read: (%+v, %v)", e, err)
	}
	all, err := f.Captures(ctx, "s1")
	if err != nil || len(all) != 1 {
		t.Fatalf("captures: (%d, %v)", len(all), err)
	}
}

// The Fake's Retain must agree with Policy.Decide — that agreement is what makes
// the Fake usable as a stand-in for the Postgres adapter in handler tests.
func TestFakeRetainMatchesThePolicy(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	f := NewFake()
	f.Now = func() time.Time { return now }

	// live: two fresh rows, two aged rows, one aged capture.
	_ = f.Append(ctx, "live", SourceAgent, SampleInput{TsUnixMs: 1})
	_ = f.AppendEvent(ctx, "live", SourceAgent, EventInput{TsUnixMs: 1, Type: "abr.retarget"})
	_ = f.AppendEvent(ctx, "live", SourceAgent, EventInput{TsUnixMs: 2, Type: "diag.pipeline_graph"})
	f.Backdate("live", now.Add(-3*time.Hour))
	_ = f.Append(ctx, "live", SourceAgent, SampleInput{TsUnixMs: 3}) // fresh

	// terminal 8 h ago: inside the 24 h post-mortem, everything stays.
	_ = f.Append(ctx, "recent-dead", SourceAgent, SampleInput{TsUnixMs: 1})
	_ = f.UpsertClock(ctx, "recent-dead", 1, 1)
	f.Backdate("recent-dead", now.Add(-9*time.Hour))
	f.SetSessionState("recent-dead", true, now.Add(-8*time.Hour))

	// terminal 30 h ago: past the post-mortem, everything but the capture goes.
	_ = f.Append(ctx, "old-dead", SourceAgent, SampleInput{TsUnixMs: 1})
	_ = f.AppendEvent(ctx, "old-dead", SourceAgent, EventInput{TsUnixMs: 1, Type: "abr.retarget"})
	_ = f.AppendEvent(ctx, "old-dead", SourceAgent, EventInput{TsUnixMs: 2, Type: "diag.encoder_state"})
	_ = f.UpsertClock(ctx, "old-dead", 1, 1)
	f.Backdate("old-dead", now.Add(-31*time.Hour))
	f.SetSessionState("old-dead", true, now.Add(-30*time.Hour))

	rep, err := f.Retain(ctx, DefaultPolicy())
	if err != nil {
		t.Fatalf("retain: %v", err)
	}

	if f.CountSamples("live") != 1 {
		t.Fatalf("live: aged sample must go, fresh one must stay (have %d)", f.CountSamples("live"))
	}
	if f.CountEvents("live") != 1 {
		t.Fatalf("live: only the aged capture should remain (have %d)", f.CountEvents("live"))
	}
	if f.CountSamples("recent-dead") != 1 || !f.HasClock("recent-dead") {
		t.Fatal("a session that stopped 8 h ago must still be diagnosable")
	}
	if f.CountSamples("old-dead") != 0 || f.HasClock("old-dead") {
		t.Fatal("a session past its post-mortem must be swept, clock row included")
	}
	if f.CountEvents("old-dead") != 1 {
		t.Fatalf("the capture must survive the post-mortem sweep (have %d)", f.CountEvents("old-dead"))
	}
	if rep.RollingSamples != 1 || rep.RollingEvents != 1 {
		t.Fatalf("rolling counts: %+v", rep)
	}
	if rep.PostMortemSamples != 1 || rep.PostMortemEvents != 1 || rep.PostMortemClocks != 1 {
		t.Fatalf("post-mortem counts: %+v", rep)
	}
	if rep.Total() != 5 {
		t.Fatalf("total = %d, want 5", rep.Total())
	}
}

func TestFilterBrowserMetricsDropsUnknownKeys(t *testing.T) {
	got := FilterBrowserMetrics(json.RawMessage(`{"fps":60,"evil":"drop me","rtt_ms":12}`))
	var m map[string]any
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := m["evil"]; ok {
		t.Fatalf("unknown key survived: %s", got)
	}
	if len(m) != 2 {
		t.Fatalf("expected fps + rtt_ms, got %s", got)
	}
	for _, bad := range []json.RawMessage{nil, json.RawMessage(""), json.RawMessage("not json")} {
		if string(FilterBrowserMetrics(bad)) != "{}" {
			t.Fatalf("malformed input must yield {}, got %s", FilterBrowserMetrics(bad))
		}
	}
}
