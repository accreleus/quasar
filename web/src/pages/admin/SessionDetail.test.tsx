import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import * as adminApi from "../../api/admin";
import { ToastProvider } from "../../components/Toast";
import { SessionDetail } from "./SessionDetail";
import {
  adaptationBands,
  buildAdaptationSeries,
  CodecDecisionCard,
  EffectiveMediaCard,
  FailureCard,
  latestStagedBudget,
} from "./sessions/DiagnosticsDisclosure";
import { LatestCards } from "./sessions/LatestCards";
import { SessionCharts } from "./sessions/SessionCharts";
import { SessionHero } from "./sessions/SessionHero";
import { sessionChartSeries } from "./sessions/chartSeries";
import type { AdminSession, CodecDecision, MetricPoint } from "../../api/types";

// ── First-run-experience §S5: FailureCard ────────────────────────────────────

describe("FailureCard", () => {
  it("gives app_exited_early an operator-language headline", () => {
    render(
      <FailureCard
        session={{
          failure_code: "app_exited_early",
          error_message: "Steam needs to be online to update.",
          state_detail: "app_container_exited",
          app_log_tail: null,
        }}
      />,
    );
    expect(screen.getByText("The app exited before producing any video")).toBeInTheDocument();
    expect(screen.getByText("Steam needs to be online to update.")).toBeInTheDocument();
  });

  it("falls back to a generic headline for an unrecognized failure_code", () => {
    render(
      <FailureCard
        session={{
          failure_code: "some_future_code",
          error_message: "something else went wrong",
          state_detail: null,
          app_log_tail: null,
        }}
      />,
    );
    expect(screen.getByText("This session failed")).toBeInTheDocument();
  });

  it("renders a short log tail expanded by default, with no collapse control", () => {
    render(
      <FailureCard
        session={{
          failure_code: "app_exited_early",
          error_message: null,
          state_detail: null,
          app_log_tail: "line1\nline2",
        }}
      />,
    );
    expect(screen.getByTestId("app-log-tail")).toHaveTextContent("line1");
    expect(screen.queryByRole("button", { name: /hide app log/i })).not.toBeInTheDocument();
  });

  it("collapses a long log tail by default, with an expand affordance", () => {
    const longLog = Array.from({ length: 20 }, (_, i) => `line ${i}`).join("\n");
    render(
      <FailureCard
        session={{
          failure_code: "app_exited_early",
          error_message: null,
          state_detail: null,
          app_log_tail: longLog,
        }}
      />,
    );
    expect(screen.queryByTestId("app-log-tail")).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: /show app log/i }));
    expect(screen.getByTestId("app-log-tail")).toHaveTextContent("line 19");
  });

  it("renders no log block when app_log_tail is null", () => {
    render(
      <FailureCard
        session={{ failure_code: null, error_message: "boom", state_detail: null, app_log_tail: null }}
      />,
    );
    expect(screen.queryByTestId("app-log-tail")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /show app log/i })).not.toBeInTheDocument();
  });
});

