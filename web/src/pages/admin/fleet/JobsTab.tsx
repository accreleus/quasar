/**
 * `/admin/fleet/jobs` — every background job on either plane, its schedule and
 * run state, and the actions that reach it.
 *
 * The table is written out rather than built on the shared `Table`: that
 * component has no notion of the group-header rows this page needs. Unmanaged
 * host-scoped jobs get their own section because they carry no `targets` to
 * attribute them to.
 */

import { Fragment, useMemo, useState, type ReactNode } from "react";
import * as adminApi from "../../../api/admin";
import { useAuth } from "../../../auth/context";
import { ApiError } from "../../../api/client";
import type { Job, JobPatchRequest, JobRun, JobRunState, JobSchedule } from "../../../api/types";
import { ActionsMenu, type ActionsMenuEntry } from "../../../components/ActionsMenu";
import { Button } from "../../../components/Button";
import { Chip } from "../../../components/Chip";
import { Drawer } from "../../../components/Drawer";
import { IconChevronRight, IconLock, IconRefresh, IconSearch } from "../../../components/icons";
import { Modal } from "../../../components/Modal";
import { SegmentedControl } from "../../../components/SegmentedControl";
import { Checkbox, Switch, TextField } from "../../../components/TextField";
import { useToast } from "../../../components/Toast";
import { ResourceStates } from "../../../components/ResourceStates";
import { useAdminAction } from "../../../lib/resource/action";
import { useResource } from "../../../lib/resource/react";
import {
  relativeTime,
  relativeTimeCompact,
  relativeTimeFuture,
} from "../../../lib/format/relativeTime";
import { useSectionHead } from "../../../components/shell/sectionHead";
import { hostLabel, rollupTargets, type HostRollup } from "./jobsDerived";
import {
  DAY_ABBR,
  fmtDurationMs,
  fmtSchedule,
  patchErrorMessage,
  resultChip,
  runNowErrorMessage,
  summaryText,
  triggerLabel,
  trimSeconds,
} from "./jobsFormat";
import "../../../styles/admin/fleet.css";

const POLL_MS = 5000;

const NONE = "—";

/** Key `runPending` by (job, host): the same action on two targets of one
 *  host-scoped job must be independently pending. */
function runKey(jobId: string, hostId?: string): string {
  return `${jobId}|${hostId ?? ""}`;
}

/** A managed host-scoped job's roll-up; null for every other shape, which
 *  carries its run state at the top level instead. */
function rollupFor(job: Job): HostRollup | null {
  return job.managed && job.scope === "host" ? rollupTargets(job.targets) : null;
}

/** True when the job's most recent outcome was a failure, on either shape. */
function isFailing(job: Job): boolean {
  if ((job.consecutive_failures ?? 0) > 0) return true;
  if (job.last_run?.state === "failed") return true;
  return rollupFor(job)?.lastRun?.run.state === "failed";
}

/** `running` is a live flag rather than a run state, so it outranks the last
 *  terminal run. A job that has never run shows no result at all (§6.3). */
function ResultCell({
  running,
  lastRun,
  detail,
}: {
  running: boolean;
  /** Structural, so a `JobRun` and a `DerivedRun` share one cell. */
  lastRun: { state: JobRunState; error: string | null; finished_at: string | null } | null;
  detail?: string;
}) {
  if (running) return <Chip variant="info" dot title={detail}>Running</Chip>;
  if (!lastRun) return <span className="sub">{NONE}</span>;
  const { variant, label } = resultChip(lastRun.state);
  const when = lastRun.finished_at ?? undefined;
  return (
    <Chip variant={variant} title={lastRun.error ?? detail ?? when}>
      {label}
    </Chip>
  );
}

/** Compact age plus duration; the exact instant and any caller detail live in
 *  the title. */
function RunAgo({
  finishedAt,
  durationMs,
  detail,
}: {
  finishedAt: string;
  durationMs: number | null;
  detail?: string;
}) {
  return (
    <span className="sub" title={detail ? `${detail} · ${finishedAt}` : finishedAt}>
      {relativeTimeCompact(finishedAt)}
      {" · "}
      {fmtDurationMs(durationMs)}
    </span>
  );
}

