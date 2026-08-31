// /app/account renders the profile summary (AccountProfile.tsx): a
// `.sec-card` with an <h3> holding the authenticated user's own username —
// pulled from GET /v1/me via AuthProvider, so an empty/missing value means
// the account surface (or auth rehydrate) is broken, not just "page loaded".
export default {
  name: "user-account",
  role: "user",
  level: "ui",
  async run(page, ctx) {
    await page.goto(ctx.baseUrl + "/app/account", { waitUntil: "networkidle" });

    const landedPath = new URL(page.url()).pathname;
    if (landedPath.startsWith("/login")) {
      throw new Error(`landed on ${landedPath} instead of /app/account — auth did not stick`);
    }

    const card = page.locator(".sec-card").first();
    await card.waitFor({ state: "visible", timeout: 15000 });

    const heading = card.locator("h3").first();
    await heading.waitFor({ state: "visible", timeout: 15000 });
    const username = (await heading.textContent())?.trim() ?? "";
    if (!username) {
      throw new Error("account profile card rendered but the username <h3> is empty — GET /v1/me rehydrate likely failed");
    }

    await ctx.screenshot(page, "account-profile");
  },
};
