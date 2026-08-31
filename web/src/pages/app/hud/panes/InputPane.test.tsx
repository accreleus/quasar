// Ported from SessionDrawerInput's half of SessionDrawer.test.tsx — the Esc /
// Keyboard-Lock wording and the connected-pad readout are the behaviours that
// moved into the HUD's Controller & input section, not the pane's chrome.

import { act, render, screen, fireEvent } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { InputPane, shortGamepadLabel } from "./InputPane";
import { EMPTY_SNAPSHOT, type TelemetrySnapshot } from "../../../../webrtc/telemetry";
import type { CaptureMetrics } from "../../../../input/capture";

function makeInputMetrics(overrides: Partial<CaptureMetrics> = {}): CaptureMetrics {
  return {
    pointerLocked: false,
    captured: false,
    pointerLockSupported: true,
    coalescedSupported: true,
    inputMsgPerSec: 0,
    coalescedSamplesPerSec: 0,
    channelBufferedAmount: 0,
    backpressureDetected: false,
    gamepadCount: 0,
    pads: [],
    gamepadSendPerSec: 0,
    mmSentPerSec: 0,
    inputTrace: false,
    ...overrides,
  };
}

const base = {
  register: () => () => {},
  channelOpen: true,
  inputCaptured: false,
  onGrab: vi.fn(),
  escReleases: false,
  escInsecureContext: false,
  pointerLockAvailable: true,
  touchLook: false,
  scalingMode: "contain" as const,
  onScalingChange: vi.fn(),
};

describe("InputPane capture", () => {
  it("offers capture when input is not held", () => {
    render(<InputPane {...base} />);
    expect(screen.getByRole("button", { name: /capture input/i })).toBeTruthy();
  });

  it("offers release when input is held", () => {
    render(<InputPane {...base} inputCaptured />);
    expect(screen.getByRole("button", { name: /release input/i })).toBeTruthy();
  });

  it("is disabled until the input DataChannel is open", () => {
    render(<InputPane {...base} channelOpen={false} />);
    expect(screen.getByRole("button", { name: /capture input/i })).toHaveProperty(
      "disabled",
      true,
    );
  });

  it("names the release chord so a captured player can get back out", () => {
    render(<InputPane {...base} />);
    expect(screen.getByText("Release capture")).toBeTruthy();
    expect(screen.getByText("Z")).toBeTruthy();
  });
});

describe("InputPane keys", () => {
  it("says Esc reaches the app when Quasar owns it", () => {
    render(<InputPane {...base} inputCaptured />);
    expect(screen.getByText(/sent to the app/i)).toBeTruthy();
  });

  it("says Esc releases capture when the browser owns it", () => {
    render(<InputPane {...base} inputCaptured escReleases />);
    expect(screen.getByText(/releases capture/i)).toBeTruthy();
  });

  it("explains the fullscreen case by default", () => {
    render(<InputPane {...base} />);
    expect(screen.getByText(/Fullscreen and a secure origin/i)).toBeTruthy();
  });

  it("explains the insecure-origin case without blaming the user", () => {
    render(<InputPane {...base} inputCaptured escReleases escInsecureContext />);
    expect(
      screen.getByText(/Keyboard Lock is unavailable on this insecure origin/i),
    ).toBeTruthy();
  });

  it("names the untrusted certificate — 'use HTTPS' is wrong advice on an origin that already is https", () => {
    render(<InputPane {...base} inputCaptured escReleases escCertUntrusted />);
    expect(screen.getByText(/certificate your device doesn’t trust/i)).toBeTruthy();
    expect(screen.getByText(/\/v1\/tls\/certificate\.pem/)).toBeTruthy();
    expect(screen.queryByText(/insecure origin/i)).toBeNull();
  });

  it("cert wording wins over the API-presence wording when both apply", () => {
    render(<InputPane {...base} inputCaptured escReleases escInsecureContext escCertUntrusted />);
    expect(screen.getByText(/certificate your device doesn’t trust/i)).toBeTruthy();
    expect(screen.queryByText(/insecure origin/i)).toBeNull();
  });

  it("explains an observed lock refusal even though the API reads as supported", () => {
    render(<InputPane {...base} inputCaptured escReleases escLockRefused />);
    expect(screen.getByText(/browser refused to hand Esc to the game/i)).toBeTruthy();
  });

  // A finger has no hover state and no tooltip, so drag-to-look is invisible
  // unless the pane writes it down. These rows are that writing-down.
  it("lists the touch gestures where they are the device's mouse", () => {
    render(<InputPane {...base} pointerLockAvailable={false} touchLook />);
    expect(screen.getByText(/drag on the picture/i)).toBeTruthy();
    expect(screen.getByText("Tap")).toBeTruthy();
    expect(screen.getByText("Press and hold")).toBeTruthy();
    expect(screen.getByText("Drag two fingers")).toBeTruthy();
  });

  it("does not offer gesture instructions a desktop cannot perform", () => {
    render(<InputPane {...base} />);
    expect(screen.queryByText("Drag two fingers")).toBeNull();
    expect(screen.getByText(/available/i)).toBeTruthy();
  });

  // Mouse look is available by finger on a touchscreen; the pre-touch copy
  // said it was simply unavailable, which would now be a lie.
  it("no longer claims mouse look is impossible on a touchscreen", () => {
    render(<InputPane {...base} pointerLockAvailable={false} touchLook />);
    expect(screen.queryByText(/not on this device/i)).toBeNull();
    expect(screen.getByText(/touch is your mouse here/i)).toBeTruthy();
  });

  it("still says so on a no-lock browser with no touchscreen", () => {
    render(<InputPane {...base} pointerLockAvailable={false} touchLook={false} />);
    expect(screen.getByText(/not on this device/i)).toBeTruthy();
    expect(screen.getByText(/no touchscreen/i)).toBeTruthy();
  });
});

