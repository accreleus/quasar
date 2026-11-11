#!/usr/bin/env node
// make e2e-run: runs a plan of scenarios against a stack from a test client and leaves a run
// bundle (docs/testing-e2e-suite.md). A sibling of scripts/validate/runner.mjs: that runner mints
// storage states with a dev-agent key, which a remote client does not have, so this one signs in
// as an ordinary user through the web client. The journey shape, the evidence listeners and the
// argument parser are the same.
//
//   node run.mjs --target <url> --plan <smoke|file> [--cdp <endpoint>] [--out <dir>]
//   env: QUASAR_E2E_EMAIL / QUASAR_E2E_PASSWORD (or QUASAR_E2E_CRED_FILE=<KEY=value lines>),
//        QUASAR_E2E_APP (reference app name), CHROME (browser to launch when --cdp is absent),
//        CLIENT (a label for this test client, a role, never a host name),
//        INSECURE_TLS=1 (a stack with a self-signed certificate: turns certificate checks off)
//
// Exit 0 at least one scenario passed and none failed or errored, 1 otherwise, 2 usage, a bundle that
// fails validateBundle (it is deleted, never left), or a browser that could not be left clean.
import { execFileSync } from "node:child_process";
import crypto from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import { parseArgs } from "../lib/args.mjs";
import { attachEvidenceListeners } from "../lib/listeners.mjs";
import { FORMAT, validateBundle } from "./bundle.mjs";
import { Recorder, ScenarioRecorder } from "./record.mjs";

const HERE = path.dirname(fileURLToPath(import.meta.url));
const ROOT = path.resolve(HERE, "../../..");
const TERMINAL = ["stopped", "failed"];
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
export const DEFAULT_APP = "Quasar Benchapp";
const LABEL = /^[A-Za-z0-9_.-]{1,64}$/;
// The only writes outside the launch and sign-in calls that the page sends by itself on this path, each by exact method and path:
// the library's keep-alive (control-api.md §Warm Steam; it can leave a parked Steam session) and the device record every sign-in posts
// (web/src/auth/AuthProvider.tsx, deviceProbe.ts). Any other write is refused by default (trackLaunches).
const PAGE_WRITES = ["PUT /v1/me/steam-warm", "POST /v1/me/devices"];
const MAX_STOP_ROUNDS = 10;
const PLAN_KEYS = ["name", "scenarios"];
const ENTRY_KEYS = ["scenario", "params"];
// A secret this short cannot be taken out of unrelated values (a UUID, a key) without damaging them.
export const MIN_SECRET = 8;

const NOT_RECORDED = [
  { fact: "audio_latency_ms", why: "nothing in the product or the page measures audio latency yet" },
  { fact: "audio_video_offset_ms", why: "the WebTransport client computes an audio/video offset but bench mode does not expose it" },
  { fact: "client_thermal_state", why: "no browser API reports thermal pressure to a page" },
  { fact: "path_rtt_ms", why: "bench-mode windows carry no round-trip time; the page's own samples are not read" },
];

export function loadPlan(arg) {
  const file = /[/\\]|\.json$/.test(arg) ? arg : path.join(HERE, "plans", `${arg}.json`);
  const plan = JSON.parse(fs.readFileSync(file, "utf8"));
  if (!plan || typeof plan !== "object" || Array.isArray(plan)) throw new Error(`plan ${arg} is not an object`);
  const unknown = Object.keys(plan).filter((k) => !PLAN_KEYS.includes(k));
  if (unknown.length) throw new Error(`plan ${arg} has a key this runner does not define: ${unknown.map((k) => JSON.stringify(k.slice(0, 40))).join(", ")} (it defines ${PLAN_KEYS.join(", ")})`);
  if (!Array.isArray(plan.scenarios) || plan.scenarios.length === 0) throw new Error(`plan ${arg} lists no scenarios`);
  return plan;
}

