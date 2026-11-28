// Keeps the lint rules from dying quietly: `node --test`, run by
// scripts/verify/lint.d/web-eslint.sh.
import assert from "node:assert/strict";
import { test } from "node:test";
import { ESLint, Linter } from "eslint";
import { houseBans } from "./eslint.config.js";

const hits = (code) => new Linter().verify(code, { rules: { "no-restricted-syntax": ["error", ...houseBans] } }).length;

const bad = [
  "setInterval(async () => { await poll(); }, 1000)",
  "setInterval(() => void poll(), 1000)",
  "window.setInterval(() => { void poll(); }, 1000)",
  "setInterval(function () { void poll(); }, 1000)",
  "useEffect(() => { measure(); })",
  "useLayoutEffect(() => { measure(); observe(); return stop; })",
  "useEffect(() => measure())",
  "navigator.gpu.requestAdapter()",
  "adapter.requestDevice({})",
  "boundedRequest(other(adapter.requestDevice()), 1)",
];
const good = [
  "setInterval(() => setNow(Date.now()), 1000)",
  "setTimeout(() => void poll(), 1000)",
  "useEffect(() => { measure(); }, [open])",
  "useLayoutEffect(() => { if (!open) measure(); })",
  "useEffect(() => { latest.current = value; })",
  "boundedRequest(navigator.gpu.requestAdapter(), 1)",
  "boundedRequest(adapter.requestDevice({}), 1, drop)",
];

test("each house ban fires on its bad shape", () => {
  for (const code of bad) assert.ok(hits(code) > 0, `no finding for: ${code}`);
});
test("each house alternative passes", () => {
  for (const code of good) assert.equal(hits(code), 0, `finding for: ${code}`);
});
// Typed rules need a file the tsconfig knows, so each fixture is linted as if it were src/main.tsx.
const typed = {
  "react-hooks/rules-of-hooks": "export function C(p: { on: boolean }) { if (p.on) useState(0); return null; }",
  "react-hooks/exhaustive-deps": "export function C(p: { id: string }) { useEffect(() => { document.title = p.id; }, []); return null; }",
  "@typescript-eslint/no-floating-promises": "export function f() { Promise.resolve(1); }",
  "@typescript-eslint/no-misused-promises": "export function f(el: HTMLElement) { el.addEventListener('x', async () => {}); }",
  "@typescript-eslint/switch-exhaustiveness-check": "export function f(v: 'a' | 'b') { switch (v) { case 'a': break; } }",
  "no-restricted-syntax": "export function f() { setInterval(async () => {}, 1); }",
  // An unused disable directive carries no rule id.
  null: "// eslint-disable-next-line react-hooks/exhaustive-deps\nexport const x = 1;",
};

test("every rule is an error on source files", async () => {
  const eslint = new ESLint({ cwd: import.meta.dirname });
  for (const [rule, code] of Object.entries(typed)) {
    const [{ messages }] = await eslint.lintText(code, { filePath: "src/main.tsx" });
    assert.ok(
      messages.some((m) => String(m.ruleId) === rule && m.severity === 2),
      `${rule} did not fire: ${JSON.stringify(messages)}`,
    );
  }
});
