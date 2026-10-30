package main

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/jobs"
)

// platform.release_detect as app.go registers it (amendment 48): its period comes
// from release_check unless the env override parses, and its window opens every day
// on daily.
func TestReleaseDetectDefinition(t *testing.T) {
	ctx := context.Background()
	period := 24 * time.Hour
	env := map[string]string{}
	getenv := func(k string) string { return env[k] }
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	build := func() jobs.Definition {
		def := releaseDetectDefinition(func(context.Context) (time.Duration, error) { return period, nil }, getenv, log)
		def.Run = func(context.Context, jobs.RunContext) (jobs.Outcome, error) { return jobs.Succeeded(nil), nil }
		if err := jobs.NewRegistry().Register(def); err != nil {
			t.Fatalf("register: %v", err)
		}
		return def
	}
	resolve := func(def jobs.Definition) (time.Duration, string) {
		d, locked, by, err := jobs.ResolveInterval(ctx, jobs.Job{Schedule: def.Default}, def, getenv)
		if err != nil || !locked {
			t.Fatalf("resolve: locked=%v err=%v", locked, err)
		}
		return d, by
	}

	def := build()
	if !def.DailyOpensEveryDay {
		t.Error("the window must open every day on daily")
	}
	if d, by := resolve(def); d != 24*time.Hour || by != "instance_settings.release_check" {
		t.Errorf("from the setting: %s locked by %q", d, by)
	}
	period = 7 * 24 * time.Hour
	if d, _ := resolve(def); d != period {
		t.Errorf("the setting moved to weekly, the job reads %s", d)
	}

	env[detectIntervalEnv] = "6h"
	if d, by := resolve(def); d != 6*time.Hour || by != detectIntervalEnv {
		t.Errorf("under the env override: %s locked by %q", d, by)
	}

	env[detectIntervalEnv] = "1d"
	def = build()
	if d, by := resolve(def); d != period || by != "instance_settings.release_check" {
		t.Errorf("a malformed override must not win: %s locked by %q", d, by)
	}
	resolve(def)
	if n := strings.Count(logs.String(), "malformed"); n != 1 {
		t.Errorf("malformed override logged %d times, want once:\n%s", n, logs.String())
	}
}
