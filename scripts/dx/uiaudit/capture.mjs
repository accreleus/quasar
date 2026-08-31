#!/usr/bin/env node
// scripts/dx/uiaudit/capture.mjs — deterministic evidence capture for the
// Quasar UI audit DX capability. Invoked by scripts/dx/uiaudit.sh; not meant
// to be run bare (it needs Playwright's chromium bootstrapped, which the
// shell wrapper handles the same way scripts/dx/validate.sh does).
//
// Per route x width:
//   <evidence>/<routeId>--<WxH>.png              full-page screenshot
//   <evidence>/<routeId>--<WxH>.metrics.json      deterministic DOM checks
// Plus <evidence>/manifest.json with run metadata.
//
// Usage:
//   node capture.mjs --url https://host:8443 --out <dir> \
//     [--routes all|id,id,...] [--state-admin <file>] [--state-user <file>] \
//     [--widths 1440x900,1280x900] [--stamp <iso>] [--sha <gitsha>]

import { createRequire } from 'node:module';
import fs from 'node:fs';
import path from 'node:path';
import url from 'node:url';

const __dirname = path.dirname(url.fileURLToPath(import.meta.url));
const DX_ROOT = path.resolve(__dirname, '..', '..', '..');
const VALIDATE_DIR = path.join(DX_ROOT, 'scripts', 'validate');

// Playwright is only installed under scripts/validate/node_modules (the
// repo's existing bootstrap target — see scripts/dx/validate.sh). ESM import
// resolution does not honor NODE_PATH, so resolve it explicitly via
// createRequire rooted at that directory instead of duplicating the
// dependency here.
const requireFromValidate = createRequire(path.join(VALIDATE_DIR, 'noop.cjs'));
let chromium;
try {
  ({ chromium } = requireFromValidate('playwright'));
} catch (err) {
  console.error(
    `FAIL playwright — could not resolve 'playwright' from ${VALIDATE_DIR}/node_modules (${err.message}). ` +
      'Run this via scripts/dx/uiaudit.sh, which bootstraps it first.'
  );
  process.exit(2);
}

function parseArgs(argv) {
  const args = {
    url: '',
    out: '',
    routes: 'all',
    stateAdmin: '',
    stateUser: '',
    widths: '',
    stamp: new Date().toISOString(),
    sha: '',
  };
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    const need = () => {
      if (i + 1 >= argv.length) throw new Error(`${a} requires a value`);
      return argv[++i];
    };
    switch (a) {
      case '--url': args.url = need(); break;
      case '--out': args.out = need(); break;
      case '--routes': args.routes = need(); break;
      case '--state-admin': args.stateAdmin = need(); break;
      case '--state-user': args.stateUser = need(); break;
      case '--widths': args.widths = need(); break;
      case '--stamp': args.stamp = need(); break;
      case '--sha': args.sha = need(); break;
      default:
        throw new Error(`unknown arg '${a}'`);
    }
  }
  if (!args.url) throw new Error('--url is required');
  if (!args.out) throw new Error('--out is required');
  return args;
}

function parseWidth(spec) {
  const m = /^(\d+)x(\d+)$/.exec(spec.trim());
  if (!m) throw new Error(`bad width spec '${spec}' (expected WxH, e.g. 1440x900)`);
  return { width: Number(m[1]), height: Number(m[2]), label: `${m[1]}x${m[2]}` };
}

