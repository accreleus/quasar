#!/usr/bin/env node
// The run bundle, format 1: what a valid one is. docs/testing-e2e-suite.md is the guide.
//
//   node scripts/validate/stream/bundle.mjs <bundle-dir>     exit 0 valid, 1 not, 2 usage
//
// No dependencies: the runner validates with this before it writes a bundle, on machines
// that have Node and nothing else.
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../../..");

export const FORMAT = 1;
export const STATUSES = ["pass", "fail", "error", "skipped"];
// What quasar-bench accepts as a sample source; a scenario directory is posted as it is.
export const SOURCES = ["agent", "browser", "app", "netem", "harness"];
export const CLOCKS = ["client", "host"];

// Addresses and tokens never belong in a bundle, whoever made it.
// shortcut: IPv4 only; IPv6, bare host names and home paths (a session container's own
// home is a real path in app logs) are left to scripts/dev/leak-scan.sh with the
// operator's patterns, which runs before a bundle is posted anywhere public.
const IPV4 = /(?<![\d.])(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})(?!\d|\.\d)/g;
const DOC_NETS = ["192.0.2.", "198.51.100.", "203.0.113."]; // RFC 5737
// Shared with the collector, which replaces what these match before it writes.
export const SECRETS = [
  [/\beyJ[\w-]{10,}\.[\w-]{10,}\./, "a JWT"],
  // A token has a digit in it; "Bearer authentication." in prose has none.
  [/\bBearer\s+(?=[\w.~+/-]*\d)[\w.~+/-]{8,}/i, "a bearer token"],
  // A value that says there is none ("password": null, secret: none, token=[REDACTED], password: none.) is not one.
  // Nor is one of the markers a redaction leaves, alone (token=<credential>); anything else in angle brackets is a value.
  // No letter before the name, so MY_TOKEN= and hook_secret: count as well as token= itself.
  [/(?<![A-Za-z])(authorization|cookie|password|passwd|secret|token|api[_-]?key)["']?\s*[=:]\s*["']?(?!<(?:credential|token|address|stack|user)>(?!\w))(?!\[?(?:null|none|n\/?a|redacted|omitted|unknown|true|false|bearer)\]?(?=[\s"',}.;!?]|$))[^\s"',}]{4,}/i, "a credential"],
];

/**
 * Does a JSON key name a credential? One rule, shared with the collector's redaction:
 * case, hyphens, spaces, underscores and camelCase are normalised away first, so
 * `Authorization`, `x-api-key`, `accessToken`, `Set-Cookie` and `private key` all match.
 */
export function isCredentialName(key) {
  const k = String(key).replace(/([a-z0-9])([A-Z])/g, "$1_$2").toLowerCase().replace(/[\s-]+/g, "_");
  return /(^|_)(authorization|cookies?|set_cookie|passwords?|passwd|secret|(api|private|secret|access|signing)_?key)$|token$/.test(k);
}

const NONE = /^\[?(null|none|n\/?a|redacted|omitted|unknown|true|false)\]?[.;!?,]?$/i;

// A JSON file (or JSONL) whose key names a credential and whose value is anything but "there is none".
function jsonCredential(text) {
  const docs = [];
  try {
    docs.push(JSON.parse(text));
  } catch {
    for (const line of text.split("\n")) {
      try {
        docs.push(JSON.parse(line));
      } catch {}
    }
  }
  // A credential-named key must hold nothing: null, false, "", a none-word, or an empty container.
  const none = (x) =>
    x === null || x === false || x === "" || (typeof x === "string" && (!x.trim() || NONE.test(x.trim()))) ||
    (x !== null && typeof x === "object" && Object.keys(x).length === 0);
  const bad = (v) =>
    Array.isArray(v)
      ? v.some(bad)
      : v !== null && typeof v === "object" && Object.entries(v).some(([k, x]) => (isCredentialName(k) && !none(x)) || bad(x));
  return docs.some(bad);
}

function leaks(text, file) {
  const found = [];
  for (const m of text.matchAll(IPV4)) {
    const ip = m[0];
    if (m.slice(1).some((o) => +o > 255)) continue; // a version number, not an address
    if (ip.endsWith(".0.0.0") || ip === "127.0.0.1" || DOC_NETS.some((n) => ip.startsWith(n))) continue;
    found.push("an address");
    break;
  }
  for (const [re, what] of SECRETS) if (re.test(text)) found.push(what);
  if (/\.jsonl?$/.test(file) && jsonCredential(text) && !found.includes("a credential")) found.push("a credential");
  return found;
}

/** `text` with every address, token and credential the check above would refuse replaced by a
 *  marker. `counts` (optional) is bumped per kind. IPv6 is replaced here though not checked. */
