# Per-GPU codec advertisement and codec-aware placement (#296)

Specification for #296, written against `develop` as of 2026-09-22. The gating
decision it extends is ADR 0005 ("only evidence gates a launch"); the vocabulary
is `CONTEXT.md` under "Session launch" and "Host readiness". #296 stays the
tracking issue; this document is what its slices are built against. It touches
three frozen contracts (`agent-api.md`, `control-api.md` with `openapi.yaml`,
`schema.md`) and therefore needs the owner's sign-off on the amendment drafted
below before any placement code lands. Related: #7 (per-session GPU routing),
#297 (AMD HEVC crop), #294 (AV1 padding on resize).

## Problem Statement

A host advertises one codec set, built from which encoder elements exist in the
GStreamer registry (`probe_host_codecs`, `node-agent/src/agent.rs:963`). The
control plane stores it once per host (`hosts.codecs`), and the scheduler picks
a GPU without looking at codecs at all: the contract calls placement
"deliberately codec-blind" (`control-api.md` "Rung resolution"), and the rung is
resolved afterwards against the host's set. On a host with an NVIDIA RTX-class
GPU and an AMD iGPU whose video engine (VCN 3.x) has no AV1 encoder, the host
advertises `h264,h265,av1` because the discrete GPU can, the second session is
spread onto the iGPU, rung resolution picks AV1 from the host set, and the
encode pipeline fails to reach READY. Pinning the host to one GPU hides it.

The addendum shows it is not only a multi-GPU defect. The Vulkan encoder
elements (`vulkanav1enc` and friends) register whether or not the device behind
the render node can run them; the device is opened only when the element goes
to READY. So a single-GPU AMD iGPU host pinned with `QUASAR_RENDER_NODE` still
advertises AV1, an AV1-capable browser left on Auto resolves to AV1, and every
launch fails the same way. The media host probe (`media_probe_gpu<N>`) does not
catch either case because it only ever asks for H.264
(`MediaProbeRequest::default`, `node-agent/src/session/probe_media.rs:43`).

Two more things follow from the host-level grain. The launch panel's codec list
and its "supported / recommended" verdicts come from the union of `hosts.codecs`
over candidate hosts (`profileHostCaps`,
`control-plane/internal/session/profile_host_caps.go:15`), so a user is offered
a codec that no GPU able to take their session can encode. And a codec the user
picks by hand in Adjust (sent as `stream.codec`,
`web/src/pages/app/home/useLaunch.ts:139`) is honoured only after placement:
the host set is consulted in clamp 0 (`resolveRung`,
`control-plane/internal/session/rung.go:192`), and a host that cannot encode it
is a `409 conflict` after a GPU has already been reserved and released, not a
capacity refusal.

## Solution

Each GPU advertises the codecs it has been shown to encode, and the control
plane places a session only on a GPU that can encode the codec it will get.
The agent derives a per-GPU codec set in three layers: what the registry and
knobs can build on that GPU's render node, minus the driver-specific exclusions
already recorded in `encoder_compatibility.rs` evaluated per GPU, and, for
every codec above the H.264 floor, only with a passing *codec probe*: the
existing media host probe run on that GPU for that codec. The set rides the
capacity report as `capacity.gpus[].codecs`; the host-level `codecs` field
becomes the union over usable GPUs so an older control plane keeps working, and
a GPU that reports no set is read as inheriting the host's, so an older agent
keeps working too.

Placement becomes codec-aware in two grades. A codec the user chose by hand is
a *codec constraint*: one more candidacy gate beside the host pin, the image
gate and the readiness gate, rendered once and shared by the pick, the re-check
and the rejection classifier, so a launch can only ever land on a GPU that
encodes it. When no such GPU is free the refusal is the existing
`503 capacity_exhausted`; when no online GPU can encode it at all, the existing
`503 no_host_available`, with the message naming the codec. Nothing is
downgraded. A launch left on Auto carries a *codec preference* instead: the
codec order the chain and the client's decode probe would produce, used to
rank candidate GPUs before the load-spread keys, so an idle AV1-capable GPU is
preferred for an AV1-capable client, and a session placed on the iGPU resolves
its rung against the iGPU's own set and gets HEVC or H.264. The invariant that
falls out is simple to state and to test: `sessions.codec` is always in the
placed GPU's codec set.

The launch panel's verdicts come from the same per-GPU sets, unioned over the
GPUs that could take this user's launch, so it never offers a codec the fleet
cannot deliver. The fleet view shows each GPU's codecs beside its slots. The
single-GPU iGPU case is fixed by the same mechanism: its one GPU's codec probe
for AV1 fails, AV1 leaves that GPU's set and therefore the host's, and Auto
resolves to HEVC or H.264.

