import { useEffect, useState } from "react";
import * as adminApi from "../../../api/admin";
import { ApiError } from "../../../api/client";
import type { HostImageCleanupAttempt, HostImageCleanupCandidate, HostImageCleanupView } from "../../../api/types";
import { Button } from "../../../components/Button";
import { Modal } from "../../../components/Modal";
import { ResourceStates } from "../../../components/ResourceStates";
import { useResource } from "../../../lib/resource/react";

const reasonCopy: Record<HostImageCleanupCandidate["reasons"][number], string> = {
  required: "Required by an app or host",
  container_reference: "Used by a container, including a stopped container",
  pending_launch: "A launch is pending",
  pending_image_operation: "An image operation is running",
  pending_template_work: "A template build is pending",
  retained_previous_success: "Retained as the previous working version",
  unknown_inventory: "The host inventory is incomplete",
  offline: "The host is offline",
  removing: "Removal is already in progress",
};

const failureCopy: Record<string, string> = {
  identity_mismatch: "The verified image identity changed. Refresh the inventory before retrying.",
  inventory_unknown: "The host inventory is incomplete. Reconnect the host and refresh before retrying.",
  reference_in_use: "A container reference still uses this image. Stop or remove that reference before retrying.",
  operation_busy: "Another image operation is active. Wait for it to finish before retrying.",
  unsupported: "This host does not support exact-version cleanup. Update its agent before retrying.",
  image_still_present: "The image remains in the daemon. Refresh its inventory before retrying.",
};

interface Props {
  token: string;
  hostID: string;
  hostName: string;
  imageID?: string;
  onClose: () => void;
}

