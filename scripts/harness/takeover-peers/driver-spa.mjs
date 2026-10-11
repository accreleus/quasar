// The REAL web client against the takeover browser host (#535): a built `web/dist` and
// a mocked `/v1` served from this process on loopback, signaling pointed at the host. No
// control plane. (A real listener, not route interception: Chrome's local-network
// checks block a loopback WebSocket from a page whose response had no address.) What peer.html cannot show: the session runtime's own handling of a second
// offer (microphone slot, input channel, tracks, playout).
//
// usage: node driver-spa.mjs <scenario> <ws-url> <dist-dir>
//   takeover   a second browser resumes a session the first is streaming, mic granted
//   restart    the same; the host is also asked for an ICE restart between its two offers
//   late       the same; the host is handed the page's answer to the old offer twice
// Prints one JSON object: what each page saw, and `checks` (name -> pass).
import { chromium } from "playwright-core";
import fs from "node:fs";
import http from "node:http";
import path from "node:path";

const [scenario, wsUrl, DIST] = process.argv.slice(2);
// localhost is a secure context without TLS, which getUserMedia needs.
const WEB_PORT = Number(new URL(wsUrl).port) + 1;
const ORIGIN = `http://localhost:${WEB_PORT}`;
const SID = "11111111-1111-4111-8111-111111111111";
const APP = "22222222-2222-4222-8222-222222222222";
const USER = { id: "33333333-3333-4333-8333-333333333333", email: "player@example.test", username: "player", role: "user" };
const MIME = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml",
  ".png": "image/png", ".woff2": "font/woff2", ".woff": "font/woff", ".json": "application/json",
  ".webmanifest": "application/manifest+json", ".ico": "image/x-icon", ".sh": "text/plain" };
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

const SESSION = {
  id: SID, user_id: USER.id, app_id: APP, host_id: "44444444-4444-4444-8444-444444444444",
  state: "running", state_detail: "app presented", error_message: null, failure_code: null,
  app_log_tail: null, stop_reason: null, home_seed: null, profile_id: null, stream_profile_id: null,
  stream: { width: 320, height: 240, fps: 30, bitrate_kbps: 2000, h264_profile: "constrained-baseline",
    codec: "h264", mic: true, rungs: [[320, 240]] },
  codec_decision: null, negotiated_codec: null,
  created_at: new Date(Date.now() - 120e3).toISOString(), started_at: new Date(Date.now() - 100e3).toISOString(),
  ended_at: null,
};

