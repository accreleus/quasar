// Scenario input-kbm: keys, mouse buttons, wheel and relative motion sent to a live stream as trusted events through the
// DevTools protocol (Input.dispatchKeyEvent / Input.dispatchMouseEvent: no synthetic DOM events), in one fixed sequence.
// What can be judged from the client today is judged; the rest is `pass: null` and says what is missing.
//
// The page's own input channel (DataChannel or WebTransport) is not visible to the runner, the product's held-key set is not
// exposed, and the reference app echoes one bit, so what the HOST received per key, button and motion unit is not
// known here. input-sent.json holds what was sent and what the page received, for the host half to compare.
import { check as holdCheck } from "./stream-hold.mjs";
import { cannotRun, clickStream, crit, finite, firstFrameCriterion, keyEvent, mouseEvent, openCdp, play, shotCovered, signIn, skipped, streamCentre, verdict, waitFirstFrame, watchApiErrors } from "./stream-common.mjs";

export const name = "input-kbm";
const STEP_MS = 60; // between two steps of the sequence
const LOCK_WAIT_S = 3;
const I2P_PRESSES = 8; // __qBench.pressKey rounds, spaced so each echo is matched on its own
const I2P_GAP_MS = 600;
const I2P_KEY = "Space";

/** The sequence, fixed: the same events in the same order on every run. Keys are held 40 ms; motion is relative (pointer-locked deltas). */
export const SEQUENCE = [
  { kind: "key", code: "KeyW", key: "w", vk: 87 },
  { kind: "key", code: "KeyA", key: "a", vk: 65 },
  { kind: "key", code: "KeyS", key: "s", vk: 83 },
  { kind: "key", code: "KeyD", key: "d", vk: 68 },
  { kind: "key", code: "Space", key: " ", vk: 32 },
  { kind: "key", code: "ShiftLeft", key: "Shift", vk: 16 },
  { kind: "button", button: "left" },
  { kind: "button", button: "right" },
  { kind: "button", button: "middle" },
  { kind: "wheel", dy: -120 },
  { kind: "wheel", dy: 120 },
  { kind: "move", dx: 40, dy: 0 },
  { kind: "move", dx: 0, dy: 30 },
  { kind: "move", dx: -40, dy: -30 },
];
const BUTTONS = { left: [0, 1], right: [2, 2], middle: [1, 4] }; // name -> [DOM button, buttons bit]

/** In the page: records what the browser delivers to it (trusted or not), so a key or button left down is seen. */
export function installRecorder() {
  const r = { keys: [], buttons: [], log: [], move: { x: 0, y: 0 }, wheel: { x: 0, y: 0 }, untrusted: 0 };
  window.__e2eInput = r;
  const on = (type, fn) => window.addEventListener(type, (e) => { if (!e.isTrusted) r.untrusted += 1; fn(e); }, true);
  const toggle = (list, v, down) => { const i = list.indexOf(v); if (down && i < 0) list.push(v); else if (!down && i >= 0) list.splice(i, 1); };
  on("keydown", (e) => { toggle(r.keys, e.code, true); r.log.push(["keydown", e.code]); });
  on("keyup", (e) => { toggle(r.keys, e.code, false); r.log.push(["keyup", e.code]); });
  on("mousedown", (e) => { toggle(r.buttons, e.button, true); r.log.push(["mousedown", e.button]); });
  on("mouseup", (e) => { toggle(r.buttons, e.button, false); r.log.push(["mouseup", e.button]); });
  on("mousemove", (e) => { r.move.x += e.movementX || 0; r.move.y += e.movementY || 0; });
  on("wheel", (e) => { r.wheel.x += e.deltaX || 0; r.wheel.y += e.deltaY || 0; r.log.push(["wheel", e.deltaY]); });
}

/** Why `p` cannot be run, or null (the same parameters as stream-hold; hold_s is how long to keep the stream after the sequence). */
export const check = holdCheck;

const median = (xs) => { const s = [...xs].sort((a, b) => a - b); return s[Math.floor((s.length - 1) / 2)]; };

/**
 * `seen`: `locked` (the stream surface holds the pointer lock), `held` ({ keys, buttons } the page was left holding, or null when the
 * recorder could not be read), `i2p` (finite samples, ms), `i2pMissed`.
 */
