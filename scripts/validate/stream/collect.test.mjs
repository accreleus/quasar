// node --test scripts/validate/stream/collect.test.mjs   (make verify runs it)
//
// A fake API as an in-process `fetch` — no sockets, no stack, no lab host.
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";
import { validateBundle } from "./bundle.mjs";
import { EXIT, collect, main } from "./collect.mjs";

const HERE = path.dirname(fileURLToPath(import.meta.url));
const ROOT = path.resolve(HERE, "../../..");
const DX = path.join(ROOT, "scripts/dx");
const SC = "01-stream-hold";
const SID = "11111111-2222-3333-4444-555555555555";
const HOST_ID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee";
const BASE = "https://stack.invalid";
const TOKEN = "admintoken-9f3c2b7a41";
const TMP = fs.mkdtempSync(path.join(os.tmpdir(), "e2e-collect-"));
process.on("exit", () => fs.rmSync(TMP, { recursive: true, force: true }));

// A client-only bundle: the committed fixture (which already looks like a run
// directory), the DX suite's own series and trace, and no host half.
function clientBundle() {
  const dir = fs.mkdtempSync(path.join(TMP, "b-"));
  fs.cpSync(path.join(HERE, "fixtures/bundle"), dir, { recursive: true });
  for (const f of ["metrics.jsonl", "trace.json"]) {
    fs.copyFileSync(path.join(DX, "tests/fixtures/bench-run", f), path.join(dir, SC, f));
  }
  fs.rmSync(path.join(dir, SC, "session.json"), { force: true });
  edit(dir, "manifest.json", (m) => ({ ...m, halves: { client: true, host: false } }));
  return dir;
}

const read = (dir, rel) => fs.readFileSync(path.join(dir, rel), "utf8");
const edit = (dir, rel, fn) => {
  const file = path.join(dir, rel);
  fs.writeFileSync(file, JSON.stringify(fn(JSON.parse(fs.readFileSync(file, "utf8")))));
};
const rows = (dir, rel) =>
  read(dir, rel)
    .split("\n")
    .filter((l) => l.trim())
    .map((l) => JSON.parse(l));
const walk = (dir, rel = "") =>
  fs.readdirSync(path.join(dir, rel), { withFileTypes: true }).flatMap((e) => {
    const p = path.join(rel, e.name);
    return e.isDirectory() ? walk(dir, p) : [p];
  });

// ── the canned answers ──────────────────────────────────────────────────────
function diagnostic() {
  return {
    trace: { session_id: SID, host_id: HOST_ID, profile_id: "1080p60-h264", started_at: "2026-11-10T10:00:00Z" },
    window: { from_ms: 1786940100000, to_ms: 1786940130000 },
    clock: { client_offset_ms: 2, uncertainty_ms: 1 },
    series: {
      "encoder.fps": [{ ts_unix_ms: 1786941000000, v: 59.8 }],
      "encoder.encode_ms": [{ ts_unix_ms: 1786941000000, v: 2.4 }],
      // The fixture run already has a browser row at this ts: the merge keeps it.
      "client.present_interval_sd_ms": [{ ts_unix_ms: 1786940000000, v: 1.3 }],
      "harness.not.in.the.manifest": [{ ts_unix_ms: 1786941000000, v: 1 }],
    },
    events: [
      {
        source: "agent",
        ts_unix_ms: 1786940102000,
        type: "abr.ladder.step",
        payload: { peer: "10.20.30.40", token: "another-token-1234", note: `echoed Bearer ${TOKEN}` },
      },
    ],
  };
}

function sessionDoc() {
  return {
    session: {
      id: SID,
      state: "stopped",
      app_id: "99999999-8888-7777-6666-555555555555",
      host_id: HOST_ID,
      profile_id: "1080p60-h264",
      created_at: "2026-11-10T10:00:00Z",
      started_at: "2026-11-10T10:00:05Z",
      ended_at: "2026-11-10T10:05:05Z",
      stream: {
        width: 1920,
        height: 1080,
        fps: 60,
        bitrate_kbps: 12000,
        codec: "h264",
        rungs: [[1920, 1080], [1280, 720]],
        external_resize_supported: true,
      },
    },
  };
}

const SETTINGS = { effective: { abr_mode: "smooth", abr_ladder_resolution: true, abr_floor_kbps: "4000" } };
const HOST = {
  id: HOST_ID,
  node_name: "lab-gpu-host",
  agent_version: "0.6.1-dev",
  source_commit: "abc1234",
  built_at: "2026-11-10T09:00:00Z",
};
const IDENTITY = {
  identity: { version: "0.6.1-dev", source_commit: "f00dfeed", built_at: "2026-11-10T08:00:00Z", schema_version: 123 },
};

