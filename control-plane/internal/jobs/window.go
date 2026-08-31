package jobs

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// Schedule + run-window resolver: when may the next run start? The rule
// (design §3.3): when the interval fires outside the permitted window,
// scheduled_for snaps forward to the next window open. Two easy-to-invert
// properties: the interval runs from the END of the previous run (overlap is
// structurally impossible), and the window governs starting a run, never
// stopping one — an in-flight run is not killed at window close. Only the
// window is zone-sensitive; an interval is absolute time, so DST can shift
// when a run lands but never its length.

// maxWindowSearchDays bounds the forward scan for the next window open. Seven
// days covers any legal WindowDays set (every set that is non-empty contains a
// day that recurs weekly); the extra day absorbs a wrapping window whose open
// instant sits on the far side of a day boundary. Exceeding it means the schedule
// is unsatisfiable, which is reported rather than looped on.
const maxWindowSearchDays = 9

// LoadLocation resolves an IANA zone name, defaulting to UTC for "".
//
// Errors are returned, never swallowed to UTC. An operator who wrote
// "Europe/Londn" and silently got UTC would see their 02:00-06:00 window fire at
// the wrong time with nothing anywhere saying why.
func LoadLocation(name string) (*time.Location, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("timezone %q: unknown IANA zone", name)
	}
	return loc, nil
}

// HasWindow reports whether s constrains WHEN a run may start. WindowDays alone
// is a constraint even with no time bounds ("Sundays, any hour").
func (s Schedule) HasWindow() bool {
	return s.WindowStart != nil || len(s.WindowDays) > 0
}

// windowSpanSecs is one window occurrence's length in clock seconds. end <
// start wraps midnight; end == start is a full day, not zero-length — a
// zero-length window silently wedges a job forever, and 24h is the failure
// mode that runs the job. (The admin API rejects start == end; this only
// catches a hand-edited row.)
func (s Schedule) windowSpanSecs() int {
	const day = 24 * 3600
	start, end := s.WindowStart.secs(), s.WindowEnd.secs()
	switch {
	case end > start:
		return end - start
	case end < start:
		return day - start + end
	default:
		return day
	}
}

// dayAllowed applies WindowDays to the day the window OPENS. An empty set is
// every day.
func (s Schedule) dayAllowed(d time.Weekday) bool {
	if len(s.WindowDays) == 0 {
		return true
	}
	for _, w := range s.WindowDays {
		if time.Weekday(w) == d {
			return true
		}
	}
	return false
}

// NextWindowOpen returns the earliest instant at or after t at which s permits
// a run to start. t comes back unchanged when it is already inside a window
// (including one that opened yesterday and wraps midnight) and when s has no
// window at all — "no window" is the identity, not a caller special case.
// DST handling lives in windowOpenOn.
func NextWindowOpen(t time.Time, s Schedule, loc *time.Location) (time.Time, error) {
	if !s.HasWindow() {
		return t, nil
	}
	if loc == nil {
		loc = time.UTC
	}
	local := t.In(loc)

	// Day-constraint only, no time bounds: the whole permitted day is the window.
	if s.WindowStart == nil {
		for d := 0; d < maxWindowSearchDays; d++ {
			day := startOfDay(local).AddDate(0, 0, d)
			if !s.dayAllowed(day.Weekday()) {
				continue
			}
			if d == 0 {
				return t, nil
			}
			return day, nil
		}
		return time.Time{}, fmt.Errorf("job schedule: window_days %v never opens", s.WindowDays)
	}

	span := time.Duration(s.windowSpanSecs()) * time.Second
	// d starts at -1 so a window that opened yesterday and wraps midnight is
	// seen as currently open — otherwise 03:00 inside 22:00-04:00 would be
	// pushed to tonight's 22:00.
	for d := -1; d < maxWindowSearchDays; d++ {
		day := startOfDay(local).AddDate(0, 0, d)
		if !s.dayAllowed(day.Weekday()) {
			continue
		}
		open := windowOpenOn(day, *s.WindowStart, loc)
		shut := open.Add(span)
		if !t.Before(open) && t.Before(shut) {
			return t, nil
		}
		if open.After(t) {
			return open, nil
		}
	}
	return time.Time{}, fmt.Errorf("job schedule: window %s-%s on days %v never opens",
		s.WindowStart, s.WindowEnd, s.WindowDays)
}

