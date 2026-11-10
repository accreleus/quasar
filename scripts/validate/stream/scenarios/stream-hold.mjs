// Scenario stream-hold: sign in, launch the reference app from the library in bench mode,
// wait for the first frame, hold, stop the session. Reads only what the product exposes
// (window.__qBench, the DOM, the API); it wraps no browser API.
//
// ctx (the runner builds it; the tests pass a fake): { baseUrl, params, rec, api, creds, client,
// thresholds, now(), sleep(ms) }. Returns { status, reason?, criteria, sessions, clock_offset_ms }.
import { benchRows } from "../benchfold.mjs";

export const name = "stream-hold";
export const WARMUP_S = 10; // the first seconds of a stream judder by construction (testing-bench-mode.md)
export const FIRST_FRAME_LIMIT_S = 30;
export const MISSING_RATE_LIMIT = 0.01;
const SHOT_EVERY_S = 10;
const TERMINAL = ["stopped", "failed"];

// Steam's sign-in QR, as fractions of the 16:9 picture (measured on the lab; the same box the lab proofs cover).
const QR = { x: 0.14, y: 0.185, w: 0.22, h: 0.545 };

const mean = (xs) => xs.reduce((a, b) => a + b, 0) / xs.length;
const sd = (xs) => Math.sqrt(mean(xs.map((x) => (x - mean(xs)) ** 2)));
const crit = (id, pass, observed, expected) => ({ id, pass, observed: pass === null ? null : observed, expected });

/** Windows after the warm-up, on the windows' own clock (the runner's clock may differ from the browser's). */
export function observeWindows(windows) {
  const first = windows.find((w) => w.n > 0);
  return first ? windows.filter((w) => w.t_start_ms >= first.t_start_ms + WARMUP_S * 1000) : [];
}

/** Runs of consecutive seconds with no frame at all (a second of undecodable frames is not a freeze). */
export function freezes(windows) {
  let runs = 0;
  let inRun = false;
  for (const w of windows) {
    const frameless = w.n === 0 && !w.undecoded && !w.no_image;
    if (frameless && !inRun) runs += 1;
    inRun = frameless;
  }
  return runs;
}

/** One-second groups of present intervals whose spread reaches the hitch threshold. */
export function hitchWindows(frames, fromMs, hitchSdMs) {
  const bySecond = new Map();
  const ps = frames.map((f) => f.present_ms).filter((p) => Number.isFinite(p) && p >= fromMs);
  for (let i = 1; i < ps.length; i += 1) {
    const k = Math.floor(ps[i] / 1000);
    bySecond.set(k, [...(bySecond.get(k) ?? []), ps[i] - ps[i - 1]]);
  }
  return [...bySecond.values()].filter((iv) => iv.length >= 2 && sd(iv) >= hitchSdMs).length;
}

export function judge({ params, session, windows, frames, firstFrameS, located, thresholds }) {
  const obs = observeWindows(windows);
  const judged = located && obs.length > 0;
  const missing = obs.reduce((a, w) => a + (w.missing_indices ?? 0), 0);
  const shown = obs.reduce((a, w) => a + (w.n ?? 0), 0);
  const rate = shown + missing > 0 ? missing / (shown + missing) : null;
  const codec = session ? (session.pyrowave?.active || session.pyrowave?.used ? "pyrowave" : String(session.negotiated_codec ?? session.stream?.codec ?? "").replace("hevc", "h265") || null) : null;
  const [, h, fps] = /^(\d+)p(\d+)/.exec(params.profile ?? "") ?? [];
  const stream = session?.stream;
  const hitchSd = thresholds.hitch_sd_ms;
  const minHitch = thresholds.hitch_min_samples;
  const cadence = judged ? hitchWindows(frames, obs[0].t_start_ms, hitchSd) : null;
  return [
    crit("first_frame_s", firstFrameS === null ? null : firstFrameS <= FIRST_FRAME_LIMIT_S, firstFrameS, `<= ${FIRST_FRAME_LIMIT_S}`),
    crit("marker_decoded", located === null ? null : located, located, "true"),
    crit("transport_as_asked", session ? session.transport === params.transport : null, session?.transport ?? null, params.transport),
    crit("codec_as_asked", codec ? codec === params.codec : null, codec, params.codec),
    crit("profile_as_asked", stream && h ? stream.height === +h && stream.fps === +fps : null, stream ? `${stream.height}p${stream.fps}` : null, params.profile),
    crit("freeze_count", obs.length ? freezes(obs) === 0 : null, obs.length ? freezes(obs) : null, "0 after the warm-up"),
    crit("missing_indices_rate", judged && rate !== null ? rate <= MISSING_RATE_LIMIT : null, rate, `<= ${MISSING_RATE_LIMIT} after the warm-up`),
    crit("present_cadence", cadence !== null ? cadence < minHitch : null, cadence, `< ${minHitch} windows with present-interval sd >= ${hitchSd} ms`),
  ];
}

