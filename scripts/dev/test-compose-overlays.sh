#!/usr/bin/env bash
# Structural regression test for the compose file set.
#
#   bash scripts/dev/test-compose-overlays.sh
#
# These are the properties the base + overlay split exists to guarantee. Each one
# has been a real defect at least once, and `docker compose config -q` (which
# scripts/dx/config_check.sh runs) proves only that the files PARSE — not that
# they still compose to the right thing.
set -euo pipefail

cd "$(dirname "$0")/../.."

render() {
  ENROLLMENT_TOKEN=test \
    POSTGRES_PASSWORD=test \
    JWT_SECRET=test \
    docker compose "$@" config --format json
}

BASE=(-f deploy/docker-compose.yml)
NVIDIA=("${BASE[@]}" -f deploy/docker-compose.nvidia.yml)
CONSOLE=("${BASE[@]}" -f deploy/overlays/docker-compose.console.yml)
NVIDIA_CONSOLE=("${NVIDIA[@]}" -f deploy/overlays/docker-compose.console.yml)

# Same four chains as flat strings. bash 3.2 (the macOS system bash) has no
# namerefs, so the "for every chain" checks below word-split these instead.
ALL_CHAINS="BASE NVIDIA CONSOLE NVIDIA_CONSOLE"
chain_files() {
  case "$1" in
    BASE)           echo "-f deploy/docker-compose.yml" ;;
    NVIDIA)         echo "-f deploy/docker-compose.yml -f deploy/docker-compose.nvidia.yml" ;;
    CONSOLE)        echo "-f deploy/docker-compose.yml -f deploy/overlays/docker-compose.console.yml" ;;
    NVIDIA_CONSOLE) echo "-f deploy/docker-compose.yml -f deploy/docker-compose.nvidia.yml -f deploy/overlays/docker-compose.console.yml" ;;
    *) echo "unknown chain $1" >&2; exit 2 ;;
  esac
}

fail() { echo "FAIL: $*" >&2; exit 1; }

# agent_field <python expr over `svc`> -- <compose -f args...>
agent_field() {
  local expr=$1; shift; shift
  render "$@" | python3 -c "
import json,sys
svc = json.load(sys.stdin)['services']['quasar-node-agent']
print($expr)
"
}

# ── 1. PulseAudio sidecar follows the agent image ────────────────────────────
# A sidecar from a different lineage may have no `pulseaudio` binary, which mutes
# every session — streamed AND console-local — with only a WARN in the log
# (2026-07-26). Three separate overlays used to restate this default; the base
# now derives it, so it cannot drift per overlay again.
unset QUASAR_PULSE_IMAGE QUASAR_NODE_IMAGE QUASAR_AGENT_IMAGE || true
actual=$(agent_field "svc['environment']['QUASAR_PULSE_IMAGE']" -- "${BASE[@]}")
[ "$actual" = "quasar-node-agent:latest" ] || fail "base pulse image: expected quasar-node-agent:latest, got $actual"

actual=$(agent_field "svc['environment']['QUASAR_PULSE_IMAGE']" -- "${NVIDIA[@]}")
[ "$actual" = "quasar-node-agent:latest" ] || fail "nvidia pulse image: expected quasar-node-agent:latest, got $actual"

# Pointing the agent at a custom image drags the sidecar with it.
actual=$(QUASAR_NODE_IMAGE=registry.example/custom:test \
  agent_field "svc['environment']['QUASAR_PULSE_IMAGE']" -- "${BASE[@]}")
[ "$actual" = "registry.example/custom:test" ] || fail "pulse image did not follow QUASAR_NODE_IMAGE (got $actual)"

# QUASAR_AGENT_IMAGE is the current name and must drag the sidecar too.
actual=$(QUASAR_AGENT_IMAGE=registry.example/agent:test \
  agent_field "svc['environment']['QUASAR_PULSE_IMAGE']" -- "${BASE[@]}")
[ "$actual" = "registry.example/agent:test" ] || fail "pulse image did not follow QUASAR_AGENT_IMAGE (got $actual)"

