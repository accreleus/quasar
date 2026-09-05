import { test } from 'node:test';
import assert from 'node:assert/strict';
import { load } from 'js-yaml';

import { DEFAULTS, generate, homePath, statePath, appUser } from './stack-template.js';
import { PROXIES, proxyConfig } from './proxy-configs.js';
import { PLATFORMS } from './platforms.js';

const GPUS = ['nvidia', 'amd-intel'];
const ACCESS = ['self-signed', 'proxy', 'own-cert'];

const full = (over = {}) => ({
  ...DEFAULTS,
  tlsHosts: '192.168.1.50,quasar.lan',
  publicUrl: 'https://quasar.example.com',
  certPath: '/etc/ssl/quasar/cert.pem',
  keyPath: '/etc/ssl/quasar/key.pem',
  ...over,
});

// --- the quoting contract -------------------------------------------------
// These three are the reason this module has tests at all. Getting the heredoc
// quoting backwards produces a Compose file of empty interpolations and a .env
// full of literal $(openssl ...) strings, and both fail far from the cause.

test('compose keeps Compose interpolation literal', () => {
  const { compose } = generate(DEFAULTS);
  assert.ok(compose.includes('${POSTGRES_PASSWORD:?'), 'compose must keep ${POSTGRES_PASSWORD:?}');
  assert.ok(compose.includes('${QUASAR_HOME_ROOT:'), 'compose must keep the home-root override');
});

test('copied env has blank credentials and instructions, never executable values', () => {
  const { env, script } = generate(DEFAULTS);
  for (const key of ['POSTGRES_PASSWORD', 'ENROLLMENT_TOKEN', 'QUASAR_SECRET_KEY']) {
    assert.ok(env.split('\n').includes(`${key}=`));
  }
  assert.match(env, /paste each OUTPUT/);
  assert.ok(!env.includes('$('));
  assert.ok(script.includes("<<'ENV'"));
  assert.ok(script.includes('if [ ! -e deploy/.env ]; then'));
  assert.ok(script.includes('postgres=$(openssl rand -hex 24)'));
});

test('one complete compose includes NVIDIA wiring only when selected', () => {
  for (const gpu of GPUS) {
    const result = generate(full({ gpu }));
    const doc = load(result.compose);
    assert.equal(result.nvidia, null);
    assert.equal(doc.services['quasar-node-agent'].gpus, gpu === 'nvidia' ? 'all' : undefined);
    assert.ok(!result.script.includes('docker-compose.nvidia.yml'));
    assert.ok(doc.services['quasar-control-plane'].volumes.includes('quasar-updater-run:/run/quasar-updater'));
    assert.ok(doc.services['quasar-control-plane'].volumes.includes('quasar-control-tls:/var/lib/quasar-control'));
    assert.equal(doc.services['quasar-node-agent'].environment.QUASAR_APP_SHM_SIZE, '${QUASAR_APP_SHM_SIZE:-1g}');
  }
});

test('image choices are explicit and the GPU encoder is detected by the agent', () => {
  const { compose, env } = generate(full());
  assert.match(compose, /QUASAR_CONTROL_IMAGE:\?/);
  assert.match(compose, /QUASAR_AGENT_IMAGE:\?/);
  assert.match(env, /QUASAR_ENCODER=\n/);
  const cp = load(compose).services['quasar-control-plane'].environment;
  for (const key of ['QUASAR_PLATFORM_RELEASE_REPO', 'QUASAR_PLATFORM_RELEASE_API', 'QUASAR_PLATFORM_RELEASE_ASSET_HOSTS', 'QUASAR_PLATFORM_RELEASE_TOKEN', 'QUASAR_PLATFORM_RELEASE_DETECT_INTERVAL', 'QUASAR_PLATFORM_REGISTRY', 'QUASAR_IMAGE_REGISTRY_HOSTS']) {
    assert.equal(cp[key], '${' + key + ':-}', `${key} must retain its .env override`);
  }
});

test('only the proxy branch emits a proxy config', () => {
  for (const access of ACCESS) {
    const { proxyConfig: cfg } = generate(full({ access }));
    if (access === 'proxy') assert.ok(cfg?.body, 'proxy branch needs a config');
    else assert.equal(cfg, null, `${access} must not emit a proxy config`);
  }
});

test('the self-signed branch writes the addresses it collected', () => {
  const { env } = generate(full({ access: 'self-signed' }));
  assert.match(env, /QUASAR_TLS_HOSTS=192\.168\.1\.50,quasar\.lan/);
});

