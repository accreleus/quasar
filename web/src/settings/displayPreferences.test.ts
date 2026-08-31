import { afterEach, describe, expect, it } from "vitest";
import {
  loadDisplayPreferences,
  saveDisplayPreferences,
  type DisplayPreferences,
} from "./displayPreferences";

// jsdom provides localStorage; clear it between tests.
afterEach(() => {
  localStorage.clear();
});

describe("loadDisplayPreferences", () => {
  it("returns contain default when nothing is stored", () => {
    const prefs = loadDisplayPreferences();
    expect(prefs.scalingMode).toBe("contain");
  });

  it("returns saved preferences on round-trip", () => {
    const saved: DisplayPreferences = { scalingMode: "cover" };
    saveDisplayPreferences(saved);
    const loaded = loadDisplayPreferences();
    expect(loaded.scalingMode).toBe("cover");
  });

  it("round-trips all valid scaling modes", () => {
    const modes = ["contain", "cover", "stretch", "integer"] as const;
    for (const mode of modes) {
      saveDisplayPreferences({ scalingMode: mode });
      expect(loadDisplayPreferences().scalingMode).toBe(mode);
    }
  });

  it("falls back to contain when stored JSON is corrupt", () => {
    localStorage.setItem("quasar.display.preferences", "not-json{{{");
    const prefs = loadDisplayPreferences();
    expect(prefs.scalingMode).toBe("contain");
  });

  it("falls back to contain when scalingMode value is unknown", () => {
    localStorage.setItem(
      "quasar.display.preferences",
      JSON.stringify({ scalingMode: "bogus-mode" }),
    );
    const prefs = loadDisplayPreferences();
    expect(prefs.scalingMode).toBe("contain");
  });

  it("falls back to contain when stored object is missing scalingMode", () => {
    localStorage.setItem("quasar.display.preferences", JSON.stringify({}));
    const prefs = loadDisplayPreferences();
    expect(prefs.scalingMode).toBe("contain");
  });
});

describe("preferredMicDeviceId (microphone spec §3.4)", () => {
  it("is absent by default", () => {
    expect(loadDisplayPreferences().preferredMicDeviceId).toBeUndefined();
  });

  it("round-trips a device id", () => {
    saveDisplayPreferences({ scalingMode: "contain", preferredMicDeviceId: "dev-abc" });
    expect(loadDisplayPreferences().preferredMicDeviceId).toBe("dev-abc");
  });

  it("ignores a non-string stored value", () => {
    localStorage.setItem(
      "quasar.display.preferences",
      JSON.stringify({ scalingMode: "cover", preferredMicDeviceId: 42 }),
    );
    const prefs = loadDisplayPreferences();
    expect(prefs.preferredMicDeviceId).toBeUndefined();
    expect(prefs.scalingMode).toBe("cover");
  });

  it("ignores an empty-string stored value", () => {
    localStorage.setItem(
      "quasar.display.preferences",
      JSON.stringify({ scalingMode: "cover", preferredMicDeviceId: "" }),
    );
    expect(loadDisplayPreferences().preferredMicDeviceId).toBeUndefined();
  });

  it("survives a scaling-only save (the HUD writes only scalingMode)", () => {
    saveDisplayPreferences({ scalingMode: "contain", preferredMicDeviceId: "dev-abc" });
    saveDisplayPreferences({ scalingMode: "cover" });
    const prefs = loadDisplayPreferences();
    expect(prefs.scalingMode).toBe("cover");
    expect(prefs.preferredMicDeviceId).toBe("dev-abc");
  });
});

describe("saveDisplayPreferences", () => {
  it("persists to localStorage under the expected key", () => {
    saveDisplayPreferences({ scalingMode: "stretch" });
    const raw = localStorage.getItem("quasar.display.preferences");
    expect(raw).not.toBeNull();
    const parsed = JSON.parse(raw!) as DisplayPreferences;
    expect(parsed.scalingMode).toBe("stretch");
  });

  it("overwrites a previously saved value", () => {
    saveDisplayPreferences({ scalingMode: "cover" });
    saveDisplayPreferences({ scalingMode: "integer" });
    expect(loadDisplayPreferences().scalingMode).toBe("integer");
  });
});
