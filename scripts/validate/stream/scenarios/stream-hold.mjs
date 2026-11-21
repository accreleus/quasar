// Scenario stream-hold: sign in, launch the reference app from the game page in bench mode,
// wait for the first frame, hold, stop the session. Reads only what the product exposes
// (window.__qBench, the DOM, the API); it wraps no browser API and changes no preference.
//
// ctx (the runner builds it; the tests pass a fake): { baseUrl, params, rec, api, creds, client,
// thresholds, secrets, now(), clockOffset(), sleep(ms) }.
// Returns { status, reason?, criteria, clock_offset_ms }; the runner owns the sessions (ctx.launches) and their cleanup.
import { benchRows } from "../benchfold.mjs";
import { cannotRun, CODEC_LABEL, crit, errorReason, FIRST_FRAME_LIMIT_S, finite, firstFrameCriterion, play, probe, REVEALED, shotCovered, shown, signIn, skipped, verdict, watchApiErrors } from "./stream-common.mjs";

export { FIRST_FRAME_LIMIT_S, REVEALED, shotCovered, verdict };

const TERMINAL = ["stopped", "failed"];

export const name = "stream-hold";
const SHOT_EVERY_S = 10;
// The bench callback seeing fewer frames than the video decoded means the instrument, not the stream, set the cadence (#1304).
const INSTRUMENT_MIN_SHARE = 0.9;

const mean = (xs) => xs.reduce((a, b) => a + b, 0) / xs.length;
const sd = (xs) => Math.sqrt(mean(xs.map((x) => (x - mean(xs)) ** 2)));

const STATS_POST = /^\/v1\/sessions\/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\/stats$/i;
// The three manifest keys `review.mjs`'s anomaly rules read from browser telemetry rows.
const STATS_KEYS = ["present_interval_sd_ms", "present_interval_p95_ms", "is_hidden"];

/**
 * Projects the three review-checked cadence/visibility keys from a `POST /v1/sessions/{id}/stats`
 * request body (`{ samples: [{ ts_unix_ms, metrics: { ... } }] }`, web/src/{webrtc,webtransport}/telemetry.ts
 * and control-plane/internal/session/metrics_handler.go). Keeps only finite numbers for STATS_KEYS,
 * never copies free text or unknown keys, and drops samples carrying none of them.
 */
export function statsSamples(postData) {
  let body;
  try { body = JSON.parse(postData); } catch { return []; }
  if (!body || typeof body !== "object" || !Array.isArray(body.samples)) return [];
  const out = [];
  for (const s of body.samples) {
    if (!s || typeof s !== "object" || !finite(s.ts_unix_ms) || !s.metrics || typeof s.metrics !== "object" || Array.isArray(s.metrics)) continue;
    const metrics = {};
    for (const k of STATS_KEYS) {
      if (Object.hasOwn(s.metrics, k) && finite(s.metrics[k])) metrics[k] = s.metrics[k];
    }
    if (!Object.keys(metrics).length) continue;
    out.push({ kind: "metric", source: "browser", ts_unix_ms: Math.trunc(s.ts_unix_ms), metrics });
  }
  return out;
}

/** Filters captured `/stats` rows to `sid` and deduplicates by `ts_unix_ms` (first value per key wins). */
function dedupStatsRows(entries, sid) {
  const want = sid.toLowerCase();
  const byTs = new Map();
  const out = [];
  for (const { session, row } of entries) {
    if (session !== want) continue;
    const existing = byTs.get(row.ts_unix_ms);
    if (!existing) {
      const copy = { ...row, metrics: { ...row.metrics } };
      byTs.set(row.ts_unix_ms, copy);
      out.push(copy);
    } else {
      for (const [k, v] of Object.entries(row.metrics)) {
        if (!Object.hasOwn(existing.metrics, k)) existing.metrics[k] = v;
      }
    }
  }
  return out;
}

/** What a plan entry's params may hold; all but `app`, `bench`, `cover_qr` and `expect_marker` have a default (docs/testing-e2e-suite.md lists them). */
export const PARAM_KEYS = ["transport", "codec", "profile", "hold_s", "app", "bench", "cover_qr", "expect_marker"];

