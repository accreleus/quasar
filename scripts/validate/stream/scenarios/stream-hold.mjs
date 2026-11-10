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
    crit("freeze_count", judged ? freezes(obs) === 0 : null, judged ? freezes(obs) : null, "0 after the warm-up"),
    crit("missing_indices_rate", judged && rate !== null ? rate <= MISSING_RATE_LIMIT : null, rate, `<= ${MISSING_RATE_LIMIT} after the warm-up`),
    crit("present_cadence", cadence !== null ? cadence < minHitch : null, cadence, `< ${minHitch} windows with present-interval sd >= ${hitchSd} ms`),
  ];
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

    const sessions = new Set();
    const before = new Set((await api.liveSessions().catch(() => [])).map((s) => s.id));
    let sid = null;
    let result;
    try {
      await signIn(page, ctx);
      rec.step("signed-in");
      const apps = await api.apps();
      const app = apps.find((a) => a.name === params.app);
      if (!app) return skipped(`app "${params.app}" is not in this user's catalog`);

      const switches = [params.transport === "webtransport" && "transport=webtransport", params.codec === "pyrowave" && "codec=pyrowave"].filter(Boolean);
      await page.goto(`${ctx.baseUrl}/app${switches.length ? `?${switches.join("&")}` : ""}`, { waitUntil: "domcontentloaded" });
      const tClick = now();
      await page.locator(`button[aria-label="Play ${params.app}"]`).first().evaluate((el) => el.click());
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
      if (firstFrameAt !== null) await rec.shot(page, "first-frame");

      const holdUntil = (firstFrameAt ?? now()) + params.hold_s * 1000;
      let nextShot = (firstFrameAt ?? now()) + SHOT_EVERY_S * 1000;
      while (now() < holdUntil) {
        await sleep(Math.min(1000, holdUntil - now()));
        if (now() >= nextShot) {
          nextShot += SHOT_EVERY_S * 1000;
          await rec.shot(page, `t${Math.round((now() - firstFrameAt) / 1000)}s`);
          await pullFrames();
          rec.step("held", { stats: await probe(page) });
        }
      }
      const dump = await pullFrames();
      await rec.shot(page, "end");
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
      // Every exit path: the session this run started is stopped, whatever else went wrong.
      const started = [...sessions, ...(await api.liveSessions().catch(() => [])).map((s) => s.id).filter((id) => !before.has(id))];
      for (const id of new Set(started)) {
        sessions.add(id);
        await api.stop(id).catch((e) => rec.step("stop-failed", { session: id, message: String(e?.message ?? e) }));
        rec.step("stopped", { session: id });
      }
    }
    const failed = result.criteria?.some((c) => c.pass === false);
    return { status: result.status ?? (failed ? "fail" : "pass"), sessions: [...sessions], ...result };
  },
};

async function signIn(page, ctx) {
  await page.goto(ctx.baseUrl, { waitUntil: "domcontentloaded" });
  await page.evaluate(() => {
    for (const k of Object.keys(localStorage)) if (k.startsWith("quasar.auth.")) localStorage.removeItem(k);
    sessionStorage.clear();
    localStorage.setItem("quasar.bench", "1");
  });
  await page.goto(`${ctx.baseUrl}/login`, { waitUntil: "domcontentloaded" });
  await page.fill("#login-email", ctx.creds.email);
  await page.fill("#login-password", ctx.creds.password);
  await page.click('button[type="submit"]');
  await page.waitForURL((u) => !new URL(u).pathname.startsWith("/login"), { timeout: 20000 });
}

async function waitForSession(page, ctx) {
  const stop = ctx.now() + 90000;
  while (ctx.now() < stop) {
    const url = page.url();
    const m = /\/app\/session\/([0-9a-f-]{36})/.exec(url);
    if (m) return m[1];
    if (/\/app\/game\//.test(url)) await page.getByRole("button", { name: /^(play|resume)\b/i }).first().evaluate((el) => el.click()).catch(() => {});
    await ctx.sleep(300);
  }
  throw new Error("the Play click never reached a session page");
}

const probe = (page) =>
  page.evaluate(() => ({
    frames: window.__qBench?.stats().frames ?? 0,
    videoFrames: document.querySelector("video")?.getVideoPlaybackQuality?.().totalVideoFrames ?? 0,
  }));
