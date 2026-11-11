// Scenario transport-fallback: a WebTransport session whose transport is made to fail through the hook
// fault:webtransport-block, either mid-stream (when: "mid") or before the launch (when: "connect"), and what the
// client does about it (control-api.md §Transport fallback, issue #1260). Reads only what the product exposes
// (the session row, the events the page itself posts, the <video> the page shows); wraps no browser API.
//
// A criterion with no evidence source today is `pass: null` and says what is missing; nothing is invented.
import { check as holdCheck } from "./stream-hold.mjs";
import { cannotRun, clickStream, crit, finite, firstFrameCriterion, openCdp, keyEvent, play, probe, shotCovered, signIn, skipped, verdict, waitFirstFrame, watchApiErrors } from "./stream-common.mjs";

export const name = "transport-fallback";
export const HOOK = "fault:webtransport-block";
const TERMINAL = ["stopped", "failed"];
const REASON = /^[a-z0-9_]{1,64}$/;
const TRACE_POST = /^\/v1\/sessions\/[0-9a-f-]{36}\/trace\/events$/i;
const TAIL_S = 5; // how long the stream is watched once on WebRTC, to see it keeps going
const KEY = { code: "KeyW", key: "w", vk: 87 };

/** Why `p` cannot be run, or null. */
export function check(p) {
  const { when, ...rest } = p;
  if (when !== undefined && !["mid", "connect"].includes(when)) return `when must be mid or connect, got ${JSON.stringify(when)?.slice(0, 40)}`;
  if (rest.transport !== "webtransport") return "transport-fallback needs transport webtransport: it is the WebTransport session that falls back";
  return holdCheck(rest);
}

/** The `transport.fallback` events in a trace-events POST body, shape-checked; anything else in the body is ignored. */
export function fallbackEvents(postData) {
  let body;
  try { body = JSON.parse(postData); } catch { return []; }
  return (Array.isArray(body?.events) ? body.events : [])
    .filter((e) => e?.type === "transport.fallback" && typeof e.payload?.reason === "string" && REASON.test(e.payload.reason))
    .map((e) => ({ ts_unix_ms: finite(e.ts_unix_ms) ? e.ts_unix_ms : null, reason: e.payload.reason }));
}

/**
 * The criteria. `seen` holds what the run observed: `row` (the session row at the end), `fallbackReason` (from the row),
 * `posted` (events the page posted), `videoAfterS` (seconds from the fault to the first <video> frame beyond the baseline, or
 * null), `tail` ({ before, after } <video> frame counts across the tail, or null), `keyHeld` (a key was held across the fault).
 */
export function judge({ params, firstFrameS, seen }) {
  const when = params.when ?? "mid";
  const s = seen;
  const reason = s.fallbackReason ?? s.posted[0]?.reason ?? null;
  const ended = !s.row || TERMINAL.includes(s.row.state);
  let continues = crit("continues_on_webrtc", null, null, "not judged: the session row could not be read");
  if (s.row) {
    const alive = !ended && finite(s.tail?.before) && finite(s.tail?.after) && s.tail.after > s.tail.before;
    continues = crit("continues_on_webrtc", reason !== null && alive, reason === null ? "no fallback was reported" : alive ? `the <video> advanced ${s.tail.after - s.tail.before} frames in ${TAIL_S} s` : ended ? `the session was ${s.row.state}` : "the <video> did not advance", `after the fallback the session is not over and the <video> (WebRTC; WebTransport draws on a canvas) keeps decoding frames over ${TAIL_S} s`);
  }
  return [
    firstFrameCriterion(firstFrameS),
    crit("webtransport_closed_first", null, null, "not judged: no source today. The page does not expose when its WebTransport session closed relative to opening WebRTC, the trace has no event for it, and the agent logs nothing of its own for a lost WebTransport client (#1260). It needs a client trace event or bench field for the close time, or an agent log line, in the host half"),
    continues,
    crit("fallback_event", reason !== null, reason === null ? `none within ${params.hold_s} s of the fault` : `${reason} (${s.fallbackReason ? "the session row" : "the page's own post only: the session row has none"}${s.fallbackReason && s.posted.length ? "; the page also posted it" : ""})`, `a transport.fallback event with a reason matching ${REASON}, as the session row's transport_fallback_reason or the event the page posts to the stack`),
    crit("frame_after_fallback", finite(s.videoAfterS), s.videoAfterS === null ? `no <video> frame within ${params.hold_s} s of the fault` : `${s.videoAfterS} s after the fault`, "a frame decoded by the <video> (the WebRTC path) after the fault was applied; the baseline is the frames it had counted when the fault went on"),
    s.keyHeld
      ? crit("held_key_released", null, null, "not judged: the client cannot see the host's key state. A key was held across the fallback (timeline key-held / key-released); whether the host released it needs the host half (an agent input log) or the reference app logging individual keys")
      : crit("held_key_released", null, null, when === "connect" ? "not judged: nothing was held; no input is captured before the first picture, and the fault was on before it (#1260)" : "not judged: no key could be held (the stream surface took no click)"),
  ];
}

