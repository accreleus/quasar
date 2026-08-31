# deploy/overlays — situational compose overlays

**Nothing in this directory is part of a normal install.** `deploy/README.md`
Part 1 gets a host streaming with the base file and at most one hardware
overlay; everything here is a deliberate extra you add on top for one specific
reason, and most of it exists for contributors rather than operators.

Apply one the same way as any overlay — an extra `-f` after the base file (and
after the hardware overlay, when there is one):

```bash
docker compose -f deploy/docker-compose.yml \
               -f deploy/docker-compose.nvidia.yml \
               -f deploy/overlays/docker-compose.console.yml up -d
```

| File | Adds | Who it is for |
|---|---|---|
| `docker-compose.console.yml` | DRM master (`CAP_SYS_ADMIN`), the DRM/i2c device classes, host audio | Operators running **console mode**: the GPU host drives its own monitor and speakers instead of (or alongside) streaming to a browser. Set `QUASAR_CONSOLE=1` in `deploy/.env` and `deploy/redeploy.sh` adds this file for you |
| `docker-compose.dev.yml` | The `build:` keys, the `web/dist` bind mount and the wget healthcheck | Anyone building the stack from a source tree. `deploy/redeploy.sh` adds it automatically, so you rarely type it. The base file is the production shape (published images, no build, no source mounts); this restores the development shape |
| `docker-compose.local.yml` | agentless Postgres + control-plane, no GPU | Contributors doing UI/API work on a laptop (`make up`) |
| `docker-compose.multiagent.yml` | two extra node-agents with their own identities | Multi-agent scheduling tests |
| `docker-compose.profiling.yml` | the profiling image, `PERFMON`, a seccomp exception | Taking a CPU capture. Never a deployment |
| `docker-compose.cores.yml` | `ulimit core` plus a core-dump bind mount | A crash hunt (#429) |
| `docker-compose.adopt-volumes.yml` | `QUASAR_*_VOLUME` name overrides | Adopting pre-existing named volumes (#448); `redeploy.sh` layers it automatically when those vars are set |

The contributor tooling these belong to is catalogued in
[`docs/developer-tooling.md`](../../docs/developer-tooling.md).
