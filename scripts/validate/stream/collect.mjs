#!/usr/bin/env node
// The host half of a run bundle: every fact the stack still holds about a
// scenario's sessions, fetched by session id and written into the bundle.
// docs/testing-e2e-suite.md is the guide.
//
//   QUASAR_ADMIN_TOKEN=<bearer> node scripts/validate/stream/collect.mjs <bundle-dir> \
//       --base-url <stack url>
//
// The bearer is read from the environment and never from argv: any local user can
// read another process's argv out of `ps`. It is never written into the bundle.
//
// No dependencies, and it never launches anything: the client half may have run on
// another machine days earlier. A session the API no longer holds (404, or a trace
// that has been pruned) is recorded as not collected, never as empty.
//
// Exit: 0 every session collected and the bundle valid, 1 the bundle is not valid,
//       2 usage or the bearer was rejected, 3 incomplete (some session could not be
//       collected — the reason is in that scenario's host-half.json).
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { validateBundle } from "./bundle.mjs";

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../../..");

export const EXIT = { ok: 0, invalid: 1, error: 2, incomplete: 3 };

// A field whose name says it holds a credential never reaches a bundle, whatever
// the route called it (token, sig_token, access_token, password, ...).
const CREDENTIAL = /(^|_)(token|password|passwd|secret|api_?key)$/i;

// The same rule bundle.mjs applies when it reads a bundle back: an address is four
// in-range octets, except loopback, a documentation network and x.0.0.0 (which is
// how a browser's version reads).
const IPV4 = /(?<![\d.])(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})(?!\d|\.\d)/g;
const DOC_NETS = ["192.0.2.", "198.51.100.", "203.0.113."]; // RFC 5737

const isObj = (v) => v !== null && typeof v === "object" && !Array.isArray(v);

function isAddress(ip) {
  const o = ip.split(".").map(Number);
  if (o.some((n) => n > 255)) return false;
  return !(ip.endsWith(".0.0.0") || ip === "127.0.0.1" || DOC_NETS.some((n) => ip.startsWith(n)));
}

function redactText(text, token) {
  let out = text.replace(IPV4, (m) => (isAddress(m) ? "[address]" : m));
  if (token) out = out.split(token).join("[redacted]");
  return out;
}

/** A deep copy of `value` with credentials dropped, addresses replaced and keys sorted. */
export function redact(value, token) {
  if (typeof value === "string") return redactText(value, token);
  if (Array.isArray(value)) return value.map((v) => redact(v, token));
  if (isObj(value)) {
    const out = {};
    for (const k of Object.keys(value).sort()) {
      if (CREDENTIAL.test(k)) continue;
      out[k] = redact(value[k], token);
    }
    return out;
  }
  return value;
}

function readJson(file) {
  try {
    return JSON.parse(fs.readFileSync(file, "utf8"));
  } catch (e) {
    throw new Error(`${file}: ${e.code === "ENOENT" ? "missing" : `not JSON (${e.message})`}`);
  }
}

function readJsonl(file) {
  if (!fs.existsSync(file)) return [];
  return fs
    .readFileSync(file, "utf8")
    .split("\n")
    .filter((l) => l.trim())
    .map((l, i) => {
      try {
        return JSON.parse(l);
      } catch {
        throw new Error(`${file}:${i + 1}: not JSON`);
      }
    });
}

// Write only when the bytes would change: a second collection against the same
// answers then touches nothing at all.
function writeIfChanged(file, text) {
  if (fs.existsSync(file) && fs.readFileSync(file, "utf8") === text) return;
  fs.writeFileSync(file, text);
}

const writeJson = (file, value) => writeIfChanged(file, JSON.stringify(value, null, 2) + "\n");
const writeJsonl = (file, rows) =>
  writeIfChanged(file, rows.length ? rows.map((r) => JSON.stringify(r)).join("\n") + "\n" : "");

