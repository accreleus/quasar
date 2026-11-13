// node --test scripts/validate/stream/compare.test.mjs   (make verify runs it)
//
// `e2e-compare` reports what changed between two run bundles, and no bundle is trusted: the page
// carries bundle text in code spans only, the JSON holds only values in a known shape, nothing is
// written into a bundle and nothing is overwritten.
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";
import { MAX_CRITERIA, MAX_METRIC_KEYS, MAX_PAIRS, compare, renderCompare } from "./compare.mjs";
import { validateBundle } from "./bundle.mjs";

const HERE = path.dirname(fileURLToPath(import.meta.url));
const FIX = path.join(HERE, "fixtures/compare");
const TMP = fs.mkdtempSync(path.join(os.tmpdir(), "e2e-compare-"));
process.on("exit", () => fs.rmSync(TMP, { recursive: true, force: true }));

let n = 0;
/** A private copy of the two fixture bundles, so a case may break one without touching the repo. */
function pair() {
  n += 1;
  const dir = path.join(TMP, `case-${n}`);
  fs.cpSync(path.join(FIX, "a"), path.join(dir, "a"), { recursive: true });
  fs.cpSync(path.join(FIX, "b"), path.join(dir, "b"), { recursive: true });
  return { a: path.join(dir, "a"), b: path.join(dir, "b"), out: path.join(dir, "out") };
}
const run = (a, b, out) =>
  spawnSync(process.execPath, [path.join(HERE, "compare.mjs"), a, b, ...(out ? ["--out", out] : [])], { encoding: "utf8" });
const read = (dir, file) => fs.readFileSync(path.join(dir, file), "utf8");
const edit = (dir, rel, fn) => {
  const file = path.join(dir, rel);
  fs.writeFileSync(file, JSON.stringify(fn(JSON.parse(fs.readFileSync(file, "utf8"))), null, 2));
};
const golden = (file) => fs.readFileSync(path.join(FIX, file), "utf8");

/**
 * The lines the page's structure is made of: everything that is bundle text lives inside a code
 * span, so the same lines must come out whatever that text is. Sorted, so a value that legitimately
 * changes a row's class (and so its place in the table) is not read as a structural change, and
 * with every count collapsed, so a count that follows from the data is not one either.
 */