// Runs in the page before the app: sign in, seat what the library's launch hands the
// session route, and record what WebRTC and the signaling socket do.
function init([user, sid, app, wsUrl]) {
  localStorage.setItem("quasar.auth.token", "tok");
  localStorage.setItem("quasar.auth.expires_at", new Date(Date.now() + 3600e3).toISOString());
  localStorage.setItem("quasar.auth.user", JSON.stringify(user));
  const common = { appName: "Takeover", appId: app, tier: "320×240@30", resolvedCodec: "h264", micGranted: true, playoutOverride: null };
  const mode = new URLSearchParams(location.search).get("as");
  if (location.pathname === `/app/session/${sid}` && !sessionStorage.getItem("seeded") && mode !== "resume") {
    sessionStorage.setItem("seeded", "1");
    // "launch": the coords the launch response carried. "resume-mic": a resume (no
    // coords, so the page mints) that still knows the mic was granted.
    const usr = mode === "launch" ? { ...common, signalingUrl: wsUrl, signalingToken: "t0", iceServers: [] } : common;
    history.replaceState({ usr, key: "default", idx: 0 }, "");
  }
  const R = window.__rtc = { pcs: [], channels: [], wsCloses: [] };
  const NativePC = window.RTCPeerConnection;
  window.RTCPeerConnection = class extends NativePC {
    constructor(...args) {
      super(...args);
      const rec = { offers: 0, tracks: [], pc: this };
      R.pcs.push(rec);
      this.addEventListener("track", (e) => rec.tracks.push(e.track.kind));
      this.addEventListener("datachannel", (e) => {
        const c = { label: e.channel.label, open: e.channel.readyState === "open", closed: false, pongs: 0, channel: e.channel };
        R.channels.push(c);
        e.channel.addEventListener("open", () => { c.open = true; });
        e.channel.addEventListener("close", () => { c.closed = true; });
        e.channel.addEventListener("message", (m) => { if (String(m.data).includes('"pong"')) c.pongs++; });
      });
    }
    setRemoteDescription(desc) {
      const rec = R.pcs.find((r) => r.pc === this);
      if (desc?.type === "offer") rec.offers++;
      return super.setRemoteDescription(desc);
    }
  };
  const NativeWS = window.WebSocket;
  window.WebSocket = class extends NativeWS {
    constructor(...args) {
      super(...args);
      this.addEventListener("close", (e) => R.wsCloses.push(e.code));
    }
  };
  R.snapshot = async () => {
    const v = document.querySelector("video");
    const pcs = [];
    for (const r of R.pcs) {
      let frames = null;
      (await r.pc.getStats()).forEach((s) => { if (s.type === "inbound-rtp" && s.kind === "video") frames = s.framesDecoded; });
      pcs.push({ offers: r.offers, tracks: r.tracks, ice: r.pc.iceConnectionState, conn: r.pc.connectionState, framesDecoded: frames,
        mids: r.pc.getTransceivers().map((t) => `${t.mid}:${t.direction}/${t.currentDirection}`) });
    }
    return {
      pcs, wsCloses: R.wsCloses,
      channels: R.channels.map((c) => ({ label: c.label, open: c.open, closed: c.closed, pongs: c.pongs })),
      frames: v?.getVideoPlaybackQuality?.().totalVideoFrames ?? 0,
      audioElements: document.querySelectorAll("audio").length,
      audioTracksPerElement: [...document.querySelectorAll("audio")].map((a) => a.srcObject?.getAudioTracks().length ?? 0),
      loader: document.querySelector(".sl-root") !== null,
      banners: [...document.querySelectorAll(".banner strong")].map((b) => b.textContent),
      micButton: (() => { const b = document.querySelector('[aria-label="Microphone"]'); return b ? { disabled: b.disabled } : null; })(),
      micHot: document.querySelector(".mic-hot") !== null,
    };
  };
  R.ping = () => R.channels.at(-1)?.channel.send('{"t":"ping","id":1,"tc":0}');
}

// The control plane, as far as the session page needs one.
let mints = 0;
http.createServer((req, res) => {
  const url = new URL(req.url, ORIGIN);
  const json = (status, body) => { res.writeHead(status, { "content-type": "application/json" }); res.end(JSON.stringify(body)); };
  if (url.pathname.startsWith("/v1/")) {
    if (url.pathname === `/v1/sessions/${SID}` && req.method === "GET") return json(200, { session: SESSION });
    if (url.pathname === `/v1/sessions/${SID}/signaling-token`) {
      mints++;
      return json(200, { signaling: { url: wsUrl, token: `t${mints}`, expires_at: new Date(Date.now() + 60e3).toISOString(), ice_servers: [] } });
    }
    if (url.pathname === "/v1/setup/status") return json(200, { admin_exists: true, setup_completed: true });
    if (url.pathname === "/v1/me") return json(200, { user: USER });
    if (url.pathname === "/v1/apps" || url.pathname === "/v1/sessions") return json(200, { items: [], next_cursor: null });
    if (req.method === "GET") return json(200, {});
    res.writeHead(204); return res.end();
  }
  let file = path.join(DIST, decodeURIComponent(url.pathname));
  if (!file.startsWith(DIST) || !fs.existsSync(file) || fs.statSync(file).isDirectory()) file = path.join(DIST, "index.html");
  res.writeHead(200, { "content-type": MIME[path.extname(file)] ?? "application/octet-stream" });
  res.end(fs.readFileSync(file));
}).listen(WEB_PORT, "127.0.0.1");

async function open(as) {
  const browser = await chromium.launch({
    executablePath: process.env.CHROME || "/usr/bin/google-chrome",
    headless: false,
    args: ["--no-sandbox", "--autoplay-policy=no-user-gesture-required",
      "--use-fake-device-for-media-stream", "--use-fake-ui-for-media-stream",
      "--disable-features=WebRtcHideLocalIpsWithMdns,CalculateNativeWinOcclusion",
      "--disable-backgrounding-occluded-windows", "--disable-renderer-backgrounding", "--disable-background-timer-throttling"],
  });
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 720 }, permissions: ["microphone"] });
  await ctx.addInitScript(init, [USER, SID, APP, wsUrl]);
  const page = await ctx.newPage();
  const errors = [];
  page.on("pageerror", (e) => errors.push(String(e).slice(0, 200)));
  page.on("console", (m) => { if (m.type() === "error" && consoleErrors.length < 12) consoleErrors.push(m.text().slice(0, 200)); });
  await page.goto(`${ORIGIN}/app/session/${SID}?as=${as}`);
  return { browser, page, errors };
}