/** Why `p` cannot be run, or null. The runner asks before it opens a page. */
export function check(p) {
  const stray = Object.keys(p).filter((k) => !PARAM_KEYS.includes(k));
  if (stray.length) return `params has a key stream-hold does not define: ${stray.map((k) => JSON.stringify(k.slice(0, 40))).join(", ")} (it defines ${PARAM_KEYS.join(", ")})`;
  if (!["webrtc", "webtransport"].includes(p.transport)) return `transport must be webrtc or webtransport, got ${shown(p.transport)}`;
  if (!Object.hasOwn(CODEC_LABEL, p.codec)) return `codec must be one of ${Object.keys(CODEC_LABEL).join(", ")}, got ${shown(p.codec)}`;
  if (typeof p.profile !== "string" || !/^\d{1,5}p\d{1,3}$/.test(p.profile)) return `profile must look like 1080p60, got ${shown(p.profile)}`;
  if (!finite(p.hold_s) || p.hold_s <= 0) return `hold_s must be a positive number of seconds, got ${shown(p.hold_s)}`;
  if (typeof p.app !== "string" || !p.app) return `app must be a catalog name, got ${shown(p.app)}`;
  if (p.expect_marker !== undefined && typeof p.expect_marker !== "boolean") return `expect_marker must be a boolean, got ${shown(p.expect_marker)}`;
  // WebTransport draws on a canvas: only the bench ring counts its frames, and the page's loader is not a frame.
  if (p.transport === "webtransport" && p.bench === false) return "bench mode is needed to see a frame on this transport";
  return null;
}

const validSpan = (span) => Array.isArray(span) && finite(span[0]) && finite(span[1]);
const validWindowTimes = (w) => Boolean(w && typeof w === "object" && finite(w.t_start_ms) && finite(w.t_end_ms) && w.t_end_ms >= w.t_start_ms);
const overlaps = (w, hidden) => (Array.isArray(hidden) ? hidden : []).some((span) => validSpan(span) && w.t_start_ms < span[1] && w.t_end_ms > span[0]);

/** Windows after the warm-up, on the windows' own clock (the runner's clock may differ from the browser's), minus any the page was hidden for. */
export function observeWindows(windows, warmupS, hidden = []) {
  if (!Array.isArray(windows) || !finite(warmupS)) return [];
  const first = windows.find((w) => validWindowTimes(w) && finite(w.n) && w.n > 0);
  if (!first) return [];
  return windows.filter((w) => validWindowTimes(w) && w.t_start_ms >= first.t_start_ms + warmupS * 1000 && !overlaps(w, hidden));
}

/** One-second bench windows in which no frame reached the bench callback. A stall shorter than a window is invisible here. */
export function framelessSeconds(obs) {
  return obs.filter((w) => finite(w?.n) && w.n === 0 && !(finite(w?.no_image) && w.no_image > 0)).length;
}

/** Every judged window states how many frames it saw and how many had no image; one that does not (or states a negative or non-finite number) makes the count unknown, not zero. */
const countsKnown = (obs) => obs.every((w) => [w?.n, w?.no_image].every((x) => finite(x) && x >= 0));

/** missing / (decoded + missing) over the windows: the indices that were due. Any absent, negative or non-finite count makes it null. */
export function missingRate(obs) {
  let missing = 0;
  let decoded = 0;
  for (const w of obs) {
    if (![w?.missing_indices, w?.decoded].every((x) => finite(x) && x >= 0)) return null;
    missing += w.missing_indices;
    decoded += w.decoded;
  }
  return decoded + missing > 0 ? missing / (decoded + missing) : null;
}

/**
 * The control plane's hitch rule on the bench ring: groups consecutive present intervals
 * by clock second (`Math.floor(present_ms / 1000)` — not bench window boundaries), skips
 * any interval overlapping a hidden span, keeps seconds with at least `present_min_samples`
 * intervals, and counts seconds whose sd is `>= t.hitch_sd_ms`.
 */