async function scanPage(page) {
  return page.evaluate(() => {
    const DESIGN_CLASS = { SELECT: 'select', INPUT: 'input', TEXTAREA: 'input' };
    const results = {
      overflowingElements: [],
      pageOverflowX: false,
      scrollWidth: document.documentElement.scrollWidth,
      clientWidth: document.documentElement.clientWidth,
      unstyledControls: [],
      title: document.title,
      finalUrl: location.href,
    };
    results.pageOverflowX = results.scrollWidth > results.clientWidth + 1;

    const TOLERANCE = 2;
    const MAX_ITEMS = 50;
    const all = document.body ? document.body.querySelectorAll('*') : [];
    for (const el of all) {
      if (results.overflowingElements.length >= MAX_ITEMS) break;
      const parent = el.parentElement;
      if (!parent) continue;
      const rect = el.getBoundingClientRect();
      if (rect.width === 0 && rect.height === 0) continue;
      const parentRect = parent.getBoundingClientRect();
      if (rect.right > parentRect.right + TOLERANCE && rect.width > 0) {
        results.overflowingElements.push({
          tag: el.tagName.toLowerCase(),
          id: el.id || null,
          class: el.className && typeof el.className === 'string' ? el.className : null,
          rightOverflowPx: Math.round(rect.right - parentRect.right),
          text: (el.textContent || '').trim().slice(0, 60),
        });
      }
    }

    const controls = document.body
      ? document.body.querySelectorAll('select, input, textarea')
      : [];
    for (const el of controls) {
      if (results.unstyledControls.length >= MAX_ITEMS) break;
      const tag = el.tagName;
      const needed = DESIGN_CLASS[tag];
      if (!needed) continue;
      // Checkbox/radio/hidden inputs legitimately carry no `.input` class in
      // this design system (they get bespoke or native-ok styling), so
      // flagging them is pure noise for the judgment pass.
      const inputType = (el.getAttribute('type') || 'text').toLowerCase();
      if (tag === 'INPUT' && ['checkbox', 'radio', 'hidden', 'range', 'file', 'color'].includes(inputType)) continue;
      const classes = (el.className && typeof el.className === 'string' ? el.className : '').split(/\s+/);
      if (!classes.includes(needed)) {
        results.unstyledControls.push({
          tag: tag.toLowerCase(),
          type: el.getAttribute('type') || null,
          id: el.id || null,
          name: el.getAttribute('name') || null,
          class: el.className || '',
          expectedClass: needed,
        });
      }
    }

    return results;
  });
}

