// One audit-log row: the summary <tr> plus its hidden detail <tr>
// (handoff-v3-spec §A.20). Everything that turns the row's ids into words —
// the sentence, the key=value summary, the annotated readout — lives in
// describe.ts; this file is the markup.

import { useEffect, useRef, useState } from "react";
import type { AdminActivityItem, AdminActivitySeverity } from "../../../api/admin";
import { Button } from "../../../components/Button";
import type { ChipVariant } from "../../../components/Chip";
import { Chip } from "../../../components/Chip";
import { IconCheck, IconChevronRight, IconCopy } from "../../../components/icons";
import { actorLabel } from "./auditFilters";
import { detailReadout, summaryLine, targetLabel } from "./describe";

const SEVERITY_VARIANT: Record<AdminActivitySeverity, ChipVariant> = {
  err: "danger",
  warn: "warning",
  info: "neutral",
};

/** Local 24-hour HH:MM:SS — `hour12:false` so a PM row never grows an AM/PM
 *  suffix past the 88px Time column, and so the string is deterministic
 *  enough to assert on directly in a test (locale still supplies separators,
 *  but this repo's test/build locale is en-US throughout). Deliberately not
 *  `lib/format.ts`'s `fmtTime` (locale-default hour cycle, includes AM/PM). */
function auditTime(iso: string): string {
  return new Date(iso).toLocaleTimeString(undefined, {
    hour12: false,
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  });
}

/** `copyAudit`'s clipboard text: "{time}  {actor}  {action}  {target}\n{pre}". */
export function copyText(item: AdminActivityItem): string {
  return `${auditTime(item.created_at)}  ${actorLabel(item)}  ${item.action}  ${targetLabel(item)}\n${detailReadout(item)}`;
}

interface AuditRowProps {
  item: AdminActivityItem;
  expanded: boolean;
  onToggle: () => void;
}

export function AuditRow({ item, expanded, onToggle }: AuditRowProps) {
  const [copied, setCopied] = useState(false);
  const timeoutRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  useEffect(() => () => {
    if (timeoutRef.current) clearTimeout(timeoutRef.current);
  }, []);

  async function handleCopy(e: React.MouseEvent) {
    e.stopPropagation();
    // No clipboard (an insecure origin has none) is the same outcome as a
    // rejected write: nothing was copied, so the button must not say it was.
    if (!navigator.clipboard?.writeText) return;
    try {
      await navigator.clipboard.writeText(copyText(item));
    } catch {
      return;
    }
    setCopied(true);
    if (timeoutRef.current) clearTimeout(timeoutRef.current);
    timeoutRef.current = setTimeout(() => setCopied(false), 1200);
  }

  function handleToggleClick(e: React.MouseEvent) {
    // The caret is its own button now (a11y: real keyboard focus + Enter/
    // Space activation) — stop the click reaching the row's own onClick, or
    // a caret press would toggle twice (once here, once via bubbling).
    e.stopPropagation();
    onToggle();
  }

  const variant = SEVERITY_VARIANT[item.severity];
  const initial = item.actor_username ? item.actor_username[0].toUpperCase() : "S";

  return (
    <>
      <tr className="clickable" onClick={onToggle}>
        <td className="aud-caret-cell">
          <button
            type="button"
            className={`aud-caret${expanded ? " open" : ""}`}
            aria-expanded={expanded}
            aria-label={expanded ? "Collapse entry" : "Expand entry"}
            onClick={handleToggleClick}
          >
            <IconChevronRight />
          </button>
        </td>
        <td className="num mono">{auditTime(item.created_at)}</td>
        <td>
          <div className="rowflex">
            <span className={`u-ava aud-ava${item.actor_username ? "" : " aud-ava-system"}`}>
              {initial}
            </span>
            <span className="primary">{actorLabel(item)}</span>
          </div>
        </td>
        <td>
          <Chip variant={variant} className="aud-action">
            {item.action}
          </Chip>
        </td>
        <td className="primary">{targetLabel(item)}</td>
        <td className="aud-summary mono">{summaryLine(item)}</td>
        <td onClick={(e) => e.stopPropagation()}>
          <div className="cell-actions">
            <button type="button" className="icon-btn" title="Copy entry" onClick={handleCopy}>
              {copied ? <IconCheck /> : <IconCopy />}
            </button>
          </div>
        </td>
      </tr>
      {expanded && (
        <tr className="aud-detail">
          <td />
          <td colSpan={6} className="aud-cell">
            <div className="aud-det">
              <div className="rowflex aud-det-head">
                <span className="eyebrow">Detail</span>
                <Button
                  type="button"
                  variant="ghost"
                  size="sm"
                  style={{ marginLeft: "auto" }}
                  onClick={handleCopy}
                >
                  {copied ? "Copied" : (
                    <>
                      <IconCopy />
                      Copy
                    </>
                  )}
                </Button>
              </div>
              <pre className="aud-pre">{detailReadout(item)}</pre>
            </div>
          </td>
        </tr>
      )}
    </>
  );
}
