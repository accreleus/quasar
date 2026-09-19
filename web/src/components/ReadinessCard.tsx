// ReadinessCard — shared host-readiness display (first-run-experience §S1).
// No mockup covers a readiness surface; built from sibling idioms. Rendered in
// full by StepHosts, the Hosts tab's expanded row and the host detail page (as
// a full-width grid). Host settings deliberately does not repeat it.

import type { ReadinessCheck } from "../api/types";
import { groupChecks } from "../lib/readiness/groups";
import { Chip } from "./Chip";
import { CopyableCommand } from "./CopyableCommand";
import { IconCheck, IconClose, IconWarning } from "./icons";

// One glyph per status, in a circle: a tick, a cross, an exclamation, a dash.
// The status word travels as the accessible name so a screen reader and a
// tooltip still get "Pass"/"Fail"; an unrecognised status draws the neutral
// dash with its raw value as the name (the check set is agent-owned per the
// contract's ReadinessCheck doc comment).
const DASH = <span aria-hidden="true">–</span>;
const QUESTION = <span aria-hidden="true">?</span>;
const GLYPH: Record<string, { label: string; className: string; icon: React.ReactNode }> = {
  pass: { label: "Pass", className: "rdy-glyph rdy-ok", icon: <IconCheck /> },
  fail: { label: "Fail", className: "rdy-glyph rdy-bad", icon: <IconClose /> },
  warn: { label: "Warning", className: "rdy-glyph rdy-warn", icon: <IconWarning /> },
  skip: { label: "Skipped", className: "rdy-glyph rdy-off", icon: DASH },
  provisioning: { label: "Provisioning", className: "rdy-glyph rdy-off", icon: DASH },
  // Amendment 11: a host probe that ran but couldn't tell — distinct from
  // "not applicable" (skip) and never blocks (contract, ReadinessCheck.status).
  unknown: { label: "Indeterminate", className: "rdy-glyph rdy-off", icon: QUESTION },
};

function ReadinessGlyph({ status }: { status: string }) {
  const g = GLYPH[status] ?? { label: status, className: "rdy-glyph rdy-off", icon: DASH };
  return (
    <span className={g.className} role="img" aria-label={g.label} title={g.label}>
      {g.icon}
    </span>
  );
}

// Amendment 11, open string (contract: ReadinessCheck.source) — unrecognised
// values are shown raw rather than dropped.
const SOURCE_LABEL: Record<string, string> = {
  host_probe: "host probe",
  local: "local check",
  runtime: "container runtime",
  operator: "operator configuration",
};

function sourceLabel(source: string): string {
  return SOURCE_LABEL[source] ?? source;
}

// A malformed `observed_at` must never surface as "Invalid Date" — same idiom
// as the card header's `reportedAt`, but tolerant since this one is per-check.
function observedLabel(observedAt: string): string | null {
  const d = new Date(observedAt);
  return Number.isNaN(d.getTime()) ? null : `Observed ${d.toLocaleString()}`;
}

function provenanceText(c: ReadinessCheck): string | null {
  const parts = [c.observed_at && observedLabel(c.observed_at), c.source && sourceLabel(c.source)].filter(
    (p): p is string => Boolean(p),
  );
  return parts.length > 0 ? parts.join(" · ") : null;
}

type ReadinessBlocks = NonNullable<ReadinessCheck["blocks"]>;

// Unrecognised scope, or a `gpu` scope missing its index, never blocks
// (contract) — no marker rather than a misleading one.
function blocksTitle(b: ReadinessBlocks): string | null {
  let base: string;
  if (b.scope === "host") base = "Blocks every launch on this host";
  else if (b.scope === "homes") base = "Blocks launches that use a managed home";
  else if (b.scope === "gpu") {
    if (typeof b.gpu_index !== "number") return null;
    base = `Blocks launches placed on GPU ${b.gpu_index}`;
  } else {
    return null;
  }
  return b.enforced_by === "agent" ? `${base}. Enforced by the host agent; cannot be overridden.` : base;
}

export interface ReadinessCardProps {
  /** `Host.readiness`. `null` = never reported; `[]` = reported, nothing to check. */
  checks: ReadinessCheck[] | null;
  /** `Host.readiness_reported_at`. */
  reportedAt?: string | null;
  /** Small print under the card — e.g. what action re-runs the checks and what
   *  it does/doesn't cover (driver fixes vs a plain restart). */
  footnote?: React.ReactNode;
  /** Non-blocking notice shown above the checks (wizard usage: failures don't
   *  block Continue). */
  advisoryNote?: React.ReactNode;
  /** `list` (default) stacks the checks, for a rail or a wizard step. `grid`
   *  tiles them in an auto-fill grid for a full-width card, so a dozen checks
   *  take two rows on a wide screen instead of a column three screens tall. */
  layout?: "list" | "grid";
}