/** The product's own numbers (docs/session-trace/thresholds.json); a missing or non-numeric one stops the run before it starts. */
export function readThresholds(file = path.join(ROOT, "docs/session-trace/thresholds.json")) {
  const t = JSON.parse(fs.readFileSync(file, "utf8")).thresholds;
  const get = (key) => {
    const v = t[key]?.value;
    if (typeof v !== "number" || !Number.isFinite(v)) throw new Error(`${file} states no number for ${key}`);
    return v;
  };
  return {
    hitch_sd_ms: get("classifier.hitch_sd_ms"),
    hitch_min_samples: get("classifier.hitch_min_samples"),
    warmup_exclude_s: get("classifier.warmup_exclude_s"),
    present_min_samples: get("web.present_cadence.min_samples"),
  };
}

export function readCreds(env) {
  const file = {};
  if (env.QUASAR_E2E_CRED_FILE) {
    for (const line of fs.readFileSync(env.QUASAR_E2E_CRED_FILE, "utf8").split("\n")) {
      const i = line.indexOf("=");
      if (i > 0) file[line.slice(0, i).trim()] = line.slice(i + 1).trim();
    }
  }
  return {
    email: env.QUASAR_E2E_EMAIL ?? file.QUASAR_E2E_EMAIL ?? file.EMAIL,
    password: env.QUASAR_E2E_PASSWORD ?? file.QUASAR_E2E_PASSWORD ?? file.PASSWORD,
  };
}

/** Why the sign-in cannot be removed safely from what the run writes, or null. */
export function secretTooShort(creds) {
  const local = String(creds.email).split("@")[0];
  if (String(creds.password).length < MIN_SECRET || local.length < MIN_SECRET) {
    return `the test user's password and e-mail local part must each be at least ${MIN_SECRET} characters: a shorter secret cannot be removed from the bundle without damaging unrelated values. Use a test user with a longer password`;
  }
  return null;
}

/**
 * A write is any method but GET, HEAD and OPTIONS (those read; a method this runner has never heard of is a write until proven otherwise,
 * because a write it does not know is one it cannot put back). Not counted: the launch and the session's own calls (/v1/sessions), the
 * sign-in calls (/v1/auth/), and the two exact PAGE_WRITES. Methods compare as sent: "patch" is not "GET".
 */
export function isUnexpectedWrite(method, pathname) {
  if (["GET", "HEAD", "OPTIONS"].includes(method)) return false;
  if (pathname === "/v1/sessions" || pathname.startsWith("/v1/sessions/") || pathname.startsWith("/v1/auth/")) return false;
  return !PAGE_WRITES.includes(`${method} ${pathname}`);
}

/**
 * The sessions this run owns: the ids in the 201s of its page's own POST /v1/sessions to the stack's own
 * origin, and nothing else. A page address, a list or a baseline never adds one. `writes` records every other
 * write the page sent to the stack: the runner does not know how to put it back, so the scenario reports it as left behind.
 */
export function trackLaunches(page, baseUrl, onOwned = () => {}) {
  const origin = new URL(baseUrl).origin;
  const t = { owned: new Set(), unread: 0, writes: [], pending: new Set() };
  const track = (p) => { t.pending.add(p); p.finally(() => t.pending.delete(p)); };
  /** Waits for every response handler started so far, and those they start. */
  t.settle = async () => { while (t.pending.size) await Promise.allSettled([...t.pending]); };
  page.on("response", (r) => {
    let u;
    try { u = new URL(r.url()); } catch { return; }
    if (u.origin !== origin) return;
    const method = r.request().method();
    if (r.status() === 201 && method === "POST" && u.pathname === "/v1/sessions") {
      track((async () => {
        const id = (await r.json().catch(() => null))?.session?.id;
        if (typeof id === "string" && UUID.test(id) && !t.owned.has(id)) { t.owned.add(id); onOwned(id); }
        else t.unread += 1;
      })());
    } else if (isUnexpectedWrite(method, u.pathname)) {
      const w = `${method.slice(0, 16)} ${u.pathname.slice(0, 80)}`;
      if (!t.writes.includes(w)) t.writes.push(w);
    }
  });
  return t;
}

