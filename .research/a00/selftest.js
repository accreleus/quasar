// Headless self-test: node selftest.js '<page-url-with-query>' [timeoutSec] [screenshot.png]
const { chromium } = require('playwright');
(async () => {
  const [url, tmo = '120', shot] = process.argv.slice(2);
  // HEADED=1 for a real window (Mac); CHANNEL=chrome for installed Chrome instead of Playwright's Chromium
  const b = await chromium.launch({ headless: !process.env.HEADED, channel: process.env.CHANNEL || undefined, args: process.env.HEADED ? [] : ['--no-sandbox'] });
  const p = await b.newPage({ viewport: { width: 1300, height: 700 } });
  p.on('console', m => console.error('console:', m.text()));
  p.on('pageerror', e => console.error('pageerror:', e.message));
  await p.goto(url);
  await p.waitForFunction(() => document.title === 'A00-DONE', null, { timeout: +tmo * 1000 });
  if (shot) await p.screenshot({ path: shot, fullPage: true });
  const res = await p.textContent('#result');
  require('fs').writeFileSync(process.env.OUT || 'a00-result.json', res);
  console.log(res);
  await b.close();
})().catch(e => { console.error(e); process.exit(1); });