# ...and it must win over the legacy name when both are set, since that is the
# direction of the migration.
actual=$(QUASAR_AGENT_IMAGE=registry.example/new:test QUASAR_NODE_IMAGE=registry.example/old:test \
  agent_field "svc['image']" -- "${BASE[@]}")
[ "$actual" = "registry.example/new:test" ] || fail "QUASAR_AGENT_IMAGE did not take precedence over QUASAR_NODE_IMAGE (got $actual)"

# An explicit sidecar override still wins.
actual=$(QUASAR_PULSE_IMAGE=registry.example/pulse:test \
  agent_field "svc['environment']['QUASAR_PULSE_IMAGE']" -- "${NVIDIA[@]}")
[ "$actual" = "registry.example/pulse:test" ] || fail "operator QUASAR_PULSE_IMAGE override was not preserved (got $actual)"

# ── 2. No compose-level GST_PLUGIN_PATH override ─────────────────────────────
# The runtime image owns /opt/gst/lib64/gstreamer-1.0; a legacy /usr/local path
# loads an older waylanddisplaysrc and hides its runtime cadence properties.
for chain in $ALL_CHAINS; do
  # shellcheck disable=SC2046  # deliberate word split of a controlled string
  value=$(agent_field "svc.get('environment', {}).get('GST_PLUGIN_PATH', '<unset>')" -- $(chain_files "$chain"))
  [ "$value" = "<unset>" ] || fail "$chain sets GST_PLUGIN_PATH=$value"
done

# ── 3. The agent never mounts the source tree ────────────────────────────────
# The binary is baked into the image, so the image tag alone says what is
# running. A /workspace mount here is the regression that made a deployed agent
# unidentifiable from its tag (CLAUDE.md "Runtime image ≠ build image").
for chain in $ALL_CHAINS; do
  # shellcheck disable=SC2046
  targets=$(agent_field "','.join(v.get('target','') for v in svc.get('volumes', []))" -- $(chain_files "$chain"))
  case ",$targets," in
    *,/workspace,*) fail "$chain mounts the source tree at /workspace" ;;
  esac
done

# ── 4. Console capabilities are scoped to the console overlay ────────────────
# A stream-only deployment must not carry DRM master or the host's audio devices.
caps=$(agent_field "','.join(svc.get('cap_add', []))" -- "${BASE[@]}")
case ",$caps," in *,SYS_ADMIN,*) fail "base grants SYS_ADMIN without the console overlay" ;; esac

caps=$(agent_field "','.join(svc.get('cap_add', []))" -- "${CONSOLE[@]}")
case ",$caps," in *,SYS_ADMIN,*) ;; *) fail "console overlay does not grant SYS_ADMIN (got: $caps)" ;; esac

rules=$(agent_field "';'.join(svc.get('device_cgroup_rules', []))" -- "${BASE[@]}")
case "$rules" in *"226:"*) fail "base carries the DRM cgroup rule without the console overlay" ;; esac

rules=$(agent_field "';'.join(svc.get('device_cgroup_rules', []))" -- "${NVIDIA_CONSOLE[@]}")
case "$rules" in *"226:"*) ;; *) fail "console overlay is missing the DRM cgroup rule (got: $rules)" ;; esac
case "$rules" in *"13:"*) ;; *) fail "console overlay dropped the base evdev cgroup rule (got: $rules)" ;; esac

# ── 5. Docker's default seccomp profile is retained everywhere ───────────────
# No shipped chain weakens it. The profiling overlay is the single documented
# exception and states so itself; a console host that genuinely needs one adds a
# local overlay rather than shipping the exception for everybody.
for chain in $ALL_CHAINS; do
  # shellcheck disable=SC2046
  opts=$(agent_field "','.join(svc.get('security_opt', []))" -- $(chain_files "$chain"))
  case ",$opts," in
    *,seccomp:unconfined,*) fail "$chain weakens seccomp" ;;
  esac
