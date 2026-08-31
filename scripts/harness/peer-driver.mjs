// P4-07 troubleshooting harness — Playwright browser runner.
//
// Authenticates the Quasar SPA (via localStorage), opens the session view,
// waits for real H.264 video decode (videoWidth>0 AND frames advancing), then
// collects lightweight telemetry from WebRTC getStats() and the diagnostic
// panel snapshot. Emits a JSON blob to stdout; all progress to stderr.
//
// Env (required):
//   SPA_URL        http://HOST:PORT  — base URL of the web SPA
//   SID            session UUID
//   SIG_URL        ws://... signaling URL (from POST /v1/sessions)
//   SIG_TOKEN      signaling token
//   AUTH_TOKEN     user bearer token (for localStorage injection)
//   APP_NAME       display name of the launched app (metadata only)
//
// Env (optional):
//   CHROME         path to Chrome-for-Testing binary
//   SECS           measurement window in seconds (default 35)
//   WARMUP         seconds to wait for decode before starting measurement (default 8)
//   CONNECT_TIMEOUT_MS  millis before giving up on video decode (default 45000)
//
// Task-10 (adaptive external resolution harness) — HOLD MODE, CLI flags:
//   --hold <secs>          after decode is confirmed, instead of the normal
//                           single-shot warmup+measurement+exit, hold the SAME
//                           connection open for <secs> and print one JSON line
//                           per --probe-every to stdout (flushed as printed):
//                             {t, videoWidth, videoHeight, totalVideoFrames,
//                              framesDecoded, decodeFailed}
//                           decodeFailed is true once videoWidth==0 (no
//                           decode) or the frame counter has been stalled for
//                           >3s (a hung/frozen decode).
//   --probe-every <ms>      poll interval in HOLD mode (default 1000)
//   --sid <SID>              overrides $SID (all other required env vars —
//                           SPA_URL/SIG_URL/SIG_TOKEN/AUTH_TOKEN — are still
//                           read from the environment)
//
//   Design choice (brief offered two options — "--probe-once" attach-and-exit,
//   OR "--hold N --probe-every 1s" keep-the-connection-open): HOLD was picked.
//   qses matrix steps MULTIPLE resolution rungs against one already-running
//   session and asserts `offer created` stays at 1 for the whole run — a
//   fresh --probe-once attach PER RUNG would each open a new RTCPeerConnection
//   (a new offer), which fails that invariant outright. HOLD opens exactly one
//   connection for the whole matrix run; qses matrix starts it once in the
//   background on the peer host, then reads timestamped samples out of its
//   output file as it steps each rung.
//
// Exit codes:
//   0  success — JSON report on stdout (normal mode) or clean HOLD completion
//   2  video never started (H.264 decode failure or ICE failure)
//   3  no getStats available (unexpected browser error)

function parseCliArgs(argv) {
  const out = {};
  for (let i = 2; i < argv.length; i++) {
    const a = argv[i];
    if (a === '--hold') out.hold = parseInt(argv[++i], 10);
    else if (a === '--probe-every') out.probeEvery = parseInt(argv[++i], 10);
    else if (a === '--sid') out.sid = argv[++i];
  }
  return out;
}
const CLI_ARGS = parseCliArgs(process.argv);
const HOLD_SECS = Number.isFinite(CLI_ARGS.hold) ? CLI_ARGS.hold : 0;
const PROBE_EVERY_MS = Number.isFinite(CLI_ARGS.probeEvery) ? CLI_ARGS.probeEvery : 1000;

// Import playwright-core from the T8 driver dir where it was npm-installed.
// The PW_DIR env is set by the shell harness.
// Fallback to the standard module name if running from a dir with node_modules.
const PW_DIR = process.env.PW_DIR || '/tmp/t8-driver';
const { chromium } = await import(`${PW_DIR}/node_modules/playwright-core/index.mjs`);

const CHROME = process.env.CHROME || '/tmp/cft/chrome-linux64/chrome';
const SPA_URL = process.env.SPA_URL || 'http://localhost:8080';
const SID = CLI_ARGS.sid || process.env.SID;
const SIG_URL = process.env.SIG_URL;
const SIG_TOKEN = process.env.SIG_TOKEN;
const AUTH_TOKEN = process.env.AUTH_TOKEN;
const APP_NAME = process.env.APP_NAME || 'unknown';
const SECS = parseInt(process.env.SECS || '35', 10);
const WARMUP = parseInt(process.env.WARMUP || '8', 10);
const CONNECT_TIMEOUT = parseInt(process.env.CONNECT_TIMEOUT_MS || '45000', 10);
// PLAYOUT_MS: if set, appends ?playout=<N> to the session URL (AS-04 sweep).
const PLAYOUT_MS = process.env.PLAYOUT_MS || '';
// KEEP_MDNS=1 keeps Chrome's default candidate hiding (random <uuid>.local mDNS
// hostnames instead of real IPs), so a run exercises the node-agent's in-container
// .local resolution path — the same shape a real user's Chrome presents. Default
// off: the harness historically disabled hiding because agent containers could not
// resolve .local at all (fixed by the in-image avahi + nss-mdns stack).
const KEEP_MDNS = process.env.KEEP_MDNS === '1';
// BENCH_MODE=1 opens the session page with ?bench=1, which arms the SPA's in-page
// quasar-benchapp marker decoder (web/src/bench). The decoder has to be BUNDLED —
// the SPA's CSP blocks page.addScriptTag and in-page eval, so a driver cannot
// inject it (2026-08-18 benchapp bring-up §6). We only drive it and read it out.
const BENCH_MODE = process.env.BENCH_MODE === '1';
// BENCH_PULSE_EVERY=N sends one Space over the input DataChannel every N seconds
// via window.__qBench.pressKey, so input-to-photon samples exist at all. 0 = off.
const BENCH_PULSE_EVERY = parseInt(process.env.BENCH_PULSE_EVERY || '0', 10);
// How many per-frame ring records to carry back. The ring itself caps at 5000
// (RING_CAPACITY); this bounds the stdout line the shell harness has to read.
const BENCH_MAX_FRAMES = parseInt(process.env.BENCH_MAX_FRAMES || '5000', 10);
// PEER_UNLOCK_FPS=1: headless Chrome-for-Testing's RVFC-driven present loop is
// capped at 60fps regardless of decoded content rate (overnight-2 §C,
// docs/reports/2026-08-19-overnight-2/README.md — a clean 1080p120 h264 run
// measured agent/decoded fps 120.0 but present_fps pinned at exactly 60.00 in
// every run, with zero frames_dropped/packets_lost/freeze_count; the marker
// instrument's ~50% "missing index" reading is that ceiling, not real loss).
// This adds `--disable-frame-rate-limit --disable-gpu-vsync` to the Chrome
// launch to lift Chrome's own compositor frame-rate cap. Default OFF —
// unproven on a 60fps profile and not needed there; bench_run.sh auto-arms
// this only when the requested profile's fps exceeds 60 (see its
// --peer-unlock-fps flag / QSES_PEER_UNLOCK_FPS env, threaded through qses).
// Kept as a single opt-in flag rather than always-on so a 60fps baseline's
// Chrome launch stays byte-identical to every prior report.
const PEER_UNLOCK_FPS = process.env.PEER_UNLOCK_FPS === '1';

