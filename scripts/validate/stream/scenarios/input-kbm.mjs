// Scenario input-kbm: keys, mouse buttons, wheel and relative motion sent to a live stream as trusted events through the
// DevTools protocol (Input.dispatchKeyEvent / Input.dispatchMouseEvent: no synthetic DOM events), in one fixed sequence.
// What can be judged from the client today is judged; the rest is `pass: null` and says what is missing.
//
// The page's own input channel (DataChannel or WebTransport) is not visible to the runner, the product's held-key set is not
// exposed, and the reference app echoes one bit, so what the HOST received per key, button and motion unit is not
// known here. input-sent.json holds what was sent and what the page received, for the host half to compare.
import { check as holdCheck } from "./stream-hold.mjs";
import { cannotRun, crit, finite, firstFrameCriterion, keyEvent, mouseEvent, openCdp, play, shotCovered, signIn, skipped, streamCentre, takeLock, verdict, waitFirstFrame, watchApiErrors } from "./stream-common.mjs";

export const name = "input-kbm";
const STEP_MS = 60; // between two steps of the sequence
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
  { kind: "move", dx: 10, dy: -5 },
]; // the moves sum to (50, 25), not zero: no motion and motion that cancels are different bundles
const BUTTONS = { left: [0, 1], right: [2, 2], middle: [1, 4] }; // name -> [DOM button, buttons bit]

/** In the page: records what the browser delivers to it (trusted or not), so a key or button left down is seen. */
export function installRecorder() {
  const r = { keys: [], buttons: [], log: [], moves: [], moveCount: 0, move: { x: 0, y: 0 }, wheel: { x: 0, y: 0 }, untrusted: 0 };
  window.__e2eInput = r;
  const on = (type, fn) => window.addEventListener(type, (e) => { if (!e.isTrusted) r.untrusted += 1; fn(e); }, true);
  const toggle = (list, v, down) => { const i = list.indexOf(v); if (down && i < 0) list.push(v); else if (!down && i >= 0) list.splice(i, 1); };
  on("keydown", (e) => { toggle(r.keys, e.code, true); r.log.push(["keydown", e.code]); });
  on("keyup", (e) => { toggle(r.keys, e.code, false); r.log.push(["keyup", e.code]); });
  on("mousedown", (e) => { toggle(r.buttons, e.button, true); r.log.push(["mousedown", e.button]); });
  on("mouseup", (e) => { toggle(r.buttons, e.button, false); r.log.push(["mouseup", e.button]); });
  on("mousemove", (e) => {
    r.move.x += e.movementX || 0;
    r.move.y += e.movementY || 0;
    r.moveCount += 1;
    if (r.moves.length < 200) r.moves.push([e.movementX, e.movementY, e.isTrusted]);
  });
  on("wheel", (e) => { r.wheel.x += e.deltaX || 0; r.wheel.y += e.deltaY || 0; r.log.push(["wheel", e.deltaY]); });
}

/** In the page: forgets the motion seen so far (the pointer's walk to the lock click), so what is kept is the sequence's own. */
export function resetMotion() {
  const r = window.__e2eInput;
  r.move = { x: 0, y: 0 };
  r.moves = [];
  r.moveCount = 0;
}

const sum = (xs) => xs.reduce((a, v) => ({ x: a.x + v.dx, y: a.y + v.dy }), { x: 0, y: 0 });
// The page is untrusted: a move is two finite numbers and a flag, anything else is null.
const cleanMoves = (m) => (Array.isArray(m) ? m.slice(0, 200).map((v) => [finite(v?.[0]) ? v[0] : null, finite(v?.[1]) ? v[1] : null, v?.[2] === true]) : []);

/** Why `p` cannot be run, or null (the same parameters as stream-hold; hold_s is how long to keep the stream after the sequence). */
export const check = holdCheck;

const median = (xs) => { const s = [...xs].sort((a, b) => a - b); return s[Math.floor((s.length - 1) / 2)]; };

/**
 * `seen`: `locked` (the stream surface holds the pointer lock), `lockClicks` (clicks made on the layer that takes it), `sequenceRan` (the
 * sequence was sent), `held` ({ keys, buttons } the page was left holding, or null when the recorder could not be read), `i2p` (finite
 * samples, ms), `i2pMissed`, `lockHud` (null, "no-button" or "clicked": the HUD's route to the lock), `motion` ({ sent: {x, y}, received:
 * {x, y} | null, events } what was sent against what the page received).
 */
