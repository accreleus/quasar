import { describe, expect, it } from "vitest";
import { buildZones, cityOf, formatOffset, groupZones, offsetMinutes, searchZones, zoneFor } from "./zones";
import { ZONE_TABLE } from "./zoneTable";

const WINTER = new Date("2026-01-15T12:00:00Z");
const SUMMER = new Date("2026-07-15T12:00:00Z");

describe("timezone helpers", () => {
  it("formats offsets with a real minus sign and half-hour zones", () => {
    expect(formatOffset(0)).toBe("UTC+00:00");
    expect(formatOffset(330)).toBe("UTC+05:30");
    expect(formatOffset(-540)).toBe("UTC−09:00");
  });

  it("reads the offset at the given instant, so daylight time counts", () => {
    expect(offsetMinutes("Europe/London", WINTER)).toBe(0);
    expect(offsetMinutes("Europe/London", SUMMER)).toBe(60);
    expect(offsetMinutes("Asia/Kolkata", WINTER)).toBe(330);
    expect(offsetMinutes("America/Adak", WINTER)).toBe(-600);
    expect(offsetMinutes("America/Adak", SUMMER)).toBe(-540);
    expect(offsetMinutes("Not/AZone", WINTER)).toBeNull();
  });

  it("names the place from the zone", () => {
    expect(cityOf("America/Argentina/Buenos_Aires")).toBe("Buenos Aires");
    expect(cityOf("UTC")).toBe("UTC");
  });

  it("builds the curated list sorted by place, every zone formattable", () => {
    const zones = buildZones(WINTER);
    expect(zones.length).toBeGreaterThan(380);
    expect(new Set(zones.map((z) => z.tz)).size).toBe(zones.length);
    expect(zones.map((z) => z.city)).toEqual([...zones.map((z) => z.city)].sort((a, b) => a.localeCompare(b)));
    expect(zones.find((z) => z.tz === "Asia/Tokyo")).toMatchObject({ city: "Tokyo", country: "Japan", offset: 540 });
  });

  it("holds no zone the contract refuses: no Local, every name a tz identifier", () => {
    for (const [tz, cc] of ZONE_TABLE) {
      expect(tz, tz).toMatch(/^[A-Za-z]+(\/[A-Za-z0-9_+-]+)+$/);
      expect(cc).toMatch(/^[A-Z]{2}$/);
    }
  });

  it("shows a zone outside the list as itself", () => {
    expect(zoneFor("Asia/Calcutta", buildZones(WINTER), WINTER)).toMatchObject({ tz: "Asia/Calcutta", city: "Calcutta", offset: 330 });
  });

  it("needs every word to match the place, country, zone or offset", () => {
    const zones = buildZones(WINTER);
    expect(searchZones(zones, "japan").map((z) => z.tz)).toContain("Asia/Tokyo");
    expect(searchZones(zones, "new york america").map((z) => z.tz)).toEqual(["America/New_York"]);
    expect(searchZones(zones, "utc+05:30").map((z) => z.tz)).toContain("Asia/Kolkata");
    expect(searchZones(zones, "  ")).toHaveLength(zones.length);
    expect(searchZones(zones, "zzzz")).toEqual([]);
  });

  it("groups by area for the admin dropdown", () => {
    const groups = groupZones(buildZones(WINTER));
    expect(groups.map(([a]) => a)).toEqual(expect.arrayContaining(["Africa", "America", "Asia", "Europe", "Pacific"]));
    expect(groups.find(([a]) => a === "Europe")?.[1].every((z) => z.tz.startsWith("Europe/"))).toBe(true);
  });
});
