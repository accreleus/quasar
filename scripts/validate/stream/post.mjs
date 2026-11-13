#!/usr/bin/env node
// Put a suite run on quasar-bench: one run per scenario, through scripts/dx/bench_submit.py.
// docs/testing-e2e-suite.md is the guide.
//
//   node scripts/validate/stream/post.mjs <bundle-dir> --host <role> [--dry-run]
//
// The bundle is untrusted: it is validated first, and every value taken from it reaches
// bench_submit.py as its own argv element (never a shell string) and only when it matches
// the shape its field has in bundle.mjs / collect.mjs. A param in no shape refuses its
// scenario rather than being left out (two cases that differ in it would share one name), and
// so does a file in a scenario directory that this suite does not write: bench_submit.py opens
// several, and each would reach the run unchecked. Notes carry criterion ids (identifiers the
// validator enforced) and numbers only: `expected`, `reason` and any other free text is never
// copied.
//
// bench_submit.py owns the server, the key and the API (BENCH_URL / BENCH_KEY, else
// qbench's config); this script never reads, prints or passes either.
import { spawnSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { validateBundle } from "./bundle.mjs";
import { EFFECTIVE_UNKNOWN, SHAPES } from "./collect.mjs";

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
// order; the app follows, then every other param, sorted.
const NAME_KEYS = ["transport", "codec", "profile"];
// The three that do not change what is measured in a way the bench splits on: `hold_s` is
// kept as a tag (how long it ran), the other two are how the app was presented. A plan run
// against another app measured another thing, so `app` is in the name (`appSlug`).
const NOT_IN_NAME = new Set(["bench", "cover_qr", "hold_s"]);
/** A bench scenario's whole name; the service takes it as text, and this is our own cap. */
const NAME_MAX = 200;
/** The shape such a name has: what `[scenario, k=v, ...].join("/")` can make out of identifiers,
 *  finite numbers and booleans — letters, digits, `_ . - + / =` — at most NAME_MAX characters.
 *  `e2e-compare` keeps a scenario's identity in its JSON only in this shape, so two bundles pair
 *  by exactly what this file posts. */
export const NAME_SHAPE = /^[A-Za-z0-9_.\-+/=]{1,200}$/;

/** A param's value as name or tag text, or null when its type is not one a value may have. */
function paramValue(v) {
  if (typeof v === "string") return fits(SHAPES.identifier, v) ? v : null;
  if (typeof v === "number") return Number.isFinite(v) ? String(v) : null;
  if (typeof v === "boolean") return String(v);
  return null;
}

/** How long an app's slug may be in a scenario's name. */
const APP_MAX = 40;
/** An app's name as it may stand in a scenario's name: `Quasar Benchapp` is `quasar-benchapp`. */
export function appSlug(name) {
  return name
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, APP_MAX)
    .replace(/-+$/, "");
}

/**
 * The name to post under, the tags, the params left out, and the reason a scenario cannot be
 * posted at all (`refused`, null when it can). A param whose value is not an identifier, a
 * finite number or a boolean is refused rather than left out, and a `params` that is not an
 * object with it: two cases that differ in it would otherwise share one name. `e2e-compare`
 * pairs two bundles' scenarios by `name`, so the two verbs agree on when two runs measured one
 * thing, and it refuses the same scenarios.
 */