// The metric manifest's series names (its `taxonomy`, e.g. `encoder.fps`) and the
// stored sample key and source each one becomes in a scenario directory. Bench-mode
// keys ride in a browser sample, as everywhere else.
function seriesKeys(file = path.join(ROOT, "docs/session-trace/metrics.json")) {
  const map = new Map();
  for (const m of JSON.parse(fs.readFileSync(file, "utf8")).metrics) {
    if (!m.taxonomy) continue;
    map.set(m.taxonomy, { key: m.key, source: m.source === "bench" ? "browser" : m.source });
  }
  return map;
}

function metricRows(bundle, keys) {
  const rows = new Map();
  const unknown = [];
  for (const [name, points] of Object.entries(bundle?.series ?? {})) {
    const m = keys.get(name);
    if (!m) {
      unknown.push(name);
      continue;
    }
    for (const p of Array.isArray(points) ? points : []) {
      if (typeof p?.ts_unix_ms !== "number" || typeof p.v !== "number" || !Number.isFinite(p.v)) continue;
      const id = `${m.source}|${p.ts_unix_ms}`;
      if (!rows.has(id)) rows.set(id, { kind: "metric", source: m.source, ts_unix_ms: p.ts_unix_ms, metrics: {} });
      rows.get(id).metrics[m.key] = p.v;
    }
  }
  return { rows: [...rows.values()], unknown };
}

// Same source + ts is the same row, so the client half's own sample wins and the
// host's is not appended twice. Sorted by ts then source, as the format asks.
function mergeMetrics(existing, added) {
  const seen = new Set(existing.map((r) => `${r?.source}|${r?.ts_unix_ms}`));
  const out = existing.slice();
  for (const r of added) {
    const id = `${r.source}|${r.ts_unix_ms}`;
    if (!seen.has(id)) out.push(r);
  }
  const cmp = (a, b) => (a.source < b.source ? -1 : a.source > b.source ? 1 : 0);
  return out.sort((a, b) => (a?.ts_unix_ms ?? 0) - (b?.ts_unix_ms ?? 0) || cmp(a, b));
}

function mergeEvents(existing, added) {
  const id = (e) => `${e?.ts_unix_ms}|${e?.type}|${e?.source}`;
  const seen = new Set(existing.map(id));
  const out = existing.slice();
  for (const e of added) if (!seen.has(id(e))) out.push(e);
  const byText = (a, b) => (a < b ? -1 : a > b ? 1 : 0);
  return out.sort(
    (a, b) =>
      (a?.ts_unix_ms ?? 0) - (b?.ts_unix_ms ?? 0) ||
      byText(String(a?.type), String(b?.type)) ||
      byText(String(a?.source), String(b?.source)),
  );
}

// `bench_run.sh` writes the SAME read into conditions.json: booleans as on/off,
// everything else stringified, with the ladder aliases quasar-bench compares by
// name. `effective` is null until an amendment-aware agent has reported, and an
// absent key is "not measured", never {}.
const CONDITION_ALIAS = {
  abr_ladder_resolution: "ladder_resolution",
  abr_ladder_fps: "ladder_fps",
  abr_ladder_floor_follows_rung: "floor_follows_rung",
};

function conditions(settings) {
  const eff = settings?.effective;
  if (!isObj(eff)) {
    return { unknown: { effective: "the host has not reported its effective settings" } };
  }
  const projected = {};
  for (const k of Object.keys(eff).sort()) {
    const v = eff[k];
    const flag = typeof v === "boolean" ? (v ? "on" : "off") : String(v);
    projected[k] = flag;
    if (CONDITION_ALIAS[k]) projected[CONDITION_ALIAS[k]] = flag;
  }
  return { effective: projected };
}

