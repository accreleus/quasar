import { describe, expect, it } from "vitest";
import { hudItems, hudItemsForPreset } from "./hudItems";
import { itemsForPreset } from "../../../settings/overlayPreferences";

describe("hudItems", () => {
  it("Full shows every readout and every control", () => {
    expect(hudItemsForPreset("full")).toEqual({
      signal: true,
      metrics: true,
      title: true,
      codec: true,
      hint: true,
      capture: true,
      mic: true,
      fullscreen: true,
      exit: true,
    });
  });

  it("Minimal keeps the signal and the controls it names, and nothing else", () => {
    const items = hudItemsForPreset("minimal");
    expect(items.signal).toBe(true);
    expect(items.title).toBe(false);
    expect(items.metrics).toBe(false);
    expect(items.codec).toBe(false);
    expect(items.hint).toBe(false);
    expect(items.capture).toBe(true);
    expect(items.mic).toBe(true);
    expect(items.exit).toBe(true);
    expect(items.fullscreen).toBe(false);
  });

  it("Metrics drops the signal and the title but keeps the numbers", () => {
    const items = hudItemsForPreset("metrics");
    expect(items.signal).toBe(false);
    expect(items.title).toBe(false);
    expect(items.metrics).toBe(true);
    expect(items.hint).toBe(true);
  });

  it("maps the contract's `identity` onto the bar title", () => {
    const items = hudItems({ ...itemsForPreset("full"), identity: false });
    expect(items.title).toBe(false);
    expect(items.metrics).toBe(true);
  });

  it("passes a custom item set straight through, key for key", () => {
    const custom = { ...itemsForPreset("full"), mic: false, fullscreen: false, signal: false };
    expect(hudItems(custom)).toMatchObject({
      signal: false,
      mic: false,
      fullscreen: false,
      capture: true,
      exit: true,
    });
  });
});
