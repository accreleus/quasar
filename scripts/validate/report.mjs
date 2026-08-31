// scripts/validate/report.mjs — renders report.json into a single self-
// contained report.html (screenshots referenced by relative path under the
// same run dir, not embedded — keeps this a tiny string template, no deps).
function esc(s) {
  return String(s ?? "").replace(/[&<>"']/g, (c) => ({
    "&": "&amp;",
    "<": "&lt;",
    ">": "&gt;",
    '"': "&quot;",
    "'": "&#39;",
  })[c]);
}

function renderJourney(j) {
  const cls = j.verdict === "PASS" ? "pass" : "fail";
  const shots = (j.screenshots || [])
    .map((s) => `<a href="${esc(s)}" target="_blank"><img src="${esc(s)}" loading="lazy" /></a>`)
    .join("\n");
  const consoleErrors = (j.console_errors || []).map((e) => `<li>${esc(e)}</li>`).join("");
  const networkFailures = (j.network_failures || []).map((e) => `<li>${esc(e)}</li>`).join("");
  const warnings = (j.warnings || []).map((e) => `<li>${esc(e)}</li>`).join("");
  const metrics = j.metrics && Object.keys(j.metrics).length > 0
    ? `<pre class="metrics">${esc(JSON.stringify(j.metrics, null, 2))}</pre>`
    : "";
  return `
  <section class="journey ${cls}">
    <h2><span class="badge ${cls}">${j.verdict}</span> ${esc(j.name)} <span class="meta">role=${esc(j.role)} level=${esc(j.level)} ${j.duration_ms}ms</span></h2>
    ${j.reason ? `<p class="reason">${esc(j.reason)}</p>` : ""}
    ${metrics}
    ${warnings ? `<h3>Warnings</h3><ul class="warn">${warnings}</ul>` : ""}
    ${consoleErrors ? `<h3>Console errors</h3><ul>${consoleErrors}</ul>` : ""}
    ${networkFailures ? `<h3>Network failures</h3><ul>${networkFailures}</ul>` : ""}
    ${shots ? `<h3>Screenshots</h3><div class="shots">${shots}</div>` : ""}
  </section>`;
}

function renderSkipped(skipped) {
  if (!skipped || skipped.length === 0) return "";
  const items = skipped
    .map((s) => `<li><strong>${esc(s.name)}</strong>${s.reason ? ` — ${esc(s.reason)}` : ""}</li>`)
    .join("");
  return `
  <section class="journey skip">
    <h2><span class="badge skip">SKIPPED</span> ${skipped.length} journey(s) not run</h2>
    <ul>${items}</ul>
  </section>`;
}

export function renderReport(report) {
  const journeys = (report.journeys || []).map(renderJourney).join("\n");
  const skippedSection = renderSkipped(report.skipped);
  const overall = report.summary.fail > 0 ? "FAIL" : "PASS";
  return `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8" />
<title>Quasar validate report — ${esc(report.target)} (${esc(overall)})</title>
<style>
  :root { color-scheme: light dark; }
  body { font-family: system-ui, sans-serif; max-width: 1000px; margin: 2rem auto; padding: 0 1rem; line-height: 1.5; }
  header { margin-bottom: 2rem; }
  .badge { display: inline-block; padding: 0.1em 0.6em; border-radius: 0.3em; font-weight: 700; font-size: 0.85em; }
  .badge.pass, .overall.pass { background: #1a7f37; color: #fff; }
  .badge.fail, .overall.fail { background: #cf222e; color: #fff; }
  .badge.skip { background: #9a6700; color: #fff; }
  .journey.skip { border-color: #9a6700; }
  ul.warn li { color: #9a6700; }
  .overall { display: inline-block; padding: 0.2em 0.8em; border-radius: 0.4em; font-weight: 700; }
  .journey { border: 1px solid #8883; border-radius: 0.5em; padding: 1rem; margin-bottom: 1rem; }
  .journey.fail { border-color: #cf222e; }
  .journey h2 { margin: 0 0 0.5rem 0; font-size: 1.1rem; }
  .meta { font-weight: 400; font-size: 0.85em; opacity: 0.7; }
  .reason { color: #cf222e; font-weight: 600; }
  .shots { display: flex; flex-wrap: wrap; gap: 0.5rem; }
  .shots img { max-width: 220px; border: 1px solid #8883; border-radius: 0.3em; }
  .metrics { background: #8881; padding: 0.5em; border-radius: 0.3em; overflow-x: auto; }
  table.meta-table { border-collapse: collapse; }
  table.meta-table td { padding: 0.15em 0.8em 0.15em 0; }
</style>
</head>
<body>
<header>
  <h1>Quasar validate report <span class="overall ${overall.toLowerCase()}">${overall}</span></h1>
  <table class="meta-table">
    <tr><td>Target</td><td>${esc(report.target)} (${esc(report.target_kind)})</td></tr>
    <tr><td>Level</td><td>${esc(report.level)}</td></tr>
    <tr><td>Commit</td><td>${esc(report.commit)}</td></tr>
    <tr><td>Generated</td><td>${esc(report.generated_at)}</td></tr>
    <tr><td>Journeys</td><td>${report.summary.pass} pass / ${report.summary.fail} fail / ${report.summary.total} total${report.summary.skipped ? ` / ${report.summary.skipped} skipped` : ""}</td></tr>
  </table>
</header>
${journeys}
${skippedSection}
</body>
</html>`;
}
