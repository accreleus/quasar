// Ported from SessionDrawer.test.tsx's display-row blocks. The rows moved into
// the shelf's fourth section; the behaviour (independent axes, the launch-size
// ceiling, the busy latch, the unsupported-encoder reason) did not.

import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { DisplayPane, SHOW_INTERFACE_SIZE_CONTROL } from "./DisplayPane";

const base = {
  streamSize: { w: 1920, h: 1080 },
  renderSize: { w: 1920, h: 1080 },
  onRenderSizeChange: vi.fn(),
  uiScale: 1,
  onUiScaleChange: vi.fn(),
  displayBusy: false,
};

const streamBase = {
  ...base,
  externalSize: null,
  onStreamSizeChange: vi.fn(),
  streamRungs: [
    [1920, 1080],
    [1600, 900],
    [1280, 720],
  ] as [number, number][],
};

describe("DisplayPane render resolution and interface size", () => {
  it("renders the render-resolution control", () => {
    render(<DisplayPane {...base} />);
    expect(screen.getByText("Render resolution")).toBeTruthy();
    expect(screen.getByRole("combobox", { name: /render resolution/i })).toBeTruthy();
  });

  it("renders the interface-size control by default", () => {
    // The gate is on since the KDE image shipped the kwin patch that adopts a
    // changed host fractional-scale hint (docs/reports/2026-08-19-kde-ui-scale/).
    expect(SHOW_INTERFACE_SIZE_CONTROL).toBe(true);
    render(<DisplayPane {...base} />);
    expect(screen.getByText("Interface size")).toBeTruthy();
    expect(screen.getByRole("combobox", { name: /interface size/i })).toBeTruthy();
    expect(screen.getByText("Scales the desktop UI (KDE); games ignore it.")).toBeTruthy();
  });

  it("hides the interface-size control when the gate is off", () => {
    // A capability switch on the host image, not a removal: an image whose
    // compositor cannot honour the hint turns the control off whole, caption
    // included.
    render(<DisplayPane {...base} showInterfaceSize={false} />);
    expect(screen.queryByText("Interface size")).toBeNull();
    expect(screen.queryByRole("combobox", { name: /interface size/i })).toBeNull();
    expect(screen.queryByText("Scales the desktop UI (KDE); games ignore it.")).toBeNull();
  });

  it("never calls the render-resolution control 'scaling' — that is the input pane's CSS control", () => {
    render(<DisplayPane {...base} />);
    expect(screen.queryByText(/^Scaling$/)).toBeNull();
  });

  it("names the stream size in the caption so it is clear the stream does not move", () => {
    render(<DisplayPane {...base} />);
    expect(
      screen.getByText(
        "Keeps the stream at 1920×1080. Desktops redraw at the new size; some launchers (Steam Big Picture) keep their launch resolution.",
      ),
    ).toBeTruthy();
  });

  it("says why there is nothing to change when the stream size is unknown", () => {
    render(<DisplayPane {...base} streamSize={null} />);
    expect(screen.queryByRole("combobox", { name: /render resolution/i })).toBeNull();
    expect(screen.queryByRole("combobox", { name: /interface size/i })).toBeNull();
    expect(screen.getByText(/did not report a stream size/i)).toBeTruthy();
  });

  it("forwards a render-resolution pick to the page", () => {
    const onRenderSizeChange = vi.fn();
    render(<DisplayPane {...base} onRenderSizeChange={onRenderSizeChange} />);
    fireEvent.change(screen.getByRole("combobox", { name: /render resolution/i }), {
      target: { value: "1280x720" },
    });
    expect(onRenderSizeChange).toHaveBeenCalledWith({ w: 1280, h: 720 });
  });

  it("forwards an interface-size pick to the page", () => {
    const onUiScaleChange = vi.fn();
    render(<DisplayPane {...base} onUiScaleChange={onUiScaleChange} />);
    fireEvent.change(screen.getByRole("combobox", { name: /interface size/i }), {
      target: { value: "1.50" },
    });
    expect(onUiScaleChange).toHaveBeenCalledWith(1.5);
  });

  it("goes inert together while a display PATCH is in flight", () => {
    render(<DisplayPane {...base} displayBusy />);
    expect(screen.getByRole("combobox", { name: /render resolution/i })).toBeDisabled();
    expect(screen.getByRole("combobox", { name: /interface size/i })).toBeDisabled();
    // jsdom applies no stylesheet, so the disabled AFFORDANCE is only assertable
    // through the class the `:disabled` rule hangs off.
    expect(screen.getByRole("combobox", { name: /render resolution/i })).toHaveClass(
      "session-scaling-select",
    );
  });
});