done

# ── 6. The NVIDIA overlay carries the CUDA-host specifics ────────────────────
# QUASAR_ENCODER: no compose default anywhere — empty means the agent
# auto-detects the GPU vendor; an operator .env value must still flow through.
unset QUASAR_ENCODER || true
actual=$(agent_field "svc['environment']['QUASAR_ENCODER']" -- "${NVIDIA[@]}")
[ "$actual" = "" ] || fail "nvidia chain must not default QUASAR_ENCODER (agent auto-detects), got $actual"
actual=$(agent_field "svc['environment']['QUASAR_GPU_NVIDIA']" -- "${NVIDIA[@]}")
[ "$actual" = "1" ] || fail "nvidia overlay did not set QUASAR_GPU_NVIDIA (got $actual)"
actual=$(agent_field "svc['environment']['QUASAR_ENCODER']" -- "${BASE[@]}")
[ "$actual" = "" ] || fail "base must not default QUASAR_ENCODER (agent auto-detects), got $actual"
actual=$(QUASAR_ENCODER=nvenc agent_field "svc['environment']['QUASAR_ENCODER']" -- "${NVIDIA[@]}")
[ "$actual" = "nvenc" ] || fail "operator QUASAR_ENCODER does not flow through the nvidia chain (got $actual)"

