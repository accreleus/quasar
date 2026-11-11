// Hooks of make e2e-run (docs/testing-e2e-suite.md "Hooks"): a step a scenario cannot do itself
// (a transport fault, network shaping) is a script the caller supplies. The directory HOOKS= is the
// caller's own and trusted; a hook NAME comes from a scenario module (trusted code) but is shape-checked
// and must resolve to a file inside that directory. A script runs as `<dir>/<name with ':' as '-'> apply|clear`,
// never through a shell, with a timeout.
//
// Every applied hook is cleared on every exit path. A runner killed between apply and clear leaves its
// ledger entry behind (written BEFORE the apply); the next run with the same directory clears it first.
import { spawn } from "node:child_process";
import crypto from "node:crypto";
import fs from "node:fs";
import path from "node:path";

export const HOOK_NAME = /^[a-z]+:[a-z0-9-]{1,40}$/;
export const LEDGER = ".e2e-hooks-applied.json";
export const DEFAULT_TIMEOUT_MS = 30_000;
const PHASES = ["apply", "clear"];
const OUTPUT_MAX = 2000; // characters of each stream written to the timeline
const READ_MAX = 64_000; // bytes of each stream kept at all
const FACT = /^[\w.:-]{1,100}$/;

/** The hook names a scenario module declares; a name of the wrong shape is a defect of the module, said before anything runs. */
export function declaredHooks(scenario) {
  const names = scenario.hooks ?? [];
  if (!Array.isArray(names) || names.some((n) => typeof n !== "string" || !HOOK_NAME.test(n)) || new Set(names).size !== names.length) {
    throw new Error(`the scenario declares hooks that are not a list of distinct names like fault:webtransport-block (${JSON.stringify(names).slice(0, 80)})`);
  }
  return names;
}

/** The executable for `name` inside `dir`, or null: a missing file, one that is not a plain executable file, or a link out of the directory. */
export function hookScript(dir, name) {
  if (typeof name !== "string" || !HOOK_NAME.test(name)) return null;
  try {
    const root = fs.realpathSync(dir);
    const file = fs.realpathSync(path.join(root, name.replace(":", "-")));
    if (path.dirname(file) !== root || !fs.statSync(file).isFile()) return null;
    fs.accessSync(file, fs.constants.X_OK);
    return file;
  } catch {
    return null;
  }
}

/** Runs `file phase` with `env`; resolves (never rejects) with the exit, the output as read (the caller redacts, then caps it) and whether the timeout killed it. */
export function runHook(file, phase, env, timeoutMs, onSpawn = () => {}) {
  return new Promise((resolve) => {
    const t0 = Date.now();
    const out = { stdout: "", stderr: "" };
    const take = (k) => (b) => { if (out[k].length < READ_MAX) out[k] += b.toString("utf8"); };
    let done = false;
    let child;
    const end = (r) => {
      if (done) return;
      done = true;
      clearTimeout(timer);
      resolve({ ...out, ms: Date.now() - t0, code: null, signal: null, timedOut: false, ...r });
    };
    // detached: the whole process group is killed at the timeout, so a hook's own children cannot outlive it.
    const timer = setTimeout(() => { try { process.kill(-child.pid, "SIGKILL"); } catch { /* already gone */ } end({ timedOut: true }); }, timeoutMs);
    try {
      child = spawn(file, [phase], { env, cwd: path.dirname(file), stdio: ["ignore", "pipe", "pipe"], detached: true, shell: false });
    } catch (e) {
      return end({ stderr: String(e?.message ?? e) });
    }
    onSpawn(child.pid); // detached: the pid is the hook's process group
    child.stdout.on("data", take("stdout"));
    child.stderr.on("data", take("stderr"));
    child.on("error", (e) => end({ stderr: String(e?.message ?? e) }));
    child.on("close", (code, signal) => end({ code, signal }));
  });
}

const ok = (r) => r.code === 0 && !r.timedOut;
const why = (r) => (r.timedOut ? "timed out" : r.signal ? `killed by ${r.signal}` : `exit ${r.code}`);

