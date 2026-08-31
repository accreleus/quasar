// The editor's one-draft-one-save contract: what a save sends, and when the
// Save button is even reachable. The PATCH shape is the load-bearing half —
// sending an unchanged `launchable_profile_ids` or dropping an unknown
// runtime_spec key are both real regressions this pins.

import { describe, expect, it } from "vitest";
import type { AdminApp, LaunchProfile } from "../../../../api/types";
import {
  createBody,
  draftFromApp,
  isDirty,
  patchBody,
  tabForError,
  validateDraft,
  withLaunchProfile,
  withLibraryProvider,
  withProfilePolicy,
} from "./appDraft";

function app(over: Partial<AdminApp> = {}): AdminApp {
  return {
    id: "app-1",
    name: "Steam",
    description: "The client",
    cover_url: null,
    hero_url: null,
    kind: "launcher",
    parent_app_id: null,
    external_source: "",
    external_id: "",
    origin: "manual",
    library_provider: "steam",
    library_discovery_suspended: false,
    favourite: false,
    sessions_30d: 12,
    default_width: 1920,
    default_height: 1080,
    default_fps: 60,
    default_bitrate_kbps: 8000,
    default_profile_id: null,
    profile_policy: "inherit",
    enabled: true,
    default_vram_mb: 0,
    default_encode_slots: 1,
    runtime_spec: {
      image: "quasar-steam:latest",
      args: [],
      env: { HOME: "/home/quasar" },
      mounts: [],
      gpu: true,
      no_new_privileges: false,
    },
    managed_home: true,
    home_container_path: "/home/quasar",
    runtime_preset_id: null,
    launchable_profile_ids: [],
    ...over,
  } as AdminApp;
}

function profile(over: Partial<LaunchProfile> = {}): LaunchProfile {
  return {
    id: "lp-1",
    display_name: "Adaptive 1440p",
    description: "",
    visibility: "user",
    sort_order: 1,
    rungs: [
      {
        position: 1,
        stream_profile: {
          id: "sp-1",
          display_name: "HEVC 1440p60",
          codec: "hevc",
          width: 2560,
          height: 1440,
          fps: 60,
          nominal_bitrate_kbps: 24000,
        },
      },
    ],
    ...over,
  } as LaunchProfile;
}

describe("draftFromApp", () => {
  it("round-trips an app, keeping runtime_spec keys the form does not edit", () => {
    const draft = draftFromApp(app());
    expect(draft.image).toBe("quasar-steam:latest");
    expect(draft.env).toEqual([["HOME", "/home/quasar"]]);
    expect(draft.specExtras).toEqual({ no_new_privileges: false });
    expect(patchBody(app(), draft)).toEqual({});
  });

  it("defaults a new app rather than inventing an empty one", () => {
    const draft = draftFromApp(null);
    expect(draft.kind).toBe("game");
    expect(draft.profilePolicy).toBe("inherit");
    expect(draft.containerPath).toBe("/home/quasar");
    expect(draft.width).toBe(1280);
  });
});

describe("validateDraft", () => {
  it("requires a name", () => {
    expect(validateDraft({ ...draftFromApp(app()), name: "  " }).name).toBeTruthy();
  });

  it("requires an image, unless a preset or a parent tile supplies one", () => {
    const bare = { ...draftFromApp(app()), image: "" };
    expect(validateDraft(bare).image).toBeTruthy();
    expect(validateDraft({ ...bare, runtimePresetId: "preset-1" }).image).toBeUndefined();
    expect(validateDraft(bare, { hasParent: true }).image).toBeUndefined();
  });

  it("rejects an env key that is not a shell identifier, and blank rows", () => {
    const draft = {
      ...draftFromApp(app()),
      env: [["ok", "1"], ["9bad", "2"], ["", "3"]] as [string, string][],
      args: [" "],
      mounts: [""],
    };
    const errs = validateDraft(draft);
    expect(errs.env_key_0).toBeUndefined();
    expect(errs.env_key_1).toMatch(/Invalid key/);
    expect(errs.env_key_2).toMatch(/empty/);
    expect(errs.arg_0).toBeTruthy();
    expect(errs.mount_0).toBeTruthy();
  });

  it("needs a launch profile once the policy pins one", () => {
    const draft = { ...draftFromApp(app()), profilePolicy: "prefer" as const, defaultProfileId: "" };
    expect(validateDraft(draft).defaultProfileId).toBeTruthy();
  });

  // "Only these" with nothing ticked serialises to [], which the contract reads
  // as unrestricted: the inverse of the operator's intent.
  it("refuses an empty allow-list under Only these", () => {
    const draft = { ...draftFromApp(app()), restrictLaunchable: true, launchableIds: [] };
    expect(validateDraft(draft).launchable).toBeTruthy();
  });

  it("routes each error to the tab that shows the field", () => {
    expect(tabForError("name")).toBe("identity");
    expect(tabForError("launchable")).toBe("quality");
    expect(tabForError("defaultProfileId")).toBe("quality");
    expect(tabForError("env_key_2")).toBe("runtime");
  });
});

