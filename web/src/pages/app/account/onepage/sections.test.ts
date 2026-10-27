import { describe, expect, it } from "vitest";
import { ACCOUNT_TABS } from "../accountNav";
import { onePageRail, ONE_PAGE_SECTIONS, sectionForPath } from "./sections";

describe("one-page account sections", () => {
  it("every classic tab path lands on a section, so old links keep working", () => {
    for (const tab of Object.values(ACCOUNT_TABS).flat()) {
      expect(sectionForPath(tab.to), tab.to).not.toBeNull();
    }
    expect(sectionForPath("/app/account/security")).toBe("password");
  });

  it("holds the Classic layout switch in its own section so either layout can turn it back", () => {
    expect(ONE_PAGE_SECTIONS.map((s) => s.id)).toContain("appearance");
    expect(sectionForPath("/app/account/appearance")).toBe("appearance");
  });

  it("puts Region after Stream quality, on its own path", () => {
    const ids = ONE_PAGE_SECTIONS.map((s) => s.id);
    expect(ids.indexOf("region")).toBe(ids.indexOf("streaming") + 1);
    expect(sectionForPath("/app/account/region")).toBe("region");
  });

  it("the rail lights the profile row on the bare account path", () => {
    const profile = onePageRail()[0].items.find((i) => i.id === "profile");
    expect(profile?.match?.("/app/account")).toBe(true);
  });
});
