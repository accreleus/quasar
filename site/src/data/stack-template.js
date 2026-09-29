/**
 * What the quick start generates: the seed, for one machine.
 *
 * An owned install declares exactly one container per machine, the seed. The
 * recovery actor it creates generates every secret and creates Postgres, the
 * control plane and the node agent, so nothing here writes a Compose stack of
 * Quasar services, an .env of secrets, or runs `openssl rand`. The seed's inputs
 * are docs/configuration.md "Seed" / "Recovery actor"; the GPU-host stack has the
 * same shape Admin -> Fleet -> Add host writes (web/src/lib/addHost.ts).
 *
 * Images: a static page cannot know the current digests, and the seed refuses a
 * tag for the agent and control-plane images. So the script resolves the edge
 * channel's `o2-develop` tags to digests on the host at run time, and the stack
 * pane carries placeholders plus a one-line command that prints the three pins.
 *
 * Engine and mode (RH07-14, #406). Which (platform, engine, mode) combinations
 * are offered at all is `testdata/engine-profiles/profiles.json` via
 * `engine-profiles.js` — the same table the node agent's `runtime_engine` check
 * and the enrollment script read, so the badge the quick start shows is the
 * verdict the host's readiness card will give. An unsupported combination
 * generates no install artifacts; the UI is the thing that blocks it earlier.
 *
 * Host preparation (`deploy/prepare-host.sh`) is a separate root step ahead of
 * starting the seed: it creates the `quasar` account, udev rules, kernel
 * settings and (on Podman) the boot-restart wiring. It writes under /etc, so
 * it is skipped entirely on Unraid (a ramdisk there) — Unraid keeps its own
 * self-contained script, unchanged. Everywhere else, `generate()` also returns
 * `prep` (the curl/checksum/`sudo sh prepare-host.sh` block) and, for Podman,
 * `quadlet` (the unit for on-screen reference; the script itself resolves
 * digests and writes/starts the same unit).
 */
import { proxyConfig } from './proxy-configs.js';
import { platform } from './platforms.js';
import { profileFor } from './engine-profiles.js';

/**
 * `prepare-host-source.js` is Node-only (it reads `deploy/prepare-host.sh`
 * with `node:fs`) and must never be imported from here: this module is also
 * bundled into the quick start's browser `<script>`, and Vite externalizes
 * `node:fs` to a stub that throws on property access for that target — a
 * static import used to crash the whole wizard silently before any event
 * listener attached. `QuickStart.astro`'s frontmatter (SSR, safe) reads the
 * real checksum and calls `setPrepareHostSha256()`; the test file does the
 * same for its own assertions. Until called, `prepText()` falls back to a
 * placeholder rather than a stale or wrong digest.
 */
let prepareHostSha256 = '<checksum unavailable — call setPrepareHostSha256()>';
export function setPrepareHostSha256(value) {
  prepareHostSha256 = value;
}
export const PREPARE_HOST_URL = 'https://accreleus.github.io/quasar/prepare-host.sh';

// `process` itself is a Node global, undefined in the browser this module is
// also bundled for (the quick start's client <script>) — `typeof` is the one
// operator that can safely probe an undeclared identifier without throwing.
const env = typeof process !== 'undefined' ? process.env : {};
export const REGISTRY_NS = env.QUASAR_IMAGE_NAMESPACE || 'ghcr.io/accreleus/quasar';
/** The edge channel's tag family for builds that ship owned installs (#365). */
export const CHANNEL_TAG = env.QUASAR_IMAGE_TAG || 'o2-develop';
export const IMAGE_NAMES = {
  seed: 'quasar-recovery',
  control: 'quasar-control-plane',
  agent: 'quasar-node-agent',
};

export const ROLES = {
  combined: { label: 'Combined host', seedRole: 'combined', agent: true, control: true },
  'control-only': { label: 'Control-only host', seedRole: 'control-only', agent: false, control: true },
  gpu: { label: 'GPU host', seedRole: 'gpu', agent: true, control: false },
};

export const ENGINES = ['docker', 'podman'];
export const MODES = ['rootful', 'rootless'];

export const DEFAULTS = {
  platform: 'fedora', // see platforms.js
  role: 'combined', // see ROLES
  engine: 'docker', // 'docker' | 'podman'
  mode: 'rootful', // 'rootful' | 'rootless'
  console: false,
  kernelLog: false,
  lowPorts: false, // forced on below 1024 on a rootless engine regardless of this flag
  basePath: '/var/lib/quasar',
  separateSaves: false,
  savesPath: '',
  owner: 'dedicated', // 'dedicated' | 'custom'
  uid: 1000,
  gid: 1000,
  access: 'self-signed', // 'self-signed' | 'proxy'
  publicHost: '',
  tlsHosts: '',
  publicUrl: '',
  proxy: 'caddy',
  trustedProxies: '',
  database: 'owned', // 'owned' | 'external'
  dbHost: '',
  dbPort: 5432,
  dbUser: 'quasar',
  dbName: 'quasar',
  dbSslmode: 'require',
  controlPort: 8080,
  tlsPort: 8443,
};

export function role(id) {
  return ROLES[id] ?? ROLES.combined;
}

