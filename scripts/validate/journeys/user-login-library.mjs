import { BENCH_APP_NAME } from "../lib/seed.mjs";

// #399 acceptance box: a minted user storage-state lands on /app with no
// login redirect, and the library/hero surface renders (AppHomeNext.tsx —
// the default landing page; App.tsx wires it as AppIndexPage). Structural
// anchors: the topbar (AppShell.tsx `<header class="topbar">`).
//
// Against the LOCAL ephemeral stack, scripts/dx/validate.sh seeds one known
// app (BENCH_APP_NAME) via the admin API before journeys run and grants it
// by default (POST /v1/apps entitle default is "all"), so this asserts the
// seeded app's own tile/card by NAME — an all-empty-data regression (e.g. the
// library fetch silently failing and falling back to the empty-state) would
// NOT satisfy this. Against a remote TARGET the seed step is skipped (this
// harness doesn't own that stack's catalogue), so the assertion relaxes to
// "the hero heading renders OR the documented empty state renders" — still
// real (fails on a stuck spinner, blank page, or error state).
export default {
  name: "user-login-library",
  role: "user",
  level: "ui",
  async run(page, ctx) {
    await page.goto(ctx.baseUrl + "/app", { waitUntil: "networkidle" });

    const landedPath = new URL(page.url()).pathname;
    if (landedPath.startsWith("/login")) {
      throw new Error(
        `landed on ${landedPath} — the user storage-state did not authenticate ` +
          `(#399 acceptance box: a valid storage-state must never bounce to /login)`,
      );
    }

    const topbar = page.locator("header.topbar");
    await topbar.waitFor({ state: "visible", timeout: 15000 });

    if (ctx.target === "local") {
      // Scope the match to the library GRID (`.lib-grid`, AppHomeNext.tsx:878),
      // not the whole document — with a single app the hero band also carries
      // the name, so a document-wide getByText would pass even if the tile grid
      // were broken. The tile must render inside the grid for this to pass.
      await page
        .locator(".lib-grid")
        .getByText(BENCH_APP_NAME, { exact: false })
        .first()
        .waitFor({ state: "visible", timeout: 15000 });
    } else {
      // AppHomeNext renders one of two real states: the hero <h1> when the
      // caller's library has apps (showHero = apps.length > 0), or the
      // `.home-state[role=status]` empty card ("Your library is empty") when
      // it doesn't. Either is real evidence the library surface fetched and
      // rendered; a bare spinner or blank page satisfies neither.
      const heading = page.locator('main h1, h1, .home-state[role="status"] h3').first();
      await heading.waitFor({ state: "visible", timeout: 15000 });
      const headingText = (await heading.textContent())?.trim() ?? "";
      if (!headingText) {
        throw new Error("library/hero heading on /app is present but empty — the surface did not render content");
      }
    }

    await ctx.screenshot(page, "app-home");
  },
};
