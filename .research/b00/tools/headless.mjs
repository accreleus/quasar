// Correctness run in headless Chromium (software WebGPU; timings are meaningless there).
// Usage (inside mcr.microsoft.com/playwright:v1.56.0-noble, with playwright installed):
//   node headless.mjs /path/to/.research/b00 /out/dir
import { chromium } from 'playwright';
import fs from 'node:fs';
const [dir, out] = process.argv.slice(2);
// HW=1: use the real GPU (Vulkan) instead of SwiftShader.
const args = process.env.HW === '1'
  ? ['--enable-unsafe-webgpu', '--enable-features=Vulkan', '--use-vulkan=native', '--ignore-gpu-blocklist',
     '--disable-vulkan-surface', '--use-angle=vulkan', '--enable-webgpu-developer-features']
  : ['--enable-unsafe-webgpu', '--enable-features=Vulkan', '--use-vulkan=swiftshader',
     '--use-webgpu-adapter=swiftshader', '--disable-vulkan-surface', '--enable-unsafe-swiftshader'];
const browser = await chromium.launch({ args });
const page = await browser.newPage({ viewport: { width: 1400, height: 1500 } });
page.on('console', (m) => console.log('[page]', m.text()));
page.on('pageerror', (e) => console.log('[pageerror]', e.message));
await page.goto('file://' + dir + '/index.html?reps=' + (process.env.REPS || '3') + (process.env.QS || ''));
await page.waitForFunction(() => window.B00_RESULT && window.B00_RESULT.done, null, { timeout: 30 * 60 * 1000 });
const r = await page.evaluate(() => window.B00_RESULT);
fs.writeFileSync(out + '/headless-result.json', JSON.stringify(r, null, 2));
await page.screenshot({ path: out + '/headless.png', fullPage: true });
console.log(JSON.stringify({ env: r.env, correctness: r.correctness, errors: r.errors }, null, 2));
await browser.close();
process.exit(r.errors.length || r.correctness.some((c) => c.pass === false) ? 1 : 0);