const snap = async (p) => { try { return await p.page.evaluate(() => window.__rtc.snapshot()); } catch (e) { return { error: String(e) }; } };
const until = async (p, pred, ms) => {
  const end = Date.now() + ms; let s;
  do { s = await snap(p); if (pred(s)) break; await sleep(250); } while (Date.now() < end);
  return s;
};
const flowing = async (p, ms = 1500) => { const a = (await snap(p)).frames ?? 0; await sleep(ms); return ((await snap(p)).frames ?? 0) - a; };
const pong = async (p) => {
  const before = (await snap(p)).channels?.at(-1)?.pongs ?? 0;
  await p.page.evaluate(() => window.__rtc.ping()); await sleep(500);
  return ((await snap(p)).channels?.at(-1)?.pongs ?? 0) > before;
};
const video = (s) => s.pcs?.[0] ?? {};
const audio = (s) => s.pcs?.[1] ?? {};

const consoleErrors = [];
const out = { scenario, checks: {}, consoleErrors };
const check = (name, pass) => { out.checks[name] = Boolean(pass); };

const A = await open("launch");
out.A = await until(A, (s) => s.frames >= 30 && s.channels?.some((c) => c.open), 25000);
check("first page streams through the real client", out.A.frames >= 30);

const B = await open("resume-mic");
out.B = await until(B, (s) => s.frames >= 30 && s.channels?.some((c) => c.open), 25000);
check("the page that resumes decodes", out.B.frames >= 30);
check("its launch screen gives way to the stream", (await until(B, (s) => s.loader === false, 10000)).loader === false);
const offers = scenario === "takeover" ? 2 : 3;
check(`it applied ${offers} video offers on one peer connection`, video(out.B).offers === offers && out.B.pcs.length === 2);
check("it was given exactly one input channel, and it is open", out.B.channels.length === 1 && out.B.channels[0].open && !out.B.channels[0].closed);
check("its input channel reaches the host", await pong(B));
check("one hidden audio element", out.B.audioElements === 1);
check("each track arrived once (the second offer is no new stream)", video(out.B).tracks?.length === 1 && audio(out.B).tracks?.length === 1);
check("the mic slot is still claimed after the second audio offer", audio(out.B).mids?.some((m) => m.includes(":sendonly")));

// The microphone: the control lives in the session menu.
await B.page.mouse.move(640, 360);
await B.page.locator('[aria-label="Open menu"], [aria-label="Session menu"]').first().click({ timeout: 5000 }).catch(() => {});
await B.page.locator('[aria-label="Microphone"]').click({ timeout: 5000 }).catch((e) => { out.micClick = String(e).slice(0, 160); });
out.B_mic = await until(B, (s) => s.micHot, 6000);
check("the microphone turns on", out.B_mic.micHot);

out.A_after = await snap(A);
check("the displaced page was told (4410)", out.A_after.wsCloses?.includes(4410));
check("the displaced page says so", out.A_after.banners?.some((b) => /moved to another tab/i.test(b)));
check("the displaced page stops receiving", (await flowing(A)) === 0);
// One mint so far: the resuming page's. The displaced page asked for none.
check("the displaced page asks for no new token", mints === 1);

await A.browser.close(); await sleep(3000);
check("the stream survives the first page leaving", (await flowing(B)) > 20);
check("input still works after the first page left", await pong(B));
out.B_final = await snap(B);
// Past here pages that own the session close: the host seeing them go is expected.
out.done = new Date().toISOString();
check("no page error", A.errors.length === 0 && B.errors.length === 0);
out.errors = [...A.errors, ...B.errors];

// A plain resume carries no launch state: what the page that really takes over shows.
const C = await open("resume");
out.C = await until(C, (s) => s.frames >= 30, 25000);
await C.page.mouse.move(640, 360);
await C.page.locator('[aria-label="Open menu"], [aria-label="Session menu"]').first().click({ timeout: 5000 }).catch(() => {});
out.C_menu = await snap(C);
out.plainResumeMicButton = out.C_menu.micButton;
await C.browser.close();
await B.browser.close();
console.log(JSON.stringify(out, null, 1));
process.exit(0);
