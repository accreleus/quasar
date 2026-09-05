/**
 * Revert, and what a failed attempt shows (#118).
 *
 * A revert is an apply with an older digest set, so there is no version picker:
 * the only thing offerable is the digest set this host was demonstrably on,
 * which is the `previous_digests` of its last succeeded attempt. The control
 * plane is never revertible (ADR 0002) and so never appears here.
 *
 * `PlatformReleaseTarget` carries no `can_revert`, and adding one would be a
 * contract change; the signal is derived from the attempt history instead.
 */

import { useState } from "react";
import * as adminApi from "../../../api/admin";
import type {
  PlatformApplyAttempt,
  PlatformApplyAttemptsResponse,
  PlatformRelease,
} from "../../../api/types";
import { useAuth } from "../../../auth/context";
import { Button } from "../../../components/Button";
import { CopyableCommand } from "../../../components/CopyableCommand";
import { Modal } from "../../../components/Modal";
import { registryCommands } from "../../../lib/platform/manualUpdate";
import { useAdminAction } from "../../../lib/resource/action";
import { useResource } from "../../../lib/resource/react";
import { failureText, releaseLabel, shortDigest } from "./releasesCopy";

const AGENT = "node-agent";

/** What a host can go back to, and what went wrong last. */
export interface RevertState {
  /** The digest a revert would restore, with the repository it belongs to. */
  digest: string;
  image: string | null;
  /** The failed attempt to explain, when the host's newest attempt failed. */
  failed: PlatformApplyAttempt | null;
}

function agentPrevious(a: PlatformApplyAttempt): { digest: string; image: string | null } | null {
  const prev = a.previous_digests.find((p) => p.name === AGENT);
  if (!prev?.digest) return null;
  const image = a.requested_digests.find((c) => c.name === AGENT)?.image ?? null;
  return { digest: prev.digest, image };
}

/**
 * Per-host revert state from the attempt history. Server twin:
 * internal/platform.PlanRevert — the endpoint decides, and refuses on the same
 * facts; this only decides whether to draw the button.
 */
export function revertStates(attempts: PlatformApplyAttempt[]): Map<string, RevertState> {
  const out = new Map<string, RevertState>();
  const newest = new Map<string, PlatformApplyAttempt>();
  // Newest first (the contract's order), so the first row per host wins.
  for (const a of attempts) {
    if (a.target !== "host" || !a.host_id) continue;
    if (!newest.has(a.host_id)) newest.set(a.host_id, a);
    if (a.state !== "succeeded" || out.has(a.host_id)) continue;
    const prev = agentPrevious(a);
    if (prev) out.set(a.host_id, { ...prev, failed: null });
  }
  for (const [hostId, a] of newest) {
    if (a.state !== "failed") continue;
    const state = out.get(hostId);
    if (state) state.failed = a;
    // A failed attempt is not evidence of what the host is on, so it never
    // fills `digest`; the failure still has to be readable without a revert.
    else out.set(hostId, { digest: "", image: null, failed: a });
  }
  return out;
}

/** The history read the targets area needs. Separate from the history table's:
 *  this one is a derived signal, that one is a list an operator scrolls. */
export function useRevertStates(refreshKey: number): Map<string, RevertState> {
  const res = useResource<PlatformApplyAttemptsResponse>(
    {
      label: "apply history",
      fetch: ({ token, signal }) => adminApi.listPlatformAttempts(token, { limit: 200 }, signal),
    },
    [refreshKey],
  );
  return revertStates(res.data?.attempts ?? []);
}

/** The release a digest belongs to, when this page is still showing it. */
export function releaseForDigest(
  available: PlatformRelease[],
  digest: string,
): PlatformRelease | undefined {
  return available.find((r) => {
    const components = (r.manifest as { components?: { digest?: unknown }[] } | null)?.components;
    return Array.isArray(components) && components.some((c) => c?.digest === digest);
  });
}

