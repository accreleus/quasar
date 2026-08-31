// scripts/qa/probe.mjs — peer-side driver for the image-QA harness (`make qa`).
//
// Spec: docs/design/plans/2026-08-13-image-qa-harness-spec.md
//
// Attaches to an ALREADY-LAUNCHED session (the orchestrator owns the session
// lifecycle so it can keep the session up across the input and shutdown gates),
// confirms decode, holds until the app has SETTLED, captures the oracle frame,
// then injects per-device stimuli and measures what changed on screen. Emits one
// JSON object on stdout; all progress on stderr.
//
// This is deliberately NOT a patch to scripts/harness/peer-driver.mjs: that harness's
// input probe asserts an XFCE right-click menu, which is meaningless on a Steam
// Big Picture or KDE surface, and its job (troubleshooting) is not this one's
// (per-image QA evidence).
//
// Env (required):
//   SPA_URL SID SIG_URL SIG_TOKEN AUTH_TOKEN
//   QA_CONFIG    JSON: the profile's { launch, oracle, input } gate config
// Env (optional):
//   APP_NAME CHROME PW_DIR CONNECT_TIMEOUT_MS QA_SKIP_DEVICES(csv) QA_RUN_LABEL
//
// Exit codes: 0 ok (JSON on stdout) · 2 never decoded · 1 harness error.
const PW_DIR = process.env.PW_DIR || '/tmp/t8-driver';
const { chromium } = await import(`${PW_DIR}/node_modules/playwright-core/index.mjs`);

const CHROME = process.env.CHROME || '/tmp/cft/chrome-linux64/chrome';
const SPA_URL = process.env.SPA_URL || 'http://localhost:8080';
const { SID, SIG_URL, SIG_TOKEN, AUTH_TOKEN } = process.env;
const APP_NAME = process.env.APP_NAME || 'unknown';
const RUN_LABEL = process.env.QA_RUN_LABEL || 'run';
const CONNECT_TIMEOUT = parseInt(process.env.CONNECT_TIMEOUT_MS || '45000', 10);
const SKIP = new Set(
  (process.env.QA_SKIP_DEVICES || '')
    .split(',')
    .map((s) => s.trim().toLowerCase())
    .filter(Boolean),
);

if (!SID || !SIG_URL || !SIG_TOKEN || !AUTH_TOKEN) {
  console.error('[qa-probe] FATAL: SID, SIG_URL, SIG_TOKEN, AUTH_TOKEN are required');
  process.exit(1);
}

let CFG;
try {
  CFG = JSON.parse(process.env.QA_CONFIG || '{}');
} catch (e) {
  console.error(`[qa-probe] FATAL: QA_CONFIG is not valid JSON: ${e.message}`);
  process.exit(1);
}
const LAUNCH_CFG = CFG.launch || {};
const INPUT_CFG = CFG.input || {};
const SETTLE = Object.assign({ frames: 3, tolerance_pct: 1.0, timeout_s: 90 }, INPUT_CFG.settle || {});
const REGION = INPUT_CFG.region || [0, 0, 1, 1];
const DELTA_THRESHOLD = INPUT_CFG.delta_threshold_pct ?? 4.0;
// Luma below this reads as "nothing drawn yet" — same constant peer-driver
// uses for first-content detection.
const CONTENT_THRESHOLD = 5;
const SAMPLE_MS = 500;

const log = (...a) => console.error('[qa-probe]', ...a);
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

const browser = await chromium.launch({
  executablePath: CHROME,
  headless: true,
  args: [
    '--no-sandbox',
    '--disable-features=WebRtcHideLocalIpsWithMdns',
    '--autoplay-policy=no-user-gesture-required',
    '--disable-dev-shm-usage',
    '--use-fake-device-for-media-stream',
    '--ignore-certificate-errors',
  ],
});
const page = await browser.newPage({ ignoreHTTPSErrors: true });

