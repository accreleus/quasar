/**
 * The session detail's Diagnostics disclosure (handoff §A.3): the whole of the
 * previous drill-down, closed by default, below the four headline charts.
 * Nothing here is summarised or dropped.
 */

import { lazy, memo, Suspense, useMemo, useState } from "react";

const TraceViewer = lazy(() =>
  import("../../../components/TraceViewer").then((m) => ({ default: m.TraceViewer })),
);
import type {
  AdminSession,
  AgentMetrics,
  BrowserMetrics,
  CodecDecision,
  MetricPoint,
} from "../../../api/types";
import { Chip, LiveDot } from "../../../components/Chip";
import { LineChart2, type LineSeries2 } from "../../../components/Charts";
import { IconChevronRight, IconWarning } from "../../../components/icons";
import { LoadingState } from "../../../components/LoadingState";
import { CollapsibleLogTail, failurePresentation } from "../../../components/SessionFailureDetail";
import { StackedBar, type StackedBarSegment } from "../../../components/TelemetryChart";
import {
  codecDisplayName,
  codecOutcome,
  codecOutcomeSummary,
  compareCodecs,
  rungRejectionLabel,
} from "../../../lib/codecDisplay";
import { splitBySource } from "./chartSeries";

const COLOR_AGENT = "var(--accent)";
const COLOR_BROWSER = "var(--success)";

type EffectiveMedia = {
  configured?: Record<string, unknown>;
  resolved?: Record<string, unknown>;
  actual?: Record<string, unknown>;
  /** #384: the mode the app container renders at, vs the streamed mode — only
   *  this tells "app renders 1080p, compositor upscales to 1440p" apart. */
  app_display?: AppDisplay;
  /** Mic spec §3.2: "off" | "negotiated" | "active" — state only, never content.
   *  Absent on pre-amendment agents; treated as an open enum. */
  mic?: string;
};

type AppDisplay = {
  width?: number;
  height?: number;
  refresh_hz?: number;
  /** "agent" (injected from the profile) | "app-catalog" (app pinned it) | "disabled". */
  source?: string;
  gamescope_env?: boolean;
  injected?: Record<string, string>;
};

/** The streamed mode, for the app-vs-stream mismatch check. */
type StreamMode = { width?: number; height?: number; fps?: number };

function appDisplayNote(app: AppDisplay, stream?: StreamMode): string | null {
  if (app.source === "disabled") {
    return "Display-mode injection is disabled on this host (QUASAR_APP_DISPLAY_ENV=0) — the app runs at its image default, not the session profile.";
  }
  if (!stream || app.width === undefined || app.height === undefined) return null;
  const sizeDiffers = stream.width !== app.width || stream.height !== app.height;
  const rateDiffers = stream.fps !== undefined && app.refresh_hz !== undefined && stream.fps !== app.refresh_hz;
  if (!sizeDiffers && !rateDiffers) return null;
  return `App renders ${app.width}x${app.height}@${app.refresh_hz ?? "?"} but the stream is ${stream.width}x${stream.height}@${stream.fps ?? "?"} — frames are being rescaled.`;
}

function mediaValue(value: unknown): string {
  if (value === null || value === undefined || value === "") return "—";
  if (typeof value === "object") return JSON.stringify(value);
  return String(value);
}

export function EffectiveMediaCard({ media, stream }: { media: EffectiveMedia; stream?: StreamMode }) {
  const app = media.app_display;
  const note = app ? appDisplayNote(app, stream) : null;
  return (
    <div className="card card-pad" data-testid="effective-media">
      <div className="sec-head">
        <div>
          <h3>Effective media</h3>
          <div className="desc">
            Configured request, resolved assignment, and readback from the live media elements.
          </div>
        </div>
      </div>
      {app && (
        <div data-testid="app-display" style={{ marginBottom: "0.75rem" }}>
          <div className="row between gap4">
            <dt className="muted">app display</dt>
            <dd className="mono" style={{ textAlign: "right", overflowWrap: "anywhere" }}>
              {app.width}x{app.height}@{app.refresh_hz ?? "?"} ({app.source ?? "unknown"})
            </dd>
          </div>
          {note && <p className="muted" style={{ margin: "0.25rem 0 0" }}>{note}</p>}
        </div>
      )}
      {typeof media.mic === "string" && (
        <div data-testid="mic-state" style={{ marginBottom: "0.75rem" }}>
          <div className="row between gap4">
            <dt className="muted">microphone</dt>
            <dd className="mono" style={{ textAlign: "right", overflowWrap: "anywhere" }}>
              {media.mic}
            </dd>
          </div>
        </div>
      )}
      <div className="sd-latest-grid">
        {(["configured", "resolved", "actual"] as const).map((stage) => (
          <div key={stage} className="sd-latest">
            <h3 style={{ textTransform: "capitalize" }}>{stage}</h3>
            <dl>
              {Object.entries(media[stage] ?? {}).map(([key, value]) => (
                <div key={key} className="row between gap4">
                  <dt className="muted">{key.replaceAll("_", " ")}</dt>
                  <dd className="mono" style={{ textAlign: "right", overflowWrap: "anywhere" }}>
                    {mediaValue(value)}
                  </dd>
                </div>
              ))}
            </dl>
          </div>
        ))}
      </div>
    </div>
  );
}

