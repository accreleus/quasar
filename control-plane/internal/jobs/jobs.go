// Package jobs is the background-jobs framework: code-owned job definitions,
// an admin-owned schedule per job, and a `job_runs` row for every run on
// either plane. Design of record:
// docs/design/plans/2026-08-12-jobs-framework-and-viewer.md.
//
// Two shaping rules. The framework requests a run; the job decides — a job's
// own safety gate is never overridden, not by the schedule, not by "Run now";
// a refusal becomes a deferred outcome with a reason and a persisted backoff.
// And state is external (invariant #5): single-flight is a partial unique
// index, backoff is a column, a claim is a row — so a second dispatcher, a
// restart mid-run, and an agent dying with a claim are ordinary cases.
//
// This package contains no adopted job; with an empty registry the dispatcher
// materializes and claims nothing.
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	// Embed the tz database so IANA names (QUASAR_JOBS_TIMEZONE, per-row
	// timezone) resolve identically in the dev container, the scratch runtime
	// image and tests — no dependency on /usr/share/zoneinfo.
	_ "time/tzdata"
)

// Plane says which side of the control-plane/node-agent split executes a job.
// A PlaneControl job runs in this process; a PlaneAgent job is claimed over the
// agent pull channel and executed on the host. The record of the run is
// identical either way — that sameness is what lets one viewer serve both.
type Plane string

const (
	PlaneControl Plane = "control"
	PlaneAgent   Plane = "agent"
)

// Scope says what a run targets: the instance as a whole (host_id NULL) or one
// host (host_id set, one run row per host).
type Scope string

const (
	ScopeInstance Scope = "instance"
	ScopeHost     Scope = "host"
)

// ScheduleKind is how a run comes to exist: KindInterval fires IntervalSecs
// from the END of the previous run (overlap-proof by construction); KindEvent
// never fires on a clock — a run row comes from Dispatcher.Enqueue, and still
// gets last-run/result; KindManual only runs from an admin trigger.
type ScheduleKind string

const (
	KindInterval ScheduleKind = "interval"
	KindEvent    ScheduleKind = "event"
	KindManual   ScheduleKind = "manual"
)

// State is a run's lifecycle position.
//
//	materialize -> pending -> (claim) -> running -> succeeded
//	                                             -> failed
//	                                             -> deferred  (the job's own gate refused)
//	                                             -> skipped   (nothing to do / feature unconfigured)
//	                                             -> aborted   (claimed, never reported; reaped)
//
// StateSkipped is first-class because "configured but found nothing" and "not
// configured at all" must not both be silence.
type State string

const (
	StatePending   State = "pending"
	StateRunning   State = "running"
	StateSucceeded State = "succeeded"
	StateFailed    State = "failed"
	StateDeferred  State = "deferred"
	StateSkipped   State = "skipped"
	StateAborted   State = "aborted"
)

// Terminal reports whether s closes a run.
func (s State) Terminal() bool {
	switch s {
	case StateSucceeded, StateFailed, StateDeferred, StateSkipped, StateAborted:
		return true
	}
	return false
}

// Trigger records why a run exists — separate from State because "why did this
// run" and "how did it end" are independently interesting.
type Trigger string

const (
	TriggerSchedule Trigger = "schedule"
	TriggerManual   Trigger = "manual"
	TriggerEvent    Trigger = "event"
)

// TimeOfDay is a window bound: wall-clock time, no date, no zone — what
// Postgres `TIME` stores. Not a time.Time, which would invite comparing two
// window bounds as instants.
type TimeOfDay struct {
	Hour   int
	Minute int
	Second int
}

var timeOfDayRe = regexp.MustCompile(`^([0-9]{1,2}):([0-9]{2})(?::([0-9]{2}))?$`)

// ParseTimeOfDay accepts "HH:MM" and "HH:MM:SS" (what the admin API takes and
// what Postgres renders a TIME as).
func ParseTimeOfDay(s string) (TimeOfDay, error) {
	m := timeOfDayRe.FindStringSubmatch(s)
	if m == nil {
		return TimeOfDay{}, fmt.Errorf("time of day %q: want HH:MM or HH:MM:SS", s)
	}
	var t TimeOfDay
	_, _ = fmt.Sscanf(m[1], "%d", &t.Hour)
	_, _ = fmt.Sscanf(m[2], "%d", &t.Minute)
	if m[3] != "" {
		_, _ = fmt.Sscanf(m[3], "%d", &t.Second)
	}
	// 24:00:00 is rejected, not normalized to midnight — it is ambiguous about
	// which day it belongs to.
	if t.Hour > 23 || t.Minute > 59 || t.Second > 59 {
		return TimeOfDay{}, fmt.Errorf("time of day %q: out of range", s)
	}
	return t, nil
}

