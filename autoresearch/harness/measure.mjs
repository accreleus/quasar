#!/usr/bin/env node
// Frozen autoresearch measurement. Runs inside mcr.microsoft.com/playwright:v1.56.0-noble
// against server.mjs (already listening on BASE). Writes one JSON result.
//
//   node measure.mjs --dist /w/web/dist --out out/result.json [--runs 7] [--only perf|polish|shots]
//
// Metrics (see ../program.md for the method):
//   bytes.<page>   gzip-6 bytes (the control plane's level) of every /assets JS+CSS file the browser requested
//                  before the page's usable marker fired (unthrottled, cold cache).
//   entry_static   gzip bytes of the manifest entry + its static imports + its CSS.
//   ttu.<page>     time to usable (ms from navigation start), CPU x4 + NET profile,
//                  cold cache, median of --runs.
//   shots          PNGs written to out/shots/ for the pixel-diff guard (compare.mjs).
//   polish         axe-core violation nodes inside the Region/Input scopes, per
//                  page x theme x width; impeccable detect primary findings per URL.
import fs from "node:fs";
import path from "node:path";
import zlib from "node:zlib";
import { execFileSync } from "node:child_process";
import { chromium } from "playwright";
import { AxeBuilder } from "@axe-core/playwright";

const arg = (n, d) => { const i = process.argv.indexOf(n); return i > 0 ? process.argv[i + 1] : d; };
const DIST = arg("--dist", "/w/web/dist");
const OUT = arg("--out", "out/result.json");
const RUNS = Number(arg("--runs", 7));
const ONLY = arg("--only", "");
const BASE = arg("--base", "http://127.0.0.1:4900");
const IMPECCABLE = arg("--impeccable", "");
// A/B timing: when set, every throttled candidate load is followed by one of the
// reference build (served there), so both medians see the same machine load.
const REF_BASE = arg("--ref-base", "");
const NOW = new Date("2026-10-28T12:00:00Z");

// Fixed network profile: 20 Mbit/s down, 5 up, 40 ms RTT. CPU x4.
const NET = { offline: false, latency: 40, downloadThroughput: (20e6 / 8), uploadThroughput: (5e6 / 8) };
const CPU = 4;

// Usable = the page's own content is in the DOM (checked on every DOM mutation).
const PAGES = {
  app: { path: "/app", usable: `(() => { const t = document.body && document.body.textContent || ""; return t.includes("Vector Rally") && t.includes("Most played this week"); })()` },
  account: { path: "/app/account/region", usable: `!!(document.getElementById("region-language") && document.querySelector("#set-region .tz-pick") && document.getElementById("set-input"))` },
  settings: { path: "/admin/settings", usable: `(() => { const t = document.body && document.body.textContent || ""; return t.includes("Region defaults") && t.includes("Input defaults") && !!document.querySelector("main select"); })()` },
};

const initScript = (cond) => `(() => {
  const check = () => { try { if (window.__arUsable == null && (${cond})) { window.__arUsable = performance.now(); obs && obs.disconnect(); } } catch (_) {} };
  let obs = null;
  const start = () => { obs = new MutationObserver(check); obs.observe(document, { subtree: true, childList: true, attributes: true, characterData: true }); check(); };
  start();
})();`;

const gz = new Map();
function gzipSize(rel) {
  if (!gz.has(rel)) gz.set(rel, zlib.gzipSync(fs.readFileSync(path.join(DIST, rel)), { level: 6 }).length);
  return gz.get(rel);
}

async function load(browser, name, { throttle, viewport = { width: 1280, height: 900 }, theme, freeze, base = BASE }) {
  const ctx = await browser.newContext({ viewport, timezoneId: "UTC", locale: "en-US", colorScheme: "dark" });
  const page = await ctx.newPage();
  if (freeze) await page.clock.setFixedTime(NOW);
  await page.addInitScript(initScript(PAGES[name].usable));
  if (throttle) {
    const cdp = await ctx.newCDPSession(page);
    await cdp.send("Network.enable");
    await cdp.send("Network.emulateNetworkConditions", NET);
    await cdp.send("Emulation.setCPUThrottlingRate", { rate: CPU });
  }
  const q = theme ? `?ar_theme=${theme}` : "";
  await page.goto(base + PAGES[name].path + q, { waitUntil: "commit" });
  await page.waitForFunction("window.__arUsable != null", null, { timeout: 60000, polling: 100 });
  const ttu = await page.evaluate("window.__arUsable");
  // Every /assets JS+CSS the page started fetching before it was usable.
  const assets = await page.evaluate(`performance.getEntriesByType("resource")
    .filter((e) => e.startTime <= window.__arUsable)
    .map((e) => new URL(e.name).pathname)
    .filter((p) => /^\\/assets\\/.*\\.(js|css)$/.test(p))
    .map((p) => p.slice(1))`);
  return { ctx, page, ttu, assets };
}

const median = (a) => { const s = [...a].sort((x, y) => x - y); return s[Math.floor(s.length / 2)]; };

const result = { at: new Date().toISOString(), runs: RUNS, net: NET, cpu: CPU };
const browser = await chromium.launch();