## User Stories

1. As a user on a mixed-GPU host, I want a session left on Auto to stream in a
   codec the GPU it landed on can encode, so that my second session does not
   fail within a second.
2. As a user on a single AMD iGPU host, I want Auto to never pick AV1, so that
   my launches stop failing at "encode pipeline failed to reach READY".
3. As a user who chose AV1 by hand, I want the launch to wait for or refuse a
   GPU that can encode AV1, never to silently start in H.264, so that the codec
   I chose is the codec I get.
4. As a user, I want the codec list in Adjust to show only codecs some GPU that
   could take my session can encode, so that a choice never leads to a refusal
   the panel could have predicted.
5. As a user with an AV1-capable browser, I want Auto to prefer the GPU that can
   give me AV1 when it is free, so that a mixed host gives me its best codec.
6. As an admin, I want each GPU's codecs on the fleet view, so that I can see
   why a host offers HEVC but not AV1 without reading agent logs.
7. As an admin, I want a GPU that cannot encode a codec to say so on the
   readiness card with the evidence, so that I know it is the hardware and not
   a broken image.
8. As an operator, I want an older agent that reports only host-level codecs
   and an older control plane that ignores per-GPU codecs to keep working
   unchanged, so that a mixed-version fleet during an update is not a fleet
   outage.
9. As a maintainer, I want the codec decision to remain one pure function fed
   by gathered inputs, so that the placed GPU's set is tested the way the host
   set is today.

## Implementation Decisions

**Vocabulary.** Three terms are added to `CONTEXT.md` when the work lands, and
one entry is corrected.

- *GPU codec set* — the wire codecs one GPU has been shown to encode, as the
  agent reports them on `capacity.gpus[].codecs`. The *host codec set*
  (`capacity.codecs`, `hosts.codecs`) is its union over usable GPUs.
- *Codec probe* — a host probe: the media probe run on one GPU for one codec
  above the H.264 floor. A pass is the evidence that admits that codec to that
  GPU's codec set. It is a host probe, not a device probe.
- *Codec constraint* — an explicit codec on a launch (`stream.codec`), applied
  at placement as a candidacy gate. The unconstrained launch carries a *codec
  preference* instead, which orders candidates and never excludes one.
- The "Rung resolution" entry loses "codec-blind placement is deliberate". The
  host is still chosen before the rung; what changes is that the GPU is chosen
  with the codec constraint applied and the codec preference ranking, and that
  the rung resolves against the placed GPU's codec set.

**What the agent knows per GPU.** The agent's encoder choice stays one per
host (`settings.encoder`; on a mixed-vendor host the vendor auto-default picks
one vendor, `node-agent/src/gpu_vendor.rs:60`), and it already binds each
session to its scheduled GPU's render node (`agent::bind_gpu`,
`node-agent/src/agent.rs:4391`). Per-GPU *encoder* choice (VA on the iGPU,
Vulkan on the discrete card) is #7's work and is not done here. What this spec
adds is per-GPU *codec* knowledge under the host's one encoder choice, derived
in three layers:

1. *Registry plan on this GPU's render node.* `probe_codec_support`
   (`node-agent/src/session/pipeline/encoders.rs:531`) calls
   `effective_encoder` with the literal render node `"software"`. It is called
   once per GPU instead, with that GPU's canonical `renderD*` node, so the VA
   candidates resolve to the device-prefixed element names
   (`va_encoder_candidates`, `encoders.rs:269`) and the Vulkan and NVENC
   candidates resolve as they do today. This alone changes nothing for Vulkan,
   whose elements are device-agnostic in the registry; that is the addendum's
   defect and the reason layer 3 exists.
2. *Compatibility exclusion, per GPU.* `encoder_compatibility::inspect`
   (`node-agent/src/encoder_compatibility.rs:37`) classifies every accessible
   render node and returns `KnownCorrupt` if any matches, because "host codec
   advertisements are host-wide today" (its own doc comment). It gains a
   per-render-node classification used by layer 1's per-GPU plan; the host-wide
   result stays for the readiness check `nvidia_vulkan_av1_compatibility`. The
   exclusion stays in front of the probe on purpose: the RTX 5090 / 595.99.02
   case encodes frames that decode to corruption, which a frame-count probe
   cannot see. Evidence recorded in the compatibility table and evidence from a
   probe are both evidence; neither replaces the other.
