#!/usr/bin/env node
// `e2e-review`: the deterministic summary an agent reads first (#1275).
//
//   node scripts/validate/stream/review.mjs <bundle-dir>
//
// Writes <bundle-dir>/review/summary.json and <bundle-dir>/review/REVIEW.md from the
// bundle alone. The same bundle in gives the same bytes out: nothing here reads the
// clock, walks a directory in listing order, or writes an absolute path.
//
// The bundle is validated first. An invalid one is printed as its problem lines and
// nothing is written (the bundle a reviewer could not read to the end is not a bundle
// to summarise). Exit 0 when every scenario's status is pass, 1 when one is fail or
// error, 2 when the bundle is not valid — a skipped scenario is listed and does not
// fail the run.
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { validateBundle } from "./bundle.mjs";

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../../..");
// The golden threshold file. `scripts/dx/thresholds.py` is the harness-side reader of
// the same file; this is the review-side one, and it takes only the keys that are a
// limit on a sample key the bundle records.
const THRESHOLDS_FILE = path.join(ROOT, "docs/session-trace/thresholds.json");
const REVIEW_DIR = "review";
const SUMMARY_FILE = "summary.json";
const MARKDOWN_FILE = "REVIEW.md";
export const FORMAT = 1;

// One browser sample is one second of the run. These four keys each say how bad that
// second was; a key the samples do not carry is skipped, never scored as 0, so a
// bundle that records only fps is ranked on fps and the report says so.
export const WINDOW_KEYS = [
  { key: "present_interval_max_ms", worst: "highest" },
  { key: "g2g_p95_ms", worst: "highest" },
  { key: "missing_indices", worst: "highest" },
  { key: "fps", worst: "lowest" },
];

// A threshold is used only when it is a limit ON one sample key, per threshold key in
// docs/session-trace/thresholds.json. Deliberately absent, and why: `min_host_fps` is
// a p10 over a window and `congestion_loss_delta` / `congestion_rtt_p95_ms` are a
// delta and a p95 over one (not a value on a sample); `encoder_ceiling_ms` is a p95 of
// encode_ms, which the bundle carries as a per-sample `encode_ms`; `encoder_stall_ms`
// is a silence over time; the `playout.*` and `signal.*` values other than the two
// below are controller steps and glyph bands, not a pass rule on a sample.
export const ANOMALY_RULES = [
  ["classifier.hitch_sd_ms", "present_interval_sd_ms"],
  ["web.client_health.present_sd_degraded_ms", "present_interval_sd_ms"],
  ["web.client_health.present_p95_degraded_ms", "present_interval_p95_ms"],
  ["web.client_health.freeze_degraded_count", "freeze_count"],
  ["web.signal.packets_lost_fair", "packets_lost"],
  ["web.signal.packets_lost_poor", "packets_lost"],
];

const PICTURES = new Set([".png", ".jpg", ".jpeg", ".webp", ".gif"]);
const WORST_SHOWN = 5;

const readJson = (file) => JSON.parse(fs.readFileSync(file, "utf8"));
const readJsonl = (file) =>
  fs.existsSync(file)
    ? fs
        .readFileSync(file, "utf8")
        .split("\n")
        .filter((line) => line.trim())
        .map((line) => JSON.parse(line))
    : [];

/** The threshold values and their version, or null when the file cannot be read. */
function loadThresholds(file) {
  try {
    const doc = readJson(file);
    if (!doc || typeof doc.thresholds !== "object" || doc.thresholds === null) return null;
    return { version: typeof doc.version === "string" ? doc.version : null, thresholds: doc.thresholds };
  } catch {
    return null;
  }
}

// A pass rule is stated as `>=` in every rule above; an unjudged criterion is neither
// a pass nor a failure and sorts between the two.
const rank = (pass) => (pass === false ? 0 : pass === null ? 1 : 2);

/**
 * The five worst one-second windows, each named by the key that made it worst.
 *
 * Every key ranks the windows on its own (`highest` descending, `lowest` ascending,
 * ties by timestamp), a window takes its best rank across the keys, and the five with
 * the best rank are reported. So a window is listed because at least one key put it at
 * or near the top, never because four keys' numbers were added together into a score
 * nobody defined. A tie between keys goes to the key listed first in WINDOW_KEYS.
 */
