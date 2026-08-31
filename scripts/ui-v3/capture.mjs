#!/usr/bin/env node
// scripts/ui-v3/capture.mjs — the driver behind scripts/ui-v3/capture.sh.
//
// Talks raw CDP (node 22's global WebSocket, no dependencies — playwright is
// NOT bootstrapped in this tree) to a Chrome that capture.sh already launched
// with the flags the v3 glass surfaces need (no --disable-gpu). Per route it
// seeds localStorage, navigates, settles, runs the route's actions, captures a
// 1440x900 PNG (plus a full-page PNG when the document is taller), and records
// console errors / failed requests.
//
// Usage (normally via capture.sh):
//   node capture.mjs --cdp http://127.0.0.1:PORT --out DIR --mode app \
//     --base http://127.0.0.1:PORT --api http://127.0.0.1:BRIDGE --token-file FILE
//   node capture.mjs --cdp ... --out DIR --mode mock --mock-dir design_handoff_v3/screens
//   node capture.mjs --cdp ... --out DIR --mode session --base https://host:8443 \
//     --api http://127.0.0.1:BRIDGE --token-file FILE --app "Quasar Bench: Ball"

import fs from "node:fs";
import path from "node:path";
import { setTimeout as sleep } from "node:timers/promises";

/* ------------------------------------------------------------------ args */

function arg(name, fallback = "") {
  const i = process.argv.indexOf(name);
  return i >= 0 && i + 1 < process.argv.length ? process.argv[i + 1] : fallback;
}
const CDP = arg("--cdp", "http://127.0.0.1:9222");
const OUT = arg("--out", "/tmp/ui-v3-capture");
const MODE = arg("--mode", "app"); // app | mock
const BASE = arg("--base", "").replace(/\/$/, "");
const API = arg("--api", "").replace(/\/$/, "");
const TOKEN_FILE = arg("--token-file", "");
const MOCK_DIR = arg("--mock-dir", "");
const ROUTES_FILE = arg("--routes", path.join(path.dirname(new URL(import.meta.url).pathname), "routes.json"));
const ONLY = arg("--only", "");
const WIDTH = Number(arg("--width", "1440"));
const HEIGHT = Number(arg("--height", "900"));
const SETTLE = Number(arg("--settle", "900")); // ms after load before capture
// --mode session only: which app to launch, by id or by (case-insensitive)
// name substring. Empty = the first app the catalogue offers.
const APP = arg("--app", "");

/* ------------------------------------------------------- minimal CDP client */

class Cdp {
  constructor(ws) {
    this.ws = ws;
    this.id = 0;
    this.pending = new Map();
    this.handlers = new Map();
    ws.addEventListener("message", (ev) => {
      const msg = JSON.parse(ev.data);
      if (msg.id !== undefined) {
        const p = this.pending.get(msg.id);
        if (!p) return;
        this.pending.delete(msg.id);
        msg.error ? p.reject(new Error(`${msg.error.message} (${JSON.stringify(msg.error)})`)) : p.resolve(msg.result);
        return;
      }
      for (const cb of this.handlers.get(msg.method) ?? []) cb(msg.params);
    });
  }

  static async attach(cdpBase) {
    // The page target chrome opens on start-up. /json/list is the only
    // discovery endpoint that returns a page-level websocket URL.
    for (let i = 0; i < 50; i++) {
      try {
        const list = await (await fetch(`${cdpBase}/json/list`)).json();
        const page = list.find((t) => t.type === "page");
        if (page?.webSocketDebuggerUrl) {
          const ws = new WebSocket(page.webSocketDebuggerUrl);
          await new Promise((res, rej) => {
            ws.addEventListener("open", res, { once: true });
            ws.addEventListener("error", rej, { once: true });
          });
          return new Cdp(ws);
        }
      } catch {
        /* chrome still starting */
      }
      await sleep(200);
    }
    throw new Error(`no page target on ${cdpBase} after 10s`);
  }

  send(method, params = {}) {
    const id = ++this.id;
    this.ws.send(JSON.stringify({ id, method, params }));
    return new Promise((resolve, reject) => {
      this.pending.set(id, { resolve, reject });
      setTimeout(() => {
        if (this.pending.delete(id)) reject(new Error(`${method} timed out`));
      }, 25_000);
    });
  }

  on(event, cb) {
    if (!this.handlers.has(event)) this.handlers.set(event, []);
    this.handlers.get(event).push(cb);
  }

  async eval(expression, { awaitPromise = false } = {}) {
    const r = await this.send("Runtime.evaluate", {
      expression,
      returnByValue: true,
      awaitPromise,
      userGesture: true,
    });
    if (r.exceptionDetails) throw new Error(r.exceptionDetails.exception?.description ?? "evaluate threw");
    return r.result?.value;
  }

  async waitFor(expression, timeoutMs = 12_000, everyMs = 120) {
    const deadline = Date.now() + timeoutMs;
    for (;;) {
      try {
        if (await this.eval(`Boolean(${expression})`)) return true;
      } catch {
        /* mid-navigation; retry */
      }
      if (Date.now() > deadline) return false;
      await sleep(everyMs);
    }
  }
}

/* --------------------------------------------------------------- helpers */

const CLICK_TEXT = (label) => `(() => {
  const want = ${JSON.stringify(label)}.toLowerCase();
  const nodes = [...document.querySelectorAll('button, a, [role="button"], [role="tab"], summary')];
  const hit = nodes.find(n => (n.textContent || '').trim().toLowerCase().includes(want)
    || (n.getAttribute('aria-label') || '').toLowerCase().includes(want));
  if (!hit) return false;
  hit.scrollIntoView({ block: 'center' });
  hit.click();
  return true;
})()`;

const CLICK_SEL = (sel) => `(() => {
  const hit = document.querySelector(${JSON.stringify(sel)});
  if (!hit) return false;
  hit.scrollIntoView({ block: 'center' });
  hit.click();
  return true;
})()`;

// "Still loading" = a ResourceStates loading line (p.muted[role=status]) or a
// route suspense skeleton is on screen. Captured mid-load screenshots are the
// exact defect class this pass hunts, so the driver waits them out and records
// when it could not.
const LOADING_EXPR = `(() => {
  const status = [...document.querySelectorAll('[role="status"]')]
    .some(n => /loading|working/i.test(n.textContent || ''));
  const skeleton = document.querySelector('.route-skeleton, .skeleton, [data-loading="true"]');
  return status || Boolean(skeleton);
})()`;