describe("EffectiveMediaCard", () => {
  it("shows configured, resolved, and actual element readback separately", () => {
    render(
      <EffectiveMediaCard
        media={{
          configured: { render_node: "/dev/dri/by-path/pci-0000:01:00.0-render" },
          resolved: { render_node: "/dev/dri/renderD128", cuda_device_id: 0 },
          actual: {
            encoder_factory: "nvcudah264enc",
            encoder_sink_caps: "video/x-raw(memory:CUDAMemory), format=NV12",
            memory_path: "CUDAMemory",
            zero_copy: true,
          },
        }}
      />,
    );

    const card = screen.getByTestId("effective-media");
    expect(within(card).getByText("configured")).toBeInTheDocument();
    expect(within(card).getByText("resolved")).toBeInTheDocument();
    expect(within(card).getByText("actual")).toBeInTheDocument();
    expect(within(card).getByText("nvcudah264enc")).toBeInTheDocument();
    expect(within(card).getByText("CUDAMemory")).toBeInTheDocument();
    expect(within(card).getByText("/dev/dri/renderD128")).toBeInTheDocument();
  });

  // #384: a session can stream 1440p while the app renders 1080p and the
  // compositor upscales it. The app's own display mode must be visible, and a
  // mismatch against the streamed mode called out — that divergence is the bug.
  it("shows the app-container display mode and flags a mismatch with the stream", () => {
    render(
      <EffectiveMediaCard
        media={{ app_display: { width: 1920, height: 1080, refresh_hz: 60, source: "agent" } }}
        stream={{ width: 2560, height: 1440, fps: 120 }}
      />,
    );

    const app = screen.getByTestId("app-display");
    expect(within(app).getByText("1920x1080@60 (agent)")).toBeInTheDocument();
    expect(within(app).getByText(/rescaled/)).toBeInTheDocument();
  });

  it("does not flag a mismatch when the app matches the stream", () => {
    render(
      <EffectiveMediaCard
        media={{ app_display: { width: 2560, height: 1440, refresh_hz: 120, source: "agent" } }}
        stream={{ width: 2560, height: 1440, fps: 120 }}
      />,
    );

    const app = screen.getByTestId("app-display");
    expect(within(app).getByText("2560x1440@120 (agent)")).toBeInTheDocument();
    expect(within(app).queryByText(/rescaled/)).not.toBeInTheDocument();
  });

  it("says so when display-mode injection is disabled on the host", () => {
    render(
      <EffectiveMediaCard
        media={{ app_display: { width: 2560, height: 1440, refresh_hz: 120, source: "disabled" } }}
        stream={{ width: 2560, height: 1440, fps: 120 }}
      />,
    );

    expect(within(screen.getByTestId("app-display")).getByText(/QUASAR_APP_DISPLAY_ENV=0/)).toBeInTheDocument();
  });
});

describe("latestStagedBudget", () => {
  it("requires RVFC marker and never double-counts asynchronous encode", () => {
    const budget = latestStagedBudget([{ ts: 1, m: {
      rvfc_capture_time_available: 1,
      glass_to_glass_ms: 50,
      network_pacing_ms: 10,
      jitter_buffer_ms: 15,
      decode_display_ms: 25,
      encode_ms: 12,
    } }]);

    expect(budget).not.toBeNull();
    expect(budget!.segments.map((segment) => segment.label)).not.toContain("encode");
    expect(budget!.totalMs).toBeLessThanOrEqual(budget!.captureToDisplayMs);
  });

  it("hides unmarked history and contradictory totals", () => {
    expect(latestStagedBudget([{ ts: 1, m: {
      glass_to_glass_ms: 50, network_pacing_ms: 10, jitter_buffer_ms: 15, decode_display_ms: 25,
    } }])).toBeNull();
    expect(latestStagedBudget([{ ts: 1, m: {
      rvfc_capture_time_available: 1,
      glass_to_glass_ms: 49, network_pacing_ms: 10, jitter_buffer_ms: 15, decode_display_ms: 25,
    } }])).toBeNull();
  });
});

describe("buildAdaptationSeries / adaptationBands", () => {
  it("builds the adaptation series from setpoint, gcc and the ladder rung", () => {
    const agent = [
      { ts: 1000, m: { abr_setpoint_kbps: 9000, gcc_estimate_kbps: 9500, adaptation_state: "healthy" } },
      {
        ts: 6000,
        m: {
          abr_setpoint_kbps: 4000,
          gcc_estimate_kbps: 3800,
          adaptation_state: "network_congested",
          ladder_res_rung: 1,
        },
      },
    ] as never;
    const s = buildAdaptationSeries(agent);
    expect(s.setpoint[0].points).toHaveLength(2);
    expect(s.gcc[0].points).toHaveLength(2);
    expect(s.rung[0].points).toEqual([{ x: 6000, y: 1 }]);
  });

  it("reports the adaptation state band as contiguous segments", () => {
    const agent = [
      { ts: 0, m: { adaptation_state: "healthy" } },
      { ts: 5000, m: { adaptation_state: "healthy" } },
      { ts: 10000, m: { adaptation_state: "network_congested" } },
    ] as never;
    expect(adaptationBands(agent)).toEqual([
      { state: "healthy", from: 0, to: 10000 },
      { state: "network_congested", from: 10000, to: 10000 },
    ]);
  });
});

