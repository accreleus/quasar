/**
 * Stream-resolution control — the SERVER-OWNED external rung list.
 *
 * The option arithmetic is pure (`streamResolutionOptions`,
 * `fallbackStreamRungs`) so it can be asserted without a DOM; the component
 * tests cover the select wiring and the unsupported-encoder state.
 */

import { describe, expect, it, vi } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";
import {
  AUTO_OPTION_VALUE,
  STREAM_RESOLUTION_NOTE,
  StreamResolutionControl,
  fallbackStreamRungs,
  streamResolutionOptions,
  streamRowLabel,
} from "./StreamResolutionControl";

const RUNGS_1080: [number, number][] = [
  [1920, 1080],
  [1600, 900],
  [1280, 720],
];

describe("streamResolutionOptions", () => {
  it("labels the launch size 'Launch (W×H)' and leaves the rest as sizes", () => {
    expect(streamResolutionOptions({ w: 1920, h: 1080 }, RUNGS_1080).map((o) => o.label)).toEqual([
      "Launch (1920×1080)",
      "1600×900",
      "1280×720",
    ]);
  });

  it("renders the server's rungs in the ORDER GIVEN, never re-sorted", () => {
    // Deliberately not descending: the server owns the order, this component
    // must not impose one of its own.
    const odd: [number, number][] = [
      [1280, 720],
      [1920, 1080],
      [1600, 900],
    ];
    expect(streamResolutionOptions({ w: 1920, h: 1080 }, odd).map((o) => [o.w, o.h])).toEqual(odd);
  });

  it("prepends the launch size when the rung list omits it", () => {
    // Returning to launch must always be reachable, whatever the family holds.
    const opts = streamResolutionOptions({ w: 1366, h: 768 }, [[1280, 720]]);
    expect(opts[0]).toMatchObject({ w: 1366, h: 768, label: "Launch (1366×768)" });
    expect(opts).toHaveLength(2);
  });
});

describe("fallbackStreamRungs", () => {
  it("mirrors the control plane's 16:9 family filtered to ≤ the launch size", () => {
    expect(fallbackStreamRungs({ w: 1920, h: 1080 })).toEqual([
      [1920, 1080],
      [1600, 900],
      [1280, 720],
    ]);
  });

  it("offers the higher rungs for a 4K launch", () => {
    expect(fallbackStreamRungs({ w: 3840, h: 2160 })[0]).toEqual([3840, 2160]);
    expect(fallbackStreamRungs({ w: 3840, h: 2160 })).toHaveLength(5);
  });

  it("offers launch only for a non-16:9 session (the server owns those families)", () => {
    expect(fallbackStreamRungs({ w: 1280, h: 800 })).toEqual([[1280, 800]]);
  });
});

