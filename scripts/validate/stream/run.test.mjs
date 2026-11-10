// node --test scripts/validate/stream/run.test.mjs: the runner and stream-hold against a fake page
// and a fake API. No browser, no network. The page's own evaluate() callbacks really run, against
// a stand-in window, so the in-page code is exercised too.
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";
import { validateBundle } from "./bundle.mjs";
import { benchRows } from "./benchfold.mjs";
import { runSuite, readThresholds } from "./run.mjs";
import { freezes, hitchWindows, observeWindows, shotCovered, verdict } from "./scenarios/stream-hold.mjs";

const HERE = path.dirname(fileURLToPath(import.meta.url));
const FIXTURE = JSON.parse(fs.readFileSync(path.join(HERE, "../../dx/tests/fixtures/appfold/bench-windows.json"), "utf8")).windows;
const [RECORDED, FROZEN] = FIXTURE; // a 60-frame second and a frameless one, as recorded
const HEALTHY = { ...RECORDED, missing_indices: 0 }; // the recorded second misses 1 index in 61, over the 1 % limit
const BASE = "https://stack.example.test:8443";
const SID = "11111111-2222-3333-4444-555555555555";
const T0 = 1_800_000_000_000;

/** `n` one-second windows; the seconds listed in `frozen` are frameless. */
function windows(n, frozen = []) {
  return Array.from({ length: n }, (_, i) => ({ ...(frozen.includes(i) ? FROZEN : HEALTHY), t_start_ms: T0 + i * 1000, t_end_ms: T0 + (i + 1) * 1000, t_end_host_ms: T0 + (i + 1) * 1000 + 3, last_host_time_ms: frozen.includes(i) ? null : T0 + i * 1000 + 950 }));
}
const frames = (n) => Array.from({ length: n * 60 }, (_, i) => ({ present_ms: T0 + Math.round(i * (1000 / 60)), decoded: true }));

/** A world holding the fake clock, the fake page's state and the fake API's record. */
function world({ wins = windows(30), located = true, session = {}, failScreenshotAt = null, webtransport = true, noBench = false, refuse = false } = {}) {
  const w = { t: T0, stopped: [], live: [], url: "about:blank", console: [], ls: new Map(), shots: 0, failScreenshotAt };
  w.now = () => w.t;
  w.sleep = async (ms) => { w.t += ms; };
  w.clicks = [];
  w.classic = false;
  w.api = {
    classicLayout: async () => w.classic,
    setClassicLayout: async (on) => { w.classic = on; },
    apps: async () => [{ id: "app-1", name: "Quasar Benchapp" }],
    liveSessions: async () => w.live,
    session: async () => ({ id: SID, state: "running", transport: "webrtc", negotiated_codec: "h264", stream: { height: 1080, fps: 60 }, ...session }),
    stop: async (id) => { w.stopped.push(id); w.live = []; },
  };
  const qbench = { dump: () => ({ frames: frames(wins.length), windows: wins }), stats: () => ({ frames: 100, decoded: 100, located }), windows: () => wins };
  // A chainable fake locator: its element is enabled, and clicking "Play now" starts the session.
  const loc = (desc) => ({
    first: () => loc(desc),
    getByRole: (role, o) => loc(`${desc} > ${role}:${o?.name}`),
    evaluate: async (fn) => fn({ disabled: false, getAttribute: () => null, click: () => { w.clicks.push(desc); if (/Play now/.test(desc) && refuse) w.emit("response", { status: () => 409, url: () => `${BASE}/v1/sessions`, request: () => ({ method: () => "POST" }), text: async () => '{"error":{"code":"profile_ineligible"}}' }); else if (/Play now/.test(desc)) { w.url = `${BASE}/app/session/${SID}`; w.live = [{ id: SID, state: "running" }]; } } }),
  });
  const page = () => {
    const handlers = {};
    return {
      on: (ev, fn) => { handlers[ev] = fn; w.emit = (e, m) => handlers[e]?.(m); },
      goto: async () => {},
      fill: async () => {},
      click: async () => { w.ls.set("quasar.auth.token", "the-test-user's"); w.ls.set("quasar.auth.user", "{}"); },
      waitForURL: async () => {},
      url: () => w.url,
      close: async () => {},
      screenshot: async ({ path: p }) => {
        if (w.failScreenshotAt !== null && ++w.shots > w.failScreenshotAt) throw new Error("the page crashed");
        fs.writeFileSync(p, Buffer.from([0x89, 0x50, 0x4e, 0x47, 0]));
      },
      locator: (sel) => loc(sel),
      getByRole: (role, o) => loc(`${role}:${o?.name}`),
      evaluate: async (fn, arg) => {
        // Own enumerable properties are the stored items, as Object.keys(localStorage) sees them.
        const store = Object.create({ getItem(k) { return this[k] ?? null; }, setItem(k, v) { this[k] = v; }, removeItem(k) { delete this[k]; }, clear() { for (const k of Object.keys(this)) delete this[k]; } });
        for (const [k, v] of w.ls) store[k] = v;
        const saved = Object.fromEntries(["window", "localStorage", "sessionStorage", "document"].map((k) => [k, globalThis[k]]));
        Object.assign(globalThis, { window: noBench ? {} : { __qBench: qbench }, localStorage: store, sessionStorage: Object.create(store), document: { querySelector: () => null } });
        try { return await fn(arg); } finally { w.ls = new Map(Object.entries(store)); Object.assign(globalThis, saved); }
      },
    };
  };
  w.opened = [];
  w.closed = [];
  w.host = {
    newPage: async () => { const p = page(); w.opened.push(p); return p; },
    closePage: async (p) => { w.closed.push(p); },
    probeClient: async () => ({ kind: "web", browser: { name: "Chrome", version: "157.0.0.0" }, os: { name: "macOS", version: "26.1" }, cpu: { cores: 8 }, gpu: { adapter: null }, displays: [], decode: { h264: { supported: true, hardware: true } }, power: null, webtransport, webgpu: true }),
  };
  return w;
}

