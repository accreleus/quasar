// node --test scripts/validate/stream/run.test.mjs: the runner and stream-hold against a fake page
// and a fake API. No browser, no network. The page's own evaluate() callbacks really run, against
// a stand-in window, so the in-page code is exercised too.
import assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";
import { benchRows } from "./benchfold.mjs";
import { Recorder } from "./record.mjs";
import { configureTls, loadPlan, makeApi, openHost, probeClient, readThresholds, runSuite, secretTooShort, summarise } from "./run.mjs";
import { check, framelessSeconds, hitchCount, instrumentShare, judge, missingRate, observeWindows, shotCovered, verdict } from "./scenarios/stream-hold.mjs";

const HERE = path.dirname(fileURLToPath(import.meta.url));
const FIXTURE = JSON.parse(fs.readFileSync(path.join(HERE, "../../dx/tests/fixtures/appfold/bench-windows.json"), "utf8")).windows;
const [RECORDED, FROZEN] = FIXTURE; // a 60-frame second and a frameless one, as recorded
const HEALTHY = { ...RECORDED, missing_indices: 0 };
const BASE = "https://stack.example.test:8443";
const SID = "11111111-2222-3333-4444-555555555555";
const OTHER = "99999999-8888-7777-6666-555555555555";
const T0 = 1_800_000_000_000;
const T = readThresholds();
const API_BEARER = "ApiBearerLettersOnlyNoDigitsAtAll";
const PAGE_BEARER = "PageBearerAlsoLettersOnlyNoDigits";
const CREDS = { email: "tester@example.test", password: "correct-horse-battery" };

/** `n` one-second windows; the seconds listed in `frozen` are frameless. */
function windows(n, frozen = []) {
  return Array.from({ length: n }, (_, i) => ({ ...(frozen.includes(i) ? FROZEN : HEALTHY), t_start_ms: T0 + i * 1000, t_end_ms: T0 + (i + 1) * 1000, t_end_host_ms: T0 + (i + 1) * 1000 + 3, last_host_time_ms: frozen.includes(i) ? null : T0 + i * 1000 + 950 }));
}
const frames = (n) => Array.from({ length: n * 60 }, (_, i) => ({ present_ms: T0 + Math.round(i * (1000 / 60)), decoded: true }));
const response = (status, p, body, method = "POST", base = BASE) => ({ status: () => status, url: () => `${base}${p}`, request: () => ({ method: () => method }), json: async () => body, text: async () => JSON.stringify(body) });

/** Does a loader with these classes match `sel` (a comma list of .class and :not(.class) compounds, the only forms the probe uses)? */
const matches = (sel, classes) => {
  const have = classes.split(" ");
  const ok = sel.split(",").some((part) => [...part.matchAll(/(:not\()?\.([\w-]+)\)?/g)].every(([, not, c]) => (not ? !have.includes(c) : have.includes(c) || c === "sl-root")));
  return ok ? {} : null;
};

/** A world holding the fake clock, the fake page's state and the fake API's record. */
function world({ wins = windows(50), session = {}, failScreenshotAt = null, webtransport = true, noBench = false, refuse = false, noFrame = false, revealed = false, stopFails = false, closeFails = false, noUrl = false, url = null, stats = { frames: 100, decoded: 100 }, videoRate = 60, hiddenBetween = null, skew = 0, extraFrames = null, loader = null, onPlay = null, bare = false } = {}) {
  const w = { t: T0, stopped: [], live: [], url: "about:blank", ls: new Map(), shots: 0, failScreenshotAt, calls: [], secrets: new Set(), handlers: {}, opened: [], closed: [], clicks: [], gotos: [] };
  w.now = () => w.t;
  w.sleep = async (ms) => { w.t += ms; };
  w.emit = (e, m) => (w.handlers[e] ?? []).forEach((fn) => fn(m));
  const rows = { [SID]: { id: SID, app_id: "app-1", state: "running", transport: "webrtc", negotiated_codec: "h264", stream: { height: 1080, fps: 60 }, ...session } };
  w.api = {
    apps: async () => { w.calls.push("apps"); w.secrets.add(API_BEARER); return [{ id: "app-1", name: "Quasar Benchapp" }]; },
    session: async (id) => { w.calls.push(`session ${id}`); return rows[id]; },
    stop: async (id) => {
      w.calls.push(`stop ${id}`);
      if (stopFails) throw new Error("the session did not reach a terminal state in 30 s");
      w.stopped.push(id);
      w.live = w.live.filter((s) => s !== id);
    },
  };
  const qbench = { dump: () => ({ frames: noFrame ? [] : extraFrames ?? frames(wins.length), windows: noFrame ? [] : wins }), stats: () => (noFrame ? { frames: 0, decoded: 0 } : stats) };
  // A chainable fake locator: its element is enabled, and clicking "Play now" starts the session.
  const loc = (desc) => ({
    first: () => loc(desc),
    getByRole: (role, o) => loc(`${desc} > ${role}:${o?.name}`),
    evaluate: async (fn) => fn({ disabled: false, getAttribute: () => null, click: () => {
      w.clicks.push(desc);
      if (!/Play now/.test(desc)) return;
      if (onPlay) onPlay(w);
      if (bare) return;
      if (refuse) return w.emit("response", response(409, "/v1/sessions", { error: { code: "profile_ineligible" } }));
      w.live.push(SID);
      w.emit("response", response(201, "/v1/sessions", { session: { id: SID }, signaling: { token: "single-use" } }));
      if (!noUrl) w.url = url ?? `${BASE}/app/session/${SID}`;
    } }),
  });
  const page = () => ({
    on: (ev, fn) => { (w.handlers[ev] ??= []).push(fn); },
    goto: async (u) => { w.gotos.push(u); w.url = u; },
    fill: async () => {},
    click: async () => { w.ls.set("quasar.auth.token", PAGE_BEARER); w.ls.set("quasar.auth.user", "{}"); w.emit("response", response(200, "/v1/auth/login", { access_token: PAGE_BEARER })); w.afterLogin?.(w); },
    waitForURL: async () => {},
    url: () => w.url,
    close: async () => {},
    screenshot: async ({ path: p }) => {
      if (w.failScreenshotAt !== null && ++w.shots > w.failScreenshotAt) throw new Error("the page crashed");
      fs.writeFileSync(p, Buffer.from([0x89, 0x50, 0x4e, 0x47, 0]));
    },
    getByRole: (role, o) => loc(`${role}:${o?.name}`),
    evaluate: async (fn, arg) => {
      // Own enumerable properties are the stored items, as Object.keys(localStorage) sees them.
      const store = Object.create({ getItem(k) { return this[k] ?? null; }, setItem(k, v) { this[k] = v; }, removeItem(k) { delete this[k]; } });
      for (const [k, v] of w.ls) store[k] = v;
      const saved = Object.fromEntries(["window", "localStorage", "sessionStorage", "document"].map((k) => [k, globalThis[k]]));
      const hidden = hiddenBetween && w.t >= hiddenBetween[0] && w.t < hiddenBetween[1];
      const video = { getVideoPlaybackQuality: () => ({ totalVideoFrames: Math.round(((w.t - T0) / 1000) * videoRate) }) };
      const doc = { visibilityState: hidden ? "hidden" : "visible", hasFocus: () => true, querySelector: (sel) => (/sl-root/.test(sel) ? (loader ? matches(sel, loader) : revealed ? {} : null) : /video/.test(sel) && !noBench && !revealed && !noFrame ? video : null) };
      Object.assign(globalThis, { window: noBench ? {} : { __qBench: qbench }, localStorage: store, sessionStorage: { getItem: () => null }, document: doc });
      try { return await fn(arg); } finally { w.ls = new Map(Object.entries(store)); Object.assign(globalThis, saved); }
    },
  });
  w.host = {
    newPage: async () => {
      if (closeFails === "open" && w.opened.length >= 1) throw new Error("the browser would not open a page");
      const p = page();
      w.opened.push(p);
      return p;
    },
    closePage: async (p) => { if (closeFails === true && w.opened.indexOf(p) > 0) throw new Error("close failed"); w.closed.push(p); },
    browserNow: async () => w.t + skew,
    probeClient: async () => ({ kind: "web", browser: { name: "Chrome", version: "157.0.0.0" }, os: { name: "macOS", version: "26.1" }, cpu: { cores: 8 }, gpu: { adapter: null }, displays: [], decode: { h264: { supported: true, power_efficient: true } }, power: null, webtransport, webgpu: true }),
  };
  return w;
}