// ── UI-P6: codec decision surfacing ──────────────────────────────────────────
//
// The acceptance criteria in component form:
//   1. a session that fell back shows WHICH clamp rejected the preferred rung;
//   2. server/browser codec disagreement is visible to an OPERATOR, not just in
//      the client.
//
// The third thing these pin is that the three ways a rung gets dispatched stay
// distinguishable on screen. `stream.codec` is identical for a merit win, an
// operator override and the h264 floor, so if the card rendered them the same
// an operator would be no better off than before this phase.

const fellBack: CodecDecision = {
  result_rung: "1080p60-h264",
  result_codec: "h264",
  override: null,
  floor: false,
  considered: [
    { rung_id: "1440p60-av1", codec: "av1", rejected_by: "client_decode", selected: false, clamps_bypassed: false },
    { rung_id: "1440p60-hevc", codec: "h265", rejected_by: "host_encoder", selected: false, clamps_bypassed: false },
    { rung_id: "1080p60-h264", codec: "h264", rejected_by: null, selected: true, clamps_bypassed: false },
  ],
};

describe("CodecDecisionCard — why this codec", () => {
  it("names the clamp that rejected each higher-preference rung", () => {
    render(<CodecDecisionCard decision={fellBack} resolvedCodec="h264" negotiatedCodec="video/H264" />);
    const walk = screen.getByTestId("codec-walk");

    expect(within(walk).getByTestId("codec-rung-1440p60-av1")).toHaveTextContent(/cannot decode this codec/i);
    expect(within(walk).getByTestId("codec-rung-1440p60-hevc")).toHaveTextContent(/host cannot encode/i);
    expect(within(walk).getByTestId("codec-rung-1080p60-h264")).toHaveTextContent(/dispatched/i);
    // The winner is not described as rejected.
    expect(within(walk).getByTestId("codec-rung-1080p60-h264")).not.toHaveTextContent(/despite/i);
  });

  it("describes a clean win as merit, with no warning", () => {
    render(<CodecDecisionCard decision={fellBack} resolvedCodec="h264" negotiatedCodec="video/H264" />);
    const outcome = screen.getByTestId("codec-outcome");
    expect(outcome).toHaveAttribute("data-outcome", "merit");
    expect(outcome).toHaveTextContent(/survived every clamp/i);
    expect(outcome.className).not.toMatch(/warn/);
  });

  it("shows the h264 floor as dispatched DESPITE its rejection, not as a pass", () => {
    const floor: CodecDecision = {
      result_rung: "4k60-h264",
      result_codec: "h264",
      override: null,
      floor: true,
      considered: [
        { rung_id: "4k60-av1", codec: "av1", rejected_by: "client_decode", selected: false, clamps_bypassed: false },
        // The load-bearing shape: selected AND bypassed AND still carrying the
        // clamp that killed it.
        { rung_id: "4k60-h264", codec: "h264", rejected_by: "decode_history", selected: true, clamps_bypassed: true },
      ],
    };
    render(<CodecDecisionCard decision={floor} resolvedCodec="h264" negotiatedCodec="video/H264" />);

    const outcome = screen.getByTestId("codec-outcome");
    expect(outcome).toHaveAttribute("data-outcome", "floor");
    expect(outcome).toHaveTextContent(/no rung survived/i);
    // The operator must be able to see the session is running something this
    // device has already failed to decode.
    expect(screen.getByTestId("codec-rung-4k60-h264")).toHaveTextContent(/despite being rejected/i);
    expect(screen.getByTestId("codec-rung-4k60-h264")).toHaveTextContent(/previously failed to decode/i);
  });

  it("shows an operator override as an override, never as having won on merit", () => {
    const overridden: CodecDecision = {
      result_rung: "1440p60-hevc",
      result_codec: "h265",
      override: "h265",
      floor: false,
      considered: [
        { rung_id: "1440p60-hevc", codec: "h265", rejected_by: null, selected: true, clamps_bypassed: true },
      ],
    };
    render(<CodecDecisionCard decision={overridden} resolvedCodec="h265" negotiatedCodec="video/H265" />);

    const outcome = screen.getByTestId("codec-outcome");
    expect(outcome).toHaveAttribute("data-outcome", "override");
    expect(outcome).toHaveTextContent(/forced by an operator override/i);
    expect(outcome).toHaveTextContent(/skipped, not passed/i);
    expect(outcome).not.toHaveTextContent(/merit/i);
    expect(screen.getByTestId("codec-rung-1440p60-hevc"))
      .toHaveTextContent(/without being checked against the remaining clamps/i);
  });

  it("says so plainly when no decision was recorded", () => {
    render(<CodecDecisionCard decision={null} resolvedCodec="h264" negotiatedCodec={null} />);
    expect(screen.queryByTestId("codec-outcome")).not.toBeInTheDocument();
    expect(screen.getByTestId("codec-decision")).toHaveTextContent(/walked no rung chain/i);
  });
});

