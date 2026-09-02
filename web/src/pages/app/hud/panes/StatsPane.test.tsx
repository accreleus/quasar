// Ported from DiagPanel.test.tsx. The rows, the estimator labels and the
// verdict block are the deliverable: on 2026-08-22 a healthy 2560×1440@120
// session was investigated as an encoder fault because one unlabelled headline
// (fps from the mean interval) read 88-108. These tests are the guard.

import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { ComponentProps } from "react";
import { StatsPane, sparkPath } from "./StatsPane";
import { EMPTY_SNAPSHOT, type TelemetrySnapshot } from "../../../../webrtc/telemetry";
import type { CaptureMetrics } from "../../../../input/capture";
import { AuthContext, type AuthContextValue } from "../../../../auth/context";
import { ApiError } from "../../../../api/client";
import type { Verdict } from "../../../../api/verdict";

const getSessionVerdict = vi.hoisted(() => vi.fn());
vi.mock("../../../../api/verdict", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../../../api/verdict")>();
  return { ...actual, getSessionVerdict };
});

const ADVANCED_KEY = "quasar.diag.advancedOpen";

/** The verdict is read through useResource, which needs a token. */
const authValue = {
  status: "authenticated",
  user: null,
  token: "t0k3n",
  isAdmin: false,
  login: async () => {},
  claim: async () => {},
  logout: async () => {},
} as unknown as AuthContextValue;

function makeRegister() {
  const fns: ((s: TelemetrySnapshot) => void)[] = [];
  const register = (fn: (s: TelemetrySnapshot) => void) => {
    fns.push(fn);
    return () => {};
  };
  return { register, push: (s: TelemetrySnapshot) => fns.forEach((f) => f(s)) };
}

function renderPane(props: Partial<ComponentProps<typeof StatsPane>> = {}) {
  const { register, push } = makeRegister();
  const view = render(
    <AuthContext.Provider value={authValue}>
      <StatsPane register={register} {...props} />
    </AuthContext.Provider>,
  );
  return { ...view, push };
}

/** The Simple view is the default; the tables live behind "Detailed". */
function renderDetail(props: Partial<ComponentProps<typeof StatsPane>> = {}) {
  const view = renderPane(props);
  fireEvent.click(screen.getByRole("tab", { name: "Detailed" }));
  return view;
}

function rowLabels(): string[] {
  return Array.from(document.querySelectorAll("tr")).map((tr) =>
    (tr.querySelector("td")?.textContent ?? "").trim(),
  );
}

function verdictFixture(over: Partial<Verdict> = {}): Verdict {
  return {
    verdict: "nominal",
    evidence: [],
    reason:
      "No congestion, encoder-saturation, or presentation-judder signal over a 300 s window (300 host, 290 client samples).",
    window: { from_ms: 1_000_000, to_ms: 1_300_000, n_host: 300, n_client: 290 },
    clock: { quality: "measured", offset_ms: -3.2, uncertainty_ms: 1.8 },
    evidence_tier: "full",
    falsifiers: [
      { name: "encoder.fps", estimator: "p10", value: 60, op: ">=", threshold: 50, unit: "fps", n: 5, holds: true },
      { name: "client.present_long_frames", estimator: "max", value: 0, op: "==", threshold: 0, unit: "count", n: 5, holds: true },
    ],
    thresholds_version: "2026-08-23.2",
    ...over,
  } as Verdict;
}

/** A 120 fps client on a 120 Hz panel with a gentle beat — the 2026-08-22 shape. */
function beatingCadence(): TelemetrySnapshot["presentCadence"] {
  return {
    n: 118,
    medianMs: 1000 / 120,
    meanMs: 9.3,
    p95Ms: 16.7,
    maxMs: 16.7,
    sdMs: 1.9,
    fpsFromMedian: 120,
    fpsFromMean: 107.5,
    doubledFraction: 0.12,
    longFrames: 0,
    driftMs: 0,
    inherentBeat: true,
  };
}

