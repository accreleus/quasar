// Bench-mode windows -> `browser` metric rows. A port of bench_rows() in
// scripts/dx/bench_app_samples.py (the key names, the stage_* prefix rule, the i2p roll-up and
// the timestamp preference order must stay the same); a change there is made here too.
const num = (v) => typeof v === "number" && Number.isFinite(v);

// Payload keys renamed 2026-08-18; an older instrument still sends the left-hand names.
const LEGACY_KEYS = { dropped: "missing_indices", crc_fail: "undecoded" };
const PLAIN_KEYS = ["n", "decoded", "undecoded", "no_image", "missing_indices", "duplicated", "reordered",
  "g2g_p50_ms", "g2g_p95_ms", "g2g_max_ms", "render_w", "render_h", "offset_ms", "i2p_missed"];

function percentile(values, q) {
  if (!values.length) return null;
  const s = [...values].sort((a, b) => a - b);
  return s[Math.max(0, Math.min(s.length - 1, Math.floor(s.length * q + 0.999999) - 1))];
}

/** A window's timestamp: its own host-clock stamps first, strictly after the previous row's. */
function windowTs(w, prev, fallback) {
  const cands = ["last_host_time_ms", "t_end_host_ms", "t_end_ms"].filter((k) => num(w[k]) && w[k] > 0).map((k) => [Math.trunc(w[k]), k]);
  for (const [v, k] of cands) if (prev === null || v > prev) return { ts: v, src: k, nudged: false };
  const [base, src] = cands[0] ?? [Math.trunc(fallback), "fallback_ordinal"];
  return { ts: prev === null ? base : prev + 1, src, nudged: true };
}

/** One `{kind, source: "browser", ts_unix_ms, metrics}` row per window. */
export function benchRows(windows, fallbackT0Ms = 0) {
  const rows = [];
  let prev = null;
  windows.forEach((raw, i) => {
    if (raw === null || typeof raw !== "object") return;
    const w = { ...raw };
    for (const [old, now] of Object.entries(LEGACY_KEYS)) if (old in w && !(now in w)) { w[now] = w[old]; delete w[old]; }
    const { ts, nudged } = windowTs(w, prev, fallbackT0Ms + i * 1000);
    prev = ts;
    const m = {};
    for (const k of PLAIN_KEYS) if (num(w[k])) m[k === "n" || k === "decoded" ? `bench_${k}` : k] = w[k];
    for (const [k, v] of Object.entries(w)) if (k.startsWith("stage_") && num(v)) m[k] = v;
    const i2p = (w.i2p_ms ?? []).filter(num);
    if (i2p.length) Object.assign(m, { i2p_p50_ms: percentile(i2p, 0.5), i2p_max_ms: Math.max(...i2p), i2p_n: i2p.length });
    m.offset_unknown = w.offset_unknown ? 1 : 0;
    if (nudged) m.ts_nudged = 1;
    rows.push({ kind: "metric", source: "browser", ts_unix_ms: ts, metrics: m });
  });
  return rows;
}