// Adaptive external resolution (§D6): the stream row, and the coupling that
// makes it more than a third select.
describe("DisplayPane stream resolution", () => {
  it("renders the control with its caption", () => {
    render(<DisplayPane {...streamBase} />);
    expect(screen.getByText("Stream resolution")).toBeTruthy();
    expect(screen.getByRole("combobox", { name: /stream resolution/i })).toBeTruthy();
    expect(
      screen.getByText(
        "Changes what is encoded and sent to you; the app keeps drawing at its own size.",
      ),
    ).toBeTruthy();
  });

  it("orders Stream resolution ABOVE Render resolution", () => {
    const { container } = render(<DisplayPane {...streamBase} />);
    const labels = Array.from(container.querySelectorAll(".col-lb")).map((el) => el.textContent);
    expect(labels.indexOf("Stream resolution")).toBeGreaterThanOrEqual(0);
    expect(labels.indexOf("Stream resolution")).toBeLessThan(labels.indexOf("Render resolution"));
  });

  it("selects the launch size while nothing has changed", () => {
    render(<DisplayPane {...streamBase} />);
    expect(
      (screen.getByRole("combobox", { name: /stream resolution/i }) as HTMLSelectElement).value,
    ).toBe("1920x1080");
  });

  it("forwards a pick to the page", () => {
    const onStreamSizeChange = vi.fn();
    render(<DisplayPane {...streamBase} onStreamSizeChange={onStreamSizeChange} />);
    fireEvent.change(screen.getByRole("combobox", { name: /stream resolution/i }), {
      target: { value: "1280x720" },
    });
    expect(onStreamSizeChange).toHaveBeenCalledWith({ w: 1280, h: 720 });
  });

  it("goes inert with the other controls while a PATCH is in flight", () => {
    render(<DisplayPane {...streamBase} displayBusy />);
    expect(screen.getByRole("combobox", { name: /stream resolution/i })).toBeDisabled();
  });

  it("offers no control, and says so, when there is only one rung", () => {
    render(<DisplayPane {...streamBase} streamRungs={[[1920, 1080]]} />);
    expect(screen.queryByRole("combobox", { name: /stream resolution/i })).toBeNull();
    expect(screen.getByText(/one stream resolution/i)).toBeTruthy();
    // The render control is independent of it and stays.
    expect(screen.getByRole("combobox", { name: /render resolution/i })).toBeTruthy();
  });

  it("keeps the control visible but inert, with the reason, when the encoder can't resize", () => {
    render(<DisplayPane {...streamBase} externalResizeSupported={false} />);
    expect(screen.getByRole("combobox", { name: /stream resolution/i })).toBeDisabled();
    expect(
      screen.getByText("This session's encoder can't change stream resolution live."),
    ).toBeTruthy();
  });

  it("shows the Auto chip when the poll carries external_owner: auto", () => {
    render(
      <DisplayPane {...streamBase} externalSize={{ w: 1280, h: 720 }} externalOwner="auto" />,
    );
    expect(screen.getByText("Auto · 1280×720")).toBeTruthy();
  });

  it("shows no ownership chip when the size is pinned, or when no owner is known", () => {
    const { unmount } = render(
      <DisplayPane {...streamBase} externalSize={{ w: 1280, h: 720 }} externalOwner="pinned" />,
    );
    expect(screen.queryByText(/^Auto ·/)).toBeNull();
    unmount();
    render(<DisplayPane {...streamBase} externalSize={{ w: 1280, h: 720 }} />);
    expect(screen.queryByText(/^Auto ·/)).toBeNull();
  });

  // Independent axes (2026-08-16 amendment): render and stream size no longer
  // constrain each other. The render ladder's ceiling is always the launch size.
  it("keeps the render ladder at the LAUNCH size regardless of the current external size", () => {
    render(<DisplayPane {...streamBase} externalSize={{ w: 1280, h: 720 }} />);
    const opts = Array.from(
      (screen.getByRole("combobox", { name: /render resolution/i }) as HTMLSelectElement).options,
    ).map((o) => o.textContent);
    expect(opts).toEqual(["Match stream (1920×1080)", "1600×900", "1280×720", "960×540"]);
  });

  it("does not force the render value down when the stream drops below it", () => {
    // A stream drop never clamps render: the encoder downsamples the render
    // framebuffer, and the app never sees a mode change.
    render(
      <DisplayPane
        {...streamBase}
        externalSize={{ w: 1280, h: 720 }}
        renderSize={{ w: 1600, h: 900 }}
      />,
    );
    expect(
      (screen.getByRole("combobox", { name: /render resolution/i }) as HTMLSelectElement).value,
    ).toBe("1600x900");
  });

  it("keeps the render caption pinned to the LAUNCH size, not the current external size", () => {
    render(<DisplayPane {...streamBase} externalSize={{ w: 1280, h: 720 }} />);
    expect(screen.getByText(/Keeps the stream at 1920×1080\./)).toBeTruthy();
  });

  it("shows nothing extra when no stream change is in flight", () => {
    render(<DisplayPane {...streamBase} />);
    expect(screen.queryByText("Adapting…")).toBeNull();
  });

  it("reports 'Adapting…' while a stream change is in flight, and announces it", () => {
    render(<DisplayPane {...streamBase} streamAdapting />);
    const note = screen.getByText("Adapting…");
    expect(note).toHaveClass("col-note");
    expect(note.getAttribute("role")).toBe("status");
    // The caption stays alongside it.
    expect(
      screen.getByText(
        "Changes what is encoded and sent to you; the app keeps drawing at its own size.",
      ),
    ).toBeTruthy();
  });

  it("does not report 'Adapting…' when the stream control itself is absent", () => {
    render(<DisplayPane {...streamBase} streamRungs={[[1920, 1080]]} streamAdapting />);
    expect(screen.queryByText("Adapting…")).toBeNull();
  });
});
