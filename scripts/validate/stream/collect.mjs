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
// A bundle that already claims a host half is refused unless --resume is given: everything
// in a bundle is untrusted, so "already collected" is never believed from the bundle alone.
//
// Exit: 0 every session collected and the bundle valid, 1 the bundle is not valid,
//       2 usage, a bundle that already has a host half, or the bearer was rejected,
//       3 incomplete (some session could not be collected: the reason is in that
//       scenario's host-half.json).
import crypto from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { SECRETS, SOURCES, isCredentialName, manifestKeys, validateBundle } from "./bundle.mjs";

export const EXIT = { ok: 0, invalid: 1, error: 2, incomplete: 3 };

// ── the shapes a projected value may have ───────────────────────────────────
// Every string the collector itself takes from an answer into session.json, conditions.json,
// host-half.json or builds.json passes the shape for its own field, or it is not written and is
// counted in host-half.json's not_projected: the collector never copies free text. These are
// field shapes, not patterns that hunt for a credential.
const IDENT = /^[A-Za-z0-9_.-]{1,64}$/; // a name: a state, a transport, a codec, a settings key
const VERSION = /^[A-Za-z0-9][A-Za-z0-9_.+-]{0,63}$/; // a build's version
const COMMIT = /^[0-9a-f]{7,40}$/i; // a build's commit
// A host setting's value. A path holds a "/" and free text a space: neither matches, and neither
// belongs in a bundle.
const SETTING = /^[A-Za-z0-9_.:+-]{1,64}$/;
// A session or host id is a UUID, and nothing else. A bundle may come from another
// machine: a value from it goes into an admin-authenticated request path and into a
// write path, so anything that is not this shape is refused before it is used.
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
const isId = (v) => typeof v === "string" && UUID.test(v);
const fits = (re, v) => typeof v === "string" && re.test(v);
/** A time as ISO, or null when the text is not one: what is written back is never the answer's own text. */
const isoTime = (v) => {
  const ms = typeof v === "string" ? Date.parse(v) : NaN;
  return Number.isNaN(ms) ? null : new Date(ms).toISOString();
};
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

// Free text from an answer (an error message, a log line) can spell a credential out
// ("authorization=...", "Bearer ..."): whatever the validator would call a secret is replaced.
const SECRET_TEXT = SECRETS.map(([re]) => new RegExp(re.source, `${re.flags}g`));

