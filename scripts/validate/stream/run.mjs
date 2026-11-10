#!/usr/bin/env node
// make e2e-run: runs a plan of scenarios against a stack from a test client and leaves a run
// bundle (docs/testing-e2e-suite.md). A sibling of scripts/validate/runner.mjs: that runner mints
// storage states with a dev-agent key, which a remote client does not have, so this one signs in
// as an ordinary user through the web client. The journey shape, the evidence listeners and the
// argument parser are the same.
//
//   node run.mjs --target <url> --plan <smoke|file> [--cdp <endpoint>] [--out <dir>]
//   env: QUASAR_E2E_EMAIL / QUASAR_E2E_PASSWORD (or QUASAR_E2E_CRED_FILE=<KEY=value lines>),
//        QUASAR_E2E_APP (reference app name), CHROME (browser to launch when --cdp is absent)
//
// Exit 0 no scenario failed, 1 one did, 2 usage or a bundle that fails validateBundle.
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
const AUTH_KEYS = "quasar.auth.";
export const DEFAULT_APP = "Quasar Benchapp";

const NOT_RECORDED = [
  { fact: "audio_latency_ms", why: "nothing in the product or the page measures audio latency yet" },
  { fact: "audio_video_offset_ms", why: "the WebTransport client computes an audio/video offset but bench mode does not expose it" },
  { fact: "client_thermal_state", why: "no browser API reports thermal pressure to a page" },
  { fact: "path_rtt_ms", why: "bench-mode windows carry no round-trip time; the page's own samples are not read" },
];

export function loadPlan(arg) {
  const file = /[/\\]|\.json$/.test(arg) ? arg : path.join(HERE, "plans", `${arg}.json`);
  const plan = JSON.parse(fs.readFileSync(file, "utf8"));
  if (!Array.isArray(plan.scenarios) || plan.scenarios.length === 0) throw new Error(`plan ${arg} lists no scenarios`);
  return plan;
}

export function readThresholds(file = path.join(ROOT, "docs/session-trace/thresholds.json")) {
  const t = JSON.parse(fs.readFileSync(file, "utf8")).thresholds;
  return { hitch_sd_ms: t["classifier.hitch_sd_ms"].value, hitch_min_samples: t["classifier.hitch_min_samples"].value };
}

export function readCreds(env) {
  const all = { ...env };
  if (env.QUASAR_E2E_CRED_FILE) {
    for (const line of fs.readFileSync(env.QUASAR_E2E_CRED_FILE, "utf8").split("\n")) {
      const i = line.indexOf("=");
      if (i > 0) all[line.slice(0, i).trim()] = line.slice(i + 1).trim();
    }
  }
  return { email: all.QUASAR_E2E_EMAIL ?? all.EMAIL, password: all.QUASAR_E2E_PASSWORD ?? all.PASSWORD };
}

/** The API as the signed-in user: their own bearer, never an admin token. */
export function makeApi(baseUrl, creds, { fetchFn = fetch, sleep = (ms) => new Promise((r) => setTimeout(r, ms)) } = {}) {
  let token = null;
  const call = async (method, p, body) => {
    if (!token) {
      const r = await fetchFn(`${baseUrl}/v1/auth/login`, { method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify(creds) });
      token = (await r.json().catch(() => ({}))).access_token;
      if (!token) throw new Error(`API sign-in failed (${r.status})`);
    }
    const r = await fetchFn(baseUrl + p, { method, headers: { authorization: `Bearer ${token}`, "content-type": "application/json" }, body: body && JSON.stringify(body) });
    return { status: r.status, body: await r.json().catch(() => ({})) };
  };
  const session = async (id) => (await call("GET", `/v1/sessions/${id}`)).body.session;
  return {
    session,
    classicLayout: async () => (await call("GET", "/v1/me/ui-preferences")).body.classic_layout === true,
    setClassicLayout: async (on) => {
      const r = await call("PATCH", "/v1/me/ui-preferences", { classic_layout: on });
      if (r.status !== 200) throw new Error(`PATCH ui-preferences returned ${r.status}`);
    },
    apps: async () => (await call("GET", "/v1/apps")).body.items ?? [],
    liveSessions: async () => ((await call("GET", "/v1/sessions")).body.items ?? []).filter((s) => !TERMINAL.includes(s.state)),
    async stop(id) {
      const r = await call("DELETE", `/v1/sessions/${id}`);
      if (![200, 202, 204, 404].includes(r.status)) throw new Error(`DELETE session returned ${r.status}`);
      for (let i = 0; i < 30; i += 1) {
        const s = await session(id).catch(() => null);
        if (!s || TERMINAL.includes(s.state)) return;
        await sleep(1000);
      }
      throw new Error("the session did not reach a terminal state in 30 s");
    },
  };
}