// InWindow reports whether s permits a run to start at t.
func InWindow(t time.Time, s Schedule, loc *time.Location) bool {
	next, err := NextWindowOpen(t, s, loc)
	return err == nil && next.Equal(t)
}

// startOfDay is local midnight of d's calendar day. On a DST day whose midnight
// does not exist (a handful of zones shift at 00:00), time.Date normalizes
// forward, which only ever moves the anchor later within the same day.
func startOfDay(d time.Time) time.Time {
	return time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, d.Location())
}

// windowOpenOn resolves a wall-clock time on a calendar day in a zone to an
// instant, correctly across DST. The spring-forward gap is the case that needs
// code: time.Date resolves a nonexistent 02:00 to an instant an hour BEFORE
// the window was meant to open, which InWindow would then reject — so detect
// the normalization and fall back to midnight-plus-offset, the first instant
// of the day at or after the requested wall clock. Fall-back needs no special
// case: time.Date picks the first of the two occurrences, which is right for
// "run it once, in the small hours".
func windowOpenOn(day time.Time, at TimeOfDay, loc *time.Location) time.Time {
	open := time.Date(day.Year(), day.Month(), day.Day(), at.Hour, at.Minute, at.Second, 0, loc)
	if open.Hour() == at.Hour && open.Minute() == at.Minute && open.Second() == at.Second {
		return open
	}
	return startOfDay(day).Add(time.Duration(at.secs()) * time.Second)
}

// NextScheduledFor computes the scheduled_for of the next run of an interval
// job, given when the previous run FINISHED.
//
// lastFinished zero means the job has never run: the base is now, so a freshly
// registered (or freshly enabled) job gets a first pass promptly rather than
// after a full interval of silence — which matches the boot-then-tick behaviour
// of every janitor this framework replaces.
func NextScheduledFor(now, lastFinished time.Time, s Schedule, interval time.Duration, loc *time.Location) (time.Time, error) {
	base := now
	if !lastFinished.IsZero() {
		base = lastFinished.Add(interval)
		if base.Before(now) {
			// Down (or disabled) longer than one interval: catch up with ONE
			// run, never a backlog — a janitor owing forty passes does one.
			base = now
		}
	}
	return NextWindowOpen(base, s, loc)
}

// Deferral backoff ladder (design §3.4): 30s doubling, capped at 15 min,
// persisted in job_runs.attempt + scheduled_for so it survives an agent
// reconnect and a control-plane restart.
const (
	deferralBase = 30 * time.Second
	deferralCap  = 15 * time.Minute
)

// BackoffFor returns the delay before retrying after a run at the given attempt
// number deferred. attempt is the attempt that DEFERRED (>= 1), so the first
// deferral waits deferralBase.
func BackoffFor(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	// The `d < deferralCap` bound keeps the doubling away from overflow no
	// matter how long a job has been refusing.
	d := deferralBase
	for i := 1; i < attempt && d < deferralCap; i++ {
		d *= 2
	}
	if d > deferralCap {
		d = deferralCap
	}
	return d
}

// EffectiveInterval resolves the interval in force and says which source won.
// An env override is authoritative over the admin's value and reports itself
// as such, never silently merged (the interval_overridden_by_env treatment).
// A malformed override does not win — falling back to the admin's value beats
// scheduling on a zero.
func EffectiveInterval(j Job, envOverride string, lookup func(string) string) (d time.Duration, locked bool, lockedBy string) {
	d = time.Duration(j.Schedule.IntervalSecs) * time.Second
	if envOverride == "" {
		return d, false, ""
	}
	if lookup == nil {
		lookup = os.Getenv
	}
	raw := strings.TrimSpace(lookup(envOverride))
	if raw == "" {
		return d, false, ""
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil || parsed <= 0 {
		// A zero/negative override is the documented kill switch on
		// QUASAR_LIBRARY_SCAN_INTERVAL. Represent it as locked-at-zero and let the
		// dispatcher treat a non-positive effective interval as "do not schedule",
		// rather than inventing a third state here.
		if err == nil {
			return 0, true, envOverride
		}
		return d, false, ""
	}
	return parsed, true, envOverride
}