export function scrub(text, counts = {}) {
  const hit = (k) => (counts[k] = (counts[k] ?? 0) + 1);
  let out = text.replace(IPV4, (ip, ...o) => {
    if (o.slice(0, 4).some((x) => +x > 255) || ip.endsWith(".0.0.0") || ip === "127.0.0.1" || DOC_NETS.some((n) => ip.startsWith(n))) return ip;
    hit("addresses");
    return "<address>";
  });
  for (const v6 of [/(?<![\w:])(?:[0-9a-f]{1,4}:){3,7}[0-9a-f]{0,4}(?![\w:])/gi, /(?<![\w:])(?:[0-9a-f]{1,4}:){1,7}:(?:[0-9a-f]{1,4}(?::[0-9a-f]{1,4}){0,6})?(?![\w:])/gi]) {
    out = out.replace(v6, () => (hit("addresses"), "<address>"));
  }
  for (const [re, what] of SECRETS) {
    out = out.replace(new RegExp(re.source, re.flags.includes("g") ? re.flags : `${re.flags}g`), (m) => {
      hit(what === "a credential" ? "credentials" : "tokens");
      return /^bearer/i.test(m) ? "Bearer <token>" : what === "a JWT" ? "<token>" : "<credential>";
    });
  }
  return out;
}

// What scripts/dx/bench_app_samples.py writes into a browser sample beside the manifest's
// bench keys: two renames, the input-to-photon roll-up and its own flag.
const FOLD_KEYS = ["bench_n", "bench_decoded", "i2p_p50_ms", "i2p_max_ms", "i2p_n", "ts_nudged"];

/** Metric keys the product defines, per source. A bench-mode key rides in a browser sample. */
export function manifestKeys(file = path.join(ROOT, "docs/session-trace/metrics.json")) {
  const keys = { agent: new Set(), browser: new Set(FOLD_KEYS) };
  for (const m of JSON.parse(fs.readFileSync(file, "utf8")).metrics) {
    if (m.source === "agent") keys.agent.add(m.key);
    else keys.browser.add(m.key); // browser and bench
  }
  return keys;
}

/**
 * How the manifest says each key is read, per the source a bundle's row carries it on: its
 * `estimator` (`delta` for a count already taken over a window) and its `window` (`cumulative`
 * for a running total such as `packets_lost`). `e2e-compare` differences a counter and takes
 * the median of a gauge, so it needs both words.
 */
export function manifestEstimators(file = path.join(ROOT, "docs/session-trace/metrics.json")) {
  const out = new Map();
  for (const m of JSON.parse(fs.readFileSync(file, "utf8")).metrics) {
    const source = m.source === "agent" ? "agent" : "browser"; // browser and bench
    out.set(`${source}\u0000${m.key}`, { estimator: m.estimator ?? null, window: m.window ?? null });
  }
  return out;
}

const isObj = (v) => v !== null && typeof v === "object" && !Array.isArray(v);

/** Every way `dir` is not a valid bundle. An empty list means valid. It never throws. */
export function validateBundle(dir, opts) {
  const errors = [];
  try {
    check(dir, errors, opts);
  } catch (e) {
    errors.push(`the validator could not finish: ${e.message}`);
  }
  return errors;
}