// MustTimeOfDay is ParseTimeOfDay for Definition literals and tests, where the
// input is a constant and a typo should fail loudly at init rather than produce
// a silently-wrong window.
func MustTimeOfDay(s string) TimeOfDay {
	t, err := ParseTimeOfDay(s)
	if err != nil {
		panic(err)
	}
	return t
}

func (t TimeOfDay) String() string {
	return fmt.Sprintf("%02d:%02d:%02d", t.Hour, t.Minute, t.Second)
}

// secs is the offset from local midnight. Used only for window arithmetic.
func (t TimeOfDay) secs() int { return t.Hour*3600 + t.Minute*60 + t.Second }

// Schedule is the admin-owned half of a job: when it may run. The code-owned
// half (identity) lives on Definition.
type Schedule struct {
	Kind         ScheduleKind
	IntervalSecs int32
	// WindowStart/WindowEnd are both nil or both set (a paired CHECK in the
	// migration). nil means "any time".
	WindowStart *TimeOfDay
	WindowEnd   *TimeOfDay
	// WindowDays constrains the day on which the window OPENS. 0=Sunday..6=Saturday,
	// matching time.Weekday. Empty means every day.
	WindowDays []int
	// Timezone is an IANA name. Windows are evaluated in it; intervals are not,
	// because an interval is a duration in absolute time and has no opinion about
	// what the clock on the wall says.
	Timezone string
}

// MinIntervalSecs mirrors the migration's CHECK. Enforced here too so a bad
// Definition fails at registration rather than at the first INSERT.
const MinIntervalSecs = 60

// Definition is the code-owned identity of a job, reconciled into the `jobs`
// table at every boot. The ID is a contract — API path segment, log field, row
// key, runbook name — stable, dotted, lowercase, and never renamed: a rename
// is a delete-plus-add that cascades away the job's history.
type Definition struct {
	ID          string
	Name        string
	Description string
	Plane       Plane
	Scope       Scope
	// Managed=false lists the job without ever scheduling it (design §3.7);
	// the set of unmanaged rows is the adoption backlog.
	Managed bool
	// Seeded on first sight only. Later syncs never touch a schedule column —
	// an admin owns those, and a boot that reset an operator's window because
	// someone edited this literal would make the surface useless.
	Default Schedule
	// Names an env var (e.g. QUASAR_ARTWORK_SWEEP_INTERVAL) that, when set, is
	// authoritative over the admin's interval (parsed as a Go duration); ""
	// means the admin always wins. Not
	// a column — which knob overrides which job is code, and a stored copy
	// could disagree with the code reading the environment.
	EnvOverride string
	// Control-plane implementation; nil for PlaneAgent and unmanaged jobs.
	Run RunFunc
	// Supplies run params for a manual trigger on a job that is meaningless
	// without them (the event path builds params from the event; "Run now"
	// carries none, and an empty blob used to fail on the host with `params
	// incomplete`). Per-job because only the job knows what its params mean.
	// An error refuses the trigger (ErrParamsUnavailable) rather than queueing
	// a run that cannot succeed; nil materializes a manual run with no params.
	ResolveParams ParamsResolver
}

// ParamsResolver builds the params blob for a manual run of one job on one
// target. hostID is "" for an instance-scoped job. Returning (nil, nil) means
// "no params needed"; returning an error refuses the trigger with that reason.
type ParamsResolver func(ctx context.Context, hostID string) (any, error)

// RunContext is what a control-plane RunFunc is told about the run it is serving.
// It is deliberately thin: a job that needs more should reach for its own
// collaborators, not have the framework grow fields.
type RunContext struct {
	JobID  string
	RunID  string
	HostID string // "" for instance-scoped jobs
	// Params is the opaque blob an event trigger carried. Empty for a scheduled run.
	Params json.RawMessage
	// Attempt is 1 for a fresh run and increments across deferral backoff.
	Attempt int
	// Trigger lets a job that genuinely must behave differently under an operator's
	// hand (nothing does today) see that it was hand-triggered.
	Trigger Trigger
}

// Summary is the bounded key/value result of a run — the numbers an operator
// actually wants: {"apps_considered": 412, "artwork_resolved": 3}. Marshalled
// into job_runs.summary under the same 4096-byte ceiling admin_activity uses.
type Summary map[string]any

// Outcome is how a run ended, as the job itself describes it.
type Outcome struct {
	State   State
	Summary Summary
	// Reason explains a Deferred or Skipped outcome in one operator-readable
	// phrase ("host has 1 live session(s)", "no artwork provider configured").
	// It is stored inside the summary under "reason".
	Reason string
}