/**
 * Stops every owned session, again for any that arrive while stopping, until the set is stable or MAX_STOP_ROUNDS rounds have run
 * (then every id not stopped is named). A stop counts only once the API confirms it, and only for a row the API returns to this user first:
 * an id it answers 404 for is not shown to be this user's, so it is neither deleted nor written as stopped.
 */
export async function stopOwned(api, launches, rec) {
  const cleanup = [];
  const done = new Set();
  const unconfirmed = new Set();
  for (let round = 0; ; round += 1) {
    await launches.settle();
    const todo = [...launches.owned].filter((id) => !done.has(id));
    if (!todo.length) break;
    if (round >= MAX_STOP_ROUNDS) {
      for (const id of todo) {
        rec.step("stop-failed", { session: id, message: `not stopped: launches kept arriving after ${MAX_STOP_ROUNDS} rounds` });
        cleanup.push(`session ${id} is not stopped (launches kept arriving after ${MAX_STOP_ROUNDS} rounds of stopping)`);
      }
      break;
    }
    for (const id of todo) {
      done.add(id);
      try {
        if (!(await api.session(id))) {
          unconfirmed.add(id);
          rec.step("stop-unconfirmed", { session: id, why: "the API does not return this session to this user, so it was not stopped" });
          cleanup.push(`session ${id} is not confirmed to be this user's (the API does not return it), so it was not stopped`);
          continue;
        }
        await api.stop(id);
        rec.step("stopped", { session: id });
      } catch (e) {
        rec.step("stop-failed", { session: id, message: String(e?.message ?? e) });
        cleanup.push(`session ${id} is not confirmed stopped (${String(e?.message ?? e)})`);
      }
    }
  }
  if (launches.unread) cleanup.push(`${launches.unread} session launch(es) were accepted but the id could not be read, so they could not be stopped`);
  for (const w of launches.writes) cleanup.push(`the page changed something on the stack (${w}) that this run cannot put back`);
  return { sessions: [...launches.owned].filter((id) => !unconfirmed.has(id)), cleanup };
}

/** The API as the signed-in user: their own bearer, never an admin token. The bearer goes into `secrets` the moment it is known. */
export function makeApi(baseUrl, creds, { fetchFn = fetch, sleep = (ms) => new Promise((r) => setTimeout(r, ms)), secrets = new Set() } = {}) {
  let token = null;
  const call = async (method, p, body) => {
    if (!token) {
      const r = await fetchFn(`${baseUrl}/v1/auth/login`, { method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify(creds) });
      token = (await r.json().catch(() => ({}))).access_token;
      if (!token) throw new Error(`API sign-in failed (${r.status})`);
      secrets.add(token);
    }
    const r = await fetchFn(baseUrl + p, { method, headers: { authorization: `Bearer ${token}`, "content-type": "application/json" }, body: body && JSON.stringify(body) });
    return { status: r.status, body: await r.json().catch(() => ({})) };
  };
  const read = (id) => call("GET", `/v1/sessions/${encodeURIComponent(id)}`);
  return {
    /** The session the API returns for `id`, or undefined when it has none (404). */
    async session(id) {
      const r = await read(id);
      if (r.status === 404) return undefined;
      if (r.status !== 200) throw new Error(`GET session returned ${r.status}`);
      return r.body.session;
    },
    apps: async () => (await call("GET", "/v1/apps")).body.items ?? [],
    /** Stops `id` and returns only once the API says it is over (terminal, or gone); a read that failed is not that answer. */
    async stop(id) {
      const r = await call("DELETE", `/v1/sessions/${encodeURIComponent(id)}`);
      if (![200, 202, 204, 404].includes(r.status)) throw new Error(`DELETE session returned ${r.status}`);
      for (let i = 0; i < 30; i += 1) {
        const g = await read(id).catch(() => null);
        if (g && (g.status === 404 || TERMINAL.includes(g.body.session?.state))) return;
        await sleep(1000);
      }
      throw new Error("the session did not reach a terminal state in 30 s");
    },
  };
}

