#!/usr/bin/env node
// Put a suite run on quasar-bench: one run per scenario, through scripts/dx/bench_submit.py.
// docs/testing-e2e-suite.md is the guide.
//
//   node scripts/validate/stream/post.mjs <bundle-dir> --host <role> [--dry-run]
//
// The bundle is untrusted: it is validated first, and every value taken from it reaches
// bench_submit.py as its own argv element (never a shell string) and only when it matches
// the shape its field has in bundle.mjs / collect.mjs. A value that does not is left out
// and counted. Notes carry criterion ids (identifiers the validator enforced) and numbers
// only: `expected`, `reason` and any other free text is never copied.
//
// bench_submit.py owns the server, the key and the API (BENCH_URL / BENCH_KEY, else
// qbench's config); this script never reads, prints or passes either.
import { spawnSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { validateBundle } from "./bundle.mjs";
import { SHAPES } from "./collect.mjs";

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../../..");
export const SUBMIT = path.join(ROOT, "scripts/dx/bench_submit.py");
export const SUITE = "e2e";
export const REPO = "accreleus/quasar";
export const EXIT = { ok: 0, failed: 1, error: 2 };

const isObj = (v) => v !== null && typeof v === "object" && !Array.isArray(v);
const fits = (re, v) => typeof v === "string" && re.test(v);
const readJson = (file) => {
  try {
    return JSON.parse(fs.readFileSync(file, "utf8"));
  } catch {
    return null;
  }
};

/**
 * The run's notes: `id pass|fail|not-judged observed`, one line per criterion. An id is an
 * identifier (validateBundle refused anything else) and an observed value is copied only
 * when it is a number; `expected`, `reason` and a non-numeric observed are free text and
 * never reach the bench server.
 */
export function criteriaNotes(criteria) {
  const lines = [];
  for (const c of Array.isArray(criteria) ? criteria : []) {
    const word = c?.pass === true ? "pass" : c?.pass === false ? "fail" : "not-judged";
    const v = c?.observed;
    const observed = v === null ? "null" : typeof v === "number" && Number.isFinite(v) ? String(v) : "?";
    lines.push(`${c.id} ${word} ${observed}`);
  }
  return lines.join("\n");
}

// ── the bench scenario's name ───────────────────────────────────────────────
// `<scenario>/<k>=<v>/<k>=<v>...`: a separator (`/`) and a joiner (`=`) that a name
// (`[A-Za-z0-9_.-]{1,64}`) and a UUID cannot hold, so a scenario whose name reads like
// another's params is still another name. What decides comparison comes first, in this
// order; every other param follows, sorted.
const NAME_KEYS = ["transport", "codec", "profile"];
// The four that do not change what is measured in a way the bench splits on: `hold_s` is
// kept as a tag (how long it ran), the other three pick the app and its presentation.
const NOT_IN_NAME = new Set(["app", "bench", "cover_qr", "hold_s"]);
/** A bench scenario's whole name; the service takes it as text, and this is our own cap. */
const NAME_MAX = 200;

/** A param's value as name or tag text, or null when its type is not one a value may have. */
function paramValue(v) {
  if (typeof v === "string") return fits(SHAPES.identifier, v) ? v : null;
  if (typeof v === "number") return Number.isFinite(v) ? String(v) : null;
  if (typeof v === "boolean") return String(v);
  return null;
}

/**
 * The name to post under, the tags, and the params left out. A value (or a key) in any other
 * shape is left out and counted, never passed on: the bundle is untrusted.
 */
function benchIdentity(sc) {
  const params = isObj(sc.params) ? sc.params : {};
  const pairs = [];
  const leftOut = [];
  const take = (k) => {
    const v = paramValue(params[k]);
    if (v !== null) return v;
    if (params[k] !== undefined) leftOut.push(k);
    return null;
  };
  for (const k of NAME_KEYS) {
    const v = take(k);
    if (v !== null) pairs.push([k, v]);
  }
  for (const k of Object.keys(params).sort()) {
    if (NAME_KEYS.includes(k) || NOT_IN_NAME.has(k)) continue;
    if (!fits(SHAPES.identifier, k)) {
      leftOut.push(k);
      continue;
    }
    const v = take(k);
    if (v !== null) pairs.push([k, v]);
  }
  const tags = pairs.filter(([k]) => NAME_KEYS.includes(k));
  const hold = take("hold_s");
  if (hold !== null) tags.push(["hold_s", hold]);
  const name = [sc.scenario, ...pairs.map(([k, v]) => `${k}=${v}`)].join("/");
  return { name, tags, leftOut };
}

/**
 * bench_submit.py builds the run's identity and several tags from session.json (`ext_id`,
 * `derive_tags`) and copies `encoder` and `profile_id` into the run's conditions.negotiated:
 * the id must be one of the scenario's own sessions, in the collector's lower-case form (an
 * id spelled in another case hashes to another external id and forks a second run), and every
 * field is used only in the shape its field has. Null when the file may be posted (or is absent).
 */
function sessionProblem(file, sc) {
  if (!fs.existsSync(file)) return null;
  const s = readJson(file);
  if (!isObj(s)) return "session.json is not an object";
  const mine = (Array.isArray(sc.sessions) ? sc.sessions : []).map((id) => String(id).toLowerCase());
  if (!fits(SHAPES.uuid, s.id) || !mine.includes(s.id.toLowerCase())) return "session.json's id is not one of the scenario's sessions (run e2e-collect first)";
  if (s.id !== s.id.toLowerCase()) return "session.json's id is not lower case (the collector writes it lower case; another case is another run)";
  for (const k of ["codec", "profile", "host_id"]) {
    if (s[k] !== undefined && s[k] !== null && !fits(SHAPES.identifier, s[k])) return `session.json's ${k} is not a plain name`;
  }
  if (s.encoder !== undefined && s.encoder !== null && !fits(SHAPES.setting, s.encoder)) return "session.json's encoder is not a plain name";
  // A launch profile's id: an identifier or a UUID, both of which an identifier covers.
  if (s.profile_id !== undefined && s.profile_id !== null && !fits(SHAPES.identifier, s.profile_id)) return "session.json's profile_id is not an identifier or a UUID";
  return null;
}

/** The scenario's own files as artifacts: the pictures, and the two files the run directory lacks. */
function scenarioArtifacts(dir, name) {
  const out = [];
  for (const f of ["scenario.json", "timeline.jsonl"]) {
    const p = path.join(dir, name, f);
    if (fs.existsSync(p)) out.push(p);
  }
  const shots = path.join(dir, name, "shots");
  let entries = [];
  try {
    entries = fs.readdirSync(shots, { withFileTypes: true });
  } catch {}
  for (const e of entries.filter((x) => x.isFile() && x.name.endsWith(".png")).map((x) => x.name).sort()) {
    out.push(path.join(shots, e));
  }
  return out;
}

/** The whole bundle as one tar.gz in a private temporary directory (removed by the caller). */
function archive(dir, spawn, tar) {
  const src = dir; // already the real path: a link given as BUNDLE would archive as a link and nothing else
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "e2e-post-"));
  const dest = path.join(tmp, `${path.basename(src)}.tar.gz`);
  const r = spawn(tar, ["-czf", dest, "-C", path.dirname(src), "--", path.basename(src)], { encoding: "utf8" });
  if (r?.error || r?.status !== 0) {
    fs.rmSync(tmp, { recursive: true, force: true });
    console.error(`the bundle could not be archived (${tar} -czf): ${r?.error?.message ?? `exit ${r?.status}`}; nothing is posted`);
    return null;
  }
  return { dest, tmp };
}

