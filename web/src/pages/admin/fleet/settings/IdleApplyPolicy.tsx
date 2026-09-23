import { useState } from "react";
import * as adminApi from "../../../../api/admin";
import { ApiError } from "../../../../api/client";
import { Button } from "../../../../components/Button";
import { ResourceStates } from "../../../../components/ResourceStates";
import { useAdminAction } from "../../../../lib/resource/action";
import { useResource } from "../../../../lib/resource/react";
import { KnobPanel } from "./KnobPanel";

type IdleData = { policy: adminApi.HostPolicyView | null; attempt: adminApi.IdleApplyAttempt | null };
type Reviewed = {
  hostId: string;
  preview: adminApi.IdleApplyPreview;
  sources: Record<string, string>;
  signature: string;
};

function reviewSignature(preview: adminApi.IdleApplyPreview, policy: adminApi.HostPolicyView): string {
  const settings = Object.keys(preview.resolved).sort().map((key) =>
    [key, preview.resolved[key], policy.choices[key]?.source ?? "unknown"]);
  return JSON.stringify({
    boot: preview.approval_boot_incarnation, review: preview.approval_review_id,
    revision: preview.revision, content: preview.content_sha256,
    prerequisites: preview.prerequisites_sha256, facts: preview.prerequisites,
    settings,
  });
}

function snapshot(hostId: string, preview: adminApi.IdleApplyPreview, policy: adminApi.HostPolicyView): Reviewed {
  // Keep displayed values and the submitted request in one immutable snapshot.
  // A later poll can mark it stale but cannot silently replace it.
  const values = Object.fromEntries(Object.entries(preview.resolved).map(([key, value]) => [key, value]));
  const facts = preview.prerequisites.map((fact) => ({ ...fact }));
  const frozenPreview = { ...preview, resolved: values, prerequisites: facts };
  const sources = Object.fromEntries(Object.keys(values).map((key) => [key, policy.choices[key]?.source ?? "unknown"]));
  return { hostId, preview: frozenPreview, sources, signature: reviewSignature(frozenPreview, policy) };
}

function valueText(value: unknown): string {
  return typeof value === "string" ? value : JSON.stringify(value) ?? "undefined";
}

function reviewDetails(reviewed: Reviewed) {
  return <div style={{ gridColumn: "1 / -1" }}>
    <p className="hint">Review revision <code>{reviewed.preview.revision}</code>, content digest <code>{reviewed.preview.content_sha256}</code>.</p>
    <h4>Resolved settings and sources</h4>
    <ul>
      {Object.keys(reviewed.preview.resolved).sort().map((key) => <li key={key}>
        <code>{key}</code>: <code>{valueText(reviewed.preview.resolved[key])}</code> (<span>{reviewed.sources[key]}</span>)
      </li>)}
    </ul>
    <h4>Prerequisites</h4>
    <p className="hint">Facts digest: <code>{reviewed.preview.prerequisites_sha256}</code></p>
    <ul>
      {reviewed.preview.prerequisites.map((fact) => <li key={`${fact.kind}:${fact.id}`}>
        <code>{fact.kind}</code>: <code style={{ overflowWrap: "anywhere" }}>{fact.id}</code>
      </li>)}
    </ul>
  </div>;
}