/** What the page can tell about the client it runs on; client.json is this, builds.json takes browser and os from it. */
export async function probeClient(page) {
  return page.evaluate(async () => {
    const ua = navigator.userAgentData;
    const hi = ua ? await ua.getHighEntropyValues(["platformVersion", "architecture", "fullVersionList"]).catch(() => null) : null;
    const brand = (hi?.fullVersionList ?? ua?.brands ?? []).find((b) => /chrom/i.test(b.brand) && !/not/i.test(b.brand));
    // A hidden tab never runs requestAnimationFrame: the probe gives up after 2 s and says null.
    const refresh_hz = await new Promise((resolve) => {
      let n = 0;
      let t0 = 0;
      setTimeout(() => resolve(null), 2000);
      const tick = (t) => {
        if (!t0) t0 = t;
        n += 1;
        if (t - t0 >= 1000) resolve(Math.round(((n - 1) * 1000) / (t - t0)));
        else requestAnimationFrame(tick);
      };
      requestAnimationFrame(tick);
    });
    const adapter = navigator.gpu ? await navigator.gpu.requestAdapter().catch(() => null) : null;
    const gl = document.createElement("canvas").getContext("webgl");
    const ext = gl?.getExtension("WEBGL_debug_renderer_info");
    const renderer = ext ? gl.getParameter(ext.UNMASKED_RENDERER_WEBGL) : null;
    const gpu = renderer ?? (adapter?.info ? [adapter.info.vendor, adapter.info.architecture].filter(Boolean).join(" ") : null);
    const types = { h264: "video/H264", h265: "video/H265", av1: "video/AV1" };
    const webcodecs = { h264: "avc1.64002a", h265: "hvc1.1.6.L153.B0", av1: "av01.0.08M.08" };
    const decode = {};
    for (const c of Object.keys(types)) {
      const info = await navigator.mediaCapabilities?.decodingInfo({ type: "webrtc", video: { contentType: types[c], width: 1920, height: 1080, framerate: 60, bitrate: 10_000_000 } }).catch(() => null);
      const wc = typeof VideoDecoder === "function" ? await VideoDecoder.isConfigSupported({ codec: webcodecs[c] }).catch(() => null) : null;
      decode[c] = { supported: info ? info.supported : null, power_efficient: info ? info.powerEfficient : null, webcodecs: wc ? wc.supported : null };
    }
    const bat = navigator.getBattery ? await navigator.getBattery().catch(() => null) : null;
    // A full battery that is charging is also what a machine with no battery reports: that says nothing.
    const power = bat && !(bat.charging && bat.level === 1) ? { source: bat.charging ? "ac" : "battery", level: bat.level } : null;
    return {
      kind: "web",
      browser: { name: brand?.brand ?? "unknown", version: brand?.version ?? "unknown" },
      os: { name: hi?.platform ?? ua?.platform ?? navigator.platform, version: hi?.platformVersion ?? null },
      cpu: { cores: navigator.hardwareConcurrency ?? null, architecture: hi?.architecture ?? null },
      gpu: { adapter: gpu },
      displays: [{ width: screen.width, height: screen.height, refresh_hz, pixel_ratio: devicePixelRatio }],
      decode,
      power,
      webtransport: typeof WebTransport === "function",
      webgpu: !!adapter,
      user_agent: navigator.userAgent, // Chrome's reduced string; browser, os and cpu above come from client hints
    };
  });
}

const nn = (i) => String(i + 1).padStart(2, "0");

