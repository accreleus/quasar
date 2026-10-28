#!/usr/bin/env node
// Supplementary probe (NOT part of the frozen harness, never a keep criterion):
// the first open of a lazily split route from a usable library page. Same
// throttling as the harness (CPU x4, 20/5 Mbit, 40 ms), cold cache, A/B interleaved.
//
//   node first-open.mjs --a http://127.0.0.1:4901 --b http://127.0.0.1:4900 [--runs 7] [--idle 0]
//
// game:    hover a library tile for 150 ms, click it -> `.gp-title` rendered
// session: hover the tile's Play button for 150 ms, then client-side navigation to
//          /app/session/<id> (what a launch does once POST /v1/sessions answers;
//          the mock does not launch) -> `.session-root` rendered
// --idle N waits N ms on the usable library page before acting (idle-time preloads).
import { chromium } from "/w/autoresearch/harness/node_modules/playwright/index.mjs";

const arg = (n, d) => { const i = process.argv.indexOf(n); return i > 0 ? process.argv[i + 1] : d; };
const A = arg("--a"), B = arg("--b"), RUNS = Number(arg("--runs", 7)), IDLE = Number(arg("--idle", 0));
const NET = { offline: false, latency: 40, downloadThroughput: 20e6 / 8, uploadThroughput: 5e6 / 8 };
const APP_ID = "00000000-0000-4000-8000-000000000100"; // fixture "Hollow Peak"
const median = (a) => [...a].sort((x, y) => x - y)[Math.floor(a.length / 2)];

async function once(browser, base, scenario) {
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 900 }, timezoneId: "UTC", locale: "en-US" });
  const page = await ctx.newPage();
  const cdp = await ctx.newCDPSession(page);
  await cdp.send("Network.enable");
  await cdp.send("Network.emulateNetworkConditions", NET);
  await cdp.send("Emulation.setCPUThrottlingRate", { rate: 4 });
  await page.goto(base + "/app", { waitUntil: "commit" });
  await page.waitForFunction(() => document.body?.textContent?.includes("Vector Rally"), null, { timeout: 60000, polling: 50 });
  if (IDLE) await page.waitForTimeout(IDLE);
  const marker = scenario === "game" ? ".gp-title" : ".session-root";
  await page.evaluate((sel) => {
    window.__arOpened = null;
    new MutationObserver((_, o) => { if (document.querySelector(sel)) { window.__arOpened = performance.now(); o.disconnect(); } })
      .observe(document, { subtree: true, childList: true });
  }, marker);
  let t0;
  if (scenario === "game") {
    const tile = page.locator(`.lib-tile-surface[data-app-id="${APP_ID}"]`).first();
    // The tile's Play button covers its centre; the top-left corner opens the page.
    await tile.hover({ position: { x: 10, y: 10 } });
    await page.waitForTimeout(150);
    t0 = await page.evaluate(() => performance.now());
    await tile.click({ position: { x: 10, y: 10 } });
  } else {
    await page.locator(`.lib-tile-surface[data-app-id="${APP_ID}"]`).first().hover({ position: { x: 10, y: 10 } });
    await page.locator(`button.lib-tile-play[aria-label="Play Hollow Peak"]`).first().hover({ force: true });
    await page.waitForTimeout(150);
    t0 = await page.evaluate(() => { const t = performance.now(); history.pushState({}, "", "/app/session/00000000-0000-4000-8000-0000000009aa"); dispatchEvent(new PopStateEvent("popstate")); return t; });
  }
  await page.waitForFunction("window.__arOpened != null", null, { timeout: 30000, polling: 50 });
  const dt = (await page.evaluate("window.__arOpened")) - t0;
  await ctx.close();
  return Math.round(dt);
}

const browser = await chromium.launch();
const out = {};
for (const scenario of ["game", "session"]) {
  const a = [], b = [];
  for (let i = 0; i < RUNS; i++) { a.push(await once(browser, A, scenario)); b.push(await once(browser, B, scenario)); }
  out[scenario] = { a: median(a), b: median(b), a_runs: a, b_runs: b };
}
await browser.close();
console.log(JSON.stringify({ idle: IDLE, ...out }));
