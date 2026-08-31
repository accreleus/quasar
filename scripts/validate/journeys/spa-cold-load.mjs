// Uncached load of `/` (anon — no storage-state, so this exercises the real
// cold path: `/` -> redirect -> /app -> RequireAuth -> /login). Not a gate on
// bundle size yet (#386 is the tracking issue) — this journey records
// transfer bytes + request count as report evidence per the spec, and keeps
// a real, failable assertion: the SPA shell must actually mount into #root
// (the #131 stale-bundle trap: a missing/mismatched asset falls back to the
// HTML shell with 200, gets MIME-blocked, and React never mounts — #root
// stays empty with no console error).
export default {
  name: "spa-cold-load",
  role: "anon",
  level: "ui",
  async run(page, ctx) {
    const resp = await page.goto(ctx.baseUrl + "/", { waitUntil: "networkidle" });
    if (!resp || !resp.ok()) {
      throw new Error(`cold load of / failed: HTTP ${resp ? resp.status() : "no response"}`);
    }

    const rootChildren = await page.evaluate(() => document.getElementById("root")?.childElementCount ?? -1);
    if (rootChildren <= 0) {
      throw new Error(
        `#root has ${rootChildren} children after the cold load settled — the SPA shell did not ` +
          `mount (see .claude/rules/webrtc-testing.md #131: a missing hashed asset 200s back to ` +
          `index.html, MIME-blocks silently, and React never mounts)`,
      );
    }

    const metrics = await page.evaluate(() => {
      const entries = performance.getEntriesByType("resource");
      const nav = performance.getEntriesByType("navigation")[0];
      const transferBytes =
        entries.reduce((sum, e) => sum + (e.transferSize || 0), 0) + (nav ? nav.transferSize || 0 : 0);
      return { request_count: entries.length + 1, transfer_bytes: transferBytes };
    });
    ctx.metrics.request_count = metrics.request_count;
    ctx.metrics.transfer_bytes = metrics.transfer_bytes;

    await ctx.screenshot(page, "cold-load");
  },
};
