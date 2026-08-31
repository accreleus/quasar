import { describe, expect, it } from "vitest";
import { dominantInFlightState, HOST_STATE_COPY, hostsInFlight, isImageInFlight } from "./imageStatus";
import type { CatalogImage } from "../../../api/types";

function image(hosts: CatalogImage["hosts"]): CatalogImage {
  return {
    id: "x",
    display_name: "X",
    kind: "prebuilt",
    version: "1.0.0",
    installed: false,
    hosts,
  } as CatalogImage;
}

describe("HOST_STATE_COPY", () => {
  // First-run-experience §S4: "pulling" reads as "Downloading…", distinct
  // from "building"'s "Building…" — collapsing both into "Installing…" hid
  // whether a stalled install was a stuck download or a stuck build.
  it("labels pulling as Downloading… and building as Building…", () => {
    expect(HOST_STATE_COPY.pulling.label).toBe("Downloading…");
    expect(HOST_STATE_COPY.building.label).toBe("Building…");
  });
});

describe("dominantInFlightState", () => {
  it("returns null when nothing is in flight", () => {
    expect(dominantInFlightState(image([]))).toBeNull();
    expect(
      dominantInFlightState(image([{ host_id: "h1", state: "ready", version: null, error: null }])),
    ).toBeNull();
  });

  it("returns pulling when a host is pulling", () => {
    expect(
      dominantInFlightState(image([{ host_id: "h1", state: "pulling", version: null, error: null }])),
    ).toBe("pulling");
  });

  it("returns building when a host is building and none is pulling", () => {
    expect(
      dominantInFlightState(image([{ host_id: "h1", state: "building", version: null, error: null }])),
    ).toBe("building");
  });

  it("prefers pulling over building when both are present across hosts", () => {
    expect(
      dominantInFlightState(
        image([
          { host_id: "h1", state: "building", version: null, error: null },
          { host_id: "h2", state: "pulling", version: null, error: null },
        ]),
      ),
    ).toBe("pulling");
  });
});

describe("hostsInFlight / isImageInFlight", () => {
  it("agree with dominantInFlightState on whether anything is moving", () => {
    const img = image([{ host_id: "h1", state: "pulling", version: null, error: null }]);
    expect(hostsInFlight(img)).toBe(true);
    expect(isImageInFlight(img)).toBe(true);
    expect(dominantInFlightState(img)).not.toBeNull();
  });
});