/** Operator approval and observation for the saved restart group. */
export function IdleApplyPolicy({ hostId }: { hostId: string | undefined }) {
  const resource = useResource<IdleData>({
    label: "idle apply status",
    pollMs: 5000,
    fetch: async ({ token }) => {
      if (!hostId) return { policy: null, attempt: null };
      let policy: adminApi.HostPolicyView;
      try { policy = await adminApi.getHostPolicy(token, hostId); }
      catch (error) {
        if (error instanceof ApiError && error.status === 404) return { policy: null, attempt: null };
        throw error;
      }
      const id = policy.groups.hardware?.attempt_id;
      const attempt = id ? await adminApi.getHostIdleApply(token, hostId, id) : null;
      return { policy, attempt };
    },
  }, [hostId]);
  const [selected, setSelected] = useState<Reviewed | null>(null);
  const [reviewRejected, setReviewRejected] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);
  const policy = resource.data?.policy;
  const group = policy?.groups.hardware;
  const preview = group?.approval_preview as adminApi.IdleApplyPreview | null | undefined;
  const current = hostId && policy && preview?.available ? snapshot(hostId, preview, policy) : null;
  const reviewed = selected?.hostId === hostId ? selected : null;
  const stale = Boolean(reviewed && (!current || reviewed.signature !== current.signature || reviewRejected));
  const displayed = reviewed ?? current;
  const attempt = resource.data?.attempt;
  const waiting = attempt?.phase === "waiting" || attempt?.phase === "offered" || attempt?.phase === "cancel_pending";

  const approve = useAdminAction<[Reviewed], adminApi.IdleApplyAttempt>(
    (selection) => resource.mutate(
      ({ token }) => adminApi.approveHostIdleApply(token, selection.hostId, "hardware", selection.preview,
        new Date(Date.now() + 60 * 60 * 1000).toISOString()),
      (data, result) => ({ ...data, attempt: result }),
    ),
    {
      failure: (error) => error instanceof ApiError && error.code === "approval_superseded"
        ? "Host facts changed. Review the refreshed configuration before approving again."
        : error instanceof ApiError ? error.message : "Could not approve idle apply.",
      onSuccess: () => { setActionError(null); setReviewRejected(false); },
      onFailure: (error) => {
        setActionError(error instanceof ApiError && error.code === "approval_superseded"
          ? "This review was superseded. Refresh and review the current configuration."
          : error instanceof ApiError ? error.message : "Could not approve idle apply.");
        if (error instanceof ApiError && error.code === "approval_superseded") {
          setReviewRejected(true);
          void resource.refresh();
        }
      },
    },
  );
  const cancel = useAdminAction<[string, string], adminApi.IdleApplyAttempt>(
    (id, owner) => resource.mutate(
      ({ token }) => adminApi.cancelHostIdleApply(token, owner, id),
      (data, result) => ({ ...data, attempt: result }),
    ),
    {
      failure: "Could not cancel idle apply.",
      onSuccess: () => setActionError(null),
      onFailure: (error) => setActionError(error instanceof ApiError ? error.message : "Could not cancel idle apply."),
    },
  );
  if (!hostId || (resource.data && (!group || group.scope !== "restart"))) return null;
  const busy = approve.pending !== null || cancel.pending !== null;

  return <KnobPanel title="Idle apply" hint="Approve a reviewed host configuration for an idle window. Saving settings alone does not apply them.">
    <div className="cset">
      <ResourceStates loading={resource.loading} error={resource.errorMessage} loadingLabel="Loading idle apply status…" />
      {group && <>
        <div>
          <h3>Restart configuration</h3>
          <p className="hint">Saved configuration: {group.status.replaceAll("_", " ")}. {attempt ? `Approval: ${attempt.phase.replaceAll("_", " ")}.` : "No approval is active."}</p>
          {attempt && <p className="hint">Execution {attempt.started ? "started" : "has not started"}; admission {attempt.admission_restricted ? "protected" : "open"}.</p>}
          {attempt?.phase === "recovered" && <p className="form-error" role="alert">The requested configuration failed. The agent restored its last verified configuration; review the failure before retrying.</p>}
          {attempt?.phase === "uncertain" && <p className="form-error" role="alert">Recovery could not be verified. Admission remains protected until an operator repairs this host.</p>}
          {attempt?.remedy && <p className="hint">{attempt.remedy}</p>}
          {preview?.remedy && !waiting && <p className="hint">{preview.remedy}</p>}
          {!preview && group.remedy && !waiting && <p className="hint">{group.remedy}</p>}
          {stale && <p className="form-error" role="alert">The reviewed configuration changed. Review the current values before approving.</p>}
          {actionError && <p className="form-error" role="alert">{actionError}</p>}
          <p className="hint">The agent starts only after current idle and preparation checks pass. Approval never ends a running session.</p>
        </div>
        <div className="acts">
          <Button variant="ghost" disabled={busy} onClick={() => void resource.refresh()}>Refresh status</Button>
          {waiting ? <Button variant="ghost" disabled={busy || attempt?.phase === "cancel_pending"} onClick={() => { if (attempt) void cancel.run(attempt.attempt_id, hostId); }}>
            {attempt?.phase === "cancel_pending" ? "Cancellation pending" : "Cancel approval"}
          </Button> : <>
            {current && (!reviewed || stale) && <Button variant="ghost" disabled={busy} onClick={() => {
              setSelected(current); setReviewRejected(false); setActionError(null);
            }}>{reviewed ? "Review current configuration" : "Review this configuration"}</Button>}
            <Button variant="primary" disabled={busy || !reviewed || stale || !preview?.available || Boolean(resource.errorMessage)}
              onClick={() => { if (reviewed) void approve.run(reviewed); }}>Approve idle apply</Button>
          </>}
        </div>
        {displayed && reviewDetails(displayed)}
      </>}
    </div>
  </KnobPanel>;
}