function worstWindows(rows) {
  // One entry per second: rows that share a timestamp are folded, first value of a
  // key wins, so a telemetry row and a folded bench row describe the same window.
  const byTs = new Map();
  for (const row of rows) {
    if (row.source !== "browser" || !row.metrics || typeof row.ts_unix_ms !== "number") continue;
    let w = byTs.get(row.ts_unix_ms);
    if (!w) byTs.set(row.ts_unix_ms, (w = { ts_unix_ms: row.ts_unix_ms, metrics: {} }));
    for (const [k, v] of Object.entries(row.metrics)) {
      if (typeof v === "number" && !(k in w.metrics)) w.metrics[k] = v;
    }
  }
  const all = [...byTs.values()];
  const ranked = WINDOW_KEYS.filter(({ key }) => all.some((w) => typeof w.metrics[key] === "number"));
  const place = new Map(all.map((w) => [w.ts_unix_ms, { ts_unix_ms: w.ts_unix_ms, rank: -1, worst_for: null, value: null }]));
  for (const { key, worst } of ranked) {
    const scored = all.filter((w) => typeof w.metrics[key] === "number");
    scored.sort((a, b) =>
      worst === "lowest"
        ? a.metrics[key] - b.metrics[key] || a.ts_unix_ms - b.ts_unix_ms
        : b.metrics[key] - a.metrics[key] || a.ts_unix_ms - b.ts_unix_ms,
    );
    scored.forEach((w, i) => {
      const p = place.get(w.ts_unix_ms);
      if (p.rank < 0 || i < p.rank) {
        p.rank = i;
        p.worst_for = key;
        p.value = w.metrics[key];
      }
    });
  }
  return {
    keys: ranked.map(({ key }) => key),
    absent: WINDOW_KEYS.filter(({ key }) => !ranked.some((r) => r.key === key)).map(({ key }) => key),
    rows: [...place.values()]
      .filter((p) => p.rank >= 0)
      .sort((a, b) => a.rank - b.rank || a.ts_unix_ms - b.ts_unix_ms)
      .slice(0, WORST_SHOWN)
      .map(({ ts_unix_ms, worst_for, value }) => ({ ts_unix_ms, worst_for, value })),
  };
}

/** Each mapped threshold, once, with how many windows crossed it and the worst value. */
function anomalies(rows, thresholds) {
  if (!thresholds) return [];
  const samples = rows.filter((s) => s.source === "browser" && s.metrics && typeof s.ts_unix_ms === "number");
  const out = [];
  for (const [threshold, sampleKey] of ANOMALY_RULES) {
    const t = thresholds.thresholds[threshold];
    if (!t || typeof t.value !== "number") continue; // a threshold that moved is not invented here
    const hits = samples
      .filter((s) => typeof s.metrics[sampleKey] === "number" && s.metrics[sampleKey] >= t.value)
      .sort((a, b) => a.ts_unix_ms - b.ts_unix_ms);
    if (!hits.length) continue;
    out.push({
      threshold,
      sample_key: sampleKey,
      limit: t.value,
      unit: typeof t.unit === "string" ? t.unit : null,
      windows_over: hits.length,
      worst: hits.reduce((m, h) => Math.max(m, h.metrics[sampleKey]), hits[0].metrics[sampleKey]),
      first_ts_unix_ms: hits[0].ts_unix_ms,
    });
  }
  return out;
}

/** What builds.json could say, and what it says it could not. Sorted, not file order. */
function buildsSummary(builds) {
  return Object.keys(builds.components)
    .sort()
    .map((component) => {
      const value = builds.components[component];
      if (value === null) return { component, build: null, why: builds.unknown?.[component] ?? null };
      const parts = [];
      if (value.version !== undefined) parts.push(String(value.version));
      if (value.commit !== undefined) parts.push(`(${value.commit})`);
      if (!parts.length) parts.push(JSON.stringify(value, Object.keys(value).sort()));
      return { component, build: parts.join(" ") };
    });
}

