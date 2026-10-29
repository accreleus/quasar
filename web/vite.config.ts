import fs from "node:fs";
import path from "node:path";
import { defineConfig, type Plugin } from "vite";
import react from "@vitejs/plugin-react";

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

// The control plane serves deploy/enroll-host.sh from the SPA root as
// /enroll-host.sh (#100), so the script a second host fetches is the one from the
// tree the control plane was built from. Copied at build time, never duplicated in
// web/: deploy/ stays the single home of operator scripts. A static file, not an
// API route (protocol/control-api.md's note on /enroll-host.sh). Host preparation
// (deploy/prepare-host.sh) is deliberately not served: nobody is told to download
// and run a script as root; the docs write its steps out instead.
// Missing must fail the build — the SPA handler would otherwise answer the URL
// with index.html and `sh` would choke on HTML.
const OPERATOR_SCRIPTS = ["enroll-host.sh"] as const;
function operatorScripts(): Plugin {
  return {
    name: "quasar-operator-scripts",
    apply: "build",
    generateBundle() {
      for (const name of OPERATOR_SCRIPTS) {
        const src = path.resolve(__dirname, "..", "deploy", name);
        if (!fs.existsSync(src)) {
          throw new Error(`quasar-operator-scripts: ${src} not found — the build context must include deploy/${name}`);
        }
        this.emitFile({ type: "asset", fileName: name, source: fs.readFileSync(src) });
      }
    },
  };
}

// Code the account area and the admin console both use and the library page does
// not. Left to the default split it is a dozen tiny chunks every console page
// waits on; grouped it is one. The `$initial` group outranks it, so a module the
// library page imports stays with the entry even if it matches here.
const CONSOLE_SHARED = /\/src\/(components\/(Bar|PageHeader|LoadingState|ResourceStates|Table)\.tsx|components\/shell\/(ConsoleShell|Rail|Topbar|sectionHead)\.tsx|components\/shell\/(sectionTabs|paletteScope|settingsSearch\/place)\.ts|lib\/format\/(bytes|duration|relativeTime)\.ts|lib\/formatLegacy\.ts|lib\/region\/(options|zoneTable|zones)\.ts|input\/mouseScale\.ts|pages\/app\/account\/DeviceCard\.tsx)$/;

// What the lazy setup wizard shares with the admin pages: one chunk instead of
// several, reached by the /admin route preload through the admin layout's imports.
const ADMIN_SHARED = /\/src\/(api\/(admin|types)\.ts|components\/(SecretField|ReadinessCard|StatusChip)\.tsx|lib\/(hostStorage|hostCodecs)\.ts|pages\/admin\/library\/(imageStatus|imageRollout)\.tsx?)$/;

// A lazily split area opened by a deep link would wait for the entry to run and
// render the route before its chunks are even requested. For these path
// prefixes a small script asks for them up front, in parallel with the entry.
const ROUTE_PRELOADS: Record<string, string> = {
  "/app/account": "src/pages/app/account/area.ts",
  "/app/session": "src/pages/app/SessionPage.tsx",
  "/app/game": "src/pages/app/game/GamePage.tsx",
  "/admin": "src/pages/admin/AdminLayout.tsx",
  "/login": "src/pages/LoginPage.tsx",
};