// Capture the agent-offered "input" DataChannel before any navigation, so
// stimuli bypass the client's DOM handlers and its pointer-lock gate: this gate
// is about the HOST half of the input path (agent -> uinput -> compositor).
await page.addInitScript(() => {
  const Native = window.RTCPeerConnection;
  if (!Native) return;
  window.__qaPCs = [];
  const Wrapped = function (...args) {
    const pc = new Native(...args);
    window.__qaPCs.push(pc);
    pc.addEventListener('datachannel', (e) => {
      if (e.channel && e.channel.label === 'input') window.__qaInputCh = e.channel;
    });
    return pc;
  };
  Wrapped.prototype = Native.prototype;
  window.RTCPeerConnection = Wrapped;
});

page.on('pageerror', (err) => log('[page-err]', err.message));

// ── attach to the running session ────────────────────────────────────────────
await page.goto(SPA_URL, { waitUntil: 'domcontentloaded' }).catch(async (e) => {
  // veth churn from a neighbouring session teardown aborts the first navigation.
  if (!/ERR_NETWORK_CHANGED/.test(e.message)) throw e;
  log('ERR_NETWORK_CHANGED on first navigation — retrying once');
  await sleep(2000);
  await page.goto(SPA_URL, { waitUntil: 'domcontentloaded' });
});

await page.evaluate(({ token, expires }) => {
  localStorage.setItem('quasar.auth.token', token);
  localStorage.setItem('quasar.auth.expires_at', expires);
  localStorage.setItem('quasar.auth.user', JSON.stringify({
    id: 'qa-probe', email: 'qa@quasar.local', username: 'qa-probe', role: 'user',
  }));
}, { token: AUTH_TOKEN, expires: new Date(Date.now() + 3600 * 1000).toISOString() });

// React Router v6 reads window.history.state.usr on init; seed it so the
// session page does not bounce back to /app for missing signaling state.
await page.addInitScript(({ sigUrl, sigTok, appNm }) => {
  history.replaceState({ usr: { signalingUrl: sigUrl, signalingToken: sigTok, appName: appNm }, key: 'qa', idx: 0 }, '');
}, { sigUrl: SIG_URL, sigTok: SIG_TOKEN, appNm: APP_NAME });

await page.goto(`${SPA_URL}/app/session/${SID}`, { waitUntil: 'domcontentloaded' });
await sleep(1500);

// ── page-side helpers (installed once, used by every capture below) ──────────
await page.evaluate(() => {
  const W = 320, H = 180;          // analysis raster — cheap, plenty for deltas
  const SHOT_W = 960, SHOT_H = 540; // evidence raster
  window.__qa = {
    grid(w, h) {
      const v = document.querySelector('video');
      if (!v || !v.videoWidth) return null;
      const c = document.createElement('canvas');
      c.width = w; c.height = h;
      const ctx = c.getContext('2d', { willReadFrequently: true });
      if (!ctx) return null;
      ctx.drawImage(v, 0, 0, w, h);
      return { canvas: c, ctx };
    },
    luma() {
      const g = window.__qa.grid(W, H);
      if (!g) return null;
      let d;
      try { d = g.ctx.getImageData(0, 0, W, H).data; } catch { return null; }
      let sum = 0;
      for (let i = 0; i < d.length; i += 4) sum += 0.2126 * d[i] + 0.7152 * d[i + 1] + 0.0722 * d[i + 2];
      return sum / (d.length / 4);
    },
    // Mean absolute per-pixel luma difference inside a fractional region,
    // expressed as a percentage of full scale (255).
    snapshot(region) {
      const g = window.__qa.grid(W, H);
      if (!g) return null;
      const [fx, fy, fw, fh] = region;
      const x = Math.max(0, Math.floor(fx * W)), y = Math.max(0, Math.floor(fy * H));
      const w = Math.max(1, Math.min(W - x, Math.round(fw * W)));
      const h = Math.max(1, Math.min(H - y, Math.round(fh * H)));
      let d;
      try { d = g.ctx.getImageData(x, y, w, h).data; } catch { return null; }
      const out = new Float32Array(d.length / 4);
      for (let i = 0, j = 0; i < d.length; i += 4, j++) {
        out[j] = 0.2126 * d[i] + 0.7152 * d[i + 1] + 0.0722 * d[i + 2];
      }
      return Array.from(out);
    },
    shot() {
      const g = window.__qa.grid(SHOT_W, SHOT_H);
      if (!g) return null;
      try { return g.canvas.toDataURL('image/png').split(',')[1]; } catch { return null; }
    },
    send(msgs) {
      const ch = window.__qaInputCh;
      if (!ch) return 'input_datachannel_not_captured';
      if (ch.readyState !== 'open') return `input_datachannel_${ch.readyState}`;
      for (const m of msgs) ch.send(JSON.stringify(m));
      return null;
    },
    async stats() {
      const pc = (window.__qaPCs || []).find((p) => p.getStats);
      if (!pc) return null;
      const s = await pc.getStats();
      const out = {};
      s.forEach((r) => {
        if (r.type === 'inbound-rtp' && r.kind === 'video') {
          out.fps = r.framesPerSecond ?? null;
          out.frames_decoded = r.framesDecoded ?? null;
          out.resolution = r.frameWidth ? `${r.frameWidth}x${r.frameHeight}` : null;
        }
        if (r.type === 'candidate-pair' && r.state === 'succeeded' && r.currentRoundTripTime != null) {
          out.rtt_ms = Math.round(r.currentRoundTripTime * 1000);
        }
      });
      return out;
    },
  };
});