/**
 * UI-P6: the design system's codec pair (`.cpair`, styleguide §Codec pair).
 * `agrees === null` (either side unknown) must not be flagged as a disagreement —
 * every session would light up amber before the client reports a codec.
 */
function CodecPair({
  resolved,
  negotiated,
}: {
  resolved: string | null | undefined;
  negotiated: string | null | undefined;
}) {
  const pair = compareCodecs(resolved, negotiated);
  const disagree = pair.agrees === false;
  return (
    <span
      className={disagree ? "cpair disagree" : "cpair"}
      data-testid="codec-pair"
      data-disagree={disagree ? "true" : "false"}
      title={
        disagree
          ? "The server resolved one codec and the client reports decoding another — a silent fallback or a mis-negotiated m-line."
          : undefined
      }
    >
      <span className="chip chip-neutral">{codecDisplayName(pair.resolved) ?? "—"}</span>
      <svg className="arrow" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.5" aria-hidden="true">
        <path d="M2 8h10M8 4l4 4-4 4" strokeLinecap="round" strokeLinejoin="round" />
      </svg>
      <span className="chip chip-neutral">{codecDisplayName(pair.negotiated) ?? "—"}</span>
    </span>
  );
}

/**
 * The server's rung-resolution record: each considered rung in walk order, with
 * the clamp that rejected it. The three outcomes must stay visually distinct
 * (contract §Codec decision) — override, floor, and clean win all produce the
 * same `stream.codec`; the floor's selected rung keeps its rejection reason.
 */
