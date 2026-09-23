import type { Host } from "../../../api/types";

type Restriction = Host["admission_restrictions"][number];

const REASON_LABELS: Record<string, string> = {
  manual_drain: "Operator drain",
  legacy_drain: "Existing drain",
  platform_apply: "Platform apply",
  idle_configuration: "Configuration apply",
  configuration_recovery: "Configuration recovery",
  journal_reconciliation: "Agent journal reconciliation",
  journal_quarantine: "Agent journal review",
};

export function activeAdmissionRestrictions(host: Host): Restriction[] {
  // An older control plane may omit the additive field during an upgrade.
  return host.admission_restrictions ?? [];
}

export function hasOperatorDrain(host: Host): boolean {
  return activeAdmissionRestrictions(host).some(
    (hold) => hold.owner_kind === "manual" || hold.owner_kind === "legacy",
  );
}

export function admissionActionLabel(host: Host): string {
  if (hasOperatorDrain(host)) return "Release operator drain";
  if (host.status === "draining") return "Add operator drain";
  return "Drain";
}

export function canChangeOperatorDrain(host: Host): boolean {
  return hasOperatorDrain(host) || host.status === "online" || host.status === "draining";
}

function restrictionLabel(hold: Restriction): string {
  return REASON_LABELS[hold.reason] ?? "Admission hold";
}

function recordedAt(value: string): string {
  const time = Date.parse(value);
  if (Number.isNaN(time)) return "upgrade time unknown";
  return `${new Intl.DateTimeFormat("en", {
    dateStyle: "medium", timeStyle: "short", timeZone: "UTC",
  }).format(time)} UTC`;
}

export function AdmissionReasons({ host, className = "note warn" }: { host: Host; className?: string }) {
  const restrictions = activeAdmissionRestrictions(host);
  if (restrictions.length === 0 && host.status !== "draining") return null;

  return (
    <div className={className}>
      <b>Scheduling restrictions.</b> No new sessions are assigned while a hold remains.
      {restrictions.length > 0 && (
        <ul>
          {restrictions.map((hold, index) => (
            <li key={`${hold.owner_kind}:${hold.created_at}:${index}`}>
              {restrictionLabel(hold)}
              {hold.reason === "legacy_drain" && (
                <span className="sub">
                  {" · recorded during upgrade at "}
                  <time dateTime={hold.created_at}>{recordedAt(hold.created_at)}</time>
                  {"; original drain time unknown"}
                </span>
              )}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