// ── decode gate ──────────────────────────────────────────────────────────────
const t0 = Date.now();
let decoded = false;
let lastFrames = 0;
log(`waiting for decode (timeout ${CONNECT_TIMEOUT}ms)`);
while (Date.now() - t0 < CONNECT_TIMEOUT) {
  const info = await page.evaluate(() => {
    const v = document.querySelector('video');
    if (!v) return { vw: 0, frames: 0 };
    return { vw: v.videoWidth, frames: v.getVideoPlaybackQuality().totalVideoFrames };
  });
  if (info.vw > 0 && info.frames > lastFrames + 5) { decoded = true; break; }
  if (info.frames > lastFrames) lastFrames = info.frames;
  await sleep(500);
}
const decode_s = (Date.now() - t0) / 1000;

if (!decoded) {
  console.log(JSON.stringify({
    run: RUN_LABEL, error: 'decode_failed',
    message: 'video never decoded — H.264/ICE failure', decode_s,
  }));
  await browser.close();
  process.exit(2);
}
log(`decoded after ${decode_s.toFixed(1)}s`);

// ── settle gate ──────────────────────────────────────────────────────────────
// Hold until the app has actually drawn something stable. Sampling before this
// point is what made the hand-run's input deltas inconclusive: a cold Steam home
// is legitimately black for 30-55s AFTER decode is confirmed, so a baseline
// taken at decode measures app startup, not the input path.
const samples = [];
let firstContentS = null;
let settle = 'timeout';
const settleDeadline = Date.now() + SETTLE.timeout_s * 1000;
const tol = (SETTLE.tolerance_pct / 100) * 255;
while (Date.now() < settleDeadline) {
  const l = await page.evaluate(() => window.__qa.luma());
  if (l != null) {
    samples.push(l);
    if (firstContentS === null && l >= CONTENT_THRESHOLD) firstContentS = (Date.now() - t0) / 1000;
    if (firstContentS !== null && samples.length >= SETTLE.frames) {
      const tail = samples.slice(-SETTLE.frames);
      if (Math.max(...tail) - Math.min(...tail) <= tol) { settle = 'settled'; break; }
    }
  }
  await sleep(SAMPLE_MS);
}
const settleS = (Date.now() - t0) / 1000;
log(`settle=${settle} at t=${settleS.toFixed(1)}s first_content=${firstContentS ?? 'never'}`);

const tail = samples.slice(-10);
const mean = tail.length ? tail.reduce((a, b) => a + b, 0) / tail.length : null;
const sd = tail.length
  ? Math.sqrt(tail.reduce((a, b) => a + (b - mean) ** 2, 0) / tail.length)
  : null;

const stats = await page.evaluate(() => window.__qa.stats());
const oracleShot = await page.evaluate(() => window.__qa.shot());

