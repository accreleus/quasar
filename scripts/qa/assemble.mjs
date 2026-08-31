#!/usr/bin/env node
// scripts/qa/assemble.mjs — turns raw image-QA harness artifacts (meta.json,
// profile.json, per-run scripts/qa/probe.mjs JSON, optional shutdown.json /
// preflight.json) into report.json (the schema in the spec's "report.json
// schema" section) and report.html (rendered via report.mjs's renderReport,
// unmodified).
//
// Spec: docs/design/plans/2026-08-13-image-qa-harness-spec.md — "Gates",
// "report.json schema", "Report".
//
// CLI:
//   node scripts/qa/assemble.mjs --out <resultsDir> --meta <meta.json> \
//     --profile <profile.json> --runs <run-1.json,run-2.json,...> \
//     [--shutdown <shutdown.json>] [--preflight <preflight.json>]
//
// Writes <resultsDir>/report.json, <resultsDir>/report.html, and
// <resultsDir>/shots/*.png (decoded from the base64 payloads embedded in
// report.json — the HTML stays self-contained via inline base64; the PNGs on
// disk are for local inspection only). Prints one final line:
//   RESULT overall=<PASS|FAIL> pass=<n> fail=<n> skipped=<n> evidence=<n>
// and exits 1 if overall is FAIL.

import { readFile, writeFile, mkdir } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { renderReport } from "./report.mjs";

const B64_RE = /^[A-Za-z0-9+/=]+$/;

// ---------------------------------------------------------------------------
// small formatting helpers
// ---------------------------------------------------------------------------

function fmtRange(values) {
  const v = values.filter((x) => typeof x === "number" && Number.isFinite(x));
  if (v.length === 0) return "n/a";
  const lo = Math.min(...v);
  const hi = Math.max(...v);
  return lo === hi ? String(lo) : `${lo}-${hi}`;
}

function joinSlash(values, digits = 1) {
  return values
    .map((x) => (typeof x === "number" && Number.isFinite(x) ? x.toFixed(digits) : "null"))
    .join("/");
}

function letterFor(i) {
  return String.fromCharCode(97 + i); // 0 -> 'a', 1 -> 'b', ...
}

function pad(s, n) {
  return String(s).padEnd(n);
}

// ---------------------------------------------------------------------------
// gate 0 — preflight
// ---------------------------------------------------------------------------

function gatePreflight(preflight, meta) {
  const n = "0";
  const title = "preflight";
  const proves = "Stack can host the test at all";
  const threshold = "health 200, image tag present, dev-agent auth on";

  if (!preflight) {
    return {
      id: "preflight", n, title, proves, threshold,
      measured: "not run", verdict: "SKIPPED",
      reason: "no preflight data captured for this run",
    };
  }

  const fields = [
    { key: "health", ok: preflight.health === "ok", display: preflight.health === "ok" ? "ok" : String(preflight.health ?? "missing") },
    { key: "image_present", ok: preflight.image_present === true, display: preflight.image_present === true ? "ok" : String(preflight.image_present ?? "missing") },
    { key: "dev_agent_auth", ok: preflight.dev_agent_auth === true, display: preflight.dev_agent_auth === true ? "ok" : String(preflight.dev_agent_auth ?? "missing") },
  ];
  const failed = fields.filter((f) => !f.ok);
  const verdict = failed.length ? "FAIL" : "PASS";
  const measured = fields.map((f) => f.display).join(" / ");
  const detail = [
    `GET /health -> ${preflight.health ?? "missing"}`,
    `docker image inspect ${meta?.image?.tag ?? "?"} -> ${preflight.image_present ? "present" : "missing"}`,
    `QUASAR_DEV_AGENT_AUTH -> ${preflight.dev_agent_auth ? "1, dev key reachable" : "not reachable"}`,
  ].join("\n");
  const reason = failed.length ? `field(s) not ok: ${failed.map((f) => f.key).join(", ")}` : "";

  return { id: "preflight", n, title, proves, threshold, measured, verdict, reason, detail };
}

// ---------------------------------------------------------------------------
// gate 1 — repoint
// ---------------------------------------------------------------------------