const PLAN = (...params) => ({ name: "t", scenarios: params.map((p) => ({ scenario: "stream-hold", params: { hold_s: 20, profile: "1080p60", ...p } })) });
const CREDS = { email: "tester@example.test", password: "correct-horse-battery" };

async function run(w, plan = PLAN({}), extra = {}) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "e2e-run-"));
  const result = await runSuite({ plan, dir, baseUrl: BASE, creds: CREDS, host: w.host, api: w.api, thresholds: readThresholds(), now: w.now, sleep: w.sleep, ...extra });
  const scenario = (i = 1) => JSON.parse(fs.readFileSync(path.join(dir, `0${i}-stream-hold/scenario.json`), "utf8"));
  return { dir, result, scenario };
}

test("a healthy hold writes a bundle the validator accepts", async () => {
  const w = world();
  const { dir, result, scenario } = await run(w);
  assert.deepEqual(result.errors, []);
  const sc = scenario();
  assert.equal(sc.status, "pass", JSON.stringify(sc.criteria));
  assert.deepEqual(sc.criteria.map((c) => c.id), ["first_frame_s", "marker_decoded", "transport_as_asked", "codec_as_asked", "profile_as_asked", "freeze_count", "missing_indices_rate", "present_cadence"]);
  assert.equal(sc.clock_offset_ms, 3);
  assert.deepEqual(sc.sessions, [SID]);
  const d = path.join(dir, "01-stream-hold");
  for (const f of ["timeline.jsonl", "metrics.jsonl", "bench-windows.json", "bench-frames.json", "session.json", "console.log", "shots/first-frame.png", "shots/t10s.png", "shots/end.png"]) assert.ok(fs.existsSync(path.join(d, f)), f);
  const manifest = JSON.parse(fs.readFileSync(path.join(dir, "manifest.json"), "utf8"));
  assert.deepEqual(manifest.halves, { client: true, host: false });
  assert.ok(manifest.not_recorded.some((n) => n.fact === "audio_latency_ms" && n.why));
  assert.equal(manifest.target, "stack");
  assert.deepEqual(w.stopped, [SID]);
});

test("the codec, frame rate and resolution are asked for through Launch options, and the layout is put back", async () => {
  const w = world();
  await run(w, PLAN({ codec: "h265", profile: "1080p60" }));
  assert.deepEqual(w.clicks, [
    'button[aria-label="Quasar Benchapp"]',
    "button:/^Adjust/",
    "radiogroup:Codec > radio:/^HEVC/",
    "radiogroup:Frame rate > radio:/^60 fps/",
    "radiogroup:Resolution > radio:/^1080p/",
    "button:/^Play now/",
  ]);
  assert.equal(w.classic, false, "the account's layout setting is restored");
});