export default {
  name,
  hooks: [HOOK],
  check,
  async run(page, ctx) {
    const { params, rec, api, client, now, sleep } = ctx;
    const why = cannotRun(params, client);
    if (why) return skipped(why);
    const when = params.when ?? "mid";
    const cover = params.cover_qr ?? params.app === "Steam";
    const shot = (label) => shotCovered(page, rec, label, cover);
    const pending = watchApiErrors(page, ctx);
    const origin = new URL(ctx.baseUrl).origin;
    const posted = [];
    // What the page itself tells the stack: a read of its own request, not a write.
    page.on("request", (q) => {
      let u;
      try { u = new URL(q.url()); } catch { return; }
      if (u.origin === origin && q.method() === "POST" && TRACE_POST.test(u.pathname)) posted.push(...fallbackEvents(q.postData?.() ?? ""));
    });

    let result;
    let stage = "sign-in";
    let cdp = null;
    let keyDown = false;
    try {
      await signIn(page, ctx);
      rec.step("signed-in");
      stage = "the launch";
      const app = (await api.apps()).find((a) => a.name === params.app);
      if (!app) return skipped(`app "${params.app}" is not in this user's catalog`);

      let tFault = null;
      if (when === "connect") {
        await ctx.hook(HOOK, "apply");
        tFault = now();
        rec.step("fault-applied", { when });
      }
      const { sid, tClick } = await play(page, ctx, app);
      stage = "the stream";
      const firstFrameAt = await waitFirstFrame(page, ctx, tClick);
      const firstFrameS = firstFrameAt === null ? null : (firstFrameAt - tClick) / 1000;
      rec.step("first-frame", { first_frame_s: firstFrameS });
      await shot(firstFrameAt === null ? "no-frame" : "first-frame");

      let baseline = 0;
      let keyHeld = false;
      if (firstFrameAt !== null && when === "mid") {
        // A trusted click so the page may capture input, then a key held across the fault.
        cdp = await openCdp(page);
        if (await clickStream(page, cdp)) {
          await keyEvent(cdp, "keyDown", KEY);
          keyDown = true;
          keyHeld = true;
          rec.step("key-held", { code: KEY.code });
        }
        const v = (await probe(page)).videoFrames;
        baseline = finite(v) ? v : 0;
        await ctx.hook(HOOK, "apply");
        tFault = now();
        rec.step("fault-applied", { when, video_frames_before: baseline });
      }

      const seen = { row: undefined, fallbackReason: null, posted, videoAfterS: null, tail: null, keyHeld };
      if (firstFrameAt !== null) {
        const until = now() + params.hold_s * 1000;
        let fallbackAt = null;
        let videoAt = null;
        while (now() < until && !(fallbackAt !== null && videoAt !== null)) {
          await sleep(1000);
          const row = await api.session(sid);
          if (row && TERMINAL.includes(row.state)) break;
          if (row?.transport_fallback_reason && REASON.test(String(row.transport_fallback_reason)) && fallbackAt === null) {
            fallbackAt = now();
            seen.fallbackReason = row.transport_fallback_reason;
            rec.step("fallback-seen", { reason: seen.fallbackReason, since_fault_s: (fallbackAt - tFault) / 1000 });
          }
          const p = await probe(page);
          if (videoAt === null && finite(p.videoFrames) && p.videoFrames > baseline) {
            videoAt = now();
            seen.videoAfterS = (videoAt - tFault) / 1000;
            rec.step("video-frame", { since_fault_s: seen.videoAfterS });
          }
        }
        await shot("after-fallback");
        // Does it keep going: the <video>'s frames across a short tail.
        const before = (await probe(page)).videoFrames;
        for (let i = 0; i < TAIL_S; i += 1) await sleep(1000);
        seen.tail = { before, after: (await probe(page)).videoFrames };
      }
      seen.row = await api.session(sid);
      if (seen.row && seen.row.id !== sid) throw new Error("the API returned another session than the one this run launched");
      rec.json("session.json", { session: seen.row ?? null });
      rec.json("fallback.json", { when, reason: seen.fallbackReason, posted_events: posted, video_after_fault_s: seen.videoAfterS, tail_video_frames: seen.tail, key_held: keyHeld });

      if (keyDown) {
        await keyEvent(cdp, "keyUp", KEY);
        keyDown = false;
        rec.step("key-released", { code: KEY.code, by: "the runner, at the end: the host half must read the host's key state before this" });
      }
      result = { criteria: judge({ params, firstFrameS, seen }), clock_offset_ms: null };
      if (seen.row && TERMINAL.includes(seen.row.state)) result = { ...result, status: "error", reason: `the session ended during the run (it was ${seen.row.state} before this run stopped it)` };
    } catch (err) {
      const message = String(err?.message ?? err);
      if (rec.holding) rec.pageStep("error", { message });
      else rec.step("error", { message });
      result = { status: "error", reason: rec.holding ? `${stage} did not complete (the page's own words are not written before its sign-in is read)` : message, criteria: [], clock_offset_ms: null };
    } finally {
      if (keyDown && cdp) await keyEvent(cdp, "keyUp", KEY).catch(() => {});
      await Promise.allSettled(pending);
    }
    return { ...result, ...verdict(result) };
  },
};