/** What the page can tell about the client it runs on; client.json is this, builds.json takes browser and os from it. */
export async function probeClient(page, codecs) {
  return page.evaluate(async (codecs) => {
    const ua = navigator.userAgentData;
    const hi = ua ? await ua.getHighEntropyValues(["platformVersion", "architecture", "fullVersionList"]).catch(() => null) : null;
    const brand = (hi?.fullVersionList ?? ua?.brands ?? []).find((b) => /chrom/i.test(b.brand) && !/not/i.test(b.brand));
    const refresh_hz = await new Promise((resolve) => {
      let n = 0;
      let t0 = 0;
      const tick = (t) => {
        if (!t0) t0 = t;
        n += 1;
        if (t - t0 >= 1000) resolve(Math.round(((n - 1) * 1000) / (t - t0)));
        else requestAnimationFrame(tick);
      };
      requestAnimationFrame(tick);
    });
    const adapter = navigator.gpu ? await navigator.gpu.requestAdapter().catch(() => null) : null;
    let gpu = adapter?.info ? [adapter.info.vendor, adapter.info.architecture].filter(Boolean).join(" ") : null;
    if (!gpu) {
      const gl = document.createElement("canvas").getContext("webgl");
      const ext = gl?.getExtension("WEBGL_debug_renderer_info");
      gpu = ext ? gl.getParameter(ext.UNMASKED_RENDERER_WEBGL) : null;
    }
    const types = { h264: "video/H264", h265: "video/H265", av1: "video/AV1" };
    const decode = {};
    for (const c of codecs.filter((c) => types[c])) {
      const info = await navigator.mediaCapabilities?.decodingInfo({ type: "webrtc", video: { contentType: types[c], width: 1920, height: 1080, framerate: 60, bitrate: 10_000_000 } }).catch(() => null);
      decode[c] = { supported: info ? info.supported : null, hardware: info ? info.powerEfficient : null };
    }
    const bat = navigator.getBattery ? await navigator.getBattery().catch(() => null) : null;
    return {
      kind: "web",
      browser: { name: brand?.brand ?? "unknown", version: brand?.version ?? "unknown" },
      os: { name: hi?.platform ?? ua?.platform ?? navigator.platform, version: hi?.platformVersion ?? null },
      cpu: { cores: navigator.hardwareConcurrency ?? null, architecture: hi?.architecture ?? null },
      gpu: { adapter: gpu },
      displays: [{ width: screen.width, height: screen.height, refresh_hz, pixel_ratio: devicePixelRatio }],
      decode,
      power: bat ? { source: bat.charging ? "ac" : "battery", level: bat.level } : null,
      webtransport: typeof WebTransport === "function",
      webgpu: !!adapter,
      user_agent: navigator.userAgent,
      browser_epoch_ms: Date.now(),
    };
  }, codecs);
}

/**
 * The runner arms bench mode and signs in through localStorage on the stack's origin, and the page writes
 * its own keys as it runs. In attach mode that is someone else's profile: the origin's storage is read before
 * and put back exactly after. Returns the function that restores it; it never throws.
 */
export async function guardStorage(page, baseUrl, note = () => {}) {
  const origin = new URL(baseUrl).origin;
  const onOrigin = async () => { if (!page.url().startsWith(origin)) await page.goto(baseUrl, { waitUntil: "domcontentloaded" }); };
  await onOrigin();
  const before = await page.evaluate(() => Object.fromEntries(Object.keys(localStorage).map((k) => [k, localStorage.getItem(k)])));
  return async () => {
    try {
      await onOrigin();
      await page.evaluate((keep) => {
        for (const k of Object.keys(localStorage)) if (!(k in keep)) localStorage.removeItem(k);
        for (const [k, v] of Object.entries(keep)) localStorage.setItem(k, v);
      }, before);
    } catch (e) {
      note(`storage restore failed: ${e?.message ?? e}`);
    }
  };
}

