# #173 — live evidence record (2026-09-12)

Item 4 of #173: the fleet fixes exercised through **published artifacts** on the operator's
appliance stack (a registry install on the `edge` channel, one enrolled host, AMD iGPU).
Every build is named by its develop commit; digests are the `sha256:` prefixes the release
card served at apply time. Items 1–3 of #173 are #175, #177 and #176, closed on their merges.

## Builds used

| develop commit | schema | role in the record |
|---|---|---|
| `8a86cd2` (#185 branch tip) | 83 | starting point of the stack |
| `9a3267a` (#182 merged) | 84 | **migrating** release (83 → 84) |
| `67f072b` (docs only) | 84 | **non-migrating** release (84 → 84) |

## Gates

**#122 — the unattended path refuses a migrating release. PASS.** With
`platform_auto_apply=true`, "Check now" at 06:01Z found `9a3267a`; the detection job's
summary recorded `auto_apply: carries_migration` with the release id and no run id, the
control plane logged `platform auto-apply: nothing started reason=carries_migration`, and no
run row was created.

**#170 — an admin's own cordon survives a fleet apply. PASS.** The host was cordoned by an
admin (`POST /v1/hosts/{id}/drain`), then `9a3267a` was applied manually. Run
`e8ec4174`: `succeeded` in 21 s, control plane `sha256:e8f9408…` → `sha256:6aaad66…`, host
`sha256:05dcf8e…` → `sha256:4b93f3d…`, schema 83 → 84. The run recorded
`cordoned_hosts: [{was_cordoned: true}]`, stamped `cordons_restored_at` (migration 0084),
and the host was still `draining` afterwards — the cordon was put back, not lifted. The
opposite half (`was_cordoned: false` → uncordoned at the end, host `online`) is the run below.

**#153 + #128 — a browser session streams through an unattended, non-migrating apply,
with rendering proven. PASS.** A `Quasar Bench: Snow` session (1080p60, h264, 8 Mbps) was
held by a single measuring peer on the `aux-infra` host. With `platform_auto_apply=true`,
"Check now" at 06:13Z started run `f85c55af` unattended (`unattended: true`,
`requested_by: null`, `force: false`) on `67f072b`. The control-plane step ran
06:13:14 → 06:13:17 (`sha256:6aaad66…` → `sha256:8846cd3…`); across it the browser series
(777 samples at 500 ms) shows `decodeFailed=0`, width 1920 throughout, fps min 56.2 / mean
60.0, luma mean 94.7 (Snow renders; black reads ~2.8). The agent logged
`sessions-held-for-grace` → `session-grace-cleared` → `sessions-survived-reconnect`; the
control plane re-adopted the run after its own recreate and ordered no drain. The session
stayed `running` for a further 6 min 15 s until its peer's hold expired; the host step
(`sha256:4b93f3d…` → `sha256:6be3617…`) waited in `waiting_sessions` for that natural drain
because an unattended run never carries `force`, then completed within 8 s of it.

**Final state.** Control plane and host on `67f072b`, schema 84, both targets `up_to_date`,
every preflight check `pass`; an independent fresh session afterwards decoded at 60 fps,
luma 94.7.

## Notes for the next pass

- The updater is not part of a release: it stays on its pinned tag across applies.
- A "session survives X" gate needs one long-lived holding peer; the stock `qses run` deletes
  its session at the end. A luma-sampling hold mode in `peer-driver.mjs` would save re-patching.
- Two-host gates (skip-then-continue; an offline host's cordon left alone while another host
  proceeds) follow in the next section once a second host is enrolled.

## Two-host gates (the `aux-infra` host enrolled temporarily, then restored)

Release under test: develop `752d6c2` (schema 84, non-migrating). Host A = the aux-infra node,
host B = the appliance's own node; both registry installs on `67f072b`, both `online`, every
preflight check `pass`.

**#169 — skip, then continue to the next eligible host. PASS.** A's agent stopped (A
`offline`). Run `2c086004`: control plane `sha256:8846cd3…` → `sha256:5219864…`; A skipped
`host_offline`; **B attempted and succeeded** (`sha256:6be3617…` → `sha256:d795769…`); state
`succeeded_partial`, `skipped: [{A, host_offline}]`; `cordoned_hosts` both `was_cordoned: false`,
`cordons_restored_at` stamped. Afterwards A was `offline`, not `draining`; A's agent started and
registered `online` with no manual uncordon; Retry (`retry_of`) ended `succeeded` with A applied
and B `up_to_date`.

**#170 — a run that fails at one host while another was already offline. PASS.** Both hosts
reverted to `sha256:6be3617…`, A's agent stopped, a squatter holding B's health port. Run
`a2bac317`: A skipped `host_offline`; B's new agent logged `health-bind-failed`, the updater's
restore failed on the same held port (`recreate_failed`, then the restore's own failure); run
`failed`. Through the failure A and B read `offline`; **neither was left `draining`**. Port
freed → B's agent came back on its previous digest and registered `online` in 27 s with no
manual uncordon; A's agent started → `online`. A plain fleet apply `778b487b` then brought both
current (`succeeded`).

Two deviations, both explained by the code: the B attempt's reason is `timeout`, because with
no agent left to relay it the updater's `recreate_failed` never reaches the control plane
(#201); and the run's `cordoned_hosts` was `[]`, because a run whose control plane is already
current takes no fleet cordon (#200, a latent stranded-cordon path). Enrolment also surfaced
#199 (a stale node secret defeats re-enrolment and the error names the wrong remedy).

The aux-infra host was restored to its prior config byte for byte (its `.env` hash unchanged,
backup kept) and the temporary enrolled stack removed with its volumes.
