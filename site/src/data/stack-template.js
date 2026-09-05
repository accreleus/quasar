/** Install artifacts derived from the repository Compose files. */
import { dump } from 'js-yaml';
import templates from './compose-template.generated.js';
import { proxyConfig } from './proxy-configs.js';
import { platform } from './platforms.js';

export const DEFAULTS = {
  platform: 'fedora', // see platforms.js
  kernelLogs: false, // Optional host /dev/kmsg diagnostics.
  gpu: 'amd-intel', // 'nvidia' | 'amd-intel'
  basePath: '/var/lib/quasar',
  separateSaves: false,
  savesPath: '',
  owner: 'dedicated', // 'dedicated' | 'me' | 'custom'
  uid: 1000,
  gid: 1000,
  access: 'self-signed', // 'self-signed' | 'proxy' | 'own-cert'
  tlsHosts: '',
  publicUrl: '',
  proxy: 'caddy',
  certPath: '',
  keyPath: '',
  controlPort: 8080,
  tlsPort: 8443,
};

/** Certificate, artwork cache and other control-plane state. */
export function statePath(a) {
  return `${a.basePath.replace(/\/+$/, '')}/control`;
}

/** Per-user home directories. This is the one that grows. */
export function homePath(a) {
  if (a.separateSaves && a.savesPath.trim()) return a.savesPath.trim().replace(/\/+$/, '');
  return `${a.basePath.replace(/\/+$/, '')}/homes`;
}

/**
 * Who owns save data, as {uid, gid}.
 *
 * This drives QUASAR_APP_PUID/PGID (the game container drops to it) and the
 * ownership of the created directories. It deliberately does NOT change the
 * control plane's own user: that image runs as uid 1000 and owns its files as
 * 1000 on its named volume; bind-storage overrides are a separate choice.
 */
export function appUser(a) {
  const p = platform(a.platform);
  if (a.owner === 'custom') {
    return { uid: Number(a.uid) ?? p.defaultUid, gid: Number(a.gid) ?? p.defaultGid };
  }
  return { uid: p.defaultUid, gid: p.defaultGid };
}

/** The -f list every generated docker compose command carries. */
export function composeFiles() {
  return ['deploy/docker-compose.yml'];
}

