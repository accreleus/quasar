#!/usr/bin/env node
// scripts/validate/runner.mjs — loads journeys/*.mjs, runs each in a fresh
// browser context with the right storageState, captures screenshots/console/
// network evidence, and writes report.json + report.html.
//
// docs/design/plans/2026-08-07-validation-harness-spec.md is the contract.
// Invoked by scripts/dx/validate.sh; not meant to be run standalone (though it
// can be, with the same flags, for iterating on a journey).
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { chromium } from "playwright";
import { parseArgs, requireArg } from "./lib/args.mjs";
import { attachEvidenceListeners } from "./lib/listeners.mjs";
import { renderReport } from "./report.mjs";

const __dirname = path.dirname(fileURLToPath(import.meta.url));

async function loadJourneys(level) {
  const dir = path.join(__dirname, "journeys");
  const files = fs
    .readdirSync(dir)
    .filter((f) => f.endsWith(".mjs"))
    .sort();
  const journeys = [];
  for (const file of files) {
    const mod = await import(path.join(dir, file));
    const journey = mod.default;
    if (!journey || !journey.name || !journey.run) {
      throw new Error(`journeys/${file} does not export a valid default journey`);
    }
    journeys.push(journey);
  }
  // ui level runs every "ui" journey. session level runs ui + session
  // journeys (the session-launch journey is the only "session" one, and it
  // self-refuses against TARGET=local — see journeys/session-launch.mjs).
  const wanted = level === "session" ? new Set(["ui", "session"]) : new Set(["ui"]);
  return journeys.filter((j) => wanted.has(j.level));
}

function storageStateFor(role, args) {
  if (role === "admin") return args["admin-state"];
  if (role === "user") return args["user-state"];
  return undefined; // anon
}

// --skipped "name1=reason1,name2=reason2" — journeys the orchestrator decided
// not to run at all (e.g. session-launch under LEVEL=all TARGET=local — see
// scripts/dx/validate.sh). Surfaced in report.json/.html as a distinct
// "skipped" list so LEVEL=all never silently narrows to what LEVEL=ui ran.
function parseSkipped(raw) {
  if (!raw) return [];
  return raw
    .split(",")
    .map((entry) => entry.trim())
    .filter(Boolean)
    .map((entry) => {
      const eq = entry.indexOf("=");
      if (eq < 0) return { name: entry, reason: "" };
      return { name: entry.slice(0, eq), reason: entry.slice(eq + 1) };
    });
}

async function runJourney(browser, journey, args) {
  const start = Date.now();
  const shotsDir = path.join(args["results-dir"], "shots", journey.name);
  fs.mkdirSync(shotsDir, { recursive: true });

  const consoleErrors = [];
  const networkFailures = [];
  const screenshots = [];
  const warnings = [];
  let verdict = "PASS";
  let reason = "";
  let context;

  const metrics = {};

  try {
    context = await browser.newContext({
      storageState: storageStateFor(journey.role, args),
      ignoreHTTPSErrors: true, // target stacks use self-signed certs
    });
    const page = await context.newPage();
    const evidence = { consoleErrors, networkFailures };
    attachEvidenceListeners(page, args["base-url"], evidence);

    const journeyCtx = {
      browser,
      baseUrl: args["base-url"],
      target: args.target,
      resultsDir: args["results-dir"],
      userState: args["user-state"],
      adminState: args["admin-state"],
      metrics,
      warnings,
      evidence,
      // Instruments a SECONDARY page/context the same way the primary one is
      // instrumented (MINOR 8, adversarial review) — used by
      // lib/adminGate.mjs's assertUserDenied, which opens its own context for
      // the user-role identity.
      attachListeners(p) {
        attachEvidenceListeners(p, args["base-url"], evidence);
      },
      async screenshot(p, label) {
        const file = path.join(shotsDir, `${label}.png`);
        await p.screenshot({ path: file, fullPage: true });
        screenshots.push(path.relative(args["results-dir"], file));
      },
    };

    await journey.run(page, journeyCtx);
    await journeyCtx.screenshot(page, "final");

    if (consoleErrors.length > 0) {
      verdict = "FAIL";
      reason = `console error(s) from ${args["base-url"]}: ${consoleErrors.join(" | ")}`;
    }
    if (networkFailures.length > 0) {
      verdict = "FAIL";
      reason = (reason ? `${reason}; ` : "") + `network failure(s): ${networkFailures.join(" | ")}`;
    }
  } catch (err) {
    verdict = "FAIL";
    reason = err instanceof Error ? err.message : String(err);
    try {
      if (context) {
        const pages = context.pages();
        const page = pages[pages.length - 1];
        if (page) {
          const file = path.join(shotsDir, "failure.png");
          await page.screenshot({ path: file, fullPage: true });
          screenshots.push(path.relative(args["results-dir"], file));
        }
      }
    } catch {
      // best-effort — never let the failure screenshot mask the real error
    }
  } finally {
    if (context) await context.close();
  }

  return {
    name: journey.name,
    role: journey.role,
    level: journey.level,
    verdict,
    reason,
    duration_ms: Date.now() - start,
    screenshots,
    console_errors: consoleErrors,
    network_failures: networkFailures,
    warnings,
    metrics,
  };
}

async function main() {
  const args = parseArgs(process.argv.slice(2));
  const level = requireArg(args, "level");
  requireArg(args, "target");
  requireArg(args, "base-url");
  requireArg(args, "results-dir");
  requireArg(args, "user-state");
  requireArg(args, "admin-state");

  if (!["ui", "session"].includes(level)) {
    throw new Error(`runner.mjs --level must be ui|session (got '${level}')`);
  }

  fs.mkdirSync(args["results-dir"], { recursive: true });

  const journeys = await loadJourneys(level);
  if (journeys.length === 0) {
    throw new Error(`no journeys matched level=${level}`);
  }

  const browser = await chromium.launch();
  const results = [];
  try {
    for (const journey of journeys) {
      // eslint-disable-next-line no-await-in-loop
      const result = await runJourney(browser, journey, args);
      results.push(result);
      const mark = result.verdict === "PASS" ? "PASS" : "FAIL";
      console.log(`${mark} ${result.name} (${result.duration_ms}ms)${result.reason ? " — " + result.reason : ""}`);
    }
  } finally {
    await browser.close();
  }

  const skipped = parseSkipped(args.skipped);

  const summary = {
    pass: results.filter((r) => r.verdict === "PASS").length,
    fail: results.filter((r) => r.verdict === "FAIL").length,
    total: results.length,
    skipped: skipped.length,
  };

  const report = {
    target: args["base-url"],
    target_kind: args.target,
    level,
    commit: args.commit || "unknown",
    generated_at: new Date().toISOString(),
    journeys: results,
    skipped,
    summary,
  };

  fs.writeFileSync(path.join(args["results-dir"], "report.json"), JSON.stringify(report, null, 2));
  fs.writeFileSync(path.join(args["results-dir"], "report.html"), renderReport(report));

  if (skipped.length > 0) {
    console.log(`SKIPPED ${skipped.map((s) => `${s.name} (${s.reason})`).join(", ")}`);
  }
  console.log(`RESULT status=${summary.fail > 0 ? "failed" : "ok"} pass=${summary.pass} fail=${summary.fail} total=${summary.total} skipped=${summary.skipped}`);
  process.exit(summary.fail > 0 ? 1 : 0);
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
