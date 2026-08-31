// scripts/qa/report.mjs — renders report.json (scripts/qa/schema.md) into a
// single self-contained report.html: every screenshot is inlined as a base64
// PNG data URI, no external CSS/JS/fonts/images. Zero deps, Node 20+.
//
// House style matches scripts/validate/report.mjs: esc() helper, template-
// literal HTML, `:root { color-scheme: light dark; }`, badge/section class
// names carried over 1:1 (pass/fail/skip/evidence) so the two reports read
// as one family.

const B64_RE = /^[A-Za-z0-9+/=]+$/;

function esc(s) {
  return String(s ?? "").replace(/[&<>"']/g, (c) => ({
    "&": "&amp;",
    "<": "&lt;",
    ">": "&gt;",
    '"': "&quot;",
    "'": "&#39;",
  })[c]);
}

function verdictClass(v) {
  switch (v) {
    case "PASS":
      return "pass";
    case "FAIL":
      return "fail";
    case "SKIPPED":
      return "skip";
    case "EVIDENCE":
      return "evidence";
    default:
      return "unknown";
  }
}

function badge(v) {
  return `<span class="badge ${verdictClass(v)}">${esc(v)}</span>`;
}

function fmtDuration(ms) {
  if (typeof ms !== "number" || !Number.isFinite(ms) || ms < 0) return "";
  const totalSec = Math.round(ms / 1000);
  const m = Math.floor(totalSec / 60);
  const s = totalSec % 60;
  return `${m}m ${String(s).padStart(2, "0")}s`;
}

// Screenshots are the only field allowed raw base64 bytes; everything else
// interpolated into HTML goes through esc(). A payload that fails the
// base64-alphabet check is dropped (not emitted at all) and replaced with a
// visible placeholder caption so a bad upstream capture is never silently lost.
function renderScreenshots(shots) {
  const list = Array.isArray(shots) ? shots : [];
  if (list.length === 0) return "";
  const figures = list
    .map((s) => {
      const label = s && s.label ? String(s.label) : "";
      const raw = s && typeof s.png_b64 === "string" ? s.png_b64 : "";
      const valid = raw.length > 0 && B64_RE.test(raw);
      if (!valid) {
        return `<figure><figcaption>screenshot dropped: invalid payload${label ? ` (${esc(label)})` : ""}</figcaption></figure>`;
      }
      return `<figure><img alt="${esc(label)}" src="data:image/png;base64,${raw}" /><figcaption>${esc(label)}</figcaption></figure>`;
    })
    .join("\n");
  return `<div class="shots">${figures}</div>`;
}

function renderWarnings(warnings) {
  const list = Array.isArray(warnings) ? warnings : [];
  if (list.length === 0) return "";
  const items = list.map((w) => `<li>${esc(w)}</li>`).join("");
  return `<ul class="warn">${items}</ul>`;
}

function renderMetrics(metrics) {
  if (!metrics || typeof metrics !== "object" || Object.keys(metrics).length === 0) return "";
  return `<pre class="metrics">${esc(JSON.stringify(metrics, null, 2))}</pre>`;
}

function gateLabel(gate) {
  const n = gate.n !== undefined && gate.n !== null ? String(gate.n) : "";
  const title = gate.title ? String(gate.title) : String(gate.id ?? "");
  return n ? `${n} · ${title}` : title;
}

function renderGateSummaryRow(gate) {
  const verdict = gate.verdict || "";
  return `
    <tr>
      <td>${esc(gateLabel(gate))}</td>
      <td>${esc(gate.proves ?? "")}</td>
      <td>${esc(gate.threshold ?? "")}</td>
      <td class="num">${esc(gate.measured ?? "")}</td>
      <td>${badge(verdict)}</td>
    </tr>`;
}