const nn = (i) => String(i + 1).padStart(2, "0");

/**
 * Runs `plan` and writes the bundle into `dir`. `host` supplies the browser: newPage(), closePage(page),
 * and (optionally) `info` for builds.json. Never throws for a scenario's failure; returns the counts.
 */
export async function runSuite({ plan, dir, baseUrl, creds, host, api, thresholds, app = DEFAULT_APP, commit = "unknown", abort = new Promise(() => {}), now = Date.now, sleep = (ms) => new Promise((r) => setTimeout(r, ms)) }) {
  const suiteRunId = `e2e-${new Date(now()).toISOString().replace(/[-:]|\.\d+/g, "")}-${crypto.randomBytes(2).toString("hex")}`;
  const url = new URL(baseUrl);
  const bundle = new Recorder(dir, { now, literals: [[url.host, "<stack>"], [url.hostname, "<stack>"], [creds.email, "<user>"], [creds.password, "<credential>"]] });
  const startedAt = now();
  const racedSleep = (ms) => Promise.race([sleep(ms), abort.then(() => { throw new Error("interrupted"); })]);

  const entries = plan.scenarios.map((s, i) => ({ id: `${nn(i)}-${s.scenario}`, scenario: s.scenario, params: { transport: "webrtc", codec: "h264", profile: "1080p60", hold_s: 60, app, ...s.params } }));
  const codecs = [...new Set(entries.map((e) => e.params.codec))];

  const probePage = await host.newPage();
  let client;
  try {
    await probePage.goto(baseUrl, { waitUntil: "domcontentloaded" });
    client = await host.probeClient(probePage, codecs);
  } finally {
    await host.closePage(probePage).catch(() => {});
  }
  bundle.json("client.json", client);

  const counts = { pass: 0, fail: 0, error: 0, skipped: 0 };
  const dirs = [];
  let interrupted = false;
  abort.then(() => { interrupted = true; });
  for (const e of entries) {
    dirs.push(e.id);
    const rec = new ScenarioRecorder(bundle, e.id);
    let out;
    if (interrupted) {
      out = { status: "skipped", reason: "the run was interrupted before this scenario", criteria: [], sessions: [], clock_offset_ms: null };
    } else {
      const { default: scenario } = await import(pathToFileURL(path.join(HERE, "scenarios", `${e.scenario}.mjs`)).href);
      const page = await host.newPage();
      const sink = { consoleErrors: [], networkFailures: [] };
      let restore = async () => {};
      try {
        rec.put("console.log", "");
        attachEvidenceListeners(page, baseUrl, sink);
        page.on("console", (m) => bundle.append(`${e.id}/console.log`, `${new Date(now()).toISOString()} [${m.type()}] ${m.text()}\n`));
        page.on("pageerror", (err) => bundle.append(`${e.id}/console.log`, `${new Date(now()).toISOString()} [pageerror] ${err.message}\n`));
        restore = await guardStorage(page, baseUrl, (msg) => rec.step("storage-restore-failed", { message: msg }));
        out = await scenario.run(page, { baseUrl, params: e.params, rec, api, creds, client, thresholds, now, sleep: racedSleep });
      } catch (err) {
        out = { status: "error", reason: String(err?.message ?? err), criteria: [], sessions: [], clock_offset_ms: null };
      } finally {
        await restore();
        rec.json("failed-requests.json", { console_errors: sink.consoleErrors, network_failures: sink.networkFailures });
        await host.closePage(page).catch(() => {});
      }
    }
    counts[out.status === "pass" ? "pass" : out.status === "fail" ? "fail" : out.status === "skipped" ? "skipped" : "error"] += 1;
    rec.json("scenario.json", { id: e.id, scenario: e.scenario, params: e.params, status: out.status, ...(out.reason ? { reason: out.reason } : {}), sessions: out.sessions ?? [], clock_offset_ms: out.clock_offset_ms ?? null, criteria: out.criteria ?? [] });
    if (out.status === "skipped") rec.put("timeline.jsonl", JSON.stringify({ ts_unix_ms: now(), clock: "client", ev: "skipped", reason: out.reason }) + "\n");
  }

  const info = host.info ?? {};
  bundle.json("builds.json", {
    components: { browser: client.browser, os: client.os, web: null, control_plane: null, node_agent: null, reference_app: null, gpu_driver: null },
    unknown: Object.fromEntries(["web", "control_plane", "node_agent", "reference_app", "gpu_driver"].map((k) => [k, info[k] ?? "the host half (e2e-collect) fills this; the client cannot see it"])),
  });
  bundle.json("network.json", { path: { rtt_ms: null }, shaping: null });
  bundle.json("manifest.json", {
    format: FORMAT,
    suite_run_id: suiteRunId,
    plan: { name: plan.name ?? "plan", scenarios: entries.map(({ id, scenario, params }) => ({ id, scenario, params })) },
    started_at_ms: startedAt,
    ended_at_ms: now(),
    runner: { commit },
    halves: { client: true, host: false },
    scenarios: dirs,
    not_recorded: NOT_RECORDED,
    redacted: Object.keys(bundle.redacted),
    target: "stack",
  });
  return { counts, suiteRunId, errors: validateBundle(dir) };
}