export function judge({ firstFrameS, seen }) {
  const s = seen;
  const nothing = "no key or button the page was sent down is still down at the end (as the page received them; the page's own held-key set is not exposed, so a key the page failed to release on the host is not seen here)";
  const noLog = "not judged: the reference app does not log individual keys, buttons or motion today (its echo is one bit), and the runner cannot see the page's input channel. What was sent, and what the page received, is in input-sent.json for the host half to compare";
  let i2p = crit("input_to_photon_ms", null, null, `not judged: the marker echoed no key (${s.i2pMissed ?? 0} abandoned): the app has no echo, or no marker was found`);
  if (s.i2p.length) i2p = crit("input_to_photon_ms", null, null, `not judged: no limit is calibrated for input-to-photon. Measured ${s.i2p.length} samples, median ${median(s.i2p)} ms, max ${Math.max(...s.i2p)} ms (${s.i2pMissed ?? 0} abandoned)`);
  return [
    firstFrameCriterion(firstFrameS),
    crit("pointer_lock_taken", s.locked, s.locked ? "the stream surface holds the pointer lock" : "no pointer lock", "document.pointerLockElement is the stream surface after a trusted click on it"),
    crit("keys_match", null, null, noLog),
    crit("buttons_match", null, null, noLog),
    crit("motion_sum_matches", null, null, noLog),
    s.held === null
      ? crit("nothing_held_at_end", null, null, "not judged: the page's input record could not be read")
      : crit("nothing_held_at_end", s.held.keys.length === 0 && s.held.buttons.length === 0, s.held.keys.length || s.held.buttons.length ? `still down: keys ${JSON.stringify(s.held.keys)}, buttons ${JSON.stringify(s.held.buttons)}` : "nothing down", nothing),
    i2p,
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
    const pending = watchApiErrors(page, ctx);

    let result;
    let stage = "sign-in";
    try {
      await signIn(page, ctx);
      rec.step("signed-in");
      stage = "the launch";
      const app = (await api.apps()).find((a) => a.name === params.app);
      if (!app) return skipped(`app "${params.app}" is not in this user's catalog`);
      const { sid, tClick } = await play(page, ctx, app);
      stage = "the input";
      const firstFrameAt = await waitFirstFrame(page, ctx, tClick);
      const firstFrameS = firstFrameAt === null ? null : (firstFrameAt - tClick) / 1000;
      rec.step("first-frame", { first_frame_s: firstFrameS });
      await shot(firstFrameAt === null ? "no-frame" : "first-frame");

      const seen = { locked: false, held: null, i2p: [], i2pMissed: 0 };
      if (firstFrameAt !== null) {
        const cdp = await openCdp(page);
        await page.evaluate(installRecorder);
        const lockHeld = () => page.evaluate(() => document.pointerLockElement?.classList?.contains("session-video") === true);
        if (!(await lockHeld())) await clickStream(page, cdp);
        for (let i = 0; i < LOCK_WAIT_S * 4 && !(await lockHeld()); i += 1) await sleep(250);
        seen.locked = await lockHeld();
        rec.step("pointer-lock", { held: seen.locked });

        const centre = (await streamCentre(page)) ?? { x: 0, y: 0 };
        const sent = [];
        const tStart = now();
        let pos = { ...centre };
        for (const [seq, step] of SEQUENCE.entries()) {
          const t = now() - tStart;
          if (step.kind === "key") {
            await keyEvent(cdp, "keyDown", step);
            await sleep(40);
            await keyEvent(cdp, "keyUp", step);
          } else if (step.kind === "button") {
            const [, bits] = BUTTONS[step.button];
            await mouseEvent(cdp, { type: "mousePressed", x: pos.x, y: pos.y, button: step.button, buttons: bits, clickCount: 1 });
            await sleep(40);
            await mouseEvent(cdp, { type: "mouseReleased", x: pos.x, y: pos.y, button: step.button, buttons: 0, clickCount: 1 });
          } else if (step.kind === "wheel") {
            await mouseEvent(cdp, { type: "mouseWheel", x: pos.x, y: pos.y, deltaX: 0, deltaY: step.dy });
          } else {
            pos = { x: pos.x + step.dx, y: pos.y + step.dy };
            await mouseEvent(cdp, { type: "mouseMoved", x: pos.x, y: pos.y });
          }
          sent.push({ seq, ...step, ms_after_start: t });
          await sleep(STEP_MS);
        }
        rec.step("sequence-sent", { steps: SEQUENCE.length });

        // Input-to-photon: the product's own bench key, one echo at a time.
        let pressed = 0;
        for (let i = 0; i < I2P_PRESSES; i += 1) {
          if (await page.evaluate((k) => window.__qBench?.pressKey(k) === true, I2P_KEY)) pressed += 1;
          await sleep(I2P_GAP_MS);
        }
        rec.step("bench-keys", { pressed });

        const holdUntil = now() + params.hold_s * 1000;
        while (now() < holdUntil) await sleep(Math.min(1000, holdUntil - now()));
        await shot("end");
        const dump = await page.evaluate(() => window.__qBench?.dump() ?? null);
        const windows = dump?.windows ?? [];
        rec.json("bench-windows.json", { windows });
        seen.i2p = windows.flatMap((w) => (Array.isArray(w?.i2p_ms) ? w.i2p_ms : [])).filter(finite);
        seen.i2pMissed = windows.reduce((n, w) => n + (finite(w?.i2p_missed) ? w.i2p_missed : 0), 0);
        const received = await page.evaluate(() => window.__e2eInput ?? null);
        seen.held = received && Array.isArray(received.keys) && Array.isArray(received.buttons) ? { keys: received.keys.map(String), buttons: received.buttons.filter(finite) } : null;
        rec.json("input-sent.json", {
          sequence: sent,
          received_by_page: received ? { events: received.log, motion_sum: received.move, wheel_sum: received.wheel, untrusted: received.untrusted } : null,
          held_at_end: seen.held,
          bench_keys_pressed: pressed,
        });
      }
      const row = await api.session(sid);
      if (row) rec.json("session.json", { session: row });
      result = { criteria: judge({ firstFrameS, seen }), clock_offset_ms: null };
      if (row && ["stopped", "failed"].includes(row.state)) result = { ...result, status: "error", reason: `the session ended during the run (it was ${row.state} before this run stopped it)` };
    } catch (err) {
      const message = String(err?.message ?? err);
      if (rec.holding) rec.pageStep("error", { message });
      else rec.step("error", { message });
      result = { status: "error", reason: rec.holding ? `${stage} did not complete (the page's own words are not written before its sign-in is read)` : message, criteria: [], clock_offset_ms: null };
    } finally {
      await Promise.allSettled(pending);
    }
    return { ...result, ...verdict(result) };
  },
};