function renderGateSection(gate) {
  const verdict = gate.verdict || "";
  const cls = verdictClass(verdict);
  const n = gate.n !== undefined && gate.n !== null ? String(gate.n) : "";
  const title = gate.title ? String(gate.title) : String(gate.id ?? "");
  const heading = n ? `Gate ${n} — ${title}` : title;

  const evidenceNote =
    verdict === "EVIDENCE"
      ? `<p class="proves">No assertion was made for this gate — a green report does not mean anyone looked at this evidence. You do.</p>`
      : "";

  return `
  <section class="gate ${cls}">
    <h2>${badge(verdict)} ${esc(heading)}</h2>
    ${gate.proves ? `<p class="proves">${esc(gate.proves)}</p>` : ""}
    ${evidenceNote}
    ${gate.reason ? `<p class="reason">${esc(gate.reason)}</p>` : ""}
    ${gate.detail ? `<pre>${esc(gate.detail)}</pre>` : ""}
    ${renderMetrics(gate.metrics)}
    ${renderWarnings(gate.warnings)}
    ${renderScreenshots(gate.screenshots)}
  </section>`;
}

function renderNotes(notes) {
  const list = Array.isArray(notes) ? notes : [];
  if (list.length === 0) return "";
  const items = list
    .map((note) => {
      const title = note && note.title ? esc(note.title) : "";
      const body = note && note.body ? esc(note.body) : "";
      return `<li>${title ? `<strong>${title}.</strong> ` : ""}${body}</li>`;
    })
    .join("");
  return `
<section class="notes">
  <h2 style="margin-top:0;font-size:1.05rem">Notes you need to know</h2>
  <ol>${items}</ol>
</section>`;
}

function renderEnvFooter(env) {
  if (!env || typeof env !== "object") return "";
  const parts = [];
  if ("pin" in env) parts.push(`runtime pin restored ${env.pin ? "✔" : "✖"}`);
  if ("stray_containers" in env) parts.push(`${esc(String(env.stray_containers))} stray session containers`);
  if ("sessions_deleted" in env) parts.push(`${esc(String(env.sessions_deleted))} sessions deleted`);
  if (parts.length === 0) return "";
  return `<p class="env">Environment restored: ${parts.join(" &middot; ")}.</p>`;
}