export function ImageCleanupModal({ token, hostID, hostName, imageID, onClose }: Props) {
  const inventory = useResource<HostImageCleanupView>({
    label: "cached versions",
    fetch: (ctx) => adminApi.getHostImageCleanup(ctx.token, hostID, ctx.signal),
  }, [hostID]);
  const view = inventory.data;
  const loading = inventory.loading;
  const [error, setError] = useState<string | null>(null);
  const [selected, setSelected] = useState<HostImageCleanupCandidate | null>(null);
  const [removing, setRemoving] = useState(false);
  const [requested, setRequested] = useState<HostImageCleanupAttempt | null>(null);
  const attemptStatus = useResource<HostImageCleanupAttempt | null>({
    label: "cleanup status",
    fetch: (ctx) => requested
      ? adminApi.getHostImageCleanupAttempt(ctx.token, hostID, requested.attempt_id, ctx.signal)
      : Promise.resolve(null),
    // A numeric cadence retries even when the first status read fails before
    // the resource has data. Terminal outcomes pause it below.
    pollMs: 3000,
  }, [hostID, requested?.attempt_id]);
  const attempt = attemptStatus.errorMessage ? null : attemptStatus.data ?? requested;
  const attemptUnavailable = attemptStatus.error instanceof ApiError && attemptStatus.error.status === 404;
  useEffect(() => {
    if (!requested || attemptUnavailable || attempt?.state === "removed" || attempt?.state === "failed") {
      attemptStatus.pause();
    } else {
      attemptStatus.resume();
    }
  }, [requested, attemptUnavailable, attempt?.state, attemptStatus.pause, attemptStatus.resume]);
  useEffect(() => {
    if (attempt?.state === "removed" || attempt?.state === "failed") {
      void inventory.refresh({ silent: true });
    }
  }, [attempt?.attempt_id, attempt?.state, inventory.refresh]);
  const pendingState = attempt ?? requested;
  const attemptPending = !!pendingState && (pendingState.state === "removing" || pendingState.state === "unknown");

  async function remove() {
    if (!selected) return;
    setRemoving(true);
    try {
      const attempt = await adminApi.requestHostImageCleanup(token, hostID, {
        image_id: selected.image_id,
        version: selected.version,
        image_ref: selected.image_ref,
        runtime_image_id: selected.runtime_image_id,
        expected_generation: selected.generation,
      });
      setRequested(attempt);
      setError(null);
      setSelected(null);
      await inventory.refresh({ silent: true });
    } catch (e) {
      setSelected(null);
      await inventory.refresh({ silent: true });
      setError(e instanceof ApiError && e.status === 409
        ? "The host or image changed. Review the refreshed protection reasons before trying again."
        : e instanceof ApiError ? e.message : "Could not request removal.");
    } finally {
      setRemoving(false);
    }
  }

  const candidates = view?.images.filter((candidate) => !imageID || candidate.image_id === imageID) ?? [];
  const current = view?.inventory_status === "current";
  return <Modal open onClose={onClose} title={`Cached images on ${hostName}`} maxWidth={680}
    footer={selected ? <>
      <Button variant="ghost" disabled={removing} onClick={() => setSelected(null)}>Back</Button>
      <Button variant="danger" disabled={removing || !current} onClick={() => void remove()}>
        {removing ? "Requesting…" : "Remove this cached version"}
      </Button>
    </> : <Button variant="secondary" onClick={onClose}>Close</Button>}>
    <ResourceStates loading={loading} error={inventory.errorMessage}
      loadingLabel="Checking the host inventory…" />
    {error && <p className="note" role="alert">{error}</p>}
    {requested && (attemptUnavailable ?
      <p className="note" role="alert">The prior cleanup outcome is unavailable. Refresh the inventory; absence from the preview does not prove removal.</p> :
      attemptStatus.errorMessage ?
        <p className="note" role="alert">Could not check the cleanup outcome. It remains unconfirmed. Check again after reconnecting the host.</p> :
        <p className="note" role="status">{attempt?.state === "removed"
          ? "Removal confirmed by the host inventory."
          : attempt?.state === "failed"
            ? `Removal failed. ${failureCopy[attempt.reason ?? ""] ?? "Refresh the inventory and inspect the current blocker before retrying."}`
            : attempt?.state === "unknown"
              ? "The removal outcome is uncertain. Reconnect the host and restore its cleanup journal, then check again."
              : "Removal requested. Waiting for the host to confirm the outcome."}</p>)}
    {requested && (attemptPending || attemptStatus.errorMessage) &&
      <Button variant="ghost" size="sm" onClick={() => void attemptStatus.refresh()}>
        Check removal status
      </Button>}
    {view && <>
      {current ? <p className="hint">Only versions verified on this current host connection are listed.</p> :
        <p className="note" role="status">Inventory {view.inventory_status}. {view.remedy ?? "Reconnect the host and wait for a complete inventory before cleanup."} An empty list does not mean the cache is empty.</p>}
      {current && candidates.length === 0 && <p className="hint">No verified cached version is available for this image on this host.</p>}
      {current && candidates.map((candidate) => <div className="ae-facts mt4" key={`${candidate.image_id}:${candidate.version}:${candidate.runtime_image_id}`}>
        <div className="ae-fact"><span>Image</span><span>{candidate.image_id}</span></div>
        <div className="ae-fact"><span>Version</span><span className="num">{candidate.version}</span></div>
        <div className="ae-fact"><span>Reference</span><span className="mono lib-wrap-any">{candidate.image_ref}</span></div>
        <div className="ae-fact"><span>Daemon image ID</span><span className="mono lib-wrap-any">{candidate.runtime_image_id}</span></div>
        {candidate.reasons.length > 0 && <div className="note">
          {candidate.reasons.map((reason) => <div key={reason}>{reasonCopy[reason]}</div>)}
          {candidate.remedy && <div>{candidate.remedy}</div>}
        </div>}
        {candidate.eligible && <Button variant="danger" size="sm" disabled={removing || loading || attemptPending}
          onClick={() => { setSelected(candidate); }}>
          Review removal
        </Button>}
      </div>)}
      {selected && <p className="note" role="status">
        Confirm removal of <strong>{selected.image_id} {selected.version}</strong> from <strong>{hostName}</strong>.
        The server will recheck references and the host inventory before deletion.
      </p>}
    </>}
    <Button variant="ghost" size="sm" disabled={loading || removing} onClick={() => void inventory.refresh()}>
      Refresh inventory
    </Button>
  </Modal>;
}
