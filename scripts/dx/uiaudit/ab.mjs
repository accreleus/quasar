#!/usr/bin/env node
// scripts/dx/uiaudit/ab.mjs — diff two capture.mjs evidence dirs (before/after)
// into a single self-contained A/B HTML report.
//
// Usage:
//   node ab.mjs --before <dir> --after <dir> --out <report.html>

import fs from 'node:fs';
import path from 'node:path';
import url from 'node:url';

const __dirname = path.dirname(url.fileURLToPath(import.meta.url));

function parseArgs(argv) {
  const args = { before: '', after: '', out: '' };
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    const need = () => {
      if (i + 1 >= argv.length) throw new Error(`${a} requires a value`);
      return argv[++i];
    };
    switch (a) {
      case '--before': args.before = need(); break;
      case '--after': args.after = need(); break;
      case '--out': args.out = need(); break;
      default: throw new Error(`unknown arg '${a}'`);
    }
  }
  if (!args.before) throw new Error('--before is required');
  if (!args.after) throw new Error('--after is required');
  if (!args.out) throw new Error('--out is required');
  return args;
}

function esc(s) {
  return String(s == null ? '' : s).replace(/[&<>"']/g, (c) => ({
    '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#x27;',
  }[c]));
}

function loadManifest(dir) {
  const p = path.join(dir, 'manifest.json');
  if (!fs.existsSync(p)) throw new Error(`no manifest.json in ${dir} — run capture first`);
  return JSON.parse(fs.readFileSync(p, 'utf-8'));
}

function loadMetrics(dir, relPath) {
  const p = path.join(dir, relPath);
  if (!fs.existsSync(p)) return null;
  return JSON.parse(fs.readFileSync(p, 'utf-8'));
}

function embedImage(dir, relPath) {
  const p = path.join(dir, relPath);
  if (!fs.existsSync(p)) return null;
  return `data:image/png;base64,${fs.readFileSync(p).toString('base64')}`;
}

function key(entry) {
  return `${entry.route}--${entry.width}`;
}

function diffMetrics(before, after) {
  const diffs = [];
  const kind = (label, b, a) => {
    if (b === a) return;
    diffs.push({ label, before: b, after: a, regressed: a > b });
  };
  if (!before || !after) {
    diffs.push({ label: 'capture', before: before ? 'ok' : 'missing', after: after ? 'ok' : 'missing', regressed: !after });
    return diffs;
  }
  kind('overflowing elements', before.overflowingElements.length, after.overflowingElements.length);
  kind('unstyled controls', before.unstyledControls.length, after.unstyledControls.length);
  kind('console errors/warnings', before.consoleMessages.length, after.consoleMessages.length);
  if (before.pageOverflowX !== after.pageOverflowX) {
    diffs.push({ label: 'horizontal page overflow', before: before.pageOverflowX, after: after.pageOverflowX, regressed: after.pageOverflowX && !before.pageOverflowX });
  }
  if (before.title !== after.title) {
    diffs.push({ label: 'page title', before: before.title, after: after.title, regressed: false });
  }
  if (before.finalUrl !== after.finalUrl) {
    diffs.push({ label: 'final URL / redirect', before: before.finalUrl, after: after.finalUrl, regressed: false });
  }
  return diffs;
}