function log(...m) {
  console.log(...m);
}

/* ------------------------------------------------------------------- main */

const routesAll = JSON.parse(fs.readFileSync(ROUTES_FILE, "utf8"));
let routes = routesAll[MODE] ?? [];
if (ONLY) {
  const re = new RegExp(ONLY);
  routes = routes.filter((r) => re.test(r.id));
}

const shotDir = path.join(OUT, MODE);
fs.mkdirSync(shotDir, { recursive: true });

let cdp = await Cdp.attach(CDP);
async function enableDomains() {
  await cdp.send("Page.enable");
// --window-size includes the OS window frame even in headless, so the viewport
// lands short of the flag (1440x757 for 1440,900). Pin it instead of trusting it.
  await cdp.send("Emulation.setDeviceMetricsOverride", {
    width: WIDTH,
    height: HEIGHT,
    deviceScaleFactor: 1,
    mobile: false,
  });
  await cdp.send("Runtime.enable");
  await cdp.send("Log.enable");
  await cdp.send("Network.enable");
}

// Console + network diagnostics land in the bucket for the route in flight.
let bucket = { console: [], failures: [] };
const text = (a) => (a.value !== undefined ? String(a.value) : (a.description ?? a.unserializableValue ?? a.type));
// react-router v6 prints two future-flag warnings on every page; they say
// nothing about this UI.
const NOISE = /React Router Future Flag Warning/;
let inflight = 0;
let lastActivity = Date.now();
const settleRequest = () => {
  inflight = Math.max(0, inflight - 1);
  lastActivity = Date.now();
};

/** Resolves when nothing has been in flight for `quietMs`, or on timeout. */
async function networkQuiet(quietMs = 500, timeoutMs = 10_000) {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    if (inflight <= 0 && Date.now() - lastActivity > quietMs) return true;
    if (Date.now() > deadline) return false;
    await sleep(120);
  }
}

function bindHandlers() {
  cdp.on("Runtime.consoleAPICalled", (p) => {
    if (!["error", "warning", "assert"].includes(p.type)) return;
    if (NOISE.test((p.args ?? []).map(text).join(" "))) return;
    bucket.console.push({ level: p.type, text: (p.args ?? []).map(text).join(" ").slice(0, 500) });
  });
  cdp.on("Runtime.exceptionThrown", (p) => {
    bucket.console.push({
      level: "exception",
      text: (p.exceptionDetails?.exception?.description ?? p.exceptionDetails?.text ?? "exception").slice(0, 800),
    });
  });
  cdp.on("Log.entryAdded", (p) => {
    if (p.entry?.level !== "error") return;
    bucket.console.push({ level: "log", text: `${p.entry.text} ${p.entry.url ?? ""}`.slice(0, 500) });
  });
  cdp.on("Network.requestWillBeSent", () => {
    inflight++;
    lastActivity = Date.now();
  });
  cdp.on("Network.loadingFinished", settleRequest);
  cdp.on("Network.loadingFailed", settleRequest);
  cdp.on("Network.responseReceived", (p) => {
    if ((p.response?.status ?? 0) >= 400) bucket.failures.push({ status: p.response.status, url: p.response.url });
  });
  cdp.on("Network.loadingFailed", (p) => {
    if (!p.canceled) bucket.failures.push({ status: "net", url: p.errorText ?? "loadingFailed" });
  });
}

let seedScriptId = null;

await enableDomains();
bindHandlers();

/** A page that wedges (an infinite render loop) makes every later CDP call time
 *  out, which would lose the rest of the pass. Open a fresh target instead. */
async function recoverSession() {
  try {
    cdp.ws.close();
  } catch {
    /* already gone */
  }
  await fetch(`${CDP}/json/new?about:blank`, { method: "PUT" }).catch(() => {});
  cdp = await Cdp.attach(CDP);
  await enableDomains();
  bindHandlers();
  seedScriptId = null;
  inflight = 0;
}

/* ---- placeholder resolution (app mode): real ids off the control plane ---- */

const ids = {};
let token = "";
const NEEDS_TOKEN = MODE === "app" || MODE === "session";
const get = async (p) => {
  const r = await fetch(`${API}${p}`, { headers: { authorization: `Bearer ${token}` } });
  if (!r.ok) return null;
  return r.json();
};
if (NEEDS_TOKEN) token = fs.readFileSync(TOKEN_FILE, "utf8").trim();
if (MODE === "app") {
  const hosts = await get("/v1/hosts");
  ids.hostId = hosts?.items?.[0]?.id ?? "";
  const apps = await get("/v1/admin/apps");
  ids.appId = apps?.items?.[0]?.id ?? "";
  const sessions = await get("/v1/admin/sessions");
  ids.sessionId = sessions?.items?.[0]?.id ?? "";
  const images = await get("/v1/admin/images");
  ids.imageId = images?.images?.[0]?.id ?? images?.images?.[0]?.name ?? "";
  log("resolved ids:", JSON.stringify(ids));
}

const me = NEEDS_TOKEN ? await (await fetch(`${API}/v1/me`, { headers: { authorization: `Bearer ${token}` } })).json() : null;
const expiresAt = new Date(Date.now() + 12 * 3600 * 1000).toISOString();

async function seedStorage(route) {
  // Injected before every document of the next navigation, so the SPA boots
  // already authenticated. The alternative (navigate somewhere same-origin,
  // write localStorage, navigate again) doubled the page loads per route.
  if (seedScriptId) {
    const stale = seedScriptId;
    seedScriptId = null;
    await cdp.send("Page.removeScriptToEvaluateOnNewDocument", { identifier: stale }).catch(() => {});
  }
  const extra = JSON.stringify(route.storage ?? {});
  const auth =
    route.auth === false
      ? "localStorage.removeItem('quasar.auth.token');localStorage.removeItem('quasar.auth.expires_at');localStorage.removeItem('quasar.auth.user');sessionStorage.clear();"
      : `localStorage.setItem('quasar.auth.token', ${JSON.stringify(token)});
         localStorage.setItem('quasar.auth.expires_at', ${JSON.stringify(expiresAt)});
         localStorage.setItem('quasar.auth.user', ${JSON.stringify(JSON.stringify(me))});`;
  const source = `(() => { try {
    localStorage.removeItem('quasar-theme');
    localStorage.removeItem('quasar-density');
    localStorage.removeItem('quasar-rail');
    ${auth}
    for (const [k, v] of Object.entries(${extra})) localStorage.setItem(k, v);
  } catch (e) { /* opaque origin */ } })()`;
  const r = await cdp.send("Page.addScriptToEvaluateOnNewDocument", { source });
  seedScriptId = r.identifier;
}

