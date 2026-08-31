import { describe, expect, it } from "vitest";
import { isPresetInUse } from "./presetHelpers";

describe("isPresetInUse", () => {
  it("is false for an unused preset (delete stays enabled)", () => {
    expect(isPresetInUse([])).toBe(false);
  });

  it("is true once any app references the preset (delete disables)", () => {
    expect(isPresetInUse([{ id: "app-1", name: "Snow" }])).toBe(true);
  });

  it("stays true for multiple consumers", () => {
    expect(
      isPresetInUse([
        { id: "app-1", name: "Snow" },
        { id: "app-2", name: "Ball" },
      ]),
    ).toBe(true);
  });
});