const PLAN = (...params) => ({ name: "t", scenarios: params.map((p) => ({ scenario: "stream-hold", params: { hold_s: 30, profile: "1080p60", ...p } })) });

async function run(w, plan = PLAN({}), extra = {}) {
  const dir = path.join(fs.mkdtempSync(path.join(os.tmpdir(), "e2e-run-")), "bundle");
  const result = await runSuite({ plan, dir, baseUrl: BASE, creds: CREDS, host: w.host, api: w.api, thresholds: T, secrets: w.secrets, now: w.now, sleep: w.sleep, ...extra });
  const scenario = (i = 1) => JSON.parse(fs.readFileSync(path.join(dir, `0${i}-stream-hold/scenario.json`), "utf8"));
  const by = (i = 1) => Object.fromEntries(scenario(i).criteria.map((c) => [c.id, c]));
  const timeline = (i = 1) => fs.readFileSync(path.join(dir, `0${i}-stream-hold/timeline.jsonl`), "utf8").trim().split("\n").filter(Boolean).map((l) => JSON.parse(l));
  return { dir, result, scenario, by, timeline };
}
const read = (dir, rel) => fs.readFileSync(path.join(dir, rel), "utf8");
const everyFile = (dir) => fs.readdirSync(dir, { recursive: true, withFileTypes: true }).filter((e) => e.isFile()).map((e) => path.join(e.parentPath, e.name));

// ── a healthy run ───────────────────────────────────────────────────────────

test("a healthy hold writes a bundle the validator accepts, passed, with the criteria the guide names", async () => {
  const w = world();
  const { dir, result, scenario, by } = await run(w);
  assert.deepEqual(result.errors, []);
  assert.equal(result.bundle, dir);
  const sc = scenario();
  assert.equal(sc.status, "pass", JSON.stringify(sc.criteria));
  assert.deepEqual(sc.criteria.map((c) => c.id), ["first_frame_s", "marker_decoded", "transport_as_asked", "codec_as_asked", "profile_as_asked", "frameless_seconds", "missing_indices_rate", "present_cadence"]);
  assert.equal(by().first_frame_s.pass, true);
  assert.equal(by().present_cadence.pass, true, JSON.stringify(by().present_cadence));
  assert.equal(sc.clock_offset_ms, 3);
  assert.deepEqual(sc.sessions, [SID]);
  const d = path.join(dir, "01-stream-hold");
  for (const f of ["timeline.jsonl", "metrics.jsonl", "bench-windows.json", "bench-frames.json", "session.json", "console.log", "shots/first-frame.png", "shots/t10s.png", "shots/end.png"]) assert.ok(fs.existsSync(path.join(d, f)), f);
  const manifest = JSON.parse(read(dir, "manifest.json"));
  assert.deepEqual(manifest.halves, { client: true, host: false });
  assert.ok(manifest.not_recorded.some((n) => n.fact === "audio_latency_ms" && n.why));
  assert.deepEqual(w.stopped, [SID]);
  assert.deepEqual(fs.readdirSync(path.dirname(dir)), ["bundle"], "the staging directory is gone");
});

test("the codec, frame rate and resolution are asked through the game page's Launch options, and no preference is touched", async () => {
  const w = world();
  await run(w, PLAN({ codec: "h265", profile: "1080p60" }));
  const sheet = "dialog:/^Launch options/";
  assert.deepEqual(w.clicks, ["button:Change", `${sheet} > radiogroup:Codec > radio:/^HEVC/`, `${sheet} > radiogroup:Frame rate > radio:/^60 fps/`, `${sheet} > radiogroup:Resolution > radio:/^1080p/`, `${sheet} > button:/^Play now/`]);
  assert.ok(w.gotos.some((u) => u === `${BASE}/app/game/app-1`), "the game page by the catalog's id, not a CSS selector built from the name");
  assert.deepEqual([...new Set(w.calls.map((c) => c.split(" ")[0]))].sort(), ["apps", "session", "stop"], "the runner reads the catalog and its own session, and stops it: it changes no setting");
});

test("a catalog name with CSS in it is never made into a selector", async () => {
  const w = world();
  const nasty = 'missing"] , button[aria-label="Steam';
  w.api.apps = async () => [{ id: "app-1", name: nasty }];
  const { scenario } = await run(w, PLAN({ app: nasty }));
  assert.equal(scenario().status, "pass");
  assert.ok(w.gotos.every((u) => !u.includes("aria-label")));
  assert.ok(w.clicks.every((c) => !c.includes("aria-label") && !c.includes("Steam")));
});

test("an account the app sends to the library instead of the game page is an error that says so", async () => {
  const w = world();
  const orig = w.host.newPage;
  w.host.newPage = async () => { const p = await orig(); const goto = p.goto; p.goto = async (u) => { await goto(u.includes("/app/game/") ? `${BASE}/app` : u); }; return p; };
  const { scenario } = await run(w);
  assert.equal(scenario().status, "error");
  assert.match(scenario().reason, /did not open the game page.*Classic layout/);
});

test("a page that never offers the option is an error with its reason", async () => {
  const w = world();
  const orig = w.host.newPage;
  w.host.newPage = async () => { const p = await orig(); const get = p.getByRole; p.getByRole = (r, o) => (o?.name === "Change" ? { first: () => ({ evaluate: async () => { throw new Error("none"); } }) } : get(r, o)); return p; };
  const { scenario } = await run(w);
  assert.equal(scenario().status, "error");
  assert.match(scenario().reason, /Change \(Launch options\) never became available/);
});

// ── D1: a run never claims more than it measured ────────────────────────────

test("a stream that never shows a frame fails first_frame_s with no value, and so fails the scenario", async () => {
  const w = world({ noFrame: true });
  const { result, scenario, by } = await run(w);
  assert.deepEqual(result.errors, []);
  assert.deepEqual(by().first_frame_s, { id: "first_frame_s", pass: false, observed: null, expected: "<= 30" });
  assert.equal(scenario().status, "fail");
  assert.deepEqual(w.stopped, [SID]);
});

test("with bench mode off a session row alone cannot pass a scenario: the first frame must be seen", async () => {
  const w = world({ noBench: true });
  const { scenario, by } = await run(w, PLAN({ bench: false }));
  assert.equal(by().first_frame_s.pass, false);
  assert.equal(by().transport_as_asked.pass, true);
  assert.equal(scenario().status, "fail");
});

test("the page's own reveal of the stream is a first frame on either transport (the WebTransport canvas has no video)", async () => {
  const w = world({ noBench: true, revealed: true, session: { transport: "webtransport" } });
  const { scenario, by } = await run(w, PLAN({ bench: false, transport: "webtransport" }));
  assert.equal(by().first_frame_s.pass, true);
  assert.equal(scenario().status, "pass");
});