async function main() {
  const args = parseArgs(process.argv.slice(2));
  fs.mkdirSync(args.out, { recursive: true });

  const routesJson = JSON.parse(
    fs.readFileSync(path.join(__dirname, 'routes.json'), 'utf-8')
  );
  const allRoutes = routesJson.routes;

  let selectedRoutes;
  if (args.routes === 'all') {
    selectedRoutes = allRoutes;
  } else {
    const ids = new Set(args.routes.split(',').map((s) => s.trim()).filter(Boolean));
    selectedRoutes = allRoutes.filter((r) => ids.has(r.id));
    const missing = [...ids].filter((id) => !allRoutes.some((r) => r.id === id));
    if (missing.length) {
      console.error(`FAIL routes — unknown route id(s): ${missing.join(', ')}`);
      process.exit(2);
    }
  }

  const widthOverride = args.widths
    ? args.widths.split(',').map((s) => s.trim()).filter(Boolean)
    : null;

  const browser = await chromium.launch();
  const contexts = {}; // role -> BrowserContext
  const failures = [];
  const captured = [];
  let hostId = null;

  async function getContext(role) {
    if (contexts[role]) return contexts[role];
    const opts = { ignoreHTTPSErrors: true };
    if (role === 'admin' && args.stateAdmin) opts.storageState = args.stateAdmin;
    if (role === 'user' && args.stateUser) opts.storageState = args.stateUser;
    if (role === 'admin' && !args.stateAdmin) {
      throw new Error('route requires role=admin but --state-admin was not provided');
    }
    if (role === 'user' && !args.stateUser) {
      throw new Error('route requires role=user but --state-user was not provided');
    }
    const ctx = await browser.newContext(opts);
    contexts[role] = ctx;
    return ctx;
  }

  async function resolveHostId(baseUrl) {
    if (hostId) return hostId;
    const ctx = await getContext('admin');
    const page = await ctx.newPage();
    try {
      await page.setViewportSize({ width: 1440, height: 900 });
      await page.goto(`${baseUrl}/admin/hosts`, { waitUntil: 'networkidle', timeout: 30000 });
      const href = await page.evaluate(() => {
        const link = Array.from(document.querySelectorAll('a[href*="/admin/hosts/"]')).find(
          (a) => /\/admin\/hosts\/[^/]+\/(settings|console)/.test(a.getAttribute('href') || '')
        );
        return link ? link.getAttribute('href') : null;
      });
      if (href) {
        const m = /\/admin\/hosts\/([^/]+)\//.exec(href);
        if (m) hostId = m[1];
      }
    } finally {
      await page.close();
    }
    return hostId;
  }

  for (const route of selectedRoutes) {
    let routePath = route.path;
    if (route.dynamic) {
      const id = await resolveHostId(args.url);
      if (!id) {
        failures.push({ route: route.id, reason: 'could not resolve a host id from /admin/hosts (no hosts registered on this stack?)' });
        continue;
      }
      routePath = routePath.replace('{id}', id);
    }

    const widths = (widthOverride || route.widths || routesJson.defaultWidths).map(parseWidth);

    let ctx;
    try {
      ctx = await getContext(route.role === 'none' ? 'anon' : route.role);
    } catch (err) {
      failures.push({ route: route.id, reason: err.message });
      continue;
    }

    for (const w of widths) {
      const page = await ctx.newPage();
      const consoleMessages = [];
      page.on('console', (msg) => {
        const t = msg.type();
        if (t === 'error' || t === 'warning') {
          consoleMessages.push({ type: t, text: msg.text().slice(0, 300) });
        }
      });
      page.on('pageerror', (err) => {
        consoleMessages.push({ type: 'pageerror', text: String(err).slice(0, 300) });
      });

      const base = `${route.id}--${w.label}`;
      const pngPath = path.join(args.out, `${base}.png`);
      const metricsPath = path.join(args.out, `${base}.metrics.json`);

      try {
        await page.setViewportSize({ width: w.width, height: w.height });
        const resp = await page.goto(`${args.url}${routePath}`, {
          waitUntil: 'networkidle',
          timeout: 30000,
        });
        await page.waitForTimeout(300); // settle late layout/animation
        const scan = await scanPage(page);
        await page.screenshot({ path: pngPath, fullPage: true });

        const metrics = {
          route: route.id,
          path: routePath,
          role: route.role,
          width: w.label,
          httpStatus: resp ? resp.status() : null,
          title: scan.title,
          finalUrl: scan.finalUrl,
          requestedUrl: `${args.url}${routePath}`,
          redirected: scan.finalUrl !== `${args.url}${routePath}`,
          pageOverflowX: scan.pageOverflowX,
          scrollWidth: scan.scrollWidth,
          clientWidth: scan.clientWidth,
          overflowingElements: scan.overflowingElements,
          unstyledControls: scan.unstyledControls,
          consoleMessages,
          screenshot: `${base}.png`,
        };
        fs.writeFileSync(metricsPath, JSON.stringify(metrics, null, 2));
        captured.push({ route: route.id, width: w.label, screenshot: `${base}.png`, metrics: `${base}.metrics.json` });
      } catch (err) {
        failures.push({ route: route.id, width: w.label, reason: err.message });
      } finally {
        await page.close();
      }
    }
  }

  for (const ctx of Object.values(contexts)) await ctx.close();
  await browser.close();

  const manifest = {
    url: args.url,
    sha: args.sha || null,
    stamp: args.stamp,
    routesRequested: selectedRoutes.map((r) => r.id),
    captured,
    failures,
  };
  fs.writeFileSync(path.join(args.out, 'manifest.json'), JSON.stringify(manifest, null, 2));

  console.log(`captured ${captured.length} route/width shots, ${failures.length} failure(s) -> ${args.out}`);
  if (failures.length) {
    for (const f of failures) console.error(`  FAIL ${f.route}${f.width ? ' @' + f.width : ''} — ${f.reason}`);
  }
  process.exit(failures.length > 0 && captured.length === 0 ? 1 : 0);
}

main().catch((err) => {
  console.error(`FAIL capture — ${err.stack || err.message}`);
  process.exit(1);
});
