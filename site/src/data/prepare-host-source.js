/**
 * Where the quick start's prep step gets `deploy/prepare-host.sh` and its
 * checksum, computed once at build time from the file this repo ships — the
 * one place both the URL and the checksum come from, so the prep block never
 * drifts from the file it downloads.
 *
 * The quick start fetches the file from the docs site (`prepareHostIntegration`
 * below publishes it there), because the first machine in an install has
 * no control plane yet to fetch it from. Enrollment of later hosts fetches
 * the control plane's own copy instead (`/prepare-host.sh`, served next to
 * `/enroll-host.sh` the same way — protocol/control-api.md's note on
 * `/enroll-host.sh` covers both: a static file, not an API route).
 */
/**
 * NODE-ONLY. This file touches `node:fs`, so it must never be imported by
 * `stack-template.js` (which the quick start's browser `<script>` also
 * bundles): Vite externalizes `node:fs` to a stub that throws on property
 * access for the CLIENT target, and a top-level `readFileSync` call used to
 * throw there before any event listener attached — the whole wizard was
 * inert in a real browser, silently. Safe callers are plain `node --test`
 * (this file, unbundled) and Astro's SSR frontmatter (Vite-bundled, but
 * still Node underneath — real fs works there). `QuickStart.astro`'s
 * frontmatter reads this and hands the checksum to the client via a
 * `data-*` attribute; `stack-template.js#setPrepareHostSha256` is how the
 * client (and this file's own test) supplies it to `generate()`.
 */
import { existsSync, readFileSync, writeFileSync } from 'node:fs';
import { createHash } from 'node:crypto';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

// import.meta.url is this SOURCE file's location under `node --test` (cwd
// site/, no bundling). Both `npm test` and `npm run build` run with cwd
// site/, so prefer a path relative to that (stable across any bundling)
// and fall back to the import.meta.url-derived one for any other invocation.
const here = dirname(fileURLToPath(import.meta.url));
const CANDIDATES = [join(process.cwd(), '../deploy/prepare-host.sh'), join(here, '../../../deploy/prepare-host.sh')];
const PREPARE_HOST_PATH = CANDIDATES.find(existsSync) ?? CANDIDATES[0];

export const PREPARE_HOST_SOURCE = readFileSync(PREPARE_HOST_PATH);
export const PREPARE_HOST_SHA256 = createHash('sha256').update(PREPARE_HOST_SOURCE).digest('hex');

/** The file name the site publishes it under, at the root of its base path. */
export const PREPARE_HOST_ASSET = 'prepare-host.sh';

/**
 * Served from the docs site's own origin: `prepareHostIntegration()` below writes the
 * file into the built site, so this URL answers with the very bytes whose checksum the
 * quick start prints. `stack-template.js` repeats the URL (it cannot import this file);
 * a test holds the two equal.
 */
export const PREPARE_HOST_URL = `https://accreleus.github.io/quasar/${PREPARE_HOST_ASSET}`;

/**
 * Writes the script into a built site's output directory and checks what landed
 * there against the checksum the quick start shows. Returns the path written.
 */
export function writePrepareHost(outDir) {
  const dest = join(outDir, PREPARE_HOST_ASSET);
  writeFileSync(dest, PREPARE_HOST_SOURCE);
  const written = createHash('sha256').update(readFileSync(dest)).digest('hex');
  if (written !== PREPARE_HOST_SHA256) {
    throw new Error(`${dest}: sha256 ${written}, but the quick start shows ${PREPARE_HOST_SHA256}`);
  }
  return dest;
}

/**
 * The Astro integration that publishes `deploy/prepare-host.sh` with the site: into
 * the build output (served at `PREPARE_HOST_URL` under the site's base path), and
 * from the dev server at the same path. The bytes are `PREPARE_HOST_SOURCE`, the
 * same the checksum was computed from, never a copy under `site/`.
 */
export function prepareHostIntegration() {
  return {
    name: 'quasar-prepare-host',
    hooks: {
      'astro:server:setup': ({ server }) => {
        server.middlewares.use((req, res, next) => {
          if ((req.url ?? '').split('?')[0].endsWith(`/${PREPARE_HOST_ASSET}`)) {
            res.setHeader('Content-Type', 'text/x-shellscript; charset=utf-8');
            res.end(PREPARE_HOST_SOURCE);
            return;
          }
          next();
        });
      },
      'astro:build:done': ({ dir, logger }) => {
        const dest = writePrepareHost(fileURLToPath(dir));
        logger.info(`${PREPARE_HOST_ASSET} (sha256 ${PREPARE_HOST_SHA256}) -> ${dest}`);
      },
    },
  };
}