/** Clears the mock's own preference keys so each mock route renders from the
 *  same baseline (dark, comfortable, expanded). */
async function seedMockPrefs() {
  if (seedScriptId) {
    const stale = seedScriptId;
    seedScriptId = null;
    await cdp.send("Page.removeScriptToEvaluateOnNewDocument", { identifier: stale }).catch(() => {});
  }
  const r = await cdp.send("Page.addScriptToEvaluateOnNewDocument", {
    source: `(() => { try {
      for (const k of ['quasar-v3-theme','quasar-v2-density','quasar-v2-rail','quasar-v2-route']) localStorage.removeItem(k);
    } catch (e) {} })()`,
  });
  seedScriptId = r.identifier;
}

async function runActions(route) {
  for (const step of route.actions ?? []) {
    if (step.wait) await sleep(step.wait);
    if (step.waitFor) {
      const ok = await cdp.waitFor(`document.querySelector(${JSON.stringify(step.waitFor)})`, 10_000);
      if (!ok) return `waitFor ${step.waitFor} never matched`;
    }
    if (step.click) {
      const ok = await cdp.eval(CLICK_SEL(step.click));
      if (!ok) return `click ${step.click} matched nothing`;
      await sleep(350);
    }
    if (step.clickText) {
      const ok = await cdp.eval(CLICK_TEXT(step.clickText));
      if (!ok) return `clickText "${step.clickText}" matched nothing`;
      await sleep(350);
    }
    if (step.eval) await cdp.eval(step.eval);
  }
  return null;
}

async function shoot(id) {
  let png;
  for (let i = 0; ; i++) {
    try {
      png = await cdp.send("Page.captureScreenshot", { format: "png", captureBeyondViewport: false });
      break;
    } catch (err) {
      // "Internal error" here is a renderer still painting; one retry clears it.
      if (i >= 2) throw err;
      await sleep(800);
    }
  }
  fs.writeFileSync(path.join(shotDir, `${id}.png`), Buffer.from(png.data, "base64"));
  // The v3 shell scrolls `.main`, not the document, so Page.getLayoutMetrics
  // reports the viewport height for every console page. Ask the DOM for the
  // tallest scroller instead, then take the long shot by growing the emulated
  // viewport (a clip past the viewport captures nothing when an inner element
  // owns the scroll).
  const h = await cdp
    .eval(`Math.max(document.documentElement.scrollHeight, document.body.scrollHeight,
      ...[...document.querySelectorAll('.main, .page, .app')].map(e => e.scrollHeight))`)
    .catch(() => 0);
  if (h > HEIGHT + 24) {
    try {
      await cdp.send("Emulation.setDeviceMetricsOverride", {
        width: WIDTH,
        height: Math.min(Math.ceil(h), 6000),
        deviceScaleFactor: 1,
        mobile: false,
      });
      await sleep(400);
      const full = await cdp.send("Page.captureScreenshot", { format: "png", captureBeyondViewport: false });
      fs.writeFileSync(path.join(shotDir, `${id}.full.png`), Buffer.from(full.data, "base64"));
    } catch {
      /* the viewport shot is the one that matters */
    }
    await cdp.send("Emulation.setDeviceMetricsOverride", {
      width: WIDTH,
      height: HEIGHT,
      deviceScaleFactor: 1,
      mobile: false,
    });
    await sleep(200);
  }
  return h;
}

/* ============================== session mode ==============================
 * `--mode session` is the live half of the pass: it launches a real session
 * through the SPA's own Play button, screenshots the loader while it is
 * actually establishing (one frame per distinct phase word), proves the
 * WebRTC path with `RTCPeerConnection.getStats()`, walks the HUD (panes,
 * docks, content presets, hidden), ends the session through the HUD's exit
 * and captures the summary card the home page renders on the way out.
 *
 * Nothing here simulates: every screenshot is of a page attached to a running
 * node-agent session. The only synthetic input is CDP key/mouse dispatch.
 */

/** Modifier bitmask CDP wants on Input.dispatchKeyEvent. */
const MOD = { alt: 1, ctrl: 2, meta: 4, shift: 8 };

/** One key press (down + up) with the modifiers a real keyboard would carry. */
async function pressKey(key, { code, vk, modifiers = 0, text } = {}) {
  const base = {
    key,
    code: code ?? key,
    windowsVirtualKeyCode: vk ?? 0,
    nativeVirtualKeyCode: vk ?? 0,
    modifiers,
  };
  await cdp.send("Input.dispatchKeyEvent", {
    ...base,
    type: text ? "keyDown" : "rawKeyDown",
    ...(text ? { text } : {}),
  });
  await cdp.send("Input.dispatchKeyEvent", { ...base, type: "keyUp" });
}

/** Screenshot straight to `dir/<id>.png`. Separate from `shoot()` because the
 *  session pass never wants the full-page variant (the stage is viewport-sized
 *  by construction) and must not resize the viewport mid-stream — a device
 *  metrics override during a live session restarts the video layout. */
async function snap(dir, id) {
  const png = await cdp.send("Page.captureScreenshot", { format: "png", captureBeyondViewport: false });
  fs.writeFileSync(path.join(dir, `${id}.png`), Buffer.from(png.data, "base64"));
  return `${id}.png`;
}

/** Wraps RTCPeerConnection so the driver can reach the live peer connections
 *  from `Runtime.evaluate`. Installed as an on-new-document script, so it is in
 *  place before the SPA's first module runs — the runtime constructs its peer
 *  connection during mount and there is no other handle to it. */