// The script is emitted as an asset and loaded with <script src>, never inlined:
// the control plane's CSP allows 'self' plus two hashes for index.html's own
// anti-FOUC scripts, and this one names hashed chunks, so no fixed hash could pin it.
function routePreloads(): Plugin {
  let scriptFile = "";
  let map: Record<string, string[]> = {};
  return {
    name: "quasar-route-preloads",
    enforce: "post",
    apply: "build",
    generateBundle: {
      // Before vite:build-html writes index.html, which needs the file name.
      order: "pre",
      handler(_options, bundle) {
        const chunks = Object.values(bundle).filter((c) => c.type === "chunk");
        const entry = chunks.find((c) => c.isEntry);
        const inEntry = new Set<string>();
        const walk = (file: string, into: Set<string>) => {
          if (into.has(file)) return;
          into.add(file);
          const c = bundle[file];
          if (c?.type === "chunk") for (const i of c.imports) walk(i, into);
        };
        if (entry) walk(entry.fileName, inEntry);
        map = {};
        for (const [prefix, src] of Object.entries(ROUTE_PRELOADS)) {
          const chunk = chunks.find((c) => c.facadeModuleId?.endsWith("/" + src));
          if (!chunk) throw new Error(`quasar-route-preloads: no chunk for ${src}; update ROUTE_PRELOADS in vite.config.ts`);
          const files = new Set<string>();
          walk(chunk.fileName, files);
          const lazy = [...files].filter((f) => !inEntry.has(f));
          const css = lazy.flatMap((f) => {
            const c = bundle[f];
            return c?.type === "chunk" ? [...(c.viteMetadata?.importedCss ?? [])] : [];
          });
          // A chunk that only carries a stylesheet is deleted later in generateBundle
          // (its CSS stays in `css`), so asking for its .js would 404.
          const js = lazy.filter((f) => {
            const c = bundle[f];
            return c?.type !== "chunk" || (c.code.trim() !== "" && !c.moduleIds.every((id) => /\.css($|\?)/.test(id)));
          });
          map[prefix] = [...js, ...css].map((f) => "/" + f);
        }
        const source =
          "(function(){var p=location.pathname,m=" +
          JSON.stringify(map) +
          ";for(var k in m)if(p===k||p.indexOf(k+'/')===0)m[k].forEach(function(f){var l=document.createElement('link');" +
          "if(/\\.css$/.test(f)){l.rel='preload';l.as='style'}else{l.rel='modulepreload'}" +
          "l.href=f;l.crossOrigin='';document.head.appendChild(l)})})();";
        scriptFile = this.getFileName(this.emitFile({ type: "asset", name: "route-preloads.js", source }));
      },
    },
    transformIndexHtml() {
      if (!scriptFile) throw new Error("quasar-route-preloads: the script was not emitted before index.html");
      return [{ tag: "script", attrs: { src: "/" + scriptFile }, injectTo: "head-prepend" as const }];
    },
    // generateBundle above runs before Vite drops CSS-only chunks; by now the bundle
    // is final, so every file the script will request must be in it.
    writeBundle(_options, bundle) {
      const missing = Object.entries(map).flatMap(([prefix, files]) =>
        files.filter((f) => !(f.slice(1) in bundle)).map((f) => `${prefix}: ${f}`),
      );
      if (missing.length) throw new Error(`quasar-route-preloads: the preload script names files the build does not emit (each would 404): ${missing.join(", ")}`);
    },
  };
}

const INLINE_SCRIPT = /<script(?![^>]*\bsrc=)[^>]*>([\s\S]*?)<\/script>/g;
const inlineScripts = (html: string) => [...html.matchAll(INLINE_SCRIPT)].map((m) => m[1]);

// The CSP pins index.html's own inline scripts by hash (httpx/security.go, and
// TestCSPAllowsWebIndexInlineScripts checks the source file against it). A build
// step that adds another inline script, or edits one, would be blocked in
// production and only show up there. So the built page may hold exactly the
// inline scripts the source page does. It only sees what the transforms before it
// produced: keep it LAST in `plugins`.
function pinnedInlineScripts(): Plugin {
  let sourceScripts: string[] = [];
  return {
    name: "quasar-pinned-inline-scripts",
    enforce: "post",
    apply: "build",
    configResolved(config) {
      sourceScripts = inlineScripts(fs.readFileSync(path.join(config.root, "index.html"), "utf8"));
    },
    transformIndexHtml: {
      order: "post",
      handler(html) {
        const stray = inlineScripts(html).filter((body) => !sourceScripts.includes(body));
        if (stray.length) {
          throw new Error(
            `quasar-pinned-inline-scripts: the built index.html has an inline <script> that web/index.html does not (${stray[0].slice(0, 60).trim()}...). ` +
              "The control plane's CSP only allows the two hashed ones; emit the code as an asset and load it with <script src>.",
          );
        }
      },
    },
  };
}

export default defineConfig({
  plugins: [react(), preloadFonts(), operatorScripts(), routePreloads(), pinnedInlineScripts()],
  build: {
    rolldownOptions: {
      output: {
        codeSplitting: {
          groups: [
            { name: "web", tags: ["$initial"], priority: 2 },
            { name: "console", test: CONSOLE_SHARED, priority: 1 },
            { name: "admin-shared", test: ADMIN_SHARED, priority: 1 },
          ],
        },
      },
    },
  },
  server: {
    proxy: {
      "/v1": { target: controlOrigin, changeOrigin: true, ws: true },
      "/health": { target: controlOrigin, changeOrigin: true },
    },
  },
});