export function benchIdentity(sc) {
  const noName = (refused) => ({ name: null, tags: [], leftOut: 0, refused });
  if (!isObj(sc.params)) return noName("its params is not an object");
  const params = sc.params;
  const pairs = [];
  const leftOut = [];
  // A key names the param in the reason; a key that is not an identifier cannot, so it is not echoed.
  const badValue = (k) => `its param ${k} is not an identifier, a finite number or a boolean (the bench scenario name cannot hold it)`;
  for (const k of NAME_KEYS) {
    if (params[k] === undefined) continue;
    const v = paramValue(params[k]);
    if (v === null) return noName(badValue(k));
    pairs.push([k, v]);
  }
  // The app is what was on the screen: a slug of its own name, which may hold spaces and capitals.
  if (params.app !== undefined) {
    if (typeof params.app !== "string") return noName("its param app is not an app name (a string)");
    const slug = appSlug(params.app);
    if (slug) pairs.push(["app", slug]);
    else leftOut.push("app"); // a name with nothing to slug: no app in the name, and counted
  }
  for (const k of Object.keys(params).sort()) {
    if (NAME_KEYS.includes(k) || NOT_IN_NAME.has(k) || k === "app") continue;
    if (!fits(SHAPES.identifier, k)) return noName("one of its params has a name that is not an identifier (the bench scenario name cannot hold it)");
    const v = paramValue(params[k]);
    if (v === null) return noName(badValue(k));
    pairs.push([k, v]);
  }
  const tags = pairs.filter(([k]) => NAME_KEYS.includes(k) || k === "app");
  const hold = paramValue(params.hold_s);
  if (hold !== null) tags.push(["hold_s", hold]);
  else if (params.hold_s !== undefined) leftOut.push("hold_s");
  const name = [sc.scenario, ...pairs.map(([k, v]) => `${k}=${v}`)].join("/");
  return { name, tags, leftOut, refused: null };
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
  // The rest of what bench_submit.py copies out of this file: `launch` and `fps` into
  // conditions.negotiated and the tags `launch`, `height` and `fps`, `bitrate_kbps` into a tag,
  // `started_at_ms` into an event's anchor. It casts each to an integer, so a string is a crash
  // and a non-finite number is not a size or a rate.
  const launch = s.launch;
  if (launch !== undefined && launch !== null && (!Array.isArray(launch) || launch.length !== 2 || !launch.every((v) => typeof v === "number" && Number.isFinite(v)))) {
    return "session.json's launch is not two finite numbers";
  }
  for (const k of ["fps", "bitrate_kbps", "started_at_ms"]) {
    if (s[k] !== undefined && s[k] !== null && !(typeof s[k] === "number" && Number.isFinite(s[k]))) return `session.json's ${k} is not a finite number`;
  }
  return null;
}

/**
 * Files `bench_submit.py` reads from or uploads out of a scenario directory that this suite
 * never writes: another harness's session verdict, its caps.negotiated trace, the soak's marks
 * and steps, and a hand-written report. Each would reach the run without passing a shape — the
 * verdict as the run's validity and its free-text reason, the caps as the tags `codec`,
 * `codec_profile` and `encoder_negotiated`, the marks and steps as events, the report as an
 * artifact — so a bundle that carries one is refused, and the file is named.
 */
const FOREIGN_FILES = ["caps-negotiated.json", "verdict.json", "steps.jsonl", "marks.jsonl"];
// A name a reason may echo: a file the bundle wrote may be called anything.
const PLAIN_NAME = /^[A-Za-z0-9_.-]{1,64}$/;

/** The first file in a scenario directory that a post may not carry, or null. */
function foreignFileProblem(sdir) {
  for (const f of FOREIGN_FILES) {
    if (fs.existsSync(path.join(sdir, f))) return `${f} is not a file this suite writes`;
  }
  let entries = [];
  try {
    entries = fs.readdirSync(sdir, { withFileTypes: true });
  } catch {}
  const md = entries.filter((e) => e.name.endsWith(".md")).map((e) => e.name).sort()[0]; // a file or a directory: the script globs the name
  if (md) return `${PLAIN_NAME.test(md) ? md : "a .md file"} is not a file this suite writes`;
  return null;
}

// The shapes of `conditions.json`'s `negotiated` members: what `session.json` was checked for
// above, which bench_submit.py copies into the same map.
const NEGOTIATED_SHAPES = new Map([
  ["width", "number"],
  ["height", "number"],
  ["fps", "number"],
  ["codec", SHAPES.identifier],
  ["codec_profile", SHAPES.identifier],
  ["encoder", SHAPES.setting],
  ["encoder_negotiated", SHAPES.setting],
  ["profile_id", SHAPES.identifier],
]);

