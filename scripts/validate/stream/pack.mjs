#!/usr/bin/env node
// make e2e-pack: the client half of the end-to-end suite (docs/testing-e2e-suite.md) as one archive that runs with one command on a
// machine that has only Node 22 and Chrome. The archive mirrors the repo's layout, so the runner finds its plans, thresholds and
// metric manifest where it always does. What goes in is computed, not listed by hand: the static import closure of the entry and the
// scenario modules, plus a short data list. Tests are never packed.
//
//   node pack.mjs [--out <path without extension>]   writes <out>.tar.gz and <out>.zip
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { tarGz, zip } from "./archive.mjs";

const HERE = path.dirname(fileURLToPath(import.meta.url));
const ROOT = path.resolve(HERE, "../../..");
const STREAM = "scripts/validate/stream";
const TOP = "quasar-e2e-pack";
// The only places a packed file may come from.
const ALLOWED = [`${STREAM}/`, "scripts/validate/lib/", "docs/session-trace/"];
const ENTRY = `${STREAM}/pack-run.mjs`;
const DATA = ["docs/session-trace/thresholds.json", "docs/session-trace/metrics.json"];
const LAUNCHERS = { "run.sh": 0o755, "run.cmd": 0o644, "README.txt": 0o644 };

/** The repo-relative files `file` imports, statically or with a literal import(); node: and bare specifiers are not files. */
export function importsOf(file, root = ROOT) {
  const src = fs.readFileSync(path.join(root, file), "utf8");
  const out = [];
  for (const m of src.matchAll(/(?:\bfrom\s*|\bimport\s*\(\s*|^\s*import\s+)["'](\.{1,2}\/[^"']+)["']/gm)) {
    out.push(path.posix.normalize(path.posix.join(path.posix.dirname(file), m[1])));
  }
  return out;
}

/** The closure of imports from `entries`, as sorted repo-relative paths. A file outside ALLOWED, or a test, stops the build. */
export function closure(entries, root = ROOT) {
  const seen = new Set();
  const todo = [...entries];
  while (todo.length) {
    const f = todo.pop();
    if (seen.has(f)) continue;
    if (!ALLOWED.some((a) => f.startsWith(a)) || /\.test\.mjs$/.test(f) || !fs.existsSync(path.join(root, f))) throw new Error(`${f} cannot go in the pack`);
    seen.add(f);
    todo.push(...importsOf(f, root));
  }
  return [...seen].sort();
}

/** package.json and package-lock.json for playwright-core alone, the one dependency: the lockfile is the repo's own, cut down, so versions and hashes are the ones already reviewed. */
export function playwrightFiles(root = ROOT) {
  const lock = JSON.parse(fs.readFileSync(path.join(root, "scripts/validate/package-lock.json"), "utf8"));
  const core = lock.packages["node_modules/playwright-core"];
  if (!core) throw new Error("scripts/validate/package-lock.json has no playwright-core entry");
  const name = "quasar-e2e-pack";
  const pkg = { name, private: true, version: "0.0.0", type: "module", dependencies: { "playwright-core": core.version } };
  const out = { name, version: "0.0.0", lockfileVersion: 3, requires: true, packages: { "": { name, version: "0.0.0", dependencies: pkg.dependencies }, "node_modules/playwright-core": core } };
  return { "package.json": JSON.stringify(pkg, null, 2) + "\n", "package-lock.json": JSON.stringify(out, null, 2) + "\n" };
}

/** What the pack was made from: the commit, then " dirty" when tracked files differ from it. */
export function versionOf(root = ROOT) {
  const git = (...a) => execFileSync("git", a, { cwd: root, encoding: "utf8" }).trim();
  try { return git("rev-parse", "HEAD") + (git("status", "--porcelain", "--untracked-files=no") ? " dirty" : "") + "\n"; } catch { return "unknown\n"; }
}

export function packMembers(root = ROOT, version = versionOf(root)) {
  const scenarios = fs.readdirSync(path.join(root, STREAM, "scenarios")).filter((f) => f.endsWith(".mjs")).map((f) => `${STREAM}/scenarios/${f}`);
  const plans = fs.readdirSync(path.join(root, STREAM, "plans")).filter((f) => f.endsWith(".json")).map((f) => `${STREAM}/plans/${f}`);
  const files = [...closure([ENTRY, ...scenarios], root), ...plans.sort(), ...DATA];
  // VERSION says "dirty" for changed tracked files only: a file git does not know would ride along under a clean commit.
  let untracked = [];
  try { untracked = execFileSync("git", ["ls-files", "--others", "--exclude-standard", "--", ...ALLOWED], { cwd: root, encoding: "utf8", stdio: ["ignore", "pipe", "ignore"] }).split("\n").filter((f) => files.includes(f)); } catch { /* not a git checkout: nothing to compare with */ }
  if (untracked.length) throw new Error(`${untracked[0]} is not tracked by git: commit it or remove it before packing`);
  for (const f of files) if (!fs.lstatSync(path.join(root, f)).isFile()) throw new Error(`${f} is not a plain file`);
  const members = files.map((f) => ({ name: `${TOP}/${f}`, data: fs.readFileSync(path.join(root, f)), mode: 0o644 }));
  for (const [f, mode] of Object.entries(LAUNCHERS)) members.push({ name: `${TOP}/${f}`, data: fs.readFileSync(path.join(root, STREAM, "pack", f)), mode });
  for (const [f, text] of Object.entries(playwrightFiles(root))) members.push({ name: `${TOP}/${f}`, data: Buffer.from(text), mode: 0o644 });
  members.push({ name: `${TOP}/VERSION`, data: Buffer.from(version), mode: 0o644 });
  return members.sort((a, b) => (a.name < b.name ? -1 : 1));
}

function main() {
  const i = process.argv.indexOf("--out");
  const version = versionOf();
  const base = path.resolve(i > 0 ? process.argv[i + 1] : `quasar-e2e-pack-${version.trim().slice(0, 8)}`);
  const members = packMembers(ROOT, version);
  fs.writeFileSync(`${base}.tar.gz`, tarGz(members));
  fs.writeFileSync(`${base}.zip`, zip(members));
  const size = (f) => fs.statSync(f).size;
  console.log(`wrote ${base}.tar.gz (${size(`${base}.tar.gz`)} bytes) and ${base}.zip (${size(`${base}.zip`)} bytes), ${members.length} files`);
  console.log(`RESULT status=ok target=e2e-pack tar=${base}.tar.gz zip=${base}.zip files=${members.length}`);
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  try { main(); } catch (e) { console.error(e.message); console.log("RESULT status=error target=e2e-pack tar= zip= files=0"); process.exit(1); }
}
