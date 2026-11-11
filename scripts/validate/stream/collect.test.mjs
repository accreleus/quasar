// node --test scripts/validate/stream/collect.test.mjs   (make verify runs it)
//
// A fake API as an in-process `fetch` — no sockets, no stack, no lab host. Its answers are the
// control plane's own shapes, each one commented with the handler it mirrors (values are
// synthetic). It honours `from`/`to` as resolveWindow does, answers empty once telemetry has
// been deleted, and records every URL it was asked for.
import assert from "node:assert/strict";
import crypto from "node:crypto";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";
import { validateBundle } from "./bundle.mjs";
import { EXIT, collect, main, redact } from "./collect.mjs";

const HERE = path.dirname(fileURLToPath(import.meta.url));
const ROOT = path.resolve(HERE, "../../..");
const DX = path.join(ROOT, "scripts/dx");
const SC = "01-stream-hold";
const SID = "11111111-2222-3333-4444-555555555555";
const SID2 = "11111111-2222-3333-4444-666666666666";
const HOST_ID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee";
const BASE = "https://stack.invalid";
const TOKEN = "admintoken-9f3c2b7a41";
const T_START = 1786939995000; // the client half's fixture rows sit just after this
const T_END = 1786940300000;
const TMP = fs.mkdtempSync(path.join(os.tmpdir(), "e2e-collect-"));
process.on("exit", () => fs.rmSync(TMP, { recursive: true, force: true }));

// A client-only bundle: the committed fixture (which already looks like a run directory), the
// DX suite's own series and trace, and no host half.
function clientBundle() {
  const dir = fs.mkdtempSync(path.join(TMP, "b-"));
  fs.cpSync(path.join(HERE, "fixtures/bundle"), dir, { recursive: true });
  for (const f of ["metrics.jsonl", "trace.json"]) {
    fs.copyFileSync(path.join(DX, "tests/fixtures/bench-run", f), path.join(dir, SC, f));
  }
  fs.rmSync(path.join(dir, SC, "session.json"), { force: true });
  edit(dir, "manifest.json", (m) => ({ ...m, halves: { client: true, host: false } }));
  return dir;
}

const read = (dir, rel) => fs.readFileSync(path.join(dir, rel), "utf8");
const readJson = (dir, rel) => JSON.parse(read(dir, rel));
const edit = (dir, rel, fn) => {
  const file = path.join(dir, rel);
  fs.writeFileSync(file, JSON.stringify(fn(JSON.parse(fs.readFileSync(file, "utf8")))));
};
const rows = (dir, rel) =>
  read(dir, rel)
    .split("\n")
    .filter((l) => l.trim())
    .map((l) => JSON.parse(l));
const walk = (dir, rel = "") =>
  fs.readdirSync(path.join(dir, rel), { withFileTypes: true }).flatMap((e) => {
    const p = path.join(rel, e.name);
    return e.isDirectory() ? walk(dir, p) : [p];
  });

// One hash over every path and byte in a tree: "the collector wrote nothing" as a value.
function treeHash(dir) {
  const h = crypto.createHash("sha256");
  for (const rel of walk(dir).sort()) {
    h.update(rel).update("\0").update(fs.readFileSync(path.join(dir, rel)));
  }
  return h.digest("hex");
}

// ── the fake control plane ──────────────────────────────────────────────────
const iso = (ms) => new Date(ms).toISOString();
const SAMPLE_CAP = 1000; // telemetry.clampLimit and session.traceReadLimit

function world(over = {}) {
  return {
    ids: [SID],
    now: T_END + 3_600_000, // collected an hour after the session ended
    start: T_START,
    end: T_END,
    pruned: false, // telemetry deleted after the retention period (telemetry/policy.go DefaultPostMortem)
    captures: [], // diag.* events: kept by retention (policy.go rule 3), so they outlive `pruned`
    hitches: [], // from_ms of the hitch windows the classifier would find
    clockOffsetMs: 2,
    // telemetry store rows, as Recent returns them (newest first once sorted)
    samples: [
      { source: "agent", ts_unix_ms: 1786940000000, metrics: { fps: 59.8, rtp_fps: 59.7, abr_mode: "smooth", adaptation_state: "healthy" } },
      { source: "agent", ts_unix_ms: 1786940101000, metrics: { fps: 59.8, encode_ms: 2.4, abr_mode: "smooth", adaptation_state: "healthy", not_a_manifest_key: 1 } },
      { source: "browser", ts_unix_ms: 1786940001000, metrics: { fps: 118, decode_ms: 3.1 } },
      { source: "native", ts_unix_ms: 1786940002000, metrics: { fps: 1 } },
    ],
    events: [
      { source: "agent", ts_unix_ms: 1786940102000, type: "abr.ladder.step", payload: { peer: "10.20.30.40", note: `echoed Bearer ${TOKEN}` } },
      { source: "browser", ts_unix_ms: 1786940103000, type: "client.stall", payload: {} },
    ],
    // crud/handler.go hostToResp (only the identity fields matter here)
    host: { id: HOST_ID, node_name: "lab-gpu-host", agent_version: "0.6.1-dev", source_commit: "abc1234", built_at: "2026-11-10T09:00:00Z" },
    // platform/handler.go identityResponse
    identity: { version: "0.6.1-dev", source_commit: "f00dfeed", built_at: "2026-11-10T08:00:00Z", schema_version: 123 },
    // hostcfg/store.go GetEffective: map[string]string — values are strings, never booleans
    effective: { abr_mode: "smooth", abr_ladder_resolution: "true", abr_ladder: "TRUE", abr_ladder_fps: "False", abr_floor_kbps: "4000" },
    rowExtra: {}, // more fields on the session row (failure_code, error_message, app_log_tail ...)
    clock: null, // replaces the diagnostic bundle's clock
    redirect: () => false,
    tweak: () => undefined, // (pathname, body) => [status, body] to replace an answer
    onRequest: () => {},
    ...over,
  };
}

// session.sessionResp (handler.go): the fields the collector reads, in the real shape.
function sessionRow(w, id) {
  return {
    id,
    user_id: "99999999-0000-0000-0000-000000000001",
    app_id: "99999999-8888-7777-6666-555555555555",
    host_id: HOST_ID,
    state: "stopped",
    kind: "play",
    profile_id: "1080p60-h264",
    stream_profile_id: "1080p60-h264",
    // streamResp: width/height/fps/bitrate_kbps/codec, rungs always serialized
    stream: { width: 1920, height: 1080, fps: 60, bitrate_kbps: 12000, codec: "h264", rungs: [[1920, 1080], [1280, 720]], external_resize_supported: true },
    negotiated_codec: "h264",
    transport: "webtransport",
    pyrowave: { allowed: true, reason: null, wanted: false, choice: null, auto_reason: null, active: false, used: false, target_kbps: null, switch_reason: null },
    created_at: iso(w.start ?? w.end),
    started_at: w.start == null ? null : iso(w.start),
    ended_at: w.end == null ? null : iso(w.end),
    ...w.rowExtra,
  };
}

// diagnostic_handler.go resolveWindow, ported: explicit from/to are honoured and clamped to
// [2 min, 10 min]; with neither it is the 5 minutes ending NOW.
function resolveWindow(q, now) {
  if (!q.get("from") && !q.get("to")) return [now - 300_000, now];
  let to = Number(q.get("to"));
  let from = Number(q.get("from"));
  if (!(to > 0)) to = now;
  if (!(from > 0) || from >= to) from = to - 300_000;
  const span = to - from;
  if (span < 120_000) from = to - 120_000;
  if (span > 600_000) from = to - 600_000;
  return [from, to];
}

const TAXONOMY = { agent: { fps: "encoder.fps", encode_ms: "encoder.encode_ms" }, browser: { fps: "client.fps", decode_ms: "client.decode_ms" } };

