import { describe, expect, it } from "vitest";
import { dockLayout, hudShellPin } from "./hudDock";

describe("dockLayout", () => {
  it("horizontal docks centre on x and open to full width", () => {
    expect(dockLayout("bottom", false)).toMatchObject({
      axis: "h",
      open: false,
      radius: "12px 12px 0 0",
    });
    expect(dockLayout("top", true)).toMatchObject({
      axis: "h",
      open: true,
      width: "100vw",
      radius: "0",
    });
  });

  it("vertical docks open to full height and flip flex direction", () => {
    expect(dockLayout("left", true)).toMatchObject({
      axis: "v",
      direction: "row",
      height: "100vh",
    });
    expect(dockLayout("right", false)).toMatchObject({ axis: "v", direction: "row-reverse" });
  });

  it("chevron rotation per dock", () => {
    expect(dockLayout("left", false).chevron).toBe(90);
    expect(dockLayout("left", true).chevron).toBe(270);
    expect(dockLayout("right", true).chevron).toBe(90);
    expect(dockLayout("bottom", true).chevron).toBe(180);
  });

  it("rounds only the inward corners at rest, and nothing once it spans its edge", () => {
    expect(dockLayout("top", false).radius).toBe("0 0 12px 12px");
    expect(dockLayout("left", false).radius).toBe("0 12px 12px 0");
    expect(dockLayout("right", false).radius).toBe("12px 0 0 12px");
    expect(dockLayout("right", true).radius).toBe("0");
  });

  it("drives only the docked axis, so the other stays content-sized", () => {
    expect(dockLayout("bottom", true).height).toBe("auto");
    expect(dockLayout("left", true).width).toBe("auto");
    // Before the first measure() the cached custom property is absent; the
    // fallback must still be intrinsic, never a guessed pixel size.
    expect(dockLayout("bottom", false).width).toBe("var(--hud-w, auto)");
    expect(dockLayout("right", false).height).toBe("var(--hud-h, auto)");
  });
});

describe("hudShellPin", () => {
  it("pins the measured bar box plus the shell's borders, clamped to the viewport", () => {
    expect(hudShellPin({ width: 372.4, height: 36 })).toEqual({
      w: "min(375px, calc(100vw - 48px))",
      h: "min(38px, calc(100vh - 48px))",
    });
  });

  it("tracks the content, so a wider readout pins a wider pill", () => {
    // "0fps" -> "60fps" is one glyph. The derivation must move with it, or the
    // pill stays a glyph too narrow and the title ellipsises.
    expect(hudShellPin({ width: 360, height: 36 })!.w).toBe("min(362px, calc(100vw - 48px))");
    expect(hudShellPin({ width: 368, height: 36 })!.w).toBe("min(370px, calc(100vw - 48px))");
  });

  it("pins nothing for a degenerate box, leaving the intrinsic fallback", () => {
    expect(hudShellPin({ width: 0, height: 36 })).toBeNull();
    expect(hudShellPin({ width: 372, height: 0 })).toBeNull();
  });
});
