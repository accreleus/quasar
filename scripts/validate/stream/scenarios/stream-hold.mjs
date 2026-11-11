// Scenario stream-hold: sign in, launch the reference app from the game page in bench mode,
// wait for the first frame, hold, stop the session. Reads only what the product exposes
// (window.__qBench, the DOM, the API); it wraps no browser API and changes no preference.
//
// ctx (the runner builds it; the tests pass a fake): { baseUrl, params, rec, api, creds, client,
// thresholds, secrets, now(), clockOffset(), sleep(ms) }.
// Returns { status, reason?, criteria, clock_offset_ms }; the runner owns the sessions (ctx.launches) and their cleanup.
import { benchRows } from "../benchfold.mjs";

const TERMINAL = ["stopped", "failed"];

export const name = "stream-hold";
// The budget asked of a launched stream. The product publishes none; spec #1271 chose 30 s.
export const FIRST_FRAME_LIMIT_S = 30;
const SHOT_EVERY_S = 10;
const SESSION_URL = /\/app\/session\/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})(?:[/?#]|$)/i;
const CODEC_LABEL = { h264: "H.264", h265: "HEVC", av1: "AV1", pyrowave: "Auto" };
// The bench callback seeing fewer frames than the video decoded means the instrument, not the stream, set the cadence (#1304).
const INSTRUMENT_MIN_SHARE = 0.9;

// Steam's sign-in QR, as fractions of the picture (measured on the lab; the same box the lab proofs cover).
const QR = { x: 0.14, y: 0.185, w: 0.22, h: 0.545 };

const finite = (x) => typeof x === "number" && Number.isFinite(x);
const mean = (xs) => xs.reduce((a, b) => a + b, 0) / xs.length;
const sd = (xs) => Math.sqrt(mean(xs.map((x) => (x - mean(xs)) ** 2)));
const shown = (v) => JSON.stringify(v)?.slice(0, 40);
// A criterion that was not judged has no observed value (the validator's rule).
const crit = (id, pass, observed, expected) => ({ id, pass, observed: pass === null ? null : observed, expected });

/** What a plan entry's params may hold; all but `app` and `bench`/`cover_qr` have a default (docs/testing-e2e-suite.md lists them). */
export const PARAM_KEYS = ["transport", "codec", "profile", "hold_s", "app", "bench", "cover_qr"];

/** Why `p` cannot be run, or null. The runner asks before it opens a page. */
export function check(p) {
  const stray = Object.keys(p).filter((k) => !PARAM_KEYS.includes(k));
  if (stray.length) return `params has a key stream-hold does not define: ${stray.map((k) => JSON.stringify(k.slice(0, 40))).join(", ")} (it defines ${PARAM_KEYS.join(", ")})`;
  if (!["webrtc", "webtransport"].includes(p.transport)) return `transport must be webrtc or webtransport, got ${shown(p.transport)}`;
  if (!Object.hasOwn(CODEC_LABEL, p.codec)) return `codec must be one of ${Object.keys(CODEC_LABEL).join(", ")}, got ${shown(p.codec)}`;
  if (typeof p.profile !== "string" || !/^\d+p\d+$/.test(p.profile)) return `profile must look like 1080p60, got ${shown(p.profile)}`;
  if (!finite(p.hold_s) || p.hold_s <= 0) return `hold_s must be a positive number of seconds, got ${shown(p.hold_s)}`;
  if (typeof p.app !== "string" || !p.app) return `app must be a catalog name, got ${shown(p.app)}`;
  return null;
}

const overlaps = (w, hidden) => hidden.some(([a, b]) => w.t_start_ms < b && w.t_end_ms > a);

/** Windows after the warm-up, on the windows' own clock (the runner's clock may differ from the browser's), minus any the page was hidden for. */
export function observeWindows(windows, warmupS, hidden = []) {
  const first = windows.find((w) => w && w.n > 0);
  if (!first) return [];
  return windows.filter((w) => w && finite(w.t_start_ms) && finite(w.t_end_ms) && w.t_start_ms >= first.t_start_ms + warmupS * 1000 && !overlaps(w, hidden));
}

/** One-second bench windows in which no frame reached the bench callback. A stall shorter than a window is invisible here. */
export function framelessSeconds(obs) {
  return obs.filter((w) => w.n === 0 && !(w.no_image > 0)).length;
}

/** Every judged window states how many frames it saw; one that does not makes the count unknown, not zero. */
const countsKnown = (obs) => obs.every((w) => finite(w.n) && w.n >= 0);

/** missing / (decoded + missing) over the windows: the indices that were due. Any absent, negative or non-finite count makes it null. */
export function missingRate(obs) {
  let missing = 0;
  let decoded = 0;
  for (const w of obs) {
    if (![w.missing_indices, w.decoded].every((x) => finite(x) && x >= 0)) return null;
    missing += w.missing_indices;
    decoded += w.decoded;
  }
  return decoded + missing > 0 ? missing / (decoded + missing) : null;
}

/** The control plane's hitch rule on the bench ring: one-second groups with at least `present_min_samples` intervals, sd against the hitch threshold. */
export function hitchCount(frames, fromMs, toMs, hidden, t) {
  const ps = frames.map((f) => f?.present_ms).filter((p) => finite(p) && p >= fromMs && p <= toMs).sort((a, b) => a - b);
  const by = new Map();
  for (let i = 1; i < ps.length; i += 1) {
    if (hidden.some(([a, b]) => ps[i - 1] < b && ps[i] > a)) continue;
    const k = Math.floor(ps[i] / 1000);
    by.set(k, [...(by.get(k) ?? []), ps[i] - ps[i - 1]]);
  }
  const sds = [...by.values()].filter((iv) => iv.length >= t.present_min_samples).map(sd);
  return { judged: sds.length, hitches: sds.filter((s) => s >= t.hitch_sd_ms).length };
}

/** The share of the frames the video element decoded over [a, b] that the bench callback saw; null when there is no video to compare with. */
export function instrumentShare(frames, videoSamples, a, b) {
  const v = videoSamples.filter((s) => finite(s.videoFrames) && s.t >= a && s.t <= b);
  if (v.length < 2) return null;
  const decoded = v.at(-1).videoFrames - v[0].videoFrames;
  if (!(decoded > 0)) return null;
  return frames.filter((f) => f.present_ms >= v[0].t && f.present_ms <= v.at(-1).t).length / decoded;
}

function transportObserved(s) {
  if (s.transport_fallback_reason) return `${s.transport} (the client fell back to webrtc: ${s.transport_fallback_reason})`;
  if (s.transport_reason) return `${s.transport} (stream plan: ${s.transport_reason})`;
  return String(s.transport);
}

export function judge({ params, session, windows, frames, firstFrameS, stats, hidden = [], videoSamples = [], thresholds: T }) {
  const obs = observeWindows(windows, T.warmup_exclude_s, hidden);
  const warm = `${T.warmup_exclude_s} s warm-up`;
  const s = session;
  const codec = !s ? null : s.pyrowave?.active || s.pyrowave?.used ? ["pyrowave", "pyrowave.active/used"] : s.negotiated_codec ? [String(s.negotiated_codec), "negotiated_codec"] : s.stream?.codec ? [String(s.stream.codec), "stream.codec"] : null;
  const [, h, fps] = /^(\d+)p(\d+)$/.exec(params.profile ?? "") ?? [];
  const stream = s?.stream;

  const rate = missingRate(obs);
  const spanFrom = obs[0]?.t_start_ms;
  const spanTo = obs.at(-1)?.t_end_ms;
  const cad = obs.length ? hitchCount(frames, spanFrom, spanTo, hidden, T) : null;
  const share = obs.length ? instrumentShare(frames, videoSamples, spanFrom, spanTo) : null;
  let cadence = crit("present_cadence", null, null, `no ${warm} window had ${T.present_min_samples} present intervals`);
  if (cad?.judged && share !== null && share < INSTRUMENT_MIN_SHARE) {
    cadence = crit("present_cadence", null, null, `not judged: the bench callback saw ${Math.round(share * 100)}% of the frames the video decoded, so the instrument set the cadence (issue #1304)`);
  } else if (cad?.judged) {
    cadence = crit("present_cadence", cad.hitches < T.hitch_min_samples, cad.hitches, `< ${T.hitch_min_samples} one-second windows with present-interval sd >= ${T.hitch_sd_ms} ms, after the ${warm}`);
  }
  const frameless = framelessSeconds(obs);

  return [
    crit("first_frame_s", finite(firstFrameS) && firstFrameS >= 0 ? firstFrameS <= FIRST_FRAME_LIMIT_S : firstFrameS === null ? false : null, finite(firstFrameS) && firstFrameS >= 0 ? firstFrameS : null, `<= ${FIRST_FRAME_LIMIT_S}`),
    crit("marker_decoded", stats ? stats.decoded > 0 : null, stats?.decoded ?? null, "> 0 marker frames decoded"),
    crit("transport_as_asked", s ? s.transport === params.transport && !s.transport_fallback_reason : null, s ? transportObserved(s) : null, params.transport),
    crit("codec_as_asked", codec ? codec[0] === params.codec : null, codec ? `${codec[0]} (${codec[1]})` : null, params.codec),
    crit("profile_as_asked", stream && h ? stream.height === +h && stream.fps === +fps : null, stream ? `${stream.height}p${stream.fps}` : null, `${params.profile} (the launch size, not external_*)`),
    crit("frameless_seconds", obs.length && countsKnown(obs) ? frameless === 0 : null, frameless, `0 one-second bench windows without a frame, after the ${warm}`),
    {
      id: "missing_indices_rate",
      pass: null,
      observed: null,
      expected: `not judged: no limit calibrated on the marker app yet${rate === null ? " (no usable counts)" : `; measured ${(rate * 100).toFixed(2)} % over ${obs.length} windows`}`,
    },
    cadence,
  ];
}

/** pass needs the first frame and no failed criterion; a first frame that could not be judged is an error with its reason. */
export function verdict({ status, reason, criteria = [] }) {
  if (status) return { status, ...(reason ? { reason } : {}) };
  if (criteria.some((c) => c.pass === false)) return { status: "fail" };
  if (criteria.find((c) => c.id === "first_frame_s")?.pass === true) return { status: "pass" };
  return { status: "error", reason: "no first frame could be judged" };
}

/** A screenshot with a black box over the QR a launcher shows; no stream surface to place it on means no shot at all. */
export async function shotCovered(page, rec, label, cover) {
  if (!cover) return rec.shot(page, label);
  const placed = await page.evaluate((q) => {
    const el = [...document.querySelectorAll(".session-video")].find((e) => e.videoWidth || e.tagName === "CANVAS");
    if (!el) return false;
    const r = el.getBoundingClientRect();
    const ar = +el.dataset.compositorW / +el.dataset.compositorH || 16 / 9;
    const mode = el.dataset.scalingMode || "contain";
    const k = mode === "stretch" ? null : mode === "cover" ? Math.max(r.width / ar, r.height) : Math.min(r.width / ar, r.height);
    const pw = k ? k * ar : r.width;
    const ph = k ?? r.height;
    const d = document.createElement("div");
    d.id = "__qrcover";
    d.style.cssText = `position:fixed;z-index:2147483647;pointer-events:none;background:#000;left:${Math.floor(r.left + (r.width - pw) / 2 + pw * q.x)}px;top:${Math.floor(r.top + (r.height - ph) / 2 + ph * q.y)}px;width:${Math.ceil(pw * q.w)}px;height:${Math.ceil(ph * q.h)}px`;
    document.documentElement.appendChild(d);
    return true;
  }, QR);
  try {
    if (placed) await rec.shot(page, label);
    else rec.step("shot-skipped", { label, why: "no stream surface to place the QR cover on" });
  } finally {
    await page.evaluate(() => document.getElementById("__qrcover")?.remove()).catch(() => {});
  }
}

export default {
  name,
  check,
  async run(page, ctx) {
    const { params, rec, api, client, now, sleep } = ctx;
    const skipped = (reason) => ({ status: "skipped", reason, criteria: [], sessions: [], clock_offset_ms: null });
    if (params.transport === "webtransport" && !client.webtransport) return skipped("this browser has no WebTransport");
    if (params.codec === "pyrowave" ? !client.webgpu : client.decode?.[params.codec]?.supported === false) {
      return skipped(`this client cannot decode ${params.codec}`);
    }

    const cover = params.cover_qr ?? params.app === "Steam";
    const shot = (label) => shotCovered(page, rec, label, cover);
    const tb = () => now() + (ctx.clockOffset?.() ?? 0); // the browser's clock, which the bench windows are stamped on
    ctx.refused = null;

    // The sessions this run owns are the runner's to track and stop (ctx.launches): a page address never adds one.
    const origin = new URL(ctx.baseUrl).origin;
    const pending = [];
    page.on("response", (r) => {
      const u = new URL(r.url());
      if (u.origin !== origin || r.status() < 400 || !u.pathname.startsWith("/v1/")) return;
      const method = r.request().method();
      pending.push((async () => {
        const body = (await r.text().catch(() => "")).slice(0, 300);
        rec.pageStep("api-error", { method, path: u.pathname, status: r.status(), body });
        if (method === "POST" && u.pathname === "/v1/sessions") ctx.refused = `the launch was refused: ${r.status()} ${body.trim()}`;
      })());
    });

    let result;
    try {
      await signIn(page, ctx);
      rec.step("signed-in");
      const app = (await api.apps()).find((a) => a.name === params.app);
      if (!app) return skipped(`app "${params.app}" is not in this user's catalog`);

      const switches = [params.transport === "webtransport" && "transport=webtransport", params.codec === "pyrowave" && "codec=pyrowave"].filter(Boolean);
      await page.goto(`${ctx.baseUrl}/app/game/${encodeURIComponent(app.id)}${switches.length ? `?${switches.join("&")}` : ""}`, { waitUntil: "domcontentloaded" });
      const tClick = await pickAndPlay(page, ctx);
      rec.step("play-clicked", { app: params.app });
      const sid = await waitForSession(page, ctx);
      const row0 = await api.session(sid);
      if (!row0 || row0.id !== sid) throw new Error("the API no longer returns the session this run launched");
      rec.step("session-route", { session: sid });

      const frames = [];
      let lastPresent = 0;
      const pullFrames = async () => {
        const dump = await page.evaluate(() => window.__qBench?.dump() ?? null);
        for (const f of dump?.frames ?? []) if (f.present_ms > lastPresent) { frames.push(f); lastPresent = f.present_ms; }
        return dump;
      };
      // One look at the page per second: is a frame showing, is the page visible. The hidden spans leave the judged span.
      const hidden = [];
      const videoSamples = [];
      let hiddenFrom = null;
      const look = async () => {
        const p = await probe(page);
        const t = tb();
        videoSamples.push({ t, videoFrames: p.videoFrames });
        if (p.visible !== "visible") hiddenFrom ??= t;
        else if (hiddenFrom !== null) { hidden.push([hiddenFrom, t]); hiddenFrom = null; }
        return p;
      };

      let firstFrameAt = null;
      const deadline = tClick + (FIRST_FRAME_LIMIT_S + 90) * 1000; // cold hosts launch slowly; the criterion still judges the limit
      while (now() < deadline && firstFrameAt === null) {
        const p = await look();
        if (p.frames > 0 || p.videoFrames > 0 || p.revealed) firstFrameAt = now();
        else await sleep(250);
      }
      const firstFrameS = firstFrameAt === null ? null : (firstFrameAt - tClick) / 1000;
      rec.step("first-frame", { first_frame_s: firstFrameS });
      await shot(firstFrameAt === null ? "no-frame" : "first-frame");

      const holdUntil = firstFrameAt === null ? now() : firstFrameAt + params.hold_s * 1000;
      let nextShot = (firstFrameAt ?? now()) + SHOT_EVERY_S * 1000;
      while (now() < holdUntil) {
        await sleep(Math.min(1000, holdUntil - now()));
        const p = await look();
        if (now() >= nextShot) {
          nextShot += SHOT_EVERY_S * 1000;
          await shot(`t${Math.round((now() - firstFrameAt) / 1000)}s`);
          await pullFrames();
          rec.step("held", { stats: p });
        }
      }
      if (hiddenFrom !== null) hidden.push([hiddenFrom, tb()]);
      const dump = await pullFrames();
      await shot("end");
      const windows = dump?.windows ?? [];
      const stats = await page.evaluate(() => window.__qBench?.stats() ?? null);
      rec.json("bench-windows.json", { windows });
      rec.json("bench-frames.json", { frames });
      rec.jsonl("metrics.jsonl", benchRows(windows, 0));

      const row = await api.session(sid);
      if (!row || row.id !== sid) throw new Error("the API no longer returns the session this run started");
      if (row.app_id !== undefined && row.app_id !== app.id) throw new Error("the launched session is for another app than the one asked for");
      rec.json("session.json", { session: row });
      const known = [...windows].reverse().find((w) => w?.offset_unknown === false && Number.isFinite(w.offset_ms));
      result = {
        criteria: judge({ params, session: row, windows, frames, firstFrameS, stats, hidden, videoSamples, thresholds: ctx.thresholds }),
        clock_offset_ms: known ? known.offset_ms : null,
      };
      // A session that is already over before this run's own stop was not held: whatever the criteria say.
      if (TERMINAL.includes(row.state)) result = { ...result, status: "error", reason: `the session ended during the hold (it was ${row.state} before this run stopped it)` };
      rec.step("hold-done");
    } catch (err) {
      rec.step("error", { message: String(err?.message ?? err) });
      result = { status: "error", reason: String(err?.message ?? err), criteria: [], clock_offset_ms: null };
    } finally {
      await Promise.allSettled(pending);
    }
    return { ...result, ...verdict(result) };
  },
};

async function signIn(page, ctx) {
  await page.goto(ctx.baseUrl, { waitUntil: "domcontentloaded" });
  await page.evaluate((bench) => {
    for (const k of Object.keys(localStorage)) if (k.startsWith("quasar.auth.")) localStorage.removeItem(k);
    if (bench) localStorage.setItem("quasar.bench", "1");
  }, ctx.params.bench !== false);
  await page.goto(`${ctx.baseUrl}/login`, { waitUntil: "domcontentloaded" });
  await page.fill("#login-email", ctx.creds.email);
  await page.fill("#login-password", ctx.creds.password);
  await page.click('button[type="submit"]');
  await page.waitForURL((u) => !new URL(u).pathname.startsWith("/login"), { timeout: 20000 });
  // The page's own bearer is a secret of this run too: removed from everything written, as a value.
  const bearer = await page.evaluate(() => localStorage.getItem("quasar.auth.token") ?? sessionStorage.getItem("quasar.auth.token"));
  if (typeof bearer === "string" && bearer) {
    ctx.secrets?.add(bearer);
    ctx.rec.release(); // only now can what the page said be written through the redactor
  }
}

/** Waits for the control to be enabled, then clicks it as a script, not as a pointer (an untrusted click cannot start full screen or take the pointer lock). Returns the time of the click. */
async function press(locator, ctx, what) {
  const stop = ctx.now() + 30000;
  for (;;) {
    const ready = await locator.first().evaluate((el) => !el.disabled && el.getAttribute("aria-disabled") !== "true").catch(() => false);
    if (ready) {
      const t = ctx.now();
      await locator.first().evaluate((el) => el.click());
      return t;
    }
    if (ctx.now() > stop) throw new Error(`${what} never became available on the page`);
    await ctx.sleep(300);
  }
}

/**
 * Asks for the codec, frame rate and resolution through the game page's own Launch options ("Change"
 * opens the sheet), then Play now. No preference of the user is touched. Returns the time of the Play click.
 */
async function pickAndPlay(page, ctx) {
  const { params } = ctx;
  const [, height, fps] = /^(\d+)p(\d+)$/.exec(params.profile);
  if (!new URL(page.url(), ctx.baseUrl).pathname.startsWith("/app/game/")) {
    throw new Error(`the page did not open the game page for ${params.app} (it is at ${new URL(page.url(), ctx.baseUrl).pathname}; an account with the Classic layout on is sent to the library instead)`);
  }
  await press(page.getByRole("button", { name: "Change", exact: true }), ctx, "Change (Launch options)");
  const sheet = page.getByRole("dialog", { name: /^Launch options/ });
  for (const [group, label] of [["Codec", CODEC_LABEL[params.codec]], ["Frame rate", `${fps} fps`], ["Resolution", `${height}p`]]) {
    await press(sheet.getByRole("radiogroup", { name: group }).getByRole("radio", { name: new RegExp(`^${label.replace(/\./g, "\\.")}`) }), ctx, `${group} "${label}"`);
  }
  return press(sheet.getByRole("button", { name: /^Play now/ }), ctx, "Play now");
}

/** The session id in the page's address, but only one this run launched (a 201 of its own page); any other is an error and is left alone. */
async function waitForSession(page, ctx) {
  const stop = ctx.now() + 90000;
  while (ctx.now() < stop) {
    const m = SESSION_URL.exec(new URL(page.url(), ctx.baseUrl).pathname);
    if (m) {
      await ctx.launches.settle();
      if (ctx.launches.owned.has(m[1])) return m[1];
      throw new Error("the page went to a session this run did not launch, so it was left alone");
    }
    if (ctx.refused) throw new Error(ctx.refused);
    await ctx.sleep(300);
  }
  throw new Error("the Play click never reached a session page");
}

// The loader keeps is-locking / is-streaming on a failed or ended session (SessionLoader.tsx adds is-failed and is-over beside it), so those are not a frame.
export const REVEALED = ".sl-root.is-locking:not(.is-failed):not(.is-over), .sl-root.is-streaming:not(.is-failed):not(.is-over)";

/** What the page shows right now: bench frames, the playing <video> (WebRTC), the page's own reveal of the stream (either transport), and whether it is visible. */
const probe = (page) =>
  page.evaluate((sel) => ({
    frames: window.__qBench?.stats().frames ?? 0,
    videoFrames: document.querySelector("video.session-video:not([hidden])")?.getVideoPlaybackQuality?.().totalVideoFrames ?? 0,
    revealed: !!document.querySelector(sel),
    visible: document.visibilityState,
    focused: document.hasFocus?.(),
  }), REVEALED);