function fake(w = world()) {
  const fn = async (url, init) => {
    fn.calls.push(url);
    fn.inits.push(init);
    assert.equal(init.headers.Authorization, `Bearer ${TOKEN}`, "every request carries the bearer");
    const u = new URL(url);
    const route = u.pathname;
    w.onRequest(route);
    if (w.redirect(route)) {
      if (init.redirect === "error") throw new TypeError("fetch failed", { cause: new Error("unexpected redirect") });
      return { status: 302, json: async () => ({}) };
    }
    const [status, body] = w.tweak(route, answer(route, u.searchParams)) ?? answer(route, u.searchParams);
    return { status, json: async () => body };
  };
  function answer(route, q) {
    const m = route.match(/^\/v1\/(?:admin\/)?sessions\/([^/]+)(?:\/(.*))?$/);
    if (m && !w.ids.includes(m[1])) return [404, { error: "not_found" }];
    const live = (list) => (w.pruned ? [] : list);
    if (m && m[2] === undefined) return [200, { session: sessionRow(w, m[1]) }]; // handler.go handleGet
    if (m && m[2] === "metrics") {
      // metrics_handler.go handleAdminMetrics: newest first, offset cursor, limit clamped to 1000
      const all = live(w.samples).slice().sort((a, b) => b.ts_unix_ms - a.ts_unix_ms);
      const limit = Math.min(Number(q.get("limit")) || 100, SAMPLE_CAP);
      const offset = Number(q.get("cursor") || 0);
      const items = all.slice(offset, offset + limit);
      return [200, { items, next_cursor: offset + limit < all.length ? String(offset + limit) : null }];
    }
    if (m && (m[2] === "diagnostic-bundle" || m[2] === "trace/events")) {
      const [from, to] = resolveWindow(q, w.now);
      const inWin = (ts) => ts >= from && ts <= to;
      const events = [...live(w.events), ...w.captures].filter((e) => inWin(e.ts_unix_ms)).sort((a, b) => a.ts_unix_ms - b.ts_unix_ms).slice(-SAMPLE_CAP);
      if (m[2] === "trace/events") return [200, { window: { from_ms: from, to_ms: to }, events }]; // projectionEvents: raw
      // handleDiagnosticBundle: Window() reads the newest 1000 raw samples of the window across ALL
      // sources (samplesInWindow), then assess() turns them into series keyed by taxonomy name, with
      // browser points shifted onto the host clock (telemetry/align.go), events aligned the same way
      const taken = live(w.samples).filter((x) => inWin(x.ts_unix_ms)).sort((a, b) => b.ts_unix_ms - a.ts_unix_ms).slice(0, SAMPLE_CAP);
      const series = {};
      for (const smp of taken) {
        for (const [k, name] of Object.entries(TAXONOMY[smp.source] ?? {})) {
          const shift = smp.source === "browser" ? w.clockOffsetMs : 0;
          if (typeof smp.metrics[k] === "number") (series[name] ??= []).push({ ts_unix_ms: smp.ts_unix_ms + shift, v: smp.metrics[k] });
        }
      }
      for (const name of Object.keys(series)) series[name].sort((a, b) => a.ts_unix_ms - b.ts_unix_ms);
      const clock = w.clock ?? (w.pruned
        ? { unmeasured: true }
        : { client_offset_ms: w.clockOffsetMs, uncertainty_ms: 1, measured_at: iso(w.start + 10_000) });
      return [
        200,
        {
          trace: { session_id: m[1], host_id: HOST_ID, profile_id: "1080p60-h264", started_at: iso(w.start), ended_at: iso(w.end) },
          window: { from_ms: from, to_ms: to },
          clock,
          abr_mode: "smooth",
          series,
          events: events.map((e) => (e.source === "browser" ? { ...e, ts_unix_ms: e.ts_unix_ms + w.clockOffsetMs } : e)),
          // derived windows, the adaptation labels and the classifier are all computed over THIS read's window
          derived_windows: {
            hitches: w.hitches.filter(inWin).map((f) => ({ from_ms: f, to_ms: f + 100, present_interval_sd_ms: 5 })),
            abr_downshifts: [],
            encoder_saturation: [],
            likely_network_congestion: [],
          },
          agent_adaptation: taken.filter((x) => x.source === "agent" && x.metrics.adaptation_state).map((x) => ({ ts_unix_ms: x.ts_unix_ms, state: x.metrics.adaptation_state })).sort((a, b) => a.ts_unix_ms - b.ts_unix_ms),
          classifier: { verdict: "healthy", window: { from_ms: from, to_ms: to }, clock: { age_ms: w.now - (w.start + 10_000) } }, // verdict.go: moves with the real clock
          captures: [],
        },
      ];
    }
    if (route === `/v1/admin/hosts/${HOST_ID}/settings`) {
      // hostcfg/handler.go handleGet
      return [200, { resolved: {}, overrides: {}, effective: w.effective, codecs: ["h264", "h265", "av1"], pending_restart: false }];
    }
    if (route === `/v1/hosts/${HOST_ID}`) return [200, { host: w.host }]; // crud/handler.go handleGetHost: ENVELOPED
    if (route === "/v1/admin/platform/identity") return [200, { identity: w.identity }];
    return [404, { error: "not_found" }];
  }
  fn.calls = [];
  fn.inits = [];
  fn.paths = () => fn.calls.map((u) => new URL(u).pathname);
  return fn;
}

/** Run the CLI with `console` captured; returns its exit code and lines. */
async function cli(argv, fetchFn, env = { QUASAR_ADMIN_TOKEN: TOKEN }) {
  const out = [];
  const errs = [];
  const [log, error] = [console.log, console.error];
  console.log = (m) => out.push(String(m));
  console.error = (m) => errs.push(String(m));
  try {
    return { code: await main(argv, { fetch: fetchFn, env }), out, errs };
  } finally {
    console.log = log;
    console.error = error;
  }
}

const clientBundle2 = () => {
  const dir = clientBundle();
  fs.cpSync(path.join(dir, SC), path.join(dir, "02-second"), { recursive: true });
  edit(dir, "02-second/scenario.json", (s) => ({ ...s, id: "02-second", sessions: [SID2] }));
  edit(dir, "manifest.json", (m) => ({ ...m, scenarios: [SC, "02-second"] }));
  return dir;
};
const run = (dir, w = world(), extra = {}) => {
  const f = fake(w);
  return collect(dir, { baseUrl: BASE, token: TOKEN, fetch: f, now: () => w.now, ...extra }).then((r) => Object.assign(r, { f }));
};

// ── what a collection writes ────────────────────────────────────────────────
test("a client-only bundle comes out complete and valid", async () => {
  const dir = clientBundle();
  const r = await run(dir);
  assert.equal(r.complete, true);
  assert.deepEqual(r.problems, []);
  assert.deepEqual(r.incomplete, []);

  const manifest = readJson(dir, "manifest.json");
  assert.equal(manifest.halves.host, true);
  assert.equal(manifest.halves.client, true);

  const session = readJson(dir, `${SC}/session.json`);
  assert.deepEqual(session.launch, [1920, 1080]);
  assert.equal(session.fps, 60);
  assert.equal(session.codec, "h264");
  assert.equal(session.duration_s, 305);
  assert.deepEqual(session.rungs, [[1920, 1080], [1280, 720]]);
  // S4: the fields the 0.6 parity gate compares, named as the row names them
  assert.equal(session.transport, "webtransport");
  assert.equal(session.negotiated_codec, "h264");
  assert.equal(session.pyrowave.allowed, true);

  // The trace keeps the client half's events and adds the host's raw ones, unshifted.
  const trace = readJson(dir, `${SC}/trace.json`);
  assert.ok(trace.events.some((e) => e.type === "client.freeze_detected"));
  assert.ok(trace.events.some((e) => e.type === "abr.ladder.step"));
  assert.equal(trace.events.find((e) => e.type === "client.stall").ts_unix_ms, 1786940103000, "a browser event keeps its own clock");
  assert.equal(trace.session_id, SID);

  assert.ok(readJson(dir, `${SC}/diagnostic-bundle.json`).series["encoder.fps"], "the diagnostic bundle is kept as the artifact");

  const builds = readJson(dir, "builds.json");
  assert.deepEqual(builds.components.control_plane, { version: "0.6.1-dev", commit: "f00dfeed", built_at: "2026-11-10T08:00:00.000Z" });
  assert.deepEqual(builds.components.node_agent, { version: "0.6.1-dev", commit: "abc1234", built_at: "2026-11-10T09:00:00.000Z" });
  assert.equal(builds.components.reference_app.version, "767e2ad", "the client half's builds are untouched");
  assert.ok(builds.unknown.web, "their unknown reasons are untouched");
  assert.equal(readJson(dir, `${SC}/host-half.json`).not_projected, undefined, "a normal answer leaves nothing out");
});

test("S1 metrics.jsonl is built from the raw samples: every key, strings too, original timestamps", async () => {
  const dir = clientBundle();
  await run(dir);
  const metrics = rows(dir, `${SC}/metrics.jsonl`);
  const added = metrics.find((r) => r.source === "agent" && r.ts_unix_ms === 1786940101000);
  assert.equal(added.metrics.abr_mode, "smooth", "a string-valued key survives");
  assert.equal(added.metrics.adaptation_state, "healthy");
  assert.equal(added.metrics.encode_ms, 2.4);
  const browser = metrics.find((r) => r.source === "browser" && r.ts_unix_ms === 1786940001000);
  assert.equal(browser.metrics.decode_ms, 3.1, "the browser sample sits at its own, unshifted timestamp");
  assert.ok(!metrics.some((r) => r.source === "browser" && r.ts_unix_ms === 1786940001000 + 2), "not moved onto the host clock");
  const sorted = [...metrics].sort((a, b) => a.ts_unix_ms - b.ts_unix_ms || (a.source < b.source ? -1 : 1));
  assert.deepEqual(metrics, sorted);

  // A key the manifest lacks and a source the format lacks are dropped and listed, never written.
  assert.ok(!metrics.some((r) => "not_a_manifest_key" in r.metrics));
  assert.ok(!metrics.some((r) => r.source === "native"));
  const half = readJson(dir, `${SC}/host-half.json`);
  assert.deepEqual(half.metric_keys_not_in_manifest, ["agent.not_a_manifest_key"]);
  assert.deepEqual(half.sources_not_in_bundle, ["native"]);
  assert.deepEqual(validateBundle(dir), []);
});

test("S2 a host sample joins the client's row per key: the client wins a key both have", async () => {
  const dir = clientBundle();
  await run(dir);
  const metrics = rows(dir, `${SC}/metrics.jsonl`);
  const browser = metrics.filter((r) => r.source === "browser" && r.ts_unix_ms === 1786940001000);
  assert.equal(browser.length, 1, "one row for one source and ts");
  assert.equal(browser[0].metrics.fps, 119, "the client's value wins for fps");
  assert.equal(browser[0].metrics.decode_ms, 3.1, "the host's key the client lacks is added");
  const agent = metrics.find((r) => r.source === "agent" && r.ts_unix_ms === 1786940000000);
  assert.equal(agent.metrics.fps, 119.4);
  assert.equal(agent.metrics.rtp_fps, 59.7);
});

