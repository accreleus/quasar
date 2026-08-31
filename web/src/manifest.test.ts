// #435 — installable web app, manifest only.
//
// These assertions exist because every one of them is a silent failure in a
// browser: a manifest that does not parse is ignored with no error the user
// sees, an icon 404 shows an empty home-screen tile, a too-narrow scope opens
// Safari mid-session, and a service worker would quietly reintroduce the #131
// stale-bundle class.
import { describe, expect, it } from "vitest";
import { readFileSync, existsSync } from "node:fs";
import { resolve } from "node:path";

const webRoot = resolve(__dirname, "..");
const read = (p: string) => readFileSync(resolve(webRoot, p), "utf8");

const manifest = JSON.parse(read("public/manifest.webmanifest")) as {
  id: string;
  name: string;
  short_name: string;
  start_url: string;
  scope: string;
  display: string;
  theme_color: string;
  background_color: string;
  icons: { src: string; sizes: string; type: string; purpose?: string }[];
};

/** The literal value of a custom property in tokens.css, e.g. "--ink-1". */
function token(name: string): string {
  const css = read("src/styles/tokens.css");
  const m = new RegExp(`${name}:\\s*([^;]+);`).exec(css);
  if (!m) throw new Error(`token ${name} not found in tokens.css`);
  return m[1].trim();
}

describe("manifest.webmanifest", () => {
  it("declares the fields that make it installable", () => {
    expect(manifest.name).toBe("Quasar");
    expect(manifest.short_name).toBe("Quasar");
    expect(manifest.display).toBe("standalone");
    expect(manifest.start_url).toBe("/app");
    expect(manifest.id).toBe("/app");
  });

  // Scope must cover the WHOLE SPA, not just /app: an unauthenticated launch
  // lands on /login, and an out-of-scope navigation is thrown to the system
  // browser — i.e. the installed app would eject the user on first launch.
  it("scopes the whole SPA, so /login and /admin stay in the app", () => {
    expect(manifest.scope).toBe("/");
    for (const route of ["/app", "/login", "/register", "/admin"]) {
      expect(route.startsWith(manifest.scope)).toBe(true);
    }
  });

  // The v3 canvas is authored in oklch, which a webmanifest cannot carry
  // portably, so the manifest ships the sRGB equivalent and this test pins the
  // pair: change --surf-canvas and this fails until the hex is recomputed.
  it("takes theme and background colour from tokens.css --surf-canvas", () => {
    expect(token("--surf-canvas")).toBe("oklch(0.115 0.012 267)");
    const canvasHex = "#040509"; // sRGB of oklch(0.115 0.012 267)
    expect(manifest.theme_color).toBe(canvasHex);
    expect(manifest.background_color).toBe(canvasHex);
  });

  it("ships every icon it advertises", () => {
    expect(manifest.icons.length).toBeGreaterThan(0);
    for (const icon of manifest.icons) {
      expect(icon.src.startsWith("/")).toBe(true);
      const onDisk = resolve(webRoot, "public", icon.src.replace(/^\//, ""));
      expect(existsSync(onDisk), `${icon.src} is declared but missing`).toBe(true);
    }
  });

  it("covers the sizes iOS and Android actually use", () => {
    const sizes = manifest.icons.map((i) => i.sizes);
    expect(sizes).toContain("192x192"); // Android launcher / install prompt
    expect(sizes).toContain("512x512"); // Android splash + store surfaces
    const maskable = manifest.icons.filter((i) => i.purpose === "maskable");
    expect(maskable).toHaveLength(1); // Android adaptive-icon crop
    expect(maskable[0].sizes).toBe("512x512");
  });
});

describe("index.html", () => {
  const html = read("index.html");

  it("links the manifest", () => {
    expect(html).toMatch(/<link[^>]+rel="manifest"[^>]+href="\/manifest\.webmanifest"/);
  });

  // iOS does not read display/icons from the manifest for Add to Home Screen
  // on every version — these tags are still what gives standalone + an icon.
  it("carries the apple-mobile-web-app tags iOS still needs", () => {
    expect(html).toMatch(/<meta name="apple-mobile-web-app-capable" content="yes"/);
    expect(html).toMatch(/<meta name="apple-mobile-web-app-status-bar-style"/);
    expect(html).toMatch(/<meta name="apple-mobile-web-app-title" content="Quasar"/);
    expect(html).toMatch(/<link rel="apple-touch-icon" href="\/icons\/apple-touch-icon-180\.png"/);
    expect(existsSync(resolve(webRoot, "public/icons/apple-touch-icon-180.png"))).toBe(true);
  });

  it("sets theme-color to the same token as the manifest", () => {
    const m = /<meta name="theme-color" content="([^"]+)"/.exec(html);
    expect(m?.[1]).toBe(manifest.theme_color);
  });
});

// The load-bearing negative. #435 scopes a service worker OUT on purpose: the
// SPA handler's no-cache index + immutable assets + real 404 is what closed
// #131, and a SW puts a cache in front of it in a harder-to-diagnose form.
describe("no service worker", () => {
  it("registers nothing and depends on no SW tooling", () => {
    const pkg = read("package.json");
    expect(pkg).not.toMatch(/workbox|vite-plugin-pwa|serviceworker/i);
    expect(read("index.html")).not.toMatch(/serviceWorker/i);
    expect(read("vite.config.ts")).not.toMatch(/workbox|vite-plugin-pwa|manifest.*pwa/i);
  });
});
