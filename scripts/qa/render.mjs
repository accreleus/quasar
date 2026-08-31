#!/usr/bin/env node
// scripts/qa/render.mjs — tiny CLI wrapper around report.mjs.
// Usage: node scripts/qa/render.mjs <report.json> <out.html>
import { readFile, writeFile } from "node:fs/promises";
import { renderReport } from "./report.mjs";

async function main() {
  const [, , inPath, outPath] = process.argv;
  if (!inPath || !outPath) {
    console.error("usage: node scripts/qa/render.mjs <report.json> <out.html>");
    process.exit(2);
  }
  const raw = await readFile(inPath, "utf8");
  const report = JSON.parse(raw);
  const html = renderReport(report);
  await writeFile(outPath, html, "utf8");
  console.log(`wrote ${outPath}`);
}

main().catch((err) => {
  console.error(err.stack || String(err));
  process.exit(1);
});