describe("CodecDecisionCard — server vs client codec", () => {
  it("flags a disagreement with .cpair.disagree and an explanation", () => {
    render(<CodecDecisionCard decision={fellBack} resolvedCodec="h265" negotiatedCodec="video/H264" />);

    const pair = screen.getByTestId("codec-pair");
    expect(pair).toHaveAttribute("data-disagree", "true");
    // The design system's own class, not an invented one (styleguide §Codec pair).
    expect(pair.className.split(/\s+/)).toEqual(expect.arrayContaining(["cpair", "disagree"]));
    expect(within(pair).getByText("HEVC")).toBeInTheDocument();
    expect(within(pair).getByText("H.264")).toBeInTheDocument();
    expect(screen.getByTestId("codec-disagreement")).toHaveTextContent(/silent fallback|mis-negotiated/i);
  });

  it("does not flag when the two agree across vocabularies", () => {
    render(<CodecDecisionCard decision={fellBack} resolvedCodec="h265" negotiatedCodec="video/H265" />);
    const pair = screen.getByTestId("codec-pair");
    expect(pair).toHaveAttribute("data-disagree", "false");
    expect(pair.className).not.toMatch(/disagree/);
    expect(screen.queryByTestId("codec-disagreement")).not.toBeInTheDocument();
  });

  it("does not cry wolf before the client has reported anything", () => {
    render(<CodecDecisionCard decision={fellBack} resolvedCodec="h264" negotiatedCodec={null} />);
    const pair = screen.getByTestId("codec-pair");
    expect(pair).toHaveAttribute("data-disagree", "false");
    expect(screen.queryByTestId("codec-disagreement")).not.toBeInTheDocument();
    expect(screen.getByTestId("codec-decision")).toHaveTextContent(/not reported a decoded codec yet/i);
  });

  it("surfaces a codec the server never resolves rather than dropping it", () => {
    // vp9 is the loudest possible disagreement — it must reach the operator.
    render(<CodecDecisionCard decision={fellBack} resolvedCodec="h264" negotiatedCodec="video/VP9" />);
    const pair = screen.getByTestId("codec-pair");
    expect(pair).toHaveAttribute("data-disagree", "true");
    expect(within(pair).getByText("VP9")).toBeInTheDocument();
  });
});

// ── Handoff §A.3: the hero, the six facts, the charts, the latest cards ──────

const NOW = Date.parse("2024-01-01T01:00:00Z");

function makeSession(overrides: Partial<AdminSession> = {}): AdminSession {
  return {
    id: "aaaaaaaa-0000-0000-0000-000000000001",
    user_id: "11111111-1111-1111-1111-111111111111",
    app_id: "app",
    host_id: "h1",
    state: "running",
    state_detail: null,
    created_at: "2024-01-01T00:00:00Z",
    started_at: "2024-01-01T00:00:00Z",
    ended_at: null,
    username: "ada",
    app_name: "Hades II",
    host_name: "quasar-node-1",
    stream: { width: 1920, height: 1080, fps: 60, codec: "av1" },
    latest_metrics: {
      agent: { source: "agent", ts_unix_ms: NOW - 2000, metrics: { bitrate_kbps: 24_000 } },
      browser: { source: "browser", ts_unix_ms: NOW - 2000, metrics: { fps: 59.6, rtt_ms: 14 } },
    },
    ...overrides,
  } as AdminSession;
}

function renderHero(session: AdminSession, terminable = true) {
  return render(
    <SessionHero
      session={session}
      now={NOW}
      terminable={terminable}
      terminating={false}
      onTerminate={() => {}}
      onExportTrace={() => {}}
      exporting={false}
    />,
  );
}

