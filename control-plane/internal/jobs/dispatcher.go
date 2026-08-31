package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"
)

// The Dispatcher is one ticker replacing N per-job tickers. Per tick: reap
// abandoned claims, materialize due runs, claim + execute control-plane runs,
// prune history. Order matters: reaping first frees the single-flight slot so
// step 2 can re-materialize in the same tick; pruning last sees the run that
// just finished. Multi-instance safe: claims go through FOR UPDATE ... SKIP
// LOCKED, materialization through a per-job advisory lock plus a partial
// unique index — two control planes against one database do not double-fire.

// DefaultTickInterval / DefaultClaimTimeout / DefaultHistoryLimit /
// DefaultRetentionDays mirror docs/configuration.md's QUASAR_JOBS_* defaults.
const (
	DefaultTickInterval  = 10 * time.Second
	DefaultClaimTimeout  = time.Hour
	DefaultHistoryLimit  = 50
	DefaultRetentionDays = 30
)

// Config is the dispatcher's operator-visible knobs, resolved from the
// environment by internal/config and passed in whole.
type Config struct {
	// QUASAR_JOBS. False stops adopted jobs running at all (they no longer own
	// their own tickers) — a framework kill switch, never a way to run jobs
	// the old way. Per-job `enabled` is the single-job switch.
	Enabled bool
	// TickInterval is QUASAR_JOBS_TICK_SECS. It bounds manual-trigger latency for
	// control-plane jobs; agent-plane latency is bounded by the agent's own poll.
	TickInterval time.Duration
	// Timezone is QUASAR_JOBS_TIMEZONE: the SEED default for a newly registered
	// job's window. Never re-applied to an existing row.
	Timezone string
	// HistoryLimit is QUASAR_JOBS_RUN_RETENTION, the per-job default row cap.
	HistoryLimit int
	// RetentionDays is QUASAR_JOBS_RUN_RETENTION_DAYS, an age cap applied in
	// addition to the row cap. 0 disables the age rule.
	RetentionDays int
	// ClaimTimeout is QUASAR_JOBS_CLAIM_TIMEOUT_SECS: how long a claimed run may
	// go unreported before it is aborted and re-materialized. It must exceed the
	// longest expected job.
	ClaimTimeout time.Duration
}

// DefaultConfig is the framework as it ships: on, 10s tick, UTC windows, 50 runs
// or 30 days of history, one-hour claim timeout.
func DefaultConfig() Config {
	return Config{
		Enabled:       true,
		TickInterval:  DefaultTickInterval,
		Timezone:      "UTC",
		HistoryLimit:  DefaultHistoryLimit,
		RetentionDays: DefaultRetentionDays,
		ClaimTimeout:  DefaultClaimTimeout,
	}
}

func (c Config) normalized() Config {
	if c.TickInterval <= 0 {
		c.TickInterval = DefaultTickInterval
	}
	if c.Timezone == "" {
		c.Timezone = "UTC"
	}
	if c.HistoryLimit < 1 || c.HistoryLimit > 500 {
		c.HistoryLimit = DefaultHistoryLimit
	}
	if c.RetentionDays < 0 {
		c.RetentionDays = 0
	}
	if c.ClaimTimeout <= 0 {
		c.ClaimTimeout = DefaultClaimTimeout
	}
	return c
}

// Dispatcher owns the tick. Construct with New, start with Start, drive one pass
// deterministically in a test with Tick.
type Dispatcher struct {
	store *Store
	reg   *Registry
	cfg   Config
	log   *slog.Logger

	// now and getenv are test seams. Production uses time.Now and os.Getenv;
	// window and DST tests need to stand at a chosen instant, and env-override
	// tests need to not mutate the process environment.
	now    func() time.Time
	getenv func(string) string

	// running tracks in-flight control-plane run goroutines so Stop can wait for
	// them. A job body that outlives the process it reports to would leave a
	// `running` row for the reaper to abort — correct, but a needless hour of
	// looking broken.
	running sync.WaitGroup
}

// New builds a dispatcher. reg may be empty, which is WP1's shipping state.
func New(store *Store, reg *Registry, cfg Config, log *slog.Logger) *Dispatcher {
	return &Dispatcher{
		store:  store,
		reg:    reg,
		cfg:    cfg.normalized(),
		log:    log,
		now:    time.Now,
		getenv: os.Getenv,
	}
}

// Config returns the normalized configuration in force.
func (d *Dispatcher) Config() Config { return d.cfg }

