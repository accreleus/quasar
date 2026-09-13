/**
 * The fleet half of the Releases page (#117): one button that moves the whole
 * instance, the live run panel, and the note that covers the control plane's
 * own restart.
 *
 * No v3 mock covers this tab (ReleasesTab.tsx says why), so it composes the
 * same card/table/chip primitives the rest of the page uses; the amendment-9
 * additions (the skip list in the confirmation, the partial banner, Retry)
 * likewise add no style of their own.
 */

import { useState, type ReactNode } from "react";
import * as adminApi from "../../../api/admin";
import type {
  PlatformApplyRun,
  PlatformApplyRunsResponse,
  PlatformReleaseTarget,
  PlatformReleaseView,
} from "../../../api/types";
import { useAuth } from "../../../auth/context";
import { Button } from "../../../components/Button";
import { Card } from "../../../components/Card";
import { Chip, type ChipVariant } from "../../../components/Chip";
import { Modal } from "../../../components/Modal";
import { Table, type TableColumn } from "../../../components/Table";
import { useAdminAction } from "../../../lib/resource/action";
import { useResource } from "../../../lib/resource/react";
import { AttemptProgress } from "./ApplyControls";
import { blockingChecks, partialSummary, willBeSkipped } from "./preflight";
import { eligibilityText, hasUpdate, preflightCheckText, releaseLabel, runStateText } from "./releasesCopy";

function eligibleHosts(targets: PlatformReleaseTarget[]): PlatformReleaseTarget[] {
  return targets.filter((t) => t.kind === "host" && t.eligible);
}

function hostCount(n: number): string {
  return `${n} host${n === 1 ? "" : "s"}`;
}

/** The control plane's ineligibility reason, or null when a fleet run may
 *  start. Server twin: apply_fleet_handler.go's refusal. */
function controlPlaneBlocker(targets: PlatformReleaseTarget[]): string | null {
  const cp = targets.find((t) => t.kind === "control_plane");
  if (!cp || cp.eligible || !cp.reason || cp.reason === "up_to_date") return null;
  return cp.reason;
}

/** "Update Quasar" — the whole instance, control plane first. Absent while a
 *  run is active: the run panel is then the only control. */
export function FleetApplyButton({
  view,
  onStarted,
  children,
}: {
  view: PlatformReleaseView;
  onStarted: () => void;
  /** The label, so the head can put its icon in front of it. */
  children?: ReactNode;
}) {
  const [confirming, setConfirming] = useState(false);
  const newest = view.available[0];

  if (!newest || !hasUpdate(view) || view.active_apply?.run != null) return null;

  // Nothing moves before the control plane, so a run it cannot take is refused
  // outright (409 release_not_offered). `up_to_date` is the one reason that is
  // not a refusal: the run then goes straight to the hosts.
  const blocked = controlPlaneBlocker(view.targets);
  const cp = view.targets.find((t) => t.kind === "control_plane");
  const blockingCheck = cp && blocked === "preflight_blocked" ? blockingChecks(cp)[0] : undefined;
  const title = blockingCheck
    ? `${preflightCheckText(blockingCheck.id)}: ${blockingCheck.detail}`
    : blocked
      ? eligibilityText(blocked)
      : undefined;

  return (
    <>
      <Button onClick={() => setConfirming(true)} disabled={blocked != null} title={title}>
        {children ?? "Update Quasar"}
      </Button>
      {confirming && (
        <FleetApplyModal
          view={view}
          onClose={() => setConfirming(false)}
          onStarted={onStarted}
        />
      )}
    </>
  );
}

/** Force is the operator agreeing to end every live session on N hosts, so the
 *  confirmation names N (control-api.md §"Platform-release apply"). */
