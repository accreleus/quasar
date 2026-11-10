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

// Addresses, tokens and home paths never belong in a bundle, whoever made it.
// shortcut: IPv4 only, IPv6 and bare host names are not caught; scripts/dev/leak-scan.sh
// with the operator's patterns runs before a bundle is posted anywhere public.
const IPV4 = /(?<![\d.])(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})(?![\d.])/g;
const DOC_NETS = ["192.0.2.", "198.51.100.", "203.0.113."]; // RFC 5737
const SECRETS = [
  [/\beyJ[\w-]{10,}\.[\w-]{10,}\./, "a JWT"],
  [/\bBearer\s+[\w.~+/-]{8,}/i, "a bearer token"],
  [/"(password|access_token|refresh_token|sig_token)"\s*:\s*"[^"]+"/i, "a credential"],
  [/(\/home\/|\/Users\/)[\w.-]+/, "a home path"],
];

function leaks(text) {
  const found = [];
  for (const m of text.matchAll(IPV4)) {
    const ip = m[0];
    if (m.slice(1).some((o) => +o > 255)) continue; // a version number, not an address
    if (ip.endsWith(".0.0.0") || ip === "127.0.0.1" || DOC_NETS.some((n) => ip.startsWith(n))) continue;
    found.push("an address");
    break;
  }
  for (const [re, what] of SECRETS) if (re.test(text)) found.push(what);
  return found;
}

/** Metric keys the product defines, per source. A bench-mode key rides in a browser sample. */
export function manifestKeys(file = path.join(ROOT, "docs/session-trace/metrics.json")) {
  const keys = { agent: new Set(), browser: new Set() };
  for (const m of JSON.parse(fs.readFileSync(file, "utf8")).metrics) {
    if (m.source === "agent") keys.agent.add(m.key);
    else keys.browser.add(m.key); // browser and bench
  }
  return keys;
}

const isObj = (v) => v !== null && typeof v === "object" && !Array.isArray(v);

/** Every way `dir` is not a valid bundle. An empty list means valid. */
export function validateBundle(dir, { keys = manifestKeys() } = {}) {
  const errors = [];
  const err = (where, what) => errors.push(`${where}: ${what}`);
  const readJson = (rel) => {
    try {
      return JSON.parse(fs.readFileSync(path.join(dir, rel), "utf8"));
    } catch (e) {
      err(rel, e.code === "ENOENT" ? "missing" : `not JSON (${e.message})`);
      return null;
    }
  };
  const readJsonl = (rel) => {
    const file = path.join(dir, rel);
    if (!fs.existsSync(file)) return null;
    const rows = [];
    fs.readFileSync(file, "utf8").split("\n").forEach((line, i) => {
      if (!line.trim()) return;
      try {
        rows.push(JSON.parse(line));
      } catch {
        err(`${rel}:${i + 1}`, "not JSON");
      }
    });
    return rows;
  };

  const manifest = readJson("manifest.json");
  const client = readJson("client.json");
  const builds = readJson("builds.json");
  const network = readJson("network.json");

  if (manifest) {
    if (manifest.format !== FORMAT) err("manifest.json", `format must be ${FORMAT}`);
    for (const k of ["suite_run_id", "plan", "started_at_ms", "ended_at_ms", "runner", "halves", "scenarios", "not_recorded"]) {
      if (manifest[k] === undefined) err("manifest.json", `no ${k}`);
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
  }
  if (network && !("shaping" in network)) err("network.json", "no shaping (null when none was applied)");

  const notRecorded = new Set((manifest?.not_recorded ?? []).map((n) => n?.fact));
  for (const name of Array.isArray(manifest?.scenarios) ? manifest.scenarios : []) {
    const sc = readJson(`${name}/scenario.json`);
    if (!sc) continue;
    const at = `${name}/scenario.json`;
    for (const k of ["id", "params", "sessions"]) if (sc[k] === undefined) err(at, `no ${k}`);
    if (!STATUSES.includes(sc.status)) err(at, `status must be one of ${STATUSES.join(", ")}`);
    if (["skipped", "error"].includes(sc.status) && !sc.reason) err(at, `status ${sc.status} needs a reason`);
    if (!("clock_offset_ms" in sc)) err(at, "no clock_offset_ms (null when the offset was not measured)");
    if (!Array.isArray(sc.criteria)) err(at, "no criteria");
    for (const c of sc.criteria ?? []) {
      if (!c?.id || !("observed" in c) || !("expected" in c) || ![true, false, null].includes(c.pass)) {
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
      if (typeof row.ts_unix_ms !== "number" || !CLOCKS.includes(row.clock)) {
        err(`${name}/timeline.jsonl:${i + 1}`, `needs ts_unix_ms and clock (${CLOCKS.join(" or ")})`);
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
  const walk = (rel) => {
    for (const e of fs.readdirSync(path.join(dir, rel), { withFileTypes: true })) {
      const p = path.join(rel, e.name);
      if (e.isDirectory()) walk(p);
      else if (/\.(json|jsonl|md|txt|log|html)$/.test(e.name)) {
        for (const what of leaks(fs.readFileSync(path.join(dir, p), "utf8"))) err(p, `holds ${what}`);
      }
    }
  };
  if (fs.existsSync(dir)) walk("");
  return errors;
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  const dir = process.argv[2];
  if (!dir || !fs.existsSync(dir)) {
    console.error("usage: bundle.mjs <bundle-dir>");
    process.exit(2);
  }
  const errors = validateBundle(dir);
  for (const e of errors) console.error(e);
  console.log(`RESULT status=${errors.length ? "failed" : "ok"} target=e2e-bundle errors=${errors.length}`);
  process.exit(errors.length ? 1 : 0);
}