function gateRepoint(meta) {
  const n = "1";
  const title = "repoint";
  const proves = "Sessions actually launch the candidate, not the pinned digest";
  const threshold = `pin == ${meta?.image?.tag ?? "?"}`;

  if (meta?.repointed === false) {
    return {
      id: "repoint", n, title, proves, threshold,
      measured: "not applied", verdict: "SKIPPED", reason: "--no-repoint",
    };
  }

  const rp = meta?.replaced_pin;
  const verdict = rp ? "PASS" : "FAIL";
  const measured = rp ? `applied, pin -> ${meta?.image?.tag ?? "?"}` : "not applied";
  const detail = rp
    ? `previous image_ref recorded: ${rp.image_ref}\npin -> ${meta?.image?.tag ?? "?"} (confirmed via re-read)`
    : "no replaced_pin recorded in meta.json";
  const reason = rp ? "" : "meta.replaced_pin missing — repoint was never confirmed";

  return { id: "repoint", n, title, proves, threshold, measured, verdict, reason, detail };
}

// ---------------------------------------------------------------------------
// gate 2 — launch & decode
// ---------------------------------------------------------------------------

function gateLaunch(runs, profile) {
  const n = "2";
  const title = "launch & decode";
  const proves = "Each run: launch via control-plane, headless peer attaches, decode confirmed, steady-state luma sampled after the content-settle gate.";
  const launchCfg = profile?.gates?.launch || {};
  const [lo, hi] = launchCfg.first_content_s || [0, Infinity];
  const threshold = `fps>=${launchCfg.min_fps} · luma>=${launchCfg.min_luma} · first content ${lo}-${hi}s`;

  const reasons = [];
  const rows = [];
  const fpsVals = [];
  const lumaVals = [];
  const fcVals = [];
  let successCount = 0;

  for (const run of runs) {
    const label = run.run ?? "?";
    if (run.error) {
      reasons.push(`run ${label}: ${run.error} — ${run.message ?? ""}`);
      rows.push({ run: label, session_id: run.session_id ?? "", state: `error: ${run.error}`, fps: "", luma: "", first_content_s: "" });
      continue;
    }
    successCount++;
    const fps = run.stats?.fps ?? null;
    const lumaMean = run.luma?.mean ?? null;
    const fc = run.first_content_s;
    fpsVals.push(fps);
    lumaVals.push(lumaMean);
    fcVals.push(fc);

    if (fps == null || fps < launchCfg.min_fps) {
      reasons.push(`run ${label}: fps ${fps ?? "null"} < ${launchCfg.min_fps}`);
    }
    if (lumaMean == null || lumaMean < launchCfg.min_luma) {
      reasons.push(`run ${label}: luma ${lumaMean ?? "null"} < ${launchCfg.min_luma}`);
    }
    if (fc == null) {
      reasons.push(`run ${label}: first_content_s never painted`);
    } else if (fc < lo || fc > hi) {
      reasons.push(`run ${label}: first_content_s ${fc}s outside budget [${lo}, ${hi}]`);
    }

    rows.push({ run: label, session_id: run.session_id ?? "", state: "running", fps: fps ?? "null", luma: lumaMean ?? "null", first_content_s: fc ?? "null" });
  }

  const verdict = reasons.length ? "FAIL" : "PASS";
  const measured = `${successCount}/${runs.length} · ${fmtRange(fpsVals)} · ${joinSlash(lumaVals, 1)} · ${joinSlash(fcVals, 1)}s`;

  const header = `${pad("run", 5)}${pad("session_id", 38)}${pad("state", 15)}${pad("fps", 6)}${pad("luma", 7)}first_content_s`;
  const lines = rows.map((r) =>
    `${pad(r.run, 5)}${pad(r.session_id, 38)}${pad(r.state, 15)}${pad(r.fps, 6)}${pad(r.luma, 7)}${r.first_content_s}`
  );
  const budgetLine = `budget: fps>=${launchCfg.min_fps}  luma>=${launchCfg.min_luma}  first_content ${lo}-${hi}s`;
  const detail = [header, ...lines, budgetLine].join("\n");

  return {
    id: "launch", n, title, proves, threshold, measured, verdict,
    reason: reasons.join("; "),
    detail,
    metrics: { fps: fpsVals, luma: lumaVals, first_content_s: fcVals },
  };
}

// ---------------------------------------------------------------------------
// gate 3 — screen oracle
// ---------------------------------------------------------------------------

