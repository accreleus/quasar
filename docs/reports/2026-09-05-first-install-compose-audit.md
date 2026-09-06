# First-install investigation: exhaustive Compose and identity audit

Investigated 2026-09-05 at `40232da8f8970b1babdf72a8a09fefeb5b3b80a4`. Read-only code/issue investigation; no deployments or fixes. This appendix supports the parent first-install investigation. It inventories all **115 environment entries in the base file and 9 in the NVIDIA overlay**, including duplicate overrides, and all **17 volume-mount entries** across both files. Device mappings are recorded separately. Line citations refer to that revision.

## Findings and classification rules

The image-facts pattern holds, but the proposed test needs one distinction: a setting can be a legitimate advanced operator choice without belonging in the basic-install form or requiring its default to be duplicated in Compose. Most of the long environment list is optional policy, diagnostics, or tuning. Removing its passthrough without offering an equivalent override would remove real capability. Defaults should be owned once by code/image, with an advanced override surface generated from that catalog.

Classes used below:

- **O — operator choice:** reasonable operators can need different answers; preserve an override. Code should supply the safe default; basic installs should not need to pick it.
- **W — deployment wiring:** derived from selected topology, ports, images, or storage. Generate it consistently rather than asking the operator to repeat it.
- **I — image invariant:** stable container-internal fact; put its default in the image or binary. A specialist override need not appear in basic Compose.
- **E — entrypoint setup:** establish before exec or before library initialization, preserving explicit supported overrides.
- **R — runtime derivation:** derive from actual GPU/mount/container identity; validate the result before consuming it.
- **D — Docker creation requirement:** must exist in the container-create request. An entrypoint cannot repair its own missing devices, bind mounts, or NVIDIA runtime injection. Generate these from host facts and validate them before `up`.

A class describes the recommended ownership, not a claim that current code already implements it. Every environment row below links to its actual declaration; explanatory code citations follow the tables.

## Base environment entries

