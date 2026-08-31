// The launch-options overlay's own contract (v3 handoff §B "Launch options").
// The rules are pinned in launchOptionRules.test.ts and the wiring in
// DetailBand.*.test.tsx; this is the panel: its head, its three radio columns,
// the row anatomy, and the foot.

import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, within } from "@testing-library/react";
import { LaunchOptions } from "./LaunchOptions";
import type { OptionColumns, Verdict } from "./launchOptionRules";

const COLUMNS: OptionColumns = {
  codec: [
    { value: "auto", label: "Auto", enabled: true, selected: true },
    { value: "h264", label: "H.264", enabled: true, selected: false },
    { value: "av1", label: "AV1", enabled: true, selected: false },
  ],
  fps: [
    { value: 60, label: "60 fps", sub: "12 Mbps at 1440p", enabled: true, selected: true },
    {
      value: 120,
      label: "120 fps",
      sub: "Not available on this device",
      enabled: false,
      title: "Not available at 120 fps on this device",
      selected: false,
    },
  ],
  resolution: [
    {
      value: 2160,
      label: "4K",
      sub: "3840×2160 · 18 Mbps at 60 fps",
      enabled: true,
      selected: false,
      tags: ["risky"],
      why: "Your connection has little room to spare here.",
    },
    {
      value: 1440,
      label: "1440p",
      sub: "2560×1440 · 12 Mbps at 60 fps",
      enabled: true,
      selected: true,
      tags: ["recommended"],
    },
    {
      value: 1080,
      label: "1080p",
      sub: "1920×1080 · not available at 60 fps",
      enabled: false,
      title: "Not available at 1080p on this device",
      selected: false,
    },
  ],
  codecHint: "Your host picks the codec, best first. Here it lands on AV1.",
  fpsHint: "A frame rate this codec cannot reach is shown but cannot be picked.",
};

const SPEC = {
  resolution: "2560×1440",
  fps: "60 fps",
  bitrate: "12 Mbps",
  codec: "Auto · AV1",
};

const OK: Verdict = { tone: "ok", text: "Recommended for this device." };

function renderPanel(over: Partial<Parameters<typeof LaunchOptions>[0]> = {}) {
  const props = {
    id: "lo-a1",
    open: true,
    appName: "Portal 2",
    columns: COLUMNS,
    spec: SPEC,
    verdict: OK,
    launching: false,
    waitingForSlot: false,
    onSelectCodec: vi.fn(),
    onSelectFps: vi.fn(),
    onSelectHeight: vi.fn(),
    onCancel: vi.fn(),
    onPlay: vi.fn(),
    ...over,
  };
  return { ...render(<LaunchOptions {...props} />), props };
}

const group = (name: string) => screen.getByRole("radiogroup", { name });