describe("InputPane connected controllers", () => {
  function renderWithSnapshot(snap: TelemetrySnapshot) {
    const fns: ((s: TelemetrySnapshot) => void)[] = [];
    const result = render(
      <InputPane
        {...base}
        register={(fn) => {
          fns.push(fn);
          return () => {};
        }}
      />,
    );
    act(() => {
      fns.forEach((f) => f(snap));
    });
    return result;
  }

  it("keeps the zero-state note verbatim when no pad has been seen", () => {
    renderWithSnapshot({
      ...EMPTY_SNAPSHOT,
      inputMetrics: makeInputMetrics({ gamepadCount: 0, pads: [] }),
    });
    expect(
      screen.getByText(
        "No controller seen yet. Press a button on an idle pad — browsers only report a " +
          "gamepad after its first input.",
      ),
    ).toBeTruthy();
  });

  it("renders one row per pad, with a shortened name and the full id in title", () => {
    renderWithSnapshot({
      ...EMPTY_SNAPSHOT,
      inputMetrics: makeInputMetrics({
        gamepadCount: 1,
        pads: [
          { index: 0, id: "Xbox Wireless Controller (STANDARD GAMEPAD Vendor: 045e Product: 0b13)" },
        ],
      }),
    });
    expect(screen.queryByText(/No controller seen yet/)).toBeNull();
    const nameEl = screen.getByText("Xbox Wireless Controller");
    expect(nameEl.getAttribute("title")).toBe(
      "Xbox Wireless Controller (STANDARD GAMEPAD Vendor: 045e Product: 0b13)",
    );
    expect(screen.getByText("Slot 1")).toBeTruthy();
  });

  it("reports the browser's own slot numbers for a sparse pad list", () => {
    renderWithSnapshot({
      ...EMPTY_SNAPSHOT,
      inputMetrics: makeInputMetrics({
        gamepadCount: 2,
        pads: [
          { index: 0, id: "Xbox Wireless Controller (STANDARD GAMEPAD Vendor: 045e Product: 0b13)" },
          { index: 2, id: "DualSense Wireless Controller (STANDARD GAMEPAD Vendor: 054c Product: 0ce6)" },
        ],
      }),
    });
    expect(screen.getByText("Xbox Wireless Controller")).toBeTruthy();
    expect(screen.getByText("DualSense Wireless Controller")).toBeTruthy();
    // Slot 1 / Slot 3 from the raw pad.index, not a re-numbered 1..count run.
    expect(screen.getByText("Slot 1")).toBeTruthy();
    expect(screen.getByText("Slot 3")).toBeTruthy();
    // The count and the number of rows must agree.
    expect(screen.getByText("2")).toBeTruthy();
    expect(screen.getAllByText(/^Slot \d+$/)).toHaveLength(2);
  });
});

describe("InputPane scaling", () => {
  it("offers every presentation mode, with the current one selected", () => {
    render(<InputPane {...base} scalingMode="cover" />);
    const fill = screen.getByRole("tab", { name: "Fill" });
    expect(fill.getAttribute("aria-selected")).toBe("true");
    expect(screen.getByRole("tab", { name: "Fit" }).getAttribute("aria-selected")).toBe("false");
    // Four segments, not the mock's three: `stretch` is a real mode today.
    expect(screen.getAllByRole("tab")).toHaveLength(4);
  });

  it("reports a pick to the owner", () => {
    const onScalingChange = vi.fn();
    render(<InputPane {...base} onScalingChange={onScalingChange} />);
    fireEvent.click(screen.getByRole("tab", { name: "1:1" }));
    expect(onScalingChange).toHaveBeenCalledWith("integer");
  });
});

describe("shortGamepadLabel", () => {
  it("strips the parenthesized STANDARD GAMEPAD/vendor/product suffix", () => {
    expect(
      shortGamepadLabel("Xbox Wireless Controller (STANDARD GAMEPAD Vendor: 045e Product: 0b13)"),
    ).toBe("Xbox Wireless Controller");
  });

  it("passes an id through unchanged when it has no parenthetical suffix", () => {
    expect(shortGamepadLabel("Generic USB Joystick")).toBe("Generic USB Joystick");
  });

  it("falls back to a placeholder for an empty id", () => {
    expect(shortGamepadLabel("")).toBe("Unknown controller");
  });

  it("caps very long labels instead of overflowing the column", () => {
    const long = "A".repeat(60);
    expect(shortGamepadLabel(long)).toHaveLength(40);
    expect(shortGamepadLabel(long).endsWith("…")).toBe(true);
  });
});
