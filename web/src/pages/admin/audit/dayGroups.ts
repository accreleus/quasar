// Groups audit rows into local-day cards, newest day first (handoff-v3-spec
// §A.20's "Today · 8 August 2026" panel heads). Pure — no fetch, no React.

import type { AdminActivityItem } from "../../../api/admin";

export interface DayGroup {
  /** Local day, as a stringified epoch-ms timestamp. Sorting already
   *  happened by the time this is set (via the same numeric value, in
   *  `groupByDay`) — this is a stable React list key, not a sort input. */
  key: string;
  label: string;
  items: AdminActivityItem[];
}

const MONTHS = [
  "January", "February", "March", "April", "May", "June",
  "July", "August", "September", "October", "November", "December",
];
const WEEKDAYS = ["Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"];

function startOfLocalDay(d: Date): Date {
  return new Date(d.getFullYear(), d.getMonth(), d.getDate());
}

function formatDate(d: Date): string {
  return `${d.getDate()} ${MONTHS[d.getMonth()]} ${d.getFullYear()}`;
}

function dayLabel(day: Date, now: Date): string {
  const diffDays = Math.round(
    (startOfLocalDay(now).getTime() - day.getTime()) / (24 * 60 * 60 * 1000),
  );
  const dateStr = formatDate(day);
  if (diffDays === 0) return `Today · ${dateStr}`;
  if (diffDays === 1) return `Yesterday · ${dateStr}`;
  return `${WEEKDAYS[day.getDay()]} · ${dateStr}`;
}

export function groupByDay(items: AdminActivityItem[], now: Date = new Date()): DayGroup[] {
  const map = new Map<number, AdminActivityItem[]>();
  for (const item of items) {
    const day = startOfLocalDay(new Date(item.created_at)).getTime();
    const list = map.get(day);
    if (list) list.push(item);
    else map.set(day, [item]);
  }
  return [...map.entries()]
    .sort((a, b) => b[0] - a[0])
    .map(([day, dayItems]) => ({
      key: String(day),
      label: dayLabel(new Date(day), now),
      items: dayItems,
    }));
}