export function CodecDecisionCard({
  decision,
  resolvedCodec,
  negotiatedCodec,
}: {
  decision: CodecDecision | null | undefined;
  resolvedCodec: string | null | undefined;
  negotiatedCodec: string | null | undefined;
}) {
  const outcome = codecOutcome(decision);
  const summary = codecOutcomeSummary(decision);
  const pair = compareCodecs(resolvedCodec, negotiatedCodec);

  return (
    // Not `.card-head`: that class exists in no stylesheet, so it renders as nothing.
    <div className="card card-pad" data-testid="codec-decision">
      <div className="sec-head">
        <div>
          <h3>Codec</h3>
          <div className="desc">
            What the server resolved, what the client is decoding, and why this rung and not a
            higher one.
          </div>
        </div>
      </div>

      <dl style={{ margin: 0 }}>
        <div className="row between gap4">
          <dt className="muted">server resolved → client decoded</dt>
          <dd style={{ margin: 0, textAlign: "right" }}>
            <CodecPair resolved={resolvedCodec} negotiated={negotiatedCodec} />
          </dd>
        </div>
      </dl>

      {pair.agrees === false && (
        <p className="note warn" data-testid="codec-disagreement" style={{ marginBottom: 0 }}>
          <IconWarning />
          <span>
            The server resolved <b>{codecDisplayName(pair.resolved)}</b> but the client reports
            decoding <b>{codecDisplayName(pair.negotiated)}</b>. That is a silent fallback or a
            mis-negotiated m-line, not a display preference — the stream is not running the codec
            this session was resolved for.
          </span>
        </p>
      )}
      {pair.negotiated === null && (
        <p className="muted" style={{ marginBottom: 0, fontSize: 12 }}>
          The client has not reported a decoded codec yet, so there is nothing to compare against.
        </p>
      )}

      {!decision ? (
        <p className="muted" style={{ marginBottom: 0, fontSize: 12 }}>
          No resolution record — this session walked no rung chain (a console launch, a launch
          that forced a codec without naming a profile, or a session that predates
          codec-decision recording).
        </p>
      ) : (
        <>
          {summary && (
            <p
              className={outcome === "merit" ? "muted" : "note warn"}
              data-testid="codec-outcome"
              data-outcome={outcome ?? ""}
              style={{ marginBottom: 0 }}
            >
              {outcome !== "merit" && <IconWarning />}
              <span>{summary}</span>
            </p>
          )}
          <dl style={{ marginBottom: 0 }} data-testid="codec-walk">
            {decision.considered.map((rung) => {
              const reason = rungRejectionLabel(rung.rejected_by);
              return (
                <div key={rung.rung_id} className="row between gap4">
                  <dt className="mono" style={{ overflowWrap: "anywhere" }}>
                    {rung.rung_id}{" "}
                    <span className="chip chip-sm chip-neutral">
                      {codecDisplayName(rung.codec) ?? rung.codec}
                    </span>
                  </dt>
                  <dd
                    className="muted"
                    style={{ margin: 0, textAlign: "right", overflowWrap: "anywhere" }}
                    data-testid={`codec-rung-${rung.rung_id}`}
                  >
                    {rung.selected && rung.clamps_bypassed && reason
                      ? `dispatched despite being rejected — ${reason}`
                      : rung.selected && rung.clamps_bypassed
                        ? "dispatched without being checked against the remaining clamps"
                        : rung.selected
                          ? "dispatched"
                          : (reason ?? "passed")}
                  </dd>
                </div>
              );
            })}
          </dl>
        </>
      )}
    </div>
  );
}

/**
 * First-run-experience §S5. Adapter from raw `AdminSession` fields to the
 * presentation shared with the stream client (`components/SessionFailureDetail.tsx`).
 */
export function FailureCard({
  session,
}: {
  session: Pick<AdminSession, "failure_code" | "error_message" | "state_detail" | "app_log_tail">;
}) {
  const headline = failurePresentation(session.failure_code) ?? "This session failed";

  return (
    <div className="card card-pad" data-testid="failure-card">
      <div className="sec-head">
        <div>
          <h3>{headline}</h3>
          <div className="desc">
            {session.error_message ?? session.state_detail ?? "No further detail was reported."}
          </div>
        </div>
      </div>

      {session.app_log_tail && <CollapsibleLogTail text={session.app_log_tail} testId="app-log-tail" />}
    </div>
  );
}

// ── helpers ───────────────────────────────────────────────────────────────────

function fmt(n: number | undefined, unit = ""): string {
  if (n === undefined || n === null) return "—";
  return `${n.toFixed(1)}${unit}`;
}

/** AS10-06: map a computed health_state to a badge severity variant (UI-12). */
export function healthBadgeVariant(health: string): "success" | "warning" | "danger" | "neutral" {
  if (health === "healthy") return "success";
  if (health === "unsustainable" || health === "failed") return "danger";
  return "warning";
}

/** AS10-11: is this a client-side (device) health state vs a network/server one? */
export function isClientHealth(health: string): boolean {
  return health === "client_decode_degrading" || health === "client_presentation_degrading";
}

/** Build a multi-series line config from agent+browser data for a given metric key. */
function buildSeries(
  agent: Array<{ ts: number; m: AgentMetrics }>,
  browser: Array<{ ts: number; m: BrowserMetrics }>,
  agentKey: keyof AgentMetrics,
  browserKey: keyof BrowserMetrics,
): LineSeries2[] {
  const out: LineSeries2[] = [];
  const aPts = agent
    .filter((r) => r.m[agentKey] !== undefined)
    .map((r) => ({ x: r.ts, y: r.m[agentKey] as number }));
  const bPts = browser
    .filter((r) => r.m[browserKey] !== undefined)
    .map((r) => ({ x: r.ts, y: r.m[browserKey] as number }));
  if (aPts.length > 0) out.push({ label: "agent", color: COLOR_AGENT, points: aPts });
  if (bPts.length > 0) out.push({ label: "browser", color: COLOR_BROWSER, points: bPts });
  return out;
}