/** Whether generate() should produce install artifacts for this platform/engine/mode at all. */
export function supportedProfile(a) {
  return profileFor(a.platform, a.engine, a.mode).status !== 'unsupported';
}

/** A digest placeholder in the one shape the seed accepts. */
export function placeholderImage(name) {
  return `${REGISTRY_NS}/${name}@sha256:<digest>`;
}

const PLACEHOLDERS = {
  seed: placeholderImage(IMAGE_NAMES.seed),
  control: placeholderImage(IMAGE_NAMES.control),
  agent: placeholderImage(IMAGE_NAMES.agent),
};

/** Per-user home directories. This is the one that grows. */
export function homePath(a) {
  if (a.separateSaves && a.savesPath.trim()) return a.savesPath.trim().replace(/\/+$/, '');
  return `${a.basePath.replace(/\/+$/, '')}/homes`;
}

/** Always written out: the recovery actor and the agent default it differently. */
export function templatePath(a) {
  const home = homePath(a);
  return `${home.slice(0, home.lastIndexOf('/'))}/templates`;
}

/**
 * Who owns save data, as {uid, gid}: QUASAR_APP_PUID/PGID, which the game
 * containers drop to. It does not change what the platform services run as.
 */
export function appUser(a) {
  const p = platform(a.platform);
  if (a.owner === 'custom') {
    return { uid: Number(a.uid), gid: Number(a.gid) };
  }
  return { uid: p.defaultUid, gid: p.defaultGid };
}

/** The app-container user is passed only when it differs from the image's (1000). */
function appUserInputs(a) {
  const { uid, gid } = appUser(a);
  if (uid === 1000 && gid === 1000) return [];
  return [
    ['QUASAR_APP_PUID', String(uid)],
    ['QUASAR_APP_PGID', String(gid)],
  ];
}

const ENROLLMENT_PLACEHOLDER = 'qenr1.<paste the string from Admin, Fleet, Add host>';

/**
 * The seed's inputs for these answers, in order, as [name, value]. `images` are
 * the three references (placeholders, or the script's resolved variables).
 * The operator's database password is never a value here: it is
 * `${QUASAR_DATABASE_PASSWORD}`, interpolated from the stack's own .env, or the
 * script's environment.
 */
export function seedInputs(a, images = PLACEHOLDERS) {
  const r = role(a.role);
  const out = [['QUASAR_ROLE', r.seedRole]];
  if (r.seedRole === 'gpu') out.push(['QUASAR_ENROLLMENT', ENROLLMENT_PLACEHOLDER]);
  if (r.control) {
    out.push(['QUASAR_PUBLIC_HOST', a.publicHost.trim() || '<the name or LAN address you browse to>']);
    if (a.tlsHosts.trim()) out.push(['QUASAR_TLS_HOSTS', a.tlsHosts.trim()]);
    if (a.access === 'proxy' && a.trustedProxies.trim()) {
      out.push(['QUASAR_TRUSTED_PROXIES', a.trustedProxies.trim()]);
    }
    if (Number(a.controlPort) !== 8080) out.push(['QUASAR_HTTP_PORT', String(a.controlPort)]);
    if (Number(a.tlsPort) !== 8443) out.push(['QUASAR_TLS_PORT', String(a.tlsPort)]);
  }
  if (r.agent) {
    out.push(['QUASAR_HOME_ROOT', homePath(a)]);
    out.push(['QUASAR_TEMPLATE_ROOT', templatePath(a)]);
    out.push(...appUserInputs(a));
  }
  if (r.control) out.push(['QUASAR_CONTROL_PLANE_IMAGE', images.control]);
  out.push(['QUASAR_AGENT_IMAGE', images.agent]);
  if (r.control && a.database === 'external') {
    out.push(['QUASAR_DATABASE_HOST', a.dbHost.trim() || '<your database host>']);
    if (Number(a.dbPort) !== 5432) out.push(['QUASAR_DATABASE_PORT', String(a.dbPort)]);
    if (a.dbUser.trim() && a.dbUser.trim() !== 'quasar') out.push(['QUASAR_DATABASE_USER', a.dbUser.trim()]);
    if (a.dbName.trim() && a.dbName.trim() !== 'quasar') out.push(['QUASAR_DATABASE_NAME', a.dbName.trim()]);
    out.push(['QUASAR_DATABASE_SSLMODE', a.dbSslmode]);
    out.push(['QUASAR_DATABASE_PASSWORD', '${QUASAR_DATABASE_PASSWORD}']);
  }
  return out;
}

/**
 * The seed alone, as a one-service stack for Dockge or Arcane. The volume's own
 * `name:` matters: without it Compose names it `<project>_quasar-machine`, which
 * the seed refuses (`seed-self-invalid`).
 */
export function seedStack(a, images = PLACEHOLDERS) {
  // Double-quoted (JSON is valid YAML): a port or a node name stays a string.
  const q = (v) => JSON.stringify(v);
  return [
    'services:',
    '  quasar-seed:',
    '    container_name: quasar-seed',
    `    image: ${q(images.seed)}`,
    '    command: seed',
    '    restart: unless-stopped',
    '    security_opt: [label=disable]',
    '    environment:',
    ...seedInputs(a, images).map(([k, v]) => `      ${k}: ${q(v)}`),
    '    volumes:',
    '      - /var/run/docker.sock:/var/run/docker.sock',
    '      - quasar-machine:/var/lib/quasar-machine:ro',
    'volumes:',
    '  quasar-machine:',
    '    name: quasar-machine',
    '',
  ].join('\n');
}

