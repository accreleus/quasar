import { describe, expect, it } from "vitest";
import { ACCOUNT_TABS, buildAccountSections } from "./accountNav";

const items = () => buildAccountSections().flatMap((s) => s.items);

describe("account IA", () => {
  it("is one untitled section, as the mock's rail renders", () => {
    expect(buildAccountSections().map((s) => s.title)).toEqual([undefined]);
  });

  it("is the three v3 rows, in order, each pointing at its section's first page", () => {
    expect(items().map((i) => [i.label, i.to])).toEqual([
      ["Account", "/app/account/profile"],
      ["Preferences", "/app/account/overlay"],
      ["Usage", "/app/account/storage"],
    ]);
  });

  it("routes every row under /app/account and gives it a glyph", () => {
    for (const item of items()) {
      expect(item.to.startsWith("/app/account")).toBe(true);
      expect(item.icon).toBeTruthy();
    }
  });

  it("carries Usage, which holds Storage after it moved out of the /app topbar", () => {
    expect(items().find((i) => i.id === "usage")?.to).toBe("/app/account/storage");
  });

  it("has no duplicate routes", () => {
    const routes = items().map((i) => i.to);
    expect(new Set(routes).size).toBe(routes.length);
  });

  // The rail has three rows for six pages, so a row must light for every page
  // in its section — and for no page outside it.
  it("lights each row for exactly its section's tab paths", () => {
    const matched = (id: string) =>
      Object.values(ACCOUNT_TABS)
        .flat()
        .filter((t) => items().find((i) => i.id === id)!.match(t.to))
        .map((t) => t.to);
    for (const [key, tabs] of Object.entries(ACCOUNT_TABS)) {
      expect(matched(key)).toEqual(tabs.map((t) => t.to));
    }
  });

  it("lights exactly one row per destination", () => {
    for (const tab of Object.values(ACCOUNT_TABS).flat()) {
      expect(items().filter((i) => i.match(tab.to)).length).toBe(1);
    }
  });

  it("lights Account on the /app/account index too, which is the profile page today", () => {
    expect(items().filter((i) => i.match("/app/account")).map((i) => i.id)).toEqual(["account"]);
  });
});