function isAlive(pid) {
  try { process.kill(pid, 0); return true; } catch (e) { return e.code === "EPERM"; }
}
const STATES = ["applying", "applied", "clearing"];
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

export class Hooks {
  /** @param ledgerDir where the ledger of applied hooks lives: the bundle's parent (the staging directory is beside the bundle). */
  constructor({ dir, ledgerDir, env = process.env, timeoutMs = DEFAULT_TIMEOUT_MS, redact = (s) => s }) {
    this.redact = redact; // the recorder's: output is redacted whole and only then cut, so no secret is cut in half
    this.root = fs.realpathSync(dir);
    this.ledger = path.join(ledgerDir, LEDGER);
    this.env = env;
    this.timeoutMs = timeoutMs;
    this.applied = new Map(); // name -> { id, facts }
  }

  has(name) { return hookScript(this.root, name) !== null; }

  readLedger() {
    let text;
    try { text = fs.readFileSync(this.ledger, "utf8"); } catch (e) { if (e.code === "ENOENT") return []; throw e; }
    let entries;
    try { entries = JSON.parse(text).entries; } catch { entries = null; }
    const valid = (e) => e && typeof e.id === "string" && Number.isInteger(e.pid) && typeof e.hooks_dir === "string" && HOOK_NAME.test(e.name)
      && STATES.includes(e.state) && (e.hook_pid === undefined || (Number.isInteger(e.hook_pid) && e.hook_pid > 1))
      && e.facts && typeof e.facts === "object" && Object.values(e.facts).every((v) => FACT.test(v));
    if (!Array.isArray(entries) || !entries.every(valid)) throw new Error(`${this.ledger} is not a ledger this runner wrote; it cannot say what is still applied, so it is not guessed (remove it once the hooks are known to be clear)`);
    return entries;
  }

  /** Temp file then rename, so a runner killed mid-write never leaves half a ledger. */
  writeLedger(entries) {
    if (!entries.length) { fs.rmSync(this.ledger, { force: true }); return; }
    const tmp = `${this.ledger}.${crypto.randomBytes(4).toString("hex")}.tmp`;
    fs.writeFileSync(tmp, `${JSON.stringify({ format: 1, entries })}\n`);
    fs.renameSync(tmp, this.ledger);
  }

  /** The environment a hook sees: the caller's own, plus the scenario's facts (the session only when it is known). */
  hookEnv(phase, f) {
    // A hook shapes a link or blocks a transport: it has no use for the test user's sign-in.
    const { QUASAR_E2E_EMAIL: _e, QUASAR_E2E_PASSWORD: _p, QUASAR_E2E_CRED_FILE: _f, ...env } = this.env;
    return {
      ...env,
      QUASAR_E2E_SCENARIO: f.scenario,
      ...(f.session ? { QUASAR_E2E_SESSION: f.session } : {}),
      QUASAR_E2E_TRANSPORT: f.transport,
      QUASAR_E2E_PHASE: phase,
    };
  }

  /** One entry's state in the ledger; `hook_pid` is there only while that hook process may be running. */
  mark(id, state, hookPid) {
    this.writeLedger(this.readLedger().map((e) => {
      if (e.id !== id) return e;
      const { hook_pid: _was, ...rest } = e;
      return { ...rest, state, ...(hookPid ? { hook_pid: hookPid } : {}) };
    }));
  }

  /** Runs the script for ledger entry `id`. While it runs the ledger holds its process group, so a later runner never clears over a hook that is still going. */
  async run(name, phase, facts, note, id) {
    const file = hookScript(this.root, name);
    const going = phase === "apply" ? "applying" : "clearing";
    if (file) this.mark(id, going);
    const r = file ? await runHook(file, phase, this.hookEnv(phase, facts), this.timeoutMs, (pid) => this.mark(id, going, pid)) : { code: null, signal: null, timedOut: false, ms: 0, stdout: "", stderr: "the script is gone or not executable" };
    note("hook", { hook: name, phase, exit_code: r.code, ...(r.signal ? { signal: r.signal } : {}), timed_out: r.timedOut, ms: r.ms, stdout: this.redact(r.stdout).slice(0, OUTPUT_MAX), stderr: this.redact(r.stderr).slice(0, OUTPUT_MAX) });
    // The hook process is gone (runHook waited for it, or killed its group): only now is the entry plain `applied` again.
    if (file) this.mark(id, "applied");
    return r;
  }

