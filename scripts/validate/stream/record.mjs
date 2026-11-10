// Writes a run bundle (docs/testing-e2e-suite.md). Every text file goes through scrub() at
// write time: no address, token or password reaches the disk.
import fs from "node:fs";
import path from "node:path";
import { scrub } from "./bundle.mjs";

export class Recorder {
  /** @param literals [text, marker] pairs (the target's host name, the test user's sign-in) replaced wherever they appear. */
  constructor(dir, { literals = [], now = Date.now } = {}) {
    this.dir = dir;
    this.now = now;
    this.redacted = {};
    this.literals = literals.filter(([t]) => t);
    fs.mkdirSync(dir, { recursive: true });
  }

  clean(text) {
    let out = text;
    for (const [lit, marker] of this.literals) {
      const parts = out.split(lit);
      if (parts.length > 1) this.redacted.literals = (this.redacted.literals ?? 0) + parts.length - 1;
      out = parts.join(marker);
    }
    return scrub(out, this.redacted);
  }

  put(rel, text) {
    const file = path.join(this.dir, rel);
    fs.mkdirSync(path.dirname(file), { recursive: true });
    fs.writeFileSync(file, this.clean(text));
  }

  json(rel, value) {
    this.put(rel, `${JSON.stringify(value, null, 2)}\n`);
  }

  append(rel, text) {
    const file = path.join(this.dir, rel);
    fs.mkdirSync(path.dirname(file), { recursive: true });
    fs.appendFileSync(file, this.clean(text));
  }

  jsonl(rel, rows) {
    this.put(rel, rows.map((r) => JSON.stringify(r)).join("\n") + (rows.length ? "\n" : ""));
  }

  /** A picture is binary, so it is not scrubbed: only the page's own pixels, never browser chrome. */
  async shot(page, rel) {
    const file = path.join(this.dir, rel);
    fs.mkdirSync(path.dirname(file), { recursive: true });
    await page.screenshot({ path: file });
  }
}

/** A scenario's recorder: `step()` writes a timeline row as it happens, so a crash keeps the story. */
export class ScenarioRecorder {
  constructor(bundle, dirName) {
    this.bundle = bundle;
    this.dirName = dirName;
    bundle.put(`${dirName}/timeline.jsonl`, "");
  }

  step(ev, data = {}) {
    this.bundle.append(`${this.dirName}/timeline.jsonl`, `${JSON.stringify({ ts_unix_ms: this.bundle.now(), clock: "client", ev, ...data })}\n`);
  }

  json(rel, v) { this.bundle.json(`${this.dirName}/${rel}`, v); }
  jsonl(rel, rows) { this.bundle.jsonl(`${this.dirName}/${rel}`, rows); }
  put(rel, text) { this.bundle.put(`${this.dirName}/${rel}`, text); }
  shot(page, label) { return this.bundle.shot(page, `${this.dirName}/shots/${label}.png`); }
}
