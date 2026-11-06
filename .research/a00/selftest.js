// Headless self-test: node selftest.js '<page-url-with-query>' [timeoutSec] [screenshot.png]
const { chromium } = require('playwright');
(async () => {
  const [url, tmo = '120', shot] = process.argv.slice(2);
  const b = await chromium.launch({ headless: true, args: ['--no-sandbox'] });
  const p = await b.newPage({ viewport: { width: 1300, height: 700 } });
  p.on('console', m => console.error('console:', m.text()));
  p.on('pageerror', e => console.error('pageerror:', e.message));
  await p.goto(url);
  await p.waitForFunction(() => document.title === 'A00-DONE', null, { timeout: +tmo * 1000 });
  if (shot) await p.screenshot({ path: shot, fullPage: true });
  console.log(await p.textContent('#result'));
  await b.close();
})().catch(e => { console.error(e); process.exit(1); });