// The `session.json` shape scripts/dx/bench_submit.py reads (session_soak_driver.py
// builds the same record from GET /v1/sessions/{id}).
function sessionRow(sess) {
  const st = sess?.stream ?? {};
  const row = { kind: "session", id: sess?.id };
  for (const k of ["state", "app_id", "host_id", "profile_id"]) if (sess?.[k] != null) row[k] = sess[k];
  if (typeof st.width === "number" && typeof st.height === "number") row.launch = [st.width, st.height];
  for (const k of ["fps", "bitrate_kbps", "codec", "external_resize_supported"]) if (st[k] != null) row[k] = st[k];
  if (Array.isArray(st.rungs)) row.rungs = st.rungs.map((r) => [Number(r[0]), Number(r[1])]);
  const start = Date.parse(sess?.started_at ?? "");
  const end = Date.parse(sess?.ended_at ?? "");
  if (!Number.isNaN(start)) row.started_at_ms = start;
  if (!Number.isNaN(end)) {
    row.ended_at_ms = end;
    if (!Number.isNaN(start)) row.duration_s = Math.round((end - start) / 100) / 10;
  }
  return row;
}

function identity(version, commit, builtAt) {
  const out = {};
  if (version != null) out.version = version;
  if (commit != null) out.commit = commit;
  if (builtAt != null) out.built_at = builtAt;
  return out;
}

const trimBase = (baseUrl) => String(baseUrl).replace(/\/+$/, "");

function getJson(baseUrl, token, fetchFn, route) {
  return (async () => {
    let res;
    try {
      res = await fetchFn(`${trimBase(baseUrl)}${route}`, {
        headers: { Authorization: `Bearer ${token}`, Accept: "application/json" },
      });
    } catch (e) {
      throw new Error(`GET ${route} could not be made (${e.message}) — is ${trimBase(baseUrl)} reachable?`);
    }
    if (res.status === 401 || res.status === 403) {
      const e = new Error(
        `the admin bearer was rejected (HTTP ${res.status} on GET ${route}). ` +
          `Next: scripts/dx/admin_token.sh --fresh`,
      );
      e.code = "unauthorized";
      throw e;
    }
    let body = null;
    if (res.status === 200) {
      try {
        body = await res.json();
      } catch {
        body = null;
      }
    }
    return { status: res.status, body };
  })();
}

/**
 * Fetch the host half of `dir` and write it in. `fetch` defaults to Node's own; a
 * test passes a canned in-process one.
 *
 * Returns `{ complete, problems, scenarios, sessions, collected }`. Throws on a
 * rejected bearer (`code: "unauthorized"`) and on a directory that is not a bundle.
 */
