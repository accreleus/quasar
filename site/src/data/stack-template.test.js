/**
 * SAFETY: these tests generate real install scripts and execute some of them
 * against a fake container engine. NEVER run this file natively — on this
 * repo's dev host, a fake can be bypassed and a REAL docker daemon is
 * reachable; a prior native run left a real Quasar install behind. Run only
 * in a container with no docker/podman socket, e.g.:
 *   docker run --rm -v "$PWD":/w -w /w/site node:22 sh -c 'npm ci && npm test'
 * See test-harness.js for how execution is isolated even so.
 */
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { existsSync, readFileSync, writeFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { load } from 'js-yaml';

import {
  DEFAULTS,
  ROLES,
  ENGINES,
  MODES,
  REGISTRY_NS,
  CHANNEL_TAG,
  generate,
  homePath,
  templatePath,
  appUser,
  seedInputs,
  hostSteps,
  quadletUnit,
  podmanRunSeed,
  ROOTLESS_DOCS_URL,
  NESTED_GPU_CIL,
  RUNTIME_DIR_TMPFILES,
} from './stack-template.js';
import { PROXIES, proxyConfig } from './proxy-configs.js';
import { PLATFORMS } from './platforms.js';
import { profileFor } from './engine-profiles.js';
import { fakeEngineDir, runScript } from './test-harness.js';

const UNRAID_GOLDEN = readFileSync(fileURLToPath(new URL('./__fixtures__/unraid-golden.sh', import.meta.url)), 'utf8');

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

test('generated scripts parse for every platform, engine, mode, role and access mode', () => {
  for (const platform of Object.keys(PLATFORMS)) {
    for (const engine of ENGINES) {
      for (const mode of MODES) {
        for (const role of ROLE_IDS.filter((r) => ROLES[r].control)) {
          for (const access of ACCESS) {
            for (const database of ['owned', 'external']) {
              const out = generate(full({ platform, engine, mode, role, access, database, dbHost: 'db', nvidia: true }));
              const label = `${platform}/${engine}/${mode}/${role}/${access}/${database}`;
              if (profileFor(platform, engine, mode).status === 'unsupported' || (mode === 'rootless' && platform !== 'unraid')) {
                assert.equal(out.script, null, `${label}: unsupported or rootless must yield no script`);
                assert.equal(out.hostSteps, null, `${label}: no host steps either`);
                continue;
              }
              assert.ok(out.script, `${label}: expected a script`);
              const r = spawnSync('bash', ['-n'], { input: out.script, encoding: 'utf8' });
              assert.equal(r.status, 0, `${label}: ${r.stderr}`);
              if (out.hostSteps) assert.equal(spawnSync('bash', ['-n'], { input: out.hostSteps, encoding: 'utf8' }).status, 0, `${label}: host steps`);
            }
          }
        }
      }
    }
  }
});

test('an unsupported profile generates no install artifacts: the UI blocks it', () => {
  // Unraid + rootless Docker is unsupported per testdata/engine-profiles/profiles.json.
  const out = generate(full({ platform: 'unraid', engine: 'docker', mode: 'rootless' }));
  assert.equal(out.script, null);
  assert.equal(out.hostSteps, null);
  assert.equal(out.quadlet, null);
  // The seed's own stack (Dockge/Arcane) and pins are not engine/mode specific and stay available.
  assert.ok(out.stack);
});

test('unraid keeps its self-contained script, byte for byte (D4)', () => {
  const script = generate(full({ platform: 'unraid' })).script;
  assert.equal(script, UNRAID_GOLDEN);
});

test('unraid runs without sudo, touches no kernel setting, and has no host steps', () => {
  const out = generate(full({ platform: 'unraid' }));
  assert.ok(!out.script.includes('sudo '));
  for (const s of ['/boot/config/go', 'sysctl', 'wmem_default', 'modprobe', 'uinput']) assert.ok(!out.script.includes(s), s);
  assert.equal(out.hostSteps, null);
});

test('no generated output downloads a script or touches kernel settings', () => {
  for (const platform of Object.keys(PLATFORMS)) {
    for (const engine of ENGINES) {
      for (const nvidia of [false, true]) {
        const out = generate(full({ platform, engine, nvidia }));
        for (const text of [out.script, out.hostSteps, out.quadlet, out.podmanRun, out.stack].filter(Boolean)) {
          for (const s of ['prepare-host', 'sha256sum -c', 'curl -fsSL -o', 'wmem_default', 'dmesg_restrict', 'modprobe', 'modules-load.d', 'sysctl']) {
            assert.ok(!text.includes(s), `${platform}/${engine}/nvidia=${nvidia}: ${s}`);
          }
        }
      }
    }
  }
});

test('a rootless answer generates no install artifacts: the rootless page covers it', () => {
  for (const engine of ENGINES) {
    const out = generate(full({ engine, mode: 'rootless' }));
    assert.equal(out.script, null, engine);
    assert.equal(out.hostSteps, null, engine);
    assert.equal(out.quadlet, null, engine);
    assert.equal(out.podmanRun, null, engine);
  }
  assert.equal(ROOTLESS_DOCS_URL, 'https://accreleus.github.io/quasar/install/rootless/');
});

test('a control-only machine prepares nothing for games', () => {
  const script = generate(full({ role: 'control-only' })).script;
  for (const s of ['/dev/dri', 'uinput', 'install -d']) assert.ok(!script.includes(s), s);
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
  }
});

