// Shelf section 3 — Performance stats (handoff §E.3), the v3 home of what
// DiagPanel showed.
//
// Two views over one series set: Simple is the four numbers anyone acts on,
// Detailed is the full table support and engineering read. They are fed from
// the same rolling buffer, so the card and the table can never disagree — the
// property the mock's telemetry note calls out and the reason the ring buffer
// lives here rather than in each view.
//
// #139: this pane owns every high-frequency value. It subscribes to the fan-out
// itself and holds the snapshot in its own state, so a 1 Hz tick re-renders the
// shelf's contents and nothing above them.

import { useEffect, useRef, useState } from "react";
import { EMPTY_SNAPSHOT, type TelemetrySnapshot } from "../../../../webrtc/telemetry";
import { codecDisplayName, compareCodecs } from "../../../../lib/codecDisplay";
import { useResource } from "../../../../lib/resource/react";
import { ApiError } from "../../../../api/client";
import {
  getSessionVerdict,
  isLikelyState,
  type Falsifier,
  type Verdict,
} from "../../../../api/verdict";
import {
  metricInfo,
  metricTooltip,
  estimatorLabel,
  seriesInfo,
} from "../../../../lib/metricsManifest";
import { IconHzWarn } from "../icons";
import type { TelemetryRegistrar } from "../HudBar";

export interface StatsPaneProps {
  register: TelemetryRegistrar;
  /** Session tier ("1920×1080@60") — without it a legit 720p30 reads as a bug. */
  tier?: string;
  /** Server-resolved codec (wire id), for the multi-codec mismatch check. */
  resolvedCodec?: string;
  /** Without it the pane shows only client-side numbers (no server verdict). */
  sessionId?: string;
  /** Set when the local display cannot present every streamed frame. */
  displayHzWarning: { displayHz: number; streamFps: number } | null;
  /** ISO `session.started_at`. The v1 drawer header carried this timer and the
   *  v3 mock has no home for it, so it lands with the other session facts. */
  startedAt?: string | null;
}

/** How often the owner Verdict is re-read while the pane is open (ms). */
const VERDICT_POLL_MS = 5_000;

/** Samples behind each sparkline — the mock's rolling window. */
const SERIES_LEN = 44;

/** The reason sentence names every unsatisfied falsifier and can run long. */
const REASON_MAX_CHARS = 180;

type SeriesKey = "fps" | "lat" | "br" | "jit";

const CARDS: { k: SeriesKey; label: string; unit: string; dp: number }[] = [
  { k: "fps", label: "Frame rate", unit: "fps", dp: 0 },
  { k: "lat", label: "Latency", unit: "ms", dp: 0 },
  { k: "br", label: "Bitrate", unit: "Mb/s", dp: 1 },
  { k: "jit", label: "Jitter buffer", unit: "ms", dp: 1 },
];

const fmt = (v: number | null, unit = "ms") => (v == null ? "–" : `${v.toFixed(1)}${unit}`);

/** mm:ss since `startedAt`, ticking once a second. No timer runs while the
 *  pane is unmounted, which is every moment the shelf is not on this section. */
function useElapsed(startedAt: string | null | undefined): string {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(id);
  }, []);
  if (!startedAt) return "–";
  const secs = Math.max(0, Math.floor((now - new Date(startedAt).getTime()) / 1000));
  return `${Math.floor(secs / 60)}:${String(secs % 60).padStart(2, "0")}`;
}

function truncate(text: string, max = REASON_MAX_CHARS): string {
  return text.length <= max ? text : `${text.slice(0, max - 1).trimEnd()}…`;
}

/** Estimator/window qualifier for a row label, read from the metric manifest
 *  rather than hand-typed — a hand-typed label is how `present_fps` sat
 *  unlabelled as a mean. `n` is appended by the caller. */
function qualifier(key: string, extra?: string): { text: string; title: string } {
  const info = metricInfo(key);
  const parts = [estimatorLabel(info), extra].filter(Boolean);
  return { text: parts.length ? `· ${parts.join(" ")}` : "", title: metricTooltip(info) };
}

/** A `<td>` label with its manifest-derived qualifier and `why` tooltip. */
function MetricLabel({ text, metric, extra }: { text: string; metric: string; extra?: string }) {
  const q = qualifier(metric, extra);
  return (
    <td title={q.title || undefined}>
      {text} {q.text && <span className="diag-dim">{q.text}</span>}
    </td>
  );
}

