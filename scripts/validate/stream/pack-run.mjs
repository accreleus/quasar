#!/usr/bin/env node
// The one entry of the e2e pack (make e2e-pack, docs/testing-e2e-suite.md): checks this machine, runs the plan, checks the bundle with the
// suite's own validator and zips it. No model, no repo, no ssh. Also runs from a checkout (node scripts/validate/stream/pack-run.mjs).
//
//   pack-run.mjs [doctor] <stack url> [<plan>] [--insecure]
//   sign-in: QUASAR_E2E_EMAIL / QUASAR_E2E_PASSWORD, QUASAR_E2E_CRED_FILE, or a signin.txt of KEY=value lines in the pack folder
//   env: CHROME (browser to use), CLIENT (a role label), QUASAR_E2E_APP (reference app)
//
// Ends with one RESULT line. Exit 0: a zip was written (full run) or every check passed (doctor); 1: a check failed or no valid bundle; 2: usage.
import { spawn, spawnSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { zip } from "./archive.mjs";
import { validateBundle } from "./bundle.mjs";
import { configureTls, DEFAULT_APP, headless, loadPlan, makeApi, probeClient, readCreds, secretTooShort } from "./run.mjs";
import { cannotRun } from "./scenarios/stream-common.mjs";

const HERE = path.dirname(fileURLToPath(import.meta.url));
const ROOT = path.resolve(HERE, "../../..");
const STRIP = ["QUASAR_E2E_EMAIL", "QUASAR_E2E_PASSWORD", "QUASAR_E2E_CRED_FILE", "NODE_TLS_REJECT_UNAUTHORIZED"];
const USAGE = "usage: pack-run.mjs [doctor] <stack url> [<plan: smoke|fallback|input>] [--insecure]\nsign-in: signin.txt in this folder (QUASAR_E2E_EMAIL=... and QUASAR_E2E_PASSWORD=... lines), or those two variables";

export function parseCli(argv) {
  const rest = argv.filter((a) => !a.startsWith("--"));
  const doctor = rest[0] === "doctor";
  if (doctor) rest.shift();
  return { help: argv.includes("--help") || argv.includes("-h"), insecure: argv.includes("--insecure"), doctor, target: rest[0]?.replace(/\/$/, ""), plan: rest[1] ?? "smoke" };
}

/** Chrome (or Chromium) on this machine: CHROME if set, else the usual places. null when none. */
export function findChrome(env = process.env, platform = process.platform, exists = fs.existsSync) {
  if (env.CHROME) return exists(env.CHROME) ? env.CHROME : null;
  const win = ["ProgramFiles", "ProgramFiles(x86)", "LOCALAPPDATA"].filter((k) => env[k]).map((k) => path.win32.join(env[k], "Google", "Chrome", "Application", "chrome.exe"));
  const mac = ["Google Chrome", "Chromium"].map((n) => `/Applications/${n}.app/Contents/MacOS/${n}`);
  const linux = ["google-chrome-stable", "google-chrome", "chromium", "chromium-browser"].flatMap((n) => (env.PATH ?? "").split(path.delimiter).filter(Boolean).map((d) => path.join(d, n)));
  return ({ win32: win, darwin: mac }[platform] ?? linux).find((p) => exists(p)) ?? null;
}

/** `env` as a child that must not see the sign-in gets it (npm on the first run). */
export function withoutSignIn(env) {
  const out = { ...env };
  for (const k of STRIP) delete out[k];
  return out;
}

/** The pack carries no node_modules: the first run installs playwright-core from the lockfile. The sign-in is not in this process' environment. */
function ensureDeps() {
  if (!fs.existsSync(path.join(ROOT, "package.json")) || fs.existsSync(path.join(ROOT, "node_modules", "playwright-core"))) return null;
  console.error("first run: fetching the one library this needs (npm ci)");
  const r = spawnSync("npm", ["ci", "--omit=dev", "--no-audit", "--no-fund"], { cwd: ROOT, env: withoutSignIn(process.env), stdio: ["ignore", "inherit", "inherit"], shell: process.platform === "win32" });
  return r.status === 0 ? null : "npm ci failed: is this machine online, and is npm installed with Node?";
}

const loadChromium = async () => (await import("playwright").catch(() => import("playwright-core"))).chromium;

/**
 * Each check says ok or why not, in words for someone who is not a developer. Returns { ok, checks: [{ name, ok, detail }] }.
 * `deps` lets a test stand in for the network and the browser.
 */
export async function doctor({ target, plan, creds, chrome, insecure, deps = {}, env = process.env, platform = process.platform }) {
  const checks = [];
  const add = (name, ok, detail) => { checks.push({ name, ok, detail }); return ok; };
  const fetchFn = deps.fetch ?? fetch;
  const major = Number(process.versions.node.split(".")[0]);
  add("node", major >= 22, major >= 22 ? `v${process.versions.node}` : `v${process.versions.node} is too old: install Node 22 or newer from https://nodejs.org`);
  add("chrome", !!chrome, chrome ?? "Google Chrome was not found: install it from https://www.google.com/chrome, or set CHROME=<its path>");
  let planDoc = null;
  try { planDoc = loadPlan(plan); add("plan", true, `${plan}: ${planDoc.scenarios.length} scenarios`); } catch (e) { add("plan", false, e.message); }

  let stackOk = false;
  try {
    const r = await fetchFn(target, { signal: AbortSignal.timeout(15000), redirect: "manual" });
    stackOk = add("stack", true, `answers (HTTP ${r.status})`);
  } catch (e) {
    const code = e.cause?.code ?? e.cause?.errors?.[0]?.code ?? e.code ?? e.message;
    const cert = /CERT|SELF_SIGNED|UNABLE_TO_VERIFY/.test(String(code));
    add("stack", false, `does not answer (${code}). ${cert ? "Its certificate is not trusted here: add --insecure if you were told it is self-made." : "Check the address, and that this machine is on the same network as the stack."}`);
  }

  let signedIn = false;
  let apps = [];
  if (!creds.email || !creds.password) add("sign-in", false, "no sign-in: make signin.txt in this folder with QUASAR_E2E_EMAIL=... and QUASAR_E2E_PASSWORD=... lines");
  else if (secretTooShort(creds)) add("sign-in", false, secretTooShort(creds));
  else if (stackOk) {
    try {
      apps = await makeApi(target, creds, { fetchFn }).apps();
      signedIn = add("sign-in", true, "accepted");
    } catch (e) { add("sign-in", false, `${e.message}. Check the e-mail and password you were given.`); }
  }
  const app = env.QUASAR_E2E_APP || DEFAULT_APP;
  if (signedIn) add("library", apps.some((a) => a.name === app), apps.some((a) => a.name === app) ? `"${app}" is there` : `"${app}" is not in this user's library: ask whoever gave you the sign-in`);

  if (chrome && stackOk) {
    let browser = null;
    let page = null;
    try {
      const chromium = await (deps.loadChromium ?? loadChromium)();
      browser = await chromium.launch({ executablePath: chrome, headless: headless(env, platform) });
      page = await (await browser.newContext({ ignoreHTTPSErrors: insecure })).newPage();
      await page.goto(target, { timeout: 30000 });
      add("browser", true, `Chrome ${browser.version()} opened the stack's page`);
    } catch (e) {
      const why = String(e.message).split("\n")[0];
      add("browser", false, platform === "darwin" && /ERR_(ADDRESS|INTERNET|NETWORK|CONNECTION)|timeout/i.test(why)
        ? `Chrome could not reach the stack although this program can (${why}). On a Mac this is almost always the Local Network permission: System Settings > Privacy & Security > Local Network > allow Google Chrome, then run again.`
        : `Chrome could not open the stack's page (${why})`);
    }
    if (page && planDoc) {
      try {
        const client = await probeClient(page);
        const skips = (planDoc?.scenarios ?? []).map((s, i) => [i + 1, cannotRun(s.params ?? {}, client)]).filter(([, why]) => why);
        add("decoders", skips.length < (planDoc?.scenarios.length ?? 0), skips.length ? skips.map(([n, why]) => `scenario ${n} will be skipped: ${why}`).join("; ") : "this Chrome can play what the plan asks");
      } catch (e) { add("decoders", false, `the client probe failed (${String(e.message).split("\n")[0]})`); }
    }
    await browser?.close().catch(() => {});
  }
  return { ok: checks.every((c) => c.ok), checks };
}

/** Every file under `dir` as zip members under the folder's own name; a symlink or anything else that is not a plain file stops it. */
export function zipBundle(dir, outFile, date = new Date()) {
  const top = path.basename(dir);
  const members = [];
  const walk = (d) => {
    for (const e of fs.readdirSync(d, { withFileTypes: true })) {
      const p = path.join(d, e.name);
      if (e.isDirectory()) walk(p);
      else if (e.isFile()) members.push({ name: `${top}/${path.relative(dir, p).split(path.sep).join("/")}`, data: fs.readFileSync(p) });
      else throw new Error(`${p} is not a plain file`);
    }
  };
  walk(dir);
  fs.writeFileSync(outFile, zip(members.sort((a, b) => (a.name < b.name ? -1 : 1)), { date }));
  return members.length;
}

/** Zips a valid bundle. Null when the zip is on disk; otherwise why not. */
export function sendBack(bundle, file, zipFn = zipBundle) {
  try { zipFn(bundle, file); } catch (e) { return `the bundle could not be zipped (${e.message})`; }
  return fs.existsSync(file) ? null : "no zip was written";
}

const result = (status, extra) => console.log(`RESULT status=${status} target=e2e-pack ${extra}`);

async function main() {
  const cli = parseCli(process.argv.slice(2));
  if (cli.help) { console.log(USAGE); return 0; }
  if (!cli.target) { console.error(USAGE); result("error", "zip="); return 2; }
  const label = process.env.CLIENT || null;
  if (label !== null && !/^[A-Za-z0-9_.-]{1,64}$/.test(label)) { console.error("CLIENT must be a plain identifier (letters, digits, _ . -)"); result("error", "zip="); return 2; }
  const insecure = configureTls(cli.insecure || process.env.INSECURE_TLS === "1");
  const signin = path.join(ROOT, "signin.txt");
  if (!process.env.QUASAR_E2E_EMAIL && !process.env.QUASAR_E2E_CRED_FILE && fs.existsSync(signin)) process.env.QUASAR_E2E_CRED_FILE = signin;
  const failed = ensureDeps();
  if (failed) { console.error(failed); result("failed", "zip= reason=install"); return 1; }
  // The sign-in lives in this process and the runner's child only: Chrome and npm never see it.
  const creds = readCreds(process.env);
  for (const k of STRIP.slice(0, 3)) delete process.env[k];

  const chrome = findChrome();
  const d = await doctor({ target: cli.target, plan: cli.plan, creds, chrome, insecure });
  for (const c of d.checks) console.log(`${c.ok ? "ok  " : "FAIL"} ${c.name}: ${c.detail}`);
  if (!d.ok) { result("failed", `zip= reason=doctor`); return 1; }
  if (cli.doctor) { result("ok", "zip= doctor=ok"); return 0; }

  const stamp = new Date().toISOString().replace(/[:.]/g, "-").slice(0, 19);
  const results = path.join(ROOT, "results");
  fs.mkdirSync(results, { recursive: true });
  const bundle = path.join(results, `bundle-${stamp}`);
  const env = { ...process.env, CHROME: chrome, QUASAR_E2E_EMAIL: creds.email, QUASAR_E2E_PASSWORD: creds.password, INSECURE_TLS: insecure ? "1" : "0" };
  delete env.NODE_TLS_REJECT_UNAUTHORIZED;
  const child = spawn(process.execPath, [path.join(HERE, "run.mjs"), "--target", cli.target, "--plan", cli.plan, "--out", bundle], { env, stdio: ["ignore", "pipe", "inherit"] });
  let out = "";
  child.stdout.on("data", (b) => { out += b; process.stdout.write(b); });
  const code = await new Promise((r) => child.on("close", (c) => r(c ?? 1)));
  const counts = /RESULT status=(\w+) .*pass=(\d+) fail=(\d+) error=(\d+) skipped=(\d+)/.exec(out);
  const line = counts ? `run=${counts[1]} pass=${counts[2]} fail=${counts[3]} error=${counts[4]} skipped=${counts[5]}` : "run=unknown";
  if (code > 1 || !fs.existsSync(bundle)) { result("failed", `zip= reason=no-bundle ${line}`); return 1; }
  const problems = validateBundle(bundle);
  if (problems.length) { for (const p of problems) console.error(p); result("failed", `zip= reason=bundle-invalid ${line}`); return 1; }
  const file = path.join(results, `quasar-e2e-${stamp}.zip`);
  const bad = sendBack(bundle, file);
  if (bad) { console.error(bad); result("failed", `zip= reason=no-zip ${line}`); return 1; }
  console.log(`\nSend back this one file:\n  ${file}`);
  result("ok", `zip=${file} ${line}`);
  return 0;
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  main().then((c) => process.exit(c), (e) => { console.error(e); result("error", "zip="); process.exit(2); });
}
