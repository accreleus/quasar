package telemetry

import (
	"testing"
	"time"
)

// The policy is pure, so its test is a table: given a row's ingestion time, the
// owning session's state, and the event type, what happens to it.

func TestPolicyDecide(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	p := DefaultPolicy() // rolling 1h, post-mortem 24h

	cases := []struct {
		name string
		row  Row
		want Disposition
	}{
		{
			name: "live session, fresh sample: kept",
			row:  Row{CreatedAt: now.Add(-5 * time.Minute)},
			want: Keep,
		},
		{
			name: "live session, sample just inside the window: kept",
			row:  Row{CreatedAt: now.Add(-59 * time.Minute)},
			want: Keep,
		},
		{
			name: "live session, sample past the window: swept",
			row:  Row{CreatedAt: now.Add(-61 * time.Minute)},
			want: SweepRolling,
		},
		{
			name: "terminal session, hours-old sample inside post-mortem: KEPT",
			// This is the behaviour the whole change exists for: the rolling
			// window is NOT re-applied once a session is terminal, so the last
			// hour of a failed session survives until the post-mortem expires.
			row: Row{
				CreatedAt:       now.Add(-9 * time.Hour),
				SessionTerminal: true,
				SessionEndedAt:  now.Add(-8 * time.Hour),
			},
			want: Keep,
		},
		{
			name: "terminal session past post-mortem: swept",
			row: Row{
				CreatedAt:       now.Add(-30 * time.Hour),
				SessionTerminal: true,
				SessionEndedAt:  now.Add(-25 * time.Hour),
			},
			want: SweepPostMortem,
		},
		{
			name: "terminal session exactly at post-mortem: swept",
			row: Row{
				CreatedAt:       now.Add(-25 * time.Hour),
				SessionTerminal: true,
				SessionEndedAt:  now.Add(-24 * time.Hour),
			},
			want: SweepPostMortem,
		},
		{
			name: "capture on a live session, ancient: EXEMPT",
			row:  Row{CreatedAt: now.Add(-100 * time.Hour), Type: "diag.pipeline_graph"},
			want: Keep,
		},
		{
			name: "capture on a long-dead session: EXEMPT",
			row: Row{
				CreatedAt:       now.Add(-100 * time.Hour),
				SessionTerminal: true,
				SessionEndedAt:  now.Add(-99 * time.Hour),
				Type:            "diag.encoder_state",
			},
			want: Keep,
		},
		{
			name: "an ordinary event on a live session ages out like a sample",
			row:  Row{CreatedAt: now.Add(-2 * time.Hour), Type: "abr.retarget"},
			want: SweepRolling,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := p.Decide(tc.row, now); got != tc.want {
				t.Fatalf("Decide = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestPolicyValidate(t *testing.T) {
	if err := DefaultPolicy().Validate(); err != nil {
		t.Fatalf("default policy must validate: %v", err)
	}
	bad := []Policy{
		{Rolling: 0, PostMortem: time.Hour},
		{Rolling: time.Hour, PostMortem: 0},
		{Rolling: 2 * time.Hour, PostMortem: time.Hour}, // post-mortem < rolling
	}
	for i, p := range bad {
		if err := p.Validate(); err == nil {
			t.Fatalf("case %d: expected a validation error for %+v", i, p)
		}
	}
}

// A post-mortem shorter than the rolling window would delete the evidence it
// exists to keep; normalized() must not let a Retain pass run that way even if a
// caller bypasses Validate.
func TestPolicyNormalizeRaisesPostMortemToRolling(t *testing.T) {
	p := Policy{Rolling: 2 * time.Hour, PostMortem: 30 * time.Minute}.normalized()
	if p.PostMortem < p.Rolling {
		t.Fatalf("post-mortem %s < rolling %s after normalize", p.PostMortem, p.Rolling)
	}
}

func TestIsCapture(t *testing.T) {
	for _, tc := range []struct {
		typ  string
		want bool
	}{
		{"diag.pipeline_graph", true},
		{"diag.", true},
		{"diag", false},
		{"", false},
		{"abr.retarget", false},
		{"operator.annotation", false},
	} {
		if got := IsCapture(tc.typ); got != tc.want {
			t.Fatalf("IsCapture(%q) = %v, want %v", tc.typ, got, tc.want)
		}
	}
}