3. *Codec probe.* H.264 is the floor: it is in a GPU's set whenever layer 1
   builds it, and the existing `media_probe_gpu<N>` (which asks for H.264 and
   scores pixels, #282) keeps blocking the whole GPU when it fails. Every other
   codec the first two layers allow is admitted to the GPU's set only after a
   codec probe on that GPU for that codec has passed. The probe is the same
   child process, `media-probe --gpu N --codec h265|av1`, whose argv already
   accepts the codec (`node-agent/src/main.rs:149`), asked for 10 frames with a
   10 s budget. Its pass criterion is "the encode pipeline reached PLAYING and
   produced the requested frames without a bus error"; the pixel check stays an
   H.264-only concern, so the child no longer reports Indeterminate for a
   non-H.264 request because the pixel check could not run
   (`probe_media.rs` "pixel check NotRun"). A pipeline that cannot reach READY,
   the VCN 3.x AV1 signature, is a definitive Fail.

The rule is deliberately asymmetric, and it mirrors the client side: an
undecodable codec is a black stream, so HEVC and AV1 need an explicit `true`
from the device probe (`deviceAccepts`, `control-plane/internal/session/codec.go:57`).
On the host side an unencodable codec is a dead session, so HEVC and AV1 need an
explicit pass from a codec probe. Unknown is not advertised. The cost is a
quality window, not a failure: after an agent restart a GPU advertises H.264
only until its codec probes have run, and a launch in that window resolves to
H.264. The alternative, advertising the registry plan immediately and
withdrawing a codec on a failed probe, was rejected because it reproduces the
reported failure for every launch in the window and forever on a host whose
probe stays indeterminate.

An alternative sole mechanism was considered and kept as a later
optimisation: asking each Vulkan physical device whether it exposes the
encode extension for a codec. It is cheap and needs no process, but it is a
capability the driver advertises rather than proof the path works (the
595.99.02 card advertises AV1 and corrupts it), and it says nothing about the
NVENC or VA fallback elements. It may later be used to skip probing a codec a
device does not even claim.

**Probe scheduling and cost.** Codec probes are host probes and inherit the
RH-02 lifecycle unchanged: one probe in flight per host, never on a GPU with a
live session, pre-empted by a launch (then indeterminate), a child process
killed at its deadline, a retained last definitive result that a periodic
report never erases. Concretely, `ProbeTarget` (`node-agent/src/host_probe.rs:57`)
gains an optional codec; the check id is `media_probe_gpu<N>_<codec>`, which the
console's group rule (`web/src/lib/readiness/groups.ts:67`, strip the per-GPU
suffix) is widened to fold under `media_probe`. The check carries `source:
host_probe` and `observed_at` and **no `blocks`**: a failed codec probe removes
the codec from that GPU's set, it does not block the GPU, and the readiness
`blocks` vocabulary is not extended for it. Its status is `fail` with a summary
saying this GPU does not encode the codec and sessions will not use it there,
or `warn` at the owner's preference; the spec recommends `fail`, because the
card's colour should say the codec is gone.

Codec probes for a GPU are queued behind that GPU's media probe and run only if
it passed; a GPU whose H.264 probe failed is blocked anyway and probes nothing
else. They re-run on the media probe's triggers (agent image identity, driver
identity, media settings, GPU set) and after a launch failure that
`launch_failure::explains` maps to the media kind
(`node-agent/src/host_probe/launch_failure.rs:11`), where the failed session's
codec and GPU select the codec target to re-queue. A launch failure does not
withdraw the codec by itself; the probe decides.

Cost: a codec probe is one child spawn, one GStreamer init and 10 encoded
frames at 720p60, expected in the low single-digit seconds and bounded by its
budget. A host with two GPUs and three codecs runs four codec probes after its
two media probes, serially, so the H.264-only window after a restart is on the
order of tens of seconds. The NVIDIA first-boot case needs no special handling:
while the driver volume is provisioning the Vulkan elements are absent, layer 1
admits nothing above the floor, no codec probe is queued, and the provisioner's
self-restart re-registers and re-probes against the healthy ICD, exactly as
the media probe does today (`vulkan_plan_degradation_is_pending_driver_volume`,
`agent.rs:944`).

**Wire shape.** `capacity.gpus[]` gains one optional additive field:

```json
"gpus": [
  { "index": 0, "vendor": "nvidia", "model": "...", "encode_slots_total": 3,
    "render_node": "/dev/dri/by-path/pci-0000:01:00.0-render",
    "driver_identity": "nvidia:610.57.04",
    "codecs": ["h264", "h265", "av1"] },
  { "index": 1, "vendor": "amd", "model": "...", "encode_slots_total": 2,
    "render_node": "/dev/dri/by-path/pci-0000:0e:00.0-render",
    "driver_identity": "vk:radv:Mesa 25.3.6",
    "codecs": ["h264", "h265"] }
],
"codecs": ["h264", "h265", "av1"]
```