export function hitchCount(frames, fromMs, toMs, hidden, t) {
  if (!Array.isArray(frames) || !finite(fromMs) || !finite(toMs) || !finite(t?.present_min_samples) || !finite(t?.hitch_sd_ms)) {
    return { judged: 0, hitches: 0 };
  }
  const spans = Array.isArray(hidden) ? hidden.filter(validSpan) : [];
  const ps = frames.map((f) => f?.present_ms).filter((p) => finite(p) && p >= fromMs && p <= toMs).sort((a, b) => a - b);
  const by = new Map();
  for (let i = 1; i < ps.length; i += 1) {
    if (spans.some(([a, b]) => ps[i - 1] < b && ps[i] > a)) continue;
    const k = Math.floor(ps[i] / 1000);
    by.set(k, [...(by.get(k) ?? []), ps[i] - ps[i - 1]]);
  }
  const sds = [...by.values()].filter((iv) => iv.length >= t.present_min_samples).map(sd);
  return { judged: sds.length, hitches: sds.filter((s) => s >= t.hitch_sd_ms).length };
}

/** The frames the bench callback saw (`seen`) of the frames the video element decoded (`decoded`) over [a, b]; null when there is no video to compare with, `{ malformed: true }` when a time or a count is not a number. */
export function instrumentShare(frames, videoSamples, a, b) {
  // Malformed evidence is never counted and never dropped to improve the share: the cadence is then not judged.
  if (!finite(a) || !finite(b) || !Array.isArray(videoSamples) || !Array.isArray(frames)) return { malformed: true };
  if (!videoSamples.every((s) => finite(s?.t) && finite(s.videoFrames) && s.videoFrames >= 0) || !frames.every((f) => finite(f?.present_ms))) return { malformed: true };
  const v = videoSamples.filter((s) => s.t >= a && s.t <= b);
  if (v.length < 2) return null;
  const decoded = v.at(-1).videoFrames - v[0].videoFrames;
  if (!(decoded > 0)) return null;
  return { seen: frames.filter((f) => f.present_ms >= v[0].t && f.present_ms <= v.at(-1).t).length, decoded };
}

function transportObserved(s) {
  if (s.transport_fallback_reason) return `${s.transport} (the client fell back to webrtc: ${s.transport_fallback_reason})`;
  if (s.transport_reason) return `${s.transport} (stream plan: ${s.transport_reason})`;
  return String(s.transport);
}

