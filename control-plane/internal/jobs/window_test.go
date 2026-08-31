// window_test.go — the schedule + run-window math. Pure unit tests: no database,
// so these run in a bare `go test ./...` with no TEST_DATABASE_URL.
//
// This is the file that earns the framework's window feature. The math has three
// ways to be quietly wrong — an off-by-one-day on a wrapping window, a
// day-of-week filter applied to the wrong end of the window, and a DST
// transition that lands a run in a wall-clock hour that does not exist — and
// each of them produces a job that runs at the wrong time or not at all, with
// nothing in a log to say so.
package jobs

import (
	"testing"
	"time"
)

func tod(s string) *TimeOfDay {
	t := MustTimeOfDay(s)
	return &t
}

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := LoadLocation(name)
	if err != nil {
		t.Fatalf("load %s: %v", name, err)
	}
	return loc
}

func TestNextWindowOpen(t *testing.T) {
	utc := time.UTC
	// 2026-08-12 is a Wednesday (time.Wednesday == 3).
	wed := func(h, m int) time.Time { return time.Date(2026, 8, 12, h, m, 0, 0, utc) }

	cases := []struct {
		name string
		at   time.Time
		sch  Schedule
		want time.Time
	}{
		{
			name: "no window is the identity",
			at:   wed(14, 0),
			sch:  Schedule{Timezone: "UTC"},
			want: wed(14, 0),
		},
		{
			name: "inside the window runs now",
			at:   wed(3, 0),
			sch:  Schedule{WindowStart: tod("02:00"), WindowEnd: tod("06:00"), Timezone: "UTC"},
			want: wed(3, 0),
		},
		{
			name: "before today's window snaps to today's open",
			at:   wed(1, 0),
			sch:  Schedule{WindowStart: tod("02:00"), WindowEnd: tod("06:00"), Timezone: "UTC"},
			want: wed(2, 0),
		},
		{
			name: "after today's window snaps to tomorrow's open",
			at:   wed(14, 0),
			sch:  Schedule{WindowStart: tod("02:00"), WindowEnd: tod("06:00"), Timezone: "UTC"},
			want: time.Date(2026, 8, 13, 2, 0, 0, 0, utc),
		},
		{
			name: "the window's closing instant is exclusive",
			at:   wed(6, 0),
			sch:  Schedule{WindowStart: tod("02:00"), WindowEnd: tod("06:00"), Timezone: "UTC"},
			want: time.Date(2026, 8, 13, 2, 0, 0, 0, utc),
		},
		{
			// The off-by-one-day this scan's d=-1 start exists to prevent: at 03:00
			// inside a 22:00->04:00 window, a naive same-day-only scan pushes the run
			// to tonight's 22:00 and delays it nineteen hours.
			name: "a wrapping window opened yesterday is still open",
			at:   wed(3, 0),
			sch:  Schedule{WindowStart: tod("22:00"), WindowEnd: tod("04:00"), Timezone: "UTC"},
			want: wed(3, 0),
		},
		{
			name: "a wrapping window is open after its start too",
			at:   wed(23, 30),
			sch:  Schedule{WindowStart: tod("22:00"), WindowEnd: tod("04:00"), Timezone: "UTC"},
			want: wed(23, 30),
		},
		{
			name: "midday is outside a wrapping window and snaps to tonight",
			at:   wed(12, 0),
			sch:  Schedule{WindowStart: tod("22:00"), WindowEnd: tod("04:00"), Timezone: "UTC"},
			want: wed(22, 0),
		},
		{
			// Wednesday -> the next permitted opening is Saturday (6).
			name: "window_days skips to the next permitted day",
			at:   wed(3, 0),
			sch: Schedule{WindowStart: tod("02:00"), WindowEnd: tod("06:00"),
				WindowDays: []int{0, 6}, Timezone: "UTC"},
			want: time.Date(2026, 8, 15, 2, 0, 0, 0, utc),
		},
		{
			// The day constrains the instant the window OPENS, so a Friday-only
			// wrapping window is open in Saturday's small hours.
			name: "window_days constrains the opening day, not the closing one",
			at:   time.Date(2026, 8, 15, 3, 0, 0, 0, utc), // Saturday 03:00
			sch: Schedule{WindowStart: tod("22:00"), WindowEnd: tod("04:00"),
				WindowDays: []int{5}, Timezone: "UTC"}, // Friday
			want: time.Date(2026, 8, 15, 3, 0, 0, 0, utc),
		},
		{
			name: "window_days alone permits the whole day",
			at:   wed(14, 0),
			sch:  Schedule{WindowDays: []int{0, 6}, Timezone: "UTC"},
			want: time.Date(2026, 8, 15, 0, 0, 0, 0, utc),
		},
		{
			// start == end is 24 hours, not zero. A zero-length window would be
			// unsatisfiable and would wedge the job silently forever.
			name: "start equal to end is a full day, never a dead window",
			at:   wed(14, 0),
			sch:  Schedule{WindowStart: tod("02:00"), WindowEnd: tod("02:00"), Timezone: "UTC"},
			want: wed(14, 0),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NextWindowOpen(tc.at, tc.sch, mustLoc(t, tc.sch.Timezone))
			if err != nil {
				t.Fatalf("NextWindowOpen: %v", err)
			}
			if !got.Equal(tc.want) {
				t.Fatalf("got %s, want %s", got.Format(time.RFC3339), tc.want.Format(time.RFC3339))
			}
		})
	}
}