/** pass needs a criterion that held and none that failed; if nothing could be judged the run says so rather than pass. */
export function verdict({ status, reason, criteria = [] }) {
  if (status) return { status, ...(reason ? { reason } : {}) };
  if (criteria.some((c) => c.pass === false)) return { status: "fail" };
  if (criteria.some((c) => c.pass === true)) return { status: "pass" };
  return { status: "error", reason: "nothing could be judged" };
}

/** A screenshot with a black box over the QR a launcher shows; no stream surface to place it on means no shot at all. */
export async function shotCovered(page, rec, label, cover) {
  if (!cover) return rec.shot(page, label);
  const placed = await page.evaluate((q) => {
    const el = [...document.querySelectorAll(".session-video")].find((e) => e.videoWidth || e.tagName === "CANVAS");
    if (!el) return false;
    const r = el.getBoundingClientRect();
    const ar = 16 / 9;
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
  async run(page, ctx) {
    const { params, rec, api, client, now, sleep } = ctx;
    const skipped = (reason) => ({ status: "skipped", reason, criteria: [], sessions: [], clock_offset_ms: null });
    if (params.transport === "webtransport" && !client.webtransport) return skipped("this browser has no WebTransport");
    if (params.codec === "pyrowave" ? !client.webgpu : client.decode?.[params.codec]?.supported === false) {
      return skipped(`this client cannot decode ${params.codec}`);
    }

    const cover = params.cover_qr ?? params.app === "Steam";
    const shot = (label) => shotCovered(page, rec, label, cover);
    ctx.refused = null;
    page.on("response", async (r) => {
      const { pathname } = new URL(r.url());
      if (r.status() < 400 || !pathname.startsWith("/v1/")) return;
      const body = (await r.text().catch(() => "")).slice(0, 300);
      rec.step("api-error", { method: r.request().method(), path: pathname, status: r.status(), body });
      if (r.request().method() === "POST" && pathname === "/v1/sessions") ctx.refused = `the launch was refused: ${r.status()} ${body.trim()}`;
    });
    const sessions = new Set();
    const before = new Set((await api.liveSessions().catch(() => [])).map((s) => s.id));
    let sid = null;
    let wasClassic = true;
    let result;
    try {
      await signIn(page, ctx);
      rec.step("signed-in");
      const apps = await api.apps();
      const app = apps.find((a) => a.name === params.app);
      if (!app) return skipped(`app "${params.app}" is not in this user's catalog`);

      const switches = [params.transport === "webtransport" && "transport=webtransport", params.codec === "pyrowave" && "codec=pyrowave"].filter(Boolean);
      await page.goto(`${ctx.baseUrl}/app${switches.length ? `?${switches.join("&")}` : ""}`, { waitUntil: "domcontentloaded" });
      const tClick = await pickAndPlay(page, ctx, async () => {
        wasClassic = await api.classicLayout();
        if (!wasClassic) await api.setClassicLayout(true);
      });
      rec.step("play-clicked", { app: params.app });
      sid = await waitForSession(page, ctx);
      sessions.add(sid);
      rec.step("session-route", { session: sid });

      const frames = [];
      let lastPresent = 0;
      const pullFrames = async () => {
        const dump = await page.evaluate(() => window.__qBench?.dump() ?? null);
        for (const f of dump?.frames ?? []) if (f.present_ms > lastPresent) { frames.push(f); lastPresent = f.present_ms; }
        return dump;
      };
      let firstFrameAt = null;
      const deadline = tClick + (FIRST_FRAME_LIMIT_S + 90) * 1000; // cold hosts launch slowly; the criterion still judges the limit
      while (now() < deadline && firstFrameAt === null) {
        const p = await probe(page);
        if (p.frames > 0 || p.videoFrames > 0) firstFrameAt = now();
        else await sleep(500);
      }
      const firstFrameS = firstFrameAt === null ? null : (firstFrameAt - tClick) / 1000;
      rec.step("first-frame", { first_frame_s: firstFrameS });
      if (firstFrameAt !== null) await shot("first-frame");

      const holdUntil = (firstFrameAt ?? now()) + params.hold_s * 1000;
      let nextShot = (firstFrameAt ?? now()) + SHOT_EVERY_S * 1000;
      while (now() < holdUntil) {
        await sleep(Math.min(1000, holdUntil - now()));
        if (now() >= nextShot) {
          nextShot += SHOT_EVERY_S * 1000;
          await shot(`t${Math.round((now() - firstFrameAt) / 1000)}s`);
          await pullFrames();
          rec.step("held", { stats: await probe(page) });
        }
      }
      const dump = await pullFrames();
      await shot("end");
      const windows = dump?.windows ?? [];
      const stats = await page.evaluate(() => window.__qBench?.stats() ?? null);
      rec.json("bench-windows.json", { windows });
      rec.json("bench-frames.json", { frames });
      rec.jsonl("metrics.jsonl", benchRows(windows, 0));

      const row = await api.session(sid);
      rec.json("session.json", { session: row });
      const known = [...windows].reverse().find((w) => w.offset_unknown === false && Number.isFinite(w.offset_ms));
      result = {
        criteria: judge({ params, session: row, windows, frames, firstFrameS, located: stats ? stats.located : null, thresholds: ctx.thresholds }),
        clock_offset_ms: known ? known.offset_ms : null,
      };
      rec.step("hold-done");
    } catch (err) {
      rec.step("error", { message: String(err?.message ?? err) });
      result = { status: "error", reason: String(err?.message ?? err), criteria: [], clock_offset_ms: null };
    } finally {
      if (!wasClassic) await api.setClassicLayout(false).catch((e) => rec.step("layout-restore-failed", { message: String(e?.message ?? e) }));
      // Every exit path: the session this run started is stopped, whatever else went wrong.
      const started = [...sessions, ...(await api.liveSessions().catch(() => [])).map((s) => s.id).filter((id) => !before.has(id))];
      for (const id of new Set(started)) {
        sessions.add(id);
        await api.stop(id).catch((e) => rec.step("stop-failed", { session: id, message: String(e?.message ?? e) }));
        rec.step("stopped", { session: id });
      }
    }
    return { sessions: [...sessions], ...result, ...verdict(result) };
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
}

const CODEC_LABEL = { h264: "H.264", h265: "HEVC", av1: "AV1", pyrowave: "Auto" };

/** Click as a script, not as a pointer: an untrusted click cannot start full screen or take the pointer lock. Waits for the element to be enabled. */
async function press(locator, ctx, what) {
  const stop = ctx.now() + 30000;
  for (;;) {
    const ready = await locator.first().evaluate((el) => !el.disabled && el.getAttribute("aria-disabled") !== "true").catch(() => false);
    if (ready) return locator.first().evaluate((el) => el.click());
    if (ctx.now() > stop) throw new Error(`${what} never became available on the page`);
    await ctx.sleep(300);
  }
}

/**
 * Asks for the codec, frame rate and resolution through the page's own Launch options (the classic library
 * layout, a per-user setting that `enableClassic` turns on and the caller puts back), then Play now.
 * Returns the time of the Play click.
 */
async function pickAndPlay(page, ctx, enableClassic) {
  const { params } = ctx;
  const [, height, fps] = /^(\d+)p(\d+)/.exec(params.profile ?? "") ?? [];
  if (!height || !CODEC_LABEL[params.codec]) throw new Error(`cannot ask the page for codec ${params.codec} at ${params.profile}`);
  await enableClassic();
  await page.goto(page.url(), { waitUntil: "domcontentloaded" });
  await press(page.locator(`button[aria-label="${params.app}"]`), ctx, `the ${params.app} tile`);
  await press(page.getByRole("button", { name: /^Adjust/ }), ctx, "Adjust (Launch options)");
  for (const [group, label] of [["Codec", CODEC_LABEL[params.codec]], ["Frame rate", `${fps} fps`], ["Resolution", `${height}p`]]) {
    await press(page.getByRole("radiogroup", { name: group }).getByRole("radio", { name: new RegExp(`^${label.replace(/\./g, "\\.")}`) }), ctx, `${group} "${label}"`);
  }
  const t = ctx.now();
  await press(page.getByRole("button", { name: /^Play now/ }), ctx, "Play now");
  return t;
}

async function waitForSession(page, ctx) {
  const stop = ctx.now() + 90000;
  while (ctx.now() < stop) {
    const url = page.url();
    const m = /\/app\/session\/([0-9a-f-]{36})/.exec(url);
    if (m) return m[1];
    if (ctx.refused) throw new Error(ctx.refused);
    if (/\/app\/game\//.test(url)) await page.getByRole("button", { name: /^(play|resume)\b/i }).first().evaluate((el) => el.click()).catch(() => {});
    await ctx.sleep(300);
  }
  throw new Error("the Play click never reached a session page");
}

const probe = (page) =>
  page.evaluate(() => ({
    frames: window.__qBench?.stats().frames ?? 0,
    videoFrames: document.querySelector("video")?.getVideoPlaybackQuality?.().totalVideoFrames ?? 0,
    visible: document.visibilityState,
    focused: document.hasFocus?.(),
  }));