test("the suite is ok, exit 0, only when something passed; nothing passed is failed with its reason", () => {
  assert.deepEqual(summarise({ counts: { pass: 1, fail: 0, error: 0, skipped: 2 }, errors: [], bundle: "/b" }), { status: "ok", code: 0, more: "" });
  assert.deepEqual(summarise({ counts: { pass: 0, fail: 0, error: 0, skipped: 3 }, errors: [], bundle: "/b" }), { status: "failed", code: 1, more: " reason=nothing-passed" });
  assert.deepEqual(summarise({ counts: { pass: 2, fail: 1, error: 0, skipped: 0 }, errors: [], bundle: "/b" }), { status: "failed", code: 1, more: "" });
  assert.deepEqual(summarise({ counts: { pass: 2, fail: 0, error: 1, skipped: 0 }, errors: [], bundle: "/b" }), { status: "failed", code: 1, more: "" });
  assert.equal(summarise({ counts: { pass: 2, fail: 0, error: 0, skipped: 0 }, errors: ["x: y"], bundle: null }).code, 2);
  assert.equal(summarise({ counts: { pass: 2, fail: 0, error: 0, skipped: 0 }, errors: [], bundle: "/b", closeFailed: new Error("x") }).code, 2);
});

test("pass needs the first frame and no failed criterion; a first frame that could not be judged is an error with its reason", () => {
  const ok = { id: "first_frame_s", pass: true };
  assert.deepEqual(verdict({ criteria: [ok, { id: "a", pass: null }] }), { status: "pass" });
  assert.deepEqual(verdict({ criteria: [ok, { id: "a", pass: false }] }), { status: "fail" });
  assert.deepEqual(verdict({ criteria: [{ id: "first_frame_s", pass: false }] }), { status: "fail" });
  assert.deepEqual(verdict({ criteria: [{ id: "a", pass: true }] }), { status: "error", reason: "no first frame could be judged" });
  assert.deepEqual(verdict({ criteria: [] }), { status: "error", reason: "no first frame could be judged" });
  assert.deepEqual(verdict({ status: "error", reason: "x", criteria: [] }), { status: "error", reason: "x" });
});

test("a session stop that is not confirmed makes the scenario an error naming the session, and 'stopped' is never written", async () => {
  const w = world({ stopFails: true });
  const { result, scenario, timeline } = await run(w);
  assert.deepEqual(result.errors, []);
  assert.equal(scenario().status, "error");
  assert.match(scenario().reason, new RegExp(`left behind: session ${SID} is not confirmed stopped`));
  const evs = timeline().map((r) => r.ev);
  assert.ok(evs.includes("stop-failed") && evs.includes("cleanup-failed"));
  assert.ok(!evs.includes("stopped"), "a stop the API did not confirm is not written as one");
});

test("a page that would not close makes the scenario an error saying so", async () => {
  const w = world({ closeFails: true });
  const { result, scenario } = await run(w);
  assert.deepEqual(result.errors, []);
  assert.equal(scenario().status, "error");
  assert.match(scenario().reason, /left behind: a page it opened would not close \(close failed\)/);
  assert.deepEqual(w.stopped, [SID]);
});

test("an unknown scenario, a bad hold and a browser that will not open a page are errors with reasons: the bundle is complete and the run goes on", async () => {
  for (const [bad, reason] of [[{ scenario: "does-not-exist", params: {} }, /no scenario named "does-not-exist"/], [{ scenario: "../escape", params: {} }, /no scenario named/]]) {
    const w = world();
    const { result, dir } = await run(w, { name: "t", scenarios: [bad, { scenario: "stream-hold", params: { hold_s: 30 } }] });
    assert.deepEqual(result.errors, []);
    const first = JSON.parse(fs.readFileSync(path.join(dir, fs.readdirSync(dir).find((n) => n.startsWith("01-")), "scenario.json"), "utf8"));
    assert.equal(first.status, "error");
    assert.match(first.reason, reason);
    assert.equal(JSON.parse(read(dir, "02-stream-hold/scenario.json")).status, "pass", "the next scenario ran");
    assert.equal(w.opened.length, 2, "no page was opened for the scenario that cannot run");
  }
  const w = world({ closeFails: "open" });
  const { result, scenario } = await run(w);
  assert.deepEqual(result.errors, []);
  assert.match(scenario().reason, /would not open a page/);
});

test("hold_s must be a positive finite number: 0, -1, 'nope', '20', NaN and null launch nothing", async () => {
  for (const hold_s of [0, -1, "nope", "20", NaN, null, Infinity]) {
    const w = world();
    const { result, scenario } = await run(w, PLAN({ hold_s }));
    assert.deepEqual(result.errors, [], String(hold_s));
    assert.equal(scenario().status, "error", String(hold_s));
    assert.match(scenario().reason, /hold_s must be a positive number/);
    assert.deepEqual(w.clicks, [], `${hold_s}: nothing was launched`);
  }
  assert.equal(check({ transport: "webrtc", codec: "h264", profile: "1080p60", hold_s: 90, app: "x" }), null);
  for (const p of [{ transport: "udp" }, { codec: "vp9" }, { profile: "1080" }, { profile: "1080p60 " }, { app: "" }, { codec: "__proto__" }]) {
    assert.ok(check({ transport: "webrtc", codec: "h264", profile: "1080p60", hold_s: 90, app: "x", ...p }), JSON.stringify(p));
  }
});

test("the smoke plan holds long enough to judge 60 s after the warm-up", () => {
  const plan = JSON.parse(read(HERE, "plans/smoke.json"));
  for (const s of plan.scenarios) assert.ok(s.params.hold_s >= T.warmup_exclude_s + 60, JSON.stringify(s));
});

// ── D2: the run owns what it created, by identity ───────────────────────────

test("a session the run did not start is never stopped, even when it is live in the same account", async () => {
  const w = world();
  w.live.push(OTHER);
  const { scenario } = await run(w);
  assert.equal(scenario().status, "pass");
  assert.deepEqual(w.stopped, [SID]);
  assert.ok(!w.calls.some((c) => c.includes(OTHER)), "it was not even looked at");
});

test("a launch the page made but never navigated to is stopped by the id in its own 201", async () => {
  const w = world({ noUrl: true });
  const sleep = async (ms) => { w.t += ms; };
  const { scenario, timeline } = await run(w, PLAN({}), { sleep });
  assert.equal(scenario().status, "error");
  assert.match(scenario().reason, /never reached a session page/);
  assert.deepEqual(scenario().sessions, [SID]);
  assert.deepEqual(w.stopped, [SID]);
  assert.ok(timeline().some((r) => r.ev === "session-started" && r.session === SID));
});

test("an id in the page URL that this run did not launch is an error, and that session is left alone (A17)", async () => {
  const hyphens = "-".repeat(36);
  for (const [url, why] of [[`${BASE}/app/session/${hyphens}`, /never reached a session page/], [`${BASE}/app/session/${OTHER}`, /did not launch/]]) {
    const w = world({ url });
    const { result, scenario } = await run(w);
    assert.deepEqual(result.errors, []);
    assert.equal(scenario().status, "error", url);
    assert.match(scenario().reason, why);
    assert.ok(!w.stopped.includes(OTHER) && !w.calls.includes(`stop ${hyphens}`));
  }
  // The seat's input: no launch 201 at all, the page just goes to a session the API shows as the user's own and running.
  const w = world({ bare: true, onPlay: (x) => { x.url = `${BASE}/app/session/${OTHER}`; } });
  w.api.session = async (id) => ({ id, app_id: "app-1", state: "running", transport: "webrtc", negotiated_codec: "h264", stream: { height: 1080, fps: 60 } });
  const { scenario } = await run(w);
  assert.equal(scenario().status, "error");
  assert.match(scenario().reason, /did not launch/);
  assert.deepEqual(w.stopped, [], "a session the run has no launch 201 for is never stopped");
});

test("a session row that is not the session asked for is an error, not a pass", async () => {
  const w = world({ session: { id: OTHER } });
  const { scenario } = await run(w);
  assert.equal(scenario().status, "error");
  assert.deepEqual(w.stopped, [SID]);
});

