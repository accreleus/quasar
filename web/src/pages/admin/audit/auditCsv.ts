// Client-side CSV export (handoff-v3-spec §9 — Export CSV is client-side over
// the loaded rows, no server endpoint). Pure — no fetch, no React.

import type { AdminActivityItem } from "../../../api/admin";
import { actorLabel } from "./auditFilters";
import { targetLabel } from "./describe";

// target_name sits beside target_id, never instead of it: a spreadsheet is read
// by a human and pivoted by an id.
const HEADER = "time,actor,action,target_type,target_id,target_name,severity,details";

/** RFC 4180: quote only a field that needs it (holds a comma, quote or
 *  newline), doubling any embedded quote. */
function csvField(value: string): string {
  if (/[",\n\r]/.test(value)) return `"${value.replace(/"/g, '""')}"`;
  return value;
}

function csvRow(item: AdminActivityItem): string {
  return [
    item.created_at,
    actorLabel(item),
    item.action,
    item.target_type,
    item.target_id ?? "",
    item.target_id ? targetLabel(item) : "",
    item.severity,
    JSON.stringify(item.details ?? {}),
  ]
    .map(csvField)
    .join(",");
}

export function toCsv(items: AdminActivityItem[]): string {
  return [HEADER, ...items.map(csvRow)].join("\n");
}