function gateOracle(runs, profile) {
  const n = "3";
  const oc = profile?.gates?.oracle || {};
  const title = `screen oracle${oc.label ? `: ${oc.label}` : ""}`;
  const threshold = oc.matcher ? `pixel matcher: ${oc.matcher}` : "operator eyeball (no matcher declared)";

  if (oc.matcher) {
    return {
      id: "oracle", n, title,
      proves: `A matcher (${oc.matcher}) is declared for oracle: ${oc.label ?? ""}.`,
      threshold, measured: "not implemented", verdict: "FAIL",
      reason: "matcher declared but not implemented",
    };
  }

  const proves = `No pixel matcher is declared for oracle: ${oc.label ?? ""}, so this gate captures evidence and does not assert. Expected on screen: ${oc.expect ?? ""}. Failure mode being ruled out: ${oc.rules_out ?? ""}.`;
  const detail = [
    `Expected on screen: ${oc.expect ?? ""}`,
    `Failure mode being ruled out: ${oc.rules_out ?? ""}`,
  ].join("\n");
  const screenshots = runs
    .filter((r) => r.oracle_png_b64)
    .map((r) => ({ label: `run ${r.run} · t=${r.settle_s ?? "?"}s`, png_b64: r.oracle_png_b64 }));

  return {
    id: "oracle", n, title, proves, threshold,
    measured: `${screenshots.length} screenshots below`,
    verdict: "EVIDENCE", reason: "", detail, screenshots,
  };
}

// ---------------------------------------------------------------------------
// gate 4 — input, one per device
// ---------------------------------------------------------------------------

function gateInputDevices(runs, profile, meta) {
  const devicesCfg = profile?.gates?.input?.devices || {};
  const names = Object.keys(devicesCfg).sort();
  const skipInput = new Set((meta?.skip_input || []).map((s) => String(s).toLowerCase()));
  const deltaThreshold = profile?.gates?.input?.delta_threshold_pct;
  const gates = [];

  names.forEach((name, idx) => {
    const n = `4${letterFor(idx)}`;
    const title = `input · ${name}`;
    const threshold = `focus-region delta >= ${deltaThreshold}%`;
    const proves = `Injects the profile-declared stimuli for ${name} on the agent-offered input DataChannel. Baseline is taken only after the content-settle gate.`;

    const runsWithData = runs.filter((r) => r.devices && Object.prototype.hasOwnProperty.call(r.devices, name));
    const first = runsWithData[0];

    if ((first && first.devices[name].skipped) || skipInput.has(name.toLowerCase())) {
      const reason = (first && first.devices[name].skipped && first.devices[name].reason)
        || `operator passed SKIP_INPUT=${name}`;
      gates.push({
        id: `input-${name}`, n, title, proves, threshold,
        measured: `not run — ${reason}`, verdict: "SKIPPED", reason,
      });
      return;
    }

    if (runsWithData.length === 0) {
      gates.push({
        id: `input-${name}`, n, title, proves, threshold,
        measured: "no data", verdict: "SKIPPED", reason: "no run reported data for this device",
      });
      return;
    }

    const metrics = { runs: runsWithData.map((r) => ({ run: r.run, delta_pct: r.devices[name].delta_pct ?? null, passed: r.devices[name].passed ?? null })) };
    const screenshots = [];
    for (const r of runsWithData) {
      const d = r.devices[name];
      if (d.before_png_b64) screenshots.push({ label: `run ${r.run} · before`, png_b64: d.before_png_b64 });
      if (d.after_png_b64) screenshots.push({ label: `run ${r.run} · after · Δ${d.delta_pct ?? "?"}%`, png_b64: d.after_png_b64 });
    }

    const timedOutRuns = runsWithData.filter((r) => r.settle === "timeout");
    const erroredRuns = runsWithData.filter((r) => r.devices[name].error);
    const nullDeltaRuns = runsWithData.filter((r) => r.devices[name].delta_pct == null);

    if (timedOutRuns.length || erroredRuns.length || nullDeltaRuns.length) {
      const reasons = [];
      if (timedOutRuns.length) reasons.push(`settle timed out on run(s) ${timedOutRuns.map((r) => r.run).join(", ")} — no reliable baseline`);
      if (erroredRuns.length) reasons.push(`device error on run(s) ${erroredRuns.map((r) => r.run).join(", ")}: ${erroredRuns.map((r) => r.devices[name].error).join("; ")}`);
      if (nullDeltaRuns.length) reasons.push(`delta_pct null on run(s) ${nullDeltaRuns.map((r) => r.run).join(", ")}`);
      gates.push({
        id: `input-${name}`, n, title, proves, threshold,
        measured: "evidence only, no verdict",
        verdict: "EVIDENCE", reason: reasons.join("; "),
        metrics, screenshots,
      });
      return;
    }

    const allPassed = runsWithData.every((r) => r.devices[name].passed === true);
    const measured = runsWithData.map((r) => `${r.devices[name].delta_pct}%`).join(" / ");
    gates.push({
      id: `input-${name}`, n, title, proves, threshold,
      measured,
      verdict: allPassed ? "PASS" : "FAIL",
      reason: allPassed ? "" : `delta below threshold on run(s) ${runsWithData.filter((r) => !r.devices[name].passed).map((r) => r.run).join(", ")}`,
      metrics, screenshots,
    });
  });

  return gates;
}

