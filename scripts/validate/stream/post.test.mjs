// node --test scripts/validate/stream/post.test.mjs   (make verify runs it)
//
// Offline: the spawn that runs bench_submit.py is injected and only records argv. One test
// drives the real bench_submit.py with --dry-run, which contacts no service.
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";
import { postBundle } from "./post.mjs";

const HERE = path.dirname(fileURLToPath(import.meta.url));
const ROOT = path.resolve(HERE, "../../..");
const SC = "01-stream-hold";
const DX = path.join(ROOT, "scripts/dx");
const HOST = "gpu-test";
const TMP = fs.mkdtempSync(path.join(os.tmpdir(), "e2e-post-test-"));
process.on("exit", () => fs.rmSync(TMP, { recursive: true, force: true }));
// No bench server and no operator config: even the real script must not reach a service.
const ENV = { ...process.env, BENCH_URL: "", BENCH_KEY: "", XDG_CONFIG_HOME: TMP };

// The committed fixture, with the DX suite's own bench run directory in the scenario, so
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

/** A spawn that records every argv and answers `status`; nothing is run. */
function recorder(status = 0) {
  const calls = [];
  const spawn = (cmd, args) => {
    calls.push({ cmd, args });
    return { status, stdout: "", stderr: "" };
  };
  return { calls, spawn, submitted: () => calls.filter((c) => c.cmd === "python3") };
}
const after = (args, flag) => args[args.indexOf(flag) + 1];
const valuesOf = (args, flag) => args.flatMap((a, i) => (args[i - 1] === flag ? [a] : []));

test("a pass scenario is one run with the bundle's build, tags and artifacts", () => {
  const dir = bundle();
  const rec = recorder();
  const out = postBundle(dir, { host: HOST, spawn: rec.spawn });
  assert.equal(out.exit, 0);
  assert.equal(out.status, "ok");
  assert.deepEqual([out.posted, out.skipped, out.failed, out.leftOut], [1, 0, 0, 0]);

  const [call] = rec.submitted();
  const a = call.args;
  assert.equal(call.cmd, "python3");
  assert.equal(a[0], path.join(DX, "bench_submit.py"));
  assert.equal(after(a, "--suite"), "e2e");
  assert.equal(after(a, "--scenario"), "stream-hold");
  assert.equal(after(a, "--host"), "gpu-test@mac-test-client");
  assert.equal(after(a, "--repo"), "accreleus/quasar");
  assert.equal(after(a, "--commit"), "9e3c207f");
  assert.equal(after(a, "--verdict"), "PASS");
  assert.equal(after(a, "--ext-id-salt"), "fixture-0001");
  const tags = valuesOf(a, "--tag");
  for (const t of ["clock=raw", "transport=webrtc", "codec=h264", "profile=1080p60", "suite_run=fixture-0001", "client=mac-test-client", "git_quasar=9e3c207f"]) {
    assert.ok(tags.includes(t), `${t} is missing from ${tags.join(" ")}`);
  }
  const artifacts = valuesOf(a, "--artifact");
  assert.ok(artifacts.some((p) => p.endsWith(path.join(SC, "scenario.json"))));
  assert.ok(artifacts.some((p) => p.endsWith(path.join(SC, "timeline.jsonl"))));
});

test("the shots and the bundle archive ride on the first run only", () => {
  const dir = bundle();
  const SC2 = "02-stream-hold";
  fs.cpSync(path.join(dir, SC), path.join(dir, SC2), { recursive: true });
  edit(dir, `${SC2}/scenario.json`, (s) => ({ ...s, id: SC2, scenario: "stream-hold-b" }));
  edit(dir, "manifest.json", (m) => ({ ...m, scenarios: [SC, SC2] }));
  fs.mkdirSync(path.join(dir, SC, "shots"));
  fs.writeFileSync(path.join(dir, SC, "shots/first-frame.png"), "");

  const rec = recorder();
  const out = postBundle(dir, { host: HOST, spawn: rec.spawn });
  assert.equal(out.posted, 2);
  const calls = rec.submitted();
  assert.equal(calls.length, 2);
  const archives = calls.map((c) => valuesOf(c.args, "--artifact").filter((p) => p.endsWith(".tar.gz")));
  assert.equal(archives[0].length, 1);
  assert.equal(archives[1].length, 0);
  assert.ok(valuesOf(calls[0].args, "--artifact").some((p) => p.endsWith("shots/first-frame.png")));
});

test("a skipped scenario posts nothing and is counted", () => {
  const dir = bundle();
  edit(dir, `${SC}/scenario.json`, (s) => ({ ...s, status: "skipped", reason: "the client has no WebTransport" }));
  const rec = recorder();
  const out = postBundle(dir, { host: HOST, spawn: rec.spawn });
  assert.equal(out.exit, 0);
  assert.deepEqual([out.posted, out.skipped, out.failed], [0, 1, 0]);
  assert.equal(rec.calls.length, 0);
});

test("a failed scenario posts FAIL and is not withheld", () => {
  const dir = bundle();
  edit(dir, `${SC}/scenario.json`, (s) => ({ ...s, status: "fail", criteria: [{ id: "first_frame_s", pass: false, observed: 41, expected: "<= 20" }] }));
  const rec = recorder();
  const out = postBundle(dir, { host: HOST, spawn: rec.spawn });
  assert.equal(out.exit, 0);
  assert.equal(out.posted, 1);
  const a = rec.submitted()[0].args;
  assert.equal(after(a, "--verdict"), "FAIL");
  assert.ok(!valuesOf(a, "--tag").includes("status=error"));
});