test("M3 the effective settings are strings: 'true'/'false' in any case read on/off, as bench_run.sh's flag() does", async () => {
  const dir = clientBundle();
  await run(dir);
  const { effective } = readJson(dir, `${SC}/conditions.json`);
  assert.equal(effective.abr_ladder_resolution, "on");
  assert.equal(effective.ladder_resolution, "on", "the alias carries the same projection");
  assert.equal(effective.abr_ladder, "on", "TRUE in any case");
  assert.equal(effective.abr_ladder_fps, "off", "False in any case");
  assert.equal(effective.ladder_fps, "off");
  assert.equal(effective.abr_floor_kbps, "4000");
  assert.equal(effective.abr_mode, "smooth");

  const none = clientBundle();
  await run(none, world({ effective: null }));
  assert.match(readJson(none, `${SC}/conditions.json`).unknown.effective, /not reported/);
});

test("M2 the host row is enveloped: an identity with no field is 'not read', never {}", async () => {
  const dir = clientBundle();
  const w = world({ host: { id: HOST_ID, node_name: "lab-gpu-host", agent_version: null, source_commit: null, built_at: null } });
  const r = await run(dir, w);
  const builds = readJson(dir, "builds.json");
  assert.equal(builds.components.node_agent, null);
  assert.match(builds.unknown.node_agent, /did not share one readable host agent build/);
  assert.deepEqual(r.problems, []);

  const ok = clientBundle();
  await run(ok);
  assert.equal(readJson(ok, "builds.json").components.node_agent.version, "0.6.1-dev", "read from body.host");
});

test("S3 when the settings and the builds were read is recorded", async () => {
  const dir = clientBundle();
  const w = world();
  await run(dir, w);
  assert.equal(readJson(dir, `${SC}/host-half.json`).read_at_ms, w.now);
});

// ── truth comes from the control plane's code ───────────────────────────────
test("M1 the trace is asked for with the session's own window, in reads the server allows", async () => {
  const dir = clientBundle();
  const r = await run(dir);
  const bundles = r.f.calls.filter((u) => u.includes("/diagnostic-bundle"));
  assert.equal(bundles.length, 1);
  const q = new URL(bundles[0]).searchParams;
  assert.equal(Number(q.get("from")), T_START - 5000, "from = started_at - 5 s");
  assert.equal(Number(q.get("to")), T_END + 5000, "to = ended_at + 5 s");
  assert.ok(r.f.calls.filter((u) => u.includes("/trace/events")).every((u) => u.includes("from=") && u.includes("to=")));
});

test("M1 a session whose telemetry has been deleted is not collected, never recorded as empty", async () => {
  const dir = clientBundle();
  const before = read(dir, `${SC}/metrics.jsonl`);
  const r = await run(dir, world({ pruned: true }));
  assert.equal(r.complete, false);
  assert.deepEqual(r.problems, []);
  assert.match(r.incomplete[0].reason, /the trace is no longer kept/);
  const half = readJson(dir, `${SC}/host-half.json`);
  assert.equal(half.collected, false);
  assert.equal(readJson(dir, "manifest.json").halves.host, false);
  assert.equal(read(dir, `${SC}/metrics.jsonl`), before);
  assert.ok(!fs.existsSync(path.join(dir, SC, "diagnostic-bundle.json")));
});

test("M1 a read the server fills is split, not trusted", async () => {
  const dir = clientBundle();
  // 1200 agent samples in a 300 s session: the first read (a 10-minute window) comes back at the 1000-point cap.
  const samples = Array.from({ length: 1200 }, (_, i) => ({ source: "agent", ts_unix_ms: T_START + 1000 + i * 250, metrics: { fps: 60 + (i % 7) } }));
  const r = await run(dir, world({ samples }));
  assert.equal(r.complete, true);
  assert.ok(r.f.calls.filter((u) => u.includes("/diagnostic-bundle")).length > 1, "it asked again for narrower windows");
  assert.equal(readJson(dir, `${SC}/diagnostic-bundle.json`).series["encoder.fps"].length, 1200, "no point was lost to the cap");
  assert.ok(r.f.calls.filter((u) => u.includes("/metrics?")).length === 2, "samples are paged until the cursor ends");
  assert.equal(rows(dir, `${SC}/metrics.jsonl`).filter((x) => x.source === "agent" && x.ts_unix_ms > T_START).length, 1200);

  // A window too small to split further is a reason, not a silent truncation.
  const tiny = clientBundle();
  const dense = Array.from({ length: 1200 }, (_, i) => ({ source: "agent", ts_unix_ms: T_START + 1000 + i * 20, metrics: { fps: 60 } }));
  const t = await run(tiny, world({ samples: dense, end: T_START + 60_000 }));
  assert.equal(t.complete, false);
  assert.match(t.incomplete[0].reason, /denser than one read holds/);
});

// ── idempotence: a collected scenario is never fetched again ────────────────
test("I2 under --resume a collected scenario is not fetched again, and a second run with other answers changes no byte of it", async () => {
  const dir = clientBundle();
  await run(dir);
  const first = treeHash(dir);
  const other = world({
    effective: { abr_mode: "protective" },
    identity: { version: "9.9.9", source_commit: "deadbeef", built_at: "2027-01-01T00:00:00Z" },
    host: { id: HOST_ID, agent_version: "9.9.9", source_commit: "cafe", built_at: "2027-01-01T00:00:00Z" },
    events: [],
    pruned: true,
  });
  const r = await run(dir, other, { resume: true });
  assert.equal(r.f.calls.length, 0, "not one request is made");
  assert.equal(r.complete, true);
  assert.equal(treeHash(dir), first, "not one byte of the bundle changed");
});

test("I2 under --resume a first-run failure is retried, and the scenario that was collected is left alone", async () => {
  const dir = clientBundle();
  // a second scenario with its own session
  fs.cpSync(path.join(dir, SC), path.join(dir, "02-second"), { recursive: true });
  edit(dir, "02-second/scenario.json", (s) => ({ ...s, id: "02-second", sessions: [SID2] }));
  edit(dir, "manifest.json", (m) => ({ ...m, scenarios: [SC, "02-second"] }));

  const failing = world({ ids: [SID, SID2], tweak: (route) => (route.includes(SID2) && route.endsWith("/metrics") ? [500, {}] : undefined) });
  const r1 = await run(dir, failing);
  assert.equal(r1.complete, false);
  assert.equal(r1.incomplete[0].scenario, "02-second");
  assert.equal(readJson(dir, `${SC}/host-half.json`).collected, true);
  assert.equal(readJson(dir, "02-second/host-half.json").collected, false);
  assert.equal(readJson(dir, "manifest.json").halves.host, false);
  const kept = treeHash(path.join(dir, SC));

  const r2 = await run(dir, world({ ids: [SID, SID2], effective: { abr_mode: "protective" } }), { resume: true });
  assert.equal(r2.complete, true);
  assert.ok(r2.f.calls.every((u) => !u.includes(SID + "/") && !u.endsWith(SID)), "the collected session is not asked for");
  assert.ok(r2.f.calls.some((u) => u.includes(SID2)), "the failed one is retried");
  assert.equal(treeHash(path.join(dir, SC)), kept, "the collected scenario is byte-identical");
  assert.equal(readJson(dir, "02-second/host-half.json").collected, true);
  assert.equal(readJson(dir, "manifest.json").halves.host, true);
  assert.equal(readJson(dir, "builds.json").components.node_agent.version, "0.6.1-dev");
  assert.deepEqual(validateBundle(dir), []);
});

// ── one write function: redaction, replacement, confinement ─────────────────
test("A1 the bearer is in no file, whatever field an answer put it in (builds.json included)", async () => {
  const dir = clientBundle();
  const w = world({
    identity: { version: TOKEN, source_commit: TOKEN, built_at: TOKEN },
    host: { id: HOST_ID, agent_version: TOKEN, source_commit: `x${TOKEN}y`, built_at: TOKEN },
  });
  const r = await run(dir, w);
  for (const rel of walk(dir)) assert.ok(!read(dir, rel).includes(TOKEN), `${rel} holds the bearer`);
  assert.deepEqual(r.problems, []);
});

test("A2 credential names are matched after normalising case, hyphens and camelCase; an address is replaced", async () => {
  const dir = clientBundle();
  const payload = {
    authorization: "credential-from-authorization",
    accessToken: "credential-from-access-token",
    Cookie: "credential-from-cookie",
    "Set-Cookie": "credential-from-set-cookie",
    sigToken: "credential-from-sig-token",
    "x-api-key": "credential-from-x-api-key",
    "private key": "credential-from-private-key",
    PASSWORD: "credential-from-password",
    keep: "kept",
    peer: "http://10.20.30.40:8080/path",
  };
  const w = world({ events: [{ source: "agent", ts_unix_ms: 1786940102500, type: "abr.ladder.step", payload }] });
  const r = await run(dir, w);
  for (const rel of [`${SC}/trace.json`, `${SC}/diagnostic-bundle.json`]) {
    assert.doesNotMatch(read(dir, rel), /credential-from/);
  }
  const ev = readJson(dir, `${SC}/trace.json`).events.find((e) => e.ts_unix_ms === 1786940102500);
  assert.deepEqual(Object.keys(ev.payload).sort(), ["keep", "peer"]);
  assert.match(ev.payload.peer, /\[address\]/);
  assert.deepEqual(r.problems, []);
});

test("A2 a value from an answer is never echoed into a reason", async () => {
  const dir = clientBundle();
  const r = await run(dir, world({ tweak: (route, body) => (route === `/v1/sessions/${SID}` ? [200, { session: { ...body[1].session, host_id: "authorization=secret-value-1234" } }] : undefined) }));
  assert.equal(r.complete, false);
  assert.match(r.incomplete[0].reason, /host id is not a UUID/);
  assert.doesNotMatch(read(dir, `${SC}/host-half.json`), /secret-value/);
});

