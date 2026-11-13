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
import { main, postBundle } from "./post.mjs";

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

/** main() with its output captured: the RESULT line is what a wrapper reads. */
function cli(args, opts) {
  const lines = [];
  const { log, error } = console;
  console.log = (...a) => lines.push(a.join(" "));
  console.error = () => {};
  try {
    return { code: main(args, opts), out: lines.join("\n") };
  } finally {
    console.log = log;
    console.error = error;
  }
}

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
  assert.equal(after(a, "--scenario"), "stream-hold/transport=webrtc/codec=h264/profile=1080p60");
  assert.equal(after(a, "--host"), "gpu-test@mac-test-client");
  assert.equal(after(a, "--repo"), "accreleus/quasar");
  assert.equal(after(a, "--commit"), "9e3c207f");
  assert.equal(after(a, "--verdict"), "PASS");
  assert.equal(after(a, "--ext-id-salt"), "fixture-0001");
  const tags = valuesOf(a, "--tag");
  for (const t of ["clock=raw", "git_protocol=unknown", "transport=webrtc", "codec=h264", "profile=1080p60", "hold_s=240", "suite_run=fixture-0001", "client=mac-test-client", "git_quasar=9e3c207f"]) {
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

test("the same scenario with other params is another bench scenario (the server pairs by suite, scenario and host)", () => {
  const dir = bundle();
  const SC2 = "02-stream-hold";
  fs.cpSync(path.join(dir, SC), path.join(dir, SC2), { recursive: true });
  edit(dir, `${SC2}/scenario.json`, (s) => ({ ...s, id: SC2, params: { ...s.params, transport: "webtransport", codec: "pyrowave" } }));
  edit(dir, "manifest.json", (m) => ({ ...m, scenarios: [SC, SC2] }));
  const rec = recorder();
  assert.equal(postBundle(dir, { host: HOST, spawn: rec.spawn }).posted, 2);
  assert.deepEqual(rec.submitted().map((c) => after(c.args, "--scenario")), [
    "stream-hold/transport=webrtc/codec=h264/profile=1080p60",
    "stream-hold/transport=webtransport/codec=pyrowave/profile=1080p60",
  ]);
});

test("every param that changes what is measured is in the name, in a fixed order, and `hold_s` is only a tag", () => {
  const dir = bundle();
  // The fallback plan's two cases differ only in `when`: the bench must not compare them.
  edit(dir, `${SC}/scenario.json`, (s) => ({
    ...s,
    scenario: "transport-fallback",
    params: { when: "connect", hold_s: 60, codec: "h264", profile: "1080p60", transport: "webtransport", app: "Steam", bench: true, cover_qr: true },
  }));
  const rec = recorder();
  assert.equal(postBundle(dir, { host: HOST, spawn: rec.spawn }).posted, 1);
  const a = rec.submitted()[0].args;
  assert.equal(after(a, "--scenario"), "transport-fallback/transport=webtransport/codec=h264/profile=1080p60/when=connect");
  const tags = valuesOf(a, "--tag");
  for (const t of ["hold_s=60", "transport=webtransport", "codec=h264", "profile=1080p60"]) assert.ok(tags.includes(t), t);
  for (const t of ["app=", "bench=", "cover_qr=", "when="]) assert.ok(!tags.some((x) => x.startsWith(t)), t);
});

test("a scenario name holding a dot cannot collide with another scenario's params", () => {
  const dir = bundle();
  const SC2 = "02-stream-hold";
  fs.cpSync(path.join(dir, SC), path.join(dir, SC2), { recursive: true });
  // The old dotted form gave this pair one name: `stream-hold.webrtc.h264.1080p60`.
  edit(dir, `${SC}/scenario.json`, (s) => ({ ...s, scenario: "stream-hold.webrtc", params: { hold_s: 240 } }));
  edit(dir, `${SC2}/scenario.json`, (s) => ({ ...s, id: SC2, scenario: "stream-hold", params: { transport: "webrtc", hold_s: 240 } }));
  edit(dir, "manifest.json", (m) => ({ ...m, scenarios: [SC, SC2] }));
  const rec = recorder();
  assert.equal(postBundle(dir, { host: HOST, spawn: rec.spawn }).posted, 2);
  const names = rec.submitted().map((c) => after(c.args, "--scenario"));
  assert.deepEqual(names, ["stream-hold.webrtc", "stream-hold/transport=webrtc"]);
});

test("a numeric or boolean param is used; an over-long name is refused, never truncated", () => {
  const dir = bundle();
  edit(dir, `${SC}/scenario.json`, (s) => ({ ...s, params: { ...s.params, retries: 3, strict: true, skipped: null } }));
  const rec = recorder();
  const out = postBundle(dir, { host: HOST, spawn: rec.spawn });
  assert.equal(out.leftOut, 1); // `null` is not a shape a value has
  assert.equal(after(rec.submitted()[0].args, "--scenario"), "stream-hold/transport=webrtc/codec=h264/profile=1080p60/retries=3/strict=true");

  const long = bundle();
  const many = {};
  for (let i = 0; i < 4; i += 1) many[`p${i}`] = "x".repeat(60);
  edit(long, `${SC}/scenario.json`, (s) => ({ ...s, params: many }));
  const rec2 = recorder();
  const out2 = postBundle(long, { host: HOST, spawn: rec2.spawn });
  assert.deepEqual([out2.status, out2.exit, out2.posted, out2.failed], ["failed", 1, 0, 1]);
  assert.equal(rec2.calls.length, 0); // the archive is not made for a scenario that is refused
});

test("A1: a scenario whose name is not a name posts nothing; the directory name is never a stand-in", () => {
  for (const scenario of ["stream hold", undefined, 7]) {
    const dir = bundle();
    edit(dir, `${SC}/scenario.json`, (s) => ({ ...s, scenario }));
    const rec = recorder();
    const out = postBundle(dir, { host: HOST, spawn: rec.spawn });
    assert.deepEqual([out.status, out.posted, out.failed], ["failed", 0, 1], String(scenario));
    assert.equal(rec.submitted().length, 0);
  }
});

test("A2: session.json's id must be one of the scenario's sessions, and its fields plain names", () => {
  const cases = [
    (s) => ({ ...s, id: "not-a-uuid\nFORGED" }),
    (s) => ({ ...s, id: "99999999-2222-3333-4444-555555555555" }),
    (s) => ({ session: s }),
    (s) => ({ ...s, codec: "h264 x" }),
    (s) => ({ ...s, profile: "a;b" }),
    (s) => ({ ...s, host_id: "x".repeat(300) }),
    // What bench_submit.py copies into conditions.negotiated is never free text either.
    (s) => ({ ...s, encoder: "vulkan ; rm -rf /" }),
    (s) => ({ ...s, encoder: "x".repeat(65) }),
    (s) => ({ ...s, encoder: 7 }),
    (s) => ({ ...s, profile_id: "a b" }),
    (s) => ({ ...s, profile_id: ["1080p60"] }),
  ];
  for (const [i, fn] of cases.entries()) {
    const dir = bundle();
    edit(dir, `${SC}/session.json`, fn);
    const rec = recorder();
    const out = postBundle(dir, { host: HOST, spawn: rec.spawn });
    assert.deepEqual([out.status, out.posted, out.failed], ["failed", 0, 1], `case ${i}`);
    assert.equal(rec.submitted().length, 0, `case ${i}`);
  }
  // The id is compared without case, as the collector does, but it is posted as written and
  // that is another external id: an upper-case id would fork a second run, so it is refused.
  const dir = bundle();
  edit(dir, `${SC}/session.json`, (s) => ({ ...s, id: "aaaa1111-2222-3333-4444-bbbbccccdddd".toUpperCase() }));
  edit(dir, `${SC}/scenario.json`, (s) => ({ ...s, sessions: ["aaaa1111-2222-3333-4444-bbbbccccdddd"] }));
  const rec = recorder();
  const out = postBundle(dir, { host: HOST, spawn: rec.spawn });
  assert.deepEqual([out.status, out.posted, out.failed], ["failed", 0, 1]);
  assert.equal(rec.submitted().length, 0);
  // The collector's lower-case form of that id is one of the scenario's sessions, and posts.
  const lower = bundle();
  edit(lower, `${SC}/session.json`, (s) => ({ ...s, id: "aaaa1111-2222-3333-4444-bbbbccccdddd" }));
  edit(lower, `${SC}/scenario.json`, (s) => ({ ...s, sessions: ["AAAA1111-2222-3333-4444-BBBBCCCCDDDD"] }));
  assert.equal(postBundle(lower, { host: HOST, spawn: recorder().spawn }).posted, 1);
  // A plain factory name and a launch profile's id are what a session.json may hold.
  const ok = bundle();
  edit(ok, `${SC}/session.json`, (s) => ({ ...s, encoder: "vulkanh264enc:0+1", profile_id: "11111111-2222-3333-4444-555555555555" }));
  assert.equal(postBundle(ok, { host: HOST, spawn: recorder().spawn }).posted, 1);
  const nulls = bundle();
  edit(nulls, `${SC}/session.json`, (s) => ({ ...s, encoder: null, profile_id: null }));
  assert.equal(postBundle(nulls, { host: HOST, spawn: recorder().spawn }).posted, 1);
});

test("A3: the archive holds the bundle's files, also through a link and an option-like name; without it nothing is posted", (t) => {
  if (spawnSync("tar", ["--version"]).status !== 0) return t.skip("no tar on PATH");
  const real = bundle();
  const odd = path.join(path.dirname(real), "--exclude=*");
  fs.renameSync(real, odd);
  const link = `${real}-link`;
  fs.symlinkSync(odd, link);
  for (const dir of [odd, link]) {
    const listed = [];
    const spawn = (cmd, args, o) => {
      if (cmd !== "tar") return { status: 0, stdout: "", stderr: "" };
      const r = spawnSync(cmd, args, o);
      listed.push(spawnSync("tar", ["-tzf", args[1]], { encoding: "utf8" }).stdout);
      return r;
    };
    assert.equal(postBundle(dir, { host: HOST, spawn }).posted, 1, dir);
    assert.match(listed[0], /manifest\.json/, dir);
  }
  // tar fails: the first run is not posted without its archive.
  const rec = recorder();
  const out = postBundle(odd, { host: HOST, spawn: (cmd, args) => (cmd === "tar" ? { status: 2, stdout: "", stderr: "" } : rec.spawn(cmd, args)) });
  assert.deepEqual([out.status, out.reason, out.posted], ["failed", "no-archive", 0]);
  assert.equal(rec.submitted().length, 0);
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
  const out = postBundle(bundle(), { host: HOST, spawn: (cmd, args) => (cmd === "tar" ? { status: 0 } : rec.spawn(cmd, args)) });
  assert.equal(out.exit, 1);
  assert.equal(out.status, "failed");
  assert.deepEqual([out.posted, out.failed], [0, 1]);
});

// bench_submit.py exit 3 is a run the service HAS, whose tags disagree with what the host did:
// it is evidence, not a lost post, so it is counted as posted, the archive does not ride on a
// later run, and the verb says so without failing.
test("bench_submit.py exit 3 counts as posted and warns (warn=N, exit 0)", () => {
  const dir = bundle();
  const SC2 = "02-stream-hold";
  fs.cpSync(path.join(dir, SC), path.join(dir, SC2), { recursive: true });
  edit(dir, `${SC2}/scenario.json`, (s) => ({ ...s, id: SC2, scenario: "stream-hold-b" }));
  edit(dir, "manifest.json", (m) => ({ ...m, scenarios: [SC, SC2] }));
  const calls = [];
  let posts = 0;
  const spawn = (cmd, args) => {
    calls.push({ cmd, args });
    if (cmd === "tar") return { status: 0, stdout: "", stderr: "" };
    posts += 1;
    return { status: posts === 1 ? 3 : 0, stdout: "", stderr: "" };
  };
  const { code, out } = cli([dir, "--host", HOST], { spawn });
  assert.equal(code, 0);
  assert.match(out, /RESULT status=ok target=e2e-post posted=2 skipped=0 warn=1/);
  const archives = calls
    .filter((c) => c.cmd === "python3")
    .map((c) => valuesOf(c.args, "--artifact").filter((p) => p.endsWith(".tar.gz")));
  assert.deepEqual(archives.map((a) => a.length), [1, 0]);
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

// The guide's warning: a plan needs the key too. `bench_submit.py` builds its client before it
// reads --dry-run, so an address with no key is refused before anything is contacted.
test("DRY_RUN=1 with a bench address and no key fails", (t) => {
  if (spawnSync("python3", ["--version"], { encoding: "utf8" }).error) {
    t.skip("python3 is not installed; the real bench_submit.py cannot be driven");
    return;
  }
  const dir = bundle();
  // A loopback port: nothing listens, and nothing is asked, because the key is refused first.
  const r = spawnSync(process.execPath, [path.join(HERE, "post.mjs"), dir, "--host", HOST, "--dry-run"], {
    env: { ...ENV, BENCH_URL: "http://127.0.0.1:1" },
    encoding: "utf8",
  });
  assert.equal(r.status, 1, r.stderr);
  assert.match(r.stdout, /RESULT status=failed target=e2e-post posted=0 skipped=0 failed=1 dry_run=1/);
  assert.match(r.stderr, /no API key/);
});
