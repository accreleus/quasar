/**
 * Where the quick start's prep step gets `deploy/prepare-host.sh` and its
 * checksum, computed once at build time from the file this repo ships — the
 * one place both the URL and the checksum come from, so the prep block never
 * drifts from the file it downloads.
 *
 * The quick start fetches the file from the docs site (this is chunk 4's job:
 * actually publishing it there), because the first machine in an install has
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
import { existsSync, readFileSync } from 'node:fs';
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

/** Served from the docs site's own origin (chunk 4 wires up the actual copy). */
export const PREPARE_HOST_URL = 'https://accreleus.github.io/quasar/prepare-host.sh';
