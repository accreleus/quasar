/**
 * TraceViewer tests (ST-07).
 *
 * Follows Charts.test.tsx patterns:
 * - Stub ResizeObserver
 * - Mock getDiagnosticBundle from ../../api/admin
 */

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, render, screen, waitFor } from "@testing-library/react";
import type { DiagnosticBundle } from "../api/types";

// jsdom does not implement ResizeObserver — provide a stub that records what it
// was pointed at. #124: the chart was pinned to its fallback width forever
// because the observer was never attached to the real container, so "did anything
// get observed at all" is the assertion that catches the regression.
const observedEls: Element[] = [];
class MockResizeObserver {
  observe(el: Element) {
    observedEls.push(el);
  }
  unobserve() {}
  disconnect() {}
}
globalThis.ResizeObserver = MockResizeObserver as unknown as typeof ResizeObserver;

// Mock getDiagnosticBundle
vi.mock("../api/admin", () => ({
  getDiagnosticBundle: vi.fn(),
}));

import { getDiagnosticBundle } from "../api/admin";
import { TraceViewer } from "./TraceViewer";

const mockGetDiagnosticBundle = vi.mocked(getDiagnosticBundle);

// ST-09: the classifier is now the Verdict value. `verdictOf` builds a complete
// one so the fixtures stay about the thing each test is checking.
function verdictOf(
  verdict: string,
  evidence: string[],
): DiagnosticBundle["classifier"] {
  return {
    verdict,
    evidence,
    reason: `${verdict} over a 60 s window (60 host, 58 client samples).`,
    window: { from_ms: Date.now() - 60_000, to_ms: Date.now(), n_host: 60, n_client: 58 },
    clock: { quality: "measured", offset_ms: 5, uncertainty_ms: 2.5 },
    evidence_tier: "full",
    falsifiers: [
      {
        name: "encoder.encode_ms",
        estimator: "p95",
        value: 20.4,
        op: ">=",
        threshold: 16,
        unit: "ms",
        n: 58,
        holds: true,
      },
      {
        name: "transport.rtt_ms",
        estimator: "p95",
        value: null,
        op: "<=",
        threshold: 50,
        unit: "ms",
        n: 0,
        holds: false,
        note: "no samples",
      },
    ],
    thresholds_version: "2026-08-23.1",
  } as DiagnosticBundle["classifier"];
}

const BASE_BUNDLE: DiagnosticBundle = {
  trace: {
    session_id: "sess-abc",
    host_id: "host-1",
    profile_id: "prof-1",
    started_at: new Date(Date.now() - 60_000).toISOString(),
    ended_at: null,
  },
  window: { from_ms: Date.now() - 60_000, to_ms: Date.now() },
  clock: { client_offset_ms: 5, uncertainty_ms: 2.5, measured_at: new Date().toISOString() },
  series: {
    "encoder.encode_ms": [
      { ts_unix_ms: Date.now() - 30_000, v: 16 },
      { ts_unix_ms: Date.now() - 10_000, v: 22 },
    ],
    "encoder.fps": [
      { ts_unix_ms: Date.now() - 30_000, v: 60 },
      { ts_unix_ms: Date.now() - 10_000, v: 58 },
    ],
    "transport.rtt_ms": [
      { ts_unix_ms: Date.now() - 30_000, v: 12 },
    ],
    "transport.packets_lost": [],
    "client.present_interval_sd_ms": [
      { ts_unix_ms: Date.now() - 30_000, v: 8.5 },
    ],
    "client.glass_to_glass_ms": [],
    "abr.setpoint_kbps": [
      { ts_unix_ms: Date.now() - 30_000, v: 8000 },
    ],
  },
  events: [
    {
      source: "agent",
      ts_unix_ms: Date.now() - 20_000,
      type: "abr.retarget",
      payload: { kbps: 6000 },
    },
    {
      source: "browser",
      ts_unix_ms: Date.now() - 15_000,
      type: "playout.changed",
      payload: { playout_ms: 50 },
    },
  ],
  derived_windows: {
    hitches: [{ from_ms: 5000, to_ms: 10000, present_interval_sd_ms: 22 }],
  },
  classifier: verdictOf("likely_encoder_saturation", [
    "encode_ms p95 > 20ms for 3 consecutive windows",
  ]),
  // session-capture: the bundle always carries the key, empty when the session
  // has no captures — so a consumer iterates without a presence check.
  captures: [],
};

