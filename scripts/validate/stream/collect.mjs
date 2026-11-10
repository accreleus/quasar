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
// that retention has deleted) is recorded as not collected, never as empty.
//
// Exit: 0 every session collected and the bundle valid, 1 the bundle is not valid,
//       2 usage or the bearer was rejected, 3 incomplete (some session could not be
//       collected — the reason is in that scenario's host-half.json).
import crypto from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { SOURCES, isCredentialName, manifestKeys, validateBundle } from "./bundle.mjs";

export const EXIT = { ok: 0, invalid: 1, error: 2, incomplete: 3 };

// A session or host id is a UUID, and nothing else. A bundle may come from another
// machine: a value from it goes into an admin-authenticated request path and into a
// write path, so anything that is not this shape is refused before it is used.
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
const isId = (v) => typeof v === "string" && UUID.test(v);
const clip = (v) => (typeof v === "string" ? v.slice(0, 80) : String(v).slice(0, 80));

// The same rule bundle.mjs applies when it reads a bundle back: an address is four
// in-range octets, except loopback, a documentation network and x.0.0.0 (which is
// how a browser's version reads).
const IPV4 = /(?<![\d.])(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})(?!\d|\.\d)/g;
const DOC_NETS = ["192.0.2.", "198.51.100.", "203.0.113."]; // RFC 5737

const isObj = (v) => v !== null && typeof v === "object" && !Array.isArray(v);
const num = (v) => typeof v === "number" && Number.isFinite(v);

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

/**
 * A deep copy of `value` with credential-named fields dropped, addresses replaced, the exact
 * bearer replaced everywhere and keys sorted. Objects are built with no prototype, so a
 * `__proto__` key from an answer is an ordinary key and cannot reach Object.prototype.
 * Limit (declined): a bearer base64-, URL-encoded or split inside an answer is not recognised.
 */
export function redact(value, token) {
  if (typeof value === "string") return redactText(value, token);
  if (Array.isArray(value)) return value.map((v) => redact(v, token));
  if (isObj(value)) {
    const out = Object.create(null);
    for (const k of Object.keys(value).sort()) {
      if (!isCredentialName(k)) out[redactText(k, token)] = redact(value[k], token);
    }
    return out;
  }
  return value;
}

/** A problem with what an answer or a file held, never with the bearer: that scenario is not collected. */
class Unreadable extends Error {}
const need = (cond, what) => {
  if (!cond) throw new Unreadable(what);
};

/**
 * The ONE place the collector writes a file (JSON, or JSONL when the name ends in .jsonl and
 * the value is a list of rows). The value is redacted and serialised with sorted keys, written
 * to a temporary file beside the target and renamed over it, so a hard link or symlink already
 * at the target is replaced, never written through. The target must resolve inside the bundle.
 */
function writer(dir, token) {
  const lexical = path.resolve(dir);
  const root = fs.realpathSync(dir);
  return (file, value) => {
    const target = path.resolve(file);
    need(target.startsWith(lexical + path.sep), "a write left the bundle");
    fs.mkdirSync(path.dirname(target), { recursive: true });
    const parent = fs.realpathSync(path.dirname(target));
    need(parent === root || parent.startsWith(root + path.sep), "a write left the bundle");
    const text = file.endsWith(".jsonl")
      ? value.map((r) => JSON.stringify(redact(r, token)) + "\n").join("")
      : JSON.stringify(redact(value, token), null, 2) + "\n";
    const tmp = path.join(parent, `.${path.basename(target)}.${crypto.randomBytes(6).toString("hex")}.tmp`);
    try {
      fs.writeFileSync(tmp, text, { flag: "wx" });
      fs.renameSync(tmp, path.join(parent, path.basename(target)));
    } catch (e) {
      fs.rmSync(tmp, { force: true });
      throw e;
    }
  };
}

