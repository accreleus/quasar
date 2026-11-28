// The web lint gate, run by scripts/verify/lint.d/web-eslint.sh. Only rules that
// would have caught a real bug here. Findings that predate the gate are counted
// per file in eslint-suppressions.json, which may only shrink.
import reactHooks from "eslint-plugin-react-hooks";
import tseslint from "typescript-eslint";

const setIntervalCall = "CallExpression:matches([callee.name='setInterval'], [callee.property.name='setInterval'])";
const POLL =
  "setInterval does not wait for the last run, so slow reads stack and land out of order. " +
  "Use useResource (web/src/lib/resource/) or a chained setTimeout.";
const effectNoDeps = "CallExpression[callee.name=/^use(Layout)?Effect$/][arguments.length=1] > :function.arguments";
const NO_DEPS =
  "This effect has no dependency array and does this on every render. Give it a dependency array, " +
  "or guard the call with the condition it is needed under (as Hud's measure() is).";
const GPU =
  "A browser may never answer a WebGPU request. Bound it with boundedRequest() " +
  "(web/src/webtransport/presenter.ts).";
const gpuRequest = "CallExpression[callee.property.name=/^request(Adapter|Device)$/]";

export const houseBans = [
  // Polls: an async callback, or one that drops a promise with `void` (a3a3f7dc9, 55c1e8845).
  { selector: `${setIntervalCall} > :function.arguments[async=true]`, message: POLL },
  { selector: `${setIntervalCall} > :function.arguments UnaryExpression[operator='void']`, message: POLL },
  // Only a bare function call that always runs; `if (x) f()` and `ref.current = y` pass (a1045ee48).
  { selector: `${effectNoDeps} > BlockStatement > ExpressionStatement > CallExpression[callee.type='Identifier']`, message: NO_DEPS },
  { selector: `${effectNoDeps} > CallExpression.body[callee.type='Identifier']`, message: NO_DEPS },
  // b53af8cd7: an unanswered request left the stream black.
  { selector: `${gpuRequest}:not(CallExpression[callee.name='boundedRequest'] > CallExpression)`, message: GPU },
];

export default tseslint.config(
  { ignores: ["dist/", "coverage/"] },
  {
    files: ["**/*.{ts,tsx}"],
    languageOptions: {
      parser: tseslint.parser,
      parserOptions: { projectService: true, tsconfigRootDir: import.meta.dirname },
    },
    linterOptions: { reportUnusedDisableDirectives: "error" },
    plugins: { "@typescript-eslint": tseslint.plugin, "react-hooks": reactHooks },
    rules: {
      "react-hooks/rules-of-hooks": "error",
      "react-hooks/exhaustive-deps": "error",
      "@typescript-eslint/no-floating-promises": "error",
      "@typescript-eslint/no-misused-promises": "error",
      "@typescript-eslint/switch-exhaustiveness-check": "error",
      "no-restricted-syntax": ["error", ...houseBans],
    },
  },
  {
    // Tests stub these APIs and drive real adapters on purpose.
    files: ["**/*.test.{ts,tsx}"],
    rules: { "no-restricted-syntax": "off" },
  },
);
