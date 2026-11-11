// node --test scripts/validate/stream/review.test.mjs   (make verify runs it)
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";
import { validateBundle } from "./bundle.mjs";
import { ANOMALY_RULES, review, renderReview } from "./review.mjs";

const HERE = path.dirname(fileURLToPath(import.meta.url));
const ROOT = path.resolve(HERE, "../../..");
const SC = "01-stream-hold";
const DX = path.join(ROOT, "scripts/dx");
const GOLDEN = path.join(HERE, "fixtures/review");
const TMP = fs.mkdtempSync(path.join(os.tmpdir(), "e2e-review-"));
process.on("exit", () => fs.rmSync(TMP, { recursive: true, force: true }));

// The valid bundle bundle.test.mjs builds: the committed fixture plus the DX suite's
// own bench run directory, so the scenario carries real browser samples.
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
  fs.writeFileSync(file, JSON.stringify(fn(JSON.parse(fs.readFileSync(file, "utf8"))), null, 2));
};
const run = (dir) => spawnSync(process.execPath, [path.join(HERE, "review.mjs"), dir], { encoding: "utf8" });
const read = (dir, file) => fs.readFileSync(path.join(dir, "review", file), "utf8");
const golden = (file) => fs.readFileSync(path.join(GOLDEN, file), "utf8");
const metrics = (dir, rows, name = SC) =>
  fs.writeFileSync(path.join(dir, name, "metrics.jsonl"), rows.map((r) => JSON.stringify(r) + "\n").join(""));