interface JobColumn {
  key: string;
  header: string;
  /** Cell class; only the Job column takes one, being the wrapping cell. */
  className?: string;
  render: (job: Job, rollup: HostRollup | null) => ReactNode;
}

interface HistoryTarget {
  job: Job;
  hostId?: string;
  hostLabel?: string;
}

type Segment = "all" | "failing" | "unmanaged";

export function JobsTab() {
  const { token } = useAuth();
  const { addToast } = useToast();

  const [expandedIds, setExpandedIds] = useState<Set<string>>(new Set());
  const [runPending, setRunPending] = useState<Set<string>>(new Set());
  const [scheduleTarget, setScheduleTarget] = useState<Job | null>(null);
  const [historyTarget, setHistoryTarget] = useState<HistoryTarget | null>(null);
  const [segment, setSegment] = useState<Segment>("all");
  const [query, setQuery] = useState("");

  const res = useResource({
    label: "jobs",
    pollMs: POLL_MS,
    initialData: [] as Job[],
    fetch: async (ctx) => (await adminApi.listJobs(ctx.token)).items,
  });
  const jobs = res.data ?? [];

  const failingCount = jobs.filter(isFailing).length;
  const unmanagedCount = jobs.filter((j) => !j.managed).length;

  const visible = useMemo(() => {
    const text = query.trim().toLowerCase();
    return jobs.filter((job) => {
      if (segment === "failing" && !isFailing(job)) return false;
      if (segment === "unmanaged" && job.managed) return false;
      if (!text) return true;
      return (
        job.name.toLowerCase().includes(text) || job.description.toLowerCase().includes(text)
      );
    });
  }, [jobs, segment, query]);

  const controlPlaneJobs = visible.filter((j) => j.scope !== "host");
  const hostJobs = visible.filter((j) => j.scope === "host" && j.managed);
  const unmanagedHostJobs = visible.filter((j) => j.scope === "host" && !j.managed);

  const toggleExpand = (jobId: string) => {
    setExpandedIds((prev) => {
      const next = new Set(prev);
      if (next.has(jobId)) next.delete(jobId);
      else next.add(jobId);
      return next;
    });
  };

  // Not useAdminAction: an instance-scoped job and each of a host-scoped job's
  // targets can be in flight at once, which is a set, not one newest call.
  async function handleRunNow(job: Job, hostId?: string) {
    const key = runKey(job.id, hostId);
    setRunPending((prev) => new Set(prev).add(key));
    try {
      const queued = await res.mutate((ctx) =>
        adminApi.runJobNow(ctx.token, job.id, hostId ? { host_id: hostId } : {}),
      );
      addToast({ variant: "success", title: `${job.name} queued`, body: queued.eta_note });
      await res.refresh({ silent: true });
    } catch (e: unknown) {
      const msg =
        e instanceof ApiError ? runNowErrorMessage(e.code, e.message) : "Could not run this job.";
      addToast({ variant: "danger", title: "Could not start this job", body: msg });
    } finally {
      setRunPending((prev) => {
        const next = new Set(prev);
        next.delete(key);
        return next;
      });
    }
  }

  const toggleEnabled = useAdminAction<[Job], Job>(
    (job) =>
      res.mutate(
        (ctx) => adminApi.patchJob(ctx.token, job.id, { enabled: !job.enabled }),
        (items, updated) => items.map((j) => (j.id === updated.id ? updated : j)),
      ),
    {
      success: (updated, job) => (updated.enabled ? `${job.name} enabled` : `${job.name} disabled`),
      failure: (e, job) => ({
        title: "Could not update job",
        body:
          e instanceof ApiError
            ? patchErrorMessage(e.code, e.message, job.schedule.locked_by)
            : "Could not update this job.",
      }),
    },
  );
  const handleToggleEnabled = (job: Job) => void toggleEnabled.run(job);

  function actionsForJob(job: Job): ActionsMenuEntry[] {
    // Unmanaged: nothing to run, schedule or toggle.
    if (!job.managed) {
      return [
        {
          key: "run",
          label: "Run now",
          disabled: true,
          title: "Not yet adopted by the job framework",
          onClick: () => {},
        },
      ];
    }

    const items: ActionsMenuEntry[] = [];
    if (job.scope === "instance") {
      const pending = runPending.has(runKey(job.id));
      items.push({
        key: "run",
        label: pending ? "Running…" : "Run now",
        disabled: !job.enabled || pending || Boolean(job.running),
        onClick: () => void handleRunNow(job),
      });
    }
    items.push({ key: "history", label: "View history", onClick: () => setHistoryTarget({ job }) });
    items.push({ key: "sep", separator: true });
    items.push({ key: "edit", label: "Edit schedule", onClick: () => setScheduleTarget(job) });
    items.push({
      key: "toggle",
      label: job.enabled ? "Disable" : "Enable",
      variant: job.enabled ? "danger" : "default",
      onClick: () => void handleToggleEnabled(job),
    });
    return items;
  }

  function renderScheduleCell(job: Job) {
    if (!job.managed) return <span className="sub">Unmanaged</span>;
    const lockedBy = job.schedule.locked ? job.schedule.locked_by : null;
    return (
      <span className="sub">
        {fmtSchedule(job.schedule)}
        {lockedBy && (
          <span className="job-lock" title={`${lockedBy} sets this job's interval.`}>
            <IconLock />
          </span>
        )}
      </span>
    );
  }

  function renderLastRunCell(job: Job, rollup: HostRollup | null) {
    if (!job.managed) return <span className="sub">{NONE}</span>;
    if (rollup) {
      if (!rollup.lastRun) return <span className="sub">Never</span>;
      return (
        <RunAgo
          finishedAt={rollup.lastRun.finishedAt}
          durationMs={rollup.lastRun.run.duration_ms}
          detail={`Most recent across ${plural(rollup.targetCount, "host")}, on ${rollup.lastRun.host}`}
        />
      );
    }
    if (!job.last_run) return <span className="sub">Never</span>;
    if (!job.last_run.finished_at) return <span className="sub">{NONE}</span>;
    return <RunAgo finishedAt={job.last_run.finished_at} durationMs={job.last_run.duration_ms} />;
  }

  function renderResultCell(job: Job, rollup: HostRollup | null) {
    if (!job.managed) return <span className="sub">{NONE}</span>;
    if (rollup) {
      return (
        <ResultCell
          running={Boolean(rollup.runningOn)}
          lastRun={rollup.lastRun?.run ?? null}
          detail={
            rollup.runningOn
              ? `Running on ${rollup.runningOn}`
              : rollup.lastRun
                ? `Most recent run, on ${rollup.lastRun.host}`
                : undefined
          }
        />
      );
    }
    return <ResultCell running={Boolean(job.running)} lastRun={job.last_run ?? null} />;
  }

  function renderNextRunCell(job: Job, rollup: HostRollup | null) {
    if (!job.managed || !job.enabled) return <span className="sub">{NONE}</span>;
    if (rollup) {
      if (!rollup.nextRun) return <span className="sub">{NONE}</span>;
      const soonest = `Soonest of ${plural(rollup.targetCount, "host")}, on ${rollup.nextRun.host}`;
      return (
        <span className="sub" title={soonest}>
          {relativeTimeFuture(rollup.nextRun.at)}
        </span>
      );
    }
    if (!job.next_run_at) return <span className="sub">{NONE}</span>;
    return <span className="sub">{relativeTimeFuture(job.next_run_at)}</span>;
  }

  /** One block per host, on the Hosts tab's `.exp-in` drawer idiom. */
  function renderExpansion(job: Job) {
    const targets = job.targets ?? [];
    if (targets.length === 0) {
      return <p className="sub exp-note">No hosts are running this job.</p>;
    }
    return (
      <div className="exp-in">
        {targets.map((target) => (
          <div key={target.host_id}>
            <div className="eyebrow">{hostLabel(target)}</div>
            <div className="exp-fact">
              <span>Last run</span>
              <span>
                {target.last_run?.finished_at ? (
                  <RunAgo
                    finishedAt={target.last_run.finished_at}
                    durationMs={target.last_run.duration_ms}
                  />
                ) : (
                  <span className="sub">{target.last_run ? NONE : "Never"}</span>
                )}
              </span>
            </div>
            <div className="exp-fact">
              <span>Result</span>
              <span>
                <ResultCell running={target.running} lastRun={target.last_run} />
              </span>
            </div>
            <div className="exp-fact">
              <span>Next run</span>
              <span className="sub">
                {job.enabled && target.next_run_at ? relativeTimeFuture(target.next_run_at) : NONE}
              </span>
            </div>
            <div className="exp-actions">
              <Button
                variant="ghost"
                size="sm"
                disabled={!job.enabled || runPending.has(runKey(job.id, target.host_id)) || target.running}
                onClick={() => void handleRunNow(job, target.host_id)}
              >
                {runPending.has(runKey(job.id, target.host_id)) ? "Running…" : "Run now"}
              </Button>
              <Button
                variant="ghost"
                size="sm"
                onClick={() =>
                  setHistoryTarget({ job, hostId: target.host_id, hostLabel: hostLabel(target) })
                }
              >
                View history
              </Button>
            </div>
          </div>
        ))}
      </div>
    );
  }

  const columns: JobColumn[] = [
    {
      key: "job",
      header: "Job",
      className: "job-cell",
      render: (j) => (
        <div className="stack job-ident">
          <span className="rowflex">
            <span className="primary">{j.name}</span>
            {!j.enabled && <Chip variant="neutral" className="chip-sm">Disabled</Chip>}
          </span>
          <span className="sub">{j.description}</span>
        </div>
      ),
    },
    { key: "schedule", header: "Schedule", render: renderScheduleCell },
    { key: "last_run", header: "Last run", render: renderLastRunCell },
    { key: "result", header: "Result", render: renderResultCell },
    { key: "next_run", header: "Next run", render: renderNextRunCell },
    {
      key: "actions",
      header: "",
      className: "cell-actions",
      render: (j) => <ActionsMenu items={actionsForJob(j)} label={`Actions for ${j.name}`} />,
    },
  ];

  /** Data columns plus the expand column, for every full-width row. */
  const spanAll = columns.length + 1;

  function groupRow(label: string, count: number) {
    return (
      <tr className="group-row">
        <td colSpan={spanAll}>
          <span className="eyebrow">{label}</span>
          <span className="num">{count}</span>
        </td>
      </tr>
    );
  }

  function renderJobRow(job: Job) {
    const expanded = expandedIds.has(job.id);
    const rollup = rollupFor(job);
    return (
      <Fragment key={job.id}>
        <tr style={job.enabled ? undefined : { opacity: 0.55 }}>
          <td className="qtable-expand-cell">
            {rollup && (
              <button
                type="button"
                className="exp-btn"
                aria-expanded={expanded}
                aria-label={`Show per-host breakdown for ${job.name}`}
                title={expanded ? "Hide per-host breakdown" : "Show per-host breakdown"}
                onClick={() => toggleExpand(job.id)}
              >
                <IconChevronRight />
              </button>
            )}
          </td>
          {columns.map((col) => (
            <td key={col.key} className={col.className}>
              {col.render(job, rollup)}
            </td>
          ))}
        </tr>
        {rollup && expanded && (
          <tr className="exp-row">
            <td colSpan={spanAll}>{renderExpansion(job)}</td>
          </tr>
        )}
      </Fragment>
    );
  }

  useSectionHead({
    sub: "Background work on the control plane and every host",
    actions: (
      <Button variant="ghost" onClick={() => void res.refresh()}>
        <IconRefresh />
        Refresh
      </Button>
    ),
    counts: { jobs: jobs.length },
  });

  return (
    <section className="page jobs-page">
      <div className="toolbar">
        <SegmentedControl<Segment>
          aria-label="Filter jobs"
          value={segment}
          onChange={setSegment}
          options={[
            { value: "all", label: "All" },
            {
              value: "failing",
              label: (
                <>
                  Failing <span className="num">{failingCount}</span>
                </>
              ),
              ariaLabel: `Failing, ${failingCount}`,
            },
            {
              value: "unmanaged",
              label: (
                <>
                  Unmanaged <span className="num">{unmanagedCount}</span>
                </>
              ),
              ariaLabel: `Unmanaged, ${unmanagedCount}`,
            },
          ]}
        />
        <div className="search">
          <IconSearch />
          <input
            aria-label="Filter jobs"
            placeholder="Filter jobs"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
          />
        </div>
      </div>

      <div className="card table-wrap">
        <ResourceStates loading={res.loading} error={res.errorMessage} />

        {!res.loading && jobs.length === 0 && (
          <div className="empty">
            <h3>No jobs registered</h3>
            <p>The control plane and each host register their background work at startup.</p>
          </div>
        )}

        {jobs.length > 0 && visible.length === 0 && (
          <div className="empty">
            <h3>No jobs match</h3>
            <p>No job matches this filter. Clear it to see them all.</p>
          </div>
        )}

        {visible.length > 0 && (
          <table className="qtable">
            <thead>
              <tr>
                <th className="qtable-expand-th" aria-hidden="true" />
                {columns.map((col) => (
                  <th key={col.key}>{col.header}</th>
                ))}
              </tr>
            </thead>
            <tbody>
              {controlPlaneJobs.length > 0 && groupRow("Control plane", controlPlaneJobs.length)}
              {controlPlaneJobs.map(renderJobRow)}
              {hostJobs.length > 0 && groupRow("Hosts", hostJobs.length)}
              {hostJobs.map(renderJobRow)}
              {unmanagedHostJobs.length > 0 &&
                groupRow("Hosts, unmanaged", unmanagedHostJobs.length)}
              {unmanagedHostJobs.map(renderJobRow)}
            </tbody>
          </table>
        )}
      </div>

      {scheduleTarget && (
        <ScheduleModal
          job={scheduleTarget}
          token={token}
          onClose={() => setScheduleTarget(null)}
          onSaved={(updated) => {
            // setData also discards any list GET already in flight, so it
            // cannot repaint the old schedule over this save.
            res.setData((prev) => prev.map((j) => (j.id === updated.id ? updated : j)));
            setScheduleTarget(null);
            addToast({ variant: "success", title: `${updated.name} schedule updated` });
          }}
        />
      )}

      {historyTarget && (
        <HistoryDrawer target={historyTarget} onClose={() => setHistoryTarget(null)} />
      )}
    </section>
  );
}

