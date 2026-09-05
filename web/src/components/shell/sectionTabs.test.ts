import { describe, expect, it } from "vitest";
import { FLEET_TABS, LIBRARY_TABS, STREAMING_TABS, PEOPLE_TABS, activeTab } from "./sectionTabs";

describe("section tabs", () => {
  it("fleet has hosts, storage, jobs", () => {
    expect(FLEET_TABS.map((t) => t.id)).toEqual(["hosts", "storage", "jobs", "releases"]);
  });

  it("library has apps, presets, images, sources", () => {
    expect(LIBRARY_TABS.map((t) => t.id)).toEqual(["apps", "presets", "images", "sources"]);
  });

  it("streaming has launch, profiles; people has users, invites", () => {
    expect(STREAMING_TABS.map((t) => t.id)).toEqual(["launch", "profiles"]);
    expect(PEOPLE_TABS.map((t) => t.id)).toEqual(["users", "invites"]);
  });

  it("every tab points inside its own section", () => {
    expect(FLEET_TABS.every((t) => t.to.startsWith("/admin/fleet/"))).toBe(true);
    expect(LIBRARY_TABS.every((t) => t.to.startsWith("/admin/library/"))).toBe(true);
    expect(STREAMING_TABS.every((t) => t.to.startsWith("/admin/streaming/"))).toBe(true);
    expect(PEOPLE_TABS.every((t) => t.to.startsWith("/admin/people/"))).toBe(true);
  });

  it("activeTab resolves nested paths", () => {
    expect(activeTab(FLEET_TABS, "/admin/fleet/hosts/abc/settings")).toBe("hosts");
    expect(activeTab(LIBRARY_TABS, "/admin/library/images/xyz")).toBe("images");
    expect(activeTab(LIBRARY_TABS, "/admin/library/apps?preset=x")).toBe("apps");
  });

  it("a segment that merely shares a prefix is not the tab", () => {
    // /admin/fleet/hostsomething must not light Hosts.
    expect(activeTab(FLEET_TABS, "/admin/fleet/hostsomething")).toBeUndefined();
    expect(activeTab(FLEET_TABS, "/admin/fleet")).toBeUndefined();
  });
});