test("an error scenario posts FAIL and carries status=error", () => {
  const dir = bundle();
  edit(dir, `${SC}/scenario.json`, (s) => ({ ...s, status: "error", reason: "the session ended during the hold" }));
  const rec = recorder();
  postBundle(dir, { host: HOST, spawn: rec.spawn });
  const a = rec.submitted()[0].args;
  assert.equal(after(a, "--verdict"), "FAIL");
  assert.ok(valuesOf(a, "--tag").includes("status=error"));
});

test("no node agent commit refuses the whole post, before anything runs", () => {
  const dir = bundle();
  edit(dir, "builds.json", (b) => ({
    ...b,
    components: { ...b.components, node_agent: null },
    unknown: { ...b.unknown, node_agent: "the host row has no source commit" },
  }));
  const rec = recorder();
  const out = postBundle(dir, { host: HOST, spawn: rec.spawn });
  assert.equal(out.exit, 1);
  assert.equal(out.reason, "no-commit");
  assert.equal(rec.calls.length, 0);
});

test("an invalid bundle is refused before anything is posted", () => {
  const dir = bundle();
  edit(dir, "manifest.json", (m) => ({ ...m, format: 2 }));
  const rec = recorder();
  const out = postBundle(dir, { host: HOST, spawn: rec.spawn });
  assert.equal(out.exit, 1);
  assert.equal(out.reason, "invalid-bundle");
  assert.equal(out.problems.length, 1);
  assert.equal(rec.calls.length, 0);
});

test("a hostile tag value is left out and counted", () => {
  const dir = bundle();
  edit(dir, `${SC}/scenario.json`, (s) => ({
    ...s,
    params: { ...s.params, transport: "webrtc ; rm -rf /", codec: "x".repeat(300) },
  }));
  const rec = recorder();
  const out = postBundle(dir, { host: HOST, spawn: rec.spawn });
  assert.equal(out.exit, 0);
  assert.equal(out.leftOut, 2);
  const tags = valuesOf(rec.submitted()[0].args, "--tag");
  assert.ok(!tags.some((t) => t.startsWith("transport=")));
  assert.ok(!tags.some((t) => t.startsWith("codec=")));
  assert.ok(tags.includes("profile=1080p60"));
  assert.ok(!rec.submitted()[0].args.join(" ").includes("rm -rf"));
});

test("the notes carry criterion ids and numbers only", () => {
  const dir = bundle();
  edit(dir, `${SC}/scenario.json`, (s) => ({
    ...s,
    criteria: [
      { id: "first_frame_s", pass: true, observed: "4.2\nEVIL", expected: "<= 20\nEVIL" },
      { id: "freeze_count", pass: true, observed: 0, expected: "0" },
      { id: "audio_latency_ms", pass: null, observed: null, expected: "<= 80\nEVIL" },
    ],
  }));
  const rec = recorder();
  postBundle(dir, { host: HOST, spawn: rec.spawn });
  const a = rec.submitted()[0].args;
  assert.equal(after(a, "--notes"), "first_frame_s pass ?\nfreeze_count pass 0\naudio_latency_ms not-judged null");
  assert.ok(!a.join(" ").includes("EVIL"));
});

test("a missing client label leaves the host as the role and is counted", () => {
  const dir = bundle();
  edit(dir, "client.json", ({ label, ...c }) => c);
  const rec = recorder();
  const out = postBundle(dir, { host: HOST, spawn: rec.spawn });
  assert.equal(out.leftOut, 1);
  const a = rec.submitted()[0].args;
  assert.equal(after(a, "--host"), HOST);
  assert.ok(!valuesOf(a, "--tag").some((t) => t.startsWith("client=")));
});

test("a post that fails is counted and the verb exits 1", () => {
  const rec = recorder(1);
  const out = postBundle(bundle(), { host: HOST, spawn: rec.spawn });
  assert.equal(out.exit, 1);
  assert.equal(out.status, "failed");
  assert.deepEqual([out.posted, out.failed], [0, 1]);
});

// The real script, twice, on the fixture: --dry-run contacts nothing, prints each run's plan
// and its external id, and posting twice must converge onto the same run.
test("the fixture plans through the real bench_submit.py, with the same external id twice", (t) => {
  if (spawnSync("python3", ["--version"], { encoding: "utf8" }).error) {
    t.skip("python3 is not installed; the real bench_submit.py cannot be driven");
    return;
  }
  const dir = bundle();
  const run = () => spawnSync(process.execPath, [path.join(HERE, "post.mjs"), dir, "--host", HOST, "--dry-run"], { env: ENV, encoding: "utf8" });
  const one = run();
  assert.equal(one.status, 0, one.stderr);
  assert.match(one.stdout, /RESULT status=ok target=e2e-post posted=1 skipped=0 dry_run=1/);
  assert.match(one.stdout, /dry run, no service contacted/);
  const id = (out) => out.stdout.match(/ext_id\s+([0-9a-f]{16})/)?.[1];
  assert.ok(id(one));
  const two = run();
  assert.equal(two.status, 0, two.stderr);
  assert.equal(id(two), id(one));
});
