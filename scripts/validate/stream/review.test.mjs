// node --test scripts/validate/stream/review.test.mjs   (make verify runs it)
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";
import { validateBundle } from "./bundle.mjs";
import { review } from "./review.mjs";

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
  fs.writeFileSync(file, JSON.stringify(fn(JSON.parse(fs.readFileSync(file, "utf8")))));
};
const run = (dir) => spawnSync(process.execPath, [path.join(HERE, "review.mjs"), dir], { encoding: "utf8" });
const read = (dir, file) => fs.readFileSync(path.join(dir, "review", file), "utf8");
const golden = (file) => fs.readFileSync(path.join(GOLDEN, file), "utf8");

test("the fixture bundle reviews to the committed golden files", () => {
  const dir = bundle();
  const out = run(dir);
  assert.equal(out.status, 0, out.stderr);
  assert.match(out.stdout, /RESULT status=ok target=e2e-review scenarios=1 pass=1 fail=0 error=0 skipped=0\n$/);
  assert.equal(read(dir, "REVIEW.md"), golden("REVIEW.md"));
  assert.equal(read(dir, "summary.json"), golden("summary.json"));
});

test("the same bundle in gives the same bytes out, and review/ leaves it a valid bundle", () => {
  const dir = bundle();
  assert.equal(run(dir).status, 0);
  const before = { md: read(dir, "REVIEW.md"), json: read(dir, "summary.json") };
  // A second bundle built the same way, and a second run over the first one: no
  // clock, no absolute path and no directory-listing order may reach the bytes.
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

test("a failed criterion exits 1, is named first, and its files are listed", () => {
  const dir = bundle();
  edit(dir, `${SC}/scenario.json`, (s) => ({
    ...s,
    status: "fail",
    criteria: [...s.criteria, { id: "freeze_count", pass: false, observed: 3, expected: "0" }],
  }));
  fs.writeFileSync(path.join(dir, SC, "frame-2.png"), Buffer.from([0x89, 0x50, 0x4e, 0x47, 0, 0, 0, 0]));
  const out = run(dir);
  assert.equal(out.status, 1, out.stderr);
  assert.match(out.stdout, /RESULT status=failed target=e2e-review scenarios=1 pass=0 fail=1 error=0 skipped=0\n$/);

  const md = read(dir, "REVIEW.md");
  const rows = md.split("\n").filter((l) => l.startsWith("| ") && !l.startsWith("| criterion") && !l.startsWith("| ---"));
  assert.match(rows[0], /^\| freeze_count \| fail \| 3 \| 0 \|$/, "the failed criterion comes first");
  assert.match(md, /Files worth opening: `01-stream-hold\/`, `01-stream-hold\/timeline.jsonl`, `01-stream-hold\/frame-2.png`/);
});

test("an invalid bundle exits 2, prints its problems and writes nothing", () => {
  const dir = bundle();
  edit(dir, "manifest.json", (m) => ({ ...m, format: 2 }));
  assert.throws(
    () => review(dir),
    (e) => e.problems.length === 1 && /manifest\.json: format must be 1/.test(e.problems[0]),
  );
  const out = run(dir);
  assert.equal(out.status, 2);
  assert.match(out.stderr, /^manifest\.json: format must be 1$/m);
  assert.match(out.stdout, /RESULT status=error target=e2e-review problems=1\n$/);
  assert.equal(fs.existsSync(path.join(dir, "review")), false);
});

test("a four-scenario bundle fits in 150 lines", () => {
  const dir = bundle();
  const extra = ["02-stream-hold", "03-stream-hold", "04-stream-hold"];
  for (const name of extra) {
    fs.cpSync(path.join(dir, SC), path.join(dir, name), { recursive: true });
    edit(dir, `${name}/scenario.json`, (s) => ({ ...s, id: name }));
  }
  edit(dir, "manifest.json", (m) => ({ ...m, scenarios: [...m.scenarios, ...extra] }));
  assert.deepEqual(validateBundle(dir), []);
  const out = run(dir);
  assert.equal(out.status, 0, out.stderr);
  const md = read(dir, "REVIEW.md");
  const lines = md.replace(/\n$/, "").split("\n").length;
  assert.ok(lines <= 150, `REVIEW.md is ${lines} lines, over the 150-line budget`);
  assert.match(md, /## 04-stream-hold — pass/);
});