// ---------------------------------------------------------------------------
// gate 5 — clean shutdown
// ---------------------------------------------------------------------------

function gateShutdown(shutdown, profile) {
  const n = "5";
  const title = "clean shutdown";
  const proves = "Times docker stop on the live session container and asserts the launcher relayed SIGTERM instead of hitting the SIGKILL backstop.";
  const cfg = profile?.gates?.shutdown;
  const threshold = cfg ? `stop <= ${cfg.budget_s}s · exit == ${cfg.expect_exit} · required log lines present` : "";

  if (!shutdown) {
    return {
      id: "shutdown", n, title, proves, threshold,
      measured: "not run", verdict: "SKIPPED", reason: "no shutdown.json provided",
    };
  }

  const budget = cfg?.budget_s ?? Infinity;
  const expectExit = cfg?.expect_exit;
  const requireLogs = cfg?.require_logs || [];
  const forbidLogs = cfg?.forbid_logs || [];

  const reasons = [];
  const detailLines = [];
  const stops = [];
  const exits = [];
  let totalRequiredChecks = 0;
  let totalMatched = 0;

  for (const att of shutdown.attempts || []) {
    stops.push(att.stop_seconds);
    exits.push(att.exit_code);
    const log = att.log || "";
    detailLines.push(`run ${att.run}: stop_seconds=${att.stop_seconds}  exit_code=${att.exit_code}`);

    if (att.stop_seconds > budget) {
      reasons.push(`run ${att.run}: stop_seconds ${att.stop_seconds} exceeds budget ${budget}s`);
    }
    if (att.exit_code !== expectExit) {
      reasons.push(`run ${att.run}: exit_code ${att.exit_code} !== expected ${expectExit}`);
    }

    const matched = [];
    const missing = [];
    for (const line of requireLogs) {
      totalRequiredChecks++;
      if (log.includes(line)) {
        matched.push(line);
        totalMatched++;
      } else {
        missing.push(line);
      }
    }
    if (missing.length) {
      reasons.push(`run ${att.run}: missing required log line(s): ${missing.map((m) => `"${m}"`).join(", ")}`);
    }
    const forbidden = forbidLogs.filter((l) => log.includes(l));
    if (forbidden.length) {
      reasons.push(`run ${att.run}: forbidden log line(s) present: ${forbidden.map((m) => `"${m}"`).join(", ")}`);
    }

    detailLines.push(`  matched required (${matched.length}/${requireLogs.length}): ${matched.map((m) => `"${m}"`).join(", ") || "none"}`);
    if (missing.length) detailLines.push(`  missing required: ${missing.map((m) => `"${m}"`).join(", ")}`);
    if (forbidden.length) detailLines.push(`  forbidden present: ${forbidden.map((m) => `"${m}"`).join(", ")}`);
  }

  const verdict = reasons.length ? "FAIL" : "PASS";
  const measured = `${stops.map((s) => `${s}s`).join(" / ")} · ${exits.join(" / ")} · ${totalMatched}/${totalRequiredChecks} lines`;

  return {
    id: "shutdown", n, title, proves, threshold, measured, verdict,
    reason: reasons.join("; "),
    detail: [`container: ${shutdown.container ?? "?"}`, ...detailLines, `budget: <=${budget}s, exit ${expectExit}`].join("\n"),
  };
}