test("a page that never offers the option is an error with its reason, and the layout is still put back", async () => {
  const w = world();
  const orig = w.host.newPage;
  w.host.newPage = async () => { const p = await orig(); const get = p.getByRole; p.getByRole = (r, o) => { const l = get(r, o); return /Adjust/.test(String(o?.name)) ? { first: () => ({ evaluate: async () => { throw new Error("none"); } }) } : l; }; return p; };
  const sleep = async (ms) => { w.t += ms; };
  const { scenario } = await run(w, PLAN({}), { sleep });
  assert.equal(scenario().status, "error");
  assert.match(scenario().reason, /Adjust \(Launch options\) never became available/);
  assert.equal(w.classic, false);
});

test("a 3 s freeze fails freeze_count and so the scenario", async () => {
  const w = world({ wins: windows(30, [14, 15, 16]) });
  const { result, scenario } = await run(w);
  assert.deepEqual(result.errors, []);
  const sc = scenario();
  assert.equal(sc.status, "fail");
  assert.deepEqual(sc.criteria.find((c) => c.id === "freeze_count"), { id: "freeze_count", pass: false, observed: 1, expected: "0 after the warm-up" });
  assert.deepEqual(w.stopped, [SID]);
});

test("a freeze inside the warm-up is not counted", () => {
  assert.equal(freezes(observeWindows(windows(30, [2, 3, 4]))), 0);
  assert.equal(freezes(windows(30, [2, 3, 4, 9, 10])), 2);
});

test("an app without a marker leaves the marker criteria unjudged, not failed", async () => {
  const w = world({ located: false, wins: windows(30).map((x) => ({ ...x, n: 60, decoded: 0, undecoded: 60 })) });
  const { result, scenario } = await run(w);
  assert.deepEqual(result.errors, []);
  const sc = scenario();
  assert.equal(sc.criteria.find((c) => c.id === "marker_decoded").pass, false);
  // the bench windows count frames with or without a marker: a freeze is judged, the marker-dependent ones are not
  assert.equal(sc.criteria.find((c) => c.id === "freeze_count").pass, true);
  for (const id of ["missing_indices_rate", "present_cadence"]) assert.equal(sc.criteria.find((c) => c.id === id).pass, null, id);
});

test("an error mid-hold still stops the session and leaves a valid bundle", async () => {
  const w = world({ failScreenshotAt: 2 });
  const { result, scenario } = await run(w);
  assert.deepEqual(result.errors, []);
  assert.equal(scenario().status, "error");
  assert.match(scenario().reason, /crashed/);
  assert.deepEqual(w.stopped, [SID]);
  assert.equal(result.counts.error, 1);
});

test("an interrupt mid-hold stops the session and skips the rest of the plan", async () => {
  const w = world();
  let stop;
  const abort = new Promise((r) => { stop = r; });
  const sleep = async (ms) => { w.t += ms; if (w.t > T0 + 40_000) stop(); await new Promise((r) => setImmediate(r)); };
  const { result, scenario } = await run(w, PLAN({ hold_s: 60 }, {}), { abort, sleep });
  assert.deepEqual(result.errors, []);
  assert.equal(scenario(1).status, "error");
  assert.match(scenario(1).reason, /interrupted/);
  assert.equal(scenario(2).status, "skipped");
  assert.deepEqual(w.stopped, [SID]);
});

test("a session the page started but the run never saw is stopped too", async () => {
  const w = world();
  w.api.session = async () => { throw new Error("no such session"); };
  const { scenario } = await run(w);
  assert.equal(scenario().status, "error");
  assert.deepEqual(w.stopped, [SID]);
});

test("addresses, tokens and the sign-in never reach a file", async () => {
  const w = world();
  const origScreenshot = w.host.newPage;
  w.host.newPage = async () => {
    const p = await origScreenshot();
    const orig = p.locator;
    p.locator = (...a) => { w.emit?.("console", { type: () => "log", text: () => `ice candidate 10.20.30.40 and ${BASE}/x Authorization: Bearer abc123def456ghi789 for ${CREDS.email} pw ${CREDS.password}` }); return orig(...a); };
    return p;
  };
  const { dir, result } = await run(w);
  assert.deepEqual(result.errors, []);
  const log = fs.readFileSync(path.join(dir, "01-stream-hold/console.log"), "utf8");
  assert.match(log, /ice candidate <address> and https:\/\/<stack>\/x Authorization: Bearer <token> for <user> pw <credential>/);
  assert.ok(Object.keys(JSON.parse(fs.readFileSync(path.join(dir, "manifest.json"), "utf8")).redacted).length > 0);
});

