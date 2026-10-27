import { describe, expect, it } from "vitest";
import { searchPalette } from "../paletteSearch";
import { ACCOUNT_SETTINGS, ADMIN_SETTINGS, arrangeWithSettings, deviceSettings, placeOf, searchSettings } from "./settingsIndex";

const ADMIN = true;
const labels = (q: string, entries = ACCOUNT_SETTINGS) => searchSettings(q, entries).map((e) => e.label);
const titles = (gs: { title: string }[]) => gs.map((g) => g.title);

describe("settings search", () => {
  it("finds settings by the words people type", () => {
    expect(labels("1080p")[0]).toBe("Stream default");
    expect(labels("resolution")).toContain("Stream default");
    expect(labels("HUD")).toContain("In-session overlay");
    expect(labels("sign out everywhere")).toEqual(expect.arrayContaining(["Change password", "Password"]));
    expect(labels("bitrate")).toContain("FPS, latency, bitrate");
    expect(labels("auto-hide")[0]).toBe("Auto-hide");
  });

  it("finds Steam dialogs, and the warm-Steam admin rows under Library sources", () => {
    expect(labels("steam dialogs")[0]).toBe("Steam dialogs");
    expect(labels("controller notice")).toContain("Steam dialogs");
    expect(searchSettings("warm", ADMIN_SETTINGS).map((e) => e.label)).toContain("Sources");
    expect(searchSettings("preparation", ADMIN_SETTINGS).map((e) => e.label)).toContain("Sources");
  });

  it("ranks a label match above a keyword match", () => {
    expect(labels("overlay")[0]).toBe("In-session overlay");
    expect(labels("password")[0]).toBe("Password");
  });

  it("needs every word to match and ignores an empty query", () => {
    expect(labels("overlay password")).toEqual([]);
    expect(labels("   ")).toEqual([]);
    expect(labels("zzzz")).toEqual([]);
  });

  it("finds each device by its name", () => {
    const devs = deviceSettings([{ id: "d1", label: "iPhone · Safari", current: false }]);
    const hit = searchSettings("iphone", [...ACCOUNT_SETTINGS, ...devs]);
    expect(hit[0]).toMatchObject({ label: "iPhone · Safari", target: { sel: "#os-dev-d1" } });
  });

  it("finds the Classic layout switch by the words people type", () => {
    for (const q of ["classic", "layout", "old design", "previous design", "turn off new design"]) {
      expect(labels(q), q).toContain("Classic layout");
    }
    expect(searchSettings("classic", ACCOUNT_SETTINGS)[0]).toMatchObject({
      to: "/app/account/appearance",
      target: { sel: "#classic-layout" },
    });
  });

  it("finds the Region section and its timezone setting", () => {
    expect(labels("timezone")[0]).toBe("Timezone");
    expect(labels("clock")).toEqual(expect.arrayContaining(["Region", "Timezone"]));
    expect(searchSettings("timezone", ACCOUNT_SETTINGS)[0]).toMatchObject({ to: "/app/account/region", target: { sel: "#tz-label" } });
    expect(labels("timezone", ADMIN_SETTINGS)).toContain("Timezone");
  });

  it("finds admin pages, tabs and settings rows", () => {
    expect(labels("releases", ADMIN_SETTINGS)[0]).toBe("Releases");
    expect(labels("1080p", ADMIN_SETTINGS)).toContain("Launch profiles");
    expect(labels("cors", ADMIN_SETTINGS)).toEqual(["Allowed origins"]);
    expect(labels("dark", ADMIN_SETTINGS)).toContain("Theme");
  });
});

