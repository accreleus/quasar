import { assertUserDenied } from "../lib/adminGate.mjs";
import { BENCH_APP_NAME } from "../lib/seed.mjs";

// /admin/apps (AdminApps/index.tsx): h1 "App catalog" + a REAL data row, not
// just the qtable shell (Table.tsx renders <thead> and, on zero rows, an
// `empty` message — both are unconditional page chrome that an all-empty-data
// regression would still satisfy). Against the local ephemeral stack,
// scripts/dx/validate.sh seeds one known app (BENCH_APP_NAME) via the admin
// API before journeys run, so this asserts that exact row by name. Against a
// remote TARGET the seed step is skipped (no admin write against a stack we
// don't own the data lifecycle of) — the assertion relaxes to "at least one
// row OR the documented empty state", still real (fails on an error state or
// a stuck loading spinner).
//
// Also proves the user-role identity is denied both the admin API
// (GET /v1/admin/apps -> 403) and the admin route (adminGate.mjs).
export default {
  name: "admin-apps",
  role: "admin",
  level: "ui",
  async run(page, ctx) {
    await page.goto(ctx.baseUrl + "/admin/apps", { waitUntil: "networkidle" });

    await page.locator("h1", { hasText: "App catalog" }).waitFor({ state: "visible", timeout: 15000 });

    if (ctx.target === "local") {
      await page
        .locator("table.qtable")
        .getByText(BENCH_APP_NAME, { exact: false })
        .first()
        .waitFor({ state: "visible", timeout: 15000 });
    } else {
      await page
        .locator('table.qtable tbody tr, table.qtable :text("No apps yet")')
        .first()
        .waitFor({ state: "visible", timeout: 15000 });
    }

    await ctx.screenshot(page, "admin-apps");

    await assertUserDenied(ctx, "/admin/apps", "/v1/admin/apps");
  },
};
