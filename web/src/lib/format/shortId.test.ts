import { describe, expect, it } from "vitest";
import { shortId } from "./shortId";

describe("shortId", () => {
  it("takes the first eight characters of a uuid", () => {
    expect(shortId("624e14ee-c722-4a77-8851-e4afa486676e")).toBe("624e14ee");
  });

  it("leaves an id shorter than eight characters alone", () => {
    expect(shortId("app-bg3")).toBe("app-bg3");
  });

  it("renders nothing for a missing id, so a caller can drop it into JSX", () => {
    expect(shortId(null)).toBe("");
    expect(shortId(undefined)).toBe("");
    expect(shortId("")).toBe("");
  });
});
