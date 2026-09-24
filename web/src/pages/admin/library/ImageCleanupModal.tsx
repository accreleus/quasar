import { useCallback, useEffect, useState } from "react";
import * as adminApi from "../../../api/admin";
import { ApiError } from "../../../api/client";
import type { HostImageCleanupCandidate, HostImageCleanupView } from "../../../api/types";
import { Button } from "../../../components/Button";
import { Modal } from "../../../components/Modal";

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

interface Props {
  token: string;
  hostID: string;
  hostName: string;
  imageID?: string;
  onClose: () => void;
}

export function ImageCleanupModal({ token, hostID, hostName, imageID, onClose }: Props) {
  const [view, setView] = useState<HostImageCleanupView | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [selected, setSelected] = useState<HostImageCleanupCandidate | null>(null);
  const [removing, setRemoving] = useState(false);
  const [result, setResult] = useState<string | null>(null);

  const refresh = useCallback(async (signal?: AbortSignal) => {
    setLoading(true);
    try {
      const next = await adminApi.getHostImageCleanup(token, hostID, signal);
      setView(next);
      setError(null);
    } catch (e) {
      if (signal?.aborted) return;
      setError(e instanceof ApiError ? e.message : "Could not load cached versions.");
    } finally {
      if (!signal?.aborted) setLoading(false);
    }
  }, [hostID, token]);

  useEffect(() => {
    const ctrl = new AbortController();
    void refresh(ctrl.signal);
    return () => ctrl.abort();
  }, [refresh]);

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
      setResult(attempt.state === "removed" ? "Already removed." : "Removal requested. The host will confirm the result.");
      setError(null);
      setSelected(null);
      await refresh();
    } catch (e) {
      setSelected(null);
      await refresh();
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
    {loading && !view && <p className="hint">Checking the host inventory…</p>}
    {error && <p className="note" role="alert">{error}</p>}
    {result && <p className="note" role="status">{result}</p>}
    {view && <>
      {current ? <p className="hint">Only versions verified on this current host connection are listed.</p> :
        <p className="note" role="status">Inventory {view.inventory_status}. {view.remedy ?? "Reconnect the host and wait for a complete inventory before cleanup."} An empty list does not mean the cache is empty.</p>}
      {current && candidates.length === 0 && <p className="hint">No verified cached version is available for this image on this host.</p>}
      {current && candidates.map((candidate) => <div className="ae-facts" key={`${candidate.image_id}:${candidate.version}:${candidate.runtime_image_id}`} style={{ marginTop: "var(--s4)" }}>
        <div className="ae-fact"><span>Image</span><span>{candidate.image_id}</span></div>
        <div className="ae-fact"><span>Version</span><span className="num">{candidate.version}</span></div>
        <div className="ae-fact"><span>Reference</span><span className="mono" style={{ overflowWrap: "anywhere" }}>{candidate.image_ref}</span></div>
        <div className="ae-fact"><span>Daemon image ID</span><span className="mono" style={{ overflowWrap: "anywhere" }}>{candidate.runtime_image_id}</span></div>
        {candidate.reasons.length > 0 && <div className="note">
          {candidate.reasons.map((reason) => <div key={reason}>{reasonCopy[reason]}</div>)}
          {candidate.remedy && <div>{candidate.remedy}</div>}
        </div>}
        {candidate.eligible && <Button variant="danger" size="sm" disabled={removing || loading}
          onClick={() => { setSelected(candidate); setResult(null); }}>
          Review removal
        </Button>}
      </div>)}
      {selected && <p className="note" role="status">
        Confirm removal of <strong>{selected.image_id} {selected.version}</strong> from <strong>{hostName}</strong>.
        The server will recheck references and the host inventory before deletion.
      </p>}
      <Button variant="ghost" size="sm" disabled={loading || removing} onClick={() => void refresh()}>Refresh inventory</Button>
    </>}
  </Modal>;
}
