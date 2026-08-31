import { assertUserDenied } from "../lib/adminGate.mjs";

// /admin/hosts (AdminHosts.tsx): h2 "Host capacity" (this page has no <h1>).
// This harness's local ephemeral stack has no node-agent registered, so a
// row-based assertion would be asserting a false "empty because it's broken"
// vs "empty because there's genuinely nothing" — instead assert the
// DOCUMENTED empty-state message the Table's `empty` prop renders
// ("No hosts registered. Start a node-agent to register one." —
// AdminHosts.tsx), which only appears once the fetch has actually resolved
// (a stuck loading state or an error would not produce this exact text).
// Real remote stacks generally DO have registered hosts; this still passes
// there too since the assertion only requires the table shell + either the
// empty message or at least one row.
//
// Also proves the user-role identity is denied both the admin API
// (GET /v1/hosts -> 403) and the admin route (adminGate.mjs).
export default {
  name: "admin-hosts",
  role: "admin",
  level: "ui",
  async run(page, ctx) {
    await page.goto(ctx.baseUrl + "/admin/hosts", { waitUntil: "networkidle" });

    await page.locator("h2", { hasText: "Host capacity" }).waitFor({ state: "visible", timeout: 15000 });
    await page
      .locator('table.qtable tbody tr, table.qtable :text("No hosts registered")')
      .first()
      .waitFor({ state: "visible", timeout: 15000 });

    await ctx.screenshot(page, "admin-hosts");

    await assertUserDenied(ctx, "/admin/hosts", "/v1/hosts");
  },
};
