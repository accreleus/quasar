// node --test pack.test.mjs: the e2e pack (#1280). Offline: the archive is unpacked into an empty directory and its entry is run against a
// stack served in-process, so a file the closure missed is a module-not-found here and not on someone else's machine.
import assert from "node:assert/strict";
import fs from "node:fs";
import http from "node:http";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { execFileSync, spawn, spawnSync } from "node:child_process";
import zlib from "node:zlib";
import { fileURLToPath } from "node:url";
import { checkName, crc32, tarGz, zip } from "./archive.mjs";
import { validateBundle } from "./bundle.mjs";
import { closure, packMembers, playwrightFiles, requireTracked } from "./pack.mjs";
import { doctor, dropSignIn, findChrome, parseCli, resolveSignIn, runnerFailureReason, sendBack, withoutSignIn, zipBundle } from "./pack-run.mjs";
import { headless, runnerState, summarise } from "./run.mjs";

const HERE = path.dirname(fileURLToPath(import.meta.url));
const ROOT = path.resolve(HERE, "../../..");
const tmp = () => fs.mkdtempSync(path.join(os.tmpdir(), "e2e-pack-test-"));
const SHA = "0123456789abcdef0123456789abcdef01234567";

/** Reads a zip back with the spec's own fields (central directory), checking each CRC. Returns Map name -> { data, mode }. */
function unzip(buf) {
  const eocd = buf.length - 22;
  assert.equal(buf.readUInt32LE(eocd), 0x06054b50);
  const out = new Map();
  let p = buf.readUInt32LE(eocd + 16);
  for (let i = 0; i < buf.readUInt16LE(eocd + 10); i += 1) {
    assert.equal(buf.readUInt32LE(p), 0x02014b50);
    const [method, crc, csize, usize, nlen, elen, clen] = [10, 16, 20, 24, 28, 30, 32].map((o, k) => (k === 0 || k > 4 ? buf.readUInt16LE(p + o) : buf.readUInt32LE(p + o)));
    const mode = buf.readUInt32LE(p + 38) >>> 16;
    const off = buf.readUInt32LE(p + 42);
    const name = buf.toString("utf8", p + 46, p + 46 + nlen);
    const start = off + 30 + buf.readUInt16LE(off + 26) + buf.readUInt16LE(off + 28);
    assert.equal(method, 8);
    const data = zlib.inflateRawSync(buf.subarray(start, start + csize));
    assert.equal(data.length, usize);
    assert.equal(crc32(data), crc);
    out.set(name, { data, mode: mode & 0o777 });
    p += 46 + nlen + elen + clen;
  }
  return out;
}

function extract(dir, members) {
  for (const [name, { data, mode }] of members) {
    const f = path.join(dir, name);
    fs.mkdirSync(path.dirname(f), { recursive: true });
    fs.writeFileSync(f, data, { mode });
  }
}

const members = packMembers(ROOT, `${SHA}\n`);
const names = members.map((m) => m.name);

test("the archive holds nothing that is not the suite's client half, and every name is safe", () => {
  for (const n of names) {
    checkName(n);
    assert.ok(n.startsWith("quasar-e2e-pack/"), n);
    assert.doesNotMatch(n, /\.claude|hosts\.json|(^|\/)\.env|\.test\.mjs|fixtures|node_modules|signin/, `${n} must not be packed`);
  }
  assert.equal(new Set(names).size, names.length);
  for (const must of ["run.sh", "run.cmd", "README.txt", "VERSION", "package.json", "package-lock.json", "docs/session-trace/thresholds.json", "docs/session-trace/metrics.json", "scripts/validate/stream/plans/smoke.json", "scripts/validate/stream/run.mjs", "scripts/validate/stream/pack-run.mjs"]) {
    assert.ok(names.includes(`quasar-e2e-pack/${must}`), `${must} is missing`);
  }
  assert.equal(members.find((m) => m.name.endsWith("/run.sh")).mode, 0o755);
  assert.equal(members.find((m) => m.name.endsWith("/VERSION")).data.toString(), `${SHA}\n`);
});

test("a name that could leave the unpack directory is refused when the archive is made", () => {
  for (const bad of ["/etc/passwd", "../x", "a/../../x", "a//b", "C:/x", "a\\b", "a/./b", "", "x\0y", "a".repeat(101)]) assert.throws(() => zip([{ name: bad, data: Buffer.alloc(0) }]), /unsafe archive member name/, JSON.stringify(bad));
  assert.throws(() => tarGz([{ name: "../x", data: Buffer.alloc(0) }]), /unsafe/);
});

test("the closure stops at a test, a file outside the suite's directories, and a file that does not exist", () => {
  const root = tmp();
  try {
    fs.mkdirSync(path.join(root, "scripts/validate/stream"), { recursive: true });
    fs.mkdirSync(path.join(root, "web"));
    const w = (f, s) => fs.writeFileSync(path.join(root, f), s);
    w("scripts/validate/stream/a.mjs", 'import x from "./b.mjs";\nimport("../../../web/c.mjs");\n');
    w("scripts/validate/stream/b.mjs", 'import fs from "node:fs";\nimport pw from "playwright-core";\n');
    w("web/c.mjs", "");
    assert.throws(() => closure(["scripts/validate/stream/a.mjs"], root), /web\/c\.mjs cannot go in the pack/);
    w("scripts/validate/stream/a.mjs", 'import x from "./b.mjs";\n');
    assert.deepEqual(closure(["scripts/validate/stream/a.mjs"], root), ["scripts/validate/stream/a.mjs", "scripts/validate/stream/b.mjs"]);
    w("scripts/validate/stream/b.test.mjs", "");
    w("scripts/validate/stream/a.mjs", 'import "./b.test.mjs";\n');
    assert.throws(() => closure(["scripts/validate/stream/a.mjs"], root), /b\.test\.mjs cannot go in the pack/);
    w("scripts/validate/stream/a.mjs", 'import "./gone.mjs";\n');
    assert.throws(() => closure(["scripts/validate/stream/a.mjs"], root), /gone\.mjs cannot go in the pack/);
  } finally { fs.rmSync(root, { recursive: true, force: true }); }
});

