// Shared helper for every admin-* journey. Proves the server-enforced role
// gate from the OTHER side (CLAUDE.md invariant #6: "Authorization is
// server-enforced, never UI-gated") — a user-role identity must be denied
// BOTH the admin API endpoint AND the admin UI route.
//
// The PRIMARY assertion is a direct fetch of the journey's admin API
// endpoint with the user-role bearer token, asserting HTTP 403. This is the
// actual enforcement boundary (control-api.md §Authorization /
// auth.RequireAuth -> RequireAdmin middleware) — deleting that middleware
// server-side must fail this check.
//
// The SECONDARY assertion is the pre-existing UI-navigation check:
// RequireAdmin (web/src/auth/RequireAdmin.tsx) redirects a non-admin away
// from /admin/*. This is UX only (the component's own header says so) — kept
// here as evidence the client behaves correctly ON TOP OF the server denial,
// never as a substitute for it. A prior version of this file asserted ONLY
// the UI-navigation outcome, which meant deleting the server's RequireAdmin
// middleware entirely still passed every admin journey (the client-side
// Navigate still fires from cached role state) — that gap is why the API
// check above exists now (adversarial review, BLOCKER 1).
export async function assertUserDenied(ctx, path, apiPath) {
  const context = await ctx.browser.newContext({
    storageState: ctx.userState,
    ignoreHTTPSErrors: true,
  });
  try {
    const page = await context.newPage();

    // Same-origin navigation FIRST (fast — domcontentloaded, not
    // networkidle) so the probe fetch below is a normal same-origin request
    // (no CORS involved) and can read the user's token from localStorage,
    // which Playwright's storageState only populates once a page has loaded
    // that origin. Listeners are attached AFTER this probe deliberately: the
    // probe's own expected 403 must not be double-reported by the shared
    // network-failure listener (which has no way to know a 403 here is the
    // assertion under test, not a defect).
    await page.goto(ctx.baseUrl + path, { waitUntil: "domcontentloaded" });

    const probe = await page.evaluate(async (endpoint) => {
      const token = localStorage.getItem("quasar.auth.token");
      const resp = await fetch(endpoint, { headers: { Authorization: `Bearer ${token}` } });
      return { status: resp.status };
    }, apiPath);

    if (probe.status !== 403) {
      throw new Error(
        `SERVER-SIDE admin gate did NOT deny the user-role identity: ${apiPath} returned ` +
          `HTTP ${probe.status} (want 403) for a user bearer token. This is the actual ` +
          `enforcement boundary (control-api.md §Authorization) — a passing UI redirect does ` +
          `not compensate for this.`,
      );
    }

    // Now instrument the rest of this secondary context/page the same way
    // the journey's primary page is instrumented (MINOR 8) — any console
    // error or unexpected network failure from here on counts toward the
    // journey's evidence too.
    ctx.attachListeners(page);

    // RequireAdmin renders a <Navigate replace>; give the SPA a moment to
    // settle on wherever it lands (secondary, UX-only check — see header).
    await page
      .waitForFunction(() => !window.location.pathname.startsWith("/admin"), {
        timeout: 5000,
      })
      .catch(() => {});
    const landedPath = new URL(page.url()).pathname;
    if (landedPath.startsWith("/admin")) {
      throw new Error(
        `user-role identity was denied server-side (${apiPath} -> 403) but the UI did NOT ` +
          `navigate away from ${path} — still on ${landedPath}. The server enforcement is ` +
          `intact; the client-side RequireAdmin redirect regressed (UX-only, but worth fixing).`,
      );
    }
    await ctx.screenshot(page, `user-denied-${path.replace(/\//g, "_")}`);
  } finally {
    await context.close();
  }
}
