// node --test scripts/validate/stream/   (make verify runs it)
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";
import { validateBundle } from "./bundle.mjs";

const HERE = path.dirname(fileURLToPath(import.meta.url));
const ROOT = path.resolve(HERE, "../../..");
const SC = "01-stream-hold";

// The valid bundle: the committed fixture plus the DX suite's own bench run directory, so
// "a scenario directory is a bench_submit.py run directory" is true by construction.
function bundle() {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "e2e-bundle-"));
  fs.cpSync(path.join(HERE, "fixtures/bundle"), dir, { recursive: true });
  for (const f of ["metrics.jsonl", "trace.json", "session.json", "summary.json"]) {
    fs.copyFileSync(path.join(ROOT, "scripts/dx/tests/fixtures/bench-run", f), path.join(dir, SC, f));
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
  const out = execFileSync("python3", [path.join(ROOT, "scripts/dx/bench_submit.py"), "--dir", path.join(bundle(), SC), "--suite", "e2e-smoke", "--scenario", "stream-hold", "--dry-run"], { env: { ...process.env, BENCH_URL: "", BENCH_KEY: "" }, encoding: "utf8" });
  assert.match(out, /RESULT status=ok target=bench-submit dry_run=1 samples=[1-9]/);
});

// One broken bundle per rule; each must fail on its own rule and on nothing else.
const RULES = [
  ["a format it does not know", (d) => edit(d, "manifest.json", (m) => ({ ...m, format: 2 })), /format must be 1/],
  ["a status outside the four", (d) => edit(d, `${SC}/scenario.json`, (s) => ({ ...s, status: "ok" })), /status must be one of/],
  ["a skip with no reason", (d) => edit(d, `${SC}/scenario.json`, (s) => ({ ...s, status: "skipped" })), /skipped needs a reason/],
  ["a build that is null with no reason", (d) => edit(d, "builds.json", (b) => ({ ...b, unknown: { gpu_driver: b.unknown.gpu_driver } })), /web is null with no reason/],
  ["a key that is not in the metric manifest", (d) => append(d, `${SC}/metrics.jsonl`, { kind: "metric", source: "browser", ts_unix_ms: 1, metrics: { smoothness: 1 } }), /browser key smoothness is not in the metric manifest/],
  ["a source quasar-bench would refuse", (d) => append(d, `${SC}/metrics.jsonl`, { kind: "metric", source: "client", ts_unix_ms: 1, metrics: {} }), /source must be one of/],
  ["a timestamp with no clock", (d) => append(d, `${SC}/timeline.jsonl`, { ts_unix_ms: 1, ev: "x" }), /needs ts_unix_ms and clock/],
  ["a 0 where the fact was not measured", (d) => edit(d, `${SC}/scenario.json`, (s) => ({ ...s, criteria: [{ id: "audio_latency_ms", pass: null, observed: 0, expected: "<= 80" }] })), /was not judged, so observed must be null/],
  ["a value for a fact declared not recorded", (d) => edit(d, `${SC}/scenario.json`, (s) => ({ ...s, criteria: [{ id: "audio_latency_ms", pass: true, observed: 0, expected: "<= 80" }] })), /the manifest says was not recorded/],
  ["an address", (d) => edit(d, "network.json", (n) => ({ ...n, host: "10.20.30.40" })), /network.json: holds an address/],
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

test("a browser version and a documentation address are not addresses", () => {
  const dir = bundle();
  edit(dir, "network.json", (n) => ({ ...n, note: "Chrome/157.0.7339.81 via 192.0.2.5" }));
  assert.deepEqual(validateBundle(dir), []);
});
