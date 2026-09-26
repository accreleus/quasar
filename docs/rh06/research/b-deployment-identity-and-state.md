# RH-06 research B — how Quasar is installed, deployed, identified, and where its state lives

Snapshot: branch `develop` at `32aee45` (2026-09-24). Read-only research; no deploy, no ssh.
All paths are repo-relative. `path:line` citations were checked against the file. Anything under
**Inferences / open questions** is a reading of the code rather than something it states.

Milestone framing (GitHub milestone 6, read with `gh`): "Remove manager-owned Compose file coupling
only after recoverable replacement paths are proven." Issues: #218 "Define service ownership and
recoverable API-based update execution" and #219 "Ship minimal enrollment and explicit migration
from existing stacks". #219's acceptance says: "Managed updates no longer read/write manager .env
or depend on Compose labels/paths." Most of the couplings listed below are what that sentence
refers to.

---

## 1. Deployment topology: compose files, overlays, env, installers

### Confirmed facts

**The base file is the production deployment.**
- `deploy/docker-compose.yml:1-27`: "THE base compose file, and a complete deployment." A release
  install is "two env vars, not an overlay": `QUASAR_CONTROL_IMAGE` and `QUASAR_AGENT_IMAGE` pinned
  to digests in `deploy/.env` (`:13-18`). The old `docker-compose.release.yml` is retired.
- No `build:` key in the base file (`:71-76`). The dev overlay adds it back
  (`deploy/overlays/docker-compose.dev.yml:41-57`), and `deploy/redeploy.sh:173` always appends
  that overlay.
- Linux only, because the agent uses `network_mode: host` for WebRTC ICE (`:32-38`, `:299`).

**Services in the base file.** There are four. There is no separate web service (the SPA is baked
into the control-plane image, `:250-253`) and no TURN service.

| Service | Image (default) | Restart | Volumes | Notes |
|---|---|---|---|---|
| `quasar-postgres` | `${QUASAR_POSTGRES_IMAGE:-postgres:16-alpine}` (`:53`) | `unless-stopped` (`:65`) | named `quasar-postgres-data` (`:59`) | `POSTGRES_PASSWORD:?` required (`:57`); pg_isready healthcheck (`:60-64`) |
| `quasar-control-plane` | `${QUASAR_CONTROL_IMAGE:-quasar-control-plane:latest}` (`:77`) | `unless-stopped` (`:275`) | named `quasar-control-tls:/var/lib/quasar-control` (`:255`, "LOAD-BEARING AND MUST NEVER BE DROPPED" `:244-249`); named `quasar-updater-run:/run/quasar-updater` (`:259`) | ports `CONTROL_PORT:-8080` and `QUASAR_TLS_PORT:-8443` (`:82`, `:88`); `ENROLLMENT_TOKEN:?` required (`:113`); `depends_on` postgres healthy (`:260-262`); curl `/health` healthcheck (`:263-274`) |
| `quasar-node-agent` | `${QUASAR_AGENT_IMAGE:-${QUASAR_NODE_IMAGE:-quasar-node-agent:latest}}` (`:288`) | `unless-stopped` (`:658`) | see table below | `network_mode: host` (`:299`); `cap_add: [NET_ADMIN, SYSLOG]` (`:318`); `init: true` (`:322`); devices `/dev/dri`, `/dev/uinput`, `/dev/kmsg:r` (`:631-647`); cgroup rule `c 13:* rmw` (`:648-654`); `depends_on` control plane healthy (`:655-657`) |
| `quasar-updater` | `${QUASAR_UPDATER_IMAGE:-quasar-updater:latest}` (`:670`), a tag rather than a digest on purpose (`:666-668`) | `unless-stopped` (`:698`) | docker socket `${QUASAR_DOCKER_SOCKET:-/var/run/docker.sock}` (`:684`); stack dir bind `${QUASAR_STACK_DIR}` mounted at the same path (`:685-693`); named `quasar-updater-run` (`:694`) | `security_opt: label=disable` (`:697`); no healthcheck |

Node-agent mounts (`deploy/docker-compose.yml:564-625`):

