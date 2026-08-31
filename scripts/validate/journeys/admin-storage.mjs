import { assertUserDenied } from "../lib/adminGate.mjs";

// /admin/storage (AdminStorage.tsx): h1 "Storage" + either the qtable
// primary table (managed homes exist) or the dedicated empty-state panel
// AdminStorage renders INSTEAD of the table when there are none — a fresh
// ephemeral stack has zero managed homes, so the empty panel is the real
// state to expect there. Also proves the user-role identity is denied.
export default {
  name: "admin-storage",
  role: "admin",
  level: "ui",
  async run(page, ctx) {
    await page.goto(ctx.baseUrl + "/admin/storage", { waitUntil: "networkidle" });

    await page.locator("h1", { hasText: "Storage" }).waitFor({ state: "visible", timeout: 15000 });
    await page
      .locator('table.qtable, .panel:has-text("No managed homes")')
      .first()
      .waitFor({ state: "visible", timeout: 15000 });

    await ctx.screenshot(page, "admin-storage");

    await assertUserDenied(ctx, "/admin/storage", "/v1/admin/storage/homes");
  },
};
