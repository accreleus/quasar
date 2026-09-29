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
import { readFileSync } from 'node:fs';
import { createHash } from 'node:crypto';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const PREPARE_HOST_PATH = join(here, '../../../deploy/prepare-host.sh');

export const PREPARE_HOST_SOURCE = readFileSync(PREPARE_HOST_PATH);
export const PREPARE_HOST_SHA256 = createHash('sha256').update(PREPARE_HOST_SOURCE).digest('hex');

/** Served from the docs site's own origin (chunk 4 wires up the actual copy). */
export const PREPARE_HOST_URL = 'https://accreleus.github.io/quasar/prepare-host.sh';
