import { test } from 'node:test';
import assert from 'node:assert/strict';

import { DEFAULTS, generate, agentImage, encoder, homePath, statePath, appUser } from './stack-template.js';
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
  assert.ok(compose.includes('${QUASAR_HOME_ROOT:?'), 'compose must keep ${QUASAR_HOME_ROOT:?}');
});

test('env carries openssl calls, not literal secrets', () => {
  const { env } = generate(DEFAULTS);
  assert.ok(env.includes('$(openssl rand -hex 24)'), 'POSTGRES_PASSWORD is generated on the host');
  assert.ok(env.includes('$(openssl rand -hex 32)'), 'ENROLLMENT_TOKEN is generated on the host');
  assert.ok(env.includes('$(openssl rand -base64 32)'), 'QUASAR_SECRET_KEY is generated on the host');
});

test('script quotes the compose heredoc and leaves the env heredoc unquoted', () => {
  const { script } = generate(DEFAULTS);
  assert.ok(script.includes("<<'COMPOSE'"), 'compose heredoc must be quoted so ${...} survives');
  assert.ok(script.includes('<<ENV'), 'env heredoc must be unquoted so $(openssl ...) runs');
  assert.ok(!script.includes("<<'ENV'"), 'a quoted env heredoc writes literal $(openssl ...) into .env');
});

// --- branch coverage ------------------------------------------------------

test('nvidia is the only gpu that gets an overlay', () => {
  for (const gpu of GPUS) {
    const { nvidia, script } = generate(full({ gpu }));
    if (gpu === 'nvidia') {
      assert.ok(nvidia?.includes('gpus: all'), 'nvidia needs the overlay');
      assert.ok(script.includes('docker-compose.nvidia.yml'), 'nvidia must write and use the overlay');
    } else {
      assert.equal(nvidia, null, `${gpu} must not get an nvidia overlay`);
      assert.ok(!script.includes('nvidia'), `${gpu} script must not mention nvidia`);
    }
  }
});

test('Vulkan is the encoder on every gpu the wizard offers', () => {
  assert.equal(encoder(), 'vulkan');
  for (const gpu of GPUS) {
    assert.match(generate(full({ gpu })).env, /QUASAR_ENCODER=vulkan/, `${gpu} must use vulkan`);
  }
});

// One universal agent image: the GPU answer selects the compose overlay, never
// the image. A per-vendor lineage here would hand the user a 404.
test('agent image is the same for every gpu', () => {
  assert.match(agentImage('nvidia'), /quasar-node-agent:latest$/);
  assert.equal(agentImage('nvidia'), agentImage('amd-intel'));
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

test('the own-cert branch mounts the directory and names both files', () => {
  const { env, compose } = generate(full({ access: 'own-cert' }));
  assert.match(env, /QUASAR_TLS_CERT_DIR=\/etc\/ssl\/quasar/);
  assert.match(env, /QUASAR_TLS_CERT=\/etc\/quasar\/tls\/cert\.pem/);
  assert.match(env, /QUASAR_TLS_KEY=\/etc\/quasar\/tls\/key\.pem/);
  assert.ok(compose.includes('/etc/quasar/tls:ro'), 'own-cert needs the read-only mount');
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
  assert.ok(script.includes('-o 1000 -g 1000'), 'the state dir stays 1000');
});

// --- the destructive step -------------------------------------------------

test('the docker restart prompts before running', () => {
  const { script } = generate(full({ gpu: 'nvidia' }));
  const restart = script.indexOf('systemctl restart docker');
  assert.ok(restart > -1, 'nvidia needs the restart');
  assert.ok(script.slice(0, restart).includes('read -r'), 'the restart must sit behind a prompt');
});

test('a non-NVIDIA host never restarts docker', () => {
  assert.ok(
    !generate(full({ gpu: 'amd-intel' })).script.includes('restart docker'),
    'only the NVIDIA branch touches the docker daemon'
  );
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
  assert.match(script, /\/etc\/rc\.d\/rc\.docker restart/, 'unraid restarts docker through rc.d');
  assert.ok(!script.includes('systemctl'), 'systemctl does not exist on unraid');
  assert.ok(!script.includes('sudo '), 'the unraid shell is already root');
});

test('systemd platforms use the drop-in and systemctl', () => {
  for (const id of ['fedora', 'debian', 'arch', 'other']) {
    const { script } = generate(full({ platform: id, gpu: 'nvidia' }));
    assert.match(script, /\/etc\/sysctl\.d\/99-quasar\.conf/, `${id}: sysctl drop-in`);
    assert.match(script, /systemctl restart docker/, `${id}: systemctl restart`);
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
test('the agent can read the kernel ring buffer for GPU faults', () => {
  for (const gpu of GPUS) {
    const { compose } = generate(full({ gpu }));
    assert.match(compose, /- \/dev\/kmsg:\/dev\/kmsg:r\b/, `${gpu}: /dev/kmsg must be passed read-only`);
    assert.match(compose, /cap_add: \[NET_ADMIN, SYSLOG\]/, `${gpu}: reading kmsg needs CAP_SYSLOG under dmesg_restrict=1`);
  }
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