const PC_SHIM = `(() => { try {
  const Orig = window.RTCPeerConnection;
  if (!Orig || Orig.__quasarWrapped) return;
  const Wrapped = function (...a) {
    const pc = new Orig(...a);
    (window.__quasarPCs = window.__quasarPCs || []).push(pc);
    return pc;
  };
  Wrapped.prototype = Orig.prototype;
  Wrapped.__quasarWrapped = true;
  if (Orig.generateCertificate) Wrapped.generateCertificate = Orig.generateCertificate.bind(Orig);
  window.RTCPeerConnection = Wrapped;
  window.webkitRTCPeerConnection = Wrapped;
} catch (e) { /* no WebRTC in this context */ } })()`;

/** inbound video stats off every peer connection the page built. */
const STATS_EXPR = `(async () => {
  const pcs = window.__quasarPCs || [];
  const out = [];
  for (const pc of pcs) {
    const codecs = {}, rows = [], pairs = [];
    let report;
    try { report = await pc.getStats(); } catch (e) { continue; }
    report.forEach((r) => { if (r.type === 'codec') codecs[r.id] = r; });
    report.forEach((r) => {
      if (r.type === 'inbound-rtp') rows.push({
        kind: r.kind, framesDecoded: r.framesDecoded, framesReceived: r.framesReceived,
        framesDropped: r.framesDropped, frameWidth: r.frameWidth, frameHeight: r.frameHeight,
        framesPerSecond: r.framesPerSecond, bytesReceived: r.bytesReceived,
        packetsLost: r.packetsLost, jitter: r.jitter, decoderImplementation: r.decoderImplementation,
        mimeType: codecs[r.codecId] ? codecs[r.codecId].mimeType : null,
        sdpFmtpLine: codecs[r.codecId] ? codecs[r.codecId].sdpFmtpLine : null,
      });
      if (r.type === 'candidate-pair' && r.state === 'succeeded') pairs.push({
        currentRoundTripTime: r.currentRoundTripTime,
        availableIncomingBitrate: r.availableIncomingBitrate,
      });
    });
    out.push({
      connectionState: pc.connectionState,
      iceConnectionState: pc.iceConnectionState,
      inbound: rows,
      pairs,
    });
  }
  return out;
})()`;

/** Mean/peak luminance of a 32x18 downsample of the <video>, so the report can
 *  say whether the decoded picture was actually there (headless has no GPU, so
 *  a black frame is a plausible outcome and must be recorded, not hidden). */
const VIDEO_EXPR = `(() => {
  const v = document.querySelector('video');
  if (!v) return null;
  const out = { videoWidth: v.videoWidth, videoHeight: v.videoHeight, readyState: v.readyState,
    paused: v.paused, currentTime: v.currentTime };
  try {
    const q = v.getVideoPlaybackQuality ? v.getVideoPlaybackQuality() : null;
    if (q) { out.totalVideoFrames = q.totalVideoFrames; out.droppedVideoFrames = q.droppedVideoFrames; }
    const c = document.createElement('canvas');
    c.width = 32; c.height = 18;
    const x = c.getContext('2d');
    x.drawImage(v, 0, 0, 32, 18);
    const d = x.getImageData(0, 0, 32, 18).data;
    let sum = 0, max = 0;
    for (let i = 0; i < d.length; i += 4) {
      const l = (d[i] + d[i + 1] + d[i + 2]) / 3;
      sum += l; if (l > max) max = l;
    }
    out.meanLuma = Math.round((sum / (d.length / 4)) * 10) / 10;
    out.maxLuma = max;
  } catch (e) { out.sampleError = String(e && e.message); }
  return out;
})()`;

/** The loader's visible phase, read the way a person reads it. */
const PHASE_EXPR = `(() => {
  const root = document.querySelector('.sl-root');
  const hud = document.querySelector('.hud-root');
  const verb = document.querySelector('.sl-verb');
  const stage = document.querySelector('.sl-stage');
  const rail = [...document.querySelectorAll('.sl-step')].map(n => n.dataset.state || '');
  const stall = document.querySelector('.sl-stall-title');
  const fail = document.querySelector('.sl-fail-msg');
  return {
    loader: Boolean(root),
    hud: Boolean(hud),
    cls: root ? root.className : '',
    verb: verb ? verb.textContent.trim() : '',
    word: stage ? stage.textContent.trim() : '',
    changing: Boolean(stage && stage.classList.contains('changing')),
    rail,
    stall: stall ? stall.textContent.trim() : '',
    fail: fail ? fail.textContent.trim() : '',
    path: location.pathname,
  };
})()`;

/** Wire item sets for the named content presets, mirroring
 *  web/src/settings/overlayPreferences.ts PRESET_ITEMS. Duplicated rather than
 *  imported because this driver must run against a DEPLOYED build it cannot
 *  import from; the README records the two as a pair to keep in step. */
const PRESET_ITEMS = {
  full: { signal: true, identity: true, codec: true, metrics: true, hint: true, capture: true, exit: true, mic: true, fullscreen: true },
  minimal: { signal: true, identity: false, codec: false, metrics: false, hint: false, capture: true, exit: true, mic: true, fullscreen: false },
  metrics: { signal: false, identity: false, codec: false, metrics: true, hint: true, capture: true, exit: true, mic: true, fullscreen: true },
};

async function patchOverlayPrefs(overlay) {
  const r = await fetch(`${API}/v1/me/ui-preferences`, {
    method: "PATCH",
    headers: { authorization: `Bearer ${token}`, "content-type": "application/json" },
    body: JSON.stringify({ session_overlay: overlay }),
  });
  if (!r.ok) throw new Error(`PATCH /v1/me/ui-preferences -> ${r.status} ${await r.text()}`);
  return r.json();
}

const overlayWire = (preset, position, autoHide) => ({
  strip_preset: preset,
  strip_items: PRESET_ITEMS[preset],
  strip_position: position,
  strip_auto_hide: autoHide,
});

/** Back on a live HUD after a navigation: video element present and the HUD
 *  drawn (hidden counts — `.hud.hidden` is still in the DOM). */
async function waitForHud(timeoutMs = 180_000) {
  return cdp.waitFor(`document.querySelector('.hud-root') && !document.querySelector('.sl-root')`, timeoutMs, 250);
}