export function judge({ firstFrameS, seen }) {
  const s = seen;
  const nothing = "no key or button the page was sent down is still down at the end (as the page received them; the page's own held-key set is not exposed, so a key the page failed to release on the host is not seen here)";
  const noLog = "not judged: the reference app does not log individual keys, buttons or motion today (its echo is one bit), and the runner cannot see the page's input channel. What was sent, and what the page received, is in input-sent.json for the host half to compare";
  const unsent = "not judged: no first frame, so the input sequence was not sent";
  let i2p = !s.sequenceRan ? crit("input_to_photon_ms", null, null, unsent) : crit("input_to_photon_ms", null, null, `not judged: the marker echoed no key (${s.i2pMissed ?? 0} abandoned): the app has no echo, or no marker was found`);
  if (s.i2p.length) i2p = crit("input_to_photon_ms", null, null, `not judged: no limit is calibrated for input-to-photon. Measured ${s.i2p.length} samples, median ${median(s.i2p)} ms, max ${Math.max(...s.i2p)} ms (${s.i2pMissed ?? 0} abandoned)`);
  return [
    firstFrameCriterion(firstFrameS),
    s.sequenceRan
      ? crit("pointer_lock_taken", s.locked, s.locked ? `the stream surface holds the pointer lock${s.lockHud === "clicked" ? ' (taken through the HUD\'s "Capture input": the launch was not immersive, so there was no "Click to play" layer)' : ""}` : s.lockClicks ? 'no pointer lock after the click on the "Click to play" layer' : `no pointer lock: the launch was not immersive, so there is no layer to click${s.lockHud === "clicked" ? '; the HUD\'s "Capture input" was clicked and did not lock' : s.lockHud === "no-button" ? '; a click on the stream showed no "Capture input" button in the HUD' : ""}`, `document.pointerLockElement is the stream surface (the product takes it from a trusted click on its "Click to play" layer)`)
      : crit("pointer_lock_taken", null, null, unsent),
    crit("keys_match", null, null, noLog),
    crit("buttons_match", null, null, noLog),
    crit("motion_sum_matches", null, null, s.motion ? `${noLog}. The page received ${s.motion.events} mousemove events${s.motion.received ? ` summing to (${s.motion.received.x}, ${s.motion.received.y})` : " (their sum could not be read)"} against (${s.motion.sent.x}, ${s.motion.sent.y}) sent` : noLog),
    !s.sequenceRan
      ? crit("nothing_held_at_end", null, null, unsent)
      : s.held === null
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
    let cdp = null;
    let down = null; // the key or button currently down: { kind, ...step, pos }
    const cleanup = [];
    // Releases whatever is down; a release that fails is a step and a "left behind" line, never swallowed.
    const release = async (by) => {
      if (!down) return;
      const d = down;
      down = null;
      try {
        if (d.kind === "key") await keyEvent(cdp, "keyUp", d);
        else await mouseEvent(cdp, { type: "mouseReleased", x: d.pos.x, y: d.pos.y, button: d.button, buttons: 0, clickCount: 1 });
        rec.step("released", { what: d.code ?? d.button, by });
      } catch (err) {
        rec.step("release-failed", { what: d.code ?? d.button, message: String(err?.message ?? err) });
        cleanup.push(`${d.kind === "key" ? `key ${d.code}` : `mouse button ${d.button}`} could not be released (${String(err?.message ?? err)})`);
      }
    };
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

      const seen = { locked: false, lockClicks: 0, lockHud: null, motion: null, sequenceRan: false, held: null, i2p: [], i2pMissed: 0 };
      if (firstFrameAt !== null) {
        cdp = await openCdp(page);
        await page.evaluate(installRecorder);
        const lock = await takeLock(page, cdp, ctx);
        seen.locked = lock.held;
        seen.lockClicks = lock.clicks;
        seen.lockHud = lock.hud;
        rec.step("pointer-lock", { held: lock.held, clicks: lock.clicks, hud: lock.hud });

        // The pointer starts where the lock click left it; with no click, on the visible stream surface.
        let centre = lock.at ?? (await streamCentre(page));
        if (!centre) {
          rec.step("no-stream-surface", { why: "no visible stream surface: the pointer events go to 0,0" });
          centre = { x: 0, y: 0 };
        }
        await page.evaluate(resetMotion);
        seen.sequenceRan = true;
        const sent = [];
        const tStart = now();
        let pos = { ...centre };
        for (const [seq, step] of SEQUENCE.entries()) {
          const t = now() - tStart;
          if (step.kind === "key") {
            down = { ...step };
            await keyEvent(cdp, "keyDown", step);
            await sleep(40);
            await release("the sequence");
          } else if (step.kind === "button") {
            const [, bits] = BUTTONS[step.button];
            down = { ...step, pos: { ...pos } };
            await mouseEvent(cdp, { type: "mousePressed", x: pos.x, y: pos.y, button: step.button, buttons: bits, clickCount: 1 });
            await sleep(40);
            await release("the sequence");
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
        const sentMotion = sum(sent.filter((x) => x.kind === "move"));
        const receivedMotion = received && finite(received.move?.x) && finite(received.move?.y) ? { x: received.move.x, y: received.move.y } : null;
        seen.motion = { sent: sentMotion, received: receivedMotion, events: finite(received?.moveCount) ? received.moveCount : 0 };
        seen.held = received && Array.isArray(received.keys) && Array.isArray(received.buttons) ? { keys: received.keys.map(String), buttons: received.buttons.filter(finite) } : null;
        rec.json("input-sent.json", {
          sequence: sent,
          sent_motion_sum: sentMotion,
          received_by_page: received ? { events: received.log, motion_sum: received.move, mouse_moves: cleanMoves(received.moves), mouse_moves_total: seen.motion.events, wheel_sum: received.wheel, untrusted: received.untrusted } : null,
          held_at_end: seen.held,
          bench_keys_pressed: pressed,
        });
      }
      const row = await api.session(sid);
      if (!row || row.id !== sid) throw new Error("the API no longer returns the session this run started");
      rec.json("session.json", { session: row });
      result = { criteria: judge({ firstFrameS, seen }), clock_offset_ms: null };
      if (row && ["stopped", "failed"].includes(row.state)) result = { ...result, status: "error", reason: `the session ended during the run (it was ${row.state} before this run stopped it)` };
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