# ── 7. Volume-name adoption ──────────────────────────────────────────────────
# The base file itself must NEVER carry a `name:` override (#448: Compose v5
# rejects an empty `name:` value at `up` — "invalid volume name or ID: value is
# empty" — for every service, since the volume is a project-level definition;
# `docker compose config` silently drops the empty key instead of erroring, so
# this class of defect renders clean and only breaks at `up`). The override now
# lives ONLY in the opt-in deploy/overlays/docker-compose.adopt-volumes.yml overlay.
#
# `config` always resolves a concrete `name` (Compose's own <project>_<key>
# default), even with no override in the file — so "absent name key" is not a
# thing to assert on the base chain. What must hold is that the base chain
# IGNORES QUASAR_POSTGRES_VOLUME entirely: setting it must not change the
# resolved name, proving the base file has no `${QUASAR_POSTGRES_VOLUME}`
# reference left in it at all.
default_name=$(render "${BASE[@]}" | python3 -c "
import json,sys; print(json.load(sys.stdin)['volumes']['quasar-postgres-data']['name'])")
name=$(QUASAR_POSTGRES_VOLUME=should_be_ignored render "${BASE[@]}" | python3 -c "
import json,sys; print(json.load(sys.stdin)['volumes']['quasar-postgres-data']['name'])")
[ "$name" = "$default_name" ] || fail "base compose file still honours QUASAR_POSTGRES_VOLUME ($name) — the override belongs only in docker-compose.adopt-volumes.yml"

# Applying the overlay without all three required vars must fail closed, not
# fall back to defaults or adopt only some volumes — `:?` gives an actionable
# per-var error rather than a silent partial adoption.
ADOPT=("${BASE[@]}" -f deploy/overlays/docker-compose.adopt-volumes.yml)
if ENROLLMENT_TOKEN=test POSTGRES_PASSWORD=test JWT_SECRET=test \
    docker compose "${ADOPT[@]}" config --format json >/dev/null 2>&1; then
  fail "adopt-volumes overlay rendered with no QUASAR_*_VOLUME vars set — should have failed on the ':?' required vars"
fi

# With all three set, every name must be used verbatim — this is what lets a
# stack that was previously on a forked compose file keep its existing data
# volumes instead of silently starting against an empty database.
adopt_render() {
  ENROLLMENT_TOKEN=test POSTGRES_PASSWORD=test JWT_SECRET=test \
    QUASAR_POSTGRES_VOLUME=legacy_pg_volume \
    QUASAR_AGENT_VOLUME=legacy_agent_volume \
    QUASAR_CONTROL_VOLUME=legacy_tls_volume \
    docker compose "${ADOPT[@]}" config --format json
}
name=$(adopt_render | python3 -c "
import json,sys; print(json.load(sys.stdin)['volumes']['quasar-postgres-data']['name'])")
[ "$name" = "legacy_pg_volume" ] || fail "QUASAR_POSTGRES_VOLUME was not honoured under the adopt overlay (got $name)"
name=$(adopt_render | python3 -c "
import json,sys; print(json.load(sys.stdin)['volumes']['quasar-agent-data']['name'])")
[ "$name" = "legacy_agent_volume" ] || fail "QUASAR_AGENT_VOLUME was not honoured under the adopt overlay (got $name)"
name=$(adopt_render | python3 -c "
import json,sys; print(json.load(sys.stdin)['volumes']['quasar-control-tls']['name'])")
[ "$name" = "legacy_tls_volume" ] || fail "QUASAR_CONTROL_VOLUME was not honoured under the adopt overlay (got $name)"

# ── 7b. The base file is the PRODUCTION shape ────────────────────────────────
# docker-compose.release.yml was retired by making these properties true of the
# base file itself. Each assertion below is one thing that overlay used to have
# to rewrite, and the last one is the rewrite that broke twice.
cp_field() {
  local expr=$1; shift; shift
  render "$@" | python3 -c "
import json,sys
svc = json.load(sys.stdin)['services']['quasar-control-plane']
print($expr)
"
}

# No build key: a production file that can build is a production file that will
# silently build a 'release' out of whatever is in the working tree.
value=$(cp_field "'yes' if svc.get('build') else 'no'" -- "${BASE[@]}")
[ "$value" = "no" ] || fail "base compose file carries a build: key on the control plane — that belongs in overlays/docker-compose.dev.yml"

# No source mount: the production image bakes the SPA.
targets=$(cp_field "','.join(v.get('target','') for v in svc.get('volumes', []))" -- "${BASE[@]}")
case ",$targets," in
  *,/app/web,*) fail "base compose file bind-mounts the SPA at /app/web — that belongs in the dev overlay" ;;
esac

# The healthcheck must call the binary the PRODUCTION image actually ships
# (curl; Dockerfile.control.prod has no wget). Wrong binary here does not fail
# loudly: the container stays unhealthy forever and the node agent, gated on
# service_healthy, never starts.
probe=$(cp_field "' '.join(svc.get('healthcheck', {}).get('test', []))" -- "${BASE[@]}")
case "$probe" in *curl*) ;; *) fail "base healthcheck does not use curl (got: $probe)" ;; esac

# Digest pinning must be two env vars and nothing else.
value=$(QUASAR_CONTROL_IMAGE=registry.example/cp@sha256:abc cp_field "svc['image']" -- "${BASE[@]}")
[ "$value" = "registry.example/cp@sha256:abc" ] || fail "QUASAR_CONTROL_IMAGE did not pin the control-plane image (got $value)"

# ── 7b-i. Documented .env knobs actually reach the control plane ─────────────
# An .env entry with no passthrough in this file is silently inert: the operator
# follows deploy/README.md, sets the var, and nothing happens. QUASAR_TRUSTED_PROXIES
# was that for the whole BYO-reverse-proxy path (only the hardened overlay passed
# it), which left every client on one rate-limit budget — a pre-auth lockout on
# POST /v1/setup/claim. Each var below is documented in deploy/README.md or
# deploy/.env.example and must flow through the BASE chain.
for var in QUASAR_TRUSTED_PROXIES QUASAR_PUBLIC_HOST QUASAR_ALLOWED_ORIGINS \
           QUASAR_ICE_SERVERS PUBLIC_BASE_URL QUASAR_TLS_HOSTS \
           QUASAR_ARTWORK_MAX_BYTES QUASAR_ARTWORK_SWEEP_INTERVAL \
           QUASAR_PLATFORM_RELEASE_REPO QUASAR_PLATFORM_RELEASE_API \
           QUASAR_PLATFORM_RELEASE_ASSET_HOSTS QUASAR_PLATFORM_RELEASE_TOKEN \
           QUASAR_PLATFORM_RELEASE_DETECT_INTERVAL QUASAR_PLATFORM_REGISTRY \
           QUASAR_IMAGE_REGISTRY_HOSTS; do
  # Subshell + `export`: bash 3.2 cannot write `"$var"=value cmd`, and cp_field
  # is a function, so `env VAR=… cp_field` is not available either.
  actual=$(export "$var=sentinel-$var"; \
    cp_field "svc.get('environment', {}).get('$var', '<unset>')" -- "${BASE[@]}")
  [ "$actual" = "sentinel-$var" ] || \
    fail "base compose does not pass $var to the control plane (got: $actual) — a documented .env knob with no passthrough is silently inert"
done

# THE REGRESSION THAT MUST NEVER RETURN: the control plane creates its TLS pair
# under /var/lib/quasar-control and EXITS AT BOOT when that path is not
# writable. The retired release overlay did `volumes: !reset []` to strip the
# dev bind mount and took this with it, crash-looping every pull-based install.
# It must be present on the base chain AND survive the dev overlay, which
# appends to the list rather than replacing it.
for chain in BASE DEV; do
  case "$chain" in
    BASE) files="-f deploy/docker-compose.yml" ;;
    DEV)  files="-f deploy/docker-compose.yml -f deploy/overlays/docker-compose.dev.yml" ;;
  esac
  # shellcheck disable=SC2046
  targets=$(cp_field "','.join(v.get('target','') for v in svc.get('volumes', []))" -- $files)
  case ",$targets," in
    *,/var/lib/quasar-control,*) ;;
    *) fail "$chain lost the quasar-control-tls mount at /var/lib/quasar-control (got: $targets)" ;;
  esac
