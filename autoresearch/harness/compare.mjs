#!/usr/bin/env node
// Frozen keep/discard rule. Compares a candidate run with the reference run (the
// last kept head) using the noise bands in ../baseline.json.
//
//   node compare.mjs --track P|U --ref <label> --cand <label>
//
// Track P keeps iff bytes.app drops by more than the bytes noise band AND
//   every ttu.<page> <= ref + ttu band AND tests pass AND every screenshot
//   differs from the reference in <= PIXEL_MAX of its pixels.
// Track U keeps iff polish_primary (axe nodes + impeccable findings) drops by more
//   than its noise band AND tests pass. (Its picture is meant to change; DESIGN.md
//   and the mock are checked by reading, and recorded in results.tsv.)
import fs from "node:fs";
import path from "node:path";
import { PNG } from "pngjs";
import pixelmatch from "pixelmatch";

const arg = (n) => { const i = process.argv.indexOf(n); return i > 0 ? process.argv[i + 1] : undefined; };
const here = path.dirname(new URL(import.meta.url).pathname);
const out = path.join(here, "out");
const track = arg("--track");
const ref = JSON.parse(fs.readFileSync(path.join(out, arg("--ref") + ".json"), "utf8"));
const cand = JSON.parse(fs.readFileSync(path.join(out, arg("--cand") + ".json"), "utf8"));
const base = JSON.parse(fs.readFileSync(path.join(here, "..", "baseline.json"), "utf8"));
const guardsFile = path.join(out, arg("--cand") + ".guards");
const tests = fs.existsSync(guardsFile) ? fs.readFileSync(guardsFile, "utf8").trim() : "not-run";
const testsPass = tests.startsWith("pass");
const PIXEL_MAX = 0.001; // 0.1 % of a shot's pixels

function pixels() {
  const res = {};
  for (const f of cand.shots ?? []) {
    const a = PNG.sync.read(fs.readFileSync(path.join(out, arg("--ref") + ".shots", f)));
    const b = PNG.sync.read(fs.readFileSync(path.join(out, arg("--cand") + ".shots", f)));
    if (a.width !== b.width || a.height !== b.height) { res[f] = 1; continue; }
    res[f] = pixelmatch(a.data, b.data, null, a.width, a.height, { threshold: 0.1 }) / (a.width * a.height);
  }
  return res;
}

const verdict = { track, tests };
if (track === "P") {
  verdict.primary_before = ref.bytes.app;
  verdict.primary_after = cand.bytes.app;
  verdict.primary_ok = ref.bytes.app - cand.bytes.app > base.noise.bytes;
  // Interleaved A/B: the reference build was timed in the same window as the candidate.
  const refTtu = cand.ttu_ref ?? ref.ttu;
  verdict.ttu = Object.fromEntries(Object.keys(refTtu).map((p) => [p, { ref: refTtu[p], cand: cand.ttu[p], ok: cand.ttu[p] <= refTtu[p] + base.noise.ttu[p] }]));
  verdict.pixels = pixels();
  verdict.pixels_ok = Object.values(verdict.pixels).every((x) => x <= PIXEL_MAX);
  verdict.keep = verdict.primary_ok && Object.values(verdict.ttu).every((t) => t.ok) && verdict.pixels_ok && testsPass;
} else if (track === "U") {
  verdict.primary_before = ref.polish_primary;
  verdict.primary_after = cand.polish_primary;
  verdict.axe = [ref.axe_total, cand.axe_total];
  verdict.impeccable = [ref.impeccable_total, cand.impeccable_total];
  verdict.primary_ok = ref.polish_primary - cand.polish_primary > base.noise.polish;
  verdict.keep = verdict.primary_ok && testsPass;
} else {
  throw new Error("--track P|U");
}
console.log(JSON.stringify(verdict, null, 1));
process.exitCode = verdict.keep ? 0 : 1;