/** Compact number for a falsifier value/threshold: integers stay integral. */
function num(v: number): string {
  if (Number.isInteger(v)) return String(v);
  return Math.abs(v) >= 100 ? v.toFixed(0) : v.toFixed(v < 1 ? 3 : 1);
}

/** One falsifier as a row. The estimator rides in the label, not a tooltip:
 *  "present σ 19 ms" means something different as a mean vs a p95 vs a max,
 *  and n must stay visible too (4 samples != 400). */
function falsifierRow(f: Falsifier) {
  const value = f.value == null ? "no samples" : `${num(f.value)} ${f.op} ${num(f.threshold)}`;
  // Verdict vocabulary grows server-side; an unrecognised series name is data
  // (trace-format.md §8), not an error — it just renders with no tooltip.
  const why = metricTooltip(seriesInfo(f.name));
  return (
    <tr key={f.name}>
      <td title={why || undefined}>
        {f.name} <span className="diag-dim">· {f.estimator}</span>
      </td>
      <td className={f.holds ? undefined : "diag-warn"} title={f.note || undefined}>
        {value} <span className="diag-dim">· n={f.n}</span>
        {f.holds ? "" : " ✗"}
      </td>
    </tr>
  );
}

/**
 * `g2gMs == null && rvfcCaptureTimeCapability === "unavailable"` conflates two
 * causes: (a) abs-capture-time was never negotiated, vs (b) it was negotiated
 * but this Chrome build doesn't surface captureTime in RVFC metadata.
 * `absCaptureTimeNegotiation` distinguishes them; diagnosis only, no fix.
 */
function captureTimeCauseText(
  negotiation: TelemetrySnapshot["absCaptureTimeNegotiation"],
): string {
  switch (negotiation) {
    case "negotiated":
      return "abs-capture-time negotiated on wire — this browser build isn't surfacing a frame-correlated captureTime.";
    case "unavailable":
      return "abs-capture-time was not negotiated with the browser.";
    case "pending":
    default:
      return "abs-capture-time negotiation not yet confirmed.";
  }
}

/** `M…L…` through the samples, normalised to the mock's 100×30 viewBox. A flat
 *  series would divide by zero, so the range floors at 1. */
export function sparkPath(samples: readonly number[]): string {
  if (samples.length === 0) return "";
  const lo = Math.min(...samples);
  const hi = Math.max(...samples);
  const range = hi - lo || 1;
  return samples
    .map(
      (v, i) =>
        `${i ? "L" : "M"}${((i / Math.max(1, samples.length - 1)) * 100).toFixed(2)} ${(
          27 -
          ((v - lo) / range) * 24
        ).toFixed(2)}`,
    )
    .join(" ");
}

// Mirrors the guarded try/catch localStorage pattern in
// OverlayPreferencesContext: a throwing or absent localStorage must fall back
// to collapsed, never crash the pane.
const ADVANCED_OPEN_KEY = "quasar.diag.advancedOpen";

function readAdvancedOpen(): boolean {
  try {
    return localStorage.getItem(ADVANCED_OPEN_KEY) === "1";
  } catch {
    return false;
  }
}

function writeAdvancedOpen(open: boolean): void {
  try {
    localStorage.setItem(ADVANCED_OPEN_KEY, open ? "1" : "0");
  } catch {
    // Quota or private mode: the toggle still works, just unremembered.
  }
}