test("a transport this browser lacks is skipped with its reason, and the run goes on", async () => {
  const w = world({ webtransport: false });
  const { result, scenario, dir } = await run(w, PLAN({ transport: "webtransport" }, { transport: "webrtc" }));
  assert.deepEqual(result.errors, []);
  assert.equal(scenario(1).status, "skipped");
  assert.match(scenario(1).reason, /no WebTransport/);
  assert.equal(scenario(2).status, "pass");
  assert.deepEqual(result.counts, { pass: 1, fail: 0, error: 0, skipped: 1 });
  assert.ok(fs.existsSync(path.join(dir, "02-stream-hold/bench-windows.json")));
});

test("a codec this client cannot decode, and an app not in the catalog, are skipped", async () => {
  const w = world();
  w.host.probeClient = async () => ({ kind: "web", browser: { name: "Chrome", version: "1.0.0" }, os: { name: "Linux", version: null }, decode: { av1: { supported: false, hardware: false } }, webtransport: true, webgpu: false });
  const a = await run(w, PLAN({ codec: "av1" }));
  assert.match(a.scenario().reason, /cannot decode av1/);
  const b = await run(w, PLAN({ codec: "pyrowave", transport: "webtransport" }));
  assert.match(b.scenario().reason, /cannot decode pyrowave/);
  const c = await run(world(), PLAN({ app: "No Such App" }));
  assert.match(c.scenario().reason, /not in this user's catalog/);
});

test("transport, codec and profile are judged against the session row", async () => {
  const w = world({ session: { transport: "webtransport", negotiated_codec: "hevc", stream: { height: 720, fps: 60 } } });
  const { scenario } = await run(w, PLAN({ transport: "webtransport", codec: "h265" }));
  const by = Object.fromEntries(scenario().criteria.map((c) => [c.id, c]));
  assert.equal(by.transport_as_asked.pass, true);
  assert.equal(by.codec_as_asked.pass, true);
  assert.equal(by.profile_as_asked.pass, false);
  assert.equal(by.profile_as_asked.observed, "720p60");
});

test("bench windows fold into the browser keys bench_app_samples.py writes", () => {
  const rows = benchRows(FIXTURE);
  assert.equal(rows.length, 2);
  assert.deepEqual(rows[0].metrics.bench_n, 60);
  assert.equal(rows[0].metrics.i2p_p50_ms, 44);
  assert.equal(rows[0].metrics.i2p_max_ms, 52);
  assert.equal(rows[0].metrics.i2p_n, 2);
  assert.equal(rows[0].metrics.offset_unknown, 0);
  assert.equal(rows[0].metrics.stage_host_to_receive_p50_ms, 21.4);
  assert.ok(!("stage_assembly_mean_ms" in rows[0].metrics), "a null stage is absent, never 0");
  assert.equal(rows[0].ts_unix_ms, 1800000000950);
  assert.equal(rows[1].ts_unix_ms, 1800000002003); // no marker stamp: the window's host close
});

test("present cadence counts one-second groups whose interval spread reaches the threshold", () => {
  const steady = frames(5);
  assert.equal(hitchWindows(steady, T0, 18), 0);
  const jerky = steady.map((f, i) => ({ present_ms: f.present_ms + (i % 2 ? 30 : 0) }));
  assert.ok(hitchWindows(jerky, T0, 18) >= 4);
});

test("pass needs one criterion that held; nothing judged is an error with its reason", async () => {
  assert.deepEqual(verdict({ criteria: [{ pass: null }, { pass: null }] }), { status: "error", reason: "nothing could be judged" });
  assert.deepEqual(verdict({ criteria: [] }), { status: "error", reason: "nothing could be judged" });
  assert.deepEqual(verdict({ criteria: [{ pass: null }, { pass: true }] }), { status: "pass" });
  assert.deepEqual(verdict({ criteria: [{ pass: true }, { pass: false }] }), { status: "fail" });
  // end to end: no session row, no bench mode, no frame ever seen: every criterion is null, and the bundle says error
  const w = world({ noBench: true });
  w.api.session = async () => null;
  const { scenario, result } = await run(w);
  assert.deepEqual(result.errors, []);
  assert.equal(scenario().status, "error");
  assert.equal(scenario().reason, "nothing could be judged");
});

const OWNER = new Map([["quasar.auth.token", "owners-token"], ["quasar.auth.user", "{\"name\":\"owner\"}"], ["quasar.display.preferences", "{}"], ["unrelated", "1"]]);

test("the runner leaves the stack's storage as it found it, on every exit path", async () => {
  for (const mode of ["healthy", "error", "interrupt"]) {
    const w = world(mode === "error" ? { failScreenshotAt: 2 } : {});
    w.ls = new Map(OWNER);
    let extra = {};
    if (mode === "interrupt") {
      let stop;
      extra = { abort: new Promise((r) => { stop = r; }), sleep: async (ms) => { w.t += ms; if (w.t > T0 + 40_000) stop(); await new Promise((r) => setImmediate(r)); } };
    }
    await run(w, PLAN(mode === "interrupt" ? { hold_s: 60 } : {}), extra);
    assert.deepEqual([...w.ls].sort(), [...OWNER].sort(), `${mode}: storage back to the owner's`);
    assert.ok(!w.ls.has("quasar.bench"), `${mode}: bench flag removed`);
  }
});

test("a profile with no sign-in ends with none, and bench mode was armed during the run", async () => {
  const w = world();
  let armed = false;
  const orig = w.host.newPage;
  w.host.newPage = async () => { const p = await orig(); const ev = p.evaluate; p.evaluate = async (...a) => { const r = await ev(...a); if (w.ls.get("quasar.bench") === "1") armed = true; return r; }; return p; };
  await run(w);
  assert.ok(armed, "the runner armed bench mode");
  assert.equal(w.ls.size, 0);
});

test("only the pages the runner opened are closed, and all of them are", async () => {
  for (const w of [world(), world({ failScreenshotAt: 2 })]) {
    await run(w, PLAN({}, { transport: "webtransport" }));
    assert.ok(w.opened.length >= 3);
    assert.deepEqual(w.closed, w.opened);
  }
});

test("a shot of an app that shows a sign-in QR has the cover on it, and is not taken with no surface to place it on", async () => {
  const covers = [];
  const doc = (surface) => ({
    querySelectorAll: () => (surface ? [surface] : []),
    createElement: () => ({ style: {} }),
    documentElement: { appendChild: (d) => covers.push(d) },
    getElementById: () => ({ remove: () => covers.splice(0) }),
  });
  const page = (surface) => ({ evaluate: async (fn, arg) => { const saved = globalThis.document; globalThis.document = doc(surface); try { return await fn(arg); } finally { globalThis.document = saved; } } });
  const taken = [];
  const steps = [];
  const rec = { shot: async (_p, label) => { taken.push([label, covers.length]); }, step: (ev) => steps.push(ev) };
  const canvas = { tagName: "CANVAS", getBoundingClientRect: () => ({ left: 0, top: 0, width: 1920, height: 1080 }), dataset: {} };
  await shotCovered(page(canvas), rec, "a", true);
  assert.deepEqual(taken, [["a", 1]], "the cover was on the page during the shot");
  assert.equal(covers.length, 0, "and is gone after it");
  await shotCovered(page(null), rec, "b", true);
  assert.deepEqual(taken, [["a", 1]]);
  assert.deepEqual(steps, ["shot-skipped"]);
  await shotCovered(page(null), rec, "c", false);
  assert.deepEqual(taken.at(-1), ["c", 0]);
});

test("a launch the product refuses ends the scenario at once with the product's own words", async () => {
  const w = world({ refuse: true });
  const { scenario, dir } = await run(w);
  assert.equal(scenario().status, "error");
  assert.match(scenario().reason, /the launch was refused: 409 .*profile_ineligible/);
  assert.match(fs.readFileSync(path.join(dir, "01-stream-hold/timeline.jsonl"), "utf8"), /"ev":"api-error".*"status":409/);
  assert.deepEqual(w.stopped, []);
});

test("params.bench false leaves bench mode off", async () => {
  const w = world();
  let armed = false;
  const orig = w.host.newPage;
  w.host.newPage = async () => { const p = await orig(); const ev = p.evaluate; p.evaluate = async (...a) => { const r = await ev(...a); if (w.ls.get("quasar.bench") === "1") armed = true; return r; }; return p; };
  const { scenario } = await run(w, PLAN({ bench: false }));
  assert.equal(armed, false);
  assert.ok(scenario().status);
});
