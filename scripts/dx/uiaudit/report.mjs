#!/usr/bin/env node
// scripts/dx/uiaudit/report.mjs — evidence dir (+ optional findings.json) ->
// a single self-contained HTML report (all screenshots base64-embedded).
//
// Usage:
//   node report.mjs --evidence <dir> --out <report.html> [--findings <findings.json>]
//
// With no --findings (or a missing file), produces a COVERAGE-ONLY report:
// every captured surface is listed CLEAN unless its metrics.json flags an
// overflow / unstyled control / console error, in which case it's called out
// as "N metric flag(s)" without editorializing (that judgment is the agent's
// job — see .claude/skills/quasar-ui-audit/SKILL.md).

import fs from 'node:fs';
import path from 'node:path';
import url from 'node:url';

const __dirname = path.dirname(url.fileURLToPath(import.meta.url));

function parseArgs(argv) {
  const args = { evidence: '', out: '', findings: '' };
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    const need = () => {
      if (i + 1 >= argv.length) throw new Error(`${a} requires a value`);
      return argv[++i];
    };
    switch (a) {
      case '--evidence': args.evidence = need(); break;
      case '--out': args.out = need(); break;
      case '--findings': args.findings = need(); break;
      default: throw new Error(`unknown arg '${a}'`);
    }
  }
  if (!args.evidence) throw new Error('--evidence is required');
  if (!args.out) throw new Error('--out is required');
  return args;
}

function esc(s) {
  return String(s == null ? '' : s).replace(/[&<>"']/g, (c) => ({
    '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#x27;',
  }[c]));
}

function embedImage(evidenceDir, relPath) {
  const p = path.join(evidenceDir, relPath);
  if (!fs.existsSync(p)) return null;
  const b64 = fs.readFileSync(p).toString('base64');
  return `data:image/png;base64,${b64}`;
}

function loadManifest(evidenceDir) {
  const p = path.join(evidenceDir, 'manifest.json');
  if (!fs.existsSync(p)) throw new Error(`no manifest.json in ${evidenceDir} — run capture first`);
  return JSON.parse(fs.readFileSync(p, 'utf-8'));
}

function loadMetrics(evidenceDir, entry) {
  const p = path.join(evidenceDir, entry.metrics);
  if (!fs.existsSync(p)) return null;
  return JSON.parse(fs.readFileSync(p, 'utf-8'));
}

function metricFlags(metrics) {
  if (!metrics) return [];
  const flags = [];
  if (metrics.pageOverflowX) flags.push(`horizontal page overflow (scrollWidth ${metrics.scrollWidth} > clientWidth ${metrics.clientWidth})`);
  if (metrics.overflowingElements && metrics.overflowingElements.length) {
    flags.push(`${metrics.overflowingElements.length} element(s) overflow their container`);
  }
  if (metrics.unstyledControls && metrics.unstyledControls.length) {
    flags.push(`${metrics.unstyledControls.length} form control(s) missing design-system class`);
  }
  if (metrics.consoleMessages && metrics.consoleMessages.length) {
    flags.push(`${metrics.consoleMessages.length} console error/warning`);
  }
  if (metrics.redirected) {
    flags.push(`redirected to ${metrics.finalUrl}`);
  }
  return flags;
}

function buildFindingsHtml(findings, evidenceDir) {
  const rows = findings
    .map(
      (f) => `  <tr>
      <td class="mono">${esc(f.id)}</td>
      <td>${esc(f.surface)}</td>
      <td class="mono small">${esc(f.route)}</td>
      <td><span class="badge sev-${esc(f.severity)}">${esc(f.severity)}</span></td>
      <td>${esc(f.title)}</td>
    </tr>`
    )
    .join('\n');

  const sections = findings
    .map((f) => {
      const shot = f.annotated_screenshot || f.screenshot;
      const img = shot ? embedImage(evidenceDir, shot) : null;
      return `    <section class="finding" id="${esc(f.id)}">
      <h2><span class="badge sev-${esc(f.severity)}">${esc(f.severity)}</span> ${esc(f.id)} — ${esc(f.title)}</h2>
      <div class="meta">
        <div><b>Surface:</b> ${esc(f.surface)}</div>
        <div><b>Route:</b> <span class="mono">${esc(f.route)}</span></div>
        <div><b>Component:</b> <span class="mono">${esc(f.component_file || 'unknown')}</span></div>
        <div><b>Mockup:</b> ${esc(f.mockup || 'not specified')}</div>
      </div>
      ${img ? `<img src="${img}" alt="${esc(f.title)}">` : ''}
      <p>${esc(f.description)}</p>
    </section>`;
    })
    .join('\n\n');

  return { rows, sections };
}