// --- host steps and env overrides --------------------------------------------

test('host steps: Docker starts at boot; Podman gets its socket and podman-restart', () => {
  const docker = hostSteps(full({ engine: 'docker' }));
  assert.match(docker, /^sudo systemctl enable --now docker$/m);
  assert.ok(!docker.includes('nvidia'), 'no NVIDIA lines unless asked');
  const podman = hostSteps(full({ engine: 'podman' }));
  assert.match(podman, /^sudo systemctl enable --now podman\.socket$/m);
  assert.match(podman, /^sudo systemctl enable podman-restart\.service$/m);
  assert.ok(podman.includes(`echo '${RUNTIME_DIR_TMPFILES}' | sudo tee /etc/tmpfiles.d/quasar.conf`));
  assert.match(podman, /^sudo systemd-tmpfiles --create \/etc\/tmpfiles\.d\/quasar\.conf$/m);
  assert.ok(!podman.includes('nvidia'));
  // Docker recreates a missing bind source itself.
  assert.ok(!docker.includes('tmpfiles'));
});

test('the runtime directory line is the one deploy/prepare-host.sh writes for rootful', () => {
  const prep = readFileSync(fileURLToPath(new URL('../../../deploy/prepare-host.sh', import.meta.url)), 'utf8');
  assert.ok(prep.includes(RUNTIME_DIR_TMPFILES));
});

test('host steps on NVIDIA: Docker gets the toolkit runtime, Podman a CDI specification', () => {
  const docker = hostSteps(full({ engine: 'docker', nvidia: true }));
  assert.match(docker, /^sudo nvidia-ctk runtime configure --runtime=docker$/m);
  assert.match(docker, /^sudo systemctl restart docker$/m);
  assert.ok(!docker.includes('cdi generate'));
  const podman = hostSteps(full({ engine: 'podman', nvidia: true }));
  assert.match(podman, /^sudo nvidia-ctk cdi generate --output=\/etc\/cdi\/nvidia\.yaml$/m);
  assert.ok(!podman.includes('runtime configure'));
  // A control-only machine has no GPU work, whatever the checkbox says.
  assert.ok(!hostSteps(full({ engine: 'podman', role: 'control-only', nvidia: true })).includes('nvidia-ctk'));
});

test('host steps on NVIDIA under SELinux (Fedora): the boolean and the one nested-sandbox rule', () => {
  for (const engine of ENGINES) {
    const fedora = hostSteps(full({ engine, nvidia: true, platform: 'fedora' }));
    assert.match(fedora, /^sudo setsebool -P container_use_xserver_devices on$/m, engine);
    assert.ok(fedora.includes(NESTED_GPU_CIL), engine);
    assert.match(fedora, /^sudo semodule -i quasar-nested-gpu\.cil$/m, engine);
    assert.ok(!hostSteps(full({ engine, nvidia: true, platform: 'ubuntu' })).includes('setsebool'), `${engine} on Ubuntu`);
  }
});

test('the nested-sandbox rule is the one deploy/prepare-host.sh writes', () => {
  const prep = readFileSync(fileURLToPath(new URL('../../../deploy/prepare-host.sh', import.meta.url)), 'utf8');
  assert.ok(prep.includes(NESTED_GPU_CIL));
});

