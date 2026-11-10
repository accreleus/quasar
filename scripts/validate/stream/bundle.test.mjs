// node --test scripts/validate/stream/   (make verify runs it)
import assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";
import { validateBundle } from "./bundle.mjs";

const HERE = path.dirname(fileURLToPath(import.meta.url));
const ROOT = path.resolve(HERE, "../../..");
const SC = "01-stream-hold";
const DX = path.join(ROOT, "scripts/dx");
const TMP = fs.mkdtempSync(path.join(os.tmpdir(), "e2e-bundle-"));
process.on("exit", () => fs.rmSync(TMP, { recursive: true, force: true }));
// No bench server and no operator config: the DX scripts must not reach a real service.
const ENV = { ...process.env, BENCH_URL: "", BENCH_KEY: "", XDG_CONFIG_HOME: TMP };

// The valid bundle: the committed fixture plus the DX suite's own bench run directory, so
// "a scenario directory is a bench_submit.py run directory" is true by construction.
function bundle() {
  const dir = fs.mkdtempSync(path.join(TMP, "b-"));
  fs.cpSync(path.join(HERE, "fixtures/bundle"), dir, { recursive: true });
  for (const f of ["metrics.jsonl", "trace.json", "session.json", "summary.json"]) {
    fs.copyFileSync(path.join(DX, "tests/fixtures/bench-run", f), path.join(dir, SC, f));
  }
  return dir;
}
const edit = (dir, rel, fn) => {
  const file = path.join(dir, rel);
  fs.writeFileSync(file, JSON.stringify(fn(JSON.parse(fs.readFileSync(file, "utf8")))));
};
const append = (dir, rel, row) => fs.appendFileSync(path.join(dir, rel), JSON.stringify(row) + "\n");

test("the fixture bundle is valid", () => {
  assert.deepEqual(validateBundle(bundle()), []);
});

test("a scenario directory is a bench_submit.py run directory", () => {
  const out = execFileSync("python3", [path.join(DX, "bench_submit.py"), "--dir", path.join(bundle(), SC), "--suite", "e2e-smoke", "--scenario", "stream-hold", "--dry-run"], { env: ENV, encoding: "utf8" });
  assert.match(out, /dry run, no service contacted/);
  assert.match(out, /RESULT status=ok target=bench-submit dry_run=1 samples=[1-9]/);
});

test("bench-mode windows folded by bench_app_samples.py are valid samples", () => {
  const dir = bundle();
  fs.cpSync(path.join(DX, "tests/fixtures/appfold"), path.join(dir, SC), { recursive: true });
  execFileSync("python3", [path.join(DX, "bench_app_samples.py"), "--dir", path.join(dir, SC), "--t0-ms", "1800000000000"], { env: ENV, stdio: "ignore" });
  assert.match(fs.readFileSync(path.join(dir, SC, "metrics.jsonl"), "utf8"), /bench_n/);
  assert.deepEqual(validateBundle(dir), []);
});

test("a file that is not an object is a problem line, not a pass and not a crash", () => {
  const dir = bundle();
  for (const f of ["manifest.json", "client.json", "builds.json", "network.json"]) fs.writeFileSync(path.join(dir, f), "null");
  fs.appendFileSync(path.join(dir, SC, "timeline.jsonl"), "null\n");
  assert.equal(validateBundle(dir).filter((e) => /not an object/.test(e)).length, 4);
  const d2 = bundle();
  fs.appendFileSync(path.join(d2, SC, "timeline.jsonl"), "null\n");
  assert.deepEqual(validateBundle(d2), [`${SC}/timeline.jsonl:4: not an object`]);
});

