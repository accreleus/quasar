/**
 * Batch B (#373) moved every raw colour out of the stylesheets and components
 * into tokens.css. This pins that no rendered colour changed.
 *
 * `MOVED` holds each token with the literal it replaced, as it stood on
 * develop before the move (the test cannot read git history, so the literals
 * are embedded). Each token must be declared in tokens.css `:root` with that
 * exact text, or, for a hex source (tokens.css is hex-free), with the keyword
 * or rgb() spelling of the same sRGB colour; it must not re-point in the light
 * block; and every file listed must read it. `REUSED` covers sites that took
 * an existing token whose value, in the theme the rule renders under, is the
 * replaced literal.
 */
import { describe, expect, it } from "vitest";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { cssDeclarations } from "./design-lint";

const SRC = resolve(__dirname, "..");
const read = (path: string) => readFileSync(resolve(SRC, path), "utf8");
const tokensCss = read("styles/tokens.css");

/** Body of the first block whose head matches `head` (braces matched). */
function block(sheet: string, head: RegExp): string {
  const m = head.exec(sheet);
  if (!m) throw new Error(`no block ${head}`);
  const open = sheet.indexOf("{", m.index);
  let depth = 0;
  for (let i = open; i < sheet.length; i++) {
    if (sheet[i] === "{") depth++;
    else if (sheet[i] === "}" && --depth === 0) return sheet.slice(open + 1, i);
  }
  throw new Error(`unterminated block ${head}`);
}

