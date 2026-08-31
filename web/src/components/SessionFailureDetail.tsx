// SessionFailureDetail — shared failure presentation (first-run §S5) for
// SessionLoader and admin/SessionDetail: the failure_code → headline map and
// the collapsible log tail, one copy so the two surfaces cannot drift.
// Callers keep their own domain-type adapters; this module knows neither.

import { useState } from "react";
import { Button } from "./Button";

/**
 * Known machine-readable `failure_code` values, mapped to an operator-language
 * headline. An unrecognized code (a newer control plane) returns `null` so
 * the caller's own generic fallback applies — this module never invents copy
 * for a code it doesn't recognize.
 */
const FAILURE_CODE_HEADLINE: Record<string, string> = {
  app_exited_early: "The app exited before producing any video",
  // #484 §3.3: the boot watchdog's failure (QUASAR_APP_BOOT_TIMEOUT_SECS
  // expiry) — the app container ran, but never committed a presented frame.
  app_never_presented: "The game never started",
};

export function failurePresentation(code: string | null | undefined): string | null {
  if (!code) return null;
  return FAILURE_CODE_HEADLINE[code] ?? null;
}

/** Lines past which the log tail starts collapsed — the same policy in the
 *  one place, rather than two copies that could drift. */
export const LOG_COLLAPSE_LINE_THRESHOLD = 8;

export interface CollapsibleLogTailProps {
  text: string;
  /** Extra class(es) on the wrapping element for caller-specific layout
   *  (e.g. SessionLoader centers and width-constrains its terminal card;
   *  admin/SessionDetail's card needs no extra layout). The scrollable
   *  `<pre>` itself always gets the shared `.failure-log-tail-pre` box
   *  styling (components.css) so both surfaces render it identically. */
  className?: string;
  /** `data-testid` on the `<pre>` — both callers' existing tests look for
   *  their own id, so this stays a prop rather than a hardcoded value. */
  testId?: string;
}

export function CollapsibleLogTail({ text, className, testId = "failure-log-tail" }: CollapsibleLogTailProps) {
  const lines = text.split("\n");
  const long = lines.length > LOG_COLLAPSE_LINE_THRESHOLD;
  const [expanded, setExpanded] = useState(!long);

  return (
    <div className={className}>
      {expanded ? (
        <>
          <pre className="mono failure-log-tail-pre" data-testid={testId}>
            {text}
          </pre>
          {long && (
            <Button variant="ghost" size="sm" onClick={() => setExpanded(false)}>
              Hide app log
            </Button>
          )}
        </>
      ) : (
        <Button variant="ghost" size="sm" onClick={() => setExpanded(true)}>
          Show app log ({lines.length} lines)
        </Button>
      )}
    </div>
  );
}