function composeYaml(a) {
  const doc = structuredClone(templates['deploy/docker-compose.yml']);
  const cp = doc.services['quasar-control-plane'];
  const agent = doc.services['quasar-node-agent'];
  if (a.gpu === 'nvidia') {
    const overlay = templates['deploy/docker-compose.nvidia.yml'];
    const nv = overlay.services['quasar-node-agent'];
    Object.assign(agent, nv, {
      environment: { ...agent.environment, ...nv.environment },
      volumes: [...agent.volumes, ...nv.volumes],
    });
    Object.assign(doc.volumes, overlay.volumes);
  }
  if (!a.kernelLogs) {
    agent.devices = agent.devices.filter(device => !String(device).startsWith('/dev/kmsg'));
    agent.cap_add = agent.cap_add.filter(capability => capability !== 'SYSLOG');
  }
  // Installation policy is explicit; service wiring comes from the repo.
  cp.image = '${QUASAR_CONTROL_IMAGE:?Select a published control-plane image}';
  agent.image = '${QUASAR_AGENT_IMAGE:?Select a published node-agent image}';
  doc.services['quasar-updater'].image = '${QUASAR_UPDATER_IMAGE:?Select a published updater image}';
  doc.services['quasar-updater'].volumes = doc.services['quasar-updater'].volumes.map(mount =>
    typeof mount === 'string' ? mount.replaceAll('${QUASAR_STACK_DIR:-/var/lib/quasar/stack-dir-unset}', '${QUASAR_STACK_DIR:?Set the absolute deploy directory in .env}') : mount);
  delete cp.environment.DATABASE_URL;
  Object.assign(cp.environment, {
    QUASAR_DATABASE_HOST: 'quasar-postgres',
    QUASAR_DATABASE_USER: '${POSTGRES_USER:-quasar}',
    QUASAR_DATABASE_PASSWORD: '${POSTGRES_PASSWORD:?Run openssl rand -hex 24 and paste its output into POSTGRES_PASSWORD in .env}',
    QUASAR_SECRET_KEY: '${QUASAR_SECRET_KEY:?Run openssl rand -base64 32 and paste its output into QUASAR_SECRET_KEY in .env}',
  });
  agent.environment.QUASAR_PULSE_IMAGE = '${QUASAR_PULSE_IMAGE:-${QUASAR_AGENT_IMAGE:?}}';
  // These are established before exec by the image entrypoint.
  for (const key of ['NODE_SECRET_PATH', 'XDG_RUNTIME_DIR', 'LD_LIBRARY_PATH', 'NVIDIA_DRIVER_CAPABILITIES']) {
    delete agent.environment[key];
  }
  // Optional settings belong in a service-specific override file. Keeping them
  // separate avoids exposing database credentials to the Docker-privileged agent.
  // Empty passthroughs have no deployment meaning; preserve all nonempty defaults.
  const choices = new Set([
    'QUASAR_HOME_ROOT', 'QUASAR_TEMPLATE_ROOT', 'QUASAR_APP_PUID', 'QUASAR_APP_PGID',
    'QUASAR_ENCODER', 'QUASAR_TLS_HOSTS', 'QUASAR_SECRET_KEY', 'PUBLIC_BASE_URL',
    'QUASAR_ALLOWED_ORIGINS', 'QUASAR_TRUSTED_PROXIES',
    // Release detection knobs retain the upstream install's .env interface.
    'QUASAR_PLATFORM_RELEASE_REPO', 'QUASAR_PLATFORM_RELEASE_API',
    'QUASAR_PLATFORM_RELEASE_ASSET_HOSTS', 'QUASAR_PLATFORM_RELEASE_TOKEN',
    'QUASAR_PLATFORM_RELEASE_DETECT_INTERVAL', 'QUASAR_PLATFORM_REGISTRY',
    'QUASAR_IMAGE_REGISTRY_HOSTS',
  ]);
  for (const [service, file] of [[cp, 'control.env'], [agent, 'agent.env']]) {
    for (const [key, value] of Object.entries(service.environment)) {
      if (value === '${' + key + ':-}' && !choices.has(key)) delete service.environment[key];
    }
    service.env_file = [{ path: file, required: false }];
  }
  if (a.access === 'own-cert') {
    Object.assign(cp.environment, {
      QUASAR_TLS_CERT: '/etc/quasar/tls/cert.pem',
      QUASAR_TLS_KEY: '/etc/quasar/tls/key.pem',
    });
    cp.volumes.push(
      { type: 'bind', source: '${QUASAR_TLS_CERT:?}', target: '/etc/quasar/tls/cert.pem', read_only: true, bind: { create_host_path: false } },
      { type: 'bind', source: '${QUASAR_TLS_KEY:?}', target: '/etc/quasar/tls/key.pem', read_only: true, bind: { create_host_path: false } },
    );
  }
  return '# Quasar generated install v2\n' + dump(doc, { lineWidth: -1, noRefs: true });
}