// ── input gates ──────────────────────────────────────────────────────────────
// Profile stimuli are a small DSL over protocol/input.md. Compiling here (not in
// the profile) keeps profiles readable while the wire stays exactly the frozen
// shape: `k` carries an evdev code, `gp` carries a FULL W3C standard-gamepad
// state array (the host diffs against the previous state).
const GP_BUTTONS = 17;
function compile(stim) {
  const out = [];
  const rep = stim.repeat || 1;
  for (let i = 0; i < rep; i++) {
    switch (stim.t) {
      case 'ma': out.push({ t: 'ma', x: stim.x, y: stim.y }); break;
      case 'mm': out.push({ t: 'mm', dx: stim.dx, dy: stim.dy }); break;
      case 'ms': out.push({ t: 'ms', dx: stim.dx || 0, dy: stim.dy || 0 }); break;
      case 'mb':
        out.push({ t: 'mb', button: stim.button, pressed: true });
        out.push({ t: 'mb', button: stim.button, pressed: false });
        break;
      case 'k':
        out.push({ t: 'k', code: stim.code, pressed: true });
        out.push({ t: 'k', code: stim.code, pressed: false });
        break;
      case 'gp': {
        const down = new Array(GP_BUTTONS).fill(0);
        down[stim.button] = 1;
        const up = new Array(GP_BUTTONS).fill(0);
        const axes = [0, 0, 0, 0];
        out.push({ t: 'gp', i: 0, buttons: down, axes });
        out.push({ t: 'gp', i: 0, buttons: up, axes });
        break;
      }
      default: throw new Error(`unknown stimulus type '${stim.t}'`);
    }
  }
  return out;
}

function deltaPct(before, after) {
  if (!before || !after || before.length !== after.length) return null;
  let sum = 0;
  for (let i = 0; i < before.length; i++) sum += Math.abs(before[i] - after[i]);
  return (sum / before.length / 255) * 100;
}

const devices = {};
for (const [name, dev] of Object.entries(INPUT_CFG.devices || {})) {
  if (SKIP.has(name)) {
    devices[name] = { skipped: true, reason: 'QA_SKIP_DEVICES' };
    log(`device ${name}: skipped`);
    continue;
  }
  try {
    const msgs = [].concat(...dev.stimuli.map(compile));
    const before = await page.evaluate((r) => window.__qa.snapshot(r), REGION);
    const beforeShot = await page.evaluate(() => window.__qa.shot());
    const sendErr = await page.evaluate((m) => window.__qa.send(m), msgs);
    if (sendErr) throw new Error(sendErr);
    await sleep(dev.settle_ms || 600);
    const after = await page.evaluate((r) => window.__qa.snapshot(r), REGION);
    const afterShot = await page.evaluate(() => window.__qa.shot());
    const delta = deltaPct(before, after);
    devices[name] = {
      delta_pct: delta == null ? null : Number(delta.toFixed(2)),
      threshold_pct: DELTA_THRESHOLD,
      passed: delta != null && delta >= DELTA_THRESHOLD,
      messages_sent: msgs.length,
      stimuli: dev.stimuli,
      before_png_b64: beforeShot,
      after_png_b64: afterShot,
    };
    log(`device ${name}: delta=${delta?.toFixed(2)}% (threshold ${DELTA_THRESHOLD}%)`);
  } catch (e) {
    devices[name] = { error: e.message };
    log(`device ${name}: error ${e.message}`);
  }
}

console.log(JSON.stringify({
  run: RUN_LABEL,
  session_id: SID,
  decode_s: Number(decode_s.toFixed(2)),
  first_content_s: firstContentS == null ? null : Number(firstContentS.toFixed(1)),
  settle,
  settle_s: Number(settleS.toFixed(1)),
  luma: mean == null ? null : { mean: Number(mean.toFixed(1)), sd: Number(sd.toFixed(2)), samples: samples.length },
  stats,
  min_fps: LAUNCH_CFG.min_fps ?? null,
  oracle_png_b64: oracleShot,
  devices,
}));

await browser.close();