/** The scenario module for `name`, or a reason it cannot be one. Names are file names under scenarios/. */
async function loadScenario(name) {
  const file = path.join(HERE, "scenarios", `${name}.mjs`);
  if (typeof name !== "string" || !/^[\w-]+$/.test(name) || !fs.existsSync(file)) throw new Error(`no scenario named ${JSON.stringify(String(name)).slice(0, 40)}`);
  return (await import(pathToFileURL(file).href)).default;
}

/**
 * Runs `plan` and writes the bundle. It is built in a staging directory beside `dir`, checked with validateBundle,
 * and only then renamed to `dir`: a bundle that fails the check is deleted and its problems returned, so no
 * file is left holding what the redaction missed. `host` supplies the browser: newPage(), closePage(page),
 * probeClient(page), browserNow(page), close(), and (optionally) `info` for builds.json. Never throws for a
 * scenario's failure; returns the counts.
 */
export async function runSuite({ plan, dir, baseUrl, creds, host, api, thresholds, label = null, insecureTls = false, secrets = new Set(), app = DEFAULT_APP, commit = "unknown", dirty = false, abort = new Promise(() => {}), now = Date.now, sleep = (ms) => new Promise((r) => setTimeout(r, ms)) }) {
  if (fs.existsSync(dir)) throw new Error(`${dir} already exists: a bundle is never written over another`);
  const suiteRunId = `e2e-${new Date(now()).toISOString().replace(/[-:]|\.\d+/g, "")}-${crypto.randomBytes(2).toString("hex")}`;
  const staging = path.join(path.dirname(path.resolve(dir)), `.${path.basename(dir)}.staging-${crypto.randomBytes(4).toString("hex")}`);
  let committed = false;
  try {
    const url = new URL(baseUrl);
    const bundle = new Recorder(staging, { now, secrets, literals: [[url.host, "<stack>"], [url.hostname, "<stack>"], [creds.email, "<user>"], [String(creds.email).split("@")[0], "<user>"], [creds.password, "<credential>"]] });
    const startedAt = now();
    const racedSleep = (ms) => Promise.race([sleep(ms), abort.then(() => { throw new Error("interrupted"); })]);
    const clock = { offset: null }; // the browser's clock minus the runner's, measured once around a round trip

    const entries = plan.scenarios.map((raw, i) => {
      const s = raw && typeof raw === "object" && !Array.isArray(raw) ? raw : {};
      const own = s.params === undefined ? {} : s.params;
      const bad = !(own && typeof own === "object" && !Array.isArray(own)) ? "params must be an object" : null;
      const stray = Object.keys(s).filter((k) => !ENTRY_KEYS.includes(k));
      const params = { transport: "webrtc", codec: "h264", profile: "1080p60", hold_s: 90, app, ...(bad ? {} : own) };
      return { id: `${nn(i)}-${String(s.scenario).replace(/[^\w.-]/g, "_").slice(0, 40)}`, scenario: s.scenario, params, problem: bad ?? (stray.length ? `the plan entry has a key this runner does not define: ${stray.map((k) => JSON.stringify(k.slice(0, 40))).join(", ")} (it defines ${ENTRY_KEYS.join(", ")})` : null) };
    });

    const probePage = await host.newPage();
    let client;
    try {
      await probePage.goto(baseUrl, { waitUntil: "domcontentloaded" });
      const t0 = now();
      const browserNow = await host.browserNow(probePage);
      const t1 = now();
      clock.offset = Math.round(browserNow - (t0 + t1) / 2);
      client = { ...(await host.probeClient(probePage)), label, clock: { browser_minus_runner_ms: clock.offset, round_trip_ms: t1 - t0 } };
    } finally {
      await host.closePage(probePage).catch((e) => { throw new Error(`the probe page would not close: ${e?.message ?? e}`); });
    }
    bundle.json("client.json", client);

    const counts = { pass: 0, fail: 0, error: 0, skipped: 0 };
    const dirs = [];
    let interrupted = false;
    abort.then(() => { interrupted = true; });
    for (const e of entries) {
      dirs.push(e.id);
      const rec = new ScenarioRecorder(bundle, e.id, { clockOffset: () => clock.offset });
      const stamp = () => new Date(now() + (clock.offset ?? 0)).toISOString();
      let out;
      if (interrupted) {
        out = { status: "skipped", reason: "the run was interrupted before this scenario", criteria: [], sessions: [], clock_offset_ms: null };
      } else {
        const sink = { consoleErrors: [], networkFailures: [] };
        let page = null;
        let left = null;
        let launches = null;
        let owned = { sessions: [], cleanup: [] };
        try {
          if (e.problem) throw new Error(`the plan cannot be run: ${e.problem}`);
          const scenario = await loadScenario(e.scenario);
          const bad = scenario.check?.(e.params);
          if (bad) throw new Error(`the plan cannot be run: ${bad}`);
          page = await host.newPage();
          rec.put("console.log", "");
          launches = trackLaunches(page, baseUrl, (id) => rec.step("session-started", { session: id }));
          attachEvidenceListeners(page, baseUrl, sink);
          // Nothing the page says is written until its bearer is known (rec.release, after sign-in): see ScenarioRecorder.
          page.on("console", (m) => rec.fromPage("console.log", `${stamp()} [${m.type()}] ${m.text()}\n`));
          page.on("pageerror", (err) => rec.fromPage("console.log", `${stamp()} [pageerror] ${err.message}\n`));
          // The page signs in on its own; its bearer is a secret of this run, whatever the page does with it afterwards.
          page.on("response", (r) => {
            if (r.request().method() === "POST" && new URL(r.url()).pathname === "/v1/auth/login" && r.status() === 200) {
              r.json().then((b) => { if (typeof b?.access_token === "string") secrets.add(b.access_token); }).catch(() => {});
            }
          });
          out = await scenario.run(page, { baseUrl, params: e.params, rec, api, creds, client, thresholds, secrets, launches, now, sleep: racedSleep, clockOffset: () => clock.offset });
        } catch (err) {
          // A page-derived message is not written while the page's bearer is unknown (the scenario catches its own errors; this is what escapes it).
          out = { status: "error", reason: page && rec.holding ? "the scenario did not complete (the page's own words are not written before its sign-in is read)" : String(err?.message ?? err), criteria: [], sessions: [], clock_offset_ms: null };
        } finally {
          // Order matters: close the page, wait for every response it was still delivering, then stop what the run owns.
          // The close error's words are the browser's about a page: held back like the page's own until its bearer is known.
          if (page) await host.closePage(page).catch((err) => { left = `a page it opened would not close${rec.holding ? "" : ` (${err?.message ?? err})`}`; });
          if (launches) owned = await stopOwned(api, launches, rec);
          const dropped = rec.dropHeld() + (rec.holding ? sink.consoleErrors.length + sink.networkFailures.length : 0);
          if (dropped) rec.step("pre-auth-evidence-dropped", { lines: dropped, why: "the page's bearer was never known, so nothing it said could be written safely" });
          rec.json("failed-requests.json", rec.holding ? { console_errors: [], network_failures: [] } : { console_errors: sink.consoleErrors, network_failures: sink.networkFailures });
        }
        out = { ...out, sessions: owned.sessions };
        const cleanup = [...(out.cleanup ?? []), ...owned.cleanup, ...(left ? [left] : [])];
        if (cleanup.length) {
          rec.step("cleanup-failed", { left_behind: cleanup });
          out = { ...out, status: "error", reason: `${out.reason ? `${out.reason}; ` : ""}left behind: ${cleanup.join("; ")}` };
        }
      }
      counts[["pass", "fail", "skipped"].includes(out.status) ? out.status : "error"] += 1;
      rec.json("scenario.json", { id: e.id, scenario: String(e.scenario), params: e.params, status: out.status, ...(out.reason ? { reason: out.reason } : {}), sessions: out.sessions ?? [], clock_offset_ms: out.clock_offset_ms ?? null, criteria: out.criteria ?? [] });
      if (out.status === "skipped") rec.step("skipped", { reason: out.reason });
    }

    const info = host.info ?? {};
    bundle.json("builds.json", {
      components: { browser: client.browser, os: client.os, web: null, control_plane: null, node_agent: null, reference_app: null, gpu_driver: null },
      // e2e-collect reads the control plane and the node agent only (collect.mjs).
      unknown: Object.fromEntries(["web", "control_plane", "node_agent", "reference_app", "gpu_driver"].map((k) => [k, info[k] ?? (["control_plane", "node_agent"].includes(k) ? "the host half (e2e-collect) fills this; the client cannot see it" : "the client cannot see it, and e2e-collect does not read it yet")])),
    });
    bundle.json("network.json", { path: { rtt_ms: null }, shaping: null, tls_verification: insecureTls ? "off (INSECURE_TLS=1)" : "on" });
    bundle.json("manifest.json", {
      format: FORMAT,
      suite_run_id: suiteRunId,
      plan: { name: plan.name ?? "plan", scenarios: entries.map(({ id, scenario, params }) => ({ id, scenario: String(scenario), params })) },
      started_at_ms: startedAt,
      ended_at_ms: now(),
      runner: { commit, ...(dirty ? { dirty: true } : {}) },
      halves: { client: true, host: false },
      scenarios: dirs,
      not_recorded: NOT_RECORDED,
      redacted: Object.keys(bundle.redacted),
      target: "stack",
    });
    const errors = [...bundle.problems, ...validateBundle(staging)];
    if (!errors.length) { fs.renameSync(staging, dir); committed = true; }
    return { counts, suiteRunId, errors, bundle: committed ? dir : null };
  } finally {
    if (!committed) fs.rmSync(staging, { recursive: true, force: true });
  }
}