// One broken bundle per rule; each must fail on its own rule and on nothing else.
const RULES = [
  ["a format it does not know", (d) => edit(d, "manifest.json", (m) => ({ ...m, format: 2 })), /format must be 1/],
  ["no manifest", (d) => fs.rmSync(path.join(d, "manifest.json")), /manifest.json: missing/],
  ["a manifest with no plan", (d) => edit(d, "manifest.json", ({ plan, ...m }) => m), /manifest.json: no plan/],
  ["not_recorded that is not a list", (d) => edit(d, "manifest.json", (m) => ({ ...m, not_recorded: "audio_latency_ms" })), /not_recorded must be a list/],
  ["criteria that is not a list", (d) => edit(d, `${SC}/scenario.json`, (s) => ({ ...s, criteria: 5 })), /no criteria/],
  ["a scenario.json that is not an object", (d) => fs.writeFileSync(path.join(d, SC, "scenario.json"), "null"), /scenario.json: not an object/],
  ["a timeline row with no event", (d) => append(d, `${SC}/timeline.jsonl`, { ts_unix_ms: 1, clock: "client" }), /needs ts_unix_ms, clock \(client or host\) and ev/],
  ["a sample row that is not an object", (d) => fs.appendFileSync(path.join(d, SC, "metrics.jsonl"), "null\n"), /metrics.jsonl:\d+: not an object/],
  ["a link", (d) => fs.symlinkSync("/nonexistent/x.json", path.join(d, SC, "x.json")), /x.json: is not a regular file/],
  ["a token in a file with no extension", (d) => fs.writeFileSync(path.join(d, SC, "agent-stdout"), "Authorization: Bearer abcdefgh12345678\n"), /agent-stdout: holds a bearer token/],
  ["a password in a log", (d) => fs.writeFileSync(path.join(d, SC, "x.log"), "login password=hunter2\n"), /x.log: holds a credential/],
  ["a status outside the four", (d) => edit(d, `${SC}/scenario.json`, (s) => ({ ...s, status: "ok" })), /status must be one of/],
  ["scenarios that is not a list", (d) => edit(d, "manifest.json", (m) => ({ ...m, scenarios: SC })), /scenarios must be a list of directory names/],
  ["a scenario name that is a path", (d) => edit(d, "manifest.json", (m) => ({ ...m, scenarios: ["../x"] })), /scenarios must be a list of directory names/],
  ["an error with no reason", (d) => edit(d, `${SC}/scenario.json`, (s) => ({ ...s, status: "error" })), /error needs a reason/],
  ["no clock offset", (d) => edit(d, `${SC}/scenario.json`, ({ clock_offset_ms, ...s }) => s), /no clock_offset_ms/],
  ["a criterion with no expected value", (d) => edit(d, `${SC}/scenario.json`, (s) => ({ ...s, criteria: [{ id: "x", pass: true, observed: 1 }] })), /criterion x needs id, pass/],
  ["a skip with no reason", (d) => edit(d, `${SC}/scenario.json`, (s) => ({ ...s, status: "skipped" })), /skipped needs a reason/],
  ["a build that is null with no reason", (d) => edit(d, "builds.json", (b) => ({ ...b, unknown: { gpu_driver: b.unknown.gpu_driver } })), /web is null with no reason/],
  ["a key that is not in the metric manifest", (d) => append(d, `${SC}/metrics.jsonl`, { kind: "metric", source: "browser", ts_unix_ms: 1, metrics: { smoothness: 1 } }), /browser key smoothness is not in the metric manifest/],
  ["an agent key that is not in the metric manifest", (d) => append(d, `${SC}/metrics.jsonl`, { kind: "metric", source: "agent", ts_unix_ms: 1, metrics: { g2g_p50_ms: 1 } }), /agent key g2g_p50_ms is not in the metric manifest/],
  ["a sample of a fact declared not recorded", (d) => { edit(d, "manifest.json", (m) => ({ ...m, not_recorded: [{ fact: "battery_pct", why: "test" }] })); append(d, `${SC}/metrics.jsonl`, { kind: "metric", source: "harness", ts_unix_ms: 1, metrics: { battery_pct: 3 } }); }, /battery_pct has a value, and the manifest says it was not recorded/],
  ["a source quasar-bench would refuse", (d) => append(d, `${SC}/metrics.jsonl`, { kind: "metric", source: "client", ts_unix_ms: 1, metrics: {} }), /source must be one of/],
  ["a timestamp with no clock", (d) => append(d, `${SC}/timeline.jsonl`, { ts_unix_ms: 1, ev: "x" }), /needs ts_unix_ms, clock/],
  ["a 0 where the fact was not measured", (d) => edit(d, `${SC}/scenario.json`, (s) => ({ ...s, criteria: [{ id: "audio_latency_ms", pass: null, observed: 0, expected: "<= 80" }] })), /was not judged, so observed must be null/],
  ["a value for a fact declared not recorded", (d) => edit(d, `${SC}/scenario.json`, (s) => ({ ...s, criteria: [{ id: "audio_latency_ms", pass: true, observed: 0, expected: "<= 80" }] })), /the manifest says was not recorded/],
  ["an address", (d) => edit(d, "network.json", (n) => ({ ...n, host: "10.20.30.40" })), /network.json: holds an address/],
  ["a JWT", (d) => fs.writeFileSync(path.join(d, "note.txt"), "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.abc"), /note.txt: holds a JWT/],
  ["a credential", (d) => edit(d, "client.json", (c) => ({ ...c, password: "hunter2" })), /client.json: holds a credential/],
  ["a token", (d) => fs.writeFileSync(path.join(d, SC, "console.log"), "Authorization: Bearer abcdefgh12345678\n"), /console.log: holds a bearer token/],
];
for (const [name, breakIt, expected] of RULES) {
  test(`invalid: ${name}`, () => {
    const dir = bundle();
    breakIt(dir);
    const errors = validateBundle(dir);
    assert.equal(errors.length, 1, errors.join("\n"));
    assert.match(errors[0], expected);
  });
}

test("a browser version, a documentation address and a container's home are not leaks", () => {
  const dir = bundle();
  edit(dir, "network.json", (n) => ({ ...n, note: "Chrome/157.0.7339.81 via 192.0.2.5, logs in /home/quasar/.local/share/Steam", sig_token: null }));
  fs.writeFileSync(path.join(dir, "README.md"), "The Bearer authentication header is required.\n");
  fs.writeFileSync(path.join(dir, SC, "shot.png"), Buffer.from([0x89, 0x50, 0x4e, 0x47, 0, 0, 0, 0]));
  assert.deepEqual(validateBundle(dir), []);
});

test("the command: 0 valid, 1 not valid, 2 usage, and a RESULT line", () => {
  const run = (...args) => spawnSync(process.execPath, [path.join(HERE, "bundle.mjs"), ...args], { encoding: "utf8" });
  const ok = run(bundle());
  assert.equal(ok.status, 0);
  assert.match(ok.stdout, /RESULT status=ok target=e2e-bundle errors=0\n$/);
  const bad = bundle();
  edit(bad, "manifest.json", (m) => ({ ...m, format: 2 }));
  const no = run(bad);
  assert.equal(no.status, 1);
  assert.match(no.stderr, /manifest.json: format must be 1/);
  assert.match(no.stdout, /RESULT status=failed target=e2e-bundle errors=1\n$/);
  assert.equal(run().status, 2);
  assert.equal(run(path.join(bad, "manifest.json")).status, 2);
});
