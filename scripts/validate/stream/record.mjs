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
    this.problems = []; // anything that makes the bundle unsafe to keep: the runner treats it as a failed validation
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

  isSecret(t) {
    this.text(""); // brings the spellings up to date
    return this.pairs.some(([lit]) => lit === t);
  }

  /** A copy of `v` with every string cleaned and every credential-named member dropped. */
  value(v) {
    if (typeof v === "string") return this.text(v);
    if (Array.isArray(v)) return v.map((x) => this.value(x));
    if (v === null || typeof v !== "object") return v;
    const out = {};
    for (const [k, x] of Object.entries(v)) {
      // A key is an identity: never rewritten by literal replacement. One that names a credential, or is a secret itself, is dropped.
      if (CREDENTIAL_KEY.test(k) || this.isSecret(k)) this.redacted.credential_keys = (this.redacted.credential_keys ?? 0) + 1;
      else {
        const key = scrub(k, this.redacted);
        if (Object.hasOwn(out, key)) this.problems.push(`redaction would make two members of one object the same key ${JSON.stringify(key.slice(0, 40))}`);
        out[key] = this.value(x);
      }
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
    // What the page says (console lines, errors, API bodies) can carry its bearer, which the runner learns only after sign-in:
    // it is held in memory until release() and written then, through the redactor. dropHeld() discards it if the bearer never became known.
    this.holding = true;
    this.held = [];
  }

  fromPage(rel, text) {
    if (this.holding) this.held.push(() => this.bundle.append(`${this.dirName}/${rel}`, text));
    else this.bundle.append(`${this.dirName}/${rel}`, text);
  }

  /** A timeline row about what the page did (an API answer's body), held like fromPage. */
  pageStep(ev, data = {}) {
    const row = this.row(ev, data);
    if (this.holding) this.held.push(() => this.bundle.row(`${this.dirName}/timeline.jsonl`, row));
    else this.bundle.row(`${this.dirName}/timeline.jsonl`, row);
  }

  release() {
    this.holding = false;
    for (const write of this.held.splice(0)) write();
  }

  /** The number of held lines discarded; holding stays on, so nothing later is written either. */
  dropHeld() {
    return this.held.splice(0).length;
  }

  /** The timeline's clock is the browser's when the offset is known, else the runner's (named in `clock_source`). */
  row(ev, data) {
    const off = this.clockOffset();
    return { ts_unix_ms: this.bundle.now() + (off ?? 0), clock: "client", clock_source: off === null ? "runner" : "browser", ev, ...data };
  }

  step(ev, data = {}) {
    this.bundle.row(`${this.dirName}/timeline.jsonl`, this.row(ev, data));
  }

  json(rel, v) { this.bundle.json(`${this.dirName}/${rel}`, v); }
  jsonl(rel, rows) { this.bundle.jsonl(`${this.dirName}/${rel}`, rows); }
  put(rel, text) { this.bundle.put(`${this.dirName}/${rel}`, text); }
  shot(page, label) { return this.bundle.shot(page, `${this.dirName}/shots/${label}.png`); }
}
