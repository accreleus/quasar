// Shared evidence-capture listeners (console/network) — factored out of
// runner.mjs so a journey helper that opens its OWN secondary browser context
// (adminGate.mjs's assertUserDenied) instruments it identically instead of
// silently going unmonitored (MINOR 8, adversarial review).
export function isSameOrigin(url, baseUrl) {
  try {
    return new URL(url).origin === new URL(baseUrl).origin;
  } catch {
    return false;
  }
}

// A narrow, explicit allowlist of expected non-2xx responses — never a blanket
// "ignore errors" escape hatch. Currently one entry: POST /v1/me/devices can
// 429 (per-user token bucket, burst=5/10s — control-plane/internal/devices/handler.go)
// when the SAME minted identity loads many pages back to back, which this
// harness deliberately does (one user storage-state reused across every
// user-role journey, including the user-side half of every admin-gate check).
// web/src/auth/AuthProvider.tsx already treats this POST as best-effort and
// fire-and-forget ("Intentionally silent — capability data is best-effort and
// must never..."), so a 429 here is correct server behavior meeting expected
// client behavior, not a defect to fail the run over.
// Session-launch (LEVEL=session only) admission codes that are retryable-by-
// design (control-api.md POST /v1/sessions: 409 session_quota_exceeded /
// profile conflicts, 503 no_host_available / capacity_exhausted) — the
// journey itself (journeys/session-launch.mjs) already turns a non-201 into
// its own descriptive thrown error; letting the generic network-failure
// listener ALSO flag the same response would just repeat the same fact in a
// less useful form (MINOR 11c, adversarial review).
const SESSION_LAUNCH_RETRYABLE_CODES = new Set([409, 503]);

export function isAllowlistedFailure(resp) {
  const url = new URL(resp.url());
  const method = resp.request().method();
  if (resp.status() === 429 && method === "POST" && url.pathname === "/v1/me/devices") return true;
  if (method === "POST" && url.pathname === "/v1/sessions" && SESSION_LAUNCH_RETRYABLE_CODES.has(resp.status())) {
    return true;
  }
  return false;
}

// Attaches console/response/requestfailed listeners to `page`, pushing
// evidence into `sink.consoleErrors` / `sink.networkFailures` (arrays owned
// by the caller — shared across a journey's primary page AND any secondary
// context it opens, so a bug surfacing only in the secondary context still
// fails the journey).
export function attachEvidenceListeners(page, baseUrl, sink) {
  page.on("console", (msg) => {
    if (msg.type() !== "error") return;
    if (!isSameOrigin(page.url(), baseUrl)) return;
    // Chrome auto-logs a "Failed to load resource: ..." console entry for
    // EVERY non-2xx response on this origin, independent of whether app code
    // handled it (it is the devtools network panel's own echo, not an
    // app-level console.error). It is pure noise on top of the "response"
    // listener below, which captures the same failure with the actual URL
    // and status — keep console-error detection for real app-level errors
    // (uncaught exceptions, React error boundaries, CSP violations, ...).
    if (/^Failed to load resource:/.test(msg.text())) return;
    sink.consoleErrors.push(msg.text());
  });
  page.on("response", (resp) => {
    if (!isSameOrigin(resp.url(), baseUrl)) return;
    if (resp.status() < 400) return;
    if (isAllowlistedFailure(resp)) return;
    sink.networkFailures.push(`${resp.status()} ${resp.request().method()} ${resp.url()}`);
  });
  page.on("requestfailed", (req) => {
    if (!isSameOrigin(req.url(), baseUrl)) return;
    const failure = req.failure();
    const errorText = failure ? failure.errorText : "unknown";
    // net::ERR_ABORTED is Chromium's generic cancellation code — it fires for
    // every in-flight fetch when we close the browser context at the end of
    // a journey (e.g. the RTT/capability probes AuthProvider kicks off on
    // every authenticated page, webrtc/capability.ts probeRttMs). That is
    // harness teardown, not a server or network fault; a real failed
    // response is already caught by the "response" listener above via its
    // status code. Only a non-abort failure (DNS, connection refused, etc.)
    // indicates a genuine reachability problem worth failing the journey.
    if (errorText === "net::ERR_ABORTED") return;
    sink.networkFailures.push(`FAILED ${req.method()} ${req.url()} (${errorText})`);
  });
}
