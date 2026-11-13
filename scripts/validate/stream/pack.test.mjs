// node --test pack.test.mjs: the e2e pack (#1280). Offline: the archive is unpacked into an empty directory and its entry is run against a
// stack served in-process, so a file the closure missed is a module-not-found here and not on someone else's machine.
import assert from "node:assert/strict";
import fs from "node:fs";
import http from "node:http";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { spawn, spawnSync } from "node:child_process";
import zlib from "node:zlib";
import { fileURLToPath } from "node:url";
import { checkName, crc32, tarGz, zip } from "./archive.mjs";
import { validateBundle } from "./bundle.mjs";
import { closure, packMembers, playwrightFiles } from "./pack.mjs";
import { doctor, findChrome, parseCli, zipBundle } from "./pack-run.mjs";
import { headless, runnerState } from "./run.mjs";

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

/** A stack that answers: the page, the sign-in, the library. */
async function fakeStack({ apps = [{ name: "Quasar Benchapp" }], login = 200 } = {}) {
  const server = http.createServer((req, res) => {
    const json = (code, body) => { res.writeHead(code, { "content-type": "application/json" }); res.end(JSON.stringify(body)); };
    if (req.method === "POST" && req.url === "/v1/auth/login") return json(login, login === 200 ? { access_token: "tok-12345678" } : { error: "no" });
    if (req.url === "/v1/apps") return json(200, { items: apps });
    res.writeHead(200, { "content-type": "text/html" });
    res.end("<title>fake stack</title>");
  });
  await new Promise((r) => server.listen(0, "127.0.0.1", r));
  return { url: `http://127.0.0.1:${server.address().port}`, close: () => new Promise((r) => server.close(r)) };
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

test("a Mac or Windows desktop is not headless; a Linux box is only without a display", () => {
  assert.equal(headless({}, "darwin"), false);
  assert.equal(headless({}, "win32"), false);
  assert.equal(headless({}, "linux"), true);
  assert.equal(headless({ DISPLAY: ":0" }, "linux"), false);
  assert.equal(headless({ WAYLAND_DISPLAY: "w" }, "linux"), false);
});