test("the lockfile is the repo's own entry for playwright-core and nothing else", () => {
  const f = playwrightFiles(ROOT);
  const repo = JSON.parse(fs.readFileSync(path.join(ROOT, "scripts/validate/package-lock.json"), "utf8"));
  const lock = JSON.parse(f["package-lock.json"]);
  assert.deepEqual(Object.keys(lock.packages), ["", "node_modules/playwright-core"]);
  assert.deepEqual(lock.packages["node_modules/playwright-core"], repo.packages["node_modules/playwright-core"]);
  assert.deepEqual(JSON.parse(f["package.json"]).dependencies, { "playwright-core": repo.packages["node_modules/playwright-core"].version });
});

test("the tar.gz and the zip hold the same files with the same bytes", () => {
  const fromZip = unzip(zip(members));
  assert.deepEqual([...fromZip.keys()], names);
  const dir = tmp();
  try {
    const f = path.join(dir, "p.tar.gz");
    fs.writeFileSync(f, tarGz(members));
    const t = spawnSync("tar", ["-xzf", f, "-C", dir]);
    assert.equal(t.status, 0, String(t.stderr));
    for (const m of members) assert.ok(fs.readFileSync(path.join(dir, m.name)).equals(fromZip.get(m.name).data), m.name);
    assert.equal(fs.statSync(path.join(dir, "quasar-e2e-pack/run.sh")).mode & 0o111, 0o111);
  } finally { fs.rmSync(dir, { recursive: true, force: true }); }
});

/** Like spawnSync, but the event loop keeps serving the in-process stack meanwhile. */
const run = (cmd, args, opts) => new Promise((resolve) => {
  const p = spawn(cmd, args, opts);
  let stdout = "";
  let stderr = "";
  p.stdout.on("data", (b) => { stdout += b; });
  p.stderr.on("data", (b) => { stderr += b; });
  p.on("close", (status) => resolve({ status, stdout, stderr }));
});

/** A stack that answers: the page, the sign-in, the library, and session status/stop for full runs. */
async function fakeStack({ apps = [{ id: "app-1", name: "Quasar Benchapp" }], login = 200 } = {}) {
  const stopped = [];
  const server = http.createServer((req, res) => {
    const json = (code, body) => { res.writeHead(code, { "content-type": "application/json" }); res.end(JSON.stringify(body)); };
    const u = new URL(req.url, "http://127.0.0.1");
    if (req.method === "POST" && u.pathname === "/v1/auth/login") return json(login, login === 200 ? { access_token: "ApiTok12345678" } : { error: "no" });
    if (req.method === "GET" && u.pathname === "/v1/apps") return json(200, { items: apps });
    const m = /^\/v1\/sessions\/([^/]+)$/.exec(u.pathname);
    if (m && req.method === "DELETE") { stopped.push(m[1]); res.writeHead(204); return res.end(); }
    if (m && req.method === "GET") {
      return json(200, {
        session: {
          id: m[1],
          app_id: apps[0]?.id ?? "app-1",
          state: stopped.includes(m[1]) ? "stopped" : "running",
          transport: "webrtc",
          negotiated_codec: "h264",
          stream: { height: 1080, fps: 60 },
        },
      });
    }
    res.writeHead(200, { "content-type": "text/html" });
    res.end("<title>fake stack</title>");
  });
  await new Promise((r) => server.listen(0, "127.0.0.1", r));
  return { url: `http://127.0.0.1:${server.address().port}`, stopped, close: () => new Promise((r) => server.close(r)) };
}

