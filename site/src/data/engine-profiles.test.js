import { test } from 'node:test';
import assert from 'node:assert/strict';

import { ENGINES, MODES, PLATFORMS as PROFILE_PLATFORMS, STATUSES, profileFor } from './engine-profiles.js';
import { PLATFORMS as QUICK_START_PLATFORMS } from './platforms.js';

const PLATFORM_IDS = Object.keys(PROFILE_PLATFORMS);
const ENGINE_IDS = Object.keys(ENGINES);
const every = () =>
  PLATFORM_IDS.flatMap((p) => ENGINE_IDS.flatMap((e) => MODES.map((m) => profileFor(p, e, m))));

test('every combination has one of the three statuses and a reason', () => {
  assert.deepEqual(STATUSES, ['supported', 'experimental', 'unsupported']);
  for (const p of every()) {
    assert.ok(STATUSES.includes(p.status), JSON.stringify(p));
    assert.ok(p.reason.length > 0, JSON.stringify(p));
  }
});

test('rootful Docker is supported on every platform, Unraid included (D4)', () => {
  for (const platform of PLATFORM_IDS) {
    assert.equal(profileFor(platform, 'docker', 'rootful').status, 'supported', platform);
  }
});

test('the other Fedora and Ubuntu 24.04 profiles are experimental until proven (D5)', () => {
  for (const platform of ['fedora', 'ubuntu']) {
    for (const [engine, mode] of [['docker', 'rootless'], ['podman', 'rootless'], ['podman', 'rootful']]) {
      assert.equal(profileFor(platform, engine, mode).status, 'experimental', `${platform} ${engine} ${mode}`);
    }
  }
});

test('anything else is unsupported, and names alternatives that are not', () => {
  for (const platform of ['debian', 'arch', 'unraid', 'other']) {
    for (const [engine, mode] of [['docker', 'rootless'], ['podman', 'rootless'], ['podman', 'rootful']]) {
      const p = profileFor(platform, engine, mode);
      assert.equal(p.status, 'unsupported', `${platform} ${engine} ${mode}`);
      assert.ok(p.alternatives.length > 0, JSON.stringify(p));
      for (const alt of p.alternatives) assert.notEqual(alt.status, 'unsupported', JSON.stringify(alt));
    }
  }
});

test('Unraid points only at its own rootful Docker', () => {
  const p = profileFor('unraid', 'podman', 'rootless');
  assert.deepEqual(p.alternatives, [{ platform: 'unraid', engine: 'docker', mode: 'rootful', status: 'supported' }]);
});

test('an engine the table does not know is unsupported everywhere', () => {
  for (const platform of PLATFORM_IDS) {
    for (const mode of MODES) {
      const p = profileFor(platform, 'lxd', mode);
      assert.equal(p.status, 'unsupported');
      assert.ok(p.alternatives.some((a) => a.platform === platform && a.engine === 'docker' && a.mode === 'rootful'));
    }
  }
});

test('an unknown platform reads as the catch-all', () => {
  assert.equal(profileFor('gentoo', 'podman', 'rootless').platform, 'other');
  assert.equal(profileFor('gentoo', 'podman', 'rootless').status, 'unsupported');
});

test('every platform the quick start offers has profiles', () => {
  for (const id of Object.keys(QUICK_START_PLATFORMS)) {
    assert.ok(Object.hasOwn(PROFILE_PLATFORMS, id), id);
  }
});
