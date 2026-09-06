import type { SteamPreparationStatus as Status } from "../../../api/types";
import { Chip } from "../../../components/Chip";

const STATES: Record<string, string> = {
  waiting_image: "Waiting for image",
  queued: "Queued",
  preparing: "Preparing",
  ready: "Prepared",
  deferred: "Deferred",
  failed: "Failed",
  disabled: "Disabled",
  unsupported: "Not supported",
  unknown: "Not reported",
  pending_policy: "Applying setting",
};
const REASONS: Record<string, string> = {
  none: "",
  source_disabled: "Steam preparation is switched off for this deployment.",
  host_warmup_disabled: "This host has opted out of background preparation. It may still use existing prepared homes.",
  host_templates_disabled: "This host has opted out of using prepared homes. It may still prepare templates.",
  host_permissions_disabled: "This host has opted out of both preparation and use of prepared homes.",
  host_setting_invalid: "A host preparation setting is invalid. Correct its advanced configuration and recreate the agent.",
  image_not_ready: "Waiting for the adopted Steam image to finish installing.",
  host_busy: "Deferred while this host serves user sessions. Preparation resumes when capacity is available.",
  storage_unavailable: "Preparation storage is unavailable. Check the template and home mounts, permissions and free space.",
  stale_policy: "Waiting for work using the previous setting to stop before applying the new setting.",
  preparation_failed: "Preparation failed. See this host’s template warm-up job for the error; ordinary Steam launches remain available.",
  unsupported_image: "This image is not eligible for automatic Steam preparation.",
  agent_upgrade_required: "Upgrade this node agent to apply and report Steam preparation policy.",
  policy_pending: "Waiting for this host to apply the current setting.",
  host_offline: "This host is offline; the saved setting has not been confirmed.",
};

/** Image download readiness is deliberately not an input: only the preparation
 * report can establish that a sanitized template was published. */
export function SteamPreparationStatus({ status, desiredEnabled }: { status?: Status | null; desiredEnabled?: boolean }) {
  if (!status) return <p className="hint">Preparation has not been reported. Upgrade older agents to apply and report the Steam setting.</p>;
  const desired = desiredEnabled ?? status.desired_enabled;
  const pending = status.policy_pending || desired !== status.desired_enabled;
  const state = !status.eligible ? "unsupported" : !status.supported ? "unknown" : pending ? "pending_policy" : status.state;
  const ready = state === "ready" && status.template !== null;
  return (
    <div className="col gap2" data-testid="steam-preparation-status">
      <span><Chip variant={ready ? "success" : state === "failed" ? "danger" : "neutral"}>
        {state === "ready" && !ready ? "Not reported" : STATES[state] ?? state}
      </Chip></span>
      <p className="hint" style={{ margin: 0 }}>
        Steam setting: {desired ? "on" : "off"}.
        {status.supported && !pending && status.preparation_enabled !== null && status.consumption_enabled !== null
          ? ` This host: preparation ${status.preparation_enabled ? "on" : "off"}; prepared homes ${status.consumption_enabled ? "on" : "off"}.`
          : " Effective host policy is not confirmed."}
      </p>
      {status.reason && status.reason !== "none" && <p className="hint" style={{ margin: 0 }}>{REASONS[status.reason] ?? status.reason}</p>}
      {status.detail && !pending && <p className="hint" style={{ margin: 0, overflowWrap: "anywhere" }}>{status.detail}</p>}
      {ready && <p className="hint" style={{ margin: 0 }}>Prepared version: {status.template!.version}</p>}
      {pending && status.reported_at && <p className="hint" style={{ margin: 0 }}>Last observed {new Date(status.reported_at).toLocaleString()}; this report does not confirm the current setting.</p>}
      {status.clone_mode && (
        <p className="hint" style={{ margin: 0 }}>
          {pending ? "Last observed home cloning" : "Home cloning"}: {status.clone_mode === "reflink" ? "reflink" : status.clone_mode === "copy" ? "full copy" : status.clone_mode}.
          {status.clone_reason ? ` ${status.clone_reason}` : ""}
        </p>
      )}
    </div>
  );
}
