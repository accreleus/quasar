# RH-02 Probe-first host readiness — reconciliation and open decisions

Planning record for #210 and #211 under #207. Baseline: RH-01 as promoted to
`develop` at `b134085`. This file records what already exists, what #210/#211 ask
for that does not, and the decisions that belong to the owner. It is not a
specification; the specification follows once the decisions below are settled.
Nothing here is implemented.

## Constraints taken as settled

- The #228 runtime contract and the #229 migration/evidence resolution. One
  Quasar-owned runtime interface; mutations need verified ownership; an interrupted
  mutation is reconciled under the same operation identity before any retry;
  cancelling observation never terminates or rolls back; cleanup obligations are
  durable; no CLI fallback and no mixed CLI/API ownership.
- `docs/runtime-api-recovery.md`, including the schema-84 restriction.
- #216 (RH-05) owns durable, versioned host facts, desired/applied generations and
  idempotent delivery. RH-02 reports facts and their freshness; it does not build
  that persistence model.
- Intel stays behind its external validation boundary
  (`docs/reports/rh01-intel-external-validation.md`, #126, #149).
- Worker extraction (#212/#213), adoption (#214), updater redesign (#218),
  Podman/rootless certification (#220/#221) and TURN (#222/#223) are out of scope.

## What exists today

### The readiness report

The agent already reports about 25 named readiness checks on
`capacity.readiness`, each `{id, status, summary, remediation}`. Status is an open
string (`pass`, `fail`, `skip`, `warn`, `provisioning`); the check set is
agent-owned, so adding a check id needs no contract change. The report is taken
after `registered` and then every 15 s while connected. The control plane stores
the array verbatim in `hosts.readiness` with `readiness_reported_at` (migration
0062) and serves it on the host read path. The console renders it through one
shared readiness card used by the setup wizard's host step, the host detail page
and the Fleet hosts row; grouping by area is a web-only taxonomy pinned to the id
list by a test, and `skip` checks sit behind a disclosure (#102). The release
preflight (amendment 9) reads four of these checks rather than probing again.

Coverage by area:

| Area | Checks today | How they observe |
| --- | --- | --- |
| GPU | `render_node`, `host_render_node`, `dri_node_app_access`, `xid_visibility` | file open / sysfs / mode arithmetic |
| NVIDIA | `nvidia_egl_vendor_json`, `nvidia_eglcore_library`, `nvidia_lib32_gl`, `driver_volume_version`, `nvidia_sibling_egl`, `nvidia_driver_mount`, `nvidia_vulkan_av1_compatibility` | file stat, plus two results that already come from disposable containers |
| Encoder | `encoder_codecs` | in-process GStreamer registry lookup (element exists; nothing is encoded) |
| Input | `uinput`, `user_namespaces`, `app_apparmor_profile` | file open / procfs / securityfs |
| Network | `media_reachability` | host firewall posture parsed from `nft`/`iptables`/`firewall-cmd` |
| Mounts | `host_container_mounts` | runtime inspect of the agent's own container |
| Update path | `updater_socket`, `updater_stack_dir`, `updater_overlays`, `health_addr_bindable` | updater socket / health identity |

Not covered at all: a compositor or encoder that actually runs, audio, storage
permissions and free space as checks (raw statvfs numbers are reported on
`capacity.storage`, nothing judges them), the container runtime itself, and CDI.

### Advisory by contract

`agent-api.md` and `control-api.md` both state that readiness is advisory only: a
failing check must not affect registration, admission or scheduling, and
`schema.md` calls `hosts.readiness` never load-bearing. The agent module says the
same and gives the reason: the checks read proxies, and refusing sessions on a
false negative is worse than a red card. Nothing in the scheduler, admission or
launch path reads host readiness today. Cordon/drain is a separate operator axis.

Two narrow exceptions already exist on the agent: the boot gate exits for one
restart-fixable render-node fault (bounded at five attempts), and a launch is
refused when the NVIDIA driver host path or the sibling EGL test is broken.

### Disposable containers through the runtime interface

RH-01 (#233, #235) delivered an owned diagnostic helper lifecycle:
`run_diagnostic` / `observe_diagnostic` / `stop_diagnostic` /
`cleanup_diagnostic`, per-operation fsynced journals, ownership verified by label
and name prefix, startup recovery of interrupted helpers, exit code never coerced
to success. A fixed NVIDIA GPU diagnostic and the audio sidecar are sibling
profiles. `nvidia_sibling_egl` and the 32-bit library discovery already use it.

The general helper profile is closed at no network, no devices, one read-only
bind, read-only root, all capabilities dropped. It has no caller deadline (callers
wrap the command in `timeout`), no environment, no writable scratch and no second
mount. The application request type has all of those but is reserved for session
containers.

There is no runtime trait and no fake runtime. Tests script a Docker Engine API
double over a unix socket (the helper double has about fifty fault switches and
119 tests); real-Docker tests are `#[ignore]` and gated on
`QUASAR_TEST_RUNTIME_SOCKET`. Readiness checks are unit-tested against a fake
filesystem root. No acceptance harness under `scripts/harness` touches readiness.

### Startup order

Ownership lease → image state → cleanup sweep (diagnostics, application cleanup,
`retire_applications`, audio sidecars, legacy containers) → health bind → NVIDIA
adoption and the 32-bit probe container → managers and provisioners → reconnect
loop. Since #191 all runtime-touching preparation happens before the dial, under a
10 s budget against the control plane's 15 s handshake. If `retire_applications`
fails the agent exits 1 to protect managed homes, so a host whose runtime is
missing or unreachable never registers and shows nothing in the console.

### CDI

No code reads CDI. It appears only in prose and in one boot-fault token for stale
CDI-generated device modes. NVIDIA injection is a Docker device request
(`driver: nvidia`) plus the Quasar driver volume, with host-injected drivers
taking precedence. The pinned Bollard models already expose the engine's
`CDISpecDirs` and `DiscoveredDevices`, so observing CDI needs no new dependency.

### Browser reachability

`media_reachability` is a host-local firewall posture reading; it cannot know
whether a browser reaches the host. The browser side has a login-time device
probe (codecs, decode height, RTT) and the admin `access-check` endpoint for
origin/TLS diagnosis. No ICE connectivity probe exists; browser connectivity
diagnostics are #223 (RH-08).

### Facts and policy currently mixed

Encoder detection vs default selection; `nvidia_gap` as both report and
provisioning trigger; `boot_action` deciding process exit by matching
operator-facing check records; `encoder_codecs` reporting a result the knob and
fallback policy already shaped; `dri_node_app_access` re-deriving the launcher's
group decision; `encode_slots_total` as a vendor guess reported as capacity.

## Gap against #210 and #211

1. Nothing exercises the real media path before a launch. The encoder check is a
   registry lookup and there is no compositor or audio check.
2. Nothing blocks a workload on a failed prerequisite, and the frozen contract
   forbids it as written.
3. A host with no usable container runtime does not register, so the console
   cannot show "missing runtime".
4. Checks carry no provenance and no per-check observation time; the only
   freshness is the whole report's `readiness_reported_at`, which is refreshed
   every 15 s even when an underlying container result is a minute old.
5. Storage is reported as numbers without a verdict.
6. CDI is not observed.
7. The glossary has no entry for readiness, and "probe" and "preflight" already
   mean other things.
8. No readiness design mock exists in `design_handoff_v3`; the readiness card was
   built from sibling idioms.
9. Fresh-install evidence exists for Unraid/NVIDIA and for an AMD Compose stack
   from earlier work, but none of it used RH-02's checks, and no standard Linux
   distribution install outside an appliance or a development host is on record.

## Proposed direction (as recommended; since approved)

- Keep the readiness check as the one unit the console shows. Add to it, do not
  build a parallel vocabulary.
- Add **host probes**: bounded disposable containers, run by the agent through the
  runtime interface from the same image that will do the media work, given exactly
  the devices, mounts and environment a session would get, by sharing the launch
  path's injection code rather than mirroring it. One new closed helper profile
  with a caller deadline; same journals, ownership and recovery as existing
  helpers.
- Let only evidence gate. A check blocks workloads only when it comes from a host
  probe or from a definitive local fact (runtime unreachable, homes root
  unwritable). Proxy checks stay advisory. An indeterminate probe neither sets nor
  clears a block.
- Register in a diagnostic state when the runtime or the startup sweep fails,
  refusing every launch until the sweep has succeeded.
- Observe CDI and report it. Do not change how GPUs are injected.
- Word every network check as host-local. Browser reachability stays with the
  browser side and #223.

## Decisions put to the owner

Settled with the owner on 2026-09-18; the outcomes are in the specification
(`2026-09-18-rh02-probe-first-spec.md`) and ADR 0005. They were:
gating and the contract amendment it needs; where a block is enforced; which
failures block which workloads; override semantics; when probes run; what an
uncertain probe means; diagnostic registration without a runtime; CDI scope;
storage thresholds; vocabulary; the #211 evidence set; and UI styling without a
mock.