function check(dir, errors, { keys = manifestKeys() } = {}) {
  const err = (where, what) => errors.push(`${where}: ${what}`);
  const readJson = (rel) => {
    try {
      const v = JSON.parse(fs.readFileSync(path.join(dir, rel), "utf8"));
      if (isObj(v)) return v;
      err(rel, "not an object");
    } catch (e) {
      err(rel, e.code === "ENOENT" ? "missing" : `not JSON (${e.message})`);
    }
    return null;
  };
  const readJsonl = (rel) => {
    const file = path.join(dir, rel);
    if (!fs.existsSync(file)) return null;
    const rows = [];
    fs.readFileSync(file, "utf8").split("\n").forEach((line, i) => {
      if (!line.trim()) return;
      try {
        const v = JSON.parse(line);
        if (isObj(v)) rows.push(v);
        else err(`${rel}:${i + 1}`, "not an object");
      } catch {
        err(`${rel}:${i + 1}`, "not JSON");
      }
    });
    return rows;
  };

  // First, what is in the directory: nothing is opened until every entry is known to be a
  // regular file (opening a named pipe would wait for ever).
  const files = [];
  const walk = (rel) => {
    for (const e of fs.readdirSync(path.join(dir, rel), { withFileTypes: true })) {
      const p = path.join(rel, e.name);
      if (e.isDirectory()) walk(p);
      else if (e.isFile()) files.push(p);
      else err(p, "is not a regular file (a bundle holds no links)");
    }
  };
  walk("");
  if (errors.length) return;

  const manifest = readJson("manifest.json");
  const client = readJson("client.json");
  const builds = readJson("builds.json");
  const network = readJson("network.json");

  if (manifest) {
    if (manifest.format !== FORMAT) err("manifest.json", `format must be ${FORMAT}`);
    for (const k of ["suite_run_id", "plan", "started_at_ms", "ended_at_ms", "runner", "halves", "scenarios", "not_recorded"]) {
      if (manifest[k] === undefined) err("manifest.json", `no ${k}`);
    }
    // A name is one directory inside the bundle, never a path out of it.
    if (manifest.scenarios !== undefined && (!Array.isArray(manifest.scenarios) || manifest.scenarios.some((n) => typeof n !== "string" || !/^\w[\w.-]*$/.test(n)))) {
      err("manifest.json", "scenarios must be a list of directory names");
      manifest.scenarios = [];
    }
    if (manifest.not_recorded !== undefined && !Array.isArray(manifest.not_recorded)) {
      err("manifest.json", "not_recorded must be a list");
      manifest.not_recorded = [];
    }
    // A name is one run of one scenario: a repeat would be reviewed and counted twice.
    const names = new Set();
    for (const n of Array.isArray(manifest.scenarios) ? manifest.scenarios : []) {
      if (names.has(n)) err("manifest.json", `scenario ${n} is listed twice`);
      names.add(n);
    }
    for (const n of manifest.not_recorded ?? []) {
      if (!n?.fact || !n?.why) err("manifest.json", "a not_recorded entry needs fact and why");
    }
  }
  if (client && !["web", "native"].includes(client.kind)) err("client.json", "kind must be web or native");
  if (builds) {
    if (!isObj(builds.components)) err("builds.json", "no components");
    for (const [name, v] of Object.entries(builds.components ?? {})) {
      if (v === null && !builds.unknown?.[name]) err("builds.json", `${name} is null with no reason in unknown`);
    }
    // A web build the runner served from a local directory (WEB_DIST): its digest and file count, never a path or a file name.
    const web = builds.components?.web;
    const served = manifest?.web_build;
    if (served !== undefined && (!isObj(served) || served.served_by !== "runner" || served.source !== "web-dist" || Object.keys(served).length !== 2)) {
      err("manifest.json", 'web_build must be {"served_by":"runner","source":"web-dist"}');
    }
    if (isObj(web) && web.source === "web-dist") {
      if (Object.keys(web).sort().join() !== "files,sha256,source" || !/^[0-9a-f]{64}$/.test(web.sha256) || !Number.isInteger(web.files) || web.files < 1) {
        err("builds.json", "web (web-dist) must be exactly source, sha256 (64 hex) and files (a count)");
      }
      if (served === undefined) err("manifest.json", "builds.json says the runner served the web build, and the manifest has no web_build");
    } else if (served !== undefined) {
      err("manifest.json", "web_build is set and builds.json has no web-dist web build");
    }
  }
  if (network && !("shaping" in network)) err("network.json", "no shaping (null when none was applied)");

  const notRecorded = new Set((manifest?.not_recorded ?? []).map((n) => n?.fact));
  for (const name of manifest?.scenarios ?? []) {
    const sc = readJson(`${name}/scenario.json`);
    if (!sc) continue;
    const at = `${name}/scenario.json`;
    for (const k of ["id", "params", "sessions"]) if (sc[k] === undefined) err(at, `no ${k}`);
    // An empty list is valid: nothing to collect for this scenario.
    if (sc.sessions !== undefined && (!Array.isArray(sc.sessions) || sc.sessions.some((id) => typeof id !== "string"))) {
      err(at, "sessions must be a list of strings");
    }
    if (!STATUSES.includes(sc.status)) err(at, `status must be one of ${STATUSES.join(", ")}`);
    if (["skipped", "error"].includes(sc.status) && !sc.reason) err(at, `status ${sc.status} needs a reason`);
    if (!("clock_offset_ms" in sc)) err(at, "no clock_offset_ms (null when the offset was not measured)");
    if (!Array.isArray(sc.criteria)) err(at, "no criteria");
    // Criterion ids name the rows of the review's table: two ids a reader cannot tell
    // apart make two rows one fact. So an id is an identifier, not text: ASCII letters,
    // digits, "_", "." and "-", which leaves nothing that prints like something else
    // (a number beside its string, a composed letter beside a decomposed one, a
    // character that does not print at all).
    const ids = new Set();
    for (const c of Array.isArray(sc.criteria) ? sc.criteria : []) {
      if (c?.id === undefined) continue; // reported below as a criterion with no id
      if (typeof c.id !== "string" || !/^[A-Za-z0-9_.-]{1,64}$/.test(c.id)) {
        err(at, `criterion id ${JSON.stringify(c.id).slice(0, 80)} must be an identifier (letters, digits, _ . -, at most 64)`);
        continue;
      }
      if (ids.has(c.id)) err(at, `criterion id ${c.id} is not unique`);
      ids.add(c.id);
    }
    // The review derives a scenario's verdict from its status and its criteria; a pass
    // with a failed criterion is the two disagreeing, so it is not a bundle.
    if (sc.status === "pass" && (Array.isArray(sc.criteria) ? sc.criteria : []).some((c) => c?.pass === false)) {
      err(at, "a pass status with a criterion that failed");
    }
    // A pass rests on something: a scenario that passed judged at least one criterion, and
    // judged it a pass. A short bundle is never a success (spec #1271, story 3).
    if (sc.status === "pass" && Array.isArray(sc.criteria) && !sc.criteria.some((c) => c?.pass === true)) {
      err(at, "a pass status with no criterion that passed");
    }
    for (const c of Array.isArray(sc.criteria) ? sc.criteria : []) {
      if (c?.id === undefined || !("observed" in c) || !("expected" in c) || ![true, false, null].includes(c.pass)) {
        err(at, `criterion ${c?.id ?? "?"} needs id, pass (true, false or null), observed and expected`);
      } else if (c.pass === null && c.observed !== null) {
        err(at, `criterion ${c.id} was not judged, so observed must be null`);
      } else if (notRecorded.has(c.id) && c.observed !== null) {
        err(at, `criterion ${c.id} has a value for a fact the manifest says was not recorded`);
      }
    }

    const timeline = readJsonl(`${name}/timeline.jsonl`);
    if (timeline === null && sc.status !== "skipped") err(`${name}/timeline.jsonl`, "missing");
    (timeline ?? []).forEach((row, i) => {
      if (typeof row.ts_unix_ms !== "number" || !CLOCKS.includes(row.clock) || typeof row.ev !== "string") {
        err(`${name}/timeline.jsonl:${i + 1}`, `needs ts_unix_ms, clock (${CLOCKS.join(" or ")}) and ev`);
      }
    });

    (readJsonl(`${name}/metrics.jsonl`) ?? []).forEach((row, i) => {
      const where = `${name}/metrics.jsonl:${i + 1}`;
      if (typeof row.ts_unix_ms !== "number" || !isObj(row.metrics)) return err(where, "needs ts_unix_ms and metrics");
      if (!SOURCES.includes(row.source)) return err(where, `source must be one of ${SOURCES.join(", ")}`);
      for (const [k, v] of Object.entries(row.metrics)) {
        if (keys[row.source] && !keys[row.source].has(k)) err(where, `${row.source} key ${k} is not in the metric manifest`);
        if (notRecorded.has(k) && v !== null) err(where, `${k} has a value, and the manifest says it was not recorded`);
      }
    });
  }

  // Every text file, whatever its name: a log or a captured console can carry a token too.
  // The check never guesses an encoding. A bundle's text is UTF-8: a file that declares
  // UTF-16 or UTF-32 with a byte-order mark is refused, not decoded, and any other file
  // with a NUL byte in its first 4 KB is a picture, a video or an archive and is not read.
  // shortcut: a text file is read whole; stream it when a bundle carries GB-size logs.
  const head = Buffer.alloc(4096);
  for (const p of files) {
    const fd = fs.openSync(path.join(dir, p), "r");
    const n = fs.readSync(fd, head, 0, head.length, 0);
    fs.closeSync(fd);
    const mark = head.readUInt16BE(0);
    if (n >= 2 && (mark === 0xfffe || mark === 0xfeff || (n >= 4 && head.readUInt32BE(0) === 0x0000feff))) {
      err(p, "is UTF-16 or UTF-32 (a bundle's text is UTF-8)");
    } else if (!head.subarray(0, n).includes(0)) {
      for (const what of leaks(fs.readFileSync(path.join(dir, p), "utf8"), p)) err(p, `holds ${what}`);
    }
  }
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  const dir = process.argv[2];
  if (!dir || !fs.statSync(dir, { throwIfNoEntry: false })?.isDirectory()) {
    console.error("usage: bundle.mjs <bundle-dir>");
    process.exit(2);
  }
  const errors = validateBundle(dir);
  for (const e of errors) console.error(e);
  console.log(`RESULT status=${errors.length ? "failed" : "ok"} target=e2e-bundle errors=${errors.length}`);
  process.exit(errors.length ? 1 : 0);
}