/** Installs an offline fake `playwright-core` inside an unpacked pack directory so the real `pack-run.mjs` -> `run.mjs` -> `stream-hold.mjs` path runs without a real Chrome binary. */
function installFakePlaywrightCore(packDir) {
  const pkgDir = path.join(packDir, "node_modules/playwright-core");
  fs.mkdirSync(pkgDir, { recursive: true });
  fs.writeFileSync(path.join(pkgDir, "package.json"), JSON.stringify({ name: "playwright-core", version: "0.0.0-test", type: "module", exports: "./index.mjs" }));
  fs.writeFileSync(
    path.join(pkgDir, "index.mjs"),
    `import fs from "node:fs";
const SID = "11111111-2222-3333-4444-555555555555";
const PAGE_BEARER = "PageTok1234567890";
const T0 = 1_800_000_000_000;
const windows = Array.from({ length: 25 }, (_, i) => ({
  t_start_ms: T0 + i * 1000, t_end_ms: T0 + (i + 1) * 1000, t_end_host_ms: T0 + (i + 1) * 1000 + 2,
  last_host_time_ms: T0 + i * 1000 + 950, n: 60, decoded: 60, no_image: 0, missing_indices: 0, duplicated: 0, reordered: 0,
  offset_unknown: false, offset_ms: 2, g2g_p50_ms: 20, g2g_p95_ms: 25,
}));
const frames = Array.from({ length: 25 * 60 }, (_, i) => ({ present_ms: T0 + Math.round(i * (1000 / 60)), decoded: true }));
const response = (status, p, body, origin) => ({
  status: () => status, url: () => origin + p, request: () => ({ method: () => "POST" }),
  json: async () => body, text: async () => JSON.stringify(body),
});
function makePage() {
  const handlers = {};
  const ls = new Map();
  let curUrl = "about:blank";
  let origin = "http://127.0.0.1";
  const emit = (ev, arg) => (handlers[ev] ?? []).forEach((fn) => fn(arg));
  const loc = (desc) => ({
    first: () => loc(desc),
    getByRole: (role, o) => loc(desc + " > " + role + ":" + o?.name),
    evaluate: async (fn) => fn({
      disabled: false, getAttribute: () => null,
      click: () => {
        if (!/Play now/.test(desc)) return;
        emit("response", response(201, "/v1/sessions", { session: { id: SID } }, origin));
        curUrl = origin + "/app/session/" + SID;
      },
    }),
  });
  return {
    on: (ev, fn) => { (handlers[ev] ??= []).push(fn); },
    goto: async (u) => { curUrl = u; try { origin = new URL(u).origin; } catch {} },
    url: () => curUrl,
    fill: async () => {},
    click: async () => { ls.set("quasar.auth.token", PAGE_BEARER); emit("response", response(200, "/v1/auth/login", { access_token: PAGE_BEARER }, origin)); curUrl = origin + "/app"; },
    waitForURL: async () => {},
    close: async () => {},
    screenshot: async ({ path: p }) => { fs.writeFileSync(p, Buffer.from([0x89, 0x50, 0x4e, 0x47, 0])); },
    getByRole: (role, o) => loc(role + ":" + o?.name),
    evaluate: async (fn, arg) => {
      const store = Object.create({ getItem(k) { return this[k] ?? null; }, setItem(k, v) { this[k] = v; }, removeItem(k) { delete this[k]; } });
      for (const [k, v] of ls) store[k] = v;
      const keys = ["window", "document", "navigator", "screen", "devicePixelRatio", "requestAnimationFrame", "WebTransport", "VideoDecoder", "localStorage", "sessionStorage"];
      const saved = Object.fromEntries(keys.map((k) => [k, Object.getOwnPropertyDescriptor(globalThis, k)]));
      const setG = (k, v) => Object.defineProperty(globalThis, k, { value: v, configurable: true, writable: true });
      const video = { tagName: "VIDEO", videoWidth: 1920, getVideoPlaybackQuality: () => ({ totalVideoFrames: 1500 }), getBoundingClientRect: () => ({ left: 0, top: 0, width: 1280, height: 720 }) };
      let rafT = 0;
      setG("window", { __qBench: { dump: () => ({ frames, windows }), stats: () => ({ frames: 1500, decoded: 1500 }) } });
      setG("document", {
        visibilityState: "visible", hasFocus: () => true,
        createElement: () => ({ getContext: () => null }),
        querySelectorAll: () => [video],
        querySelector: (s) => (/video/.test(s) ? video : null),
      });
      setG("navigator", {
        userAgent: "Chrome/157.0.0.0", platform: "Linux", hardwareConcurrency: 4,
        userAgentData: { platform: "Linux", brands: [{ brand: "Google Chrome", version: "157" }], getHighEntropyValues: async () => ({ platform: "Linux", platformVersion: "6.0", architecture: "x86", fullVersionList: [{ brand: "Google Chrome", version: "157.0.0.0" }] }) },
        gpu: { requestAdapter: async () => ({ info: { vendor: "fake", architecture: "test" } }) },
        mediaCapabilities: { decodingInfo: async () => ({ supported: true, powerEfficient: true }) },
      });
      setG("screen", { width: 1920, height: 1080 });
      setG("devicePixelRatio", 1);
      setG("requestAnimationFrame", (cb) => queueMicrotask(() => cb((rafT += 500))));
      setG("WebTransport", function WebTransport() {});
      setG("VideoDecoder", { isConfigSupported: async () => ({ supported: true }) });
      setG("localStorage", store);
      setG("sessionStorage", { getItem: () => null });
      try { return await fn(arg); } finally {
        ls.clear();
        for (const [k, v] of Object.entries(store)) ls.set(k, v);
        for (const [k, d] of Object.entries(saved)) { if (d) Object.defineProperty(globalThis, k, d); else delete globalThis[k]; }
      }
    },
  };
}
export const chromium = {
  launch: async () => ({
    version: () => "157.0.0.0",
    newContext: async () => ({ grantPermissions: async () => {}, newPage: async () => makePage(), close: async () => {} }),
    close: async () => { if (process.env.FAKE_CLOSE_FAIL === "1") throw new Error("browser hung on close"); },
  }),
};
`,
  );
}

test("unpacked alone into an empty directory the entry starts (--help, doctor) with no module missing, and says Chrome is absent plainly", async () => {
  const dir = tmp();
  const stack = await fakeStack();
  try {
    extract(dir, unzip(zip(members)));
    const pack = path.join(dir, "quasar-e2e-pack");
    fs.mkdirSync(path.join(pack, "node_modules/playwright-core"), { recursive: true }); // the launcher's npm ci is not this test's business
    fs.writeFileSync(path.join(pack, "signin.txt"), "QUASAR_E2E_EMAIL=tester-account@example.test\nQUASAR_E2E_PASSWORD=correct-horse-battery\n");
    const env = { PATH: process.env.PATH, CHROME: path.join(dir, "no-such-chrome") };
    const entry = path.join(pack, "scripts/validate/stream/pack-run.mjs");
    const help = spawnSync(process.execPath, [entry, "--help"], { cwd: pack, env, encoding: "utf8" });
    assert.equal(help.status, 0, help.stderr);
    assert.match(help.stdout, /usage: pack-run\.mjs/);
    const r = await run(process.execPath, [entry, "doctor", stack.url], { cwd: pack, env });
    assert.doesNotMatch(r.stdout + r.stderr, /ERR_MODULE_NOT_FOUND|Cannot find module/);
    assert.match(r.stdout, /ok {3}stack: answers \(HTTP 200\)/);
    assert.match(r.stdout, /ok {3}sign-in: accepted/);
    assert.match(r.stdout, /ok {3}library: "Quasar Benchapp" is there/);
    assert.match(r.stdout, /FAIL chrome: Google Chrome was not found/);
    assert.match(r.stdout, /RESULT status=failed target=e2e-pack zip= reason=doctor/);
    assert.equal(r.status, 1);
    assert.doesNotMatch(r.stdout + r.stderr, /correct-horse-battery/);
    // the launcher passes its arguments on
    const sh = spawnSync("bash", [path.join(pack, "run.sh"), "--help"], { cwd: dir, env, encoding: "utf8" });
    assert.equal(sh.status, 0, sh.stderr);
    assert.match(sh.stdout, /usage: pack-run\.mjs/);
  } finally { await stack.close(); fs.rmSync(dir, { recursive: true, force: true }); }
});