/** The relative paths worth opening, for a scenario whose run says something went wrong. */
function filesToOpen(dir, name, scenario) {
  const failed = scenario.status === "fail" || scenario.status === "error" || scenario.criteria.some((c) => c.pass === false);
  if (!failed) return [];
  const at = path.join(dir, name);
  const out = [`${name}/`];
  if (fs.existsSync(path.join(at, "timeline.jsonl"))) out.push(`${name}/timeline.jsonl`);
  const pictures = fs
    .readdirSync(at, { withFileTypes: true })
    .filter((e) => e.isFile() && PICTURES.has(path.extname(e.name).toLowerCase()))
    .map((e) => e.name)
    .sort();
  for (const p of pictures) out.push(`${name}/${p}`);
  return out;
}

function scenarioReview(dir, name, manifest, thresholds) {
  const scenario = readJson(path.join(dir, name, "scenario.json"));
  const samples = readJsonl(path.join(dir, name, "metrics.jsonl"));
  const criteria = [...scenario.criteria.entries()]
    .sort(([ia, a], [ib, b]) => rank(a.pass) - rank(b.pass) || ia - ib)
    .map(([, c]) => ({ id: c.id, pass: c.pass, observed: c.observed, expected: c.expected }));
  return {
    dir: name,
    id: scenario.id,
    scenario: scenario.scenario ?? null,
    status: scenario.status,
    reason: scenario.reason ?? null,
    params: scenario.params,
    criteria,
    windows: worstWindows(samples),
    anomalies: anomalies(samples, thresholds),
    files: filesToOpen(dir, name, scenario),
  };
}

/**
 * The summary of the run bundle at `dir`. Throws an Error with `problems` set (the
 * validator's lines) when the bundle is not valid. Writes nothing.
 */
export function review(dir, { thresholdsFile = THRESHOLDS_FILE } = {}) {
  const problems = validateBundle(dir);
  if (problems.length) {
    const err = new Error("not a valid run bundle");
    err.problems = problems;
    throw err;
  }
  const manifest = readJson(path.join(dir, "manifest.json"));
  const client = readJson(path.join(dir, "client.json"));
  const builds = readJson(path.join(dir, "builds.json"));
  const thresholds = loadThresholds(thresholdsFile);

  const scenarios = manifest.scenarios.map((name) => scenarioReview(dir, name, manifest, thresholds));
  const counts = { pass: 0, fail: 0, error: 0, skipped: 0 };
  for (const s of scenarios) if (s.status in counts) counts[s.status] += 1;

  return {
    format: FORMAT,
    suite_run_id: manifest.suite_run_id,
    plan: manifest.plan?.name ?? null,
    client: { label: client.label ?? null, kind: client.kind },
    builds: buildsSummary(builds),
    counts,
    thresholds: thresholds ? { version: thresholds.version } : null,
    not_recorded: manifest.not_recorded.map((n) => ({ fact: n.fact, why: n.why })),
    scenarios,
  };
}

const cell = (v) => {
  if (v === null || v === undefined) return "null";
  const s = typeof v === "object" ? JSON.stringify(v) : String(v);
  return s.replace(/\|/g, "\\|");
};
const passCell = (p) => (p === null ? "not judged" : p ? "pass" : "fail");
const row = (cells) => `| ${cells.map(cell).join(" | ")} |`;

