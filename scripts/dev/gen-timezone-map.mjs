#!/usr/bin/env node
/*
 * Generates web/src/lib/region/worldMap.ts, the data behind the Account > Region timezone map
 * (#847, design_handoff_v3/screens/region-input-v3.html): a 2-degree land grid and one
 * [IANA zone, latitude, longitude] point per place in the tz database's zone.tab.
 *
 * Run by hand when a source below is bumped; the web build and CI never run it, and the output
 * is committed. No dependencies, Node 22+.
 *
 *   node scripts/dev/gen-timezone-map.mjs [--out web/src/lib/region/worldMap.ts]
 *
 * Sources (both public domain), fetched into a fresh temp dir that is removed afterwards and
 * checked against the sha256 pinned here:
 *   - Natural Earth 1:50m land, natural-earth-vector v5.1.2
 *     https://raw.githubusercontent.com/nvkelso/natural-earth-vector/v5.1.2/geojson/ne_50m_land.geojson
 *   - tz database zone.tab, release 2026c
 *     https://raw.githubusercontent.com/eggert/tz/2026c/zone.tab
 * To bump one, change its URL and sha256 (printed on a mismatch) and re-run.
 *
 * A grid cell is land when at least a fifth of a SAMPLES x SAMPLES lattice of points across it lies
 * inside a Natural Earth land polygon (holes such as the Caspian Sea count as sea), so thin
 * coasts and island chains keep a dot. Rows run from MAP_TOP down in MAP_STEP degrees, so row r
 * is centred on latitude MAP_TOP - r * MAP_STEP; Antarctica (and its zones) are left off.
 * Each row is stored as run lengths, alternating sea and land and starting with sea.
 */
import { createHash } from "node:crypto";
import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { parseArgs } from "node:util";

const STEP = 2;
const TOP = 82;
const BOTTOM = -56;
const COLS = 360 / STEP;
const SAMPLES = 5;
const MIN_LAND = 0.2;

const SOURCES = {
  land: {
    url: "https://raw.githubusercontent.com/nvkelso/natural-earth-vector/v5.1.2/geojson/ne_50m_land.geojson",
    sha256: "e874b27a51d146452be360cafb3cc50c86001074a67d534113e6534682f9826b",
  },
  zoneTab: {
    url: "https://raw.githubusercontent.com/eggert/tz/2026c/zone.tab",
    sha256: "7cc78ea166261b3dedf951cdd721051460851e6fcd96c12b8e3194cf25677f21",
  },
};

async function fetchChecked(dir, name, { url, sha256 }) {
  const res = await fetch(url);
  if (!res.ok) throw new Error(`${url}: HTTP ${res.status}`);
  const buf = Buffer.from(await res.arrayBuffer());
  const got = createHash("sha256").update(buf).digest("hex");
  if (got !== sha256) throw new Error(`${url}: sha256 ${got}, expected ${sha256}`);
  const path = join(dir, name);
  await writeFile(path, buf);
  return readFile(path, "utf8");
}

/** zone.tab's ISO 6709 "+DDMM[SS]+DDDMM[SS]" as decimal degrees. */
export function parseCoords(s) {
  const m = s.match(/^([+-])(\d{2})(\d{2})(\d{2})?([+-])(\d{3})(\d{2})(\d{2})?$/);
  if (!m) throw new Error(`bad zone.tab coordinates: ${s}`);
  const deg = (sign, d, mi, se) => (sign === "-" ? -1 : 1) * (Number(d) + Number(mi) / 60 + Number(se ?? 0) / 3600);
  return [deg(m[1], m[2], m[3], m[4]), deg(m[5], m[6], m[7], m[8])];
}

export function parseZoneTab(text) {
  const points = [];
  for (const line of text.split("\n")) {
    if (!line || line.startsWith("#")) continue;
    const [, coords, tz] = line.split("\t");
    if (tz.startsWith("Antarctica/")) continue;
    const [lat, lon] = parseCoords(coords);
    points.push([tz, Number(lat.toFixed(1)), Number(lon.toFixed(1))]);
  }
  return points;
}

// Even-odd over every ring of a polygon: the outer ring in, each hole out.
function inRings(rings, x, y) {
  let inside = false;
  for (const ring of rings) {
    for (let i = 0, j = ring.length - 1; i < ring.length; j = i++) {
      const [xi, yi] = ring[i];
      const [xj, yj] = ring[j];
      if (yi > y !== yj > y && x < ((xj - xi) * (y - yi)) / (yj - yi) + xi) inside = !inside;
    }
  }
  return inside;
}

export function landRows(geojson) {
  const polys = geojson.features.flatMap((f) =>
    f.geometry.type === "Polygon" ? [f.geometry.coordinates] : f.geometry.coordinates,
  );
  const boxes = polys.map((rings) => {
    const xs = rings[0].map((p) => p[0]);
    const ys = rings[0].map((p) => p[1]);
    return [Math.min(...xs), Math.max(...xs), Math.min(...ys), Math.max(...ys)];
  });
  const rows = [];
  for (let lat = TOP; lat >= BOTTOM; lat -= STEP) {
    const runs = [];
    let cur = false;
    let len = 0;
    for (let c = 0; c < COLS; c++) {
      const lon = -180 + STEP / 2 + c * STEP;
      let hits = 0;
      for (let a = 0; a < SAMPLES; a++) {
        for (let b = 0; b < SAMPLES; b++) {
          const x = lon - STEP / 2 + ((b + 0.5) * STEP) / SAMPLES;
          const y = lat + STEP / 2 - ((a + 0.5) * STEP) / SAMPLES;
          if (polys.some((rings, k) => x >= boxes[k][0] && x <= boxes[k][1] && y >= boxes[k][2] && y <= boxes[k][3] && inRings(rings, x, y))) hits++;
        }
      }
      const land = hits >= MIN_LAND * SAMPLES * SAMPLES;
      if (land === cur) len++;
      else {
        runs.push(len);
        cur = land;
        len = 1;
      }
    }
    runs.push(len);
    rows.push(runs.join(","));
  }
  return rows;
}

export function render(rows, points) {
  return `// GENERATED by scripts/dev/gen-timezone-map.mjs: do not edit; re-run the script (its header lists the sources).
// Land: Natural Earth 1:50m (public domain). Zone points: tz database zone.tab (public domain).
// Land is a ${STEP}-degree grid; row r is centred on latitude ${TOP} - r * ${STEP}, column c on longitude -180 + ${STEP / 2} + c * ${STEP}.
// Each row is run lengths, alternating sea and land, starting with sea.
export const MAP_STEP = ${STEP};
export const MAP_TOP = ${TOP};
export const LAND_ROWS: readonly string[] = ${JSON.stringify(rows)};
/** [IANA zone, latitude, longitude] for every zone.tab place the picker lists. */
export const ZONE_POINTS: readonly (readonly [string, number, number])[] = ${JSON.stringify(points)};
`;
}

async function main() {
  const { values } = parseArgs({ options: { out: { type: "string", default: "web/src/lib/region/worldMap.ts" } } });
  const dir = await mkdtemp(join(tmpdir(), "tzmap-"));
  try {
    const [land, tab] = await Promise.all([
      fetchChecked(dir, "ne_50m_land.geojson", SOURCES.land),
      fetchChecked(dir, "zone.tab", SOURCES.zoneTab),
    ]);
    const rows = landRows(JSON.parse(land));
    const points = parseZoneTab(tab);
    await writeFile(resolve(values.out), render(rows, points));
    console.log(`${values.out}: ${rows.length} rows, ${points.length} zones`);
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
}

if (import.meta.url === `file://${process.argv[1]}`) await main();