test('QUASAR_IMAGE_NAMESPACE / QUASAR_IMAGE_TAG override the defaults', () => {
  const r = spawnSync(process.execPath, ['-e', `
    import('./stack-template.js').then(({ REGISTRY_NS, CHANNEL_TAG }) => {
      process.stdout.write(JSON.stringify({ REGISTRY_NS, CHANNEL_TAG }));
    });
  `], {
    cwd: fileURLToPath(new URL('.', import.meta.url)),
    encoding: 'utf8',
    env: { ...process.env, QUASAR_IMAGE_NAMESPACE: 'registry.test/ns', QUASAR_IMAGE_TAG: 'edge-test' },
  });
  assert.equal(r.status, 0, r.stderr);
  const { REGISTRY_NS: ns, CHANNEL_TAG: tag } = JSON.parse(r.stdout);
  assert.equal(ns, 'registry.test/ns');
  assert.equal(tag, 'edge-test');
  // Defaults are unchanged when the env vars are absent.
  assert.equal(REGISTRY_NS, 'ghcr.io/accreleus/quasar');
  assert.equal(CHANNEL_TAG, 'latest');
});

// --- the script against a fake engine ----------------------------------------

test('a control port another install already answers on stops the script before the seed', () => {
  const r = runFake(full({ role: 'control-only' }), { portTaken: true });
  assert.notEqual(r.status, 0);
  assert.match(r.stderr, /already answers on port 8080/);
  assert.ok(!/run -d --name quasar-seed/.test(String(r.calls)), 'no seed started');
});

test('rootful Podman with podman-restart.service off stops before anything is pulled, naming the command', () => {
  const r = runScript(generate(full({ engine: 'podman', role: 'control-only' })).script, { engine: fakeEngineDir({ restartOff: true }) });
  assert.notEqual(r.status, 0);
  assert.match(r.stderr, /sudo systemctl enable podman-restart\.service/);
  assert.ok(!/podman pull/.test(r.calls), 'nothing pulled');
});

test('rootful Podman with nothing making /run/quasar-agent at boot stops before anything is pulled', () => {
  const r = runScript(generate(full({ engine: 'podman', role: 'control-only' })).script, { engine: fakeEngineDir({ runDirOff: true }) });
  assert.notEqual(r.status, 0);
  assert.match(r.stderr, /\/etc\/tmpfiles\.d\/quasar\.conf/);
  assert.ok(!/podman pull/.test(r.calls), 'nothing pulled');
});

test('rootful Podman installs end to end against the fake engine', () => {
  const r = runScript(generate(full({ engine: 'podman', role: 'control-only' })).script, { engine: fakeEngineDir() });
  assert.equal(r.status, 0, r.stdout + r.stderr);
  assert.match(r.calls, /systemctl start quasar-seed/);
});

test('a Docker rootless engine is refused by the rootful script, pointing at the rootless page', () => {
  const script = generate(full({ engine: 'docker' })).script;
  assert.ok(script.includes(ROOTLESS_DOCS_URL));
});

// --- Podman: the Quadlet unit ------------------------------------------------

test('the Quadlet unit mounts /var/run/docker.sock and uses the quasar-machine volume name', () => {
  for (const mode of MODES) {
    const unit = quadletUnit(full({ engine: 'podman', mode }));
    assert.match(unit, /Volume=%t\/podman\/podman\.sock:\/var\/run\/docker\.sock/, mode);
    assert.match(unit, /Volume=quasar-machine:\/var\/lib\/quasar-machine:ro/, mode);
    assert.ok(!unit.includes('/run/podman/podman.sock'), `${mode}: must not use the mockup's bugged path`);
  }
});

test('a rootful Quadlet unit targets multi-user.target, a rootless one default.target', () => {
  assert.match(quadletUnit(full({ engine: 'podman', mode: 'rootful' })), /WantedBy=multi-user\.target/);
  assert.match(quadletUnit(full({ engine: 'podman', mode: 'rootless' })), /WantedBy=default\.target/);
});

test("the Quadlet unit carries the operator's database password as a Secret=, never inline", () => {
  const a = full({ engine: 'podman', database: 'external', dbHost: 'db.example.internal' });
  const unit = quadletUnit(a);
  assert.match(unit, /Secret=quasar-db-password,type=env,target=QUASAR_DATABASE_PASSWORD/);
  assert.ok(!unit.includes('QUASAR_DATABASE_PASSWORD='));
});

test('the Podman script writes and starts the same unit shape, with resolved digests', () => {
  const out = generate(full({ engine: 'podman', role: 'control-only' }));
  assert.match(out.script, /podman pull -q/);
  assert.match(out.script, /quasar-seed\.container/);
  assert.match(out.script, /systemctl daemon-reload/);
  assert.match(out.script, /systemctl start quasar-seed/);
});