test("a session launched for another app is an error", async () => {
  const { scenario } = await run(world({ session: { app_id: "app-2" } }));
  assert.equal(scenario().status, "error");
  assert.match(scenario().reason, /another app/);
});

test("the runner works in a browser context of its own and closes only that one", async () => {
  const made = [];
  const browser = {
    contexts: () => { throw new Error("the default context is the person's: not touched"); },
    newContext: async (o) => { const c = { o, pages: 0, closed: false, newPage: async () => { c.pages += 1; return {}; }, close: async () => { c.closed = true; } }; made.push(c); return c; },
    close: async () => { throw new Error("an attached browser is not closed"); },
  };
  for (const insecure of [false, true]) {
    const { host, close } = await openHost(browser, { attached: true, insecureTls: insecure });
    await host.newPage();
    assert.deepEqual(made.at(-1).o, { ignoreHTTPSErrors: insecure, viewport: null });
    assert.equal(made.at(-1).pages, 1);
    await close();
    assert.equal(made.at(-1).closed, true);
  }
  let shut = false;
  const own = { newContext: async () => ({ newPage: async () => ({}) }), close: async () => { shut = true; } };
  await (await openHost(own, { attached: false, insecureTls: false })).close();
  assert.ok(shut, "a browser the runner launched is closed whole");
});

test("makeApi: a stop is confirmed only by a terminal state or a 404, a failed read is not an answer, and the bearer is a secret at once", async () => {
  const secrets = new Set();
  const seen = [];
  let reads = 0;
  const fetchFn = async (url, o) => {
    seen.push(`${o.method} ${url.replace(BASE, "")}`);
    const ok = (body, status = 200) => ({ status, json: async () => body });
    if (url.endsWith("/v1/auth/login")) return ok({ access_token: API_BEARER });
    if (o.method === "DELETE") return ok({}, 202);
    reads += 1;
    if (reads === 1) throw new Error("network down");
    return ok({ session: { state: reads < 4 ? "stopping" : "stopped" } });
  };
  const api = makeApi(BASE, CREDS, { fetchFn, sleep: async () => {}, secrets });
  await api.stop("a/b");
  assert.ok(secrets.has(API_BEARER));
  assert.ok(reads >= 4, "it kept asking until the API said stopped");
  assert.ok(seen.includes("DELETE /v1/sessions/a%2Fb"));
  const never = makeApi(BASE, CREDS, { fetchFn: async (url, o) => (url.endsWith("/login") ? { status: 200, json: async () => ({ access_token: "x" }) } : { status: o.method === "DELETE" ? 204 : 200, json: async () => ({ session: { state: "running" } }) }), sleep: async () => {}, secrets: new Set() });
  await assert.rejects(never.stop("s"), /did not reach a terminal state/);
  const gone = makeApi(BASE, CREDS, { fetchFn: async (url, o) => (url.endsWith("/login") ? { status: 200, json: async () => ({ access_token: "x" }) } : { status: o.method === "DELETE" ? 204 : 404, json: async () => ({}) }), sleep: async () => {}, secrets: new Set() });
  await gone.stop("s");
  assert.equal(await gone.session("s"), undefined);
});

// ── D3: redaction as values; a bundle exists only if valid ──────────────────

test("every secret the run holds is removed as a value: the password, the e-mail, the API bearer and the page's bearer", async () => {
  const w = world();
  const orig = w.host.newPage;
  w.host.newPage = async () => {
    const p = await orig();
    const get = p.getByRole;
    p.getByRole = (...a) => {
      for (const t of [`Authorization: Bearer ${API_BEARER}`, `quasar.auth.token=${PAGE_BEARER}`, `ice 10.20.30.40 ${BASE}/x ${CREDS.email} pw ${CREDS.password}`]) w.emit("console", { type: () => "log", text: () => t });
      return get(...a);
    };
    return p;
  };
  const { dir, result, scenario } = await run(w);
  assert.deepEqual(result.errors, []);
  assert.equal(scenario().status, "pass");
  const log = read(dir, "01-stream-hold/console.log");
  assert.match(log, /ice <address> https:\/\/<stack>\/x <user> pw <credential>/);
  for (const secret of [API_BEARER, PAGE_BEARER, CREDS.password, CREDS.email, "stack.example.test"]) {
    for (const f of everyFile(dir).filter((x) => !x.endsWith(".png"))) assert.ok(!fs.readFileSync(f, "utf8").includes(secret), `${secret} in ${path.relative(dir, f)}`);
  }
  assert.ok(JSON.parse(read(dir, "manifest.json")).redacted.length > 0);
});

test("a body that echoes the sign-in leaves JSON that parses: redaction never edits serialised text", async () => {
  const w = world({ refuse: true });
  const orig = w.host.newPage;
  w.host.newPage = async () => {
    const p = await orig();
    const get = p.getByRole;
    p.getByRole = (...a) => {
      w.emit("response", response(409, "/v1/sessions", { request: CREDS, login: `${BASE}/login?password=${encodeURIComponent(CREDS.password)}`, authorization: `Bearer ${API_BEARER}`, sig_token: "abcdef1234", nested: { password: "x1y2z3w4", ok: JSON.stringify(CREDS) } }));
      return get(...a);
    };
    return p;
  };
  const { dir, result, scenario } = await run(w, PLAN({}));
  assert.deepEqual(result.errors, []);
  for (const f of everyFile(dir).filter((x) => /\.jsonl?$/.test(x))) {
    for (const line of f.endsWith(".jsonl") ? read(f, "").split("\n").filter(Boolean) : [read(f, "")]) assert.doesNotThrow(() => JSON.parse(line), path.relative(dir, f));
  }
  assert.equal(scenario().status, "error");
  assert.ok(!read(dir, "01-stream-hold/timeline.jsonl").includes("abcdef1234"));
  for (const secret of [API_BEARER, CREDS.password, "x1y2z3w4"]) for (const f of everyFile(dir).filter((x) => !x.endsWith(".png"))) assert.ok(!fs.readFileSync(f, "utf8").includes(secret), `${secret} in ${path.relative(dir, f)}`);
});

test("Recorder walks values: literals in every spelling, credential keys dropped, rows each on their own", () => {
  const dir = path.join(fs.mkdtempSync(path.join(os.tmpdir(), "rec-")), "b");
  const secrets = new Set();
  const rec = new Recorder(dir, { literals: [["pw ord/1", "<credential>"]], secrets });
  secrets.add("OpaqueBearerLetters");
  rec.json("a.json", { s: "x pw ord/1 y", enc: encodeURIComponent("pw ord/1"), embedded: JSON.stringify({ k: "pw ord/1", b: "OpaqueBearerLetters" }), Authorization: "z", list: ["OpaqueBearerLetters", 3, null], password: "q" });
  const a = JSON.parse(read(dir, "a.json"));
  assert.deepEqual(a, { s: "x <credential> y", enc: "<credential>", embedded: JSON.stringify({ k: "<credential>", b: "<credential>" }), list: ["<credential>", 3, null] });
  rec.row("t.jsonl", { ev: "one", token: "t" });
  rec.row("t.jsonl", { ev: "two" });
  assert.deepEqual(read(dir, "t.jsonl").trim().split("\n").map((l) => JSON.parse(l)), [{ ev: "one" }, { ev: "two" }]);
});

test("a bundle that fails the validator is deleted, never renamed into place, and its problems come back", async () => {
  const w = world();
  w.host.probeClient = async () => ({ kind: "bogus" });
  const { dir, result } = await run(w);
  assert.equal(result.bundle, null);
  assert.ok(result.errors.some((e) => /client\.json: kind must be web or native/.test(e)));
  assert.ok(!fs.existsSync(dir));
  assert.deepEqual(fs.readdirSync(path.dirname(dir)), [], "no staging directory is left holding files");
});