/** `answers` overrides individual routes with `[status, body]`. */
function fake(answers = {}) {
  const routes = {
    [`/v1/admin/sessions/${SID}/diagnostic-bundle`]: [200, diagnostic()],
    [`/v1/sessions/${SID}`]: [200, sessionDoc()],
    [`/v1/admin/hosts/${HOST_ID}/settings`]: [200, SETTINGS],
    [`/v1/hosts/${HOST_ID}`]: [200, HOST],
    "/v1/admin/platform/identity": [200, IDENTITY],
    ...answers,
  };
  return async (url, init) => {
    assert.equal(init.headers.Authorization, `Bearer ${TOKEN}`, "every request carries the bearer");
    const route = new URL(url).pathname;
    const [status, body] = routes[route] ?? [404, { error: "not_found" }];
    return { status, json: async () => body };
  };
}

test("a client-only bundle comes out complete and valid", async () => {
  const dir = clientBundle();
  const r = await collect(dir, { baseUrl: BASE, token: TOKEN, fetch: fake() });
  assert.equal(r.complete, true);
  assert.deepEqual(r.problems, []);
  assert.deepEqual(r.incomplete, []);

  const manifest = JSON.parse(read(dir, "manifest.json"));
  assert.equal(manifest.halves.host, true);
  assert.equal(manifest.halves.client, true);
  assert.equal(manifest.plan.name, "smoke");

  const session = JSON.parse(read(dir, `${SC}/session.json`));
  assert.equal(session.launch[0], 1920);
  assert.equal(session.fps, 60);
  assert.equal(session.codec, "h264");
  assert.equal(session.started_at_ms, Date.parse("2026-11-10T10:00:05Z"));
  assert.equal(session.duration_s, 300);

  // `effective` is the shape bench_run.sh writes: booleans on/off, ladder aliases.
  const cond = JSON.parse(read(dir, `${SC}/conditions.json`));
  assert.equal(cond.effective.abr_mode, "smooth");
  assert.equal(cond.effective.abr_ladder_resolution, "on");
  assert.equal(cond.effective.ladder_resolution, "on");
  assert.equal(cond.effective.abr_floor_kbps, "4000");

  // The host's series become samples; the client half's rows are kept, and the
  // host's row for the same source and ts is not duplicated.
  const metrics = rows(dir, `${SC}/metrics.jsonl`);
  const added = metrics.filter((r) => r.ts_unix_ms === 1786941000000);
  assert.equal(added.length, 1);
  assert.equal(added[0].source, "agent");
  assert.equal(added[0].metrics.fps, 59.8);
  assert.equal(added[0].metrics.encode_ms, 2.4);
  const shared = metrics.filter((r) => r.ts_unix_ms === 1786940000000 && r.source === "browser");
  assert.equal(shared.length, 1);
  assert.equal(shared[0].metrics.present_interval_sd_ms, 1.1, "the client half's row for that ts wins");
  const sorted = [...metrics].sort((a, b) => a.ts_unix_ms - b.ts_unix_ms || (a.source < b.source ? -1 : 1));
  assert.deepEqual(metrics, sorted);

  // The trace keeps the client half's events and adds the host's.
  const trace = JSON.parse(read(dir, `${SC}/trace.json`));
  assert.ok(trace.events.some((e) => e.type === "client.freeze_detected"));
  assert.ok(trace.events.some((e) => e.type === "abr.ladder.step"));
  assert.equal(trace.session_id, SID);

  assert.deepEqual(JSON.parse(read(dir, `${SC}/host-half.json`)), {
    collected: true,
    series_not_in_manifest: ["harness.not.in.the.manifest"],
  });

  const builds = JSON.parse(read(dir, "builds.json"));
  assert.deepEqual(builds.components.control_plane, {
    version: "0.6.1-dev",
    commit: "f00dfeed",
    built_at: "2026-11-10T08:00:00Z",
  });
  assert.deepEqual(builds.components.node_agent, {
    version: "0.6.1-dev",
    commit: "abc1234",
    built_at: "2026-11-10T09:00:00Z",
  });
  assert.equal(builds.components.reference_app.version, "767e2ad", "the client half's builds are untouched");
  assert.ok(builds.unknown.web, "their unknown reasons are untouched");
});

test("collecting the same bundle twice changes no byte", async () => {
  const dir = clientBundle();
  await collect(dir, { baseUrl: BASE, token: TOKEN, fetch: fake() });
  const first = new Map(walk(dir).map((rel) => [rel, read(dir, rel)]));
  await collect(dir, { baseUrl: BASE, token: TOKEN, fetch: fake() });
  const second = new Map(walk(dir).map((rel) => [rel, read(dir, rel)]));
  assert.deepEqual([...second.keys()].sort(), [...first.keys()].sort());
  for (const [rel, text] of first) assert.equal(second.get(rel), text, `${rel} changed on the second collection`);
});