function envFile(a, installer = false) {
  const { uid, gid } = appUser(a);
  const lines = [
    '# Replace ALL three blank values below before starting. Keep this file private.',
    '# Run these commands in a terminal, then paste each OUTPUT after the matching =.',
    '# Do not paste the command itself into a value; .env does not run shell commands.',
    '# POSTGRES_PASSWORD: openssl rand -hex 24',
    '# ENROLLMENT_TOKEN: openssl rand -hex 32',
    '# QUASAR_SECRET_KEY: openssl rand -base64 32',
    'POSTGRES_PASSWORD=',
    'ENROLLMENT_TOKEN=',
    'QUASAR_SECRET_KEY=',
    '',
    '# Where Quasar keeps things.',
    `QUASAR_HOME_ROOT=${homePath(a)}`,
    `QUASAR_TEMPLATE_ROOT=${homePath(a).replace(/\/[^/]*$/, '/templates')}`,
    '',
    '# This stack directory, at its absolute path on this host. The updater',
    '# needs it to find the compose files it is asked to act on.',
    '# Set this to the absolute directory where you save docker-compose.yml and .env.',
    'QUASAR_STACK_DIR=',
    '',
    '# Who owns save data. Game containers drop to this user.',
    `QUASAR_APP_PUID=${uid}`,
    `QUASAR_APP_PGID=${gid}`,
    '',
    '# Optional encoder override; the agent detects the GPU when empty.',
    'QUASAR_ENCODER=',
    '# Pin all images to a published release before starting.',
    `QUASAR_CONTROL_IMAGE=${a.controlImage || ''}`,
    `QUASAR_AGENT_IMAGE=${a.agentImage || ''}`,
    `QUASAR_UPDATER_IMAGE=${a.updaterImage || ''}`,
    '',
    '# Ports on the host.',
    `CONTROL_PORT=${a.controlPort}`,
    `QUASAR_TLS_PORT=${a.tlsPort}`,
  ];

  if (a.access === 'self-signed') {
    lines.push(
      '',
      '# Every address anyone will type into a browser. The certificate names',
      '# these and is generated once, on first boot.',
      `QUASAR_TLS_HOSTS=${a.tlsHosts.trim()}`
    );
  }

  if (a.access === 'proxy') {
    const origin = (a.publicUrl || '').trim().replace(/\/+$/, '');
    lines.push(
      '',
      '# Fronted by your own reverse proxy.',
      `PUBLIC_BASE_URL=${origin}`,
      `QUASAR_ALLOWED_ORIGINS=${origin}`,
      '# Only trust forwarded client addresses from the proxy you operate.',
      'QUASAR_TRUSTED_PROXIES=',
      '# Direct LAN access keeps working on the TLS port, so name those',
      '# addresses too if you use them.',
      `QUASAR_TLS_HOSTS=${a.tlsHosts.trim()}`
    );
  }

  if (a.access === 'own-cert') {
    lines.push('', '# Host paths; each file is mounted independently, read-only.',
      `QUASAR_TLS_CERT=${a.certPath.trim()}`,
      `QUASAR_TLS_KEY=${a.keyPath.trim()}`);
  }

  const resolvedByInstaller = new Set(['QUASAR_STACK_DIR', 'QUASAR_CONTROL_IMAGE', 'QUASAR_AGENT_IMAGE', 'QUASAR_UPDATER_IMAGE']);
  return lines.filter(line => !installer || !resolvedByInstaller.has(line.split('=')[0])).map(line => {
    if (line.startsWith('#') || !line.includes('=')) return line;
    const index = line.indexOf('=');
    const value = line.slice(index + 1);
    if (/^[a-zA-Z0-9_./,:@%+-]*$/.test(value)) return line;
    return line.slice(0, index + 1) + "'" + value.replaceAll("\\", "\\\\").replaceAll("'", "\\'") + "'";
  }).join('\n') + '\n';
}

