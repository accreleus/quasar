// Two real Chrome processes against the takeover browser host (#535).
// usage: node driver.mjs <scenario> <ws-url> <peer.html url>
// Prints one JSON object: what each page saw, and `checks` (name -> pass).
import { chromium } from "playwright-core";

const [scenario, wsUrl, pageUrl] = process.argv.slice(2);
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const open = () => chromium.launch({
  executablePath: process.env.CHROME || "/usr/bin/google-chrome",
  headless: false,
  args: ["--no-sandbox", "--autoplay-policy=no-user-gesture-required", "--allow-file-access-from-files",
    "--disable-features=WebRtcHideLocalIpsWithMdns,CalculateNativeWinOcclusion",
    "--disable-backgrounding-occluded-windows", "--disable-renderer-backgrounding", "--disable-background-timer-throttling"],
});
const url = (opts = "") => `${pageUrl}?ws=${encodeURIComponent(wsUrl)}${opts}`;
const snap = async (page) => { try { return await page.evaluate(() => window.__peer.snapshot()); } catch (e) { return { error: String(e) }; } };
const frames = (s) => s?.video?.framesDecoded ?? 0;
const until = async (page, n, ms) => {
  const end = Date.now() + ms; let s;
  do { s = await snap(page); if (frames(s) >= n) break; await sleep(200); } while (Date.now() < end);
  return s;
};
const pong = async (page) => {
  const before = (await snap(page)).pongs ?? 0;
  await page.evaluate(() => window.__peer.ping()); await sleep(500);
  return ((await snap(page)).pongs ?? 0) > before;
};
// Frames a page decodes over the next `ms`.
const flowing = async (page, ms = 1500) => { const a = frames(await snap(page)); await sleep(ms); return frames(await snap(page)) - a; };

const out = { scenario, checks: {} };
const check = (name, pass) => { out.checks[name] = Boolean(pass); };
const browserA = await open(); const A = await browserA.newPage();

if (scenario === "connecting") {
  await A.goto(url("&noice=1")); await sleep(1500);
  out.A = await snap(A);
  check("first page answered and is not connected", out.A.offers?.video === 1 && frames(out.A) === 0);
} else {
  await A.goto(url()); out.A = await until(A, 30, 20000);
  check("first page streams", frames(out.A) >= 30);
}

if (scenario === "takeover" || scenario === "connecting") {
  const browserB = await open(); const B = await browserB.newPage();
  await B.goto(url("&attach=1")); out.B = await until(B, 30, 15000);
  check("the page that attaches decodes", frames(out.B) >= 30);
  check("its input channel reaches the host", await pong(B));
  if (scenario === "takeover") {
    check("the displaced page was told (4410)", (await snap(A)).wsCloses?.includes(4410));
    check("the displaced page stops receiving", (await flowing(A)) === 0);
  }
  await A.close(); await sleep(3000);
  check("the stream survives the first page leaving", (await flowing(B)) > 20);
  check("input still works after the first page left", await pong(B));
  out.B_final = await snap(B);
  await browserB.close();
} else if (scenario === "reload") {
  // Same tab navigates: the old document is unloaded, as on a reload.
  await A.goto(url("&attach=1")); out.A2 = await until(A, 30, 15000);
  check("the reloaded page decodes", frames(out.A2) >= 30);
  check("its input channel reaches the host", await pong(A));
} else if (scenario === "samepeer") {
  // Media recovery: the same page asks for an ICE restart on its own socket.
  await A.evaluate(() => window.__peer.send({ type: "restart_ice", pc: "video" })); await sleep(1500);
  out.A2 = await snap(A);
  check("the page got a second offer", out.A2.offers?.video === 2);
  check("the stream continues", (await flowing(A)) > 20);
  check("input still works", await pong(A));
} else if (scenario === "reattach") {
  // Signaling loss: the same page keeps its peer connections on a new socket.
  await A.evaluate(() => window.__peer.reattach(false)); await sleep(1500);
  check("a silent re-attach changes nothing", (await snap(A)).offers?.video === 1 && (await flowing(A)) > 20);
  await A.evaluate(() => window.__peer.reattach(true)); await sleep(1500);
  out.A2 = await snap(A);
  check("a re-attach with a retry in flight gets one more offer", out.A2.offers?.video === 2);
  check("the stream continues", (await flowing(A)) > 20);
  check("input still works", await pong(A));
}
try { await browserA.close(); } catch { /* already gone */ }
console.log(JSON.stringify(out, null, 1));