/** The stack's .env, only for the operator's own database: the one secret it holds. */
export function stackEnv(a) {
  if (!role(a.role).control || a.database !== 'external') return null;
  return [
    '# Your own database password. The seed copies it into machine state at the first',
    '# install; after that it can be removed from here. Never mounted into a container.',
    'QUASAR_DATABASE_PASSWORD=',
    '',
  ].join('\n');
}

/** Prints the three references to paste into the stack, resolved on the host. */
export function pinsCommand(a) {
  const names = role(a.role).control
    ? [IMAGE_NAMES.seed, IMAGE_NAMES.control, IMAGE_NAMES.agent]
    : [IMAGE_NAMES.seed, IMAGE_NAMES.agent];
  const engineCli = a.engine === 'podman' ? 'podman' : 'docker';
  return `for i in ${names.join(' ')}; do ${engineCli} pull -q ${REGISTRY_NS}/$i:${CHANNEL_TAG} >/dev/null && ${engineCli} image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' ${REGISTRY_NS}/$i:${CHANNEL_TAG} | grep -m1 "^${REGISTRY_NS}/$i@"; done`;
}

function shellQuote(value) {
  return "'" + String(value).replaceAll("'", "'\"'\"'") + "'";
}

// --- host preparation -------------------------------------------------------

/**
 * Whether the low-ports flag is in effect: either the operator turned it on,
 * or it must be, because a chosen port is below 1024 on a rootless engine
 * (which cannot bind one without it).
 */
export function effectiveLowPorts(a) {
  return Boolean(a.lowPorts) || (a.mode === 'rootless' && (Number(a.controlPort) < 1024 || Number(a.tlsPort) < 1024));
}

/** The lowest configured port under 1024, or a sane default if the toggle was set by hand. */
function unprivilegedPortStart(a) {
  const low = [Number(a.controlPort), Number(a.tlsPort)].filter((n) => Number.isFinite(n) && n > 0 && n < 1024);
  return low.length ? Math.min(...low) : 80;
}

/** The flags `prepare-host.sh` needs for these answers, as a flat token list. */
export function prepFlags(a) {
  const r = role(a.role);
  const tokens = ['--mode', a.mode, '--engine', a.engine];
  if (a.mode === 'rootless' && r.agent) {
    tokens.push('--homes', homePath(a));
    tokens.push('--templates', templatePath(a));
  }
  if (a.console) tokens.push('--console');
  if (a.kernelLog) tokens.push('--allow-kernel-log');
  if (effectiveLowPorts(a)) tokens.push('--unprivileged-port-start', String(unprivilegedPortStart(a)));
  return tokens;
}

/**
 * The prep block: fetch `prepare-host.sh` from the docs site (the first
 * machine has no control plane yet to fetch it from), verify it, then run it
 * as root. This is the one step that needs root on every engine and mode —
 * it is what host preparation is for.
 */
export function prepText(a) {
  const flags = prepFlags(a)
    .map((t) => (t.startsWith('--') ? t : shellQuote(t)))
    .join(' ');
  return `curl -fsSL -o prepare-host.sh ${shellQuote(PREPARE_HOST_URL)}
echo ${shellQuote(`${prepareHostSha256}  prepare-host.sh`)} | sha256sum -c
sudo sh prepare-host.sh ${flags}`;
}

/**
 * The install scripts' host-preparation check. Preparation is the quick start's own
 * step 1, run once as root; the install script never runs it, because on a rootless
 * engine the script runs as the quasar account, which has no sudo (least privilege).
 * It only checks what preparation always leaves behind, and names step 1 if not.
 */
export function prepCheck(a) {
  const files = ['/etc/udev/rules.d/70-quasar.rules', '/etc/sysctl.d/99-quasar.conf'];
  if (a.mode === 'rootless') files.push('/etc/tmpfiles.d/quasar.conf');
  const lines = [
    'echo "==> Checking host preparation"',
    'unprepared=0',
    '# QUASAR_PREP_ROOT is for tests only: the root the checked files live under.',
    'pr="${QUASAR_PREP_ROOT:-}"',
    `for f in ${files.join(' ')}; do`,
    '  [ -e "$pr$f" ] || { echo "Missing $f." >&2; unprepared=1; }',
    'done',
  ];
  if (a.mode === 'rootless') {
    lines.push(
      'grep -q "^$(id -un):" "$pr/etc/subuid" 2>/dev/null || { echo "No subordinate IDs for $(id -un) in /etc/subuid." >&2; unprepared=1; }',
      '[ -e "$pr/var/lib/systemd/linger/$(id -un)" ] || { echo "Lingering is off for $(id -un)." >&2; unprepared=1; }',
    );
  }
  lines.push(
    'if [ "$unprepared" != 0 ]; then',
    '  echo "This host is not prepared for Quasar. Run step 1 (Prepare the machine) as root, then run this again." >&2',
    '  exit 1',
    'fi',
  );
  return lines.join('\n');
}