export function ReadinessCard({
  checks,
  reportedAt,
  footnote,
  advisoryNote,
  layout = "list",
}: ReadinessCardProps) {
  // `!= null` on purpose: a stale/pre-amendment fixture or agent may omit the
  // field entirely (undefined) rather than sending null, and both mean the
  // same thing here — nothing to render yet.
  const hasChecks = checks != null && checks.length > 0;
  const anyFail = hasChecks && checks.some((c) => c.status === "fail");
  // #102: grouped by the area an operator would fix; `skip` (not applicable
  // to this host, per the contract) leaves the first screen for a disclosure.
  const { groups, notApplicable } = groupChecks(checks ?? []);
  const rowClass = layout === "grid" ? "readiness-check" : "host-setting-row";
  const listClass = layout === "grid" ? "readiness-grid" : "col gap3";

  const renderCheck = (c: ReadinessCheck) => {
    const provenance = provenanceText(c);
    const blocksLabel = c.blocks && blocksTitle(c.blocks);
    return (
      <div key={c.id} className={rowClass} data-testid={`readiness-check-${c.id}`}>
        <div className="host-setting-copy">
          <div className="row gap2" style={{ alignItems: "center" }}>
            <ReadinessGlyph status={c.status} />
            <h3 style={{ fontSize: "var(--t-sm)" }}>{c.id === "nvidia_vulkan_av1_compatibility" ? "Vulkan AV1 compatibility" : c.id.replaceAll("_", " ")}</h3>
            {blocksLabel && (
              <span data-testid={`readiness-blocks-${c.id}`} title={blocksLabel}>
                <Chip variant={c.status === "fail" ? "danger" : "neutral"} className="chip-sm">
                  {c.status === "fail" ? "Blocks launches" : "Can block launches"}
                </Chip>
              </span>
            )}
          </div>
          <p>{c.summary}</p>
          {provenance && (
            // .host-setting-copy p out-specifies .muted, so the small-print colour is set here.
            <p style={{ fontSize: "var(--t-xs)", color: "var(--text-3)" }} data-testid={`readiness-provenance-${c.id}`}>
              {provenance}
            </p>
          )}
          {/* #483: `warn` is advisory-but-actionable (e.g. media_reachability) —
              it carries a real remediation command same as `fail`, just never
              blocks anything. Show it here too, or the whole point of a WARN
              check (here's exactly what to run) is invisible. */}
          {(c.status === "fail" || c.status === "warn") && c.remediation && (
            <CopyableCommand text={c.remediation} />
          )}
        </div>
      </div>
    );
  };

  return (
    <div className="card sec-card" data-testid="readiness-card">
      <div className="sec-head">
        <div>
          <h3>Readiness</h3>
          <div className="desc">
            {reportedAt
              ? `Last reported ${new Date(reportedAt).toLocaleString()}`
              : "Not reported yet."}
          </div>
        </div>
        {anyFail && (
          <Chip variant="danger" className="chip-sm">
            Needs attention
          </Chip>
        )}
      </div>

      {/* #254: host-local by definition — never a claim about browser reachability
       (CONTEXT.md "Host readiness vs browser reachability"). */}
      <p className="muted" style={{ fontSize: "var(--t-xs)", marginBottom: 0 }} data-testid="readiness-host-local-note">
        Readiness is what this host can establish about itself. It does not show whether a browser can reach the host; that depends on the network between them.
      </p>

      {advisoryNote}

      {!hasChecks && (
        <p className="muted" style={{ marginBottom: 0 }} data-testid="readiness-empty">
          {checks == null ? "This host has not reported readiness checks yet." : "No readiness checks reported."}
        </p>
      )}

      {hasChecks && (
        <div className="col gap4" data-testid="readiness-checks">
          {groups.map((g) => (
            <div key={g.key} className="readiness-group" data-testid="readiness-group" data-group={g.key}>
              <div className="eyebrow">{g.label}</div>
              <div className={listClass}>{g.checks.map(renderCheck)}</div>
            </div>
          ))}
          {groups.length === 0 && (
            <p className="muted" style={{ marginBottom: 0 }}>
              Nothing to check on this host beyond what is listed below.
            </p>
          )}
        </div>
      )}

      {notApplicable.length > 0 && (
        <details className="readiness-more" data-testid="readiness-not-applicable">
          <summary>
            {notApplicable.length} {notApplicable.length === 1 ? "check" : "checks"} not applicable to this host
          </summary>
          <div className={listClass} style={{ marginTop: "var(--s3)" }}>
            {notApplicable.map(renderCheck)}
          </div>
        </details>
      )}

      {footnote && (
        <p className="muted" style={{ fontSize: "var(--t-xs)", marginTop: "var(--s3)", marginBottom: 0 }}>
          {footnote}
        </p>
      )}
    </div>
  );
}