// QICE_STATE_MARKER — #509: the deployment's STUN/TURN list arrives on
// SignalingCoords next to the url/token. The SPA reads it from React Router
// location.state (SessionPage's SessionState.iceServers), which this driver
// synthesises — so it has to be carried here or every run builds an
// RTCPeerConnection with an empty iceServers list regardless of deployment
// config. Base64-encoded JSON, supplied by qses from the launch response.
let ICE_SERVERS = [];
if (process.env.ICE_SERVERS_B64) {
  try {
    ICE_SERVERS = JSON.parse(Buffer.from(process.env.ICE_SERVERS_B64, 'base64').toString('utf8'));
  } catch (e) {
    console.error(`[peer-driver] ICE_SERVERS_B64 unparseable: ${e.message}`);
  }
}
// REDACTED: a turn:/turns: entry carries a long-lived operator credential
// (control-plane/internal/ice/ice.go ships ice.Redact for exactly this reason),
// and this stderr is retained verbatim in the evidence bundle — qses_redact
// only strips the auth/signaling/dev tokens, not TURN passwords.
const redactIceServers = (srv) => (srv || []).map((s) => ({
  urls: s.urls,
  username: s.username || undefined,
  credential: s.credential ? '<redacted>' : undefined,
}));
console.error(`[peer-driver] ice servers from signaling coords: ${JSON.stringify(redactIceServers(ICE_SERVERS))}`);