| Service | Variable and source | Class | Owner / why different answers can be correct |
|---|---|---|---|
| postgres | [`POSTGRES_DB`](../../deploy/docker-compose.yml#L55) | W | Fixed `quasar` database in this bundled topology; retain in generated PostgreSQL/CP wiring, not in a Quasar image that cannot configure another image. |
| postgres | [`POSTGRES_USER`](../../deploy/docker-compose.yml#L56) | O/W | Database identity can differ; selected once and used in PostgreSQL, healthcheck, and connection URI. |
| postgres | [`POSTGRES_PASSWORD`](../../deploy/docker-compose.yml#L57) | O/W | Installation credential; generate securely or accept operator secret, then wire both services consistently. |
| control-plane | [`DATABASE_URL`](../../deploy/docker-compose.yml#L90) | W | Build from database connection choices; do not bake credentials or a topology-specific hostname into image. |
| control-plane | [`LISTEN_ADDR`](../../deploy/docker-compose.yml#L92) | I/W | `:8080` is already code default; pair any advanced override with published-port/healthcheck wiring. |
| control-plane | [`QUASAR_TLS`](../../deploy/docker-compose.yml#L99) | O | TLS mode differs behind ingress versus direct LAN; default in code. |
| control-plane | [`QUASAR_TLS_ADDR`](../../deploy/docker-compose.yml#L100) | I/W | Internal `:8443` endpoint; code default and generated port wiring own it. |
| control-plane | [`QUASAR_TLS_REDIRECT_PORT`](../../deploy/docker-compose.yml#L105) | W | Derived from selected external HTTPS port; bridged CP cannot infer host NAT port. |
| control-plane | [`QUASAR_HTTP_REDIRECT`](../../deploy/docker-compose.yml#L106) | O | HTTP/HTTPS exposure policy differs by ingress/development topology. |
| control-plane | [`QUASAR_TLS_HOSTS`](../../deploy/docker-compose.yml#L107) | O/W | Public/LAN SAN names need operator/network input; bridge-local enumeration cannot establish external names. |
| control-plane | [`QUASAR_PUBLIC_HOST`](../../deploy/docker-compose.yml#L110) | O | External DNS identity; not reliably discoverable inside container. |
| control-plane | [`QUASAR_TLS_CERT`](../../deploy/docker-compose.yml#L111) | O | Operator-supplied certificate path must match certificate mounting strategy. |
| control-plane | [`QUASAR_TLS_KEY`](../../deploy/docker-compose.yml#L112) | O | Operator-supplied private-key path/access; not an image fact. |
| control-plane | [`ENROLLMENT_TOKEN`](../../deploy/docker-compose.yml#L113) | O/W | Shared enrollment credential; generate/choose once and distribute securely, never bake. |
| control-plane | [`QUASAR_STORAGE_PROVIDER`](../../deploy/docker-compose.yml#L122) | O | Local versus Docker-volume homes is storage policy; changing established provider strands homes. |
| control-plane | [`QUASAR_HOME_ROOT`](../../deploy/docker-compose.yml#L123) | O/W | Host storage placement differs; same selected path must drive provider, agent, and host mounts. |
| control-plane | [`QUASAR_LIBRARY_PROVIDERS`](../../deploy/docker-compose.yml#L128) | O | Trusted provider allowlist controls automatic installation. |
| control-plane | [`QUASAR_PLACEMENT_POLICY`](../../deploy/docker-compose.yml#L130) | O | Spread versus locality is scheduling policy. |
| control-plane | [`LOG_LEVEL`](../../deploy/docker-compose.yml#L131) | O | Diagnostic verbosity is a legitimate choice; `info` already defaults in code. |
| control-plane | [`QUASAR_ALLOWED_ORIGINS`](../../deploy/docker-compose.yml#L140) | O | Security allowlist depends on direct/proxy origins; default same-origin is appropriate. |
| control-plane | [`QUASAR_TRUSTED_PROXIES`](../../deploy/docker-compose.yml#L147) | O | Trust boundary depends on actual reverse-proxy networks; never infer broadly. |
| control-plane | [`QUASAR_ICE_SERVERS`](../../deploy/docker-compose.yml#L157) | O | STUN/TURN topology and credentials depend on operator network. |
| control-plane | [`QUASAR_DEV_AGENT_AUTH`](../../deploy/docker-compose.yml#L162) | O | Explicit development-only security policy; off by default. |
| control-plane | [`QUASAR_ENV`](../../deploy/docker-compose.yml#L163) | O | Production/development policy gates privileged diagnostics. |
| control-plane | [`PUBLIC_BASE_URL`](../../deploy/docker-compose.yml#L170) | O/W | Externally reachable URL for invites/signaling; generate from ingress selection, cannot infer rewritten Host. |
| control-plane | [`AUTH_TOKEN_TTL`](../../deploy/docker-compose.yml#L171) | O | Authentication lifetime is security policy. |
| control-plane | [`BOOTSTRAP_ADMIN_EMAIL`](../../deploy/docker-compose.yml#L173) | O | Optional first-admin identity; alternative to setup claim. |
| control-plane | [`BOOTSTRAP_ADMIN_USERNAME`](../../deploy/docker-compose.yml#L174) | O | Optional first-admin identity; alternative to setup claim. |
| control-plane | [`BOOTSTRAP_ADMIN_PASSWORD`](../../deploy/docker-compose.yml#L175) | O | Optional first-admin credential, not image data. |
| control-plane | [`QUASAR_WEB_ROOT`](../../deploy/docker-compose.yml#L177) | I | Production image already sets `/app/web`; no basic Compose duplication needed. |
| control-plane | [`QUASAR_SECRET_KEY`](../../deploy/docker-compose.yml#L193) | O | Persistent secret-encryption key must agree across replicas and backups. |
| control-plane | [`QUASAR_SECRET_KEY_PREVIOUS`](../../deploy/docker-compose.yml#L194) | O | Operator-controlled key-rotation material. |
| control-plane | [`QUASAR_STEAMGRIDDB_API_KEY`](../../deploy/docker-compose.yml#L195) | O | Optional third-party credential/consent. |
| control-plane | [`QUASAR_ARTWORK_PROVIDER`](../../deploy/docker-compose.yml#L196) | O | Optional artwork service policy; default should live in code. |
| control-plane | [`QUASAR_ARTWORK_DIR`](../../deploy/docker-compose.yml#L197) | O/W | Cache placement can differ; default inside persistent state, override must carry matching mount/access. |
| control-plane | [`QUASAR_ARTWORK_MAX_BYTES`](../../deploy/docker-compose.yml#L198) | O | Storage budget differs by host capacity. |
| control-plane | [`QUASAR_ARTWORK_SWEEP_INTERVAL`](../../deploy/docker-compose.yml#L199) | O | Cache housekeeping policy. |
| control-plane | [`QUASAR_PPROF_ADDR`](../../deploy/docker-compose.yml#L208) | O | Diagnostics exposure or explicit disable; keep loopback default in code. |
| control-plane | [`GOMEMLIMIT`](../../deploy/docker-compose.yml#L213) | O/E | Heap budget differs by container resources. Go runtime reads environment at process startup; entrypoint can derive bounded default before exec. |
| node-agent | [`CONTROL_PLANE_URL`](../../deploy/docker-compose.yml#L300) | W/O | This topology derives loopback URL from CONTROL_PORT; remote-agent deployments legitimately point elsewhere. |
| node-agent | [`ENROLLMENT_TOKEN`](../../deploy/docker-compose.yml#L301) | O/W | Shared enrollment credential; generate/choose once and distribute securely, never bake. |
| node-agent | [`NODE_NAME`](../../deploy/docker-compose.yml#L302) | O/R | Operator label is optional; code already derives hostname when absent instead of needing fixed quasar-node-1. |
| node-agent | [`NODE_SECRET_PATH`](../../deploy/docker-compose.yml#L303) | I/W | Persisted internal identity path belongs to image with state mount contract; current code fallback is `/tmp`, so moving requires a code/image change. |
| node-agent | [`RUST_LOG`](../../deploy/docker-compose.yml#L304) | O | Target-specific diagnostics are legitimate; `info` already defaults in code. |
| node-agent | [`XDG_RUNTIME_DIR`](../../deploy/docker-compose.yml#L306) | I/E/W | Image default plus entrypoint mkdir/chmod; generated host bind must match. Current session fallback `/tmp/runtime-quasar` differs from Compose. |
| node-agent | [`QUASAR_ENCODER`](../../deploy/docker-compose.yml#L310) | O/R | Auto-detect hardware default; explicit compatible backend/software override is legitimate. |
| node-agent | [`QUASAR_RENDER_NODE`](../../deploy/docker-compose.yml#L311) | O/R | Multiple GPUs require selection; single compatible GPU should be derived. Prefer stable by-path selector over renderD128 assumption. |
| node-agent | [`QUASAR_HOME_ROOT`](../../deploy/docker-compose.yml#L315) | O/W | Host storage placement differs; same selected path must drive provider, agent, and host mounts. |
| node-agent | [`QUASAR_HOMES_GC`](../../deploy/docker-compose.yml#L320) | O | Home garbage-collection enablement, retention, or dry-run policy. |
| node-agent | [`QUASAR_HOMES_GC_RETENTION_HOURS`](../../deploy/docker-compose.yml#L321) | O | Home garbage-collection enablement, retention, or dry-run policy. |
| node-agent | [`QUASAR_HOMES_GC_DRY_RUN`](../../deploy/docker-compose.yml#L322) | O | Home garbage-collection enablement, retention, or dry-run policy. |
| node-agent | [`QUASAR_HOME_TEMPLATES`](../../deploy/docker-compose.yml#L331) | O | Template consumption policy; opt-in. |
| node-agent | [`QUASAR_TEMPLATE_WARMUP`](../../deploy/docker-compose.yml#L332) | O | Template production/storage/performance safety policy; operator capacity and filesystem differ. |
| node-agent | [`QUASAR_TEMPLATE_ROOT`](../../deploy/docker-compose.yml#L333) | O/W | Storage placement differs; derive default sibling of home root and generate same-path mount from same value. |
| node-agent | [`QUASAR_TEMPLATE_CLONE_MODE`](../../deploy/docker-compose.yml#L334) | O | Template production/storage/performance safety policy; operator capacity and filesystem differ. |
| node-agent | [`QUASAR_TEMPLATE_ALLOW_CROSSFS`](../../deploy/docker-compose.yml#L335) | O | Template production/storage/performance safety policy; operator capacity and filesystem differ. |
| node-agent | [`QUASAR_TEMPLATE_SETTLE_SECS`](../../deploy/docker-compose.yml#L336) | O | Template production/storage/performance safety policy; operator capacity and filesystem differ. |
| node-agent | [`QUASAR_TEMPLATE_WARMUP_TIMEOUT_SECS`](../../deploy/docker-compose.yml#L337) | O | Template production/storage/performance safety policy; operator capacity and filesystem differ. |
| node-agent | [`QUASAR_TEMPLATE_MIN_FREE_BYTES`](../../deploy/docker-compose.yml#L338) | O | Template production/storage/performance safety policy; operator capacity and filesystem differ. |
| node-agent | [`QUASAR_ZEROCOPY`](../../deploy/docker-compose.yml#L343) | O | Backend compatibility/performance override; default should live in agent. |
| node-agent | [`QUASAR_LATENCY_PROBE`](../../deploy/docker-compose.yml#L345) | O | Optional diagnostic instrumentation. |
| node-agent | [`QUASAR_CAPTURE_H264`](../../deploy/docker-compose.yml#L348) | O | Explicit stream capture/debug destination; not needed for ordinary boot. |
| node-agent | [`LIBVA_TRACE`](../../deploy/docker-compose.yml#L354) | O/E | Optional trace destination; empty must be unset before libva loads (existing entrypoint does this). |
| node-agent | [`GST_DEBUG`](../../deploy/docker-compose.yml#L355) | O/E | Optional GStreamer diagnostic filter, including legacy VULKAN_GST_DEBUG alias; set before gst initialization. |
| node-agent | [`QUASAR_TARGET_USAGE`](../../deploy/docker-compose.yml#L360) | O | Encoder speed/quality policy. |
| node-agent | [`QUASAR_QUEUE_BUFFERS`](../../deploy/docker-compose.yml#L361) | O | Queue depth trades burst tolerance against latency. |
| node-agent | [`QUASAR_SLICES`](../../deploy/docker-compose.yml#L365) | O | Encoding/loss-resilience tradeoff; not a deployment prerequisite. |
| node-agent | [`QUASAR_FEC_MODE`](../../deploy/docker-compose.yml#L374) | O | Loss-protection controller policy; bandwidth/redundancy and responsiveness differ. |
| node-agent | [`QUASAR_FEC_PERCENTAGE`](../../deploy/docker-compose.yml#L375) | O | Loss-protection controller policy; bandwidth/redundancy and responsiveness differ. |
| node-agent | [`QUASAR_FEC_ARM_LOSS_PCT`](../../deploy/docker-compose.yml#L379) | O | Loss-protection controller policy; bandwidth/redundancy and responsiveness differ. |
| node-agent | [`QUASAR_FEC_WINDOW_S`](../../deploy/docker-compose.yml#L380) | O | Loss-protection controller policy; bandwidth/redundancy and responsiveness differ. |
| node-agent | [`QUASAR_FEC_ARM_WINDOWS`](../../deploy/docker-compose.yml#L381) | O | Loss-protection controller policy; bandwidth/redundancy and responsiveness differ. |
| node-agent | [`QUASAR_FEC_DISARM_WINDOWS`](../../deploy/docker-compose.yml#L382) | O | Loss-protection controller policy; bandwidth/redundancy and responsiveness differ. |
| node-agent | [`QUASAR_FEC_MAX_FLAPS`](../../deploy/docker-compose.yml#L383) | O | Loss-protection controller policy; bandwidth/redundancy and responsiveness differ. |
| node-agent | [`QUASAR_INTRA_REFRESH`](../../deploy/docker-compose.yml#L389) | O | Encoder loss-resilience policy; compatibility/performance tradeoff. |
| node-agent | [`QUASAR_INTRA_REFRESH_PERIOD`](../../deploy/docker-compose.yml#L390) | O | Encoder loss-resilience policy; compatibility/performance tradeoff. |
| node-agent | [`QUASAR_VULKAN_H264`](../../deploy/docker-compose.yml#L396) | O | Per-codec backend opt-out; automatic supported default belongs to code. |
| node-agent | [`QUASAR_VULKAN_HEVC`](../../deploy/docker-compose.yml#L397) | O | Per-codec backend opt-out; automatic supported default belongs to code. |
| node-agent | [`QUASAR_VULKAN_AV1`](../../deploy/docker-compose.yml#L398) | O | Per-codec backend opt-out; automatic supported default belongs to code. |
| node-agent | [`WOLF_VULKAN_RING`](../../deploy/docker-compose.yml#L404) | O/R | Default/workaround is derived by agent; explicit A/B override can remain advanced. |
| node-agent | [`QUASAR_TRACE_RTP_TS`](../../deploy/docker-compose.yml#L408) | O | Optional detailed diagnostics; advanced surface only. |
| node-agent | [`QUASAR_TRACE_RTP_MARKER`](../../deploy/docker-compose.yml#L409) | O | Optional detailed diagnostics; advanced surface only. |
| node-agent | [`QUASAR_TRACE_ENC_PTS`](../../deploy/docker-compose.yml#L410) | O | Optional detailed diagnostics; advanced surface only. |
| node-agent | [`QUASAR_ABR`](../../deploy/docker-compose.yml#L424) | O | Congestion-control policy/tuning; keep code defaults authoritative and live settings where supported. |
| node-agent | [`QUASAR_ABR_MODE`](../../deploy/docker-compose.yml#L425) | O | Congestion-control policy/tuning; keep code defaults authoritative and live settings where supported. |
| node-agent | [`QUASAR_ABR_FLOOR_KBPS`](../../deploy/docker-compose.yml#L426) | O | Congestion-control policy/tuning; keep code defaults authoritative and live settings where supported. |
| node-agent | [`QUASAR_ABR_FLOOR_RATIO`](../../deploy/docker-compose.yml#L427) | O | Congestion-control policy/tuning; keep code defaults authoritative and live settings where supported. |
| node-agent | [`QUASAR_ABR_EWMA_ALPHA`](../../deploy/docker-compose.yml#L436) | O | Congestion-control policy/tuning; keep code defaults authoritative and live settings where supported. |
| node-agent | [`QUASAR_ABR_DEADBAND`](../../deploy/docker-compose.yml#L437) | O | Congestion-control policy/tuning; keep code defaults authoritative and live settings where supported. |
| node-agent | [`QUASAR_ABR_MAX_UP_STEP`](../../deploy/docker-compose.yml#L438) | O | Congestion-control policy/tuning; keep code defaults authoritative and live settings where supported. |
| node-agent | [`QUASAR_ABR_MIN_INTERVAL_MS`](../../deploy/docker-compose.yml#L439) | O | Congestion-control policy/tuning; keep code defaults authoritative and live settings where supported. |
| node-agent | [`QUASAR_ABR_MAX_DOWN_STEP`](../../deploy/docker-compose.yml#L440) | O | Congestion-control policy/tuning; keep code defaults authoritative and live settings where supported. |
| node-agent | [`QUASAR_ABR_DOWN_DWELL_MS`](../../deploy/docker-compose.yml#L441) | O | Congestion-control policy/tuning; keep code defaults authoritative and live settings where supported. |
| node-agent | [`QUASAR_ABR_CLIFF_GUARD_FRAC`](../../deploy/docker-compose.yml#L442) | O | Congestion-control policy/tuning; keep code defaults authoritative and live settings where supported. |
| node-agent | [`MALLOC_ARENA_MAX`](../../deploy/docker-compose.yml#L463) | O/E | glibc allocator tuning for resource/diagnostic experiments; set before process allocation starts (entrypoint before exec is safe). |
| node-agent | [`MALLOC_TRIM_THRESHOLD_`](../../deploy/docker-compose.yml#L464) | O/E | glibc allocator tuning for resource/diagnostic experiments; set before process allocation starts (entrypoint before exec is safe). |
| node-agent | [`MALLOC_MMAP_THRESHOLD_`](../../deploy/docker-compose.yml#L465) | O/E | glibc allocator tuning for resource/diagnostic experiments; set before process allocation starts (entrypoint before exec is safe). |
| node-agent | [`QUASAR_MALLOC_TRIM`](../../deploy/docker-compose.yml#L466) | O | Explicit memory-management diagnostic/tuning policy. |
| node-agent | [`QUASAR_AUDIO_DISABLED`](../../deploy/docker-compose.yml#L470) | O | Video-only operation is legitimate. |
| node-agent | [`QUASAR_AUDIO_NO_CLOCK`](../../deploy/docker-compose.yml#L471) | O | Diagnostic clock-policy escape hatch; advanced surface only. |
| node-agent | [`QUASAR_AUDIO_REQUIRED`](../../deploy/docker-compose.yml#L477) | O | Whether lack of audio should fail session is operator/application policy; basic streaming profile can choose safe default. |
| node-agent | [`QUASAR_INPUT_TRACE`](../../deploy/docker-compose.yml#L485) | O | Input diagnostics/transport/batching/workaround policy; default belongs to agent. |
| node-agent | [`QUASAR_INPUT_CHANNEL_MODE`](../../deploy/docker-compose.yml#L486) | O | Input diagnostics/transport/batching/workaround policy; default belongs to agent. |
| node-agent | [`QUASAR_INPUT_BATCH_MS`](../../deploy/docker-compose.yml#L487) | O | Input diagnostics/transport/batching/workaround policy; default belongs to agent. |
| node-agent | [`QUASAR_INPUT_CONTROLLER_NUDGE`](../../deploy/docker-compose.yml#L491) | O | Input diagnostics/transport/batching/workaround policy; default belongs to agent. |
| node-agent | [`LIBGL_ALWAYS_SOFTWARE`](../../deploy/docker-compose.yml#L507) | O/E | Deliberate software-only override; empty must be unset before Mesa initialization. |
| node-agent | [`MESA_LOADER_DRIVER_OVERRIDE`](../../deploy/docker-compose.yml#L508) | O/E | Specialist driver selection; empty must be unset before Mesa initialization. |
| node-agent | [`QUASAR_PULSE_IMAGE`](../../deploy/docker-compose.yml#L512) | O/R/W | Default must follow actual agent image/digest; optional compatible sidecar override. Resolve verified self identity or generate from one release manifest. |
| node-agent | [`QUASAR_APP_SHM_SIZE`](../../deploy/docker-compose.yml#L515) | O | App memory budget differs by workloads and host capacity. |
| node-agent | [`QUASAR_APP_STOP_TIMEOUT_SECS`](../../deploy/docker-compose.yml#L518) | O | Graceful-stop timeout depends on workload. |
| node-agent | [`QUASAR_CONTAINER_NETWORK`](../../deploy/docker-compose.yml#L522) | O | Isolation versus outbound network access is policy; Steam login/download needs working network. |
| node-agent | [`QUASAR_APP_PUID`](../../deploy/docker-compose.yml#L526) | O | Host filesystem identity for tenant apps; does not change CP uid. |
| node-agent | [`QUASAR_APP_PGID`](../../deploy/docker-compose.yml#L527) | O | Host filesystem group for tenant apps; does not change CP gid. |
| updater | [`QUASAR_UPDATER_ALLOWED_NAMESPACES`](../../deploy/docker-compose.yml#L637) | O | Image supply-chain trust allowlist; may differ for private mirrors. |
| updater | [`QUASAR_UPDATER_WAIT_TIMEOUT_S`](../../deploy/docker-compose.yml#L638) | O | Update waiting budget differs with hardware/network. |

## NVIDIA overlay environment entries

| Service | Variable and source | Class | Owner / why different answers can be correct |
|---|---|---|---|
| node-agent | [`NVIDIA_DRIVER_CAPABILITIES`](../../deploy/docker-compose.nvidia.yml#L66) | I/D | Required graphics/display/compute/video capability set belongs to image ENV plus host-generated NVIDIA runtime request; entrypoint is too late for toolkit injection. |
| node-agent | [`QUASAR_PULSE_IMAGE`](../../deploy/docker-compose.nvidia.yml#L71) | O/R/W | Default must follow actual agent image/digest; optional compatible sidecar override. Resolve verified self identity or generate from one release manifest. |
| node-agent | [`QUASAR_GPU_NVIDIA`](../../deploy/docker-compose.nvidia.yml#L74) | R | Fact about selected GPU/vendor, not independent boolean operator decision; derive consistently with container runtime GPU request. |
| node-agent | [`QUASAR_CUDA_DEVICE`](../../deploy/docker-compose.nvidia.yml#L75) | O/R | Multi-GPU CUDA ordinal can differ; derive from chosen GPU consistently, preserve advanced selection. |
| node-agent | [`QUASAR_RENDER_NODE`](../../deploy/docker-compose.nvidia.yml#L80) | O/R | Multiple GPUs require selection; single compatible GPU should be derived. Prefer stable by-path selector over renderD128 assumption. |
| node-agent | [`QUASAR_NVIDIA_DRIVER_VOLUME`](../../deploy/docker-compose.nvidia.yml#L90) | O/R | Automatic download/provisioning consent can differ (offline/manual hosts); need detection default plus real creation-time persistent storage. |
| node-agent | [`QUASAR_CUDA_RUNTIME`](../../deploy/docker-compose.nvidia.yml#L98) | O/R | Automatic CUDA download versus manual/air-gapped operation is legitimate policy. |
| node-agent | [`LD_LIBRARY_PATH`](../../deploy/docker-compose.nvidia.yml#L114) | E/I | Fixed driver and CUDA volume search prefixes must be set before agent exec; entrypoint can prepend them while preserving supported operator suffix. |
| node-agent | [`LIBVA_MESSAGING_LEVEL`](../../deploy/docker-compose.nvidia.yml#L124) | O/R/E | Suppress irrelevant VA scan noise only for selected non-VA backend; allow diagnostic override. Not a universal image invariant on mixed GPUs. |

## Every volume mount

The **source path/name is host deployment wiring or operator storage policy; the destination is usually an image contract**. The reasonable-operator test does not imply that fixed destinations or required mounts can disappear from Docker's create request. An entrypoint cannot make an unmounted container-local directory visible to future siblings merely by creating it.

| Service and source | Mount | Class and destination ownership | Recommendation |
|---|---|---|---|
| [Postgres L59](../../deploy/docker-compose.yml#L59) | `quasar-postgres-data:/var/lib/postgresql/data` | D/O; PostgreSQL image owns internal data destination | Persist named volume by default; operator can choose storage backend/placement with compatible permissions. |
| [CP L230](../../deploy/docker-compose.yml#L230) | `quasar-control-tls:/var/lib/quasar-control` | D/O/I; CP state destination invariant | Keep named volume default; support validated host bind alternative, with identity preparation. Contains TLS and artwork, not only TLS. |
| [CP L234](../../deploy/docker-compose.yml#L234) | `quasar-updater-run:/run/quasar-updater` | D/W/I; shared updater socket/result location | Generated internal wiring; not a basic-install choice. |
| [Agent L532](../../deploy/docker-compose.yml#L532) | `/var/run/docker.sock:/var/run/docker.sock` | D/O/W; daemon endpoint differs across runtimes | Source should follow same selected Docker/Podman endpoint as updater; agent base currently hardcodes it while updater parameterizes it. |
| [Agent L536](../../deploy/docker-compose.yml#L536) | `/run/quasar-agent:/run/quasar-agent` | D/W/I; shared sockets must resolve on daemon host | Generate one host runtime root and matching agent destination/`XDG_RUNTIME_DIR`; bind source is required by current sibling-launch implementation. |
| [Agent L541](../../deploy/docker-compose.yml#L541) | `/dev/input:/dev/input` | D; kernel interface/hotplug directory | Derive host availability before create; entrypoint cannot supply real host input directory. Retain matching cgroup device permissions. |
| [Agent L545](../../deploy/docker-compose.yml#L545) | `${QUASAR_HOME_ROOT:-/tmp/quasar-homes-unset}` self-map | D/O/W | Operator chooses home storage; generate mount from that same value, omit inactive placeholder in a generated profile. |
| [Agent L557](../../deploy/docker-compose.yml#L557) | `${QUASAR_TEMPLATE_ROOT:-/var/lib/quasar/templates}` self-map | D/O/W | Derive default sibling from chosen home root once; current Compose fixed fallback can diverge from runtime-derived sibling. Disabled templates do not need an active mount. |
| [Agent L564](../../deploy/docker-compose.yml#L564) | `/etc/os-release:/host/etc/os-release:ro` | D/R; source host identity, internal `/host` convention | Host-derived diagnostic input; no user choice normally. Optional unavailable source must yield generic host instructions, not image distro guess. |
| [Agent L573](../../deploy/docker-compose.yml#L573) | `/dev:/host/dev:ro` | D/R; host device visibility | Generated diagnostic/launch discovery input; verify visibility separately from access permissions. |
| [Agent L582](../../deploy/docker-compose.yml#L582) | `/sys/kernel/security:/host/sys/kernel/security:ro` | D/R; host security profile visibility | Host feature detection should add when meaningful; absence must surface loss of AppArmor assurance. |
| [Agent L584](../../deploy/docker-compose.yml#L584) | `quasar-agent-data:/var/lib/quasar-agent` | D/O/I; enrollment identity persistence | Named volume default; image should agree on default credential destination. External storage placement can differ. |
| [Agent L588](../../deploy/docker-compose.yml#L588) | `quasar-updater-run:/run/quasar-updater` | D/W/I | Same generated socket/result wiring as CP/updater. |
| [Updater L641](../../deploy/docker-compose.yml#L641) | `${QUASAR_DOCKER_SOCKET:-/var/run/docker.sock}:/var/run/docker.sock` | D/O/W | Select daemon source once; match agent. Internal endpoint can remain fixed. |
| [Updater L650](../../deploy/docker-compose.yml#L650) | `${QUASAR_STACK_DIR:-/var/lib/quasar/stack-dir-unset}` self-map | D/O/W | Installer knows the actual stack directory; emit it explicitly. Runtime labels can validate but cannot retroactively mount it. |
| [Updater L651](../../deploy/docker-compose.yml#L651) | `quasar-updater-run:/run/quasar-updater` | D/W/I | Required internal updater channel, generated consistently. |
| [NVIDIA agent L131](../../deploy/docker-compose.nvidia.yml#L131) | `quasar-nvidia-driver:/opt/quasar/nvidia-driver` | D/O/I/R | Generate persistent volume when required; image owns destination, runtime owns driver version/provisioning/validated host-source translation. Storage location is optional operator policy. |

Top-level named volume declarations at [base L668](../../deploy/docker-compose.yml#L668) and [overlay L135](../../deploy/docker-compose.nvidia.yml#L135) are backing objects for the mount rows, not additional mounts. The absence of explicit `name:` delegates physical name/isolation to Compose, which is sound; substituting container-local default paths does not replace persistence.

Creation-time device/runtime choices, although outside the requested environment/mount inventory, are essential to the conclusion:

- [`/dev/dri`, `/dev/uinput`, `/dev/kmsg`](../../deploy/docker-compose.yml#L594) must be present before Docker starts the agent. `/dev/kmsg` is optional visibility, but the unconditional device mapping makes its absence a Docker-start fatal error. No agent preflight can run inside a container Docker never creates.
- [`gpus: all`](../../deploy/docker-compose.nvidia.yml#L62) is Docker creation wiring. GPU subset selection is a legitimate operator policy. `NVIDIA_DRIVER_CAPABILITIES` must be known to the NVIDIA toolkit at that point; exporting it inside the agent entrypoint is too late to cause injection. Baking a required default in image metadata is an option, subject to supported runtime integration.
- The overlay comment calling `gpus: all` a CDI invocation should not be treated as a portable runtime fact. The declarative GPU request and how the installed NVIDIA runtime/toolkit fulfills it are different layers; the install gate needs to test the supported Docker/toolkit combinations.
- [`cap_add: [NET_ADMIN, SYSLOG]`](../../deploy/docker-compose.yml#L293), [`init: true`](../../deploy/docker-compose.yml#L297), and [input cgroup rule](../../deploy/docker-compose.yml#L617) likewise cannot be retrofitted by an unprivileged entrypoint. The nearby comment saying NET_ADMIN grants “READ ... only” is wrong as a Linux capability claim: the program currently uses it to read rules, but the capability itself permits network administration. This is a deployment trust decision, not read-only enforcement.

## Before exec versus before first use

The source explicitly documents the dynamic-loader constraint in [`nvidia_volume::process_env`](../../node-agent/src/nvidia_volume.rs#L1540) and [`cuda_runtime`](../../node-agent/src/cuda_runtime.rs#L111): glibc latches `LD_LIBRARY_PATH` at exec. **That justifies “before the agent starts,” not “only Compose.”** The existing [agent entrypoint](../../deploy/Dockerfile.vulkan#L1005) already performs environment repair and then `exec`s either supplied arguments or the baked binary. Prepending the two image-owned library directories there would meet the timing constraint, preserve overrides, and work when someone starts the image without the overlay's environment stanza. It would not create a missing host mount or NVIDIA device injection.

The same entrypoint already removes empty `MESA_LOADER_DRIVER_OVERRIDE`, `LIBGL_ALWAYS_SOFTWARE`, and `LIBVA_TRACE`, creates/chmods an explicitly configured XDG runtime directory, and starts Avahi. The image's [GStreamer plugin path](../../deploy/Dockerfile.vulkan#L952) is already baked; Compose correctly does not replace it. `GST_DEBUG`, Mesa variables, libva trace/message variables, and `WOLF_VULKAN_RING` need to be correct before the relevant library initializes/reads them, not necessarily at Docker creation. `MALLOC_*` allocator knobs and Go's `GOMEMLIMIT` are startup configuration; a before-exec entrypoint is a safe point to establish them, while changing a process environment later is not a reliable substitute for runtime APIs.

Conversely, driver-volume [`__EGL_VENDOR_LIBRARY_DIRS`, `VK_ADD_DRIVER_FILES`, `__EGL_EXTERNAL_PLATFORM_CONFIG_DIRS`, `GBM_BACKENDS_PATH`](../../node-agent/src/nvidia_volume.rs#L1548) are already correctly derived after inspecting actual provisioned content and before `gst::init`. Baking replacing EGL/GBM search paths unconditionally would break healthy AMD/Intel or empty-volume cases. Retain the runtime discovery and additive/fallback semantics.

The NVIDIA flag is genuinely redundant hardware identity today: [`ContainerRuntime::from_env`](../../node-agent/src/session/container.rs#L318) trusts a standalone boolean instead of the selected GPU, even though the base [encoder path already supports automatic vendor selection](../../deploy/docker-compose.yml#L307). Current [overlay documentation](../../deploy/docker-compose.nvidia.yml#L27) says it no longer sets an encoder default. The contrary sentence in CLAUDE.md is stale.

Two defaults currently make merely deleting Compose entries unsafe: [`NODE_SECRET_PATH`](../../node-agent/src/config.rs#L38) falls back to a `/tmp` file and [session `XDG_RUNTIME_DIR`](../../node-agent/src/session/mod.rs#L500) falls back to `/tmp/runtime-quasar`. Move the defaults and mount contract together. A third default is worse: [`QUASAR_PULSE_IMAGE`](../../node-agent/src/session/audio.rs#L105) falls back to `quasar-agent-dev:latest`, not the running production image. The Compose expression currently hides that defect. Deriving a verified running image ID/digest is preferable to baking a mutable tag into the binary, but failure to resolve identity must be surfaced rather than silently choosing an unrelated development image.

## Identity and ownership

The identity pattern holds for arbitrary host binds across Linux, not specifically Unraid. The source-built [control image](../../deploy/Dockerfile.control#L53) fixes uid/gid `1000:1000`, pre-owns `/var/lib/quasar-control` and `/run/quasar`, and runs non-root. The [production image](../../deploy/Dockerfile.control.prod#L112) does the equivalent using the shared `quasar` account. Named volumes inherit the image mountpoint's ownership on initial population. A pre-existing host bind does not. A host directory owned `99:100` with restrictive permissions therefore fails under uid 1000, while an explicit `user: 99:100` makes the image-owned `/run/quasar` directory (0700) inaccessible.

The setup token is [written mode 0600 under a 0700 directory](../../control-plane/internal/setup/token.go#L27); a failure is deliberately fatal because the token must never be exposed to log aggregation. [Startup only mints it when no admin exists](../../control-plane/cmd/quasar-control/app.go#L300), so this particular failure is first-claim dependent, not guaranteed for every existing DB. TLS private-key creation is another [0700-directory/0600-file path](../../control-plane/internal/tlsx/tlsx.go#L129), and artwork writes live under the persistent state root. Merely making the root directory writable does not migrate existing private TLS keys, artwork subdirectories, or setup-token ownership. Existing setups also risk stale-token cleanup warnings and loss of the `/run/quasar/dev-agent-key` fetch recipe after a uid change. `QUASAR_APP_PUID`/`PGID` are [forwarded to app containers](../../node-agent/src/session/container.rs#L886), and provide no CP ownership adaptation.

There is no inherent conflict between UID adaptation and the CP's sound reason for bypassing the shared application entrypoint. The production [Dockerfile comment](../../deploy/Dockerfile.control.prod#L132) says the static Go program handles its own signals; [`main.go`](../../control-plane/cmd/quasar-control/main.go#L236) installs SIGINT/SIGTERM handling and performs server shutdown. A **dedicated CP preparation entrypoint** can initialize only CP-owned directories, adjust the selected identity, then privilege-drop and `exec` the Go binary. That leaves Go as PID 1 with the same signal semantics, without inheriting the full application-init/tini machinery.

This would require deliberate implementation choices, not just removing `USER`:

1. Default to the existing uid/gid for compatibility; permit an explicit CP identity or carefully scoped ownership adoption for a selected writable state bind. Do not infer identity from arbitrary TLS key ownership or chown an entire user-owned host tree.
2. If adaptation is needed, startup needs narrowly scoped initial root privilege to create `/run/quasar`, prepare CP state, and handle existing CP-owned files. Root-squashed network storage, rootless user-namespace mappings, ACLs, and read-only mounts can still prevent this; report the actual path, operation, expected identity, and available remedy.
3. Create both ephemeral setup-token and persistent TLS/artwork destinations with intended restrictive modes before dropping privileges. Preserve private-key/token confidentiality, never solve by 0777 permissions or logging setup tokens.
4. Use a verified privilege-drop/exec implementation available in both image lineages, retaining consistent behavior between source and production images. Support explicit non-root `--user` mode by validating access and reporting what cannot be prepared rather than pretending adaptation succeeded.
5. Verify new and existing named volumes, a `99:100` bind, uid changes with existing key/cache files, fresh `/run` tmpfs, graceful SIGTERM, and explicit rootless/non-root modes. These are image/implementation contracts; this design does not inherently require a frozen protocol change.

An alternative fixed-uid supported path remains valid: named-volume state plus clear upfront bind ownership requirements. It does not satisfy #131's desired automatic host-owned-bind behavior, but it explains why “non-root image” itself is not the defect. The mismatch is the generated deployment offering host binds without compatible preparation and without checking all writable directories together.

## Corrections to the umbrella's claims

- **Log level is an actual operator choice, and omission is not the claimed effective drift.** [Agent logging](../../node-agent/src/logging.rs#L49) defaults to `info`; [CP config](../../control-plane/internal/config/config.go#L232) defaults to `info`. The canonical Compose values also default to `info`. A debug-level provisioning skip is invisible in both cases. The defect is failure severity/placement and readiness propagation, not simply absence of a Compose logging key.
- **“Every stop was detectable from inside the container at boot” is false literally.** A nonexistent image tag prevents that container existing, and absent required Docker devices can prevent its creation too. An installer/pre-create preflight must cover those; image entrypoints and agent readiness cover later stages.
- **“None self-describing” is too broad.** The issue itself correctly exempts the setup-token error. [CP startup](../../control-plane/cmd/quasar-control/app.go#L324) explicitly names the token-write failure and writable-path override. Better configuration can prevent it, but it is not a silent failure.
- **Facts move to different layers.** The fixed loader prefix can move into an entrypoint; NVIDIA toolkit capability injection cannot be fixed by setting a variable after container creation; persistent host paths cannot be replaced with container-local mkdir. Fewer displayed Compose settings alone is not a correctness proof.
- The runtime/default defects and ownership issues above apply to general Linux deployments. Unraid's common `99:100` identity and storage layout expose them; no Unraid-specific code path is needed to trigger them.

The parent report covers registry/workflow verification, actual wizard generation, supplied logs, the six-stop sequence, and other introspection failures. This appendix makes no claim that a generic preflight could prove a working stream: compositor/plugin construction, app hardware initialization, ICE connectivity, audio/input, and actual decode need an end-to-end fresh-install gate.
