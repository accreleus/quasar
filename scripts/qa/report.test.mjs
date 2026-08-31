// scripts/qa/report.test.mjs — golden/self-containment tests for report.mjs.
// Run: node --test scripts/qa/report.test.mjs
import test from "node:test";
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import path from "node:path";
import { renderReport } from "./report.mjs";

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const fixturePath = path.join(__dirname, "testdata", "report.json");

async function loadFixture() {
  const raw = await readFile(fixturePath, "utf8");
  return JSON.parse(raw);
}

test("renders without throwing from the fixture", async () => {
  const report = await loadFixture();
  const html = renderReport(report);
  assert.equal(typeof html, "string");
  assert.ok(html.startsWith("<!doctype html>"));
});

test("output is self-contained: no shots/ paths, no http(s) asset refs", async () => {
  const report = await loadFixture();
  const html = renderReport(report);
  assert.ok(!html.includes('src="shots/'), 'must not reference src="shots/...');
  // Text may legitimately mention an https:// stack API in the meta table,
  // but no asset (image/script/stylesheet) may be *loaded* from the network.
  assert.ok(!/src="https?:\/\//.test(html), "must not load any http(s):// image/script asset");
  assert.ok(!/href="https?:\/\/[^"]*\.(css|js)"/.test(html), "must not load any http(s):// stylesheet/script asset");
  // All <img> sources must be inline data URIs.
  const imgSrcs = [...html.matchAll(/<img[^>]*\ssrc="([^"]*)"/g)].map((m) => m[1]);
  for (const src of imgSrcs) {
    assert.ok(src.startsWith("data:image/png;base64,"), `img src must be an inline data URI, got: ${src}`);
  }
});

test("SKIPPED gate is not rendered as a pass, overall is PASS, SKIPPED badge present", async () => {
  const report = await loadFixture();
  const html = renderReport(report);

  // Overall verdict from the fixture (no FAIL gates) is PASS.
  assert.ok(html.includes('<span class="overall pass">PASS</span>'));

  // The SKIPPED keyboard gate must carry the skip badge, never a pass badge,
  // in its own section.
  const skipSectionMatch = html.match(/<section class="gate skip">[\s\S]*?<\/section>/);
  assert.ok(skipSectionMatch, "expected a gate.skip section for the keyboard gate");
  const skipSection = skipSectionMatch[0];
  assert.ok(skipSection.includes('<span class="badge skip">SKIPPED</span>'));
  assert.ok(!skipSection.includes('badge pass'));

  assert.ok(html.includes('<span class="badge skip">SKIPPED</span>'));
});

test("a FAIL gate flips overall to FAIL", async () => {
  const report = await loadFixture();
  const mutated = JSON.parse(JSON.stringify(report));
  mutated.gates[2].verdict = "FAIL";
  mutated.gates[2].reason = "luma below threshold";
  const html = renderReport(mutated);
  assert.ok(html.includes('<span class="overall fail">FAIL</span>'));
  assert.ok(html.includes('<span class="badge fail">FAIL</span>'));
});

test("invalid png_b64 payload is dropped, not emitted", async () => {
  const report = await loadFixture();
  const mutated = JSON.parse(JSON.stringify(report));
  mutated.gates[2].screenshots[0].png_b64 = "not-valid-base64!!! <script>";
  const html = renderReport(mutated);
  assert.ok(!html.includes("not-valid-base64"));
  assert.ok(html.includes("screenshot dropped: invalid payload"));
});

test("a <script> tag in a gate title/reason comes out escaped", async () => {
  const report = await loadFixture();
  const mutated = JSON.parse(JSON.stringify(report));
  mutated.gates[0].title = '<script>alert(1)</script>';
  mutated.gates[0].reason = '<script>alert(2)</script>';
  const html = renderReport(mutated);
  assert.ok(!html.includes("<script>alert(1)</script>"));
  assert.ok(!html.includes("<script>alert(2)</script>"));
  assert.ok(html.includes("&lt;script&gt;alert(1)&lt;/script&gt;"));
  assert.ok(html.includes("&lt;script&gt;alert(2)&lt;/script&gt;"));
});

test("EVIDENCE gate carries in-body 'no assertion' language", async () => {
  const report = await loadFixture();
  const html = renderReport(report);
  const evidenceSectionMatch = html.match(/<section class="gate evidence">[\s\S]*?<\/section>/);
  assert.ok(evidenceSectionMatch, "expected a gate.evidence section for the oracle gate");
  const section = evidenceSectionMatch[0];
  assert.ok(/no assertion/i.test(section));
  assert.ok(/you (do|look)/i.test(section));
});

test("missing/optional fields degrade gracefully (no 'undefined' leaks)", async () => {
  const minimal = {
    image: { tag: "quasar-minimal:dev" },
    host: "devbox",
    profile: "generic",
    runs: 1,
    generated_at: "2026-08-13T00:00:00Z",
    summary: { pass: 1, fail: 0, skipped: 0, evidence: 0 },
    gates: [
      { id: "preflight", n: "0", title: "preflight", verdict: "PASS" },
    ],
  };
  const html = renderReport(minimal);
  assert.ok(!html.includes("undefined"));
});