test("the entry with no address is a usage error, exit 2, and its RESULT line says error", () => {
  const r = spawnSync(process.execPath, [path.join(HERE, "pack-run.mjs")], { encoding: "utf8", env: { PATH: process.env.PATH } });
  assert.equal(r.status, 2);
  assert.match(r.stdout, /RESULT status=error target=e2e-pack zip=/);
});

test("parseCli: doctor is a word, the plan defaults to smoke, a trailing slash goes", () => {
  assert.deepEqual(parseCli(["doctor", "https://s.test/", "--insecure"]), { help: false, insecure: true, doctor: true, target: "https://s.test", plan: "smoke" });
  assert.deepEqual(parseCli(["https://s.test", "fallback"]), { help: false, insecure: false, doctor: false, target: "https://s.test", plan: "fallback" });
});

test("findChrome: CHROME wins and must exist; each OS looks where its Chrome lives", () => {
  const has = (...ps) => (p) => ps.includes(p);
  assert.equal(findChrome({ CHROME: "/x/chrome" }, "linux", has("/x/chrome")), "/x/chrome");
  assert.equal(findChrome({ CHROME: "/x/chrome" }, "linux", has("/usr/bin/google-chrome")), null);
  assert.equal(findChrome({ PATH: "/a:/usr/bin" }, "linux", has("/usr/bin/chromium")), "/usr/bin/chromium");
  assert.equal(findChrome({}, "darwin", has("/Applications/Google Chrome.app/Contents/MacOS/Google Chrome")), "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome");
  const win = "C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe";
  assert.equal(findChrome({ ProgramFiles: "C:\\Program Files" }, "win32", has(win)), win);
  assert.equal(findChrome({ PATH: "/a" }, "linux", () => false), null);
});

const CREDS = { email: "tester-account@example.test", password: "correct-horse-battery" };
const fakeChromium = ({ goto = async () => {}, client = { decode: { h264: { supported: true } }, webtransport: true, webgpu: true } } = {}) => async () => ({
  launch: async () => ({
    version: () => "157.0.0.0",
    newContext: async () => ({ newPage: async () => ({ goto, evaluate: async () => client }) }),
    close: async () => {},
  }),
});

test("doctor: a healthy machine passes every check, including the page and the decoders", async () => {
  const stack = await fakeStack();
  try {
    const d = await doctor({ target: stack.url, plan: "smoke", creds: CREDS, chrome: "/x/chrome", insecure: false, deps: { loadChromium: fakeChromium({ client: { decode: { h264: { supported: true } }, webtransport: true, webgpu: true } }) } });
    assert.deepEqual(d.checks.filter((c) => !c.ok), []);
    assert.deepEqual(d.checks.map((c) => c.name), ["node", "chrome", "plan", "stack", "sign-in", "library", "browser", "decoders"]);
  } finally { await stack.close(); }
});