/** The runner's own commit, and whether tracked files differ from it (a run from a tree with uncommitted changes is not the commit it names). */
export function runnerState(cwd = ROOT) {
  const git = (...args) => execFileSync("git", args, { cwd, encoding: "utf8", env: { PATH: process.env.PATH ?? "", HOME: process.env.HOME ?? "" } });
  try { return { commit: git("rev-parse", "HEAD").trim(), dirty: git("status", "--porcelain", "--untracked-files=no").trim() !== "" }; } catch { return { commit: "unknown", dirty: false }; /* not a checkout */ }
}

/** Certificate checks stay on unless the caller asks (INSECURE_TLS=1, for a stack with a self-signed certificate): the process-wide switch Node's fetch has. */
export function configureTls(insecure, env = process.env) {
  // Whatever the environment says, checks are on unless the caller asked: an inherited "0" is removed. Returns what is in force.
  if (insecure) env.NODE_TLS_REJECT_UNAUTHORIZED = "0";
  else delete env.NODE_TLS_REJECT_UNAUTHORIZED;
  return env.NODE_TLS_REJECT_UNAUTHORIZED === "0";
}

/**
 * The runner works in a browser context of its own. Attached, that is a separate storage partition of the
 * person's browser, so their tabs, storage and sign-in are never read or written, and `close` closes only it.
 */
