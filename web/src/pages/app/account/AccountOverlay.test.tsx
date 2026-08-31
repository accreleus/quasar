import { render, screen, fireEvent, waitFor, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { AccountOverlay } from "./AccountOverlay";
import {
  DEFAULT_OVERLAY_PREFERENCES,
  type SessionOverlayPreferences,
} from "../../../settings/overlayPreferences";

let prefs: SessionOverlayPreferences = DEFAULT_OVERLAY_PREFERENCES;
const save = vi.fn();
vi.mock("../../../settings/OverlayPreferencesContext", () => ({
  useOverlayPreferences: () => ({ prefs, loaded: true, save, error: null }),
}));

beforeEach(() => {
  prefs = DEFAULT_OVERLAY_PREFERENCES;
  save.mockReset().mockResolvedValue(undefined);
});

describe("AccountOverlay", () => {
  it("saves the whole preference object when a preset is picked", async () => {
    render(<AccountOverlay />);
    fireEvent.click(screen.getByRole("tab", { name: "Minimal" }));
    await waitFor(() =>
      expect(save).toHaveBeenCalledWith(
        expect.objectContaining({
          stripPreset: "minimal",
          stripItems: expect.objectContaining({ identity: false, metrics: false }),
        }),
      ),
    );
  });

  it("flips to Custom when an individual item is toggled", async () => {
    render(<AccountOverlay />);
    fireEvent.click(screen.getByLabelText("Codec"));
    await waitFor(() =>
      expect(save).toHaveBeenCalledWith(expect.objectContaining({ stripPreset: "custom" })),
    );
  });

  it("saves the position independently of the content", async () => {
    render(<AccountOverlay />);
    fireEvent.click(screen.getByRole("tab", { name: "Top" }));
    await waitFor(() =>
      expect(save).toHaveBeenCalledWith(expect.objectContaining({ stripPosition: "top" })),
    );
  });

  // The HUD docks to four edges (Task 14), so hiding two of them here would
  // make a setting unreachable from the only page that owns it.
  it("offers all four docks", async () => {
    render(<AccountOverlay />);
    for (const [label, value] of [
      ["Top", "top"],
      ["Bottom", "bottom"],
      ["Left", "left"],
      ["Right", "right"],
    ] as const) {
      const tab = screen.getByRole("tab", { name: label });
      expect(tab).toBeTruthy();
      fireEvent.click(tab);
      await waitFor(() =>
        expect(save).toHaveBeenCalledWith(expect.objectContaining({ stripPosition: value })),
      );
    }
  });

  it("offers the three auto-hide modes the contract carries, and saves a pick", async () => {
    render(<AccountOverlay />);
    const select = screen.getByLabelText("Auto-hide") as HTMLSelectElement;
    expect(Array.from(select.options).map((o) => o.textContent)).toEqual([
      "After 4 seconds",
      "Always visible",
      "Never show the overlay",
    ]);
    // The mock's "After 10 seconds" has no contract value behind it (spec §9).
    expect(screen.queryByText("After 10 seconds")).toBeNull();

    fireEvent.change(select, { target: { value: "never_visible" } });
    await waitFor(() =>
      expect(save).toHaveBeenCalledWith(
        expect.objectContaining({ stripAutoHide: "never_visible" }),
      ),
    );
  });

  it("groups the switches under Readouts and Controls", () => {
    render(<AccountOverlay />);
    expect(screen.getByText("Readouts")).toBeTruthy();
    expect(screen.getByText("Controls")).toBeTruthy();
    for (const label of [
      "Connection signal",
      "App name and quality",
      "Codec",
      "FPS, latency, bitrate",
      "Menu shortcut hint",
      "Microphone",
      "Fullscreen",
    ]) {
      expect(screen.getByRole("checkbox", { name: label })).toBeTruthy();
    }
  });

  // The mock draws Custom, so it is drawn — but a control that selects it
  // cannot say which items it would turn on, so it is never pickable.
  it("draws Custom but never lets it be picked", () => {
    render(<AccountOverlay />);
    const custom = screen.getByRole("tab", { name: "Custom" });
    expect(custom).toHaveAttribute("aria-selected", "false");
    expect((custom as HTMLButtonElement).disabled).toBe(true);
    fireEvent.click(custom);
    expect(save).not.toHaveBeenCalled();
  });

  it("shows Custom as the selected segment once the item set has been edited", () => {
    prefs = { ...DEFAULT_OVERLAY_PREFERENCES, stripPreset: "custom" };
    render(<AccountOverlay />);
    const custom = screen.getByRole("tab", { name: "Custom" });
    expect(custom).toHaveAttribute("aria-selected", "true");
    expect((custom as HTMLButtonElement).disabled).toBe(true);
    // A disabled segment cannot hold the tab stop, or the group would be
    // unreachable by Tab in exactly this state.
    expect(custom.getAttribute("tabindex")).toBe("-1");
    const group = within(screen.getByRole("tablist", { name: "Overlay content preset" }));
    expect(
      group
        .getAllByRole("tab")
        .filter((t) => t.getAttribute("tabindex") === "0")
        .map((t) => t.textContent),
    ).toEqual(["Full"]);
  });

  it("warns that hiding the strip leaves the mic dot as the only indicator", () => {
    prefs = { ...DEFAULT_OVERLAY_PREFERENCES, stripAutoHide: "never_visible" };
    render(<AccountOverlay />);
    // Scoped by id because the preview's own placeholder necessarily makes the
    // same point in its own words — but still assert the COPY, not just that
    // some element exists. The warning's whole value is what it says.
    expect(screen.getByTestId("strip-hide-hint").textContent).toMatch(/microphone indicator/i);
  });

  it("does not warn under the other auto-hide modes", () => {
    render(<AccountOverlay />);
    expect(screen.queryByTestId("strip-hide-hint")).toBeNull();
  });

  it("offers switches for the strip actions alongside the readouts", () => {
    render(<AccountOverlay />);
    // role: "checkbox" disambiguates the settings switch from the identically
    // labelled (aria-label="Capture input"/"Exit session") button the live
    // preview embeds below it (OverlayPreview.tsx renders the real
    // SessionStrip) — same accessible name, different control.
    expect(screen.getByRole("checkbox", { name: "Capture input" })).toBeTruthy();
    expect(screen.getByRole("checkbox", { name: "Exit session" })).toBeTruthy();
  });

  it("flips to Custom when the exit action is turned off", async () => {
    render(<AccountOverlay />);
    fireEvent.click(screen.getByRole("checkbox", { name: "Exit session" }));
    await waitFor(() =>
      expect(save).toHaveBeenCalledWith(
        expect.objectContaining({
          stripPreset: "custom",
          stripItems: expect.objectContaining({ exit: false }),
        }),
      ),
    );
  });
});