const inputMetrics: CaptureMetrics = {
  pointerLocked: true,
  captured: true,
  pointerLockSupported: true,
  coalescedSupported: true,
  inputMsgPerSec: 124,
  coalescedSamplesPerSec: 212,
  channelBufferedAmount: 410,
  backpressureDetected: false,
  gamepadCount: 1,
  pads: [],
  gamepadSendPerSec: 60,
  mmSentPerSec: 96,
  inputTrace: false,
};

beforeEach(() => {
  localStorage.clear();
  getSessionVerdict.mockReset();
  getSessionVerdict.mockResolvedValue(verdictFixture());
});
afterEach(() => localStorage.clear());

describe("StatsPane simple view", () => {
  it("shows the four cards anyone acts on", () => {
    const { push } = renderPane();
    act(() => push({ ...EMPTY_SNAPSHOT, fps: 60, rttMs: 18, bitrateKbps: 8400, jbMs: 11.4 }));

    expect(screen.getByText("Frame rate")).toBeTruthy();
    expect(screen.getByText("Latency")).toBeTruthy();
    expect(screen.getByText("Bitrate")).toBeTruthy();
    expect(screen.getByText("Jitter buffer")).toBeTruthy();
    expect(screen.getByText("60")).toBeTruthy();
    expect(screen.getByText("18")).toBeTruthy();
    expect(screen.getByText("8.4")).toBeTruthy();
    expect(screen.getByText("11.4")).toBeTruthy();
  });

  it("draws a sparkline once samples have accumulated", () => {
    const { push } = renderPane();
    act(() => push({ ...EMPTY_SNAPSHOT, fps: 60, rttMs: 18, bitrateKbps: 8400, jbMs: 11.4 }));
    act(() => push({ ...EMPTY_SNAPSHOT, fps: 52, rttMs: 24, bitrateKbps: 7100, jbMs: 14.0 }));

    const spark = document.querySelector<SVGPathElement>('.stat-card[data-k="fps"] .spark');
    expect(spark?.getAttribute("d")).toMatch(/^M0\.00 .*L100\.00 /);
  });

  it("keeps the tables out of the DOM until Detailed is chosen", () => {
    renderPane({ tier: "1920×1080@60" });
    expect(screen.queryByText("1920×1080@60")).toBeNull();
    expect(screen.queryByText("msg/s")).toBeNull();
  });

  // #85 — the pill needs BOTH a real refresh gap and frames actually dropping.
  /** Push `n` windows, each reporting `dropped` frames, at a fixed display Hz. */
  const pushWindows = (
    push: (s: TelemetrySnapshot) => void,
    n: number,
    dropped: number,
    displayRefreshHz: number,
  ) => {
    for (let i = 0; i < n; i++) {
      act(() => push({ ...EMPTY_SNAPSHOT, displayRefreshHz, framesDropped: dropped }));
    }
  };

  it("flags a display that cannot present every streamed frame", () => {
    const { push } = renderPane({ tier: "1920×1080@60" });
    pushWindows(push, 3, 4, 50);
    expect(screen.getByText(/50 Hz display · can't show 60 fps/)).toBeTruthy();
  });

  it("shows no Hz flag when the display keeps up", () => {
    const { push } = renderPane({ tier: "1920×1080@60" });
    pushWindows(push, 3, 4, 60);
    expect(screen.queryByText(/Hz display/)).toBeNull();
  });

  // The operator's report: 2560×1440@120 on a fast display, 119 fps received,
  // drops/freezes 0/0 — and a pill insisting frames were being dropped.
  it("#85: does not flag a 120 fps stream while zero frames are dropping", () => {
    const { push } = renderPane({ tier: "2560×1440@120" });
    pushWindows(push, 5, 0, 61);
    expect(screen.queryByText(/Hz display/)).toBeNull();
  });

  it("#85: does not flag on a single dropped window", () => {
    const { push } = renderPane({ tier: "1920×1080@60" });
    pushWindows(push, 1, 4, 50);
    expect(screen.queryByText(/Hz display/)).toBeNull();
  });

  it("#85: a clean window resets the streak, so the flag clears", () => {
    const { push } = renderPane({ tier: "1920×1080@60" });
    pushWindows(push, 3, 4, 50);
    expect(screen.getByText(/Hz display/)).toBeTruthy();
    pushWindows(push, 1, 0, 50);
    expect(screen.queryByText(/Hz display/)).toBeNull();
  });

  it("#85: tolerates the estimator's own rounding (119 measured on a 120 tier)", () => {
    const { push } = renderPane({ tier: "2560×1440@120" });
    pushWindows(push, 5, 4, 119);
    expect(screen.queryByText(/Hz display/)).toBeNull();
  });
});

describe("sparkPath", () => {
  it("is empty with no samples, and flat-safe with identical ones", () => {
    expect(sparkPath([])).toBe("");
    // A flat series must not divide by zero; every point lands on one line.
    expect(sparkPath([5, 5, 5])).toBe("M0.00 27.00 L50.00 27.00 L100.00 27.00");
  });
});

describe("StatsPane detailed view", () => {
  it("shows the session facts and the stream-health rows", () => {
    const { push } = renderDetail({ tier: "1920×1080@60", resolvedCodec: "h264" });
    act(() => push({ ...EMPTY_SNAPSHOT, rttMs: 18, jbMs: 11.4, decodeMs: 2.6, inputMetrics }));

    expect(screen.getByText("1920×1080@60")).toBeTruthy();
    expect(screen.getByText("codec (server)")).toBeTruthy();
    expect(screen.getByText("rtt")).toBeTruthy();
    expect(screen.getByText("18.0ms")).toBeTruthy();
  });

  it("shows the input-pipeline table the mock puts fourth", () => {
    const { push } = renderDetail();
    act(() => push({ ...EMPTY_SNAPSHOT, inputMetrics }));

    expect(screen.getByText("input locked")).toBeTruthy();
    expect(screen.getByText("msg/s")).toBeTruthy();
    expect(screen.getByText("124")).toBeTruthy();
    expect(screen.getByText("mm/s")).toBeTruthy();
    expect(screen.getByText("coalesced/s")).toBeTruthy();
    expect(screen.getByText("gp send/s")).toBeTruthy();
    expect(screen.getByText("ch buffered")).toBeTruthy();
    expect(screen.getByText("backpressure")).toBeTruthy();
  });

  it("carries the elapsed session timer the v1 drawer header owned", () => {
    renderDetail({ startedAt: new Date(Date.now() - 65_000).toISOString() });
    expect(screen.getByText("elapsed")).toBeTruthy();
    expect(screen.getByText("1:05")).toBeTruthy();
  });

  it("names the estimator and the sample count on every derived reading", async () => {
    const { push } = renderDetail({ sessionId: "s-1" });
    await screen.findByText("nominal");
    act(() => push({ ...EMPTY_SNAPSHOT, presentCadence: beatingCadence(), displayRefreshHz: 120 }));

    const labels = rowLabels();
    expect(labels).toContain("fps (shown) · median n=118");
    expect(labels).toContain("present σ · mean 1 s n=118");
    expect(labels).toContain("decode · mean 1 s");
  });

  it("reads the median, and says so, on the 2026-08-22 shape", () => {
    const { push } = renderDetail();
    act(() => push({ ...EMPTY_SNAPSHOT, presentFps: 107.5, presentCadence: beatingCadence() }));
    // 120, not the 107 the mean would have shown.
    expect(screen.getByText("120")).toBeTruthy();
  });

  it("falls back to the mean and LABELS it when no median exists", () => {
    const { push } = renderDetail();
    const noMedian = { ...beatingCadence(), n: 3, fpsFromMedian: null, doubledFraction: null };
    act(() => push({ ...EMPTY_SNAPSHOT, presentFps: 58.4, presentCadence: noMedian }));

    expect(rowLabels()).toContain("fps (shown) · mean n=3");
    expect(screen.getByText("58")).toBeTruthy();
  });

  it("labels capture→display as a rolling median rather than this second's number", () => {
    const { push } = renderDetail();
    act(() =>
      push({
        ...EMPTY_SNAPSHOT,
        g2gMs: 46.1,
        g2g95Ms: 58.3,
        rvfcCaptureTimeCapability: "available",
      }),
    );
    expect(rowLabels()).toContain("capture→display · median rolling ≤600");
    expect(screen.getByText("58.3ms")).toBeTruthy();
  });
});

describe("StatsPane advanced disclosure", () => {
  it("keeps the advanced rows out of the DOM until the section is opened", () => {
    const { push } = renderDetail();
    act(() => push({ ...EMPTY_SNAPSHOT, inputMetrics }));

    expect(screen.queryByText("vsync beat")).toBeNull();
    expect(screen.queryByText("gamepads")).toBeNull();
    expect(screen.getByRole("button", { name: /advanced/i }).getAttribute("aria-expanded")).toBe(
      "false",
    );
  });

  it("flips aria-expanded and reveals the rows on click", () => {
    const { push } = renderDetail();
    act(() => push({ ...EMPTY_SNAPSHOT, presentCadence: beatingCadence(), inputMetrics }));

    const toggle = screen.getByRole("button", { name: /advanced/i });
    fireEvent.click(toggle);
    expect(toggle.getAttribute("aria-expanded")).toBe("true");
    expect(screen.getByText("vsync beat")).toBeTruthy();
    expect(screen.getByText("12% doubled · inherent")).toBeTruthy();
    expect(screen.getByText("gamepads")).toBeTruthy();

    fireEvent.click(toggle);
    expect(screen.queryByText("vsync beat")).toBeNull();
  });

  it("persists the open state to localStorage", () => {
    renderDetail();
    fireEvent.click(screen.getByRole("button", { name: /advanced/i }));
    expect(localStorage.getItem(ADVANCED_KEY)).toBe("1");
    fireEvent.click(screen.getByRole("button", { name: /advanced/i }));
    expect(localStorage.getItem(ADVANCED_KEY)).toBe("0");
  });

  it("honours a persisted open state on mount", () => {
    localStorage.setItem(ADVANCED_KEY, "1");
    const { push } = renderDetail();
    act(() => push({ ...EMPTY_SNAPSHOT, presentCadence: beatingCadence(), inputMetrics }));
    expect(screen.getByRole("button", { name: /advanced/i }).getAttribute("aria-expanded")).toBe(
      "true",
    );
    expect(screen.getByText("vsync beat")).toBeTruthy();
  });

  it("falls back to collapsed when localStorage throws", () => {
    const original = window.localStorage.getItem;
    window.localStorage.getItem = () => {
      throw new Error("private mode");
    };
    try {
      renderDetail();
      expect(screen.getByRole("button", { name: /advanced/i }).getAttribute("aria-expanded")).toBe(
        "false",
      );
    } finally {
      window.localStorage.getItem = original;
    }
  });

  it("distinguishes an unnegotiated extension from a browser that won't surface captureTime", () => {
    const { push } = renderDetail();
    fireEvent.click(screen.getByRole("button", { name: /advanced/i }));

    act(() =>
      push({
        ...EMPTY_SNAPSHOT,
        g2gMs: null,
        rvfcCaptureTimeCapability: "unavailable",
        absCaptureTimeNegotiation: "unavailable",
      }),
    );
    expect(screen.getByText("unavailable (captureTime)")).toBeTruthy();
    expect(screen.getByText(/was not negotiated with the browser/)).toBeTruthy();

    act(() =>
      push({
        ...EMPTY_SNAPSHOT,
        g2gMs: null,
        rvfcCaptureTimeCapability: "unavailable",
        absCaptureTimeNegotiation: "negotiated",
      }),
    );
    expect(screen.getByText(/isn't surfacing a frame-correlated captureTime/)).toBeTruthy();
  });

  it("omits the unavailable block once a valid g2g sample exists", () => {
    const { push } = renderDetail();
    fireEvent.click(screen.getByRole("button", { name: /advanced/i }));
    act(() =>
      push({
        ...EMPTY_SNAPSHOT,
        g2gMs: 46.1,
        g2g95Ms: 58.3,
        rvfcCaptureTimeCapability: "available",
      }),
    );
    expect(screen.queryByText("unavailable (captureTime)")).toBeNull();
  });
});

describe("StatsPane verdict", () => {
  it("leads the verdict block with the state, its reason and its falsifiers", async () => {
    const { push } = renderDetail({ sessionId: "s-1" });
    await screen.findByText("nominal");
    act(() => push({ ...EMPTY_SNAPSHOT }));

    const labels = Array.from(document.querySelectorAll(".diag-extra tr")).map((tr) =>
      (tr.querySelector("td")?.textContent ?? "").trim(),
    );
    expect(labels.slice(0, 5)).toEqual([
      "verdict",
      "why",
      "encoder.fps · p10",
      "client.present_long_frames · max",
      "local",
    ]);
  });

  it("warns on a likely_* state and marks the falsifier that failed", async () => {
    getSessionVerdict.mockResolvedValue(
      verdictFixture({
        verdict: "likely_client_presentation_limit",
        falsifiers: [
          { name: "client.present_long_frames", estimator: "max", value: 3, op: "==", threshold: 0, unit: "count", n: 5, holds: false, note: "a stall, not the vsync beat" },
        ],
      }),
    );
    renderDetail({ sessionId: "s-1" });

    const state = await screen.findByText("likely_client_presentation_limit");
    expect(state.className).toContain("diag-warn");
    const failed = screen.getByText(/3 == 0/);
    expect(failed.className).toContain("diag-warn");
    expect(failed.textContent).toContain("n=5");
  });

  it("renders an unknown verdict string verbatim", async () => {
    getSessionVerdict.mockResolvedValue(verdictFixture({ verdict: "likely_thermal_throttle" }));
    renderDetail({ sessionId: "s-1" });
    expect(await screen.findByText("likely_thermal_throttle")).toBeTruthy();
  });

  it("says 'no samples' rather than a passing zero on an older client", async () => {
    getSessionVerdict.mockResolvedValue(
      verdictFixture({
        falsifiers: [
          { name: "client.present_beat_fraction", estimator: "max", value: null, op: "<=", threshold: 1, unit: "fraction", n: 0, holds: false, note: "no samples" },
        ],
      }),
    );
    renderDetail({ sessionId: "s-1" });
    expect((await screen.findByText(/no samples/)).textContent).toContain("n=0");
  });

  it("shows no verdict rows and no error line for a non-owner (403)", async () => {
    getSessionVerdict.mockRejectedValue(new ApiError(403, "forbidden", "forbidden"));
    const { push } = renderDetail({ sessionId: "s-1" });
    act(() => push({ ...EMPTY_SNAPSHOT }));

    await waitFor(() => expect(getSessionVerdict).toHaveBeenCalled());
    expect(rowLabels()).not.toContain("verdict");
    expect(rowLabels()).not.toContain("why");
    expect(screen.queryByText(/could not load/i)).toBeNull();
    // The client-side half is still there: the pane is not empty for a guest.
    expect(rowLabels()).toContain("local");
  });

  it("asks for nothing without a session id", () => {
    renderDetail();
    expect(getSessionVerdict).not.toHaveBeenCalled();
  });
});