| Mount | Kind | Purpose |
|---|---|---|
| `/var/run/docker.sock` (`:568`) | host bind | Sibling app and audio containers through the Engine API ("never dials Docker on the network", `:565-567`) |
| `/run/quasar-agent` (`:572`) | host bind (fixed path, not a variable) | XDG runtime dir. Must be a host path so sibling bind mounts resolve (`:569-571`) |
| `/dev/input` (`:577`) | host bind | Controller hotplug |
| `${QUASAR_HOME_ROOT:-/tmp/quasar-homes-unset}` mounted at the same path (`:581`) | host bind | Managed homes |
| `${QUASAR_TEMPLATE_ROOT:-/var/lib/quasar/templates}` mounted at the same path (`:593`) | host bind | Home templates (#488) |
| `/etc/os-release`, `/dev`, `/sys/kernel` → `/host/...:ro` (`:600`, `:609`, `:619`) | host bind, read-only | Readiness facts |
| `quasar-agent-data:/var/lib/quasar-agent` (`:621`) | **named volume** | "the per-node secret issued at enrollment" (`:620`) |
| `quasar-updater-run:/run/quasar-updater` (`:625`) | named volume | The agent POSTs `release_apply` to the updater socket (`:622-624`) |

Project volumes are declared at `:700-714` with no `name:` override. Compose therefore names them
`<project>_<key>` (`:701-710`).

**Networks.** No `networks:` block anywhere. Postgres and the control plane share the default
project network (`DATABASE_URL` host `quasar-postgres`, `:90-91`). The agent reaches the control
plane at `ws://localhost:${CONTROL_PORT:-8080}` (`:325`), so the agent must run on the same machine
as the control plane's published port.

**Labels.** The compose files set no custom labels. The Compose-generated labels
(`com.docker.compose.project`, `.service`, `.project.working_dir`, `.project.config_files`) are
read at run time by:
- the updater (`control-plane/internal/updater/discover.go:18-35`);
- the agent's install discovery (`node-agent/src/buildinfo.rs:103-106`, `:219-266`);
- `deploy/redeploy.sh` (`:411-413`, `:506-508`, `:625`);
- the site installer's takeover guard (`site/src/data/stack-template.js:315-334`).

**Docker socket.** Mounted into the agent (`:568`, fixed path) and into the updater (`:684`,
overridable for Podman). The control plane has no socket. It reaches its own updater only through
the shared `quasar-updater-run` volume (`:259`).

**NVIDIA overlay** (`deploy/docker-compose.nvidia.yml`):
- adds `gpus: all` (`:65`), `NVIDIA_DRIVER_CAPABILITIES` (`:69`), `QUASAR_GPU_NVIDIA`,
  `QUASAR_CUDA_DEVICE` (`:77-78`), `QUASAR_NVIDIA_DRIVER_VOLUME:-1` (`:93`), `QUASAR_CUDA_RUNTIME:-1`
  (`:101`), and a fixed `LD_LIBRARY_PATH` into the driver volume (`:117`);
- adds the named volume `quasar-nvidia-driver:/opt/quasar/nvidia-driver` (`:134`, `:136-138`).
  "The agent resolves this mount to find both the volume's name and its host path, which is also
  how it discovers that the overlay was applied at all" (`:129-133`).

**Hardened overlay** (`deploy/docker-compose.hardened.yml`):
- removes the control-plane ports (`:5`, `ports: !reset []`) and sets `QUASAR_ENV: production`
  (`:13`);
- adds a `quasar-edge` Caddy service pinned by digest (`:33-34`) with `${QUASAR_TLS_DIR:?}:/certs:ro`
  (`:43`), `read_only`, `cap_drop: ALL` (`:47-54`) and `restart: unless-stopped` (`:55`).
- Operator doc: `docs/operations/hardened-deployment.md:17-41`. It notes "Quasar administrators are
  host administrators while the node agent can access the Docker socket" (`:3-5`).

**Situational overlays** (`deploy/overlays/README.md:17-25`):
- `console` adds `SYS_ADMIN`, `/dev/snd` and DRM/i2c cgroup rules (`docker-compose.console.yml:31-47`).
  redeploy adds it when `QUASAR_CONSOLE=1` (`deploy/redeploy.sh:175-180`).
- `dev`: build keys, `../web/dist` bind, wget healthcheck.
- `local`: agentless Postgres plus control plane on loopback ports, with dev default credentials
  (`docker-compose.local.yml:25-104`, `:32`, `:65`, `:78-80`).
- `multiagent`: two extra agents via `extends`, each with its own `NODE_NAME`, `NODE_SECRET_PATH`,
  `XDG_RUNTIME_DIR` and `quasar-agent-N-data` volume (`docker-compose.multiagent.yml:36-72`).
- `profiling`, `cores`.
- `adopt-volumes`: `name:` overrides for postgres, agent and control-tls **only**. There is none for
  `quasar-updater-run` or `quasar-nvidia-driver` (`docker-compose.adopt-volumes.yml:28-34`).
  redeploy adds it when all three vars are set and refuses a partial set (`deploy/redeploy.sh:182-201`).

**Combined vs separate hosts.**
- *Combined host* (control plane, Postgres, agent and updater on one machine) is the base file's
  only shape. The base file's agent cannot be a second-host install: "its agent depends on the local
  stack" (`deploy/README.md` "Manual / air-gapped path", in the section at `:740-806`).
- *Additional GPU host*: `deploy/enroll-host.sh`, served by the control plane at `/enroll-host.sh`
  (vite copies it into the SPA, `web/vite.config.ts:97-112`; `deploy/Dockerfile.control.prod:36-37`).
  - It writes `/opt/quasar-agent/docker-compose.yml`, containing the node-agent and updater services
    only (`deploy/enroll-host.sh:189-313`), and a 0600 `.env` holding `QUASAR_ENROLLMENT`,
    `NODE_NAME`, image refs, `QUASAR_STACK_DIR`, `COMPOSE_PROJECT_NAME` (default `quasar-agent`) and
    others (`:739-763`).
  - It starts the updater before the agent (`:863-889`).
  - The printed agent service is held identical to the base file's by `TestEnrollHostComposeMatchesBase`
    (`control-plane/cmd/quasar-control/enroll_host_compose_test.go:107`).
- *Control-only host* (control plane without a local agent): **no supported production shape
  exists.** The only agentless file is the contributor overlay `docker-compose.local.yml`
  (`deploy/overlays/README.md:21`). #218 explicitly asks for control-only hosts.

**Env templates and generated `.env`.**
- `deploy/.env.example`: `POSTGRES_PASSWORD` and `ENROLLMENT_TOKEN` ship commented out so that a
  placeholder can never deploy (`:5-35`). `QUASAR_SECRET_KEY` is "strongly recommended" (`:37-53`).
  The agent-link block covers `QUASAR_ENROLLMENT`, `CONTROL_PLANE_URL`, `CONTROL_PLANE_FINGERPRINT`
  and `QUASAR_ALLOW_PLAINTEXT_AGENT` (`:156-173`). The updater block covers `QUASAR_UPDATER_IMAGE`,
  `QUASAR_STACK_DIR`, namespaces, the socket, and signatures (`:270-313`). Volume adoption is at
  `:373-391`.
- Stale text: `:369-371` still says "Digest-pinned release artifacts (docker-compose.release.yml)".
- **`deploy/redeploy.sh`** (source path; runs on the host from a git checkout). It:
  - does `git fetch/checkout` (`:225-238`);
  - builds the compose chain (`:156-204`);
  - generates the secrets into `deploy/.env` and guards each one (details in §3);
  - seeds `QUASAR_STACK_DIR` (`:564-593`), `QUASAR_HOME_ROOT` on fresh installs (`:640-680`) and
    `QUASAR_TLS_HOSTS` via `deploy/seed-tls-hosts.sh` (`:729`);
  - builds the updater image with a direct `docker build` (`:795-803`);
  - brings the updater up before the agent (`:884-890`);
  - verifies the updater's `/v1/self` `working_dir` against the stack dir (`:964-1001`);
  - prints a machine-readable `REDEPLOY …` line (`:1246`).
  - The default compose project is `deploy` (`:352-353`).
- **Public site installer** (`site/src/data/stack-template.js`), a third install path:
  - generates one compose file from the base file and the NVIDIA overlay (`:75-140`);
  - replaces `DATABASE_URL` with the split `QUASAR_DATABASE_*` variables (`:98-104`);
  - makes the image refs and `QUASAR_STACK_DIR` `:?`-required (`:93-97`);
  - moves optional knobs to `control.env` / `agent.env` (`:123-128`);
  - generates credentials only when no `.env` exists (`:399-410`);
  - resolves digests from the stable release manifest (`:219-251`);
  - refuses to take over a stack whose `working_dir` label differs (`:315-334`).
  - The stack dir is derived from a base path (`/var/lib/quasar/deploy`, or
    `/mnt/user/appdata/quasar/deploy` on Unraid), because the credentials in `.env` are "the only
    copy" (`:33-46`; `site/src/data/platforms.js:53`, `:85`).
  - Documented in `docs/configuration.md:1760-1790`.
- **Unraid.** There is no Unraid Community Applications XML template or any other manager template in
  the repo (`git ls-files` has no `*.xml`, portainer, helm or k8s files). Unraid is a platform choice
  inside the site installer (`site/src/data/platforms.js:78-90`: `/boot/config/go` persistence and
  uid 99:100). Kubernetes is deferred (`docs/future/kubernetes-native.md:1-30`).

**Operations docs.**
- `docs/operations/compose-consolidation-migration.md`: moving off forked compose files is
  "all … `.env` changes" (`:17-20`). Volume adoption is step 1 (`:32`). It warns that `NODE_NAME`
  *is* the identity: "a changed value silently creates a second host row and orphans the old one"
  (`:81-94`).
- `docs/operations/database-backup-restore.md`: a disposable drill (`deploy/db-backup-restore-drill.sh`).
  The "upgrade-from-last-supported" gate is **SKIP / blocked**, because there is no supported-version
  manifest (`:33-41`).
- `docs/operations/hardened-deployment.md`: summarised above.

### Inferences / open questions
- **(inference)** Four install paths share state conventions but differ in the details: base
  compose (registry path A), `redeploy.sh` (source path B), `enroll-host.sh` (extra GPU host), and
  the site installer. One example: the site installer uses the `QUASAR_DATABASE_*` split and
  `*.env` side files, and the others do not. RH-06's "explicit migration from existing stacks"
  (#219) has at least these four origins to recognise.
- **(inference)** Postgres sits in the same project as the control plane and is reached by the
  Compose service DNS name. A control-only host or an external database already works through
  `DATABASE_URL`/`QUASAR_DATABASE_HOST` (`control-plane/internal/config/database.go:13-41`), but no
  shipped file demonstrates it.
- **(inference)** The base file makes `ENROLLMENT_TOKEN` compose-required (`:113`, `:326`) even
  though the control plane treats it as optional (`docs/configuration.md:37`). Every combined host
  therefore carries a break-glass "enroll any node name" credential in its `.env`.

---

## 2. Node-agent ↔ control-plane enrollment and identity today

### Confirmed facts

**Identity = `node_name` + `node_secret`. There is nothing else.**
- The `hosts` row is keyed by `node_name TEXT NOT NULL UNIQUE`, with `node_secret_hash`
  (`control-plane/migrations/0001_initial_schema.up.sql:64-67`).
- There is no machine-id, hardware fingerprint, client certificate or agent keypair.
- The only certificate involved is the control plane's own TLS leaf, which the agent pins by SHA-256
  fingerprint.
- Protocol end state: "replaces the shared enrollment token with mTLS / SPIFFE identities; the
  message shape … does not change" (`protocol/agent-api.md:275-276`).

**Agent config** (`node-agent/src/config.rs:37-72`):
- `NODE_NAME` defaults to the system hostname (`:38`, `:118-132`).
- `NODE_SECRET_PATH` defaults to `/tmp/quasar-{node_name}-secret` (`:39-40`). Compose sets
  `/var/lib/quasar-agent/node-secret` on the `quasar-agent-data` volume
  (`deploy/docker-compose.yml:328`, `:621`).
- It also reads `QUASAR_ENROLLMENT`, `CONTROL_PLANE_URL`, `CONTROL_PLANE_FINGERPRINT`,
  `ENROLLMENT_TOKEN` (empty is treated as unset) and `QUASAR_ALLOW_PLAINTEXT_AGENT` (`:45-50`).
- The persisted pin is `<NODE_SECRET_PATH>.tls` (`:51`).

**Files beside the secret, all derived from `NODE_SECRET_PATH`:**

| File | Contents | Source |
|---|---|---|
| `<path>` | the `node_secret` | `node-agent/src/agent.rs:5385-5405` |
| `<path>.tls` | the control-plane certificate pin | `node-agent/src/enrollment.rs:26-28` |
| `<path>.images.json` | managed-image state | `node-agent/src/config.rs:90-95` |
| `<path>.policy.json` | RH05 restart-journal / policy | `node-agent/src/agent.rs:144` |
| `<path>.runtime-images` | runtime image state | `node-agent/src/agent.rs:149-151` |
| `<path>.container-owner` | RH-01 ownership lease | `node-agent/src/container_ownership.rs:50-59`; `docs/configuration.md:337` |

The secret is written 0600 and truncated on rewrite (`node-agent/src/agent.rs:5385-5405`).

**Enrollment string** `qenr1.<FP>.<base64url(wss-url)>.<token>` (`node-agent/src/enrollment.rs:3`):
- It must be `wss://` (`:165`).
- `CONTROL_PLANE_URL` overrides the string's URL (`:289-298`).
- A conflicting `ENROLLMENT_TOKEN` is fatal (`:315-320`).
- `CONTROL_PLANE_FINGERPRINT` overrides the string's pin (`:353-363`), which is the pin-rotation path
  (`docs/configuration.md:336`).
- Under a pin, SAN and expiry are not checked (`node-agent/src/enrollment.rs:199-202`).

**Credential choice** (`node-agent/src/agent.rs:5033-5055`):
- A saved secret wins and is sent as `Auth::Reconnect`. Otherwise the token is sent as
  `Auth::Enrollment` (`node-agent/src/messages.rs:596-602`).
- With neither, the agent exits non-zero with `boot-enrollment-unconfigured` (`agent.rs:128-134`;
  `docs/configuration.md:338`).
- HTTP side channels use `Authorization: Bearer <node_secret>` plus `X-Quasar-Node`
  (`node-agent/src/cp_http.rs:220-221`), checked against `hosts.node_secret_hash`
  (e.g. `control-plane/internal/storage/agent_gc.go:46-56`).

**Control-plane handshake:**
- The endpoint is `GET /agent/ws` (`control-plane/internal/agentws/handler.go:667-668`), with a
  per-IP failure limiter (`:49-53`, `:672-675`).
- The first message must be `register` within 15 s (`:42`, `:1312-1331`).
- `resolveAuth` routes a token to `enrollHost` and a secret to `reconnectHost` (`:1908-1919`).
- `enrollHost` (`control-plane/internal/agentws/store.go:93-202`):
  1. Compares the static token in constant time (`:105-106`), or redeems a minted token in the same
     transaction (`:125-137`).
  2. Generates a new 32-byte secret and stores its SHA-256 (`:108-114`).
  3. Takeover guard: `SELECT … FOR UPDATE` by `node_name`, refused while the host is live (`:140-152`).
  4. `INSERT … ON CONFLICT (node_name) DO UPDATE` replaces the hash and resets the restart tally and
     capacity state (`:162-182`).
- `reconnectHost` looks up by `node_name` and compares the hash (`:217-251`).
- `registered` carries `host_id` and returns `node_secret` only on enrollment
  (`control-plane/internal/agentws/handler.go:1398-1402`).

**Minted enrollment tokens** (#12, since 2026-09-03):
- Table `host_enrollments`: `token_hash UNIQUE`, optional `node_name` binding, `max_uses` (default 1),
  `used_count`, `expires_at`, `revoked_at` (`control-plane/migrations/0072_host_enrollment_tokens.up.sql:15-29`).
  Migration 0073 adds `used_by_node_name` (`0073_host_enrollment_used_by.up.sql:21`).
- Tokens are 32 random bytes as base64url, stored as SHA-256. They default to single use with a
  one-hour expiry (`control-plane/internal/hostenroll/store.go:31`, `:37`, `:80-92`, `:113-121`).
- `Redeem` is a single `UPDATE … RETURNING` that returns one generic error on any failure (`:207-227`).
  A refused takeover gives the use back through the rollback (`agentws/store.go:139-142`).
- Admin API (`control-plane/internal/hostenroll/handler.go:31-35`):
  `POST/GET /v1/admin/hosts/enrollments` and `DELETE …/{id}` (revoke). Mint accepts `node_name`,
  `max_uses` 1–100, `expires_at` up to 30 days, and `note` (`:55-58`, `:66-119`).
- The web UI (`EnrollHostModal.tsx:123`) always mints an any-node, single-use, one-hour token. It has
  no list or revoke UI (`web/src/api/admin.ts:258-282` has no callers).

**Static `ENROLLMENT_TOKEN`:** optional in Go, and empty means minted tokens only
(`control-plane/internal/config/config.go:40-42`, `:288-291`). Compose requires it. Docs call it
"break-glass" that "can enroll *any* node name" (`docs/configuration.md:37`).

**Rotation and revocation.**
- A node secret changes only through re-enrollment.
- There is no "revoke this host's credential" endpoint. `DELETE /v1/hosts/{id}` refuses while the
  host is connected or has sessions (`control-plane/internal/crud/handler.go:1241-1254`).

**Re-registration with lost state:**
- *Same name, secret lost, token available.* The existing row is re-keyed with no duplicate
  (`ON CONFLICT`). If the row still reads as live, the takeover guard refuses
  (`control-plane/internal/agentws/store.go:143-152`).
- *Secret lost, only the spent UI token in `.env`.* The result is `auth_failed`, and a new token must
  be minted (`deploy/enroll-host.sh:975`).
- *Changed `NODE_NAME`.* A new row is created and the old one is orphaned
  (`docs/operations/compose-consolidation-migration.md:92-94`).
- *Secret from a different or restored control plane (#199).* `host_not_found`; the agent retries
  once with the configured token and overwrites the secret (`node-agent/src/agent.rs:5097-5156`).
  With no token it logs `cp-register-stale-identity-unresolvable` (`:5073-5079`).
  `deploy/enroll-host.sh --reset-identity` / `QUASAR_RESET_IDENTITY=1` removes the
  `<project>_quasar-agent-data` volume (`deploy/enroll-host.sh:57`, `:92-95`, `:846-855`).

**`register` identity fields** (`protocol/agent-api.md:282-291`, `:360-392`):
- `node_name`, `agent_version` and `auth`.
- Optional `source_commit`, `built_at`, `install_mode` and `updater_present`, all replaced wholesale on
  every register. A host with any of them NULL is "never eligible for a platform-release apply".

**`BOOTSTRAP_ADMIN_*`:**
- Read at `control-plane/internal/config/config.go:293-295`; run on every boot
  (`control-plane/cmd/quasar-control/app.go:355-365`).
- `EnsureBootstrapAdmin` (`control-plane/internal/auth/bootstrap.go:51-70`;
  `control-plane/internal/auth/store.go:278-330`):
  - takes a Postgres advisory lock and does nothing if any admin exists;
  - otherwise promotes the account with that email, or creates a new admin;
  - treats a partial set as fatal.
- If there is still no admin, a per-boot setup token is written to `/run/quasar/setup-token` (0600),
  which the first-run wizard claims (`app.go:373-399`; `control-plane/internal/setup/config.go:38`).
  `redeploy.sh` deliberately does not generate these (`deploy/redeploy.sh:292-294`).
- This covers user accounts only; host enrollment is unrelated.

### Inferences / open questions
- **(inference)** For RH-06 "preserve identity during adoption" (#219), the whole host identity is
  **one file** (the `node-secret` in the `<project>_quasar-agent-data` volume) **plus the exact
  `NODE_NAME` string**. The volume name depends on the Compose project name. So a change of manager
  or project (the default project is `deploy` on the combined host and `quasar-agent` on enrolled
  hosts) silently loses the identity unless the volume is adopted.
- **(inference)** RH-01 ownership (`.container-owner`) and RH05 policy (`.policy.json`) share that
  same volume. Losing the volume costs more than the secret: it also costs container ownership of
  any surviving session containers, and the restart journal.
- **(inference)** Nothing records which *deployment* (compose project, manager, stack dir) a host
  row came from. The control plane learns only `install_mode` and `updater_present`.
- Open: after a control-plane crash, rows stay "live" (`agent_disconnected_at IS NULL`) until a
  process marks them. Could that block a legitimate re-enrollment during RH-06 adoption?

---

## 3. Secrets

### Confirmed facts

| Secret | Origin | Persisted where | Generated by | Loss consequence |
|---|---|---|---|---|
| `POSTGRES_PASSWORD` | `deploy/.env` → compose interpolation into `DATABASE_URL` (`deploy/docker-compose.yml:57`, `:90-91`), or `QUASAR_DATABASE_PASSWORD` (`control-plane/internal/config/database.go:21-23`) | `.env` plus Postgres's own auth inside the data volume | `redeploy.sh` (`openssl rand -hex 24`, `:525`) only when no postgres container or volume exists, otherwise it refuses (`:495-523`); site installer (`stack-template.js:400`) | The control plane cannot open the DB. Postgres ignores a new env password once initialised (`stack-template.js:318-321`) |
| `ENROLLMENT_TOKEN` (static) | `.env` → both CP and agent env (`deploy/docker-compose.yml:113`, `:326`) | `.env` only | `redeploy.sh` (`openssl rand -hex 32`, `:543-560`); site installer (`:401`) | Only first enrollment is affected. Enrolled agents keep reconnecting with their node secret |
| Minted enrollment tokens | Admin API | `host_enrollments.token_hash` (SHA-256) | control plane | Single use, one hour |
| `node_secret` | Minted at enrollment (`control-plane/internal/agentws/store.go:108-114`) | Plaintext in the agent's `NODE_SECRET_PATH` file (named volume); SHA-256 in `hosts.node_secret_hash` | control plane | Host must re-enroll with a fresh token (§2) |
| Bearer / session tokens | 32 random bytes (`control-plane/internal/auth/token.go:11-31`) | `auth_tokens.token_hash` | control plane | Users log in again. **There is no JWT and no server signing key** (only HMAC for the webhook, `control-plane/internal/platform/notify_client.go:163`) |
| `QUASAR_SECRET_KEY` (+`_PREVIOUS`) | env (`control-plane/internal/config/config.go:509-510`); AES-256-GCM (`control-plane/internal/secrets/keyring.go:100-112`) | `.env` only. The ciphertexts are in `instance_secrets` (migration 0040) | *Not* generated by the control plane on purpose (`deploy/docker-compose.yml:186-194`). `redeploy.sh` generates it only when `instance_secrets` is empty or absent, and refuses when rows exist (`:355-470`); the site installer makes it required and generates it (`stack-template.js:103`, `:402`) | Stored secrets (SteamGridDB key, release-webhook secret; `control-plane/internal/secrets/registry.go:80-107`) are unrecoverable |
| Control-plane TLS pair | Self-signed on first boot, or operator `QUASAR_TLS_CERT/KEY` (`docs/configuration.md:41-46`) | `quasar-control-tls` volume, `/var/lib/quasar-control/tls` | control plane | A new fingerprint. **Every pinned agent stops connecting** (`cp-tls-pin-mismatch`) until `CONTROL_PLANE_FINGERPRINT` is set per host (`docs/configuration.md:336`) |
| Agent's CP certificate pin | Enrollment string or `CONTROL_PLANE_FINGERPRINT` | `<NODE_SECRET_PATH>.tls` | agent | Falls back to string or env; if none, WebPKI |
| Setup token | Per boot (`control-plane/cmd/quasar-control/app.go:373-399`) | `/run/quasar/setup-token` in the container filesystem (not a volume) | control plane | Regenerated on each boot |
| Dev agent key | Per boot (`control-plane/cmd/quasar-control/main.go:121-133`) | `/run/quasar/dev-agent-key` | control plane | Dev only; refused with `QUASAR_ENV=production` (`control-plane/internal/devauth/config.go:79`) |
| `QUASAR_PLATFORM_RELEASE_TOKEN` | env (`control-plane/internal/platform/github.go:269-272`) | `.env` | operator | Anonymous GitHub rate limit applies |
| `QUASAR_PLATFORM_RELEASE_WEBHOOK_SECRET` | env fallback; the DB-stored value wins (`control-plane/internal/secrets/store.go:262-275`) | `.env` / `instance_secrets` | operator | Unsigned webhook |
| TURN credentials | Static JSON in `QUASAR_ICE_SERVERS` (`control-plane/internal/ice/ice.go:25-26`, `:115-122`) | `.env` | operator | No TURN server ships (`deploy/docker-compose.yml:155-156`) |
| Release signing key | Private key only in CI (`.github/workflows/images.yml:1400-1425`); hosts hold public keys in `QUASAR_UPDATER_TRUSTED_KEYS` | `.env` (public) | release pipeline | n/a |
| Registry credentials | **None.** GHCR is anonymous (`control-plane/internal/images/digest.go:290-294`); private registry is #9 | — | — | — |

### Inferences / open questions
- **(inference)** Three secrets are irreplaceable and live **only** in the manager-owned `.env`
  (`POSTGRES_PASSWORD`, `QUASAR_SECRET_KEY`, and in practice `ENROLLMENT_TOKEN`). The docs say so
  (`site/src/content/docs/install/install.mdx:41-48`). Once RH-06 stops managed updates from reading
  or writing that `.env` (#219), it needs a new, durable home for these secrets, or an explicit rule
  that they stay operator-owned env.
- **(inference)** The control plane's database credential enters only through env. Nothing in the
  database or the volumes can reconstruct it.

---

## 4. Homes, volumes and host paths

### Confirmed facts

**Managed homes (P5):**
- Provider `auto|local`. The docker-volume driver is hard-removed (#473;
  `control-plane/internal/storage/storage.go:1-19`, `:219-235`, `:251`).
- The home ref is `{root}/{userSlug}/{appSlug}`, synthesised once and reused, so a rename never
  orphans it (`control-plane/internal/storage/storage.go:96-104`).
- Per-host root precedence, first non-empty wins (`control-plane/internal/hostcfg/store.go:69-94`;
  wired at `control-plane/cmd/quasar-control/app.go:484-496`):
  1. admin override `host_settings.overrides.home_root`;
  2. agent-reported `hosts.effective_settings.home_root`;
  3. the control plane's `QUASAR_HOME_ROOT`.
- The agent reads `QUASAR_HOME_ROOT` (`node-agent/src/session/settings.rs:302`). The agent container
  must see the host path at the same path (`deploy/docker-compose.yml:578-581`).
- `redeploy.sh` seeds `/var/lib/quasar/homes` on fresh installs (`deploy/.env.example:227-228`;
  `deploy/redeploy.sh:640-680`). Unraid needs a persistent share, because `/var/lib` is RAM-backed
  (`deploy/.env.example:221-225`).
- Rows: `user_homes`. RH05 adds canonical home claims (migration 0090).

**Templates.** `QUASAR_TEMPLATE_ROOT` defaults to the sibling of the home root
(`node-agent/src/session/template.rs:265`, test at `:878-880`). It is a host bind at the same path
(`deploy/docker-compose.yml:582-593`).

**NVIDIA driver volume:**
- Named volume `quasar-nvidia-driver` at `/opt/quasar/nvidia-driver` (`node-agent/src/nvidia_volume.rs:44`),
  used only with the NVIDIA overlay. `QUASAR_NVIDIA_DRIVER_VOLUME` gates provisioning (`:336`).
- The agent finds the volume's host path by inspecting **its own container's mounts**
  (`node-agent/src/nvidia_volume.rs:359-369`, `self_container_id` at `:414-428`).
- Escape hatch: `QUASAR_NVIDIA_DRIVER_HOST_PATH`, verified with a probe container
  (`node-agent/src/nvidia_volume/host_path.rs:9-50`).

**CUDA runtime.** NVRTC is fetched into `<driver volume>/cuda/lib64` (`deploy/docker-compose.nvidia.yml:94-101`).
`QUASAR_CUDA_RUNTIME` and `QUASAR_CUDA_RUNTIME_DIR` apply (`node-agent/src/cuda_runtime.rs:218`, `:226`).
The `LD_LIBRARY_PATH` pointing at both halves is fixed in compose, because the loader latches it at
execve (`deploy/docker-compose.nvidia.yml:102-117`).

**Other volumes and paths:**
- Postgres data: named `quasar-postgres-data` (`deploy/docker-compose.yml:59`).
- Control state: named `quasar-control-tls` at `/var/lib/quasar-control`. It holds the TLS pair plus
  the artwork cache (`QUASAR_ARTWORK_DIR` default `/var/lib/quasar-control/artwork`, `:199`, `:713`).
  The site installer documents a bind-mount alternative with entrypoint chown
  (`docs/configuration.md:1791-1799`).
- Agent state: named `quasar-agent-data` at `/var/lib/quasar-agent` (`:621`). The agent treats it as
  its data root (`node-agent/src/capacity.rs:33`).
- Updater run: named `quasar-updater-run` (socket 0666 plus result files, `:714`).
- Host runtime dir: bind `/run/quasar-agent` (fixed path; two agents on one engine would share it,
  `deploy/README.md` "A second install on the same host" §, `:1043-1071`).
- Session and app images: pulled by the agent. Tag or digest per catalog `runtime_spec.image`
  (CONTEXT "Session image", `CONTEXT.md:275-281`). The PulseAudio sidecar follows the agent image
  (`deploy/docker-compose.yml:543-548`).
- Volume names are always `<project>_<key>` unless the adopt-volumes overlay renames them
  (`deploy/docker-compose.yml:700-710`).

### Inferences / open questions
- **(inference)** `QUASAR_HOME_ROOT` and `QUASAR_TEMPLATE_ROOT` "same path inside and outside" and
  `/run/quasar-agent` are docker-out-of-docker constraints. They hold whoever creates the agent
  container, but a new owner must reproduce them exactly.
- **(inference)** The agent's host-path discovery for the driver volume depends on being a
  container whose own mounts Docker can inspect. A non-container agent, or a runtime without
  inspect, needs `QUASAR_NVIDIA_DRIVER_HOST_PATH`.

---

## 5. RH-01 runtime ownership

### Confirmed facts

**Runtime interface.**
- `node-agent/src/runtime.rs` is the "Quasar-owned engine discovery and bounded image operations"
  facade (`:1`). The Docker adapter is `node-agent/src/runtime/docker.rs`, on Bollard, with its
  types kept private (`:1`, submodules `:6-15`).
- One single-worker executor per process, with at most 4 in flight and a 30 s default deadline
  (`runtime.rs:122-131`, `:381-386`). Engine API floor 1.40 (`:251-254`).

**Socket discovery** (`node-agent/src/runtime.rs:136-194`):
- It refuses `DOCKER_CONTEXT`, `DOCKER_TLS*` and `DOCKER_API_VERSION` (`:137-146`).
- `QUASAR_CONTAINER_RUNTIME` is retired and only warns (`:150-156`).
- `DOCKER_HOST` must be `unix://<abs>` (`:157-158`, `:196-204`).
- A non-default `currentContext` in `.docker/config.json` is refused (`:164-182`).
- The fallback is `/var/run/docker.sock` (`:190`).
- Registry credentials come from `DOCKER_CONFIG` / `HOME` `.docker/config.json` (`:225-229`).

**Ownership labels:**
- `io.quasar.agent-owner=<64-hex token>`. The token is generated once into
  `${NODE_SECRET_PATH}.container-owner` and `flock`ed for the life of the process
  (`node-agent/src/container_ownership.rs:10`, `:24-30`, `:85-116`).
- Per-operation labels:
  - `io.quasar.application-operation` for app containers (`node-agent/src/runtime/docker/application.rs:31`, `:328-330`);
  - `io.quasar.runtime-operation` for helpers such as audio and probes (`node-agent/src/runtime/docker/helpers.rs:27`, `:1096-1098`);
  - `io.quasar.build-operation` on locally built images (`node-agent/src/runtime/docker/build.rs:6`, `:58-59`).
- Owned name prefixes: `quasar-sess-`, `quasar-pulse-`, `quasar-probe-` (`container_ownership.rs:11-16`).
- `owned_id` requires a full id, an owned prefix **and** an exact owner label (`:127-145`).
- Stop and remove re-check the labels first (`application.rs:1102-1125`, `:1400-1404`; `helpers.rs:714-722`).

**Legacy sweep** (`node-agent/src/runtime/docker/legacy.rs`; called at boot from `node-agent/src/agent.rs:4797-4826`):
- Runs only after API-owned applications are retired (`agent.rs:4772-4788`), and only for the
  `quasar-sess-` prefix.
- Lists by owner label, then re-inspects each candidate by id (`legacy.rs:29-67`).
- Skips operation-labelled, `quasar-pulse-` and `quasar-probe-` containers (`:130-158`).
- Force-removes with `v:false` (`:76-84`) and counts a container removed only on a 404 re-inspect
  (`:94-108`).
- Everything foreign or unlabelled is preserved and counted (CONTEXT "Legacy container",
  `CONTEXT.md:104-115`).

**Does the agent touch its own container, the CP's, Postgres or the updater?**
- **Its own container: reads only.**
  - `self_container_id()` uses `/proc/self/mountinfo`, then a container-id-shaped `$HOSTNAME`
    (`node-agent/src/nvidia_volume.rs:414-433`).
  - It then inspects itself for the image ref and labels (install mode and updater presence,
    `node-agent/src/buildinfo.rs:136-160`, `:219-266`), for mounts (driver volume,
    `nvidia_volume.rs:359-370`; storage liveness `node-agent/src/session/storage_liveness.rs:30-35`;
    disk `node-agent/src/images/disk.rs:41-46`), and for its own image id
    (`node-agent/src/session/container.rs:782-800`).
  - It **runs its own image** as sibling containers: the Pulse sidecar unless `QUASAR_PULSE_IMAGE` is
    set (`node-agent/src/session/audio.rs:91-100`) and host probes
    (`node-agent/src/host_probe/runner.rs:291`).
  - It **restarts itself only by exiting** and relies on `restart: unless-stopped`. This happens on the
    `restart` control message (`agent.rs:2383-2393`), after NVIDIA driver provisioning
    (`nvidia_volume.rs:2513-2549`), and on an RH05 restart attempt (`agent.rs:2399-2422`).
- **CP and Postgres: never mutated.**
  - `live_containers()` snapshots every live container, "including containers Quasar does not own"
    (`runtime.rs:488-493`), for liveness and Compose-service listing.
  - Every stop/remove call site is ownership-gated (`legacy.rs:77`; `application.rs:1418`, `:1497`;
    `helpers.rs:1554`, `:1680`).
- **Updater: only over its socket.** POST `/v1/apply` and poll results
  (`node-agent/src/release/mod.rs:26-27`, `:130-140`, `:273-279`). The agent "runs no compose
  command" (`:5-7`), and `control-plane` is not appliable by an agent (`:43-46`).

**`docs/runtime-api-recovery.md`:**
- "The updater still owns its separate Compose workflow" (`:3-5`).
- Planned agent cutover (`:28-52`): record image ids ("a mutable tag alone does not identify a
  candidate"), drain, "update only the intended agent/Pulse image references in that stack's existing
  Compose configuration and recreate that agent. Preserve its project, environment,
  device/security settings, volumes and endpoint", verify an "unchanged host identity, and preserved
  mounts", then uncordon.
- "Startup retires owned work from the previous agent; it does not adopt sessions" (`:45-46`).
- Diagnostic mode when startup cleanup or the runtime fails (`:90-108`; CONTEXT "Diagnostic
  registration" `CONTEXT.md:442-444`).
- "Podman/rootless certification and running-session adoption remain separate work" (`:170-171`).

**Session images.**
- `session_assign` `app.image` is passed to `runtime.ensure_image(…, 600s)`
  (`node-agent/src/agent.rs:3627-3631`). A local copy wins with no registry check
  (`node-agent/src/runtime/docker.rs:201-204`), and a missing tag defaults to `latest` (`:219-223`).
- Managed catalog images (`image_ensure`) must be `@sha256:` or `:sha-<hex>`
  (`node-agent/src/images/mod.rs:286-311`).
- Platform images in `release_apply` are digest-only (`node-agent/src/release/mod.rs:552-557`;
  ADR 0001).

### Inferences / open questions
- **(inference)** The agent has zero authority over platform containers, its own included. It can
  only exit, and a restart policy brings it back. #218's line "A restart policy is not an image
  updater" is exactly the gap: today image replacement for the agent **and** the control plane is
  delegated to the Compose-driven updater.
- **(inference)** The RH-01 owner token lives in the same volume as the node secret and is tied to
  `NODE_SECRET_PATH`. An RH-06 migration that moves agent state must move `.container-owner`
  together with the secret. Otherwise surviving `quasar-sess-*` containers become "foreign" and are
  preserved rather than retired.
- **(inference)** The engine client refuses non-unix endpoints and Docker contexts. Any RH-06 owner
  that talks to a remote engine (for example the control plane recreating an agent on another host)
  cannot reuse this client unchanged.

---

## 6. RH-05 versioned desired host state

### Confirmed facts

**Ownership split** (`docs/design/rh05-contract-proposal.md:7`): "The control plane owns desired
policy, operator revisions, app placement, admission restrictions and scheduling. The authenticated
agent owns accessible-device discovery, application evidence, durable local configuration attempts
and recovery. The existing image Ensurer remains the only managed-image executor. **Platform-image
replacement remains with the updater.**"

**Entities** (migrations `control-plane/migrations/0087`–`0094`):

| Entity | Storage | Writer |
|---|---|---|
| Deployment baseline and policy capability facts | new `hosts` columns `deployment_settings`, `deployment_settings_connection`, `config_policy_versions`, `config_policy_advertised_groups`, `config_policy_confirmed_groups`, `config_policy_ever_owned_groups`, … (`0087_host_policy.up.sql:2-14`) | agent at registration → `control-plane/internal/hostcfg/policy_connection.go:35` |
| Revision (desired spec version) | `host_policy_revisions(revision, updated_by)` (`0087:16-21`, trigger `:60-69`) | `hostcfg/policy.go:432` `SavePolicy` (compare-and-swap), `:648` `SaveLegacyPatch` |
| Choices | `host_setting_choices(key, source ∈ automatic/deployment/explicit, explicit_value, revision)` (`0087:23-32`) | same |
| Group desired vs applied | `host_setting_groups(desired_revision, desired_digest, applied_revision, applied_digest, scope, status)`; `applied` only when both match (`0087:34-48`) | `policy.go:676` `ObservePolicyApplied` |
| Reconcile obligations | `host_reconcile_obligations` (`0087:50-58`) | hostcfg |
| Admission restrictions | `host_admission_restrictions` with owner kinds manual / platform / idle_apply / recovery / legacy / reconciliation (`0088:2-13`); platform cordons backfilled from `platform_apply_runs` (`0088:15-24`) | hostcfg, platform apply |
| Idle apply: approvals, attempts, journal | `rh05_control_boot`, `host_config_approvals`, `host_config_attempts`, `host_journal_reconciliation`, `host_journal_active_snapshots`, `host_hardware_evidence`, `host_idle_inventory` (`0089:2-121`) | `hostcfg/idle_apply.go`, `idle_executor.go` |
| Home claims | `managed_home_claims` (`0090:5-27`) | scheduler |
| App placement | `app_placement(mode all_eligible/fixed, revision)` and `app_placement_hosts` (`0091:5-40`) | `crud/app_placement.go` |
| Image prep history | `host_image_success_history` (current / previous version + identity) (`0092:2-12`) | `images/host_images.go:117` |
| Image operation fences | `host_image_operation_fences(generation, state)`, `host_image_cleanup_attempts` (`0094:2-29`) | `images/cleanup.go`, `session/image_fence.go` |

- **Desired images per host are derived, not stored.** `requiredImagesForHost` joins
  `installed_images` × enabled apps × placement (`control-plane/internal/images/host_images.go:329-358`)
  and dispatches the frozen adopted reference (`:293-301`) through `image_ensure`.
- **Wire:**
  - `register` carries `config_policy_versions` / `config_policy_groups` (`node-agent/src/messages.rs:91-96`).
  - The control plane sends `ConfigPolicyOffer`, carrying attempt, boot and connection incarnations,
    group, revision, `content_sha256`, scope, expiry, prerequisites and settings (`messages.rs:1136-1150`).
  - The agent replies `ConfigPolicyState` (`:100-114`), which the control plane handles at
    `control-plane/internal/agentws/handler.go:1024-1075`. Stale incarnations are dropped
    (`:1028-1031`); `applied` requires matching revision and digest plus `agent_process_id` (`:1067-1071`).
  - Journal inventory is paginated (`messages.rs:115-123`, `:1151-1156`).
  - The agent journal is `<NODE_SECRET_PATH>.policy.json` (`node-agent/src/agent.rs:144`;
    `docs/runtime-api-recovery.md:112-127`).
- **Platform versions: none in RH-05.**
  - The contract's prerequisite facts include an "agent image digest"
    (`docs/design/rh05-contract-proposal.md:83`). The implementation emits only `accessible_device`,
    `driver_identity` and `host_probe_result` (`control-plane/internal/hostcfg/idle_hardware.go:105-107`).
  - The agent test says `// no fabricated agent-image digest` (`node-agent/src/policy.rs:1977-1980`).
  - No RH-05 table stores agent or control-plane versions. Those live in `hosts.source_commit` etc.
    (migration 0074) and `platform_apply_*` (0075+).
- **Explicit deferrals to RH-06:**
  - `docs/rh05/operator-handoff.md:41-43`: "RH06 owns first-host and additional-host enrollment
    automation, deployment identity, credentials, mount/device provisioning and Quasar platform-image
    replacement. RH05 does not reconfigure those bootstrap inputs. Session adoption across agent
    replacement belongs to RH03/RH04; private registry support is #9 …"
  - `docs/rh05/operator-handoff.md:7`: "RH05 begins **after** the agent has enrolled; it does not mint
    bootstrap credentials, install GPU drivers, change device passthrough, add bind mounts or replace
    Quasar's platform containers. Keep the agent identity and owner state on its persistent volume."
  - `docs/rh05/acceptance-map.md:49`: "41. Enrollment/mount boundary | … RH06 remains separate."
    `:58` "Q2 enrolled-host boundary | Story 41; D2; Out of Scope".
  - `docs/design/rh05-contract-proposal.md:93`: "No configuration recovery replaces a platform image,
    repairs a broken binary/runtime/mount, or overrides the updater's authority (ADRs 0002 and 0004)."
  - `docs/design/rh05-contract-proposal.md:3`, `:146`: the contract rests on an owner override, not
    Opus sign-off. It does not authorize image publication or mutation of a deployed stack.

### What RH-06 can build on
- **Revision / desired-digest / applied-digest pattern** (`host_setting_groups`), durable agent
  **journal with fsync phases and one-shot recovery** (`rh05-contract-proposal.md:93`), connection
  and boot **incarnations**, and `rh05_control_boot`. These are a ready template for #218's
  "versioned service specs and a persisted phase journal".
- **Admission restrictions with named owners** (`0088`). A platform or RH-06 owner kind already
  exists (`platform`). A deployment-migration owner could be added without inventing a new cordon.
- **Deployment baseline** (`hosts.deployment_settings`) already records the env-derived setting
  source ("Setting source" = deployment baseline, `CONTEXT.md:322-325`). That is the seam where
  moving settings out of the manager's `.env` would surface.
- **Not reusable as-is:** nothing in RH-05 stores desired *platform* image digests per host, or
  agent or CP versions. The prerequisite "agent image digest" fact was specified but deliberately not
  emitted.

### Inferences / open questions
- **(inference)** CONTEXT's "Generation" (an app container in a session) and RH-05's
  "revision"/"incarnation" already occupy the obvious words. Versioned *service* specs in RH-06 need
  a distinct term.
- Open: should the RH-05 prerequisite "agent image digest" become real under RH-06? It would bind
  idle-apply approvals to the platform image that RH-06 then owns.

---

## 7. Control-plane boot, and the updater today

### Confirmed facts

**Boot order** (`control-plane/cmd/quasar-control/main.go`):
1. `config.Load()` (`:35`)
2. dev-auth check (`:55-58`)
3. `db.Preflight` with a 10 s timeout (`:64-69`)
4. `migrate.Run(migrations.FS, cfg.DatabaseURL)` (`:73`)
5. pgx pool (`:80`)
6. TLS manager (`:92-101`)
7. `NewServices` (`:104`)
8. HTTP, HTTPS and pprof listeners (`:193-267`)

**Container entrypoint** (`deploy/control-entrypoint.sh`):
- refuses symlinked `/var/lib/quasar-control` or `/run/quasar` (`:6-10`);
- when root, chowns and drops privileges with `setpriv` (`:12-29`);
- checks both dirs are writable (`:37-41`), then execs the binary (`:44`).
- The image defaults to `USER quasar` (`deploy/Dockerfile.control.prod:113-126`).

**Postgres discovery.** `DATABASE_URL` wins. Otherwise `QUASAR_DATABASE_HOST` and
`QUASAR_DATABASE_PASSWORD` are required, with the other `QUASAR_DATABASE_*` fields defaulted
(`control-plane/internal/config/database.go:13-41`).

**Migrations.**
- Embedded (`control-plane/migrations/embed.go`; the head is `0094_host_image_operation_fences`).
- golang-migrate `schema_migrations` (`control-plane/internal/migrate/migrate.go:18-42`).
- A DB newer than the binary gives a named error (`control-plane/internal/migrate/rollback.go:18-40`).
- `buildinfo.SchemaVersion` is the highest embedded number (`control-plane/internal/buildinfo/buildinfo.go:61`, `:104`).
- One-way at deploy time (CLAUDE.md; `docs/upgrading.md` "The one-way migration rule", `:225-292`).

**Updater socket, as used by the control plane:**
- The control plane applies *itself* through the local updater at `QUASAR_UPDATER_SOCKET`
  (default `/run/quasar-updater/updater.sock`), "never over an agent connection"
  (`control-plane/internal/platform/apply_self.go:20-46`).
- It persists `updater_request_id` before the call (`apply_self.go:400-412`), `POST /v1/apply`, and
  polls results (`:162-240`).
- Success is its own next boot reporting the release commit (`Adopt`, `:546-576`).
- It learns its own install mode from the updater's `/v1/self` image map, classified by
  `ClassifyImageRef` (`:86-100`, `:305-315`, TTL 30 s at `:38`).

**Remote hosts.**
1. The control plane sends `release_apply` over the agent WebSocket (`control-plane/cmd/quasar-control/app.go:1006-1024`).
2. The agent validates it and POSTs to its local updater socket. It relays only; `control-plane` is
   not appliable by an agent, which blocks a confused deputy (`node-agent/src/release/mod.rs:1-10`,
   `:26-27`, `:43-46`).
3. Progress returns as `release_state`.
4. Success is the new agent registering with the requested commit
   (`control-plane/internal/platform/apply_runner.go:658-699`).

**What the updater does** (`control-plane/cmd/quasar-updater`, `control-plane/internal/updater`):
- **Discovers its stack from its own Compose labels:** `project`, `working_dir` and `config_files`
  (`control-plane/internal/updater/discover.go:18-35`). It fails closed if a label is missing or a
  path is invisible.
- **Rewrites `<working_dir>/.env`:**
  - `QUASAR_CONTROL_IMAGE` for the service `quasar-control-plane`;
  - `QUASAR_AGENT_IMAGE` for `quasar-node-agent`, with `QUASAR_NODE_IMAGE` read as an alias
    (`control-plane/internal/updater/plan.go:60-63`; `control-plane/internal/updater/exec.go:430`).
  - It saves `.env.prev` first, and both files are 0600 (`exec.go:86-95`).
- **Runs** `docker compose -p P --project-directory D -f … pull` and then
  `up -d --force-recreate --no-deps --wait` (`plan.go:283-312`).
- **Auto-restores** a control plane that never started and a failed agent (ADR 0004). It never
  restores a control plane that did start (`exec.go:143-170`).
- **Gates:** single flight, a closed component table (it cannot update itself), digest only, the
  namespace allowlist, and an optional ed25519 manifest signature (`plan.go:160-256`;
  `docs/configuration.md:1242-1315`).
- The socket is 0666 with no caller authentication (`control-plane/internal/updater/server.go:23-26`, `:297-315`).
- The updater itself updates by hand (`docs/upgrading.md:390-403`).

**Persisted apply state** (Postgres):
- Migration 0074 adds `hosts.source_commit`, `built_at`, `install_mode`, `updater_present` and
  `platform_releases`.
- Migration 0075 adds `platform_apply_runs` and `platform_apply_attempts`, the latter with
  `requested_digests`, `previous_digests`, `updater_request_id`, `state` and `reason`, and one open
  attempt per target, where the zero uuid is the control plane.
- Later migrations: 0076 `cordoned_hosts`, 0079-0084 (beta channel, notifications, auto-apply,
  retries, `cordons_restored_at`).
- ADR 0002 (`docs/adr/0002-release-order-and-no-downgrade.md:7-28`): control plane first; never offer
  below the control plane; agents never above the control plane; revert only for agents.

### Inferences / open questions
- **(inference)** There is no "desired image per host" record. The target is the latest attempt's
  `requested_digests`. The truth of what runs is the stack's `.env` plus the reported
  `source_commit`. The durable desired state for platform images lives in a manager-owned file.
- **(inference)** Two claims are stale. `deploy/redeploy.sh:884-887` and `deploy/enroll-host.sh:863-864`
  say the agent reads updater presence once at boot. The code re-discovers before every `register`
  (`node-agent/src/buildinfo.rs:271-275`).
- **(inference)** The updater socket (0666, unauthenticated, shared by three containers) is the
  current trust boundary for "who may replace platform containers". #218's "independent CP
  replacement/recovery authority" has to replace or wrap it.

---

## 8. CONTEXT.md vocabulary relevant to RH-06 (avoid collisions)

Existing terms (line numbers in `CONTEXT.md`):

| Term | Line | Gist / avoid-notes |
|---|---|---|
| Home | 69 | per-(user, app) persistent storage |
| Generation | 78 | one app container in one session (`quasar-sess-<sid>-g<n>`); _avoid_ confusing it with the `source_policy` epoch. **Note:** RH05 "generation" in desired-state sense must not collide (see §6) |
| Intentional stop | 89 | agent's own teardown of a generation |
| Legacy container | 104-115 | pre-API sibling; boot sweep; _avoid_ "orphan", "adoption" ("nothing here resumes or observes a prior session") |
| Platform image | 268 | an image that runs or builds Quasar itself; _avoid_ "our images" |
| Session image / app image | 275 | _avoid_ "runtime image" for it (that names the agent's image) |
| Role, not implementation | 283 | image naming rule: `control`, `runtime`, `nv` (deprecated), `dev`, `toolchain`, `profiling` |
| Manifest provenance | 307 | the app-catalog manifest record (not the release manifest) |
| Setting source | 322 | Automatic / deployment baseline / explicit value |
| Configuration applied | 327 | verified evidence; _avoid_ "saved", "received" |
| Idle apply | 332 | bars new assignments, waits, applies; _avoid_ "automatic restart" |
| Admission restriction | 336 | one named owner's reason a host takes no new work; _avoid_ "the cordon" |
| Canonical home claim | 340 | |
| App placement | 345 | _avoid_ "image cache policy" |
| App prepared | 349 | _avoid_ "downloaded" |
| Home template | 353 | _avoid_ "backup" |
| Host fact | 358 | observed + provenance; _avoid_ "capability", "setting" |
| Readiness check | 363 | _avoid_ "health check", "preflight check" |
| Host probe | 369 | _avoid_ "preflight", "self-test" |
| Evidence / Indeterminate | 418 / 423 | only evidence blocks |
| Readiness override / gate | 429 / 434 | |
| Diagnostic registration | 442 | connected but refusing launches (runtime unusable or startup cleanup pending) |
| Platform release | 452 | _avoid_ "update", "image version", "build" |
| Channel | 460 | stable / beta / edge; _avoid_ "track", "branch", "unstable" |
| Release manifest | 469 | _avoid_ "release body", "catalog manifest" |
| Release signature / Trusted release key | 476 / 483 | |
| Updater | 490 | per-host actor; "a container cannot recreate itself"; _avoid_ "sidecar", "agent" |
| Install mode | 496 | registry vs source; _avoid_ "dev host", "pinned" |
| Attempt | 501 | one target's move to one digest set; _avoid_ "job", "task" |
| Preflight | 506 | per-target release evaluation (updater reachable, stack dir/overlays match, health port, images resolvable) |
| Release notification | 517 | |
| Fleet run | 525 | _avoid_ "rollout", "batch", "deployment" |

**Absent from CONTEXT.md today:** enrollment, enrollment token/string, node secret, host identity,
service ownership, owner, manager, deployment/stack, desired state/spec, service spec, adoption
(explicitly *avoided* under "Legacy container"), phase journal, control-only host, combined host.
New RH-06 terms should not reuse **Generation**, **Attempt**, **Preflight**, **Adoption** or
**Deployment** (the Fleet run entry says to avoid "deployment" for a run) without qualifying them.

---

## Coupling inventory

Every place where deployment, identity or state depends on Compose files, `.env`, Compose labels,
host paths, or a specific manager.

| # | Coupling | Depends on | Where | Who relies on it |
|---|---|---|---|---|
| C1 | Updater discovers its stack | Compose labels `project`, `working_dir`, `config_files` on its own container | `control-plane/internal/updater/discover.go:18-35` | every platform apply |
| C2 | Updater rewrites image pins | `<working_dir>/.env` keys `QUASAR_CONTROL_IMAGE` / `QUASAR_AGENT_IMAGE`; `.env.prev` | `control-plane/internal/updater/plan.go:60-63`, `exec.go:86-95`, `:430` | apply and restore |
| C3 | Updater executes | `docker compose -p … --project-directory … -f …` CLI, service names `quasar-control-plane` / `quasar-node-agent` | `control-plane/internal/updater/plan.go:283-312` | apply |
| C4 | Stack dir visible at the same host path | `QUASAR_STACK_DIR` bind | `deploy/docker-compose.yml:685-693`; seeded by `deploy/redeploy.sh:564-593`, `deploy/enroll-host.sh:755-758`, site `stack-template.js:96-97`, `:408` | updater |
| C5 | Updater socket rendezvous | shared named volume `quasar-updater-run` mounted into three services | `deploy/docker-compose.yml:259`, `:625`, `:694`, `:714` | CP self-apply, agent relay, updater presence |
| C6 | Agent `updater_present` | own `com.docker.compose.project` label + running service named `quasar-updater` | `node-agent/src/buildinfo.rs:103-106`, `:249-255` | release eligibility (`plan.go` reason `updater_absent`) |
| C7 | Install mode (agent and CP) | image-reference string shape (`@sha256:` or registry host ⇒ registry, bare tag ⇒ source) | `node-agent/src/buildinfo.rs:196-213`; `control-plane/internal/platform/apply_self.go:86-100` | release eligibility |
| C8 | Agent identity | `NODE_NAME` env string plus the file at `NODE_SECRET_PATH` in named volume `<project>_quasar-agent-data` | `deploy/docker-compose.yml:327-328`, `:621`; `deploy/enroll-host.sh:92-95` | every register |
| C9 | Enrollment input | `ENROLLMENT_TOKEN` / `QUASAR_ENROLLMENT` in `.env` (compose-required on the combined host) | `deploy/docker-compose.yml:113`, `:326`; `deploy/enroll-host.sh:739-763` | first enroll, #199 recovery |
| C10 | Agent → CP address | `ws://localhost:${CONTROL_PORT}` (same machine, host network) | `deploy/docker-compose.yml:325` | combined host |
| C11 | Volume naming | Compose `<project>_<key>`; adopt overlay for three of the five volumes | `deploy/docker-compose.yml:700-714`; `deploy/overlays/docker-compose.adopt-volumes.yml:28-34` | data continuity across a project or manager change |
| C12 | Postgres reachability | Compose service DNS `quasar-postgres`; password in `.env` | `deploy/docker-compose.yml:90-91` | CP boot |
| C13 | Secrets at rest | `.env` is the only copy of `POSTGRES_PASSWORD`, `QUASAR_SECRET_KEY`, `ENROLLMENT_TOKEN` | `site/src/content/docs/install/install.mdx:41-48`; `deploy/redeploy.sh:355-560` | CP boot, stored-secret decrypt |
| C14 | Home / template roots | host paths mounted at the same path; `/run/quasar-agent` fixed bind | `deploy/docker-compose.yml:572`, `:581`, `:593` | sessions (docker-out-of-docker) |
| C15 | NVIDIA driver volume discovery | agent inspects its own container's mounts for `/opt/quasar/nvidia-driver` | `node-agent/src/nvidia_volume.rs:359-428`; `deploy/docker-compose.nvidia.yml:129-134` | NVIDIA provisioning |
| C16 | NVIDIA loader path | fixed `LD_LIBRARY_PATH` in compose / entrypoint | `deploy/docker-compose.nvidia.yml:117`; `docs/configuration.md:1780-1782` | NVENC fallback |
| C17 | Overlay selection | operator-chosen `-f` chain (`QUASAR_CONSOLE`, adopt vars) that must be repeated on every manual compose command | `deploy/redeploy.sh:156-204`; `docs/upgrading.md:206-209` | recreate fidelity (the updater relies on `config_files`) |
| C18 | Redeploy verification / secret guards | Compose label filters to find postgres and the updater | `deploy/redeploy.sh:411-413`, `:506-508`, `:625`, `:964-1001` | source-path deploy |
| C19 | First-install takeover guard | `com.docker.compose.service=quasar-control-plane` label and `working_dir` label | `site/src/data/stack-template.js:315-334` | site installer |
| C20 | Project-name defaults | project `deploy` (dir name) on the combined host, `quasar-agent` on enrolled hosts | `deploy/redeploy.sh:352-353`; `deploy/enroll-host.sh:47`, `:88-95`; `deploy/README.md:1043-1071` | volume names, identity volume |
| C21 | Enrolled host's compose file | `/opt/quasar-agent/{docker-compose.yml,.env}` written by the installer; kept equal to the base service by a Go test | `deploy/enroll-host.sh:728-763`; `control-plane/cmd/quasar-control/enroll_host_compose_test.go:107` | extra GPU hosts |
| C22 | Installer script delivery | served from the CP's SPA at `/enroll-host.sh` | `web/vite.config.ts:97-112` | extra GPU hosts |
| C23 | Health port | agent binds `QUASAR_HEALTH_ADDR` on the host network; the image `HEALTHCHECK` follows it | `deploy/docker-compose.yml:342`; `docs/upgrading.md:375-388` | agent start, apply preflight |
| C24 | Unraid persistence | stack dir and homes under persistent storage; sysctl and uinput in `/boot/config/go` | `site/src/data/platforms.js:30-44`, `:78-90` | reboot durability |
| C25 | Restart policy as "supervisor" | `restart: unless-stopped` on every service; agent exits on missing enrollment so compose crash-loops it | `deploy/docker-compose.yml:65`, `:275`, `:658`, `:698`; `docs/configuration.md:338` | liveness (#218: "A restart policy is not an image updater") |

## State inventory

| State | Lives in | Written by | Losing it does |
|---|---|---|---|
| Postgres data (all CP state: users, hosts incl. `node_secret_hash`, sessions, `user_homes`, `host_enrollments`, `instance_secrets` ciphertext, platform releases, attempts, runs, RH05 desired/observed rows) | named volume `<project>_quasar-postgres-data` (`deploy/docker-compose.yml:59`) | control plane (migrations at boot) | Total loss of the instance. Every host must re-enroll (#199 path) and admins must be re-claimed (`deploy/README.md:1150-1151`) |
| `POSTGRES_PASSWORD` | stack `.env` | redeploy / site installer / operator | CP cannot open the DB. Not regenerable once the DB is initialised |
| `QUASAR_SECRET_KEY` (+ previous) | stack `.env` | redeploy (guarded) / site installer / operator | Every `instance_secrets` value is unrecoverable |
| Static `ENROLLMENT_TOKEN` | stack `.env` (CP and local agent) | redeploy / site / operator | New hosts need minted tokens; a combined-host agent with a lost secret cannot re-enroll without one |
| CP TLS pair + artwork cache | named volume `<project>_quasar-control-tls` → `/var/lib/quasar-control` (`:255`) | control plane | New certificate fingerprint: **every pinned remote agent disconnects** until `CONTROL_PLANE_FINGERPRINT` is updated; browsers re-accept; artwork re-fetched |
| Agent `node-secret` | `<project>_quasar-agent-data` → `/var/lib/quasar-agent/node-secret` | agent (on `registered`) | Host must re-enroll with a fresh token. Same `NODE_NAME` ⇒ same row; different name ⇒ orphaned row |
| Agent pin `node-secret.tls` | same volume | agent | Falls back to string or env fingerprint, else WebPKI; a self-signed CP then fails |
| `node-secret.container-owner` (RH-01 lease) | same volume | agent | Ownership of surviving session and audio containers is lost; they are preserved for manual review, not swept (`docs/configuration.md:337`) |
| `node-secret.policy.json` (RH05 restart journal / policy) | same volume | agent | Pending restart or idle-apply journal is lost (see §6) |
| `node-secret.images.json`, `.runtime-images` | same volume | agent | Managed-image and runtime-image state re-learned |
| `NODE_NAME` | stack `.env` / env default hostname | operator / installer | Changing it creates a new host row and orphans the old one (`docs/operations/compose-consolidation-migration.md:92-94`) |
| Image pins (`QUASAR_CONTROL_IMAGE`, `QUASAR_AGENT_IMAGE`, `QUASAR_UPDATER_IMAGE`, `QUASAR_POSTGRES_IMAGE`) | stack `.env` (+ `.env.prev`) | operator / installers / **updater** | Falls back to `:latest` local tags (source install mode); the apply history in the DB no longer matches |
| Compose file chain + stack dir | stack dir (`deploy/` or `/opt/quasar-agent/`) and Compose labels on containers | operator / installers | Updater cannot discover the stack (fails closed); manual recreate may drop overlays |
| Updater results | named volume `quasar-updater-run` → `/run/quasar-updater/results/<id>.json` | updater | CP falls back to its boot identity or agent register as evidence; in-flight apply status is lost |
| Setup token, dev agent key | `/run/quasar/*` in the CP container filesystem | control plane, per boot | Regenerated each boot |
| Managed homes | host bind `QUASAR_HOME_ROOT` (default `/var/lib/quasar/homes`) at the same path | agent (dirs) / CP (`user_homes` rows) | User save data lost; rows orphaned |
| Home templates | host bind `QUASAR_TEMPLATE_ROOT` (sibling of home root) | agent | Re-prepared (warm-up) |
| NVIDIA driver userspace + NVRTC | named volume `<project>_quasar-nvidia-driver` (NVIDIA overlay only) | agent | Re-provisioned (download) on the next start; not in the adopt overlay |
| Host runtime dir | host bind `/run/quasar-agent` | agent, compositor, pulse | Transient (sockets) |
| Session / app images, platform images | Docker image store on each host | agent (app pulls) / updater / operator | Re-pulled |
| AppArmor profile (enrolled hosts) | `/opt/quasar-agent/apparmor/quasar-app` (optionally `/etc/apparmor.d`) | `deploy/enroll-host.sh` | App containers launch unconfined until reloaded |
| Host kernel settings (sysctl, uinput) | `/etc/sysctl.d`, `/etc/modules-load.d`, or Unraid `/boot/config/go` | site installer / operator | Streams degrade or input fails after reboot |
