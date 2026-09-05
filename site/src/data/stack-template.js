/**
 * The single source of truth for the Quasar install artifacts.
 *
 * `generate(answers)` turns the quick start wizard's answers into the Compose
 * file, the NVIDIA overlay, the .env file, the one-paste shell script, and a
 * reverse proxy config. It is pure: no DOM, no I/O, no imports beyond the proxy
 * builders, so it runs in the browser (bundled into the wizard), at build time
 * (rendering the Install page) and under `node --test`.
 *
 * THE ONE RULE: the two heredocs in the generated script are quoted
 * differently, on purpose.
 *
 *   COMPOSE is quoted   (<<'COMPOSE')  so ${POSTGRES_PASSWORD:?} reaches the
 *                                      file for Compose to expand at up time.
 *   ENV is NOT quoted   (<<ENV)        so $(openssl rand ...) runs on the
 *                                      target machine and writes a real secret.
 *
 * Swap them and you get a Compose file full of empty interpolations and a .env
 * full of literal `$(openssl ...)` strings. `stack-template.test.js` asserts
 * both directions.
 */

import { proxyConfig } from './proxy-configs.js';
import { platform } from './platforms.js';

const REGISTRY = 'ghcr.io/accreleus/quasar';

export const DEFAULTS = {
  platform: 'fedora', // see platforms.js
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

/** One agent image covers every host: NVIDIA driver userspace and the CUDA
 *  runtime are provisioned into a volume at run time, so there is no per-vendor
 *  lineage to choose. The GPU answer still selects the compose overlay. */
export function agentImage() {
  return `${REGISTRY}/quasar-node-agent:latest`;
}

/**
 * Vulkan Video everywhere. It is the validated path on AMD and NVIDIA, and on
 * NVIDIA it sidesteps an NVENC teardown fault no driver version escapes. Intel
 * is untested but takes the same path; VA-API is there as a fallback if it
 * turns out not to work, which is a per-host setting rather than a fork here.
 */
export function encoder() {
  return 'vulkan';
}

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
 * 1000, so the state directory is always chowned to 1000 regardless.
 */
export function appUser(a) {
  const p = platform(a.platform);
  if (a.owner === 'custom') {
    return { uid: Number(a.uid) ?? p.defaultUid, gid: Number(a.gid) ?? p.defaultGid };
  }
  return { uid: p.defaultUid, gid: p.defaultGid };
}

/** The -f list every generated docker compose command carries. */
export function composeFiles(a) {
  const files = ['deploy/docker-compose.yml'];
  if (a.gpu === 'nvidia') files.push('deploy/docker-compose.nvidia.yml');
  return files;
}

function composeYaml(a) {
  const ownCert =
    a.access === 'own-cert'
      ? `        QUASAR_TLS_CERT: \${QUASAR_TLS_CERT:?set it in .env}
        QUASAR_TLS_KEY: \${QUASAR_TLS_KEY:?set it in .env}
`
      : '';
  const certMount =
    a.access === 'own-cert'
      ? `        - \${QUASAR_TLS_CERT_DIR:?set it in .env}:/etc/quasar/tls:ro
`
      : '';

  return `services:
  quasar-postgres:
    image: postgres:16-alpine
    environment:
      POSTGRES_DB: quasar
      POSTGRES_USER: quasar
      POSTGRES_PASSWORD: \${POSTGRES_PASSWORD:?set it in .env}
    volumes:
      - quasar-postgres-data:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U quasar -d quasar"]
      interval: 5s
      timeout: 3s
      retries: 12
    restart: unless-stopped

  quasar-control-plane:
    image: \${QUASAR_CONTROL_IMAGE:-${REGISTRY}/quasar-control-plane:latest}
    ports:
      - "\${CONTROL_PORT:-8080}:8080"
      - "\${QUASAR_TLS_PORT:-8443}:8443"
    environment:
      DATABASE_URL: postgres://quasar:\${POSTGRES_PASSWORD:?}@quasar-postgres:5432/quasar?sslmode=disable
      LISTEN_ADDR: ":8080"
      QUASAR_TLS_ADDR: ":8443"
      QUASAR_TLS_HOSTS: \${QUASAR_TLS_HOSTS:-}
      QUASAR_TLS_REDIRECT_PORT: \${QUASAR_TLS_PORT:-8443}
      ENROLLMENT_TOKEN: \${ENROLLMENT_TOKEN:?set it in .env}
      QUASAR_SECRET_KEY: \${QUASAR_SECRET_KEY:-}
      QUASAR_HOME_ROOT: \${QUASAR_HOME_ROOT:?set it in .env}
      PUBLIC_BASE_URL: \${PUBLIC_BASE_URL:-}
      QUASAR_ALLOWED_ORIGINS: \${QUASAR_ALLOWED_ORIGINS:-}
      QUASAR_TRUSTED_PROXIES: \${QUASAR_TRUSTED_PROXIES:-}
      # Self-update. Every one has a working default, so leaving them empty is
      # the normal case; they are passed because an .env entry with no
      # passthrough is silently inert. QUASAR_PLATFORM_RELEASE_REPO=off disables
      # release detection entirely.
      QUASAR_PLATFORM_RELEASE_REPO: \${QUASAR_PLATFORM_RELEASE_REPO:-}
      QUASAR_PLATFORM_RELEASE_API: \${QUASAR_PLATFORM_RELEASE_API:-}
      QUASAR_PLATFORM_RELEASE_ASSET_HOSTS: \${QUASAR_PLATFORM_RELEASE_ASSET_HOSTS:-}
      QUASAR_PLATFORM_RELEASE_TOKEN: \${QUASAR_PLATFORM_RELEASE_TOKEN:-}
      QUASAR_PLATFORM_RELEASE_DETECT_INTERVAL: \${QUASAR_PLATFORM_RELEASE_DETECT_INTERVAL:-}
      QUASAR_PLATFORM_REGISTRY: \${QUASAR_PLATFORM_REGISTRY:-}
      QUASAR_IMAGE_REGISTRY_HOSTS: \${QUASAR_IMAGE_REGISTRY_HOSTS:-}
${ownCert}      QUASAR_WEB_ROOT: /app/web
    volumes:
      - \${QUASAR_STATE_DIR:?set it in .env}:/var/lib/quasar-control
${certMount}    depends_on:
      quasar-postgres:
        condition: service_healthy
    healthcheck:
      test: ["CMD-SHELL", "curl -fsS http://localhost:8080/health"]
      interval: 10s
      timeout: 5s
      retries: 6
      start_period: 15s
    restart: unless-stopped

  quasar-node-agent:
    image: \${QUASAR_AGENT_IMAGE:-${agentImage(a.gpu)}}
    entrypoint: ["/usr/local/bin/quasar-node-agent-entrypoint"]
    network_mode: host
    cap_add: [NET_ADMIN, SYSLOG]
    init: true
    environment:
      CONTROL_PLANE_URL: ws://localhost:\${CONTROL_PORT:-8080}
      ENROLLMENT_TOKEN: \${ENROLLMENT_TOKEN:?}
      NODE_NAME: \${NODE_NAME:-quasar-node-1}
      NODE_SECRET_PATH: /var/lib/quasar-agent/node-secret
      XDG_RUNTIME_DIR: /run/quasar-agent
      QUASAR_ENCODER: \${QUASAR_ENCODER:-${encoder()}}
      QUASAR_HOME_ROOT: \${QUASAR_HOME_ROOT:?}
      QUASAR_APP_PUID: \${QUASAR_APP_PUID:-}
      QUASAR_APP_PGID: \${QUASAR_APP_PGID:-}
      QUASAR_APP_SHM_SIZE: 1g
      QUASAR_PULSE_IMAGE: \${QUASAR_AGENT_IMAGE:-${agentImage(a.gpu)}}
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - /run/quasar-agent:/run/quasar-agent
      - /dev/input:/dev/input
      - /dev:/host/dev:ro
      - /etc/os-release:/host/etc/os-release:ro
      - \${QUASAR_HOME_ROOT}:\${QUASAR_HOME_ROOT}
      - quasar-agent-data:/var/lib/quasar-agent
      - quasar-updater-run:/run/quasar-updater
    devices:
      - /dev/dri
      - /dev/uinput
      # Kernel ring buffer, read-only: GPU faults (NVIDIA Xid, amdgpu) reach the
      # session trace. Needs CAP_SYSLOG above. Drop both lines on a kernel with
      # no /dev/kmsg; the agent then reports xid_visibility: skip.
      - /dev/kmsg:/dev/kmsg:r
    device_cgroup_rules:
      - 'c 13:* rmw'
    depends_on:
      quasar-control-plane:
        condition: service_healthy
    restart: unless-stopped

  # Applies a platform release to this stack: it pulls the pinned digests and
  # recreates the containers they replace, because a container cannot recreate
  # itself. It acts only when told to, over the socket in the shared volume
  # above, and only on images inside the allow-listed namespace.
  quasar-updater:
    image: \${QUASAR_UPDATER_IMAGE:-${REGISTRY}/quasar-updater:latest}
    environment:
      QUASAR_UPDATER_ALLOWED_NAMESPACES: \${QUASAR_UPDATER_ALLOWED_NAMESPACES:-}
      QUASAR_UPDATER_WAIT_TIMEOUT_S: \${QUASAR_UPDATER_WAIT_TIMEOUT_S:-}
    volumes:
      # Podman: point QUASAR_DOCKER_SOCKET at /run/user/<uid>/podman/podman.sock.
      - \${QUASAR_DOCKER_SOCKET:-/var/run/docker.sock}:/var/run/docker.sock
      # This directory at its own host path. The updater rebuilds its compose
      # invocation from its own container labels, which record host paths, so
      # the same absolute path has to resolve inside the container. Left at the
      # placeholder below, the updater refuses to serve and says so; nothing
      # else in the stack is affected.
      - \${QUASAR_STACK_DIR:-/var/lib/quasar/stack-dir-unset}:\${QUASAR_STACK_DIR:-/var/lib/quasar/stack-dir-unset}
      - quasar-updater-run:/run/quasar-updater
    # SELinux-enforcing hosts (Podman) refuse every call to the mounted socket
    # without this. Docker is unaffected.
    security_opt: ["label=disable"]
    restart: unless-stopped

volumes:
  quasar-postgres-data:
  quasar-agent-data:
  quasar-updater-run:
`;
}

function nvidiaYaml() {
  return `services:
  quasar-node-agent:
    image: \${QUASAR_AGENT_IMAGE:-${REGISTRY}/quasar-node-agent:latest}
    gpus: all
    environment:
      NVIDIA_DRIVER_CAPABILITIES: all
      QUASAR_GPU_NVIDIA: "1"
      QUASAR_CUDA_DEVICE: \${QUASAR_CUDA_DEVICE:-0}
      QUASAR_RENDER_NODE: \${QUASAR_RENDER_NODE:-/dev/dri/renderD128}
      QUASAR_NVIDIA_DRIVER_VOLUME: \${QUASAR_NVIDIA_DRIVER_VOLUME:-1}
      LD_LIBRARY_PATH: /opt/quasar/nvidia-driver/lib64
      QUASAR_PULSE_IMAGE: \${QUASAR_AGENT_IMAGE:-${REGISTRY}/quasar-node-agent:latest}
    volumes:
      - quasar-nvidia-driver:/opt/quasar/nvidia-driver

volumes:
  quasar-nvidia-driver:
`;
}

function envFile(a) {
  const { uid, gid } = appUser(a);
  const lines = [
    '# Generated by the Quasar quick start. The three secrets below are created',
    '# on this machine when the script runs; they were never in your browser.',
    'POSTGRES_PASSWORD=$(openssl rand -hex 24)',
    'ENROLLMENT_TOKEN=$(openssl rand -hex 32)',
    'QUASAR_SECRET_KEY=$(openssl rand -base64 32)',
    '',
    '# Where Quasar keeps things.',
    `QUASAR_HOME_ROOT=${homePath(a)}`,
    `QUASAR_STATE_DIR=${statePath(a)}`,
    '',
    '# This stack directory, at its absolute path on this host. The updater',
    '# needs it to find the compose files it is asked to act on.',
    'QUASAR_STACK_DIR=$(cd deploy && pwd)',
    '',
    '# Who owns save data. Game containers drop to this user.',
    `QUASAR_APP_PUID=${uid}`,
    `QUASAR_APP_PGID=${gid}`,
    '',
    '# Encoder for this host.',
    `QUASAR_ENCODER=${encoder()}`,
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
    const dir = (a.certPath || '').trim().replace(/\/[^/]*$/, '') || '/etc/quasar/tls';
    lines.push(
      '',
      '# Your own certificate, mounted read-only into the container.',
      `QUASAR_TLS_CERT_DIR=${dir}`,
      `QUASAR_TLS_CERT=/etc/quasar/tls/${(a.certPath || 'cert.pem').split('/').pop()}`,
      `QUASAR_TLS_KEY=/etc/quasar/tls/${(a.keyPath || 'key.pem').split('/').pop()}`
    );
  }

  return lines.join('\n') + '\n';
}

function scriptText(a) {
  const { uid, gid } = appUser(a);
  const p = platform(a.platform);
  const files = composeFiles(a)
    .map((f) => `-f ${f}`)
    .join(' ');

  const nvidiaBlock =
    a.gpu === 'nvidia'
      ? `
echo "==> NVIDIA container toolkit"
${p.sudo}nvidia-ctk runtime configure

# The one destructive step in this script: restarting Docker stops every other
# container on this machine. Nothing else here touches anything you already run.
echo
echo "Docker has to restart for the NVIDIA runtime to register."
echo "This stops every container currently running on this host."
printf 'Restart Docker now? [y/N] '
read -r reply
case "$reply" in
  [yY]*) ${p.dockerRestart} ;;
  *) echo "Skipped. Run '${p.dockerRestart}' before starting Quasar." ;;
esac

cat > deploy/docker-compose.nvidia.yml <<'NVIDIA'
${nvidiaYaml()}NVIDIA
`
      : '';

  return `#!/usr/bin/env bash
# Quasar quick start for ${p.label}. Generated in your browser; nothing was sent
# anywhere. Read it before you run it.
set -euo pipefail

echo "==> Directories"
${p.sudo}install -d -m 0755 -o ${uid} -g ${gid} "${homePath(a)}"
${p.sudo}install -d -m 0755 -o 1000 -g 1000 "${statePath(a)}"
mkdir -p deploy

echo "==> UDP send buffer"
# libnice never calls setsockopt(SO_SNDBUF), so media sockets inherit the kernel
# default of 208 KB. A keyframe burst at 8 Mbps overflows it, the kernel drops
# the overflow silently, and the bitrate estimator reads that as congestion.
${p.sysctl()}

echo "==> Virtual input"
${p.module()}
${nvidiaBlock}
echo "==> Compose file"
cat > deploy/docker-compose.yml <<'COMPOSE'
${composeYaml(a)}COMPOSE

echo "==> Environment file"
umask 077
cat > deploy/.env <<ENV
${envFile(a)}ENV
umask 022

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
  return {
    compose: composeYaml(a),
    nvidia: a.gpu === 'nvidia' ? nvidiaYaml() : null,
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