/** Poll getStats until the video inbound report's framesDecoded has advanced
 *  at least `minDelta` across two reads — the only honest proof that frames
 *  are decoding rather than merely negotiating. */
async function waitForDecode(timeoutMs = 60_000, minDelta = 5) {
  const deadline = Date.now() + timeoutMs;
  let first = null;
  for (;;) {
    const snapshot = await cdp.eval(STATS_EXPR, { awaitPromise: true }).catch(() => null);
    const video = (snapshot ?? [])
      .flatMap((p) => p.inbound.map((r) => ({ ...r, pc: p })))
      .find((r) => r.kind === "video");
    if (video && typeof video.framesDecoded === "number") {
      if (first === null) first = video.framesDecoded;
      else if (video.framesDecoded - first >= minDelta) return { ok: true, first, last: video.framesDecoded, snapshot };
    }
    if (Date.now() > deadline) return { ok: false, first, snapshot };
    await sleep(1000);
  }
}

/**
 * One launch, from the library home page's own Play button. Returns the
 * session id. `sampleLoader` turns on the phase-by-phase capture loop — only
 * the first launch of a pass wants it.
 *
 * A launch per HUD variant, rather than one session reloaded with different
 * preferences, is forced by the transport: a page reload drops the input
 * DataChannel, the agent reads a dead SCTP association as a peer disconnect
 * and ends the session (`stopped`, "peer disconnected: WebRTC data channel
 * closed" — .claude/rules/webrtc-testing.md). The preferences are only read at
 * provider mount, so a fresh page is the only way to apply one, and a fresh
 * page means a fresh session.
 */
async function launchSession(wanted, { sampleLoader = false, report, dir } = {}) {
  await cdp.send("Page.navigate", { url: `${BASE}/app` });
  const home = await cdp.waitFor(
    `document.querySelector('#root') && document.querySelector('#root').childElementCount > 0
     && document.querySelector('[aria-label=${JSON.stringify(`Play ${wanted.name}`)}]')`,
    45_000,
  );
  if (!home) throw new Error("the library home never offered a Play control");
  await networkQuiet(500, 15_000);
  await sleep(800);

  const clicked = await cdp.eval(`(() => {
    const b = document.querySelector(${JSON.stringify(`[aria-label="Play ${wanted.name}"]`)});
    if (!b || b.disabled) return false;
    b.scrollIntoView({ block: 'center' });
    b.click();
    return true;
  })()`);
  if (!clicked) throw new Error(`no enabled Play control for "${wanted.name}"`);
  const launchedAt = Date.now();

  if (sampleLoader) {
    // One representative frame per distinct phase label, where the label is
    // exactly what a person reads off the screen ("Establishing / secure
    // path") — so the dedupe key cannot drift from the copy.
    //
    // The poll is cheap; the screenshot is not (roughly half a second against
    // this animated scene). So a phase is captured on sight rather than waited
    // out, and if that frame landed inside the 140 ms word cross-fade it is
    // re-shot at once — waiting for the next poll usually means the phase has
    // already gone. A phase missing from the output did not last long enough
    // to be seen at all, which the report says out loud rather than papering
    // over with a mock frame.
    const phases = new Map();
    const order = [];
    const labelFor = (p) => {
      if (p.fail) return `failed: ${p.fail.slice(0, 60)}`;
      if (p.loader && p.stall) return `stalled: ${p.verb} ${p.word}`;
      if (p.loader && /is-locking/.test(p.cls)) return `handoff: ${p.verb} ${p.word}`;
      if (p.loader) return `${p.verb} ${p.word}`.trim();
      if (p.hud) return null; // the loader is gone; the HUD half owns the frame
      return p.path.includes("/session/") ? "session route, loader not yet drawn" : "home, launch requested";
    };
    const slugFor = (label, index) =>
      `10-loader-${String(index).padStart(2, "0")}-${label.replace(/[^a-z0-9]+/gi, "-").toLowerCase().replace(/^-|-$/g, "")}`;
    // The phase read that CHOSE the label is already stale by the time the
    // frame lands, so read again straight after the capture: that reading
    // brackets the frame, and it is the one the manifest reports.
    const record = async (label, id, before) => {
      await snap(dir, id);
      const after = (await cdp.eval(PHASE_EXPR).catch(() => null)) ?? before;
      phases.set(label, {
        file: `${id}.png`,
        label,
        rail: after.rail,
        cls: after.cls,
        atMs: Date.now() - launchedAt,
        caughtMidSwap: before.changing || undefined,
        stall: before.stall || undefined,
        fail: before.fail || undefined,
      });
      return after;
    };

    while (Date.now() - launchedAt < 240_000) {
      const p = await cdp.eval(PHASE_EXPR).catch(() => null);
      if (p) {
        const label = labelFor(p);
        if (label && !phases.has(label)) {
          order.push(label);
          const id = slugFor(label, order.length);
          const after = await record(label, id, p);
          // Caught mid-swap: the accent word is at zero opacity. If the phase
          // is still on screen and settled, replace the frame now.
          if (p.changing && !after.changing && labelFor(after) === label) {
            await record(label, id, after);
          }
        }
        if (p.hud && !p.loader) break;
      }
      await sleep(150);
    }

    for (const label of order) {
      const rec = phases.get(label);
      const row = { file: rec.file, note: `loader phase — ${label}`, rail: rec.rail, atMs: rec.atMs };
      const at = report.shots.findIndex((s) => s.file === rec.file);
      if (at >= 0) report.shots[at] = row;
      else report.shots.push(row);
    }
    report.loaderPhases = order.map((l) => phases.get(l));
  }

  if (!(await waitForHud())) throw new Error("the session never reached a live HUD");
  const id = await cdp.eval(`(location.pathname.match(/\\/app\\/session\\/([^/?#]+)/) || [])[1] || ''`);
  if (!id) throw new Error("the launch never reached /app/session/<id>");
  return id;
}

/** Leave the session page (which drops the transport and ends the session),
 *  then make sure nothing is left active before the next launch. */
