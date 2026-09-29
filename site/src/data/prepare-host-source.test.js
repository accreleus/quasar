import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, readFileSync, rmSync } from 'node:fs';
import { createHash } from 'node:crypto';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import {
  PREPARE_HOST_ASSET,
  PREPARE_HOST_SHA256,
  PREPARE_HOST_SOURCE,
  PREPARE_HOST_URL,
  prepareHostIntegration,
  writePrepareHost,
} from './prepare-host-source.js';
import { PREPARE_HOST_URL as TEMPLATE_URL } from './stack-template.js';

const sha = (b) => createHash('sha256').update(b).digest('hex');

test('the published bytes are deploy/prepare-host.sh, and carry the checksum the quick start prints', () => {
  const deployed = readFileSync(new URL('../../../deploy/prepare-host.sh', import.meta.url));
  assert.deepEqual(PREPARE_HOST_SOURCE, deployed);
  assert.equal(PREPARE_HOST_SHA256, sha(deployed));
});

test('the build hook writes the script at the root of the output, byte for byte', async () => {
  const out = mkdtempSync(join(tmpdir(), 'quasar-site-'));
  try {
    const logged = [];
    const hook = prepareHostIntegration().hooks['astro:build:done'];
    await hook({ dir: new URL(`file://${out}/`), logger: { info: (m) => logged.push(m) } });
    const written = readFileSync(join(out, PREPARE_HOST_ASSET));
    assert.equal(sha(written), PREPARE_HOST_SHA256);
    assert.match(logged.join('\n'), new RegExp(PREPARE_HOST_SHA256));
    assert.equal(writePrepareHost(out), join(out, 'prepare-host.sh'));
  } finally {
    rmSync(out, { recursive: true, force: true });
  }
});

test('the URL the prep block fetches is where the site publishes it', () => {
  assert.equal(TEMPLATE_URL, PREPARE_HOST_URL);
  // astro.config.mjs: site https://accreleus.github.io, base /quasar; the build output's
  // root is served at the base path.
  assert.equal(new URL(PREPARE_HOST_URL).pathname, `/quasar/${PREPARE_HOST_ASSET}`);
});

test('the dev server answers the same path with the same bytes', () => {
  let handler;
  prepareHostIntegration().hooks['astro:server:setup']({ server: { middlewares: { use: (h) => { handler = h; } } } });
  let body;
  const headers = {};
  handler({ url: `/quasar/${PREPARE_HOST_ASSET}?x=1` }, { setHeader: (k, v) => { headers[k] = v; }, end: (b) => { body = b; } }, () => assert.fail('passed on'));
  assert.equal(sha(body), PREPARE_HOST_SHA256);
  assert.match(headers['Content-Type'], /shellscript/);
  let passed = false;
  handler({ url: '/quasar/start/' }, {}, () => { passed = true; });
  assert.ok(passed);
});
