import { describe, expect, it } from "vitest";
import { bitrate } from "./bitrate";

describe("bitrate", () => {
  it("renders kbps as Mb/s with one decimal", () => {
    expect(bitrate(8000)).toBe("8.0 Mb/s");
    expect(bitrate(24_400)).toBe("24.4 Mb/s");
    expect(bitrate(0)).toBe("0.0 Mb/s");
  });

  it("shows the no-value glyph rather than a confident zero", () => {
    expect(bitrate(undefined)).toBe("—");
    expect(bitrate(null)).toBe("—");
    expect(bitrate(Number.NaN)).toBe("—");
  });
});
