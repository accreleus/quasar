// #647 review: only the full screen a launch entered is ever left again.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { enterLaunchFullscreen, leaveLaunchFullscreen } from "./launchFullscreen";

let fsEl: Element | null;
beforeEach(() => {
  fsEl = null;
  localStorage.clear();
  Object.defineProperty(document, "fullscreenElement", { configurable: true, get: () => fsEl });
  document.documentElement.requestFullscreen = vi.fn(() => {
    fsEl = document.documentElement;
    return Promise.resolve();
  });
  document.exitFullscreen = vi.fn(() => {
    fsEl = null;
    return Promise.resolve();
  });
});
afterEach(() => vi.restoreAllMocks());

const settle = () => new Promise((r) => setTimeout(r, 0));

describe("launch full screen", () => {
  it("leaves the full screen it entered", async () => {
    expect(enterLaunchFullscreen()).toBe(true);
    leaveLaunchFullscreen();
    await settle();
    expect(document.exitFullscreen).toHaveBeenCalledTimes(1);
  });

  it("leaves a full screen the user made alone", async () => {
    fsEl = document.body; // the user's own, from the HUD button
    enterLaunchFullscreen();
    leaveLaunchFullscreen();
    await settle();
    expect(document.documentElement.requestFullscreen).not.toHaveBeenCalled();
    expect(document.exitFullscreen).not.toHaveBeenCalled();
  });

  it("waits for a pending request, so a late grant is still left", async () => {
    let grant!: () => void;
    document.documentElement.requestFullscreen = vi.fn(
      () => new Promise<void>((r) => (grant = () => ((fsEl = document.documentElement), r()))),
    );
    enterLaunchFullscreen();
    leaveLaunchFullscreen();
    await settle();
    expect(document.exitFullscreen).not.toHaveBeenCalled();
    grant();
    await settle();
    expect(document.exitFullscreen).toHaveBeenCalledTimes(1);
  });

  it("leaves nothing twice", async () => {
    enterLaunchFullscreen();
    leaveLaunchFullscreen();
    leaveLaunchFullscreen();
    await settle();
    expect(document.exitFullscreen).toHaveBeenCalledTimes(1);
  });

  it("setting off: requests nothing", () => {
    localStorage.setItem("quasar.display.preferences", JSON.stringify({ launchImmersive: false }));
    expect(enterLaunchFullscreen()).toBe(false);
    expect(document.documentElement.requestFullscreen).not.toHaveBeenCalled();
  });
});
