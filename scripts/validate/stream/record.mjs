// Writes a run bundle (docs/testing-e2e-suite.md). Redaction works on VALUES, before anything is
// serialised: the exact secrets this run holds are removed first, then the validator's own
// patterns (defence in depth). Serialised text is never edited, so a credential in an echoed
// body cannot corrupt the JSON around it.
import fs from "node:fs";
import path from "node:path";
import { scrub } from "./bundle.mjs";

// A key named like a credential is dropped with its value: the validator refuses such a member
// whatever it holds, and nothing in a bundle needs one.
const CREDENTIAL_KEY = /^(password|passwd|secret|token|authorization|access_token|refresh_token|sig_token|api[_-]?key)$/i;

export class Recorder {
  /**
   * @param literals [text, marker] pairs replaced wherever they appear (the target's host name, the test user's sign-in).
   * @param secrets  a live Set of strings the run learns later (the bearers); each becomes "<credential>".
   */
  constructor(dir, { literals = [], secrets = new Set(), now = Date.now } = {}) {
    this.dir = dir;
    this.now = now;
    this.redacted = {};
    this.literals = literals.filter(([t]) => t);
    this.secrets = secrets;
    this.known = -1;
    fs.mkdirSync(dir, { recursive: true });
  }

  /** `s` as it may be written: exact literals (and their URL- and JSON-escaped spellings), then the patterns. */
  text(s) {
    if (this.known !== this.secrets.size) {
      this.known = this.secrets.size;
      this.pairs = [...this.literals, ...[...this.secrets].filter(Boolean).map((t) => [t, "<credential>"])]
        .flatMap(([t, marker]) => [...new Set([t, encodeURIComponent(t), JSON.stringify(t).slice(1, -1)])].map((v) => [v, marker]))
        .sort((a, b) => b[0].length - a[0].length);
    }
    let out = s;
    for (const [lit, marker] of this.pairs) {
      const parts = out.split(lit);
      if (parts.length > 1) this.redacted.literals = (this.redacted.literals ?? 0) + parts.length - 1;
      out = parts.join(marker);
    }
    return scrub(out, this.redacted);
  }

  /** A copy of `v` with every string cleaned and every credential-named member dropped. */
  value(v) {
    if (typeof v === "string") return this.text(v);
    if (Array.isArray(v)) return v.map((x) => this.value(x));
    if (v === null || typeof v !== "object") return v;
    const out = {};
    for (const [k, x] of Object.entries(v)) {
      if (CREDENTIAL_KEY.test(k)) this.redacted.credential_keys = (this.redacted.credential_keys ?? 0) + 1;
      else out[this.text(k)] = this.value(x);
    }
    return out;
  }

  file(rel) {
    const file = path.join(this.dir, rel);
    fs.mkdirSync(path.dirname(file), { recursive: true });
    return file;
  }

  put(rel, text) { fs.writeFileSync(this.file(rel), this.text(text)); }
  append(rel, text) { fs.appendFileSync(this.file(rel), this.text(text)); }
  json(rel, value) { fs.writeFileSync(this.file(rel), `${JSON.stringify(this.value(value), null, 2)}\n`); }
  row(rel, value) { fs.appendFileSync(this.file(rel), `${JSON.stringify(this.value(value))}\n`); }
  jsonl(rel, rows) { fs.writeFileSync(this.file(rel), rows.map((r) => `${JSON.stringify(this.value(r))}\n`).join("")); }

  /** A picture is binary, so it is not scrubbed: only the page's own pixels, never browser chrome. */
  async shot(page, rel) {
    await page.screenshot({ path: this.file(rel) });
  }
}

/** A scenario's recorder: `step()` writes a timeline row as it happens, so a crash keeps the story. */
export class ScenarioRecorder {
  /** @param clock the browser clock offset: a timeline stamp is the runner's time plus this (null: the runner's own). */
  constructor(bundle, dirName, { clockOffset = () => null } = {}) {
    this.bundle = bundle;
    this.dirName = dirName;
    this.clockOffset = clockOffset;
    bundle.put(`${dirName}/timeline.jsonl`, "");
  }

  /** The timeline's clock is the browser's when the offset is known, else the runner's (named in `clock_source`). */
  step(ev, data = {}) {
    const off = this.clockOffset();
    this.bundle.row(`${this.dirName}/timeline.jsonl`, { ts_unix_ms: this.bundle.now() + (off ?? 0), clock: "client", clock_source: off === null ? "runner" : "browser", ev, ...data });
  }

  json(rel, v) { this.bundle.json(`${this.dirName}/${rel}`, v); }
  jsonl(rel, rows) { this.bundle.jsonl(`${this.dirName}/${rel}`, rows); }
  put(rel, text) { this.bundle.put(`${this.dirName}/${rel}`, text); }
  shot(page, label) { return this.bundle.shot(page, `${this.dirName}/shots/${label}.png`); }
}