describe("SessionHero", () => {
  it("names the app, the subject line and the six facts", () => {
    renderHero(makeSession());

    expect(screen.getByRole("heading", { name: "Hades II" })).toBeInTheDocument();
    expect(screen.getByText("ada · quasar-node-1 · running")).toBeInTheDocument();
    for (const label of ["Resolution", "Codec", "Frame rate", "Latency", "Bitrate", "Duration"]) {
      expect(screen.getByText(label)).toBeInTheDocument();
    }
    expect(screen.getByText("1920×1080")).toBeInTheDocument();
    expect(screen.getByText("AV1")).toBeInTheDocument();
    expect(screen.getByText("60 fps")).toBeInTheDocument();
    expect(screen.getByText("14 ms")).toBeInTheDocument();
    expect(screen.getByText("24.0 Mb/s")).toBeInTheDocument();
    expect(screen.getByText("1h 0m")).toBeInTheDocument();
  });

  it("shows the size the ladder is actually encoding, not the launch size", () => {
    renderHero(
      makeSession({
        stream: { width: 2560, height: 1440, fps: 60, codec: "h264", external_width: 1920, external_height: 1080 },
      } as Partial<AdminSession>),
    );
    expect(screen.getByText("1920×1080")).toBeInTheDocument();
  });

  it("hides Terminate on a terminal session and keeps Export trace", () => {
    renderHero(makeSession({ state: "failed", ended_at: "2024-01-01T00:30:00Z" }), false);
    expect(screen.queryByRole("button", { name: "Terminate" })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: /export trace/i })).toBeInTheDocument();
    // An ended session's duration is its whole length, not "since it started".
    expect(screen.getByText("30m")).toBeInTheDocument();
  });

  it("says nothing rather than zero before the first telemetry sample", () => {
    renderHero(makeSession({ latest_metrics: undefined }));
    expect(screen.getAllByText("—").length).toBeGreaterThanOrEqual(3);
  });
});

describe("SessionCharts", () => {
  const points: MetricPoint[] = [
    { source: "browser", ts_unix_ms: 1000, metrics: { fps: 60, rtt_ms: 14 } },
    { source: "agent", ts_unix_ms: 1000, metrics: { bitrate_kbps: 24_000, encode_ms_p50: 3.1 } },
    { source: "browser", ts_unix_ms: 2000, metrics: { fps: 58, rtt_ms: 19 } },
    { source: "agent", ts_unix_ms: 2000, metrics: { bitrate_kbps: 18_500, encode_ms_p50: 4.2 } },
  ] as MetricPoint[];

  it("renders the four titled cards with the newest value and its unit", () => {
    render(<SessionCharts series={sessionChartSeries(points)} />);

    for (const title of ["Frame rate", "Round-trip latency", "Bitrate", "Encode time"]) {
      expect(screen.getByText(title)).toBeInTheDocument();
    }
    expect(screen.getByText("58")).toBeInTheDocument();
    expect(screen.getByText("19")).toBeInTheDocument();
    expect(screen.getByText("18.5")).toBeInTheDocument();
    expect(screen.getByText("4.2")).toBeInTheDocument();
    expect(screen.getByText("Mb/s")).toBeInTheDocument();
  });

  it("draws no line for an empty series, and says so instead of showing zero", () => {
    const { container } = render(<SessionCharts series={sessionChartSeries([])} />);
    expect(container.querySelectorAll("polyline")).toHaveLength(0);
    expect(screen.getAllByText("—")).toHaveLength(4);
  });
});