test('the proxy branch sets the public URL and the origin allow-list', () => {
  const { env } = generate(full({ access: 'proxy' }));
  assert.match(env, /PUBLIC_BASE_URL=https:\/\/quasar\.example\.com/);
  assert.match(env, /QUASAR_ALLOWED_ORIGINS=https:\/\/quasar\.example\.com/);
});

test('own certificate and key can come from different directories', () => {
  const { compose } = generate(full({ access: 'own-cert', keyPath: '/private/key.pem' }));
  const cp = load(compose).services['quasar-control-plane'];
  assert.equal(cp.environment.QUASAR_TLS_CERT, '/etc/quasar/tls/cert.pem');
  assert.equal(cp.environment.QUASAR_TLS_KEY, '/etc/quasar/tls/key.pem');
  assert.deepEqual(cp.volumes.filter(v => typeof v === 'object').map(v => [v.source, v.target, v.read_only, v.bind.create_host_path]), [
    ['${QUASAR_TLS_CERT:?}', '/etc/quasar/tls/cert.pem', true, false],
    ['${QUASAR_TLS_KEY:?}', '/etc/quasar/tls/key.pem', true, false],
  ]);
});

// --- paths and ownership --------------------------------------------------

test('saves can live on a different disk from the rest', () => {
  assert.equal(homePath({ ...DEFAULTS }), '/var/lib/quasar/homes');
  assert.equal(statePath({ ...DEFAULTS }), '/var/lib/quasar/control');
  assert.equal(
    homePath({ ...DEFAULTS, separateSaves: true, savesPath: '/mnt/tank/quasar/' }),
    '/mnt/tank/quasar'
  );
});

test('save ownership drives PUID/PGID and the chown, never the control plane', () => {
  const custom = full({ owner: 'custom', uid: 4242, gid: 4243 });
  assert.deepEqual(appUser(custom), { uid: 4242, gid: 4243 });
  const { env, script } = generate(custom);
  assert.match(env, /QUASAR_APP_PUID=4242/);
  assert.match(env, /QUASAR_APP_PGID=4243/);
  assert.ok(script.includes('-o 4242 -g 4243'), 'the home root is chowned to the chosen owner');
  // The control plane image runs as 1000 and owns its files as 1000, so its
  // state directory is chowned to 1000 whatever the user picked for saves.
  assert.ok(!script.includes('-o 1000 -g 1000'), 'control state uses the image-owned named volume');
});

// --- the destructive step -------------------------------------------------

test('installer checks runtime but never restarts the shared Docker daemon', () => {
  for (const gpu of GPUS) {
    const { script } = generate(full({ gpu }));
    assert.ok(!script.includes('restart docker'));
    assert.ok(!script.includes('runtime configure'));
    if (gpu === 'nvidia') assert.match(script, /NVIDIA Container Toolkit/);
  }
});

// --- platform differences -------------------------------------------------
// Unraid runs / from a ramdisk and is not systemd. Getting either wrong gives
// an install that works until the box reboots and then streams badly, which is
// the hardest class of bug to attribute back to the installer.

test('unraid persists through the boot script, not /etc', () => {
  const { script } = generate(full({ platform: 'unraid' }));
  assert.match(script, /\/boot\/config\/go/, 'unraid must persist in the go file');
  assert.ok(!script.includes('/etc/sysctl.d'), '/etc does not survive a reboot on unraid');
  assert.ok(!script.includes('/etc/modules-load.d'), '/etc does not survive a reboot on unraid');
});

test('unraid is not systemd and is already root', () => {
  const { script } = generate(full({ platform: 'unraid', gpu: 'nvidia' }));
  assert.ok(!script.includes('rc.d/rc.docker restart'));
  assert.ok(!script.includes('systemctl'), 'systemctl does not exist on unraid');
  assert.ok(!script.includes('sudo '), 'the unraid shell is already root');
});

test('systemd platforms use the drop-in and systemctl', () => {
  for (const id of ['fedora', 'debian', 'arch', 'other']) {
    const { script } = generate(full({ platform: id, gpu: 'nvidia' }));
    assert.match(script, /\/etc\/sysctl\.d\/99-quasar\.conf/, `${id}: sysctl drop-in`);
    assert.ok(!script.includes('systemctl restart docker'));
    assert.ok(!script.includes('/boot/config/go'), `${id}: no unraid boot script`);
  }
});

test('the platform supplies the save-owner default', () => {
  assert.deepEqual(appUser({ ...DEFAULTS, platform: 'unraid', owner: 'dedicated' }), { uid: 99, gid: 100 });
  assert.deepEqual(appUser({ ...DEFAULTS, platform: 'fedora', owner: 'dedicated' }), { uid: 1000, gid: 1000 });
});