test("an address and a credential in an answer are redacted before they are written", async () => {
  const dir = clientBundle();
  await collect(dir, { baseUrl: BASE, token: TOKEN, fetch: fake() });
  const bundle = read(dir, `${SC}/diagnostic-bundle.json`);
  assert.match(bundle, /\[address\]/);
  assert.doesNotMatch(bundle, /10\.20\.30\.40/);
  assert.doesNotMatch(bundle, /"token"/);
  assert.doesNotMatch(bundle, /another-token-1234/);
  assert.deepEqual(validateBundle(dir), [], "the redacted bundle is still a valid bundle");
});

test("the admin bearer is in no file the collector writes", async () => {
  const dir = clientBundle();
  await collect(dir, { baseUrl: BASE, token: TOKEN, fetch: fake() });
  for (const rel of walk(dir)) {
    if (Buffer.from(read(dir, rel)).subarray(0, 4096).includes(0)) continue;
    assert.ok(!read(dir, rel).includes(TOKEN), `${rel} holds the bearer`);
  }
});

test("a session the stack no longer holds is recorded as not collected, and the run is incomplete", async () => {
  const dir = clientBundle();
  const gone = fake({ [`/v1/admin/sessions/${SID}/diagnostic-bundle`]: [404, { error: "not_found" }] });
  const r = await collect(dir, { baseUrl: BASE, token: TOKEN, fetch: gone });
  assert.equal(r.complete, false);
  assert.equal(r.collected, 0);
  assert.deepEqual(r.problems, [], "an uncollected session is data, not an invalid bundle");
  assert.match(r.incomplete[0].reason, /404/);
  const half = JSON.parse(read(dir, `${SC}/host-half.json`));
  assert.equal(half.collected, false);
  assert.match(half.reason, /no longer holds it/);
  assert.equal(JSON.parse(read(dir, "manifest.json")).halves.host, false);
  assert.equal(fs.existsSync(path.join(dir, SC, "session.json")), false);

  const quiet = console.log;
  console.log = () => {};
  try {
    assert.equal(await main([dir, "--base-url", BASE], { fetch: gone, env: { QUASAR_ADMIN_TOKEN: TOKEN } }), EXIT.incomplete);
  } finally {
    console.log = quiet;
  }
});

test("a rejected bearer stops the run and names the ladder", async () => {
  const dir = clientBundle();
  const denied = fake({ [`/v1/admin/sessions/${SID}/diagnostic-bundle`]: [401, { error: "unauthorized" }] });
  await assert.rejects(collect(dir, { baseUrl: BASE, token: TOKEN, fetch: denied }), (e) => {
    assert.equal(e.code, "unauthorized");
    assert.match(e.message, /scripts\/dx\/admin_token\.sh/);
    return true;
  });

  const quiet = console.log;
  const errs = [];
  const realErr = console.error;
  console.log = () => {};
  console.error = (m) => errs.push(m);
  try {
    assert.equal(await main([dir, "--base-url", BASE], { fetch: denied, env: { QUASAR_ADMIN_TOKEN: TOKEN } }), EXIT.error);
  } finally {
    console.log = quiet;
    console.error = realErr;
  }
  assert.ok(errs.some((m) => /admin_token\.sh/.test(m)));
});

test("the command: 0 collected, 1 invalid bundle, 2 usage", async () => {
  const quiet = console.log;
  const errs = [];
  console.log = () => {};
  console.error = (m) => errs.push(m);
  try {
    const dir = clientBundle();
    assert.equal(await main([dir, "--base-url", BASE], { fetch: fake(), env: { QUASAR_ADMIN_TOKEN: TOKEN } }), EXIT.ok);
    assert.equal(await main([dir], { fetch: fake(), env: { QUASAR_ADMIN_TOKEN: TOKEN } }), EXIT.error);
    assert.equal(await main([dir, "--base-url", BASE], { fetch: fake(), env: {} }), EXIT.error);
    assert.ok(errs.some((m) => /QUASAR_ADMIN_TOKEN/.test(m)));

    const broken = clientBundle();
    edit(broken, "manifest.json", (m) => ({ ...m, format: 2 }));
    assert.equal(
      await main([broken, "--base-url", BASE], { fetch: fake(), env: { QUASAR_ADMIN_TOKEN: TOKEN } }),
      EXIT.invalid,
    );
  } finally {
    console.log = quiet;
  }
});
