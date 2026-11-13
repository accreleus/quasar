// What the stream scenarios share (stream-hold, transport-fallback, input-kbm): the sign-in, the launch through the game
// page's own Launch options, the page probe, the covered screenshot and the verdict rule. Moved out of stream-hold.mjs
// unchanged; the scenarios read only what the product exposes (window.__qBench, the DOM, the API) and wrap no browser API.

export const FIRST_FRAME_LIMIT_S = 30;
export const CODEC_LABEL = { h264: "H.264", h265: "HEVC", av1: "AV1", pyrowave: "Auto" };
const SESSION_URL = /\/app\/session\/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})(?:[/?#]|$)/i;

// Steam's sign-in QR, as fractions of the picture (measured on the lab; the same box the lab proofs cover).
const QR = { x: 0.14, y: 0.185, w: 0.22, h: 0.545 };

export const finite = (x) => typeof x === "number" && Number.isFinite(x);
export const shown = (v) => JSON.stringify(v)?.slice(0, 40);
// A criterion that was not judged has no observed value (the validator's rule).
export const crit = (id, pass, observed, expected) => ({ id, pass, observed: pass === null ? null : observed, expected });

/** The first_frame_s criterion: null is no frame by the deadline (false); a time that is not a non-negative number is not judged. */
export const firstFrameCriterion = (firstFrameS) =>
  crit("first_frame_s", finite(firstFrameS) && firstFrameS >= 0 ? firstFrameS <= FIRST_FRAME_LIMIT_S : firstFrameS === null ? false : null, finite(firstFrameS) && firstFrameS >= 0 ? firstFrameS : null, `<= ${FIRST_FRAME_LIMIT_S} s: the first frame the client decoded (it can still be under the loader)`);

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


/** Records the stack's refusals the page sees (a refused launch ends the wait for a session with its reason). Returns the pending reads; await them at the end. */
export function watchApiErrors(page, ctx) {
  const origin = new URL(ctx.baseUrl).origin;
  const pending = [];
  ctx.refused = null;
  page.on("response", (r) => {
    const u = new URL(r.url());
    if (u.origin !== origin || r.status() < 400 || !u.pathname.startsWith("/v1/")) return;
    const method = r.request().method();
    pending.push((async () => {
      const body = (await r.text().catch(() => "")).slice(0, 300);
      ctx.rec.pageStep("api-error", { method, path: u.pathname, status: r.status(), body });
      if (method === "POST" && u.pathname === "/v1/sessions") ctx.refused = `the launch was refused: ${r.status()} ${body.trim()}`;
    })());
  });
  return pending;
}

/** The game page for `app`, the launch options, Play now; returns the session this run launched and the time of the click. */
export async function play(page, ctx, app) {
  const { params, rec, api } = ctx;
  const switches = [params.transport === "webtransport" && "transport=webtransport", params.codec === "pyrowave" && "codec=pyrowave"].filter(Boolean);
  await page.goto(`${ctx.baseUrl}/app/game/${encodeURIComponent(app.id)}${switches.length ? `?${switches.join("&")}` : ""}`, { waitUntil: "domcontentloaded" });
  const tClick = await pickAndPlay(page, ctx);
  rec.step("play-clicked", { app: params.app });
  const sid = await waitForSession(page, ctx);
  const row0 = await api.session(sid);
  if (!row0 || row0.id !== sid) throw new Error("the API no longer returns the session this run launched");
  rec.step("session-route", { session: sid });
  return { sid, tClick };
}

/** Waits for a frame (a bench count, or the playing <video>'s), up to the limit plus 90 s for a cold host. Returns the time it was seen, or null. */
export async function waitFirstFrame(page, ctx, tClick) {
  const deadline = tClick + (FIRST_FRAME_LIMIT_S + 90) * 1000;
  while (ctx.now() < deadline) {
    const p = await probe(page);
    if ([p.frames, p.videoFrames].some((n) => finite(n) && n > 0)) return ctx.now();
    await ctx.sleep(250);
  }
  return null;
}

export async function signIn(page, ctx) {
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

/** What the page shows right now: bench frames, the playing <video> (WebRTC), whether the loader has revealed the stream (noted, never a frame), and whether it is visible. */
export const probe = (page) =>
  page.evaluate((sel) => ({
    frames: window.__qBench?.stats().frames ?? 0,
    videoFrames: document.querySelector("video.session-video:not([hidden])")?.getVideoPlaybackQuality?.().totalVideoFrames ?? 0,
    revealed: !!document.querySelector(sel),
    visible: document.visibilityState,
    focused: document.hasFocus?.(),
  }), REVEALED);

/** A reason this client cannot run `params` at all (a scenario is then `skipped`), or null. */
export function cannotRun(params, client) {
  if (params.transport === "webtransport" && !client.webtransport) return "this browser has no WebTransport";
  if (params.codec === "pyrowave" ? !client.webgpu : client.decode?.[params.codec]?.supported === false) return `this client cannot decode ${params.codec}`;
  return null;
}

export const skipped = (reason) => ({ status: "skipped", reason, criteria: [], sessions: [], clock_offset_ms: null });

// ── trusted input through the DevTools protocol (never a DOM event the page could tell from a user's) ──

export const openCdp = (page) => page.context().newCDPSession(page);
export const keyEvent = (cdp, type, k) => cdp.send("Input.dispatchKeyEvent", { type, code: k.code, key: k.key, windowsVirtualKeyCode: k.vk });
export const mouseEvent = (cdp, params) => cdp.send("Input.dispatchMouseEvent", params);

/** The middle of the stream surface in page pixels, or null when there is none. The surface is the one `shotCovered` places its cover on: on WebTransport the first .session-video is a hidden <video> and the picture is the canvas. */
export const streamCentre = (page) =>
  page.evaluate(() => {
    const el = [...document.querySelectorAll(".session-video")].find((e) => e.videoWidth || e.tagName === "CANVAS");
    const r = el?.getBoundingClientRect?.();
    return r && r.width > 0 && r.height > 0 ? { x: r.left + r.width / 2, y: r.top + r.height / 2 } : null;
  });

export const LOCK_WAIT_S = 5; // how long the pointer lock, or the layer that takes it, is waited for
const HUD_WAIT_S = 2; // how long the HUD's "Capture input" button is waited for after a click on the stage

/**
 * Takes the pointer lock the way the product does (SessionPage.tsx): a stage click while input is not captured only opens the HUD
 * shelf; the lock is requested from the "Click to play" layer (`.session-play`), which the page raises once the loader is inert and the
 * channel is open (and only when Play went immersive, the default). The product removes that layer on the first click, so this waits
 * for it, clicks it ONCE with a trusted click, then waits for the lock. With no layer within LOCK_WAIT_S and no lock (auto-capture can
 * take it with no click) the launch was not immersive: the HUD's own route is tried, a click on the stage (which opens the shelf) and a
 * click on its "Capture input" button (an aria-label the product sets, HudBar.tsx).
 * Returns { held, clicks, hud, at }: `clicks` is the clicks on the layer (0 or 1); `hud` is null (not tried), "no-button" (the shelf showed
 * no enabled button) or "clicked"; `at` is where the last click was.
 */
export async function takeLock(page, cdp, { now, sleep }) {
  const held = () => page.evaluate(() => document.pointerLockElement?.classList?.contains("session-video") === true);
  const spotOf = (sel) =>
    page.evaluate((s) => {
      const el = document.querySelector(s);
      const r = el && !el.disabled ? el.getBoundingClientRect?.() : null;
      return r && r.width > 0 && r.height > 0 ? { x: r.left + r.width / 2, y: r.top + r.height / 2 } : null;
    }, sel);
  const click = async (at) => {
    await mouseEvent(cdp, { type: "mouseMoved", x: at.x, y: at.y });
    await mouseEvent(cdp, { type: "mousePressed", x: at.x, y: at.y, button: "left", buttons: 1, clickCount: 1 });
    await mouseEvent(cdp, { type: "mouseReleased", x: at.x, y: at.y, button: "left", buttons: 0, clickCount: 1 });
  };
  // Polls `ready` until it returns something truthy or the deadline passes.
  const wait = async (s, ready) => {
    const until = now() + s * 1000;
    for (;;) {
      const v = await ready();
      if (v) return v;
      if (now() >= until) return v;
      await sleep(250);
    }
  };
  const layer = await wait(LOCK_WAIT_S, async () => ((await held()) ? "held" : spotOf(".session-play")));
  if (layer === "held") return { held: true, clicks: 0, hud: null, at: null };
  if (layer) {
    await click(layer);
    return { held: !!(await wait(LOCK_WAIT_S, held)), clicks: 1, hud: null, at: layer };
  }
  // No layer: the launch was not immersive. A click on the stage opens the HUD shelf; its button takes the lock.
  const centre = await streamCentre(page);
  if (!centre) return { held: false, clicks: 0, hud: "no-button", at: null };
  await click(centre);
  const button = await wait(HUD_WAIT_S, () => spotOf('button[aria-label="Capture input"]'));
  if (!button) return { held: false, clicks: 0, hud: "no-button", at: centre };
  await click(button);
  return { held: !!(await wait(LOCK_WAIT_S, held)), clicks: 0, hud: "clicked", at: button };
}