describe("dirty tracking", () => {
  it("is clean for an untouched app and dirty after any tab edits", () => {
    const base = app();
    const draft = draftFromApp(base);
    expect(isDirty(base, draft)).toBe(false);
    expect(isDirty(base, { ...draft, name: "Steam Deck" })).toBe(true);
    expect(isDirty(base, { ...draft, managedHome: false })).toBe(true);
    expect(isDirty(base, { ...draft, args: ["-silent"] })).toBe(true);
  });

  it("treats a new app as dirty only once something is typed", () => {
    expect(isDirty(null, draftFromApp(null))).toBe(false);
    expect(isDirty(null, { ...draftFromApp(null), name: "Blender" })).toBe(true);
  });
});

describe("patchBody", () => {
  it("sends only the keys that changed, across tabs, in one body", () => {
    const base = app();
    const draft = {
      ...draftFromApp(base),
      name: "Steam Beta",
      args: ["-silent"],
      managedHome: false,
    };
    expect(patchBody(base, draft)).toEqual({
      name: "Steam Beta",
      managed_home: false,
      runtime_spec: {
        no_new_privileges: false,
        image: "quasar-steam:latest",
        args: ["-silent"],
        env: { HOME: "/home/quasar" },
        mounts: [],
        gpu: true,
      },
    });
  });

  it("omits an unchanged allow-list and sends one that emptied", () => {
    const base = app({ launchable_profile_ids: ["lp-1"], profile_policy: "prefer", default_profile_id: "lp-2" });
    const draft = draftFromApp(base);
    expect(patchBody(base, draft).launchable_profile_ids).toBeUndefined();
    expect(patchBody(base, { ...draft, restrictLaunchable: false }).launchable_profile_ids).toEqual([]);
  });

  it("keeps the pinned app default in the list, so unticking the rest cannot invert the intent", () => {
    const base = app({ profile_policy: "prefer", default_profile_id: "lp-1" });
    const draft = { ...draftFromApp(base), restrictLaunchable: true, launchableIds: [] };
    expect(patchBody(base, draft).launchable_profile_ids).toEqual(["lp-1"]);
  });

  it("ignores a key-order difference in runtime_spec rather than sending a no-op", () => {
    const base = app({ runtime_spec: { gpu: true, env: { HOME: "/home/quasar" }, image: "quasar-steam:latest", args: [], mounts: [], no_new_privileges: false } });
    expect(patchBody(base, draftFromApp(base))).toEqual({});
  });

  // The draft normalises where the row is empty; opening an app must not read
  // as an edit, and a save must not write those normalisations back.
  it("opens clean for a derived tile with an empty runtime_spec and no home path", () => {
    const base = app({ parent_app_id: "app-steam", runtime_spec: {}, home_container_path: "" });
    expect(patchBody(base, draftFromApp(base))).toEqual({});
    expect(isDirty(base, draftFromApp(base))).toBe(false);
  });

  it("sends an empty string description, never omitting a cleared field", () => {
    const base = app();
    expect(patchBody(base, { ...draftFromApp(base), description: "" })).toEqual({ description: "" });
  });
});

describe("createBody", () => {
  it("always carries the fields a create needs, including an unrestricted list", () => {
    const draft = { ...draftFromApp(null), name: " Blender ", image: " ghcr.io/quasar/blender:4.2 " };
    expect(createBody(draft)).toEqual({
      name: "Blender",
      description: "",
      kind: "game",
      library_provider: "",
      default_width: 1280,
      default_height: 720,
      default_fps: 60,
      default_bitrate_kbps: 6000,
      default_profile_id: null,
      profile_policy: "inherit",
      runtime_spec: {
        image: "ghcr.io/quasar/blender:4.2",
        args: [],
        env: {},
        mounts: [],
        gpu: false,
      },
      managed_home: false,
      home_container_path: "/home/quasar",
      runtime_preset_id: null,
      launchable_profile_ids: [],
    });
  });
});

describe("the derived edits", () => {
  it("suggests Launcher when Steam is picked, and never overrides an explicit kind", () => {
    const draft = draftFromApp(null);
    expect(withLibraryProvider(draft, "steam").kind).toBe("launcher");
    expect(withLibraryProvider({ ...draft, kind: "desktop" }, "steam").kind).toBe("desktop");
  });

  it("writes the top rung's geometry through when a launch profile is picked", () => {
    const next = withLaunchProfile(draftFromApp(null), profile());
    expect(next.defaultProfileId).toBe("lp-1");
    expect([next.width, next.height, next.fps, next.bitrateKbps]).toEqual([2560, 1440, 60, 24000]);
  });

  it("clears the pinned profile on inherit, and picks the first one otherwise", () => {
    const pinned = withLaunchProfile(draftFromApp(null), profile());
    expect(withProfilePolicy(pinned, "inherit", [profile()]).defaultProfileId).toBe("");
    expect(withProfilePolicy(draftFromApp(null), "force", [profile()]).defaultProfileId).toBe("lp-1");
  });
});