// --- the Podman Quadlet unit -------------------------------------------------

/**
 * The seed's inputs as Quadlet `Environment=` lines: the operator's database
 * password never appears here, even in its `${VAR}` form — a Quadlet unit has
 * no shell, so it is instead a `Secret=`, resolved through Podman's own secret
 * store, which the script creates alongside the unit.
 */
function quadletEnvironmentLines(a, images) {
  return seedInputs(a, images)
    .filter(([k]) => k !== 'QUASAR_DATABASE_PASSWORD')
    .map(([k, v]) => `Environment=${k}=${JSON.stringify(v)}`);
}

/**
 * The Quadlet unit, for on-screen reference. The socket is mapped at
 * `%t/podman/podman.sock` (the rootless and rootful runtime directory alike)
 * to `/var/run/docker.sock`, the one in-container path the seed accepts
 * (ADR 0007's RH07 amendment) — never the mockup's `/run/podman/podman.sock`,
 * which the seed refuses. The script itself writes and starts this same unit
 * with resolved image digests, not placeholders.
 */
export function quadletUnit(a, images = PLACEHOLDERS) {
  const external = role(a.role).control && a.database === 'external';
  const rootful = a.mode === 'rootful';
  const lines = [
    '[Unit]',
    'Description=Quasar seed',
    'Wants=network-online.target',
    'Requires=podman.socket',
    'After=network-online.target podman.socket',
    '',
    '[Container]',
    'ContainerName=quasar-seed',
    `Image=${images.seed}`,
    'Exec=seed',
    'SecurityLabelDisable=true',
    'Volume=%t/podman/podman.sock:/var/run/docker.sock',
    'Volume=quasar-machine:/var/lib/quasar-machine:ro',
    ...quadletEnvironmentLines(a, images),
  ];
  if (external) lines.push('Secret=quasar-db-password,type=env,target=QUASAR_DATABASE_PASSWORD');
  lines.push('', '[Service]', 'Restart=always', '', '[Install]', `WantedBy=${rootful ? 'multi-user.target' : 'default.target'}`, '');
  return lines.join('\n');
}

/**
 * The seed as a bare `podman run`, for "only trying it out" — not the
 * installed path (the Quadlet unit is), so it is offered behind a closed
 * disclosure in the Result step. Same socket mapping as the unit
 * (`/var/run/docker.sock` in-container, per ADR 0007's RH07 amendment).
 */
export function podmanRunSeed(a, images = PLACEHOLDERS) {
  const rootful = a.mode === 'rootful';
  const sudo = rootful ? 'sudo ' : '';
  const sock = rootful ? '/run/podman/podman.sock' : '${XDG_RUNTIME_DIR:-/run/user/$(id -u)}/podman/podman.sock';
  return `${sudo}podman run -d --name quasar-seed --restart unless-stopped \\
  --security-opt label=disable \\
  -v "${sock}:/var/run/docker.sock" \\
  -v quasar-machine:/var/lib/quasar-machine:ro \\
${envArgsFor(a, images)}
  ${images.seed} seed`;
}

// --- the scripts, per engine and mode ---------------------------------------

const EXISTING_INSTALL_CHECK = (cli, sudo) => `for name in quasar-seed quasar-recovery; do
  if ${sudo}${cli} container inspect "$name" >/dev/null 2>&1; then
    echo "Quasar is already installed on this machine ($name exists). Check it with:" >&2
    echo "  ${sudo}${cli} exec quasar-recovery quasar-recovery status" >&2
    exit 1
  fi
done`;

