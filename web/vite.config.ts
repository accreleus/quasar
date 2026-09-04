import { execSync } from "node:child_process";
import { defineConfig, type Plugin } from "vite";
import react from "@vitejs/plugin-react";

// The git ref this build is made from, baked in as __QUASAR_SOURCE_REF__ (see
// src/lib/buildInfo.ts). QUASAR_SOURCE_REF wins when set — container builds
// (deploy/redeploy.sh, Dockerfile.control.prod) pass it because they cannot see
// .git — else the exact tag if the tree sits on one, else the commit. Empty,
// not a guess, when neither is available: the enroll-host one-liner must never
// point at a tree other than the one running.
function sourceRef(): string {
  const fromEnv = process.env.QUASAR_SOURCE_REF?.trim();
  if (fromEnv) return fromEnv;
  const git = (args: string): string => {
    try {
      return execSync(`git ${args}`, { stdio: ["ignore", "pipe", "ignore"] }).toString().trim();
    } catch {
      return "";
    }
  };
  return git("describe --tags --exact-match") || git("rev-parse HEAD");
}

// Fonts the UI needs on EVERY page, in every role. @fontsource ships them via
// `@import` inside base.css, so the browser cannot discover them until the CSS
// has been fetched AND parsed — one extra round trip after the stylesheet, which
// on a high-RTT link (office wifi + VPN, #386) is a visible flash of fallback
// text. Preload hints in index.html let the browser start them alongside the CSS.
//
// Deliberately only these three: preloading a face the page does not need
// competes with the JS bundle for the same bandwidth. IBM Plex Mono is left out
// because it styles ids and metric values, which are usually below the fold, and
// the extra latin-ext / 500 / 700 faces are swapped in after first paint.
// Michroma is in despite being the wordmark only: it is one small face and it
// sits in the topbar of every route, so a fallback flash is always visible.
const PRELOAD_FONTS = [
  "ibm-plex-sans-latin-400-normal", // --font-ui, body text
  "ibm-plex-sans-latin-600-normal", // --font-ui/--font-display, headings + labels
  "michroma-latin-400-normal", // --font-brand, the wordmark
];

// preloadFonts injects <link rel="preload"> for the faces above. Filenames are
// content-hashed at build time, so the hints cannot be written by hand in
// index.html — they are resolved from the emitted bundle here.
//
// woff2 only: every browser that runs this SPA supports it, and preloading the
// legacy .woff fallback as well would download both.
function preloadFonts(): Plugin {
  return {
    name: "quasar-preload-fonts",
    enforce: "post",
    apply: "build",
    transformIndexHtml(html, ctx) {
      const files = Object.keys(ctx.bundle ?? {}).filter((f) => f.endsWith(".woff2"));
      const tags = PRELOAD_FONTS.flatMap((stem) => {
        const match = files.find((f) => f.includes(stem));
        if (!match) {
          // A renamed @fontsource asset must fail the build, not silently drop
          // the hint and leave a stale comment claiming fonts are preloaded.
          throw new Error(
            `quasar-preload-fonts: no emitted woff2 matches "${stem}". ` +
              `Update PRELOAD_FONTS in vite.config.ts to match the @fontsource imports in src/styles/base.css.`,
          );
        }
        return [
          {
            tag: "link",
            attrs: {
              rel: "preload",
              as: "font",
              type: "font/woff2",
              href: `/${match}`,
              crossorigin: "",
            },
            injectTo: "head-prepend" as const,
          },
        ];
      });
      return { html, tags };
    },
  };
}

// During local dev the SPA is served from Vite (default :5173) while the control
// plane listens on :8080. Production serves the built SPA behind the control
// plane's own origin, so the client always derives the API base from
// `location.origin` (see src/api/client.ts) — no base URL is baked in. The dev
// proxy below makes that same-origin assumption hold in development by
// forwarding /v1 (REST) and the signaling WebSocket to the control plane.
//
// Override the control-plane target with QUASAR_CONTROL_ORIGIN when it is not on
// localhost:8080.
const controlOrigin = process.env.QUASAR_CONTROL_ORIGIN ?? "http://localhost:8080";

export default defineConfig({
  plugins: [react(), preloadFonts()],
  define: { __QUASAR_SOURCE_REF__: JSON.stringify(sourceRef()) },
  server: {
    proxy: {
      "/v1": { target: controlOrigin, changeOrigin: true, ws: true },
      "/health": { target: controlOrigin, changeOrigin: true },
    },
  },
});
