// ReadinessCard — shared host-readiness display (first-run-experience §S1).
// No mockup covers a readiness surface; built from sibling idioms. Rendered in
// full by StepHosts, the Hosts tab's expanded row and the host detail page (as
// a full-width grid). Host settings deliberately does not repeat it.

import { useState } from "react";
import type { ReadinessCheck } from "../api/types";
import { Button } from "./Button";
import { Chip } from "./Chip";
import { IconCheck, IconClose, IconWarning } from "./icons";

// One glyph per status, in a circle: a tick, a cross, an exclamation, a dash.
// The status word travels as the accessible name so a screen reader and a
// tooltip still get "Pass"/"Fail"; an unrecognised status draws the neutral
// dash with its raw value as the name (the check set is agent-owned per the
// contract's ReadinessCheck doc comment).
const DASH = <span aria-hidden="true">–</span>;
const GLYPH: Record<string, { label: string; className: string; icon: React.ReactNode }> = {
  pass: { label: "Pass", className: "rdy-glyph rdy-ok", icon: <IconCheck /> },
  fail: { label: "Fail", className: "rdy-glyph rdy-bad", icon: <IconClose /> },
  warn: { label: "Warning", className: "rdy-glyph rdy-warn", icon: <IconWarning /> },
  skip: { label: "Skipped", className: "rdy-glyph rdy-off", icon: DASH },
};

function ReadinessGlyph({ status }: { status: string }) {
  const g = GLYPH[status] ?? { label: status, className: "rdy-glyph rdy-off", icon: DASH };
  return (
    <span className={g.className} role="img" aria-label={g.label} title={g.label}>
      {g.icon}
    </span>
  );
}

function CopyableRemediation({ text }: { text: string }) {
  const [copied, setCopied] = useState(false);

  const copy = async () => {
    // Guard availability explicitly rather than optional-chain into
    // `writeText` — `clipboard?.writeText(...)` on an absent API awaits
    // `undefined`, which resolves (not rejects), so the old code marked
    // "Copied" even though nothing was written. Only a real, successful
    // write may flip the button.
    if (!navigator.clipboard) return;
    try {
      await navigator.clipboard.writeText(text);
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1500);
    } catch {
      // Best-effort — the text is still visible/selectable even if the
      // write itself failed (e.g. a permission denial).
    }
  };

  return (
    <div className="row gap2" style={{ marginTop: 4, alignItems: "center" }}>
      <code
        className="mono"
        style={{ flex: 1, overflowWrap: "anywhere", fontSize: "var(--t-xs)" }}
      >
        {text}
      </code>
      <Button type="button" variant="ghost" size="sm" onClick={() => void copy()}>
        {copied ? "Copied" : "Copy"}
      </Button>
    </div>
  );
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

      {advisoryNote}

      {!hasChecks && (
        <p className="muted" style={{ marginBottom: 0 }} data-testid="readiness-empty">
          {checks == null ? "This host has not reported readiness checks yet." : "No readiness checks reported."}
        </p>
      )}

      {hasChecks && (
        <div
          className={layout === "grid" ? "readiness-grid" : "col gap3"}
          data-testid="readiness-checks"
        >
          {checks.map((c) => (
            <div
              key={c.id}
              className={layout === "grid" ? "readiness-check" : "host-setting-row"}
              data-testid={`readiness-check-${c.id}`}
            >
              <div className="host-setting-copy">
                <div className="row gap2" style={{ alignItems: "center" }}>
                  <ReadinessGlyph status={c.status} />
                  <h3 style={{ fontSize: "var(--t-sm)" }}>{c.id.replaceAll("_", " ")}</h3>
                </div>
                <p>{c.summary}</p>
                {/* #483: `warn` is advisory-but-actionable (e.g. media_reachability) —
                    it carries a real remediation command same as `fail`, just never
                    blocks anything. Show it here too, or the whole point of a WARN
                    check (here's exactly what to run) is invisible. */}
                {(c.status === "fail" || c.status === "warn") && c.remediation && (
                  <CopyableRemediation text={c.remediation} />
                )}
              </div>
            </div>
          ))}
        </div>
      )}

      {footnote && (
        <p className="muted" style={{ fontSize: "var(--t-xs)", marginTop: "var(--s3)", marginBottom: 0 }}>
          {footnote}
        </p>
      )}
    </div>
  );
}