done

# The dev overlay must restore all three development affordances.
DEV=("${BASE[@]}" -f deploy/overlays/docker-compose.dev.yml)
value=$(cp_field "'yes' if svc.get('build') else 'no'" -- "${DEV[@]}")
[ "$value" = "yes" ] || fail "dev overlay does not add a build: key"
targets=$(cp_field "','.join(v.get('target','') for v in svc.get('volumes', []))" -- "${DEV[@]}")
case ",$targets," in *,/app/web,*) ;; *) fail "dev overlay does not mount the SPA at /app/web (got: $targets)" ;; esac
probe=$(cp_field "' '.join(svc.get('healthcheck', {}).get('test', []))" -- "${DEV[@]}")
case "$probe" in *wget*) ;; *) fail "dev overlay healthcheck does not use wget (got: $probe)" ;; esac

# ── 7c. Healthcheck binary matches the image flavor on EVERY chain ───────────
# The seam: the dev CP image (Dockerfile.control, debian-slim) ships wget and no
# curl; the production image (Dockerfile.control.prod) ships curl and no wget.
# So any chain that builds from source (a build: key on the control plane) must
# probe with wget, and any pull-only chain must probe with curl. The mismatch is
# silent — the container stays `unhealthy` forever on "<binary>: not found" and
# the agent, gated on service_healthy, never starts (seen live: the dev CP image
# on the base compose alone). The DEV chains here are exactly what
# deploy/redeploy.sh composes (it appends the dev overlay unconditionally).
for spec in \
  "BASE:-f deploy/docker-compose.yml" \
  "NVIDIA:-f deploy/docker-compose.yml -f deploy/docker-compose.nvidia.yml" \
  "HARDENED:-f deploy/docker-compose.yml -f deploy/docker-compose.hardened.yml" \
  "DEV:-f deploy/docker-compose.yml -f deploy/overlays/docker-compose.dev.yml" \
  "NVIDIA_DEV:-f deploy/docker-compose.yml -f deploy/docker-compose.nvidia.yml -f deploy/overlays/docker-compose.dev.yml"; do
  chain="${spec%%:*}"; files="${spec#*:}"
  # The hardened overlay `:?`-requires QUASAR_PUBLIC_HOST + QUASAR_TLS_DIR;
  # both are harmless on the other chains.
  # shellcheck disable=SC2046,SC2086
  built=$(QUASAR_PUBLIC_HOST=test.example QUASAR_TLS_DIR=/tmp/test-tls \
    cp_field "'yes' if svc.get('build') else 'no'" -- $files)
  # shellcheck disable=SC2046,SC2086
  probe=$(QUASAR_PUBLIC_HOST=test.example QUASAR_TLS_DIR=/tmp/test-tls \
    cp_field "' '.join(svc.get('healthcheck', {}).get('test', []))" -- $files)
  if [ "$built" = yes ]; then
    case "$probe" in *wget*) ;; *) fail "$chain builds the dev CP image (wget-only) but its healthcheck is: $probe" ;; esac
    case "$probe" in *curl*) fail "$chain builds the dev CP image (no curl) but its healthcheck calls curl: $probe" ;; esac
  else
    case "$probe" in *curl*) ;; *) fail "$chain runs the production CP image (curl-only) but its healthcheck is: $probe" ;; esac
    case "$probe" in *wget*) fail "$chain runs the production CP image (no wget) but its healthcheck calls wget: $probe" ;; esac
  fi
