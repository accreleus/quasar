import { render, screen } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { OverlayPreview } from "./OverlayPreview";
import {
  DEFAULT_OVERLAY_PREFERENCES,
  type SessionOverlayPreferences,
} from "../../../settings/overlayPreferences";

/** A module-level mutable `prefs` the mocked hook reads fresh on every render,
 *  so a test can change it and either re-render or render again. */
let prefs: SessionOverlayPreferences = DEFAULT_OVERLAY_PREFERENCES;
vi.mock("../../../settings/OverlayPreferencesContext", () => ({
  useOverlayPreferences: () => ({ prefs, loaded: true, save: async () => {}, error: null }),
}));

beforeEach(() => {
  prefs = DEFAULT_OVERLAY_PREFERENCES;
});

const ACTION_LABELS = ["Capture input", "Microphone", "Fullscreen", "Exit session"];

describe("OverlayPreview content", () => {
  it("renders the real HUD (not a redrawn copy) with a static, plausible snapshot", () => {
    const { container } = render(<OverlayPreview />);
    expect(container.querySelector(".hud")).toBeTruthy();
    // The bar's own telemetry child rendered these from PREVIEW_SNAPSHOT —
    // proof the embedded component actually received and displayed data,
    // not just mounted empty.
    expect(screen.getByText("Nebula Raceway")).toBeTruthy();
    expect(screen.getByText("60")).toBeTruthy();
    expect(screen.getByText("H.264")).toBeTruthy();
  });

  it("shows every bar control so the settings being previewed are visible", () => {
    render(<OverlayPreview />);
    for (const name of ACTION_LABELS) {
      expect(screen.getByLabelText(name)).toBeTruthy();
    }
  });

  it("labels the frame as a preview", () => {
    render(<OverlayPreview />);
    expect(screen.getByText("Live preview")).toBeTruthy();
  });
});

describe("OverlayPreview inert controls", () => {
  it("keeps every action button out of the tab order", () => {
    render(<OverlayPreview />);
    for (const name of ACTION_LABELS) {
      expect(screen.getByLabelText(name).getAttribute("tabindex")).toBe("-1");
    }
  });

  it("hides the whole replica from the accessibility tree, leaving only the caption", () => {
    const { container } = render(<OverlayPreview />);
    const stage = container.querySelector(".ovprev-stage");
    expect(stage?.getAttribute("aria-hidden")).toBe("true");
  });

  it("re-sweeps tabindex onto an action that appears after a preference change", () => {
    prefs = {
      ...DEFAULT_OVERLAY_PREFERENCES,
      stripItems: { ...DEFAULT_OVERLAY_PREFERENCES.stripItems, mic: false },
    };
    const { rerender } = render(<OverlayPreview />);
    expect(screen.queryByLabelText("Microphone")).toBeNull();

    prefs = {
      ...DEFAULT_OVERLAY_PREFERENCES,
      stripItems: { ...DEFAULT_OVERLAY_PREFERENCES.stripItems, mic: true },
    };
    rerender(<OverlayPreview />);
    expect(screen.getByLabelText("Microphone").getAttribute("tabindex")).toBe("-1");
  });
});

describe("OverlayPreview auto-hide preference", () => {
  it("never_visible renders an explanatory placeholder, not an empty frame", () => {
    prefs = { ...DEFAULT_OVERLAY_PREFERENCES, stripAutoHide: "never_visible" };
    const { container } = render(<OverlayPreview />);
    expect(container.querySelector(".hud")).toBeNull();
    // The stand-in frame itself still renders (it is not just blank)...
    expect(container.querySelector(".ovprev-stage")).toBeTruthy();
    // ...carrying real explanatory text, not a faked-visible pill.
    expect(screen.getByText(/with the overlay off/i)).toBeTruthy();
    expect(screen.getByText(/microphone indicator/i)).toBeTruthy();
  });

  it("the placeholder is not hidden from assistive tech (it is real information)", () => {
    prefs = { ...DEFAULT_OVERLAY_PREFERENCES, stripAutoHide: "never_visible" };
    const { container } = render(<OverlayPreview />);
    expect(container.querySelector(".ovprev-stage")?.getAttribute("aria-hidden")).toBeNull();
  });

  it("shows the pill under always_visible and on_capture", () => {
    for (const mode of ["always_visible", "on_capture"] as const) {
      prefs = { ...DEFAULT_OVERLAY_PREFERENCES, stripAutoHide: mode };
      const { container, unmount } = render(<OverlayPreview />);
      expect(container.querySelector(".hud")).toBeTruthy();
      unmount();
    }
  });
});

describe("OverlayPreview position", () => {
  it("reflects every dock the preference offers", () => {
    for (const [pos, axis] of [
      ["bottom", "h"],
      ["top", "h"],
      ["left", "v"],
      ["right", "v"],
    ] as const) {
      prefs = { ...DEFAULT_OVERLAY_PREFERENCES, stripPosition: pos };
      const { container, unmount } = render(<OverlayPreview />);
      const root = container.querySelector(".hud-root");
      expect(root?.getAttribute("data-pos")).toBe(pos);
      expect(root?.getAttribute("data-axis")).toBe(axis);
      unmount();
    }
  });
});