test("prototype pollution: a __proto__ key in an answer reaches no prototype and is kept as a key", () => {
  const evil = JSON.parse('{"__proto__":{"polluted":"yes"},"a":{"__proto__":{"polluted":"deep"}},"constructor":{"x":1}}');
  const out = redact(evil, TOKEN);
  assert.equal({}.polluted, undefined);
  assert.equal(out.polluted, undefined);
  assert.equal(Object.getPrototypeOf(out), null);
  assert.deepEqual(Object.keys(out).sort(), ["__proto__", "a", "constructor"]);
});

test("A3 a hard link already at a target is replaced, not written through (--resume: a diagnostic-bundle.json is otherwise a D1 claim)", async () => {
  const dir = clientBundle();
  const outside = path.join(TMP, `outside-${crypto.randomUUID()}.txt`);
  fs.writeFileSync(outside, "an outside file\n");
  fs.linkSync(outside, path.join(dir, SC, "diagnostic-bundle.json"));
  assert.deepEqual(validateBundle(dir), [], "a hard link is a regular file to the validator");
  await run(dir, world(), { resume: true });
  assert.equal(fs.readFileSync(outside, "utf8"), "an outside file\n", "the outside inode is unchanged");
  assert.ok(readJson(dir, `${SC}/diagnostic-bundle.json`).series);
});

test("A3 a symlink put at a target after validation is replaced, not followed", async () => {
  const dir = clientBundle();
  const outside = path.join(TMP, `outside-${crypto.randomUUID()}.txt`);
  fs.writeFileSync(outside, "an outside file\n");
  const w = world({
    onRequest: (route) => {
      if (route.endsWith("/settings")) fs.symlinkSync(outside, path.join(dir, SC, "conditions.json"));
    },
  });
  await run(dir, w);
  assert.equal(fs.readFileSync(outside, "utf8"), "an outside file\n", "the symlink's target is unchanged");
  assert.ok(readJson(dir, `${SC}/conditions.json`).effective, "the link itself was replaced by a file");
  assert.ok(fs.lstatSync(path.join(dir, SC, "conditions.json")).isFile());
});

// ── a bundle is untrusted input ─────────────────────────────────────────────
test("A4 a scenario's sessions must be a list of strings, refused before any request", async () => {
  for (const bad of [{ zero: SID }, SID, null, [SID, 7]]) {
    const dir = clientBundle();
    edit(dir, `${SC}/scenario.json`, (s) => ({ ...s, sessions: bad }));
    const before = treeHash(dir);
    const api = fake();
    const r = await cli([dir, "--base-url", BASE], api);
    assert.equal(r.code, EXIT.invalid, JSON.stringify(bad));
    assert.equal(api.calls.length, 0, "no request");
    assert.equal(treeHash(dir), before, "no write");
    assert.ok(r.errs.some((m) => /sessions must be a list of strings/.test(m)));
  }
  // An empty list is valid and means: nothing to collect for this scenario.
  const dir = clientBundle();
  edit(dir, `${SC}/scenario.json`, (s) => ({ ...s, sessions: [] }));
  const r = await run(dir);
  assert.equal(r.complete, true);
  assert.equal(r.f.calls.length, 0);
  assert.deepEqual(readJson(dir, `${SC}/host-half.json`), { collected: true });
});

test("A5 a wrong-shaped answer is that scenario's reason, never a TypeError, and nothing of it is written", async () => {
  const shapes = {
    "events is an object": (route, body) => (route.endsWith("/trace/events") ? [200, { ...body[1], events: { not: "an array" } }] : undefined),
    "diagnostic events is an object": (route, body) => (route.endsWith("/diagnostic-bundle") ? [200, { ...body[1], events: { not: "an array" } }] : undefined),
    "series is not points": (route, body) => (route.endsWith("/diagnostic-bundle") ? [200, { ...body[1], series: { "encoder.fps": [null] } }] : undefined),
    "an empty diagnostic answer": (route) => (route.endsWith("/diagnostic-bundle") ? [200, {}] : undefined),
    "rungs hold a null": (route, body) => (route === `/v1/sessions/${SID}` ? [200, { session: { ...body[1].session, stream: { ...body[1].session.stream, rungs: [null] } } }] : undefined),
    "a timestamp is not a time": (route, body) => (route === `/v1/sessions/${SID}` ? [200, { session: { ...body[1].session, started_at: "yesterday" } }] : undefined),
    "the row is another session": (route, body) => (route === `/v1/sessions/${SID}` ? [200, { session: { ...body[1].session, id: SID2 } }] : undefined),
    "metrics items are not samples": (route) => (route.endsWith("/metrics") ? [200, { items: [null], next_cursor: null }] : undefined),
    "the host is not enveloped": (route, body) => (route === `/v1/hosts/${HOST_ID}` ? [200, body[1].host] : undefined),
  };
  for (const [what, tweak] of Object.entries(shapes)) {
    const dir = clientBundle();
    const files = new Set(walk(dir));
    const r = await run(dir, world({ tweak })); // tweak(route, [status, body]) is given the answer about to be sent
    assert.equal(r.complete, false, what);
    assert.equal(r.incomplete.length, 1, what);
    assert.deepEqual(
      walk(dir).filter((f) => !files.has(f)),
      [path.join(SC, "host-half.json")],
      `${what}: only the not-collected record was written`,
    );
    assert.equal(readJson(dir, `${SC}/host-half.json`).collected, false, what);
  }
});

test("A5 existing files the collector merges with are shape-checked before any request", async () => {
  const cases = {
    "trace.json holds null": (dir) => fs.writeFileSync(path.join(dir, SC, "trace.json"), "null"),
    "trace.json events is not a list": (dir) => edit(dir, `${SC}/trace.json`, (t) => ({ ...t, events: "x" })),
    "sessions is a file": (dir) => {
      edit(dir, `${SC}/scenario.json`, (s) => ({ ...s, sessions: [SID, SID2] }));
      fs.writeFileSync(path.join(dir, SC, "sessions"), "a file where a directory goes");
    },
    "an output is a directory": (dir) => fs.mkdirSync(path.join(dir, SC, "session.json")),
  };
  for (const [what, setup] of Object.entries(cases)) {
    const dir = clientBundle();
    setup(dir);
    const before = treeHash(dir);
    const api = fake();
    const r = await cli([dir, "--base-url", BASE], api);
    assert.equal(r.code, EXIT.invalid, what);
    assert.equal(api.calls.length, 0, `${what}: zero requests`);
    assert.equal(treeHash(dir), before, `${what}: zero writes`);
  }
});

test("A6 no redirect is followed: every request says so, and a 3xx is that scenario's failure", async () => {
  const dir = clientBundle();
  const r = await run(dir, world({ redirect: (route) => route.endsWith("/diagnostic-bundle") }));
  assert.ok(r.f.inits.length > 0 && r.f.inits.every((i) => i.redirect === "error"));
  assert.equal(r.complete, false);
  assert.match(r.incomplete[0].reason, /redirect/);
  assert.ok(!r.f.paths().some((p) => p.includes("redirected")), "no second request was steered by it");
});

test("A7 a failed identity read replaces whatever the client half held, with its reason", async () => {
  const dir = clientBundle();
  edit(dir, "builds.json", (b) => ({ ...b, components: { ...b.components, control_plane: { version: "stale", commit: "stale" }, node_agent: { version: "stale" } } }));
  const r = await run(dir, world({ tweak: (route) => (route === "/v1/admin/platform/identity" ? [500, {}] : undefined), host: { id: HOST_ID } }));
  const builds = readJson(dir, "builds.json");
  assert.equal(builds.components.control_plane, null);
  assert.match(builds.unknown.control_plane, /HTTP 500/);
  assert.equal(builds.components.node_agent, null);
  assert.ok(builds.unknown.node_agent);
  assert.deepEqual(r.problems, []);
});

test("a session id that is not a UUID is refused before any request", async () => {
  const dir = clientBundle();
  edit(dir, `${SC}/scenario.json`, (s) => ({ ...s, sessions: ["../../admin/users"] }));
  const before = treeHash(dir);
  const api = fake();

  const r = await cli([dir, "--base-url", BASE], api);
  assert.equal(r.code, EXIT.error, "a usage-class failure is exit 2");
  assert.equal(api.calls.length, 0, "no admin-authenticated request is made at all");
  assert.equal(treeHash(dir), before, "the bundle is untouched");
  assert.ok(r.errs.some((m) => m.includes(SC) && /is not a session id/.test(m)), "the message names the scenario");

  const long = "../../" + "x".repeat(200);
  edit(dir, `${SC}/scenario.json`, (s) => ({ ...s, sessions: [long] }));
  const again = await cli([dir, "--base-url", BASE], fake());
  assert.equal(again.code, EXIT.error);
  assert.ok(again.errs.every((m) => !m.includes(long)), "a 200-character id is not echoed whole");
});

test("a host id from the API that is not a UUID is recorded as not collected, never requested", async () => {
  const dir = clientBundle();
  const w = world({ tweak: (route, body) => (route === `/v1/sessions/${SID}` ? [200, { session: { ...body[1].session, host_id: "../../admin/users" } }] : undefined) });
  const f = fake(w);
  const r = await collect(dir, { baseUrl: BASE, token: TOKEN, fetch: f, now: () => w.now });
  assert.equal(r.complete, false);
  assert.ok(f.paths().every((p) => !p.startsWith("/v1/admin/hosts/") && !p.startsWith("/v1/hosts/")), "the answer steered no request");
  assert.ok(f.calls.every((u) => !u.includes("..")));
  assert.match(readJson(dir, `${SC}/host-half.json`).reason, /host id is not a UUID/);
});