test("doctor: each way a machine is wrong gets a plain sentence", async () => {
  const stack = await fakeStack();
  const bad = await fakeStack({ login: 401 });
  const noApp = await fakeStack({ apps: [] });
  const check = (o) => doctor({ target: stack.url, plan: "smoke", creds: CREDS, chrome: "/x/chrome", insecure: false, deps: { loadChromium: fakeChromium() }, ...o });
  const failing = async (o) => (await check(o)).checks.filter((c) => !c.ok);
  try {
    const gone = await fakeStack();
    await gone.close();
    assert.match((await failing({ target: gone.url }))[0].detail, /does not answer \(ECONNREFUSED\)/);
    assert.match((await failing({ target: bad.url }))[0].detail, /API sign-in failed \(401\)/);
    assert.match((await failing({ target: noApp.url }))[0].detail, /is not in this user's library/);
    assert.match((await failing({ creds: {} }))[0].detail, /signin\.txt/);
    assert.match((await failing({ creds: { email: "a@b.test", password: "x" } }))[0].detail, /at least 8 characters/);
    assert.match((await failing({ chrome: null }))[0].detail, /Chrome was not found/);
    assert.match((await failing({ plan: "nope" }))[0].detail, /ENOENT/);
    const mac = await failing({ platform: "darwin", deps: { loadChromium: fakeChromium({ goto: async () => { throw new Error("page.goto: net::ERR_ADDRESS_UNREACHABLE at http://x"); } }) } });
    assert.match(mac[0].detail, /Local Network/);
    const other = await failing({ platform: "linux", deps: { loadChromium: fakeChromium({ goto: async () => { throw new Error("net::ERR_ADDRESS_UNREACHABLE"); } }) } });
    assert.doesNotMatch(other[0].detail, /Local Network/);
    const nodep = await failing({ deps: { loadChromium: async () => { throw new Error("Cannot find package 'playwright-core'"); } } });
    assert.match(nodep[0].detail, /could not open/);
  } finally { await Promise.all([stack.close(), bad.close(), noApp.close()]); }
});

test("doctor: decoders the plan needs are notes until none of the plan can run", async () => {
  const stack = await fakeStack();
  try {
    const base = { target: stack.url, plan: "smoke", creds: CREDS, chrome: "/x/chrome", insecure: false };
    const some = await doctor({ ...base, deps: { loadChromium: fakeChromium({ client: { decode: {}, webtransport: true, webgpu: false } }) } });
    assert.equal(some.ok, true);
    assert.match(some.checks.at(-1).detail, /scenario 3 will be skipped: this client cannot decode pyrowave/);
    const none = await doctor({ ...base, deps: { loadChromium: fakeChromium({ client: { decode: { h264: { supported: false } }, webtransport: false, webgpu: false } }) } });
    assert.equal(none.ok, false);
    assert.equal(none.checks.at(-1).name, "decoders");
  } finally { await stack.close(); }
});

test("the zip a run sends back passes the bundle validator and holds the bundle only", () => {
  const dir = tmp();
  try {
    const bundle = path.join(dir, "bundle-2026-01-01T00-00-00");
    fs.cpSync(path.join(HERE, "fixtures/bundle"), bundle, { recursive: true });
    assert.deepEqual(validateBundle(bundle), []);
    const file = path.join(dir, "send.zip");
    const n = zipBundle(bundle, file, new Date(Date.UTC(2026, 0, 1, 12, 30, 20)));
    const back = unzip(fs.readFileSync(file));
    assert.equal(back.size, n);
    for (const name of back.keys()) assert.match(name, /^bundle-2026-01-01T00-00-00\/[\w./-]+$/);
    const out = path.join(dir, "out");
    extract(out, back);
    assert.deepEqual(validateBundle(path.join(out, "bundle-2026-01-01T00-00-00")), []);
    // a leak in what comes back is refused by the same validator
    fs.writeFileSync(path.join(bundle, "client.json.bak"), "Authorization: Bearer abcdef1234567890abcdef\n");
    assert.notDeepEqual(validateBundle(bundle), []);
  } finally { fs.rmSync(dir, { recursive: true, force: true }); }
});

test("npm on the first run does not get the sign-in; the run is ok only when the zip is on disk", () => {
  const env = withoutSignIn({ PATH: "/bin", QUASAR_E2E_EMAIL: "a", QUASAR_E2E_PASSWORD: "b", QUASAR_E2E_CRED_FILE: "c", NODE_TLS_REJECT_UNAUTHORIZED: "0" });
  assert.deepEqual(env, { PATH: "/bin" });
  const dir = tmp();
  try {
    const bundle = path.join(dir, "bundle-x");
    fs.cpSync(path.join(HERE, "fixtures/bundle"), bundle, { recursive: true });
    assert.equal(sendBack(bundle, path.join(dir, "a.zip")), null);
    assert.equal(sendBack(bundle, path.join(dir, "b.zip"), () => {}), "no zip was written");
    assert.match(sendBack(bundle, path.join(dir, "c.zip"), () => { throw new Error("disk full"); }), /could not be zipped \(disk full\)/);
  } finally { fs.rmSync(dir, { recursive: true, force: true }); }
});

test("a scenario or plan git does not track stops the pack", () => {
  const stray = path.join(HERE, "plans", "zz-untracked-test.json");
  fs.writeFileSync(stray, '{"name":"zz","scenarios":[]}\n');
  try { assert.throws(() => packMembers(), /zz-untracked-test\.json is not tracked by git/); } finally { fs.rmSync(stray, { force: true }); }
  assert.ok(packMembers().length > 10);
});

test("the pack holds only tracked files: an untracked or ignored one, a launcher, or a git that cannot answer stops it (#1408)", () => {
  const dir = tmp();
  const git = (...a) => execFileSync("git", ["-c", "commit.gpgsign=false", "-c", "user.name=t", "-c", "user.email=t@example.invalid", ...a], { cwd: dir, stdio: "ignore" });
  try {
    // A copy of what the pack reads, as its own repository.
    const tracked = execFileSync("git", ["ls-files", "-z", "scripts/validate", "docs/session-trace"], { cwd: ROOT, encoding: "utf8" }).split("\0").filter(Boolean);
    for (const f of tracked) { fs.mkdirSync(path.dirname(path.join(dir, f)), { recursive: true }); fs.copyFileSync(path.join(ROOT, f), path.join(dir, f)); }
    git("init", "-q");
    git("add", "-A");
    git("commit", "-q", "-m", "x");
    assert.ok(packMembers(dir).length > 10, "a clean copy packs");
    const plans = path.join(dir, "scripts/validate/stream/plans");
    // ignored: --exclude-standard used to wave it through
    fs.writeFileSync(path.join(dir, ".gitignore"), "zz-*.json\n");
    fs.writeFileSync(path.join(plans, "zz-ignored.json"), '{"name":"zz","scenarios":[]}\n');
    assert.throws(() => packMembers(dir), /zz-ignored\.json is not tracked by git/);
    fs.rmSync(path.join(plans, "zz-ignored.json"));
    // a launcher that is not tracked rides in under a clean VERSION otherwise
    git("rm", "-q", "--cached", "scripts/validate/stream/pack/run.cmd");
    assert.throws(() => packMembers(dir), /pack\/run\.cmd is not tracked by git/);
    // no git answer is no guard
    fs.rmSync(path.join(dir, ".git"), { recursive: true });
    assert.throws(() => packMembers(dir), /cannot tell which files git tracks/);
    assert.throws(() => requireTracked(dir, ["a"]), /cannot tell which files git tracks/);
  } finally { fs.rmSync(dir, { recursive: true, force: true }); }
});

test("the sign-in is dropped from this process's environment but the TLS switch stays for its own checks (#1408)", () => {
  const env = { PATH: "/bin", QUASAR_E2E_EMAIL: "a", QUASAR_E2E_PASSWORD: "b", QUASAR_E2E_CRED_FILE: "c", NODE_TLS_REJECT_UNAUTHORIZED: "0" };
  assert.deepEqual(dropSignIn(env), { PATH: "/bin", NODE_TLS_REJECT_UNAUTHORIZED: "0" });
});

test("a symlink in a bundle is not zipped", () => {
  const dir = tmp();
  try {
    fs.mkdirSync(path.join(dir, "b"));
    fs.symlinkSync("/etc/passwd", path.join(dir, "b", "x"));
    assert.throws(() => zipBundle(path.join(dir, "b"), path.join(dir, "z.zip")), /not a plain file/);
  } finally { fs.rmSync(dir, { recursive: true, force: true }); }
});

test("a pack names its commit: the runner reads VERSION, not a git history it does not have", () => {
  const dir = tmp();
  try {
    fs.writeFileSync(path.join(dir, "VERSION"), `${SHA}\n`);
    assert.deepEqual(runnerState(dir), { commit: SHA, dirty: false });
    fs.writeFileSync(path.join(dir, "VERSION"), `${SHA} dirty\n`);
    assert.deepEqual(runnerState(dir), { commit: SHA, dirty: true });
    fs.writeFileSync(path.join(dir, "VERSION"), "unknown\n");
    assert.deepEqual(runnerState(dir), { commit: "unknown", dirty: false });
  } finally { fs.rmSync(dir, { recursive: true, force: true }); }
});

test("a Mac or Windows desktop is not headless; a Linux box is only without a display; QUASAR_E2E_HEADLESS overrides", () => {
  assert.equal(headless({}, "darwin"), false);
  assert.equal(headless({}, "win32"), false);
  assert.equal(headless({}, "linux"), true);
  assert.equal(headless({ DISPLAY: ":0" }, "linux"), false);
  assert.equal(headless({ WAYLAND_DISPLAY: "w" }, "linux"), false);
  assert.equal(headless({ QUASAR_E2E_HEADLESS: "1" }, "darwin"), true);
  assert.equal(headless({ QUASAR_E2E_HEADLESS: "1" }, "win32"), true);
  assert.equal(headless({ QUASAR_E2E_HEADLESS: "1", DISPLAY: ":0" }, "linux"), true);
  assert.equal(headless({ QUASAR_E2E_HEADLESS: "0" }, "linux"), false);
});

test("archive name limit counts UTF-8 bytes (100 bytes), not JS characters (#1280)", () => {
  const overBytes = "a".repeat(99) + "é"; // 100 chars, 101 UTF-8 bytes
  assert.equal(overBytes.length, 100);
  assert.equal(Buffer.byteLength(overBytes, "utf8"), 101);
  assert.throws(() => checkName(overBytes), /unsafe archive member name/);
  assert.throws(() => tarGz([{ name: overBytes, data: Buffer.alloc(0) }]), /unsafe archive member name/);
  assert.throws(() => zip([{ name: overBytes, data: Buffer.alloc(0) }]), /unsafe archive member name/);
  assert.throws(() => checkName("é".repeat(51)), /unsafe archive member name/);
  const exact = "é".repeat(50); // 50 chars, 100 UTF-8 bytes
  assert.equal(Buffer.byteLength(exact, "utf8"), 100);
  assert.equal(checkName(exact), exact);
  assert.ok(tarGz([{ name: exact, data: Buffer.from("ok") }]).length > 0);
});

test("default pack archives written to the repo root are git-ignored (#1280)", () => {
  for (const name of ["quasar-e2e-pack-01234567.tar.gz", "quasar-e2e-pack-01234567.zip"]) {
    const r = spawnSync("git", ["check-ignore", "-q", name], { cwd: ROOT, encoding: "utf8" });
    assert.equal(r.status, 0, `${name} in repo root must be git-ignored (${r.stderr.trim()})`);
  }
});

test("Windows launcher run.cmd quotes forwarded arguments without unquoted %*, and README clarifies .\\run.cmd and doctor (#1280)", () => {
  const cmd = fs.readFileSync(path.join(HERE, "pack/run.cmd"), "utf8");
  assert.doesNotMatch(cmd, /%\*/, "run.cmd must not use unquoted %* (splits & in URLs)");
  assert.match(cmd, /pack-run\.mjs "%~1" "%~2" "%~3" "%~4"/, "run.cmd must quote every forwarded positional arg up to at least 4");
  for (const line of cmd.split(/\r?\n/)) {
    if (/node scripts\\validate\\stream\\pack-run\.mjs/.test(line)) {
      assert.doesNotMatch(line, /(?<!")%~[1-9]|%~[1-9](?!")/, `unquoted arg in: ${line}`);
    }
  }
  assert.doesNotMatch(cmd, /\([^)]*%ERRORLEVEL%[^)]*\)/s, "run.cmd must not expand %ERRORLEVEL% inside a parenthesized block");

  const readme = fs.readFileSync(path.join(HERE, "pack/README.txt"), "utf8");
  assert.doesNotMatch(readme, /(?<!\.\\)\brun\.cmd\b/, "README.txt must write .\\run.cmd for PowerShell");
  assert.match(readme, /3\.[^\n]*full test[^\n]*\.zip[\s\S]*doctor[^\n]*no \.zip/i, "README step 3 must clarify that doctor writes no .zip");
});

