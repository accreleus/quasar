import { useEffect, useState } from "react";
import * as adminApi from "../../../../api/admin";
import { ApiError } from "../../../../api/client";
import { useAuth } from "../../../../auth/context";
import { Button } from "../../../../components/Button";
import { KnobPanel } from "./KnobPanel";

/** Operator approval and observation for the saved restart group. */
export function IdleApplyPolicy({ hostId }: { hostId: string | undefined }) {
  const { token } = useAuth();
  const [view, setView] = useState<adminApi.HostPolicyView | null>(null);
  const [attempt, setAttempt] = useState<adminApi.IdleApplyAttempt | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (!token || !hostId) return;
    let cancelled = false;
    const load = async () => {
      try {
        const policy = await adminApi.getHostPolicy(token, hostId);
        if (cancelled) return;
        setView(policy);
        const id = policy.groups.hardware?.attempt_id;
        if (id) {
          const current = await adminApi.getHostIdleApply(token, hostId, id);
          if (!cancelled) setAttempt(current);
        } else setAttempt(null);
      } catch (e) {
        if (!cancelled) setError(e instanceof ApiError ? e.message : "Could not load idle apply status.");
      }
    };
    void load();
    const poll = window.setInterval(() => { if (!cancelled) void load(); }, 5000);
    return () => { cancelled = true; window.clearInterval(poll); };
  }, [token, hostId]);

  const group = view?.groups.hardware;
  if (!group || group.scope !== "restart") return null;
  const preview = group.approval_preview as adminApi.IdleApplyPreview | null;
  const waiting = attempt?.phase === "waiting" || attempt?.phase === "offered" || attempt?.phase === "cancel_pending";

  const approve = async () => {
    if (!token || !hostId || !preview?.available) return;
    setBusy(true); setError(null);
    try {
      const expiresAt = new Date(Date.now() + 60 * 60 * 1000).toISOString();
      setAttempt(await adminApi.approveHostIdleApply(token, hostId, "hardware", preview, expiresAt));
    } catch (e) {
      setError(e instanceof ApiError && e.code === "approval_superseded"
        ? "Host facts changed. Review the refreshed settings before approving again."
        : e instanceof ApiError ? e.message : "Could not approve idle apply.");
    } finally { setBusy(false); }
  };
  const cancel = async () => {
    if (!token || !hostId || !attempt) return;
    setBusy(true); setError(null);
    try { setAttempt(await adminApi.cancelHostIdleApply(token, hostId, attempt.attempt_id)); }
    catch (e) { setError(e instanceof ApiError ? e.message : "Could not cancel idle apply."); }
    finally { setBusy(false); }
  };

  return <KnobPanel title="Idle apply" hint="Approve a reviewed host configuration for an idle window. Saving settings alone does not apply them.">
    <div className="cset">
      <div>
        <h3>Restart configuration</h3>
        <p className="hint">Saved configuration: {group.status.replaceAll("_", " ")}. {attempt ? `Approval: ${attempt.phase.replaceAll("_", " ")}.` : "No approval is active."}</p>
        {attempt?.remedy && <p className="hint">{attempt.remedy}</p>}
        {preview?.remedy && !waiting && <p className="hint">{preview.remedy}</p>}
        {!preview && group.remedy && !waiting && <p className="hint">{group.remedy}</p>}
        <p className="hint">Execution is unavailable until the recovery executor is installed. No waiting approval restarts the agent or ends a session.</p>
        {error && <p role="alert" className="form-error">{error}</p>}
      </div>
      <div>
        {waiting ? <Button variant="ghost" disabled={busy || attempt?.phase === "cancel_pending"} onClick={() => void cancel()}>
          {attempt?.phase === "cancel_pending" ? "Cancellation pending" : busy ? "Cancelling…" : "Cancel approval"}
        </Button> : <Button variant="primary" disabled={busy || !preview?.available} onClick={() => void approve()}>
          {busy ? "Approving…" : "Approve idle wait"}
        </Button>}
      </div>
    </div>
  </KnobPanel>;
}