export async function openHost(browser, { attached, insecureTls }) {
  const context = await browser.newContext({ ignoreHTTPSErrors: insecureTls, viewport: null });
  return {
    host: { newPage: () => context.newPage(), closePage: (p) => p.close(), probeClient, browserNow: (p) => p.evaluate(() => Date.now()) },
    close: () => (attached ? context.close() : browser.close()),
  };
}

/** The RESULT line's status and the exit code: ok only when something passed and nothing failed. */
export function summarise({ counts: { pass, fail, error }, errors, closeFailed = null }) {
  if (errors.length || closeFailed) return { status: "error", code: 2, more: "" };
  if (fail + error) return { status: "failed", code: 1, more: "" };
  if (pass === 0) return { status: "failed", code: 1, more: " reason=nothing-passed" };
  return { status: "ok", code: 0, more: "" };
}

function usage(msg) {
  console.error(`${msg}\nusage: e2e-run TARGET=<stack url> PLAN=<plan> [CDP=<endpoint>] [OUT=<dir>] [CLIENT=<label>] [INSECURE_TLS=1]; sign-in from QUASAR_E2E_EMAIL and QUASAR_E2E_PASSWORD (or QUASAR_E2E_CRED_FILE)`);
  console.log("RESULT status=error target=e2e-run bundle= pass=0 fail=0 error=0 skipped=0");
  process.exit(2);
}