function redactText(text, token) {
  let out = text.replace(IPV4, (m) => (isAddress(m) ? "[address]" : m));
  if (token) out = out.split(token).join("[redacted]");
  for (const re of SECRET_TEXT) out = out.replace(re, "[redacted]");
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
 * The ONE place the collector writes a file (JSON, or JSONL for a list of rows): redacted, sorted
 * keys, to a temporary file beside the target. `commit` renames every staged file into place (a link
 * at the target is replaced, never written through); `discard` removes the temporaries and any
 * directory this call created. The target must resolve inside the bundle.
 */
function writer(dir, token) {
  const lexical = path.resolve(dir);
  const root = fs.realpathSync(dir);
  let staged = [];
  let made = [];
  const write = (file, value) => {
    const target = path.resolve(file);
    need(target.startsWith(lexical + path.sep), "a write left the bundle");
    const first = fs.mkdirSync(path.dirname(target), { recursive: true });
    if (first) made.push(first);
    const parent = fs.realpathSync(path.dirname(target));
    need(parent === root || parent.startsWith(root + path.sep), "a write left the bundle");
    const text = file.endsWith(".jsonl")
      ? value.map((r) => JSON.stringify(redact(r, token)) + "\n").join("")
      : JSON.stringify(redact(value, token), null, 2) + "\n";
    const tmp = path.join(parent, `.${path.basename(target)}.${crypto.randomBytes(6).toString("hex")}.tmp`);
    try {
      fs.writeFileSync(tmp, text, { flag: "wx" });
    } catch (e) {
      if (e.code !== "EEXIST") fs.rmSync(tmp, { force: true }); // EEXIST: the entry is not ours to remove
      throw e;
    }
    staged.push([tmp, path.join(parent, path.basename(target))]);
  };
  const discard = () => {
    for (const [tmp] of staged) fs.rmSync(tmp, { force: true });
    for (const d of made.reverse()) fs.rmSync(d, { recursive: true, force: true });
    staged = [];
    made = [];
  };
  const commit = () => {
    const todo = staged;
    staged = [];
    made = [];
    try {
      for (const [tmp, target] of todo) fs.renameSync(tmp, target);
    } catch (e) {
      for (const [tmp] of todo) fs.rmSync(tmp, { force: true });
      throw e;
    }
  };
  return { write, commit, discard, put: (file, value) => (write(file, value), commit()) };
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

// A key that is not a name is not written; a value is written only in a setting value's shape, so
// a host path or free text becomes "[omitted]". Both are counted once per answer value left out
// (an alias repeats the same value and is not a second one).
function conditions(effective, note) {
  if (effective === null) return { unknown: { effective: "the host has not reported its effective settings" } };
  const projected = Object.create(null);
  for (const k of Object.keys(effective).sort()) {
    if (!fits(IDENT, k)) {
      note("conditions");
      continue;
    }
    const value = flag(effective[k]);
    const shown = fits(SETTING, value) ? value : "[omitted]";
    if (shown === "[omitted]") note("conditions");
    projected[k] = shown;
    if (CONDITION_ALIAS[k]) projected[CONDITION_ALIAS[k]] = shown;
  }
  return { effective: projected };
}

// The members of sessionResp's pyrowave (session.PyroWaveRecord,
// control-plane/internal/session/pyrowave_plan.go:47-57) and the type each is written as. Nothing
// else from the answer is projected: a member not named here, and a nested object or array, is
// left out and counted. A string member is written only in a name's shape.
const PYROWAVE = {
  allowed: "boolean",
  wanted: "boolean",
  active: "boolean",
  used: "boolean",
  target_kbps: "number",
  reason: "string",
  choice: "string",
  auto_reason: "string",
  switch_reason: "string",
};

/** The answer's pyrowave record, member by member in its own type; undefined when it holds none. */
function pyrowave(o, note) {
  if (o == null) return undefined;
  const out = {};
  for (const [k, type] of Object.entries(PYROWAVE)) {
    const v = o[k];
    if (v === undefined) continue;
    // The Go field is a pointer: null is "the answer held none", and no text at all.
    if (v === null) out[k] = null;
    else if (type === "string" ? fits(IDENT, v) : type === "number" ? num(v) : typeof v === "boolean") out[k] = v;
    else note("session");
  }
  for (const k of Object.keys(o)) if (!Object.hasOwn(PYROWAVE, k) && o[k] != null) note("session");
  return out;
}

// The `session.json` shape scripts/dx/bench_submit.py reads (session_soak_driver.py builds the
// same record from GET /v1/sessions/{id}), plus the fields the 0.6 parity gate compares,
// named as the session row names them. A session that never started keeps its failure.
// Each field from the answer is written only in its own shape; the rest is left out and counted.
function sessionRow(s, never, note) {
  const st = s.stream;
  const row = { kind: "session", id: s.id };
  for (const k of ["state", "profile_id", "transport", "negotiated_codec"]) {
    if (s[k] === undefined) continue;
    if (s[k] === null || fits(IDENT, s[k])) row[k] = s[k];
    else note("session");
  }
  if (s.app_id !== undefined) {
    if (s.app_id === null || isId(s.app_id)) row.app_id = s.app_id;
    else note("session");
  }
  row.host_id = s.host_id; // a UUID: fetchSession refused anything else before this
  const pw = pyrowave(s.pyrowave, note);
  if (pw !== undefined) row.pyrowave = pw;
  // Why it never started is on the stack, not in a file. Both the failure code and the message
  // are text the agent chose (the control plane stores the code as sent, and a pull error can
  // spell out a registry credential), and no shape or pattern tells every credential from a
  // code. So neither is copied: the row says only that each exists.
  if (never) {
    row.has_failure_code = typeof s.failure_code === "string" && s.failure_code !== "";
    row.has_error_message = typeof s.error_message === "string" && s.error_message !== "";
  }
  if (num(st.width) && num(st.height)) row.launch = [st.width, st.height];
  for (const k of ["fps", "bitrate_kbps", "external_resize_supported"]) if (st[k] != null) row[k] = st[k];
  if (st.codec != null) {
    if (fits(IDENT, st.codec)) row.codec = st.codec;
    else note("session");
  }
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

/**
 * A build identity as the answer holds it (in key order, so two reads compare as text); null when
 * it holds no field ("not read"). A field is written only in its own shape, a build time only as
 * ISO, and anything else is left out and counted.
 */
function identity(o, versionKey, note) {
  const out = {};
  for (const [to, from] of [["built_at", "built_at"], ["commit", "source_commit"], ["version", versionKey]]) {
    const v = o[from];
    if (v == null) continue; // null is the answer holding none, which is "not read", not a rejection
    const ok = to === "built_at" ? isoTime(v) : fits(to === "commit" ? COMMIT : VERSION, v) ? v : null;
    if (ok) out[to] = ok;
    else note("identity");
  }
  return Object.keys(out).length ? out : null;
}

const usage = (message, code = "usage") => Object.assign(new Error(message), { code });

/** The stack's origin: http(s), no userinfo, query, fragment or path. Requests are `new URL(route, origin)`. */
function parseBase(baseUrl) {
  let u;
  try {
    u = new URL(String(baseUrl));
  } catch {
    throw usage("--base-url is not a URL");
  }
  if (!["http:", "https:"].includes(u.protocol) || u.username || u.password || /[?#]/.test(String(baseUrl)) || u.pathname !== "/") {
    throw usage("--base-url must be http(s)://host[:port] with no credentials, path, query or fragment");
  }
  return u.origin;
}

// Every request: no redirect is followed (a 3xx is not an answer), and the bearer goes only
// to the origin the operator gave. Messages name the route path, never its query.
async function getJson(origin, token, fetchFn, route) {
  const where = route.split("?")[0];
  let res;
  try {
    res = await fetchFn(new URL(route, origin).href, {
      headers: { Authorization: `Bearer ${token}`, Accept: "application/json" },
      redirect: "error",
    });
  } catch (e) {
    if (/redirect/i.test(`${e.message} ${e.cause?.message}`)) throw new Unreadable(`GET ${where} answered a redirect, which is never followed`);
    throw new Error(`GET ${where} could not be made (${e.message}): is ${origin} reachable?`);
  }
  if (res.status === 401 || res.status === 403) {
    const e = new Error(
      `the admin bearer was rejected (HTTP ${res.status} on GET ${where}). Next: scripts/dx/admin_token.sh --fresh`,
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
const PAGE = 1000; // the server's cap per read: newest 1000 raw samples across sources, 1000 events (telemetry.clampLimit; traceReadLimit)
const MAX_PAGES = 200;
const MAX_READS = 600; // window reads per session, splits included

const text = (v) => v == null || typeof v === "string";
const point = (e) => isObj(e) && num(e.ts_unix_ms);
const traceEvent = (e) => point(e) && typeof e.type === "string" && typeof e.source === "string";
// Captures (diag.*) outlive the retention sweep (telemetry/policy.go), so they are not data.
const isData = (e) => !e.type.startsWith("diag.");

// What one session's answers hold, all read and shape-checked before anything is written.
// Routes and shapes are the handlers': diagnostic_handler.go (resolveWindow,
// handleDiagnosticBundle, handleAdminTelemetryRead), metrics_handler.go (handleAdminMetrics),
// handler.go (handleGet, sessionResp), crud/handler.go (handleGetHost), hostcfg/handler.go
// (handleGet) and platform/handler.go (handleIdentity). `note` counts what the projection into a
// file had to leave out.
async function fetchSession(get, id, now, note) {
  const sid = encodeURIComponent(id);
  const ok = async (route) => {
    const where = route.split("?")[0];
    const r = await get(route);
    need(r.status !== 404, `GET ${where} answered HTTP 404: the stack no longer holds it`);
    need(r.status === 200, `GET ${where} answered HTTP ${r.status}`);
    need(isObj(r.body), `GET ${where} answered a body that is not a JSON object`);
    return r.body;
  };

  // handler.go handleGet: { "session": sessionResp }
  const s = (await ok(`/v1/sessions/${sid}`)).session;
  need(isObj(s) && typeof s.id === "string" && s.id.toLowerCase() === id, "its row is not the session that was asked for");
  s.id = id;
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
  need(
    ["state", "app_id", "profile_id", "transport", "negotiated_codec", "failure_code", "error_message"].every((k) => text(s[k])) &&
      (s.pyrowave == null || isObj(s.pyrowave)) &&
      ["width", "height", "fps", "bitrate_kbps"].every((k) => s.stream[k] == null || num(s.stream[k])) &&
      text(s.stream.codec) &&
      (s.stream.external_resize_supported == null || typeof s.stream.external_resize_supported === "boolean"),
    "its row has a field of the wrong type",
  );
  const never = s.started_at == null;
  need(!never || s.ended_at != null, "it has not started or ended yet");

  let from = null;
  let to = null;
  const reads = [];
  const samples = [];
  if (!never) {
    // The server's resolveWindow default is the 5 minutes ending NOW, empty days later, so the
    // window is explicit, in reads of at most 10 minutes; a read that comes back full is split.
    from = Date.parse(s.started_at) - PAD;
    to = (s.ended_at != null ? Date.parse(s.ended_at) : now()) + PAD;
    need(from < to, "its trace window is empty (it starts after it ends, or after now)");
    need(Math.ceil((to - from) / (10 * MIN)) <= MAX_READS, "its trace is longer than one collection reads");
    let n = 0;
    const span = async (a, b) => {
      need((n += 1) <= MAX_READS, "its trace is longer than one collection reads");
      const q = `?from=${a}&to=${b}`;
      // handleDiagnosticBundle: trace, window, clock, series{name:[{ts_unix_ms,v}]}, events, derived_windows, agent_adaptation, classifier
      const diag = await ok(`/v1/admin/sessions/${sid}/diagnostic-bundle${q}`);
      need(isObj(diag.trace) && isObj(diag.window) && num(diag.window.from_ms) && num(diag.window.to_ms) && isObj(diag.clock) && isObj(diag.series), "the diagnostic bundle is missing its trace, window, clock or series");
      for (const pts of Object.values(diag.series)) {
        need(Array.isArray(pts) && pts.every((p) => point(p) && num(p.v)), "the diagnostic bundle's series are not points");
      }
      need(Array.isArray(diag.events) && diag.events.every(traceEvent), "the diagnostic bundle's events are not events");
      need(
        isObj(diag.derived_windows) &&
          Object.values(diag.derived_windows).every((l) => Array.isArray(l) && l.every((w) => isObj(w) && num(w.from_ms ?? w.ts_unix_ms))),
        "the diagnostic bundle's derived windows are not windows",
      );
      need(Array.isArray(diag.agent_adaptation) && diag.agent_adaptation.every(point), "the diagnostic bundle's agent adaptation is not a list of points");
      need(isObj(diag.classifier), "the diagnostic bundle has no classifier");
      need(diag.trace.session_id === undefined || (typeof diag.trace.session_id === "string" && diag.trace.session_id.toLowerCase() === id), "the diagnostic bundle is another session's");
      // handleAdminTelemetryRead projectionEvents: { window, events }, the RAW events, not clock-aligned
      const ev = await ok(`/v1/admin/sessions/${sid}/trace/events${q}`);
      need(Array.isArray(ev.events) && ev.events.every(traceEvent), "the trace events are not events");
      // The store keeps the newest 1000 raw samples per read across all sources, so a series
      // can hold fewer points than the read dropped: count the distinct instants of all of them.
      const stamps = new Set();
      for (const pts of Object.values(diag.series)) for (const p of pts) stamps.add(p.ts_unix_ms);
      const full = ev.events.length >= PAGE || diag.events.length >= PAGE || stamps.size >= PAGE;
      if (!full) return reads.push({ diag, events: ev.events });
      need(b - a >= 4 * MIN, "its trace is denser than one read holds, even in the smallest window");
      const mid = Math.floor((a + b) / 2);
      await span(a, mid);
      await span(mid, b);
    };
    for (let a = from; a < to; a += 10 * MIN) await span(a, Math.min(a + 10 * MIN, to));
    need(reads.length > 0, "no trace window could be read");

    // handleAdminMetrics: { items: [{source, ts_unix_ms, metrics}], next_cursor }, newest first.
    // The RAW samples, every key and the original timestamps; paged until the cursor ends.
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
      reads.some((r) => r.events.some(isData) || r.diag.events.some(isData) || Object.values(r.diag.series).some((p) => p.length > 0));
    need(hasData, "the trace is no longer kept (the stack deletes telemetry after its retention period, a day by default)");
  }

  const hid = encodeURIComponent(s.host_id);
  // hostcfg handleGet: { resolved, overrides, effective: map[string]string | null, codecs, pending_restart }
  const settings = await ok(`/v1/admin/hosts/${hid}/settings`);
  need(
    settings.effective === null || (isObj(settings.effective) && Object.values(settings.effective).every((v) => typeof v === "string")),
    "the host settings' effective map is not a map of strings",
  );
  // crud handleGetHost: { "host": hostResp }
  const host = (await ok(`/v1/hosts/${hid}`)).host;
  need(isObj(host), "the host row is not an object");

  return { id, row: s, never, from, to, reads, samples, effective: settings.effective, agent: identity(host, "agent_version", note) };
}

// A name from an answer goes into a file only if it is a plain identifier and not a credential's
// name; any other is counted, never copied.
const nameOk = (n) => /^[A-Za-z0-9_.]{1,64}$/.test(n) && !isCredentialName(n);

// The metric keys and sources the format admits. Anything else the API sent is dropped and
// accounted for in host-half.json, never written.
function hostRows(samples, keys, dropped) {
  const rows = new Map();
  for (const smp of samples) {
    if (!SOURCES.includes(smp.source)) {
      dropped.note("sources", smp.source, smp.source);
      continue;
    }
    const id = `${smp.source}|${smp.ts_unix_ms}`;
    if (!rows.has(id)) rows.set(id, { kind: "metric", source: smp.source, ts_unix_ms: smp.ts_unix_ms, metrics: Object.create(null) });
    const row = rows.get(id);
    for (const [k, v] of Object.entries(smp.metrics)) {
      if (!nameOk(k) || (keys[smp.source] && !keys[smp.source].has(k))) dropped.note("keys", k, `${smp.source}.${k}`);
      else if (!Object.hasOwn(row.metrics, k)) row.metrics[k] = v;
    }
  }
  return [...rows.values()];
}

/** A tally of what was dropped: names that pass `nameOk` are listed, the rest only counted. */
function droppedTally() {
  const listed = { sources: new Set(), keys: new Set() };
  const other = { sources: new Set(), keys: new Set() };
  return {
    note: (kind, name, shown) => (nameOk(name) ? listed : other)[kind].add(shown),
    fields: () => ({
      ...(listed.sources.size ? { sources_not_in_bundle: [...listed.sources].sort() } : {}),
      ...(listed.keys.size ? { metric_keys_not_in_manifest: [...listed.keys].sort() } : {}),
      ...(other.sources.size || other.keys.size ? { other_dropped: { metric_keys: other.keys.size, sources: other.sources.size } } : {}),
    }),
  };
}

// The diagnostic bundle as the artifact. One read is the API's answer. A session read in several
// windows folds series and events, concatenates derived windows and agent adaptation (de-duplicated
// by time), keeps the first read's classifier and lists every read's in classifier_reads.
function foldDiagnostic(reads) {
  if (reads.length === 1) return reads[0].diag;
  const series = new Map();
  const events = [];
  const derived = Object.create(null);
  const adaptation = [];
  for (const { diag } of reads) {
    for (const [name, pts] of Object.entries(diag.series)) {
      if (!series.has(name)) series.set(name, new Map());
      for (const p of pts) series.get(name).set(p.ts_unix_ms, p);
    }
    events.push(diag.events);
    for (const [k, list] of Object.entries(diag.derived_windows)) {
      derived[k] ??= new Map();
      for (const w of list) derived[k].set(w.from_ms ?? w.ts_unix_ms, w);
    }
    adaptation.push(...diag.agent_adaptation);
  }
  const byTs = (a, b) => a.ts_unix_ms - b.ts_unix_ms;
  const sorted = (m) => [...m].sort(([a], [b]) => a - b).map(([, w]) => w);
  return {
    ...reads[0].diag,
    window: { from_ms: Math.min(...reads.map((r) => r.diag.window.from_ms)), to_ms: Math.max(...reads.map((r) => r.diag.window.to_ms)) },
    series: Object.fromEntries([...series].map(([n, m]) => [n, [...m.values()].sort(byTs)])),
    events: mergeEvents(...events),
    derived_windows: Object.fromEntries(Object.entries(derived).map(([k, m]) => [k, sorted(m)])),
    agent_adaptation: [...new Map(adaptation.map((a) => [a.ts_unix_ms, a])).values()].sort(byTs),
    classifier_reads: reads.map((r) => r.diag.classifier),
  };
}

const traceShape = (t) =>
  isObj(t) && (t.events === undefined || (Array.isArray(t.events) && t.events.every(point))) && (t.series === undefined || isObj(t.series));

// Files the collector writes for one session, in a scenario directory (the first) or sessions/<id>/.
const sessionFiles = (i) => ["diagnostic-bundle.json", "session.json", "conditions.json", "metrics.jsonl", "trace.json", ...(i === 0 ? ["host-half.json"] : [])];
// Files a client half never has: any of them means the bundle already claims a host half.
const CLAIM_FILES = ["host-half.json", "diagnostic-bundle.json", "conditions.json"];

function claimsHostHalf(scDir) {
  const holds = (d) => CLAIM_FILES.some((f) => fs.existsSync(path.join(d, f)));
  const sessions = path.join(scDir, "sessions");
  const sub = fs.statSync(sessions, { throwIfNoEntry: false })?.isDirectory() ? fs.readdirSync(sessions).map((n) => path.join(sessions, n)) : [];
  return holds(scDir) || sub.some((d) => fs.statSync(d, { throwIfNoEntry: false })?.isDirectory() && holds(d));
}

/** Is a scenario marked collected really collected: every file the collector writes for it present and well shaped? */
function intact(item) {
  try {
    if (item.prior?.collected !== true) return false;
    if (!item.ids.length) return true;
    const agent = item.prior.node_agent;
    need(agent === null || (isObj(agent) && Object.values(agent).every((v) => typeof v === "string")), "node_agent");
    item.ids.forEach((id, i) => {
      const out = item.outs[i].out;
      const session = readJson(path.join(out, "session.json"));
      need(isObj(session) && session.kind === "session" && session.id === id, "session.json");
      const cond = readJson(path.join(out, "conditions.json"));
      need(isObj(cond) && (isObj(cond.effective) || isObj(cond.unknown)), "conditions.json");
      if (session.started_at_ms === undefined) return;
      const diag = readJson(path.join(out, "diagnostic-bundle.json"));
      need(isObj(diag) && isObj(diag.series) && Array.isArray(diag.events), "diagnostic-bundle.json");
      need(fs.existsSync(path.join(out, "metrics.jsonl")), "metrics.jsonl");
      readRows(path.join(out, "metrics.jsonl"));
      const trace = readJson(path.join(out, "trace.json"));
      need(traceShape(trace) && Array.isArray(trace.events), "trace.json");
    });
    return true;
  } catch (e) {
    if (e instanceof Unreadable) return false;
    throw e;
  }
}

/**
 * Fetch the host half of `dir` and write it in. A test passes `fetch` and `now`.
 * Without `resume`, a bundle that already claims a host half is refused (code "claimed") before any
 * request. With it, a scenario is skipped only if host-half.json says collected AND every file the
 * collector writes for it is present and well shaped. A scenario's outputs are staged and renamed
 * together, host-half.json last; manifest.json and builds.json are reconciled at the end of every run.
 * Returns `{ complete, problems, incomplete, scenarios, sessions, collected }` (`refusedBefore` when
 * the bundle was refused before any request). Throws with `code` "unauthorized", "usage" or "claimed".
 */
export async function collect(dir, { baseUrl, token, fetch: fetchFn = globalThis.fetch, now = Date.now, resume = false } = {}) {
  if (!token) throw new Error("no admin bearer: QUASAR_ADMIN_TOKEN is empty");
  if (!baseUrl) throw new Error("no --base-url");
  const origin = parseBase(baseUrl);
  const refuse = (problems) => ({ complete: false, refusedBefore: true, problems, incomplete: [], scenarios: 0, sessions: 0, collected: 0 });

  // The bundle is untrusted input and may come from another machine. Its format is checked
  // FIRST, before a scenario name becomes a write path and before a session id becomes an
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
  const claims = manifest.halves?.host === true ? ["manifest.json"] : [];
  for (const name of manifest.scenarios) {
    const scDir = path.join(dir, name);
    const given = readJson(path.join(scDir, "scenario.json")).sessions;
    for (const id of given) {
      if (!isId(id)) throw usage(`${name}/scenario.json: '${clip(id)}' is not a session id (a session id is a UUID); nothing was requested`);
    }
    const ids = given.map((id) => id.toLowerCase());
    if (claimsHostHalf(scDir)) claims.push(name);
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
    const item = { name, scDir, ids, prior, outs: ids.map((id, i) => ({ id, out: i === 0 ? scDir : path.join(scDir, "sessions", id) })) };
    item.done = resume && intact(item);
    if (!item.done && ids.length) {
      try {
        for (const p of [path.join(scDir, "sessions"), ...item.outs.map((o) => o.out)]) need(!fs.existsSync(p) || fs.statSync(p).isDirectory(), `${path.relative(dir, p)} is not a directory`);
        item.outs.forEach((o, i) => {
          for (const f of sessionFiles(i)) {
            const p = path.join(o.out, f);
            need(!fs.existsSync(p) || fs.lstatSync(p).isFile(), `${path.relative(dir, p)} is not a file`);
          }
          o.metrics = readRows(path.join(o.out, "metrics.jsonl"));
        });
        const traceFile = path.join(scDir, "trace.json");
        item.trace = fs.existsSync(traceFile) ? readJson(traceFile) : {};
        need(traceShape(item.trace), `${name}/trace.json is not a trace`);
      } catch (e) {
        problems.push(e.message.startsWith(dir) ? e.message.slice(dir.length + 1) : e.message);
      }
    }
    plan.push(item);
  }
  if (problems.length) return refuse(problems);
  if (claims.length && !resume) {
    throw usage(
      `${claims.slice(0, 5).join(", ")}: the bundle already has a host half. Nothing was requested. Collect a fresh copy of the client bundle, or pass --resume (RESUME=1) to finish a collection this machine started.`,
      "claimed",
    );
  }

  const { write, commit, discard, put } = writer(dir, token);
  const get = (route) => getJson(origin, token, fetchFn, route);
  const todo = plan.filter((p) => !p.done && p.ids.length);
  const state = new Map(); // scenario -> { collected, agent } once this run is done
  for (const p of plan) state.set(p.name, p.done && p.ids.length ? { collected: true, agent: p.prior.node_agent } : { collected: !p.ids.length, agent: undefined });
  const incomplete = [];
  const withSessions = plan.some((p) => p.ids.length);

  // A scenario that names no session has nothing to collect, and says so.
  for (const p of plan) if (!p.ids.length && p.prior?.collected !== true) put(path.join(p.scDir, "host-half.json"), { collected: true });

  const allDone = () => [...state.values()].every((s) => s.collected);
  const finishedBefore = plan.every((p) => p.done || !p.ids.length);
  // platform/handler.go handleIdentity: { "identity": {...} }; read once, before any scenario is written
  let controlPlane = null;
  let cpReason = null;
  // What the projection left out, counted per file and never echoed (neither the value nor its
  // length). The platform identity is one read for the whole run, so its count rides in the first
  // scenario this run collects; each scenario counts its own.
  const cpLeftOut = { identity: 0 };
  let cpCounted = false;
  const readCp = withSessions && (todo.length > 0 || (finishedBefore && manifest.halves?.host !== true));
  if (readCp) {
    try {
      const r = await get("/v1/admin/platform/identity");
      need(r.status === 200 && isObj(r.body?.identity), `GET /v1/admin/platform/identity answered HTTP ${r.status}`);
      controlPlane = identity(r.body.identity, "version", () => {
        cpLeftOut.identity += 1;
      });
      need(controlPlane, "GET /v1/admin/platform/identity held no version, commit or build time");
    } catch (e) {
      if (!(e instanceof Unreadable)) throw e;
      cpReason = e.message;
    }
  }

  for (const p of todo) {
    const docs = [];
    const leftOut = { session: 0, conditions: 0, identity: 0 };
    const note = (file) => {
      leftOut[file] += 1;
    };
    let reason = null;
    try {
      for (const id of p.ids) docs.push(await fetchSession(get, id, now, note));
    } catch (e) {
      if (!(e instanceof Unreadable)) throw e;
      reason = `session ${p.ids[docs.length]}: ${e.message}`;
    }
    if (reason) {
      incomplete.push({ scenario: p.name, reason });
      put(path.join(p.scDir, "host-half.json"), { collected: false, reason });
      continue;
    }

    // Decide every output, stage them all, then rename them into place together.
    const readAt = now();
    const dropped = droppedTally();
    const metricFiles = [];
    try {
      docs.forEach((d, i) => {
        const { out, metrics } = p.outs[i];
        write(path.join(out, "session.json"), sessionRow(d.row, d.never, note));
        write(path.join(out, "conditions.json"), conditions(d.effective, note));
        if (d.never) return;
        write(path.join(out, "diagnostic-bundle.json"), foldDiagnostic(d.reads));
        const measured = d.reads.map((r) => r.diag.clock).find((c) => c.unmeasured !== true);
        write(path.join(out, "trace.json"), {
          session_id: d.id,
          window: { from_ms: d.from, to_ms: d.to },
          clock: measured ?? (i === 0 ? p.trace.clock : undefined) ?? d.reads[0].diag.clock,
          events: mergeEvents(...(i === 0 ? [p.trace.events ?? []] : []), ...d.reads.map((r) => r.events)),
          series: (i === 0 ? p.trace.series : undefined) ?? {},
        });
        metricFiles.push([path.join(out, "metrics.jsonl"), mergeMetrics(metrics, hostRows(d.samples, keys, dropped))]);
      });
      for (const [file, rows] of metricFiles) write(file, rows);
      const agents = new Set(docs.map((d) => JSON.stringify(d.agent)));
      const agent = agents.size === 1 ? JSON.parse([...agents][0]) : null;
      if (!cpCounted) {
        leftOut.identity += cpLeftOut.identity;
        cpCounted = true;
      }
      write(path.join(p.scDir, "host-half.json"), {
        collected: true,
        read_at_ms: readAt,
        node_agent: agent,
        ...(docs.every((d) => d.never) ? { trace: "the session never started" } : {}),
        ...(leftOut.session || leftOut.conditions || leftOut.identity ? { not_projected: { ...leftOut } } : {}),
        ...dropped.fields(),
      });
      commit();
      state.set(p.name, { collected: true, agent });
    } catch (e) {
      discard();
      throw e;
    }
  }

  // The two builds a host half can name; everything else stays as the client half wrote it.
  // A component that cannot be read is null with its reason, replacing whatever the client half held.
  const setBuild = (name, value, why) => {
    builds.components[name] = value;
    if (value) delete builds.unknown[name];
    else builds.unknown[name] = why;
  };
  if (readCp) setBuild("control_plane", controlPlane, cpReason);
  if (withSessions) {
    const agents = [...state.values()].filter((s) => s.collected && s.agent !== undefined);
    const distinct = new Set(agents.map((s) => JSON.stringify(s.agent)));
    setBuild(
      "node_agent",
      distinct.size === 1 && !distinct.has("null") ? JSON.parse([...distinct][0]) : null,
      agents.length ? "the collected sessions did not share one readable host agent build" : "no session was collected, so no host agent identity was read",
    );
  }
  manifest.halves = isObj(manifest.halves) ? manifest.halves : {};
  manifest.halves.host = allDone();
  put(path.join(dir, "builds.json"), builds); // before the manifest: halves.host true means builds.json was written
  put(path.join(dir, "manifest.json"), manifest);

  const total = plan.reduce((n, p) => n + p.ids.length, 0);
  return {
    complete: allDone(),
    incomplete,
    problems: validateBundle(dir),
    scenarios: plan.length,
    sessions: total,
    collected: plan.filter((p) => state.get(p.name).collected).reduce((n, p) => n + p.ids.length, 0),
  };
}

function parseArgs(argv) {
  const args = { dir: null, baseUrl: null, resume: false };
  for (let i = 0; i < argv.length; i += 1) {
    const a = argv[i];
    if (a === "--base-url") {
      args.baseUrl = argv[(i += 1)] ?? null;
    } else if (a.startsWith("--base-url=")) {
      args.baseUrl = a.slice("--base-url=".length);
    } else if (a === "--resume") {
      args.resume = true;
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

const USAGE = 'usage: QUASAR_ADMIN_TOKEN=<bearer> collect.mjs <bundle-dir> --base-url <stack url> [--resume]';

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
    result = await collect(args.dir, { baseUrl: args.baseUrl, token, fetch: fetchFn, resume: args.resume });
  } catch (e) {
    console.error(e.message);
    const reason = { unauthorized: "unauthorized", usage: "usage", claimed: "host-half-present" }[e.code] ?? "collect-failed";
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
