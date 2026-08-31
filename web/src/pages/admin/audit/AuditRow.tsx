// One audit-log row: the summary <tr> plus its hidden detail <tr>
// (handoff-v3-spec §A.20). Action strings are server-authored and dotted
// (`app.update`); known ones map to sentences, the rest are humanised so a
// new action never vanishes from the Detail column.

import { useEffect, useRef, useState } from "react";
import type { AdminActivityItem, AdminActivitySeverity } from "../../../api/admin";
import { Button } from "../../../components/Button";
import type { ChipVariant } from "../../../components/Chip";
import { Chip } from "../../../components/Chip";
import { IconCheck, IconChevronRight, IconCopy } from "../../../components/icons";
import { actorLabel, targetLabel } from "./auditFilters";

const ACTION_LABELS: Record<string, string> = {
  "app.create": "Created app",
  "app.update": "Updated app",
  "app.delete": "Deleted app",
  "host.drain": "Drained host",
  "host.uncordon": "Resumed scheduling",
  "host.delete": "Forgot host",
  "host.restart": "Restarted agent",
  "host.settings.update": "Updated host settings",
  "host.console.update": "Updated console settings",
  "user.create": "Created user",
  "user.update": "Updated user",
  "user.delete": "Deleted user",
  "invite.create": "Created invite",
  "invite.revoke": "Revoked invite",
  "session.force_stop": "Force-stopped session",
  "settings.update": "Updated settings",
  // Keys must match the server's emitted strings exactly (storage/handler.go
  // etc.) — a stale key silently falls through to humanise().
  "storage.home.tombstone": "Marked home for cleanup",
  "stream_profile.create": "Created stream profile",
  "stream_profile.update": "Updated stream profile",
  "stream_profile.delete": "Deleted stream profile",
  "launch_profile.create": "Created launch profile",
  "launch_profile.update": "Updated launch profile",
  "launch_profile.delete": "Deleted launch profile",
};

/** Humanise an unmapped action: "thing.some_verb" → "Thing some verb". */
function humanise(action: string): string {
  const words = action.replace(/[._]/g, " ").trim();
  return words.charAt(0).toUpperCase() + words.slice(1);
}

export function actionLabel(action: string): string {
  return ACTION_LABELS[action] ?? humanise(action);
}

const SEVERITY_VARIANT: Record<AdminActivitySeverity, ChipVariant> = {
  err: "danger",
  warn: "warning",
  info: "neutral",
};

const NO_DETAIL = "No additional detail was recorded for this action.";

function isEmptyDetail(details: unknown): boolean {
  if (details === null || details === undefined) return true;
  if (typeof details === "object" && !Array.isArray(details)) {
    return Object.keys(details as Record<string, unknown>).length === 0;
  }
  return false;
}

function prettyDetail(details: unknown): string {
  if (isEmptyDetail(details)) return NO_DETAIL;
  try {
    return JSON.stringify(details, null, 2);
  } catch {
    return String(details);
  }
}

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

/** The console readout's full text: the raw action/target/actor lines the
 *  mock's `detailFull` blocks open with, then the pretty-printed payload. */
export function detailReadout(item: AdminActivityItem): string {
  const lines = [
    `action  ${item.action}`,
    `target  ${targetLabel(item)}`,
    `actor   ${actorLabel(item)}`,
  ];
  return `${lines.join("\n")}\n\n${prettyDetail(item.details)}`;
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
        <td className="aud-summary mono">{actionLabel(item.action)}</td>
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