test("a manifest scenario name that leaves the bundle is refused before any request or write", async () => {
  const dir = clientBundle();
  const outside = path.join(path.dirname(dir), `outside-${path.basename(dir)}`);
  fs.mkdirSync(outside, { recursive: true });
  fs.writeFileSync(path.join(outside, "scenario.json"), read(dir, `${SC}/scenario.json`));
  fs.writeFileSync(path.join(outside, "keep.txt"), "not a bundle\n");
  edit(dir, "manifest.json", (m) => ({ ...m, scenarios: [`../${path.basename(outside)}`] }));
  const before = treeHash(dir);
  const outsideBefore = treeHash(outside);
  const api = fake();

  const r = await cli([dir, "--base-url", BASE], api);
  assert.equal(r.code, EXIT.invalid);
  assert.equal(api.calls.length, 0);
  assert.match(r.out.join("\n"), /reason=invalid-before-collect/);
  assert.equal(treeHash(dir), before);
  assert.equal(treeHash(outside), outsideBefore);
});

test("a scenario directory that is a symlink out of the bundle is refused before any request", async () => {
  const dir = clientBundle();
  const target = path.join(path.dirname(dir), `linked-${path.basename(dir)}`);
  fs.mkdirSync(target, { recursive: true });
  fs.writeFileSync(path.join(target, "scenario.json"), read(dir, `${SC}/scenario.json`));
  fs.rmSync(path.join(dir, SC), { recursive: true });
  fs.symlinkSync(target, path.join(dir, SC), "dir");
  const outsideBefore = treeHash(target);
  const api = fake();

  const r = await cli([dir, "--base-url", BASE], api);
  assert.equal(r.code, EXIT.invalid);
  assert.equal(api.calls.length, 0);
  assert.equal(treeHash(target), outsideBefore);
});

// ── the command ─────────────────────────────────────────────────────────────
test("a session the stack no longer holds is recorded as not collected, and the run is incomplete (exit 3)", async () => {
  const dir = clientBundle();
  const w = world({ ids: [] });
  const r = await cli([dir, "--base-url", BASE], fake(w));
  assert.equal(r.code, EXIT.incomplete);
  assert.match(r.errs.join("\n"), /404/);
  assert.equal(readJson(dir, `${SC}/host-half.json`).collected, false);
  assert.equal(readJson(dir, "manifest.json").halves.host, false);
  assert.equal(fs.existsSync(path.join(dir, SC, "session.json")), false);
});

test("a rejected bearer stops the run and names the ladder", async () => {
  const dir = clientBundle();
  const denied = fake(world({ tweak: (route) => (route.endsWith("/diagnostic-bundle") ? [401, {}] : undefined) }));
  await assert.rejects(collect(dir, { baseUrl: BASE, token: TOKEN, fetch: denied, now: () => T_END }), (e) => {
    assert.equal(e.code, "unauthorized");
    assert.match(e.message, /scripts\/dx\/admin_token\.sh/);
    return true;
  });
  const r = await cli([dir, "--base-url", BASE], denied);
  assert.equal(r.code, EXIT.error);
  assert.ok(r.errs.some((m) => /admin_token\.sh/.test(m)));
});

test("the command: 0 collected, 1 invalid bundle, 2 usage", async () => {
  const dir = clientBundle();
  const ok = await cli([dir, "--base-url", BASE], fake());
  assert.equal(ok.code, EXIT.ok);
  assert.match(ok.out.at(-1), /^RESULT status=ok target=e2e-collect scenarios=1 sessions=1 collected=1$/);
  assert.equal((await cli([dir], fake())).code, EXIT.error);
  const noToken = await cli([dir, "--base-url", BASE], fake(), {});
  assert.equal(noToken.code, EXIT.error);
  assert.ok(noToken.errs.some((m) => /QUASAR_ADMIN_TOKEN/.test(m)));

  const broken = clientBundle();
  edit(broken, "manifest.json", (m) => ({ ...m, format: 2 }));
  assert.equal((await cli([broken, "--base-url", BASE], fake())).code, EXIT.invalid);
});

// ── D1: a bundle that already claims a host half is not collected by default ─
test("D1 a second default run is the refusal: exit 2, no request, no write; --resume finishes", async () => {
  const dir = clientBundle();
  await run(dir);
  const done = treeHash(dir);
  const api = fake();
  const r = await cli([dir, "--base-url", BASE], api);
  assert.equal(r.code, EXIT.error);
  assert.equal(api.calls.length, 0);
  assert.equal(treeHash(dir), done);
  assert.match(r.errs.join("\n"), /already has a host half.*--resume/);
  assert.match(r.out.at(-1), /^RESULT status=error target=e2e-collect reason=host-half-present$/);
  const again = await cli([dir, "--base-url", BASE, "--resume"], api);
  assert.equal(again.code, EXIT.ok);
  assert.equal(api.calls.length, 0, "a collected scenario is not fetched again");
});

test("D1 a bundle's own claim is never believed by default: a forged marker, a stray file, halves.host", async () => {
  const claims = {
    "a forged host-half.json": (d) => fs.writeFileSync(path.join(d, SC, "host-half.json"), JSON.stringify({ collected: true, node_agent: { version: "forged" } })),
    "a diagnostic-bundle.json": (d) => fs.writeFileSync(path.join(d, SC, "diagnostic-bundle.json"), "{}"),
    "a conditions.json under sessions/": (d) => {
      fs.mkdirSync(path.join(d, SC, "sessions", SID2), { recursive: true });
      fs.writeFileSync(path.join(d, SC, "sessions", SID2, "conditions.json"), "{}");
    },
    "halves.host true": (d) => edit(d, "manifest.json", (m) => ({ ...m, halves: { client: true, host: true } })),
  };
  for (const [what, plant] of Object.entries(claims)) {
    const dir = clientBundle();
    plant(dir);
    const before = treeHash(dir);
    const api = fake();
    const r = await cli([dir, "--base-url", BASE], api);
    assert.equal(r.code, EXIT.error, what);
    assert.equal(api.calls.length, 0, `${what}: no request`);
    assert.equal(treeHash(dir), before, `${what}: no write`);
  }
});

test("D1 --resume counts a marked scenario collected only if its files are all there; a forged marker is collected for real", async () => {
  const dir = clientBundle();
  fs.writeFileSync(path.join(dir, SC, "host-half.json"), JSON.stringify({ collected: true, node_agent: { version: "forged" } }));
  const r = await run(dir, world(), { resume: true });
  assert.ok(r.f.calls.some((u) => u.includes(SID)), "the forged marker did not stop the fetch");
  assert.equal(readJson(dir, "builds.json").components.node_agent.version, "0.6.1-dev", "the forged agent was not promoted");
  assert.ok(readJson(dir, `${SC}/diagnostic-bundle.json`).series);

  // a real collection with one file removed afterwards is collected again, not trusted
  for (const gone of ["conditions.json", "diagnostic-bundle.json", "session.json", "trace.json"]) {
    const d = clientBundle();
    await run(d);
    fs.rmSync(path.join(d, SC, gone));
    const again = await run(d, world(), { resume: true });
    assert.ok(again.f.calls.length > 0, `${gone} missing: fetched again`);
    assert.ok(fs.existsSync(path.join(d, SC, gone)), `${gone} was rewritten`);
  }
  // ... and so is one whose file is the wrong shape
  const wrong = clientBundle();
  await run(wrong);
  fs.writeFileSync(path.join(wrong, SC, "diagnostic-bundle.json"), "[]");
  assert.ok((await run(wrong, world(), { resume: true })).f.calls.length > 0);
});

test("A9 a top-level write that failed after the scenario was committed is repaired by --resume, without fetching the session again", async () => {
  if (process.getuid?.() === 0) return; // a read-only directory does not stop root
  const dir = clientBundle();
  fs.chmodSync(dir, 0o555);
  try {
    await assert.rejects(run(dir));
  } finally {
    fs.chmodSync(dir, 0o755);
  }
  assert.equal(readJson(dir, `${SC}/host-half.json`).collected, true);
  assert.equal(readJson(dir, "manifest.json").halves.host, false, "the interrupted manifest still says no host half");
  const r = await run(dir, world(), { resume: true });
  assert.ok(!r.f.paths().some((p) => p.startsWith("/v1/sessions/") || p.includes("/v1/admin/sessions/")), "the session is not fetched again");
  assert.equal(r.complete, true);
  assert.equal(readJson(dir, "manifest.json").halves.host, true);
  assert.equal(readJson(dir, "builds.json").components.control_plane.version, "0.6.1-dev");
  assert.deepEqual(validateBundle(dir), []);
});

