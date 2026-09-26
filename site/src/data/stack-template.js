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
 */
import { proxyConfig } from './proxy-configs.js';
import { platform } from './platforms.js';

export const REGISTRY_NS = 'ghcr.io/accreleus/quasar';
/** The edge channel's tag family for builds that ship owned installs (#365). */
export const CHANNEL_TAG = 'o2-develop';
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

export const DEFAULTS = {
  platform: 'fedora', // see platforms.js
  role: 'combined', // see ROLES
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
  return `for i in ${names.join(' ')}; do docker pull -q ${REGISTRY_NS}/$i:${CHANNEL_TAG} >/dev/null && docker image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' ${REGISTRY_NS}/$i:${CHANNEL_TAG} | grep -m1 "^${REGISTRY_NS}/$i@"; done`;
}

function shellQuote(value) {
  return "'" + String(value).replaceAll("'", "'\"'\"'") + "'";
}

/** The host script: checks, host preparation, then the seed by `docker run`. */
function scriptText(a) {
  const r = role(a.role);
  const p = platform(a.platform);
  const { uid, gid } = appUser(a);
  const external = r.control && a.database === 'external';
  const vars = { seed: '$seed_image', control: '$control_image', agent: '$agent_image' };
  const envArgs = seedInputs(a, vars)
    .map(([k, v]) => {
      if (k === 'QUASAR_DATABASE_PASSWORD') return '  -e QUASAR_DATABASE_PASSWORD \\';
      const value = v.startsWith('$') && !v.startsWith('${') ? `"${v}"` : shellQuote(v);
      return `  -e ${k}=${value} \\`;
    })
    .join('\n');
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
  echo "Stop it first without deleting its volumes (docker compose -f <its directory>/docker-compose.yml down)," >&2
  echo "then run this again. See https://accreleus.github.io/quasar/install/move-existing/" >&2
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

/**
 * Turn wizard answers into every artifact the install needs.
 *
 * @returns {{stack: string, env: string|null, pins: string, script: string|null,
 *            proxyConfig: {name: string, filename: string, language: string, body: string}|null}}
 */
export function generate(input = {}) {
  const a = { ...DEFAULTS, ...input };
  for (const key of ['basePath', 'savesPath', 'publicHost', 'tlsHosts', 'publicUrl', 'trustedProxies', 'dbHost', 'dbUser', 'dbName']) {
    if (/[\r\n\0]/.test(String(a[key]))) throw new Error(`${key} must be a single line`);
  }
  const r = role(a.role);
  return {
    stack: seedStack(a),
    env: stackEnv(a),
    pins: pinsCommand(a),
    // A GPU host joins from its control plane: Admin -> Fleet -> Add host prints the
    // one-line command, which prepares the host too.
    script: r.control ? scriptText(a) : null,
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