async function main() {
  const args = parseArgs(process.argv.slice(2));
  const baseUrl = typeof args.target === "string" ? args.target.replace(/\/$/, "") : null;
  if (!baseUrl || typeof args.plan !== "string") return usage("TARGET and PLAN are required");
  const label = process.env.CLIENT || null;
  if (label !== null && !LABEL.test(label)) return usage("CLIENT must be a plain identifier (letters, digits, _ . -), a role such as mac-test-client, never a host name");
  const creds = readCreds(process.env);
  if (!creds.email || !creds.password) return usage("no sign-in: set QUASAR_E2E_EMAIL and QUASAR_E2E_PASSWORD");
  const short = secretTooShort(creds);
  if (short) return usage(short);
  // The sign-in lives in this process only: no browser or other child inherits it.
  for (const k of ["QUASAR_E2E_EMAIL", "QUASAR_E2E_PASSWORD", "QUASAR_E2E_CRED_FILE"]) delete process.env[k];
  let plan;
  let thresholds;
  try {
    plan = loadPlan(args.plan);
    thresholds = readThresholds();
  } catch (e) {
    return usage(e.message);
  }

  const insecureTls = configureTls(process.env.INSECURE_TLS === "1");
  const { chromium } = await import("playwright").catch(() => import("playwright-core"));
  const browser = args.cdp
    ? await chromium.connectOverCDP(args.cdp)
    : await chromium.launch({ executablePath: process.env.CHROME || undefined, headless: !(process.env.DISPLAY || process.env.WAYLAND_DISPLAY) });
  const out = path.resolve(typeof args.out === "string" ? args.out : `e2e-bundle-${Date.now()}`);
  const { commit, dirty } = runnerState();

  let stopRun;
  const abort = new Promise((r) => { stopRun = r; });
  for (const sig of ["SIGINT", "SIGTERM"]) process.on(sig, () => stopRun());

  const secrets = new Set();
  const api = makeApi(baseUrl, creds, { secrets });
  let result;
  let closeFailed = null;
  let opened = null;
  try {
    opened = await openHost(browser, { attached: !!args.cdp, insecureTls });
    result = await runSuite({ plan, dir: out, baseUrl, creds, host: opened.host, api, thresholds, label, insecureTls, secrets, app: process.env.QUASAR_E2E_APP || DEFAULT_APP, commit, dirty, abort });
  } finally {
    // Attached, only this run's own context is closed; the person's browser keeps running as it was.
    try { await (opened ? opened.close() : args.cdp ? null : browser.close()); } catch (e) { closeFailed = e; }
  }
  if (closeFailed) console.error(`the runner's browser context would not close: ${closeFailed.message}`);
  for (const e of result.errors) console.error(e);
  const { status, code, more } = summarise({ ...result, closeFailed });
  const { pass, fail, error, skipped } = result.counts;
  console.log(`RESULT status=${status} target=e2e-run bundle=${result.bundle ?? ""} pass=${pass} fail=${fail} error=${error} skipped=${skipped}${more}`);
  process.exit(code);
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  main().catch((e) => { console.error(e); console.log("RESULT status=error target=e2e-run bundle= pass=0 fail=0 error=1 skipped=0"); process.exit(2); });
}
