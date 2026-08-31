// scripts/qa/assemble.test.mjs — node:test coverage of assemble.mjs's gate
// computation logic. Fixtures live in scripts/qa/testdata/raw/ (happy-path
// meta/profile/preflight/shutdown/run-N.json); scenario tests clone and
// mutate them in memory, following the pattern in report.test.mjs.
//
// Run: node --test scripts/qa/assemble.test.mjs
import test from "node:test";
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import path from "node:path";
import { buildReport } from "./assemble.mjs";
import { renderReport } from "./report.mjs";

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const rawDir = path.join(__dirname, "testdata", "raw");

async function loadRaw(name) {
  const raw = await readFile(path.join(rawDir, name), "utf8");
  return JSON.parse(raw);
}

async function loadFixtures() {
  const [meta, profile, preflight, shutdown, run1, run2, run3] = await Promise.all([
    loadRaw("meta.json"),
    loadRaw("profile.json"),
    loadRaw("preflight.json"),
    loadRaw("shutdown.json"),
    loadRaw("run-1.json"),
    loadRaw("run-2.json"),
    loadRaw("run-3.json"),
  ]);
  return { meta, profile, preflight, shutdown, runs: [run1, run2, run3] };
}

function clone(x) {
  return JSON.parse(JSON.stringify(x));
}

function gate(report, id) {
  return report.gates.find((g) => g.id === id);
}

// ---------------------------------------------------------------------------
// 1. happy path
// ---------------------------------------------------------------------------

test("happy path: 3 runs, all green, keyboard skipped -> overall PASS", async () => {
  const { meta, profile, preflight, shutdown, runs } = await loadFixtures();
  const report = buildReport({ meta, profile, runs, shutdown, preflight });

  assert.equal(report.summary.overall, "PASS");
  assert.equal(report.summary.fail, 0);
  assert.equal(report.summary.skipped, 1, "expected exactly one SKIPPED gate (keyboard)");
  assert.equal(report.summary.evidence, 1, "expected exactly one EVIDENCE gate (oracle)");

  assert.equal(gate(report, "preflight").verdict, "PASS");
  assert.equal(gate(report, "repoint").verdict, "PASS");
  assert.equal(gate(report, "launch").verdict, "PASS");
  assert.equal(gate(report, "oracle").verdict, "EVIDENCE");
  assert.equal(gate(report, "input-mouse").verdict, "PASS");
  assert.equal(gate(report, "input-gamepad").verdict, "PASS");
  assert.equal(gate(report, "input-keyboard").verdict, "SKIPPED");
  assert.equal(gate(report, "shutdown").verdict, "PASS");
  assert.equal(gate(report, "teardown").verdict, "PASS");

  // devices gated in stable alphabetical order: gamepad(4a) < keyboard(4b) < mouse(4c)
  assert.equal(gate(report, "input-gamepad").n, "4a");
  assert.equal(gate(report, "input-keyboard").n, "4b");
  assert.equal(gate(report, "input-mouse").n, "4c");

  const html = renderReport(report);
  for (const line of profile.gates.shutdown.require_logs) {
    assert.ok(html.includes(line), `expected rendered HTML to contain shutdown log line: "${line}"`);
  }
});

// ---------------------------------------------------------------------------
// 2. luma below min_luma
// ---------------------------------------------------------------------------

test("luma below min_luma -> gate 2 FAIL and overall FAIL", async () => {
  const { meta, profile, preflight, shutdown, runs } = await loadFixtures();
  const mutated = clone(runs);
  mutated[0].luma.mean = 10.0; // below profile min_luma of 25

  const report = buildReport({ meta, profile, runs: mutated, shutdown, preflight });
  assert.equal(gate(report, "launch").verdict, "FAIL");
  assert.match(gate(report, "launch").reason, /luma 10 < 25/);
  assert.equal(report.summary.overall, "FAIL");
});

// ---------------------------------------------------------------------------
// 3. first_content_s outside the budget
// ---------------------------------------------------------------------------

test("first_content_s outside budget -> gate 2 FAIL naming the budget", async () => {
  const { meta, profile, preflight, shutdown, runs } = await loadFixtures();
  const mutated = clone(runs);
  mutated[1].first_content_s = 5.0; // below profile's [30, 55] budget

  const report = buildReport({ meta, profile, runs: mutated, shutdown, preflight });
  const launch = gate(report, "launch");
  assert.equal(launch.verdict, "FAIL");
  assert.match(launch.reason, /first_content_s 5s outside budget \[30, 55\]/);
  assert.equal(report.summary.overall, "FAIL");
});

// ---------------------------------------------------------------------------
// 4. settle timeout -> affected input gates degrade to EVIDENCE, not FAIL
// ---------------------------------------------------------------------------

