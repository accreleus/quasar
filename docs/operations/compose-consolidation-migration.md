# Migrating an existing stack onto the consolidated compose files

The per-vendor base files (`docker-compose.nv.yml`) and the per-image and
per-host overlays (`docker-compose.vulkan.yml`, `docker-compose.nv-vulkan.yml`,
`docker-compose.nv-console.yml`, `docker-compose.release-vulkan*.yml`) are gone.
There is now one base file plus orthogonal overlays — see `deploy/README.md`.

`docker-compose.release.yml` has since been retired too (2026-08-27): the base
file is now the production deployment, and pinning a release is setting
`QUASAR_CONTROL_IMAGE` + `QUASAR_AGENT_IMAGE` in `deploy/.env`. Building from
source now adds `overlays/docker-compose.dev.yml`, which `deploy/redeploy.sh`
applies automatically. See `docs/upgrading.md`.

A host that ran `docker-compose.yml` on its own needs **no migration**: the file
kept its name, its service names, its volume keys and its port defaults.

A host that ran `docker-compose.nv.yml` needs the steps below, because that file
carried three things the base file defaults differently: its own host ports, its
own volume names, and the console capabilities. **All three are `.env` changes —
no data is moved and nothing is recreated from scratch.**

## Before you start

Take the current values, from the host, with the stack still running:

```bash
docker volume ls --format '{{.Name}}' | grep quasar
docker compose -f deploy/docker-compose.nv.yml config --format json \
  | python3 -c 'import json,sys; s=json.load(sys.stdin)["services"]["quasar-control-plane"]; print(s["ports"])'
```

## 1. Pin the existing volumes in `deploy/.env` AND add the adoption overlay

This is the step that protects the database. The base file names its volumes
`quasar-postgres-data` / `quasar-agent-data` / `quasar-control-tls`; the NV file
named them `quasar-nv-*`. Without this step the stack comes up against an
**empty** database and an unenrolled agent.

Use the names `docker volume ls` printed, including the compose project prefix
(or run `scripts/dev/migrate-compose-volumes.sh`, which prints these ready to paste):

```dotenv
QUASAR_POSTGRES_VOLUME=deploy_quasar-nv-postgres-data
QUASAR_AGENT_VOLUME=deploy_quasar-nv-agent-data
QUASAR_CONTROL_VOLUME=deploy_quasar-nv-control-tls
```

**These three vars alone do nothing (#448).** Compose v5 rejects an empty
`name:` default on the base file at `up` ("invalid volume name or ID: value is
empty" — a project-level definition, so it fails for every service, and
`docker compose config` silently drops the empty key instead of erroring, so
this never showed up in config-only validation). The override now lives in the
opt-in overlay `deploy/overlays/docker-compose.adopt-volumes.yml`, so every compose
invocation on this host must add it:

```bash
docker compose -f deploy/docker-compose.yml \
               -f deploy/overlays/docker-compose.adopt-volumes.yml up -d
```

Compose treats an explicitly named volume as the same object regardless of which
key refers to it, so this is an adoption, not a copy. Nothing is renamed, and
reverting is deleting the three `.env` lines and dropping the overlay from the
`-f` chain.

## 2. Pin the host ports in `deploy/.env`

`docker-compose.nv.yml` defaulted to 18080/18443. The base file defaults to
8080/8443, so a host that relied on the NV defaults must now say so:

```dotenv
CONTROL_PORT=18080
QUASAR_TLS_PORT=18443
```

`CONTROL_PORT` may already be there; `QUASAR_TLS_PORT` almost certainly is not,
because the NV file hard-coded it. Getting this wrong moves the HTTPS listener,
which invalidates the browser's accepted certificate exception and every URL that
names the old port.

## 3. Declare the encoder and node name explicitly

The base file's defaults differ from the NV file's. Anything the host was relying
on implicitly has to become explicit:

```dotenv
QUASAR_ENCODER=vulkan       # the NVIDIA overlay's default since 2026-08-12;
                            # set nvenc here to opt back onto the NVENC path
NODE_NAME=quasar-nv-1       # a changed NODE_NAME enrolls a NEW host row
QUASAR_RENDER_NODE=/dev/dri/by-path/pci-0000:01:00.0-render
```

`NODE_NAME` matters most: the agent's identity in the `hosts` table is its name,
so a changed value silently creates a second host row and orphans the old one
along with its sessions and capacity.

## 4. Turn on the console overlay if the host drives a local display

`docker-compose.nv.yml` granted `CAP_SYS_ADMIN`, the DRM/i2c cgroup rules and the
host's audio devices unconditionally. Those now live in
`overlays/docker-compose.console.yml`, so a host that uses local display must add it:

```dotenv
QUASAR_CONSOLE=1
```

and its `compose_files` entry (in `.claude/skills/_shared/hosts.json`, if the
skills drive its deploys) becomes:

```json
["deploy/docker-compose.yml", "deploy/docker-compose.nvidia.yml", "deploy/overlays/docker-compose.console.yml"]
```

A stream-only host omits both and is *more* constrained than it was before.

## 5. Redeploy with the profile, not the host name

`deploy/redeploy.sh` takes a hardware profile now:

```bash
deploy/redeploy.sh nvidia <ref>     # was: redeploy.sh tower <ref>
deploy/redeploy.sh va     <ref>     # was: redeploy.sh hermes <ref>
```

## Verifying

```bash
# renders every combination
make config-check

# asserts the properties the base + overlay split guarantees
bash scripts/dev/test-compose-overlays.sh
```

On the host, after the redeploy:

```bash
docker volume ls | grep quasar          # the ORIGINAL volume names, still in use
docker exec <pg-container> psql -U quasar -d quasar -c 'select count(*) from users'
docker inspect <agent> --format '{{.HostConfig.CapAdd}} {{.HostConfig.SecurityOpt}}'
```

The agent capability set should be `[CAP_SYS_ADMIN]` on a console host and empty
on a stream-only one, and `SecurityOpt` should be empty in both cases — no shipped
chain weakens Docker's default seccomp profile.

## What changed in behaviour

Everything else was verified equal by rendering the old and new chains and
diffing them. The differences that remain are deliberate:

- **Env passthroughs the NV file was missing** now reach the agent:
  `QUASAR_ZEROCOPY`, `QUASAR_TARGET_USAGE`, `QUASAR_CAPTURE_H264`, `LIBVA_TRACE`,
  `QUASAR_AUDIO_DISABLED`, `QUASAR_AUDIO_NO_CLOCK`, `QUASAR_INPUT_TRACE`,
  `QUASAR_INPUT_CHANNEL_MODE`, `QUASAR_INPUT_BATCH_MS`, `QUASAR_TRACE_*`, and the
  control-plane's `QUASAR_PLACEMENT_POLICY`. All resolve to their existing
  defaults, so nothing changes until someone sets one — which previously did
  nothing at all on that host.
- **The ALSA device cgroup rule widened** from two specific minors
  (`c 116:8`, `c 116:12` — one box's HDMI audio numbering) to `c 116:*`, and the
  console overlay binds `/dev/snd` rather than two named nodes. A shipped file
  cannot encode one host's card numbering. Narrow it back per host with a local
  overlay if that matters.
- **`/dev/input` is bound on every host**, not just the one whose forked file had
  it. Controller hotplug now works everywhere; the evdev cgroup rule that gates
  access was already in the base file.
- **`init: true` (tini) applies everywhere**, so orphaned helper processes are
  reaped rather than accumulating as zombies.
- **The agent no longer mounts the source tree at `/workspace`.** No production
  agent path read it; the binary is baked into the image. This is what makes an
  image tag a complete statement of what is running.