export function judge({ params, session, windows, frames, firstFrameS, stats, hidden = [], videoSamples = [], malformedFrames = false, thresholds: T }) {
  const windowTimesValid = Array.isArray(windows) && windows.every(validWindowTimes);
  const obs = windowTimesValid ? observeWindows(windows, T.warmup_exclude_s, hidden) : [];
  const warm = `${T.warmup_exclude_s} s warm-up`;
  const s = session;
  const codec = !s ? null : s.pyrowave?.active || s.pyrowave?.used ? ["pyrowave", "pyrowave.active/used"] : s.negotiated_codec ? [String(s.negotiated_codec), "negotiated_codec"] : s.stream?.codec ? [String(s.stream.codec), "stream.codec"] : null;
  const [, h, fps] = /^(\d+)p(\d+)$/.exec(params.profile ?? "") ?? [];
  const stream = s?.stream;

  const rate = windowTimesValid ? missingRate(obs) : null;
  let spanFrom = null;
  let spanTo = null;
  for (const w of obs) {
    if (spanFrom === null || w.t_start_ms < spanFrom) spanFrom = w.t_start_ms;
    if (spanTo === null || w.t_end_ms > spanTo) spanTo = w.t_end_ms;
  }
  const cad = obs.length ? hitchCount(frames, spanFrom, spanTo, hidden, T) : null;
  const inst = obs.length ? instrumentShare(frames, videoSamples, spanFrom, spanTo) : null;
  const saw = inst?.decoded ? `${inst.seen} of ${inst.decoded} frames` : null;
  let cadence = crit("present_cadence", null, null, `no ${warm} window had ${T.present_min_samples} present intervals`);
  if (!windowTimesValid) {
    cadence = crit("present_cadence", null, null, "not judged: a window time in the evidence is not a finite number");
  } else if (malformedFrames || inst?.malformed) {
    cadence = crit("present_cadence", null, null, "not judged: a frame time or a video frame count in the evidence is not a number");
  } else if (cad?.judged && inst && inst.seen / inst.decoded < INSTRUMENT_MIN_SHARE) {
    cadence = crit("present_cadence", null, null, `not judged: the bench callback saw ${saw} the video decoded (${Math.round((inst.seen / inst.decoded) * 100)}%), so the instrument set the cadence (issue #1304)`);
  } else if (cad?.judged) {
    cadence = crit("present_cadence", cad.hitches < T.hitch_min_samples, cad.hitches, `< ${T.hitch_min_samples} one-second windows with present-interval sd >= ${T.hitch_sd_ms} ms, after the ${warm}${saw ? `; the bench callback saw ${saw} the video decoded` : ""}`);
  }
  const frameless = framelessSeconds(obs);

  return [
    firstFrameCriterion(firstFrameS),
    params.expect_marker === false
      ? crit("marker_decoded", null, null, "not judged: plan says this app has no marker")
      : crit("marker_decoded", stats && finite(stats.decoded) && stats.decoded >= 0 ? stats.decoded > 0 : null, stats?.decoded ?? null, "> 0 marker frames decoded"),
    crit("transport_as_asked", s ? s.transport === params.transport && !s.transport_fallback_reason : null, s ? transportObserved(s) : null, params.transport),
    crit("codec_as_asked", codec ? codec[0] === params.codec : null, codec ? `${codec[0]} (${codec[1]})` : null, params.codec),
    crit("profile_as_asked", stream && h && finite(+h) && finite(+fps) && [stream.height, stream.fps].every((x) => finite(x) && x >= 0) ? stream.height === +h && stream.fps === +fps : null, stream ? `${stream.height}p${stream.fps}` : null, `${params.profile} (the launch size, not external_*)`),
    crit("frameless_seconds", windowTimesValid && obs.length && countsKnown(obs) ? frameless === 0 : null, frameless, `0 one-second bench windows without a frame, after the ${warm}`),
    {
      id: "missing_indices_rate",
      pass: null,
      observed: null,
      expected: `not judged: no limit calibrated on the marker app yet${rate === null ? " (no usable counts)" : `; measured ${(rate * 100).toFixed(2)} % over ${obs.length} windows`}`,
    },
    cadence,
  ];
}