test("a bundle is never written over another", async () => {
  const w = world();
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "e2e-exists-"));
  await assert.rejects(runSuite({ plan: PLAN({}), dir, baseUrl: BASE, creds: CREDS, host: w.host, api: w.api, thresholds: T, now: w.now, sleep: w.sleep }), /already exists/);
  assert.deepEqual(fs.readdirSync(dir), []);
});

test("certificate checks are on unless INSECURE_TLS asks, and the bundle says which", async () => {
  const env = {};
  assert.equal(configureTls(false, env), false);
  assert.deepEqual(env, {});
  assert.equal(configureTls(true, env), true);
  assert.equal(env.NODE_TLS_REJECT_UNAUTHORIZED, "0");
  for (const insecureTls of [false, true]) {
    const { dir } = await run(world(), PLAN({}), { insecureTls });
    assert.equal(JSON.parse(read(dir, "network.json")).tls_verification, insecureTls ? "off (INSECURE_TLS=1)" : "on");
  }
});

test("the sign-in does not reach npm ci or the runner's git child", () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "e2e-sh-"));
  fs.mkdirSync(path.join(root, "scripts/dx"), { recursive: true });
  fs.mkdirSync(path.join(root, "scripts/validate/stream"), { recursive: true });
  fs.copyFileSync(path.join(HERE, "../../dx/e2e_run.sh"), path.join(root, "scripts/dx/e2e_run.sh"));
  fs.writeFileSync(path.join(root, "scripts/validate/stream/run.mjs"), "console.log('ran');\n");
  const bin = path.join(root, "bin");
  fs.mkdirSync(bin);
  fs.writeFileSync(path.join(bin, "npm"), `#!/bin/sh\nenv > "${root}/npm.env"\n`, { mode: 0o755 });
  const r = spawnSync("bash", [path.join(root, "scripts/dx/e2e_run.sh")], { env: { PATH: `${bin}:${process.env.PATH}`, TARGET: "https://x.example.test", PLAN: "smoke", QUASAR_E2E_PASSWORD: "hunter2-secret", QUASAR_E2E_EMAIL: "u@example.test", QUASAR_E2E_CRED_FILE: "/nope" }, encoding: "utf8" });
  assert.equal(r.status, 0, r.stderr);
  const env = fs.readFileSync(path.join(root, "npm.env"), "utf8");
  assert.ok(!/QUASAR_E2E|hunter2/.test(env), "npm ci got the sign-in");
  fs.rmSync(root, { recursive: true });
  const run = fs.readFileSync(path.join(HERE, "run.mjs"), "utf8");
  assert.match(run, /execFileSync\("git"[^\n]*env: \{ PATH/, "the git child gets a named environment, not process.env");
});

test("the run refuses a client label that is not a plain identifier", () => {
  const r = spawnSync(process.execPath, [path.join(HERE, "run.mjs"), "--target", "https://x.example.test", "--plan", "smoke"], { env: { PATH: process.env.PATH, CLIENT: "my laptop.local!" }, encoding: "utf8" });
  assert.equal(r.status, 2);
  assert.match(r.stdout, /RESULT status=error target=e2e-run/);
  assert.match(r.stderr, /CLIENT must be a plain identifier/);
});

// ── D4: a criterion means what its name says ────────────────────────────────

const ROW = { transport: "webrtc", negotiated_codec: "h264", stream: { height: 1080, fps: 60 } };
const PARAMS = { transport: "webrtc", codec: "h264", profile: "1080p60" };
const J = (o = {}) => Object.fromEntries(judge({ params: PARAMS, session: ROW, windows: windows(50), frames: frames(50), firstFrameS: 3, stats: { decoded: 10 }, thresholds: T, ...o }).map((c) => [c.id, c]));

test("transport_as_asked fails when the client fell back, and says what the stream plan and the client reported", () => {
  const wt = { ...PARAMS, transport: "webtransport" };
  assert.equal(J({ params: wt, session: { ...ROW, transport: "webtransport" } }).transport_as_asked.pass, true);
  const fell = J({ params: wt, session: { ...ROW, transport: "webtransport", transport_fallback_reason: "connect_timeout" } }).transport_as_asked;
  assert.equal(fell.pass, false);
  assert.equal(fell.observed, "webtransport (the client fell back to webrtc: connect_timeout)");
  const planned = J({ params: wt, session: { ...ROW, transport: "webrtc", transport_reason: "profile_disallows" } }).transport_as_asked;
  assert.equal(planned.pass, false);
  assert.equal(planned.observed, "webrtc (stream plan: profile_disallows)");
});

test("codec_as_asked names the field it read", () => {
  assert.equal(J().codec_as_asked.observed, "h264 (negotiated_codec)");
  assert.equal(J({ session: { ...ROW, negotiated_codec: undefined, stream: { ...ROW.stream, codec: "h264" } } }).codec_as_asked.observed, "h264 (stream.codec)");
  const py = J({ params: { ...PARAMS, codec: "pyrowave" }, session: { ...ROW, pyrowave: { active: true } } }).codec_as_asked;
  assert.deepEqual([py.pass, py.observed], [true, "pyrowave (pyrowave.active/used)"]);
});

test("marker_decoded is whether any marker frame decoded, not whether the marker is locked now", () => {
  assert.equal(J({ stats: { decoded: 1, located: false } }).marker_decoded.pass, true);
  assert.equal(J({ stats: { decoded: 0, located: true } }).marker_decoded.pass, false);
  assert.equal(J({ stats: null }).marker_decoded.pass, null);
});

test("frameless_seconds counts one-second windows with no frame after the product's warm-up, not runs, and not a hidden span", () => {
  const warm = T.warmup_exclude_s;
  const at = (...s) => windows(50, s);
  // a 3 s stall: three windows, one run
  assert.equal(J({ windows: at(warm + 5, warm + 6, warm + 7) }).frameless_seconds.observed, 3);
  assert.equal(J({ windows: at(warm + 5, warm + 6, warm + 7) }).frameless_seconds.pass, false);
  // the warm-up is the product's, read from thresholds.json: a stall inside it is not counted, one just after is
  assert.equal(J({ windows: at(warm - 1) }).frameless_seconds.pass, true);
  assert.equal(J({ windows: at(warm) }).frameless_seconds.pass, false);
  // a page that was hidden over the stall is not a stream that froze
  const hidden = [[T0 + (warm + 4) * 1000, T0 + (warm + 9) * 1000]];
  assert.equal(J({ windows: at(warm + 5, warm + 6), hidden }).frameless_seconds.pass, true);
  assert.equal(J({ windows: [] }).frameless_seconds.pass, null);
  assert.equal(framelessSeconds([{ n: 0, no_image: 4 }, { n: 0 }, { n: 5 }]), 1, "frames that could not be read are not a missing frame");
  assert.deepEqual(observeWindows(windows(50), warm).length, 50 - warm);
});

test("missing_indices_rate is recorded and not judged, over the indices that were due, and null for absent or negative counts", () => {
  const c = J({ windows: windows(50).map((w) => ({ ...w, decoded: 99, missing_indices: 1 })) }).missing_indices_rate;
  assert.equal(c.pass, null);
  assert.equal(c.observed, null);
  assert.match(c.expected, /^not judged: no limit calibrated on the marker app yet; measured 1\.00 % over 30 windows$/);
  assert.equal(missingRate([{ decoded: 99, missing_indices: 1 }]), 0.01);
  assert.equal(missingRate([{ decoded: 98, missing_indices: 1 }, { decoded: 100, missing_indices: 3 }]), 4 / 202);
  for (const bad of [{ decoded: 60 }, { missing_indices: 1 }, { decoded: 60, missing_indices: -1 }, { decoded: -1, missing_indices: 0 }, { decoded: NaN, missing_indices: 0 }, { decoded: 0, missing_indices: 0 }]) assert.equal(missingRate([bad]), null, JSON.stringify(bad));
  assert.match(J({ windows: windows(50).map(({ missing_indices, ...w }) => w) }).missing_indices_rate.expected, /no usable counts/);
});

