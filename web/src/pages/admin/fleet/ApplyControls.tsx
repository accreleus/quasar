/**
 * The apply half of the Releases page (#116): the per-host Apply button, its
 * confirmation, live attempt state in the targets table, and the history list.
 *
 * No v3 mock covers this tab (ReleasesTab.tsx says why), so it composes the
 * same card/table/chip primitives the read half already uses.
 */

import { useState } from "react";
import * as adminApi from "../../../api/admin";
import { ACTIVE_SESSION_STATES } from "../../../api/sessionStates";
import type {
  AdminSessionsResponse,
  PlatformApplyAttempt,
  PlatformApplyAttemptsResponse,
  PlatformRelease,
  PlatformReleaseTarget,
} from "../../../api/types";
import { useAuth } from "../../../auth/context";
import { Button } from "../../../components/Button";
import { Chip } from "../../../components/Chip";
import { Modal } from "../../../components/Modal";
import { ResourceStates } from "../../../components/ResourceStates";
import { relativeTime } from "../../../lib/format/relativeTime";
import { useAdminAction } from "../../../lib/resource/action";
import { useResource } from "../../../lib/resource/react";
import { attemptStateText, failureText, releaseLabel, shortDigest } from "./releasesCopy";

/** The open attempt for one target — null host_id is the control plane. */
export function attemptForTarget(
  attempts: PlatformApplyAttempt[] | undefined,
  target: PlatformReleaseTarget,
): PlatformApplyAttempt | undefined {
  const hostId = target.kind === "host" ? target.host_id : null;
  return attempts?.find((a) => (a.host_id ?? null) === (hostId ?? null));
}

export function AttemptProgress({ attempt }: { attempt: PlatformApplyAttempt }) {
  const waiting = attempt.state === "waiting_sessions";
  return (
    <span>
      <Chip variant={attempt.state === "failed" ? "danger" : "info"}>
        {attemptStateText(attempt.state)}
      </Chip>{" "}
      {waiting && attempt.sessions_remaining != null && (
        <span className="muted">
          waiting on {attempt.sessions_remaining} session
          {attempt.sessions_remaining === 1 ? "" : "s"}
        </span>
      )}
      {attempt.state === "failed" && attempt.reason && (
        <span className="muted"> {failureText(attempt.reason)}</span>
      )}
    </span>
  );
}

interface ConfirmProps {
  release: PlatformRelease;
  target: PlatformReleaseTarget;
  /** Live sessions on this host, or null when the count could not be read. */
  liveSessions: number | null;
  onClose: () => void;
  onApplied: () => void;
}

/** Force is the operator agreeing to end N live sessions, so the confirmation
 *  names N — a confirmation that does not is not informed consent
 *  (control-api.md §"Platform-release apply"). */
export function ApplyConfirmModal({
  release,
  target,
  liveSessions,
  onClose,
  onApplied,
}: ConfirmProps) {
  const { token } = useAuth();
  const [force, setForce] = useState(false);
  const hostId = target.host_id ?? "";
  const name = target.node_name ?? "this host";

  const apply = useAdminAction(
    async () =>
      adminApi.applyPlatformReleaseToHost(token ?? "", hostId, {
        release_id: release.id,
        force,
      }),
    {
      success: `Updating ${name} to ${releaseLabel(release)}.`,
      failure: (e) => ({
        title: `Could not update ${name}.`,
        body: e instanceof Error ? e.message : undefined,
      }),
      onSuccess: () => {
        onClose();
        onApplied();
      },
    },
  );

  const forceLabel =
    liveSessions == null
      ? "Update now — ends every live session on this host"
      : `Update now — ends ${liveSessions} live session${liveSessions === 1 ? "" : "s"}`;

  return (
    <Modal
      open
      onClose={onClose}
      title={`Update ${name}`}
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
        Move <b>{name}</b> to <b>{releaseLabel(release)}</b>. The host stops taking new sessions
        while it updates, and returns to service afterwards.
      </p>
      <label className="rowflex">
        <input type="checkbox" checked={force} onChange={(e) => setForce(e.target.checked)} />
        <span>{forceLabel}</span>
      </label>
      <p className="hint">
        Without this, the update waits for the sessions running here to end on their own.
      </p>
    </Modal>
  );
}

/** The live session count on one host, from the admin session list. Null when
 *  the list could not be read: the confirmation then says "every live session"
 *  rather than inventing a number. */
export function useHostSessionCounts(): Map<string, number> | null {
  const res = useResource<AdminSessionsResponse>({
    label: "sessions",
    fetch: ({ token }) => adminApi.listAllSessions(token, undefined, { limit: 200 }),
  });
  if (!res.data) return null;
  const counts = new Map<string, number>();
  for (const s of res.data.items ?? []) {
    if (!s.host_id || !ACTIVE_SESSION_STATES.has(s.state)) continue;
    counts.set(s.host_id, (counts.get(s.host_id) ?? 0) + 1);
  }
  return counts;
}

function when(iso: string | null | undefined): string {
  return iso ? relativeTime(iso) : "—";
}

/** Apply history: what this instance has done to itself, newest first. Drawn as
 *  the rail's compact list (design_handoff_v3/screens/releases-v3.html), not a
 *  table: it sits in a 300px column. */
export function ApplyHistory({ refreshKey }: { refreshKey: number }) {
  const res = useResource<PlatformApplyAttemptsResponse>(
    {
      label: "apply history",
      fetch: ({ token, signal }) => adminApi.listPlatformAttempts(token, { limit: 50 }, signal),
    },
    [refreshKey],
  );

  return (
    <>
      <ResourceStates loading={res.loading} error={res.errorMessage} />
      {res.data &&
        (res.data.attempts.length === 0 ? (
          <p className="muted">Nothing has been applied on this instance yet.</p>
        ) : (
          res.data.attempts.map((a) => <ApplyHistoryRow key={a.id} attempt={a} />)
        ))}
    </>
  );
}

function ApplyHistoryRow({ attempt: a }: { attempt: PlatformApplyAttempt }) {
  const from = a.previous_digests.map((p) => shortDigest(p.digest)).join(", ") || "unknown";
  const to = a.requested_digests.map((c) => shortDigest(c.digest)).join(", ");
  return (
    <div className="rel-fact stack">
      <div className="rowflex" style={{ justifyContent: "space-between", width: "100%" }}>
        <span>{a.target === "control_plane" ? "Control plane" : (a.node_name ?? "gone")}</span>
        <span className="hint">{when(a.created_at)}</span>
      </div>
      <span className="mono hint">
        {from} → {to}
      </span>
      <span className="hint">
        <span>{a.kind === "revert" ? "Revert" : "Apply"}</span> · {attemptStateText(a.state)}
        {a.reason && ` · ${failureText(a.reason)}`}
      </span>
    </div>
  );
}
