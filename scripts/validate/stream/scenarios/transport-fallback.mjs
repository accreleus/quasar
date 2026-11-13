// Scenario transport-fallback: a WebTransport session whose transport is made to fail through the hook
// fault:webtransport-block, either mid-stream (when: "mid") or before the launch (when: "connect"), and what the
// client does about it (control-api.md §Transport fallback, issue #1260). Reads only what the product exposes
// (the session row, the events the page itself posts, the <video> the page shows); wraps no browser API.
//
// A criterion with no evidence source today is `pass: null` and says what is missing; nothing is invented.
import { check as holdCheck } from "./stream-hold.mjs";
import { cannotRun, crit, finite, firstFrameCriterion, openCdp, keyEvent, play, probe, shotCovered, signIn, skipped, takeLock, verdict, waitFirstFrame, watchApiErrors } from "./stream-common.mjs";

export const name = "transport-fallback";
export const HOOK = "fault:webtransport-block";
const TERMINAL = ["stopped", "failed"];
const REASON = /^[a-z0-9_]{1,64}$/;
const TRACE_POST = /^\/v1\/sessions\/([0-9a-f-]{36})\/trace\/events$/i; // the session is the capture: only this run's own counts
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
 * `posted` (events the page posted AFTER the fault, from this session's trace endpoint), `videoAfterS` (seconds from the fault to the
 * first <video> frame, or null), `tail` ({ before, after } <video> frame counts across the tail, or null), `keyHeld` (a key was held
 * across the fault), `faultApplied` (the hook ran), `watched` (a first frame was seen, so the stream was watched after the fault),
 * `freezeS` (mid only: the longest span, in seconds, of one-second samples between the fault and the first <video> frame in which neither
 * the bench frames nor the <video> advanced; null when not known).
 */