export async function collect(dir, { baseUrl, token, fetch: fetchFn = globalThis.fetch } = {}) {
  if (!token) throw new Error("no admin bearer: QUASAR_ADMIN_TOKEN is empty");
  if (!baseUrl) throw new Error("no --base-url");
  const manifest = readJson(path.join(dir, "manifest.json"));
  if (!isObj(manifest)) throw new Error("manifest.json: not an object");
  const scenarios = Array.isArray(manifest.scenarios) ? manifest.scenarios.filter((n) => typeof n === "string") : [];
  if (!scenarios.length) throw new Error("manifest.json: no scenario directories to collect");
  const builds = readJson(path.join(dir, "builds.json"));
  if (!isObj(builds)) throw new Error("builds.json: not an object");
  builds.components = isObj(builds.components) ? builds.components : {};
  builds.unknown = isObj(builds.unknown) ? builds.unknown : {};
  const manifestKeys = seriesKeys();
  const get = (route) => getJson(baseUrl, token, fetchFn, route);

  let sessions = 0;
  let collected = 0;
  const hosts = new Map();
  const incomplete = [];

  for (const name of scenarios) {
    const scDir = path.join(dir, name);
    const scenario = readJson(path.join(scDir, "scenario.json"));
    const ids = Array.isArray(scenario?.sessions) ? scenario.sessions.filter((s) => typeof s === "string") : [];
    if (!ids.length) {
      writeJson(path.join(scDir, "host-half.json"), { collected: true });
      continue;
    }

    // Fetch everything first: a scenario is written whole or not at all.
    const docs = [];
    let reason = null;
    sessions += ids.length;
    for (const id of ids) {
      const traceRoute = `/v1/admin/sessions/${id}/diagnostic-bundle`;
      const diag = await get(traceRoute);
      if (diag.status === 404) {
        reason = `session ${id}: the stack no longer holds it or its trace (HTTP 404 on GET ${traceRoute})`;
        break;
      }
      if (diag.status !== 200 || !isObj(diag.body)) {
        reason = `session ${id}: GET ${traceRoute} answered HTTP ${diag.status}`;
        break;
      }
      const row = await get(`/v1/sessions/${id}`);
      if (row.status !== 200 || !isObj(row.body?.session)) {
        reason = `session ${id}: GET /v1/sessions/${id} answered HTTP ${row.status}`;
        break;
      }
      const hostId = row.body.session.host_id;
      if (!hostId) {
        reason = `session ${id}: its row names no host, so there are no settings or agent identity to read`;
        break;
      }
      const settings = await get(`/v1/admin/hosts/${hostId}/settings`);
      if (settings.status !== 200 || !isObj(settings.body)) {
        reason = `session ${id}: GET /v1/admin/hosts/${hostId}/settings answered HTTP ${settings.status}`;
        break;
      }
      const host = await get(`/v1/hosts/${hostId}`);
      if (host.status !== 200 || !isObj(host.body)) {
        reason = `session ${id}: GET /v1/hosts/${hostId} answered HTTP ${host.status}`;
        break;
      }
      docs.push({ id, diag: diag.body, session: row.body.session, settings: settings.body, host: host.body });
    }

    if (reason) {
      incomplete.push({ scenario: name, reason });
      writeJson(path.join(scDir, "host-half.json"), { collected: false, reason: redact(reason, token) });
      continue;
    }

    const { rows, unknown } = metricRows(docs[0].diag, manifestKeys);
    docs.forEach((doc, i) => {
      // The first session is the scenario's own (session.json and friends sit at the
      // scenario level, as bench_submit.py expects); any further one keeps its own.
      const out = i === 0 ? scDir : path.join(scDir, "sessions", doc.id);
      fs.mkdirSync(out, { recursive: true });
      writeJson(path.join(out, "diagnostic-bundle.json"), redact(doc.diag, token));
      writeJson(path.join(out, "session.json"), redact(sessionRow(doc.session), token));
      writeJson(path.join(out, "conditions.json"), redact(conditions(doc.settings), token));
      const hostRows = i === 0 ? rows : metricRows(doc.diag, manifestKeys).rows;
      const metrics = mergeMetrics(readJsonl(path.join(out, "metrics.jsonl")), hostRows);
      writeJsonl(path.join(out, "metrics.jsonl"), metrics);
    });

    const traceFile = path.join(scDir, "trace.json");
    const prev = fs.existsSync(traceFile) ? readJson(traceFile) : {};
    writeJson(
      traceFile,
      redact(
        {
          session_id: docs[0].id,
          window: docs[0].diag.window ?? prev.window ?? null,
          clock: docs[0].diag.clock ?? prev.clock ?? null,
          events: mergeEvents(Array.isArray(prev.events) ? prev.events : [], docs[0].diag.events ?? []),
          series: prev.series ?? {},
        },
        token,
      ),
    );
    writeJson(
      path.join(scDir, "host-half.json"),
      redact({ collected: true, ...(unknown.length ? { series_not_in_manifest: unknown.sort() } : {}) }, token),
    );
    collected += ids.length;
    for (const doc of docs) {
      const h = doc.host;
      hosts.set(doc.session.host_id, identity(h.agent_version, h.source_commit, h.built_at));
    }
  }

  // The two builds a host half can name. Everything else stays as the client half
  // wrote it; a component that cannot be read stays null with its reason.
  const id = await get("/v1/admin/platform/identity");
  if (id.status === 200 && isObj(id.body?.identity)) {
    const i = id.body.identity;
    builds.components.control_plane = identity(i.version, i.source_commit, i.built_at);
    delete builds.unknown.control_plane;
  } else if (!builds.components.control_plane) {
    builds.components.control_plane = null;
    builds.unknown.control_plane = `GET /v1/admin/platform/identity answered HTTP ${id.status}`;
  }

  const distinct = [...hosts.values()].map((v) => JSON.stringify(v));
  if (distinct.length === 1) {
    builds.components.node_agent = JSON.parse(distinct[0]);
    delete builds.unknown.node_agent;
  } else if (!builds.components.node_agent) {
    builds.components.node_agent = null;
    builds.unknown.node_agent =
      distinct.length === 0
        ? "no session was collected, so no host agent identity was read"
        : "the sessions ran on hosts with different agent builds";
  }

  const complete = incomplete.length === 0;
  manifest.halves = isObj(manifest.halves) ? manifest.halves : {};
  manifest.halves.host = complete;
  writeJson(path.join(dir, "manifest.json"), manifest);
  writeJson(path.join(dir, "builds.json"), builds);

  return { complete, incomplete, problems: validateBundle(dir), scenarios: scenarios.length, sessions, collected };
}