describe("LaunchOptions", () => {
  it("heads the panel with the app and the live spec of the draft", () => {
    renderPanel();
    expect(document.querySelector(".qp-eyebrow")?.textContent).toBe("Launch options");
    expect(document.querySelector(".qp-game")?.textContent).toBe("Portal 2");
    expect(document.querySelector(".qp-spec")?.textContent).toContain("2560×1440");
    expect(document.querySelector(".qp-spec")?.textContent).toContain("Auto · AV1");
    expect(document.querySelector(".lo")).toHaveClass("show");
  });

  it("renders three radiogroups in the mock's order", () => {
    renderPanel();
    expect(
      Array.from(document.querySelectorAll(".qp-col")).map((c) => c.getAttribute("aria-label")),
    ).toEqual(["Codec", "Frame rate", "Resolution"]);
  });

  it("gives a row its label, sub, tags and check", () => {
    renderPanel();
    const rows = within(group("Resolution")).getAllByRole("radio");
    expect(rows[0].querySelector(".qr-label")?.textContent).toBe("4K");
    expect(rows[0].querySelector(".qr-sub")?.textContent).toBe("3840×2160 · 18 Mbps at 60 fps");
    expect(rows[0].querySelector(".qp-tag.risky")?.textContent).toBe("Risky");
    // The risky reason is on the row, not only in a tooltip.
    expect(rows[0].querySelector(".qr-why")?.textContent).toMatch(/little room to spare/);
    expect(rows[0].querySelector(".qp-tag")?.getAttribute("title")).toMatch(/little room to spare/);
    expect(rows[1].getAttribute("aria-checked")).toBe("true");
    expect(rows[1].querySelector(".qp-tag")?.textContent).toBe("Recommended");
    expect(rows.every((r) => r.querySelector(".qp-check") !== null)).toBe(true);
  });

  it("shows a row it cannot offer, disabled, with the reason in its title", () => {
    renderPanel();
    const rows = within(group("Resolution")).getAllByRole("radio");
    expect(rows[2]).toBeDisabled();
    // On Auto the row names itself: there is no chosen codec to blame.
    expect(rows[2].getAttribute("title")).toBe("Not available at 1080p on this device");
    expect(within(group("Frame rate")).getAllByRole("radio")[1]).toBeDisabled();
  });

  it("hints each column once, and puts the codec caveat on the heading", () => {
    renderPanel();
    expect(within(group("Codec")).getByText(/Here it lands on AV1/)).toBeInTheDocument();
    expect(within(group("Frame rate")).getByText(/cannot be picked/)).toBeInTheDocument();
    expect(
      document.querySelector('[aria-label="Codec"] .qp-section')?.getAttribute("title"),
    ).toBe("Only codecs this device can decode are listed.");
  });

  it("leaves a column with nothing to explain without a hint", () => {
    const { fpsHint: _unused, ...noHint } = COLUMNS;
    renderPanel({ columns: noHint });
    expect(within(group("Frame rate")).queryByText(/cannot be picked/)).toBeNull();
    expect(group("Frame rate").querySelector(".seg-hint")).toBeNull();
  });

  it("reports a selection per column", () => {
    const { props } = renderPanel();
    fireEvent.click(within(group("Codec")).getByRole("radio", { name: /H\.264/ }));
    expect(props.onSelectCodec).toHaveBeenCalledWith("h264");
    fireEvent.click(within(group("Resolution")).getAllByRole("radio")[0]);
    expect(props.onSelectHeight).toHaveBeenCalledWith(2160);
  });

  it("moves and selects with the arrows, skipping rows that cannot be picked", () => {
    const { props } = renderPanel();
    const rows = within(group("Resolution")).getAllByRole("radio");
    // One tab stop per group: the selected row.
    expect(rows.map((r) => r.tabIndex)).toEqual([-1, 0, -1]);

    fireEvent.keyDown(rows[1], { key: "ArrowDown" });
    // 1080p is disabled, so Down from 1440p wraps past it to 4K.
    expect(props.onSelectHeight).toHaveBeenLastCalledWith(2160);
    fireEvent.keyDown(rows[1], { key: "ArrowUp" });
    expect(props.onSelectHeight).toHaveBeenLastCalledWith(2160);
  });

  it("carries the verdict and its tone in the foot", () => {
    const { unmount } = renderPanel({
      verdict: { tone: "risky", text: "This may wobble when the network dips." },
    });
    const v = document.querySelector(".qp-verdict")!;
    expect(v).toHaveClass("risky");
    expect(v.textContent).toContain("wobble");
    unmount();

    renderPanel();
    expect(document.querySelector(".qp-verdict")?.className).toBe("qp-verdict");
  });

  it("cancels from the foot and from the close control", () => {
    const { props } = renderPanel();
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    fireEvent.click(screen.getByRole("button", { name: "Close options" }));
    expect(props.onCancel).toHaveBeenCalledTimes(2);
  });

  it("plays now, unless the verdict says the combination is off", () => {
    const { props, unmount } = renderPanel();
    fireEvent.click(screen.getByRole("button", { name: /Play now/ }));
    expect(props.onPlay).toHaveBeenCalled();
    unmount();

    renderPanel({ verdict: { tone: "off", text: "4K is not available with H.264 at 60 fps." } });
    expect(screen.getByRole("button", { name: /Play now/ })).toBeDisabled();
  });

  it("says what it is doing while a launch is in flight", () => {
    const { unmount } = renderPanel({ launching: true });
    expect(screen.getByRole("button", { name: /Launching…/ })).toBeDisabled();
    unmount();

    renderPanel({ launching: true, waitingForSlot: true });
    expect(screen.getByRole("button", { name: /Waiting for a slot…/ })).toBeInTheDocument();
  });

  it("is present but not shown until it is opened", () => {
    renderPanel({ open: false });
    expect(document.querySelector(".lo")).not.toHaveClass("show");
  });
});