test('every platform is complete', () => {
  for (const [id, p] of Object.entries(PLATFORMS)) {
    for (const key of ['label', 'sudo', 'dockerRestart', 'defaultUid', 'defaultGid', 'defaultBasePath', 'ownerLabel']) {
      assert.ok(p[key] !== undefined, `${id} is missing ${key}`);
    }
    assert.equal(typeof p.sysctl(), 'string', `${id}: sysctl must render`);
    assert.equal(typeof p.module(), 'string', `${id}: module must render`);
  }
});

// --- proxy snippets -------------------------------------------------------

test('every proxy config meets the documented requirements', () => {
  for (const id of Object.keys(PROXIES)) {
    const { body } = proxyConfig(id, {
      publicUrl: 'https://quasar.example.com',
      host: '192.168.1.50',
      port: 8080,
    });
    // Caddy and Traefik proxy every path, so they satisfy this by routing at
    // all; nginx and NPM name the two WebSocket paths explicitly.
    assert.match(body, /v1\/signal|reverse_proxy|loadBalancer/, `${id}: must route signaling`);
    assert.match(body, /X-Forwarded-Proto/i, `${id}: must send X-Forwarded-Proto`);
    assert.ok(!body.includes(':8443'), `${id}: must proxy to the HTTP listener, not 8443`);
    assert.match(body, /3600|readTimeout|read_timeout/i, `${id}: must raise the read timeout`);
  }
});

// The generated compose is the operator's copy of deploy/docker-compose.yml.
// Anything the shipped file passes to the agent has to be here too, or the site
// hands out a stack that quietly lacks it (#83).
test('optional kernel diagnostics include both device and capability', () => {
  for (const gpu of GPUS) {
    const { compose } = generate(full({ gpu, kernelLogs: true }));
    assert.match(compose, /- \/dev\/kmsg:\/dev\/kmsg:r\b/, `${gpu}: /dev/kmsg must be passed read-only`);
    assert.ok(load(compose).services['quasar-node-agent'].cap_add.includes('SYSLOG'));
  }
});

test('a clean install does not require optional kernel log devices', () => {
  const doc = load(generate(full({ gpu: 'nvidia' })).compose);
  assert.ok(!doc.services['quasar-node-agent'].devices.some(device => String(device).startsWith('/dev/kmsg')));
  assert.ok(!doc.services['quasar-node-agent'].cap_add.includes('SYSLOG'));
});

test('the path-based proxies name both websocket routes', () => {
  for (const id of ['nginx', 'npm']) {
    const { body } = proxyConfig(id, {
      publicUrl: 'https://quasar.example.com',
      host: '192.168.1.50',
      port: 8080,
    });
    assert.match(body, /v1\/signal/, `${id}: must route /v1/signal`);
    assert.match(body, /agent\/ws/, `${id}: must route /agent/ws`);
    assert.match(body, /proxy_buffering off/, `${id}: must turn buffering off`);
  }
});

test('generated install scripts parse for every platform and access mode', async () => {
  const { spawnSync } = await import('node:child_process');
  for (const platform of Object.keys(PLATFORMS)) {
    for (const gpu of GPUS) {
      for (const access of ACCESS) {
        const script = generate(full({ platform, gpu, access })).script;
        const result = spawnSync('bash', ['-n'], { input: script, encoding: 'utf8' });
        assert.equal(result.status, 0, `${platform}/${gpu}/${access}: ${result.stderr}`);
      }
    }
  }
});

test('Docker Compose parses all GPU/certificate combinations', async (t) => {
  const { spawnSync } = await import('node:child_process');
  const { mkdtempSync, writeFileSync, rmSync } = await import('node:fs');
  const { tmpdir } = await import('node:os');
  const { join } = await import('node:path');
  if (spawnSync('docker', ['compose', 'version']).status !== 0) return t.skip('Docker Compose unavailable');
  const dir = mkdtempSync(join(tmpdir(), 'quasar-compose-test-'));
  try {
    for (const gpu of GPUS) {
      for (const access of ACCESS) {
        const result = generate(full({ gpu, access }));
        writeFileSync(join(dir, 'compose.yml'), result.compose);
        writeFileSync(join(dir, '.env'), result.env + '\nQUASAR_STACK_DIR=/tmp/quasar-compose-fixture\nPOSTGRES_PASSWORD=test-only\nENROLLMENT_TOKEN=test-only\nQUASAR_SECRET_KEY=test-only\nQUASAR_CONTROL_IMAGE=example/control:test\nQUASAR_AGENT_IMAGE=example/agent:test\nQUASAR_UPDATER_IMAGE=example/updater:test\n');
        const parsed = spawnSync('docker', ['compose', '-f', join(dir, 'compose.yml'), '--env-file', join(dir, '.env'), 'config', '--quiet'], { encoding: 'utf8' });
        assert.equal(parsed.status, 0, `${gpu}/${access}: ${parsed.stderr}`);
      }
    }
  } finally { rmSync(dir, { recursive: true, force: true }); }
});

