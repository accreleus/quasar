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
export const DEFAULT_APP = "Quasar Benchapp";
const LABEL = /^[A-Za-z0-9_.-]{1,64}$/;

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
export async function runSuite({ plan, dir, baseUrl, creds, host, api, thresholds, label = null, insecureTls = false, secrets = new Set(), app = DEFAULT_APP, commit = "unknown", abort = new Promise(() => {}), now = Date.now, sleep = (ms) => new Promise((r) => setTimeout(r, ms)) }) {
  if (fs.existsSync(dir)) throw new Error(`${dir} already exists: a bundle is never written over another`);
  const suiteRunId = `e2e-${new Date(now()).toISOString().replace(/[-:]|\.\d+/g, "")}-${crypto.randomBytes(2).toString("hex")}`;
  const staging = path.join(path.dirname(path.resolve(dir)), `.${path.basename(dir)}.staging-${crypto.randomBytes(4).toString("hex")}`);
  let committed = false;
  try {
    const url = new URL(baseUrl);
    const bundle = new Recorder(staging, { now, secrets, literals: [[url.host, "<stack>"], [url.hostname, "<stack>"], [creds.email, "<user>"], [creds.password, "<credential>"]] });
    const startedAt = now();
    const racedSleep = (ms) => Promise.race([sleep(ms), abort.then(() => { throw new Error("interrupted"); })]);
    const clock = { offset: null }; // the browser's clock minus the runner's, measured once around a round trip

    const entries = plan.scenarios.map((raw, i) => {
      const s = raw && typeof raw === "object" ? raw : {};
      const params = { transport: "webrtc", codec: "h264", profile: "1080p60", hold_s: 90, app, ...(s.params && typeof s.params === "object" ? s.params : {}) };
      return { id: `${nn(i)}-${String(s.scenario).replace(/[^\w.-]/g, "_").slice(0, 40)}`, scenario: s.scenario, params };
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
        try {
          const scenario = await loadScenario(e.scenario);
          const bad = scenario.check?.(e.params);
          if (bad) throw new Error(`the plan cannot be run: ${bad}`);
          page = await host.newPage();
          rec.put("console.log", "");
          attachEvidenceListeners(page, baseUrl, sink);
          page.on("console", (m) => bundle.append(`${e.id}/console.log`, `${stamp()} [${m.type()}] ${m.text()}\n`));
          page.on("pageerror", (err) => bundle.append(`${e.id}/console.log`, `${stamp()} [pageerror] ${err.message}\n`));
          // The page signs in on its own; its bearer is a secret of this run, whatever the page does with it afterwards.
          page.on("response", (r) => {
            if (r.request().method() === "POST" && new URL(r.url()).pathname === "/v1/auth/login" && r.status() === 200) {
              r.json().then((b) => { if (typeof b?.access_token === "string") secrets.add(b.access_token); }).catch(() => {});
            }
          });
          out = await scenario.run(page, { baseUrl, params: e.params, rec, api, creds, client, thresholds, secrets, now, sleep: racedSleep, clockOffset: () => clock.offset });
        } catch (err) {
          out = { status: "error", reason: String(err?.message ?? err), criteria: [], sessions: [], clock_offset_ms: null };
        } finally {
          rec.json("failed-requests.json", { console_errors: sink.consoleErrors, network_failures: sink.networkFailures });
          if (page) await host.closePage(page).catch((err) => { left = `a page it opened would not close (${err?.message ?? err})`; });
        }
        const cleanup = [...(out.cleanup ?? []), ...(left ? [left] : [])];
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
      unknown: Object.fromEntries(["web", "control_plane", "node_agent", "reference_app", "gpu_driver"].map((k) => [k, info[k] ?? "the host half (e2e-collect) fills this; the client cannot see it"])),
    });
    bundle.json("network.json", { path: { rtt_ms: null }, shaping: null, tls_verification: insecureTls ? "off (INSECURE_TLS=1)" : "on" });
    bundle.json("manifest.json", {
      format: FORMAT,
      suite_run_id: suiteRunId,
      plan: { name: plan.name ?? "plan", scenarios: entries.map(({ id, scenario, params }) => ({ id, scenario: String(scenario), params })) },
      started_at_ms: startedAt,
      ended_at_ms: now(),
      runner: { commit },
      halves: { client: true, host: false },
      scenarios: dirs,
      not_recorded: NOT_RECORDED,
      redacted: Object.keys(bundle.redacted),
      target: "stack",
    });
    const errors = validateBundle(staging);
    if (!errors.length) { fs.renameSync(staging, dir); committed = true; }
    return { counts, suiteRunId, errors, bundle: committed ? dir : null };
  } finally {
    if (!committed) fs.rmSync(staging, { recursive: true, force: true });
  }
}

/** Certificate checks stay on unless the caller asks (INSECURE_TLS=1, for a stack with a self-signed certificate): the process-wide switch Node's fetch has. */
export function configureTls(insecure, env = process.env) {
  if (insecure) env.NODE_TLS_REJECT_UNAUTHORIZED = "0";
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

  const insecureTls = process.env.INSECURE_TLS === "1";
  configureTls(insecureTls);
  const { chromium } = await import("playwright").catch(() => import("playwright-core"));
  const browser = args.cdp
    ? await chromium.connectOverCDP(args.cdp)
    : await chromium.launch({ executablePath: process.env.CHROME || undefined, headless: !(process.env.DISPLAY || process.env.WAYLAND_DISPLAY) });
  const out = path.resolve(typeof args.out === "string" ? args.out : `e2e-bundle-${Date.now()}`);
  let commit = "unknown";
  try { commit = execFileSync("git", ["rev-parse", "HEAD"], { cwd: ROOT, encoding: "utf8", env: { PATH: process.env.PATH ?? "", HOME: process.env.HOME ?? "" } }).trim(); } catch { /* not a checkout */ }

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
    result = await runSuite({ plan, dir: out, baseUrl, creds, host: opened.host, api, thresholds, label, insecureTls, secrets, app: process.env.QUASAR_E2E_APP || DEFAULT_APP, commit, abort });
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
