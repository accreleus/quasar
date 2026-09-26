import { test } from 'node:test';
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { mkdtempSync, writeFileSync, chmodSync, readFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { load } from 'js-yaml';

import { DEFAULTS, ROLES, REGISTRY_NS, CHANNEL_TAG, generate, homePath, templatePath, appUser, seedInputs } from './stack-template.js';
import { PROXIES, proxyConfig } from './proxy-configs.js';
import { PLATFORMS } from './platforms.js';

const ROLE_IDS = Object.keys(ROLES);
const ACCESS = ['self-signed', 'proxy'];

const full = (over = {}) => ({
  ...DEFAULTS,
  publicHost: '192.168.1.50',
  tlsHosts: 'quasar.lan',
  publicUrl: 'https://quasar.example.com',
  ...over,
});

const env = (stack) => load(stack).services['quasar-seed'].environment;

// --- the stack ------------------------------------------------------------

test('every role is one service, the seed, with the two mounts and the named volume', () => {
  for (const role of ROLE_IDS) {
    const doc = load(generate(full({ role })).stack);
    assert.deepEqual(Object.keys(doc.services), ['quasar-seed'], role);
    const seed = doc.services['quasar-seed'];
    assert.equal(seed.container_name, 'quasar-seed');
    assert.equal(seed.command, 'seed');
    assert.equal(seed.restart, 'unless-stopped');
    assert.deepEqual(seed.security_opt, ['label=disable']);
    assert.deepEqual(seed.volumes, ['/var/run/docker.sock:/var/run/docker.sock', 'quasar-machine:/var/lib/quasar-machine:ro']);
    // Without its own name Compose prefixes the project, and the seed refuses it.
    assert.deepEqual(doc.volumes, { 'quasar-machine': { name: 'quasar-machine' } });
  }
});

test('every image is a digest placeholder, never a tag', () => {
  for (const role of ROLE_IDS) {
    const { stack } = generate(full({ role }));
    const doc = load(stack);
    const refs = [doc.services['quasar-seed'].image, ...Object.entries(env(stack)).filter(([k]) => k.endsWith('_IMAGE')).map(([, v]) => v)];
    for (const ref of refs) assert.match(ref, new RegExp(`^${REGISTRY_NS}/quasar-[a-z-]+@sha256:<digest>$`), `${role}: ${ref}`);
    assert.ok(!stack.includes(`:${CHANNEL_TAG}`), `${role}: the stack must not carry the channel tag`);
  }
});

test('each role takes the inputs its seed needs and no others', () => {
  const combined = env(generate(full({ role: 'combined' })).stack);
  assert.equal(combined.QUASAR_ROLE, 'combined');
  assert.equal(combined.QUASAR_PUBLIC_HOST, '192.168.1.50');
  assert.equal(combined.QUASAR_TLS_HOSTS, 'quasar.lan');
  assert.ok(combined.QUASAR_CONTROL_PLANE_IMAGE && combined.QUASAR_AGENT_IMAGE);
  assert.equal(combined.QUASAR_HOME_ROOT, '/var/lib/quasar/homes');
  assert.equal(combined.QUASAR_TEMPLATE_ROOT, '/var/lib/quasar/templates');
  assert.equal(combined.QUASAR_ENROLLMENT, undefined);

  const control = env(generate(full({ role: 'control-only' })).stack);
  assert.equal(control.QUASAR_ROLE, 'control-only');
  // Add host installs this agent image on new GPU hosts.
  assert.ok(control.QUASAR_AGENT_IMAGE);
  for (const k of ['QUASAR_HOME_ROOT', 'QUASAR_TEMPLATE_ROOT', 'QUASAR_ENROLLMENT', 'QUASAR_APP_PUID']) assert.equal(control[k], undefined, k);

  const gpu = env(generate(full({ role: 'gpu' })).stack);
  assert.equal(gpu.QUASAR_ROLE, 'gpu');
  assert.match(gpu.QUASAR_ENROLLMENT, /^qenr1\./);
  for (const k of ['QUASAR_PUBLIC_HOST', 'QUASAR_TLS_HOSTS', 'QUASAR_CONTROL_PLANE_IMAGE', 'QUASAR_DATABASE_HOST', 'QUASAR_HTTP_PORT']) {
    assert.equal(gpu[k], undefined, k);
  }
});

test('the ports are inputs only when they move', () => {
  assert.equal(env(generate(full()).stack).QUASAR_HTTP_PORT, undefined);
  const moved = env(generate(full({ controlPort: 9080, tlsPort: 9443 })).stack);
  assert.equal(moved.QUASAR_HTTP_PORT, '9080');
  assert.equal(moved.QUASAR_TLS_PORT, '9443');
});

test('trusted proxies are an input only behind a proxy', () => {
  assert.equal(env(generate(full({ trustedProxies: '192.168.1.2' })).stack).QUASAR_TRUSTED_PROXIES, undefined);
  assert.equal(env(generate(full({ access: 'proxy', trustedProxies: '192.168.1.2' })).stack).QUASAR_TRUSTED_PROXIES, '192.168.1.2');
});

test('no secret is ever generated or written', () => {
  for (const role of ROLE_IDS) {
    for (const database of ['owned', 'external']) {
      const out = generate(full({ role, database, dbHost: 'db.example.internal' }));
      const all = [out.stack, out.env ?? '', out.script ?? '', out.pins].join('\n');
      assert.ok(!/openssl|rand -hex|QUASAR_SECRET_KEY|POSTGRES_PASSWORD|ENROLLMENT_TOKEN/.test(all), `${role}/${database}`);
    }
  }
});

test("the operator's own database is interpolated from the stack's .env, never written", () => {
  const out = generate(full({ database: 'external', dbHost: 'db.example.internal', dbPort: 5433, dbSslmode: 'verify-full' }));
  const e = env(out.stack);
  assert.equal(e.QUASAR_DATABASE_HOST, 'db.example.internal');
  assert.equal(e.QUASAR_DATABASE_PORT, '5433');
  assert.equal(e.QUASAR_DATABASE_SSLMODE, 'verify-full');
  assert.equal(e.QUASAR_DATABASE_PASSWORD, '${QUASAR_DATABASE_PASSWORD}');
  assert.ok(out.env.split('\n').includes('QUASAR_DATABASE_PASSWORD='));
  assert.equal(generate(full()).env, null);
  assert.equal(generate(full({ role: 'gpu', database: 'external' })).env, null);
});

test('homes can live on a different disk, templates beside them', () => {
  const a = full({ separateSaves: true, savesPath: '/mnt/tank/quasar/' });
  assert.equal(homePath(a), '/mnt/tank/quasar');
  assert.equal(templatePath(a), '/mnt/tank/templates');
  assert.equal(homePath(full({ basePath: '/srv/quasar/' })), '/srv/quasar/homes');
});

test('save ownership becomes the app-container user, and only when it differs', () => {
  assert.deepEqual(appUser(full({ platform: 'unraid' })), { uid: 99, gid: 100 });
  const unraid = env(generate(full({ platform: 'unraid' })).stack);
  assert.equal(unraid.QUASAR_APP_PUID, '99');
  assert.equal(unraid.QUASAR_APP_PGID, '100');
  assert.equal(env(generate(full()).stack).QUASAR_APP_PUID, undefined);
  assert.equal(env(generate(full({ owner: 'custom', uid: 1001, gid: 1001 })).stack).QUASAR_APP_PUID, '1001');
});

test('a value with a line break is refused', () => {
  assert.throws(() => generate(full({ publicHost: 'a\nb' })), /single line/);
});

test('the stack carries the seed inputs in the same order as the script', () => {
  const a = full({ role: 'combined', database: 'external', dbHost: 'db' });
  const names = seedInputs(a).map(([k]) => k);
  assert.deepEqual(Object.keys(env(generate(a).stack)), names);
  const script = generate(a).script;
  let at = 0;
  for (const k of names) {
    const i = script.indexOf(`-e ${k}`, at);
    assert.ok(i > 0, `script lacks ${k}`);
    at = i;
  }
});

// --- the pins -------------------------------------------------------------

test('the pins command names each image the role installs, by the channel tag', () => {
  const combined = generate(full()).pins;
  for (const name of ['quasar-recovery', 'quasar-control-plane', 'quasar-node-agent']) assert.ok(combined.includes(name), name);
  assert.ok(combined.includes(`:${CHANNEL_TAG}`));
  assert.ok(!generate(full({ role: 'gpu' })).pins.includes('quasar-control-plane'));
  assert.equal(spawnSync('bash', ['-n'], { input: combined }).status, 0);
});

// --- the script -----------------------------------------------------------

test('a GPU host gets no script: it joins with the one-line command from Add host', () => {
  assert.equal(generate(full({ role: 'gpu' })).script, null);
});

test('generated scripts parse for every platform, role and access mode', () => {
  for (const platform of Object.keys(PLATFORMS)) {
    for (const role of ROLE_IDS.filter((r) => ROLES[r].control)) {
      for (const access of ACCESS) {
        for (const database of ['owned', 'external']) {
          const script = generate(full({ platform, role, access, database, dbHost: 'db' })).script;
          const r = spawnSync('bash', ['-n'], { input: script, encoding: 'utf8' });
          assert.equal(r.status, 0, `${platform}/${role}/${access}/${database}: ${r.stderr}`);
        }
      }
    }
  }
});

test('unraid persists through the boot script and runs without sudo', () => {
  const script = generate(full({ platform: 'unraid' })).script;
  assert.match(script, /\/boot\/config\/go/);
  assert.ok(!script.includes('sudo '));
  assert.ok(!script.includes('/etc/sysctl.d'));
});

test('systemd platforms use the drop-ins', () => {
  const script = generate(full({ platform: 'fedora' })).script;
  assert.match(script, /\/etc\/sysctl\.d\/99-quasar\.conf/);
  assert.match(script, /\/etc\/modules-load\.d\/uinput\.conf/);
});

test('a control-only machine prepares nothing for games', () => {
  const script = generate(full({ role: 'control-only' })).script;
  for (const s of ['/dev/dri', 'uinput', 'wmem_default', 'install -d']) assert.ok(!script.includes(s), s);
});

test('the script never restarts the Docker daemon', () => {
  for (const platform of Object.keys(PLATFORMS)) {
    const script = generate(full({ platform })).script;
    assert.ok(!/systemctl restart docker|rc\.docker restart/.test(script), platform);
  }
});

test('every platform is complete', () => {
  for (const [id, p] of Object.entries(PLATFORMS)) {
    for (const key of ['label', 'sudo', 'defaultUid', 'defaultGid', 'defaultBasePath', 'ownerLabel']) {
      assert.ok(p[key] !== undefined, `${id} is missing ${key}`);
    }
    assert.equal(typeof p.sysctl(), 'string', `${id}: sysctl must render`);
    assert.equal(typeof p.module(), 'string', `${id}: module must render`);
  }
});

/**
 * Run a generated script against a fake docker and curl on PATH. The fake docker
 * logs every call; `legacy` makes it report a Compose-labelled control plane.
 */
function runFake(answers, { legacy = false, existing = false, extraEnv = {} } = {}) {
  const dir = mkdtempSync(join(tmpdir(), 'quasar-qs-'));
  const log = join(dir, 'calls');
  writeFileSync(join(dir, 'docker'), `#!/usr/bin/env bash
echo "$*" >> ${JSON.stringify(log)}
case "$1" in
  info) exit 0 ;;
  ps) case "$*" in *service=quasar-control-plane*) ${legacy ? 'echo deadbeef' : ':'} ;; esac ;;
  container) ${existing ? 'exit 0' : 'exit 1'} ;;
  pull) exit 0 ;;
  image) ref="\${@: -1}"; echo "\${ref%:*}@sha256:0123abcd" ;;
  run) echo cafe ;;
  exec) exit 0 ;;
esac
`);
  writeFileSync(join(dir, 'curl'), '#!/usr/bin/env bash\nexit 0\n');
  chmodSync(join(dir, 'docker'), 0o755);
  chmodSync(join(dir, 'curl'), 0o755);
  try {
    const r = spawnSync('bash', ['-c', generate(answers).script], {
      encoding: 'utf8',
      env: { PATH: `${dir}:${process.env.PATH}`, ...extraEnv },
    });
    let calls = '';
    try { calls = readFileSync(log, 'utf8'); } catch {}
    return { ...r, calls };
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
}

test('the script refuses a host still running a stack made from the Compose files', () => {
  const r = runFake(full({ role: 'control-only' }), { legacy: true });
  assert.equal(r.status, 1, r.stdout + r.stderr);
  assert.match(r.stderr, /made from the Compose files: quasar-control-plane/);
  assert.ok(!/^run /m.test(r.calls), 'nothing may start');
});

test('the script refuses a machine that is already installed', () => {
  const r = runFake(full({ role: 'control-only' }), { existing: true });
  assert.equal(r.status, 1);
  assert.match(r.stderr, /already installed/);
  assert.ok(!/^run /m.test(r.calls));
});

test('the script pins every image to its digest and starts the seed with them', () => {
  const r = runFake(full({ role: 'control-only' }));
  assert.equal(r.status, 0, r.stdout + r.stderr);
  const run = r.calls.split('\n').find((l) => l.startsWith('run '));
  assert.ok(run, r.calls);
  assert.ok(run.includes(`-e QUASAR_CONTROL_PLANE_IMAGE=${REGISTRY_NS}/quasar-control-plane@sha256:0123abcd`), run);
  assert.ok(run.includes(`-e QUASAR_AGENT_IMAGE=${REGISTRY_NS}/quasar-node-agent@sha256:0123abcd`), run);
  assert.ok(run.endsWith(`${REGISTRY_NS}/quasar-recovery@sha256:0123abcd seed`), run);
  assert.ok(run.includes('-e QUASAR_ROLE=control-only'));
  assert.match(r.stdout, /setup-token/);
});

test("the script needs the operator's database password from its environment, and never prints it", () => {
  const a = full({ role: 'control-only', database: 'external', dbHost: 'db.example.internal' });
  const without = runFake(a);
  assert.equal(without.status, 1);
  assert.match(without.stderr, /QUASAR_DATABASE_PASSWORD/);
  const withPw = runFake(a, { extraEnv: { QUASAR_DATABASE_PASSWORD: 'not-in-the-script' } });
  assert.equal(withPw.status, 0, withPw.stderr);
  const run = withPw.calls.split('\n').find((l) => l.startsWith('run '));
  assert.ok(run.includes('-e QUASAR_DATABASE_PASSWORD -e') || run.includes('-e QUASAR_DATABASE_PASSWORD '), run);
  assert.ok(!run.includes('not-in-the-script'));
  assert.ok(!generate(a).script.includes('not-in-the-script'));
});

// --- proxy snippets -------------------------------------------------------

test('only a control plane behind a proxy gets a proxy config', () => {
  assert.equal(generate(full()).proxyConfig, null);
  assert.equal(generate(full({ role: 'gpu', access: 'proxy' })).proxyConfig, null);
  assert.ok(generate(full({ access: 'proxy' })).proxyConfig.body);
});

test('every proxy config meets the documented requirements', () => {
  for (const id of Object.keys(PROXIES)) {
    const { body } = proxyConfig(id, { publicUrl: 'https://quasar.example.com', host: '192.168.1.50', port: 8080 });
    assert.match(body, /v1\/signal|reverse_proxy|loadBalancer/, `${id}: must route signaling`);
    assert.match(body, /X-Forwarded-Proto/i, `${id}: must send X-Forwarded-Proto`);
    assert.ok(!body.includes(':8443'), `${id}: must proxy to the HTTP listener, not 8443`);
    assert.match(body, /3600|readTimeout|read_timeout/i, `${id}: must raise the read timeout`);
  }
});

test('the path-based proxies name both websocket routes', () => {
  for (const id of ['nginx', 'npm']) {
    const { body } = proxyConfig(id, { publicUrl: 'https://quasar.example.com', host: '192.168.1.50', port: 8080 });
    assert.match(body, /v1\/signal/, `${id}: must route /v1/signal`);
    assert.match(body, /agent\/ws/, `${id}: must route /agent/ws`);
    assert.match(body, /proxy_buffering off/, `${id}: must turn buffering off`);
  }
});