function readJson(file) {
  try {
    return JSON.parse(fs.readFileSync(file, "utf8"));
  } catch (e) {
    throw new Unreadable(`${file}: ${e.code === "ENOENT" ? "missing" : "not JSON"}`);
  }
}

/** The rows of a JSONL file the collector will merge with; [] when there is none. */
function readRows(file) {
  if (!fs.existsSync(file)) return [];
  return fs
    .readFileSync(file, "utf8")
    .split("\n")
    .filter((l) => l.trim())
    .map((l, i) => {
      let row;
      try {
        row = JSON.parse(l);
      } catch {
        throw new Unreadable(`${file}:${i + 1}: not JSON`);
      }
      need(isObj(row) && num(row.ts_unix_ms) && isObj(row.metrics), `${file}:${i + 1}: needs ts_unix_ms and metrics`);
      return row;
    });
}

// The row's identity is its source and ts. A host sample joins the client's row for the same
// moment key by key: the client's value wins for a key both have, the host's other keys are
// added. Sorted by ts then source, as the format asks.
function mergeMetrics(existing, added) {
  const byId = new Map();
  const out = [];
  for (const r of [...existing, ...added]) {
    const id = `${r.source}|${r.ts_unix_ms}`;
    let row = byId.get(id);
    if (!row) {
      row = { ...r, metrics: Object.create(null) };
      byId.set(id, row);
      out.push(row);
    }
    for (const [k, v] of Object.entries(r.metrics)) if (!Object.hasOwn(row.metrics, k)) row.metrics[k] = v;
  }
  const cmp = (a, b) => (a.source < b.source ? -1 : a.source > b.source ? 1 : 0);
  return out.sort((a, b) => a.ts_unix_ms - b.ts_unix_ms || cmp(a, b));
}

const eventId = (e) => `${e.ts_unix_ms}|${e.type}|${e.source}`;
function mergeEvents(...lists) {
  const seen = new Set();
  const out = [];
  for (const e of lists.flat()) {
    if (!seen.has(eventId(e))) {
      seen.add(eventId(e));
      out.push(e);
    }
  }
  const byText = (a, b) => (a < b ? -1 : a > b ? 1 : 0);
  return out.sort((a, b) => a.ts_unix_ms - b.ts_unix_ms || byText(a.type, b.type) || byText(a.source, b.source));
}

// bench_run.sh writes the SAME read into conditions.json: its flag() — a value that reads
// "true" or "false" in any case becomes on/off, anything else stays as it is — with the
// ladder aliases quasar-bench compares by name. `effective` is a map of strings, or null
// until an amendment-aware agent has reported; an absent key is "not measured", never {}.
const CONDITION_ALIAS = {
  abr_ladder_resolution: "ladder_resolution",
  abr_ladder_fps: "ladder_fps",
  abr_ladder_floor_follows_rung: "floor_follows_rung",
};
const flag = (v) => (String(v).toLowerCase() === "true" ? "on" : String(v).toLowerCase() === "false" ? "off" : String(v));

function conditions(effective) {
  if (effective === null) return { unknown: { effective: "the host has not reported its effective settings" } };
  const projected = Object.create(null);
  for (const k of Object.keys(effective).sort()) {
    projected[k] = flag(effective[k]);
    if (CONDITION_ALIAS[k]) projected[CONDITION_ALIAS[k]] = flag(effective[k]);
  }
  return { effective: projected };
}

// The `session.json` shape scripts/dx/bench_submit.py reads (session_soak_driver.py builds the
// same record from GET /v1/sessions/{id}), plus the fields the 0.6 parity gate compares,
// named as the session row names them.
function sessionRow(s) {
  const st = s.stream;
  const row = { kind: "session", id: s.id };
  for (const k of ["state", "app_id", "host_id", "profile_id", "transport", "negotiated_codec", "pyrowave"]) {
    if (s[k] !== undefined) row[k] = s[k];
  }
  if (num(st.width) && num(st.height)) row.launch = [st.width, st.height];
  for (const k of ["fps", "bitrate_kbps", "codec", "external_resize_supported"]) if (st[k] != null) row[k] = st[k];
  row.rungs = st.rungs.map(([w, h]) => [w, h]);
  const start = Date.parse(s.started_at ?? "");
  const end = Date.parse(s.ended_at ?? "");
  if (!Number.isNaN(start)) row.started_at_ms = start;
  if (!Number.isNaN(end)) {
    row.ended_at_ms = end;
    if (!Number.isNaN(start)) row.duration_s = Math.round((end - start) / 100) / 10;
  }
  return row;
}