done

# The standalone local stack (agentless, different service names) builds
# Dockerfile.control too, so it must also pair build+wget.
local_probe=$(render -f deploy/overlays/docker-compose.local.yml | python3 -c "
import json,sys
svc = json.load(sys.stdin)['services']['control-plane']
assert svc.get('build'), 'local stack lost its build: key'
print(' '.join(svc.get('healthcheck', {}).get('test', [])))
")
case "$local_probe" in
  *wget*) ;;
  *) fail "local stack builds the dev CP image (wget-only) but its healthcheck is: $local_probe" ;;
esac
case "$local_probe" in
  *curl*) fail "local stack builds the dev CP image (no curl) but its healthcheck calls curl: $local_probe" ;;
esac

# ── 8. `config` is not enough — prove the chains actually come up ───────────
# #448 was invisible to every check above: `docker compose config` parses and
# silently drops an empty `name:` instead of erroring, so a config-only test
# suite would have stayed green while `up` failed on every service. `--dry-run`
# (Compose >= 2.20) walks the same path `up` does — pulling/building images
# aside — without starting anything, so it is the cheapest check that would
# actually have caught #448.
if docker compose version --short 2>/dev/null | awk -F. '{exit !($1>2 || ($1==2 && $2>=20))}'; then
  # --pull never: this is a structural check (does the plan build — volumes,
  # networks, containers — not "can we reach a registry"). A missing local
  # image is irrelevant to what #448 was; forcing a pull attempt here would
  # make the test depend on registry access it doesn't need.
  ENROLLMENT_TOKEN=test POSTGRES_PASSWORD=test JWT_SECRET=test \
    docker compose "${BASE[@]}" up --no-start --dry-run --pull never >/dev/null \
    || fail "BASE chain failed 'up --no-start --dry-run' (would have caught #448)"

  ENROLLMENT_TOKEN=test POSTGRES_PASSWORD=test JWT_SECRET=test \
    QUASAR_POSTGRES_VOLUME=legacy_pg_volume \
    QUASAR_AGENT_VOLUME=legacy_agent_volume \
    QUASAR_CONTROL_VOLUME=legacy_tls_volume \
    docker compose "${ADOPT[@]}" up --no-start --dry-run --pull never >/dev/null \
    || fail "ADOPT chain failed 'up --no-start --dry-run' with all three vars set"

  echo "  dry-run: BASE and ADOPT chains pass 'up --no-start --dry-run'"
else
  echo "  note: docker compose < 2.20 (no --dry-run support) — skipping the up-dry-run check;" \
       "config-shape checks above still ran"
fi

echo "compose overlays — sidecar lineage, plugin path, no source mount, console scoping, seccomp, nvidia specifics, production base shape, healthcheck/image-flavor pairing, volume adoption, dry-run up: PASS"