const structure = (md) =>
  md
    .split("\n")
    .filter((line) => /^(#|\||- )/.test(line))
    .map((line) => line.replace(/`[^`]*`/g, "`X`").replace(/\d+/g, "#"))
    .sort();

test("the fixture bundles compare to the committed golden files", () => {
  const { a, b, out } = pair();
  const r = run(a, b, out);
  assert.equal(r.status, 0, r.stderr);
  assert.match(r.stdout, /RESULT status=ok target=e2e-compare changed=6 scenarios=1 only_a=1 only_b=1 left_out=2\n$/);
  assert.equal(read(out, "COMPARE.md"), golden("COMPARE.md"));
  assert.equal(read(out, "compare.json"), golden("compare.json"));
});

test("the same bundles give the same bytes out, from any directory and twice over", () => {
  // No clock, no absolute path and no directory-listing order may reach either file: the other
  // copies of the same two bundles are in other directories, under the same names.
  const first = pair();
  assert.equal(run(first.a, first.b, first.out).status, 0);
  const second = pair();
  assert.equal(run(second.a, second.b, second.out).status, 0);
  const again = pair();
  const secondOut = path.join(path.dirname(again.out), "out-2");
  assert.equal(run(again.a, again.b, again.out).status, 0);
  assert.equal(run(again.a, again.b, secondOut).status, 0);
  for (const out of [second.out, again.out, secondOut]) {
    for (const file of ["COMPARE.md", "compare.json"]) {
      assert.equal(read(out, file), read(first.out, file), `${file} at ${out}`);
    }
  }
});

test("member order changes neither file", () => {
  const first = pair();
  assert.equal(run(first.a, first.b, first.out).status, 0);
  const second = pair();
  edit(second.a, "01-stream-hold/scenario.json", (s) => ({
    ...s,
    params: Object.fromEntries(Object.entries(s.params).reverse()),
    criteria: [...s.criteria].reverse(),
  }));
  edit(second.b, "01-stream-hold/scenario.json", (s) => ({ ...s, criteria: [...s.criteria].reverse() }));
  edit(second.a, "builds.json", (b) => ({ ...b, components: Object.fromEntries(Object.entries(b.components).reverse()) }));
  assert.equal(run(second.a, second.b, second.out).status, 0);
  for (const file of ["COMPARE.md", "compare.json"]) assert.equal(read(second.out, file), read(first.out, file), file);
});

test("a bundle that is not valid is refused and nothing is written", () => {
  const invalid = pair();
  edit(invalid.a, "manifest.json", (m) => ({ ...m, format: 2 }));
  const r = run(invalid.a, invalid.b, invalid.out);
  assert.equal(r.status, 1, r.stderr);
  assert.match(r.stderr, /manifest\.json: format must be 1/);
  assert.match(r.stdout, /RESULT status=failed target=e2e-compare problems=1 reason=invalid-a\n$/);
  assert.equal(fs.existsSync(invalid.out), false);

  const both = pair();
  edit(both.a, "client.json", (c) => ({ ...c, kind: "television" }));
  edit(both.b, "network.json", (doc) => ({ path: doc.path }));
  const two = run(both.a, both.b, both.out);
  assert.equal(two.status, 1);
  assert.match(two.stderr, /reason|client\.json/);
  assert.equal(fs.existsSync(both.out), false);
});

test("the comparison is never written into a bundle or over an existing file", () => {
  const { a, b, out } = pair();
  for (const [label, target] of [
    ["inside A", path.join(a, "compare")],
    ["inside B", path.join(b, "deeper", "compare")],
    ["A itself", a],
    ["B itself", b],
  ]) {
    const r = run(a, b, target);
    assert.equal(r.status, 2, `${label}: ${r.stdout}${r.stderr}`);
    assert.match(r.stdout, /RESULT status=error target=e2e-compare reason=out-inside-bundle\n$/);
    assert.equal(fs.existsSync(path.join(a, "compare")), false, label);
    assert.equal(fs.existsSync(path.join(b, "deeper")), false, label);
  }
  // An existing directory is never overwritten, and the earlier comparison survives the attempt.
  assert.equal(run(a, b, out).status, 0);
  const before = read(out, "COMPARE.md");
  const again = run(a, b, out);
  assert.equal(again.status, 2);
  assert.match(again.stdout, /RESULT status=error target=e2e-compare reason=out-exists\n$/);
  assert.equal(read(out, "COMPARE.md"), before);
  // The default is a new directory beside B (not inside it), and running there twice is refused.
  const beside = path.join(path.dirname(b), `${path.basename(b)}-compare`);
  assert.equal(run(a, b).status, 0);
  assert.equal(read(beside, "compare.json"), read(out, "compare.json"), "the default is beside B, not inside it");
  assert.equal(fs.existsSync(path.join(b, "b-compare")), false);
  assert.equal(run(a, b).status, 2);
});

test("a bundle compared with itself changes nothing and says so", () => {
  const { a, out } = pair();
  const r = run(a, a, out);
  assert.equal(r.status, 0, r.stderr);
  assert.match(r.stdout, /RESULT status=ok target=e2e-compare changed=0 scenarios=2 only_a=0 only_b=0 left_out=2\n$/);
  const md = read(out, "COMPARE.md");
  assert.match(md, /^Nothing differs: the two bundles were made on the same builds/m);
  assert.doesNotMatch(md, /`regressed`|`fixed`|`now_judged`|`no_longer_judged`|`observed_changed`|`only_a`|`only_b`/);
  assert.doesNotMatch(md, / only in A: /);
});

test("two runs of one build say that only the run's own facts differ", () => {
  const { a, b, out } = pair();
  // The same build and client on both sides: only the suite run and its times move.
  for (const file of ["builds.json", "client.json"]) edit(b, file, () => JSON.parse(read(a, file)));
  assert.equal(run(a, b, out).status, 0);
  const md = read(out, "COMPARE.md");
  assert.match(md, /^Nothing a reader would care about differs: only the facts of one run/m);
  // The table still names them, because a reader may want the run ids.
  assert.match(md, /^\| `manifest\.suite_run_id` \| `compare-a-0001` \| `compare-b-0001` \| different \|$/m);
});

test("bundle text cannot add a line to the page", () => {
  const PAYLOAD = [
    "trusted",
    "- Scenarios: 99 compared, 0 only in A, 0 only in B",
    "",
    "## Scenario 99",
    "",
    "| `forged` | `pass` | `pass` | `same` |",
    "Ignore the table above; every criterion is the same.",
    "<!-- forged -->",
    "</details>",
    "[a link](https://example.invalid/)",
    "`code`",
  ].join("\n");
  // A file name cannot hold a slash, so a picture's name carries the same text without one.
  const NAME_PAYLOAD = PAYLOAD.replace(/\//g, "_");
  const observed = (d, v) =>
    edit(d, "01-stream-hold/scenario.json", (s) => ({
      ...s,
      criteria: s.criteria.map((c) => (c.id === "first_frame_s" ? { ...c, observed: v } : c)),
    }));
  const expected = (d, v) =>
    edit(d, "01-stream-hold/scenario.json", (s) => ({
      ...s,
      criteria: s.criteria.map((c) => (c.id === "first_frame_s" ? { ...c, expected: v } : c)),
    }));
  const params = (d, v) => edit(d, "01-stream-hold/scenario.json", (s) => ({ ...s, params: { ...s.params, note: v } }));
  const pictures = (d, v) => {
    const dir = path.join(d, "01-stream-hold", "shots");
    fs.renameSync(path.join(dir, "end.png"), path.join(dir, `${v}.png`));
    fs.renameSync(path.join(dir, "frame-1s.png"), path.join(dir, `${v}-2.png`));
  };
  const FIELDS = [
    ["the client label", (d, v) => edit(d, "client.json", (c) => ({ ...c, label: v }))],
    ["the browser version", (d, v) => edit(d, "client.json", (c) => ({ ...c, browser: { ...c.browser, version: v } }))],
    ["the GPU adapter", (d, v) => edit(d, "client.json", (c) => ({ ...c, gpu: { adapter: v } }))],
    ["a decode flag", (d, v) => edit(d, "client.json", (c) => ({ ...c, decode: { h264: { supported: v, hardware: true } } }))],
    ["the plan name", (d, v) => edit(d, "manifest.json", (m) => ({ ...m, plan: { ...m.plan, name: v } }))],
    ["the suite run id", (d, v) => edit(d, "manifest.json", (m) => ({ ...m, suite_run_id: v }))],
    ["the runner commit", (d, v) => edit(d, "manifest.json", (m) => ({ ...m, runner: { ...m.runner, commit: v } }))],
    ["a build version", (d, v) => edit(d, "builds.json", (doc) => ({ ...doc, components: { ...doc.components, reference_app: { version: v } } }))],
    ["a criterion's observed value", observed],
    ["a criterion's expected value", expected],
    ["a param value", params],
    ["a picture name", pictures, NAME_PAYLOAD],
  ];
  for (const [label, apply, forgeable = PAYLOAD] of FIELDS) {
    const harmless = pair();
    apply(harmless.a, "trusted");
    apply(harmless.b, "trusted");
    assert.notEqual(run(harmless.a, harmless.b, harmless.out).status, 2, `${label}: the harmless bundles are valid`);
    const forged = pair();
    apply(forged.a, forgeable);
    apply(forged.b, forgeable);
    const r = run(forged.a, forged.b, forged.out);
    assert.notEqual(r.status, 2, `${label}: ${r.stderr}`);
    const md = read(forged.out, "COMPARE.md");
    assert.deepEqual(structure(md), structure(read(harmless.out, "COMPARE.md")), `${label} added structure to the page`);
    // And not one of the payload's own lines is a line of the page.
    for (const forgedLine of ["## Scenario 99", "- Scenarios: 99 compared, 0 only in A, 0 only in B", "| `forged` | `pass` | `pass` | `same` |", "<!-- forged -->"]) {
      assert.equal(md.split("\n").includes(forgedLine), false, `${label}: ${forgedLine}`);
    }
  }
});

test("a scenario name that is not a name cannot be a heading or a row", () => {
  // It has no trustworthy identity, so it is not paired at all: the page names the other side's
  // scenario in the one-bundle list, and the payload's own words open no line and form no row.
  const { a, b, out } = pair();
  const PAYLOAD = "## Scenario 99\n- Scenarios: 99 compared\n| `forged` | `pass` |";
  edit(a, "01-stream-hold/scenario.json", (s) => ({ ...s, scenario: PAYLOAD }));
  const r = run(a, b, out);
  assert.equal(r.status, 0, r.stderr);
  const md = read(out, "COMPARE.md");
  for (const forgedLine of ["## Scenario 99", "- Scenarios: 99 compared", "| `forged` | `pass` |"]) {
    assert.equal(md.split("\n").includes(forgedLine), false);
  }
  assert.equal(read(out, "compare.json").includes("Scenario 99"), false, "no free text in the JSON either");
  const json = JSON.parse(read(out, "compare.json"));
  // The unnamed scenario is counted, not paired: A's other scenario and both of B's are alone.
  assert.deepEqual(json.pairs, []);
  assert.equal(json.counts.only_a, 1);
  assert.equal(json.counts.only_b, 2);
  assert.ok(json.left_out >= 1, "the name it cannot trust is counted");
});

test("a value in no known shape is counted, and the page still shows it", () => {
  const { a, b, out } = pair();
  assert.equal(run(a, b, out).status, 0);
  const json = JSON.parse(read(out, "compare.json"));
  // The fixture's GPU adapter reads "apple m5 pro": a fact with a space in it, which no shape
  // holds, so compare.json leaves it out and counts it once per side.
  assert.deepEqual(json.runs.fields.find((f) => f.field === "client.gpu.adapter"), { field: "client.gpu.adapter", same: true });
  assert.equal(json.left_out, 2);
  assert.match(read(out, "COMPARE.md"), /^\| `client\.gpu\.adapter` \| `apple m5 pro` \| `apple m5 pro` \| same \|$/m);
  assert.match(read(out, "COMPARE.md"), /^- Left out of `compare\.json`: 2 value\(s\) fit no known shape\./m);
});

test("compare.json holds nothing but values in a known shape", () => {
  const { a, b, out } = pair();
  assert.equal(run(a, b, out).status, 0);
  const shapes = [
    /^[A-Za-z0-9_.-]{1,64}$/,
    /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i,
    /^[0-9a-f]{7,40}$/i,
    /^[A-Za-z0-9][A-Za-z0-9_.+-]{0,63}$/,
    /^[A-Za-z0-9_.\-+/=]{1,200}$/,
  ];
  const walk = (value, where) => {
    if (typeof value === "string") assert.ok(shapes.some((re) => re.test(value)), `${where}: ${JSON.stringify(value)} is free text`);
    else if (typeof value === "number") assert.ok(Number.isFinite(value), `${where}: ${value}`);
    else if (value === null || typeof value === "boolean") return;
    else if (Array.isArray(value)) value.forEach((v, i) => walk(v, `${where}[${i}]`));
    else if (typeof value === "object") for (const [k, v] of Object.entries(value)) walk(v, `${where}.${k}`);
    else assert.fail(`${where}: a ${typeof value}`);
  };
  walk(JSON.parse(read(out, "compare.json")), "compare.json");
});

test("the metric medians are read after the review's warm-up, each side on its own anchor", () => {
  const { a, b } = pair();
  // A first browser sample inside the warm-up whose fps would move the median if it counted.
  const write = (dir, base, anchor, rest) => {
    const rows = [{ kind: "metric", source: "browser", ts_unix_ms: base + 1000, metrics: { fps: anchor } }];
    for (let i = 1; i <= 10; i += 1) rows.push({ kind: "metric", source: "browser", ts_unix_ms: base + 20000 + i * 1000, metrics: { fps: rest } });
    fs.writeFileSync(path.join(dir, "01-stream-hold", "metrics.jsonl"), rows.map((r) => JSON.stringify(r) + "\n").join(""));
  };
  write(a, 1786940000000, 1, 60);
  write(b, 1786950000000, 1, 30);
  const fps = compare(a, b).pairs[0].metrics.compared.find((m) => m.key === "fps");
  assert.deepEqual([fps.median_a, fps.median_b, fps.delta, fps.change_pct, fps.samples_a], [60, 30, -30, -50, 10]);
  // A source the metric manifest does not name is not compared: an `app` row's `fps` is not the
  // manifest's browser `fps`, and the bundle stays valid with it there.
  fs.appendFileSync(
    path.join(b, "01-stream-hold", "metrics.jsonl"),
    JSON.stringify({ kind: "metric", source: "app", ts_unix_ms: 1786954000000, metrics: { fps: 5 } }) + "\n",
  );
  assert.deepEqual(validateBundle(b), [], "an app row carrying that key is still a valid bundle");
  assert.equal(compare(a, b).pairs[0].metrics.compared.some((m) => m.source === "app"), false);
});

test("the warm-up is cut on both sides or on neither: a side with no browser sample does not leave the other one cut alone", () => {
  const { a, b } = pair();
  const agent = [];
  for (let i = 1; i <= 7; i += 1) agent.push({ kind: "metric", source: "agent", ts_unix_ms: 1786940001000 + i * 1000, metrics: { encode_ms: 50 } });
  for (let i = 1; i <= 6; i += 1) agent.push({ kind: "metric", source: "agent", ts_unix_ms: 1786940030000 + i * 1000, metrics: { encode_ms: 5 } });
  const lines = (rows) => rows.map((r) => JSON.stringify(r) + "\n").join("");
  // A has a browser sample to anchor a warm-up on, B has none; the agent rows are the same on both.
  fs.writeFileSync(path.join(a, "01-stream-hold", "metrics.jsonl"), lines([{ kind: "metric", source: "browser", ts_unix_ms: 1786940001000, metrics: { fps: 60 } }, ...agent]));
  fs.writeFileSync(path.join(b, "01-stream-hold", "metrics.jsonl"), lines(agent));
  const cmp = compare(a, b);
  const enc = cmp.pairs[0].metrics.compared.find((m) => m.source === "agent" && m.key === "encode_ms");
  assert.deepEqual([enc.median_a, enc.median_b, enc.delta, enc.samples_a, enc.samples_b], [50, 50, 0, 13, 13]);
  assert.equal(cmp.pairs[0].warmup.applied, false);
});

test("two scenarios of one bundle with the same identity pair in plan order; none is dropped", () => {
  const { a, b } = pair();
  const LONG = "03-stream-hold-long";
  for (const dir of [a, b]) {
    // Both sides' second hold is A's first one with a longer hold: `hold_s` is not in the identity.
    fs.cpSync(path.join(a, "01-stream-hold"), path.join(dir, LONG), { recursive: true });
    edit(dir, `${LONG}/scenario.json`, (sc) => ({ ...sc, id: LONG, params: { ...sc.params, hold_s: 600 } }));
    edit(dir, "manifest.json", (m) => ({ ...m, scenarios: [...m.scenarios, LONG] }));
  }
  const before = compare(a, b);
  assert.deepEqual(validateBundle(a), []);
  assert.deepEqual(validateBundle(b), []);
  const long = before.pairs.find((p) => p.dir_a === LONG);
  assert.ok(long, "the second hold is compared");
  assert.deepEqual([long.dir_b, long.criteria_changed, before.counts.scenarios], [LONG, 0, 2]);
  // One criterion flips in B's second hold: it is counted, not lost behind the first.
  edit(b, `${LONG}/scenario.json`, (sc) => ({ ...sc, status: "fail", criteria: sc.criteria.map((c) => (c.pass === true && c.id === sc.criteria.find((x) => x.pass === true).id ? { ...c, pass: false } : c)) }));
  const after = compare(a, b);
  assert.equal(after.pairs.find((p) => p.dir_a === LONG).criteria_changed, 1);
  assert.equal(after.counts.criteria_changed, before.counts.criteria_changed + 1);
  // A second hold on one side only is "only in A", by its identity.
  fs.rmSync(path.join(b, LONG), { recursive: true });
  edit(b, "manifest.json", (m) => ({ ...m, scenarios: m.scenarios.filter((x) => x !== LONG) }));
  const lone = compare(a, b);
  assert.equal(lone.counts.scenarios, 1);
  assert.equal(lone.only_a.filter((id) => id.startsWith("stream-hold/")).length, 1);
});

test("a member named like something every object has is not read as a bundle value; no number written is infinite", () => {
  const { a, b } = pair();
  // B has a build component called `constructor`; A has none. It must read as absent on A, never as Object's own constructor.
  edit(b, "builds.json", (x) => ({ ...x, components: { ...x.components, constructor: { version: "1.0.0" } } }));
  const cmp = compare(a, b);
  const row = cmp.runs.fields.find((f) => f.field === "builds.constructor.version");
  assert.deepEqual([row.a, row.b, row.same], [null, "1.0.0", false]);
  assert.doesNotMatch(renderCompare(cmp, 0), /native code/);
  // A number too large to average or scale is no sample; a percent that overflows is absent. Every number written is finite.
  const rows = (fps, max) => Array.from({ length: 30 }, (_, i) => ({ kind: "metric", source: "browser", ts_unix_ms: 1786940000000 + i * 1000, metrics: { fps, present_interval_max_ms: max } }));
  const write = (dir, r) => fs.writeFileSync(path.join(dir, "01-stream-hold", "metrics.jsonl"), r.map((x) => JSON.stringify(x) + "\n").join(""));
  write(a, rows(1e308, 1e-300));
  write(b, rows(1e308, 1e100));
  const m = compare(a, b).pairs[0].metrics;
  assert.equal(m.compared.some((x) => x.key === "fps"), false);
  const tiny = m.compared.find((x) => x.key === "present_interval_max_ms");
  assert.equal(tiny.change_pct, null);
  for (const x of m.compared) for (const v of [x.median_a, x.median_b, x.delta]) assert.ok(Number.isFinite(v), `${x.key} ${v}`);
});

test("a key with too few samples is listed, not compared, and nothing is judged", () => {
  const { a, b, out } = pair();
  assert.equal(run(a, b, out).status, 0);
  const md = read(out, "COMPARE.md");
  // The fixture carries `missing_indices` on three rows a side.
  assert.match(md, /^Too few samples to compare \(fewer than 5 on a side\): `browser\.missing_indices` \(`3` and `3`\)\.$/m);
  assert.equal(md.split("\n").filter((l) => /^\| `browser\.missing_indices` \|/.test(l)).length, 0);
  assert.match(md, /One pair of runs is not evidence of a regression — quasar-bench's rule is at least two runs a side\.$/m);
});

test("more criteria than the page may hold is truncated, and says so", () => {
  const { a, b, out } = pair();
  const criteria = [];
  for (let i = 0; i < MAX_CRITERIA + 1; i += 1) {
    criteria.push({ id: `c${String(i).padStart(3, "0")}`, pass: true, observed: i, expected: ">= 0" });
  }
  for (const side of [a, b]) edit(side, "01-stream-hold/scenario.json", (s) => ({ ...s, criteria }));
  const r = run(a, b, out);
  assert.equal(r.status, 0, r.stderr);
  assert.match(r.stdout, /truncated=1/);
  const md = read(out, "COMPARE.md");
  assert.match(
    md,
    new RegExp(`^- Truncated: at most ${MAX_PAIRS} scenarios, ${MAX_CRITERIA} criteria and ${MAX_METRIC_KEYS} metric keys per scenario are compared\\.$`, "m"),
  );
  assert.equal(md.split("\n").filter((l) => /^\| `c\d{3}` \|/.test(l)).length, MAX_CRITERIA);
  assert.equal(JSON.parse(read(out, "compare.json")).pairs[0].criteria.length, MAX_CRITERIA);
});

test("compare() classifies every criterion id in either side, and the page reads the same", () => {
  const { a, b } = pair();
  const cmp = compare(a, b);
  assert.equal(cmp.pairs.length, 1);
  assert.equal(cmp.counts.criteria_changed, 6);
  assert.deepEqual(
    cmp.pairs[0].criteria.map((c) => [c.id, c.class]),
    [
      ["profile_as_asked", "only_a"],
      ["marker_decoded", "only_b"],
      ["transport_as_asked", "regressed"],
      ["missing_indices_rate", "now_judged"],
      ["codec_as_asked", "no_longer_judged"],
      ["first_frame_s", "observed_changed"],
      ["audio_latency_ms", "same"],
      ["freeze_count", "same"],
    ],
  );
  // The delta is B minus A, for a number that moved and only then.
  assert.equal(cmp.pairs[0].criteria.find((c) => c.id === "first_frame_s").delta, 2.6);
  assert.equal(cmp.pairs[0].criteria.find((c) => c.id === "transport_as_asked").delta, undefined);
  const md = renderCompare(cmp, 0);
  assert.match(md, /^\| `first_frame_s` \| `pass` `4\.2` \| `pass` `6\.8` \| `observed_changed` \|$/m);
  assert.match(md, /^\| `transport_as_asked` \| `pass` `webrtc` \| `fail` `webrtc` \| `regressed` \|$/m);
  assert.match(md, /^\| `profile_as_asked` \| `pass` `true` \| — \| `only_a` \|$/m);
  // Pictures: one the two runs took and that differs in its bytes, one they share, and the two
  // only one of them took.
  assert.match(md, /^\| `end\.png` \| no \| `\d+` \| `\d+` \|$/m);
  assert.match(md, /^\| `frame-1s\.png` \| yes \|/m);
  assert.match(md, /^Only in A: `only-a\.png`$/m);
  assert.match(md, /^Only in B: `only-b\.png`$/m);
  assert.match(md, /^Not compared pixel by pixel:/m);
  // And the unit's own line, last.
  assert.match(
    md,
    /^UI journeys in a plan, decoded-frame pairs at the same reference-app tick, and a pixel difference in pictures are not in this verb\.$/m,
  );
});

test("usage errors are one line and exit 2", () => {
  const { a, b } = pair();
  assert.equal(run(a).status, 2);
  const missing = run(a, path.join(TMP, "nope"));
  assert.equal(missing.status, 2);
  assert.match(missing.stdout, /RESULT status=error target=e2e-compare reason=not-a-directory\n$/);
  const extra = spawnSync(process.execPath, [path.join(HERE, "compare.mjs"), a, b, "extra"], { encoding: "utf8" });
  assert.equal(extra.status, 2);
  assert.match(extra.stdout, /reason=usage\n$/);
  const help = spawnSync(process.execPath, [path.join(HERE, "compare.mjs"), "--help"], { encoding: "utf8" });
  assert.equal(help.status, 0);
  assert.match(help.stdout, /usage: compare\.mjs/);
});