/**
 * The other files `bench_submit.py` reads from the scenario directory, checked where a field
 * reaches the run's tags or `conditions`: `trace.json` (events), `conditions.json` (`effective`
 * and `negotiated`, which the service compares against the tags) and `summary.json` (the run's
 * summary and, with no `--verdict`, its verdict). `metrics.jsonl` is the bundle validator's.
 * Null when the directory may be posted.
 */
function filesProblem(sdir) {
  const present = (f) => fs.existsSync(path.join(sdir, f));
  if (present("trace.json")) {
    const trace = readJson(path.join(sdir, "trace.json"));
    const events = isObj(trace) ? trace.events : undefined;
    if (!isObj(trace) || (events !== undefined && (!Array.isArray(events) || events.some((e) => !isObj(e) || !isObj(e.payload ?? {}))))) {
      return "trace.json is not a trace this suite writes (an object whose events carry an object payload)";
    }
  }
  if (present("conditions.json")) {
    const cond = readJson(path.join(sdir, "conditions.json"));
    if (!isObj(cond)) return "conditions.json is not an object";
    // bench_submit.py posts every member of this file as the run's conditions: only the ones the collector writes may be there.
    const extra = Object.keys(cond).find((k) => !["effective", "negotiated", "unknown"].includes(k));
    if (extra !== undefined) return `conditions.json holds ${PLAIN_NAME.test(extra) ? extra : "a member"}, which this suite does not write`;
    if (cond.unknown !== undefined && !(isObj(cond.unknown) && Object.keys(cond.unknown).length === 1 && cond.unknown.effective === EFFECTIVE_UNKNOWN)) return "conditions.json's unknown is not what the collector writes";
    const effective = cond.effective;
    if (effective !== undefined && !isObj(effective)) return "conditions.json's effective is not a map";
    for (const [k, v] of Object.entries(effective ?? {})) {
      if (!fits(SHAPES.identifier, k)) return "conditions.json's effective holds a key that is not an identifier";
      if (v !== "[omitted]" && !fits(SHAPES.setting, v)) return "conditions.json's effective holds a value that is not a setting (the collector writes a setting or [omitted])";
    }
    const negotiated = cond.negotiated;
    if (negotiated !== undefined) {
      if (!isObj(negotiated)) return "conditions.json's negotiated is not an object";
      for (const [k, v] of Object.entries(negotiated)) {
        const shape = NEGOTIATED_SHAPES.get(k);
        if (!shape) return `conditions.json's negotiated holds ${PLAIN_NAME.test(k) ? k : "a member"} this suite does not write`;
        const ok = shape === "number" ? typeof v === "number" && Number.isFinite(v) : fits(shape, v);
        if (!ok) return `conditions.json's negotiated ${k} is not the shape session.json was checked for`;
      }
    }
  }
  if (present("summary.json") && !isObj(readJson(path.join(sdir, "summary.json")))) return "summary.json is not an object";
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
      // A scenario name that reads as an option would reach bench_submit.py's parser as one, and
      // a name in no shape is no name; both are refused here, before anything runs.
      let problem = null;
      if (!fits(SHAPES.identifier, sc.scenario)) {
        problem = "its scenario is not a name (letters, digits, _ . -, at most 64)";
      } else if (sc.scenario.startsWith("-")) {
        problem = "its scenario name begins with '-' (bench_submit.py would read it as an option)";
      } else {
        problem = sessionProblem(path.join(dir, name, "session.json"), sc) ?? filesProblem(path.join(dir, name)) ?? foreignFileProblem(path.join(dir, name));
      }
      // The name and its tags are the bundle's own words or nothing: a param in no shape refuses
      // the scenario (two cases that differ in it would otherwise share one name), and a name over
      // the cap is refused, never truncated.
      const identity = problem ? null : benchIdentity(sc);
      if (!problem && identity.refused) problem = identity.refused;
      if (problem) {
        console.error(`${name}: ${problem}; nothing posted for it`);
        out.failed += 1;
        continue;
      }
      const { name: benchScenario, tags, leftOut } = identity;
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