export default {
  name,
  check,
  async run(page, ctx) {
    const { params, rec, api, client, now, sleep } = ctx;
    const why = cannotRun(params, client);
    if (why) return skipped(why);

    const cover = params.cover_qr ?? params.app === "Steam";
    const shot = (label) => shotCovered(page, rec, label, cover);
    const tb = () => {
      const offset = ctx.clockOffset?.();
      return now() + (finite(offset) ? offset : 0);
    }; // the browser's clock, which the bench windows are stamped on

    // The sessions this run owns are the runner's to track and stop (ctx.launches): a page address never adds one.
    const pending = watchApiErrors(page, ctx);
    // Capture the page's own cadence/visibility samples from POST /v1/sessions/{id}/stats on the stack's origin
    // (a read of the request the page already sends, attached before play() so nothing is missed).
    const origin = new URL(ctx.baseUrl).origin;
    const postedStats = [];
    page.on("request", (q) => {
      let u;
      try { u = new URL(q.url()); } catch { return; }
      const m = u.origin === origin && q.method() === "POST" ? STATS_POST.exec(u.pathname) : null;
      if (!m) return;
      const session = m[1].toLowerCase();
      for (const row of statsSamples(q.postData?.() ?? "")) postedStats.push({ session, row });
    });

    let result;
    // What step the run is in, for the one reason that may be written before the page's bearer is known (see the catch).
    let stage = "sign-in";
    try {
      await signIn(page, ctx);
      rec.step("signed-in");
      stage = "the launch";
      const app = (await api.apps()).find((a) => a.name === params.app);
      if (!app) return skipped(`app "${params.app}" is not in this user's catalog`);

      const { sid, tClick } = await play(page, ctx, app);
      stage = "the hold";

      const frames = [];
      let lastPresent = -Infinity;
      let malformedFrames = false;
      const pullFrames = async () => {
        const dump = await page.evaluate(() => window.__qBench?.dump() ?? null);
        if (dump != null && dump.frames !== undefined && !Array.isArray(dump.frames)) malformedFrames = true;
        for (const f of Array.isArray(dump?.frames) ? dump.frames : []) {
          if (!(f && typeof f === "object" && finite(f.present_ms))) {
            malformedFrames = true;
            continue;
          }
          if (f.present_ms > lastPresent) {
            frames.push(f);
            lastPresent = f.present_ms;
          }
        }
        return dump;
      };
      // One look at the page per second: is a frame showing, is the page visible. The hidden spans leave the judged span.
      const hidden = [];
      const videoSamples = [];
      let hiddenFrom = null;
      let revealed = false;
      const look = async () => {
        const p = await probe(page);
        const t = tb();
        // The loader's reveal comes later than the first decoded frame and is not a frame (the product can open it with none): it is only noted.
        if (p.revealed && !revealed) { revealed = true; rec.step("revealed", { since_click_s: (now() - tClick) / 1000 }); }
        videoSamples.push({ t, videoFrames: p.videoFrames });
        if (p.visible !== "visible") hiddenFrom ??= t;
        else if (hiddenFrom !== null) { hidden.push([hiddenFrom, t]); hiddenFrom = null; }
        return p;
      };

      let firstFrameAt = null;
      const deadline = tClick + (FIRST_FRAME_LIMIT_S + 90) * 1000; // cold hosts launch slowly; the criterion still judges the limit
      while (now() < deadline && firstFrameAt === null) {
        const p = await look();
        if ([p.frames, p.videoFrames].some((n) => finite(n) && n > 0)) firstFrameAt = now(); // a count, or it is not a frame
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
      const windows = Array.isArray(dump?.windows) ? dump.windows : [];
      const stats = await page.evaluate(() => window.__qBench?.stats() ?? null);
      rec.json("bench-windows.json", { windows });
      rec.json("bench-frames.json", { frames });
      rec.jsonl("metrics.jsonl", [...benchRows(windows, 0), ...dedupStatsRows(postedStats, sid)]);

      const row = await api.session(sid);
      if (!row || row.id !== sid) throw new Error("the API no longer returns the session this run started");
      if (row.app_id !== undefined && row.app_id !== app.id) throw new Error("the launched session is for another app than the one asked for");
      rec.json("session.json", { session: row });
      const known = [...windows].reverse().find((w) => w?.offset_unknown === false && finite(w.offset_ms));
      result = {
        criteria: judge({ params, session: row, windows, frames, firstFrameS, stats, hidden, videoSamples, malformedFrames, thresholds: ctx.thresholds }),
        clock_offset_ms: known ? known.offset_ms : null,
      };
      // A session that is already over before this run's own stop was not held: whatever the criteria say.
      if (TERMINAL.includes(row.state)) result = { ...result, status: "error", reason: `the session ended during the hold (it was ${row.state} before this run stopped it)` };
      rec.step("hold-done");
    } catch (err) {
      // Until the page's bearer is known, an error's words (Playwright quotes the page's address and text) are held like any
      // other page output, and the reason that is written is a fixed one for the step that failed.
      const message = errorReason(err);
      if (rec.holding) rec.pageStep("error", { message });
      else rec.step("error", { message });
      result = { status: "error", reason: rec.holding ? `${stage} did not complete (the page's own words are not written before its sign-in is read)` : message, criteria: [], clock_offset_ms: null };
    } finally {
      await Promise.allSettled(pending);
    }
    return { ...result, ...verdict(result) };
  },
};