if (!ONLY || ONLY === "perf") {
  // Static entry size from the manifest.
  const m = JSON.parse(fs.readFileSync(path.join(DIST, ".vite/manifest.json"), "utf8"));
  const entryKey = Object.keys(m).find((k) => m[k].isEntry);
  const seen = new Set();
  const walk = (k) => { if (seen.has(k)) return; seen.add(k); for (const i of m[k].imports ?? []) walk(i); };
  walk(entryKey);
  const files = new Set();
  for (const k of seen) { files.add(m[k].file); for (const c of m[k].css ?? []) files.add(c); }
  result.entry_static = [...files].reduce((s, f) => s + gzipSize(f), 0);

  result.bytes = {}; result.assets = {}; result.ttu = {}; result.ttu_runs = {};
  for (const name of Object.keys(PAGES)) {
    const { ctx, assets } = await load(browser, name, {});
    const uniq = [...new Set(assets)].sort();
    result.assets[name] = uniq;
    result.bytes[name] = uniq.reduce((s, f) => s + gzipSize(f), 0);
    await ctx.close();
  }
  if (REF_BASE) { result.ttu_ref = {}; result.ttu_ref_runs = {}; }
  for (const name of Object.keys(PAGES)) {
    const runs = [], refRuns = [];
    for (let i = 0; i < RUNS; i++) {
      const c = await load(browser, name, { throttle: true });
      runs.push(Math.round(c.ttu));
      await c.ctx.close();
      if (REF_BASE) {
        const r = await load(browser, name, { throttle: true, base: REF_BASE });
        refRuns.push(Math.round(r.ttu));
        await r.ctx.close();
      }
    }
    result.ttu_runs[name] = runs;
    result.ttu[name] = median(runs);
    if (REF_BASE) { result.ttu_ref_runs[name] = refRuns; result.ttu_ref[name] = median(refRuns); }
  }
}

if (!ONLY || ONLY === "shots") {
  const dir = path.join(path.dirname(OUT), "shots");
  fs.mkdirSync(dir, { recursive: true });
  const shots = [
    ["app", "app-desktop", { width: 1280, height: 900 }, true],
    ["account", "account-region-desktop", { width: 1280, height: 900 }, false],
    ["settings", "settings-desktop", { width: 1280, height: 900 }, false],
    ["app", "app-phone", { width: 390, height: 844 }, false],
    ["account", "account-region-phone", { width: 390, height: 844 }, false],
    ["settings", "settings-phone", { width: 390, height: 844 }, false],
  ];
  result.shots = [];
  for (const [name, file, viewport, fullPage] of shots) {
    const { ctx, page } = await load(browser, name, { viewport, freeze: true, theme: "dark" });
    await page.waitForLoadState("networkidle");
    await page.evaluate("document.fonts.ready");
    await page.waitForTimeout(600);
    await page.screenshot({ path: path.join(dir, file + ".png"), fullPage, animations: "disabled", caret: "hide" });
    result.shots.push(file + ".png");
    await ctx.close();
  }
}

if (!ONLY || ONLY === "polish") {
  // Scopes: Account Region + Input sections; Admin Settings Region + Input defaults cards.
  const markScopes = `(() => {
    for (const id of ["set-region", "set-input"]) { const e = document.getElementById(id); if (e) e.setAttribute("data-ar-scope", ""); }
    for (const t of document.querySelectorAll(".panel-title")) {
      if (t.textContent === "Region defaults" || t.textContent === "Input defaults") t.closest(".panel-head").parentElement.setAttribute("data-ar-scope", "");
    }
    return document.querySelectorAll("[data-ar-scope]").length;
  })()`;
  result.axe = {}; result.axe_rules = {};
  let axeTotal = 0;
  for (const name of ["account", "settings"]) {
    for (const theme of ["dark", "light"]) {
      for (const [vw, viewport] of [["desktop", { width: 1280, height: 900 }], ["phone", { width: 390, height: 844 }]]) {
        const { ctx, page } = await load(browser, name, { viewport, theme, freeze: true });
        await page.waitForLoadState("networkidle");
        await page.waitForTimeout(400);
        const scopes = await page.evaluate(markScopes);
        if (scopes !== 2) throw new Error(`${name}: expected 2 axe scopes, found ${scopes}`);
        const r = await new AxeBuilder({ page }).include("[data-ar-scope]").analyze();
        const key = `${name}.${theme}.${vw}`;
        result.axe[key] = r.violations.reduce((s, v) => s + v.nodes.length, 0);
        result.axe_rules[key] = r.violations.map((v) => `${v.id}:${v.nodes.length}`);
        axeTotal += result.axe[key];
        await ctx.close();
      }
    }
  }
  result.axe_total = axeTotal;

  if (IMPECCABLE) {
    result.impeccable = {}; result.impeccable_rules = {};
    let impTotal = 0;
    for (const name of ["account", "settings"]) {
      for (const theme of ["dark", "light"]) {
        for (const vp of ["1280x900", "390x844"]) {
          const url = `${BASE}${PAGES[name].path}?ar_theme=${theme}`;
          let out = "[]";
          try {
            out = execFileSync(IMPECCABLE, ["detect", "--json", "--no-advisory", "--viewport", vp, url], { cwd: path.resolve("../.."), encoding: "utf8", stdio: ["ignore", "pipe", "ignore"], timeout: 120000 });
          } catch (e) {
            if (e.status !== 2) throw new Error(`impeccable detect failed on ${url}: exit ${e.status}`);
            out = e.stdout;
          }
          const items = JSON.parse(out || "[]");
          const key = `${name}.${theme}.${vp}`;
          result.impeccable[key] = items.length;
          result.impeccable_rules[key] = items.map((i) => i.antipattern ?? i.rule ?? i.id);
          impTotal += items.length;
        }
      }
    }
    result.impeccable_total = impTotal;
    result.polish_primary = axeTotal + impTotal;
  }
}

await browser.close();
fs.mkdirSync(path.dirname(OUT), { recursive: true });
fs.writeFileSync(OUT, JSON.stringify(result, null, 2));
console.log(JSON.stringify({ entry_static: result.entry_static, bytes: result.bytes, ttu: result.ttu, ttu_runs: result.ttu_runs, axe_total: result.axe_total, impeccable_total: result.impeccable_total }, null, 1));