`gpus[].codecs` is the GPU codec set in wire vocabulary, always containing
`h264` when the GPU is usable. The host-level `codecs` keeps its shape and
becomes the union over GPUs with `encode_slots_total > 0` (a render-node pin
zeroes the others, `apply_render_node_pin`, `node-agent/src/capacity.rs:709`), so an older control plane
reads the same field with a meaning at least as true as before. `gpus[]` is
already upserted wholesale by `(host_id, index)`, so a GPU whose report omits
the field stores NULL and is read as inheriting the host set; that is the
legacy-agent path and needs no keep-if-absent rule of its own.
`codec_throughput` stays host-level (see Out of Scope). `session_assign` does
not change shape; its `stream.codec` guarantee is strengthened to "a codec in
`gpus[gpu_index].codecs`", and the agent's `ack{ok:false}` for a codec it cannot
encode on that GPU stays as the belt to the control plane's braces.

**Control-plane storage and read model.** Migration `0086_gpu_codecs.up.sql`
adds `gpus.codecs JSONB NULL`; NULL means "inherit the host's set", which is
what every existing row and every older agent produce. One SQL renderer,
`gpuCodecSetSQL`, yields `COALESCE(g.codecs, h.codecs, '["h264"]'::jsonb)` and
is the only place the inheritance is written; every query that needs a GPU's
set (the candidacy gate, the preference rank, the profile menu union, the
admin GPU list) composes it, for the reason `readinessGateSQL` and
`vramVetoSQL` are single renderers (`control-plane/internal/session/placement.go`).
Its Go twin, `gpuCodecSet(gpuCodecs, hostCodecs []string)`, feeds the stream
plan and is guarded against the SQL by a DB test in the manner of
`TestCertForRungMatchesPickCert`. The capacity ingest
(`upsertCapacityWithDetection`, `control-plane/internal/agentws/store.go:312`)
writes `gpus[].codecs` beside `driver_identity`; `upsertHostCodecs` is
unchanged.

**Placement.** Two designs were evaluated against the owner's direction.

*GPU first, then codec.* Keep the pick as it is, and only make clamp 1 read the
placed GPU's set. Correct by construction (the session codec always matches its
GPU), one query unchanged, but Auto lands wherever the spread policy sends it:
with the discrete card's three slots against the iGPU's two, the first two
sessions go to the card and the third to the iGPU, regardless of whether the
client could have had AV1 on the card's last slot. And an explicit codec would
still be a post-placement check, with the same reserve-then-409 shape as today.

*Joint (codec, GPU).* Compute the codec the session would get on each GPU and
choose the pair. Full joint selection would mean running rung resolution once
per candidate GPU inside the scheduler's transaction, moving the host-side
clamps (throughput, hardware encoder, cert cap) into placement and coupling
the stream plan to the pick; the cert cap alone reads per-rung certs and hops
chains, which has no place inside a locked pick.

*Recommended: constraint and preference.* The explicit codec is a gate; the
Auto codec is a preference that orders the pick; the rung resolves after
placement against the placed GPU's set. This is the joint decision in effect,
carried by the pieces that already exist:

- Pre-placement, the launch path computes `codecPreference(chain.Rungs, probe,
  failedRungs) []string`: the distinct wire codecs of the chain's rungs in
  chain order, keeping only rungs that survive the client-side clamps (device
  decode, decode height, decode history), so it is the order rung resolution
  would prefer if every GPU could encode everything. A pure function beside
  `resolveRung`, tested like it. The device scope is read once here and once in
  `gatherStreamInputs`; a failed read degrades to an empty preference, never to
  a refusal. On the legacy tier path there is no chain and no preference.
- `CreateParams` gains `RequireCodec string` (the constraint, wire vocabulary,
  set when `stream.codec` was given) and `CodecPreference []string` (Auto).
  `candidacy` renders the constraint as `codecGate`: `AND gpuCodecSetSQL ? $n`,
  present in the candidate query, the re-check, the totals probe, the veto
  diagnostic, the readiness diagnostic and the readiness totals, exactly as the
  pin and image gates are, so pick and re-check cannot disagree
  (`TestPickAndRecheckAgree`) and every diagnostic explains the same candidate
  set. `profileHostCaps` reuses the same renderer for the menu.
- The preference is an ORDER BY key, never a WHERE: `COALESCE((SELECT MIN(ord)
  FROM unnest($n::text[]) WITH ORDINALITY AS w(codec, ord) WHERE gpuCodecSetSQL
  ? w.codec), <max>) ASC`. An empty preference is a constant and orders
  nothing. It sits after the locality key (a home on the other host is a
  different install and must win) and before the spread keys, so a free
  AV1-capable GPU beats a freer HEVC-only GPU for an AV1-capable client. The
  owner may prefer spread first; that is a one-line reorder and is listed as an
  open question. The re-check does not order, so it does not see the key.