describe("palette order: the current place first", () => {
  const apps = [{ id: "a1", name: "Overlay Racer", kind: "game" }];
  const sources = { hosts: [], sessions: [], apps, users: [], isAdmin: false };

  it("account: Settings, then games, then actions", () => {
    const g = arrangeWithSettings(searchPalette("overlay", sources), "overlay", placeOf("/app/account/profile", ADMIN));
    expect(titles(g)).toEqual(["Settings", "Apps"]);
    expect(g[0].items[0]).toMatchObject({ label: "In-session overlay", to: "/app/account/overlay", reveal: { sel: "#set-overlay" } });
  });

  it("account with no query lists the sections first", () => {
    const g = arrangeWithSettings(searchPalette("", sources), "", placeOf("/app/account", ADMIN));
    expect(titles(g)).toEqual(["Settings", "Actions"]);
    expect(g[0].items.map((i) => i.label)).toEqual(["Profile", "Stream quality", "Region", "In-session overlay", "Appearance", "Devices", "Password", "Storage", "My sessions"]);
  });

  it("home: games, then Settings", () => {
    const g = arrangeWithSettings(searchPalette("overlay", sources), "overlay", placeOf("/app", ADMIN));
    expect(titles(g)).toEqual(["Apps", "Settings"]);
  });

  it("home with no query adds nothing", () => {
    const shipped = searchPalette("", sources);
    expect(arrangeWithSettings(shipped, "", placeOf("/app/library", ADMIN))).toEqual(shipped);
  });

  it("admin: this area, then objects, then elsewhere", () => {
    const admin = { ...sources, isAdmin: true, hosts: [{ id: "h1", node_name: "image-box", status: "online" }] };
    const g = arrangeWithSettings(searchPalette("image", admin), "image", placeOf("/admin/fleet/hosts", ADMIN));
    expect(titles(g)).toEqual(["Hosts", "Elsewhere in the console"]);
    const s = arrangeWithSettings(searchPalette("storage", admin), "storage", placeOf("/admin/fleet/storage", ADMIN));
    expect(titles(s)).toEqual(["Fleet"]);
    expect(s[0].items[0].label).toBe("Storage");
  });

  it("admin: the Go-to jumps drop once you type, Go to Account stays", () => {
    const admin = { ...sources, isAdmin: true };
    const fleet = arrangeWithSettings(searchPalette("fleet", admin), "fleet", placeOf("/admin", ADMIN));
    expect(fleet.flatMap((x) => x.items).some((i) => i.id === "go-fleet")).toBe(false);
    const acct = arrangeWithSettings(searchPalette("account", admin), "account", placeOf("/admin", ADMIN));
    expect(acct.find((x) => x.title === "Actions")?.items.map((i) => i.id)).toEqual(["go-account"]);
  });

  it("admin with no query: this area's pages, then the shipped actions", () => {
    const admin = { ...sources, isAdmin: true };
    const g = arrangeWithSettings(searchPalette("", admin), "", placeOf("/admin/settings", ADMIN));
    expect(g[0].title).toBe("Settings");
    expect(g[0].items.map((i) => i.label)).not.toContain("Settings");
    expect(g[1]).toEqual(searchPalette("", admin)[0]);
    const overview = arrangeWithSettings(searchPalette("", admin), "", placeOf("/admin", ADMIN));
    expect(overview.map((x) => x.title)).toEqual(["Actions"]);
    const fleet = arrangeWithSettings(searchPalette("", admin), "", placeOf("/admin/fleet/hosts", ADMIN));
    expect(fleet[0].items.map((i) => i.label)).toEqual(["Storage", "Jobs", "Releases"]);
  });

  it("admin entries need an admin: a non-admin on /admin is treated as being at home", () => {
    expect(placeOf("/admin/fleet/hosts", false)).toEqual({ kind: "home" });
    expect(placeOf("/admin/fleet/hosts", true)).toMatchObject({ kind: "admin", area: "fleet" });
    const g = arrangeWithSettings(searchPalette("releases", sources), "releases", placeOf("/admin", false));
    expect(g.flatMap((x) => x.items).some((i) => i.to?.startsWith("/admin"))).toBe(false);
  });

  it("says No matches when nothing does", () => {
    expect(arrangeWithSettings(searchPalette("qqqq", sources), "qqqq", placeOf("/app/account", ADMIN))).toEqual([{ title: "No matches", items: [] }]);
  });
});