function parseArgs(argv) {
  const args = { dir: null, baseUrl: null };
  for (let i = 0; i < argv.length; i += 1) {
    const a = argv[i];
    if (a === "--base-url") {
      args.baseUrl = argv[(i += 1)] ?? null;
    } else if (a.startsWith("--base-url=")) {
      args.baseUrl = a.slice("--base-url=".length);
    } else if (a === "-h" || a === "--help") {
      args.help = true;
    } else if (!args.dir && !a.startsWith("-")) {
      args.dir = a;
    } else {
      args.bad = a;
    }
  }
  return args;
}

const USAGE = 'usage: QUASAR_ADMIN_TOKEN=<bearer> collect.mjs <bundle-dir> --base-url <stack url>';

/** The CLI. Returns the exit code so a test can drive it with a canned `fetch`. */
export async function main(argv, { fetch: fetchFn = globalThis.fetch, env = process.env } = {}) {
  const args = parseArgs(argv);
  if (args.help) {
    console.log(USAGE);
    return EXIT.ok;
  }
  if (args.bad || !args.dir || !args.baseUrl) {
    console.error(args.bad ? `unexpected argument '${args.bad}'` : "a bundle directory and --base-url are both required");
    console.error(USAGE);
    console.log(`RESULT status=error target=e2e-collect reason=usage`);
    return EXIT.error;
  }
  if (!fs.statSync(args.dir, { throwIfNoEntry: false })?.isDirectory()) {
    console.error(`${args.dir}: not a directory (a zip is not read; unzip it first)`);
    console.log(`RESULT status=error target=e2e-collect reason=not-a-directory`);
    return EXIT.error;
  }
  const token = env.QUASAR_ADMIN_TOKEN ?? "";
  if (!token) {
    console.error(
      "no admin bearer: set QUASAR_ADMIN_TOKEN (the ladder that prints one is scripts/dx/admin_token.sh)",
    );
    console.log(`RESULT status=error target=e2e-collect reason=no-token`);
    return EXIT.error;
  }

  let result;
  try {
    result = await collect(args.dir, { baseUrl: args.baseUrl, token, fetch: fetchFn });
  } catch (e) {
    console.error(e.message);
    const reason = e.code === "unauthorized" ? "unauthorized" : "collect-failed";
    console.log(`RESULT status=error target=e2e-collect reason=${reason}`);
    return EXIT.error;
  }

  if (result.problems.length) {
    for (const p of result.problems) console.error(p);
    console.log(`RESULT status=failed target=e2e-collect errors=${result.problems.length}`);
    return EXIT.invalid;
  }
  if (!result.complete) {
    for (const i of result.incomplete) console.error(`INCOMPLETE ${i.scenario}: ${i.reason}`);
    console.log(
      `RESULT status=failed target=e2e-collect scenarios=${result.scenarios} sessions=${result.sessions} collected=${result.collected}`,
    );
    return EXIT.incomplete;
  }
  console.log(
    `RESULT status=ok target=e2e-collect scenarios=${result.scenarios} sessions=${result.sessions} collected=${result.collected}`,
  );
  return EXIT.ok;
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  process.exitCode = await main(process.argv.slice(2));
}