export function judge({ params, firstFrameS, seen }) {
  const when = params.when ?? "mid";
  const s = seen;
  const reason = s.fallbackReason ?? s.posted[0]?.reason ?? null;
  const ended = !s.row || TERMINAL.includes(s.row.state);
  // No first frame: the stream was never watched (and in `mid` the fault was never applied), so nothing after the fault is judged.
  // Where the wait for the fallback and the frame starts: the fault in `mid`, the first frame in `connect` (the fault was on before the launch).
  const from = when === "connect" ? "the first frame (the fault was applied before the launch)" : "the fault";
  const unwatched = `not judged: no first frame, so ${s.faultApplied ? "the stream was not watched after the fault" : "the fault was never applied"}`;
  let continues = crit("continues_on_webrtc", null, null, "not judged: the session row could not be read");
  if (!s.watched) continues = crit("continues_on_webrtc", null, null, unwatched);
  else if (s.row) {
    const alive = !ended && finite(s.tail?.before) && finite(s.tail?.after) && s.tail.after > s.tail.before;
    continues = crit("continues_on_webrtc", reason !== null && alive, reason === null ? "no fallback was reported" : alive ? `the <video> advanced ${s.tail.after - s.tail.before} frames in ${TAIL_S} s` : ended ? `the session was ${s.row.state}` : "the <video> did not advance", `after the fallback the session is not over and the <video> (WebRTC; WebTransport draws on a canvas) keeps decoding frames over ${TAIL_S} s`);
  }
  return [
    firstFrameCriterion(firstFrameS),
    crit("webtransport_closed_first", null, null, "not judged: no source today. The page does not expose when its WebTransport session closed relative to opening WebRTC, the trace has no event for it, and the agent logs nothing of its own for a lost WebTransport client (#1260). It needs a client trace event or bench field for the close time, or an agent log line, in the host half"),
    continues,
    !s.watched ? crit("fallback_event", null, null, unwatched) : crit("fallback_event", reason !== null, reason === null ? `none within ${params.hold_s} s of ${from}` : `${reason} (${s.fallbackReason ? "the session row" : "the page's own post only: the session row has none"}${s.fallbackReason && s.posted.length ? "; the page also posted it" : ""})`, `a transport.fallback event with a reason matching ${REASON}, as the session row's transport_fallback_reason or the event the page posts to the stack`),
    !s.watched ? crit("frame_after_fallback", null, null, unwatched) : crit("frame_after_fallback", finite(s.videoAfterS), s.videoAfterS === null ? `no <video> frame within ${params.hold_s} s of ${from}` : `${s.videoAfterS} s after the fault${finite(s.freezeS) ? `; no frame advanced for ${s.freezeS} s of it` : ""}`, "a frame decoded by the <video> (the WebRTC path) after the fault was applied (a <video> already decoding before it ends the run: the fallback happened at connect)"),
    // Nothing is held in `connect` (no input is captured before the first picture), with or without a frame.
    when === "connect"
      ? crit("held_key_released", null, null, "not judged: nothing was held; no input is captured before the first picture, and the fault was on before it (#1260)")
      : !s.watched
      ? crit("held_key_released", null, null, unwatched)
      : s.keyHeld
      ? crit("held_key_released", null, null, "not judged: the client cannot see the host's key state. A key was held across the fallback (timeline key-held / key-released); whether the host released it needs the host half (an agent input log) or the reference app logging individual keys")
      : crit("held_key_released", null, null, "not judged: no key could be held (the pointer lock was not taken: the \"Click to play\" layer that takes it did not appear, or the lock was not held after the click)"),
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
    let sid = null;
    const mine = () => posted.filter((e) => e.session === sid?.toLowerCase());
    // What the page itself tells the stack: a read of its own request, not a write. Kept with the session whose endpoint it went to.
    page.on("request", (q) => {
      let u;
      try { u = new URL(q.url()); } catch { return; }
      const m = u.origin === origin && q.method() === "POST" ? TRACE_POST.exec(u.pathname) : null;
      if (m) posted.push(...fallbackEvents(q.postData?.() ?? "").map((e) => ({ ...e, session: m[1].toLowerCase() })));
    });

    let result;
    let stage = "sign-in";
    let cdp = null;
    let keyDown = false;
    const cleanup = [];
    // Releases the held key once; a release that fails is a step and a "left behind" line, never swallowed. Returns whether the key is up.
    const release = async (by) => {
      if (!keyDown) return null;
      keyDown = false;
      try {
        await keyEvent(cdp, "keyUp", KEY);
        rec.step("key-released", { code: KEY.code, by });
        return true;
      } catch (err) {
        rec.step("key-release-failed", { code: KEY.code, message: String(err?.message ?? err) });
        cleanup.push(`key ${KEY.code} could not be released (${String(err?.message ?? err)})`);
        return false;
      }
    };
    try {
      await signIn(page, ctx);
      rec.step("signed-in");
      stage = "the launch";
      const app = (await api.apps()).find((a) => a.name === params.app);
      if (!app) return skipped(`app "${params.app}" is not in this user's catalog`);

      let tFault = null;
      let faultApplied = false;
      if (when === "connect") {
        await ctx.hook(HOOK, "apply");
        faultApplied = true;
        tFault = now();
        rec.step("fault-applied", { when });
      }
      const played = await play(page, ctx, app);
      sid = played.sid;
      const tClick = played.tClick;
      stage = "the stream";
      const firstFrameAt = await waitFirstFrame(page, ctx, tClick);
      const firstFrameS = firstFrameAt === null ? null : (firstFrameAt - tClick) / 1000;
      rec.step("first-frame", { first_frame_s: firstFrameS });
      await shot(firstFrameAt === null ? "no-frame" : "first-frame");

      let postedBefore = 0;
      let keyHeld = false;
      let frames0 = null; // the probe just before the fault (mid): the baseline for the freeze
      if (firstFrameAt !== null && when === "mid") {
        // Already on WebRTC before any fault (a fallback at connect): the mid-stream fallback cannot be exercised, and the hook is not applied.
        // Checked before the lock is taken and a key held, so a run that cannot exercise the fault touches nothing.
        const row0 = await api.session(sid);
        if (!row0 || row0.id !== sid) throw new Error("the session row could not be read just before the fault, so whether it had already fallen back is not known");
        const early = (typeof row0.transport_fallback_reason === "string" && REASON.test(row0.transport_fallback_reason) ? row0.transport_fallback_reason : null) ?? mine()[0]?.reason ?? null;
        const p0 = await probe(page);
        if (early !== null || (finite(p0.videoFrames) && p0.videoFrames > 0)) throw new Error(`fell back at connect (${early ?? "no reason reported"}) before the fault; the mid-stream fallback was not exercised`);
        frames0 = p0;
        // The pointer lock through the product's own layer, then a key held across the fault (only when the lock is held).
        cdp = await openCdp(page);
        const lock = await takeLock(page, cdp, ctx);
        rec.step("pointer-lock", { held: lock.held, clicks: lock.clicks, hud: lock.hud });
        if (lock.held) {
          await keyEvent(cdp, "keyDown", KEY);
          keyDown = true;
          keyHeld = true;
          rec.step("key-held", { code: KEY.code });
        }
        postedBefore = mine().length;
        await ctx.hook(HOOK, "apply");
        faultApplied = true;
        tFault = now();
        rec.step("fault-applied", { when });
      }

      const seen = { row: undefined, fallbackReason: null, posted: [], videoAfterS: null, tail: null, keyHeld, faultApplied, watched: firstFrameAt !== null, freezeS: null };
      let waitedS = null;
      let waitEndedBy = null;
      if (firstFrameAt !== null) {
        const until = now() + params.hold_s * 1000;
        let fallbackAt = null;
        let videoAt = null;
        const waitStart = now();
        // The freeze (mid): the longest run of samples, from the fault to the first <video> frame, in which neither the bench frames nor the <video> advanced.
        let last = frames0;
        let lastMoveAt = tFault;
        let freeze = frames0 ? 0 : null;
        let ended = false;
        while (now() < until && !(fallbackAt !== null && videoAt !== null)) {
          await sleep(1000);
          const row = await api.session(sid);
          if (row && TERMINAL.includes(row.state)) { ended = true; break; }
          if (row?.transport_fallback_reason && REASON.test(String(row.transport_fallback_reason)) && fallbackAt === null) {
            fallbackAt = now();
            seen.fallbackReason = row.transport_fallback_reason;
            rec.step("fallback-seen", { reason: seen.fallbackReason, since_fault_s: (fallbackAt - tFault) / 1000 });
          }
          const p = await probe(page);
          if (videoAt === null && freeze !== null) {
            if (![p.frames, p.videoFrames, last.frames, last.videoFrames].every(finite)) freeze = null;
            else if (p.frames > last.frames || p.videoFrames > last.videoFrames) lastMoveAt = now();
            else freeze = Math.max(freeze, (now() - lastMoveAt) / 1000);
            last = p;
          }
          if (videoAt === null && finite(p.videoFrames) && p.videoFrames > 0) {
            videoAt = now();
            seen.videoAfterS = (videoAt - tFault) / 1000;
            rec.step("video-frame", { since_fault_s: seen.videoAfterS });
          }
        }
        seen.freezeS = freeze;
        waitedS = (now() - waitStart) / 1000;
        waitEndedBy = ended ? "session_ended" : fallbackAt !== null && videoAt !== null ? "fallback_and_frame_seen" : "hold_elapsed";
        rec.step("wait-ended", { by: waitEndedBy, waited_s: waitedS, no_frame_advance_s: freeze });
        await shot("after-fallback");
        // Does it keep going: the <video>'s frames across a short tail.
        const before = (await probe(page)).videoFrames;
        for (let i = 0; i < TAIL_S; i += 1) await sleep(1000);
        seen.tail = { before, after: (await probe(page)).videoFrames };
      }
      seen.posted = mine().slice(postedBefore).map(({ session, ...e }) => e);
      seen.row = await api.session(sid);
      if (!seen.row || seen.row.id !== sid) throw new Error("the API no longer returns the session this run started");
      rec.json("session.json", { session: seen.row });

      // The release is part of the record: fallback.json does not claim a clean release that did not happen.
      const released = await release("the runner, at the end: the host half must read the host's key state before this");
      rec.json("fallback.json", { when, fault_applied: faultApplied, reason: seen.fallbackReason, posted_events: seen.posted, video_after_fault_s: seen.videoAfterS, no_frame_advance_before_video_s: seen.freezeS, wait_ended_by: waitEndedBy, waited_s: waitedS, tail_video_frames: seen.tail, key_held: keyHeld, key_released_by_runner: released });
      result = { criteria: judge({ params, firstFrameS, seen }), clock_offset_ms: null };
      if (seen.row && TERMINAL.includes(seen.row.state)) result = { ...result, status: "error", reason: `the session ended during the run (it was ${seen.row.state} before this run stopped it)` };
    } catch (err) {
      const message = String(err?.message ?? err);
      if (rec.holding) rec.pageStep("error", { message });
      else rec.step("error", { message });
      result = { status: "error", reason: rec.holding ? `${stage} did not complete (the page's own words are not written before its sign-in is read)` : message, criteria: [], clock_offset_ms: null };
    } finally {
      await release("the runner, on the way out");
      await Promise.allSettled(pending);
    }
    return { ...result, ...verdict(result), ...(cleanup.length ? { cleanup } : {}) };
  },
};
