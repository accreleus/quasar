#!/usr/bin/env bash
# Quasar quick start: a combined host on Unraid. Generated in your
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
if [ ! -d /dev/dri ]; then
  echo "GPU devices are unavailable under /dev/dri. Check the host graphics driver." >&2
  preflight_failed=1
fi
if [ "$preflight_failed" != 0 ]; then
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
# The current stable release's images, pinned to their digests here:
# the seed refuses a tag for the images it installs.
resolve() {
  local ref="ghcr.io/accreleus/quasar/$1:latest" pinned
  docker pull -q "$ref" >/dev/null || { echo "Could not pull $ref." >&2; return 1; }
  pinned=$(docker image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' "$ref" | grep -m1 "^ghcr.io/accreleus/quasar/$1@sha256:" || true)
  [ -n "$pinned" ] || { echo "$ref has no registry digest." >&2; return 1; }
  printf '%s\n' "$pinned"
}
seed_image=$(resolve quasar-recovery)
control_image=$(resolve quasar-control-plane)
agent_image=$(resolve quasar-node-agent)

echo "==> Directories"
install -d -m 0755 -o 99 -g 100 '/var/lib/quasar/homes'
install -d -m 0755 -o 99 -g 100 '/var/lib/quasar/templates'

echo "==> UDP send buffer"
# libnice never calls setsockopt(SO_SNDBUF), so media sockets inherit the kernel
# default of 208 KB. A keyframe burst at 8 Mbps overflows it, the kernel drops
# the overflow silently, and the bitrate estimator reads that as congestion.
sysctl -w net.core.wmem_default=2097152 >/dev/null
# /etc is a ramdisk on Unraid, so persist through the boot script instead.
grep -q 'wmem_default' /boot/config/go || echo 'sysctl -w net.core.wmem_default=2097152' >> /boot/config/go

echo "==> Virtual input"
modprobe uinput
grep -q 'modprobe uinput' /boot/config/go || echo 'modprobe uinput' >> /boot/config/go
[ -c /dev/uinput ] || { echo "Virtual input device /dev/uinput is unavailable after loading uinput" >&2; exit 1; }

echo "==> Starting the seed"
docker run -d --name quasar-seed --restart unless-stopped \
  --security-opt label=disable \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v quasar-machine:/var/lib/quasar-machine:ro \
  -e QUASAR_ROLE='combined' \
  -e QUASAR_PUBLIC_HOST='192.168.1.50' \
  -e QUASAR_TLS_HOSTS='quasar.lan' \
  -e QUASAR_HOME_ROOT='/var/lib/quasar/homes' \
  -e QUASAR_TEMPLATE_ROOT='/var/lib/quasar/templates' \
  -e QUASAR_APP_PUID='99' \
  -e QUASAR_APP_PGID='100' \
  -e QUASAR_CONTROL_PLANE_IMAGE="$control_image" \
  -e QUASAR_AGENT_IMAGE="$agent_image" \
  "$seed_image" seed >/dev/null

echo "==> Waiting for Quasar"
ready=0
for _ in $(seq 1 120); do
  if docker exec quasar-recovery quasar-recovery status >/dev/null 2>&1 && curl -fsS http://localhost:8080/health >/dev/null 2>&1; then
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
echo "Quasar is running. Open https://192.168.1.50:8443 and accept the certificate once."
echo "Claim the first admin with the one-time setup token:"
echo "  docker exec quasar-control-plane cat /run/quasar/setup-token"