const browser = (ts_unix_ms, m) => ({ kind: "metric", source: "browser", ts_unix_ms, metrics: m });
// The lines the page's structure is made of. Everything that is bundle text lives inside
// a code span, so the same lines must come out whatever that text is.
const structure = (md) =>
  md
    .split("\n")
    .filter((line) => /^(#|\||- )/.test(line))
    .map((line) => line.replace(/`[^`]*`/g, "`X`"));

test("the fixture bundle reviews to the committed golden files", () => {
  const dir = bundle();
  const out = run(dir);
  assert.equal(out.status, 0, out.stderr);
  assert.match(out.stdout, /RESULT status=ok target=e2e-review scenarios=1 pass=1 fail=0 error=0 skipped=0\n$/);
  assert.equal(read(dir, "REVIEW.md"), golden("REVIEW.md"));
  assert.equal(read(dir, "summary.json"), golden("summary.json"));
});

test("the same bundle and thresholds file give the same bytes out, and review/ leaves it a valid bundle", () => {
  const dir = bundle();
  assert.equal(run(dir).status, 0);
  const before = { md: read(dir, "REVIEW.md"), json: read(dir, "summary.json") };
  // A second bundle built the same way, and a second run over the first one: no clock,
  // no absolute path and no directory-listing order may reach the bytes.
  const again = bundle();
  assert.equal(run(again).status, 0);
  assert.equal(run(dir).status, 0);
  for (const other of [again, dir]) {
    assert.equal(read(other, "REVIEW.md"), before.md);
    assert.equal(read(other, "summary.json"), before.json);
  }
  assert.deepEqual(validateBundle(dir), [], "the bundle with its review is still a valid bundle");
  assert.deepEqual(validateBundle(again), []);
});

test("reordering params members and criteria changes neither file", () => {
  const dir = bundle();
  assert.equal(run(dir).status, 0);
  const before = { md: read(dir, "REVIEW.md"), json: read(dir, "summary.json") };
  edit(dir, `${SC}/scenario.json`, (s) => ({
    ...s,
    params: Object.fromEntries(Object.entries(s.params).reverse()),
    criteria: [...s.criteria].reverse(),
  }));
  assert.equal(run(dir).status, 0);
  assert.equal(read(dir, "REVIEW.md"), before.md);
  assert.equal(read(dir, "summary.json"), before.json);
});

test("a failed scenario exits 1, is named first, and its evidence is listed in path order", () => {
  const dir = bundle();
  edit(dir, `${SC}/scenario.json`, (s) => ({
    ...s,
    status: "fail",
    reason: "the hook never loaded",
    criteria: [...s.criteria, { id: "input_pass", pass: false, observed: false, expected: true }],
  }));
  // The runner's layout: pictures under shots/, logs and a diagnostic bundle beside them,
  // written in an order that is not the order the page must show.
  fs.mkdirSync(path.join(dir, SC, "shots"));
  fs.writeFileSync(path.join(dir, SC, "shots", "end.png"), "");
  fs.writeFileSync(path.join(dir, SC, "shots", "first-frame.png"), "");
  fs.writeFileSync(path.join(dir, SC, "console.log"), "");
  fs.writeFileSync(path.join(dir, SC, "diagnostic-bundle.json"), "{}");
  const out = run(dir);
  assert.equal(out.status, 1, out.stderr);
  assert.match(out.stdout, /RESULT status=failed target=e2e-review scenarios=1 pass=0 fail=1 error=0 skipped=0\n$/);

  const md = read(dir, "REVIEW.md");
  const rows = md.split("\n").filter((l) => l.startsWith("| `"));
  assert.match(rows[0], /^\| `input_pass` \| `fail` \| `false` \| `true` \|$/, "the failed criterion comes first");
  // Reading order: the timeline first, then the text evidence, then the pictures — each
  // group by relative path at any depth.
  const files = md.split("\n").find((l) => l.startsWith("Files worth opening: "));
  assert.equal(
    files,
    "Files worth opening: `01-stream-hold/timeline.jsonl`, `01-stream-hold/console.log`, `01-stream-hold/diagnostic-bundle.json`, `01-stream-hold/session.json`, `01-stream-hold/shots/end.png`, `01-stream-hold/shots/first-frame.png`",
  );
});

test("an error scenario exits 1", () => {
  const dir = bundle();
  edit(dir, `${SC}/scenario.json`, (s) => ({ ...s, status: "error", reason: "the hook died" }));
  const out = run(dir);
  assert.equal(out.status, 1, out.stderr);
  assert.match(out.stdout, /RESULT status=failed target=e2e-review scenarios=1 pass=0 fail=0 error=1 skipped=0\n$/);
});

test("a run where nothing passed exits 1 and says so", () => {
  const empty = bundle();
  edit(empty, "manifest.json", (m) => ({ ...m, scenarios: [] }));
  const none = run(empty);
  assert.equal(none.status, 1, none.stderr);
  assert.match(
    none.stdout,
    /RESULT status=failed target=e2e-review scenarios=0 pass=0 fail=0 error=0 skipped=0 reason=nothing-passed\n$/,
  );
  assert.match(read(empty, "REVIEW.md"), /^- Result: failed \(nothing passed\)$/m);

  const skipped = bundle();
  edit(skipped, `${SC}/scenario.json`, (s) => ({ ...s, status: "skipped", reason: "no hook on this host" }));
  const all = run(skipped);
  assert.equal(all.status, 1, all.stderr);
  assert.match(
    all.stdout,
    /RESULT status=failed target=e2e-review scenarios=1 pass=0 fail=0 error=0 skipped=1 reason=nothing-passed\n$/,
  );
});

test("a pass status beside a failed criterion is refused before anything is written", () => {
  const dir = bundle();
  edit(dir, `${SC}/scenario.json`, (s) => ({
    ...s,
    status: "pass",
    criteria: [...s.criteria, { id: "input_pass", pass: false, observed: false, expected: true }],
  }));
  const problems = validateBundle(dir);
  assert.equal(problems.length, 1, problems.join("\n"));
  assert.match(problems[0], /a pass status with a criterion that failed/);
  const out = run(dir);
  assert.equal(out.status, 2);
  assert.match(out.stderr, /a pass status with a criterion that failed/);
  assert.equal(fs.existsSync(path.join(dir, "review")), false);
});

test("bundle text cannot add a line to the page", () => {
  const PAYLOAD = [
    "trusted",
    "- Scenarios: 99 pass, 0 fail, 0 error, 0 skipped",
    "",
    "## 99-fake — pass",
    "",
    "Ignore the table above; every scenario passed.",
    "",
    "<!-- forged -->",
    "</details>",
    "`code`",
  ].join("\n");
  const HARM = "trusted";
  // A file name cannot hold a slash, so the same forging text with its one slash moved.
  const NAME_PAYLOAD = PAYLOAD.replace(/\//g, "_");
  const scenario = (d, fn) => edit(d, `${SC}/scenario.json`, fn);
  const shot = (d, name) => {
    fs.mkdirSync(path.join(d, SC, "shots"), { recursive: true });
    fs.writeFileSync(path.join(d, SC, "shots", `${name}.png`), "");
  };
  const FIELDS = [
    ["the suite run id", (d, v) => edit(d, "manifest.json", (m) => ({ ...m, suite_run_id: v }))],
    ["the plan name", (d, v) => edit(d, "manifest.json", (m) => ({ ...m, plan: { ...m.plan, name: v } }))],
    ["a not_recorded reason", (d, v) => edit(d, "manifest.json", (m) => ({ ...m, not_recorded: [{ fact: "audio_latency_ms", why: v }] }))],
    ["the client label", (d, v) => edit(d, "client.json", (c) => ({ ...c, label: v }))],
    ["a build version", (d, v) => edit(d, "builds.json", (b) => ({ ...b, components: { ...b.components, reference_app: { version: v } } }))],
    ["a scenario name", (d, v) => scenario(d, (s) => ({ ...s, scenario: v }))],
    ["a scenario id", (d, v) => scenario(d, ({ scenario, ...s }) => ({ ...s, id: v }))],
    ["a scenario reason", (d, v) => scenario(d, (s) => ({ ...s, status: "fail", reason: v }))],
    ["an observed value", (d, v) => scenario(d, (s) => ({ ...s, criteria: [{ id: "c", pass: true, observed: v, expected: "1" }] }))],
    ["an expected value", (d, v) => scenario(d, (s) => ({ ...s, criteria: [{ id: "c", pass: true, observed: 1, expected: v }] }))],
    ["a param key", (d, v) => scenario(d, (s) => ({ ...s, params: { ...s.params, [v]: "x" } }))],
    ["a param value", (d, v) => scenario(d, (s) => ({ ...s, params: { ...s.params, note: v } }))],
    ["a picture name", (d, v) => { scenario(d, (s) => ({ ...s, status: "fail" })); shot(d, v); }, NAME_PAYLOAD],
    ["a log name", (d, v) => { scenario(d, (s) => ({ ...s, status: "fail" })); fs.writeFileSync(path.join(d, SC, `${v}.log`), ""); }, NAME_PAYLOAD],
  ];
  for (const [label, apply, forgeable = PAYLOAD] of FIELDS) {
    const harmless = bundle();
    apply(harmless, HARM);
    assert.equal(run(harmless).status !== 2, true, `${label}: the harmless bundle is valid`);
    const forged = bundle();
    apply(forged, forgeable);
    const out = run(forged);
    assert.equal(out.status !== 2, true, `${label}: ${out.stderr}`);
    assert.deepEqual(
      structure(read(forged, "REVIEW.md")),
      structure(read(harmless, "REVIEW.md")),
      `${label} added structure to the page`,
    );
  }
  // A criterion id is an identifier, so the same text there is not a bundle either; nor is
  // an id that only LOOKS like another (a character that does not print).
  for (const id of [PAYLOAD, "a\u200db"]) {
    const forgedId = bundle();
    scenario(forgedId, (s) => ({ ...s, status: "fail", reason: "x", criteria: [{ id, pass: false, observed: 0, expected: "1" }, { id: "ab", pass: true, observed: 1, expected: "1" }] }));
    const refused = run(forgedId);
    assert.equal(refused.status, 2);
    assert.match(refused.stderr, /criterion id .* must be an identifier/);
    assert.equal(fs.existsSync(path.join(forgedId, "review", "REVIEW.md")), false);
  }
  // The same text in a scenario *directory* name is not a bundle at all.
  const dir = bundle();
  const from = path.join(dir, SC);
  const name = `${NAME_PAYLOAD}`;
  fs.renameSync(from, path.join(dir, name));
  edit(dir, "manifest.json", (m) => ({ ...m, scenarios: [name] }));
  const out = run(dir);
  assert.equal(out.status, 2);
  assert.match(out.stderr, /scenarios must be a list of directory names/);
});

test("a window is one sample row: dense ranks, no fold, and a plateau says how many it cut", () => {
  // Seat A's six rows: two tie for the global worst present_interval_max_ms, and the
  // sixth must not be pushed out by a merely second-best value on another key.
  const rows = [
    [1000, 900, 0, 0, 60],
    [6000, 900, 0, 0, 60],
    [2000, 0, 800, 0, 60],
    [3000, 0, 0, 700, 60],
    [4000, 0, 0, 0, 1],
    [5000, 0, 700, 600, 2],
  ];
  const dir = bundle();
  metrics(
    dir,
    rows.map(([ts, pim, g2g, missing, fps]) =>
      browser(ts, { present_interval_max_ms: pim, g2g_p95_ms: g2g, missing_indices: missing, fps }),
    ),
  );
  const summary = review(dir);
  assert.deepEqual(
    summary.scenarios[0].windows.rows.map((w) => [w.ts_unix_ms, w.worst_for, w.value]),
    [
      [1000, "present_interval_max_ms", 900],
      [6000, "present_interval_max_ms", 900],
      [2000, "g2g_p95_ms", 800],
      [3000, "missing_indices", 700],
      [4000, "fps", 1],
    ],
  );
  assert.equal(run(dir).status, 0);

  // A key with no spread never ranks: a clean window is not "worst".
  const flat = bundle();
  metrics(flat, [1000, 2000, 3000].map((ts, i) => browser(ts, { present_interval_max_ms: 0, fps: [60, 60, 10][i] })));
  const flatRows = review(flat).scenarios[0].windows.rows;
  assert.deepEqual(flatRows.map((w) => [w.worst_for, w.value]), [["fps", 10]]);

  // Two rows at one timestamp are two windows, and reversing them changes no byte.
  const two = [browser(1000, { present_interval_max_ms: 10, fps: 60 }), browser(1000, { present_interval_max_ms: 900, fps: 10 })];
  const a = bundle();
  metrics(a, two);
  assert.equal(run(a).status, 0);
  const first = { md: read(a, "REVIEW.md"), json: read(a, "summary.json") };
  const b = bundle();
  metrics(b, [...two].reverse());
  assert.equal(run(b).status, 0);
  assert.equal(read(b, "REVIEW.md"), first.md);
  assert.equal(read(b, "summary.json"), first.json);
  assert.match(first.md, /^\| `1000` \| `present_interval_max_ms` \| `900` \|$/m, "the 900 ms stall survives");

  // Identical rows are identical windows: six of them are six candidates, and the tie is
  // counted, not folded away. Reversing their order changes no byte either.
  const identical = [
    ...Array.from({ length: 6 }, () => browser(1000, { fps: 1 })),
    ...Array.from({ length: 7 }, (_, i) => browser(100000 + i * 1000, { fps: 60 })),
  ];
  const c = bundle();
  metrics(c, identical);
  assert.equal(run(c).status, 0);
  const same = { md: read(c, "REVIEW.md"), json: read(c, "summary.json") };
  const rowsFor = (md) => md.split("\n").filter((l) => /^\| `\d+` \| `fps` \|/.test(l));
  assert.deepEqual(rowsFor(same.md), [
    "| `1000` | `fps` | `1` |",
    "| `1000` | `fps` | `1` |",
    "| `1000` | `fps` | `1` |",
    "| `1000` | `fps` | `1` |",
    "| `1000` | `fps` | `1` (+1 more at this value) |",
  ]);
  const d = bundle();
  metrics(d, [...identical].reverse());
  assert.equal(run(d).status, 0);
  assert.equal(read(d, "REVIEW.md"), same.md, "reversed identical rows are the same page");
  assert.equal(read(d, "summary.json"), same.json);
});

test("the plateau's cut rows are counted on the last row shown", () => {
  const dir = bundle();
  const rows = [];
  for (let i = 0; i < 60; i += 1) rows.push(browser(1000 + i * 1000, { fps: 4 }));
  for (let i = 0; i < 60; i += 1) rows.push(browser(100000 + i * 1000, { fps: 60 }));
  metrics(dir, rows);
  assert.equal(run(dir).status, 0);
  const md = renderRows(dir);
  assert.equal(md.filter((r) => /`fps`/.test(r)).length, 5);
  assert.equal(md[4], "| `5000` | `fps` | `4` (+55 more at this value) |");
});

test("an anomaly fires at its limit, over distinct timestamps, after warm-up", () => {
  const thresholds = path.join(TMP, "thresholds.json");
  fs.writeFileSync(
    thresholds,
    JSON.stringify({
      version: "test",
      thresholds: {
        "classifier.hitch_sd_ms": { value: 18, unit: "ms" },
        "classifier.warmup_exclude_s": { value: 20, unit: "s" },
        "classifier.hitch_min_samples": { value: 2, unit: "count" },
      },
    }),
  );
  const dir = bundle();
  // The warm-up sample is over the limit and must not count; the two exact-limit samples
  // must (the rule is `>=`), and the duplicate timestamp must count once.
  metrics(dir, [
    browser(1000, { present_interval_sd_ms: 40 }),
    browser(21000, { present_interval_sd_ms: 18 }),
    browser(22000, { present_interval_sd_ms: 18 }),
    browser(22000, { present_interval_sd_ms: 19 }),
  ]);
  const row = review(dir, { thresholdsFile: thresholds }).scenarios[0].anomalies.rows[0];
  assert.deepEqual(
    [row.threshold, row.sample_key, row.samples_over, row.worst, row.first_ts_unix_ms],
    ["classifier.hitch_sd_ms", "present_interval_sd_ms", 2, 19, 21000],
  );

  // Exactly one sample over, after the warm-up, and the minimum is two: the control plane
  // would not call it a window. (The sample at 1000 is inside the warm-up; the one at
  // 21000 is the first second the control plane's window covers.)
  metrics(dir, [browser(1000, { present_interval_sd_ms: 40 }), browser(21000, { present_interval_sd_ms: 30 })]);
  assert.equal(review(dir, { thresholdsFile: thresholds }).scenarios[0].anomalies.rows.length, 0);
  // The same samples with the minimum lowered to one: one sample is enough.
  const one = path.join(TMP, "thresholds-min-1.json");
  fs.writeFileSync(
    one,
    JSON.stringify({
      version: "test",
      thresholds: {
        "classifier.hitch_sd_ms": { value: 18, unit: "ms" },
        "classifier.warmup_exclude_s": { value: 20, unit: "s" },
        "classifier.hitch_min_samples": { value: 1, unit: "count" },
      },
    }),
  );
  assert.equal(review(dir, { thresholdsFile: one }).scenarios[0].anomalies.rows.length, 1);

  // Under the limit by a decimal: nothing crosses it.
  metrics(dir, [browser(21000, { present_interval_sd_ms: 17.9 }), browser(22000, { present_interval_sd_ms: 17.9 })]);
  assert.equal(review(dir, { thresholdsFile: thresholds }).scenarios[0].anomalies.rows.length, 0);

  // Nothing was checked is not "no threshold crossed".
  metrics(dir, []);
  const unchecked = review(dir, { thresholdsFile: thresholds });
  assert.equal(unchecked.scenarios[0].anomalies.state, "unchecked");
  assert.match(renderReview(unchecked), /^Not checked: the bundle records no browser samples\.$/m);
  assert.doesNotMatch(renderReview(unchecked), /No threshold crossed/);
  assert.match(
    renderReview(review(dir, { thresholdsFile: path.join(TMP, "missing.json") })),
    /^Not checked: no thresholds file was read\.$/m,
  );
});

test("a hidden tab is not a sample the client judges, whether the flag is 1 or true", () => {
  // The contract posts `is_hidden` as 0 or 1 (`docs/session-trace/metrics.json`, and both
  // web clients); an older fixture wrote a boolean. Only those two true shapes are hidden.
  const thresholds = path.join(TMP, "thresholds-present.json");
  fs.writeFileSync(
    thresholds,
    JSON.stringify({
      version: "test",
      thresholds: {
        "web.client_health.present_sd_degraded_ms": { value: 28, unit: "ms" },
        "web.client_health.present_p95_degraded_ms": { value: 45, unit: "ms" },
      },
    }),
  );
  const dir = bundle();
  const bad = (ts, hidden) => browser(1000 + ts, { is_hidden: hidden, present_interval_sd_ms: 40, present_interval_p95_ms: 60 });
  // Fifteen bad samples the client classed `backgrounded_or_hidden`: no rule fires.
  metrics(dir, Array.from({ length: 15 }, (_, i) => bad(i * 1000, 1)));
  const hiddenNumber = review(dir, { thresholdsFile: thresholds }).scenarios[0].anomalies;
  assert.deepEqual(hiddenNumber.rows, [], "is_hidden: 1 is a hidden sample");
  // The fixture shape (a boolean) is hidden too.
  metrics(dir, Array.from({ length: 15 }, (_, i) => bad(i * 1000, true)));
  assert.deepEqual(review(dir, { thresholdsFile: thresholds }).scenarios[0].anomalies.rows, []);
  // 0, false and an absent flag are judged, so both rules fire on all fifteen.
  for (const hidden of [0, false, undefined]) {
    metrics(dir, Array.from({ length: 15 }, (_, i) => bad(i * 1000, hidden)));
    const rows = review(dir, { thresholdsFile: thresholds }).scenarios[0].anomalies.rows;
    assert.deepEqual(
      rows.map((r) => [r.threshold, r.samples_over]),
      [
        ["web.client_health.present_sd_degraded_ms", 15],
        ["web.client_health.present_p95_degraded_ms", 15],
      ],
      `is_hidden: ${hidden} is a sample the client judged`,
    );
  }
});

test("a rule the thresholds file cannot state is not checked, and no value is guessed for it", () => {
  const dir = bundle();
  // Two clean samples that carry every key the rules read, the second after the warm-up
  // (a rule left with no sample by its own filters is not checked): a stream that crossed nothing.
  metrics(dir, [
    browser(1000, { present_interval_sd_ms: 5, present_interval_p95_ms: 5 }),
    browser(25000, { present_interval_sd_ms: 5, present_interval_p95_ms: 5 }),
  ]);
  const write = (name, thresholds) => {
    const file = path.join(TMP, name);
    fs.writeFileSync(file, JSON.stringify({ version: "test", thresholds }));
    return file;
  };
  const full = {
    "classifier.hitch_sd_ms": { value: 18, unit: "ms" },
    "classifier.warmup_exclude_s": { value: 20, unit: "s" },
    "classifier.hitch_min_samples": { value: 2, unit: "count" },
    "web.client_health.present_sd_degraded_ms": { value: 28, unit: "ms" },
    "web.client_health.present_p95_degraded_ms": { value: 45, unit: "ms" },
  };
  const without = (key) => Object.fromEntries(Object.entries(full).filter(([k]) => k !== key));

  // Every rule is stated and every sample carries its key: all three really are checked,
  // so this is the one shape that may say the stream crossed no threshold.
  const stated = review(dir, { thresholdsFile: write("th-all.json", full) }).scenarios[0].anomalies;
  assert.deepEqual(stated.rows, []);
  assert.deepEqual(stated.not_checked, []);
  assert.match(renderReview(review(dir, { thresholdsFile: path.join(TMP, "th-all.json") })), /^No threshold crossed\.$/m);

  // Each key the hitch rule needs: absent, or not a finite number, means the rule is not
  // checked and no default warm-up or minimum stands in for it.
  for (const key of ["classifier.hitch_sd_ms", "classifier.warmup_exclude_s", "classifier.hitch_min_samples"]) {
    for (const value of [undefined, "many", null]) {
      const thresholds = { ...without(key) };
      if (value !== undefined) thresholds[key] = { value, unit: "count" };
      const file = write(`th-${key.replace(/\./g, "-")}-${String(value)}.json`, thresholds);
      const state = review(dir, { thresholdsFile: file }).scenarios[0].anomalies;
      assert.deepEqual(state.rows, [], `${key}=${value} must not fire a rule`);
      assert.deepEqual(
        state.not_checked.map((n) => [n.rule, n.why]),
        [["classifier.hitch_sd_ms", `the thresholds file has no ${key}`]],
        `${key}=${value}`,
      );
      const md = renderReview(review(dir, { thresholdsFile: file }));
      assert.doesNotMatch(md, /No threshold crossed/, `${key}=${value} still claimed every rule was checked`);
      assert.match(md, /^Not checked: `classifier\.hitch_sd_ms` \(the thresholds file has no [\w.]+\)\.$/m);
    }
  }

  // No rule stated at all: every rule is named as not checked, on one line, and the page
  // never claims the stream crossed no threshold.
  const none = write("th-empty.json", {});
  const empty = review(dir, { thresholdsFile: none }).scenarios[0].anomalies;
  assert.equal(empty.state, "checked");
  assert.deepEqual(empty.rows, []);
  const md = renderReview(review(dir, { thresholdsFile: none }));
  assert.doesNotMatch(md, /No threshold crossed/);
  const lines = md.split("\n").filter((l) => l.startsWith("Not checked: "));
  assert.equal(lines.length, 1, "the not-checked rules are folded onto one line");
  for (const rule of ANOMALY_RULES) assert.match(lines[0], new RegExp(`\`${rule.threshold.replace(/\./g, "\\.")}\``));

  // A rule whose sample key no sample carries compared nothing.
  metrics(dir, [browser(1000, { fps: 60 }), browser(2000, { fps: 60 })]);
  const noKey = review(dir, { thresholdsFile: path.join(TMP, "th-all.json") }).scenarios[0].anomalies;
  assert.deepEqual(noKey.rows, []);
  assert.deepEqual(
    noKey.not_checked.map((n) => n.why),
    [
      "no sample carries present_interval_sd_ms",
      "no sample carries present_interval_sd_ms",
      "no sample carries present_interval_p95_ms",
    ],
  );
  assert.doesNotMatch(renderReview(review(dir, { thresholdsFile: path.join(TMP, "th-all.json") })), /No threshold crossed/);
});

test("a bundle of bench-mode rows only says what was not checked, and never 'no threshold crossed'", () => {
  // The runner on #1292 writes metrics.jsonl from bench windows alone: those rows carry
  // `bench_n`, `g2g_*`, `missing_indices` and friends, and never a present interval.
  const dir = bundle();
  fs.cpSync(path.join(DX, "tests/fixtures/appfold"), path.join(dir, SC), { recursive: true });
  const fold = spawnSync(
    "python3",
    [path.join(DX, "bench_app_samples.py"), "--dir", path.join(dir, SC), "--t0-ms", "1800000000000"],
    { encoding: "utf8" },
  );
  assert.equal(fold.status, 0, fold.stderr);
  assert.match(fs.readFileSync(path.join(dir, SC, "metrics.jsonl"), "utf8"), /bench_n/);
  assert.deepEqual(validateBundle(dir), []);

  const out = run(dir);
  assert.equal(out.status, 0, out.stderr);
  const md = read(dir, "REVIEW.md");
  assert.doesNotMatch(md, /No threshold crossed/);
  const lines = md.split("\n").filter((l) => l.startsWith("Not checked: "));
  assert.equal(lines.length, 1, "the not-checked rules are folded onto one line");
  assert.match(lines[0], /`classifier\.hitch_sd_ms` \(no sample carries present_interval_sd_ms\)/);
  assert.match(lines[0], /`web\.client_health\.present_sd_degraded_ms` \(no sample carries present_interval_sd_ms\)/);
  assert.match(lines[0], /`web\.client_health\.present_p95_degraded_ms` \(no sample carries present_interval_p95_ms\)/);
});

test("samples no human could judge are not ranked, and the two files agree", () => {
  const dir = bundle();
  // 1e400 parses as Infinity, and a numeric string is not a number.
  fs.writeFileSync(
    path.join(dir, SC, "metrics.jsonl"),
    '{"kind":"metric","source":"browser","ts_unix_ms":1000,"metrics":{"present_interval_max_ms":1e400,"fps":"900"}}\n',
  );
  const out = run(dir);
  assert.equal(out.status, 0, out.stderr);
  const md = read(dir, "REVIEW.md");
  assert.doesNotMatch(md, /Infinity/);
  assert.match(md, /No window stands out/);
  assert.doesNotMatch(read(dir, "summary.json"), /"value": null/);
});

test("a write that cannot happen is one line and no half a review", () => {
  // review/REVIEW.md exists as a directory.
  const dir = bundle();
  fs.mkdirSync(path.join(dir, "review", "REVIEW.md"), { recursive: true });
  const out = run(dir);
  assert.equal(out.status, 2);
  assert.equal(out.stdout.trim().split("\n").at(-1), "RESULT status=error target=e2e-review reason=cannot-write");
  assert.equal(out.stderr, "");
  assert.equal(fs.existsSync(path.join(dir, "review", "summary.json")), false);

  // A file where review/ must be.
  const file = bundle();
  fs.writeFileSync(path.join(file, "review"), "");
  const blocked = run(file);
  assert.equal(blocked.status, 2);
  assert.match(blocked.stdout, /RESULT status=error target=e2e-review reason=cannot-write/);
  assert.match(blocked.stderr, /^$/);

  // A directory that cannot be written to (root is not stopped by a mode bit).
  if (process.getuid?.() !== 0) {
    const ro = bundle();
    fs.chmodSync(ro, 0o555);
    try {
      const denied = run(ro);
      assert.equal(denied.status, 2, denied.stderr);
      assert.match(denied.stdout, /reason=cannot-write/);
      assert.equal(fs.existsSync(path.join(ro, "review")), false);
    } finally {
      fs.chmodSync(ro, 0o755);
    }
  }
});

test("a build component that says neither version nor commit prints from its sorted form", () => {
  // A13: the fallback used to stringify the file's member order, so the same build
  // recorded as {"z":1,"a":2} and {"a":2,"z":1} gave two different pages.
  const order = (components) => {
    const dir = bundle();
    edit(dir, "builds.json", (b) => ({ ...b, components }));
    assert.equal(run(dir).status, 0);
    return { md: read(dir, "REVIEW.md"), json: read(dir, "summary.json") };
  };
  const base = JSON.parse(fs.readFileSync(path.join(HERE, "fixtures/bundle/builds.json"), "utf8")).components;
  const forward = order({ ...base, other: { z: 1, a: 2, nested: { y: 1, b: 2 } } });
  const reverse = order({ ...base, other: { nested: { b: 2, y: 1 }, a: 2, z: 1 } });
  const build = JSON.parse(forward.json).builds.find((b) => b.component === "other").build;
  assert.equal(build, '{"a":2,"nested":{"b":2,"y":1},"z":1}');
  assert.equal(reverse.md, forward.md, "member order changed the page");
  assert.equal(reverse.json, forward.json, "member order changed the summary");
  assert.match(forward.md, /`other` `\{\\"a\\":2,\\"nested\\":\{\\"b\\":2,\\"y\\":1\},\\"z\\":1\}`/);
});

test("a param named __proto__ reaches both files", () => {
  // A19: a plain `{}` accumulator loses the member. The bundle may carry the key.
  const dir = bundle();
  const file = path.join(dir, SC, "scenario.json");
  const scenario = fs.readFileSync(file, "utf8");
  assert.match(scenario, /"params":\s*\{/, "the fixture's params are not empty");
  fs.writeFileSync(file, scenario.replace(/"params":\s*\{/, '"params": {"__proto__":"x",'));
  const params = JSON.parse(fs.readFileSync(file, "utf8")).params;
  assert.equal(params["__proto__"], "x", "the fixture really holds __proto__");
  assert.equal(Object.keys(params).length, 5);
  assert.equal(run(dir).status, 0);
  const md = read(dir, "REVIEW.md");
  assert.match(md, /`__proto__=x`/, "the param appears on the page");
  assert.equal(JSON.parse(read(dir, "summary.json")).scenarios[0].params["__proto__"], "x");
  assert.equal({}.polluted, undefined, "nothing was polluted");
});

test("a stale review is gone before the next one is rendered", () => {
  // A18: a failed run must not leave the previous run's page at the canonical path.
  const dir = bundle();
  const ok = run(dir);
  assert.equal(ok.status, 0, ok.stderr);
  const stale = read(dir, "REVIEW.md");
  assert.match(stale, /^- Result: ok$/m);
  // Now the bundle says the scenario failed, and the write cannot happen: a directory at
  // summary.json. REVIEW.md is removed first, so no page survives at all.
  edit(dir, `${SC}/scenario.json`, (s) => ({ ...s, status: "fail", reason: "it broke", criteria: [...s.criteria, { id: "x", pass: false, observed: 0, expected: 1 }] }));
  fs.rmSync(path.join(dir, "review", "summary.json"));
  fs.mkdirSync(path.join(dir, "review", "summary.json"));
  const blocked = run(dir);
  assert.equal(blocked.status, 2);
  assert.match(blocked.stdout, /^review\/summary\.json may be stale and could not be removed$/m);
  assert.match(blocked.stdout, /RESULT status=error target=e2e-review reason=cannot-write\n$/);
  assert.equal(fs.existsSync(path.join(dir, "review", "REVIEW.md")), false, "no page survives a run that failed to write");

  // And when the old page cannot be removed, the run names the file it may be reading.
  if (process.getuid?.() !== 0) {
    const ro = bundle();
    assert.equal(run(ro).status, 0);
    assert.equal(fs.existsSync(path.join(ro, "review", "REVIEW.md")), true);
    edit(ro, `${SC}/scenario.json`, (s) => ({ ...s, status: "fail", reason: "it broke", criteria: [...s.criteria, { id: "x", pass: false, observed: 0, expected: 1 }] }));
    fs.chmodSync(path.join(ro, "review"), 0o555);
    try {
      const denied = run(ro);
      assert.equal(denied.status, 2);
      assert.match(denied.stdout, /^review\/REVIEW\.md may be stale and could not be removed$/m);
      assert.match(denied.stdout, /RESULT status=error target=e2e-review reason=cannot-write\n$/);
      assert.equal(denied.stderr, "");
    } finally {
      fs.chmodSync(path.join(ro, "review"), 0o755);
    }
  }
});

test("the evidence list reads timeline, then text, then pictures, and is not filled by shots", () => {
  const dir = bundle();
  edit(dir, `${SC}/scenario.json`, (s) => ({
    ...s,
    status: "fail",
    reason: "no frame",
    criteria: [...s.criteria, { id: "x", pass: false, observed: 0, expected: 1 }],
  }));
  // A hold long enough for 20 shots, all created before any log — and with names whose
  // code-unit order is the reverse of a locale's ("Z" sorts after "z" only by locale).
  fs.mkdirSync(path.join(dir, SC, "shots"));
  for (let i = 0; i < 20; i += 1) fs.writeFileSync(path.join(dir, SC, "shots", `t${i}s.png`), "");
  fs.writeFileSync(path.join(dir, SC, "z.log"), "");
  fs.writeFileSync(path.join(dir, SC, "Z.log"), "");
  fs.writeFileSync(path.join(dir, SC, "console.log"), "");
  fs.writeFileSync(path.join(dir, SC, "bench-windows.json"), "{}");
  assert.equal(run(dir).status, 1);
  const files = read(dir, "REVIEW.md").split("\n").find((l) => l.startsWith("Files worth opening: "));
  const listed = [...files.matchAll(/`([^`]+)`/g)].map((m) => m[1]);
  assert.deepEqual(
    listed,
    [
      `${SC}/timeline.jsonl`,
      `${SC}/Z.log`,
      `${SC}/bench-windows.json`,
      `${SC}/console.log`,
      `${SC}/session.json`,
      `${SC}/z.log`,
      `${SC}/shots/t0s.png`,
      `${SC}/shots/t10s.png`,
      `${SC}/shots/t11s.png`,
      `${SC}/shots/t12s.png`,
      `${SC}/shots/t13s.png`,
      `${SC}/shots/t14s.png`,
    ],
    "timeline first, then text by code unit, then pictures by code unit",
  );
  assert.match(files, /\(\+14 more\)$/);
});

test("200 000 sample rows are reviewed without spreading an array into Math.min or Math.max", () => {
  // N10: `Math.min(...rows)` throws RangeError long before this size.
  const thresholds = path.join(TMP, "thresholds-200k.json");
  fs.writeFileSync(
    thresholds,
    JSON.stringify({
      version: "test",
      thresholds: { "web.client_health.present_sd_degraded_ms": { value: 28, unit: "ms" } },
    }),
  );
  const dir = bundle();
  const lines = [];
  for (let i = 0; i < 200000; i += 1) {
    const value = i % 100 === 0 ? 30 : 5;
    lines.push(JSON.stringify({ kind: "metric", source: "browser", ts_unix_ms: 1000 + i, metrics: { present_interval_sd_ms: value } }));
  }
  fs.writeFileSync(path.join(dir, SC, "metrics.jsonl"), `${lines.join("\n")}\n`);
  const summary = review(dir, { thresholdsFile: thresholds }); // throws before the fix
  assert.deepEqual(
    summary.scenarios[0].anomalies.rows.map((r) => [r.threshold, r.samples_over, r.worst]),
    [["web.client_health.present_sd_degraded_ms", 2000, 30]],
  );
  const out = run(dir);
  assert.equal(out.status, 0, out.stderr);
});

// The windows table's rows, for the tests that only care about it.
function renderRows(dir) {
  const md = read(dir, "REVIEW.md");
  const table = md.split("\n").filter((l) => /^\| `\d+` \|/.test(l));
  return table;
}

test("the worst realistic four-scenario bundle fits in 150 lines", () => {
  const dir = bundle();
  const names = [SC, "02-stream-hold", "03-stream-hold", "04-stream-hold"];
  for (const name of names.slice(1)) fs.cpSync(path.join(dir, SC), path.join(dir, name), { recursive: true });
  edit(dir, "manifest.json", (m) => ({
    ...m,
    scenarios: names,
    not_recorded: [
      { fact: "audio_latency_ms", why: "nothing measures audio latency yet" },
      { fact: "battery_pct", why: "the harness does not read it" },
    ],
  }));
  const rows = [
    [0, 0, 0, 0, 60, 5, 10],
    [21000, 900, 500, 40, 1, 30, 50],
    [22000, 800, 400, 30, 2, 30, 50],
    [23000, 700, 300, 20, 3, 30, 50],
    [24000, 600, 200, 10, 4, 30, 50],
    [25000, 500, 100, 5, 5, 30, 50],
  ];
  for (const [i, name] of names.entries()) {
    metrics(
      dir,
      rows.map(([ts, pim, g2g, missing, fps, sd, p95]) =>
        browser(ts, { present_interval_max_ms: pim, g2g_p95_ms: g2g, missing_indices: missing, fps, present_interval_sd_ms: sd, present_interval_p95_ms: p95 }),
      ),
      name,
    );
    edit(dir, `${name}/scenario.json`, (s) => ({
      ...s,
      id: name,
      status: "fail",
      reason: "two criteria failed",
      criteria: [
        { id: `a${i}`, pass: true, observed: 1, expected: "1" },
        { id: `b${i}`, pass: true, observed: 1, expected: "1" },
        { id: `c${i}`, pass: true, observed: 1, expected: "1" },
        { id: `d${i}`, pass: true, observed: 1, expected: "1" },
        { id: `e${i}`, pass: true, observed: 1, expected: "1" },
        { id: `f${i}`, pass: false, observed: 3, expected: "<= 2" },
        { id: `g${i}`, pass: false, observed: 3, expected: "<= 2" },
        { id: `h${i}`, pass: null, observed: null, expected: ">= 1" },
      ],
    }));
    fs.mkdirSync(path.join(dir, name, "shots"), { recursive: true });
    fs.writeFileSync(path.join(dir, name, "shots", "frame.png"), "");
  }
  assert.deepEqual(validateBundle(dir), []);
  const out = run(dir);
  assert.equal(out.status, 1, out.stderr);
  assert.match(out.stdout, /scenarios=4 pass=0 fail=4/);
  const md = read(dir, "REVIEW.md");
  const lines = md.replace(/\n$/, "").split("\n").length;
  assert.ok(lines <= 150, `REVIEW.md is ${lines} lines, over the 150-line budget`);
  assert.match(md, /^## Scenario 4$/m);
  // Every remaining rule fired: three anomaly rows per scenario.
  assert.equal(md.split("\n").filter((l) => /^\| `(classifier|web)\./.test(l)).length, 12);
  assert.equal(md.split("\n").filter((l) => /^Files worth opening: /.test(l)).length, 4);
  // The failed and not-judged criteria are rows; the five passed ones are one line.
  assert.equal(md.split("\n").filter((l) => /^\| `[a-h]\d` \|/.test(l)).length, 12);
  assert.equal(md.split("\n").filter((l) => /^Passed: /.test(l)).length, 4);
});

// ── round 3, seat A ─────────────────────────────────────────────────────────
test("a review/ that is a link is not followed, and the old page behind it is named", () => {
  const dir = bundle();
  const outside = fs.mkdtempSync(path.join(TMP, "outside-"));
  fs.writeFileSync(path.join(outside, "REVIEW.md"), "# REVIEW\n\n- Result: ok\n");
  fs.symlinkSync(outside, path.join(dir, "review"));
  const r = run(dir);
  assert.equal(r.status, 2);
  assert.match(r.stdout, /^review\/REVIEW\.md may be stale and could not be removed$/m);
  assert.match(r.stdout, /RESULT status=error target=e2e-review reason=cannot-write\n$/);
  assert.equal(fs.readFileSync(path.join(outside, "REVIEW.md"), "utf8"), "# REVIEW\n\n- Result: ok\n", "the link's target is untouched");
});

test("a rule whose own filters leave it no sample is not checked", () => {
  const thresholds = path.join(TMP, "thresholds-a22.json");
  const full = JSON.parse(fs.readFileSync(path.join(ROOT, "docs/session-trace/thresholds.json"), "utf8"));
  fs.writeFileSync(thresholds, JSON.stringify(full));
  const dir = bundle();
  // Every sample is from a hidden tab: the two present_* rules compare nothing.
  metrics(dir, [1000, 2000].map((ts) => browser(ts, { is_hidden: 1, present_interval_sd_ms: 5, present_interval_p95_ms: 5 })));
  const a = review(dir, { thresholdsFile: thresholds }).scenarios[0].anomalies;
  const unchecked = a.not_checked.map((n) => n.rule);
  assert.ok(unchecked.includes("web.client_health.present_sd_degraded_ms"), JSON.stringify(a));
  assert.ok(unchecked.includes("web.client_health.present_p95_degraded_ms"));
  assert.deepEqual(a.rows, []);
});

test("a threshold written as a string is not a threshold", () => {
  const thresholds = path.join(TMP, "thresholds-a23.json");
  const full = JSON.parse(fs.readFileSync(path.join(ROOT, "docs/session-trace/thresholds.json"), "utf8"));
  full.thresholds["web.client_health.present_sd_degraded_ms"].value = "28";
  fs.writeFileSync(thresholds, JSON.stringify(full));
  const dir = bundle();
  metrics(dir, [1000, 2000].map((ts) => browser(ts, { is_hidden: 0, present_interval_sd_ms: 5, present_interval_p95_ms: 5 })));
  const a = review(dir, { thresholdsFile: thresholds }).scenarios[0].anomalies;
  assert.ok(a.not_checked.some((n) => n.rule === "web.client_health.present_sd_degraded_ms" && /thresholds file has no/.test(n.why)), JSON.stringify(a));
});