test("doctor flags signin.txt.txt when signin.txt is absent, and does not when signin.txt exists (#1280)", async () => {
  const dir = tmp();
  const stack = await fakeStack();
  try {
    fs.writeFileSync(path.join(dir, "signin.txt.txt"), "QUASAR_E2E_EMAIL=a\nQUASAR_E2E_PASSWORD=b\n");
    const d1 = await doctor({ target: stack.url, plan: "smoke", creds: {}, chrome: "/x/chrome", insecure: false, root: dir, deps: { loadChromium: fakeChromium() } });
    const s1 = d1.checks.find((c) => c.name === "sign-in");
    assert.equal(s1.ok, false);
    assert.match(s1.detail, /signin\.txt\.txt/);
    assert.match(s1.detail, /Notepad/i);

    fs.writeFileSync(path.join(dir, "signin.txt"), "");
    const d2 = await doctor({ target: stack.url, plan: "smoke", creds: {}, chrome: "/x/chrome", insecure: false, root: dir, deps: { loadChromium: fakeChromium() } });
    const s2 = d2.checks.find((c) => c.name === "sign-in");
    assert.equal(s2.ok, false);
    assert.doesNotMatch(s2.detail, /signin\.txt\.txt/);
  } finally { await stack.close(); fs.rmSync(dir, { recursive: true, force: true }); }
});