const declared = (body: string) => new Map(cssDeclarations(body).map((d) => [d.prop, d.value]));
const root = declared(block(tokensCss, /^:root\s*\{/m));
const light = declared(block(tokensCss, /^\[data-theme="light"\]\s*\{/m));

/** Hex literal → the spelling tokens.css uses, and the sRGB bytes both name. */
const HEX_SPELLING: Record<string, [string, [number, number, number]]> = {
  "#fff": ["white", [255, 255, 255]],
  "#000": ["black", [0, 0, 0]],
  "#08080c": ["rgb(8 8 12)", [8, 8, 12]],
};

const hexBytes = (hex: string): number[] => {
  const h = hex.slice(1);
  const full = h.length === 3 ? [...h].map((c) => c + c).join("") : h;
  return [0, 2, 4].map((i) => parseInt(full.slice(i, i + 2), 16));
};

// [token, develop-era literal, files (under src/) that read it]
const MOVED: [string, string, string[]][] = [
  ["--loader-glow-corner", "oklch(0.5998 0.2179 273.21 / 7%)", ["pages/app/SessionLoader.css"]],
  ["--scene-mid", "oklch(0.13 0.012 267)", ["pages/app/SessionLoader.css", "styles/login.css"]],
  ["--loader-hot", "#fff", ["pages/app/SessionLoader.css"]],
  ["--loader-glow-1", "oklch(0.78 0.16 273)", ["pages/app/SessionLoader.css"]],
  ["--loader-jet-tail", "oklch(0.5998 0.2179 273.21 / 60%)", ["pages/app/SessionLoader.css"]],
  ["--loader-orbit", "oklch(0.5998 0.2179 273.21 / 34%)", ["pages/app/SessionLoader.css"]],
  ["--loader-orbit-b", "oklch(0.72 0.12 273 / 16%)", ["pages/app/SessionLoader.css"]],
  ["--loader-glow-2", "oklch(0.82 0.14 273)", ["pages/app/SessionLoader.css"]],
  ["--loader-disc-edge", "oklch(0.42 0.19 273)", ["pages/app/SessionLoader.css"]],
  ["--loader-disc-rim", "oklch(1 0 0 / 70%)", ["pages/app/SessionLoader.css"]],
  ["--loader-disc-shade", "oklch(0.78 0.14 273 / 30%)", ["pages/app/SessionLoader.css"]],
  ["--loader-halo", "oklch(0.5998 0.2179 273.21 / 30%)", ["pages/app/SessionLoader.css"]],
  ["--loader-done-line", "oklch(0.76 0.15 164 / 45%)", ["pages/app/SessionLoader.css"]],
  ["--loader-done-bg", "oklch(0.76 0.15 164 / 8%)", ["pages/app/SessionLoader.css"]],
  ["--loader-done-link", "oklch(0.76 0.15 164 / 50%)", ["pages/app/SessionLoader.css"]],
  ["--video-letterbox", "#000", ["styles.css"]],
  ["--avatar-ink", "#fff", ["styles/admin.css"]],
  ["--audit-readout-bg", "#000", ["styles/admin/audit.css"]],
  ["--audit-detail-rule", "oklch(0.9618 0.0086 247.91/.1)", ["styles/admin/audit.css"]],
  ["--audit-detail-shadow", "oklch(0 0 0/.95)", ["styles/admin/audit.css"]],
  ["--fg-strong", "oklch(0.985 0.004 247)", ["styles/admin/audit.css", "styles/login.css"]],
  ["--audit-btn-ink", "oklch(0.9 0.008 250)", ["styles/admin/audit.css"]],
  ["--audit-btn-hover-bg", "oklch(1 0 0/.1)", ["styles/admin/audit.css"]],
  ["--audit-btn-hover-ink", "oklch(1 0 0)", ["styles/admin/audit.css"]],
  ["--audit-btn-hover-line", "oklch(1 0 0/.18)", ["styles/admin/audit.css"]],
  ["--cover-violet-a", "oklch(0.5443 0.2453 284.39)", ["styles/admin/editor.css", "styles/primitives.css"]],
  ["--cover-violet-b", "oklch(0.3727 0.1878 282.05)", ["styles/admin/editor.css", "styles/primitives.css"]],
  ["--editor-frame-ink", "oklch(1 0 0 / 0.82)", ["styles/admin/editor.css"]],
  ["--editor-cand-art", "oklch(0.4600 0.1600 285)", ["styles/admin/editor.css"]],
  ["--editor-cand-ink", "oklch(1 0 0 / 0.85)", ["styles/admin/editor.css"]],
  ["--canvas-glow-accent", "oklch(0.5998 0.2179 273.21/.11)", ["styles/base.css"]],
  ["--canvas-glow-action", "oklch(0.5 0.19 273.21/.07)", ["styles/base.css"]],
  ["--canvas-glow-accent-light", "oklch(0.5998 0.2179 273.21/.09)", ["styles/base.css"]],
  ["--canvas-glow-action-light", "oklch(0.5 0.19 273.21/.05)", ["styles/base.css"]],
  ["--ovprev-stage-floor", "oklch(0 0 0)", ["styles/components.css"]],
  ["--failure-log-bg", "rgb(0 0 0 / 22%)", ["styles/components.css"]],
  ["--home-scrim", "oklch(0.12 0.01 285 / 0.35)", ["styles/home.css"]],
  ["--home-keyline", "oklch(0.1365 0.009 285 / 0.92)", ["styles/home.css"]],
  ["--home-on-accent", "oklch(1 0 0)", ["styles/home.css"]],
  ["--home-fnm", "oklch(1 0 0 / 0.85)", ["styles/home.css"]],
  ["--home-sweep-1", "oklch(0.5443 0.2453 284.39 / 0.1)", ["styles/home.css"]],
  ["--home-sweep-2", "oklch(0.7577 0.1529 231.09 / 0.06)", ["styles/home.css"]],
  ["--home-play-glow", "oklch(0.5998 0.2179 273.21 / 0.8)", ["styles/home.css"]],
  ["--d-ink", "oklch(0.97 0.006 268)", ["styles/home.css"]],
  ["--d-ink-strong", "oklch(0.99 0.004 268)", ["styles/home.css"]],
  ["--d-ink-2", "oklch(0.86 0.012 268)", ["styles/home.css"]],
  ["--d-ink-3", "oklch(0.74 0.02 268)", ["styles/home.css"]],
  ["--d-glass", "oklch(1 0 0 / 0.1)", ["styles/home.css"]],
  ["--d-glass-hover", "oklch(1 0 0 / 0.17)", ["styles/home.css"]],
  ["--d-glass-line", "oklch(1 0 0 / 0.18)", ["styles/home.css"]],
  ["--d-glass-line-hover", "oklch(1 0 0 / 0.3)", ["styles/home.css"]],
  ["--detail-glyph", "oklch(1 0 0 / 0.22)", ["styles/home.css"]],
  ["--detail-well", "oklch(0.16 0.012 285 / 0.55)", ["styles/home.css"]],
  ["--detail-primary-border", "oklch(0.72 0.15 273.21 / 0.68)", ["styles/home.css"]],
  ["--detail-scrim-rgb-fallback", "17, 17, 26", ["styles/home.css"]],
  ["--hud-glass", "oklch(0.13 0.012 267 / 0.74)", ["styles/hud.css"]],
  ["--hud-glass-open", "oklch(0.13 0.012 267 / 0.86)", ["styles/hud.css"]],
  ["--hud-cover-glyph", "oklch(1 0 0 / 0.9)", ["styles/hud.css"]],
  ["--hud-cover-vignette", "oklch(0.09 0.01 267 / 0.45)", ["styles/hud.css"]],
  ["--hud-toast-bg", "oklch(0.13 0.012 267 / 0.8)", ["styles/hud.css"]],
  ["--hud-banner-bg", "oklch(0.13 0.012 267 / 0.9)", ["styles/hud.css"]],
  ["--session-summon-bg", "oklch(0.13 0.012 267 / 0.72)", ["styles/session.css"]],
  ["--login-glow", "oklch(0.5998 0.2179 273.21/.07)", ["styles/login.css"]],
  ["--login-card-border", "oklch(0.9618 0.0086 247.91/.16)", ["styles/login.css"]],
  ["--login-card-bg", "oklch(0.224 0.018 263/.34)", ["styles/login.css"]],
  ["--login-input-bg", "oklch(0.105 0.01 267/.56)", ["styles/login.css"]],
  ["--login-placeholder", "oklch(0.6605 0.0265 259.81/.7)", ["styles/login.css"]],
  ["--login-spin-track", "oklch(1 0 0/.35)", ["styles/login.css"]],
  ["--on-accent", "oklch(1 0 0)", ["styles/login.css", "styles/primitives.css", "styles/shell.css"]],
  ["--status-glyph-ink", "#08080c", ["components/Toast.tsx", "components/icons.tsx"]],
  ["--btn-shadow-near", "oklch(0.05 0.01 267/.38)", ["styles/primitives.css"]],
  ["--btn-shadow-far", "oklch(0.05 0.01 267/.8)", ["styles/primitives.css"]],
  ["--btn-press-shadow", "oklch(0.05 0.01 267/.55)", ["styles/primitives.css"]],
  ["--btn-primary-border", "oklch(0.72 0.15 273.21/.68)", ["styles/primitives.css"]],
  ["--btn-primary-text-shadow", "oklch(0.05 0.01 267/.5)", ["styles/primitives.css"]],
  ["--btn-primary-highlight", "oklch(0.9618 0.0086 247.91/.2)", ["styles/primitives.css"]],
  ["--btn-primary-lowlight", "oklch(0.22 0.1 273.21/.72)", ["styles/primitives.css"]],
  ["--control-shadow", "oklch(0.05 0.01 267/.45)", ["styles/primitives.css"]],
  ["--btn-primary-glow", "oklch(0.46 0.19 273.21/.8)", ["styles/primitives.css"]],
  ["--btn-primary-border-hover", "oklch(0.78 0.13 273.21/.76)", ["styles/primitives.css"]],
  ["--btn-danger-border", "oklch(0.69 0.19 27/.55)", ["styles/primitives.css"]],
  ["--line-edge", "oklch(0.9618 0.0086 247.91/.13)", ["styles/primitives.css"]],
  ["--card-shadow-near", "oklch(0.02 0.01 267/.45)", ["styles/primitives.css"]],
  ["--card-shadow-far", "oklch(0.02 0.01 267/.85)", ["styles/primitives.css"]],
  ["--mono-tile-ink", "oklch(1 0 0/.85)", ["styles/primitives.css"]],
  ["--line-faint", "oklch(0.9618 0.0086 247.91/.07)", ["styles/primitives.css", "styles/shell.css"]],
  ["--row-hover", "oklch(0.9618 0.0086 247.91/.045)", ["styles/primitives.css"]],
  ["--table-expand-seam", "oklch(0.02 0.01 267/.5)", ["styles/primitives.css"]],
  ["--status-pulse-fade", "oklch(0.76 0.15 164/0)", ["styles/primitives.css"]],
  ["--pop-shadow", "oklch(0.05 0.01 267/.72)", ["styles/primitives.css", "styles/shell.css"]],
  ["--modal-scrim", "oklch(0.05 0.01 267/.6)", ["styles/primitives.css"]],
  ["--overlay-shadow", "oklch(0.05 0.01 267/.76)", ["styles/primitives.css"]],
  ["--drawer-scrim", "oklch(0.05 0.01 267/.52)", ["styles/primitives.css"]],
  ["--light-btn-bg", "oklch(1 0 0/.9)", ["styles/primitives.css"]],
  ["--light-btn-drop", "oklch(0.24 0.022 268/.1)", ["styles/primitives.css"]],
  ["--light-lift", "oklch(1 0 0)", ["styles/primitives.css", "styles/shell.css"]],
  ["--light-btn-primary-border", "oklch(0.42 0.19 273.21)", ["styles/primitives.css"]],
  ["--light-btn-primary-highlight", "oklch(1 0 0/.24)", ["styles/primitives.css"]],
  ["--light-btn-primary-drop", "oklch(0.24 0.022 268/.18)", ["styles/primitives.css"]],
  ["--light-btn-primary-glow", "oklch(0.5 0.19 273.21/.7)", ["styles/primitives.css"]],
  ["--light-btn-primary-border-hover", "oklch(0.38 0.19 273.21)", ["styles/primitives.css"]],
  ["--light-hover-wash", "oklch(0.24 0.022 268/.05)", ["styles/primitives.css", "styles/shell.css"]],
  ["--light-hover-fill", "oklch(0.24 0.022 268/.06)", ["styles/primitives.css", "styles/shell.css"]],
  ["--light-row-hover", "oklch(0.24 0.022 268/.035)", ["styles/primitives.css"]],
  ["--light-table-expand-seam", "oklch(0.24 0.022 268/.07)", ["styles/primitives.css"]],
  ["--light-field-bg", "oklch(1 0 0/.86)", ["styles/primitives.css", "styles/shell.css"]],
  ["--light-switch-thumb-shadow", "oklch(0.24 0.022 268/.3)", ["styles/primitives.css"]],
  ["--light-pop-bg", "oklch(0.998 0.002 268/.92)", ["styles/primitives.css", "styles/shell.css"]],
  ["--light-overlay-bg", "oklch(0.998 0.002 268/.95)", ["styles/primitives.css"]],
  ["--light-scrim", "oklch(0.24 0.022 268/.28)", ["styles/primitives.css", "styles/shell.css"]],
  ["--light-chip-bg", "oklch(1 0 0/.8)", ["styles/primitives.css"]],
  ["--light-bar-track", "oklch(0.24 0.022 268/.1)", ["styles/primitives.css"]],
  ["--light-chrome-highlight", "oklch(1 0 0/.6)", ["styles/shell.css"]],
  ["--cover-glyph", "oklch(1 0 0 / 0.92)", ["styles/primitives.css"]],
  ["--cover-cyan-a", "oklch(0.7577 0.1529 231.09)", ["styles/primitives.css"]],
  ["--cover-cyan-b", "oklch(0.4526 0.1106 245.69)", ["styles/primitives.css"]],
  ["--cover-horizon-b", "oklch(0.3657 0.1512 273.75)", ["styles/primitives.css"]],
  ["--cover-nebula-a", "oklch(0.6054 0.2201 292.20)", ["styles/primitives.css"]],
  ["--cover-nebula-b", "oklch(0.7079 0.1770 298.27)", ["styles/primitives.css"]],
  ["--cover-plasma-a", "oklch(0.6443 0.1926 256.34)", ["styles/primitives.css"]],
  ["--cover-plasma-b", "oklch(0.3612 0.1181 259.70)", ["styles/primitives.css"]],
  ["--cover-rose-a", "oklch(0.6861 0.2061 14.99)", ["styles/primitives.css"]],
  ["--cover-rose-b", "oklch(0.4033 0.1197 4.83)", ["styles/primitives.css"]],
  ["--topbar-shadow", "oklch(0.02 0.01 267/.55)", ["styles/shell.css"]],
  ["--nav-hover", "oklch(0.9618 0.0086 247.91/.05)", ["styles/shell.css"]],
  ["--rail-scrim", "oklch(0.02 0.01 267/.55)", ["styles/shell.css"]],
];

// [file, rule head, token, develop-era literal, theme the rule renders under]
const REUSED: [string, string, string, string, "dark" | "light"][] = [
  // `[data-theme="light"] .btn` replaces this box-shadow, so only the :root
  // value ever renders here.
  ["styles/primitives.css", ".btn {", "--line-2", "oklch(0.9618 0.0086 247.91/.12)", "dark"],
  ["styles/primitives.css", '[data-theme="light"] .btn:active:not(:disabled) {', "--line-2", "oklch(0.24 0.022 268/.14)", "light"],
  ["styles/primitives.css", '[data-theme="light"] .segmented button[aria-selected="true"] {', "--glass-border", "oklch(0.24 0.022 268/.12)", "light"],
  ["styles/primitives.css", '[data-theme="light"] .switch {', "--glass-border", "oklch(0.24 0.022 268/.12)", "light"],
  ["styles/primitives.css", ".cv-horizon {", "--accent", "oklch(0.5998 0.2179 273.21)", "dark"],
  // The pre-auth pages are dark-locked (useDarkLock).
  ["styles/login.css", ".auth-scene .error {", "--danger-text", "oklch(0.82 0.12 27)", "dark"],
];

// [file, rule head, text the rule must now contain]
const REPRESENTATIVE: [string, string, string][] = [
  ["styles/login.css", ".auth-scene .card {", "border: 1px solid var(--login-card-border)"],
  ["styles/hud.css", ".hud {", "background: var(--hud-glass)"],
  ["styles/hud.css", ".banner {", "background: var(--hud-banner-bg)"],
  ["styles/primitives.css", ".btn-primary {", "border-color: var(--btn-primary-border)"],
  ["styles/primitives.css", '[data-theme="light"] .chip {', "background: var(--light-chip-bg)"],
  ["styles/primitives.css", ".card {", "border: 1px solid var(--line-edge)"],
  ["styles/home.css", ".lib-tile-play {", "0 8px 28px -6px var(--home-play-glow)"],
  ["pages/app/SessionLoader.css", ".sl-quasar .core {", "background: var(--loader-hot)"],
  ["pages/app/SessionLoader.css", ".sl-root {", "var(--loader-glow-corner)"],
  ["styles.css", ".session-video {", "background: var(--video-letterbox)"],
];

const ruleBody = (file: string, head: string): string => {
  const esc = head.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  return block(read(file), new RegExp(`^${esc}`, "m"));
};

describe("colour tokens hold the literals they replaced", () => {
  it.each(MOVED)("%s = %s", (token, literal, files) => {
    const value = root.get(token);
    const hex = HEX_SPELLING[literal];
    if (hex) {
      expect(hexBytes(literal)).toEqual(hex[1]);
      expect(value).toBe(hex[0]);
    } else {
      expect(value).toBe(literal);
    }
    expect(light.has(token), `${token} must not re-point in the light block`).toBe(false);
    for (const file of files) expect(read(file), file).toContain(`var(${token})`);
  });

  it("spells #08080c as the rgb() of the same bytes", () => {
    expect(HEX_SPELLING["#08080c"][0]).toBe(`rgb(${hexBytes("#08080c").join(" ")})`);
  });

  it.each(REUSED)("%s %s reads %s", (file, head, token, literal, theme) => {
    const value = theme === "light" ? (light.get(token) ?? root.get(token)) : root.get(token);
    expect(value).toBe(literal);
    expect(ruleBody(file, head)).toContain(`var(${token})`);
  });

  it.each(REPRESENTATIVE)("%s %s uses the token", (file, head, text) => {
    expect(ruleBody(file, head).replace(/\s+/g, " ")).toContain(text);
  });

  it("keeps the detail scrim's rgb runtime-sampled, with the old fallback", () => {
    const home = read("styles/home.css");
    expect(home.match(/rgba\(var\(--scrim-rgb, var\(--detail-scrim-rgb-fallback\)\), /g)).toHaveLength(5);
    expect(home).not.toMatch(/--scrim-rgb,\s*17/);
  });
});