interface ConfirmProps {
  hostId: string;
  nodeName: string;
  state: RevertState;
  release: PlatformRelease | undefined;
  /** Live sessions on this host, or null when the count could not be read. */
  liveSessions: number | null;
  onClose: () => void;
  onReverted: () => void;
}

/** The confirmation names the digest being restored, because that — not a
 *  version — is what a revert applies. */
export function RevertConfirmModal({
  hostId,
  nodeName,
  state,
  release,
  liveSessions,
  onClose,
  onReverted,
}: ConfirmProps) {
  const { token } = useAuth();
  const [force, setForce] = useState(false);

  const revert = useAdminAction(
    async () => adminApi.revertPlatformHost(token ?? "", hostId, { force }),
    {
      success: `Putting ${nodeName} back on ${shortDigest(state.digest)}.`,
      failure: (e) => ({
        title: `Could not revert ${nodeName}.`,
        body: e instanceof Error ? e.message : undefined,
      }),
      onSuccess: () => {
        onClose();
        onReverted();
      },
    },
  );

  const forceLabel =
    liveSessions == null
      ? "Revert now — ends every live session on this host"
      : `Revert now — ends ${liveSessions} live session${liveSessions === 1 ? "" : "s"}`;

  return (
    <Modal
      open
      onClose={onClose}
      title={`Revert ${nodeName}`}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button onClick={() => void revert.run()} disabled={revert.pending != null}>
            Revert
          </Button>
        </>
      }
    >
      <p>
        Put <b>{nodeName}</b> back on <span className="mono">{shortDigest(state.digest)}</span>
        {release ? (
          <>
            {" "}
            (<b>{releaseLabel(release)}</b>)
          </>
        ) : (
          <> — the build it was running before its last update</>
        )}
        . The agent carries no migrations, so only this host moves.
      </p>
      <label className="rowflex">
        <input type="checkbox" checked={force} onChange={(e) => setForce(e.target.checked)} />
        <span>{forceLabel}</span>
      </label>
      <p className="hint">
        Without this, the revert waits for the sessions running here to end on their own.
      </p>
    </Modal>
  );
}

/** A failed attempt, in full: why, what the updater printed, what the host was
 *  on before it, and the commands to put it back by hand. */
export function FailedAttemptPanel({
  attempt,
  state,
  onRevert,
}: {
  attempt: PlatformApplyAttempt;
  state: RevertState;
  onRevert?: () => void;
}) {
  const previous = attempt.previous_digests.filter((p) => p.digest);
  // The recipe restores what THIS attempt replaced — the digests it recorded —
  // which is what an operator needs when the stack is half-moved.
  const restore = agentPrevious(attempt);
  const recipe = restore ? registryCommands(null, restore) : [];

  return (
    <div className="note" data-testid={`failed-${attempt.host_id}`}>
      <div className="rowflex">
        <b>{attempt.node_name ?? "Host"}</b>
        <span className="muted">
          {attempt.kind === "revert" ? "Revert failed" : "Update failed"}
          {attempt.reason && <> — {failureText(attempt.reason)}</>}
        </span>
        {onRevert && state.digest && (
          <Button variant="ghost" onClick={onRevert}>
            Revert
          </Button>
        )}
      </div>
      <p className="hint">
        The host is still running whatever it had; nothing was rolled back for it automatically.
      </p>
      {previous.length > 0 && (
        <p className="muted">
          Previously{" "}
          {previous.map((p) => (
            <span key={p.name} className="mono">
              {p.name} {shortDigest(p.digest)}
            </span>
          ))}
        </p>
      )}
      {attempt.output && (
        <pre className="mono" data-testid={`failed-output-${attempt.host_id}`}>
          {attempt.output}
        </pre>
      )}
      {recipe.map((c) => (
        <CopyableCommand key={c.label} label={c.label} text={c.command} />
      ))}
    </div>
  );
}