// Succeeded / Skipped / Deferred are the constructors a RunFunc should use, so
// the reason-into-summary convention has one implementation.
func Succeeded(s Summary) Outcome { return Outcome{State: StateSucceeded, Summary: s} }

func Skipped(reason string) Outcome {
	return Outcome{State: StateSkipped, Reason: reason, Summary: Summary{"reason": reason}}
}

func Deferred(reason string) Outcome {
	return Outcome{State: StateDeferred, Reason: reason, Summary: Summary{"reason": reason}}
}

// RunFunc is a control-plane job body. Returning an error is a FAILURE; a job
// that chose not to act should return an Outcome with StateSkipped or
// StateDeferred and no error, because "I declined" and "I broke" must not look
// the same in the viewer.
//
// The ctx carries the claim deadline (QUASAR_JOBS_CLAIM_TIMEOUT_SECS) and the
// dispatcher's shutdown, so a well-behaved body that respects cancellation
// cannot outlive either.
type RunFunc func(ctx context.Context, rc RunContext) (Outcome, error)

// Job is a `jobs` row: a Definition's identity as persisted, plus the admin-owned
// schedule.
type Job struct {
	ID           string
	Name         string
	Description  string
	Plane        Plane
	Scope        Scope
	Managed      bool
	Enabled      bool
	Schedule     Schedule
	HistoryLimit int
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Run is a `job_runs` row.
type Run struct {
	ID           string
	JobID        string
	HostID       string // "" when NULL
	State        State
	Trigger      Trigger
	ActorUserID  string // "" when NULL
	Attempt      int
	ScheduledFor time.Time
	ClaimedAt    *time.Time
	StartedAt    *time.Time
	FinishedAt   *time.Time
	Params       json.RawMessage
	Summary      json.RawMessage
	Error        string
	CreatedAt    time.Time
}

// DurationMS is the wall time of a completed run, or nil while it is open.
func (r Run) DurationMS() *int64 {
	if r.StartedAt == nil || r.FinishedAt == nil {
		return nil
	}
	ms := r.FinishedAt.Sub(*r.StartedAt).Milliseconds()
	return &ms
}

// Errors the HTTP layer (WP3) maps to status codes, and the dispatcher raises
// directly. They are values, not strings, so a caller cannot map the wrong one
// by matching on prose.
var (
	// ErrNotFound: no such job id in the registry or the table.
	ErrNotFound = errors.New("jobs: not found")
	// ErrUnmanaged: the job is listed but not adopted; it has no schedule to edit
	// and no runner to trigger.
	ErrUnmanaged = errors.New("jobs: job is unmanaged")
	// ErrDisabled: a disabled job never runs, NOT EVEN MANUALLY. Disabling is the
	// operator's kill switch and it has to mean it, or it is not a kill switch.
	ErrDisabled = errors.New("jobs: job is disabled")
	// ErrAlreadyRunning: a run for this (job, target) is already claimed. Distinct
	// from "already pending", which is not an error — a manual trigger pulls a
	// pending row forward instead of queueing a second one.
	ErrAlreadyRunning = errors.New("jobs: a run is already in progress")
	// ErrHostRequired / ErrHostNotAllowed enforce the jobs.scope <-> job_runs.host_id
	// agreement that no row-level CHECK can express.
	ErrHostRequired   = errors.New("jobs: host_id is required for a host-scoped job")
	ErrHostNotAllowed = errors.New("jobs: host_id is not allowed for an instance-scoped job")
	// ErrScheduleLocked: an env var is authoritative over this job's interval.
	// Reported, never silently accepted — an edit the environment will
	// overrule is the "which source is in force" confusion.
	ErrScheduleLocked = errors.New("jobs: schedule is locked by an environment override")
	// ErrParamsUnavailable: ResolveParams could not supply the params a manual
	// trigger needs. Refused at the trigger, so the run never reaches the host
	// only to fail there with `params incomplete`. The wrapped cause carries
	// the operator-readable half.
	ErrParamsUnavailable = errors.New("jobs: run params are unavailable")
)

// summaryLimit mirrors the migration's CHECK and audit.Store's ceiling.
const summaryLimit = 4096

// marshalBounded renders a summary/params blob, refusing one that would violate
// the storage CHECK. A summary that blows the bound fails the REPORT, never the
// run: the work happened, and losing the record of it is strictly better than
// pretending the work did not.
func marshalBounded(v any, what string) (json.RawMessage, error) {
	if v == nil {
		return json.RawMessage(`{}`), nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal job %s: %w", what, err)
	}
	if len(b) > summaryLimit {
		return nil, fmt.Errorf("job %s exceeds %d bytes", what, summaryLimit)
	}
	return b, nil
}