async function teardown(sessionId) {
  await cdp.send("Page.navigate", { url: `${BASE}/app` }).catch(() => {});
  await sleep(1500);
  if (sessionId) {
    await fetch(`${API}/v1/sessions/${sessionId}`, {
      method: "DELETE",
      headers: { authorization: `Bearer ${token}` },
    }).catch(() => {});
  }
  for (let i = 0; i < 20; i++) {
    const active = (await get("/v1/admin/sessions?state=active"))?.items ?? [];
    if (!active.length) return [];
    await sleep(2000);
  }
  return (await get("/v1/admin/sessions?state=active"))?.items ?? [];
}

async function runSession() {
  const dir = path.join(OUT, "session");
  fs.mkdirSync(dir, { recursive: true });
  // `--only` selects stages, so one variant can be re-captured on its own after
  // a flaky launch without paying for the other seven. Stage names: `loader`
  // (which also carries the pane walk), each variant id, and `summary`.
  const onlyRe = ONLY ? new RegExp(ONLY) : null;
  const want = (stage) => !onlyRe || onlyRe.test(stage);
  // A targeted re-run merges into the manifest already on disk, so filling one
  // gap does not throw away the other stages' evidence — including the note the
  // failed stage left behind, which this run is about to replace.
  const manifestPath = path.join(dir, "manifest-session.json");
  const previous = onlyRe && fs.existsSync(manifestPath)
    ? JSON.parse(fs.readFileSync(manifestPath, "utf8"))
    : null;
  const report = {
    mode: "session",
    base: BASE,
    startedAt: new Date().toISOString(),
    viewport: `${WIDTH}x${HEIGHT}`,
    shots: previous?.shots ?? [],
    loaderPhases: previous?.loaderPhases,
    evidence: previous?.evidence ?? {},
    notes: (previous?.notes ?? []).filter((t) => !onlyRe.test(t)),
  };
  const shot = async (id, note, extra = {}) => {
    const file = await snap(dir, id);
    // A re-run replaces its own rows rather than appending duplicates.
    const at = report.shots.findIndex((s) => s.file === file);
    const row = { file, note, ...extra };
    if (at >= 0) report.shots[at] = row;
    else report.shots.push(row);
    log(`  shot ${file.padEnd(40)} ${note}`);
  };
  // Beside the shots, not beside the other manifests: the session evidence and
  // the frames it describes are read together.
  const flush = () => fs.writeFileSync(path.join(dir, "manifest-session.json"), JSON.stringify(report, null, 2));

  // --- pick the app ---------------------------------------------------------
  const catalogue = (await get("/v1/apps"))?.items ?? [];
  if (!catalogue.length) throw new Error("no apps in the library");
  const wanted = APP
    ? catalogue.find((a) => a.id === APP) ?? catalogue.find((a) => a.name.toLowerCase().includes(APP.toLowerCase()))
    : // No app named: prefer a desktop/tool (never needs a store login), then
      // anything that is not a provider app.
      catalogue.find((a) => a.kind !== "game") ??
      catalogue.find((a) => !a.external_source) ??
      catalogue[0];
  if (!wanted) throw new Error(`no app matched --app ${APP}`);
  report.app = { id: wanted.id, name: wanted.name, kind: wanted.kind };
  log(`app: ${wanted.name} (${wanted.id})`);

  // --- start from a clean slate --------------------------------------------
  const stale = (await get("/v1/admin/sessions?state=active"))?.items ?? [];
  for (const s of stale) {
    log(`ending stale active session ${s.id}`);
    await fetch(`${API}/v1/sessions/${s.id}`, { method: "DELETE", headers: { authorization: `Bearer ${token}` } });
  }
  if (stale.length) await teardown(null);

  // --- browser: auth + the peer-connection shim ----------------------------
  await cdp.send("Page.addScriptToEvaluateOnNewDocument", { source: PC_SHIM });
  await seedStorage({});
  await patchOverlayPrefs(overlayWire("full", "bottom", "on_capture"));

  await cdp.send("Page.navigate", { url: `${BASE}/app` });
  await cdp.waitFor(`document.querySelector('#root') && document.querySelector('#root').childElementCount > 0`, 30_000);
  await networkQuiet(500, 15_000);
  await sleep(1200);
  let sessionId = null;
  if (want("loader")) {
  await shot("00-home-before-launch", "library home, the app about to be launched");

  // --- launch #1: the loader, then the whole HUD ---------------------------
  log("launch 1 — loader phases + HUD panes");
  sessionId = await launchSession(wanted, { sampleLoader: true, report, dir });
  report.sessionId = sessionId;
  log(`session ${sessionId}: loader captured ${(report.loaderPhases ?? []).length} phases`);
  flush();

  // --- WebRTC evidence ------------------------------------------------------
  const decode = await waitForDecode(90_000, 5);
  report.evidence.decode = { ok: decode.ok, framesDecodedFirst: decode.first, framesDecodedLast: decode.last };
  report.evidence.stats = summarise(decode.snapshot);
  report.evidence.video = await cdp.eval(VIDEO_EXPR).catch(() => null);
  if (!decode.ok) report.notes.push("framesDecoded never advanced; the HUD shots below sit over a black stage");
  if (report.evidence.video && report.evidence.video.maxLuma === 0) {
    report.notes.push("the <video> sampled fully black (no GPU in headless) — the HUD chrome is the subject");
  }
  flush();

  await sleep(1500);
  await shot("20-hud-rest", "HUD rest pill — bottom dock, full content preset");

  // Shift+S is the documented jump to the stats pane (hudKeys.ts) and is ours
  // only while input is not captured — which is the state a fresh session is
  // in. From there the arrows walk the sections, so the whole shelf opens from
  // the keyboard, exactly as a player would reach it.
  await pressKey("S", { code: "KeyS", vk: 83, modifiers: MOD.shift });
  await sleep(900);
  const openedOn = await cdp.eval(`(document.querySelector('.hud-root') || {}).dataset ? document.querySelector('.hud-root').dataset.shelf : null`);
  report.evidence.shiftSOpenedOn = openedOn;
  if (openedOn !== "stats") report.notes.push(`Shift+S left the shelf on "${openedOn}" (expected stats)`);

  const paneNotes = {
    games: "Games pane — quick-switch rail",
    input: "Input pane — capture CTA, shortcut rows, gamepads, scaling",
    stats: "Stats pane — Simple cards with sparklines",
    display: "Display pane — stream / render resolution, interface size",
  };
  for (const want of ["stats", "display", "games", "input"]) {
    let at = await cdp.eval(`document.querySelector('.hud-root').dataset.shelf`);
    // ArrowRight cycles games -> input -> stats -> display -> games.
    for (let i = 0; i < 4 && at !== want; i++) {
      await pressKey("ArrowRight", { code: "ArrowRight", vk: 39 });
      await sleep(500);
      at = await cdp.eval(`document.querySelector('.hud-root').dataset.shelf`);
    }
    if (at !== want) {
      await cdp.eval(`(() => { const b = document.querySelector('[data-tab="${want}"]'); if (b) { b.click(); return true; } return false; })()`);
      await sleep(600);
      at = await cdp.eval(`document.querySelector('.hud-root').dataset.shelf`);
    }
    await sleep(800);
    await shot(`21-hud-pane-${want}`, paneNotes[want] + (at === want ? "" : ` (WANTED ${want}, GOT ${at})`));
    if (want === "stats") {
      const detailed = await cdp.eval(CLICK_TEXT("Detailed"));
      await sleep(1000);
      await shot("22-hud-pane-stats-detailed", detailed ? "Stats pane — Detailed mono tables" : "Stats pane — no Detailed control found");
      await cdp.eval(CLICK_TEXT("Simple"));
      await sleep(500);
    }
  }

  // Escape collapses the shelf (hudKeys.ts) — the same key a player uses.
  await pressKey("Escape", { code: "Escape", vk: 27 });
  await sleep(900);
  report.evidence.escapeClosedShelf =
    (await cdp.eval(`document.querySelector('.hud-root').dataset.open`)) === "false";
  await shot("23-hud-rest-after-escape", "HUD back at rest after Escape");
  flush();
  }

  // --- one launch per HUD variant ------------------------------------------
  const variants = [
    { id: "30-hud-dock-top", prefs: overlayWire("full", "top", "on_capture"), note: "HUD docked top", open: true },
    { id: "31-hud-dock-left", prefs: overlayWire("full", "left", "on_capture"), note: "HUD docked left (vertical column)", open: true },
    { id: "32-hud-dock-right", prefs: overlayWire("full", "right", "on_capture"), note: "HUD docked right (vertical column)", open: true },
    // The bottom dock is the default and is already captured as 20-hud-rest.
    { id: "40-hud-preset-minimal", prefs: overlayWire("minimal", "bottom", "on_capture"), note: 'HUD rest, "minimal" content preset (signal + actions only)' },
    { id: "41-hud-preset-metrics", prefs: overlayWire("metrics", "bottom", "on_capture"), note: 'HUD rest, "metrics" content preset (no identity/codec)' },
    // `never_visible` is the deterministic route to the hidden rendering. The
    // `on_capture` route needs Pointer Lock plus a 4 s idle, and a headless
    // page that has never had a real user gesture cannot be relied on to lock.
    { id: "50-hud-hidden", prefs: overlayWire("full", "bottom", "never_visible"), note: 'HUD hidden — auto-hide "never show"', hidden: true },
  ];

  let n = 2;
  for (const v of variants.filter((v) => want(v.id))) {
    await teardown(sessionId);
    sessionId = null;
    await patchOverlayPrefs(v.prefs);
    log(`launch ${n++} — ${v.id}`);
    // A launch can lose a race with the previous session's teardown on the
    // agent side; one retry costs a minute and saves the variant.
    let failure = null;
    for (let attempt = 0; attempt < 2 && !sessionId; attempt++) {
      try {
        sessionId = await launchSession(wanted);
      } catch (e) {
        failure = e;
        log(`  attempt ${attempt + 1} failed: ${e.message}`);
        await teardown(await cdp.eval(`(location.pathname.match(/\\/app\\/session\\/([^/?#]+)/) || [])[1] || ''`).catch(() => null));
      }
    }
    if (!sessionId) {
      report.notes.push(`${v.id}: ${failure ? failure.message : "launch failed"}`);
      continue;
    }
    const d = await waitForDecode(60_000, 2);
    await sleep(1500);
    await shot(`${v.id}-rest`, `${v.note}${d.ok ? "" : " (frames were not advancing)"}`);
    if (v.hidden) {
      await sleep(2000);
      report.evidence.hiddenHudClass = await cdp.eval(`(() => { const h = document.querySelector('.hud'); return h ? h.className : null; })()`);
      await shot("51-hud-hidden-summoned-pre", "hidden HUD, immediately before the summon chord");
      await pressKey("S", { code: "KeyS", vk: 83, modifiers: MOD.shift });
      await sleep(1000);
      await shot("52-hud-hidden-summoned", "the same hidden HUD summoned back with Shift+S");
      await pressKey("Escape", { code: "Escape", vk: 27 });
      await sleep(500);
    }
    if (v.open) {
      await cdp.eval(`(() => { const b = document.querySelector('.ib.chev'); if (b) { b.click(); return true; } return false; })()`);
      await sleep(1200);
      await shot(`${v.id}-open`, `${v.note}, shelf open`);
      await pressKey("Escape", { code: "Escape", vk: 27 });
      await sleep(400);
    }
    flush();
  }

  // --- final launch: end it through the HUD and catch the summary ----------
  await teardown(sessionId);
  sessionId = null;
  await patchOverlayPrefs(overlayWire("full", "bottom", "on_capture"));
  if (want("summary")) {
  log(`launch ${n} — exit through the HUD + summary`);
  try {
    sessionId = await launchSession(wanted);
    await waitForDecode(60_000, 2);
    // Long enough that the summary's duration/percentiles have real samples.
    await sleep(12_000);
    await shot("59-hud-before-exit", "the HUD immediately before the exit action");
    const ended = await cdp.eval(`(() => { const b = document.querySelector('.hud .ib.danger'); if (b) { b.click(); return true; } return false; })()`);
    if (!ended) report.notes.push("no exit control in the HUD; the session was ended over the API instead");
    const onHome = await cdp.waitFor(`document.querySelector('.session-summary')`, 45_000);
    report.evidence.summaryRendered = onHome;
    if (onHome) {
      await sleep(1500);
      await shot("60-session-summary", "post-session summary card on /app, after the HUD exit");
      report.evidence.summaryText = await cdp.eval(`document.querySelector('.session-summary').innerText`).catch(() => null);
    } else {
      report.notes.push("the summary card never appeared after the HUD exit");
      await shot("60-after-exit", "the page after the exit action");
    }
  } catch (e) {
    report.notes.push(`exit/summary pass: ${e.message}`);
  }
  }

  // --- teardown -------------------------------------------------------------
  const left = await teardown(sessionId);
  report.evidence.activeSessionsAfter = left.map((s) => ({ id: s.id, state: s.state }));
  // A merged re-run can inherit a row for a frame that is no longer on disk
  // (a phase that did not recur, a variant whose shots were deleted). The
  // manifest must never name a file the reader cannot open.
  report.shots = report.shots.filter((row) => fs.existsSync(path.join(dir, row.file)));
  if (report.loaderPhases) {
    report.loaderPhases = report.loaderPhases.filter((r) => fs.existsSync(path.join(dir, r.file)));
  }
  if (left.length) report.notes.push(`WARNING: ${left.length} session(s) still active after teardown`);
  report.finishedAt = new Date().toISOString();
  flush();
  log(`\n${report.shots.length} shots -> ${dir}`);
}

