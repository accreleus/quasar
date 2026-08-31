import { assertUserDenied } from "../lib/adminGate.mjs";

// /admin/sessions (AdminSessions.tsx): h1 "Sessions". This harness's local
// ephemeral stack seeds an app (see admin-apps.mjs) but never launches a
// session against it (no GPU host — see journeys/session-launch.mjs), so
// this list is legitimately empty there. Assert the DOCUMENTED empty-state
// message the Table's `empty` prop renders ("No sessions found." —
// AdminSessions.tsx) rather than the bare table shell, which an
// all-empty-data regression would satisfy just as well as a real fetch. A
// remote stack with live sessions still passes: the assertion accepts either
// the empty message or at least one row.
//
// Also proves the user-role identity is denied both the admin API
// (GET /v1/admin/sessions -> 403) and the admin route (adminGate.mjs).
export default {
  name: "admin-sessions",
  role: "admin",
  level: "ui",
  async run(page, ctx) {
    await page.goto(ctx.baseUrl + "/admin/sessions", { waitUntil: "networkidle" });

    await page.locator("h1", { hasText: "Sessions" }).waitFor({ state: "visible", timeout: 15000 });
    await page
      .locator('table.qtable tbody tr, table.qtable :text("No sessions found")')
      .first()
      .waitFor({ state: "visible", timeout: 15000 });

    await ctx.screenshot(page, "admin-sessions");

    await assertUserDenied(ctx, "/admin/sessions", "/v1/admin/sessions");
  },
};