function plural(n: number, noun: string): string {
  return `${n} ${noun}${n === 1 ? "" : "s"}`;
}

/* ------------------------------------------------------------------ */
/* Schedule edit modal                                                  */
/* ------------------------------------------------------------------ */

interface ScheduleModalProps {
  job: Job;
  token: string | null;
  onClose: () => void;
  onSaved: (job: Job) => void;
}

function ScheduleModal({ job, token, onClose, onSaved }: ScheduleModalProps) {
  const s: JobSchedule = job.schedule;
  const [enabled, setEnabled] = useState(job.enabled);
  const [intervalInput, setIntervalInput] = useState(
    s.interval_secs != null ? String(s.interval_secs) : "",
  );
  const [hasWindow, setHasWindow] = useState(Boolean(s.window_start && s.window_end));
  const [windowStart, setWindowStart] = useState(s.window_start ? trimSeconds(s.window_start) : "");
  const [windowEnd, setWindowEnd] = useState(s.window_end ? trimSeconds(s.window_end) : "");
  const [windowDays, setWindowDays] = useState<number[]>(s.window_days);
  const [timezone, setTimezone] = useState(s.timezone);
  const [historyLimit, setHistoryLimit] = useState(String(job.history_limit));
  const [formError, setFormError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);

  const isInterval = s.kind === "interval";
  const intervalLocked = isInterval && s.locked;

  function toggleDay(d: number) {
    setWindowDays((prev) => (prev.includes(d) ? prev.filter((x) => x !== d) : [...prev, d].sort((a, b) => a - b)));
  }

  async function handleSave() {
    setFormError(null);

    const patch: JobPatchRequest = { enabled, timezone, window_days: windowDays };

    if (isInterval && !intervalLocked) {
      const n = Number(intervalInput);
      if (!Number.isInteger(n) || n < 60) {
        setFormError("Interval must be a whole number of at least 60 seconds.");
        return;
      }
      patch.interval_secs = n;
    }

    if (hasWindow) {
      if (!windowStart || !windowEnd) {
        setFormError(
          "A window needs both a start and an end time. Turn the window off if you do not want one.",
        );
        return;
      }
      if (windowStart === windowEnd) {
        setFormError("Window start and end must be different.");
        return;
      }
      patch.window_start = windowStart;
      patch.window_end = windowEnd;
    } else {
      patch.window_start = null;
      patch.window_end = null;
    }

    const hist = Number(historyLimit);
    if (!Number.isInteger(hist) || hist < 1 || hist > 500) {
      setFormError("History limit must be a whole number between 1 and 500.");
      return;
    }
    patch.history_limit = hist;

    if (!token) return;
    setSaving(true);
    try {
      const updated = await adminApi.patchJob(token, job.id, patch);
      onSaved(updated);
    } catch (e: unknown) {
      setFormError(
        e instanceof ApiError
          ? patchErrorMessage(e.code, e.message, s.locked_by)
          : "Could not save this schedule.",
      );
    } finally {
      setSaving(false);
    }
  }

  return (
    <Modal
      open
      onClose={onClose}
      title={`Edit schedule for ${job.name}`}
      maxWidth={520}
      footer={
        <>
          <Button variant="ghost" onClick={onClose} disabled={saving}>
            Cancel
          </Button>
          <Button variant="primary" onClick={() => void handleSave()} disabled={saving}>
            {saving ? "Saving…" : "Save"}
          </Button>
        </>
      }
    >
      <div className="col gap4">
        <Switch checked={enabled} onChange={setEnabled} label="Enabled" />

        {isInterval ? (
          <TextField
            label="Interval"
            aria-label="Interval"
            type="number"
            min={60}
            value={intervalInput}
            onChange={(e) => setIntervalInput(e.target.value)}
            disabled={intervalLocked}
            hint={
              intervalLocked
                ? `${s.locked_by} sets this interval. Unset it to edit the interval here.`
                : "Seconds. At least 60."
            }
          />
        ) : (
          <p className="hint">
            This job's schedule is {s.kind === "event" ? "event triggered" : "manual only"}. There is
            no interval to edit.
          </p>
        )}

        <Switch checked={hasWindow} onChange={setHasWindow} label="Restrict to a time window" />
        {hasWindow && (
          <>
            <div className="row gap4">
              <TextField
                label="Window start"
                aria-label="Window start"
                type="time"
                value={windowStart}
                onChange={(e) => setWindowStart(e.target.value)}
              />
              <TextField
                label="Window end"
                aria-label="Window end"
                type="time"
                value={windowEnd}
                onChange={(e) => setWindowEnd(e.target.value)}
              />
            </div>
            <div className="field">
              <span className="label">Days</span>
              <div className="row gap3 wrap">
                {DAY_ABBR.map((d, i) => (
                  <Checkbox key={d} checked={windowDays.includes(i)} onChange={() => toggleDay(i)} label={d} />
                ))}
              </div>
              <span className="field-hint">Leave empty for every day.</span>
            </div>
          </>
        )}

        <TextField
          label="Timezone"
          value={timezone}
          onChange={(e) => setTimezone(e.target.value)}
          hint="IANA name, for example Europe/London or UTC."
        />
        <TextField
          label="History limit"
          type="number"
          min={1}
          max={500}
          value={historyLimit}
          onChange={(e) => setHistoryLimit(e.target.value)}
          hint="Between 1 and 500."
        />

        {formError && (
          <p className="form-error" role="alert">
            {formError}
          </p>
        )}
      </div>
    </Modal>
  );
}