/**
 * Post every scenario of `dir` that is not skipped. `spawn` is injected so a test can record
 * the argv without running anything; the defaults are the real bench_submit.py and tar.
 */
export function postBundle(
  given,
  { host, dryRun = false, spawn = spawnSync, python = "python3", submit = SUBMIT, tar = "tar", env = process.env } = {},
) {
  const out = { status: "ok", exit: EXIT.ok, posted: 0, skipped: 0, failed: 0, warn: 0, leftOut: 0, dryRun, reason: null, problems: [] };
  const dir = fs.realpathSync(given);
  const problems = validateBundle(dir);
  if (problems.length) return { ...out, status: "failed", exit: EXIT.failed, reason: "invalid-bundle", problems };

  const manifest = readJson(path.join(dir, "manifest.json")) ?? {};
  const builds = readJson(path.join(dir, "builds.json")) ?? {};
  const client = readJson(path.join(dir, "client.json")) ?? {};
  // The build under test is the one the bundle was collected from, never this checkout's HEAD.
  const commit = builds.components?.node_agent?.commit;
  if (!fits(SHAPES.commit, commit)) {
    console.error("builds.json has no node agent commit: run e2e-collect first, or the build has no commit");
    return { ...out, status: "failed", exit: EXIT.failed, reason: "no-commit" };
  }

  // The host that pairs runs is `<role>@<client label>`: two clients on one host never compare.
  const suiteRun = fits(SHAPES.identifier, manifest.suite_run_id) ? manifest.suite_run_id : null;
  if (!suiteRun) out.leftOut += 1;
  const label = fits(SHAPES.identifier, client.label) ? client.label : null;
  if (!label) out.leftOut += 1;
  const benchHost = label ? `${host}@${label}` : host;

  const common = [];
  const summary = path.join(dir, "review/summary.json");
  if (fs.existsSync(summary)) common.push(summary);

  const names = Array.isArray(manifest.scenarios) ? manifest.scenarios : [];
  let bundleArchive = null;
  try {
    for (const name of names) {
      const sc = readJson(path.join(dir, name, "scenario.json")) ?? {};
      if (sc.status === "skipped") {
        out.skipped += 1;
        continue;
      }
      // The run's name and identity are the bundle's own words or nothing: no stand-in is posted.
      const problem = fits(SHAPES.identifier, sc.scenario) ? sessionProblem(path.join(dir, name, "session.json"), sc) : "its scenario is not a name (letters, digits, _ . -, at most 64)";
      if (problem) {
        console.error(`${name}: ${problem}; nothing posted for it`);
        out.failed += 1;
        continue;
      }
      // The name and its tags are the bundle's own words or nothing: a param in another shape
      // is left out and counted, and a name over the cap is refused, never truncated.
      const { name: benchScenario, tags, leftOut } = benchIdentity(sc);
      out.leftOut += leftOut.length;
      if (benchScenario.length > NAME_MAX) {
        console.error(`${name}: its bench scenario name is ${benchScenario.length} characters (at most ${NAME_MAX}); nothing posted for it`);
        out.failed += 1;
        continue;
      }
      // The archive is part of the post: without it nothing is posted, the first run included.
      bundleArchive ??= archive(dir, spawn, tar);
      if (!bundleArchive) return { ...out, status: "failed", exit: EXIT.failed, reason: "no-archive" };

      // The server pairs runs by suite, scenario and host, newest per commit: the same scenario
      // with other params is another bench scenario, or webrtc would be compared with webtransport.
      // `clock=raw`: the bundle's trace.json keeps client-clock browser events, so these runs
      // are not comparable with host-aligned ones event for event (issue #1276).
      const all = [["clock", "raw"], ...tags];
      if (suiteRun) all.push(["suite_run", suiteRun]);
      if (label) all.push(["client", label]);
      if (sc.status === "error") all.push(["status", "error"]);

      const verdict = sc.status === "pass" ? "PASS" : "FAIL";
      const artifacts = [...common, ...scenarioArtifacts(dir, name)];
      // The archive rides on the first run the server accepts, and on no other.
      if (bundleArchive && out.posted === 0) artifacts.push(bundleArchive.dest);

      const argv = [
        submit,
        "--dir", path.join(dir, name),
        "--suite", SUITE,
        "--scenario", benchScenario,
        "--host", benchHost,
        "--repo", REPO,
        "--commit", commit,
        "--verdict", verdict,
        "--notes", criteriaNotes(sc.criteria),
        // bench_submit.py derives this from THIS checkout; the run is of the bundle's build.
        "--tag", `git_quasar=${commit}`,
        // The same for the protocol pin, which the bundle does not hold.
        "--tag", "git_protocol=unknown",
      ];
      for (const [k, v] of all) argv.push("--tag", `${k}=${v}`);
      if (suiteRun) argv.push("--ext-id-salt", suiteRun);
      for (const a of artifacts) argv.push("--artifact", a);
      if (dryRun) argv.push("--dry-run");

      console.log(`${dryRun ? "plan" : "post"} ${name} as ${SUITE}/${benchScenario} verdict=${verdict} host=${benchHost}`);
      const r = spawn(python, argv, { encoding: "utf8", env });
      if (r?.stdout) process.stdout.write(r.stdout);
      if (r?.stderr) process.stderr.write(r.stderr);
      if (r?.error || (r?.status !== 0 && r?.status !== 3)) {
        console.error(`${name}: bench_submit.py did not finish (${r?.error?.message ?? `exit ${r?.status}`})`);
        out.failed += 1;
      } else if (r?.status === 3) {
        // bench_submit.py's exit 3 is a POSTED run whose tags disagree with what the host did,
        // not a lost one: do not re-post it, and do not let the archive ride on a later run.
        console.error(`${name}: bench_submit.py exit 3 — posted, but its tags disagree with the host (see the mismatch line above); counted as posted`);
        out.warn += 1;
        out.posted += 1;
      } else {
        out.posted += 1;
      }
    }
  } finally {
    if (bundleArchive) fs.rmSync(bundleArchive.tmp, { recursive: true, force: true });
  }

  if (out.failed) return { ...out, status: "failed", exit: EXIT.failed };
  return out;
}