function FleetApplyModal({
  view,
  onClose,
  onStarted,
}: {
  view: PlatformReleaseView;
  onClose: () => void;
  onStarted: () => void;
}) {
  const { token } = useAuth();
  const [force, setForce] = useState(false);
  const newest = view.available[0];
  const hosts = eligibleHosts(view.targets).length;
  // Consent names the partial outcome up front: the hosts this run will pass
  // over, and why (amendment 9).
  const skipped = willBeSkipped(view.targets);
  // Consent has to name what actually happens, so the SERVER decides this and
  // serves it (#153): only a migrating release ends the instance's sessions
  // before the control-plane step, and that policy must not be re-derived here.
  // No release to apply reads as migrating — the cautious answer.
  const migrates = newest?.migrates ?? true;

  const apply = useAdminAction(
    async () =>
      adminApi.applyPlatformReleaseToFleet(token ?? "", { release_id: newest.id, force }),
    {
      success: `Updating this instance to ${releaseLabel(newest)}.`,
      failure: (e) => ({
        title: "Could not start the update.",
        body: e instanceof Error ? e.message : undefined,
      }),
      onSuccess: () => {
        onClose();
        onStarted();
      },
    },
  );

  return (
    <Modal
      open
      onClose={onClose}
      title="Update Quasar"
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button onClick={() => void apply.run()} disabled={apply.pending != null}>
            Update
          </Button>
        </>
      }
    >
      <p>
        Update the control plane, then {hosts} eligible host{hosts === 1 ? "" : "s"}, to{" "}
        <b>{releaseLabel(newest)}</b>.
      </p>
      {migrates ? (
        <p>
          The control plane updates first and restarts; this page will lose contact for about 20
          seconds. This release changes the database, so the update waits for every session on the
          instance to end before it starts.
        </p>
      ) : (
        <p>
          The control plane updates first and restarts; this page will lose contact for about 20
          seconds. Live sessions keep streaming through it — each host's own sessions end when
          that host is updated.
        </p>
      )}
      {skipped.length > 0 && (
        <div className="note" data-testid="fleet-will-skip">
          <p>
            Will be skipped and stay on the old release ({skipped.length}):
          </p>
          <ul className="release-faults">
            {skipped.map((t) => {
              const blocker = t.reason === "preflight_blocked" ? blockingChecks(t)[0] : undefined;
              return (
                <li key={t.host_id}>
                  <b>{t.node_name}</b> {blocker ? blocker.detail : eligibilityText(t.reason ?? null)}
                </li>
              );
            })}
          </ul>
        </div>
      )}
      <label className="rowflex">
        <input type="checkbox" checked={force} onChange={(e) => setForce(e.target.checked)} />
        <span>Update now — ends every live session on {hostCount(hosts)}</span>
      </label>
      <p className="hint">
        {migrates
          ? "Without this, the update waits for every session on the instance to end on its own, and then for each host's own sessions in turn."
          : "Without this, the update waits for each host's own sessions to end on their own before it updates that host."}
      </p>
    </Modal>
  );
}

const RUN_STATE_CHIP: Record<string, ChipVariant> = {
  pending: "info",
  running: "info",
  succeeded: "success",
  succeeded_partial: "warning",
  failed: "danger",
  cancelled: "neutral",
};

/** The target the run is on now, named. A host the run reached has an attempt
 *  carrying its node_name; before one exists the id is all there is. */
function currentTargetName(run: PlatformApplyRun): string | null {
  if (run.current_target === "control_plane") return "Control plane";
  if (run.current_target !== "host") return null;
  const named = run.attempts.find((a) => a.host_id === run.current_host_id)?.node_name;
  return named ?? (run.current_host_id ?? "").slice(0, 8);
}

/**
 * Cancel stops the run before its next target and never interrupts an
 * in-flight attempt, so once the last target has started there is nothing left
 * for it to stop. `targets` is what makes "last" knowable; without it only the
 * flag disables the button.
 */
function nothingLeftToStop(run: PlatformApplyRun, targets: PlatformReleaseTarget[] | undefined): boolean {
  if (!targets || run.current_target !== "host") return false;
  return eligibleHosts(targets).every(
    (t) =>
      run.attempts.some((a) => a.host_id === t.host_id) ||
      run.skipped.some((s) => s.host_id === t.host_id),
  );
}