if (!SID || !SIG_URL || !SIG_TOKEN || !AUTH_TOKEN) {
  console.error('[peer-driver] FATAL: SID, SIG_URL, SIG_TOKEN, AUTH_TOKEN are required');
  process.exit(1);
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

if (PEER_UNLOCK_FPS) {
  console.error('[peer-driver] PEER_UNLOCK_FPS=1: launching Chrome with --disable-frame-rate-limit --disable-gpu-vsync');
}

const browser = await chromium.launch({
  executablePath: CHROME,
  headless: true,
  args: [
    '--no-sandbox',
    ...(KEEP_MDNS ? [] : ['--disable-features=WebRtcHideLocalIpsWithMdns']),
    '--autoplay-policy=no-user-gesture-required',
    '--disable-dev-shm-usage',
    '--use-fake-device-for-media-stream',
    // The control plane serves a self-signed cert on the HTTPS listener
    // (QUASAR_TLS=auto); harness runs must tolerate it.
    '--ignore-certificate-errors',
    ...(PEER_UNLOCK_FPS ? ['--disable-frame-rate-limit', '--disable-gpu-vsync'] : []),
  ],
});

const page = await browser.newPage({ ignoreHTTPSErrors: true });

// INPUT_PROBE (input-path bisect): capture the agent-offered "input" DataChannel
// so the probe below can inject protocol/input.md messages directly, bypassing
// the client's DOM handlers and its pointerLocked gate. That isolates the HOST
// half of the input path (agent -> uinput -> compositor -> client) from the
// browser half, which is otherwise only reachable through a real pointer lock.
// Installed unconditionally (it is inert without INPUT_PROBE=1) and before any
// navigation, so it is in place when the SPA constructs its peer connection.
await page.addInitScript(() => {
  const Native = window.RTCPeerConnection;
  if (!Native) return;
  const Wrapped = function (...args) {
    const pc = new Native(...args);
    pc.addEventListener('datachannel', (e) => {
      if (e.channel && e.channel.label === 'input') window.__qInputCh = e.channel;
    });
    return pc;
  };
  Wrapped.prototype = Native.prototype;
  window.RTCPeerConnection = Wrapped;
});

// Forward console messages to stderr for debugging.
page.on('console', (m) => {
  const t = m.text();
  if (/quasar|error|ICE|webrtc|signal|track|status|connect/i.test(t)) {
    console.error('[page]', t);
  }
});
page.on('pageerror', (err) => console.error('[page-err]', err.stack || err.message));

// Step 1: inject auth into localStorage so the SPA doesn't redirect to login.
// The token key is "quasar.auth.token" (web/src/auth/storage.ts).
// Navigate to the base URL first so we have an origin to write localStorage to.
console.error(`[peer-driver] navigating to base URL for localStorage setup`);
// One retry on ERR_NETWORK_CHANGED: when the peer browser runs on the streaming
// host itself, session-container veth create/teardown churns the interface list
// and Chrome aborts an in-flight navigation with this error (seen on the devbox
// when a qses run starts while the previous run's container is being reaped).
// It is a transient — the network is fine one beat later.
try {
  await page.goto(SPA_URL, { waitUntil: 'domcontentloaded' });
} catch (e) {
  if (!/ERR_NETWORK_CHANGED/.test(e.message)) throw e;
  console.error('[peer-driver] ERR_NETWORK_CHANGED on first navigation (veth churn) — retrying once');
  await sleep(2000);
  await page.goto(SPA_URL, { waitUntil: 'domcontentloaded' });
}

await page.evaluate(({ token, expires }) => {
  // Inject a synthetic session into localStorage. The SPA loads it on mount.
  // expires_at is required by storage.ts (it checks if the token is expired).
  localStorage.setItem('quasar.auth.token', token);
  localStorage.setItem('quasar.auth.expires_at', expires);
  // Stub the user object so the SPA doesn't redirect to /app from /v1/me.
  // The actual user data is fetched from /v1/me on load; this just prevents
  // the brief redirect flash. role=user is safe — we use the admin token.
  localStorage.setItem('quasar.auth.user', JSON.stringify({
    id: 'harness', email: 'harness@quasar.local',
    username: 'harness', role: 'user',
  }));
}, {
  token: AUTH_TOKEN,
  expires: new Date(Date.now() + 3600 * 1000).toISOString(),
});

// Step 2: navigate to the session page with signaling state.
//
// React Router's SessionPage reads useLocation().state for signalingUrl and
// signalingToken — it redirects to /app if the state is missing.
//
// React Router v6 BrowserRouter reads window.history.state.usr on init. We
// pre-seed window.history.state via addInitScript (runs before any page JS)
// so that when React Router initializes, it sees our signaling state as if
// navigate('/app/session/{id}', { state: {...} }) had been called.

const sessionQuery = [
  ...(PLAYOUT_MS ? [`playout=${PLAYOUT_MS}`] : []),
  ...(BENCH_MODE ? ['bench=1'] : []),
];
const sessionUrl = `/app/session/${SID}${sessionQuery.length ? `?${sessionQuery.join('&')}` : ''}`;

// Inject before page JS runs — React Router v6 reads window.history.state
// on startup. The format is { usr: <user-state>, key: 'default', idx: 0 }.
await page.addInitScript(({ sigUrl, sigTok, appNm, iceSrv }) => {
  // Retain peer references for standards-surface diagnostics. This does not
  // alter signaling; it lets the acceptance report prove what Chrome actually
  // negotiated and exposed when a required timing series is absent.
  const NativeRTCPeerConnection = window.RTCPeerConnection;
  window.__quasarHarnessPCs = [];
  window.RTCPeerConnection = class extends NativeRTCPeerConnection {
    constructor(...args) {
      super(...args);
      window.__quasarHarnessPCs.push(this);
    }
  };

  // Pre-seed history.state in the React Router v6 format.
  // React Router creates a BrowserHistory that reads history.state on init;
  // if history.state.usr is set, useLocation().state returns it.
  const rrState = {
    usr: {
      signalingUrl: sigUrl,
      signalingToken: sigTok,
      appName: appNm,
      iceServers: iceSrv,
    },
    key: 'harness',
    idx: 0,
  };
  // Replace the current history entry before React mounts.
  history.replaceState(rrState, '');
}, {
  sigUrl: SIG_URL,
  sigTok: SIG_TOKEN,
  appNm: APP_NAME,
  iceSrv: ICE_SERVERS,
});

console.error(`[peer-driver] navigating directly to session page`);
await page.goto(`${SPA_URL}${sessionUrl}`, { waitUntil: 'domcontentloaded' });

// Wait for React to mount and for the WebRTC signaling to start.
await sleep(1500);

// Check what URL we ended up at — if redirected to /app, state injection failed.
const currentUrl = page.url();
console.error(`[peer-driver] current URL after mount: ${currentUrl}`);

// Step 3: wait for H.264 video decode — videoWidth>0 AND frames advancing.
// This is the keystone check from CLAUDE.md: headless `running` is not enough.
const t0 = Date.now();
let connected = false;
let lastFrames = 0;
let framesAdvanced = false;

console.error(`[peer-driver] waiting for video decode (timeout ${CONNECT_TIMEOUT}ms)...`);

while (Date.now() - t0 < CONNECT_TIMEOUT) {
  const info = await page.evaluate(() => {
    const v = document.querySelector('video');
    if (!v) return { vw: 0, frames: 0, status: '' };
    const q = v.getVideoPlaybackQuality();
    const statusEl = document.querySelector('.session-status');
    return {
      vw: v.videoWidth,
      frames: q.totalVideoFrames,
      status: statusEl?.textContent || '',
    };
  });

  if (info.vw > 0) {
    if (!framesAdvanced && info.frames > lastFrames + 5) {
      framesAdvanced = true;
      console.error(`[peer-driver] H.264 decoding: videoWidth=${info.vw} frames=${info.frames} status="${info.status}"`);
      connected = true;
      break;
    }
    if (info.frames > lastFrames) lastFrames = info.frames;
  }

  console.error(`[peer-driver] t=${((Date.now()-t0)/1000).toFixed(0)}s vw=${info.vw} frames=${info.frames} status="${info.status}"`);
  await sleep(500);
}

if (!connected) {
  // Capture a screenshot for post-mortem.
  const shotPath = `/tmp/peer-driver-fail-${Date.now()}.png`;
  await page.screenshot({ path: shotPath });
  console.error(`[peer-driver] ERROR: video never decoded. Screenshot: ${shotPath}`);
  console.error('[peer-driver] This is Keystone 1 — H.264 decode failure or ICE failure.');
  console.error('[peer-driver] Try: check avahi-daemon is running on the Linux host for mDNS ICE.');

  // Emit minimal error JSON so the shell script can still produce a report.
  console.log(JSON.stringify({
    error: 'h264_decode_failed',
    message: 'video never decoded — H.264 decode or ICE failure (see Keystone 1)',
    screenshot: shotPath,
    lightweight: null,
    deep_trace: 'unavailable',
  }));
  await browser.close();
  process.exit(2);
}

// HOLD MODE (Task 10 harness): skip the normal single-shot warmup +
// measurement + JSON-and-exit path entirely. Keep this one connection open
// and print a timestamped JSON sample every --probe-every, for --hold
// seconds, then exit 0. See the file header for why this exists instead of
// reconnecting per rung.
if (HOLD_SECS > 0) {
  console.error(`[peer-driver] HOLD mode: probing every ${PROBE_EVERY_MS}ms for ${HOLD_SECS}s`);
  const holdT0 = Date.now();
  let lastFrames = -1;
  let lastFramesAt = holdT0;
  while (Date.now() - holdT0 < HOLD_SECS * 1000) {
    let info = null;
    try {
      info = await page.evaluate(() => {
        const v = document.querySelector('video');
        if (!v) return null;
        const q = v.getVideoPlaybackQuality();
        return { vw: v.videoWidth, vh: v.videoHeight, frames: q.totalVideoFrames };
      });
    } catch (e) {
      console.error(`[peer-driver] HOLD probe evaluate failed: ${e.message}`);
    }
    const now = Date.now();
    let decodeFailed = false;
    const frames = info ? info.frames : 0;
    if (!info || info.vw === 0) {
      decodeFailed = true;
    } else {
      if (lastFrames < 0 || frames > lastFrames) {
        lastFrames = frames;
        lastFramesAt = now;
      } else if (now - lastFramesAt > 3000) {
        decodeFailed = true; // frame counter stalled for >3s — hung/frozen decode
      }
    }
    const line = {
      t: new Date(now).toISOString(),
      videoWidth: info ? info.vw : 0,
      videoHeight: info ? info.vh : 0,
      totalVideoFrames: frames,
      framesDecoded: frames,
      decodeFailed,
    };
    console.log(JSON.stringify(line));
    await sleep(PROBE_EVERY_MS);
  }
  await browser.close();
  process.exit(0);
}

// Step 3.5 (T0, #378): kick off the luma content-liveness probe now that decode
// is confirmed. Fire-and-forget — it's awaited later (Step 8) — so its sampling
// window overlaps the warmup/measurement windows below instead of adding
// wall-clock time to the run. Draws each displayed frame into a small
// (160x90 — cheap) offscreen canvas and computes ITU-R BT.709 luma
// (0.2126R+0.7152G+0.0722B): a black/frozen stream reads mean~16 sd~0, real
// motion reads mean>40 or sd>2 across samples (the pairing used by the #378
// test matrix's content-live gate). Every failure mode (no <video>, no 2D
// context, tainted canvas, page teardown) is caught and turned into `{ error }`
// rather than throwing — this must never fail the run, only report
// "unavailable".
//
// The probe samples for the WHOLE run and reports STEADY STATE (the last 10
// samples), not the first 5 seconds. Reason (devbox, 2026-08-11): the probe
// used to take exactly 10 samples over ~5s starting the instant decode was
// confirmed, and decode is confirmed as soon as the compositor streams — which
// on a cold app is long before the app has drawn anything. A Steam session on a
// fresh home reached decode at t=0 and gamescope's first content commit at
// t≈36s, so the gate sampled a legitimately black pre-paint window and reported
// `mean=0.0`, i.e. "black stream". A full 90s run of the same session ends on a
// perfectly rendered Steam Big Picture at mean≈35. Sampling the head of the run
// measures app startup, not stream liveness. `first_content_s` is reported so a
// genuinely slow app start is still visible rather than silently tolerated.
// Bench mode: drive the in-page instrument. The pulse is what makes
// input-to-photon measurable at all — the app's echo is a 3-frame (50 ms at
// 60 fps) pulse, so it only exists in the pixels if something presses a key.
// pressKey() refuses to overlap two outstanding echoes, so a too-short interval
// degrades to "some pulses skipped", never to a mis-attributed sample.
let benchPulseTimer = null;
let benchPulsesSent = 0;
let benchPulsesRefused = 0;
if (BENCH_MODE && BENCH_PULSE_EVERY > 0) {
  console.error(`[peer-driver] bench mode: pressing Space every ${BENCH_PULSE_EVERY}s`);
  benchPulseTimer = setInterval(async () => {
    try {
      const ok = await page.evaluate(() => {
        const b = window.__qBench;
        return b ? b.pressKey('Space') : null;
      });
      if (ok === true) benchPulsesSent += 1;
      else if (ok === false) benchPulsesRefused += 1;
    } catch (e) {
      console.error(`[peer-driver] bench pulse failed: ${e.message}`);
    }
  }, BENCH_PULSE_EVERY * 1000);
}

const LUMA_SAMPLE_MS = 500;
const LUMA_STEADY_SAMPLES = 10;
const LUMA_CONTENT_THRESHOLD = 5;
const lumaWindowMs = (WARMUP + SECS) * 1000;
const lumaPromise = page.evaluate(async (cfg) => {
  try {
    const v = document.querySelector('video');
    if (!v) return { error: 'no_video_element' };
    const w = 160, h = 90;
    const canvas = document.createElement('canvas');
    canvas.width = w;
    canvas.height = h;
    const ctx = canvas.getContext('2d', { willReadFrequently: true });
    if (!ctx) return { error: 'no_2d_context' };
    const means = [];
    let firstContentMs = null;
    const t0 = Date.now();
    // Always take at least `steady` samples, even for a very short run.
    while (Date.now() - t0 < cfg.windowMs || means.length < cfg.steady) {
      ctx.drawImage(v, 0, 0, w, h);
      let data;
      try {
        data = ctx.getImageData(0, 0, w, h).data;
      } catch (e) {
        return { error: `getImageData_failed: ${e.message}` };
      }
      let sum = 0;
      const n = data.length / 4;
      for (let p = 0; p < data.length; p += 4) {
        sum += 0.2126 * data[p] + 0.7152 * data[p + 1] + 0.0722 * data[p + 2];
      }
      const m = sum / n;
      if (firstContentMs === null && m >= cfg.threshold) firstContentMs = Date.now() - t0;
      means.push(m);
      await new Promise((r) => setTimeout(r, cfg.sampleMs));
    }
    const steady = means.slice(-cfg.steady);
    const mean = steady.reduce((a, b) => a + b, 0) / steady.length;
    const variance = steady.reduce((a, b) => a + (b - mean) * (b - mean), 0) / steady.length;
    const allMean = means.reduce((a, b) => a + b, 0) / means.length;
    return {
      mean: +mean.toFixed(2),
      sd: +Math.sqrt(variance).toFixed(2),
      samples: steady.length,
      samples_total: means.length,
      mean_whole_run: +allMean.toFixed(2),
      first_content_s: firstContentMs === null ? null : +(firstContentMs / 1000).toFixed(1),
    };
  } catch (e) {
    return { error: `probe_failed: ${e.message}` };
  }
}, {
  windowMs: lumaWindowMs,
  sampleMs: LUMA_SAMPLE_MS,
  steady: LUMA_STEADY_SAMPLES,
  threshold: LUMA_CONTENT_THRESHOLD,
}).catch((e) => ({ error: `eval_rejected: ${e.message}` }));

// Step 4: warmup window — let getStats() counters settle.
console.error(`[peer-driver] warmup ${WARMUP}s...`);
await sleep(WARMUP * 1000);

// Step 4.5 (INPUT_PROBE=1): does injected input reach the guest's UI?
//
// Motion alone proves nothing — the compositor draws its own cursor, so a moving
// pointer is visible even when no client surface holds pointer focus and no event
// is ever delivered. The oracle is therefore a UI REACTION, not a cursor: on an
// XFCE desktop a right-click opens the applications menu, which changes a large
// fraction of the frame. We measure the mean absolute luma delta between a frame
// captured before the click and one captured after; a static desktop with an
// undelivered click differs only by the cursor (~0), a menu differs a lot.
//
// Messages follow protocol/input.md: {t:mm,dx,dy} relative motion, {t:mb,button,
// pressed} with Linux codes (272 BTN_LEFT / 273 BTN_RIGHT).
let inputProbe = null;
if (process.env.INPUT_PROBE === '1' || process.env.INPUT_PROBE === '2') {
  console.error('[peer-driver] INPUT_PROBE: injecting motion + right-click');
  inputProbe = await page.evaluate(async (stage2) => {
    window.__qProbeStage2 = stage2;
    const ch = window.__qInputCh;
    if (!ch) return { error: 'input_datachannel_not_captured' };
    if (ch.readyState !== 'open') return { error: `datachannel_${ch.readyState}` };

    const v = document.querySelector('video');
    if (!v) return { error: 'no_video_element' };
    const w = 160, h = 90;
    const canvas = document.createElement('canvas');
    canvas.width = w; canvas.height = h;
    const ctx = canvas.getContext('2d', { willReadFrequently: true });
    if (!ctx) return { error: 'no_2d_context' };
    const grab = () => {
      ctx.drawImage(v, 0, 0, w, h);
      const d = ctx.getImageData(0, 0, w, h).data;
      const out = new Float64Array(w * h);
      for (let p = 0, i = 0; p < d.length; p += 4, i++) {
        out[i] = 0.2126 * d[p] + 0.7152 * d[p + 1] + 0.0722 * d[p + 2];
      }
      return out;
    };
    const diff = (a, b) => {
      let s = 0;
      for (let i = 0; i < a.length; i++) s += Math.abs(a[i] - b[i]);
      return +(s / a.length).toFixed(3);
    };
    const send = (m) => ch.send(JSON.stringify(m));
    const wait = (ms) => new Promise((r) => setTimeout(r, ms));

    // Park the pointer at a known spot: slam to the top-left, then walk to the
    // middle. Relative motion accumulates in the compositor, so absolute
    // placement needs the slam first.
    for (let i = 0; i < 40; i++) send({ t: 'mm', dx: -400, dy: -400 });
    await wait(300);
    for (let i = 0; i < 30; i++) send({ t: 'mm', dx: 40, dy: 22 });
    await wait(800);

    // Baseline AFTER motion, so cursor movement is not counted as the reaction.
    const before = grab();
    const idle = diff(before, grab());

    send({ t: 'mb', button: 273, pressed: true });
    await wait(60);
    send({ t: 'mb', button: 273, pressed: false });
    await wait(1600);
    const after = grab();
    const reaction = diff(before, after);

    // Stage 2 (INPUT_PROBE=2): does mapping a NEW toplevel heal pointer focus?
    // If focus is only ever resolved at map time, a window opened by KEYBOARD
    // (which works) re-runs that path with a correct bbox, and clicks should
    // start landing afterwards. Alt+F2 opens the XFCE app finder.
    let keyReaction = null, reactionAfterMap = null;
    if (window.__qProbeStage2) {
      const b2 = grab();
      send({ t: 'k', code: 56, pressed: true });   // KEY_LEFTALT
      send({ t: 'k', code: 60, pressed: true });   // KEY_F2
      await wait(80);
      send({ t: 'k', code: 60, pressed: false });
      send({ t: 'k', code: 56, pressed: false });
      await wait(2500);
      const b3 = grab();
      keyReaction = diff(b2, b3);                  // did a window appear?

      for (let i = 0; i < 10; i++) send({ t: 'mm', dx: 12, dy: 7 });
      await wait(600);
      const b4 = grab();
      send({ t: 'mb', button: 273, pressed: true });
      await wait(60);
      send({ t: 'mb', button: 273, pressed: false });
      await wait(1600);
      reactionAfterMap = diff(b4, grab());         // do clicks work now?
    }

    // Dismiss whatever may have opened so the session is left clean.
    send({ t: 'k', code: 1, pressed: true });   // KEY_ESC
    send({ t: 'k', code: 1, pressed: false });
    await wait(400);

    return { idle, reaction, keyReaction, reactionAfterMap, sent: true };
  }, process.env.INPUT_PROBE === '2').catch((e) => ({ error: `probe_rejected: ${e.message}` }));
  console.error(`[peer-driver] INPUT_PROBE result: ${JSON.stringify(inputProbe)}`);
}

// Step 5: measurement window.
// #108: start a presentation-interval sampler before the window. requestVideoFrameCallback
// fires once per *displayed* frame, so the σ of these intervals is the direct smoothness
// metric (getStats reports 0 drops while the display still judders). NOTE: a headless
// browser has no real vsync surface, so this σ is a lower bound — it reads cleaner here
// than on a real monitor; treat it as a regression tripwire, not the absolute number.
await page.evaluate(() => {
  window.__presentIvals = [];
  window.__presentStop = false;
  const v = document.querySelector('video');
  if (!v || !('requestVideoFrameCallback' in v)) return;
  let last = null;
  const cb = (now) => {
    if (last != null) window.__presentIvals.push(now - last);
    last = now;
    if (!window.__presentStop) v.requestVideoFrameCallback(cb);
  };
  v.requestVideoFrameCallback(cb);
});

console.error(`[peer-driver] measuring for ${SECS}s...`);
// Session-view watchdog: the SPA replaces the whole session view (removing
// <video>) when the server reports the session failed (e.g. host_lost). When
// that happens mid-run the later telemetry read finds no video and used to be
// reported as an inscrutable "no_telemetry" — log the transition the moment it
// happens instead, and remember it so the exit path can name the real cause.
// (2026-08-11: three qses runs aborted this way; the cause was a node-agent
// segfault reaping the session, not a harness defect.)
let sessionViewLost = null;
const watchdog = setInterval(async () => {
  try {
    const s = await page.evaluate(() => ({
      url: location.pathname,
      video: !!document.querySelector('video'),
      lost: !!document.querySelector('.session-lost'),
    }));
    if (!s.video && sessionViewLost === null) {
      sessionViewLost = { at_s: +((Date.now() - t0) / 1000).toFixed(0), ...s };
      console.error(`[peer-driver] session view lost mid-run: ${JSON.stringify(sessionViewLost)}`);
    }
  } catch { /* page busy/navigating; the pre-telemetry snapshot still runs */ }
}, 5000);
await sleep(SECS * 1000);
clearInterval(watchdog);

// #108: stop the sampler and reduce to σ/p95/fps (≥5 frames needed to be meaningful).
const present = await page.evaluate(() => {
  window.__presentStop = true;
  const iv = window.__presentIvals || [];
  if (iv.length < 5) return { present_fps: null, present_interval_sd_ms: null, present_interval_p95_ms: null };
  const n = iv.length, mean = iv.reduce((a, b) => a + b, 0) / n;
  const sd = Math.sqrt(iv.reduce((a, b) => a + (b - mean) * (b - mean), 0) / n);
  const s = [...iv].sort((a, b) => a - b);
  const p95 = s[Math.min(s.length - 1, Math.floor(0.95 * s.length))];
  return {
    present_fps: +(1000 / mean).toFixed(2),
    present_interval_sd_ms: +sd.toFixed(2),
    present_interval_p95_ms: +p95.toFixed(2),
  };
});

// Capture one displayed-frame metadata sample plus negotiated receiver header
// extensions. These diagnostics distinguish a missing producer from a browser
// build that negotiated the extension but did not surface timing fields.
const timingDiagnostics = await page.evaluate(async () => {
  const v = document.querySelector('video');
  const pcs = window.__quasarHarnessPCs || [];
  const receivers = pcs.flatMap((pc) => pc.getReceivers());
  const videoReceiver = receivers.find((r) => r.track?.kind === 'video');
  const sync = videoReceiver?.getSynchronizationSources?.()[0] || null;
  const headerExtensions = videoReceiver?.getParameters?.().headerExtensions || [];
  let frameMetadata = null;
  if (v && 'requestVideoFrameCallback' in v) {
    frameMetadata = await Promise.race([
      new Promise((resolve) => v.requestVideoFrameCallback((_now, metadata) => {
        resolve({
          keys: Object.keys(metadata).sort(),
          captureTime: metadata.captureTime ?? null,
          receiveTime: metadata.receiveTime ?? null,
          expectedDisplayTime: metadata.expectedDisplayTime ?? null,
          rtpTimestamp: metadata.rtpTimestamp ?? null,
        });
      })),
      new Promise((resolve) => setTimeout(() => resolve({ timeout: true }), 2000)),
    ]);
  }
  return {
    peerCount: pcs.length,
    receiverHeaderExtensions: headerExtensions,
    synchronizationSource: sync ? {
      keys: Object.keys(sync).sort(),
      captureTimestamp: sync.captureTimestamp ?? null,
      senderCaptureTimeOffset: sync.senderCaptureTimeOffset ?? null,
      timestamp: sync.timestamp ?? null,
      rtpTimestamp: sync.rtpTimestamp ?? null,
    } : null,
    frameMetadata,
  };
});

// Step 6: collect telemetry from the page.
// We use page.evaluate to call getStats() directly on the RTCPeerConnection,
// plus read the video quality counters. The SPA's SessionTelemetry class posts
// to /v1/sessions/{id}/stats, but it also exposes the current snapshot via the
// diagnostic panel DOM. We collect from WebRTC directly for reliability.
// Snapshot page state right before the telemetry read so a failed read always
// carries the page's own story (host-lost card text, current route).
let preTelemetry = null;
try {
  preTelemetry = await page.evaluate(() => ({
    url: location.pathname,
    video: !!document.querySelector('video'),
    sessionLost: !!document.querySelector('.session-lost'),
    bodyHead: document.body.innerText.slice(0, 200).replace(/\s+/g, ' '),
  }));
  console.error(`[peer-driver] pre-telemetry page state: ${JSON.stringify(preTelemetry)}`);
} catch (e) {
  console.error(`[peer-driver] pre-telemetry probe failed: ${e.message}`);
}
const telemetry = await page.evaluate(async () => {
  const v = document.querySelector('video');
  if (!v) return null;

  const q = v.getVideoPlaybackQuality();
  const result = {
    videoWidth: v.videoWidth,
    videoHeight: v.videoHeight,
    totalFrames: q.totalVideoFrames,
    droppedFrames: q.droppedVideoFrames,
    fps: 0,
    rttMs: null,
    jitterBufferMs: null,
    decodeMs: null,
    packetsLost: 0,
    framesDropped: 0,
    // Deep trace values from diagnostic panel DOM (if present)
    isDeep: false,
    g2gMs: null,
    g2g95Ms: null,
    interactiveMs: null,
    interactive95Ms: null,
    networkMs: null,
    decodeDisplayMs: null,
  };

  // Try to get RTCPeerConnection stats. The SPA stores the PC on the session
  // object; we find it via the webkitRTCPeerConnection globals or enumerate
  // RTCPeerConnection instances.
  //
  // Strategy: read the diagnostic panel table if it's open, since the SPA's
  // SessionTelemetry already computed everything. Otherwise fall back to raw getStats().
  //
  // Try to open the diag panel first (click the Stats button).
  const statsBtn = Array.from(document.querySelectorAll('button')).find(
    (b) => b.textContent?.trim() === 'Stats' || b.textContent?.trim() === 'Hide stats'
  );
  if (statsBtn && statsBtn.textContent?.trim() === 'Stats') {
    statsBtn.click();
    await new Promise((r) => setTimeout(r, 500));
  }

  // Read from the diag panel table.
  const diagTable = document.querySelector('.diag-table');
  if (diagTable) {
    const rows = diagTable.querySelectorAll('tr');
    for (const row of rows) {
      const cells = row.querySelectorAll('td');
      if (cells.length < 2) continue;
      const key = cells[0].textContent?.trim().toLowerCase() || '';
      const val = parseFloat(cells[1].textContent || '0');
      if (isNaN(val)) continue;
      switch (key) {
        case 'fps': result.fps = val; break;
        case 'rtt': result.rttMs = val; break;
        case 'jitter-buf': result.jitterBufferMs = val; break;
        case 'decode': result.decodeMs = val; break;
        case 'pkt lost': result.packetsLost = val; break;
      }
      // Deep trace rows
      if (key === 'glass-to-glass') {
        result.isDeep = true;
        result.g2gMs = val;
        // p95 is in a nested span
        const dim = cells[1].querySelector('.diag-dim');
        if (dim) {
          const m = dim.textContent?.match(/([\d.]+)/);
          if (m) result.g2g95Ms = parseFloat(m[1]);
        }
      }
      if (key === 'interactive') {
        result.interactiveMs = val;
        const dim = cells[1].querySelector('.diag-dim');
        if (dim) {
          const m = dim.textContent?.match(/([\d.]+)/);
          if (m) result.interactive95Ms = parseFloat(m[1]);
        }
      }
      if (key === 'network+pacing') result.networkMs = val;
      if (key === 'decode+display') result.decodeDisplayMs = val;
    }
  }

  return result;
});

if (!telemetry) {
  // No <video> at read time. Distinguish "the session died under the run" (the
  // SPA swapped in its host-lost card after the server reported failed) from a
  // genuine harness/DOM defect — the two need entirely different follow-up.
  const lost = preTelemetry?.sessionLost || sessionViewLost !== null;
  if (lost) {
    console.error('[peer-driver] ERROR: session died mid-run (host_lost) — the SPA replaced the session view. Check the node-agent (crash/restart?) and the control-plane session record; this is not a browser-harness defect.');
    console.log(JSON.stringify({
      error: 'session_lost_mid_run',
      message: 'server reported the session failed (host_lost) during the measurement window',
      session_view_lost: sessionViewLost,
      page_state: preTelemetry,
      lightweight: null,
      deep_trace: 'unavailable',
    }));
  } else {
    console.error('[peer-driver] ERROR: could not collect telemetry from page');
    console.log(JSON.stringify({ error: 'no_telemetry', page_state: preTelemetry, lightweight: null, deep_trace: 'unavailable' }));
  }
  await browser.close();
  process.exit(3);
}

// Step 7: also collect raw getStats() via page.evaluate for fps/rtt if the panel
// didn't populate (e.g., panel never opened or diagnostic values are zero).
if (telemetry.fps === 0 || telemetry.rttMs === null) {
  const raw = await page.evaluate(async () => {
    // Find the RTCPeerConnection. Chrome exposes all PCs via the internal
    // RTCPeerConnectionInternals but not JS-accessible. Instead, we patch
    // RTCPeerConnection at page init — but we didn't do that here.
    // Fallback: try to get the current video frame rate from the video element.
    const v = document.querySelector('video');
    if (!v) return null;
    // Frame rate from getVideoPlaybackQuality over a short window.
    const q1 = v.getVideoPlaybackQuality();
    await new Promise((r) => setTimeout(r, 1000));
    const q2 = v.getVideoPlaybackQuality();
    const deltaFrames = q2.totalVideoFrames - q1.totalVideoFrames;
    return { fps: deltaFrames, droppedFrames: q2.droppedVideoFrames };
  });
  if (raw) {
    if (telemetry.fps === 0) telemetry.fps = raw.fps;
    telemetry.droppedFrames = raw.droppedFrames;
  }
}

// Step 8 (T0, #378): collect the luma probe kicked off at Step 3.5. Its sampling
// window spans warmup + measurement, so this resolves as that window closes.
const luma = await lumaPromise;
if (luma.error) {
  console.error(`[peer-driver] luma probe unavailable: ${luma.error}`);
} else {
  console.error(
    `[peer-driver] luma probe: mean=${luma.mean} sd=${luma.sd} samples=${luma.samples}`
    + ` (steady state; whole-run mean=${luma.mean_whole_run} over ${luma.samples_total} samples,`
    + ` first content at ${luma.first_content_s === null ? 'never' : `${luma.first_content_s}s`})`,
  );
  // A dark steady state is the one luma verdict a human always wants to eyeball:
  // capture the page so "black stream" reports carry a frame instead of a number.
  if (luma.mean < LUMA_CONTENT_THRESHOLD) {
    const darkShot = `/tmp/peer-driver-luma-${Date.now()}.png`;
    try {
      await page.screenshot({ path: darkShot });
      console.error(`[peer-driver] luma steady state is dark — screenshot: ${darkShot}`);
    } catch (e) {
      console.error(`[peer-driver] luma screenshot failed: ${e.message}`);
    }
  }
}

// Bench mode readout. Everything below comes from the page, not from the driver:
// the SPA already decoded every displayed frame and aggregated it per second.
// This is deliberately independent of the bench.window trace-event POST path, so
// a control plane that has not yet learned the event type still yields results.
if (benchPulseTimer) clearInterval(benchPulseTimer);
let bench = null;
if (BENCH_MODE) {
  // Read the PER-FRAME ring as well as the windows. Without it the ring never
  // leaves the page, and a submitted run has only per-second aggregates — so
  // time-aligned drop attribution ("which frame, against what playout target")
  // is reconstructable from a live browser but not from a bench artifact.
  // Bounded: a tail, so a long hold cannot produce an unbounded stdout line.
  bench = await page.evaluate((maxFrames) => {
    const b = window.__qBench;
    if (!b) return { error: 'bench instrument absent — is this SPA build bench-aware?' };
    const dumped = b.dump();
    const frames = dumped.frames || [];
    return {
      windows: b.windows(),
      stats: b.stats(),
      frames: frames.length > maxFrames ? frames.slice(-maxFrames) : frames,
      frames_truncated: frames.length > maxFrames,
    };
  }, BENCH_MAX_FRAMES).catch((e) => ({ error: e.message }));
  if (bench && !bench.error) {
    bench.pulses_sent = benchPulsesSent;
    bench.pulses_refused = benchPulsesRefused;
    console.error(`[peer-driver] bench: ${bench.windows.length} windows, ` +
      `${bench.stats.decoded}/${bench.stats.frames} frames decoded, ` +
      `${(bench.frames || []).length} ring records` +
      `${bench.frames_truncated ? ' (truncated)' : ''}, ` +
      `${benchPulsesSent} pulses sent, ${bench.stats.i2p_missed ?? 0} echoes missed`);
  } else if (bench) {
    console.error(`[peer-driver] bench readout failed: ${bench.error}`);
  }
}


// ── QICE_MARKER: ICE diagnostics (#509 / #536 live validation) ──────────────
// Which candidate pair Chrome ACTUALLY selected, what the page was configured
// with, the full local/remote candidate inventory, and the inbound-rtp
// counters. Additive: nothing above reads it.
const iceDiagnostics = await page.evaluate(async () => {
  const pcs = window.__quasarHarnessPCs || [];
  const out = [];
  for (const pc of pcs) {
    let cfg = null;
    try { cfg = pc.getConfiguration(); } catch { /* ignore */ }
    const stats = await pc.getStats();
    const byId = new Map();
    stats.forEach((s) => byId.set(s.id, s));
    const cand = (id) => {
      const c = byId.get(id);
      if (!c) return null;
      return {
        type: c.candidateType ?? null,
        protocol: c.protocol ?? null,
        relayProtocol: c.relayProtocol ?? null,
        address: c.address ?? c.ip ?? null,
        port: c.port ?? null,
        url: c.url ?? null,
        networkType: c.networkType ?? null,
      };
    };
    let pair = null;
    stats.forEach((s) => {
      if (s.type === 'transport' && s.selectedCandidatePairId) {
        pair = byId.get(s.selectedCandidatePairId) || pair;
      }
    });
    if (!pair) {
      stats.forEach((s) => {
        if (s.type === 'candidate-pair' && (s.selected === true ||
            (s.nominated === true && s.state === 'succeeded'))) pair = s;
      });
    }
    const locals = []; const remotes = []; const pairs = [];
    stats.forEach((s) => {
      if (s.type === 'local-candidate') locals.push(cand(s.id));
      if (s.type === 'remote-candidate') remotes.push(cand(s.id));
      if (s.type === 'candidate-pair') pairs.push({
        state: s.state, nominated: s.nominated ?? null,
        local: cand(s.localCandidateId), remote: cand(s.remoteCandidateId),
        bytesReceived: s.bytesReceived ?? null,
      });
    });
    let inbound = null;
    stats.forEach((s) => {
      if (s.type === 'inbound-rtp' && s.kind === 'video') inbound = {
        bytesReceived: s.bytesReceived ?? null,
        packetsReceived: s.packetsReceived ?? null,
        packetsLost: s.packetsLost ?? null,
        framesDecoded: s.framesDecoded ?? null,
        framesDropped: s.framesDropped ?? null,
        freezeCount: s.freezeCount ?? null,
        totalFreezesDuration: s.totalFreezesDuration ?? null,
        framesPerSecond: s.framesPerSecond ?? null,
        jitter: s.jitter ?? null,
        jitterBufferDelay: s.jitterBufferDelay ?? null,
        jitterBufferEmittedCount: s.jitterBufferEmittedCount ?? null,
        decoderImplementation: s.decoderImplementation ?? null,
        mimeType: (byId.get(s.codecId) || {}).mimeType ?? null,
      };
    });
    out.push({
      configured_ice_servers: cfg ? (cfg.iceServers || []).map((s) => ({
        urls: s.urls, has_username: !!s.username, has_credential: !!s.credential,
      })) : null,
      ice_transport_policy: cfg ? (cfg.iceTransportPolicy ?? null) : null,
      selected_pair: pair ? {
        state: pair.state,
        nominated: pair.nominated ?? null,
        currentRoundTripTime: pair.currentRoundTripTime ?? null,
        availableIncomingBitrate: pair.availableIncomingBitrate ?? null,
        availableOutgoingBitrate: pair.availableOutgoingBitrate ?? null,
        bytesReceived: pair.bytesReceived ?? null,
        bytesSent: pair.bytesSent ?? null,
        local: cand(pair.localCandidateId),
        remote: cand(pair.remoteCandidateId),
      } : null,
      local_candidate_types: [...new Set(locals.map((c) => c && c.type))],
      local_candidates: locals,
      remote_candidate_types: [...new Set(remotes.map((c) => c && c.type))],
      remote_candidates: remotes,
      succeeded_pairs: pairs.filter((x) => x.state === 'succeeded'),
      inbound_video: inbound,
      connection_state: pc.connectionState,
      ice_connection_state: pc.iceConnectionState,
    });
  }
  return out;
}).catch((e) => ({ error: e.message }));
console.error(`[peer-driver] ice: ${JSON.stringify(iceDiagnostics)}`);
// ── end QICE_MARKER ─────────────────────────────────────────────────────────


const result = {
  ice: iceDiagnostics,
  ice_elapsed_s: +((Date.now() - t0) / 1000).toFixed(2),
  ...(BENCH_MODE ? { bench } : {}),
  timing_diagnostics: timingDiagnostics,
  luma,
  input_probe: inputProbe,
  lightweight: {
    fps: telemetry.fps,
    rtt_ms: telemetry.rttMs,
    jitter_buffer_ms: telemetry.jitterBufferMs,
    decode_ms: telemetry.decodeMs,
    packets_lost: telemetry.packetsLost,
    frames_dropped: telemetry.droppedFrames,
    resolution: telemetry.videoWidth > 0
      ? `${telemetry.videoWidth}x${telemetry.videoHeight}`
      : 'unknown',
    total_frames: telemetry.totalFrames,
    // #108 presentation pacing (RVFC σ — headless lower bound, see note above).
    present_fps: present.present_fps,
    present_interval_sd_ms: present.present_interval_sd_ms,
    present_interval_p95_ms: present.present_interval_p95_ms,
  },
  deep_trace: telemetry.isDeep ? {
    glass_to_glass_ms: telemetry.g2gMs,
    g2g_p95_ms: telemetry.g2g95Ms,
    interactive_ms: telemetry.interactiveMs,
    interactive_p95_ms: telemetry.interactive95Ms,
    network_pacing_ms: telemetry.networkMs,
    decode_display_ms: telemetry.decodeDisplayMs,
    // encode_ms comes from agent metrics (populated by shell script)
    encode_ms: null,
  } : 'unavailable',
};

console.log(JSON.stringify(result));
await browser.close();