describe("TraceViewer", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    observedEls.length = 0;
  });

  it("renders loading state initially", () => {
    // Return a promise that never resolves so we stay in loading state
    mockGetDiagnosticBundle.mockReturnValue(new Promise(() => {}));
    render(<TraceViewer sessionId="sess-abc" token="tok" />);
    expect(screen.getByText("Loading trace…")).toBeTruthy();
  });

  it("renders verdict chip when data loads", async () => {
    mockGetDiagnosticBundle.mockResolvedValue(BASE_BUNDLE);
    render(<TraceViewer sessionId="sess-abc" token="tok" />);
    await waitFor(() =>
      expect(screen.getByText("Likely: Encoder Saturation")).toBeTruthy(),
    );
  });

  it("renders SVG lanes when data loads", async () => {
    mockGetDiagnosticBundle.mockResolvedValue(BASE_BUNDLE);
    const { container } = render(<TraceViewer sessionId="sess-abc" token="tok" />);
    await waitFor(() =>
      // At least one svg should be rendered for the lane charts
      expect(container.querySelectorAll("svg").length).toBeGreaterThan(0),
    );
  });

  it("renders clock badge with uncertainty", async () => {
    mockGetDiagnosticBundle.mockResolvedValue(BASE_BUNDLE);
    render(<TraceViewer sessionId="sess-abc" token="tok" />);
    await waitFor(() =>
      expect(screen.getByText("clock ±2.5ms")).toBeTruthy(),
    );
  });

  it("renders 'clock unmeasured' badge when clock is unmeasured", async () => {
    const bundle: DiagnosticBundle = {
      ...BASE_BUNDLE,
      clock: { unmeasured: true },
    };
    mockGetDiagnosticBundle.mockResolvedValue(bundle);
    render(<TraceViewer sessionId="sess-abc" token="tok" />);
    await waitFor(() =>
      expect(screen.getByText("clock unmeasured")).toBeTruthy(),
    );
  });

  it("renders 'no trace data' when series and events are both empty", async () => {
    const bundle: DiagnosticBundle = {
      ...BASE_BUNDLE,
      series: {},
      events: [],
    };
    mockGetDiagnosticBundle.mockResolvedValue(bundle);
    render(<TraceViewer sessionId="sess-abc" token="tok" />);
    await waitFor(() =>
      expect(
        screen.getByText("No trace data for this session yet."),
      ).toBeTruthy(),
    );
  });

  it("renders error state when API fails", async () => {
    mockGetDiagnosticBundle.mockRejectedValue(new Error("network error"));
    render(<TraceViewer sessionId="sess-abc" token="tok" />);
    // Non-ApiError falls back to generic message
    await waitFor(() =>
      expect(screen.getByText("could not load trace bundle")).toBeTruthy(),
    );
  });

  it("renders lane labels for Encoder, Transport, Client, ABR", async () => {
    mockGetDiagnosticBundle.mockResolvedValue(BASE_BUNDLE);
    render(<TraceViewer sessionId="sess-abc" token="tok" />);
    await waitFor(() => {
      expect(screen.getByText("Encoder")).toBeTruthy();
      expect(screen.getByText("Transport")).toBeTruthy();
      expect(screen.getByText("Client")).toBeTruthy();
      expect(screen.getByText("ABR")).toBeTruthy();
    });
  });

  it("renders event legend items for known event types", async () => {
    mockGetDiagnosticBundle.mockResolvedValue(BASE_BUNDLE);
    render(<TraceViewer sessionId="sess-abc" token="tok" />);
    await waitFor(() => {
      expect(screen.getByText("abr.retarget")).toBeTruthy();
      expect(screen.getByText("playout.changed")).toBeTruthy();
    });
  });

  it("calls getDiagnosticBundle with the correct session id and token", async () => {
    mockGetDiagnosticBundle.mockResolvedValue(BASE_BUNDLE);
    render(<TraceViewer sessionId="sess-xyz" token="admin-tok" />);
    await waitFor(() =>
      expect(mockGetDiagnosticBundle).toHaveBeenCalledWith("admin-tok", "sess-xyz"),
    );
  });

  // ST-07 (#324): verdict de-overload — nominal / indeterminate_client_hidden chips.
  it("renders a Nominal chip for the nominal verdict", async () => {
    const bundle: DiagnosticBundle = {
      ...BASE_BUNDLE,
      classifier: verdictOf("nominal", ["no negative signal; tab not hidden"]),
    };
    mockGetDiagnosticBundle.mockResolvedValue(bundle);
    render(<TraceViewer sessionId="sess-abc" token="tok" />);
    await waitFor(() => expect(screen.getByText("Nominal")).toBeTruthy());
  });

  it("renders an Indeterminate (client hidden) chip for indeterminate_client_hidden", async () => {
    const bundle: DiagnosticBundle = {
      ...BASE_BUNDLE,
      classifier: verdictOf("indeterminate_client_hidden", ["tab was hidden/backgrounded"]),
    };
    mockGetDiagnosticBundle.mockResolvedValue(bundle);
    render(<TraceViewer sessionId="sess-abc" token="tok" />);
    await waitFor(() =>
      expect(screen.getByText("Indeterminate (client hidden)")).toBeTruthy(),
    );
  });

  // ST-09: the verdict block now carries the value's new fields. These are the
  // parts an operator uses to CHECK the verdict rather than take it on faith.
  describe("verdict value (ST-09)", () => {
    it("shows the reason, the evidence tier and a measured clock without expanding", async () => {
      mockGetDiagnosticBundle.mockResolvedValue(BASE_BUNDLE);
      render(<TraceViewer sessionId="sess-abc" token="tok" />);
      await waitFor(() =>
        expect(screen.getByText(/60 host, 58 client samples/)).toBeTruthy(),
      );
      expect(screen.getByText(/full \(host \+ client, clocks aligned\)/)).toBeTruthy();
      expect(screen.getByText(/clock: measured/)).toBeTruthy();
    });

    it("flags an unmeasured clock and a capped tier", async () => {
      const bundle: DiagnosticBundle = {
        ...BASE_BUNDLE,
        classifier: {
          ...BASE_BUNDLE.classifier,
          clock: { quality: "unmeasured" },
          evidence_tier: "host_only",
        },
      };
      mockGetDiagnosticBundle.mockResolvedValue(bundle);
      render(<TraceViewer sessionId="sess-abc" token="tok" />);
      await waitFor(() => expect(screen.getByText("clock: unmeasured")).toBeTruthy());
      expect(screen.getByText(/host only/)).toBeTruthy();
    });

    it("renders falsifier rows with value, threshold, n and hold state", async () => {
      mockGetDiagnosticBundle.mockResolvedValue(BASE_BUNDLE);
      render(<TraceViewer sessionId="sess-abc" token="tok" />);
      await waitFor(() => expect(screen.getByText("evidence")).toBeTruthy());
      await act(async () => {
        screen.getByText("evidence").click();
      });
      expect(
        screen.getByText(/encoder\.encode_ms p95 = 20\.4 ms \(needs >= 16 ms, n=58\)/),
      ).toBeTruthy();
      // A series with no samples reads as a dash and says why — never as a pass.
      expect(
        screen.getByText(/transport\.rtt_ms p95 = — \(needs <= 50 ms, n=0\) — no samples/),
      ).toBeTruthy();
      const holds = screen.getAllByText("holds");
      const doesNot = screen.getAllByText("does not hold");
      expect(holds.length).toBe(1);
      expect(doesNot.length).toBe(1);
    });

    it("renders a pre-ST-09 bundle that carries only verdict + evidence", async () => {
      // The admin page is routinely pointed at a stack that has not been
      // redeployed yet; an older control plane must render, not throw.
      const legacy = {
        ...BASE_BUNDLE,
        classifier: { verdict: "nominal", evidence: ["healthy"] },
      } as unknown as DiagnosticBundle;
      mockGetDiagnosticBundle.mockResolvedValue(legacy);
      render(<TraceViewer sessionId="sess-abc" token="tok" />);
      await waitFor(() => expect(screen.getByText("Nominal")).toBeTruthy());
    });

    it("renders an unrecognised verdict string verbatim", async () => {
      const bundle: DiagnosticBundle = {
        ...BASE_BUNDLE,
        classifier: verdictOf("a_verdict_from_the_future", ["something new"]),
      };
      mockGetDiagnosticBundle.mockResolvedValue(bundle);
      render(<TraceViewer sessionId="sess-abc" token="tok" />);
      await waitFor(() => expect(screen.getByText("a_verdict_from_the_future")).toBeTruthy());
    });
  });

  // ST-07 (#324): live auto-update polling.
  describe("live polling", () => {
    beforeEach(() => {
      vi.useFakeTimers({ shouldAdvanceTime: true });
    });

    afterEach(() => {
      vi.useRealTimers();
    });

    it("polls the diagnostic bundle while the session is non-terminal", async () => {
      mockGetDiagnosticBundle.mockResolvedValue(BASE_BUNDLE);
      render(<TraceViewer sessionId="sess-abc" token="tok" sessionState="running" />);
      await waitFor(() => expect(mockGetDiagnosticBundle).toHaveBeenCalledTimes(1));

      await act(async () => {
        await vi.advanceTimersByTimeAsync(4000);
      });
      await waitFor(() => expect(mockGetDiagnosticBundle).toHaveBeenCalledTimes(2));

      await act(async () => {
        await vi.advanceTimersByTimeAsync(4000);
      });
      await waitFor(() => expect(mockGetDiagnosticBundle).toHaveBeenCalledTimes(3));
    });

    it("does not poll when the session state is terminal", async () => {
      mockGetDiagnosticBundle.mockResolvedValue(BASE_BUNDLE);
      render(<TraceViewer sessionId="sess-abc" token="tok" sessionState="stopped" />);
      await waitFor(() => expect(mockGetDiagnosticBundle).toHaveBeenCalledTimes(1));

      await act(async () => {
        await vi.advanceTimersByTimeAsync(10000);
      });
      expect(mockGetDiagnosticBundle).toHaveBeenCalledTimes(1);
    });

    it("does not poll when no session state is provided", async () => {
      mockGetDiagnosticBundle.mockResolvedValue(BASE_BUNDLE);
      render(<TraceViewer sessionId="sess-abc" token="tok" />);
      await waitFor(() => expect(mockGetDiagnosticBundle).toHaveBeenCalledTimes(1));

      await act(async () => {
        await vi.advanceTimersByTimeAsync(10000);
      });
      expect(mockGetDiagnosticBundle).toHaveBeenCalledTimes(1);
    });

    it("shows a live indicator while polling and hides it once terminal", async () => {
      mockGetDiagnosticBundle.mockResolvedValue(BASE_BUNDLE);
      const { rerender } = render(
        <TraceViewer sessionId="sess-abc" token="tok" sessionState="running" />,
      );
      await waitFor(() => expect(screen.getByText("live")).toBeTruthy());

      rerender(<TraceViewer sessionId="sess-abc" token="tok" sessionState="stopped" />);
      await waitFor(() => expect(screen.queryByText("live")).toBeNull());
    });
  });

  // ── #124: the chart was stuck at its fallback width and off the token palette ──
  describe("layout and theming (#124)", () => {
    it("observes the real chart container once the bundle has loaded", async () => {
      // The container carries the ref but sits behind the loading early-return,
      // so an effect that only runs on mount attaches to nothing and the chart
      // stays at its 600px fallback for the life of the component.
      mockGetDiagnosticBundle.mockResolvedValue(BASE_BUNDLE);
      const { container } = render(<TraceViewer sessionId="sess-abc" token="tok" />);
      await waitFor(() => expect(container.querySelector(".trace-viewer")).toBeTruthy());

      const plot = container.querySelector(".trace-plot");
      expect(plot).toBeTruthy();
      await waitFor(() => expect(observedEls).toContain(plot));
    });

    it("re-attaches the observer when the chart remounts after an error", async () => {
      // Same defect, second shape: an error render followed by a successful
      // Refresh must not leave the observer pointing at a detached node.
      mockGetDiagnosticBundle.mockRejectedValueOnce(new Error("boom"));
      const { container } = render(<TraceViewer sessionId="sess-abc" token="tok" />);
      await waitFor(() => expect(screen.getByText("could not load trace bundle")).toBeTruthy());

      mockGetDiagnosticBundle.mockResolvedValue(BASE_BUNDLE);
      await act(async () => {
        screen.getByText("Refresh").click();
      });
      await waitFor(() => expect(container.querySelector(".trace-plot")).toBeTruthy());
      expect(observedEls).toContain(container.querySelector(".trace-plot"));
    });

    it("labels the x axis on round second intervals", async () => {
      mockGetDiagnosticBundle.mockResolvedValue(BASE_BUNDLE);
      const { container } = render(<TraceViewer sessionId="sess-abc" token="tok" />);
      await waitFor(() => expect(container.querySelector(".trace-axis")).toBeTruthy());

      const labels = [...container.querySelectorAll(".trace-axis text")].map(
        (t) => t.textContent ?? "",
      );
      expect(labels.length).toBeGreaterThan(1);
      const secs = labels.map((l) => Number(/^\+(\d+)s$/.exec(l)?.[1]));
      expect(secs.every((n) => Number.isFinite(n))).toBe(true);
      // Every gap is the same, and it is a round step an operator can read off.
      const step = secs[1] - secs[0];
      expect([1, 2, 5, 10, 15, 30, 60, 120, 300, 600]).toContain(step);
      secs.forEach((n, i) => expect(n).toBe(secs[0] + step * i));
    });

    it("draws every series and every event marker from a design token", async () => {
      // No literal hex may survive: the hardcoded GitHub-Primer palette did not
      // follow [data-theme="light"] and matched nothing else in the console.
      mockGetDiagnosticBundle.mockResolvedValue(BASE_BUNDLE);
      const { container } = render(<TraceViewer sessionId="sess-abc" token="tok" />);
      await waitFor(() => expect(container.querySelector(".trace-plot")).toBeTruthy());

      const strokes = [...container.querySelectorAll("svg [stroke]")]
        .map((el) => el.getAttribute("stroke") ?? "")
        .filter((s) => s !== "none");
      expect(strokes.length).toBeGreaterThan(0);
      strokes.forEach((s) => expect(s).toMatch(/^var\(--/));

      const fills = [...container.querySelectorAll("svg [fill]")]
        .map((el) => el.getAttribute("fill") ?? "")
        .filter((s) => s !== "none");
      fills.forEach((s) => expect(s).toMatch(/^var\(--/));
    });

    it("scales each series in a lane on its own range", async () => {
      // fps ~60 and encode_ms ~20 shared one y-scale, which flattened encode_ms
      // onto the baseline and made the lane look broken.
      mockGetDiagnosticBundle.mockResolvedValue(BASE_BUNDLE);
      const { container } = render(<TraceViewer sessionId="sess-abc" token="tok" />);
      await waitFor(() => expect(container.querySelector(".trace-plot")).toBeTruthy());

      const encode = container.querySelector<SVGPathElement>(
        '[data-series="encoder.encode_ms"]',
      );
      expect(encode).toBeTruthy();
      const ys = [...(encode!.getAttribute("d") ?? "").matchAll(/[ML][\d.]+,([\d.]+)/g)].map(
        (m) => Number(m[1]),
      );
      expect(ys.length).toBeGreaterThan(1);
      // 16 -> 22 must be a visible climb, not a flat line pinned to the floor.
      expect(Math.max(...ys) - Math.min(...ys)).toBeGreaterThan(8);
    });

    it("states each series' scale in the lane", async () => {
      mockGetDiagnosticBundle.mockResolvedValue(BASE_BUNDLE);
      const { container } = render(<TraceViewer sessionId="sess-abc" token="tok" />);
      await waitFor(() => expect(container.querySelector(".trace-plot")).toBeTruthy());
      // The lane says what the top of each curve is worth; without it no value
      // on the chart can be read at all.
      expect(container.querySelector('[data-scale-for="encoder.fps"]')?.textContent).toMatch(
        /60/,
      );
    });

    it("draws event markers once across the lane stack, not once per lane", async () => {
      mockGetDiagnosticBundle.mockResolvedValue(BASE_BUNDLE);
      const { container } = render(<TraceViewer sessionId="sess-abc" token="tok" />);
      await waitFor(() => expect(container.querySelector(".trace-markers")).toBeTruthy());

      // Two events in the fixture, one overlay, one line each.
      const markers = container.querySelectorAll(".trace-markers line");
      expect(markers.length).toBe(2);
      // …and no lane draws its own copy.
      container.querySelectorAll(".trace-lane-svg").forEach((laneSvg) => {
        expect(laneSvg.querySelectorAll("line[stroke-dasharray]").length).toBe(0);
      });
    });

    it("marks where a series' samples stop short of the axis", async () => {
      // Host lanes ran to the right edge while the browser-sourced lanes stopped
      // early with nothing to say so — it read as a truncated chart.
      mockGetDiagnosticBundle.mockResolvedValue(BASE_BUNDLE);
      const { container } = render(<TraceViewer sessionId="sess-abc" token="tok" />);
      await waitFor(() => expect(container.querySelector(".trace-plot")).toBeTruthy());
      // transport.rtt_ms has a single sample 30s before the last encoder sample.
      expect(
        container.querySelector('[data-series-end="transport.rtt_ms"]'),
      ).toBeTruthy();
      // encoder.encode_ms runs to the end of the domain, so it gets no end cap.
      expect(
        container.querySelector('[data-series-end="encoder.encode_ms"]'),
      ).toBeNull();
    });

    it("says so when the axis covers less than the classifier's window", async () => {
      mockGetDiagnosticBundle.mockResolvedValue(BASE_BUNDLE);
      render(<TraceViewer sessionId="sess-abc" token="tok" />);
      // Fixture: samples span 30 s of a declared 60 s window.
      await waitFor(() =>
        expect(screen.getByText(/sampled range/i).textContent).toMatch(/60 s window/),
      );
    });
  });
});