describe("StreamResolutionControl", () => {
  const base = {
    launch: { w: 1920, h: 1080 },
    rungs: RUNGS_1080,
    value: { w: 1920, h: 1080 },
    onChange: vi.fn(),
  };

  it("renders the rungs, launch first, with Auto leading the list", () => {
    render(<StreamResolutionControl {...base} onChange={vi.fn()} />);
    expect(screen.getAllByRole("option").map((o) => o.textContent)).toEqual([
      "Auto (host decides)",
      "Launch (1920×1080)",
      "1600×900",
      "1280×720",
    ]);
  });

  it("selects the current external size", () => {
    render(<StreamResolutionControl {...base} value={{ w: 1280, h: 720 }} onChange={vi.fn()} />);
    expect((screen.getByRole("combobox") as HTMLSelectElement).value).toBe("1280x720");
  });

  it("calls onChange with the picked size", () => {
    const onChange = vi.fn();
    render(<StreamResolutionControl {...base} onChange={onChange} />);
    fireEvent.change(screen.getByRole("combobox", { name: /stream resolution/i }), {
      target: { value: "1280x720" },
    });
    expect(onChange).toHaveBeenCalledOnce();
    expect(onChange).toHaveBeenCalledWith({ w: 1280, h: 720 });
  });

  it("ignores a value that is not on the ladder", () => {
    const onChange = vi.fn();
    render(<StreamResolutionControl {...base} onChange={onChange} />);
    fireEvent.change(screen.getByRole("combobox"), { target: { value: "999x999" } });
    expect(onChange).not.toHaveBeenCalled();
  });

  it("disables the select while a change is in flight", () => {
    render(<StreamResolutionControl {...base} onChange={vi.fn()} busy />);
    expect(screen.getByRole("combobox")).toBeDisabled();
  });

  it("goes inert with the reason as its tooltip when the encoder can't resize", () => {
    render(
      <StreamResolutionControl
        {...base}
        onChange={vi.fn()}
        disabledReason="This session's encoder can't change stream resolution live."
      />,
    );
    const sel = screen.getByRole("combobox");
    expect(sel).toBeDisabled();
    expect(sel.getAttribute("title")).toMatch(/can't change stream resolution live/i);
  });

  it("shows an off-ladder current size rather than displaying one it isn't at", () => {
    render(<StreamResolutionControl {...base} value={{ w: 1024, h: 576 }} onChange={vi.fn()} />);
    expect((screen.getByRole("combobox") as HTMLSelectElement).value).toBe("1024x576");
    expect(screen.getByRole("option", { name: "1024×576" })).toBeInTheDocument();
  });

  it("uses the same select primitive as the other drawer rows", () => {
    const { container } = render(<StreamResolutionControl {...base} onChange={vi.fn()} />);
    expect(container.querySelector("select.session-scaling-select")).not.toBeNull();
  });
});

describe("STREAM_RESOLUTION_NOTE", () => {
  it("says what moves (the encode) and what does not (the app)", () => {
    expect(STREAM_RESOLUTION_NOTE).toBe(
      "Changes what is encoded and sent to you; the app keeps drawing at its own size.",
    );
  });
});

describe("auto vs pinned", () => {
  const LAUNCH = { w: 1920, h: 1080 };
  const RUNGS: [number, number][] = [
    [1920, 1080],
    [1600, 900],
    [1280, 720],
  ];

  it("labels an auto session with the Auto prefix and a pinned one without", () => {
    expect(streamRowLabel("auto", 1280, 720)).toBe("Auto · 1280×720");
    expect(streamRowLabel("pinned", 1280, 720)).toBe("1280×720");
    // Unknown owner (pre-ladder agent) reads as a plain size, never a false "Auto".
    expect(streamRowLabel(undefined, 1280, 720)).toBe("1280×720");
  });

  it("offers an Auto entry at the top of the picker when the ladder owns the size", () => {
    render(
      <StreamResolutionControl
        launch={LAUNCH}
        rungs={RUNGS}
        value={{ w: 1280, h: 720 }}
        owner="pinned"
        onChange={() => {}}
      />,
    );
    const options = screen.getAllByRole("option");
    expect(options[0]).toHaveTextContent("Auto");
    expect(options[0]).toHaveValue(AUTO_OPTION_VALUE);
  });

  it("picking Auto sends the LAUNCH size (the release)", () => {
    const onChange = vi.fn();
    render(
      <StreamResolutionControl
        launch={LAUNCH}
        rungs={RUNGS}
        value={{ w: 1280, h: 720 }}
        owner="pinned"
        onChange={onChange}
      />,
    );
    fireEvent.change(screen.getByLabelText("Stream resolution"), {
      target: { value: AUTO_OPTION_VALUE },
    });
    expect(onChange).toHaveBeenCalledWith({ w: 1920, h: 1080 });
  });

  it("selects the Auto entry when the ladder owns a NON-launch size", () => {
    render(
      <StreamResolutionControl
        launch={LAUNCH}
        rungs={RUNGS}
        value={{ w: 1280, h: 720 }}
        owner="auto"
        onChange={() => {}}
      />,
    );
    expect(screen.getByLabelText("Stream resolution")).toHaveValue(AUTO_OPTION_VALUE);
  });
});