/* ------------------------------------------------------------------ */
/* Run-history drawer                                                   */
/* ------------------------------------------------------------------ */

interface HistoryDrawerProps {
  target: HistoryTarget;
  onClose: () => void;
}

function HistoryDrawer({ target, onClose }: HistoryDrawerProps) {
  const { job, hostId, hostLabel: host } = target;

  const runs = useResource<JobRun[]>(
    {
      label: "run history",
      initialData: [],
      fetch: async (ctx) =>
        (await adminApi.listJobRuns(ctx.token, job.id, { hostId, limit: job.history_limit })).items,
    },
    [job.id, hostId, job.history_limit],
  );
  const items = runs.data ?? [];

  /** Four columns, not six: the drawer is 640px, and a failed run's message
   *  needs more room than a column can give it. */
  function runRow(r: JobRun) {
    const { variant, label } = resultChip(r.state);
    const summary = summaryText(r.summary);
    return (
      <Fragment key={r.id}>
        <tr>
          <td>
            <div className="stack">
              <span className="sub" title={r.started_at ?? undefined}>
                {r.started_at ? relativeTime(r.started_at) : NONE}
              </span>
              <span className="sub mono">{triggerLabel(r.trigger)}</span>
            </div>
          </td>
          <td>
            <Chip variant={variant} title={r.error ?? r.finished_at ?? undefined}>
              {label}
            </Chip>
          </td>
          <td>
            <span className="sub mono">{fmtDurationMs(r.duration_ms)}</span>
          </td>
          <td>
            <span className="sub mono job-summary" title={summary}>
              {summary}
            </span>
          </td>
        </tr>
        {r.error && (
          <tr className="exp-row">
            <td colSpan={4}>
              <p className="form-error run-error">{r.error}</p>
            </td>
          </tr>
        )}
      </Fragment>
    );
  }

  return (
    <Drawer
      open
      onClose={onClose}
      title={`Run history for ${job.name}${host ? ` (${host})` : ""}`}
      width={640}
    >
      <div className="fsec">
        <div className="fs-label">
          <h4>Overview</h4>
          <p>How this job is scheduled, and whether it is on.</p>
        </div>
        <div className="fs-fields">
          <div className="ae-facts">
            <div className="ae-fact">
              <span>Schedule</span>
              <span>{job.managed ? fmtSchedule(job.schedule) : "Unmanaged"}</span>
            </div>
            <div className="ae-fact">
              <span>State</span>
              <span>
                <Chip variant={job.enabled ? "success" : "neutral"}>
                  {job.enabled ? "Enabled" : "Disabled"}
                </Chip>
              </span>
            </div>
            <div className="ae-fact">
              <span>Runs kept</span>
              <span>{job.history_limit}</span>
            </div>
          </div>
        </div>
      </div>

      {/* Stacked rather than label-beside-fields: `.fsec`'s 210px label column
          leaves under 360px for the table, which is less than four columns of
          content need. */}
      <div className="fsec fsec-stacked">
        <div className="fs-label">
          <h4>Runs</h4>
          <p>
            The last {job.history_limit} runs{host ? ` on ${host}` : ""}.
          </p>
        </div>
        <div className="fs-fields">
          <ResourceStates
            loading={runs.loading}
            error={runs.errorMessage}
            isEmpty={items.length === 0}
            empty="No runs recorded."
          />
          {items.length > 0 && (
            <div className="table-wrap">
              <table className="qtable">
                <thead>
                  <tr>
                    <th>Started</th>
                    <th>Result</th>
                    <th>Duration</th>
                    <th>Summary</th>
                  </tr>
                </thead>
                <tbody>{items.map(runRow)}</tbody>
              </table>
            </div>
          )}
        </div>
      </div>
    </Drawer>
  );
}