// The release manifest's two-component contract is shared with self-update.
// The independently published updater is checked by pulling its matching version.
function releaseSelection() {
  return `
# Fresh installations select the latest stable GitHub release, never a guessed
# image tag or a prerelease. Existing .env files keep their image pins.
echo "==> Published release"
release=$(curl --fail --silent --show-error --connect-timeout 10 --max-time 60 https://api.github.com/repos/accreleus/quasar/releases/latest) || {
  echo "No stable release could be retrieved. Select a published release explicitly before installing." >&2
  exit 1
}
manifest_url=$(printf '%s' "$release" | jq -er '.assets[] | select(.name == "platform-release-manifest.json") | .browser_download_url')
case "$manifest_url" in
  https://github.com/accreleus/quasar/releases/download/*/platform-release-manifest.json) ;;
  *) echo "Release has no recognized platform manifest asset" >&2; exit 1 ;;
esac
manifest=$(curl --fail --silent --show-error --location --connect-timeout 10 --max-time 60 "$manifest_url")
printf '%s' "$manifest" | jq -e '
  .format_version == 1 and .prerelease == false and
  (.version | test("^[0-9]+[.][0-9]+[.][0-9]+$")) and
  (.components | length == 2) and
  ([.components[].name] | sort == ["control-plane", "node-agent"]) and
  all(.components[]; .image == ("ghcr.io/accreleus/quasar/quasar-" + .name) and
    (.digest | test("^sha256:[a-f0-9]{64}$")))
' >/dev/null || { echo "Invalid stable platform release manifest" >&2; exit 1; }
control_image=$(printf '%s' "$manifest" | jq -r '.components[] | select(.name == "control-plane") | .image + "@" + .digest')
agent_image=$(printf '%s' "$manifest" | jq -r '.components[] | select(.name == "node-agent") | .image + "@" + .digest')
version=$(printf '%s' "$manifest" | jq -r '.version')
updater_image="ghcr.io/accreleus/quasar/quasar-updater:$version"
# Verify availability before writing credentials or creating stack services.
docker pull "$control_image"
docker pull "$agent_image"
docker pull "$updater_image"
entrypoint=$(docker image inspect --format '{{json .Config.Entrypoint}}' "$control_image")
printf '%s' "$entrypoint" | jq -e '.[0] == "/usr/local/bin/quasar-control-entrypoint"' >/dev/null || {
  echo "The latest stable release predates this installer configuration. Use the installation instructions shipped with that release, or wait for a compatible release." >&2
  exit 1
}
`;
}

function shellQuote(value) {
  return "'" + value.replaceAll("'", "'\"'\"'") + "'";
}