// ---------------------------------------------------------------------------
// gate 6 — teardown
// ---------------------------------------------------------------------------

function gateTeardown(meta) {
  const n = "6";
  const title = "teardown";
  const proves = "No state pollution, environment restored";
  const threshold = "sessions deleted via API · pin restored · 0 stray containers";
  const env = meta?.environment_restored;

  if (!env) {
    return {
      id: "teardown", n, title, proves, threshold,
      measured: "no data", verdict: "FAIL", reason: "meta.environment_restored missing",
    };
  }

  const pinOk = meta?.repointed === false ? true : env.pin === true;
  const strayOk = env.stray_containers === 0;
  const reasons = [];
  if (!pinOk) reasons.push("runtime pin not restored");
  if (!strayOk) reasons.push(`${env.stray_containers} stray session container(s) left running`);
  const verdict = reasons.length ? "FAIL" : "PASS";
  const measured = `${env.sessions_deleted ?? 0} deleted · ${env.pin ? "restored" : "not restored"} · ${env.stray_containers ?? "?"}`;
  const detail = [
    `sessions deleted via DELETE /v1/sessions/{id}: ${env.sessions_deleted ?? 0}`,
    `pin restored: ${env.pin ? "yes" : "no"}`,
    `stray session containers: ${env.stray_containers ?? "?"}`,
  ].join("\n");

  return { id: "teardown", n, title, proves, threshold, measured, verdict, reason: reasons.join("; "), detail };
}

// ---------------------------------------------------------------------------
// notes
// ---------------------------------------------------------------------------

function buildNotes({ gates, runs, profile }) {
  const notes = [];

  for (const g of gates) {
    if (g.verdict === "SKIPPED" && g.id.startsWith("input-")) {
      const device = g.title.replace("input · ", "");
      const Device = device.charAt(0).toUpperCase() + device.slice(1);
      notes.push({
        title: `${Device} is unproven, not broken`,
        body: `Skipped this run (${g.reason || "operator SKIP_INPUT"}). Re-run without SKIP_INPUT=${device} to close it.`,
      });
    }
  }

  const oracleGate = gates.find((g) => g.id === "oracle");
  if (oracleGate && oracleGate.verdict === "EVIDENCE") {
    notes.push({
      title: "Oracle gate is evidence, not a verdict",
      body: "Nobody asserted this automatically — the reader must look at the screenshots and judge against the profile's expect/rules_out.",
    });
  }

  const timedOut = runs.filter((r) => r.settle === "timeout");
  if (timedOut.length) {
    const degraded = gates.filter((g) => g.id.startsWith("input-") && g.verdict === "EVIDENCE").map((g) => g.n);
    notes.push({
      title: "Settle gate timed out",
      body: `Run(s) ${timedOut.map((r) => r.run).join(", ")} never reached a stable frame within the settle timeout, so gate(s) ${degraded.join(", ") || "(none — see per-gate reasons)"} degraded to EVIDENCE instead of a real verdict.`,
    });
  }

  const RTT_HIGH_MS = 50;
  const highRtt = runs.filter((r) => typeof r.stats?.rtt_ms === "number" && r.stats.rtt_ms > RTT_HIGH_MS);
  if (highRtt.length) {
    notes.push({
      title: "Peer-side reading, not an encode defect",
      body: `Run(s) ${highRtt.map((r) => r.run).join(", ")} reported elevated presentation-side rtt_ms (${highRtt.map((r) => r.stats.rtt_ms).join(", ")}ms). That is the headless peer's presentation side, not the encoder — not counted against any gate.`,
    });
  }

  for (const note of profile?.notes || []) {
    notes.push({ title: "Profile note", body: String(note) });
  }

  return notes;
}

// ---------------------------------------------------------------------------
// top-level assembly
// ---------------------------------------------------------------------------

