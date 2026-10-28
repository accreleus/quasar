import { describe, expect, it } from "vitest";
import { H, W, cityDots, landPaths, lonLatAt, mapPoints, mapX, mapY, nearest } from "./mapPick";
import { LAND_ROWS, ZONE_POINTS } from "./worldMap";
import { ZONE_TABLE } from "./zoneTable";
import { buildZones } from "./zones";

const points = mapPoints(buildZones(new Date("2026-01-15T12:00:00Z")));
const tzAt = (lon: number, lat: number) => nearest(points, lon, lat)?.zone.tz;

describe("the generated map data", () => {
  it("has a point for every curated zone, and no other", () => {
    expect(ZONE_POINTS.map(([tz]) => tz).sort()).toEqual(ZONE_TABLE.map(([tz]) => tz).sort());
  });

  it("is a 180 by 70 grid of alternating run lengths with land where it should be", () => {
    expect(LAND_ROWS).toHaveLength(70);
    const grid = LAND_ROWS.map((row) =>
      row.split(",").flatMap((n, i) => Array<boolean>(Number(n)).fill(i % 2 === 1)),
    );
    expect(grid.every((r) => r.length === 180)).toBe(true);
    const land = (lon: number, lat: number) => grid[Math.round((82 - lat) / 2)]![Math.floor((lon + 180) / 2)];
    expect(land(139.7, 36)).toBe(true); // Honshu
    expect(land(-0.1, 52)).toBe(true); // Britain
    expect(land(-150, 20)).toBe(false); // the Pacific
    expect(land(50, 42)).toBe(false); // the Caspian
  });
});

describe("picking a point on the map", () => {
  it("maps the pointer to degrees and back", () => {
    const { lon, lat } = lonLatAt(mapX(139.7) / W, mapY(35.7) / H);
    expect(lon).toBeCloseTo(139.7, 5);
    expect(lat).toBeCloseTo(35.7, 5);
  });

  it("picks the nearest zone.tab point: near Tokyo, Asia/Tokyo", () => {
    expect(tzAt(139.7, 35.7)).toBe("Asia/Tokyo");
    expect(tzAt(141, 37)).toBe("Asia/Tokyo");
  });

  it("picks the nearest point from open ocean too", () => {
    expect(tzAt(-150, 20)).toBe("Pacific/Honolulu");
  });

  it("wraps at the antimeridian", () => {
    expect(tzAt(-179.5, -18)).toBe("Pacific/Fiji");
    expect(tzAt(179.5, -18)).toBe("Pacific/Fiji");
  });

  it("gives nothing when no zone is listed", () => {
    expect(nearest([], 0, 0)).toBeNull();
  });
});

describe("drawing the map", () => {
  it("groups every land cell under the offset of its nearest zone", () => {
    const paths = landPaths(points);
    const cells = [...paths.values()].reduce((n, d) => n + d.split("M").length - 1, 0);
    const land = LAND_ROWS.reduce((n, row) => n + row.split(",").reduce((s, v, i) => s + (i % 2 ? Number(v) : 0), 0), 0);
    expect(cells).toBe(land);
    expect(paths.has(540)).toBe(true); // Japan's clock
  });

  it("marks only big cities the list has", () => {
    const dots = cityDots(points).map((p) => p.zone.tz);
    expect(dots).toContain("Asia/Tokyo");
    expect(dots).not.toContain("Asia/Ulaanbaatar");
    expect(dots.length).toBeGreaterThan(40);
  });
});