// ── D2: a scenario's outputs are committed together, from the client's rows only ─
test("A14 a write that fails mid-scenario leaves it untouched, and a retry with another answer is consistent", async () => {
  if (process.getuid?.() === 0) return;
  const dir = clientBundle();
  edit(dir, `${SC}/scenario.json`, (s) => ({ ...s, sessions: [SID, SID2] }));
  const locked = path.join(dir, SC, "sessions", SID2);
  fs.mkdirSync(locked, { recursive: true });
  fs.chmodSync(locked, 0o555);
  const before = treeHash(dir);
  const sample = (fps) => [{ source: "agent", ts_unix_ms: T_START + 77_500, metrics: { fps } }];
  try {
    await assert.rejects(run(dir, world({ ids: [SID, SID2], samples: sample(60) })), /EACCES/);
  } finally {
    fs.chmodSync(locked, 0o755);
  }
  assert.equal(treeHash(dir), before, "not one byte, file or temporary file was left");
  assert.ok(!fs.existsSync(path.join(dir, SC, "host-half.json")));

  const r = await run(dir, world({ ids: [SID, SID2], samples: sample(61) }));
  assert.equal(r.complete, true);
  const row = rows(dir, `${SC}/metrics.jsonl`).find((x) => x.source === "agent" && x.ts_unix_ms === T_START + 77_500);
  assert.equal(row.metrics.fps, 61, "no host row of the failed run was merged as a client row");
  assert.equal(readJson(dir, `${SC}/diagnostic-bundle.json`).series["encoder.fps"].find((p) => p.ts_unix_ms === T_START + 77_500).v, 61);
  assert.ok(fs.existsSync(path.join(dir, SC, "sessions", SID2, "metrics.jsonl")));
});

test("D2 the renames come after every temporary file exists, and the marker is the last scenario file", async () => {
  const dir = clientBundle();
  const log = [];
  const [rename, write] = [fs.renameSync, fs.writeFileSync];
  fs.renameSync = (a, b) => (log.push(`rename ${path.basename(b)}`), rename(a, b));
  fs.writeFileSync = (f, ...rest) => (log.push(`write ${path.basename(String(f)).replace(/^\.(.*)\.[0-9a-f]{12}\.tmp$/, "$1")}`), write(f, ...rest));
  try {
    await run(dir);
  } finally {
    [fs.renameSync, fs.writeFileSync] = [rename, write];
  }
  const scenario = log.slice(0, log.indexOf("rename host-half.json") + 1);
  const firstRename = scenario.findIndex((l) => l.startsWith("rename"));
  assert.ok(scenario.slice(0, firstRename).every((l) => l.startsWith("write")), "every temporary file is written before the first rename");
  assert.equal(scenario.slice(firstRename).at(-1), "rename host-half.json");
  assert.deepEqual(scenario.slice(firstRename).slice(-2), ["rename metrics.jsonl", "rename host-half.json"]);
});

// ── D3: nothing from an answer reaches a file unless it is a value of a checked shape ─
test("A10 a name from an answer is listed only if it is a plain identifier; the rest is counted", async () => {
  const dir = clientBundle();
  const evil = "authorization=secret-value-1234";
  const samples = [
    { source: "agent", ts_unix_ms: T_START + 1000, metrics: { fps: 60, [evil]: 1, not_a_manifest_key: 1, "x y": 1, accessToken: 1 } },
    { source: evil, ts_unix_ms: T_START + 2000, metrics: { fps: 1 } },
    { source: "app", ts_unix_ms: T_START + 3000, metrics: { [evil]: 1, frames: 3 } },
  ];
  const r = await run(dir, world({ samples }));
  assert.equal(r.complete, true);
  for (const rel of walk(dir)) assert.doesNotMatch(read(dir, rel), /secret-value-1234/, rel);
  const half = readJson(dir, `${SC}/host-half.json`);
  assert.deepEqual(half.metric_keys_not_in_manifest, ["agent.not_a_manifest_key"]);
  assert.equal(half.sources_not_in_bundle, undefined);
  assert.deepEqual(half.other_dropped, { metric_keys: 4, sources: 1 });
  assert.deepEqual(rows(dir, `${SC}/metrics.jsonl`).find((x) => x.source === "app").metrics, { frames: 3 });
  assert.deepEqual(r.problems, []);
});

test("A10 a reason names the route path only: a metrics cursor is never in a file", async () => {
  const dir = clientBundle();
  const w = world({
    tweak: (route, body) => {
      if (!route.endsWith("/metrics")) return undefined;
      return w.pages++ === 0 ? [200, { items: [body[1].items[0]], next_cursor: "authorization=secret-value-1234" }] : [500, {}];
    },
  });
  w.pages = 0;
  const r = await run(dir, w);
  assert.equal(r.complete, false);
  assert.match(r.incomplete[0].reason, /HTTP 500/);
  assert.doesNotMatch(read(dir, `${SC}/host-half.json`), /secret-value|cursor|\?/);
});

test("A11 a window that is empty or inverted is that scenario's reason, never a TypeError", async () => {
  const sample = [{ source: "agent", ts_unix_ms: T_START, metrics: { fps: 60 } }];
  const cases = {
    "a live session that starts after now": world({ start: T_END + 7_200_000, end: null, samples: sample }),
    "an end before the start": world({ start: T_END, end: T_START - 60_000, samples: sample }),
  };
  for (const [what, w] of Object.entries(cases)) {
    const dir = clientBundle();
    const r = await run(dir, w);
    assert.equal(r.complete, false, what);
    assert.match(r.incomplete[0].reason, /window is empty/, what);
    assert.equal(readJson(dir, `${SC}/host-half.json`).collected, false);
    assert.ok(!r.f.paths().some((p) => p.includes("diagnostic-bundle")), `${what}: no read was made`);
  }
});

test("A13 a wrong-typed field nested in an answer is that scenario's reason, and nothing of it is written", async () => {
  const row = (patch) => (route, body) => (route === `/v1/sessions/${SID}` ? [200, { session: { ...body[1].session, ...patch(body[1].session) } }] : undefined);
  const diag = (patch) => (route, body) => (route.endsWith("/diagnostic-bundle") ? [200, { ...body[1], ...patch(body[1]) }] : undefined);
  const shapes = {
    "a numeric setting": [{ effective: { abr_mode: 7 } }, null],
    "a boolean setting": [{ effective: { abr_mode: true } }, null],
    "a null setting": [{ effective: { abr_mode: null } }, null],
    "pyrowave a string": [{}, row(() => ({ pyrowave: "not-an-object" }))],
    "transport a number": [{}, row(() => ({ transport: 7 }))],
    "stream width a string": [{}, row((s) => ({ stream: { ...s.stream, width: "1920" } }))],
    "failure_code an object": [{}, row(() => ({ failure_code: { a: 1 } }))],
    "trace a string": [{}, diag(() => ({ trace: "not-an-object" }))],
    "an event with no type": [{}, diag((b) => ({ events: [{ ts_unix_ms: 1786940102000, source: "agent" }, ...b.events] }))],
    "an event with no source": [{}, diag(() => ({ events: [{ ts_unix_ms: 1786940102000, type: "x" }] }))],
    "derived windows not lists": [{}, diag(() => ({ derived_windows: { hitches: "x" } }))],
    "agent adaptation an object": [{}, diag(() => ({ agent_adaptation: {} }))],
    "no classifier": [{}, diag(() => ({ classifier: null }))],
  };
  for (const [what, [over, tweak]] of Object.entries(shapes)) {
    const dir = clientBundle();
    const files = new Set(walk(dir));
    const r = await run(dir, world({ ...over, ...(tweak ? { tweak } : {}) }));
    assert.equal(r.complete, false, what);
    assert.deepEqual(walk(dir).filter((f) => !files.has(f)), [path.join(SC, "host-half.json")], what);
  }
});

test("A15 a trace window longer than the budget is not collected, and costs few requests", async () => {
  const year = 365 * 24 * 3_600_000;
  const dir = clientBundle();
  const r = await run(dir, world({ start: T_START, end: null, now: T_START + year }));
  assert.equal(r.complete, false);
  assert.match(r.incomplete[0].reason, /longer than one collection reads/);
  assert.ok(r.f.calls.length < 10, `${r.f.calls.length} requests`);

  // 250 windows of 10 minutes, each read full and split in two: 750 reads, over the budget
  const hours = 41;
  const full = world({
    start: T_START,
    end: T_START + hours * 3_600_000,
    now: T_START + hours * 3_600_000 + 1000,
    tweak: (route, body) => {
      if (!route.endsWith("/diagnostic-bundle")) return undefined;
      const span = body[1].window.to_ms - body[1].window.from_ms;
      return span < 9 * 60_000 ? undefined : [200, { ...body[1], events: Array.from({ length: 1000 }, (_, i) => ({ ts_unix_ms: body[1].window.from_ms + i, source: "agent", type: "x", payload: {} })) }];
    },
  });
  const dir2 = clientBundle();
  const t = await run(dir2, full);
  assert.equal(t.complete, false);
  assert.match(t.incomplete[0].reason, /longer than one collection reads/);
  assert.ok(t.f.calls.filter((u) => u.includes("/diagnostic-bundle")).length <= 600, "no more than 600 window reads");
});

test("A16 --base-url is http(s)://host[:port] only; a bad one is exit 2 before any request", async () => {
  for (const bad of ["https://stack.invalid/prefix", "https://stack.invalid?x=1", "https://stack.invalid?", "https://stack.invalid#f", "https://user:pw@stack.invalid", "ftp://stack.invalid", "not a url", "https://stack.invalid//"]) {
    const dir = clientBundle();
    const api = fake();
    const r = await cli([dir, "--base-url", bad], api);
    assert.equal(r.code, EXIT.error, bad);
    assert.equal(api.calls.length, 0, bad);
  }
  const dir = clientBundle();
  const api = fake();
  assert.equal((await cli([dir, "--base-url", `${BASE}:8443/`], api)).code, EXIT.ok);
  assert.ok(api.calls.every((u) => u.startsWith(`${BASE}:8443/v1/`)));
});

test("N6 a session id is compared case-insensitively and requested in lower case", async () => {
  const lower = "abcdef12-2222-3333-4444-5555555555ab";
  const dir = clientBundle();
  edit(dir, `${SC}/scenario.json`, (s) => ({ ...s, sessions: [lower.toUpperCase()] }));
  const r = await run(dir, world({ ids: [lower] }));
  assert.equal(r.complete, true);
  assert.ok(r.f.paths().includes(`/v1/sessions/${lower}`));
  assert.equal(readJson(dir, `${SC}/session.json`).id, lower);
});