test("present_cadence is the control plane's hitch rule: windows need the product's interval count, the warm-up is the product's, and the count is judged at its minimum", () => {
  const warm = T.warmup_exclude_s;
  // a steady 50 ms beat; the jerky seconds alternate 10/90 ms (the same mean, sd 40)
  const stream = (jerkySeconds) => {
    const out = [];
    let t = T0;
    for (let s = 0; s < 50; s += 1) for (let i = 0; i < 20; i += 1) { t += s >= warm + 1 && s < warm + 1 + jerkySeconds ? (i % 2 ? 90 : 10) : 50; out.push({ present_ms: t }); }
    return out;
  };
  const run = (f, over = {}) => hitchCount(f, T0 + warm * 1000, T0 + 50_000, [], { ...T, ...over });
  assert.equal(run(stream(0)).hitches, 0);
  assert.ok(run(stream(3)).hitches >= 3);
  // at hitch_min_samples windows it fails, one fewer passes
  const exactly = J({ frames: stream(T.hitch_min_samples) }).present_cadence;
  assert.equal(exactly.pass, false, JSON.stringify(exactly));
  assert.equal(J({ frames: stream(T.hitch_min_samples - 1) }).present_cadence.pass, true);
  // a second with fewer than present_min_samples intervals is a fragment, not a cadence
  const sparse = Array.from({ length: 120 }, (_, i) => ({ present_ms: T0 + warm * 1000 + i * 250 + (i % 2 ? 200 : 0) }));
  assert.deepEqual(hitchCount(sparse, T0, T0 + 1e6, [], T), { judged: 0, hitches: 0 });
  // and with no judged window at all the criterion is not judged, never a clean zero
  assert.equal(J({ frames: [] }).present_cadence.pass, null);
  // a jerk before the warm-up ends is not counted
  assert.equal(run(stream(0).map((f, i) => ({ present_ms: f.present_ms + (i < 10 * 20 && i % 2 ? 40 : 0) }))).hitches, 0);
});

test("present_cadence is not judged when the bench callback missed the frames the video decoded (issue #1304)", () => {
  const video = (rate) => Array.from({ length: 50 }, (_, s) => ({ t: T0 + s * 1000, videoFrames: s * rate }));
  assert.ok(instrumentShare(frames(50), video(60), T0 + 20_000, T0 + 49_000) > 0.99);
  assert.ok(instrumentShare(frames(50), video(150), T0 + 20_000, T0 + 49_000) < 0.5);
  assert.equal(instrumentShare(frames(50), [], T0, T0 + 1e6), null);
  const missed = J({ videoSamples: video(150) }).present_cadence;
  assert.equal(missed.pass, null);
  assert.match(missed.expected, /saw \d+% of the frames the video decoded.*#1304/);
  assert.equal(J({ videoSamples: video(60) }).present_cadence.pass, true);
});

test("an app without a marker leaves the marker criteria unjudged or failed, and judges what needs none", async () => {
  const w = world({ stats: { frames: 100, decoded: 0 }, wins: windows(50).map((x) => ({ ...x, n: 60, decoded: 0, undecoded: 60, missing_indices: 0 })) });
  const { result, scenario, by } = await run(w);
  assert.deepEqual(result.errors, []);
  assert.equal(by().marker_decoded.pass, false);
  assert.equal(by().frameless_seconds.pass, true);
  assert.equal(by().missing_indices_rate.pass, null);
  assert.equal(by().present_cadence.pass, true, "the cadence needs no marker");
  assert.equal(scenario().status, "fail");
});

test("transport, codec and profile are judged against the session row", async () => {
  const w = world({ session: { transport: "webtransport", negotiated_codec: "h265", stream: { height: 720, fps: 60 } } });
  const { by } = await run(w, PLAN({ transport: "webtransport", codec: "h265" }));
  assert.equal(by().transport_as_asked.pass, true);
  assert.equal(by().codec_as_asked.pass, true);
  assert.equal(by().profile_as_asked.pass, false);
  assert.equal(by().profile_as_asked.observed, "720p60");
});

// ── the other runner paths ──────────────────────────────────────────────────

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
  const { result, scenario } = await run(w, PLAN({ hold_s: 90 }, {}), { abort, sleep });
  assert.deepEqual(result.errors, []);
  assert.equal(scenario(1).status, "error");
  assert.match(scenario(1).reason, /interrupted/);
  assert.equal(scenario(2).status, "skipped");
  assert.deepEqual(w.stopped, [SID]);
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
  w.host.probeClient = async () => ({ kind: "web", browser: { name: "Chrome", version: "1.0.0" }, os: { name: "Linux", version: null }, decode: { av1: { supported: false } }, webtransport: true, webgpu: false });
  assert.match((await run(w, PLAN({ codec: "av1" }))).scenario().reason, /cannot decode av1/);
  assert.match((await run(w, PLAN({ codec: "pyrowave", transport: "webtransport" }))).scenario().reason, /cannot decode pyrowave/);
  assert.match((await run(world(), PLAN({ app: "No Such App" }))).scenario().reason, /not in this user's catalog/);
});

test("a launch the product refuses ends the scenario at once with the product's own words", async () => {
  const w = world({ refuse: true });
  const { scenario, dir } = await run(w);
  assert.equal(scenario().status, "error");
  assert.match(scenario().reason, /the launch was refused: 409 .*profile_ineligible/);
  assert.match(read(dir, "01-stream-hold/timeline.jsonl"), /"ev":"api-error".*"status":409/);
  assert.deepEqual(w.stopped, []);
});

test("bench mode is armed in the runner's own context and left off with params.bench false", async () => {
  for (const [bench, expected] of [[undefined, "1"], [false, undefined]]) {
    const w = world();
    let armed;
    const orig = w.host.newPage;
    w.host.newPage = async () => { const p = await orig(); const ev = p.evaluate; p.evaluate = async (...a) => { const r = await ev(...a); armed ??= w.ls.get("quasar.bench"); return r; }; return p; };
    await run(w, PLAN(bench === undefined ? {} : { bench }));
    assert.equal(armed, expected);
  }
});

// ── D5: where and when ──────────────────────────────────────────────────────

test("the bundle names the client, measures the runner-to-browser clock offset, and stamps the timeline on the browser's clock", async () => {
  const w = world({ skew: 5000 });
  const { dir, timeline, result } = await run(w, PLAN({}), { label: "mac-test-client" });
  assert.deepEqual(result.errors, []);
  const client = JSON.parse(read(dir, "client.json"));
  assert.equal(client.label, "mac-test-client");
  assert.deepEqual(client.clock, { browser_minus_runner_ms: 5000, round_trip_ms: 0 });
  const t = timeline();
  assert.ok(t.every((r) => r.clock === "client" && r.clock_source === "browser"));
  assert.ok(t[0].ts_unix_ms >= T0 + 5000, "stamped on the browser's clock");
  assert.match(read(dir, "01-stream-hold/console.log") + "x", /x/);
});

test("a hidden tab is excluded from the judged span", async () => {
  const warm = T.warmup_exclude_s;
  const w = world({ wins: windows(50, [warm + 5, warm + 6]), hiddenBetween: [T0 + 1000 * (warm + 3), T0 + 1000 * (warm + 9)] });
  const { by } = await run(w, PLAN({ hold_s: 60 }));
  assert.equal(by().frameless_seconds.pass, true);
});

