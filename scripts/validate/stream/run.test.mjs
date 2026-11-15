// node --test scripts/validate/stream/run.test.mjs: the runner and stream-hold against a fake page
// and a fake API. No browser, no network. The page's own evaluate() callbacks really run, against
// a stand-in window, so the in-page code is exercised too.
import assert from "node:assert/strict";
import { execFileSync, spawn, spawnSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";
import { benchRows } from "./benchfold.mjs";
import { Recorder } from "./record.mjs";
import { loadWebDist } from "./webdist.mjs";
import { configureTls, loadPlan, makeApi, openHost, probeClient, readThresholds, runnerState, runSuite, secretTooShort, summarise } from "./run.mjs";
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
const request = (method, p, base = BASE) => ({ url: () => `${base}${p}`, method: () => method });
const response = (status, p, body, method = "POST", base = BASE) => ({ status: () => status, url: () => `${base}${p}`, request: () => ({ method: () => method }), json: async () => body, text: async () => JSON.stringify(body) });

/** Does a loader with these classes match `sel` (a comma list of .class and :not(.class) compounds, the only forms the probe uses)? */
const matches = (sel, classes) => {
  const have = classes.split(" ");
  const ok = sel.split(",").some((part) => [...part.matchAll(/(:not\()?\.([\w-]+)\)?/g)].every(([, not, c]) => (not ? !have.includes(c) : have.includes(c) || c === "sl-root")));
  return ok ? {} : null;
};

/** A world holding the fake clock, the fake page's state and the fake API's record. */
function world({ wins = windows(50), session = {}, failScreenshotAt = null, webtransport = true, noBench = false, refuse = false, noFrame = false, revealed = false, stopFails = false, closeFails = false, noUrl = false, url = null, stats = { frames: 100, decoded: 100 }, videoRate = 60, hiddenBetween = null, skew = 0, extraFrames = null, loader = null, onPlay = null, bare = false, hooksDir = null, lockOnClick = true, hud = false, layerStays = false, lockDelayMs = 0, swallowMoves = false, layerAfterMs = 0, noLayer = false, keyUpFails = null, swallowKeyUp = null, fallbackDelayMs = 3000, fallbackReason = "stream_lost", noFallback = false } = {}) {
  const w = { cdp: [], listeners: {}, locked: false, lastX: null, lastY: null, videoStart: T0, hooksDir, faultSeenAt: null, t: T0, stopped: [], live: [], url: "about:blank", ls: new Map(), shots: 0, failScreenshotAt, calls: [], gone: new Set(), secrets: new Set(), handlers: {}, opened: [], closed: [], clicks: [], gotos: [], playAt: null, stageClicks: 0, layerClicks: 0, hudOpen: false, hudClicks: 0, lockAt: 0 };
  // The page's "Click to play" layer (SessionPage.tsx): raised some time after Play and removed by the FIRST click on it, whether or not that click
  // took the lock (handleClickToPlay); a click before it only opens the HUD shelf.
  const layerUp = () => !noLayer && w.playAt !== null && w.t >= w.playAt + layerAfterMs && !w.locked && (layerStays || w.layerClicks === 0);
  // With `hud`, a click on the stage opens the shelf, whose "Capture input" button takes the lock (HudBar.tsx onGrab).
  const hudButton = { x: 30, y: 620 };
  const hudUp = () => hud && w.hudOpen && !w.locked;
  w.now = () => w.t;
  w.sleep = async (ms) => { w.t += ms; };
  w.emit = (e, m) => (w.handlers[e] ?? []).forEach((fn) => fn(m));
  const rows = { [SID]: { id: SID, app_id: "app-1", state: "running", transport: "webrtc", negotiated_codec: "h264", stream: { height: 1080, fps: 60 }, ...session } };
  w.api = {
    apps: async () => { w.calls.push("apps"); w.secrets.add(API_BEARER); return [{ id: "app-1", name: "Quasar Benchapp" }]; },
    // The API returns a row for any id this run was told about, except those a test lists in w.gone (its 404: unknown, or not this user's).
    session: async (id) => {
      w.calls.push(`session ${id}`);
      // A scenario's fault takes effect when its hook has run (the hook scripts log "apply" in their directory): the stack then reports a fallback and the <video> starts.
      if (w.hooksDir && fs.existsSync(path.join(w.hooksDir, "log")) && fs.readFileSync(path.join(w.hooksDir, "log"), "utf8").includes("apply")) {
        if (w.faultSeenAt === null) { w.faultSeenAt = w.t; w.onFault?.(w); }
        if (!noFallback && w.t >= w.faultSeenAt + fallbackDelayMs && rows[SID] && !rows[SID].transport_fallback_reason) {
          rows[SID].transport_fallback_reason = fallbackReason;
          w.videoStart = w.t;
        }
      }
      return rows[id] ?? (w.gone.has(id) ? undefined : { id, app_id: "app-1", state: "running" });
    },
    stop: async (id) => {
      w.calls.push(`stop ${id}`);
      if (stopFails) throw new Error("the session did not reach a terminal state in 30 s");
      w.stopped.push(id);
      w.live = w.live.filter((s) => s !== id);
    },
  };
  w.rows = rows;
  const qbench = { pressKey: () => true, dump: () => ({ frames: noFrame ? [] : extraFrames ?? frames(wins.length), windows: noFrame ? [] : wins }), stats: () => (noFrame ? { frames: 0, decoded: 0 } : stats) };
  // A chainable fake locator: its element is enabled, and clicking "Play now" starts the session.
  const loc = (desc) => ({
    first: () => loc(desc),
    getByRole: (role, o) => loc(`${desc} > ${role}:${o?.name}`),
    evaluate: async (fn) => fn({ disabled: false, getAttribute: () => null, click: () => {
      w.clicks.push(desc);
      if (!/Play now/.test(desc)) return;
      w.playAt = w.t;
      if (onPlay) onPlay(w);
      if (bare) return;
      if (refuse) return w.emit("response", response(409, "/v1/sessions", { error: { code: "profile_ineligible" } }));
      w.live.push(SID);
      w.emit("response", response(201, "/v1/sessions", { session: { id: SID }, signaling: { token: "single-use" } }));
      if (!noUrl) w.url = url ?? `${BASE}/app/session/${SID}`;
    } }),
  });
  // The DevTools session: an event is delivered to the page's listeners as a trusted one, as the browser would.
  const deliver = (type, e) => (w.listeners[type] ?? []).forEach((fn) => fn({ isTrusted: true, ...e }));
  const cdp = {
    send: async (method, p) => {
      w.cdp.push([method, p]);
      if (method === "Input.dispatchKeyEvent") {
        if (p.type === "keyUp" && p.code === keyUpFails) throw new Error("the DevTools session is gone");
        if (p.type === "keyDown") deliver("keydown", { code: p.code });
        else if (p.type === "keyUp" && p.code !== swallowKeyUp) deliver("keyup", { code: p.code });
      } else if (method === "Input.dispatchMouseEvent") {
        const idx = { left: 0, middle: 1, right: 2 }[p.button];
        if (p.type === "mousePressed") {
          if (idx === 0 && layerUp()) { w.layerClicks += 1; if (lockOnClick) { w.locked = true; w.lockAt = w.t + lockDelayMs; } }
          else if (idx === 0 && hudUp() && p.x === hudButton.x && p.y === hudButton.y) { w.hudClicks += 1; if (lockOnClick) { w.locked = true; w.lockAt = w.t + lockDelayMs; } }
          else if (idx === 0 && !w.locked) { w.stageClicks += 1; w.hudOpen = true; }
          deliver("mousedown", { button: idx });
        } else if (p.type === "mouseReleased") deliver("mouseup", { button: idx });
        else if (p.type === "mouseWheel") deliver("wheel", { deltaX: p.deltaX, deltaY: p.deltaY });
        else if (p.type === "mouseMoved") {
          if (!swallowMoves) deliver("mousemove", { movementX: w.lastX === null ? 0 : p.x - w.lastX, movementY: w.lastY === null ? 0 : p.y - w.lastY });
          w.lastX = p.x;
          w.lastY = p.y;
        }
      }
    },
  };
  const page = () => ({
    on: (ev, fn) => { (w.handlers[ev] ??= []).push(fn); },
    goto: async (u) => { w.gotos.push(u); w.url = u; },
    fill: async () => {},
    click: async () => { w.ls.set("quasar.auth.token", PAGE_BEARER); w.ls.set("quasar.auth.user", "{}"); w.emit("response", response(200, "/v1/auth/login", { access_token: PAGE_BEARER })); w.afterLogin?.(w); },
    waitForURL: async () => {},
    url: () => w.url,
    close: async () => {},
    context: () => ({ newCDPSession: async () => cdp }),
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
      // On WebTransport the first .session-video is a hidden <video> and the picture is a canvas (SessionPage.tsx); after the fallback it is the <video>.
      const hiddenVideo = { tagName: "VIDEO", videoWidth: 0, getBoundingClientRect: () => ({ left: 0, top: 0, width: 0, height: 0 }) };
      const canvas = { tagName: "CANVAS", getBoundingClientRect: () => ({ left: 100, top: 50, width: 800, height: 600 }) };
      const video = { tagName: "VIDEO", videoWidth: 1920, getVideoPlaybackQuality: () => ({ totalVideoFrames: w.t < w.videoStart ? 0 : Math.round(((w.t - w.videoStart) / 1000) * videoRate) }), getBoundingClientRect: () => ({ left: 100, top: 50, width: 800, height: 600 }) };
      const doc = { pointerLockElement: w.locked && w.t >= w.lockAt ? { classList: { contains: (c) => c === "session-video" } } : null, visibilityState: hidden ? "hidden" : "visible", hasFocus: () => true, querySelectorAll: () => (noFrame ? [] : w.t < w.videoStart ? [hiddenVideo, canvas] : [video]), querySelector: (sel) => (sel === ".session-video" ? (noFrame ? [] : w.t < w.videoStart ? [hiddenVideo, canvas] : [video])[0] ?? null : /Capture input/.test(sel) ? (hudUp() ? { getBoundingClientRect: () => ({ left: hudButton.x - 20, top: hudButton.y - 20, width: 40, height: 40 }) } : null) : /session-play/.test(sel) ? (layerUp() ? { getBoundingClientRect: () => ({ left: 0, top: 0, width: 1280, height: 720 }) } : null) : /sl-root/.test(sel) ? (loader ? matches(sel, loader) : revealed ? {} : null) : /video/.test(sel) && !noBench && !revealed && !noFrame ? video : null) };
      w.win ??= { addEventListener: (type, fn) => { (w.listeners[type] ??= []).push(fn); } };
      w.win.__qBench = noBench ? undefined : qbench;
      Object.assign(globalThis, { window: w.win, localStorage: store, sessionStorage: { getItem: () => null }, document: doc });
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
  assert.deepEqual(by().first_frame_s, { id: "first_frame_s", pass: false, observed: null, expected: "<= 30 s: the first frame the client decoded (it can still be under the loader)" });
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

test("A30: the loader's reveal is not a frame: with no bench frame and no video frame first_frame_s fails", async () => {
  const shown = await run(world({ noBench: true, revealed: true }), PLAN({ bench: false }));
  assert.equal(shown.by().first_frame_s.pass, false);
  assert.equal(shown.scenario().status, "fail");
  // The seat's input: a matching, non-failed loader, no bench, no video frames.
  const seat = world({ noBench: true, loader: "sl-root is-streaming" });
  const r = await run(seat, PLAN({ bench: false }));
  assert.equal(r.by().first_frame_s.pass, false);
  assert.notEqual(r.scenario().status, "pass");
});

test("A30: bench mode off on WebTransport cannot see a frame, so it is an error before anything is launched", async () => {
  const w = world({ noBench: true, revealed: true, session: { transport: "webtransport" } });
  const { scenario, result } = await run(w, PLAN({ bench: false, transport: "webtransport" }));
  assert.deepEqual(result.errors, []);
  assert.equal(scenario().status, "error");
  assert.match(scenario().reason, /bench mode is needed to see a frame on this transport/);
  assert.deepEqual(w.clicks, [], "nothing was launched");
  assert.equal(check({ transport: "webtransport", codec: "h264", profile: "1080p60", hold_s: 30, app: "x", bench: false }), "bench mode is needed to see a frame on this transport");
  assert.equal(check({ transport: "webtransport", codec: "h264", profile: "1080p60", hold_s: 30, app: "x" }), null);
});

test("B6: the loader's reveal is noted once as a step when the page shows it, and a failed loader's is not", async () => {
  const shown = await run(world({ loader: "sl-root is-streaming" }));
  assert.equal(shown.timeline().filter((r) => r.ev === "revealed").length, 1);
  assert.equal(shown.by().first_frame_s.pass, true, "the frame came from the bench ring, not the reveal");
  const failed = await run(world({ loader: "sl-root is-streaming is-failed" }));
  assert.equal(failed.timeline().filter((r) => r.ev === "revealed").length, 0);
  assert.equal((await run(world())).timeline().filter((r) => r.ev === "revealed").length, 0);
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
  const share = (v) => { const i = instrumentShare(frames(50), v, T0 + 20_000, T0 + 49_000); return i.seen / i.decoded; };
  assert.ok(share(video(60)) > 0.99);
  assert.ok(share(video(150)) < 0.5);
  assert.equal(instrumentShare(frames(50), [], T0, T0 + 1e6), null);
  const missed = J({ videoSamples: video(150) }).present_cadence;
  assert.equal(missed.pass, null);
  assert.match(missed.expected, /saw \d+ of \d+ frames the video decoded \(\d+%\).*#1304/);
  const ok = J({ videoSamples: video(60) }).present_cadence;
  assert.equal(ok.pass, true);
  assert.match(ok.expected, /the bench callback saw \d+ of \d+ frames the video decoded/);
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
  const w = world({ onPlay: (x) => x.emit("request", request("PUT", `/v1/apps/app-1/settings`)) });
  const { scenario } = await run(w);
  assert.equal(scenario().status, "error");
  assert.match(scenario().reason, /left behind: the page changed something on the stack \(PUT \/v1\/apps\/app-1\/settings\) that this run cannot put back/);
  assert.deepEqual(w.stopped, [SID]);
  // The library's keep-alive heartbeat is not a preference (the real page sends it, once a minute).
  const hb = await run(world({ onPlay: (x) => { for (let i = 0; i < 3; i += 1) x.emit("request", request("PUT", "/v1/me/steam-warm")); } }));
  assert.equal(hb.scenario().status, "pass");
  // The same write twice is named once.
  const twice = await run(world({ onPlay: (x) => { for (let i = 0; i < 2; i += 1) x.emit("request", request("PUT", "/v1/apps/app-1/settings")); } }));
  assert.equal([...twice.scenario().reason.matchAll(/PUT \/v1\/apps\/app-1\/settings/g)].length, 1);
  // The sessions endpoint's own writes (the page stopping its session) and reads are not changes of a preference.
  const ok = await run(world({ onPlay: (x) => x.emit("request", request("DELETE", `/v1/sessions/${SID}`)) }));
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

test("A21: a failed or ended loader is never a first frame (the stopped session behind it is not a pass)", async () => {
  for (const loader of ["sl-root is-locking is-failed is-over", "sl-root is-streaming is-failed is-over", "sl-root is-streaming is-failed"]) {
    const w = world({ noBench: true, loader });
    const { scenario, by } = await run(w, PLAN({ bench: false }));
    assert.equal(by().first_frame_s.pass, false, loader);
    assert.equal(scenario().status, "fail", loader);
  }
  const stopped = await run(world({ noBench: true, loader: "sl-root is-streaming is-failed is-over", session: { state: "stopped" } }), PLAN({ bench: false }));
  assert.notEqual(stopped.scenario().status, "pass");
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

// ── round 4: the third review's list (A26..A32) and the approving seat's small truths ──

const uuid = (n) => `00000000-0000-4000-8000-${String(n).padStart(12, "0")}`;
const noBearerLogin = (w) => onPage(w, (p) => { p.click = async () => {}; return p; }); // the page signs in without ever storing a bearer

test("A26: every write the page sends is refused by default; the launch, the sign-in calls and exactly two named writes are not", async () => {
  // A33: the write is seen when the page sends it. With no status here the answer never arrives; the write is refused all the same.
  const wrote = (method, p, status) => run(world({ onPlay: (x) => { x.emit("request", request(method, p)); if (status) x.emit("response", response(status, p, {}, method)); } }));
  // The two exceptions, by exact method and path (the real sign-in posts the device record; the library keeps warm Steam alive).
  for (const [m, p] of [["POST", "/v1/me/devices"], ["PUT", "/v1/me/steam-warm"], ["POST", "/v1/auth/login"], ["POST", "/v1/auth/refresh"], ["POST", `/v1/sessions/${SID}/stats`], ["DELETE", `/v1/sessions/${SID}`], ["GET", "/v1/me/anything"], ["OPTIONS", "/v1/me/x"]]) {
    assert.equal((await wrote(m, p)).scenario().status, "pass", `${m} ${p}`);
  }
  // Everything else is a write the run cannot put back: a POST too, a failed one too, any prefix or case trick.
  for (const [m, p, st] of [["POST", "/v1/me/password"], ["POST", "/v1/me/devices/extra"], ["PUT", "/v1/me/devices"], ["PATCH", "/v1/me/steam-warm"], ["POST", "/v1/me/steam-warm"], ["PUT", "/v1/me/steam-warm/x"], ["POST", "/v1/sessionsx"], ["POST", "/v1/authx/login"], ["POST", "/telemetry"], ["patch", "/v1/me/ui-preferences"], ["POST", "/v1/me/favourites", 500]]) {
    const r = await wrote(m, p, st);
    assert.equal(r.scenario().status, "error", `${m} ${p}`);
    assert.match(r.scenario().reason, new RegExp(`the page changed something on the stack \\(${m} ${p.replace(/\//g, "\\/")}\\) that this run cannot put back`), `${m} ${p}`);
  }
});

test("A27: an owned-looking id the API does not return is not deleted and not written as stopped; the scenario is an error naming it", async () => {
  const w = world({ bare: true, onPlay: (x) => { x.emit("response", response(201, "/v1/sessions", { session: { id: OTHER } })); x.url = `${BASE}/app/session/${OTHER}`; } });
  w.gone.add(OTHER); // the ordinary-user answer for an unknown id and for another user's
  const { scenario, timeline, result } = await run(w);
  assert.deepEqual(result.errors, []);
  assert.equal(scenario().status, "error");
  assert.match(scenario().reason, new RegExp(`session ${OTHER} is not confirmed to be this user's`));
  assert.ok(!w.calls.includes(`stop ${OTHER}`), "no DELETE was sent");
  assert.deepEqual(w.stopped, []);
  const evs = timeline();
  assert.ok(evs.some((r) => r.ev === "stop-unconfirmed" && r.session === OTHER));
  assert.ok(!evs.some((r) => r.ev === "stopped"));
  assert.deepEqual(scenario().sessions, [], "the id was never shown to be this run's");
  // A confirmed row that is then stopped is still `stopped`.
  const ok = await run(world());
  assert.ok(ok.timeline().some((r) => r.ev === "stopped" && r.session === SID));
});

test("A28: while the page's bearer is unknown no page-derived error text is written, in the timeline or in the reason", async () => {
  const w = world();
  onPage(w, (p) => { p.click = async () => {}; p.waitForURL = async () => { throw new Error(`page is at https://stack.example.test/callback#${PAGE_BEARER}`); }; return p; });
  const { dir, scenario, result } = await run(w);
  assert.deepEqual(result.errors, []);
  assert.equal(scenario().status, "error");
  assert.match(scenario().reason, /^sign-in did not complete/);
  for (const f of everyFile(dir).filter((x) => !x.endsWith(".png"))) assert.ok(!fs.readFileSync(f, "utf8").includes(PAGE_BEARER), path.relative(dir, f));
  // The same for a page that would not close.
  const c = world();
  noBearerLogin(c);
  c.host.closePage = async (p) => { c.closed.push(p); if (c.opened.indexOf(p) > 0) throw new Error(`target closed at https://stack.example.test/#${PAGE_BEARER}`); };
  const r = await run(c);
  assert.match(r.scenario().reason, /left behind: a page it opened would not close(;|$)/);
  for (const f of everyFile(r.dir).filter((x) => !x.endsWith(".png"))) assert.ok(!fs.readFileSync(f, "utf8").includes(PAGE_BEARER), path.relative(r.dir, f));
  // A34: the same for the route of a write the page sent: it is the page's text, so only the fact is written.
  const wr = world();
  onPage(wr, (p) => { p.click = async () => { wr.emit("request", request("PUT", `/v1/me/x/${PAGE_BEARER}`)); }; return p; });
  const rw = await run(wr);
  assert.match(rw.scenario().reason, /the page changed something on the stack \(route withheld: the page's sign-in was never known\)/);
  for (const f of everyFile(rw.dir).filter((x) => !x.endsWith(".png"))) assert.ok(!fs.readFileSync(f, "utf8").includes(PAGE_BEARER), path.relative(rw.dir, f));
  // Once the bearer is known the words are written in full (and redacted): the close error is as before.
  const known = await run(world({ closeFails: true }));
  assert.match(known.scenario().reason, /would not close \(close failed\)/);
});

test("A29: a count a criterion reads must be a finite, non-negative number", () => {
  const bad = (w) => windows(50).map((x, i) => (i === 40 ? { ...x, ...w } : x));
  for (const w of [{ n: 0, no_image: Infinity }, { n: 0, no_image: NaN }, { n: 0, no_image: -1 }, { n: 0, no_image: "3" }, { n: 0, no_image: null }, { n: Infinity }, { n: 0, no_image: undefined }]) {
    const c = J({ windows: bad(w) }).frameless_seconds;
    assert.equal(c.pass, null, JSON.stringify(w));
    assert.equal(c.observed, null);
  }
  // A malformed window as the first with frames does not move the warm-up.
  const shifted = windows(50);
  shifted[0] = { ...shifted[0], n: Infinity };
  assert.equal(observeWindows(shifted, 20)[0].t_start_ms, T0 + 21_000);
  // The marker count is a number too.
  for (const decoded of [Infinity, -1, NaN, "5"]) assert.equal(J({ stats: { decoded } }).marker_decoded.pass, null, String(decoded));
  assert.equal(J({ stats: { decoded: 0 } }).marker_decoded.pass, false);
  assert.equal(J({ stats: { decoded: 3 } }).marker_decoded.pass, true);
});

test("A35: a frame time or a video frame count that is not a number leaves present_cadence unjudged", () => {
  const video = (rate, from = 0) => Array.from({ length: 50 }, (_, i) => ({ t: T0 + i * 1000, videoFrames: from + i * rate }));
  // The seat's inputs: string timestamps that a comparison would coerce into the share, and a counter that starts below zero.
  const strings = Array.from({ length: 2500 }, (_, i) => ({ present_ms: String(T0 + 20_000 + (i % 1700)) }));
  for (const [name, o] of [["string present_ms", { frames: [...frames(50), ...strings], videoSamples: video(150) }], ["negative counter", { videoSamples: video(60, -10_000) }], ["string sample time", { videoSamples: video(60).map((v) => ({ ...v, t: String(v.t) })) }], ["non-finite counter", { videoSamples: video(60).map((v, i) => (i === 30 ? { ...v, videoFrames: Infinity } : v)) }], ["a frame with no time", { frames: [...frames(50), {}], videoSamples: video(60) }]]) {
    const c = J(o).present_cadence;
    assert.equal(c.pass, null, name);
    assert.match(c.expected, /not a number/, name);
  }
  assert.deepEqual(instrumentShare(frames(50), video(60, -1), T0 + 20_000, T0 + 49_000), { malformed: true });
  assert.equal(J({ videoSamples: video(60) }).present_cadence.pass, true);
});

test("A36: a frame counter and a profile number must be finite numbers", async () => {
  // A non-number from stats() is not a first frame (no video frame either).
  for (const n of [Infinity, NaN, "5", -1]) {
    const r = await run(world({ stats: { frames: n, decoded: 0 }, videoRate: 0 }));
    assert.equal(r.by().first_frame_s.pass, false, String(n));
    assert.notEqual(r.scenario().status, "pass", String(n));
  }
  // The seat's input: a profile of 400 digits is Infinity as a number, and would equal a non-finite height.
  const huge = `${"9".repeat(400)}p60`;
  assert.match(check({ transport: "webrtc", codec: "h264", profile: huge, hold_s: 90, app: "x" }), /profile must look like 1080p60/);
  for (const stream of [{ height: Infinity, fps: 60 }, { height: "1080", fps: 60 }, { height: 1080, fps: NaN }, { height: -1080, fps: 60 }]) {
    for (const profile of [huge, "1080p60"]) assert.equal(J({ params: { ...PARAMS, profile }, session: { ...ROW, stream } }).profile_as_asked.pass, null, JSON.stringify(stream));
  }
  assert.equal(J().profile_as_asked.pass, true);
});

test("A31: the drain-and-stop loop runs at most 10 rounds; what is left is named and the bundle is still written", async () => {
  const w = world();
  let n = 0;
  const stop = w.api.stop;
  w.api.stop = async (id) => { await stop(id); if (n < 40) { n += 1; w.emit("response", response(201, "/v1/sessions", { session: { id: uuid(n) } })); } };
  const { scenario, result, timeline } = await run(w);
  assert.deepEqual(result.errors, []);
  assert.ok(result.bundle, "the bundle was committed");
  assert.equal(w.stopped.length, 10);
  assert.equal(scenario().status, "error");
  assert.match(scenario().reason, new RegExp(`session ${uuid(10)} is not stopped \\(launches kept arriving after 10 rounds`));
  assert.ok(timeline().some((r) => r.ev === "stop-failed" && r.session === uuid(10)));
});

test("A32: the pre-auth hold keeps at most 2000 lines and 1 MB, counts the rest, and the timeline says so", async () => {
  const lines = (w, count, size) => (p) => { let once = false; p.fill = async () => { if (once) return; once = true; for (let i = 0; i < count; i += 1) w.emit("console", { type: () => "log", text: () => "x".repeat(size) }); }; return p; };
  // Released: the first 2000 are written, the rest counted.
  const w = world();
  onPage(w, lines(w, 5000, 10));
  const a = await run(w);
  assert.equal(read(a.dir, "01-stream-hold/console.log").trim().split("\n").length, 2000);
  assert.deepEqual(a.timeline().find((r) => r.ev === "pre-auth-evidence-dropped")?.lines, 3000);
  // The byte cap: 20 lines of ~100 kB, the ninth is the last that fits under 1 MB.
  const b = world();
  onPage(b, lines(b, 20, 100_000));
  const big = await run(b);
  assert.equal(read(big.dir, "01-stream-hold/console.log").trim().split("\n").length, 9);
  assert.equal(big.timeline().find((r) => r.ev === "pre-auth-evidence-dropped")?.lines, 11);
  // Never released: everything seen, kept or not, is counted as dropped and nothing is written.
  const c = world();
  noBearerLogin(c);
  onPage(c, lines(c, 5000, 10));
  const d = await run(c);
  assert.ok(!read(d.dir, "01-stream-hold/console.log").includes("x"));
  assert.equal(d.timeline().find((r) => r.ev === "pre-auth-evidence-dropped")?.lines, 5000);
});

test("B7: the manifest says when the runner's tracked files differ from its commit", async () => {
  assert.equal(JSON.parse(read((await run(world(), PLAN({}), { commit: "abc" })).dir, "manifest.json")).runner.dirty, undefined);
  assert.deepEqual(JSON.parse(read((await run(world(), PLAN({}), { commit: "abc", dirty: true })).dir, "manifest.json")).runner, { commit: "abc", dirty: true });
  const repo = fs.mkdtempSync(path.join(os.tmpdir(), "state-"));
  const git = (...a) => execFileSync("git", ["-c", "user.name=t", "-c", "user.email=t@example.test", "-c", "commit.gpgsign=false", ...a], { cwd: repo, encoding: "utf8", env: { PATH: process.env.PATH, HOME: repo, GIT_CONFIG_NOSYSTEM: "1" } });
  git("init", "-q");
  fs.writeFileSync(path.join(repo, "f"), "1");
  git("add", "f");
  git("commit", "-qm", "x");
  assert.equal(runnerState(repo).dirty, false);
  fs.writeFileSync(path.join(repo, "untracked"), "1");
  assert.equal(runnerState(repo).dirty, false, "an untracked file does not count");
  fs.writeFileSync(path.join(repo, "f"), "2");
  assert.deepEqual(runnerState(repo), { commit: git("rev-parse", "HEAD").trim(), dirty: true });
  assert.deepEqual(runnerState(os.tmpdir()), { commit: "unknown", dirty: false });
});

test("B8: builds.json says which builds e2e-collect fills and which it does not read yet", async () => {
  const { dir } = await run(world());
  const u = JSON.parse(read(dir, "builds.json")).unknown;
  for (const k of ["control_plane", "node_agent"]) assert.match(u[k], /e2e-collect\) fills this/);
  for (const k of ["web", "gpu_driver", "reference_app"]) assert.equal(u[k], "the client cannot see it, and e2e-collect does not read it yet");
});

test("WEB_DIST: the bundle says which web build was served (digest and count, no path), and a run without it is as before", async () => {
  const src = fs.mkdtempSync(path.join(os.tmpdir(), "e2e-wd-secret-name-"));
  fs.writeFileSync(path.join(src, "index.html"), "<html></html>");
  const webDist = loadWebDist(src);
  const { dir, result } = await run(world(), PLAN({}), { webDist });
  assert.deepEqual(result.errors, []);
  const builds = JSON.parse(read(dir, "builds.json"));
  assert.deepEqual(builds.components.web, { source: "web-dist", sha256: webDist.sha256, files: 1 });
  assert.equal("web" in builds.unknown, false, "a known build has no reason for being unknown");
  assert.deepEqual(JSON.parse(read(dir, "manifest.json")).web_build, { served_by: "runner", source: "web-dist" });
  for (const f of everyFile(dir)) assert.ok(!read(f, "").includes("e2e-wd-secret-name") && !read(f, "").includes(src), `${f} names the directory`);
  const plain = await run(world());
  assert.equal(JSON.parse(read(plain.dir, "builds.json")).components.web, null);
  assert.equal("web_build" in JSON.parse(read(plain.dir, "manifest.json")), false);
  fs.rmSync(src, { recursive: true, force: true });
});

test("WEB_DIST: the runner's own context serves it, and without it no route is installed", async () => {
  const routes = [];
  const browser = { newContext: async () => ({ route: async (m) => { routes.push(m); }, newPage: async () => ({}), close: async () => {} }), close: async () => {} };
  await openHost(browser, { attached: true, insecureTls: false });
  assert.equal(routes.length, 0);
  const src = fs.mkdtempSync(path.join(os.tmpdir(), "e2e-wd-"));
  fs.writeFileSync(path.join(src, "index.html"), "<html></html>");
  await openHost(browser, { attached: true, insecureTls: false, baseUrl: BASE, webDist: loadWebDist(src) });
  assert.equal(routes.length, 1);
  assert.equal(routes[0](new URL(BASE)), true);
  assert.equal(routes[0](new URL("https://elsewhere.test/")), false);
  fs.rmSync(src, { recursive: true, force: true });
});

// ── hooks (hooks.mjs): apply and clear around a scenario ────────────────────

import { declaredHooks, Hooks, hookScript, LEDGER, runHook } from "./hooks.mjs";
import { pathToFileURL } from "node:url";

const BLOCK = "fault:webtransport-block";
const tmp = (p = "e2e-hooks-") => fs.mkdtempSync(path.join(os.tmpdir(), p));
/** A hooks directory: each script logs "<phase> <scenario> <session> <transport>" to `log` beside it, then runs `extra` (a phase-aware snippet). */
function mkHooks(extra = "", names = ["fault-webtransport-block"]) {
  const dir = tmp();
  for (const n of names) {
    fs.writeFileSync(path.join(dir, n), `#!/bin/sh\necho "$1 $QUASAR_E2E_SCENARIO $QUASAR_E2E_SESSION $QUASAR_E2E_TRANSPORT" >> "$(dirname "$0")/log"\n${extra}\nexit 0\n`, { mode: 0o755 });
  }
  return dir;
}
const logOf = (dir) => (fs.existsSync(path.join(dir, "log")) ? fs.readFileSync(path.join(dir, "log"), "utf8").trim().split("\n") : []);
const PASS = [{ id: "x", pass: true, observed: 1, expected: "1" }];

/** A scenario module that launches (so a session id is known), applies the hook, then does `during(ctx, w)`. */
const fake = (w, during = async () => {}, hooks = [BLOCK]) => ({
  hooks,
  async run(page, ctx) {
    ctx.rec.release(); // as a real scenario does once signed in: before that the runner withholds the page's words
    w.emit("response", response(201, "/v1/sessions", { session: { id: SID } }));
    await ctx.launches.settle();
    await ctx.hook(BLOCK, "apply");
    await during(ctx, w);
    return { status: "pass", criteria: PASS, sessions: [], clock_offset_ms: null };
  },
});

async function runHooks(w, scenario, { hooksDir, parent = tmp("e2e-parent-"), ...extra } = {}) {
  const dir = path.join(parent, "bundle");
  const result = await runSuite({ plan: { name: "t", scenarios: [{ scenario: "fake" }] }, dir, baseUrl: BASE, creds: CREDS, host: w.host, api: w.api, thresholds: T, secrets: w.secrets, now: w.now, sleep: w.sleep, hooksDir, load: async () => scenario, onNote: (l) => notes.push(l), ...extra });
  const sc = () => JSON.parse(fs.readFileSync(path.join(dir, "01-fake/scenario.json"), "utf8"));
  const timeline = () => fs.readFileSync(path.join(dir, "01-fake/timeline.jsonl"), "utf8").trim().split("\n").map((l) => JSON.parse(l));
  return { dir, parent, result, sc, timeline };
}
let notes = [];

test("hooks: a hook asked for mid-scenario is applied with the scenario's facts and cleared at the end", async () => {
  notes = [];
  const w = world();
  const hooksDir = mkHooks();
  const r = await runHooks(w, fake(w, async (ctx) => ctx.hook(BLOCK, "clear")), { hooksDir });
  assert.deepEqual(r.result.errors, []);
  assert.equal(r.sc().status, "pass");
  assert.deepEqual(logOf(hooksDir), [`apply 01-fake ${SID} webrtc`, `clear 01-fake ${SID} webrtc`]);
  assert.deepEqual(r.timeline().filter((s) => s.ev === "hook").map((s) => [s.phase, s.exit_code]), [["apply", 0], ["clear", 0]]);
  assert.equal(fs.existsSync(path.join(r.parent, LEDGER)), false, "the ledger is removed once nothing is applied");
});

test("hooks: a scenario that never clears has its hook cleared by the runner; one that throws as well", async () => {
  for (const during of [async () => {}, async () => { throw new Error("the scenario broke"); }]) {
    notes = [];
    const w = world();
    const hooksDir = mkHooks();
    const r = await runHooks(w, fake(w, during), { hooksDir });
    assert.deepEqual(logOf(hooksDir).map((l) => l.split(" ")[0]), ["apply", "clear"]);
    assert.equal(fs.existsSync(path.join(r.parent, LEDGER)), false);
    assert.equal(r.sc().status, during.toString().includes("broke") ? "error" : "pass");
  }
});

test("hooks: an interrupt (SIGINT/SIGTERM resolve `abort`) while a hook is applied still clears it", async () => {
  notes = [];
  const w = world();
  const hooksDir = mkHooks();
  let stop;
  const abort = new Promise((r) => { stop = r; });
  const r = await runHooks(w, fake(w, async (ctx) => { stop(); await ctx.sleep(1000); }), { hooksDir, abort, sleep: () => new Promise(() => {}) });
  assert.deepEqual(logOf(hooksDir).map((l) => l.split(" ")[0]), ["apply", "clear"]);
  assert.equal(r.sc().status, "error");
  assert.match(r.sc().reason, /interrupted/);
});

test("hooks: a clear that fails makes the scenario error, names the hook, and stays in the ledger", async () => {
  notes = [];
  const w = world();
  const hooksDir = mkHooks('[ "$1" = clear ] && exit 3');
  const r = await runHooks(w, fake(w), { hooksDir });
  assert.equal(r.sc().status, "error");
  assert.match(r.sc().reason, /left behind: .*hook fault:webtransport-block clear failed \(exit 3\)/);
  const ledger = JSON.parse(fs.readFileSync(path.join(r.parent, LEDGER), "utf8"));
  assert.deepEqual(ledger.entries.map((e) => e.name), [BLOCK]);
});

test("hooks: after a failed clear the next scenario is skipped, not judged on a faulted path", async () => {
  const w = world();
  const hooksDir = mkHooks('[ "$1" = clear ] && exit 3');
  const dir = path.join(tmp("e2e-parent-"), "bundle");
  const result = await runSuite({ plan: { name: "t", scenarios: [{ scenario: "fake" }, { scenario: "fake" }] }, dir, baseUrl: BASE, creds: CREDS, host: w.host, api: w.api, thresholds: T, secrets: w.secrets, now: w.now, sleep: w.sleep, hooksDir, load: async () => fake(w), onNote: () => {} });
  assert.deepEqual(result.counts, { pass: 0, fail: 0, error: 1, skipped: 1 });
  assert.match(JSON.parse(fs.readFileSync(path.join(dir, "02-fake/scenario.json"), "utf8")).reason, /would not clear/);
});

test("hooks: a hook with no script, a non-executable one or no HOOKS= at all skips the scenario naming it, and launches nothing", async () => {
  const empty = tmp();
  const plain = mkHooks();
  fs.chmodSync(path.join(plain, "fault-webtransport-block"), 0o644);
  for (const [hooksDir, wording] of [[empty, /needs hook fault:webtransport-block, which the hooks directory has no executable script for/], [plain, /needs hook fault:webtransport-block/], [null, /no hooks directory \(HOOKS=\) was given/]]) {
    const w = world();
    const r = await runHooks(w, fake(w), { hooksDir });
    assert.equal(r.sc().status, "skipped");
    assert.match(r.sc().reason, wording);
    assert.equal(w.opened.length, 1, "only the probe page was opened");
    assert.equal(w.calls.length, 0, "the API was not touched");
    assert.deepEqual(r.result.errors, []);
  }
});

test("hooks: a name of the wrong shape, or a script that links out of the directory, is never run", async () => {
  for (const bad of ["../etc:x", "Fault:x", "fault:", "fault:a b", "fault", "fault:x/y"]) assert.equal(hookScript(mkHooks(), bad), null, bad);
  assert.throws(() => declaredHooks({ hooks: ["fault:x", "fault:x"] }), /distinct names/);
  assert.throws(() => declaredHooks({ hooks: "fault:x" }), /distinct names/);
  assert.deepEqual(declaredHooks({}), []);
  const outside = mkHooks();
  const dir = tmp();
  fs.symlinkSync(path.join(outside, "fault-webtransport-block"), path.join(dir, "fault-webtransport-block"));
  assert.equal(hookScript(dir, BLOCK), null);
  assert.equal(hookScript(outside, BLOCK), fs.realpathSync(path.join(outside, "fault-webtransport-block")));
});

test("hooks: a scenario cannot ask for a hook it did not declare, or an unknown phase", async () => {
  const w = world();
  const hooksDir = mkHooks("", ["fault-webtransport-block", "shape-slow"]);
  const r = await runHooks(w, { hooks: [], async run(page, ctx) { ctx.rec.release(); await ctx.hook("shape:slow", "apply"); return { status: "pass", criteria: PASS }; } }, { hooksDir });
  assert.equal(r.sc().status, "error");
  assert.match(r.sc().reason, /did not declare hook/);
  assert.deepEqual(logOf(hooksDir), []);
  const h = new Hooks({ dir: hooksDir, ledgerDir: tmp() });
  await assert.rejects(h.call(BLOCK, "toggle", {}, () => {}), /apply or clear/);
  await assert.rejects(h.call(BLOCK, "clear", {}, () => {}), /not applied/);
});

test("hooks: a hook that hangs is killed at the timeout, which fails the apply; the clear still runs", async () => {
  notes = [];
  const w = world();
  const hooksDir = mkHooks('[ "$1" = apply ] && sleep 30');
  const t0 = Date.now();
  const r = await runHooks(w, fake(w), { hooksDir, hookTimeoutMs: 300 });
  assert.ok(Date.now() - t0 < 10_000, "did not wait for the sleep");
  assert.equal(r.sc().status, "error");
  assert.match(r.sc().reason, /hook fault:webtransport-block apply failed \(timed out\)/);
  assert.deepEqual(logOf(hooksDir).map((l) => l.split(" ")[0]), ["apply", "clear"]);
  assert.equal(fs.existsSync(path.join(r.parent, LEDGER)), false);
  assert.equal(r.timeline().find((s) => s.ev === "hook").timed_out, true);
});

test("hooks: output is redacted and capped before it reaches the timeline, and a hook gets no shell", async () => {
  const w = world();
  const hooksDir = mkHooks(`echo "token ${CREDS.password} 10.20.30.40"; i=0; while [ $i -lt 400 ]; do echo xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx; i=$((i+1)); done`);
  const r = await runHooks(w, fake(w), { hooksDir });
  const text = fs.readFileSync(path.join(r.dir, "01-fake/timeline.jsonl"), "utf8");
  assert.ok(!text.includes(CREDS.password));
  assert.ok(!text.includes("10.20.30.40"));
  assert.match(text, /<credential>/);
  assert.ok(r.timeline().find((s) => s.ev === "hook").stdout.length <= 2000);
  // Arguments are one array entry each: the phase is never parsed by a shell.
  assert.deepEqual(r.result.errors, []);
});

test("hooks: output is redacted whole and only then cut, so a secret across the cut is not written in part", async () => {
  const w = world();
  const hooksDir = mkHooks(`printf '%1990s' | tr ' ' x; echo " 10.20.30.40 more"; { printf '%1993s' | tr ' ' y; echo " ${CREDS.password} end"; } >&2`);
  const r = await runHooks(w, fake(w), { hooksDir });
  const text = fs.readFileSync(path.join(r.dir, "01-fake/timeline.jsonl"), "utf8");
  assert.ok(!text.includes("10.20."), "no part of the address");
  assert.ok(!text.includes(CREDS.password.slice(0, 4)), "no part of the password");
});

test("hooks: a hook does not get the test user's sign-in, and facts that are not plain names are refused before the ledger", async () => {
  const dir = mkHooks();
  const parent = tmp("e2e-parent-");
  const h = new Hooks({ dir, ledgerDir: parent, env: { PATH: process.env.PATH, QUASAR_E2E_EMAIL: "a", QUASAR_E2E_PASSWORD: "b", QUASAR_E2E_CRED_FILE: "c", KEEP: "1" } });
  const env = h.hookEnv("apply", { scenario: "s", transport: "webrtc" });
  assert.deepEqual(Object.keys(env).filter((k) => /EMAIL|PASSWORD|CRED/.test(k)), []);
  assert.equal(env.KEEP, "1");
  await assert.rejects(h.call(BLOCK, "apply", { scenario: "s", transport: "web rtc" }, () => {}), /must be plain names/);
  assert.equal(fs.existsSync(path.join(parent, LEDGER)), false);
  assert.deepEqual(logOf(dir), [], "nothing was applied");
});

test("hooks: the ledger is on disk while the apply is still running", async () => {
  const parent = tmp("e2e-parent-");
  const dir = mkHooks('[ "$1" = apply ] && { [ -f "$LEDGER_FILE" ] || exit 7; }');
  const h = new Hooks({ dir, ledgerDir: parent, env: { PATH: process.env.PATH, LEDGER_FILE: path.join(parent, LEDGER) } });
  await h.call(BLOCK, "apply", { scenario: "s", transport: "webrtc" }, () => {});
  await h.call(BLOCK, "clear", {}, () => {});
  assert.equal(fs.existsSync(path.join(parent, LEDGER)), false);
});

test("hooks: the timeout kills the hook's own children too", async () => {
  const dir = mkHooks('sleep 30 & echo $! > "$(dirname "$0")/child"; sleep 30');
  const r = await runHook(path.join(dir, "fault-webtransport-block"), "apply", { PATH: process.env.PATH }, 400);
  assert.equal(r.timedOut, true);
  const pid = Number(fs.readFileSync(path.join(dir, "child"), "utf8"));
  await new Promise((res) => setTimeout(res, 200));
  assert.throws(() => process.kill(pid, 0), "the grandchild is gone");
});

test("hooks: a run with no HOOKS= says so when a ledger beside the bundle shows a hook left applied", async () => {
  notes = [];
  const w = world();
  const parent = tmp("e2e-parent-");
  fs.writeFileSync(path.join(parent, LEDGER), "{}");
  await runHooks(w, fake(w, async () => {}, []), { parent, hooksDir: null }).catch(() => {});
  assert.ok(notes.some((n) => /earlier run left a hook applied/.test(n)), notes.join("|"));
});

test("hooks: a runner killed between apply and clear leaves a ledger; the next run clears it first and says so", async () => {
  notes = [];
  const w = world();
  const hooksDir = mkHooks();
  const parent = tmp("e2e-parent-");
  const hooksUrl = pathToFileURL(path.join(HERE, "hooks.mjs")).href;
  const child = spawn(process.execPath, ["--input-type=module", "-e", `import { Hooks } from ${JSON.stringify(hooksUrl)};
    const h = new Hooks({ dir: ${JSON.stringify(hooksDir)}, ledgerDir: ${JSON.stringify(parent)} });
    await h.call(${JSON.stringify(BLOCK)}, "apply", { scenario: "01-fake", transport: "webtransport", session: ${JSON.stringify(SID)} }, () => {});
    console.log("applied"); setInterval(() => {}, 1000);`], { stdio: ["ignore", "pipe", "inherit"] });
  await new Promise((resolve, reject) => { child.stdout.on("data", (b) => /applied/.test(String(b)) && resolve()); child.on("exit", () => reject(new Error("the child ended early"))); });
  const gone = new Promise((r) => child.on("exit", r));
  child.kill("SIGKILL");
  await gone;
  assert.deepEqual(logOf(hooksDir).map((l) => l.split(" ")[0]), ["apply"]);
  assert.equal(JSON.parse(fs.readFileSync(path.join(parent, LEDGER), "utf8")).entries.length, 1);

  const r = await runHooks(w, { hooks: [], async run() { return { status: "pass", criteria: PASS }; } }, { hooksDir, parent });
  assert.deepEqual(r.result.recovered, [BLOCK]);
  assert.deepEqual(logOf(hooksDir), [`apply 01-fake ${SID} webtransport`, `clear 01-fake ${SID} webtransport`], "cleared with the facts it was applied with");
  assert.ok(notes.some((l) => /cleared hook\(s\) a runner that died had left applied: fault:webtransport-block/.test(l)), notes.join("\n"));
  assert.equal(fs.existsSync(path.join(parent, LEDGER)), false);
  assert.equal(r.sc().status, "pass");
});

test("hooks: a runner killed while its apply is still running: the next run waits for that hook to end before it clears, or does not start", async () => {
  const kill9DuringApply = async (sleepS) => {
    const hooksDir = mkHooks(`[ "$1" = apply ] && { echo began >> "$(dirname "$0")/log"; sleep ${sleepS}; echo finished >> "$(dirname "$0")/log"; }`);
    const parent = tmp("e2e-parent-");
    const hooksUrl = pathToFileURL(path.join(HERE, "hooks.mjs")).href;
    const child = spawn(process.execPath, ["--input-type=module", "-e", `import { Hooks } from ${JSON.stringify(hooksUrl)};
      const h = new Hooks({ dir: ${JSON.stringify(hooksDir)}, ledgerDir: ${JSON.stringify(parent)} });
      await h.call(${JSON.stringify(BLOCK)}, "apply", { scenario: "01-fake", transport: "webtransport" }, () => {});`], { stdio: "ignore" });
    while (!logOf(hooksDir).includes("began")) await new Promise((r) => setTimeout(r, 20));
    const gone = new Promise((r) => child.on("exit", r));
    child.kill("SIGKILL");
    await gone;
    return { hooksDir, parent };
  };
  // The orphaned apply ends by itself: recovery waits for it, and only then clears.
  const a = await kill9DuringApply(1);
  assert.equal(JSON.parse(fs.readFileSync(path.join(a.parent, LEDGER), "utf8")).entries[0].state, "applying");
  assert.deepEqual(await new Hooks({ dir: a.hooksDir, ledgerDir: a.parent }).recover(() => {}), [BLOCK]);
  assert.deepEqual(logOf(a.hooksDir).map((l) => l.split(" ")[0]), ["apply", "began", "finished", "clear"], "the clear is the last thing that happened");
  assert.equal(fs.existsSync(path.join(a.parent, LEDGER)), false);
  // One that does not end in time: the run does not start, nothing is cleared over it, the ledger stays.
  const b = await kill9DuringApply(30);
  await assert.rejects(new Hooks({ dir: b.hooksDir, ledgerDir: b.parent, timeoutMs: 300 }).recover(() => {}), /still running \(process group \d+\)/);
  const [entry] = JSON.parse(fs.readFileSync(path.join(b.parent, LEDGER), "utf8")).entries;
  assert.deepEqual(logOf(b.hooksDir).map((l) => l.split(" ")[0]), ["apply", "began"]);
  process.kill(-entry.hook_pid, "SIGKILL"); // the test's own orphan
  // An entry that says a hook was running but not which process: not guessed.
  fs.writeFileSync(path.join(b.parent, LEDGER), JSON.stringify({ format: 1, entries: [{ ...entry, hook_pid: undefined }] }));
  await assert.rejects(new Hooks({ dir: b.hooksDir, ledgerDir: b.parent }).recover(() => {}), /its process is not known/);
});

test("hooks: a credential only the hook knows (token=, MY_TOKEN=) does not reach the timeline", async () => {
  const w = world();
  const hooksDir = mkHooks("echo token=hook-only-secret-123; echo MY_TOKEN=abcdef123456 >&2");
  const r = await runHooks(w, fake(w), { hooksDir });
  const text = fs.readFileSync(path.join(r.dir, "01-fake/timeline.jsonl"), "utf8");
  assert.ok(!text.includes("hook-only-secret-123") && !text.includes("abcdef123456"), text.slice(0, 400));
  assert.deepEqual(r.result.errors, []);
});

test("hooks: a stale hook that will not clear stops the run before anything starts; a live holder or a damaged ledger does too", async () => {
  const noop = { hooks: [], async run() { return { status: "pass", criteria: PASS }; } };
  const ledger = (parent, hooksDir, pid) => fs.writeFileSync(path.join(parent, LEDGER), JSON.stringify({ format: 1, entries: [{ id: "abc", pid, hooks_dir: fs.realpathSync(hooksDir), name: BLOCK, state: "applied", facts: { scenario: "01-fake", transport: "webrtc" } }] }));
  const dead = 2 ** 22 + 12345; // beyond pid_max
  // clear fails
  let w = world();
  let hooksDir = mkHooks('[ "$1" = clear ] && exit 1');
  let parent = tmp("e2e-parent-");
  ledger(parent, hooksDir, dead);
  await assert.rejects(runHooks(w, noop, { hooksDir, parent }), /refusing to start: .*fault:webtransport-block \(exit 1\)/);
  assert.equal(w.opened.length, 0, "no browser page was opened");
  assert.ok(fs.existsSync(path.join(parent, LEDGER)), "the entry stays for the next try");
  assert.deepEqual(fs.readdirSync(parent).filter((n) => n.includes("staging")), [], "no staging directory is left");
  // a live holder
  w = world();
  hooksDir = mkHooks();
  parent = tmp("e2e-parent-");
  ledger(parent, hooksDir, process.pid);
  await assert.rejects(runHooks(w, noop, { hooksDir, parent }), /still holds hook fault:webtransport-block/);
  assert.deepEqual(logOf(hooksDir), []);
  // not a ledger
  parent = tmp("e2e-parent-");
  fs.writeFileSync(path.join(parent, LEDGER), "{not json");
  await assert.rejects(runHooks(world(), noop, { hooksDir, parent }), /is not a ledger this runner wrote/);
  // another hooks directory's entries are left alone
  parent = tmp("e2e-parent-");
  ledger(parent, mkHooks(), dead);
  const r = await runHooks(world(), noop, { hooksDir, parent });
  assert.deepEqual(r.result.recovered, []);
  assert.equal(JSON.parse(fs.readFileSync(path.join(parent, LEDGER), "utf8")).entries.length, 1);
});

// ── transport-fallback and input-kbm (the real scenario modules, against the same fake page) ─────────────────

import { fallbackEvents, judge as judgeFallback, check as checkFallback } from "./scenarios/transport-fallback.mjs";
import { judge as judgeInput, SEQUENCE, check as checkInput } from "./scenarios/input-kbm.mjs";

/** Runs one real scenario from the plan; the bundle is checked by runSuite itself (result.errors). */
async function runReal(w, entry, { hooksDir = null, parent = tmp("e2e-parent-"), plan } = {}) {
  const dir = path.join(parent, "bundle");
  const result = await runSuite({ plan: plan ?? { name: "t", scenarios: [entry] }, dir, baseUrl: BASE, creds: CREDS, host: w.host, api: w.api, thresholds: T, secrets: w.secrets, now: w.now, sleep: w.sleep, hooksDir, onNote: () => {} });
  const id = (i = 1) => `0${i}-${plan ? plan.scenarios[i - 1].scenario : entry.scenario}`;
  const sc = (i = 1) => JSON.parse(fs.readFileSync(path.join(dir, id(i), "scenario.json"), "utf8"));
  const by = (i = 1) => Object.fromEntries(sc(i).criteria.map((c) => [c.id, c]));
  const timeline = (i = 1) => fs.readFileSync(path.join(dir, id(i), "timeline.jsonl"), "utf8").trim().split("\n").map((l) => JSON.parse(l));
  return { dir, result, sc, by, timeline, id };
}
const FALLBACK = (when, extra = {}) => ({ scenario: "transport-fallback", params: { transport: "webtransport", hold_s: 30, when, ...extra } });
const fallbackWorld = (opts = {}) => {
  const hooksDir = mkHooks();
  const w = world({ hooksDir, ...opts });
  w.videoStart = Infinity; // no <video> frame until the fallback: WebTransport draws on a canvas
  return { w, hooksDir };
};

test("transport-fallback mid: key held, fault applied, fallback seen, a video frame, the key released at the end", async () => {
  const { w, hooksDir } = fallbackWorld();
  const r = await runReal(w, FALLBACK("mid"), { hooksDir });
  assert.deepEqual(r.result.errors, []);
  const c = r.by();
  assert.equal(r.sc().status, "pass", JSON.stringify(r.sc().criteria));
  assert.deepEqual(r.sc().criteria.map((x) => x.id), ["first_frame_s", "webtransport_closed_first", "continues_on_webrtc", "fallback_event", "frame_after_fallback", "held_key_released"]);
  assert.equal(c.fallback_event.pass, true);
  assert.match(c.fallback_event.observed, /^stream_lost \(the session row\)$/);
  assert.equal(c.frame_after_fallback.pass, true);
  assert.equal(c.continues_on_webrtc.pass, true);
  assert.equal(c.webtransport_closed_first.pass, null);
  assert.match(c.webtransport_closed_first.expected, /no source today/);
  assert.equal(c.held_key_released.pass, null);
  assert.match(c.held_key_released.expected, /host half/);
  for (const k of ["webtransport_closed_first", "held_key_released"]) assert.equal(c[k].observed, null);
  // Order: the key goes down before the fault, comes up after it; the hook is applied once and cleared by the runner.
  const ev = r.timeline().map((s) => s.ev);
  assert.ok(ev.indexOf("key-held") < ev.indexOf("fault-applied") && ev.indexOf("fault-applied") < ev.indexOf("fallback-seen") && ev.indexOf("fallback-seen") < ev.indexOf("key-released"), ev.join(" "));
  const keys = w.cdp.filter(([m, p]) => m === "Input.dispatchKeyEvent").map(([, p]) => `${p.type}:${p.code}`);
  assert.deepEqual(keys, ["keyDown:KeyW", "keyUp:KeyW"]);
  assert.deepEqual(logOf(hooksDir).map((l) => l.split(" ")[0]), ["apply", "clear"]);
  assert.ok(logOf(hooksDir)[0].endsWith("webtransport"), "the hook was told the transport");
});

test("transport-fallback connect: the fault is on before the launch, no key is held, and it says why", async () => {
  const { w, hooksDir } = fallbackWorld({ fallbackReason: "connect_failed" });
  const r = await runReal(w, FALLBACK("connect"), { hooksDir });
  assert.equal(r.sc().status, "pass", JSON.stringify(r.sc().criteria));
  assert.equal(r.by().fallback_event.observed.split(" ")[0], "connect_failed");
  assert.match(r.by().held_key_released.expected, /before the first picture/);
  assert.deepEqual(w.cdp.filter(([m]) => m === "Input.dispatchKeyEvent"), []);
  const ev = r.timeline().map((s) => s.ev);
  assert.ok(ev.indexOf("fault-applied") < ev.indexOf("play-clicked"), "applied before the launch");
  // No session id is known to the hook before the launch.
  assert.match(logOf(hooksDir)[0], /^apply 01-transport-fallback  webtransport$/);
});

test("transport-fallback: a fault that changes nothing fails fallback_event, frame_after_fallback and continues_on_webrtc", async () => {
  const { w, hooksDir } = fallbackWorld({ noFallback: true });
  const r = await runReal(w, FALLBACK("mid", { hold_s: 10 }), { hooksDir });
  assert.equal(r.sc().status, "fail");
  for (const k of ["fallback_event", "frame_after_fallback", "continues_on_webrtc"]) assert.equal(r.by()[k].pass, false, k);
  assert.equal(r.by().first_frame_s.pass, true);
});

test("transport-fallback: the event the page posts counts, and only a well-formed one", async () => {
  const body = (events) => JSON.stringify({ client: "browser", events });
  assert.deepEqual(fallbackEvents(body([{ ts_unix_ms: 5, type: "transport.fallback", payload: { from: "webtransport", to: "webrtc", reason: "stream_lost" } }])), [{ ts_unix_ms: 5, reason: "stream_lost" }]);
  assert.deepEqual(fallbackEvents(body([{ type: "transport.fallback", payload: { reason: "Bad Reason!" } }, { type: "playout.changed", payload: { reason: "x" } }, null, { type: "transport.fallback", payload: {} }])), []);
  assert.deepEqual(fallbackEvents("{not json"), []);
  assert.deepEqual(fallbackEvents(body([{ ts_unix_ms: "soon", type: "transport.fallback", payload: { reason: "internal" } }])), [{ ts_unix_ms: null, reason: "internal" }]);
  // Through the page: a POST to this session's trace endpoint AFTER the fault is read; the same body to another path, or to another session's endpoint, is not.
  for (const [path_, seen] of [[`/v1/sessions/${SID}/trace/events`, true], [`/v1/sessions/${OTHER}/trace/events`, false], ["/v1/other", false]]) {
    const { w, hooksDir } = fallbackWorld({ noFallback: true });
    w.onFault = () => w.emit("request", { url: () => `${BASE}${path_}`, method: () => "POST", postData: () => body([{ ts_unix_ms: 5, type: "transport.fallback", payload: { reason: "stream_lost" } }]) });
    const r = await runReal(w, FALLBACK("mid", { hold_s: 10 }), { hooksDir });
    assert.equal(r.by().fallback_event.pass, seen, path_);
    if (seen) assert.match(r.by().fallback_event.observed, /^stream_lost \(the page's own post only/);
    assert.deepEqual(JSON.parse(read(r.dir, `${r.id()}/fallback.json`)).posted_events, seen ? [{ ts_unix_ms: 5, reason: "stream_lost" }] : []);
  }
});

test("transport-fallback: judge reads the tail, a session that ended and an unreadable row", () => {
  const base = { params: { hold_s: 30 }, firstFrameS: 1 };
  const good = { row: { state: "running" }, fallbackReason: "stream_lost", posted: [], videoAfterS: 4, tail: { before: 10, after: 70 }, keyHeld: true, faultApplied: true, watched: true };
  const get = (seen) => Object.fromEntries(judgeFallback({ ...base, seen: { ...good, ...seen } }).map((c) => [c.id, c]));
  assert.equal(get({}).continues_on_webrtc.pass, true);
  assert.equal(get({ tail: { before: 70, after: 70 } }).continues_on_webrtc.pass, false, "frames stopped");
  assert.equal(get({ tail: { before: 10, after: NaN } }).continues_on_webrtc.pass, false, "a count that is not a number is never an advance");
  assert.equal(get({ row: { state: "stopped" } }).continues_on_webrtc.pass, false);
  assert.equal(get({ row: undefined }).continues_on_webrtc.pass, null);
  assert.equal(get({ fallbackReason: null, posted: [{ reason: "internal" }] }).fallback_event.pass, true, "the posted event is enough");
  assert.equal(get({ fallbackReason: null }).fallback_event.pass, false);
  assert.equal(get({ videoAfterS: NaN }).frame_after_fallback.pass, false);
  assert.equal(get({ keyHeld: false }).held_key_released.pass, null);
  // B6: no fallback reported is never "continues on WebRTC", however the tail looks.
  const none = get({ fallbackReason: null, posted: [] }).continues_on_webrtc;
  assert.equal(none.pass, false);
  assert.equal(none.observed, "no fallback was reported");
});

test("transport-fallback: a plan that cannot run is refused before a page opens; no hook script skips it", async () => {
  assert.match(checkFallback({ transport: "webrtc", codec: "h264", profile: "1080p60", hold_s: 30, app: "x" }), /needs transport webtransport/);
  assert.match(checkFallback({ transport: "webtransport", codec: "h264", profile: "1080p60", hold_s: 30, app: "x", when: "late" }), /when must be mid or connect/);
  assert.match(checkFallback({ transport: "webtransport", codec: "h264", profile: "1080p60", hold_s: 30, app: "x", when: "mid", extra: 1 }), /does not define/);
  assert.equal(checkFallback({ transport: "webtransport", codec: "h264", profile: "1080p60", hold_s: 30, app: "x" }), null);
  const w = world();
  const r = await runReal(w, FALLBACK("mid"), { hooksDir: null });
  assert.equal(r.sc().status, "skipped");
  assert.match(r.sc().reason, /needs hook fault:webtransport-block/);
  assert.equal(w.opened.length, 1);
  const w2 = world({ webtransport: false });
  const r2 = await runReal(w2, FALLBACK("mid"), { hooksDir: mkHooks() });
  assert.equal(r2.sc().reason, "this browser has no WebTransport");
});

// input-kbm

const INPUT = (extra = {}) => ({ scenario: "input-kbm", params: { hold_s: 5, ...extra } });
const echoWindows = (i2p) => windows(10).map((x, i) => ({ ...x, i2p_ms: i === 5 ? i2p : [], i2p_missed: 0 }));

test("input-kbm: the fixed sequence goes out as trusted events in order, and the judgeable criteria pass", async () => {
  const w = world({ wins: echoWindows([40, 50, 60]) });
  const r = await runReal(w, INPUT());
  assert.deepEqual(r.result.errors, []);
  assert.equal(r.sc().status, "pass", JSON.stringify(r.sc().criteria));
  const c = r.by();
  assert.deepEqual(r.sc().criteria.map((x) => x.id), ["first_frame_s", "pointer_lock_taken", "keys_match", "buttons_match", "motion_sum_matches", "nothing_held_at_end", "input_to_photon_ms"]);
  assert.equal(c.pointer_lock_taken.pass, true);
  assert.equal(c.nothing_held_at_end.pass, true);
  for (const k of ["keys_match", "buttons_match", "motion_sum_matches"]) {
    assert.equal(c[k].pass, null);
    assert.match(c[k].expected, /does not log individual keys/);
  }
  assert.equal(c.input_to_photon_ms.pass, null);
  assert.match(c.input_to_photon_ms.expected, /Measured 3 samples, median 50 ms, max 60 ms/);
  // What went out through the DevTools protocol, in order (the click that takes the lock comes first).
  const sent = w.cdp.map(([m, p]) => `${m.replace("Input.dispatch", "")}:${p.type}${p.code ? `:${p.code}` : ""}${p.button ? `:${p.button}` : ""}`);
  const expected = SEQUENCE.flatMap((s) => (s.kind === "key" ? [`KeyEvent:keyDown:${s.code}`, `KeyEvent:keyUp:${s.code}`] : s.kind === "button" ? [`MouseEvent:mousePressed:${s.button}`, `MouseEvent:mouseReleased:${s.button}`] : s.kind === "wheel" ? ["MouseEvent:mouseWheel"] : ["MouseEvent:mouseMoved"]));
  assert.deepEqual(sent.slice(-expected.length), expected);
  assert.deepEqual(sent.slice(0, 3), ["MouseEvent:mouseMoved", "MouseEvent:mousePressed:left", "MouseEvent:mouseReleased:left"]);
  const rec = JSON.parse(read(r.dir, `${r.id()}/input-sent.json`));
  assert.equal(rec.sequence.length, SEQUENCE.length);
  assert.deepEqual(rec.sequence.map((s) => s.seq), SEQUENCE.map((_, i) => i));
  assert.equal(rec.received_by_page.untrusted, 0);
  assert.deepEqual(rec.received_by_page.motion_sum, { x: 50, y: 25 });
  assert.deepEqual(rec.received_by_page.wheel_sum, { x: 0, y: 0 });
  assert.deepEqual(rec.held_at_end, { keys: [], buttons: [] });
});

test("input-kbm: a key the page is left holding makes nothing_held_at_end false, and the scenario fails (stuck-key reversion proof)", async () => {
  const w = world({ wins: echoWindows([40]), swallowKeyUp: "ShiftLeft" });
  const r = await runReal(w, INPUT());
  assert.equal(r.by().nothing_held_at_end.pass, false);
  assert.match(r.by().nothing_held_at_end.observed, /still down: keys \["ShiftLeft"\]/);
  assert.equal(r.sc().status, "fail");
  assert.deepEqual(JSON.parse(read(r.dir, `${r.id()}/input-sent.json`)).held_at_end, { keys: ["ShiftLeft"], buttons: [] });
});

test("input-kbm: no pointer lock fails pointer_lock_taken; an unreadable record and an absent echo are not judged", async () => {
  const w = world({ wins: echoWindows([]), lockOnClick: false });
  const r = await runReal(w, INPUT());
  assert.equal(r.by().pointer_lock_taken.pass, false);
  assert.equal(r.sc().status, "fail");
  assert.match(r.by().input_to_photon_ms.expected, /marker echoed no key/);
  const get = (seen) => Object.fromEntries(judgeInput({ firstFrameS: 1, seen: { locked: true, lockClicks: 1, sequenceRan: true, held: { keys: [], buttons: [] }, i2p: [], i2pMissed: 0, ...seen } }).map((c) => [c.id, c]));
  assert.equal(get({ held: null }).nothing_held_at_end.pass, null);
  assert.equal(get({ held: { keys: [], buttons: [0] } }).nothing_held_at_end.pass, false);
  // A move log that could not be read is said so; it is not "0 events".
  assert.match(get({ motion: { sent: { x: 50, y: 25 }, received: null, events: null } }).motion_sum_matches.expected, /move log could not be read/);
  assert.match(get({ motion: { sent: { x: 50, y: 25 }, received: { x: 50, y: 25 }, events: 3 } }).motion_sum_matches.expected, /received 3 mousemove events summing to \(50, 25\)/);
  assert.match(get({ locked: false, lockClicks: 0, lockHud: "no-surface" }).pointer_lock_taken.observed, /no visible stream surface to click/);
  assert.equal(checkInput({ transport: "webrtc", codec: "h264", profile: "1080p60", hold_s: 5, app: "x" }), null);
});

test("input-kbm: a sample that is not a number is not counted", async () => {
  const w = world({ wins: echoWindows([40, "55", null, Infinity]) });
  const r = await runReal(w, INPUT());
  assert.match(r.by().input_to_photon_ms.expected, /Measured 1 samples, median 40 ms/);
});

// ── review round 1 on #1338 ─────────────────────────────────────────────────

import { streamCentre } from "./scenarios/stream-common.mjs";
const keyEvents = (w) => w.cdp.filter(([m]) => m === "Input.dispatchKeyEvent").map(([, p]) => `${p.type}:${p.code}`);
const posts = (reason = "stream_lost") => JSON.stringify({ client: "browser", events: [{ ts_unix_ms: 5, type: "transport.fallback", payload: { reason } }] });
const post = (w, body) => w.emit("request", { url: () => `${BASE}/v1/sessions/${SID}/trace/events`, method: () => "POST", postData: () => body });

test("B1: a mid run whose session fell back at connect is an error, the hook is not applied, and nothing before the fault counts", async () => {
  // Each way the run can already know: the page posted the event after login, the row has a reason, the <video> plays from the start.
  const cases = {
    "the page's post": (o) => { const f = fallbackWorld(o); f.w.afterLogin = () => post(f.w, posts("connect_failed")); return f; },
    "the session row": (o) => fallbackWorld({ session: { transport_fallback_reason: "connect_timeout" }, ...o }),
    "a <video> playing from the start": (o) => { const f = fallbackWorld(o); f.w.videoStart = T0 - 5000; return f; },
  };
  for (const [what, make] of Object.entries(cases)) {
    const { w, hooksDir } = make({ noFallback: true });
    const r = await runReal(w, FALLBACK("mid", { hold_s: 10 }), { hooksDir });
    assert.equal(r.sc().status, "error", what);
    assert.match(r.sc().reason, /fell back at connect \((connect_failed|connect_timeout|no reason reported)\) before the fault; the mid-stream fallback was not exercised/, what);
    assert.deepEqual(logOf(hooksDir), [], `${what}: the hook was not applied`);
    // Item 4 (#1343): the check comes before the lock and the key, so a run that cannot exercise the fault touches nothing.
    assert.deepEqual(keyEvents(w), [], `${what}: no key was held`);
    assert.equal(w.layerClicks + w.stageClicks, 0, `${what}: the lock was not asked for`);
  }
  // An event posted AFTER the fault is the evidence; the same one posted before it is not (covered above) and is not double counted.
  const { w, hooksDir } = fallbackWorld({ noFallback: true });
  w.onFault = () => post(w, posts());
  const r = await runReal(w, FALLBACK("mid", { hold_s: 10 }), { hooksDir });
  assert.equal(r.by().fallback_event.pass, true);
});

test("M1: with no first frame the fault is never applied and the rows after it are not judged, not failed", async () => {
  const { w, hooksDir } = fallbackWorld({ noFrame: true });
  const r = await runReal(w, FALLBACK("mid", { hold_s: 10 }), { hooksDir });
  assert.deepEqual(logOf(hooksDir), [], "no hook ran");
  assert.deepEqual(keyEvents(w), []);
  for (const k of ["continues_on_webrtc", "fallback_event", "frame_after_fallback", "held_key_released"]) {
    assert.equal(r.by()[k].pass, null, k);
    assert.match(r.by()[k].expected, /^not judged: no first frame, so the fault was never applied$/, k);
  }
  assert.equal(r.by().first_frame_s.pass, false);
  assert.equal(r.sc().status, "fail");
  // In `connect` the fault is on before the launch, but nothing was watched after it.
  const c = fallbackWorld({ noFrame: true });
  const rc = await runReal(c.w, FALLBACK("connect", { hold_s: 10 }), { hooksDir: c.hooksDir });
  assert.deepEqual(logOf(c.hooksDir).map((l) => l.split(" ")[0]), ["apply", "clear"]);
  for (const k of ["continues_on_webrtc", "fallback_event", "frame_after_fallback", "held_key_released"]) assert.equal(rc.by()[k].pass, null, k);
  assert.match(rc.by().fallback_event.expected, /the stream was not watched after the fault/);
});

test("M1: input-kbm with no first frame sends nothing and judges neither the lock nor the held keys", async () => {
  const w = world({ noFrame: true });
  const r = await runReal(w, INPUT());
  assert.deepEqual(w.cdp, [], "nothing went over the DevTools protocol");
  for (const k of ["pointer_lock_taken", "nothing_held_at_end"]) {
    assert.equal(r.by()[k].pass, null, k);
    assert.match(r.by()[k].expected, /^not judged: no first frame, so the input sequence was not sent$/, k);
  }
  assert.equal(r.sc().status, "fail");
});

test("B2: the stream surface is the visible one: on WebTransport the hidden <video> is skipped for the canvas", async () => {
  const el = (tag, vw, rect) => ({ tagName: tag, videoWidth: vw, getBoundingClientRect: () => rect });
  const hidden = el("VIDEO", 0, { left: 0, top: 0, width: 0, height: 0 });
  const canvas = el("CANVAS", undefined, { left: 100, top: 50, width: 800, height: 600 });
  const video = el("VIDEO", 1920, { left: 0, top: 0, width: 1000, height: 500 });
  const at = (list) => streamCentre({ evaluate: async (fn) => { const saved = globalThis.document; globalThis.document = { querySelectorAll: () => list }; try { return await fn(); } finally { globalThis.document = saved; } } });
  assert.deepEqual(await at([hidden, canvas]), { x: 500, y: 350 });
  assert.deepEqual(await at([video]), { x: 500, y: 250 });
  assert.equal(await at([hidden]), null, "no visible surface");
  assert.equal(await at([]), null);
  // Through the scenario: with no layer to click, the sequence's pointer starts on the canvas, not at 0,0.
  const w = world({ noLayer: true });
  w.videoStart = Infinity;
  await runReal(w, INPUT());
  const press = w.cdp.find(([m, p]) => m === "Input.dispatchMouseEvent" && p.type === "mousePressed");
  assert.deepEqual([press[1].x, press[1].y], [500, 350]);
});

test("B3 (#1343): the lock is taken from the \"Click to play\" layer: waited for, clicked ONCE, then waited for, and never by a stage click", async () => {
  // The layer is raised 2 s after Play: a stage click before it would only open the HUD shelf.
  const late = world({ layerAfterMs: 2000 });
  const r = await runReal(late, INPUT());
  assert.equal(r.by().pointer_lock_taken.pass, true);
  assert.equal(late.layerClicks, 1);
  assert.equal(late.stageClicks, 0, "the stage was never clicked to take the lock");
  // The lock arrives 3 s after the click: it is waited for, not clicked for again.
  const slow = world({ lockDelayMs: 3000, layerStays: true });
  assert.equal((await runReal(slow, INPUT())).by().pointer_lock_taken.pass, true);
  assert.equal(slow.layerClicks, 1);
  // A layer that stays up after a click that did not lock is NOT clicked again (the product removes it on the first click; reversion: the old retry makes 3).
  const sticky = world({ lockOnClick: false, layerStays: true });
  const rs = await runReal(sticky, INPUT());
  const beforeSequence = sticky.cdp.slice(0, sticky.cdp.findIndex(([, p]) => p.type === "keyDown"));
  assert.equal(beforeSequence.filter(([, p]) => p.type === "mousePressed").length, 1, "one click on the layer");
  assert.equal(rs.by().pointer_lock_taken.pass, false);
  assert.match(rs.by().pointer_lock_taken.observed, /^no pointer lock after the click on the "Click to play" layer$/);
  // transport-fallback takes the lock the same way before it holds a key.
  const f = fallbackWorld({ layerAfterMs: 2000 });
  await runReal(f.w, FALLBACK("mid", { hold_s: 10 }), { hooksDir: f.hooksDir });
  assert.equal(f.w.layerClicks, 1);
  assert.equal(f.w.stageClicks, 0);
});

test("#1343 item 3: with no layer the HUD's own route is tried, and what it found is said; with no route the text says the launch was not immersive", async () => {
  // The page offers the HUD route: a trusted click on the stage opens the shelf, a trusted click on "Capture input" takes the lock.
  const h = world({ noLayer: true, hud: true });
  const r = await runReal(h, INPUT());
  assert.equal(h.hudClicks, 1);
  assert.equal(h.layerClicks, 0);
  assert.equal(r.by().pointer_lock_taken.pass, true);
  assert.match(r.by().pointer_lock_taken.observed, /taken through the HUD's "Capture input": the launch was not immersive/);
  assert.equal(r.timeline().find((x) => x.ev === "pointer-lock").hud, "clicked");
  // The sequence's pointer starts where the lock click left it, so the first move is a real delta.
  assert.deepEqual(JSON.parse(read(r.dir, `${r.id()}/input-sent.json`)).received_by_page.motion_sum, { x: 50, y: 25 });
  // The button was clicked and the lock did not follow.
  const nolock = world({ noLayer: true, hud: true, lockOnClick: false });
  const rn = await runReal(nolock, INPUT());
  assert.equal(rn.by().pointer_lock_taken.pass, false);
  assert.match(rn.by().pointer_lock_taken.observed, /^no pointer lock: the launch was not immersive, so there is no layer to click; the HUD's "Capture input" was clicked and did not lock$/);
  // No layer and no button (the stage click showed none): said so.
  const bare = world({ noLayer: true });
  const rb = await runReal(bare, INPUT());
  assert.equal(rb.by().pointer_lock_taken.pass, false);
  assert.match(rb.by().pointer_lock_taken.observed, /^no pointer lock: the launch was not immersive, so there is no layer to click; a click on the stream showed no "Capture input" button in the HUD$/);
  assert.equal(bare.hudClicks, 0);
  assert.equal(rb.timeline().find((x) => x.ev === "pointer-lock").hud, "no-button");
  // transport-fallback: the same route lets it hold the key.
  const f = fallbackWorld({ noLayer: true, hud: true });
  await runReal(f.w, FALLBACK("mid", { hold_s: 10 }), { hooksDir: f.hooksDir });
  assert.deepEqual(keyEvents(f.w), ["keyDown:KeyW", "keyUp:KeyW"]);
  // ... and with no lock, no key is held and the scenario says why.
  const none = fallbackWorld({ noLayer: true });
  const rf = await runReal(none.w, FALLBACK("mid", { hold_s: 10 }), { hooksDir: none.hooksDir });
  assert.deepEqual(keyEvents(none.w), [], "no lock, so no key is held");
  assert.match(rf.by().held_key_released.expected, /pointer lock was not taken/);
});

test("B4: input-kbm releases the key or button it holds when the run is interrupted mid-hold; a release that fails is named", async () => {
  for (const [held, up] of [["KeyS", "KeyS"], ["right", "right"]]) {
    const w = world();
    const orig = w.sleep;
    // The run is cut during the 40 ms hold of this key (or button): after its press, before its release.
    w.sleep = async (ms) => {
      const down = w.cdp.some(([, p]) => p.code === held || (p.type === "mousePressed" && p.button === held));
      const upSent = w.cdp.some(([, p]) => p.type === "keyUp" ? p.code === held : p.type === "mouseReleased" && p.button === held);
      if (down && !upSent) throw new Error("interrupted");
      return orig(ms);
    };
    const r = await runReal(w, INPUT());
    assert.equal(r.sc().status, "error", held);
    assert.equal(w.cdp.some(([, p]) => (p.type === "keyUp" && p.code === up) || (p.type === "mouseReleased" && p.button === up)), true, `${held} was released on the way out`);
    assert.ok(r.timeline().some((x) => x.ev === "released" && /on the way out/.test(x.by)), held);
  }
  // A key that cannot be released: recorded as a step and as left behind, and the scenario is an error whatever the criteria say.
  const w = world({ keyUpFails: "KeyD" });
  const r = await runReal(w, INPUT());
  assert.equal(r.sc().status, "error");
  assert.match(r.sc().reason, /left behind: key KeyD could not be released \(the DevTools session is gone\)/);
  assert.ok(r.timeline().some((x) => x.ev === "release-failed" && x.what === "KeyD"));
});

test("B5: transport-fallback releases its held key on a thrown path; a release that fails is a step, left behind, and not a clean release in fallback.json", async () => {
  // Thrown after the key went down: the key comes up on the way out.
  const t = fallbackWorld();
  const orig = t.w.api.session;
  t.w.api.session = async (id) => { if (keyEvents(t.w).includes("keyDown:KeyW")) throw new Error("the API is down"); return orig(id); };
  const r = await runReal(t.w, FALLBACK("mid"), { hooksDir: t.hooksDir });
  assert.equal(r.sc().status, "error");
  assert.match(r.sc().reason, /the API is down/);
  assert.deepEqual(keyEvents(t.w), ["keyDown:KeyW", "keyUp:KeyW"]);
  assert.ok(r.timeline().some((x) => x.ev === "key-released" && /on the way out/.test(x.by)));
  assert.deepEqual(logOf(t.hooksDir).map((l) => l.split(" ")[0]), ["apply", "clear"], "applied after the key went down, cleared on the way out");
  // A release that fails, on the thrown path and on the normal one.
  const f = fallbackWorld({ keyUpFails: "KeyW" });
  const rf = await runReal(f.w, FALLBACK("mid"), { hooksDir: f.hooksDir });
  assert.equal(rf.sc().status, "error");
  assert.match(rf.sc().reason, /left behind: key KeyW could not be released \(the DevTools session is gone\)/);
  assert.ok(rf.timeline().some((x) => x.ev === "key-release-failed"));
  const rec = JSON.parse(read(rf.dir, `${rf.id()}/fallback.json`));
  assert.equal(rec.key_held, true);
  assert.equal(rec.key_released_by_runner, false);
  const g = fallbackWorld({ keyUpFails: "KeyW" });
  const gr = g.w.api.session;
  g.w.api.session = async (id) => { if (keyEvents(g.w).includes("keyDown:KeyW")) throw new Error("the API is down"); return gr(id); };
  const rg = await runReal(g.w, FALLBACK("mid"), { hooksDir: g.hooksDir });
  assert.match(rg.sc().reason, /the API is down; left behind: key KeyW could not be released/);
});

test("B7/N2: a session row that cannot be read at the end, or is another session, is an error in both scenarios", async () => {
  const unread = fallbackWorld();
  const orig = unread.w.api.session;
  unread.w.api.session = async (id) => (unread.w.faultSeenAt !== null && unread.w.t >= unread.w.faultSeenAt + 7000 ? undefined : orig(id));
  const r = await runReal(unread.w, FALLBACK("mid"), { hooksDir: unread.hooksDir });
  assert.equal(r.sc().status, "error");
  assert.match(r.sc().reason, /the API no longer returns the session this run started/);
  for (const reply of [undefined, { id: OTHER, app_id: "app-1", state: "running" }]) {
    const w = world();
    const first = w.api.session;
    let n = 0;
    w.api.session = async (id) => (++n === 1 ? first(id) : reply);
    const ri = await runReal(w, INPUT());
    assert.equal(ri.sc().status, "error", JSON.stringify(reply));
    assert.match(ri.sc().reason, /the API no longer returns the session this run started/);
  }
});

test("input and fallback plans load and name only scenarios that exist", () => {
  for (const [name, scenarios] of [["input", ["input-kbm"]], ["fallback", ["transport-fallback", "transport-fallback"]]]) {
    const plan = loadPlan(name);
    assert.deepEqual(plan.scenarios.map((s) => s.scenario), scenarios);
    for (const s of plan.scenarios) assert.equal((s.scenario === "input-kbm" ? checkInput : checkFallback)({ transport: "webrtc", codec: "h264", profile: "1080p60", hold_s: 90, app: "x", ...s.params }), null);
  }
  assert.ok(!loadPlan("smoke").scenarios.some((s) => ["input-kbm", "transport-fallback"].includes(s.scenario)));
});

// ── #1343: what the scenarios record, and say, when little happened ─────────────────────────────────────────

import { installRecorder } from "./scenarios/input-kbm.mjs";

test("#1343 item 1: the sequence's motion does not sum to zero, and the bundle holds each move the page received beside both sums", async () => {
  const sum = SEQUENCE.filter((s) => s.kind === "move").reduce((a, m) => ({ x: a.x + m.dx, y: a.y + m.dy }), { x: 0, y: 0 });
  assert.deepEqual(sum, { x: 50, y: 25 });
  const w = world({ wins: echoWindows([40]) });
  const r = await runReal(w, INPUT());
  const rec = JSON.parse(read(r.dir, `${r.id()}/input-sent.json`));
  assert.deepEqual(rec.sent_motion_sum, { x: 50, y: 25 });
  assert.deepEqual(rec.received_by_page.motion_sum, { x: 50, y: 25 });
  // The three moves of the sequence, each trusted; the pointer's walk to the lock click is not counted.
  assert.deepEqual(rec.received_by_page.mouse_moves, [[40, 0, true], [0, 30, true], [10, -5, true]]);
  assert.equal(rec.received_by_page.mouse_moves_total, 3);
  const m = r.by().motion_sum_matches;
  assert.equal(m.pass, null, "the host's side is still unknown");
  assert.equal(m.observed, null);
  assert.match(m.expected, /does not log individual keys/);
  assert.match(m.expected, /The page received 3 mousemove events summing to \(50, 25\) against \(50, 25\) sent/);
  // No motion arrived at all: a different bundle from motion that cancels.
  const none = world({ wins: echoWindows([40]), swallowMoves: true });
  const rn = await runReal(none, INPUT());
  assert.match(rn.by().motion_sum_matches.expected, /The page received 0 mousemove events summing to \(0, 0\) against \(50, 25\) sent/);
  assert.deepEqual(JSON.parse(read(rn.dir, `${rn.id()}/input-sent.json`)).received_by_page.mouse_moves, []);
  // No first frame: nothing was sent, so there is nothing to compare.
  const nf = await runReal(world({ noFrame: true }), INPUT());
  assert.doesNotMatch(nf.by().motion_sum_matches.expected, /The page received/);
});

test("#1343 item 1: the page's move log is capped at 200 entries", () => {
  const listeners = {};
  const saved = globalThis.window;
  globalThis.window = { addEventListener: (t, fn) => { listeners[t] = fn; } };
  try {
    installRecorder();
    for (let i = 0; i < 250; i += 1) listeners.mousemove({ isTrusted: true, movementX: 1, movementY: 2 });
    const r = globalThis.window.__e2eInput;
    assert.equal(r.moves.length, 200);
    assert.equal(r.moveCount, 250);
    assert.deepEqual(r.move, { x: 250, y: 500 });
  } finally { globalThis.window = saved; }
});

test("#1343 item 4: the mid run asks whether it already fell back BEFORE the lock and the key; a row it cannot read is an error that says so", async () => {
  const { w, hooksDir } = fallbackWorld({ noFallback: true });
  const first = w.api.session;
  let n = 0;
  w.api.session = async (id) => (++n === 2 ? undefined : first(id));
  const r = await runReal(w, FALLBACK("mid", { hold_s: 10 }), { hooksDir });
  assert.equal(r.sc().status, "error");
  assert.match(r.sc().reason, /the session row could not be read just before the fault/);
  assert.deepEqual(keyEvents(w), []);
  assert.equal(w.layerClicks + w.stageClicks, 0, "the lock was not asked for");
  assert.deepEqual(logOf(hooksDir), []);
  // A reason that is not a string is no reason: a number is not read as "fell back" (String() made it one).
  const num = fallbackWorld({ noFallback: true, session: { transport_fallback_reason: 12345 } });
  const rnum = await runReal(num.w, FALLBACK("mid", { hold_s: 10 }), { hooksDir: num.hooksDir });
  assert.doesNotMatch(rnum.sc().reason ?? "", /fell back at connect/);
  assert.deepEqual(logOf(num.hooksDir).map((l) => l.split(" ")[0]), ["apply", "clear"]);
});

test("#1343 item 4: the key is held only when the lock is held, not when a click was merely made", async () => {
  const { w, hooksDir } = fallbackWorld({ lockOnClick: false });
  const r = await runReal(w, FALLBACK("mid", { hold_s: 10 }), { hooksDir });
  assert.equal(w.layerClicks, 1, "the layer was clicked");
  assert.deepEqual(keyEvents(w), [], "no lock, so no key");
  const rec = JSON.parse(read(r.dir, `${r.id()}/fallback.json`));
  assert.equal(rec.key_held, false);
  assert.equal(rec.key_released_by_runner, null);
  assert.match(r.by().held_key_released.expected, /the pointer lock was not taken, through the "Click to play" layer or the HUD's Capture input button/);
  assert.ok(!r.timeline().some((x) => x.ev === "key-held"));
});

test("#1343 item 5: wording when nothing ran: where each wait starts, the connect wording, and the sequence that was not sent", async () => {
  const mid = fallbackWorld({ noFallback: true });
  const rm = await runReal(mid.w, FALLBACK("mid", { hold_s: 10 }), { hooksDir: mid.hooksDir });
  assert.match(rm.by().fallback_event.observed, /^none within 10 s of the fault$/);
  assert.match(rm.by().frame_after_fallback.observed, /^no <video> frame within 10 s of the fault$/);
  const con = fallbackWorld({ noFallback: true });
  const rc = await runReal(con.w, FALLBACK("connect", { hold_s: 10 }), { hooksDir: con.hooksDir });
  assert.match(rc.by().fallback_event.observed, /^none within 10 s of the first frame \(the fault was applied before the launch\)$/);
  // connect with no first frame: nothing is held in connect, and it says so (not "the stream was not watched").
  const nf = fallbackWorld({ noFrame: true });
  const rnf = await runReal(nf.w, FALLBACK("connect", { hold_s: 10 }), { hooksDir: nf.hooksDir });
  assert.match(rnf.by().held_key_released.expected, /nothing was held; no input is captured before the first picture/);
  assert.match(rnf.by().fallback_event.expected, /the stream was not watched after the fault/);
  // input-kbm with no first frame: the echo criterion says the sequence was not sent.
  const ri = await runReal(world({ noFrame: true }), INPUT());
  assert.match(ri.by().input_to_photon_ms.expected, /^not judged: no first frame, so the input sequence was not sent$/);
});

test("#1343 items 6 and 7: why the wait ended, how long, and how long no frame advanced before the first <video> frame", async () => {
  // The fallback is reported 3 s after the first sample and the <video> starts with it: the canvas stops at the fault.
  const { w, hooksDir } = fallbackWorld();
  const r = await runReal(w, FALLBACK("mid"), { hooksDir });
  const rec = JSON.parse(read(r.dir, `${r.id()}/fallback.json`));
  assert.equal(rec.wait_ended_by, "fallback_and_frame_seen");
  assert.equal(rec.waited_s, 5);
  assert.equal(rec.no_frame_advance_before_video_s, 4);
  const step = r.timeline().find((x) => x.ev === "wait-ended");
  assert.deepEqual([step.by, step.waited_s, step.no_frame_advance_s], ["fallback_and_frame_seen", 5, 4]);
  assert.match(r.by().frame_after_fallback.observed, /^5 s after the fault; no frame advanced for 4 s of it$/);
  // No fallback: the wait ends because the hold ran out.
  const none = fallbackWorld({ noFallback: true });
  const rn = await runReal(none.w, FALLBACK("mid", { hold_s: 10 }), { hooksDir: none.hooksDir });
  const recN = JSON.parse(read(rn.dir, `${rn.id()}/fallback.json`));
  assert.equal(recN.wait_ended_by, "hold_elapsed");
  assert.equal(recN.waited_s, 10);
  // Bench frames that keep advancing are a stream that did not freeze, whatever the <video> says.
  let ww;
  const moving = { get frames() { return Math.floor((ww.t - T0) / 1000) * 30; }, decoded: 100 };
  const live = fallbackWorld({ stats: moving });
  ww = live.w;
  const rl = await runReal(live.w, FALLBACK("mid"), { hooksDir: live.hooksDir });
  assert.equal(JSON.parse(read(rl.dir, `${rl.id()}/fallback.json`)).no_frame_advance_before_video_s, 0);
  assert.match(rl.by().frame_after_fallback.observed, /no frame advanced for 0 s of it/);
  // In connect nothing was streaming before the fault, so there is no freeze to state.
  const con = fallbackWorld({ fallbackReason: "connect_failed" });
  const rc = await runReal(con.w, FALLBACK("connect"), { hooksDir: con.hooksDir });
  assert.equal(JSON.parse(read(rc.dir, `${rc.id()}/fallback.json`)).no_frame_advance_before_video_s, null);
  assert.doesNotMatch(rc.by().frame_after_fallback.observed, /no frame advanced/);
  // The session ends before the first sample: nothing was compared, so no freeze is stated (not 0).
  const gone = fallbackWorld();
  const session = gone.w.api.session;
  gone.w.api.session = async (id) => { const row = await session(id); return row && logOf(gone.hooksDir).some((l) => l.startsWith("apply")) ? { ...row, state: "stopped" } : row; };
  const rg = await runReal(gone.w, FALLBACK("mid"), { hooksDir: gone.hooksDir });
  const recG = JSON.parse(read(rg.dir, `${rg.id()}/fallback.json`));
  assert.deepEqual([recG.wait_ended_by, recG.no_frame_advance_before_video_s], ["session_ended", null]);
  assert.equal(rg.timeline().find((x) => x.ev === "wait-ended").no_frame_advance_s, null);
  // The bench ring is full: its count no longer grows, so "no advance" is not known.
  const full = fallbackWorld({ stats: { frames: 6000, decoded: 100 } });
  const rf = await runReal(full.w, FALLBACK("mid"), { hooksDir: full.hooksDir });
  assert.equal(JSON.parse(read(rf.dir, `${rf.id()}/fallback.json`)).no_frame_advance_before_video_s, null);
  assert.doesNotMatch(rf.by().frame_after_fallback.observed, /no frame advanced/);
  // A value that is not a finite number is not stated.
  const get = (freezeS) => Object.fromEntries(judgeFallback({ params: { hold_s: 30 }, firstFrameS: 1, seen: { row: { state: "running" }, fallbackReason: "x", posted: [], videoAfterS: 4, tail: { before: 1, after: 9 }, keyHeld: false, faultApplied: true, watched: true, freezeS } }).map((c) => [c.id, c]));
  assert.doesNotMatch(get(NaN).frame_after_fallback.observed, /no frame advanced/);
  assert.match(get(2).frame_after_fallback.observed, /no frame advanced for 2 s of it/);
});