function readyBlock(a, r, cli, sudo, statusCmd) {
  return `echo "==> Waiting for Quasar"
ready=0
for _ in $(seq 1 120); do
  if ${statusCmd} >/dev/null 2>&1${r.control ? ` && curl -fsS http://localhost:${a.controlPort}/health >/dev/null 2>&1` : ''}; then
    ready=1
    break
  fi
  sleep 5
done
if [ "$ready" != 1 ]; then
  echo "Quasar did not report ready within ten minutes. What the seed and the recovery actor say:" >&2
  echo "  ${sudo}${cli} logs quasar-seed 2>/dev/null || true" >&2
  echo "  ${statusCmd}" >&2
  exit 1
fi`;
}

function doneBlock(a, r, host, recoveryExecPrefix) {
  return r.control
    ? `echo "Quasar is running. Open https://${host}:${a.tlsPort} and accept the certificate once."
echo "Claim the first admin with the one-time setup token:"
echo "  ${recoveryExecPrefix} cat /run/quasar/setup-token"`
    : `echo "The recovery actor is running and will install the node agent."`;
}

function preflightBlock({ tools, checkCmd, checkFailMsg, r, external }) {
  return `echo "==> Host preflight"
preflight_failed=0
for tool in ${tools.join(' ')}; do
  if ! command -v "$tool" >/dev/null; then
    echo "Install required tool: $tool" >&2
    preflight_failed=1
  fi
done
${checkCmd ? `if ${checkCmd}; then
  echo "${checkFailMsg}" >&2
  preflight_failed=1
fi
` : ''}${r.agent ? `if [ ! -d /dev/dri ]; then
  echo "GPU devices are unavailable under /dev/dri. Check the host graphics driver." >&2
  preflight_failed=1
fi
` : ''}${external ? `if [ -z "\${QUASAR_DATABASE_PASSWORD:-}" ]; then
  echo "Set QUASAR_DATABASE_PASSWORD in this shell's environment (your database's password) and run again." >&2
  preflight_failed=1
fi
` : ''}if [ "$preflight_failed" != 0 ]; then
  echo "Correct the preflight problems above before starting Quasar." >&2
  exit 1
fi`;
}

function imagesBlock(cli, sudo, r) {
  return `echo "==> Images"
# The edge channel's builds that ship owned installs, pinned to their digests here:
# the seed refuses a tag for the images it installs.
resolve() {
  local ref="${REGISTRY_NS}/$1:${CHANNEL_TAG}" pinned
  ${sudo}${cli} pull -q "$ref" >/dev/null || { echo "Could not pull $ref." >&2; return 1; }
  pinned=$(${sudo}${cli} image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' "$ref" | grep -m1 "^${REGISTRY_NS}/$1@sha256:" || true)
  [ -n "$pinned" ] || { echo "$ref has no registry digest." >&2; return 1; }
  printf '%s\\n' "$pinned"
}
seed_image=$(resolve ${IMAGE_NAMES.seed})
${r.control ? `control_image=$(resolve ${IMAGE_NAMES.control})\n` : ''}agent_image=$(resolve ${IMAGE_NAMES.agent})`;
}

function envArgsFor(a, vars) {
  return seedInputs(a, vars)
    .map(([k, v]) => {
      if (k === 'QUASAR_DATABASE_PASSWORD') return '  -e QUASAR_DATABASE_PASSWORD \\';
      const value = v.startsWith('$') && !v.startsWith('${') ? `"${v}"` : shellQuote(v);
      return `  -e ${k}=${value} \\`;
    })
    .join('\n');
}

/**
 * Docker rootful: today's script (D4), minus the sysctl/modprobe lines that
 * host preparation now does, plus the prep step and an engine/mode check.
 * `install -d` of homes stays: rootful preparation skips creating them
 * (`prepare-host.sh`'s `data_root`), same as before this change.
 */
function dockerRootfulScript(a, r, p) {
  const { uid, gid } = appUser(a);
  const external = r.control && a.database === 'external';
  const vars = { seed: '$seed_image', control: '$control_image', agent: '$agent_image' };
  const host = a.publicHost.trim() || '<this-host>';

  return `#!/usr/bin/env bash
# Quasar quick start: a ${r.label.toLowerCase()} on ${p.label}, Docker rootful.
# Generated in your browser; nothing was sent anywhere. Read it before you run it.
#
# It checks the host was prepared, then starts ONE container, the seed. The seed creates
# Quasar's recovery actor, which generates every secret and creates the rest.
# Nothing here writes a Compose file or an .env.
set -euo pipefail

${preflightBlock({
  tools: ['docker', 'curl'],
  checkCmd: `command -v docker >/dev/null && ! docker info >/dev/null 2>&1`,
  checkFailMsg: 'Docker is unavailable or this user cannot access its socket.',
  r,
  external,
})}
if command -v docker >/dev/null && docker info --format '{{json .SecurityOptions}}' 2>/dev/null | grep -q rootless; then
  echo "This is Docker rootless, but this script installs rootful. Regenerate the quick start with Rootless selected." >&2
  exit 1
fi

echo "==> Existing installs"
# A stack made from the Compose files would be an owner conflict the recovery actor
# never acts on, and it holds the ports. Stop it first, keeping its volumes.
legacy=""
for svc in quasar-postgres quasar-control-plane quasar-node-agent quasar-updater; do
  found=$(docker ps -aq --filter "label=com.docker.compose.service=$svc" 2>/dev/null | head -n 1 || true)
  [ -z "$found" ] || legacy="$legacy $svc"
done
if [ -n "$legacy" ]; then
  echo "This host still runs a Quasar stack made from the Compose files:$legacy." >&2
  echo "To keep its accounts and library, follow https://accreleus.github.io/quasar/install/move-existing/" >&2
  echo "instead of this script: it dumps the old database before anything is stopped." >&2
  echo "To start afresh, remove its containers without deleting its volumes" >&2
  echo "(docker compose -f <its directory>/docker-compose.yml down) and run this again." >&2
  exit 1
fi
${EXISTING_INSTALL_CHECK('docker', '')}

${prepCheck(a)}

${imagesBlock('docker', '', r)}
${r.agent ? `
echo "==> Directories"
sudo install -d -m 0755 -o ${uid} -g ${gid} ${shellQuote(homePath(a))}
sudo install -d -m 0755 -o ${uid} -g ${gid} ${shellQuote(templatePath(a))}
` : ''}
echo "==> Starting the seed"
docker run -d --name quasar-seed --restart unless-stopped \\
  --security-opt label=disable \\
  -v /var/run/docker.sock:/var/run/docker.sock \\
  -v quasar-machine:/var/lib/quasar-machine:ro \\
${envArgsFor(a, vars)}
  "$seed_image" seed >/dev/null

${readyBlock(a, r, 'docker', '', 'docker exec quasar-recovery quasar-recovery status')}

echo
${doneBlock(a, r, host, 'docker exec quasar-control-plane')}
`;
}

/**
 * Docker rootless: no root at any point except the prep line. The seed's
 * socket mount is the user's own rootless socket, resolved the way the
 * daemon documents it (`$XDG_RUNTIME_DIR/docker.sock`, defaulting to
 * `/run/user/$(id -u)`); no directories are created here (`--homes`/
 * `--templates` on `prepare-host.sh` do that, owned correctly already).
 */
function dockerRootlessScript(a, r, p) {
  const external = r.control && a.database === 'external';
  const vars = { seed: '$seed_image', control: '$control_image', agent: '$agent_image' };
  const host = a.publicHost.trim() || '<this-host>';
  const sockExpr = '${XDG_RUNTIME_DIR:-/run/user/$(id -u)}/docker.sock';

  return `#!/usr/bin/env bash
# Quasar quick start: a ${r.label.toLowerCase()} on ${p.label}, Docker rootless.
# Generated in your browser; nothing was sent anywhere. Read it before you run it.
#
# Runs entirely as the quasar account: the only root step is host preparation,
# which creates that account and its rootless Docker service. Nothing here
# writes a Compose file or an .env.
set -euo pipefail

if [ "$(id -u)" = 0 ]; then
  echo "Run this as the quasar account, not root: rootless Docker is per-user." >&2
  exit 1
fi
export DOCKER_HOST="unix://${sockExpr}"

${preflightBlock({
  tools: ['docker', 'curl'],
  checkCmd: `command -v docker >/dev/null && ! docker info >/dev/null 2>&1`,
  checkFailMsg: 'Docker is unavailable at $DOCKER_HOST. Check the rootless Docker service for this user.',
  r,
  external,
})}

echo "==> Existing installs"
${EXISTING_INSTALL_CHECK('docker', '')}

${prepCheck(a)}

${imagesBlock('docker', '', r)}

echo "==> Starting the seed"
docker run -d --name quasar-seed --restart unless-stopped \\
  --security-opt label=disable \\
  -v "${sockExpr}:/var/run/docker.sock" \\
  -v quasar-machine:/var/lib/quasar-machine:ro \\
${envArgsFor(a, vars)}
  "$seed_image" seed >/dev/null

${readyBlock(a, r, 'docker', '', 'docker exec quasar-recovery quasar-recovery status')}

echo
${doneBlock(a, r, host, 'docker exec quasar-control-plane')}
`;
}

/**
 * Podman, rootful or rootless (D-g/owner decision 3): the copy block is a
 * script. It resolves the three image digests with `podman`, writes the
 * Quadlet unit with them substituted in, then starts it through systemd —
 * the same unit `quadlet` above returns for on-screen reference, with real
 * digests instead of placeholders. `--homes`/`--templates` on `prepare-host.sh`
 * create the directories when rootless; rootful keeps `install -d` here, same
 * as Docker rootful.
 */
function podmanScript(a, r, p) {
  const rootful = a.mode === 'rootful';
  const { uid, gid } = appUser(a);
  const external = r.control && a.database === 'external';
  const sudo = rootful ? 'sudo ' : '';
  const svc = rootful ? 'systemctl' : 'systemctl --user';
  const unitDir = rootful ? '/etc/containers/systemd' : '$HOME/.config/containers/systemd';
  const vars = { seed: '$seed_image', control: '$control_image', agent: '$agent_image' };
  const host = a.publicHost.trim() || '<this-host>';

  const unitLines = [
    '[Unit]',
    'Description=Quasar seed',
    'Wants=network-online.target',
    'Requires=podman.socket',
    'After=network-online.target podman.socket',
    '',
    '[Container]',
    'ContainerName=quasar-seed',
    'Image=$seed_image',
    'Exec=seed',
    'SecurityLabelDisable=true',
    'Volume=%t/podman/podman.sock:/var/run/docker.sock',
    'Volume=quasar-machine:/var/lib/quasar-machine:ro',
    ...quadletEnvironmentLines(a, vars),
  ];
  if (external) unitLines.push('Secret=quasar-db-password,type=env,target=QUASAR_DATABASE_PASSWORD');
  unitLines.push('', '[Service]', 'Restart=always', '', '[Install]', `WantedBy=${rootful ? 'multi-user.target' : 'default.target'}`);

  return `#!/usr/bin/env bash
# Quasar quick start: a ${r.label.toLowerCase()} on ${p.label}, Podman ${rootful ? 'rootful' : 'rootless'}.
# Generated in your browser; nothing was sent anywhere. Read it before you run it.
#
# It checks the host was prepared, resolves the three image digests, then writes and
# starts a Quadlet unit for the seed. Nothing here writes a Compose file.
set -euo pipefail

${rootful ? '' : `if [ "$(id -u)" = 0 ]; then
  echo "Run this as the quasar account, not root: rootless Podman is per-user." >&2
  exit 1
fi
`}${preflightBlock({
  tools: ['podman', 'curl', 'sha256sum'],
  checkCmd: `command -v podman >/dev/null && ! ${sudo}podman info >/dev/null 2>&1`,
  checkFailMsg: 'Podman is unavailable.',
  r,
  external,
})}

echo "==> Existing installs"
${EXISTING_INSTALL_CHECK('podman', sudo)}

${prepCheck(a)}

${imagesBlock('podman', sudo, r)}
${rootful && r.agent ? `
echo "==> Directories"
sudo install -d -m 0755 -o ${uid} -g ${gid} ${shellQuote(homePath(a))}
sudo install -d -m 0755 -o ${uid} -g ${gid} ${shellQuote(templatePath(a))}
` : ''}${external ? `
echo "==> Database secret"
if ! ${sudo}podman secret exists quasar-db-password 2>/dev/null; then
  printf '%s' "$QUASAR_DATABASE_PASSWORD" | ${sudo}podman secret create quasar-db-password - >/dev/null
fi
` : ''}
echo "==> Writing the Quadlet unit"
${sudo}mkdir -p "${unitDir}"
${sudo}tee "${unitDir}/quasar-seed.container" >/dev/null <<'QUASAR_UNIT_HEADER'
# Written by the Quasar quick start.
QUASAR_UNIT_HEADER
cat <<UNIT | ${sudo}tee -a "${unitDir}/quasar-seed.container" >/dev/null
${unitLines.join('\n')}
UNIT

echo "==> Starting the seed"
${sudo}${svc} daemon-reload
${sudo}${svc} start quasar-seed

${readyBlock(a, r, 'podman', sudo, `${sudo}podman exec quasar-recovery quasar-recovery status`)}

echo
${doneBlock(a, r, host, `${sudo}podman exec quasar-control-plane`)}
`;
}

/**
 * Unraid: unchanged (D-h). `prepare-host.sh` writes under /etc, a ramdisk on
 * Unraid, so it is skipped entirely; this script persists the sysctl and the
 * uinput module through `/boot/config/go` itself, exactly as before RH07-14.
 * Unraid's own Docker is always rootful, so `a.engine`/`a.mode` do not apply.
 */
function unraidScript(a, r, p) {
  const { uid, gid } = appUser(a);
  const external = r.control && a.database === 'external';
  const vars = { seed: '$seed_image', control: '$control_image', agent: '$agent_image' };
  const envArgs = envArgsFor(a, vars);
  const host = a.publicHost.trim() || '<this-host>';

  return `#!/usr/bin/env bash
# Quasar quick start: a ${r.label.toLowerCase()} on ${p.label}. Generated in your
# browser; nothing was sent anywhere. Read it before you run it.
#
# It checks the host, prepares it, and starts ONE container, the seed. The seed
# creates Quasar's recovery actor, which generates every secret and creates the
# rest. Nothing here writes a Compose file or an .env.
set -euo pipefail

echo "==> Host preflight"
preflight_failed=0
for tool in docker curl; do
  if ! command -v "$tool" >/dev/null; then
    echo "Install required tool: $tool" >&2
    preflight_failed=1
  fi
done
if command -v docker >/dev/null && ! docker info >/dev/null; then
  echo "Docker is unavailable or this user cannot access its socket." >&2
  preflight_failed=1
fi
${r.agent ? `if [ ! -d /dev/dri ]; then
  echo "GPU devices are unavailable under /dev/dri. Check the host graphics driver." >&2
  preflight_failed=1
fi
` : ''}${external ? `if [ -z "\${QUASAR_DATABASE_PASSWORD:-}" ]; then
  echo "Set QUASAR_DATABASE_PASSWORD in this shell's environment (your database's password) and run again." >&2
  preflight_failed=1
fi
` : ''}if [ "$preflight_failed" != 0 ]; then
  echo "Correct the preflight problems above before starting Quasar." >&2
  exit 1
fi

echo "==> Existing installs"
# A stack made from the Compose files would be an owner conflict the recovery actor
# never acts on, and it holds the ports. Stop it first, keeping its volumes.
legacy=""
for svc in quasar-postgres quasar-control-plane quasar-node-agent quasar-updater; do
  found=$(docker ps -aq --filter "label=com.docker.compose.service=$svc" 2>/dev/null | head -n 1 || true)
  [ -z "$found" ] || legacy="$legacy $svc"
done
if [ -n "$legacy" ]; then
  echo "This host still runs a Quasar stack made from the Compose files:$legacy." >&2
  echo "To keep its accounts and library, follow https://accreleus.github.io/quasar/install/move-existing/" >&2
  echo "instead of this script: it dumps the old database before anything is stopped." >&2
  echo "To start afresh, remove its containers without deleting its volumes" >&2
  echo "(docker compose -f <its directory>/docker-compose.yml down) and run this again." >&2
  exit 1
fi
for name in quasar-seed quasar-recovery; do
  if docker container inspect "$name" >/dev/null 2>&1; then
    echo "Quasar is already installed on this machine ($name exists). Check it with:" >&2
    echo "  docker exec quasar-recovery quasar-recovery status" >&2
    exit 1
  fi
done

echo "==> Images"
# The edge channel's builds that ship owned installs, pinned to their digests here:
# the seed refuses a tag for the images it installs.
resolve() {
  local ref="${REGISTRY_NS}/$1:${CHANNEL_TAG}" pinned
  docker pull -q "$ref" >/dev/null || { echo "Could not pull $ref." >&2; return 1; }
  pinned=$(docker image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' "$ref" | grep -m1 "^${REGISTRY_NS}/$1@sha256:" || true)
  [ -n "$pinned" ] || { echo "$ref has no registry digest." >&2; return 1; }
  printf '%s\\n' "$pinned"
}
seed_image=$(resolve ${IMAGE_NAMES.seed})
${r.control ? `control_image=$(resolve ${IMAGE_NAMES.control})\n` : ''}agent_image=$(resolve ${IMAGE_NAMES.agent})
${r.agent ? `
echo "==> Directories"
${p.sudo}install -d -m 0755 -o ${uid} -g ${gid} ${shellQuote(homePath(a))}
${p.sudo}install -d -m 0755 -o ${uid} -g ${gid} ${shellQuote(templatePath(a))}

echo "==> UDP send buffer"
# libnice never calls setsockopt(SO_SNDBUF), so media sockets inherit the kernel
# default of 208 KB. A keyframe burst at 8 Mbps overflows it, the kernel drops
# the overflow silently, and the bitrate estimator reads that as congestion.
${p.sysctl()}

echo "==> Virtual input"
${p.module()}
[ -c /dev/uinput ] || { echo "Virtual input device /dev/uinput is unavailable after loading uinput" >&2; exit 1; }
` : ''}
echo "==> Starting the seed"
docker run -d --name quasar-seed --restart unless-stopped \\
  --security-opt label=disable \\
  -v /var/run/docker.sock:/var/run/docker.sock \\
  -v quasar-machine:/var/lib/quasar-machine:ro \\
${envArgs}
  "$seed_image" seed >/dev/null

echo "==> Waiting for Quasar"
ready=0
for _ in $(seq 1 120); do
  if docker exec quasar-recovery quasar-recovery status >/dev/null 2>&1${r.control ? ` && curl -fsS http://localhost:${a.controlPort}/health >/dev/null 2>&1` : ''}; then
    ready=1
    break
  fi
  sleep 5
done
if [ "$ready" != 1 ]; then
  echo "Quasar did not report ready within ten minutes. What the seed and the recovery actor say:" >&2
  echo "  docker logs quasar-seed" >&2
  echo "  docker exec quasar-recovery quasar-recovery status" >&2
  exit 1
fi

echo
${r.control ? `echo "Quasar is running. Open https://${host}:${a.tlsPort} and accept the certificate once."
echo "Claim the first admin with the one-time setup token:"
echo "  docker exec quasar-control-plane cat /run/quasar/setup-token"` : `echo "The recovery actor is running and will install the node agent."`}
`;
}

/** The host script: dispatches on platform, then engine and mode. */
function scriptText(a) {
  const r = role(a.role);
  const p = platform(a.platform);
  if (a.platform === 'unraid') return unraidScript(a, r, p);
  if (a.engine === 'podman') return podmanScript(a, r, p);
  return a.mode === 'rootless' ? dockerRootlessScript(a, r, p) : dockerRootfulScript(a, r, p);
}

/**
 * Turn wizard answers into every artifact the install needs.
 *
 * @returns {{stack: string, env: string|null, pins: string, prep: string|null,
 *            quadlet: string|null, podmanRun: string|null, script: string|null,
 *            proxyConfig: {name: string, filename: string, language: string, body: string}|null}}
 */
export function generate(input = {}) {
  const a = { ...DEFAULTS, ...input };
  for (const key of ['basePath', 'savesPath', 'publicHost', 'tlsHosts', 'publicUrl', 'trustedProxies', 'dbHost', 'dbUser', 'dbName']) {
    if (/[\r\n\0]/.test(String(a[key]))) throw new Error(`${key} must be a single line`);
  }
  const r = role(a.role);
  const showInstall = r.control && supportedProfile(a);
  const podman = showInstall && a.engine === 'podman';
  return {
    stack: seedStack(a),
    env: stackEnv(a),
    pins: pinsCommand(a),
    // A GPU host joins from its control plane: Admin -> Fleet -> Add host prints the
    // one-line command, which prepares the host too. Unraid needs no separate prep
    // block: prepare-host.sh writes under /etc, a ramdisk there.
    prep: showInstall && a.platform !== 'unraid' ? prepText(a) : null,
    quadlet: podman ? quadletUnit(a) : null,
    // "Only trying it out?" alternative to the Quadlet unit (owner decision 3):
    // never the installed path, so it is offered behind a closed disclosure.
    podmanRun: podman ? podmanRunSeed(a) : null,
    script: showInstall ? scriptText(a) : null,
    proxyConfig:
      r.control && a.access === 'proxy'
        ? proxyConfig(a.proxy, {
            publicUrl: (a.publicUrl || 'https://quasar.example.com').trim().replace(/\/+$/, ''),
            host: a.publicHost.trim() || 'quasar-host.lan',
            port: a.controlPort,
          })
        : null,
  };
}