function buildCoverageHtml(manifest, evidenceDir, findingsById) {
  const byRoute = new Map();
  for (const entry of manifest.captured) {
    if (!byRoute.has(entry.route)) byRoute.set(entry.route, []);
    byRoute.get(entry.route).push(entry);
  }

  const items = [];
  for (const [routeId, entries] of byRoute) {
    // Use the first captured width as the representative coverage shot.
    const entry = entries[0];
    const metrics = loadMetrics(evidenceDir, entry);
    const flags = metricFlags(metrics);
    const linkedFindings = (findingsById.get(routeId) || []);
    const img = embedImage(evidenceDir, entry.screenshot);
    const verdictClass = linkedFindings.length || flags.length ? 'cov-finding' : 'cov-clean';
    const verdictText = linkedFindings.length
      ? `${linkedFindings.length} finding(s) (${linkedFindings.map((f) => f.id).join(', ')})`
      : flags.length
      ? `${flags.length} metric flag(s)`
      : 'CLEAN';
    items.push(`<div class="cov-item">
      <h3>${esc(routeId)} <span class="cov-verdict ${verdictClass}">${esc(verdictText)}</span></h3>
      <div class="mono small route">${esc(metrics ? metrics.path : entry.route)} (${entries.map((e) => e.width).join(', ')})</div>
      ${img ? `<img src="${img}" alt="${esc(routeId)}">` : ''}
      ${flags.length ? `<ul class="diff-list">${flags.map((f) => `<li>${esc(f)}</li>`).join('')}</ul>` : ''}
    </div>`);
  }
  return items.join('\n');
}

function buildNotCoveredHtml(manifest) {
  if (!manifest.failures || !manifest.failures.length) return '';
  const items = manifest.failures
    .map((f) => `<li><span class="mono">${esc(f.route)}${f.width ? ' @' + esc(f.width) : ''}</span> — ${esc(f.reason)}</li>`)
    .join('\n');
  return `<h2>Not covered / capture failures</h2>\n<ul class="not-covered">\n${items}\n</ul>`;
}

async function main() {
  const args = parseArgs(process.argv.slice(2));
  const manifest = loadManifest(args.evidence);

  let findings = [];
  if (args.findings && fs.existsSync(args.findings)) {
    findings = JSON.parse(fs.readFileSync(args.findings, 'utf-8'));
  }

  const findingsById = new Map();
  for (const f of findings) {
    const key = f.route_id || f.route;
    if (!findingsById.has(key)) findingsById.set(key, []);
    findingsById.get(key).push(f);
  }

  const sevCounts = { broken: 0, inconsistent: 0, polish: 0 };
  for (const f of findings) {
    if (sevCounts[f.severity] !== undefined) sevCounts[f.severity]++;
  }
  const surfaceCount = new Set(manifest.captured.map((c) => c.route)).size;

  const template = fs.readFileSync(path.join(__dirname, 'report-template.html'), 'utf-8');

  const heading = findings.length ? 'Quasar UI Visual Audit' : 'Quasar UI Coverage Report';
  const subtitle = `Live instance <span class="mono">${esc(manifest.url)}</span> · captured ${esc(manifest.stamp)}${
    manifest.sha ? ` · <span class="mono">${esc(manifest.sha)}</span>` : ''
  }`;

  const statCards = findings.length
    ? `    <div class="stat broken"><div class="n">${sevCounts.broken}</div><div class="l">Broken</div></div>
    <div class="stat inconsistent"><div class="n">${sevCounts.inconsistent}</div><div class="l">Inconsistent</div></div>
    <div class="stat polish"><div class="n">${sevCounts.polish}</div><div class="l">Polish</div></div>
    <div class="stat"><div class="n">${surfaceCount}</div><div class="l">Surfaces audited</div></div>`
    : `    <div class="stat"><div class="n">${surfaceCount}</div><div class="l">Surfaces captured</div></div>
    <div class="stat"><div class="n">${manifest.captured.length}</div><div class="l">Route/width shots</div></div>
    <div class="stat"><div class="n">${manifest.failures.length}</div><div class="l">Capture failures</div></div>`;

  let body = '';
  if (findings.length) {
    const { rows, sections } = buildFindingsHtml(findings, args.evidence);
    body += `<h2>Summary</h2>\n<table class="summary">\n  <thead><tr><th>ID</th><th>Surface</th><th>Route</th><th>Severity</th><th>Finding</th></tr></thead>\n  <tbody>\n${rows}\n  </tbody>\n</table>\n\n<h2>Findings</h2>\n\n${sections}\n\n`;
  }
  body += `<h2>Coverage appendix</h2>\n<div class="cov-grid">\n${buildCoverageHtml(manifest, args.evidence, findingsById)}\n</div>\n\n`;
  body += buildNotCoveredHtml(manifest);

  const footer = `Quasar UI Audit · generated via scripts/dx/uiaudit · all screenshots embedded inline, no external assets`;

  const html = template
    .replace('{{TITLE}}', esc(heading))
    .replace('{{HEADING}}', esc(heading))
    .replace('{{SUBTITLE}}', subtitle)
    .replace('{{STAT_CARDS}}', statCards)
    .replace('{{BODY}}', body)
    .replace('{{FOOTER}}', footer);

  fs.mkdirSync(path.dirname(path.resolve(args.out)), { recursive: true });
  fs.writeFileSync(args.out, html);
  console.log(`wrote ${args.out} (${findings.length} finding(s), ${surfaceCount} surface(s))`);
}

main().catch((err) => {
  console.error(`FAIL report — ${err.stack || err.message}`);
  process.exit(1);
});