// Start runs the tick loop until ctx is cancelled. The initial delay keeps
// startup off the critical path — same reason every janitor starts late.
func (d *Dispatcher) Start(ctx context.Context) {
	if !d.cfg.Enabled {
		// Loud: with the framework off, an adopted job does not run at all.
		d.log.Warn("job: dispatcher disabled by QUASAR_JOBS=0 — no scheduled job will run on this instance")
		return
	}
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
		t := time.NewTicker(d.cfg.TickInterval)
		defer t.Stop()
		for {
			d.Tick(ctx)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

// Wait blocks until every in-flight control-plane run has finished. Called after
// the dispatcher's context is cancelled, on shutdown.
func (d *Dispatcher) Wait() { d.running.Wait() }

// Tick performs one dispatcher pass. Exported so a test can drive a
// deterministic pass instead of waiting on a ticker — the same reason the
// library janitor exports RunOnce and artwork exports SweepOnce.
func (d *Dispatcher) Tick(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	d.reapAbandoned(ctx)
	d.materializeDue(ctx)
	d.executeControlRuns(ctx)
	d.prune(ctx)
}

// --- step 1: reap ----------------------------------------------------------

func (d *Dispatcher) reapAbandoned(ctx context.Context) {
	reaped, err := d.store.ReapAbandoned(ctx, d.cfg.ClaimTimeout)
	if err != nil {
		d.log.Warn("job: could not reap abandoned runs", "err", err)
		return
	}
	for _, r := range reaped {
		age := time.Duration(0)
		if r.ClaimedAt != nil {
			age = d.now().Sub(*r.ClaimedAt)
		}
		d.log.Warn("job: reaped abandoned run",
			"run_id", r.ID, "job_id", r.JobID, "host_id", hostField(r.HostID),
			"claimed_secs_ago", int(age.Seconds()))
	}
}

// ReclaimHostRuns is the eager half of reclamation (#492): the time-based
// reaper cannot fire before the claim timeout, but an agent re-register is a
// positive signal that every run it was executing is gone — and the only
// liveness signal the framework gets (no heartbeat refreshes a claim). Wired
// to session.Coordinator.AgentReconnected. Runs even with QUASAR_JOBS=0: with
// the tick off there is no reaper, and a wedge from before the switch flipped
// would otherwise outlive the process.
func (d *Dispatcher) ReclaimHostRuns(ctx context.Context, hostID, reason string) (int, error) {
	reclaimed, err := d.store.ReclaimHostRuns(ctx, hostID, reason)
	if err != nil {
		return 0, err
	}
	for _, r := range reclaimed {
		age := time.Duration(0)
		if r.ClaimedAt != nil {
			age = d.now().Sub(*r.ClaimedAt)
		}
		// Same field set as "job: reaped abandoned run", so one grep for `job: `
		// reconstructs the run end to end across both planes (design §3.8).
		d.log.Warn("job: reclaimed abandoned run on agent re-register",
			"run_id", r.ID, "job_id", r.JobID, "host_id", hostField(r.HostID),
			"claimed_secs_ago", int(age.Seconds()), "reason", reason)
	}
	return len(reclaimed), nil
}

// --- step 2: materialize ---------------------------------------------------

// materializeDue creates the pending row that IS the next run for every
// managed, enabled, interval job with no open run. Event and manual jobs are
// skipped — their runs come from Enqueue and RunNow.
func (d *Dispatcher) materializeDue(ctx context.Context) {
	jobs, err := d.store.List(ctx)
	if err != nil {
		d.log.Warn("job: could not list jobs", "err", err)
		return
	}
	var hosts []string
	for _, j := range jobs {
		if !j.Managed || !j.Enabled || j.Schedule.Kind != KindInterval {
			continue
		}
		def, known := d.reg.Get(j.ID)
		if !known {
			// The table is reconciled at boot, so this means the registry changed
			// under a running process. Skip rather than guess.
			continue
		}
		interval, locked, lockedBy := EffectiveInterval(j, def.EnvOverride, d.getenv)
		if interval <= 0 {
			// The documented kill-switch shape (QUASAR_LIBRARY_SCAN_INTERVAL=0):
			// pinned off in the environment, so nothing is scheduled.
			if locked {
				d.log.Debug("job: scheduling suppressed by environment override",
					"job_id", j.ID, "locked_by", lockedBy)
			}
			continue
		}
		loc, err := LoadLocation(j.Schedule.Timezone)
		if err != nil {
			d.log.Warn("job: unusable timezone, skipping", "job_id", j.ID,
				"timezone", j.Schedule.Timezone, "err", err)
			continue
		}

		targets := []string{""}
		if j.Scope == ScopeHost {
			if hosts == nil {
				hosts, err = d.store.HostIDs(ctx)
				if err != nil {
					d.log.Warn("job: could not enumerate hosts", "err", err)
					return
				}
			}
			targets = hosts
		}
		for _, target := range targets {
			d.materializeTarget(ctx, j, target, interval, loc)
		}
	}
}

func (d *Dispatcher) materializeTarget(ctx context.Context, j Job, hostID string, interval time.Duration, loc *time.Location) {
	_, open, err := d.store.OpenRun(ctx, j.ID, hostID)
	if err != nil {
		d.log.Warn("job: could not read open run", "job_id", j.ID, "host_id", hostField(hostID), "err", err)
		return
	}
	if open {
		// A pending row IS the next run; a running row is this one. Nothing to
		// materialize either way.
		return
	}
	last, hadRun, err := d.store.LastTerminalRun(ctx, j.ID, hostID)
	if err != nil {
		d.log.Warn("job: could not read last run", "job_id", j.ID, "host_id", hostField(hostID), "err", err)
		return
	}
	// Interval runs from the END of the previous pass; created_at fallback
	// keeps a hand-edited row from wedging the schedule.
	var lastFinished time.Time
	if hadRun {
		if last.FinishedAt != nil {
			lastFinished = *last.FinishedAt
		} else {
			lastFinished = last.CreatedAt
		}
	}
	// A scheduled run after any terminal outcome starts a fresh attempt; the
	// deferral ladder rides Report's follow-up row.
	const attempt = 1
	now := d.now()
	when, err := NextScheduledFor(now, lastFinished, j.Schedule, interval, loc)
	if err != nil {
		d.log.Warn("job: unsatisfiable run window, skipping", "job_id", j.ID, "err", err)
		return
	}
	run, created, err := d.store.Materialize(ctx, MaterializeParams{
		JobID:        j.ID,
		HostID:       hostID,
		Trigger:      TriggerSchedule,
		ScheduledFor: when,
		Attempt:      attempt,
	})
	if err != nil {
		if errors.Is(err, ErrDisabled) || errors.Is(err, ErrUnmanaged) || errors.Is(err, ErrNotFound) {
			return
		}
		d.log.Warn("job: could not materialize run", "job_id", j.ID, "host_id", hostField(hostID), "err", err)
		return
	}
	// Say out loud when a window pushed a run out — the operator wondering why
	// nothing happened at 14:00 should find the answer in the log.
	if created && j.Schedule.HasWindow() && when.After(now.Add(time.Second)) {
		d.log.Info("job: window closed", "job_id", j.ID, "host_id", hostField(hostID),
			"next_run_at", when.In(loc).Format(time.RFC3339), "run_id", run.ID)
	}
}

// --- step 3: execute -------------------------------------------------------

func (d *Dispatcher) executeControlRuns(ctx context.Context) {
	claimed, err := d.store.ClaimDue(ctx, ClaimOptions{
		Plane: PlaneControl,
		Now:   d.now(),
		// The same per-poll cap the agent channel uses. A dispatcher that claimed
		// everything due at once after an outage would start every job in the
		// instance simultaneously.
		Limit: 5,
	})
	if err != nil {
		d.log.Warn("job: could not claim control-plane runs", "err", err)
		return
	}
	for _, run := range claimed {
		def, known := d.reg.Get(run.JobID)
		if !known || def.Run == nil {
			// Nothing can execute this. Close it honestly rather than leave it for
			// the reaper an hour later.
			d.finish(ctx, run, Outcome{State: StateFailed}, fmt.Errorf("no runner registered for %s", run.JobID))
			continue
		}
		d.log.Info("job: run started", "job_id", run.JobID, "run_id", run.ID,
			"host_id", hostField(run.HostID), "trigger", string(run.Trigger), "attempt", run.Attempt)
		d.running.Add(1)
		go func(run Run, def Definition) {
			defer d.running.Done()
			// The claim timeout is the run's deadline, so a wedged body cannot hold
			// its single-flight slot past the point the reaper would have freed it.
			runCtx, cancel := context.WithTimeout(ctx, d.cfg.ClaimTimeout)
			defer cancel()
			out, err := d.invoke(runCtx, run, def)
			d.finish(ctx, run, out, err)
		}(run, def)
	}
}

// invoke calls a RunFunc with panic recovery. The dispatcher must never die
// because a job did — one bad sweep taking the scheduler with it would stop
// every other job in the instance.
func (d *Dispatcher) invoke(ctx context.Context, run Run, def Definition) (out Outcome, err error) {
	defer func() {
		if r := recover(); r != nil {
			out = Outcome{State: StateFailed}
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return def.Run(ctx, RunContext{
		JobID:   run.JobID,
		RunID:   run.ID,
		HostID:  run.HostID,
		Params:  run.Params,
		Attempt: run.Attempt,
		Trigger: run.Trigger,
	})
}

// finish records the outcome on a background-derived context: a run cancelled
// by shutdown still has a result worth persisting, and losing it would leave a
// `running` row for the reaper to abort an hour later with no explanation.
func (d *Dispatcher) finish(parent context.Context, run Run, out Outcome, runErr error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 10*time.Second)
	defer cancel()

	state := out.State
	errText := ""
	if runErr != nil {
		state = StateFailed
		errText = runErr.Error()
	}
	if state == "" {
		state = StateSucceeded
	}
	summary := out.Summary
	if summary == nil {
		summary = Summary{}
	}
	if out.Reason != "" {
		summary["reason"] = out.Reason
	}
	if _, err := d.Report(ctx, run.ID, state, summary, errText); err != nil {
		d.log.Error("job: could not record run outcome",
			"job_id", run.JobID, "run_id", run.ID, "state", string(state), "err", err)
	}
}

// Report closes a run and applies the deferral ladder — the one path both the
// in-process executor and the agent pull channel's report endpoint take, so
// "what happens after a run ends" has one implementation.
func (d *Dispatcher) Report(ctx context.Context, runID string, state State, summary any, errText string) (Run, error) {
	run, applied, err := d.store.Report(ctx, runID, state, summary, errText)
	if err != nil {
		return Run{}, err
	}
	if !applied {
		// Usually an idempotent retry of a report already recorded — not logged
		// as a second outcome. Unless the stored row is `aborted`: only the
		// framework writes that state, so a report finding it means a real
		// verdict lost a race to the reaper/reclaim and was discarded. That
		// must never be silent — the history now disagrees with what happened
		// on the host, and a dropped `deferred` also loses its retry row
		// (scheduleDeferral fires only from an applied report).
		if run.State == StateAborted {
			d.log.Warn("job: report lost to reclaim",
				"job_id", run.JobID, "run_id", run.ID, "host_id", hostField(run.HostID),
				"reported_state", string(state), "stored_state", string(run.State),
				"deferral_dropped", state == StateDeferred, "stored_err", run.Error)
		}
		return run, nil
	}

	dur := int64(0)
	if ms := run.DurationMS(); ms != nil {
		dur = *ms
	}
	reason := summaryReason(run.Summary)
	switch run.State {
	case StateFailed:
		d.log.Warn("job: run failed", "job_id", run.JobID, "run_id", run.ID,
			"host_id", hostField(run.HostID), "state", string(run.State),
			"dur_ms", dur, "err", run.Error)
	case StateSkipped:
		d.log.Info("job: run skipped", "job_id", run.JobID, "run_id", run.ID,
			"host_id", hostField(run.HostID), "dur_ms", dur, "reason", reason)
	case StateDeferred:
		retryAt := d.scheduleDeferral(ctx, run)
		d.log.Info("job: run deferred", "job_id", run.JobID, "run_id", run.ID,
			"host_id", hostField(run.HostID), "reason", reason,
			"retry_at", retryAt, "attempt", run.Attempt)
	default:
		d.log.Info("job: run finished", "job_id", run.JobID, "run_id", run.ID,
			"host_id", hostField(run.HostID), "state", string(run.State),
			"dur_ms", dur, "summary", compactJSON(run.Summary))
	}
	return run, nil
}

// scheduleDeferral materializes the follow-up run after a gate refused. The
// backoff is persisted — it survives reconnects and restarts, and an operator
// can see it; a succeeded or skipped run resets attempt by starting fresh. A
// manually triggered deferral does not snap to the window: the operator asked
// for this run now, and making its retry wait for 02:00 would be the framework
// overruling a human.
func (d *Dispatcher) scheduleDeferral(ctx context.Context, run Run) string {
	when := d.now().Add(BackoffFor(run.Attempt))
	job, err := d.store.Get(ctx, run.JobID)
	if err != nil {
		d.log.Warn("job: could not read job for deferral backoff", "job_id", run.JobID, "err", err)
		return ""
	}
	if run.Trigger != TriggerManual {
		loc, lerr := LoadLocation(job.Schedule.Timezone)
		if lerr == nil {
			if snapped, serr := NextWindowOpen(when, job.Schedule, loc); serr == nil {
				when = snapped
			}
		}
	}
	next, _, err := d.store.Materialize(ctx, MaterializeParams{
		JobID:        run.JobID,
		HostID:       run.HostID,
		Trigger:      run.Trigger,
		ActorUserID:  run.ActorUserID,
		ScheduledFor: when,
		Attempt:      run.Attempt + 1,
		Params:       rawOrNil(run.Params),
	})
	if err != nil {
		if !errors.Is(err, ErrDisabled) && !errors.Is(err, ErrUnmanaged) {
			d.log.Warn("job: could not schedule deferral retry", "job_id", run.JobID, "err", err)
		}
		return ""
	}
	return next.ScheduledFor.UTC().Format(time.RFC3339)
}

// --- step 4: prune ---------------------------------------------------------

func (d *Dispatcher) prune(ctx context.Context) {
	n, err := d.store.PruneRuns(ctx, d.cfg.RetentionDays)
	if err != nil {
		d.log.Warn("job: could not prune run history", "err", err)
		return
	}
	if n > 0 {
		d.log.Debug("job: pruned run history", "rows", n)
	}
}

// --- triggers --------------------------------------------------------------

// Enqueue is the event trigger: a pending run for a KindEvent job. params is
// the event's opaque blob, handed to the runner verbatim. An already-open run
// is returned, not duplicated — a burst of events collapses into one run, the
// cap-1-channel coalescing the hand-rolled workers had.
func (d *Dispatcher) Enqueue(ctx context.Context, jobID, hostID string, params any) (Run, error) {
	job, err := d.store.Get(ctx, jobID)
	if err != nil {
		return Run{}, err
	}
	when := d.now()
	if job.Schedule.HasWindow() {
		loc, lerr := LoadLocation(job.Schedule.Timezone)
		if lerr != nil {
			return Run{}, lerr
		}
		if when, err = NextWindowOpen(when, job.Schedule, loc); err != nil {
			return Run{}, err
		}
	}
	run, _, err := d.store.Materialize(ctx, MaterializeParams{
		JobID:        jobID,
		HostID:       hostID,
		Trigger:      TriggerEvent,
		ScheduledFor: when,
		Params:       params,
	})
	return run, err
}

// RunNow is the admin trigger. It ignores the window and never ignores the
// job's own gates — an operator can always ask, the job can always refuse, and
// the refusal is a recorded `deferred` outcome. A pending run is pulled
// forward, not queued twice; running returns ErrAlreadyRunning; disabled
// returns ErrDisabled — a kill switch a button can bypass is not a kill
// switch.
func (d *Dispatcher) RunNow(ctx context.Context, jobID, hostID, actorUserID string) (Run, error) {
	params, err := d.manualParams(ctx, jobID, hostID)
	if err != nil {
		return Run{}, err
	}
	run, _, err := d.store.Materialize(ctx, MaterializeParams{
		JobID:        jobID,
		HostID:       hostID,
		Trigger:      TriggerManual,
		ActorUserID:  actorUserID,
		ScheduledFor: d.now(),
		Params:       params,
	})
	if err != nil {
		return Run{}, err
	}
	if run.State == StateRunning {
		return run, ErrAlreadyRunning
	}
	return run, nil
}

// manualParams runs the job's ParamsResolver for a manual trigger (nil when
// the job has none). It does not resolve when an open run already carries
// params: a manual trigger on a pending event run pulls it forward keeping the
// event's params — re-resolving could refuse a trigger for a run that is
// already perfectly formed.
func (d *Dispatcher) manualParams(ctx context.Context, jobID, hostID string) (any, error) {
	if d.reg == nil {
		return nil, nil
	}
	def, ok := d.reg.Get(jobID)
	if !ok || def.ResolveParams == nil {
		return nil, nil
	}
	if open, found, err := d.store.OpenRun(ctx, jobID, hostID); err == nil && found {
		if p := rawOrNil(open.Params); p != nil {
			return nil, nil
		}
	}
	params, err := def.ResolveParams(ctx, hostID)
	if err != nil {
		// %w on the sentinel, %v on the cause: the cause is the operator-readable
		// half and is rendered into the HTTP body by writeRunNowErr.
		return nil, fmt.Errorf("%w: %v", ErrParamsUnavailable, err)
	}
	return params, nil
}

// --- helpers ---------------------------------------------------------------

// hostField renders a host id for a log line, using "-" for instance scope so
// every `job: ` line has the same field set and a single grep reconstructs a run
// end to end across both planes.
func hostField(hostID string) string {
	if hostID == "" {
		return "-"
	}
	return hostID
}

func compactJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	return string(raw)
}

func rawOrNil(raw json.RawMessage) any {
	if len(raw) == 0 || string(raw) == "{}" {
		return nil
	}
	return raw
}