function scriptText(a) {
  const { uid, gid } = appUser(a);
  const p = platform(a.platform);
  const files = composeFiles(a)
    .map((f) => `-f ${f}`)
    .join(' ');

  const nvidiaBlock = a.gpu === 'nvidia' ? `
if command -v docker >/dev/null && command -v jq >/dev/null; then
  if ! docker info --format '{{json .Runtimes}}' | jq -e 'has("nvidia")' >/dev/null; then
    echo "NVIDIA Container Toolkit is not registered with Docker. Configure it on this host, then rerun this installer." >&2
    preflight_failed=1
  fi
fi
if [ ! -r /sys/module/nvidia/version ]; then
  echo "The NVIDIA kernel driver is not loaded. Install/enable the host graphics driver first." >&2
  preflight_failed=1
fi
` : '';


  return `#!/usr/bin/env bash
# Quasar quick start for ${p.label}. Generated in your browser; nothing was sent
# anywhere. Read it before you run it.
set -euo pipefail

echo "==> Host preflight"
preflight_failed=0
for tool in docker openssl curl jq; do
  if ! command -v "$tool" >/dev/null; then
    echo "Install required tool: $tool" >&2
    preflight_failed=1
  fi
done
if command -v docker >/dev/null; then
  if ! docker info >/dev/null; then
    echo "Docker is unavailable or this user cannot access its socket." >&2
    preflight_failed=1
  fi
  compose_version=$(docker compose version --short 2>/dev/null || true)
  if [[ ! "$compose_version" =~ ^v?([0-9]+)[.]([0-9]+) ]] ||
     (( BASH_REMATCH[1] < 2 || (BASH_REMATCH[1] == 2 && BASH_REMATCH[2] < 30) )); then
    echo "Install Docker Compose v2.30 or newer (found: $compose_version)." >&2
    preflight_failed=1
  fi
fi
if [ ! -d /dev/dri ]; then
  echo "GPU devices are unavailable under /dev/dri. Check the host graphics driver." >&2
  preflight_failed=1
fi
${nvidiaBlock}
if [ "$preflight_failed" != 0 ]; then
  echo "Correct the preflight problems above before starting Quasar." >&2
  exit 1
fi
if [ -e deploy/docker-compose.yml ] && ! grep -q '^# Quasar generated install v2$' deploy/docker-compose.yml; then
  echo "This directory contains an existing stack. Keep its deployment commands; this first-install script will not rewrite it." >&2
  exit 1
fi
if [ ! -e deploy/.env ]; then
${releaseSelection()}fi

echo "==> Directories"
${p.sudo}install -d -m 0755 -o ${uid} -g ${gid} ${shellQuote(homePath(a))}
mkdir -p deploy

echo "==> UDP send buffer"
# libnice never calls setsockopt(SO_SNDBUF), so media sockets inherit the kernel
# default of 208 KB. A keyframe burst at 8 Mbps overflows it, the kernel drops
# the overflow silently, and the bitrate estimator reads that as congestion.
${p.sysctl()}

echo "==> Virtual input"
${p.module()}
[ -c /dev/uinput ] || { echo "Virtual input device /dev/uinput is unavailable after loading uinput" >&2; exit 1; }
[ -d /dev/dri ] || { echo "GPU device directory /dev/dri is unavailable; check the host graphics driver" >&2; exit 1; }

echo "==> Compose file"
if [ ! -e deploy/docker-compose.yml ]; then
  cat > deploy/docker-compose.yml <<'COMPOSE'
${composeYaml(a)}COMPOSE
fi

echo "==> Environment file"
umask 077
if [ ! -e deploy/.env ]; then
  postgres=$(openssl rand -hex 24)
  enrollment=$(openssl rand -hex 32)
  secret=$(openssl rand -base64 32)
  env_tmp=$(mktemp deploy/.env.XXXXXX)
  trap 'rm -f "$env_tmp"' EXIT
  cat > "$env_tmp" <<'ENV'
${envFile(a, true)}ENV
  sed -i "s|^POSTGRES_PASSWORD=$|POSTGRES_PASSWORD=$postgres|; s|^ENROLLMENT_TOKEN=$|ENROLLMENT_TOKEN=$enrollment|; s|^QUASAR_SECRET_KEY=$|QUASAR_SECRET_KEY=$secret|" "$env_tmp"
  printf '\nQUASAR_STACK_DIR=%s\nQUASAR_CONTROL_IMAGE=%s\nQUASAR_AGENT_IMAGE=%s\nQUASAR_UPDATER_IMAGE=%s\n' "$(cd deploy && pwd)" "$control_image" "$agent_image" "$updater_image" >> "$env_tmp"
  # noclobber refuses a concurrent installer instead of replacing its credentials.
  (set -o noclobber; cat "$env_tmp" > deploy/.env)
  rm -f "$env_tmp"
  trap - EXIT
fi
umask 022

docker compose ${files} config --quiet
docker compose ${files} pull

echo "==> Starting Quasar"
docker compose ${files} up -d

echo
echo "Quasar is starting. Check it with:"
echo "  curl http://localhost:${a.controlPort}/health"
echo
echo "Then open https://<this-host>:${a.tlsPort} and accept the certificate once."
`;
}

/**
 * Turn wizard answers into every artifact the install needs.
 *
 * @returns {{compose: string, nvidia: string|null, env: string, script: string,
 *            proxyConfig: {name: string, filename: string, language: string, body: string}|null}}
 */
export function generate(input = {}) {
  const a = { ...DEFAULTS, ...input };
  for (const key of ['basePath', 'savesPath', 'certPath', 'keyPath', 'tlsHosts', 'publicUrl']) {
    if (/[\r\n\0]/.test(a[key])) throw new Error(`${key} must be a single line`);
  }
  return {
    compose: composeYaml(a),
    nvidia: null,
    env: envFile(a),
    script: scriptText(a),
    proxyConfig:
      a.access === 'proxy'
        ? proxyConfig(a.proxy, {
            publicUrl: (a.publicUrl || 'https://quasar.example.com').trim().replace(/\/+$/, ''),
            host: 'quasar-host.lan',
            port: a.controlPort,
          })
        : null,
  };
}