// ── D4 ──────────────────────────────────────────────────────────────────────
test("D4 a credential held in an object under a credential-named key is refused before any request", async () => {
  const dir = clientBundle();
  edit(dir, "client.json", (c) => ({ ...c, authorization: { value: "credential-in-object" }, nested: [{ accessToken: ["credential-in-array"] }] }));
  const before = treeHash(dir);
  const api = fake();
  const r = await cli([dir, "--base-url", BASE], api);
  assert.equal(r.code, EXIT.invalid);
  assert.equal(api.calls.length, 0);
  assert.equal(treeHash(dir), before);
  assert.ok(r.errs.some((m) => /client.json: holds a credential/.test(m)));
});

// ── truth from the control plane's code ─────────────────────────────────────
test("B1 a capture outlives retention, and a session with only a capture is not collected", async () => {
  const dir = clientBundle();
  const capture = { source: "agent", ts_unix_ms: T_START + 5000, type: "diag.encoder_props", payload: {} };
  const r = await run(dir, world({ pruned: true, captures: [capture] }));
  assert.equal(r.complete, false);
  assert.match(r.incomplete[0].reason, /the trace is no longer kept/);
  assert.equal(readJson(dir, `${SC}/host-half.json`).collected, false);
  assert.ok(!fs.existsSync(path.join(dir, SC, "diagnostic-bundle.json")));

  // a capture is still kept in the artifact when there is other data
  const kept = clientBundle();
  await run(kept, world({ captures: [capture] }));
  assert.ok(readJson(kept, `${SC}/trace.json`).events.some((e) => e.type === "diag.encoder_props"));
});

test("B2 a session read in several windows keeps its whole analysis; a single read is the API's answer", async () => {
  const end = T_START + 25 * 60_000;
  const samples = Array.from({ length: 150 }, (_, i) => ({ source: "agent", ts_unix_ms: T_START + 1000 + i * 10_000, metrics: { fps: 60, adaptation_state: i % 2 ? "healthy" : "degraded" } }));
  const hitches = [T_START + 120_000, T_START + 1_400_000];
  const dir = clientBundle();
  const r = await run(dir, world({ end, now: end + 3_600_000, samples, hitches }));
  assert.equal(r.complete, true);
  assert.equal(r.f.calls.filter((u) => u.includes("/diagnostic-bundle")).length, 3);
  const d = readJson(dir, `${SC}/diagnostic-bundle.json`);
  assert.deepEqual(d.derived_windows.hitches.map((h) => h.from_ms), hitches, "a hitch after the first read is kept");
  assert.deepEqual(d.window, { from_ms: T_START - 5000, to_ms: end + 5000 });
  assert.equal(d.classifier_reads.length, 3);
  assert.equal(d.classifier.window.from_ms, T_START - 5000, "classifier is the first read's");
  assert.deepEqual(d.classifier_reads[0], d.classifier);
  assert.ok(d.classifier_reads.every((c, i, a) => i === 0 || c.window.from_ms === a[i - 1].window.to_ms), "each classifier names its own window");
  assert.equal(d.agent_adaptation.length, 150, "agent adaptation concatenated, de-duplicated by time");
  assert.equal(d.series["encoder.fps"].length, 150);

  const one = clientBundle();
  const w = world({ hitches: [T_START + 100_000] });
  await run(one, w);
  const single = readJson(one, `${SC}/diagnostic-bundle.json`);
  assert.equal(single.classifier_reads, undefined);
  assert.deepEqual(single.window, { from_ms: T_START - 5000, to_ms: T_END + 5000 });
  assert.equal(single.derived_windows.hitches.length, 1);
});

test("S5 a read is full when its series hold 1000 distinct instants: the store caps samples per read across sources", async () => {
  const agent = Array.from({ length: 600 }, (_, i) => ({ source: "agent", ts_unix_ms: T_START + 1000 + i * 400, metrics: { fps: 60 } }));
  const browser = Array.from({ length: 600 }, (_, i) => ({ source: "browser", ts_unix_ms: T_START + 1200 + i * 400, metrics: { fps: 120 } }));
  const dir = clientBundle();
  const r = await run(dir, world({ samples: [...agent, ...browser] }));
  assert.equal(r.complete, true);
  assert.ok(r.f.calls.filter((u) => u.includes("/diagnostic-bundle")).length > 1, "the read the cap truncated was split");
  const d = readJson(dir, `${SC}/diagnostic-bundle.json`);
  assert.equal(d.series["encoder.fps"].length, 600);
  assert.equal(d.series["client.fps"].length, 600, "no point was lost to the cap");
});

test("S6 a session that never started is collected: its row, its conditions and the agent identity, no trace", async () => {
  const dir = clientBundle();
  const traceBefore = read(dir, `${SC}/trace.json`);
  const metricsBefore = read(dir, `${SC}/metrics.jsonl`);
  const w = world({ start: null, rowExtra: { state: "failed", failure_code: "launch_failed", error_message: "prepare session resources: mounts", app_log_tail: "peer 10.20.30.40 token=abc123456" } });
  const r = await run(dir, w);
  assert.equal(r.complete, true);
  assert.ok(!r.f.paths().some((p) => p.includes("/diagnostic-bundle") || p.includes("/trace/") || p.endsWith("/metrics")), "no trace was asked for");
  const session = readJson(dir, `${SC}/session.json`);
  assert.equal(session.failure_code, "launch_failed");
  assert.equal(session.error_message, undefined, "the free-text message is never copied");
  assert.equal(session.has_error_message, true);
  assert.doesNotMatch(read(dir, `${SC}/session.json`), /mounts/);
  assert.equal(session.started_at_ms, undefined);
  assert.doesNotMatch(read(dir, `${SC}/session.json`), /app_log_tail|abc123456/);
  assert.ok(readJson(dir, `${SC}/conditions.json`).effective);
  const half = readJson(dir, `${SC}/host-half.json`);
  assert.equal(half.collected, true);
  assert.equal(half.trace, "the session never started");
  assert.equal(half.node_agent.version, "0.6.1-dev");
  assert.equal(read(dir, `${SC}/trace.json`), traceBefore);
  assert.equal(read(dir, `${SC}/metrics.jsonl`), metricsBefore);
  assert.ok(!fs.existsSync(path.join(dir, SC, "diagnostic-bundle.json")));
  assert.equal(readJson(dir, "manifest.json").halves.host, true);
  assert.deepEqual(r.problems, []);
  // --resume knows it is whole
  assert.equal((await run(dir, world({ start: null }), { resume: true })).f.calls.length, 0);

  // a session still launching (no start, no end) is not collected
  const live = clientBundle();
  const l = await run(live, world({ start: null, end: null }));
  assert.equal(l.complete, false);
  assert.match(l.incomplete[0].reason, /not started or ended/);
});

test("N4 the clock the client half measured is kept when the server's is unmeasured", async () => {
  const dir = clientBundle();
  const mine = { client_offset_ms: 7, uncertainty_ms: 2 };
  edit(dir, `${SC}/trace.json`, (t) => ({ ...t, clock: mine }));
  await run(dir, world({ clock: { unmeasured: true } }));
  assert.deepEqual(readJson(dir, `${SC}/trace.json`).clock, mine);
  const both = clientBundle();
  edit(both, `${SC}/trace.json`, (t) => ({ ...t, clock: mine }));
  await run(both);
  assert.equal(readJson(both, `${SC}/trace.json`).clock.client_offset_ms, 2, "a measured server clock wins");
});

test("N5 every session's raw events are kept, each in its own session directory; the scenario's trace is the first session's", async () => {
  const dir = clientBundle();
  edit(dir, `${SC}/scenario.json`, (s) => ({ ...s, sessions: [SID, SID2] }));
  const r = await run(dir, world({ ids: [SID, SID2] }));
  assert.equal(r.complete, true);
  assert.equal(readJson(dir, `${SC}/trace.json`).session_id, SID);
  const second = readJson(dir, `${SC}/sessions/${SID2}/trace.json`);
  assert.equal(second.session_id, SID2);
  assert.ok(second.events.some((e) => e.type === "abr.ladder.step"));
  assert.ok(!second.events.some((e) => e.type === "client.freeze_detected"), "the client half's events belong to the first session");
  assert.deepEqual(validateBundle(dir), []);
});

// ── round 3, seat A ─────────────────────────────────────────────────────────
test("A17 a derived-window kind named like an object member folds like any other", async () => {
  const end = T_START + 11 * 60_000; // two reads
  for (const kind of ["__proto__", "constructor"]) {
    const dir = clientBundle();
    const tweak = (route, body) =>
      route.endsWith("/diagnostic-bundle") ? [200, { ...body[1], derived_windows: JSON.parse(`{"${kind}":[{"from_ms":${T_START + 1000},"to_ms":${T_START + 2000}}]}`) }] : undefined;
    const r = await run(dir, world({ end, now: end + 3_600_000, tweak }));
    assert.equal(r.complete, true, kind);
    assert.match(read(dir, `${SC}/diagnostic-bundle.json`), new RegExp(`"${kind}": \\[`), `${kind} is kept as a kind`);
  }
});