/** REVIEW.md, rendered from the summary alone, so both files describe the same run. */
export function renderReview(summary) {
  const L = [];
  const { counts } = summary;
  L.push(`# REVIEW — ${summary.suite_run_id}`, "");
  L.push(`- Plan: \`${summary.plan ?? "unknown"}\``);
  L.push(`- Client: ${cell(summary.client.label ?? "unnamed")} (${summary.client.kind})`);
  L.push(`- Builds: ${summary.builds.map((b) => (b.build === null ? `${b.component} unknown (${b.why ?? "no reason"})` : `${b.component} ${b.build}`)).join(", ")}`);
  L.push(`- Scenarios: ${counts.pass} pass, ${counts.fail} fail, ${counts.error} error, ${counts.skipped} skipped`);
  L.push(`- Thresholds: ${summary.thresholds ? `\`${summary.thresholds.version ?? "unversioned"}\`` : "none read, so no rule-based anomalies"} (\`docs/session-trace/thresholds.json\`)`);
  for (const n of summary.not_recorded) L.push(`- Not recorded: \`${n.fact}\` — ${n.why}`);

  for (const s of summary.scenarios) {
    L.push("", `## ${s.dir} — ${s.status}`, "");
    const params = Object.entries(s.params ?? {}).map(([k, v]) => `${k}=${cell(v)}`).join(", ");
    L.push(`\`${s.scenario ?? s.id}\`${params ? ` · ${params}` : ""}`);
    if (s.reason) L.push(`Reason: ${s.reason}`);

    L.push("", "### Criteria", "");
    if (!s.criteria.length) L.push("No criteria recorded.");
    else {
      L.push("| criterion | pass | observed | expected |", "| --- | --- | --- | --- |");
      for (const c of s.criteria) L.push(row([c.id, passCell(c.pass), c.observed, c.expected]));
    }

    L.push("", "### Worst one-second windows", "");
    if (!s.windows.rows.length) L.push("No browser samples, so no windows to rank.");
    else {
      const way = (k) => (WINDOW_KEYS.find((w) => w.key === k)?.worst === "lowest" ? "lowest" : "highest");
      L.push(`Ranked on ${s.windows.keys.map((k) => `\`${k}\` (${way(k)})`).join(", ")}${s.windows.absent.length ? `; absent: ${s.windows.absent.map((k) => `\`${k}\``).join(", ")}` : ""}.`, "");
      L.push("| ts_unix_ms | worst for | value |", "| --- | --- | --- |");
      for (const w of s.windows.rows) L.push(row([w.ts_unix_ms, w.worst_for, w.value]));
    }

    L.push("", "### Anomalies", "");
    if (!s.anomalies.length) L.push("No threshold crossed.");
    else {
      L.push("| threshold | sample key | limit | windows over | worst | first ts_unix_ms |", "| --- | --- | --- | --- | --- | --- |");
      for (const a of s.anomalies) L.push(row([a.threshold, a.sample_key, `>= ${a.limit}${a.unit ? ` ${a.unit}` : ""}`, a.windows_over, a.worst, a.first_ts_unix_ms]));
    }

    if (s.files.length) {
      L.push("", `Files worth opening: ${s.files.map((f) => `\`${f}\``).join(", ")}`);
    }
  }
  return L.join("\n") + "\n";
}

/** Write review/summary.json and review/REVIEW.md inside the bundle. */
export function writeReview(dir, summary) {
  const out = path.join(dir, REVIEW_DIR);
  fs.mkdirSync(out, { recursive: true });
  fs.writeFileSync(path.join(out, SUMMARY_FILE), JSON.stringify(summary, null, 2) + "\n");
  fs.writeFileSync(path.join(out, MARKDOWN_FILE), renderReview(summary));
  return out;
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  const dir = process.argv[2];
  if (!dir || !fs.statSync(dir, { throwIfNoEntry: false })?.isDirectory()) {
    console.error("usage: review.mjs <bundle-dir>");
    console.log("RESULT status=error target=e2e-review usage=1");
    process.exit(2);
  }
  let summary;
  try {
    summary = review(dir);
  } catch (e) {
    const problems = e.problems ?? [e.message];
    for (const p of problems) console.error(p);
    console.log(`RESULT status=error target=e2e-review problems=${problems.length}`);
    process.exit(2);
  }
  writeReview(dir, summary);
  const { counts } = summary;
  const n = summary.scenarios.length;
  console.log(`wrote review/REVIEW.md and review/summary.json (${n} scenario${n === 1 ? "" : "s"})`);
  console.log(
    `RESULT status=${counts.fail + counts.error ? "failed" : "ok"} target=e2e-review scenarios=${n} pass=${counts.pass} fail=${counts.fail} error=${counts.error} skipped=${counts.skipped}`,
  );
  process.exit(counts.fail + counts.error ? 1 : 0);
}