test('omitted advanced knobs have service-specific override files', () => {
  const doc = load(generate().compose);
  assert.deepEqual(doc.services['quasar-node-agent'].env_file, [{ path: 'agent.env', required: false }]);
  assert.deepEqual(doc.services['quasar-control-plane'].env_file, [{ path: 'control.env', required: false }]);
  assert.ok(!('QUASAR_SECRET_KEY' in doc.services['quasar-node-agent'].environment));
});

test('installer creates private credentials once and preserves them on rerun', async () => {
  const { spawnSync } = await import('node:child_process');
  const { mkdtempSync, mkdirSync, readFileSync, statSync, rmSync } = await import('node:fs');
  const { tmpdir } = await import('node:os');
  const { join } = await import('node:path');
  const dir = mkdtempSync(join(tmpdir(), 'quasar-credentials-test-'));
  try {
    mkdirSync(join(dir, 'deploy'));
    const { script } = generate(full());
    const start = script.indexOf('echo "==> Environment file"');
    const end = script.indexOf('\ndocker compose ', start);
    const envStep = script.slice(start, end);
    const run = () => spawnSync('bash', ['-eu', '-c', envStep], {
      cwd: dir, encoding: 'utf8',
      env: { ...process.env, control_image: 'control:test', agent_image: 'agent:test', updater_image: 'updater:test' },
    });
    assert.equal(run().status, 0);
    const path = join(dir, 'deploy', '.env');
    const original = readFileSync(path, 'utf8');
    const keys = original.split('\n').filter(line => /^[A-Z_]+=/.test(line)).map(line => line.split('=')[0]);
    assert.equal(new Set(keys).size, keys.length, 'image pins must be unique so self-update can replace them unambiguously');
    assert.match(original, /^POSTGRES_PASSWORD=[a-f0-9]{48}$/m);
    assert.match(original, /^ENROLLMENT_TOKEN=[a-f0-9]{64}$/m);
    assert.match(original, /^QUASAR_SECRET_KEY=[a-zA-Z0-9+/]{43}=$/m);
    assert.equal(statSync(path).mode & 0o777, 0o600);
    assert.equal(run().status, 0);
    assert.equal(readFileSync(path, 'utf8'), original);
  } finally { rmSync(dir, { recursive: true, force: true }); }
});

test('release selection rejects prereleases and foreign component images', async (t) => {
  const { spawnSync } = await import('node:child_process');
  if (spawnSync('jq', ['--version']).status !== 0) return t.skip('jq unavailable');
  const { script } = generate();
  const start = script.indexOf("jq -e '\n") + 7;
  const filter = script.slice(start, script.indexOf("' >/dev/null", start));
  const manifest = {
    format_version: 1, version: '1.2.3', prerelease: false,
    components: ['control-plane', 'node-agent'].map(name => ({
      name, image: 'ghcr.io/accreleus/quasar/quasar-' + name, digest: 'sha256:' + 'a'.repeat(64),
    })),
  };
  const accepts = value => spawnSync('jq', ['-e', filter], { input: JSON.stringify(value) }).status === 0;
  assert.ok(accepts(manifest));
  assert.ok(!accepts({ ...manifest, prerelease: true }));
  assert.ok(!accepts({ ...manifest, version: '1.2.3-rc.1' }));
  manifest.components[0].image = 'example.invalid/foreign/control';
  assert.ok(!accepts(manifest));
});

test('installer rejects missing and old Compose versions before host changes', async () => {
  const { spawnSync } = await import('node:child_process');
  const { script } = generate();
  const start = script.indexOf('  compose_version=');
  const end = script.indexOf('\nfi\nif [ ! -d /dev/dri', start);
  const check = script.slice(start, end);
  for (const [version, accepted] of [['', false], ['garbage', false], ['1.29.2', false], ['2.23.3', false], ['2.29.9', false], ['v2.30.0', true], ['2.40.1', true], ['3.0.0', true]]) {
    const result = spawnSync('bash', ['-eu', '-c', `docker() { echo "$TEST_VERSION"; }; preflight_failed=0; ${check}\nexit "$preflight_failed"`], {
      encoding: 'utf8', env: { ...process.env, TEST_VERSION: version },
    });
    assert.equal(result.status, accepted ? 0 : 1, `${version}: ${result.stderr}`);
  }
});