test("A18 a failure's free text is never copied: no credential shape in it can reach session.json", async () => {
  // Every shape the reviews found, and one no pattern would find: none is in the file,
  // because the message is not copied and a failure code that is not a code is not either.
  const shapes = ["authorization=credential-value-1234", "Authorization: Basic dXNlcjpwYXNz", "Set-Cookie: a=b", "Bearer abc123def456", "hunter2 is the registry password"];
  for (const text of shapes) {
    for (const field of ["failure_code", "error_message"]) {
      const dir = clientBundle();
      const r = await run(dir, world({ start: null, rowExtra: { state: "failed", failure_code: "launch_failed", error_message: "pull failed", [field]: `pull failed: ${text}` } }));
      assert.equal(r.complete, true);
      assert.deepEqual(r.problems, []);
      const file = read(dir, `${SC}/session.json`);
      assert.doesNotMatch(file, /credential-value-1234|dXNlcjpwYXNz|a=b|abc123def456|hunter2|pull failed/, `${field}: ${text}`);
      const session = JSON.parse(file);
      assert.equal(session.has_error_message, true);
      assert.equal(session.failure_code, field === "failure_code" ? undefined : "launch_failed");
    }
  }
});

test("A19 a trace whose session id only prints like the id is another session's", async () => {
  const dir = clientBundle();
  const tweak = (route, body) => (route.endsWith("/diagnostic-bundle") ? [200, { ...body[1], trace: { ...body[1].trace, session_id: [SID] } }] : undefined);
  const r = await run(dir, world({ tweak }));
  assert.equal(r.complete, false);
  assert.match(readJson(dir, `${SC}/host-half.json`).reason, /another session's/);
  assert.ok(!fs.existsSync(path.join(dir, SC, "diagnostic-bundle.json")));
});

test("A20 a file already at a temporary's name is left alone when the write fails on it", async (t) => {
  const dir = clientBundle();
  t.mock.method(crypto, "randomBytes", () => Buffer.from("0123456789ab", "hex"));
  const planted = path.join(dir, SC, ".metrics.jsonl.0123456789ab.tmp");
  fs.writeFileSync(planted, "pre-planted sentinel");
  const before = treeHash(dir);
  await assert.rejects(run(dir), /EEXIST/);
  assert.equal(fs.readFileSync(planted, "utf8"), "pre-planted sentinel");
  assert.equal(treeHash(dir), before, "not one byte changed");
});

// ── round 5, seat A: the projection invariant ───────────────────────────────
// Every string the collector itself projects into session.json, conditions.json, host-half.json or
// builds.json passes the shape for its own field, or it is not written and is counted in
// host-half.json's not_projected. Answers stored whole (diagnostic-bundle.json, the raw events in
// trace.json) are the documented best-effort limit and are not this test's subject.
const MARKER = "hunter2 is the registry password";

// The marker (and the word a pattern would hunt for) is in no file of the bundle: the value is
// left out, never echoed, and neither is its length.
function assertMarkerNowhere(dir) {
  for (const rel of walk(dir)) {
    assert.ok(!read(dir, rel).includes(MARKER), `${rel} holds the marker`);
    assert.ok(!read(dir, rel).includes("hunter2"), `${rel} holds the marker's word`);
  }
}

test("A22 session.json: every projected string passes its own field's shape; the rest is counted", async () => {
  const dir = clientBundle();
  const w = world({
    tweak: (route, body) =>
      route === `/v1/sessions/${SID}`
        ? [
            200,
            {
              session: {
                ...body[1].session,
                state: MARKER,
                app_id: MARKER,
                profile_id: MARKER,
                transport: MARKER,
                negotiated_codec: MARKER,
                stream: { ...body[1].session.stream, codec: MARKER },
                pyrowave: {
                  allowed: true,
                  reason: MARKER,
                  wanted: false,
                  choice: MARKER,
                  auto_reason: MARKER,
                  active: false,
                  used: false,
                  target_kbps: 900,
                  switch_reason: MARKER,
                  extra_member: MARKER, // a member PyroWaveRecord does not have
                  nested: { reason: MARKER }, // an object, never a member
                },
              },
            },
          ]
        : undefined,
  });
  const r = await run(dir, w);
  assert.equal(r.complete, true);
  assert.deepEqual(r.problems, []);
  assert.deepEqual(r.incomplete, []);

  const session = readJson(dir, `${SC}/session.json`);
  for (const k of ["state", "app_id", "profile_id", "transport", "negotiated_codec", "codec"]) {
    assert.equal(session[k], undefined, `${k} is not written`);
  }
  assert.deepEqual(
    session.pyrowave,
    { allowed: true, wanted: false, active: false, used: false, target_kbps: 900 },
    "only PyroWaveRecord's own members, each in its own type",
  );
  // The well-shaped fields of the same answer are still written.
  assert.equal(session.host_id, HOST_ID);
  assert.deepEqual(session.launch, [1920, 1080]);
  assert.equal(session.fps, 60);
  assert.deepEqual(session.rungs, [[1920, 1080], [1280, 720]]);
  assert.equal(session.started_at_ms, T_START);
  assertMarkerNowhere(dir);
  assert.deepEqual(readJson(dir, `${SC}/host-half.json`).not_projected, { session: 12, conditions: 0, identity: 0 });
});

test("A22 conditions.json: a host path and free text are [omitted]; a key that is not a name is left out", async () => {
  const dir = clientBundle();
  const w = world({
    effective: {
      abr_mode: "smooth",
      abr_ladder_resolution: "true",
      abr_floor_kbps: "4000",
      home_root: "/srv/quasar-home", // a host path: a "/" is no setting value's shape
      note: MARKER, // free text: a space is not either
      "a key": "1", // not a name, so not a key
    },
  });
  const r = await run(dir, w);
  assert.equal(r.complete, true);
  assert.deepEqual(r.problems, []);

  const { effective } = readJson(dir, `${SC}/conditions.json`);
  assert.equal(effective.home_root, "[omitted]", "a host path is not copied");
  assert.equal(effective.note, "[omitted]");
  assert.ok(!("a key" in effective));
  // The well-shaped settings of the same answer are still written, alias included.
  assert.equal(effective.abr_mode, "smooth");
  assert.equal(effective.abr_floor_kbps, "4000");
  assert.equal(effective.abr_ladder_resolution, "on");
  assert.equal(effective.ladder_resolution, "on", "the alias carries the same value, and is not a second omission");
  assertMarkerNowhere(dir);
  assert.deepEqual(readJson(dir, `${SC}/host-half.json`).not_projected, { session: 0, conditions: 3, identity: 0 });
});

test("A22 host-half.json and builds.json: a build's version, commit and time are written only in their own shapes", async () => {
  const dir = clientBundle();
  const w = world({
    host: { id: HOST_ID, node_name: "lab-gpu-host", agent_version: MARKER, source_commit: MARKER, built_at: MARKER },
    identity: { version: MARKER, source_commit: MARKER, built_at: MARKER },
  });
  const r = await run(dir, w);
  assert.equal(r.complete, true);
  assert.deepEqual(r.problems, []);

  const half = readJson(dir, `${SC}/host-half.json`);
  assert.equal(half.collected, true);
  assert.equal(half.node_agent, null, "an identity left with no field is not read");
  const builds = readJson(dir, "builds.json");
  assert.equal(builds.components.node_agent, null);
  assert.equal(builds.components.control_plane, null);
  assert.ok(builds.unknown.node_agent);
  assert.ok(builds.unknown.control_plane);
  assertMarkerNowhere(dir);
  // Three fields on the host row and three on the platform identity: one count per value.
  assert.deepEqual(half.not_projected, { session: 0, conditions: 0, identity: 6 });
});

test("A22 a normal answer leaves nothing out: PyroWaveRecord member for member, and no not_projected", async () => {
  const dir = clientBundle();
  const r = await run(dir);
  assert.equal(r.complete, true);
  const session = readJson(dir, `${SC}/session.json`);
  assert.deepEqual(
    session.pyrowave,
    { allowed: true, reason: null, wanted: false, choice: null, auto_reason: null, active: false, used: false, target_kbps: null, switch_reason: null },
    "a null member is the answer holding none, and is written as null, not counted",
  );
  assert.equal(session.state, "stopped");
  assert.equal(session.app_id, "99999999-8888-7777-6666-555555555555");
  assert.equal(session.profile_id, "1080p60-h264");
  const { effective } = readJson(dir, `${SC}/conditions.json`);
  assert.equal(Object.values(effective).filter((v) => v === "[omitted]").length, 0);
  assert.equal(readJson(dir, `${SC}/host-half.json`).not_projected, undefined);
  assert.equal(readJson(dir, "builds.json").components.node_agent.version, "0.6.1-dev");
});

test("A23 a failure code is written in a code's shape, and a credential's hex token is no code", async () => {
  const token = "0123456789abcdef0123456789abcdef01234567";
  for (const [code, kept] of [
    [token, false],
    ["launch_failed", true],
    ["app_exited_early", true],
  ]) {
    const dir = clientBundle();
    const r = await run(dir, world({ start: null, rowExtra: { state: "failed", failure_code: code, error_message: `pull failed: ${MARKER}` } }));
    assert.equal(r.complete, true, code);
    assert.deepEqual(r.problems, [], code);
    const session = readJson(dir, `${SC}/session.json`);
    assert.equal(session.failure_code, kept ? code : undefined, code);
    assert.equal(session.has_error_message, true, code);
    assert.equal(session.error_message, undefined, "the message is never copied");
    assertMarkerNowhere(dir);
    if (!kept) assert.ok(!read(dir, `${SC}/host-half.json`).includes(token), "not even counted as text");
    assert.deepEqual(
      readJson(dir, `${SC}/host-half.json`).not_projected,
      kept ? undefined : { session: 1, conditions: 0, identity: 0 },
      code,
    );
  }
});
