// LEVEL=session only. Launches an app via the real API, waits for `running`,
// opens the session view, and requires a REAL decode verdict —
// `getVideoPlaybackQuality().totalVideoFrames > 0` — never just `state ===
// "running"` (.claude/rules/webrtc-testing.md: "running alone is never a
// pass"; also the #68/#304/interpipe history of sessions that reach running
// while delivering zero or near-zero frames). Tears down via DELETE and
// asserts the session reaches a terminal state (stopped|failed —
// protocol/schema.md's session.state CHECK).
//
// Deliberately refuses to run when the harness target is the self-booted
// local stack: there is no GPU/compositor/encoder in that ephemeral
// container, so a "pass" there would be a lie about what got tested (per the
// spec: "this journey must REFUSE to run when TARGET=local with a clear
// message"). It is included in the file tree and wired behind LEVEL=session,
// but is validated for real only against a live GPU stack (Tower/hermes).
const TOTAL_FRAMES_TIMEOUT_MS = 45000;
// 120s, matching the loader's max-hold precedent elsewhere in the client
// (MINOR 11a) — a cold host (image pull, compositor/agent warm-up) can take
// noticeably longer than a "just poll for a few seconds" timeout assumes.
const SESSION_RUNNING_TIMEOUT_MS = 120000;
const TERMINAL_TIMEOUT_MS = 30000;

async function pollSession(page, baseUrl, id, wantStates, timeoutMs) {
  const deadline = Date.now() + timeoutMs;
  let last = null;
  while (Date.now() < deadline) {
    // eslint-disable-next-line no-await-in-loop
    last = await page.evaluate(
      async ({ baseUrl, id }) => {
        const token = localStorage.getItem("quasar.auth.token");
        const resp = await fetch(`${baseUrl}/v1/sessions/${id}`, {
          headers: { Authorization: `Bearer ${token}` },
        });
        const body = await resp.json().catch(() => ({}));
        return { status: resp.status, session: body.session };
      },
      { baseUrl, id },
    );
    if (last.session && wantStates.includes(last.session.state)) return last.session;
    if (last.session && last.session.state === "failed") return last.session;
    // eslint-disable-next-line no-await-in-loop
    await new Promise((r) => setTimeout(r, 1500));
  }
  throw new Error(
    `session ${id} never reached ${wantStates.join("/")} within ${timeoutMs}ms ` +
      `(last seen: ${last ? JSON.stringify(last) : "no response"})`,
  );
}

// Bench-app selection (MINOR 11a): $QUASAR_VALIDATE_APP names an exact app,
// set by the operator when a stack has more than one candidate and the
// default heuristics would pick the wrong one. Otherwise prefer a name
// matching /bench/i (the operator convention this harness's own local-stack
// seeding follows — see scripts/dx/validate.sh's BENCH_APP_NAME
// "validate-bench"). Falling back to items[0] is a last resort worth a loud
// WARN in the report: an arbitrary app was launched, and if it is unusually
// slow/large that is why, not a harness defect.
function pickBenchApp(items, ctx) {
  const wanted = (typeof process !== "undefined" && process.env && process.env.QUASAR_VALIDATE_APP) || "";
  if (wanted) {
    const named = items.find((a) => a.name === wanted);
    if (named) return named;
    ctx.warnings.push(`QUASAR_VALIDATE_APP="${wanted}" not found in GET /v1/apps — falling back to heuristics`);
  }
  const bench = items.find((a) => /bench/i.test(a.name));
  if (bench) return bench;
  ctx.warnings.push(
    `no app name matched /bench/i and $QUASAR_VALIDATE_APP is unset — falling back to the first ` +
      `entitled app ("${items[0].name}"); set $QUASAR_VALIDATE_APP to pin a specific bench app`,
  );
  return items[0];
}

