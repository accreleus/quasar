// Geometry for the Region timezone map: the land grid as SVG dots, the nearest zone.tab point to a
// pointer, and the points that mark a few big cities. Pure, so mapPick.test.ts covers it without a DOM.
import { LAND_ROWS, MAP_STEP, MAP_TOP, ZONE_POINTS } from "./worldMap";
import type { Zone } from "./zones";

/** SVG units per grid cell. */
export const CELL = 6;
export const COLS = 360 / MAP_STEP;
export const W = COLS * CELL;
export const H = LAND_ROWS.length * CELL;

export const mapX = (lon: number) => ((lon + 180) / MAP_STEP) * CELL;
export const mapY = (lat: number) => ((MAP_TOP - lat) / MAP_STEP) * CELL + CELL / 2;

/** Where a pointer is, as fractions of the drawn map's width and height (0..1), in degrees. */
export const lonLatAt = (fx: number, fy: number) => ({
  lon: ((fx * W) / CELL) * MAP_STEP - 180,
  lat: MAP_TOP - (((fy * H - CELL / 2) / CELL) * MAP_STEP),
});

/** The places the map marks with a dot, as the mock draws them. */
const CITY_DOTS = [
  "America/Los_Angeles America/Denver America/Chicago America/New_York America/Mexico_City America/Bogota",
  "America/Lima America/Sao_Paulo America/Argentina/Buenos_Aires America/Santiago America/Anchorage America/Halifax",
  "America/St_Johns Pacific/Honolulu Atlantic/Reykjavik Europe/London Europe/Lisbon Europe/Paris Europe/Berlin",
  "Europe/Madrid Europe/Rome Europe/Stockholm Europe/Athens Europe/Helsinki Europe/Istanbul Europe/Moscow",
  "Africa/Lagos Africa/Cairo Africa/Nairobi Africa/Johannesburg Asia/Dubai Asia/Tehran Asia/Karachi Asia/Kolkata",
  "Asia/Dhaka Asia/Bangkok Asia/Singapore Asia/Shanghai Asia/Seoul Asia/Tokyo Asia/Jakarta Asia/Manila",
  "Australia/Perth Australia/Adelaide Australia/Sydney Pacific/Auckland Asia/Yekaterinburg Asia/Novosibirsk Asia/Vladivostok",
].join(" ").split(" ");

export interface MapPoint {
  zone: Zone;
  lat: number;
  lon: number;
}

/** The listed zones that have a place on the map. */
export function mapPoints(zones: readonly Zone[]): MapPoint[] {
  const byTz = new Map(zones.map((z) => [z.tz, z]));
  return ZONE_POINTS.flatMap(([tz, lat, lon]) => {
    const zone = byTz.get(tz);
    return zone ? [{ zone, lat, lon }] : [];
  });
}

export const cityDots = (points: readonly MapPoint[]) => points.filter((p) => CITY_DOTS.includes(p.zone.tz));

/** The point nearest (lon, lat); longitude is scaled by cos(latitude) and wraps at the antimeridian. */
export function nearest(points: readonly MapPoint[], lon: number, lat: number): MapPoint | null {
  const k = Math.cos((lat * Math.PI) / 180);
  let best: MapPoint | null = null;
  let bestD = Infinity;
  for (const p of points) {
    let dl = Math.abs(p.lon - lon);
    if (dl > 180) dl = 360 - dl;
    const d = (dl * k) ** 2 + (p.lat - lat) ** 2;
    if (d < bestD) {
      bestD = d;
      best = p;
    }
  }
  return best;
}

/** Land as one SVG path of zero-length dots (drawn round by stroke-linecap) per UTC offset: each cell goes to its nearest zone's offset. */
export function landPaths(points: readonly MapPoint[]): Map<number, string> {
  const byOffset = new Map<number, string[]>();
  LAND_ROWS.forEach((row, r) => {
    const lat = MAP_TOP - r * MAP_STEP;
    let col = 0;
    row.split(",").forEach((run, i) => {
      const len = Number(run);
      for (let c = col; i % 2 === 1 && c < col + len; c++) {
        const off = nearest(points, -180 + MAP_STEP / 2 + c * MAP_STEP, lat)?.zone.offset;
        if (off == null) continue;
        const d = byOffset.get(off) ?? [];
        d.push(`M${c * CELL + CELL / 2} ${r * CELL + CELL / 2}h0`);
        byOffset.set(off, d);
      }
      col += len;
    });
  });
  return new Map([...byOffset].map(([off, d]) => [off, d.join("")]));
}
