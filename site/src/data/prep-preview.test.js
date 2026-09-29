import { test } from 'node:test';
import assert from 'node:assert/strict';

import { prepPreviewLines } from './prep-preview.js';
import { DEFAULTS } from './stack-template.js';

const a = (over = {}) => ({ ...DEFAULTS, engine: 'podman', mode: 'rootless', ...over });

test('names the engine and closes with the same line as the real script', () => {
  const out = prepPreviewLines(a());
  assert.match(out, /engine: podman/);
  assert.match(out, /Host preparation is complete\. Run it again at any time: it changes only what is missing\.$/);
});

test('a first run reports "changed"; a rerun reports only "ok", with nothing left unexplained', () => {
  const first = prepPreviewLines(a());
  const rerun = prepPreviewLines(a(), { rerun: true });
  assert.match(first, /changed/);
  assert.ok(!rerun.includes('changed'), rerun);
  assert.match(rerun, /^\s*ok\s+account quasar$/m);
});

test('rootless creates the quasar account and prints the homes root; rootful creates only a group', () => {
  assert.match(prepPreviewLines(a({ mode: 'rootless' })), /account quasar/);
  assert.match(prepPreviewLines(a({ mode: 'rootless' })), /homes root \/var\/lib\/quasar\/homes/);
  const rootful = prepPreviewLines(a({ mode: 'rootful' }));
  assert.ok(!rootful.includes('account quasar'));
  assert.match(rootful, /group quasar/);
  assert.ok(!rootful.includes('homes root'));
});

test('podman enables its socket and restart units; rootful Docker enables docker.service; rootless Docker notes the user setup', () => {
  assert.match(prepPreviewLines(a({ engine: 'podman' })), /podman\.socket enabled for quasar/);
  assert.match(prepPreviewLines(a({ engine: 'docker', mode: 'rootful' })), /docker\.service enabled/);
  assert.match(prepPreviewLines(a({ engine: 'docker', mode: 'rootless' })), /dockerd-rootless-setuptool\.sh/);
});

test('the console toggle adds the i2c-dev module line', () => {
  assert.ok(!prepPreviewLines(a({ console: false })).includes('i2c-dev'));
  assert.match(prepPreviewLines(a({ console: true })), /i2c-dev/);
});
