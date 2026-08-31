/**
 * Render-resolution control — the decoupled render resolution ladder.
 *
 * The ladder is pure (`renderResolutionOptions`) so the rung arithmetic can be
 * asserted without a DOM; the component tests cover the select wiring only.
 */

import { describe, expect, it, vi } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";
import {
  RenderResolutionControl,
  renderResolutionNote,
  renderResolutionOptions,
} from "./RenderResolutionControl";

describe("renderResolutionOptions", () => {
  it("puts 'Match stream' first, labelled with the stream size", () => {
    const opts = renderResolutionOptions(1920, 1080);
    expect(opts[0]).toMatchObject({ w: 1920, h: 1080, label: "Match stream (1920×1080)" });
  });

  it("lists the same-aspect rungs at or below a 1920×1080 stream", () => {
    expect(renderResolutionOptions(1920, 1080).map((o) => o.label)).toEqual([
      "Match stream (1920×1080)",
      "1600×900",
      "1280×720",
      "960×540",
    ]);
  });

  it("includes 1920×1080 for a 2560×1440 stream", () => {
    const opts = renderResolutionOptions(2560, 1440);
    expect(opts.map((o) => o.label)).toEqual([
      "Match stream (2560×1440)",
      "1920×1080",
      "1600×900",
      "1280×720",
      "960×540",
    ]);
  });

  it("never offers a rung above the stream", () => {
    for (const opt of renderResolutionOptions(1280, 720)) {
      expect(opt.w).toBeLessThanOrEqual(1280);
      expect(opt.h).toBeLessThanOrEqual(720);
    }
  });

  it("drops rungs of a different aspect ratio", () => {
    // 1280×800 is 16:10 — no 16:9 rung shares its aspect, so only the stream
    // itself is offered. Offering a 16:9 rung here would letterbox the app
    // inside its own render surface.
    expect(renderResolutionOptions(1280, 800).map((o) => o.label)).toEqual([
      "Match stream (1280×800)",
    ]);
  });

  it("does not duplicate the stream size as a plain rung", () => {
    const opts = renderResolutionOptions(1280, 720);
    expect(opts.filter((o) => o.w === 1280 && o.h === 720)).toHaveLength(1);
  });

  it("offers only even dimensions (the contract rejects odd)", () => {
    for (const opt of renderResolutionOptions(3840, 2160)) {
      expect(opt.w % 2).toBe(0);
      expect(opt.h % 2).toBe(0);
    }
  });
});

describe("renderResolutionNote", () => {
  it("names the stream size that stays put, and warns that apps may not follow", () => {
    expect(renderResolutionNote(1920, 1080)).toBe(
      "Keeps the stream at 1920×1080. Desktops redraw at the new size; some launchers (Steam Big Picture) keep their launch resolution.",
    );
  });

  it("interpolates the actual stream size", () => {
    expect(renderResolutionNote(2560, 1440)).toContain("2560×1440");
  });
});

describe("RenderResolutionControl", () => {
  it("renders the ladder for a 1920×1080 stream", () => {
    render(
      <RenderResolutionControl
        streamWidth={1920}
        streamHeight={1080}
        value={{ w: 1920, h: 1080 }}
        onChange={vi.fn()}
      />,
    );
    expect(screen.getByRole("option", { name: "Match stream (1920×1080)" })).toBeInTheDocument();
    expect(screen.getByRole("option", { name: "1600×900" })).toBeInTheDocument();
    expect(screen.getByRole("option", { name: "1280×720" })).toBeInTheDocument();
    expect(screen.getByRole("option", { name: "960×540" })).toBeInTheDocument();
    expect(screen.getAllByRole("option")).toHaveLength(4);
  });

  it("selects the current value", () => {
    render(
      <RenderResolutionControl
        streamWidth={1920}
        streamHeight={1080}
        value={{ w: 1280, h: 720 }}
        onChange={vi.fn()}
      />,
    );
    expect((screen.getByRole("combobox") as HTMLSelectElement).value).toBe("1280x720");
  });

  it("calls onChange with the picked size", () => {
    const onChange = vi.fn();
    render(
      <RenderResolutionControl
        streamWidth={1920}
        streamHeight={1080}
        value={{ w: 1920, h: 1080 }}
        onChange={onChange}
      />,
    );
    fireEvent.change(screen.getByRole("combobox", { name: /render resolution/i }), {
      target: { value: "1280x720" },
    });
    expect(onChange).toHaveBeenCalledOnce();
    expect(onChange).toHaveBeenCalledWith({ w: 1280, h: 720 });
  });

  it("ignores a value that is not on the ladder", () => {
    const onChange = vi.fn();
    render(
      <RenderResolutionControl
        streamWidth={1920}
        streamHeight={1080}
        value={{ w: 1920, h: 1080 }}
        onChange={onChange}
      />,
    );
    fireEvent.change(screen.getByRole("combobox"), { target: { value: "999x999" } });
    expect(onChange).not.toHaveBeenCalled();
  });

  it("disables the select while a change is in flight", () => {
    render(
      <RenderResolutionControl
        streamWidth={1920}
        streamHeight={1080}
        value={{ w: 1920, h: 1080 }}
        onChange={vi.fn()}
        busy
      />,
    );
    expect(screen.getByRole("combobox")).toBeDisabled();
  });

  it("shows the current value even when it is off-ladder (a server-side clamp)", () => {
    render(
      <RenderResolutionControl
        streamWidth={1920}
        streamHeight={1080}
        value={{ w: 1024, h: 576 }}
        onChange={vi.fn()}
      />,
    );
    expect((screen.getByRole("combobox") as HTMLSelectElement).value).toBe("1024x576");
    expect(screen.getByRole("option", { name: "1024×576" })).toBeInTheDocument();
  });

  it("sorts an off-ladder value into place descending, below 'Match stream'", () => {
    render(
      <RenderResolutionControl
        streamWidth={1920}
        streamHeight={1080}
        value={{ w: 1024, h: 576 }}
        onChange={vi.fn()}
      />,
    );
    expect(screen.getAllByRole("option").map((o) => o.textContent)).toEqual([
      "Match stream (1920×1080)",
      "1600×900",
      "1280×720",
      "1024×576",
      "960×540",
    ]);
  });
});
