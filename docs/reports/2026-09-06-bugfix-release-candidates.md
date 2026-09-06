# Bugfix release candidates — 2026-09-06

This is a read-only scope review, not implementation or approval to add work. It
covers the 21 open issues captured in the session inventory and verifies the
principal bug reports against `develop` at `379faa8856ad54b5ea3621dde877af2d215626be`
(the first-install/AV1 delivery, PR #138). Subsequent uncommitted work on #129,
#130 and #131 is deliberately not treated as completed here. Full bodies and
comments were read for #135, #136, #128, #126, #125, #95, #86 and #119; recent
merged PRs and the closed #140 live-gate evidence were also checked.

## Recommendation

Finish the already authorized #129/#130/#131 acceptance work. For an additional
small bugfix release, prioritize **#136 and #135**, with **#125 optional** because
it improves delivery verification rather than the installed product. Put **#126**
next in the user-impact queue, but give it a separate Intel-backed acceptance
window. Do not expand this release into session-reconnect design or speculative
browser fixes. Reconcile **#119** with the evidence already recorded under #140.

These recommendations were initially read-only. The operator subsequently selected
**#135 and #136** for this delivery; their implementation and validation are now
being completed separately. The operator also selected **#146** (ownership-aware startup cleanup) and **#145** (Steam preparation). The other candidates remain unselected. The table
records the code state at the audit baseline, not a claim that selected fixes
have already been released.

| Ticket | Verified status and user impact | Recommendation | Effort / risk and dependencies | Concrete acceptance before closing |
| --- | --- | --- | --- | --- |
| [#136](https://github.com/accreleus/quasar/issues/136) Older edge build offered as an update | Still present. `PlanRelease` sorts available releases by schema/time, but `controlPlaneReason` considers commit equality rather than comparing the installed build time. An older same-schema edge build can remain eligible. Recent release PRs fixed adoption/cordon behavior, not this ordering. | **Include if selected; first additional candidate.** Prevent an update from unexpectedly reverting application behavior. | Small-to-medium; release planning and UI wording. Preserve the database schema floor and existing reason vocabulary. Check both the displayed plan and apply-time validation, so a direct request cannot bypass the decision. | Table tests for older/newer/equal same-schema edge builds, higher-schema ordering, missing timestamps and installed stable builds switching channels; older row remains visible but not offered; live console check against a newer installed tagged build. |
| [#135](https://github.com/accreleus/quasar/issues/135) Agent reports package version | Still present. `node-agent/src/agent.rs` defines `AGENT_VERSION` from `CARGO_PKG_VERSION`; `main.rs` does likewise. `QUASAR_VERSION` is currently passed to the control-plane build, not the agent. Users see inconsistent versions for a single release despite correct commits. | **Include if selected.** Small, directly relevant to confidence in update and driver testing. | Small; build provenance, workflow and Rust identity. Keep the release tag as the shared source of truth; do not infer an unrelated ancestor tag for a branch build. Image rebuild and release-artifact verification required. | Tagged images report matching control-plane/agent versions in registration, host drawer and Releases; branch/source builds report an honest fallback; local builder and GitHub workflow both stamp the agent; ordinary tests and image contracts pass. |
| [#125](https://github.com/accreleus/quasar/issues/125) DB tests require host Go | Partially stale, but core bug remains: `scripts/dx/testdb.sh` still refuses when Go is absent. `scripts/dev/dev.sh` already defaults to `golang:1.25`, so that subtask does not need repeating. Developers on container-only hosts cannot use the promised canonical test command. | **Optional include if selected.** Useful support work for reliable bugfix delivery. | Small-to-medium; shell orchestration. Must retain fresh per-worktree database isolation, serial Go DB tests, exit propagation and cleanup. No application API change. | Run `make test-db` with Go absent from host PATH and prove containerized Go uses a fresh ephemeral Postgres; simulate failing tests and interrupted cleanup; daemon-free shim coverage; keep normal host-Go path working. |
| [#126](https://github.com/accreleus/quasar/issues/126) Intel GPU omitted | Confirmed in code and reporter logs. `capacity.rs::read_vram_mb` has only AMD and NVIDIA branches; Intel returns `None` and is discarded before scheduling. Reporters identify i3-12100 and N100 systems. The runtime does not explicitly install Intel VA driver packages. | **High-priority separate candidate, conditional on Intel hardware validation.** Entire installs cannot launch. | Medium-to-large; GPU inventory, shared-memory capacity semantics, image dependencies and encoder choice. The old comment's proposed universal switch to Vulkan is not established by the reported hardware. Preserve truthful capacity reporting rather than inventing dedicated VRAM. | Intel integrated and discrete fixtures cover available/unknown memory; inventory and placement become usable; validate actual driver/encoder registration and a real stream on the reported iGPU generation; maintain AMD/NVIDIA behavior and image contracts. Document supported generations from evidence. |
| [#128](https://github.com/accreleus/quasar/issues/128) CP restart stops streams | Still present: `agent.rs::stop_all` is invoked on connection loss. Fleet updates currently drain first, so planned updates avoid silently killing live sessions; an unplanned CP restart still ends them. | **Defer from this bounded bugfix release; separate design.** High impact, but not a safe one-line removal of teardown. | Large/high risk; reconnect ownership, authoritative session reconciliation, timeout and orphan cleanup, authentication and shutdown races. May need a frozen agent contract change and separate sign-off. | A real stream survives a 60–90-second CP recreate; reconnect reconciles state; grace expiry, explicit stop, CP refusal and abandoned containers are cleaned up; no session leaks or stale reservations. |
| [#95](https://github.com/accreleus/quasar/issues/95) Missing capture timestamps | Original sender-registration diagnosis is disproven by current code and the issue's later investigation. `rtp_ext.rs` attaches to the actual selected payloader and tests H.264/HEVC/AV1. The misleading H.264-only log was corrected. Native-client measurement still needs retesting. | **Needs reproduction; no speculative sender patch.** | Small investigation first, unknown fix scope. Needs Photon session/negotiation and receive-side evidence. | Retest HEVC and AV1 with current images; record offer/answer extmap, actual RTP extension and native latency series. If absent, locate whether negotiation, packet stamping or receiver parsing loses it before choosing a fix. |
| [#86](https://github.com/accreleus/quasar/issues/86) Chrome Escape capture | Latest reporter update says it stopped reproducing with an unchanged bundle. Current capture code requests Keyboard Lock for captured fullscreen and reports refusal. The issue's earlier lock-order/certificate theories were explicitly eliminated. | **Needs reproduction; defer code changes.** | Browser/environment investigation, potentially high regression risk for input capture. Safari's unsupported-Keyboard-Lock fallback is not this bug. | Repeat on trusted HTTPS with exact Chrome/OS/display versions; observe whether Escape keydown reaches the page and the lock promise result; verify guest Escape, hold-to-exit, both release chords, fullscreen exit and unsupported-browser fallback. |

## #119: evidence closeout, not another implementation project

[#119](https://github.com/accreleus/quasar/issues/119) is labelled an enhancement,
but it is a release acceptance ticket. Its latest own comment predates the final
stable update evidence. The [closed #140 report](https://github.com/accreleus/quasar/issues/140#issuecomment-5556383989)
records successful **0.2.2 → 0.2.3** on one host and **0.2.1 → 0.2.3** on two
hosts, with control-plane self-recreation, run adoption, final online state and
matching release identities. A Steam session remained running during the drain
wait and ended before the control-plane step. This is not survival across that
restart.

The code/changelog and merged release PRs #139/#141/#142 corroborate the update
and cordon fixes. The older repository report
`docs/reports/2026-09-05-self-update-live-gate/REPORT.md` contains the local and
prerelease runs but has not consolidated the final stable run above. Recommend
an evidence-only update linking the final timestamps, identities and apply
history, and explicitly amend/split the impossible-as-written prerelease and
session-survival criteria. Stable deliberately hides prereleases; #121 proposes
a beta channel. #128 still owns survival. Do not close #119 by claiming either
unimplemented behavior passed.

## Other open work is not an incidental bugfix

- **#129, #130, #131:** already authorized deployment/recovery work, in progress;
  their remaining acceptance is tracked separately from these optional candidates.
- **#144:** certificate reuse across driver/plugin/encoder changes is a real
  correctness concern, but needs identity design and potentially a frozen
  contract/schema change. Defer to that scoped design; the AV1 guard already
  prevents an old certificate from re-enabling an excluded codec.
- **#120:** signature verification changes release trust policy; security feature
  design, not a small regression fix.
- **#121–#123:** beta channel, unattended apply and external notifications are
  feature requests. They should not hold up the existing bugfix delivery.
- **#6–#10:** swap disposition, per-session GPU routing/selection, a second
  transport, private registries and shared/preinstalled game storage are separate
  features with lifecycle, contract or storage implications. Do not fold them
  into this release merely to reduce the open-issue count.

## Additional finding: startup cleanup crosses agent boundaries

**New release candidate, not present in the 21-ticket snapshot. Recommended P1
reliability severity; not implemented or exercised destructively in this review.**
Starting a second agent against the same Docker daemon can force-remove the
first agent’s live application and audio containers. Separate Compose project
names do not prevent it. This is general Docker behavior, not Unraid-specific.

Evidence in the reviewed source:

- `node-agent/src/agent.rs::run` invokes `sweep_orphans` at startup with the global
  `quasar-sess-` and `quasar-pulse-` prefixes, before connecting/registering with
  the control plane. The preceding enrollment check only establishes that a local
  token or saved secret exists; it does not authenticate or establish ownership.
- `session/container.rs::sweep_orphans` runs `docker ps -aq --filter name=<prefix>`.
  It includes running containers as well as stopped ones. The code itself notes
  that the name filter is a substring match, so even a name merely containing the
  prefix can match.
- Every returned ID goes directly to `force_remove`, which runs `docker rm -f`.
  There is no owning-agent/project label, active-owner check, session reconciliation
  or state inspection. App and PulseAudio creation do not add an owner label that
  the sweep could use today.

**Impact:** launching a parallel test stack, an additional agent, or a replacement
agent on a shared daemon can terminate somebody else’s running game and audio,
losing unsaved application state. An unreachable control-plane address does not
make this safe: the sweep occurs before the connection attempt. This is not an
unauthenticated remote exploit; it is an unsafe action by an agent already granted
Docker access.

**Recommended scope:** a separately selected, medium-sized fix. Establish a stable
local agent identity and apply owner labels to both app and audio containers;
restrict cleanup to that owner and prove the prior owner is inactive before
removing running containers. Exact naming alone is insufficient. Define safe
legacy-container handling explicitly: skip or report ambiguous ownership rather
than assuming all global-prefix containers are orphans. Preserve cleanup after a
real crash without reintroducing cross-instance deletion. This can likely remain
Docker-local without a protocol change, but must not invent wire identity fields.

**Acceptance:** a daemon-free command-shim test proves another owner’s running and
stopped containers, unknown legacy containers and substring lookalikes are not
removed. In an isolated Docker integration test, start two agents/projects, keep
one session running, restart the other, and prove the first session survives.
Then crash one owner and verify only its confirmed stale resources are reclaimed.
Cover failure to inspect ownership/liveness and interruption during cleanup. Never
reproduce by starting an unrestricted second agent on the production daemon.

This finding ranks ahead of cosmetic version reporting by damage potential, but
adding its implementation remains a scope decision for the operator. The ongoing
provisioning experiment uses an isolated read-only Docker proxy to avoid this
startup path reaching production resources.

## Release gate

Source merged to `develop` is not a published release or a live deployment.
Selected fixes need their component tests, database tests where applicable,
`make verify` and pre-merge preflight. Changes to images/encoders additionally
need unchanged image contracts and real GPU acceptance. Keep the public-site
instructions synchronized with the release that actually contains the fixes.
Only after the operator authorizes promotion to `main` should the documented
release workflow publish the selected set. No tracker changes or new fixes were
made by this review.

## Confirmed delivery scope and closure policy

Operator-confirmed scope: #129, #130, #131, #135, #136, #145 and #146.
The historical review above describes the baseline, not current completion.

- [ ] #129: provisioning progress, interrupted-worker recovery and actionable failure evidence.
- [ ] #130: verified driver host-path override, automatic discovery and rejected wrong-path evidence.
- [ ] #131: first-run origin persistence, signaling diagnostics and deployment/documentation parity.
- [ ] #135: tagged and source agent identity, including published image verification.
- [ ] #136: older edge rejection in planning and apply, with UI and database regressions.
- [ ] #146: owner-scoped session/audio cleanup, restart and two-agent isolation.
- [ ] #145: Steam preparation policy, lifecycle, UI, storage reporting and live cold/prepared comparisons.
  The concrete proposal is in [the design review](2026-09-06-steam-preparation-design-review.md);
  frozen-interface review/sign-off is pending.
- [ ] Complete component gates, image contracts, public documentation and live acceptance.
- [ ] Record commit/build identities, test results and remaining limitations on each issue.
- [ ] Publish the approved release and verify its artifacts and live update behavior.
- [ ] Close each issue only after the released implementation satisfies its acceptance criteria.

Do not use automatic issue-closing references in commits or pull requests. Merging
to develop is an implementation milestone; it does not satisfy the operator's
release-live closure condition. No release version is assigned until the release
contents and required sign-offs are settled.