export function buildReport({ meta, profile, runs, shutdown = null, preflight = null }) {
  const gates = [];
  gates.push(gatePreflight(preflight, meta));
  gates.push(gateRepoint(meta));
  gates.push(gateLaunch(runs, profile));
  gates.push(gateOracle(runs, profile));
  gates.push(...gateInputDevices(runs, profile, meta));
  gates.push(gateShutdown(shutdown, profile));
  gates.push(gateTeardown(meta));

  const summary = { pass: 0, fail: 0, skipped: 0, evidence: 0 };
  for (const g of gates) {
    if (g.verdict === "PASS") summary.pass++;
    else if (g.verdict === "FAIL") summary.fail++;
    else if (g.verdict === "SKIPPED") summary.skipped++;
    else if (g.verdict === "EVIDENCE") summary.evidence++;
  }
  const overall = summary.fail > 0 ? "FAIL" : "PASS";

  const notes = buildNotes({ gates, runs, profile });

  return {
    kind: "quasar-qa-report",
    schema_version: 1,
    image: meta?.image ?? {},
    replaced_pin: meta?.replaced_pin ?? null,
    host: meta?.host ?? "",
    stack_api: meta?.stack_api ?? "",
    profile: meta?.profile ?? profile?.name ?? "",
    runs: meta?.runs ?? runs.length,
    commit: meta?.commit ?? "",
    generated_at: meta?.generated_at ?? new Date().toISOString(),
    duration_ms: meta?.duration_ms ?? null,
    summary: { ...summary, overall },
    gates,
    notes,
    environment_restored: meta?.environment_restored ?? {},
  };
}

// ---------------------------------------------------------------------------
// CLI
// ---------------------------------------------------------------------------

async function writeShots(report, outDir) {
  const shotsDir = path.join(outDir, "shots");
  await mkdir(shotsDir, { recursive: true });
  let i = 0;
  for (const g of report.gates) {
    for (const s of g.screenshots || []) {
      if (!s.png_b64 || !B64_RE.test(s.png_b64)) continue;
      i++;
      const safeLabel = String(s.label || "shot").replace(/[^a-z0-9]+/gi, "-").toLowerCase();
      const filename = `${String(i).padStart(3, "0")}-${g.id}-${safeLabel}.png`;
      await writeFile(path.join(shotsDir, filename), Buffer.from(s.png_b64, "base64"));
    }
  }
}

function parseArgs(argv) {
  const args = { runs: [] };
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    switch (a) {
      case "--out": args.out = argv[++i]; break;
      case "--meta": args.meta = argv[++i]; break;
      case "--profile": args.profile = argv[++i]; break;
      case "--runs": args.runs = String(argv[++i] || "").split(",").map((s) => s.trim()).filter(Boolean); break;
      case "--shutdown": args.shutdown = argv[++i]; break;
      case "--preflight": args.preflight = argv[++i]; break;
      default:
        throw new Error(`unknown argument: ${a}`);
    }
  }
  for (const req of ["out", "meta", "profile"]) {
    if (!args[req]) throw new Error(`--${req} is required`);
  }
  if (!args.runs.length) throw new Error("--runs is required (comma-separated list of run JSON files)");
  return args;
}

async function readJson(p) {
  return JSON.parse(await readFile(p, "utf8"));
}

export async function main(argv = process.argv.slice(2)) {
  const args = parseArgs(argv);
  const meta = await readJson(args.meta);
  const profile = await readJson(args.profile);
  const runs = await Promise.all(args.runs.map(readJson));
  const shutdown = args.shutdown ? await readJson(args.shutdown) : null;
  const preflight = args.preflight ? await readJson(args.preflight) : null;

  const report = buildReport({ meta, profile, runs, shutdown, preflight });

  await mkdir(args.out, { recursive: true });
  await writeFile(path.join(args.out, "report.json"), JSON.stringify(report, null, 2), "utf8");
  const html = renderReport(report);
  await writeFile(path.join(args.out, "report.html"), html, "utf8");
  await writeShots(report, args.out);

  const s = report.summary;
  console.log(`RESULT overall=${s.overall} pass=${s.pass} fail=${s.fail} skipped=${s.skipped} evidence=${s.evidence}`);
  if (s.overall === "FAIL") process.exitCode = 1;
  return report;
}

const isMain = process.argv[1] && fileURLToPath(import.meta.url) === path.resolve(process.argv[1]);
if (isMain) {
  main().catch((err) => {
    console.error(err.stack || String(err));
    process.exit(1);
  });
}