function buildSingleSeries<T>(
  data: Array<{ ts: number; m: T }>,
  key: keyof T,
  label: string,
  color: string,
): LineSeries2[] {
  const pts = data
    .filter((r) => r.m[key] !== undefined)
    .map((r) => ({ x: r.ts, y: r.m[key] as number }));
  if (pts.length === 0) return [];
  return [{ label, color, points: pts }];
}

/** Adaptation-panel series, agent-only. `ladder_fps`/`ladder_res_rung` are
 *  omit-when-default: absent until the rung steps, so the card shows "launch". */
export function buildAdaptationSeries(agent: Array<{ ts: number; m: AgentMetrics }>) {
  return {
    setpoint: buildSingleSeries(agent, "abr_setpoint_kbps", "setpoint", COLOR_AGENT),
    gcc: buildSingleSeries(agent, "gcc_estimate_kbps", "GCC estimate", "var(--lavender)"),
    rung: buildSingleSeries(agent, "ladder_res_rung", "resolution rung", "var(--danger-text)"),
    bias: buildSingleSeries(agent, "ladder_speed_bias", "speed bias", "var(--info)"),
    fps: buildSingleSeries(agent, "ladder_fps", "ladder fps", "var(--lavender)"),
  };
}

/** Contiguous runs of adaptation_state. A run extends to the next sample's ts
 *  (each label describes the window ending at its own ts); the last run is
 *  zero-width by construction. */
export function adaptationBands(
  agent: Array<{ ts: number; m: AgentMetrics }>,
): Array<{ state: string; from: number; to: number }> {
  const out: Array<{ state: string; from: number; to: number }> = [];
  for (const { ts, m } of agent) {
    const state = m.adaptation_state ?? "unknown";
    const last = out[out.length - 1];
    if (last && last.state === state) {
      last.to = ts;
    } else {
      if (last) last.to = ts;
      out.push({ state, from: ts, to: ts });
    }
  }
  return out;
}

/** Extract latest qualified RVFC capture-to-display budget from browser points. */
export function latestStagedBudget(
  browser: Array<{ ts: number; m: BrowserMetrics }>,
): { segments: StackedBarSegment[]; totalMs: number; captureToDisplayMs: number } | null {
  for (let i = browser.length - 1; i >= 0; i--) {
    const m = browser[i].m;
    if (
      m.rvfc_capture_time_available !== 1 ||
      m.glass_to_glass_ms === undefined ||
      m.network_pacing_ms === undefined ||
      m.jitter_buffer_ms === undefined ||
      m.decode_display_ms === undefined
    ) {
      continue;
    }

    // decode_display_ms is already the residual after network+jitter; adding the
    // asynchronously sampled agent encode time would double-count latency.
    const segments: StackedBarSegment[] = [
      { label: "network", value: m.network_pacing_ms, color: "var(--info)" },
      { label: "jitter buf", value: m.jitter_buffer_ms, color: "var(--lavender)" },
      { label: "unattributed residual (decode+display)", value: m.decode_display_ms, color: "var(--success)" },
    ];
    const totalMs = segments.reduce((sum, segment) => sum + segment.value, 0);
    // Segments must never exceed their qualified total (0.001 fp tolerance).
    if (!Number.isFinite(totalMs) || totalMs > m.glass_to_glass_ms + 0.001) continue;
    return { segments, totalMs, captureToDisplayMs: m.glass_to_glass_ms };
  }
  return null;
}

// memo hits only because the caller stabilises series refs via useMemo;
// without them it would re-render every 5 s poll.
const ChartCard = memo(function ChartCard({
  title,
  series,
  unit,
  latestVal,
  latestCls = "",
}: {
  title: string;
  series: LineSeries2[];
  unit?: string;
  latestVal?: string;
  latestCls?: string;
}) {
  const hasData = series.some((s) => s.points.length > 0);
  return (
    <div className="card sd-chart-card">
      <div className="sd-chart-top">
        <div className="sd-chart-title">{title}</div>
        {latestVal && (
          <div className={`sd-chart-val mono ${latestCls}`}>{latestVal}</div>
        )}
      </div>
      <div className="sd-chart-area">
        {hasData ? (
          <LineChart2 series={series} unit={unit} height={96} />
        ) : (
          <div className="muted" style={{ fontSize: "var(--t-xs)", paddingTop: 8 }}>
            no data yet
          </div>
        )}
      </div>
    </div>
  );
});