const USAGE = "usage: post.mjs <bundle-dir> --host <role> [--dry-run]";

function parseArgs(argv) {
  const args = { dir: null, host: null, dryRun: false };
  for (let i = 0; i < argv.length; i += 1) {
    const a = argv[i];
    if (a === "--host") args.host = argv[(i += 1)] ?? null;
    else if (a.startsWith("--host=")) args.host = a.slice("--host=".length);
    else if (a === "--dry-run") args.dryRun = true;
    else if (a === "-h" || a === "--help") args.help = true;
    else if (!args.dir && !a.startsWith("-")) args.dir = a;
    else args.bad = a;
  }
  return args;
}

/** The CLI. Returns the exit code so a test can drive it with a recorded spawn. */
export function main(argv, opts = {}) {
  const args = parseArgs(argv);
  if (args.help) {
    console.log(USAGE);
    return EXIT.ok;
  }
  const usage = (msg, reason) => {
    console.error(msg);
    console.error(USAGE);
    console.log(`RESULT status=error target=e2e-post reason=${reason}`);
    return EXIT.error;
  };
  if (args.bad) return usage(`unexpected argument '${args.bad}'`, "usage");
  if (!args.dir) return usage("a bundle directory is required", "usage");
  if (!args.host) return usage("HOST is required: the role name of the host the bundle ran on", "usage");
  if (!fits(SHAPES.identifier, args.host)) {
    return usage("HOST must be a plain role name (letters, digits, _ . -), such as the lab host's role", "usage");
  }
  if (!fs.statSync(args.dir, { throwIfNoEntry: false })?.isDirectory()) {
    return usage(`${args.dir}: not a directory (a zip is not read; unzip it first)`, "not-a-directory");
  }

  const r = postBundle(args.dir, { host: args.host, dryRun: args.dryRun, ...opts });
  for (const p of r.problems) console.error(p);
  const more = [
    r.failed ? `failed=${r.failed}` : "",
    r.leftOut ? `left_out=${r.leftOut}` : "",
    r.warn ? `warn=${r.warn}` : "",
    r.dryRun ? "dry_run=1" : "",
    r.reason ? `reason=${r.reason}` : "",
  ]
    .filter(Boolean)
    .join(" ");
  console.log(`RESULT status=${r.status} target=e2e-post posted=${r.posted} skipped=${r.skipped}${more ? ` ${more}` : ""}`);
  return r.exit;
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  process.exitCode = main(process.argv.slice(2));
}