- Post-placement, `gatherStreamInputs` reads the placed GPU's set instead of
  the host's (`StreamInputs.HostCodecs` becomes `GPUCodecs`, read by
  `Store.GPUCodecs(hostID, gpuIndex)` through the Go twin), and `resolveRung`
  clamp 1 rejects a rung whose codec is not in it. The logged and persisted
  `rejected_by: host_encoder` string keeps its name (the reason vocabulary is an
  open set and consumers key on it); the "rung resolved" log line gains
  `gpu_codecs` beside `host_codecs`.

The cert cap needs no change: certs are already keyed on `(host, gpu_index,
encoder, rung)` and `pickCert` already matches the placed GPU's driver
identity. Slot accounting needs no change: slots are per GPU already, and a
constrained launch simply has fewer candidates. The stream plan stays pure and
its signature grows by one input.

**Refusals.** With the constraint as a gate, `classifyReject`
(`control-plane/internal/session/scheduler.go:393`) already produces the right
answers once the gate is in the totals probe: a capable GPU exists but is full
or vetoed is `503 capacity_exhausted` with `Retry-After`; no online usable GPU
can encode the codec is `503 no_host_available`; readiness as the sole reason
stays `503 host_not_ready`. Both messages name the codec ("no free GPU can
encode av1"). The web client already retries `capacity_exhausted` and
`no_host_available` quietly within a bounded budget and then presents the
server message (`useLaunch.ts:191`); it gains one wording change so the waiting
toast says it is waiting for a GPU that can encode the chosen codec when the
draft carried one. No new error code is introduced, per the owner's direction;
`ErrCodecUnsupportedByHost` (`409 conflict`) becomes unreachable on the launch
path and is kept as an invariant check and for the cert bench.

**What the client sees.** The only codec offer surface is `GET /v1/me/profiles`
(`web/src/api/library.ts:69`), whose `host_encoder_not_supported` reason is
computed from `profileHostCaps`. That query changes from `SELECT DISTINCT h.id,
h.codecs` to the union of `gpuCodecSetSQL` over the GPUs that could take this
launch: the existing candidacy without the slots term (busy counts, as today),
with the host pin for a derived tile and the image gate as today, and, new,
the readiness gate, so a codec only a freshly blocked GPU offers is not
offered. The launch panel already lists only codecs that are both decodable
and catalogued and already renders `host_encoder_not_supported` as "No host
that can encode this quality is available"; nothing in `web/src` changes for
this except that the verdicts become true per GPU. Non-admins see a reason, never
a host or GPU; the per-host detail stays behind the admin routes.

**Admin surface.** `GET /v1/hosts/{id}/gpus` (`GPUAvailability`,
`openapi.yaml`) gains `codecs: array | null` (wire vocabulary; null for a GPU
inheriting a host that has not reported), rendered by the same SQL renderer.
The fleet host expansion and the host detail capacity card
(`web/src/pages/admin/fleet/HostExpansion.tsx:134`,
`hostDetail/CapacityCard.tsx` `GpuRow`) show the codecs as chips beside the
slots, in the existing idioms and using `lib/codecDisplay.ts` for labels; no
mock covers a per-GPU codec chip, so the addition reuses the setup wizard's
codec section styling (`pages/setup/StepHosts.tsx:252`) rather than inventing
one, and the owner is asked to look at it before it is called done.
`HostSettingsResponse.codecs` keeps serving the host union. The readiness card
shows the codec probe checks under the media group with their evidence.

**Agent-side belt.** `pipeline::resolve_effective_encoder`
(`node-agent/src/session/pipeline.rs:167`) already refuses a codec whose
element is absent and never substitutes another codec. It additionally
consults the bound GPU's codec set at `session_assign` and refuses with
`ack{ok:false}` and a distinct log token when the control plane sends a codec
outside it. This should never fire; when it does it is a control-plane bug and
the token makes it findable.

**Contract amendment (amendment 12, #296).** One `quasar-protocol` amendment,
Opus-drafted, owner-signed, all additive in shape; the meaning changes are
called out. Exact edits:

*`agent-api.md`*
1. `capacity.gpus[]`: add `codecs` *(NEW, amendment 12, #296, optional,
   additive)*: the wire codec set this GPU can encode, a subset of
   `["h264","h265","av1"]`, `h264` always present on a usable GPU; a codec above
   the floor appears only after a passing codec probe on that GPU. Absent ⇒ the
   control plane reads the GPU's set as the host's `codecs`.
2. `capacity.codecs`: reword from "the wire codec set the host's active encoder
   path can produce" to "the union of `gpus[].codecs` over GPUs with
   `encode_slots_total > 0`"; shape and absent-rule unchanged.
3. `readiness`: note that a codec probe check (`media_probe_gpu<N>_<codec>`,
   agent-owned id as all ids are) carries `source: host_probe` and never
   `blocks`. No vocabulary change; the sentence is there so nobody adds a
   `codec` scope later without a decision.
4. `session_assign.stream.codec`: the control plane sends only a codec in the
   assigned GPU's `codecs`; the agent's `ack{ok:false}` on a codec it cannot
   encode there is unchanged.

*`control-api.md` and `openapi.yaml`*
5. "Rung resolution": delete "Placement is deliberately codec-blind"; state
   that the GPU is chosen with the codec constraint as a gate and the codec
   preference as ordering, and that the rung resolves after placement against
   the placed GPU's codec set. Clamp 0 is reworded from "admin/diagnostic
   `stream.codec` override" to the explicit codec (the launch panel sends it);
   its refusals become: no rung with that codec ⇒ `400 validation_failed`
   (unchanged); no free capable GPU ⇒ `503 capacity_exhausted`; no online
   capable GPU ⇒ `503 no_host_available`. The `409 conflict` arm is removed
   from the launch path (kept for the certification bench). Clamp 1 is
   reworded to the placed GPU's codec set.
6. "Admission control": add the codec constraint to the per-GPU gate list, and
   the codec preference to the ordering description.
7. "Codec resolution" (the older codec-only clamp chain): the same rewording of
   the override and the host-set clamp.
8. `GET /v1/me/profiles`: `host_encoder_not_supported` is computed over the
   union of GPU codec sets of GPUs that could take the launch, readiness gate
   applied. Reason vocabulary unchanged.
9. `openapi.yaml` `GPUAvailability`: add `codecs` (array of `Codec` | null).
   `LaunchRequest`, `Session`, `Error` unchanged.

*`schema.md`*
10. `gpus`: add `codecs JSONB NULL` *(amendment 12, #296, migration 0086,
    additive)*, NULL ⇒ inherit `hosts.codecs`; written wholesale with the GPU
    row. `hosts.codecs`: reword to the union. Ledger line for
    `0086_gpu_codecs.up.sql` (pull and rebase before authoring; renumber if
    0086 is taken).

Repo-side, and not part of the amendment: `CONTEXT.md` entries above,
`docs/configuration.md` "Multi-codec" resolution paragraph, `CHANGELOG.md`
under Unreleased.

**Two side findings, and where they go.** Both were found while grounding this
spec and both are real.

- `ErrRungCodecNotAvailable` (an explicit codec no rung of the chain uses) is
  documented as `400 validation_failed` (`control-api.md` clamp 0, `rung.go:34`)
  but the error switch in `handleLaunch`
  (`control-plane/internal/session/handler.go:421`) has no case for it and it falls through to `500 internal`. A one-case fix with a
  handler test; it lands first, independent of everything else, because the
  constraint path relies on that 400 being real.
- The certification bench pins the host only (`launchCertCell`,
  `control-plane/internal/session/cert_handler.go:608`, `PinHostID`) and writes
  the cert row under the requested `gpu_index` (`cert_handler.go:423`, `:664`),
  so on a multi-GPU host the bench session can run on a GPU other than the one
  the cert is recorded for. `CreateParams` gains `PinGPUIndex *int32`, rendered
  by `candidacy` beside the host pin, and the bench sets it. Folded into the
  placement slice because it is the same gate infrastructure; a separate issue
  is filed so it is tracked on its own.

## Testing Decisions

A good test states what a GPU can encode and what a user asked for, and asserts
what the user, the scheduler or the agent observes. Boundaries, all existing:

1. **Agent derivation** — the encoder matrix tests in `encoders.rs` gain the
   per-render-node plan (VA names device-prefixed, Vulkan names unchanged);
   `encoder_compatibility.rs` gains a per-GPU classification test where one
   GPU is excluded and its sibling is not; a pure test of the advertisement
   rule (registry plan × exclusion × retained probe verdict → GPU codec set,
   host set = union over usable GPUs) with the pinned-host case zeroing a
   GPU out of the union.
2. **Codec probe** — `probe_media.rs`: an H.265 request with a passing encode
   is Pass without the pixel check; a pipeline that cannot reach READY is Fail;
   `host_probe` decision tests: codec targets queue behind a passing media
   target, are dropped after a failing one, re-queue on the media triggers and
   on a launch failure naming that codec and GPU; outcome tests: a codec check
   carries no `blocks`, and an indeterminate result leaves the retained verdict
   and therefore the set unchanged. The child's exit-code mapping test gains the
   non-H.264 case.
3. **Control-plane pure functions** — `codecPreference` (chain order, client
   clamps, empty on no probe); `resolveRung` against a GPU set; `planStream`
   with the placed GPU narrower than the host (Auto lands on HEVC; explicit AV1
   on a capable GPU passes clamp 0); `gpuCodecSet` inheritance; the
   `TestAdmissionSQLMatchesPreRefactor` fixture is regenerated in the placement
   slice with the diff reviewed as the only SQL change.
4. **DB tests** (`make test-db`, `-p 1`) — ingest: `gpus[].codecs` stored,
   absent stored as NULL and read as the host set; the SQL/Go twin agreement
   test; placement: a two-GPU host where only GPU 0 encodes AV1, then (a) Auto
   with an AV1-capable probe prefers GPU 0 while it has a slot and lands on GPU 1
   with an HEVC rung once it is full, (b) explicit AV1 lands on GPU 0, (c)
   explicit AV1 with GPU 0 full is `capacity_exhausted` and never GPU 1, (d)
   explicit AV1 on a fleet with no AV1 GPU is `no_host_available`, (e) pick and
   re-check agree with the gate (`TestPickAndRecheckAgree` extended), (f) the
   cert bench lands on its pinned GPU; menu: `/v1/me/profiles` union over GPU
   sets, a readiness-blocked GPU's codec not offered; handler: the 400 for a
   codec no rung uses.
5. **Console** — the readiness group pin test gains the codec probe id shape;
   the fleet GPU row component test renders codec chips and the null case.
6. **End to end** — the RH-02 fault harness gains no fake: the genuine AMD
   VCN 3.x AV1 failure on a real host is the fixture. Hardware acceptance is
   in Further Notes.

TDD applies to every slice at these boundaries. Hardware acceptance begins with
AGENTS.md's shared-host version preflight.

## Out of Scope

Per-session encoder choice per GPU and admin GPU selection (#7): this spec's
per-GPU plan is computed under the host's one encoder choice with the GPU's
render node, which is the input #7 will later parameterise. Per-GPU
`codec_throughput`: the compiled-in element table is measured on one RTX-class
card and is not GPU-aware (`element_pixel_rate_mpix_s`, `encoders.rs:185`), so
on a mixed host the iGPU inherits the card's numbers; clamp 6 never rejects on
unknown and ABR absorbs the over-estimate, and a follow-up issue is filed for
`gpus[].codec_throughput`. The Vulkan physical-device capability query as a
probe short-cut. Persisting probe verdicts across agent restarts (RH-02 decided
an agent restart re-runs probes; the H.264-only window is accepted, see the
open questions). The launch-time eligibility input carrying no codecs while the
menu does (`probeEvalInput`, `control-plane/internal/session/profile_resolver.go:44`)
is unchanged; the codec preference reads the device probe directly. #297 and
#294 are encoder-output defects on codecs a GPU *can* encode and are not
touched. Any readiness `blocks` scope beyond `host`, `homes`, `gpu`.

## Further Notes

**Evidence plan.** Three hosts, addressed by role or class, never by name:

- *A mixed host* (an NVIDIA RTX-class GPU as GPU 0 and an AMD RDNA2 iGPU with
  VCN 3.x as GPU 1, agent unpinned). Expected capacity: GPU 0
  `["h264","h265","av1"]`, GPU 1 `["h264","h265"]`, host `["h264","h265","av1"]`;
  the readiness card shows `media_probe_gpu1_av1` failed with the READY
  evidence and `media_probe_gpu1_h265` passed. Runs, from an AV1-capable
  browser (Chrome on macOS or Windows; the Linux headless harness peer decodes
  AV1 but not HEVC, `docs/testing-bench-mode.md`):
  1. *Unpinned, Auto, one session*: lands on GPU 0, AV1, "rung resolved" logs
     `gpu_codecs=[h264 h265 av1]`.
  2. *Auto, sessions until GPU 0 is full, then one more*: the extra session
     lands on GPU 1 with an HEVC rung (`gpu_codecs=[h264 h265]`), runs, and
     `sessions.codec` is `h265`. This is the reported defect's exact shape.
  3. *Explicit AV1 while GPU 0 is full*: `503 capacity_exhausted` with the
     message naming av1, the web client waits on its toast, and the session
     starts on GPU 0 after a slot frees; it never lands on GPU 1.
  4. *Explicit HEVC*: may land on either GPU; on GPU 1 it exercises #297's
     fix if merged, otherwise the known crop defect is noted and not counted.
  5. *Adjust*: with GPU 0 drained (`POST /v1/hosts/{id}/drain` is host-level, so
     use the readiness override on `media_probe_gpu0` or a temporary pin to
     GPU 1 instead), the panel offers no AV1.
- *An AMD-only host* (the same iGPU class pinned with `QUASAR_RENDER_NODE`,
  the addendum). Expected: one GPU, `["h264","h265"]`, no AV1 on the panel, Auto
  from an AV1-capable browser resolves to HEVC or H.264 and streams.
- *`gpu-test`* (single NVIDIA GPU): regression only. Capacity shape unchanged
  but for the new field, all three codecs advertised after the probes, the
  nightly budget unaffected.

Record per host: deployed component identities, the capacity JSON, the
readiness card, the "rung resolved" lines and the refusal bodies.

**Delivery.** Branch from `develop`, integrate into `develop` with the
changelog line, push as you go. The DB-touching slice keeps its stack on the
branch until merge (migration one-way rule). The amendment PR in
`quasar-protocol` is pushed and signed before the placement slice merges; the
agent-side probe work and the 400 fix do not wait for it, the wire field and
every gate do.

**Open questions for the owner.**

1. *Positive evidence only.* A codec above the floor is advertised for a GPU
   only after its codec probe passes, so a GPU is H.264-only for some tens of
   seconds after an agent restart. Accept, or persist verdicts keyed on driver
   identity and image identity in the agent data root?
2. *Preference before spread.* For Auto, the codec preference ranks before the
   load-spread keys (quality over balance). Or after?
3. *Refusal codes.* Reuse `capacity_exhausted` and `no_host_available` with
   the codec in the message (recommended, per your direction), or add a
   non-retryable `codec_unavailable` for the no-capable-GPU-online case so the
   web client does not wait on it?
4. *Menu and readiness.* Apply the readiness gate to the profile menu union
   (recommended) so a blocked GPU's codec is not offered?
5. *Codec probe status on failure.* `fail` (recommended) or `warn`, given it
   blocks nothing?
6. *Cert bench GPU pin.* Fold into the placement slice (recommended) or a
   separate ticket?
7. *A separate issue for per-GPU `codec_throughput`* — file it now?

## Implementation sequence

| Slice | Content | Blocked by |
| --- | --- | --- |
| S0 | `ErrRungCodecNotAvailable` → `400 validation_failed` in `handleLaunch`, handler test | — |
| S1 | Amendment 12 in `quasar-protocol` (owner sign-off) | — |
| S2 | Agent: per-GPU registry plan and per-GPU compatibility; advertisement rule; host union; unit tests | — |
| S3 | Agent: codec probe (child pass rule, target with codec, scheduling, re-run triggers, check shape); console group rule | S2 |
| S4 | Agent: `gpus[].codecs` on the wire; assign-time refusal token | S1, S2, S3 |
| S5 | Control plane: migration 0086, ingest, `gpuCodecSetSQL` + Go twin, `GPUAvailability.codecs` | S1 |
| S6 | Control plane: `codecPreference`, `RequireCodec`/`CodecPreference`/`PinGPUIndex` on `CreateParams`, candidacy gate and ordering, `classifyReject`, stream plan on the GPU set, fixture regeneration, DB tests | S0, S5 |
| S7 | Control plane: profile menu union per GPU with the readiness gate | S5 |
| S8 | Web: fleet GPU codec chips; waiting-toast wording for a constrained launch | S5, S6 |
| S9 | Docs: `CONTEXT.md`, `docs/configuration.md`, changelog | S6 |
| S10 | Hardware and fresh-host evidence per the plan above; issues for the follow-ups | S4, S6, S7, S8 |

## Agent coordination for implementation

The primary agent owns coordination, test design, review of every diff,
integration and publication, and escalates per `CLAUDE.md` "Model tiering".
Workers never commit, push, merge or touch the tracker; one deliverable each,
explicit file ownership, no two workers in the same files at once.

| Slice | Implementer | Why |
| --- | --- | --- |
| S0 | Haiku | one case, one test, documented behaviour |
| S1 | Opus drafts, owner signs | frozen contracts |
| S2 | Sonnet, Opus review of the advertisement rule | tables and a pure rule; the rule is the safety property |
| S3 | Opus | probe lifecycle, crash isolation, retained verdicts |
| S4 | Sonnet | wire field and one refusal on existing paths |
| S5 | Sonnet | migration, ingest, one renderer and its twin |
| S6 | Opus | scheduling, concurrency, refusal classification, the SQL fixture |
| S7 | Sonnet | one query on the shared renderer |
| S8 | Sonnet (chips: Haiku) | existing idioms, visual check by the owner |
| S9 | Haiku | prose against this document |
| S10 | primary agent | live hosts, judgment, owner contact |

A slice escalates to Opus when its ticket is ambiguous, when it touches a
frozen interface, security or concurrency, or when a cheaper model has failed
it twice. The model actually used is reported, never silently substituted.