export default {
  name: "session-launch",
  role: "user",
  level: "session",
  async run(page, ctx) {
    if (ctx.target === "local") {
      throw new Error(
        "session-launch refuses to run against TARGET=local — the self-booted ephemeral stack " +
          "has no GPU/compositor/encoder, so a decode verdict here would be meaningless. Run with " +
          "LEVEL=session TARGET=<live-gpu-stack-base-url> (e.g. https://tower.local:18443).",
      );
    }

    await page.goto(ctx.baseUrl + "/app", { waitUntil: "networkidle" });
    if (new URL(page.url()).pathname.startsWith("/login")) {
      throw new Error("landed on /login — user storage-state did not authenticate before session-launch");
    }

    const apps = await page.evaluate(async (baseUrl) => {
      const token = localStorage.getItem("quasar.auth.token");
      const resp = await fetch(`${baseUrl}/v1/apps`, { headers: { Authorization: `Bearer ${token}` } });
      const body = await resp.json().catch(() => ({}));
      return { status: resp.status, items: body.items || [] };
    }, ctx.baseUrl);
    if (apps.status !== 200 || apps.items.length === 0) {
      throw new Error(`GET /v1/apps returned ${apps.status} with ${apps.items.length} launchable apps — no bench app available`);
    }
    const app = pickBenchApp(apps.items, ctx);

    const launch = await page.evaluate(
      async ({ baseUrl, appId }) => {
        const token = localStorage.getItem("quasar.auth.token");
        const resp = await fetch(`${baseUrl}/v1/sessions`, {
          method: "POST",
          headers: { Authorization: `Bearer ${token}`, "Content-Type": "application/json" },
          body: JSON.stringify({ app_id: appId }),
        });
        const body = await resp.json().catch(() => ({}));
        return { status: resp.status, body };
      },
      { baseUrl: ctx.baseUrl, appId: app.id },
    );
    if (launch.status !== 201) {
      throw new Error(`POST /v1/sessions returned ${launch.status}: ${JSON.stringify(launch.body)}`);
    }
    const sessionId = launch.body.session.id;

    // Primary failure (decode/running) must survive even if teardown ALSO
    // fails — a `finally` that throws its own error replaces whatever was
    // already in flight, which would silently swap "the stream never
    // decoded" for "DELETE returned 500" in the report (MINOR 11b). Teardown
    // problems are real and worth surfacing, but as a warning attached to
    // the (still-reported) primary outcome, never as a throw from `finally`.
    let primaryError = null;
    try {
      const running = await pollSession(page, ctx.baseUrl, sessionId, ["running"], SESSION_RUNNING_TIMEOUT_MS);
      if (running.state !== "running") {
        throw new Error(`session ${sessionId} reached terminal state '${running.state}' before running: ${running.error_message || running.state_detail || ""}`);
      }

      await page.goto(`${ctx.baseUrl}/app/session/${sessionId}`, { waitUntil: "networkidle" });
      await ctx.screenshot(page, "session-attached");

      // Decode verdict — never trust `running` alone.
      const deadline = Date.now() + TOTAL_FRAMES_TIMEOUT_MS;
      let totalVideoFrames = 0;
      while (Date.now() < deadline) {
        // eslint-disable-next-line no-await-in-loop
        totalVideoFrames = await page.evaluate(() => {
          const video = document.querySelector("video");
          if (!video || typeof video.getVideoPlaybackQuality !== "function") return 0;
          return video.getVideoPlaybackQuality().totalVideoFrames;
        });
        if (totalVideoFrames > 0) break;
        // eslint-disable-next-line no-await-in-loop
        await new Promise((r) => setTimeout(r, 1000));
      }
      ctx.metrics.total_video_frames = totalVideoFrames;
      if (totalVideoFrames <= 0) {
        throw new Error(
          `getVideoPlaybackQuality().totalVideoFrames stayed 0 for ${TOTAL_FRAMES_TIMEOUT_MS}ms — ` +
            `session reached 'running' but never decoded a frame (running is not a pass)`,
        );
      }
      await ctx.screenshot(page, "session-decoding");
    } catch (err) {
      primaryError = err;
    } finally {
      try {
        const stop = await page.evaluate(
          async ({ baseUrl, id }) => {
            const token = localStorage.getItem("quasar.auth.token");
            const resp = await fetch(`${baseUrl}/v1/sessions/${id}`, {
              method: "DELETE",
              headers: { Authorization: `Bearer ${token}` },
            });
            return resp.status;
          },
          { baseUrl: ctx.baseUrl, id: sessionId },
        );
        if (![200, 202].includes(stop)) {
          ctx.warnings.push(`teardown: DELETE /v1/sessions/${sessionId} returned ${stop} (want 200/202)`);
        } else {
          const terminal = await pollSession(page, ctx.baseUrl, sessionId, ["stopped", "failed"], TERMINAL_TIMEOUT_MS);
          if (!["stopped", "failed"].includes(terminal.state)) {
            ctx.warnings.push(`teardown: session ${sessionId} did not reach a terminal state (last: ${terminal.state})`);
          }
        }
      } catch (teardownErr) {
        ctx.warnings.push(
          `teardown of session ${sessionId} threw: ${teardownErr instanceof Error ? teardownErr.message : String(teardownErr)}`,
        );
      }
    }

    if (primaryError) throw primaryError;
  },
};