export function FleetRunPanel({
  run,
  targets,
  onChanged,
  retriedBy,
}: {
  run: PlatformApplyRun;
  /** The release view's targets, for the cancel gate. */
  targets?: PlatformReleaseTarget[];
  onChanged: () => void;
  /** A later run that carries this run's id as retry_of, when the caller knows one. */
  retriedBy?: PlatformApplyRun;
}) {
  const { token } = useAuth();
  const active = run.state === "pending" || run.state === "running";
  const blocked = run.cancel_requested || nothingLeftToStop(run, targets);
  const current = currentTargetName(run);
  const partial = run.state === "succeeded_partial";
  // Retry is a plain fleet apply of the same release carrying `retry_of`: the
  // updated targets read up_to_date and are skipped, so only the hosts left
  // behind move. Offered on a partial run only, never on a failed one.
  const retry = useAdminAction(
    async () =>
      adminApi.applyPlatformReleaseToFleet(token ?? "", {
        release_id: run.release_id,
        force: false,
        retry_of: run.id,
      }),
    {
      success: "Retrying the hosts that were skipped.",
      failure: (e) => ({
        title: "Could not start the retry.",
        body: e instanceof Error ? e.message : undefined,
      }),
      onSuccess: onChanged,
    },
  );

  const cancel = useAdminAction(
    async () => adminApi.cancelPlatformApplyRun(token ?? "", run.id),
    {
      success: "The update will stop before its next target.",
      failure: (e) => ({
        title: "Could not cancel the update.",
        body: e instanceof Error ? e.message : undefined,
      }),
      onSuccess: onChanged,
    },
  );

  const columns: TableColumn<PlatformApplyRun["attempts"][number]>[] = [
    {
      key: "target",
      header: "Target",
      render: (a) => (a.target === "control_plane" ? "Control plane" : (a.node_name ?? "gone")),
    },
    {
      key: "state",
      header: "State",
      // When the control-plane step waits at all it waits for the WHOLE fleet,
      // not one host's sessions. Since #153 that is only a release carrying a
      // migration; otherwise the step never enters this state.
      render: (a) =>
        a.target === "control_plane" && a.state === "waiting_sessions" ? (
          <span>
            <Chip variant="info">Waiting for sessions to end</Chip>{" "}
            {a.sessions_remaining != null && (
              <span className="muted">
                waiting on {a.sessions_remaining} session
                {a.sessions_remaining === 1 ? "" : "s"} across the fleet
              </span>
            )}
          </span>
        ) : (
          <AttemptProgress attempt={a} />
        ),
    },
  ];

  return (
    <>
      <div className="rowflex" style={{ alignItems: "center" }}>
        <Chip variant={RUN_STATE_CHIP[run.state] ?? "neutral"}>{run.state}</Chip>
        <span>{runStateText(run.state)}</span>
        {/* #122: an admin finding a fleet run they did not start is owed the
            explanation, and `requested_by` cannot give it — that is null both
            for an unattended run and for one whose requesting admin was later
            deleted. `control-api.md` says a client SHOULD say so. Reuses the
            existing Chip; the v3 handoff has no mock for this marker, so nothing
            new is styled. */}
        {run.unattended && (
          <Chip variant="neutral" title="Started automatically by release detection, not by an admin">
            automatic
          </Chip>
        )}
        {run.retry_of && (
          <Chip variant="neutral" title={`Started to finish run ${run.retry_of}`}>
            retry
          </Chip>
        )}
        {retriedBy && (
          <Chip variant="neutral" title={`Retried by run ${retriedBy.id} (${retriedBy.state})`}>
            retried
          </Chip>
        )}
        {current && <span className="muted">Now: {current}</span>}
        {partial && !retriedBy && (
          <Button variant="ghost" disabled={retry.pending != null} onClick={() => void retry.run()}>
            Retry skipped hosts
          </Button>
        )}
        {active && (
          <Button
            variant="ghost"
            disabled={blocked || cancel.pending != null}
            title={
              blocked && !run.cancel_requested
                ? "The last target is already updating; a cancel cannot interrupt it."
                : undefined
            }
            onClick={() => void cancel.run()}
          >
            Cancel
          </Button>
        )}
      </div>
      {blocked && !run.cancel_requested && (
        <p className="hint">
          The last target is already updating; a cancel cannot interrupt it.
        </p>
      )}
      {run.error && (
        <p className="form-error" role="alert">
          {run.error}
        </p>
      )}
      {partial && (
        <p className="note" role="status" data-testid="fleet-partial">
          {partialSummary(run)}
        </p>
      )}
      <Table
        columns={columns}
        rows={run.attempts}
        rowKey={(a) => a.id}
        empty="No target has been reached yet."
      />
      {run.skipped.length > 0 && (
        <>
          <p className="panel-title">Not updated</p>
          <ul className="release-faults">
            {run.skipped.map((s) => (
              <li key={s.host_id}>
                <b>{s.node_name}</b> {eligibilityText(s.reason)}
              </li>
            ))}
          </ul>
        </>
      )}
    </>
  );
}

/** The window in which the API is gone: the control plane is applying itself,
 *  and the process that would answer this page is the one being replaced. */
export function ControlPlaneRestarting() {
  return (
    <p className="note" role="status">
      The control plane is restarting on the new release. This page will reconnect on its own.
    </p>
  );
}

/**
 * The most recent finished run, when it needs attention: `active_apply` only
 * carries a run while it is pending or running, so without this a run that
 * ended partial or failed vanished from the page the moment it finished, and
 * the retry it asks for had nowhere to live. A clean success is not shown; the
 * update banner already says the instance is current.
 */
export function LastRunPanel({
  targets,
  onChanged,
}: {
  targets: PlatformReleaseTarget[];
  onChanged: () => void;
}) {
  const res = useResource<PlatformApplyRunsResponse>({
    label: "fleet runs",
    fetch: ({ token, signal }) => adminApi.listPlatformApplyRuns(token, { limit: 10 }, signal),
  });
  const runs = res.data?.runs ?? [];
  const last = runs[0];
  if (!last || (last.state !== "succeeded_partial" && last.state !== "failed")) return null;
  // Newest first, so a later retry is earlier in the list than what it retries.
  const retriedBy = runs.find((r) => r.retry_of === last.id);
  return (
    <Card className="card-pad mb4">
      <div className="eyebrow">Last fleet update</div>
      <div className="mt3">
        <FleetRunPanel
          run={last}
          targets={targets}
          retriedBy={retriedBy}
          onChanged={() => {
            void res.refresh();
            onChanged();
          }}
        />
      </div>
    </Card>
  );
}
