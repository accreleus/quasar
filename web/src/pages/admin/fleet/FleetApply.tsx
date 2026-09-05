/**
 * The fleet half of the Releases page (#117): one button that moves the whole
 * instance, the live run panel, and the note that covers the control plane's
 * own restart.
 *
 * No v3 mock covers this tab (ReleasesTab.tsx says why), so it composes the
 * same card/table/chip primitives the rest of the page uses.
 */

import { useState, type ReactNode } from "react";
import * as adminApi from "../../../api/admin";
import type {
  PlatformApplyRun,
  PlatformReleaseTarget,
  PlatformReleaseView,
} from "../../../api/types";
import { useAuth } from "../../../auth/context";
import { Button } from "../../../components/Button";
import { Chip, type ChipVariant } from "../../../components/Chip";
import { Modal } from "../../../components/Modal";
import { Table, type TableColumn } from "../../../components/Table";
import { useAdminAction } from "../../../lib/resource/action";
import { AttemptProgress } from "./ApplyControls";
import { eligibilityText, hasUpdate, releaseLabel, runStateText } from "./releasesCopy";

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

  return (
    <>
      <Button
        onClick={() => setConfirming(true)}
        disabled={blocked != null}
        title={blocked ? eligibilityText(blocked) : undefined}
      >
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
      <p>
        The control plane updates first and restarts; this page will lose contact for about 20
        seconds. That restart ends every session on the instance, so the update waits for the
        whole fleet to be empty before it starts.
      </p>
      <label className="rowflex">
        <input type="checkbox" checked={force} onChange={(e) => setForce(e.target.checked)} />
        <span>Update now — ends every live session on {hostCount(hosts)}</span>
      </label>
      <p className="hint">
        Without this, the update waits for every session on the instance to end on its own, and
        then for each host's own sessions in turn.
      </p>
    </Modal>
  );
}

const RUN_STATE_CHIP: Record<string, ChipVariant> = {
  pending: "info",
  running: "info",
  succeeded: "success",
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
}: {
  run: PlatformApplyRun;
  /** The release view's targets, for the cancel gate. */
  targets?: PlatformReleaseTarget[];
  onChanged: () => void;
}) {
  const { token } = useAuth();
  const active = run.state === "pending" || run.state === "running";
  const blocked = run.cancel_requested || nothingLeftToStop(run, targets);
  const current = currentTargetName(run);

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
      // The control-plane step waits for the WHOLE fleet: its recreate drops
      // every agent connection, and an agent stops its sessions when that drops.
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
        {current && <span className="muted">Now: {current}</span>}
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