describe("LatestCards", () => {
  it("reads each side's own fields and stamps the sample's age", () => {
    render(
      <LatestCards
        agent={{ source: "agent", ts_unix_ms: NOW - 2000, metrics: { fps: 60, bitrate_kbps: 24_000, encode_ms_p50: 3.1, encode_ms_p95: 5.4, frames_dropped: 0 } } as MetricPoint}
        browser={{ source: "browser", ts_unix_ms: NOW - 3000, metrics: { fps: 59, rtt_ms: 14, jitter_buffer_ms: 12, decode_ms: 2.4, freeze_count: 0 } } as MetricPoint}
        now={NOW}
      />,
    );

    const agentCard = screen.getByText("Agent latest").closest(".card") as HTMLElement;
    expect(within(agentCard).getByText("24.0 Mb/s")).toBeInTheDocument();
    expect(within(agentCard).getByText("3.1 ms")).toBeInTheDocument();
    expect(within(agentCard).getByText("2s ago")).toBeInTheDocument();

    const browserCard = screen.getByText("Browser latest").closest(".card") as HTMLElement;
    expect(within(browserCard).getByText("14 ms")).toBeInTheDocument();
    expect(within(browserCard).getByText("12.0 ms")).toBeInTheDocument();
    expect(within(browserCard).getByText("3s ago")).toBeInTheDocument();
  });

  it("falls back to the plain encode_ms an older agent sends", () => {
    render(
      <LatestCards
        agent={{ source: "agent", ts_unix_ms: NOW, metrics: { encode_ms: 7.5 } } as MetricPoint}
        browser={undefined}
        now={NOW}
      />,
    );
    expect(screen.getByText("7.5 ms")).toBeInTheDocument();
  });

  it("says there is no sample rather than showing a stale age", () => {
    render(<LatestCards agent={undefined} browser={undefined} now={NOW} />);
    expect(screen.getAllByText("no sample yet")).toHaveLength(2);
  });
});

// ── The page: what it reads, and when ────────────────────────────────────────

vi.mock("../../auth/context", () => ({ useAuth: () => ({ token: "tok" }) }));
vi.mock("../../api/admin");
// TraceViewer reads and polls the bundle on its own; stubbed so the counts
// below are the page's reads, not the viewer's.
vi.mock("../../components/TraceViewer", () => ({ TraceViewer: () => null }));

const mockedApi = vi.mocked(adminApi);

function openDiagnostics() {
  const details = screen.getByText("Diagnostics").closest("details") as HTMLDetailsElement;
  details.open = true;
  // jsdom does not fire `toggle` off the property, and React attaches onToggle
  // directly (the event does not bubble), so the event is dispatched by hand.
  fireEvent(details, new Event("toggle"));
}

describe("SessionDetail — the diagnostic bundle", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    // The download helper's anchor click is a jsdom navigation ("not implemented").
    vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(() => {});
    mockedApi.getSessionMetrics.mockResolvedValue({ items: [], next_cursor: null });
    mockedApi.listAllSessions.mockResolvedValue({
      items: [makeSession({ id: "sess-1" })],
      next_cursor: null,
    });
    mockedApi.getDiagnosticBundle.mockResolvedValue({
      session_id: "sess-1",
      events: [
        { type: "session.effective_media", ts_unix_ms: 1, payload: { negotiated_codec: "video/AV1" } },
      ],
    } as never);
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  async function mountPage() {
    await act(async () => {
      render(
        <MemoryRouter initialEntries={["/admin/sessions/sess-1"]}>
          <ToastProvider>
            <Routes>
              <Route path="/admin/sessions/:id" element={<SessionDetail />} />
            </Routes>
          </ToastProvider>
        </MemoryRouter>,
      );
    });
  }

  it("does not read the bundle on load — only on the first Diagnostics open", async () => {
    await mountPage();
    expect(mockedApi.getSessionMetrics).toHaveBeenCalled();
    expect(mockedApi.getDiagnosticBundle).not.toHaveBeenCalled();

    await act(async () => {
      openDiagnostics();
    });
    expect(mockedApi.getDiagnosticBundle).toHaveBeenCalledWith("tok", "sess-1");
    expect(mockedApi.getDiagnosticBundle).toHaveBeenCalledTimes(1);
  });

  it("keeps the bundle once opened, so Export trace costs no second read", async () => {
    await mountPage();
    await act(async () => {
      openDiagnostics();
    });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: /export trace/i }));
    });
    expect(mockedApi.getDiagnosticBundle).toHaveBeenCalledTimes(1);
  });

  it("shows the effective-media snapshot the bundle carried, once opened", async () => {
    await mountPage();
    expect(screen.queryByText("Effective media")).not.toBeInTheDocument();

    await act(async () => {
      openDiagnostics();
    });
    expect(screen.getByText("Effective media")).toBeInTheDocument();
  });

  it("reads the bundle once when Export trace runs without Diagnostics ever opened", async () => {
    await mountPage();
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: /export trace/i }));
    });
    expect(mockedApi.getDiagnosticBundle).toHaveBeenCalledTimes(1);
  });
});
