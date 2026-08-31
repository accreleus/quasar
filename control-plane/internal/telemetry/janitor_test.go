package telemetry

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// The janitor's contract with an operator is its log line, so that is what is
// tested: exactly one INFO per pass, carrying the counts.
func TestRunRetentionLogsOneLineWithCounts(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	f := NewFake()
	f.Now = func() time.Time { return now }
	ctx := context.Background()
	_ = f.Append(ctx, "live", SourceAgent, SampleInput{TsUnixMs: 1})
	f.Backdate("live", now.Add(-3*time.Hour))

	rep, err := RunRetention(ctx, f, DefaultPolicy(), log)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if rep.RollingSamples != 1 {
		t.Fatalf("report: %+v", rep)
	}
	out := buf.String()
	if n := strings.Count(out, "telemetry retention"); n != 1 {
		t.Fatalf("want exactly one line per pass, got %d:\n%s", n, out)
	}
	for _, want := range []string{"rolling_samples=1", "total=1", "rolling=1h0m0s", "post_mortem=24h0m0s"} {
		if !strings.Contains(out, want) {
			t.Fatalf("line missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "level=WARN") {
		t.Fatalf("a fast, complete pass must not warn:\n%s", out)
	}
}

type slowStore struct {
	*Fake
	took time.Duration
}

func (s slowStore) Retain(ctx context.Context, p Policy) (Report, error) {
	rep, err := s.Fake.Retain(ctx, p)
	rep.Duration = s.took
	return rep, err
}

// A pass that takes longer than SlowRunThreshold is an operator-visible WARN:
// Retain batches specifically so a pass stays short, so a slow one means a
// backlog or a plan that has stopped working.
func TestRunRetentionWarnsOnASlowPass(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	s := slowStore{Fake: NewFake(), took: SlowRunThreshold + time.Second}
	if _, err := RunRetention(context.Background(), s, DefaultPolicy(), log); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(buf.String(), "pass was slow") {
		t.Fatalf("expected a slow-pass warning:\n%s", buf.String())
	}
}
