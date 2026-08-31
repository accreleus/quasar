// Which tabs exist, and the head/rail derivations beside them (handoff §A.10).

import { describe, expect, it } from "vitest";
import type { AdminApp, CatalogImage } from "../../../../api/types";
import {
  activeEditorTab,
  appSourceLabel,
  editorSubtitle,
  editorTabs,
  imagePresence,
} from "./editorTabs";

const ids = (opts: Parameters<typeof editorTabs>[0]) => editorTabs(opts).map((t) => t.id);

const base = { appId: "app-1", isProvider: false, grants: 0, suppressed: 0 };

function app(over: Partial<AdminApp> = {}): AdminApp {
  return {
    id: "app-1",
    name: "Cyberpunk 2077",
    kind: "game",
    external_source: "steam",
    external_id: "1091500",
    origin: "discovered",
    sessions_30d: 214,
    ...over,
  } as AdminApp;
}

describe("editorTabs", () => {
  it("gives a plain app five tabs, in the mock's order", () => {
    expect(ids(base)).toEqual(["identity", "artwork", "access", "quality", "runtime"]);
  });

  // Routes are keyed on the provider's app id, and a derived tile can never be
  // a provider (apps_derived_shape_ck).
  it("adds Library only for a provider app, and counts what is suppressed", () => {
    const tabs = editorTabs({ ...base, isProvider: true, suppressed: 4 });
    expect(tabs.map((t) => t.id)).toContain("library");
    expect(tabs.find((t) => t.id === "library")?.count).toBe(4);
    expect(editorTabs({ ...base, isProvider: true }).find((t) => t.id === "library")?.count).toBe(0);
  });

  it("drops the id-keyed tabs while creating, since they have no app to key on", () => {
    expect(ids({ ...base, appId: null, isProvider: true })).toEqual([
      "identity",
      "quality",
      "runtime",
    ]);
  });

  it("badges Access with the number of grants, and shows no badge at zero", () => {
    expect(editorTabs({ ...base, grants: 3 }).find((t) => t.id === "access")?.count).toBe(3);
    expect(editorTabs(base).find((t) => t.id === "access")?.count).toBeUndefined();
  });

  it("routes each tab under the app, with Identity on the bare path", () => {
    const tabs = editorTabs({ ...base, isProvider: true });
    expect(tabs.find((t) => t.id === "identity")?.to).toBe("/admin/library/apps/app-1");
    expect(tabs.find((t) => t.id === "library")?.to).toBe("/admin/library/apps/app-1/library");
    expect(editorTabs({ ...base, appId: null })[0].to).toBe("/admin/library/apps/new");
  });
});

describe("activeEditorTab", () => {
  it("defaults to Identity, and falls back there for a tab this app does not have", () => {
    const tabs = editorTabs(base);
    expect(activeEditorTab(tabs, undefined)).toBe("identity");
    expect(activeEditorTab(tabs, "runtime")).toBe("runtime");
    expect(activeEditorTab(tabs, "library")).toBe("identity");
    expect(activeEditorTab(tabs, "nonsense")).toBe("identity");
  });
});

describe("the head derivations", () => {
  it("reads kind, source and 30-day sessions into the sub-line", () => {
    expect(editorSubtitle(app())).toBe("Game · Steam · 214 sessions in the last 30 days");
    expect(editorSubtitle(app({ sessions_30d: 1 }))).toMatch(/1 session in the last 30 days$/);
  });

  it("calls a hand-made tile Manual", () => {
    expect(appSourceLabel(app({ external_source: "" }))).toBe("Manual");
  });
});

describe("imagePresence", () => {
  const image = (over: Partial<CatalogImage> = {}): CatalogImage =>
    ({
      id: "img-1",
      display_name: "Proton",
      kind: "prebuilt",
      version: "9.0",
      registry_ref: "ghcr.io/quasar/proton:9.0",
      installed: true,
      hosts: [
        { host_id: "h1", state: "ready" },
        { host_id: "h2", state: "ready" },
        { host_id: "h3", state: "absent" },
      ],
      ...over,
    }) as CatalogImage;

  it("counts the hosts that hold the app's image", () => {
    expect(
      imagePresence([image()], { image: "ghcr.io/quasar/proton:9.0", runtimePresetId: "" }),
    ).toEqual({ ready: 2, total: 3 });
  });

  it("falls back to the catalog image that installs the app's runtime preset", () => {
    expect(
      imagePresence([image({ registry_ref: null, runtime_preset_id: "preset-1" })], {
        image: "",
        runtimePresetId: "preset-1",
      }),
    ).toEqual({ ready: 2, total: 3 });
  });

  // The rail omits the fact rather than reporting somebody else's host count.
  it("is null when no catalog image is this app's", () => {
    expect(imagePresence([image()], { image: "ghcr.io/other:1", runtimePresetId: "" })).toBeNull();
    expect(imagePresence([], { image: "ghcr.io/quasar/proton:9.0", runtimePresetId: "" })).toBeNull();
    expect(
      imagePresence([image({ hosts: [] })], {
        image: "ghcr.io/quasar/proton:9.0",
        runtimePresetId: "",
      }),
    ).toBeNull();
  });
});
