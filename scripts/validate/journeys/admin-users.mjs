import { assertUserDenied } from "../lib/adminGate.mjs";

// /admin/users (AdminUsers/index.tsx): h1 "Users" + a REAL data row — not
// just the qtable shell (Table.tsx renders <thead> and, on zero rows, an
// `empty` message — both are unconditional page chrome an all-empty-data
// regression would still satisfy).
//
// Against the LOCAL ephemeral stack, scripts/dx/validate.sh boots the
// control-plane with BOOTSTRAP_ADMIN_USERNAME=admin, so that exact username
// is asserted verbatim in the `.u-name` cell (see the `columns` array in
// AdminUsers/index.tsx). Against a remote TARGET the bootstrap username is
// operator-configured and unknown to this harness, so the assertion relaxes
// to "at least one row" — GET /v1/users always includes at least the admin
// identity currently viewing the page, so an empty table there is still a
// real failure.
//
// Also proves the user-role identity is denied both the admin API
// (GET /v1/users -> 403) and the admin route (adminGate.mjs).
export default {
  name: "admin-users",
  role: "admin",
  level: "ui",
  async run(page, ctx) {
    await page.goto(ctx.baseUrl + "/admin/users", { waitUntil: "networkidle" });

    await page.locator("h1", { hasText: "Users" }).waitFor({ state: "visible", timeout: 15000 });

    if (ctx.target === "local") {
      await page
        .locator(".u-name")
        .filter({ hasText: /^admin$/ })
        .first()
        .waitFor({ state: "visible", timeout: 15000 });
    } else {
      await page.locator("table.qtable tbody tr").first().waitFor({ state: "visible", timeout: 15000 });
    }

    await ctx.screenshot(page, "admin-users");

    await assertUserDenied(ctx, "/admin/users", "/v1/users");
  },
};