/** built_at/commit/version as the answer holds them (in key order, so two reads compare as text); null when it holds none ("not read"). */
function identity(o, versionKey) {
  const out = {};
  for (const [to, from] of [["built_at", "built_at"], ["commit", "source_commit"], ["version", versionKey]]) {
    if (typeof o[from] === "string" && o[from]) out[to] = o[from];
  }
  return Object.keys(out).length ? out : null;
}

const trimBase = (baseUrl) => String(baseUrl).replace(/\/+$/, "");

// Every request: no redirect is followed (a 3xx is not an answer), and the bearer goes only
// to the base URL the operator gave.
async function getJson(baseUrl, token, fetchFn, route) {
  let res;
  try {
    res = await fetchFn(`${trimBase(baseUrl)}${route}`, {
      headers: { Authorization: `Bearer ${token}`, Accept: "application/json" },
      redirect: "error",
    });
  } catch (e) {
    if (/redirect/i.test(`${e.message} ${e.cause?.message}`)) throw new Unreadable(`GET ${route} answered a redirect, which is never followed`);
    throw new Error(`GET ${route} could not be made (${e.message}) — is ${trimBase(baseUrl)} reachable?`);
  }
  if (res.status === 401 || res.status === 403) {
    const e = new Error(
      `the admin bearer was rejected (HTTP ${res.status} on GET ${route}). Next: scripts/dx/admin_token.sh --fresh`,
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
}

const MIN = 60_000;
const PAD = 5_000; // the window around a session, as session_soak_driver.py and bench_run.sh read it
const PAGE = 1000; // the server's sample cap per read (telemetry.clampLimit; traceReadLimit)
const MAX_PAGES = 200;

// What one session's answers hold, all read and shape-checked before anything is written.
// Every route and shape here is the handler's: diagnostic_handler.go (resolveWindow,
// handleDiagnosticBundle, handleAdminTelemetryRead), metrics_handler.go (handleAdminMetrics),
// handler.go (handleGet, sessionResp), crud/handler.go (handleGetHost), hostcfg/handler.go
// (handleGet) and platform/handler.go (handleIdentity).
async function fetchSession(get, id, now) {
  const sid = encodeURIComponent(id);
  const ok = async (route) => {
    const r = await get(route);
    need(r.status !== 404, `GET ${route} answered HTTP 404: the stack no longer holds it`);
    need(r.status === 200, `GET ${route} answered HTTP ${r.status}`);
    need(isObj(r.body), `GET ${route} answered a body that is not a JSON object`);
    return r.body;
  };

  // handler.go handleGet: { "session": sessionResp }
  const s = (await ok(`/v1/sessions/${sid}`)).session;
  need(isObj(s) && s.id === id, "its row is not the session that was asked for");
  need(isObj(s.stream), "its row has no stream object");
  need(
    Array.isArray(s.stream.rungs) && s.stream.rungs.every((r) => Array.isArray(r) && r.length === 2 && r.every(num)),
    "its row's rungs are not [width, height] pairs",
  );
  for (const k of ["started_at", "ended_at"]) {
    need(s[k] == null || (typeof s[k] === "string" && !Number.isNaN(Date.parse(s[k]))), `its row's ${k} is not a time`);
  }
  need(s.host_id != null, "its row names no host, so there are no settings or agent identity to read");
  need(isId(s.host_id), "its row's host id is not a UUID");
  need(s.started_at != null, "it never started, so there is no trace window to ask for");

  // The window the server's resolveWindow is given explicitly (its default is the 5 minutes
  // ending NOW, which is empty days later), in reads of at most 10 minutes; a read that comes
  // back full (the server caps a series at 1000 points) is split, never trusted.
  const from = Date.parse(s.started_at) - PAD;
  const to = (s.ended_at != null ? Date.parse(s.ended_at) : now()) + PAD;
  const reads = [];
  const span = async (a, b) => {
    const q = `?from=${a}&to=${b}`;
    // diagnostic_handler.go handleDiagnosticBundle: trace, window, clock, series{name:[{ts_unix_ms,v}]}, events
    const diag = await ok(`/v1/admin/sessions/${sid}/diagnostic-bundle${q}`);
    need(isObj(diag.series), "the diagnostic bundle has no series object");
    for (const pts of Object.values(diag.series)) {
      need(Array.isArray(pts) && pts.every((p) => isObj(p) && num(p.ts_unix_ms) && num(p.v)), "the diagnostic bundle's series are not points");
    }
    need(Array.isArray(diag.events) && diag.events.every((e) => isObj(e) && num(e.ts_unix_ms)), "the diagnostic bundle's events are not events");
    need(diag.trace?.session_id === undefined || diag.trace.session_id === id, "the diagnostic bundle is another session's");
    // handleAdminTelemetryRead projectionEvents: { window, events } — the RAW events, not clock-aligned
    const ev = await ok(`/v1/admin/sessions/${sid}/trace/events${q}`);
    need(
      Array.isArray(ev.events) && ev.events.every((e) => isObj(e) && num(e.ts_unix_ms) && typeof e.type === "string" && typeof e.source === "string"),
      "the trace events are not events",
    );
    const full = ev.events.length >= PAGE || Object.values(diag.series).some((p) => p.length >= PAGE);
    if (!full) return reads.push({ diag, events: ev.events });
    need(b - a >= 4 * MIN, "its trace is denser than one read holds, even in the smallest window");
    const mid = Math.floor((a + b) / 2);
    await span(a, mid);
    await span(mid, b);
  };
  for (let a = from; a < to; a += 10 * MIN) await span(a, Math.min(a + 10 * MIN, to));

  // handleAdminMetrics: { items: [{source, ts_unix_ms, metrics}], next_cursor }, newest first.
  // The RAW samples, every key and the original timestamps; paged until the cursor ends.
  const samples = [];
  let cursor = "";
  for (let page = 0; ; page += 1) {
    need(page < MAX_PAGES, "it has more samples than one collection reads");
    const m = await ok(`/v1/admin/sessions/${sid}/metrics?limit=${PAGE}${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""}`);
    need(
      Array.isArray(m.items) && m.items.every((i) => isObj(i) && typeof i.source === "string" && num(i.ts_unix_ms) && isObj(i.metrics)),
      "the metrics are not samples",
    );
    samples.push(...m.items);
    cursor = m.next_cursor;
    if (cursor == null) break;
    need(typeof cursor === "string" && cursor !== "", "the metrics cursor is not a cursor");
  }

  const hasData =
    samples.length > 0 ||
    reads.some((r) => r.events.length > 0 || r.diag.events.length > 0 || Object.values(r.diag.series).some((p) => p.length > 0));
  need(hasData, "the trace is no longer kept (telemetry is deleted a day after a session ends)");

  const hid = encodeURIComponent(s.host_id);
  // hostcfg handleGet: { resolved, overrides, effective: map[string]string | null, codecs, pending_restart }
  const settings = await ok(`/v1/admin/hosts/${hid}/settings`);
  need(
    settings.effective === null || (isObj(settings.effective) && Object.values(settings.effective).every((v) => v === null || typeof v !== "object")),
    "the host settings' effective map is not a map of values",
  );
  // crud handleGetHost: { "host": hostResp }
  const host = (await ok(`/v1/hosts/${hid}`)).host;
  need(isObj(host), "the host row is not an object");

  return { id, row: s, from, to, reads, samples, effective: settings.effective, agent: identity(host, "agent_version") };
}

// The metric keys and sources the format admits. Anything else the API sent is dropped and
// listed in host-half.json, never written.
function hostRows(samples, keys, dropped) {
  const rows = new Map();
  for (const smp of samples) {
    if (!SOURCES.includes(smp.source)) {
      dropped.sources.add(clip(smp.source));
      continue;
    }
    const id = `${smp.source}|${smp.ts_unix_ms}`;
    if (!rows.has(id)) rows.set(id, { kind: "metric", source: smp.source, ts_unix_ms: smp.ts_unix_ms, metrics: Object.create(null) });
    const row = rows.get(id);
    for (const [k, v] of Object.entries(smp.metrics)) {
      if (keys[smp.source] && !keys[smp.source].has(k)) dropped.keys.add(`${smp.source}.${clip(k)}`);
      else if (!Object.hasOwn(row.metrics, k)) row.metrics[k] = v;
    }
  }
  return [...rows.values()];
}

// The diagnostic bundle as the artifact: the reads' series and events folded together (a
// session longer than one read), everything else as the first read returned it.
function foldDiagnostic(reads, from, to) {
  const series = new Map();
  const events = [];
  for (const { diag } of reads) {
    for (const [name, pts] of Object.entries(diag.series)) {
      if (!series.has(name)) series.set(name, new Map());
      for (const p of pts) series.get(name).set(p.ts_unix_ms, p);
    }
    events.push(diag.events);
  }
  return {
    ...reads[0].diag,
    window: { from_ms: from, to_ms: to },
    series: Object.fromEntries([...series].map(([n, m]) => [n, [...m.values()].sort((a, b) => a.ts_unix_ms - b.ts_unix_ms)])),
    events: mergeEvents(...events),
  };
}

/**
 * Fetch the host half of `dir` and write it in. `fetch` defaults to Node's own; a test passes
 * a canned in-process one, and `now` for the clock.
 *
 * A scenario whose host-half.json says `collected: true` is never fetched again; only the others
 * are retried. Everything for a scenario is fetched and shape-checked, then every output is
 * computed, and only then written.
 *
 * Returns `{ complete, problems, incomplete, scenarios, sessions, collected }`, with
 * `refusedBefore: true` when the bundle was refused before any request or write. Throws on a
 * rejected bearer (`code: "unauthorized"`) and on a value from the bundle that is not a UUID
 * (`code: "usage"`).
 */
export async function collect(dir, { baseUrl, token, fetch: fetchFn = globalThis.fetch, now = Date.now } = {}) {
  if (!token) throw new Error("no admin bearer: QUASAR_ADMIN_TOKEN is empty");
  if (!baseUrl) throw new Error("no --base-url");
  const refuse = (problems) => ({ complete: false, refusedBefore: true, problems, incomplete: [], scenarios: 0, sessions: 0, collected: 0 });

  // The bundle is untrusted input and may come from another machine. Its format is checked
  // FIRST — before a scenario name becomes a write path and before a session id becomes an
  // admin request. A client-only bundle is a valid bundle, so this costs nothing.
  const bad = validateBundle(dir);
  if (bad.length) return refuse(bad);

  const manifest = readJson(path.join(dir, "manifest.json"));
  const builds = readJson(path.join(dir, "builds.json"));
  builds.unknown = isObj(builds.unknown) ? builds.unknown : {};
  need(manifest.scenarios.length > 0, "manifest.json: no scenario directories to collect");

  // The whole plan, before the first request: what each scenario names, what is already
  // collected, and every existing file the collector will merge with or replace.
  const plan = [];
  const problems = [];
  const keys = manifestKeys();
  for (const name of manifest.scenarios) {
    const scDir = path.join(dir, name);
    const ids = readJson(path.join(scDir, "scenario.json")).sessions;
    for (const id of ids) {
      if (!isId(id)) {
        const e = new Error(`${name}/scenario.json: '${clip(id)}' is not a session id (a session id is a UUID); nothing was requested`);
        e.code = "usage";
        throw e;
      }
    }
    const halfFile = path.join(scDir, "host-half.json");
    let prior = null;
    try {
      if (fs.existsSync(halfFile)) {
        prior = readJson(halfFile);
        need(isObj(prior), "not an object");
      }
    } catch (e) {
      problems.push(`${name}/host-half.json: ${e.message.replace(/^.*: /, "")}`);
    }
    const item = { name, scDir, ids, prior, outs: [] };
    if (prior?.collected !== true && ids.length) {
      try {
        ids.forEach((id, i) => {
          const out = i === 0 ? scDir : path.join(scDir, "sessions", id);
          for (const p of [path.join(scDir, "sessions"), out]) need(!fs.existsSync(p) || fs.statSync(p).isDirectory(), `${path.relative(dir, p)} is not a directory`);
          for (const f of ["diagnostic-bundle.json", "session.json", "conditions.json", "metrics.jsonl", ...(i === 0 ? ["trace.json", "host-half.json"] : [])]) {
            const p = path.join(out, f);
            need(!fs.existsSync(p) || fs.lstatSync(p).isFile(), `${path.relative(dir, p)} is not a file`);
          }
          item.outs.push({ id, out, metrics: readRows(path.join(out, "metrics.jsonl")) });
        });
        const traceFile = path.join(scDir, "trace.json");
        item.trace = fs.existsSync(traceFile) ? readJson(traceFile) : {};
        need(isObj(item.trace) && (item.trace.events === undefined || (Array.isArray(item.trace.events) && item.trace.events.every((e) => isObj(e) && num(e.ts_unix_ms)))), `${name}/trace.json is not a trace`);
        need(item.trace.series === undefined || isObj(item.trace.series), `${name}/trace.json is not a trace`);
      } catch (e) {
        problems.push(e.message.startsWith(dir) ? e.message.slice(dir.length + 1) : e.message);
      }
    }
    plan.push(item);
  }
  if (problems.length) return refuse(problems);

  const write = writer(dir, token);
  const get = (route) => getJson(baseUrl, token, fetchFn, route);
  const todo = plan.filter((p) => p.prior?.collected !== true && p.ids.length);
  const state = new Map(); // scenario -> { collected, agent } once this run is done
  for (const p of plan) state.set(p.name, p.prior?.collected === true ? { collected: true, agent: p.prior.node_agent } : { collected: !p.ids.length, agent: undefined });
  const incomplete = [];

  // A scenario that names no session has nothing to collect, and says so.
  const empty = plan.filter((p) => !p.ids.length && p.prior?.collected !== true);
  if (todo.length || empty.length) {
    for (const p of empty) write(path.join(p.scDir, "host-half.json"), { collected: true });

    // platform/handler.go handleIdentity: { "identity": {...} }; read once, before any scenario is written
    let controlPlane = null;
    let cpReason = null;
    if (todo.length) {
      try {
        const r = await get("/v1/admin/platform/identity");
        need(r.status === 200 && isObj(r.body?.identity), `GET /v1/admin/platform/identity answered HTTP ${r.status}`);
        controlPlane = identity(r.body.identity, "version");
        need(controlPlane, "GET /v1/admin/platform/identity held no version, commit or build time");
      } catch (e) {
        if (!(e instanceof Unreadable)) throw e;
        cpReason = e.message;
      }
    }

    for (const p of todo) {
      const docs = [];
      let reason = null;
      try {
        for (const id of p.ids) docs.push(await fetchSession(get, id, now));
      } catch (e) {
        if (!(e instanceof Unreadable)) throw e;
        reason = `session ${p.ids[docs.length]}: ${e.message}`;
      }
      if (reason) {
        incomplete.push({ scenario: p.name, reason });
        write(path.join(p.scDir, "host-half.json"), { collected: false, reason });
        continue;
      }

      // Decide every output, then write them all.
      const readAt = now();
      const dropped = { sources: new Set(), keys: new Set() };
      const writes = [];
      docs.forEach((d, i) => {
        const { out, metrics } = p.outs[i];
        writes.push(
          [path.join(out, "diagnostic-bundle.json"), foldDiagnostic(d.reads, d.from, d.to)],
          [path.join(out, "session.json"), sessionRow(d.row)],
          [path.join(out, "conditions.json"), conditions(d.effective)],
          [path.join(out, "metrics.jsonl"), mergeMetrics(metrics, hostRows(d.samples, keys, dropped))],
        );
      });
      const first = docs[0];
      writes.push([
        path.join(p.scDir, "trace.json"),
        {
          session_id: first.id,
          window: { from_ms: first.from, to_ms: first.to },
          clock: first.reads[0].diag.clock ?? p.trace.clock ?? null,
          events: mergeEvents(p.trace.events ?? [], ...first.reads.map((r) => r.events)),
          series: p.trace.series ?? {},
        },
      ]);
      const agents = new Set(docs.map((d) => JSON.stringify(d.agent)));
      const agent = agents.size === 1 ? JSON.parse([...agents][0]) : null;
      writes.push([
        path.join(p.scDir, "host-half.json"),
        {
          collected: true,
          read_at_ms: readAt,
          node_agent: agent,
          ...(dropped.sources.size ? { sources_not_in_bundle: [...dropped.sources].sort() } : {}),
          ...(dropped.keys.size ? { metric_keys_not_in_manifest: [...dropped.keys].sort() } : {}),
        },
      ]);
      for (const [file, value] of writes) write(file, value);
      state.set(p.name, { collected: true, agent });
    }

    // The two builds a host half can name; everything else stays as the client half wrote it.
    // A component that cannot be read is null with its reason, replacing whatever the client
    // half held.
    const setBuild = (name, value, why) => {
      builds.components[name] = value;
      if (value) delete builds.unknown[name];
      else builds.unknown[name] = why;
    };
    if (todo.length) setBuild("control_plane", controlPlane, cpReason);
    const agents = [...state.values()].filter((s) => s.collected && s.agent !== undefined);
    const distinct = new Set(agents.map((s) => JSON.stringify(s.agent)));
    setBuild(
      "node_agent",
      distinct.size === 1 && !distinct.has("null") ? JSON.parse([...distinct][0]) : null,
      agents.length ? "the collected sessions did not share one readable host agent build" : "no session was collected, so no host agent identity was read",
    );
    manifest.halves = isObj(manifest.halves) ? manifest.halves : {};
    manifest.halves.host = [...state.values()].every((s) => s.collected);
    write(path.join(dir, "manifest.json"), manifest);
    write(path.join(dir, "builds.json"), builds);
  }

  const total = plan.reduce((n, p) => n + p.ids.length, 0);
  return {
    complete: [...state.values()].every((s) => s.collected),
    incomplete,
    problems: validateBundle(dir),
    scenarios: plan.length,
    sessions: total,
    collected: plan.filter((p) => state.get(p.name).collected).reduce((n, p) => n + p.ids.length, 0),
  };
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
    const reason = e.code === "unauthorized" ? "unauthorized" : e.code === "usage" ? "usage" : "collect-failed";
    console.log(`RESULT status=error target=e2e-collect reason=${reason}`);
    return EXIT.error;
  }

  if (result.problems.length) {
    for (const p of result.problems) console.error(p);
    const why = result.refusedBefore ? " reason=invalid-before-collect" : "";
    console.log(`RESULT status=failed target=e2e-collect errors=${result.problems.length}${why}`);
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