test("doctor reports which sign-in source was used without leaking credentials (#1280)", async () => {
  const dir = tmp();
  const stack = await fakeStack();
  const bad = await fakeStack({ login: 401 });
  try {
    fs.writeFileSync(path.join(dir, "signin.txt"), "QUASAR_E2E_EMAIL=file-user@example.test\nQUASAR_E2E_PASSWORD=file-secret-password\n");
    assert.equal(resolveSignIn({}, dir).source, "signin.txt");
    const over = resolveSignIn({ QUASAR_E2E_EMAIL: "env-user@example.test", QUASAR_E2E_PASSWORD: "env-secret-password" }, dir);
    assert.equal(over.source, "environment; signin.txt ignored");
    assert.equal(over.creds.email, "env-user@example.test");
    const credPath = path.join(dir, "other.creds");
    fs.writeFileSync(credPath, "QUASAR_E2E_EMAIL=other-user@example.test\nQUASAR_E2E_PASSWORD=other-secret-pass\n");
    const fromFile = resolveSignIn({ QUASAR_E2E_CRED_FILE: credPath }, dir);
    assert.equal(fromFile.source, "QUASAR_E2E_CRED_FILE; signin.txt ignored");
    assert.ok(!fromFile.source.includes(credPath), "must not leak local file path");

    const okRes = await doctor({ target: stack.url, plan: "smoke", creds: over.creds, credSource: over.source, chrome: "/x/chrome", insecure: false, root: dir, deps: { loadChromium: fakeChromium() } });
    const okLine = okRes.checks.find((c) => c.name === "sign-in");
    assert.equal(okLine.ok, true);
    assert.match(okLine.detail, /\(from environment; signin\.txt ignored\)/);
    assert.doesNotMatch(okLine.detail, /env-secret-password|env-user@example\.test/);

    const failRes = await doctor({ target: bad.url, plan: "smoke", creds: over.creds, credSource: over.source, chrome: "/x/chrome", insecure: false, root: dir, deps: { loadChromium: fakeChromium() } });
    const failLine = failRes.checks.find((c) => c.name === "sign-in");
    assert.equal(failLine.ok, false);
    assert.match(failLine.detail, /API sign-in failed \(401\) \(from environment; signin\.txt ignored\)/);
    assert.doesNotMatch(failLine.detail, /env-secret-password|env-user@example\.test/);
  } finally { await Promise.all([stack.close(), bad.close()]); fs.rmSync(dir, { recursive: true, force: true }); }
});

test("unpacked alone, the full run path runs a plan end-to-end against a fake stack and writes a valid bundle zip (#1280)", async () => {
  const dir = tmp();
  const stack = await fakeStack();
  try {
    extract(dir, unzip(zip(members)));
    const pack = path.join(dir, "quasar-e2e-pack");
    installFakePlaywrightCore(pack);
    const chrome = path.join(dir, "fake-chrome");
    fs.writeFileSync(chrome, "#!/bin/sh\nexit 0\n", { mode: 0o755 });
    const planFile = path.join(dir, "short-plan.json");
    fs.writeFileSync(planFile, JSON.stringify({ name: "short", scenarios: [{ scenario: "stream-hold", params: { transport: "webrtc", codec: "h264", profile: "1080p60", hold_s: 0.1 } }] }));
    fs.writeFileSync(path.join(pack, "signin.txt"), "QUASAR_E2E_EMAIL=tester-account@example.test\nQUASAR_E2E_PASSWORD=correct-horse-battery\n");

    const r = await run("bash", [path.join(pack, "run.sh"), stack.url, planFile], { cwd: dir, env: { PATH: process.env.PATH, CHROME: chrome } });
    assert.equal(r.status, 0, `stdout:\n${r.stdout}\nstderr:\n${r.stderr}`);
    assert.doesNotMatch(r.stdout + r.stderr, /ERR_MODULE_NOT_FOUND|Cannot find module/);
    assert.match(r.stdout, /ok {3}sign-in: accepted \(from signin\.txt\)/);
    const resMatch = /RESULT status=ok target=e2e-pack zip=(\S+) run=ok pass=1 fail=0 error=0 skipped=0/.exec(r.stdout);
    assert.ok(resMatch, `missing ok RESULT line in:\n${r.stdout}`);
    assert.deepEqual(stack.stopped, ["11111111-2222-3333-4444-555555555555"]);
    assert.doesNotMatch(r.stdout + r.stderr, /correct-horse-battery|tester-account@example\.test|ApiTok12345678|PageTok1234567890/);

    const zipPath = resMatch[1];
    assert.ok(fs.existsSync(zipPath), `expected zip on disk at ${zipPath}`);
    const out = path.join(dir, "unzipped");
    extract(out, unzip(fs.readFileSync(zipPath)));
    const [top] = fs.readdirSync(out);
    const unzippedBundle = path.join(out, top);
    assert.deepEqual(validateBundle(unzippedBundle), []);
    assert.equal(JSON.parse(fs.readFileSync(path.join(unzippedBundle, "manifest.json"), "utf8")).runner.commit, SHA);
    const sc = JSON.parse(fs.readFileSync(path.join(unzippedBundle, "01-stream-hold/scenario.json"), "utf8"));
    assert.equal(sc.status, "pass", JSON.stringify(sc));
  } finally { await stack.close(); fs.rmSync(dir, { recursive: true, force: true }); }
});

test("npm ci really spawns without the sign-in or NODE_TLS_REJECT_UNAUTHORIZED in its environment (#1280)", async () => {
  const dir = tmp();
  const stack = await fakeStack();
  try {
    extract(dir, unzip(zip(members)));
    const pack = path.join(dir, "quasar-e2e-pack");
    const bin = path.join(dir, "bin");
    fs.mkdirSync(bin);
    const dumpFile = path.join(dir, "npm-env.json");
    fs.writeFileSync(
      path.join(bin, "npm"),
      `#!${process.execPath}\nimport fs from "node:fs";\nfs.writeFileSync(process.env.NPM_ENV_DUMP, JSON.stringify({ argv: process.argv.slice(2), env: process.env }));\n`,
      { mode: 0o755 },
    );
    const credFile = path.join(dir, "creds.txt");
    fs.writeFileSync(credFile, "QUASAR_E2E_EMAIL=file-user@example.test\nQUASAR_E2E_PASSWORD=file-secret-password\n");
    const env = {
      PATH: `${bin}:${process.env.PATH}`,
      NPM_ENV_DUMP: dumpFile,
      CHROME: path.join(dir, "no-such-chrome"),
      QUASAR_E2E_EMAIL: "tester-account@example.test",
      QUASAR_E2E_PASSWORD: "correct-horse-battery",
      QUASAR_E2E_CRED_FILE: credFile,
      NODE_TLS_REJECT_UNAUTHORIZED: "0",
    };
    await run(process.execPath, [path.join(pack, "scripts/validate/stream/pack-run.mjs"), "doctor", stack.url], { cwd: pack, env });
    assert.ok(fs.existsSync(dumpFile), "fake npm was not spawned by ensureDeps");
    const dump = JSON.parse(fs.readFileSync(dumpFile, "utf8"));
    assert.deepEqual(dump.argv, ["ci", "--omit=dev", "--no-audit", "--no-fund"]);
    assert.equal(dump.env.NPM_ENV_DUMP, dumpFile);
    for (const k of ["QUASAR_E2E_EMAIL", "QUASAR_E2E_PASSWORD", "QUASAR_E2E_CRED_FILE", "NODE_TLS_REJECT_UNAUTHORIZED"]) {
      assert.equal(Object.hasOwn(dump.env, k), false, `npm ci environment must not contain ${k}`);
    }
  } finally { await stack.close(); fs.rmSync(dir, { recursive: true, force: true }); }
});