const NAV = (o) => ({ userAgentData: undefined, hardwareConcurrency: 8, platform: "MacIntel", userAgent: "UA", gpu: undefined, mediaCapabilities: { decodingInfo: async () => ({ supported: true, powerEfficient: false }) }, ...o });
async function inPage(nav, { raf = false } = {}) {
  const keep = Object.fromEntries(["navigator", "document", "screen", "devicePixelRatio", "requestAnimationFrame", "WebTransport", "VideoDecoder", "setTimeout"].map((k) => [k, Object.getOwnPropertyDescriptor(globalThis, k)]));
  const set = (k, v) => Object.defineProperty(globalThis, k, { value: v, configurable: true, writable: true });
  const realTimeout = globalThis.setTimeout;
  set("navigator", nav);
  set("document", { createElement: () => ({ getContext: () => ({ getExtension: () => ({ UNMASKED_RENDERER_WEBGL: 37446 }), getParameter: (p) => (p === 37446 ? "Apple M-class GPU" : null) }) }) });
  set("screen", { width: 2560, height: 1440 });
  set("devicePixelRatio", 2);
  set("requestAnimationFrame", raf ? (fn) => realTimeout(() => fn(performance.now()), 4) : () => 0);
  set("WebTransport", function WebTransport() {});
  set("VideoDecoder", Object.assign(function VideoDecoder() {}, { isConfigSupported: async () => ({ supported: true }) }));
  set("setTimeout", (fn, ms) => (ms === 2000 && !raf ? realTimeout(fn, 5) : realTimeout(fn, ms).unref()));
  try { return await probeClient({ evaluate: (fn) => fn() }); } finally { for (const [k, d] of Object.entries(keep)) d ? Object.defineProperty(globalThis, k, d) : delete globalThis[k]; }
}

test("client.json: a full charging battery is not a power source, a hidden tab does not hang the probe, and the GPU is the unmasked renderer", async () => {
  const full = await inPage(NAV({ getBattery: async () => ({ charging: true, level: 1 }) }));
  assert.equal(full.power, null);
  assert.equal(full.displays[0].refresh_hz, null, "no animation frames, so no refresh rate, and no hang");
  assert.equal(full.gpu.adapter, "Apple M-class GPU");
  assert.deepEqual(full.decode.h264, { supported: true, power_efficient: false, webcodecs: true });
  assert.deepEqual(Object.keys(full.decode).sort(), ["av1", "h264", "h265"]);
  const half = await inPage(NAV({ getBattery: async () => ({ charging: false, level: 0.5 }) }), { raf: true });
  assert.deepEqual(half.power, { source: "battery", level: 0.5 });
  assert.ok(half.displays[0].refresh_hz > 0, "with frames, the refresh rate is measured");
  assert.equal((await inPage(NAV({ getBattery: async () => ({ charging: true, level: 0.8 }) }))).power.source, "ac");
  assert.equal((await inPage(NAV({}))).power, null);
});

// ── the instruments that did not change ─────────────────────────────────────

test("readThresholds states four numbers and refuses a file that does not", () => {
  assert.deepEqual(Object.keys(T).sort(), ["hitch_min_samples", "hitch_sd_ms", "present_min_samples", "warmup_exclude_s"]);
  const f = path.join(fs.mkdtempSync(path.join(os.tmpdir(), "thr-")), "t.json");
  fs.writeFileSync(f, JSON.stringify({ thresholds: { "classifier.hitch_sd_ms": { value: 18 } } }));
  assert.throws(() => readThresholds(f), /states no number for classifier\.hitch_min_samples/);
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
  const canvas = { tagName: "CANVAS", getBoundingClientRect: () => ({ left: 0, top: 0, width: 1920, height: 1080 }), dataset: { compositorW: "1920", compositorH: "1080" } };
  await shotCovered(page(canvas), rec, "a", true);
  assert.deepEqual(taken, [["a", 1]], "the cover was on the page during the shot");
  assert.equal(covers.length, 0, "and is gone after it");
  await shotCovered(page(null), rec, "b", true);
  assert.deepEqual(taken, [["a", 1]]);
  assert.deepEqual(steps, ["shot-skipped"]);
  await shotCovered(page(null), rec, "c", false);
  assert.deepEqual(taken.at(-1), ["c", 0]);
});

// ── round 3: the second review's list (A15..A25) ────────────────────────────

const LATE = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee";
const onPage = (w, fn) => { const orig = w.host.newPage; w.host.newPage = async () => fn(await orig()); };

test("A15: a page that changes something on the stack the run cannot put back makes the scenario an error naming it", async () => {
  // The game page's per-game settings calls are stubs today (gameData.ts, issue #720); this is what a real PUT would look like.
  const w = world({ onPlay: (x) => x.emit("response", response(200, `/v1/apps/app-1/settings`, {}, "PUT")) });
  const { scenario } = await run(w);
  assert.equal(scenario().status, "error");
  assert.match(scenario().reason, /left behind: the page changed something on the stack \(PUT \/v1\/apps\/app-1\/settings\) that this run cannot put back/);
  assert.deepEqual(w.stopped, [SID]);
  // The sessions endpoint's own writes (the page stopping its session) and reads are not changes of a preference.
  const ok = await run(world({ onPlay: (x) => x.emit("response", response(200, `/v1/sessions/${SID}`, {}, "DELETE")) }));
  assert.equal(ok.scenario().status, "pass");
});

test("A16: a 201 from another origin is not this run's session and is never stopped", async () => {
  const w = world({ bare: true, onPlay: (x) => x.emit("response", response(201, "/v1/sessions", { session: { id: OTHER } }, "POST", "https://other.example.test")) });
  const { scenario } = await run(w);
  assert.equal(scenario().status, "error");
  assert.deepEqual(scenario().sessions, []);
  assert.deepEqual(w.stopped, []);
});

test("A18: a launch 201 delivered while the page is closing is still stopped and listed", async () => {
  const w = world({ refuse: true });
  const close = w.host.closePage;
  w.host.closePage = async (p) => {
    await close(p);
    if (w.opened.indexOf(p) > 0) { await Promise.resolve(); w.emit("response", response(201, "/v1/sessions", { session: { id: LATE } })); }
  };
  const { scenario, timeline } = await run(w);
  assert.equal(scenario().status, "error"); // the refusal
  assert.deepEqual(w.stopped, [LATE]);
  assert.deepEqual(scenario().sessions, [LATE]);
  assert.ok(timeline().some((r) => r.ev === "stopped" && r.session === LATE));
});

test("A19: what the page says before its bearer is known is held, then written without it", async () => {
  const w = world();
  // In the same turn as the login answer, before the runner has read the answer's body (the seat's input).
  w.afterLogin = () => { w.emit("console", { type: () => "log", text: () => `quasar.auth.token=${PAGE_BEARER}` }); w.emit("pageerror", { message: `boom ${PAGE_BEARER}` }); };
  const { dir, scenario, result } = await run(w);
  assert.deepEqual(result.errors, []);
  assert.equal(scenario().status, "pass");
  const log = read(dir, "01-stream-hold/console.log");
  assert.match(log, /quasar\.auth\.token=<credential>/);
  assert.match(log, /\[pageerror\] boom <credential>/);
  for (const f of everyFile(dir).filter((x) => !x.endsWith(".png"))) assert.ok(!fs.readFileSync(f, "utf8").includes(PAGE_BEARER), path.relative(dir, f));
});

test("A19: when the page's bearer never becomes known what was held is dropped, and the timeline says how many lines", async () => {
  const w = world();
  onPage(w, (p) => {
    p.click = async () => { w.emit("console", { type: () => "log", text: () => "line one" }); w.emit("console", { type: () => "log", text: () => "line two" }); };
    return p;
  });
  const { dir, scenario, timeline, result } = await run(w);
  assert.deepEqual(result.errors, []);
  assert.ok(!read(dir, "01-stream-hold/console.log").includes("line"), "nothing the page said was written");
  const drop = timeline().find((r) => r.ev === "pre-auth-evidence-dropped");
  assert.ok(drop && drop.lines >= 2, JSON.stringify(drop));
  assert.ok(["pass", "error", "fail"].includes(scenario().status));
});