export interface DiagnosticsDisclosureProps {
  sessionId: string;
  token: string;
  session: AdminSession | null;
  effectiveMedia: EffectiveMedia | null;
  points: MetricPoint[];
  /** Fires the first time the disclosure is opened — the page loads the
   *  diagnostic bundle then, not on every poll. */
  onOpen: () => void;
}

/** Health, codec walk, effective media, every raw series, and the trace. */
export function DiagnosticsDisclosure({
  sessionId,
  token,
  session,
  effectiveMedia,
  points,
  onOpen,
}: DiagnosticsDisclosureProps) {
  // Stable identities so ChartCard's memo hits and LineChart2 skips path
  // recomputation on a poll that changed nothing.
  const { agent, browser } = useMemo(() => splitBySource(points), [points]);
  const adaptation = useMemo(() => buildAdaptationSeries(agent), [agent]);
  const bands = useMemo(() => adaptationBands(agent), [agent]);
  const chartSeries = useMemo(
    () => ({
      fps: buildSeries(agent, browser, "fps", "fps"),
      bitrate: buildSeries(agent, browser, "bitrate_kbps", "bitrate_kbps"),
      rtt: buildSingleSeries(browser, "rtt_ms", "browser RTT", COLOR_BROWSER),
      jitter: buildSingleSeries(browser, "jitter_buffer_ms", "jitter buffer", COLOR_BROWSER),
      sigma: buildSingleSeries(browser, "present_interval_sd_ms", "present σ", "var(--lavender)"),
      encode: buildSingleSeries(agent, "encode_ms", "agent encode", COLOR_AGENT),
      decode: buildSingleSeries(browser, "decode_ms", "browser decode", COLOR_BROWSER),
      pktLost: buildSingleSeries(browser, "packets_lost", "packets lost", "var(--danger-text)"),
      framesDropped: buildSingleSeries(agent, "frames_dropped", "agent dropped", COLOR_AGENT),
      streamHeight: buildSingleSeries(agent, "stream_height", "external height", COLOR_AGENT),
    }),
    [agent, browser],
  );

  const stagedBudget = latestStagedBudget(browser);
  const latestAgent = agent.length > 0 ? agent[agent.length - 1].m : undefined;
  const latestBrowser = browser.length > 0 ? browser[browser.length - 1].m : undefined;

  // Each card's headline figure is the newest sample, never a window average.
  // fps prefers the browser's: what the viewer saw beats what the host encoded.
  const latestFps = latestBrowser?.fps ?? latestAgent?.fps;
  const latestBitrate = latestAgent?.bitrate_kbps;
  const latestRtt = latestBrowser?.rtt_ms;
  const latestJitter = latestBrowser?.jitter_buffer_ms;
  const latestSigma = latestBrowser?.present_interval_sd_ms;
  const latestEncode = latestAgent?.encode_ms;
  const latestDecode = latestBrowser?.decode_ms;
  const latestPktLost = latestBrowser?.packets_lost;

  const [open, setOpen] = useState(false);

  return (
    <details
      className="card card-pad sd-diagnostics"
      onToggle={(e) => {
        const isOpen = e.currentTarget.open;
        setOpen(isOpen);
        if (isOpen) onOpen();
      }}
    >
      <summary>
        <IconChevronRight className="sd-diag-caret" />
        <span className="panel-title">Diagnostics</span>
        <span className="hint" style={{ marginLeft: "auto" }}>
          Codec walk, effective media, adaptation, raw series and the trace
        </span>
      </summary>

      <div style={{ display: "grid", gap: "var(--s4)", marginTop: "var(--s4)" }}>
        {session?.health_state && (
          <div className="card card-pad">
            <div className="rowflex" style={{ gap: "var(--s3)", flexWrap: "wrap" }}>
              <span className="eyebrow">Health</span>
              <Chip variant={healthBadgeVariant(session.health_state)}>{session.health_state}</Chip>
              {isClientHealth(session.health_state) && (
                <span className="hint">client-side (device) bottleneck</span>
              )}
              {session.health_reason && <span className="hint">{session.health_reason}</span>}
            </div>
          </div>
        )}

        {session?.state === "failed" &&
          (session.failure_code || session.error_message || session.app_log_tail) && (
            <FailureCard session={session} />
          )}

        {/* "Why not a better codec" is the lead question, so it sits first. */}
        {session && (
          <CodecDecisionCard
            decision={session.codec_decision}
            resolvedCodec={session.stream?.codec}
            negotiatedCodec={session.negotiated_codec}
          />
        )}

        {effectiveMedia && <EffectiveMediaCard media={effectiveMedia} stream={session?.stream} />}

        {(agent.length > 0 || browser.length > 0) && (
          <div className="sd-latest-grid">
            {agent.length > 0 && (
              <div className="card sd-latest">
                <div className="sd-latest-head">
                  <span>Agent (latest)</span>
                  <LiveDot />
                </div>
                <div className="sd-latest-rows">
                  {(
                    [
                      ["FPS", fmt(latestAgent?.fps), latestAgent?.fps !== undefined && latestAgent.fps >= 45 ? "good" : ""],
                      ["Bitrate", fmt(latestAgent?.bitrate_kbps, " kbps"), ""],
                      ["Encode", fmt(latestAgent?.encode_ms, " ms"), ""],
                      ["Dropped", fmt(latestAgent?.frames_dropped), latestAgent?.frames_dropped === 0 ? "good" : ""],
                    ] as [string, string, string][]
                  ).map(([k, v, cls]) => (
                    <div key={k} className="sd-lr">
                      <span className="l">{k}</span>
                      <span className={`v${cls ? ` ${cls}` : ""}`}>{v}</span>
                    </div>
                  ))}
                </div>
              </div>
            )}
            {browser.length > 0 && (
              <div className="card sd-latest">
                <div className="sd-latest-head">
                  <span>Browser (latest)</span>
                  <LiveDot />
                </div>
                <div className="sd-latest-rows">
                  {(
                    [
                      ["FPS", fmt(latestBrowser?.fps), latestBrowser?.fps !== undefined && latestBrowser.fps >= 45 ? "good" : ""],
                      ["RTT", fmt(latestBrowser?.rtt_ms, " ms"), latestBrowser?.rtt_ms !== undefined && latestBrowser.rtt_ms < 20 ? "info" : ""],
                      ["Jitter buf", fmt(latestBrowser?.jitter_buffer_ms, " ms"), ""],
                      ["Present σ", fmt(latestBrowser?.present_interval_sd_ms), ""],
                      ["Playout", fmt(latestBrowser?.playout_target_ms, " ms"), ""],
                      ["Decode", fmt(latestBrowser?.decode_ms, " ms"), ""],
                      ["Pkt lost", fmt(latestBrowser?.packets_lost), latestBrowser?.packets_lost === 0 ? "good" : ""],
                      ["Dropped", fmt(latestBrowser?.frames_dropped), latestBrowser?.frames_dropped === 0 ? "good" : ""],
                    ] as [string, string, string][]
                  ).map(([k, v, cls]) => (
                    <div key={k} className="sd-lr">
                      <span className="l">{k}</span>
                      <span className={`v${cls ? ` ${cls}` : ""}`}>{v}</span>
                    </div>
                  ))}
                </div>
              </div>
            )}
          </div>
        )}

        {points.length > 0 && (
          <>
            {/* The headline four are single-source by design; these keep the
                agent/browser overlay, where the divergence is the finding. */}
            <div className="sd-charts">
              <ChartCard
                title="FPS"
                unit="fps"
                series={chartSeries.fps}
                latestVal={latestFps !== undefined ? latestFps.toFixed(0) : undefined}
                latestCls={latestFps !== undefined && latestFps >= 45 ? "good" : ""}
              />
              <ChartCard
                title="Bitrate"
                unit="kbps"
                series={chartSeries.bitrate}
                latestVal={
                  latestBitrate !== undefined ? `${(latestBitrate / 1000).toFixed(1)} Mbps` : undefined
                }
              />
              <ChartCard
                title="RTT"
                unit="ms"
                series={chartSeries.rtt}
                latestVal={latestRtt !== undefined ? `${latestRtt.toFixed(0)} ms` : undefined}
              />
              <ChartCard
                title="Jitter buffer"
                unit="ms"
                series={chartSeries.jitter}
                latestVal={latestJitter !== undefined ? `${latestJitter.toFixed(0)} ms` : undefined}
              />
              <ChartCard
                title="Presentation σ"
                unit="ms"
                series={chartSeries.sigma}
                latestVal={latestSigma !== undefined ? latestSigma.toFixed(2) : undefined}
              />
              <ChartCard
                title="Encode time"
                unit="ms"
                series={chartSeries.encode}
                latestVal={latestEncode !== undefined ? `${latestEncode.toFixed(1)} ms` : undefined}
              />
              <ChartCard
                title="Decode time"
                unit="ms"
                series={chartSeries.decode}
                latestVal={latestDecode !== undefined ? `${latestDecode.toFixed(1)} ms` : undefined}
              />
              <ChartCard
                title="Packets lost"
                series={chartSeries.pktLost}
                latestVal={latestPktLost !== undefined ? latestPktLost.toFixed(0) : undefined}
                latestCls={latestPktLost === 0 ? "good" : ""}
              />
              <ChartCard title="Frames dropped (agent)" series={chartSeries.framesDropped} />
            </div>

            <div className="card card-pad">
              <div className="sd-chart-title" style={{ marginBottom: 12 }}>
                Adaptation
                <span className="hint" style={{ marginLeft: 8, fontWeight: 400 }}>
                  setpoint vs GCC, ladder rungs, classifier state
                </span>
              </div>
              <div className="sd-charts">
                <ChartCard
                  title="Setpoint vs GCC"
                  unit="kbps"
                  series={[...adaptation.setpoint, ...adaptation.gcc]}
                  latestVal={
                    latestAgent?.abr_setpoint_kbps !== undefined
                      ? `${(latestAgent.abr_setpoint_kbps / 1000).toFixed(1)} Mbps`
                      : undefined
                  }
                />
                <ChartCard
                  title="Ladder rungs"
                  series={[...adaptation.rung, ...adaptation.bias]}
                  latestVal={
                    latestAgent?.ladder_res_rung !== undefined
                      ? `res ${latestAgent.ladder_res_rung}`
                      : undefined
                  }
                />
                <ChartCard
                  title="Streamed height"
                  unit="px"
                  series={chartSeries.streamHeight}
                  latestVal={
                    latestAgent?.stream_height !== undefined ? `${latestAgent.stream_height}p` : "launch"
                  }
                />
                <ChartCard
                  title="Streamed frame rate"
                  unit="fps"
                  series={adaptation.fps}
                  latestVal={
                    latestAgent?.ladder_fps !== undefined ? `${latestAgent.ladder_fps} fps` : "launch"
                  }
                />
              </div>
              <div className="sd-legend" role="list" aria-label="Adaptation state">
                {bands.map((b) => (
                  <Chip key={`${b.state}-${b.from}`} variant={b.state === "healthy" ? "success" : "warning"}>
                    {b.state} · {Math.round((b.to - b.from) / 1000)}s
                  </Chip>
                ))}
              </div>
            </div>

            {stagedBudget && (
              <div className="card card-pad">
                <div className="sd-chart-title" style={{ marginBottom: 12 }}>
                  RVFC capture-to-display breakdown (residual only)
                  <span className="hint" style={{ marginLeft: 8, fontWeight: 400 }}>
                    (latest sample)
                  </span>
                </div>
                <StackedBar segments={stagedBudget.segments} totalLabel="RVFC capture-to-display" />
              </div>
            )}

            <div className="sd-legend">
              <span style={{ color: COLOR_AGENT }}>—</span>
              <span>agent</span>
              <span style={{ color: COLOR_BROWSER, marginLeft: 12 }}>—</span>
              <span>browser</span>
            </div>
          </>
        )}

        {/* Mounted only while open: TraceViewer loads the diagnostic bundle on
            mount and then polls it for a live session, so a collapsed
            disclosure would keep the page's heaviest read running. */}
        {open && (
          <div className="card card-pad">
            <div className="sd-chart-title" style={{ marginBottom: 12 }}>
              Trace
              <span className="hint" style={{ marginLeft: 8, fontWeight: 400 }}>
                stacked time-series + events
              </span>
            </div>
            <Suspense fallback={<LoadingState>Loading trace…</LoadingState>}>
              <TraceViewer sessionId={sessionId} token={token} sessionState={session?.state} />
            </Suspense>
          </div>
        )}
      </div>
    </details>
  );
}
