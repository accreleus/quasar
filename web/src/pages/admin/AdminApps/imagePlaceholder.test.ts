import { describe, expect, it } from "vitest";
import { imageFieldPlaceholder } from "./imagePlaceholder";

describe("imageFieldPlaceholder", () => {
  it("shows the generic example when no preset is selected", () => {
    expect(imageFieldPlaceholder(false)).toBe("e.g. quasar-agent-dev:latest");
  });

  it("shows the inherit hint when a preset is selected", () => {
    expect(imageFieldPlaceholder(true)).toBe("inherited from preset");
  });
});
