import { assertUserDenied } from "../lib/adminGate.mjs";

// /admin/settings (AdminSettings.tsx): h1 "Settings" + the instance-config
// panel finishing its load — either the "no master key configured" warning
// banner (role=status, the default on a fresh ephemeral stack with no
// QUASAR_SECRET_KEY) or the secrets/libraries panel once one is set. Either
// is real evidence the settings surface fetched and rendered; a bare
// "Loading…" would not satisfy this locator and the journey would fail.
export default {
  name: "admin-settings",
  role: "admin",
  level: "ui",
  async run(page, ctx) {
    await page.goto(ctx.baseUrl + "/admin/settings", { waitUntil: "networkidle" });

    await page.locator("h1", { hasText: "Settings" }).waitFor({ state: "visible", timeout: 15000 });
    await page
      .locator('.note.warn[role="status"], .panel')
      .first()
      .waitFor({ state: "visible", timeout: 15000 });

    await ctx.screenshot(page, "admin-settings");

    await assertUserDenied(ctx, "/admin/settings", "/v1/admin/settings");
  },
};