test("unpacked pack prefers its pinned playwright-core over an unrelated playwright above the folder (#1280)", async () => {
  const outer = tmp();
  const stack = await fakeStack();
  try {
    // Unrelated playwright in an ancestor folder above the unpacked pack: imports cleanly so .catch() cannot mask it, then fails on launch.
    const parentPw = path.join(outer, "node_modules/playwright");
    fs.mkdirSync(parentPw, { recursive: true });
    fs.writeFileSync(path.join(parentPw, "package.json"), JSON.stringify({ name: "playwright", version: "9.9.9", type: "module", exports: "./index.mjs" }));
    fs.writeFileSync(path.join(parentPw, "index.mjs"), 'export const chromium = { launch: async () => { throw new Error("unrelated parent playwright was loaded"); } };\n');

    const sub = path.join(outer, "sub");
    fs.mkdirSync(sub);
    extract(sub, unzip(zip(members)));
    const pack = path.join(sub, "quasar-e2e-pack");
    installFakePlaywrightCore(pack);
    const chrome = path.join(outer, "fake-chrome");
    fs.writeFileSync(chrome, "#!/bin/sh\nexit 0\n", { mode: 0o755 });
    const planFile = path.join(outer, "short-plan.json");
    fs.writeFileSync(planFile, JSON.stringify({ name: "short", scenarios: [{ scenario: "stream-hold", params: { transport: "webrtc", codec: "h264", profile: "1080p60", hold_s: 0.1 } }] }));
    fs.writeFileSync(path.join(pack, "signin.txt"), "QUASAR_E2E_EMAIL=tester-account@example.test\nQUASAR_E2E_PASSWORD=correct-horse-battery\n");

    const env = { PATH: process.env.PATH, CHROME: chrome };
    const doc = await run(process.execPath, [path.join(pack, "scripts/validate/stream/pack-run.mjs"), "doctor", stack.url, planFile], { cwd: pack, env });
    assert.equal(doc.status, 0, `doctor failed:\n${doc.stdout}\n${doc.stderr}`);
    assert.doesNotMatch(doc.stdout + doc.stderr, /unrelated parent playwright/);
    const full = await run(process.execPath, [path.join(pack, "scripts/validate/stream/pack-run.mjs"), stack.url, planFile], { cwd: pack, env });
    assert.equal(full.status, 0, `full run failed:\n${full.stdout}\n${full.stderr}`);
    assert.doesNotMatch(full.stdout + full.stderr, /unrelated parent playwright/);
  } finally { await stack.close(); fs.rmSync(outer, { recursive: true, force: true }); }
});

test("when the runner exits 2 after writing a bundle because the browser would not close, pack reports browser-close instead of no-bundle (#1280)", async () => {
  assert.deepEqual(summarise({ counts: { pass: 1, fail: 0, error: 0, skipped: 0 }, errors: [], closeFailed: new Error("hung") }), { status: "error", code: 2, more: " reason=browser-close" });
  assert.equal(runnerFailureReason(2, false, ""), "no-bundle");
  assert.equal(runnerFailureReason(2, true, "RESULT status=error target=e2e-run bundle=/b pass=1 fail=0 error=0 skipped=0 reason=browser-close\n"), "browser-close");
  assert.equal(runnerFailureReason(2, true, "RESULT status=error target=e2e-run bundle=/b pass=1 fail=0 error=0 skipped=0\n"), "runner-error");
  assert.equal(runnerFailureReason(0, true, ""), null);
  assert.equal(runnerFailureReason(1, true, ""), null);

  const dir = tmp();
  const stack = await fakeStack();
  try {
    extract(dir, unzip(zip(members)));
    const pack = path.join(dir, "quasar-e2e-pack");
    installFakePlaywrightCore(pack);
    const chrome = path.join(dir, "fake-chrome");
    fs.writeFileSync(chrome, "#!/bin/sh\nexit 0\n", { mode: 0o755 });
    const planFile = path.join(dir, "short-plan.json");
    fs.writeFileSync(planFile, JSON.stringify({ name: "short", scenarios: [{ scenario: "stream-hold", params: { transport: "webrtc", codec: "h264", profile: "1080p60", hold_s: 0.1 } }] }));
    fs.writeFileSync(path.join(pack, "signin.txt"), "QUASAR_E2E_EMAIL=tester-account@example.test\nQUASAR_E2E_PASSWORD=correct-horse-battery\n");

    const r = await run(process.execPath, [path.join(pack, "scripts/validate/stream/pack-run.mjs"), stack.url, planFile], { cwd: pack, env: { PATH: process.env.PATH, CHROME: chrome, FAKE_CLOSE_FAIL: "1" } });
    assert.equal(r.status, 1, `stdout:\n${r.stdout}\nstderr:\n${r.stderr}`);
    assert.match(r.stdout, /RESULT status=failed target=e2e-pack zip= reason=browser-close run=error pass=1 fail=0 error=0 skipped=0/);
    assert.doesNotMatch(r.stdout, /reason=no-bundle/);
  } finally { await stack.close(); fs.rmSync(dir, { recursive: true, force: true }); }
});