// TestNextWindowOpenAcrossTimezone is the reason jobs.timezone exists at all:
// Michael's requirement is "02:00-06:00 host-local", and a window evaluated in
// UTC on a London instance fires an hour early for half the year.
func TestNextWindowOpenAcrossTimezone(t *testing.T) {
	london := mustLoc(t, "Europe/London")
	sch := Schedule{WindowStart: tod("02:00"), WindowEnd: tod("06:00"), Timezone: "Europe/London"}

	// 2026-08-12 14:00 UTC is 15:00 BST — outside the window. The next open is
	// 02:00 BST on the 13th, which is 01:00 UTC.
	at := time.Date(2026, 8, 12, 14, 0, 0, 0, time.UTC)
	got, err := NextWindowOpen(at, sch, london)
	if err != nil {
		t.Fatalf("NextWindowOpen: %v", err)
	}
	want := time.Date(2026, 8, 13, 1, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("got %s, want %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
	if h := got.In(london).Hour(); h != 2 {
		t.Fatalf("local open hour: got %d want 2", h)
	}
}

// TestNextWindowOpenDSTSpringForward — the transition where 02:00 local does not
// exist. New York moves 02:00 EST -> 03:00 EDT on 2026-03-08.
//
// The requirement is NOT that the run lands at a particular wall-clock time; it
// is that the window still opens ONCE, at a well-defined instant inside the
// intended stretch of night, rather than erroring or being skipped to the
// following day.
func TestNextWindowOpenDSTSpringForward(t *testing.T) {
	ny := mustLoc(t, "America/New_York")
	sch := Schedule{WindowStart: tod("02:00"), WindowEnd: tod("06:00"), Timezone: "America/New_York"}

	at := time.Date(2026, 3, 7, 12, 0, 0, 0, ny) // Saturday midday, outside
	got, err := NextWindowOpen(at, sch, ny)
	if err != nil {
		t.Fatalf("NextWindowOpen: %v", err)
	}
	local := got.In(ny)
	if local.Year() != 2026 || local.Month() != time.March || local.Day() != 8 {
		t.Fatalf("opened on the wrong day: %s", local.Format(time.RFC3339))
	}
	// 02:00 does not exist that morning; the first instant at or after it is
	// 03:00 EDT == 07:00 UTC. The wrong answer — the one time.Date returns
	// unaided — is 01:00 EST == 06:00 UTC, an hour BEFORE the window opens.
	if want := time.Date(2026, 3, 8, 7, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("got %s (%s local), want %s",
			got.UTC().Format(time.RFC3339), local.Format(time.RFC3339), want.Format(time.RFC3339))
	}
	// And the window is genuinely open there.
	if !InWindow(got, sch, ny) {
		t.Fatalf("the instant the window opened is not inside the window")
	}
}

// TestNextWindowOpenDSTFallBack — the transition where 01:30 local happens twice
// (New York 2026-11-01, 02:00 EDT -> 01:00 EST). The run must happen ONCE. Go
// resolves the ambiguity to the first occurrence; what this test pins is that
// the resolution is deterministic and that the resulting instant is inside the
// window, so the dispatcher does not oscillate across the transition.
func TestNextWindowOpenDSTFallBack(t *testing.T) {
	ny := mustLoc(t, "America/New_York")
	sch := Schedule{WindowStart: tod("01:30"), WindowEnd: tod("02:30"), Timezone: "America/New_York"}

	at := time.Date(2026, 10, 31, 12, 0, 0, 0, ny)
	first, err := NextWindowOpen(at, sch, ny)
	if err != nil {
		t.Fatalf("NextWindowOpen: %v", err)
	}
	again, err := NextWindowOpen(at, sch, ny)
	if err != nil {
		t.Fatalf("NextWindowOpen (repeat): %v", err)
	}
	if !first.Equal(again) {
		t.Fatalf("ambiguous local time resolved two ways: %s vs %s", first, again)
	}
	if !InWindow(first, sch, ny) {
		t.Fatalf("the instant the window opened is not inside the window")
	}
	if local := first.In(ny); local.Day() != 1 || local.Hour() != 1 || local.Minute() != 30 {
		t.Fatalf("unexpected local open: %s", local.Format(time.RFC3339))
	}
}

func TestNextScheduledForMeasuresFromTheEndOfThePreviousRun(t *testing.T) {
	utc := time.UTC
	now := time.Date(2026, 8, 12, 14, 0, 0, 0, utc)
	sch := Schedule{Kind: KindInterval, IntervalSecs: 900, Timezone: "UTC"}

	t.Run("never run yet fires immediately", func(t *testing.T) {
		got, err := NextScheduledFor(now, time.Time{}, sch, 15*time.Minute, utc)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Equal(now) {
			t.Fatalf("got %s want %s", got, now)
		}
	})

	t.Run("interval runs from finished_at, not started_at", func(t *testing.T) {
		// A pass that started at 13:50 and took ten minutes finished at 14:00; the
		// next one is due at 14:15, not 14:05. This is the timer-reset-after-pass
		// property that makes overlap structurally impossible.
		finished := now
		got, err := NextScheduledFor(now, finished, sch, 15*time.Minute, utc)
		if err != nil {
			t.Fatal(err)
		}
		if want := now.Add(15 * time.Minute); !got.Equal(want) {
			t.Fatalf("got %s want %s", got, want)
		}
	})

	t.Run("a long outage catches up with one run, not a backlog", func(t *testing.T) {
		finished := now.Add(-48 * time.Hour)
		got, err := NextScheduledFor(now, finished, sch, 15*time.Minute, utc)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Equal(now) {
			t.Fatalf("got %s want %s (a janitor owed 192 passes should do one)", got, now)
		}
	})

	t.Run("the interval fire instant is snapped into the window", func(t *testing.T) {
		windowed := Schedule{Kind: KindInterval, IntervalSecs: 3600,
			WindowStart: tod("02:00"), WindowEnd: tod("06:00"), Timezone: "UTC"}
		// Hourly + a 02:00-06:00 window: the 15:00 firing lands outside and snaps
		// forward to tomorrow's 02:00. Only four of the twenty-four land.
		got, err := NextScheduledFor(now, now, windowed, time.Hour, utc)
		if err != nil {
			t.Fatal(err)
		}
		want := time.Date(2026, 8, 13, 2, 0, 0, 0, utc)
		if !got.Equal(want) {
			t.Fatalf("got %s want %s", got, want)
		}
	})
}

func TestBackoffLadder(t *testing.T) {
	want := []time.Duration{
		30 * time.Second, // attempt 1 deferred
		time.Minute,
		2 * time.Minute,
		4 * time.Minute,
		8 * time.Minute,
		15 * time.Minute, // capped
		15 * time.Minute,
	}
	for i, w := range want {
		if got := BackoffFor(i + 1); got != w {
			t.Fatalf("BackoffFor(%d): got %s want %s", i+1, got, w)
		}
	}
	// A job that has been refusing for a very long time must not overflow into a
	// negative or absurd delay.
	if got := BackoffFor(10_000); got != 15*time.Minute {
		t.Fatalf("BackoffFor(10000): got %s want the cap", got)
	}
}

func TestEffectiveIntervalEnvOverridePrecedence(t *testing.T) {
	j := Job{Schedule: Schedule{Kind: KindInterval, IntervalSecs: 900}}
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}

	t.Run("no override named", func(t *testing.T) {
		d, locked, by := EffectiveInterval(j, "", env(nil))
		if d != 15*time.Minute || locked || by != "" {
			t.Fatalf("got %s locked=%v by=%q", d, locked, by)
		}
	})
	t.Run("named but unset leaves the admin in charge", func(t *testing.T) {
		d, locked, _ := EffectiveInterval(j, "QUASAR_X", env(nil))
		if d != 15*time.Minute || locked {
			t.Fatalf("got %s locked=%v", d, locked)
		}
	})
	t.Run("set wins and says so", func(t *testing.T) {
		d, locked, by := EffectiveInterval(j, "QUASAR_X", env(map[string]string{"QUASAR_X": "1h"}))
		if d != time.Hour || !locked || by != "QUASAR_X" {
			t.Fatalf("got %s locked=%v by=%q", d, locked, by)
		}
	})
	t.Run("zero is the documented kill switch, locked at zero", func(t *testing.T) {
		d, locked, by := EffectiveInterval(j, "QUASAR_X", env(map[string]string{"QUASAR_X": "0s"}))
		if d != 0 || !locked || by != "QUASAR_X" {
			t.Fatalf("got %s locked=%v by=%q", d, locked, by)
		}
	})
	t.Run("garbage does not win", func(t *testing.T) {
		// config.Load already fails startup on a malformed knob, so reaching here
		// with garbage means an unvalidated variable — falling back to the admin's
		// value beats scheduling on a zero.
		d, locked, _ := EffectiveInterval(j, "QUASAR_X", env(map[string]string{"QUASAR_X": "banana"}))
		if d != 15*time.Minute || locked {
			t.Fatalf("got %s locked=%v", d, locked)
		}
	})
}

func TestParseTimeOfDay(t *testing.T) {
	ok := map[string]TimeOfDay{
		"02:00":    {Hour: 2},
		"02:00:00": {Hour: 2},
		"22:15:30": {Hour: 22, Minute: 15, Second: 30},
		"0:05":     {Minute: 5},
	}
	for in, want := range ok {
		got, err := ParseTimeOfDay(in)
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if got != want {
			t.Fatalf("%q: got %+v want %+v", in, got, want)
		}
	}
	// 24:00 is rejected rather than normalized: it is ambiguous about which day it
	// belongs to, and a window bound that means two things is the seed of a missed
	// run nobody can explain.
	for _, bad := range []string{"", "24:00", "2:0", "02:60", "02:00:60", "2pm", "02-00"} {
		if _, err := ParseTimeOfDay(bad); err == nil {
			t.Fatalf("%q parsed but should not have", bad)
		}
	}
}

func TestLoadLocationRejectsUnknownZone(t *testing.T) {
	if _, err := LoadLocation("Europe/Londn"); err == nil {
		t.Fatal("a typo'd zone must fail, never fall back to UTC")
	}
	loc, err := LoadLocation("")
	if err != nil || loc != time.UTC {
		t.Fatalf("empty zone: got %v %v want UTC", loc, err)
	}
}