function usage(msg) {
  console.error(`${msg}\nusage: e2e-run TARGET=<stack url> PLAN=<plan> [CDP=<endpoint>] [OUT=<dir>]; sign-in from QUASAR_E2E_EMAIL and QUASAR_E2E_PASSWORD (or QUASAR_E2E_CRED_FILE)`);
  console.log("RESULT status=error target=e2e-run bundle= pass=0 fail=0 error=0 skipped=0");
  process.exit(2);
}

async function main() {
  const args = parseArgs(process.argv.slice(2));
  const baseUrl = typeof args.target === "string" ? args.target.replace(/\/$/, "") : null;
  if (!baseUrl || typeof args.plan !== "string") return usage("TARGET and PLAN are required");
  const creds = readCreds(process.env);
  if (!creds.email || !creds.password) return usage("no sign-in: set QUASAR_E2E_EMAIL and QUASAR_E2E_PASSWORD");
  let plan;
  let thresholds;
  try {
    plan = loadPlan(args.plan);
    thresholds = readThresholds();
  } catch (e) {
    return usage(e.message);
  }

  process.env.NODE_TLS_REJECT_UNAUTHORIZED = "0"; // test stacks use self-signed certificates, as scripts/validate/runner.mjs assumes
  const { chromium } = await import("playwright").catch(() => import("playwright-core"));
  let browser;
  let context;
  if (args.cdp) {
    browser = await chromium.connectOverCDP(args.cdp);
    context = browser.contexts()[0]; // the default context: a real window, no emulation
  } else {
    browser = await chromium.launch({ executablePath: process.env.CHROME || undefined, headless: !(process.env.DISPLAY || process.env.WAYLAND_DISPLAY) });
    context = await browser.newContext({ ignoreHTTPSErrors: true, viewport: null });
  }
  const host = { newPage: () => context.newPage(), closePage: (p) => p.close(), probeClient };
  const out = path.resolve(typeof args.out === "string" ? args.out : `e2e-bundle-${Date.now()}`);
  let commit = "unknown";
  try { commit = execFileSync("git", ["rev-parse", "HEAD"], { cwd: ROOT, encoding: "utf8" }).trim(); } catch { /* not a checkout */ }

  let stopRun;
  const abort = new Promise((r) => { stopRun = r; });
  for (const sig of ["SIGINT", "SIGTERM"]) process.on(sig, () => stopRun());

  const api = makeApi(baseUrl, creds);
  let result;
  try {
    result = await runSuite({ plan, dir: out, baseUrl, creds, host, api, thresholds, app: process.env.QUASAR_E2E_APP || DEFAULT_APP, commit, abort });
  } finally {
    if (!args.cdp) await browser.close();
  }
  for (const e of result.errors) console.error(e);
  const { pass, fail, error, skipped } = result.counts;
  const line = (status) => `RESULT status=${status} target=e2e-run bundle=${out} pass=${pass} fail=${fail} error=${error} skipped=${skipped}`;
  if (result.errors.length) { console.log(line("error")); process.exit(2); }
  console.log(line(fail + error ? "failed" : "ok"));
  process.exit(fail + error ? 1 : 0);
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  main().catch((e) => { console.error(e); console.log("RESULT status=error target=e2e-run bundle= pass=0 fail=0 error=1 skipped=0"); process.exit(2); });
}