async function main() {
  const args = parseArgs(process.argv.slice(2));
  const beforeManifest = loadManifest(args.before);
  const afterManifest = loadManifest(args.after);

  const beforeByKey = new Map(beforeManifest.captured.map((e) => [key(e), e]));
  const afterByKey = new Map(afterManifest.captured.map((e) => [key(e), e]));
  const allKeys = new Set([...beforeByKey.keys(), ...afterByKey.keys()]);

  const onlyBefore = [...allKeys].filter((k) => !afterByKey.has(k));
  const onlyAfter = [...allKeys].filter((k) => !beforeByKey.has(k));
  const common = [...allKeys].filter((k) => beforeByKey.has(k) && afterByKey.has(k));

  let regressions = 0;
  let improvements = 0;
  const rows = [];
  const pairs = [];

  for (const k of common.sort()) {
    const bEntry = beforeByKey.get(k);
    const aEntry = afterByKey.get(k);
    const bMetrics = loadMetrics(args.before, bEntry.metrics);
    const aMetrics = loadMetrics(args.after, aEntry.metrics);
    const diffs = diffMetrics(bMetrics, aMetrics);
    if (!diffs.length) continue;

    const hasRegression = diffs.some((d) => d.regressed);
    const hasImprovement = diffs.some((d) => !d.regressed && d.before !== d.after && typeof d.before === 'number' && d.after < d.before);
    if (hasRegression) regressions++;
    else if (hasImprovement) improvements++;

    const verdict = hasRegression ? 'regression' : hasImprovement ? 'improvement' : 'unchanged';
    rows.push(`  <tr>
      <td class="mono">${esc(k)}</td>
      <td><span class="badge sev-${verdict}">${verdict}</span></td>
      <td>${diffs.map((d) => esc(`${d.label}: ${d.before} → ${d.after}`)).join('<br>')}</td>
    </tr>`);

    const bImg = embedImage(args.before, bEntry.screenshot);
    const aImg = embedImage(args.after, aEntry.screenshot);
    pairs.push(`    <section class="finding" id="ab-${esc(k)}">
      <h2><span class="badge sev-${verdict}">${verdict}</span> ${esc(k)}</h2>
      <div class="ab-pair">
        <figure><figcaption>before</figcaption>${bImg ? `<img src="${bImg}" alt="before ${esc(k)}">` : '<p>no screenshot</p>'}</figure>
        <figure><figcaption>after</figcaption>${aImg ? `<img src="${aImg}" alt="after ${esc(k)}">` : '<p>no screenshot</p>'}</figure>
      </div>
      <ul class="diff-list">${diffs.map((d) => `<li>${esc(d.label)}: <span class="mono">${esc(JSON.stringify(d.before))}</span> → <span class="mono">${esc(JSON.stringify(d.after))}</span>${d.regressed ? ' <b>(regression)</b>' : ''}</li>`).join('')}</ul>
    </section>`);
  }

  const template = fs.readFileSync(path.join(__dirname, 'report-template.html'), 'utf-8');
  const heading = 'Quasar UI A/B Report';
  const subtitle = `<span class="mono">${esc(args.before)}</span> (before, ${esc(beforeManifest.stamp)}) vs <span class="mono">${esc(args.after)}</span> (after, ${esc(afterManifest.stamp)}) · ${esc(afterManifest.url)}`;
  const statCards = `    <div class="stat regression"><div class="n">${regressions}</div><div class="l">Regressions</div></div>
    <div class="stat improvement"><div class="n">${improvements}</div><div class="l">Improvements</div></div>
    <div class="stat"><div class="n">${common.length}</div><div class="l">Common route/widths</div></div>
    <div class="stat"><div class="n">${onlyBefore.length + onlyAfter.length}</div><div class="l">Only in one run</div></div>`;

  let body = `<h2>Summary</h2>\n<table class="summary">\n  <thead><tr><th>Route / width</th><th>Verdict</th><th>Diffs</th></tr></thead>\n  <tbody>\n${
    rows.length ? rows.join('\n') : '  <tr><td colspan="3">No metric diffs — identical on every common route/width.</td></tr>'
  }\n  </tbody>\n</table>\n\n`;

  body += `<h2>Side-by-side</h2>\n\n${pairs.join('\n\n') || '<p>No diffs to show side-by-side.</p>'}\n\n`;

  if (onlyBefore.length || onlyAfter.length) {
    body += `<h2>Only in one run</h2>\n<ul class="not-covered">\n`;
    for (const k of onlyBefore) body += `<li><span class="mono">${esc(k)}</span> — only in <b>before</b> (missing from after)</li>\n`;
    for (const k of onlyAfter) body += `<li><span class="mono">${esc(k)}</span> — only in <b>after</b> (missing from before)</li>\n`;
    body += `</ul>\n`;
  }

  const footer = `Quasar UI A/B Report · generated via scripts/dx/uiaudit ab · all screenshots embedded inline, no external assets`;

  const html = template
    .replace('{{TITLE}}', esc(heading))
    .replace('{{HEADING}}', esc(heading))
    .replace('{{SUBTITLE}}', subtitle)
    .replace('{{STAT_CARDS}}', statCards)
    .replace('{{BODY}}', body)
    .replace('{{FOOTER}}', footer);

  fs.mkdirSync(path.dirname(path.resolve(args.out)), { recursive: true });
  fs.writeFileSync(args.out, html);
  console.log(`wrote ${args.out} (${regressions} regression(s), ${improvements} improvement(s), ${common.length} common)`);
  if (regressions > 0) process.exitCode = 1;
}

main().catch((err) => {
  console.error(`FAIL ab — ${err.stack || err.message}`);
  process.exit(1);
});