  /** `note(ev, data)` records a timeline row (the recorder redacts it). Throws when the hook fails; an apply that failed is still owed a clear. */
  async call(name, phase, facts, note) {
    if (!PHASES.includes(phase)) throw new Error(`a hook phase is apply or clear, not ${JSON.stringify(String(phase)).slice(0, 20)}`);
    const mine = { scenario: String(facts.scenario), transport: String(facts.transport), ...(facts.session ? { session: String(facts.session) } : {}) };
    // The same shape the ledger is read with: a fact that would not read back is refused before anything is applied.
    if (!Object.values(mine).every((v) => FACT.test(v))) throw new Error(`hook ${name}: the scenario's facts (its id, transport and session) must be plain names`);
    if (phase === "apply") {
      if (this.applied.has(name)) throw new Error(`hook ${name} is already applied`);
      const id = crypto.randomBytes(6).toString("hex");
      // The ledger first: a runner killed during the apply must still be known to have started it.
      this.writeLedger([...this.readLedger(), { id, pid: process.pid, hooks_dir: this.root, name, state: "applying", facts: mine }]);
      this.applied.set(name, { id, facts: mine });
      const r = await this.run(name, "apply", mine, note, id);
      if (!ok(r)) throw new Error(`hook ${name} apply failed (${why(r)})`);
      return;
    }
    const a = this.applied.get(name);
    if (!a) throw new Error(`hook ${name} is not applied`);
    const r = await this.run(name, "clear", a.facts, note, a.id);
    if (!ok(r)) throw new Error(`hook ${name} clear failed (${why(r)})`);
    this.applied.delete(name);
    this.writeLedger(this.readLedger().filter((e) => e.id !== a.id));
  }

  /** Clears everything still applied, last applied first. Returns what could not be cleared (it stays applied, and in the ledger). */
  async clearAll(note) {
    const left = [];
    for (const name of [...this.applied.keys()].reverse()) {
      try { await this.call(name, "clear", {}, note); } catch (e) { left.push(`${e.message}`); }
    }
    return left;
  }

  /** Clears what a runner that died left applied for this directory, before anything else. Refuses to start when it cannot. Returns the names cleared. */
  async recover(note) {
    const mine = this.readLedger().filter((e) => e.hooks_dir === this.root);
    const live = mine.filter((e) => isAlive(e.pid));
    if (live.length) throw new Error(`refusing to start: process ${live[0].pid} still holds hook ${live[0].name}; it is another e2e-run using these hooks`);
    // A runner killed while a hook ran left that hook running (it is its own process group). Clearing over it
    // would let the old apply land after the clear, so it is waited for; one that does not end stops the run.
    for (const e of mine.filter((x) => x.state !== "applied")) {
      if (!e.hook_pid) throw new Error(`refusing to start: hook ${e.name} was ${e.state} when its runner died and its process is not known; remove ${this.ledger} once the hook is known to be clear`);
      const until = Date.now() + this.timeoutMs;
      while (isAlive(-e.hook_pid) && Date.now() < until) await sleep(100);
      if (isAlive(-e.hook_pid)) throw new Error(`refusing to start: hook ${e.name} from a runner that died is still running (process group ${e.hook_pid}); end it, then run again`);
    }
    const cleared = [];
    const failed = [];
    for (const e of mine.reverse()) {
      const r = await this.run(e.name, "clear", e.facts, note, e.id);
      if (ok(r)) {
        this.writeLedger(this.readLedger().filter((x) => x.id !== e.id));
        cleared.push(e.name);
      } else failed.push(`${e.name} (${why(r)})`);
    }
    if (failed.length) throw new Error(`refusing to start: a runner that died left hook(s) applied and they would not clear: ${failed.join(", ")}`);
    return cleared;
  }
}