export function renderReport(report) {
  const r = report && typeof report === "object" ? report : {};
  const image = r.image || {};
  const replacedPin = r.replaced_pin || {};
  const summary = r.summary || {};
  const gates = Array.isArray(r.gates) ? r.gates : [];

  const overall = gates.some((g) => g && g.verdict === "FAIL") ? "FAIL" : "PASS";
  const overallCls = overall.toLowerCase();

  const summaryLine = [
    `${summary.pass ?? 0} pass`,
    `${summary.fail ?? 0} fail`,
    `${summary.skipped ?? 0} skipped`,
    `${summary.evidence ?? 0} evidence-only`,
  ].join(" / ");

  const gateRows = gates.map(renderGateSummaryRow).join("\n");
  const gateSections = gates.map(renderGateSection).join("\n");
  const notes = renderNotes(r.notes);
  const envFooter = renderEnvFooter(r.environment_restored);
  const wall = fmtDuration(r.duration_ms);

  return `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8" />
<title>Quasar QA report — ${esc(image.tag ?? "")} (${esc(overall)})</title>
<style>
  :root { color-scheme: light dark; }
  body { font-family: system-ui, sans-serif; max-width: 1040px; margin: 2rem auto; padding: 0 1rem; line-height: 1.5; }
  header { margin-bottom: 1.5rem; }
  h1 { font-size: 1.5rem; margin-bottom: .4rem; }
  .badge { display: inline-block; padding: 0.1em 0.6em; border-radius: 0.3em; font-weight: 700; font-size: 0.85em; white-space: nowrap; }
  .badge.pass, .overall.pass { background: #1a7f37; color: #fff; }
  .badge.fail, .overall.fail { background: #cf222e; color: #fff; }
  .badge.skip { background: #9a6700; color: #fff; }
  .badge.evidence { background: #0969da; color: #fff; }
  .overall { display: inline-block; padding: 0.2em 0.8em; border-radius: 0.4em; font-weight: 700; }
  table.meta-table { border-collapse: collapse; font-size: .92rem; }
  table.meta-table td { padding: 0.15em 0.9em 0.15em 0; vertical-align: top; }
  table.meta-table td:first-child { opacity: .65; white-space: nowrap; }
  table.gates { border-collapse: collapse; width: 100%; margin: 1rem 0 2rem; font-size: .92rem; }
  table.gates th, table.gates td { border-bottom: 1px solid #8883; padding: .5em .6em; text-align: left; vertical-align: top; }
  table.gates th { font-size: .8rem; text-transform: uppercase; letter-spacing: .04em; opacity: .7; }
  table.gates td.num { font-variant-numeric: tabular-nums; white-space: nowrap; }
  .gate { border: 1px solid #8883; border-radius: 0.5em; padding: 1rem; margin-bottom: 1rem; }
  .gate.fail { border-color: #cf222e; }
  .gate.skip { border-color: #9a6700; }
  .gate.evidence { border-color: #0969da; }
  .gate h2 { margin: 0 0 0.5rem 0; font-size: 1.05rem; }
  .meta { font-weight: 400; font-size: 0.85em; opacity: 0.7; }
  .proves { margin: .1rem 0 .7rem; opacity: .8; font-size: .92rem; }
  .reason { color: #cf222e; font-weight: 600; }
  .shots { display: flex; flex-wrap: wrap; gap: 0.6rem; }
  .shots figure { margin: 0; }
  .shots img { max-width: 240px; border: 1px solid #8883; border-radius: 0.3em; display: block; }
  .shots figcaption { font-size: .78rem; opacity: .7; margin-top: .2rem; }
  pre { background: #8881; padding: 0.6em; border-radius: 0.3em; overflow-x: auto; font-size: .82rem; }
  ul.warn li { color: #9a6700; }
  section.notes { border: 1px solid #9a670055; border-radius: .5em; padding: 1rem; background: #9a670010; }
  .env { font-size: .85rem; opacity: .75; margin-top: 1.5rem; border-top: 1px solid #8883; padding-top: .8rem; }
  code { font-size: .88em; }
</style>
</head>
<body>
<header>
  <h1>Quasar QA report <span class="overall ${overallCls}">${overall}</span></h1>
  <table class="meta-table">
    <tr><td>Image under test</td><td><code>${esc(image.tag ?? "")}</code>${image.digest ? ` &nbsp;<span class="meta">${esc(image.digest)}</span>` : ""}</td></tr>
    ${replacedPin.image_ref ? `<tr><td>Replaced pin</td><td><code>${esc(replacedPin.image_ref)}</code> <span class="meta">restored ${replacedPin.restored ? "✔" : "✖"}</span></td></tr>` : ""}
    <tr><td>Host / stack</td><td>${esc(r.host ?? "")}${r.stack_api ? ` — <code>${esc(r.stack_api)}</code>` : ""}</td></tr>
    <tr><td>Profile</td><td><code>${esc(r.profile ?? "")}</code></td></tr>
    <tr><td>Runs</td><td>${esc(r.runs ?? "")}</td></tr>
    <tr><td>Quasar commit</td><td><code>${esc(r.commit ?? "")}</code></td></tr>
    <tr><td>Generated</td><td>${esc(r.generated_at ?? "")}${wall ? ` &nbsp;<span class="meta">wall ${wall}</span>` : ""}</td></tr>
    <tr><td>Result</td><td><strong>${esc(summaryLine)}</strong></td></tr>
  </table>
</header>

<h2 style="font-size:1.1rem">Gate summary</h2>
<table class="gates">
  <thead><tr><th>Gate</th><th>What it proves</th><th>Threshold</th><th>Measured</th><th>Verdict</th></tr></thead>
  <tbody>${gateRows}
  </tbody>
</table>

${gateSections}
${notes}
${envFooter}
</body>
</html>`;
}