/** The two numbers the report quotes, lifted out of a getStats snapshot. */
function summarise(snapshot) {
  if (!snapshot) return null;
  const video = snapshot.flatMap((p) => p.inbound.filter((r) => r.kind === "video"));
  const audio = snapshot.flatMap((p) => p.inbound.filter((r) => r.kind === "audio"));
  return {
    video: video[0] ?? null,
    audio: audio[0] ? { framesReceived: audio[0].framesReceived, mimeType: audio[0].mimeType, bytesReceived: audio[0].bytesReceived } : null,
    pairs: snapshot.flatMap((p) => p.pairs),
    connectionStates: snapshot.map((p) => p.connectionState),
  };
}

if (MODE === "session") {
  await runSession();
  cdp.ws.close();
  process.exit(0);
}

const manifest = [];
const writeManifest = () =>
  fs.writeFileSync(
    path.join(OUT, `manifest-${MODE}.json`),
    JSON.stringify({ mode: MODE, base: BASE, ids, captured: manifest }, null, 2)
  );

for (const route of routes) {
  bucket = { console: [], failures: [] };
  const record = { id: route.id, mode: MODE };
  let url;
  try {

    if (MODE === "app") {
      const missing = [...route.path.matchAll(/\{(\w+)\}/g)].map((m) => m[1]).filter((k) => !ids[k]);
      if (missing.length) {
        manifest.push({ ...record, skipped: `no ${missing.join(", ")} on this stack` });
        log(`SKIP ${route.id} (no ${missing.join(", ")})`);
        continue;
      }
      const p = route.path.replace(/\{(\w+)\}/g, (_, k) => ids[k]);
      url = `${BASE}${p}`;
      record.path = p;
      await seedStorage(route);
    } else {
      url = `file://${path.resolve(MOCK_DIR, route.file)}${route.hash ?? ""}`;
      record.path = `${route.file}${route.hash ?? ""}`;
      // The mock persists theme/density/rail (its own key names) the moment a
      // variant route toggles one, and would then render every later route in
      // that state. Reset before each load.
      await seedMockPrefs();
      await cdp.send("Page.navigate", { url: "about:blank" });
      await sleep(150);
    }

    await cdp.send("Page.navigate", { url });
    const loaded = await cdp.waitFor("document.readyState === 'complete'", 20_000);
    record.loaded = loaded;
    if (MODE === "mock" && route.eval) {
      await sleep(300);
      try {
        await cdp.eval(route.eval);
      } catch (e) {
        record.actionError = `eval failed: ${e.message}`;
      }
    }
    // React must have mounted something before any of the other waits mean
    // anything: an empty #root satisfies "not loading" trivially.
    record.mounted = await cdp.waitFor(
      MODE === "app"
        ? "document.querySelector('#root') && document.querySelector('#root').childElementCount > 0 && (document.body.innerText || '').trim().length > 40"
        : "(document.body.innerText || '').trim().length > 40",
      MODE === "app" ? 20_000 : 6_000
    );
    await networkQuiet(500, 10_000);
    await cdp.waitFor("document.fonts && document.fonts.status === 'loaded'", 3_000).catch(() => {});
    const quiet = await cdp.waitFor(`!(${LOADING_EXPR})`, 10_000);
    record.stillLoadingAtCapture = !quiet;
    await sleep(route.wait ?? SETTLE);

    const actionError = await runActions(route);
    if (actionError) record.actionError = (record.actionError ? record.actionError + "; " : "") + actionError;

    record.url = await cdp.eval("location.pathname + location.search + location.hash").catch(() => null);
    record.title = await cdp.eval("document.title").catch(() => null);
    record.viewport = await cdp.eval("innerWidth + 'x' + innerHeight").catch(() => null);
    record.textLength = await cdp.eval("(document.body.innerText || '').trim().length").catch(() => 0);
    record.rootChildren = await cdp.eval("document.querySelector('#root') ? document.querySelector('#root').childElementCount : null").catch(() => null);
    record.contentHeight = await shoot(route.id);
    record.console = bucket.console;
    record.failures = bucket.failures.filter((f) => !/favicon|\.map$/.test(String(f.url)));
    record.blank = (record.textLength ?? 0) < 40;
    manifest.push(record);

    const flags = [
      record.blank ? "BLANK" : "",
      record.stillLoadingAtCapture ? "LOADING" : "",
      record.actionError ? "ACTION" : "",
      record.console.length ? `console:${record.console.length}` : "",
      record.failures.length ? `http:${record.failures.length}` : "",
    ]
      .filter(Boolean)
      .join(" ");
    log(`${record.blank || record.actionError ? "WARN" : "ok  "} ${route.id.padEnd(34)} ${record.path} ${flags}`);
  } catch (err) {
    // One bad route must not lose the pass: record it and carry on.
    record.error = String(err.message ?? err);
    manifest.push(record);
    log(`FAIL ${route.id.padEnd(34)} ${record.error}`);
    await recoverSession().catch((e) => log(`  recovery failed: ${e.message}`));
  }
  writeManifest();
}

writeManifest();
log(`\n${manifest.length} routes -> ${shotDir}`);
cdp.ws.close();
process.exit(0);
