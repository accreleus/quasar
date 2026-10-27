// What the timezone pickers (account Region, admin Region defaults) show for a zone: place name,
// country, the UTC offset now and the clock there. Pure, so it is tested on its own (zones.test.ts).
import { ZONE_TABLE } from "./zoneTable";

export interface Zone {
  tz: string;
  city: string;
  country: string;
  /** Minutes east of UTC at the instant the list was built. */
  offset: number;
  /** Lower-case text a search matches against. */
  key: string;
}

const names = new Intl.DisplayNames(["en"], { type: "region" });

export const cityOf = (tz: string) => (tz.split("/").pop() ?? tz).replace(/_/g, " ");

/** "UTC+05:30": the real minus sign, as the mock sets it. */
export function formatOffset(min: number): string {
  const abs = Math.abs(min);
  return `UTC${min < 0 ? "−" : "+"}${String(Math.floor(abs / 60)).padStart(2, "0")}:${String(abs % 60).padStart(2, "0")}`;
}

/** The zone's offset from UTC at `at`, or null when this browser does not know the zone. */
export function offsetMinutes(tz: string, at: Date): number | null {
  try {
    const part = new Intl.DateTimeFormat("en-US", { timeZone: tz, timeZoneName: "shortOffset" })
      .formatToParts(at)
      .find((p) => p.type === "timeZoneName")?.value;
    const m = part?.match(/GMT([+-])(\d+)(?::(\d+))?/);
    return m ? (m[1] === "-" ? -1 : 1) * (Number(m[2]) * 60 + Number(m[3] ?? 0)) : 0;
  } catch {
    return null;
  }
}

/** "14:05" in the zone. */
export const clockIn = (tz: string, at: Date) =>
  new Intl.DateTimeFormat("en-GB", { timeZone: tz, hour: "2-digit", minute: "2-digit" }).format(at);

/** This browser's own zone, or null when it reports none this page can use. */
export function browserZone(): string | null {
  const tz = Intl.DateTimeFormat().resolvedOptions().timeZone;
  return tz && offsetMinutes(tz, new Date()) !== null ? tz : null;
}

function makeZone(tz: string, cc: string | null, offset: number): Zone {
  let country = "";
  try {
    country = cc ? (names.of(cc) ?? cc) : "";
  } catch {
    country = cc ?? "";
  }
  const city = cityOf(tz);
  return { tz, city, country, offset, key: `${city} ${country} ${tz} ${formatOffset(offset)}`.toLowerCase() };
}

/** The curated list, sorted by place, minus any zone this browser cannot format. */
export function buildZones(at: Date): Zone[] {
  const zones: Zone[] = [];
  for (const [tz, cc] of ZONE_TABLE) {
    const off = offsetMinutes(tz, at);
    if (off !== null) zones.push(makeZone(tz, cc, off));
  }
  return zones.sort((a, b) => a.city.localeCompare(b.city) || a.tz.localeCompare(b.tz));
}

/** A zone by name; one outside the curated list (set by API, or a link) still shows as itself. */
export function zoneFor(tz: string, zones: readonly Zone[], at: Date): Zone {
  return zones.find((z) => z.tz === tz) ?? makeZone(tz, null, offsetMinutes(tz, at) ?? 0);
}

/** Every word of `term` must appear in the zone's place, country, name or offset. */
export function searchZones(zones: readonly Zone[], term: string): Zone[] {
  const words = term.trim().toLowerCase().split(/\s+/).filter(Boolean);
  return words.length ? zones.filter((z) => words.every((w) => z.key.includes(w))) : [...zones];
}

/** Zones grouped by their area (`Europe`, `America`, …), for a plain dropdown. */
export function groupZones(zones: readonly Zone[]): [string, Zone[]][] {
  const groups = new Map<string, Zone[]>();
  for (const z of zones) {
    const area = z.tz.split("/")[0] ?? z.tz;
    groups.set(area, [...(groups.get(area) ?? []), z]);
  }
  return [...groups].sort(([a], [b]) => a.localeCompare(b));
}
