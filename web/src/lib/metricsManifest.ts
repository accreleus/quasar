/**
 * The metric manifest, web side. Source of truth is
 * `docs/session-trace/metrics.json` (unit / clock / window / estimator per
 * key); `make docs-metrics-sync` copies it to `metrics.generated.json` here
 * and a Go drift test fails when the copy is stale. Go half:
 * `control-plane/internal/telemetry/manifest.go`.
 *
 * Exists so the diagnostics panel never hand-writes window/estimator labels —
 * an unlabelled `present_fps` mean once sent a healthy session down an
 * encoder-fault investigation.
 */

import raw from "./metrics.generated.json";

export type MetricSource = "agent" | "browser" | "native" | "bench";
export type MetricUnit =
  | "fps" | "ms" | "kbps" | "count" | "bool" | "fraction" | "px" | "string";
export type MetricClock =
  | "host_monotonic" | "host_wall" | "gst_pts" | "rtp"
  | "client_performance" | "client_wall" | "none";
export type MetricWindow =
  | "heartbeat(~5s)" | "1s" | "rolling_600" | "cumulative" | "snapshot" | "event";
export type MetricEstimator =
  | "mean" | "median" | "p50" | "p95" | "p10" | "max" | "min"
  | "sum" | "count" | "delta" | "last" | "raw";

export interface MetricInfo {
  key: string;
  source: MetricSource;
  /** Namespaced read-time series name, or null when outside the diagnostic lens. */
  taxonomy: string | null;
  unit: MetricUnit;
  clock: MetricClock;
  window: MetricWindow;
  estimator: MetricEstimator;
  /** The key carrying this one's sample count, or null. */
  n_key: string | null;
  /** The replacement key when this one is deprecated as a reading, or null. */
  deprecated_for: string | null;
  since: string;
  /** One line: what it measures and the trap it avoids. Rendered as a tooltip. */
  why: string;
  /** Absent means true. False = the key travels on the wire and is never stored. */
  stored?: boolean;
  rvfc_qualified?: boolean;
}

interface ManifestDoc {
  version: string;
  metrics: MetricInfo[];
}

const doc = raw as unknown as ManifestDoc;

export const METRICS_MANIFEST_VERSION = doc.version;
export const METRICS: readonly MetricInfo[] = doc.metrics;

// Two indexes. The key index is source-scoped where it has to be: `frames_dropped`
// exists for BOTH agent and browser with different meanings, and collapsing them
// is precisely the confusion the taxonomy namespaces exist to prevent.
const bySourceKey = new Map<string, MetricInfo>();
const byTaxonomy = new Map<string, MetricInfo>();
for (const e of doc.metrics) {
  bySourceKey.set(`${e.source}/${e.key}`, e);
  if (e.taxonomy) byTaxonomy.set(e.taxonomy, e);
}

/**
 * Look up a raw metric key. `source` defaults to `browser` because every web
 * caller is describing a number the client itself produced; pass it explicitly
 * for anything host-side.
 *
 * Returns undefined for an unknown key — callers render what they were going to
 * render anyway. A missing manifest row must never blank a live number.
 */
export function metricInfo(key: string, source: MetricSource = "browser"): MetricInfo | undefined {
  return bySourceKey.get(`${source}/${key}`);
}

/** Look up by namespaced taxonomy series name (`client.present_interval_sd_ms`). */
export function seriesInfo(taxonomy: string): MetricInfo | undefined {
  return byTaxonomy.get(taxonomy);
}

/**
 * The estimator/window qualifier shown beside a label, e.g. `mean 1 s`,
 * `median rolling ≤600`, `p95 ~5 s`. Empty string when the key is unknown, so a
 * caller can concatenate without a guard.
 */
export function estimatorLabel(info: MetricInfo | undefined): string {
  if (!info) return "";
  return `${info.estimator} ${windowLabel(info.window)}`.trim();
}

/** Human form of the window vocabulary. Kept short: it rides inside a table row. */
export function windowLabel(w: MetricWindow): string {
  switch (w) {
    case "heartbeat(~5s)": return "~5 s";
    case "1s": return "1 s";
    // Not "600 samples": the point a reader needs is that this is NOT the
    // current second, and that the ring is never drained.
    case "rolling_600": return "rolling ≤600";
    case "cumulative": return "cumulative";
    case "snapshot": return "at sample";
    case "event": return "once";
  }
}

/**
 * The full tooltip for a metric: what it measures, the trap, and the four
 * qualifiers spelled out. Empty string for an unknown key so `title={... ||
 * undefined}` degrades to no tooltip.
 */
export function metricTooltip(info: MetricInfo | undefined): string {
  if (!info) return "";
  const bits = [
    `${info.estimator} over ${windowLabel(info.window)}`,
    `unit ${info.unit}`,
    `clock ${info.clock}`,
  ];
  if (info.n_key) bits.push(`n from ${info.n_key}`);
  if (info.deprecated_for) bits.push(`DEPRECATED — read ${info.deprecated_for}`);
  return `${info.why}\n\n(${bits.join(" · ")})`;
}