export function StatsPane(props: StatsPaneProps) {
  const [telemetry, setTelemetry] = useState<TelemetrySnapshot>(EMPTY_SNAPSHOT);
  const [detail, setDetail] = useState(false);
  const elapsed = useElapsed(props.startedAt);
  const [advancedOpen, setAdvancedOpen] = useState<boolean>(() => readAdvancedOpen());

  // The ref accumulates; `series` is the copy React renders from. Both are
  // written in the same tick as the snapshot, so a card and its sparkline can
  // never disagree, and render never reads a mutable value.
  const buffer = useRef<Record<SeriesKey, number[]>>({ fps: [], lat: [], br: [], jit: [] });
  const [series, setSeries] = useState<Record<SeriesKey, readonly number[]>>(() => ({
    fps: [],
    lat: [],
    br: [],
    jit: [],
  }));

  useEffect(
    () =>
      props.register((snap) => {
        const push = (k: SeriesKey, v: number | null) => {
          if (v == null) return;
          const a = buffer.current[k];
          a.push(v);
          if (a.length > SERIES_LEN) a.shift();
        };
        push("fps", snap.fps);
        push("lat", snap.rttMs);
        push("br", snap.bitrateKbps == null ? null : snap.bitrateKbps / 1000);
        push("jit", snap.jbMs);
        setSeries({
          fps: [...buffer.current.fps],
          lat: [...buffer.current.lat],
          br: [...buffer.current.br],
          jit: [...buffer.current.jit],
        });
        setTelemetry(snap);
      }),
    [props.register],
  );

  // ST-09 owner verdict. Owner-or-admin server-side; anything the API refuses
  // yields no rows and no error line — a diagnostics aid must not itself add
  // noise on a bad night.
  const sessionId = props.sessionId;
  const verdictRes = useResource<Verdict | null>(
    {
      label: "the session verdict",
      fetch: async ({ token, signal }) => {
        if (!sessionId) return null;
        try {
          return await getSessionVerdict(token, sessionId, signal);
        } catch (e) {
          if (e instanceof ApiError) return null; // 403 / 404 / 429 — stay quiet
          throw e;
        }
      },
      pollMs: VERDICT_POLL_MS,
    },
    [sessionId],
  );
  const verdict = verdictRes.data ?? null;

  const toggleAdvanced = () => {
    setAdvancedOpen((prev) => {
      const next = !prev;
      writeAdvancedOpen(next);
      return next;
    });
  };

  // Multi-codec spec §4. `agrees` is null until getStats resolves, so an
  // unknown never reads as a mismatch.
  const codecs = compareCodecs(props.resolvedCodec, telemetry.negotiatedCodec);

  // fpsFromMedian is the headline reading; Mean is the fallback and must say
  // so in the label — the estimator that misled before.
  const cadence = telemetry.presentCadence;
  const shownFps = cadence.fpsFromMedian ?? telemetry.presentFps;
  const shownFpsEstimator = cadence.fpsFromMedian != null ? "median" : "mean";
  const shownFpsKey = cadence.fpsFromMedian != null ? "present_fps_median" : "present_fps";
  const beatText =
    cadence.doubledFraction == null
      ? "–"
      : `${(cadence.doubledFraction * 100).toFixed(0)}% doubled${cadence.inherentBeat ? " · inherent" : ""}`;

  const value = (k: SeriesKey): number | null => {
    const a = series[k];
    return a.length ? a[a.length - 1] : null;
  };
  const input = telemetry.inputMetrics;

  return (
    <>
      <div className="pane-head">
        <h3>Performance stats</h3>
        {props.displayHzWarning && (
          <span
            className="hz-flag"
            title={`This display runs at ${props.displayHzWarning.displayHz} Hz, below the stream's ${props.displayHzWarning.streamFps} fps. Some frames can't be presented. The stream itself is healthy.`}
          >
            <IconHzWarn />
            {props.displayHzWarning.displayHz} Hz display · frames dropped
          </span>
        )}
        <div className="segmented" role="tablist" aria-label="Detail level">
          <button type="button" role="tab" aria-selected={!detail} onClick={() => setDetail(false)}>
            Simple
          </button>
          <button type="button" role="tab" aria-selected={detail} onClick={() => setDetail(true)}>
            Detailed
          </button>
        </div>
      </div>

      {!detail && (
        <div className="stat-cards">
          {CARDS.map((c) => {
            const samples = series[c.k];
            const v = value(c.k);
            const d = sparkPath(samples);
            return (
              <div className="stat-card" data-k={c.k} key={c.k}>
                <span className="lb">{c.label}</span>
                <span className="vv">
                  {v == null ? "–" : v.toFixed(c.dp)}
                  <u>{c.unit}</u>
                </span>
                <svg viewBox="0 0 100 30" preserveAspectRatio="none" aria-hidden="true">
                  <path className="area" d={d ? `${d} L100 30 L0 30 Z` : undefined} />
                  <path className="spark" d={d || undefined} />
                </svg>
              </div>
            );
          })}
        </div>
      )}

      {/* One scroll region for the whole detailed read: the shelf depth is
          fixed, and the four tables plus the verdict block are taller than it. */}
      {detail && (
        <div className="diag-scroll">
          <div className="diag-grid">
            <table>
              <tbody>
                {props.tier && (
                  <tr>
                    <td>tier</td>
                    <td>{props.tier}</td>
                  </tr>
                )}
                {codecs.resolved && (
                  <tr>
                    <td>codec (server)</td>
                    <td>{codecDisplayName(codecs.resolved)}</td>
                  </tr>
                )}
                {codecs.negotiated && (
                  <tr>
                    <td>codec (browser)</td>
                    <td className={codecs.agrees === false ? "diag-warn" : undefined}>
                      {codecDisplayName(codecs.negotiated)}
                      {codecs.agrees === false && " ⚠"}
                    </td>
                  </tr>
                )}
                {telemetry.displayRefreshHz != null && (
                  <tr>
                    <td>display Hz</td>
                    <td>{telemetry.displayRefreshHz}</td>
                  </tr>
                )}
                <tr>
                  <td>elapsed</td>
                  <td>{elapsed}</td>
                </tr>
                <tr>
                  <td>fps (recv)</td>
                  <td>{telemetry.fps.toFixed(0)}</td>
                </tr>
                <tr>
                  {/* The estimator depends on which value survived (median vs
                      mean) — the one thing the manifest can't supply. */}
                  <td title={metricTooltip(metricInfo(shownFpsKey)) || undefined}>
                    fps (shown){" "}
                    <span className="diag-dim">
                      · {shownFpsEstimator} n={cadence.n}
                    </span>
                  </td>
                  <td>{shownFps == null ? "–" : shownFps.toFixed(0)}</td>
                </tr>
              </tbody>
            </table>

            <table>
              <tbody>
                <tr>
                  <MetricLabel
                    text="present σ"
                    metric="present_interval_sd_ms"
                    extra={`n=${cadence.n}`}
                  />
                  <td>{fmt(telemetry.presentSdMs)}</td>
                </tr>
                <tr>
                  <td>playout buf</td>
                  <td>{fmt(telemetry.playoutTargetMs)}</td>
                </tr>
                <tr>
                  <MetricLabel text="rtt" metric="rtt_ms" />
                  <td>{fmt(telemetry.rttMs)}</td>
                </tr>
                <tr>
                  <MetricLabel text="jitter-buf" metric="jitter_buffer_ms" />
                  <td>{fmt(telemetry.jbMs)}</td>
                </tr>
                <tr>
                  <MetricLabel text="decode" metric="decode_ms" />
                  <td>{fmt(telemetry.decodeMs)}</td>
                </tr>
                <tr>
                  <MetricLabel text="pkt lost" metric="packets_lost" />
                  <td>{telemetry.packetsLost}</td>
                </tr>
              </tbody>
            </table>

            <table>
              <tbody>
                <tr>
                  <td>drops / freezes</td>
                  <td>
                    {telemetry.framesDropped} / {telemetry.freezeCount}
                  </td>
                </tr>
                <tr>
                  {/* Not this-second: g2g is a median over up to 600 RVFC samples. */}
                  <MetricLabel text="capture→display" metric="rvfc_capture_to_display_ms" />
                  <td>{fmt(telemetry.g2gMs)}</td>
                </tr>
                <tr>
                  <td>p95</td>
                  <td>{fmt(telemetry.g2g95Ms)}</td>
                </tr>
                <tr>
                  <MetricLabel text="network+pacing" metric="network_pacing_ms" extra="rtt/2" />
                  <td>{fmt(telemetry.networkMs)}</td>
                </tr>
                <tr>
                  {/* Can be negative (rolling g2g median minus 1 s rtt/jitter
                      buffer) — a negative residual means those estimators
                      disagree, and that is the diagnosis. */}
                  <MetricLabel text="decode+display" metric="decode_display_ms" extra="residual" />
                  <td className={(telemetry.decodeDisplayMs ?? 0) < 0 ? "diag-warn" : undefined}>
                    {fmt(telemetry.decodeDisplayMs)}
                  </td>
                </tr>
                <tr>
                  <td>backpressure</td>
                  <td>{input == null ? "–" : input.backpressureDetected ? "yes" : "no"}</td>
                </tr>
              </tbody>
            </table>

            <table>
              <tbody>
                <tr>
                  <td>input locked</td>
                  <td>{input == null ? "–" : input.pointerLocked ? "yes" : "no"}</td>
                </tr>
                <tr>
                  <td>msg/s</td>
                  <td>{input == null ? "–" : input.inputMsgPerSec.toFixed(0)}</td>
                </tr>
                <tr>
                  <td>mm/s</td>
                  <td>{input == null ? "–" : input.mmSentPerSec.toFixed(0)}</td>
                </tr>
                <tr>
                  <td>coalesced/s</td>
                  <td>{input == null ? "–" : input.coalescedSamplesPerSec.toFixed(0)}</td>
                </tr>
                <tr>
                  <td>gp send/s</td>
                  <td>{input == null ? "–" : input.gamepadSendPerSec.toFixed(0)}</td>
                </tr>
                <tr>
                  <td>ch buffered</td>
                  <td>
                    {input == null ? "–" : `${(input.channelBufferedAmount / 1024).toFixed(1)} KB`}
                  </td>
                </tr>
              </tbody>
            </table>
          </div>

          <div className="diag-extra">
            {/* Verdict first, then the readings it rests on: leading with scalars
                lets a reader assemble the wrong judgement. */}
            <table className="diag-table">
              <tbody>
                {verdict && (
                  <>
                    <tr>
                      <td>verdict</td>
                      <td className={isLikelyState(verdict.verdict) ? "diag-warn" : undefined}>
                        {/* Verbatim: an unknown state is data, never "ok". */}
                        {verdict.verdict}
                      </td>
                    </tr>
                    <tr>
                      <td>why</td>
                      <td className="diag-note" title={verdict.reason}>
                        {truncate(verdict.reason)}
                      </td>
                    </tr>
                    {verdict.falsifiers.map(falsifierRow)}
                  </>
              )}
              {/* The browser's own read beside the server's: their disagreement
                  is itself the finding. */}
              <tr>
                <td>local</td>
                <td className={telemetry.clientHealth === "smooth" ? undefined : "diag-warn"}>
                  {telemetry.clientHealthReason || telemetry.clientHealth}
                </td>
              </tr>
              {codecs.agrees === false && (
                <tr>
                  <td colSpan={2} className="diag-warn diag-note">
                    Server and browser disagree on the codec.
                  </td>
                </tr>
              )}
            </tbody>
          </table>

            <div className="diag-advanced">
              <button
                type="button"
                className="diag-advanced-toggle"
                aria-expanded={advancedOpen}
                onClick={toggleAdvanced}
              >
                <span className="diag-advanced-caret" aria-hidden="true">
                  {advancedOpen ? "▾" : "▸"}
                </span>
                Advanced
              </button>
              {advancedOpen && (
                <table className="diag-table">
                  <tbody>
                    <tr>
                      <td>vsync beat</td>
                      <td>{beatText}</td>
                    </tr>
                    <tr>
                      <MetricLabel
                        text="present max"
                        metric="present_interval_max_ms"
                        extra={`n=${cadence.n}`}
                      />
                      <td className={cadence.longFrames ? "diag-warn" : undefined}>
                        {fmt(cadence.maxMs)}
                        {cadence.longFrames ? ` · ${cadence.longFrames} long` : ""}
                      </td>
                    </tr>
                    {telemetry.g2gMs == null &&
                      telemetry.rvfcCaptureTimeCapability === "unavailable" && (
                        <>
                          <tr>
                            <td>RVFC capture-to-display</td>
                            <td>unavailable (captureTime)</td>
                          </tr>
                          <tr>
                            <td colSpan={2} className="diag-dim diag-note">
                              {captureTimeCauseText(telemetry.absCaptureTimeNegotiation)}
                            </td>
                          </tr>
                        </>
                      )}
                    {input != null && (
                      <>
                        <tr>
                          <td>gamepads</td>
                          <td>{input.gamepadCount}</td>
                        </tr>
                        {input.inputTrace && (
                          <tr>
                            <td>input trace</td>
                            <td>on (seq+tc)</td>
                          </tr>
                        )}
                      </>
                    )}
                  </tbody>
                </table>
              )}
              </div>
            </div>
        </div>
      )}
    </>
  );
}