test("settle timeout on a run -> its input gates are EVIDENCE, not FAIL, with a note", async () => {
  const { meta, profile, preflight, shutdown, runs } = await loadFixtures();
  const mutated = clone(runs);
  // run 2 never settled, but still painted content within budget so gate 2 stays clean.
  mutated[1].settle = "timeout";

  const report = buildReport({ meta, profile, runs: mutated, shutdown, preflight });

  assert.equal(gate(report, "launch").verdict, "PASS", "gate 2 should not be affected by a settle timeout alone");

  const mouse = gate(report, "input-mouse");
  const gamepad = gate(report, "input-gamepad");
  assert.equal(mouse.verdict, "EVIDENCE");
  assert.notEqual(mouse.verdict, "FAIL");
  assert.equal(gamepad.verdict, "EVIDENCE");
  assert.notEqual(gamepad.verdict, "FAIL");
  assert.match(mouse.reason, /settle timed out/);

  const timeoutNote = report.notes.find((n) => /settle/i.test(n.title));
  assert.ok(timeoutNote, "expected a note explaining the settle timeout");
  assert.match(timeoutNote.body, /Run\(s\) 2/);
});

// ---------------------------------------------------------------------------
// 5. shutdown exit_code mismatch
// ---------------------------------------------------------------------------

test("shutdown exit_code 137 -> gate 5 FAIL quoting expected 143", async () => {
  const { meta, profile, preflight, shutdown, runs } = await loadFixtures();
  const mutatedShutdown = clone(shutdown);
  mutatedShutdown.attempts[0].exit_code = 137;

  const report = buildReport({ meta, profile, runs, shutdown: mutatedShutdown, preflight });
  const sd = gate(report, "shutdown");
  assert.equal(sd.verdict, "FAIL");
  assert.match(sd.reason, /exit_code 137 !== expected 143/);
  assert.equal(report.summary.overall, "FAIL");
});

// ---------------------------------------------------------------------------
// 6. missing require_logs line
// ---------------------------------------------------------------------------

test("missing a required shutdown log line -> gate 5 FAIL naming it", async () => {
  const { meta, profile, preflight, shutdown, runs } = await loadFixtures();
  const mutatedShutdown = clone(shutdown);
  mutatedShutdown.attempts[0].log = mutatedShutdown.attempts[0].log
    .split("\n")
    .filter((l) => !l.includes("terminating gamescope"))
    .join("\n");

  const report = buildReport({ meta, profile, runs, shutdown: mutatedShutdown, preflight });
  const sd = gate(report, "shutdown");
  assert.equal(sd.verdict, "FAIL");
  assert.match(sd.reason, /missing required log line\(s\): "terminating gamescope"/);
  assert.match(sd.detail, /missing required: "terminating gamescope"/);
});

// ---------------------------------------------------------------------------
// 7. probe decode_failed error
// ---------------------------------------------------------------------------

test("probe error (decode_failed) -> gate 2 FAIL with the probe's message surfaced", async () => {
  const { meta, profile, preflight, shutdown, runs } = await loadFixtures();
  const mutated = clone(runs);
  mutated[0] = {
    run: 1,
    error: "decode_failed",
    message: "video never decoded — H.264/ICE failure",
    decode_s: 45.0,
  };

  const report = buildReport({ meta, profile, runs: mutated, shutdown, preflight });
  const launch = gate(report, "launch");
  assert.equal(launch.verdict, "FAIL");
  assert.match(launch.reason, /decode_failed/);
  assert.match(launch.reason, /video never decoded — H\.264\/ICE failure/);
  assert.equal(report.summary.overall, "FAIL");
});

// ---------------------------------------------------------------------------
// 8. output renders through report.mjs unchanged and self-contained
// ---------------------------------------------------------------------------

test("report.json renders through report.mjs without throwing and is self-contained", async () => {
  const { meta, profile, preflight, shutdown, runs } = await loadFixtures();
  const report = buildReport({ meta, profile, runs, shutdown, preflight });

  // Round-trip through JSON, exactly as the CLI writes/reads it.
  const roundTripped = JSON.parse(JSON.stringify(report));
  const html = renderReport(roundTripped);

  assert.equal(typeof html, "string");
  assert.ok(html.startsWith("<!doctype html>"));
  assert.ok(!html.includes('src="shots/'));
  assert.ok(!/src="https?:\/\//.test(html));
  assert.ok(!/href="https?:\/\/[^"]*\.(css|js)"/.test(html));
  const imgSrcs = [...html.matchAll(/<img[^>]*\ssrc="([^"]*)"/g)].map((m) => m[1]);
  assert.ok(imgSrcs.length > 0, "expected at least one embedded screenshot");
  for (const src of imgSrcs) {
    assert.ok(src.startsWith("data:image/png;base64,"), `img src must be an inline data URI, got: ${src}`);
  }
});