test("A21: a failed or ended loader is not a first frame, whatever else it carries", async () => {
  for (const loader of ["sl-root is-locking is-failed is-over", "sl-root is-streaming is-failed is-over", "sl-root is-streaming is-failed"]) {
    const w = world({ noBench: true, loader });
    const { scenario, by } = await run(w, PLAN({ bench: false }));
    assert.equal(by().first_frame_s.pass, false, loader);
    assert.equal(scenario().status, "fail", loader);
  }
  // The seat's probe: bench off, no video frames, a stopped session behind the failed loader.
  const stopped = await run(world({ noBench: true, loader: "sl-root is-streaming is-failed is-over", session: { state: "stopped" } }), PLAN({ bench: false }));
  assert.notEqual(stopped.scenario().status, "pass");
  const live = world({ noBench: true, loader: "sl-root is-streaming" });
  assert.equal((await run(live, PLAN({ bench: false }))).by().first_frame_s.pass, true);
});

test("A21: a session that is already stopped or failed when the hold ends is an error, whatever the criteria say", async () => {
  for (const state of ["stopped", "failed"]) {
    const { scenario, by } = await run(world({ session: { state } }));
    assert.equal(by().first_frame_s.pass, true, "the criteria themselves look fine");
    assert.equal(scenario().status, "error");
    assert.match(scenario().reason, /the session ended during the hold/);
  }
});

test("A22: a key is never rewritten by a secret; a key that is one is dropped; two keys redaction makes equal invalidate the bundle", async () => {
  const dir = path.join(fs.mkdtempSync(path.join(os.tmpdir(), "rec-")), "b");
  const rec = new Recorder(dir, { literals: [["hunter222", "<credential>"]] });
  rec.json("a.json", { "hunter222": 1, "hunter222-note": 2, id: "11111111-2222" });
  assert.deepEqual(JSON.parse(read(dir, "a.json")), { "hunter222-note": 2, id: "11111111-2222" });
  assert.deepEqual(rec.problems, []);
  rec.json("b.json", { "Bearer abc12345678901": 1, "Bearer zzz98765432109": 2 });
  assert.match(rec.problems[0], /two members of one object the same key/);
});

test("A22: a password or e-mail local part under 8 characters is refused before anything starts", () => {
  assert.equal(secretTooShort({ email: "tester-account@example.test", password: "correct-horse-battery" }), null);
  for (const creds of [{ email: "tester-account@example.test", password: "111111" }, { email: "t@example.test", password: "correct-horse-battery" }, { email: "abcdefg@example.test", password: "correct-horse-battery" }]) assert.match(secretTooShort(creds), /at least 8 characters.*longer password/, JSON.stringify(creds));
  const r = spawnSync(process.execPath, [path.join(HERE, "run.mjs"), "--target", "https://x.example.test", "--plan", "smoke"], { env: { PATH: process.env.PATH, QUASAR_E2E_EMAIL: "tester-account@example.test", QUASAR_E2E_PASSWORD: "111111" }, encoding: "utf8" });
  assert.equal(r.status, 2);
  assert.match(r.stdout, /RESULT status=error target=e2e-run/);
  assert.match(r.stderr, /Use a test user with a longer password/);
  assert.ok(!r.stdout.includes("111111") && !r.stderr.includes("111111"));
});

test("A23: a negative or non-finite first-frame time is not a pass, and windows with no valid frame count do not make a clean zero", async () => {
  for (const firstFrameS of [-1, NaN, Infinity]) {
    const c = J({ firstFrameS }).first_frame_s;
    assert.equal(c.pass, null, String(firstFrameS));
    assert.equal(c.observed, null);
    assert.equal(verdict({ criteria: Object.values(J({ firstFrameS })) }).status, "error");
  }
  assert.equal(J({ firstFrameS: 0 }).first_frame_s.pass, true);
  const unknown = windows(50).map(({ n, ...w }) => w);
  const c = J({ windows: unknown }).frameless_seconds;
  assert.equal(c.pass, null);
  assert.equal(c.observed, null);
  const bad = windows(50).map((w, i) => (i === 40 ? { ...w, n: -3 } : w));
  assert.equal(J({ windows: bad }).frameless_seconds.pass, null);
  assert.equal(J({ windows: windows(50) }).frameless_seconds.pass, true);
});

test("A24: an unknown key in a plan entry, its params or the plan itself is refused; omitted params take the documented defaults", async () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "plan-"));
  const write = (o) => { const f = path.join(dir, "p.json"); fs.writeFileSync(f, JSON.stringify(o)); return f; };
  assert.throws(() => loadPlan(write({ name: "x", hold_s: 0, scenarios: [{ scenario: "stream-hold" }] })), /key this runner does not define: "hold_s"/);
  assert.equal(loadPlan(write({ name: "x", scenarios: [{ scenario: "stream-hold" }] })).scenarios.length, 1);
  assert.throws(() => loadPlan(write([])), /not an object/);
  for (const [entry, why] of [[{ scenario: "stream-hold", hold_s: 30 }, /plan entry has a key this runner does not define: "hold_s"/], [{ scenario: "stream-hold", params: { hold_s: 30, codc: "av1" } }, /params has a key stream-hold does not define: "codc"/], [{ scenario: "stream-hold", params: [] }, /params must be an object/], [{ scenario: "stream-hold", params: "x" }, /params must be an object/]]) {
    const w = world();
    const { scenario, result } = await run(w, { name: "t", scenarios: [entry] });
    assert.deepEqual(result.errors, []);
    assert.equal(scenario().status, "error");
    assert.match(scenario().reason, why);
    assert.deepEqual(w.clicks, [], "nothing was launched");
  }
  const { scenario, dir: out } = await run(world(), { name: "t", scenarios: [{ scenario: "stream-hold" }] });
  assert.equal(scenario().status, "pass");
  assert.equal(JSON.parse(read(out, "manifest.json")).plan.scenarios[0].params.hold_s, 90);
});

test("A25: an inherited NODE_TLS_REJECT_UNAUTHORIZED=0 is removed unless the caller asked, and npm ci does not get it", () => {
  const env = { NODE_TLS_REJECT_UNAUTHORIZED: "0", OTHER: "kept" };
  assert.equal(configureTls(false, env), false);
  assert.deepEqual(env, { OTHER: "kept" });
  assert.equal(configureTls(true, env), true);
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "e2e-tls-"));
  fs.mkdirSync(path.join(root, "scripts/dx"), { recursive: true });
  fs.mkdirSync(path.join(root, "scripts/validate/stream"), { recursive: true });
  fs.copyFileSync(path.join(HERE, "../../dx/e2e_run.sh"), path.join(root, "scripts/dx/e2e_run.sh"));
  fs.writeFileSync(path.join(root, "scripts/validate/stream/run.mjs"), "console.log('ran');\n");
  const bin = path.join(root, "bin");
  fs.mkdirSync(bin);
  fs.writeFileSync(path.join(bin, "npm"), `#!/bin/sh\nenv > "${root}/npm.env"\n`, { mode: 0o755 });
  const r = spawnSync("bash", [path.join(root, "scripts/dx/e2e_run.sh")], { env: { PATH: `${bin}:${process.env.PATH}`, TARGET: "https://x.example.test", PLAN: "smoke", NODE_TLS_REJECT_UNAUTHORIZED: "0" }, encoding: "utf8" });
  assert.equal(r.status, 0, r.stderr);
  assert.ok(!/NODE_TLS_REJECT_UNAUTHORIZED/.test(fs.readFileSync(path.join(root, "npm.env"), "utf8")), "npm ci got the switch");
  fs.rmSync(root, { recursive: true });
});