test('podman -dryrun accepts the generated unit, when quadlet is available', () => {
  const quadlet = '/usr/libexec/podman/quadlet';
  if (!existsSync(quadlet)) return; // not installed here: nothing to check
  const unit = quadletUnit(full({ engine: 'podman' }));
  const tmp = spawnSync('mktemp', ['-d']).stdout.toString().trim();
  const path = `${tmp}/quasar-seed.container`;
  writeFileSync(path, unit);
  const r = spawnSync(quadlet, ['-dryrun', '-no-kmsg-log', tmp], { encoding: 'utf8' });
  assert.equal(r.status, 0, r.stdout + r.stderr);
});

test('generate() offers podmanRun only for a rootful Podman profile it supports, never Docker', () => {
  assert.equal(generate(full({ engine: 'docker', mode: 'rootful' })).podmanRun, null);
  assert.equal(generate(full({ platform: 'unraid', engine: 'podman', mode: 'rootful' })).podmanRun, null);
  assert.match(generate(full({ engine: 'podman', mode: 'rootful' })).podmanRun, /^sudo podman run -d --name quasar-seed/);
});

test('podmanRunSeed mounts the same in-container path as the Quadlet unit, never the mockup bug', () => {
  for (const mode of MODES) {
    const cmd = podmanRunSeed(full({ engine: 'podman', mode }));
    assert.match(cmd, /:\/var\/run\/docker\.sock/);
    assert.ok(!cmd.includes('/run/podman/podman.sock:/run/podman/podman.sock'));
  }
});

test('podmanRunSeed uses sudo only when rootful', () => {
  assert.match(podmanRunSeed(full({ engine: 'podman', mode: 'rootful' })), /^sudo podman run/);
  assert.match(podmanRunSeed(full({ engine: 'podman', mode: 'rootless' })), /^podman run/);
});

/** Runs a generated script against the hardened fake engine (test-harness.js). */
function runFake(answers, { legacy = false, existing = false, portTaken = false, extraEnv = {} } = {}) {
  return runScript(generate(answers).script, { engine: fakeEngineDir({ legacy, existing, portTaken }), extraEnv });
}

test('the script refuses a host still running a stack made from the Compose files', () => {
  const r = runFake(full({ role: 'control-only' }), { legacy: true });
  assert.equal(r.status, 1, r.stdout + r.stderr);
  assert.match(r.stderr, /made from the Compose files: quasar-control-plane/);
  assert.ok(!/^docker run /m.test(r.calls), 'nothing may start');
});

test('the script refuses a machine that is already installed', () => {
  const r = runFake(full({ role: 'control-only' }), { existing: true });
  assert.equal(r.status, 1);
  assert.match(r.stderr, /already installed/);
  assert.ok(!/^docker run /m.test(r.calls));
});

test('the script pins every image to its digest and starts the seed with them', () => {
  const r = runFake(full({ role: 'control-only' }));
  assert.equal(r.status, 0, r.stdout + r.stderr);
  const run = r.calls.split('\n').find((l) => l.startsWith('docker run '));
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
  const run = withPw.calls.split('\n').find((l) => l.startsWith('docker run '));
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

// --- the safety net --------------------------------------------------------

/**
 * The check that would have caught the accident this file's header warns
 * about. It runs last (node:test runs a file's tests in declared order) and
 * looks for a REAL docker/podman on the machine running the suite — found by
 * absolute path, never through the fake PATH the tests above build — then
 * asserts no `quasar-*` container exists there. If every test above stayed
 * inside the fake engine, this always passes trivially, including when no
 * real engine is reachable at all (the sanctioned case: the node:22
 * container this suite is meant to run in has neither docker nor podman).
 */
test('no generated script ever reaches a real container engine', () => {
  for (const bin of ['/usr/bin/docker', '/usr/local/bin/docker', '/usr/bin/podman', '/usr/local/bin/podman']) {
    const probe = spawnSync(bin, ['ps', '-a', '--format', '{{.Names}}'], { encoding: 'utf8' });
    if (probe.error || probe.status !== 0) continue; // not installed, or no socket reachable: nothing to check
    const names = probe.stdout.split('\n').filter(Boolean);
    const leaked = names.find((n) => n.startsWith('quasar-'));
    assert.equal(leaked, undefined, `a REAL ${bin} container named "${leaked}" exists — a generated script escaped the fake engine`);
  }
});
