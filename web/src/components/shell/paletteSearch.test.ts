import { describe, expect, it } from "vitest";
import {
  palettePlaceholder,
  paletteScope,
  paletteTriggerLabel,
  searchPalette,
  PALETTE_ACTIONS,
  USER_PALETTE_ACTIONS,
} from "./paletteSearch";

const hosts = [{ id: "h1", node_name: "quasar-node-1", status: "online" }];
const sessions = [{ id: "s1", app_name: "Blender", username: "devon", state: "running" }];
const apps = [{ id: "a1", name: "Blender", kind: "desktop" }];
const users = [{ id: "u1", username: "devon", email: "devon@x.io" }];

describe("searchPalette", () => {
  it("empty query lists six actions and nothing else", () => {
    const r = searchPalette("", { hosts, sessions, apps, users, isAdmin: true });
    expect(r.map((g) => g.title)).toEqual(["Actions"]);
    expect(r[0].items).toHaveLength(6);
  });

  it("matches hosts, sessions, apps and users by substring, case-insensitive", () => {
    const r = searchPalette("BLEND", { hosts, sessions, apps, users, isAdmin: true });
    expect(r.find((g) => g.title === "Sessions")?.items[0]).toMatchObject({
      label: "Blender · devon",
      to: "/admin/sessions/s1",
    });
    expect(r.find((g) => g.title === "Apps")?.items[0]).toMatchObject({
      to: "/admin/library/apps/a1",
    });
  });

  it("matches a host by name or id, and routes it into Fleet", () => {
    const r = searchPalette("node-1", { hosts, sessions, apps, users, isAdmin: true });
    expect(r.find((g) => g.title === "Hosts")?.items[0]).toMatchObject({
      label: "quasar-node-1",
      meta: "online",
      to: "/admin/fleet/hosts/h1",
    });
    expect(searchPalette("h1", { hosts, sessions, apps, users, isAdmin: true }).map((g) => g.title)).toContain(
      "Hosts",
    );
  });

  it("matches a user by username or email", () => {
    const r = searchPalette("x.io", { hosts, sessions, apps, users, isAdmin: true });
    expect(r.find((g) => g.title === "Users")?.items[0]).toMatchObject({
      label: "devon",
      meta: "devon@x.io",
      to: "/admin/people/users",
    });
  });

  it("non-admins see only actions and apps, routed to the user area", () => {
    const r = searchPalette("blend", { hosts, sessions, apps, users, isAdmin: false });
    // No user action reads "blend", so the Actions group drops out exactly as
    // it would for an admin — hosts, sessions and users are never offered.
    expect(r.map((g) => g.title)).toEqual(["Apps"]);
    expect(r[0].items[0].to).toBe("/app/library?q=Blender");
  });

  it("filters a non-admin's action list by the same rule as an admin's", () => {
    const r = searchPalette("acc", { hosts, sessions, apps, users, isAdmin: false });
    expect(r.map((g) => g.title)).toEqual(["Actions"]);
    expect(r[0].items.map((i) => i.label)).toEqual(["Go to Account"]);
  });

  it("reaches 'No matches' for a non-admin too", () => {
    expect(searchPalette("zzz", { hosts, sessions, apps, users, isAdmin: false })).toEqual([
      { title: "No matches", items: [] },
    ]);
  });

  it("lists the four user actions on an empty query", () => {
    const r = searchPalette("", { hosts, sessions, apps, users, isAdmin: false });
    expect(r.map((g) => g.title)).toEqual(["Actions"]);
    expect(r[0].items).toHaveLength(4);
  });

  it("matches regardless of the query's case on either side", () => {
    expect(
      searchPalette("BLENDER", { hosts, sessions, apps, users, isAdmin: false })[0].items[0].label,
    ).toBe("Blender");
  });

  it("returns a single 'No matches' group when nothing hits", () => {
    expect(searchPalette("zzz", { hosts, sessions, apps, users, isAdmin: true })).toEqual([
      { title: "No matches", items: [] },
    ]);
  });

  it("caps actions at five on a non-empty query and entities at four", () => {
    const many = Array.from({ length: 9 }, (_, i) => ({
      id: `a${i}`,
      name: `Blender ${i}`,
      kind: "desktop",
    }));
    const r = searchPalette("go to", { hosts, sessions, apps: many, users, isAdmin: true });
    expect(r.find((g) => g.title === "Actions")?.items).toHaveLength(5);
    expect(searchPalette("Blender", { hosts, sessions, apps: many, users, isAdmin: true })
      .find((g) => g.title === "Apps")?.items).toHaveLength(4);
  });

  it("keeps the appearance toggle as an action, not a route", () => {
    const toggle = PALETTE_ACTIONS.find((a) => a.label === "Toggle appearance");
    expect(toggle?.to).toBeUndefined();
    expect(toggle?.action).toBe("toggle-appearance");
    expect(USER_PALETTE_ACTIONS.map((a) => a.label)).toEqual([
      "Go to Home",
      "Go to Library",
      "Go to Account",
      "Toggle appearance",
    ]);
  });

  it("routes every admin action at a v3 path", () => {
    for (const action of PALETTE_ACTIONS) {
      if (!action.to) continue;
      expect(action.to.startsWith("/admin") || action.to.startsWith("/app")).toBe(true);
    }
    expect(PALETTE_ACTIONS.find((a) => a.label === "Go to Fleet")?.to).toBe("/admin/fleet/hosts");
    expect(PALETTE_ACTIONS.find((a) => a.label === "Go to Library")?.to).toBe("/admin/library/apps");
  });
});

describe("paletteScope", () => {
  it("names only the lists the shell supplied", () => {
    expect(paletteScope({ isAdmin: true, hosts, sessions, apps, users })).toEqual([
      "hosts",
      "sessions",
      "apps",
      "users",
    ]);
    // The account area holds a catalogue and nothing else, admin or not.
    expect(paletteScope({ isAdmin: true, apps })).toEqual(["apps"]);
  });

  it("counts an empty list as supplied and an absent one as not", () => {
    expect(paletteScope({ isAdmin: true, hosts: [] })).toEqual(["hosts"]);
    expect(paletteScope({ isAdmin: true })).toEqual([]);
  });

  it("hides the admin-only kinds from a user and calls apps games", () => {
    expect(paletteScope({ isAdmin: false, hosts, sessions, apps, users })).toEqual(["games"]);
  });

  it("builds both labels from the same scope", () => {
    const admin = paletteScope({ isAdmin: true, hosts, sessions, apps, users });
    expect(paletteTriggerLabel(admin)).toBe("Search hosts, sessions, apps, users\u2026");
    expect(palettePlaceholder(admin)).toBe("Search hosts, sessions, apps, users or jump to a page");

    const user = paletteScope({ isAdmin: false, apps });
    expect(paletteTriggerLabel(user)).toBe("Search games\u2026");
    expect(palettePlaceholder(user)).toBe("Search games or jump to a page");

    expect(paletteTriggerLabel([])).toBe("Jump to a page");
    expect(palettePlaceholder([])).toBe("Jump to a page");
  });
});
