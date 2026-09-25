# CONTEXT

The domain glossary for Quasar. One entry per term the code and the docs both
lean on. Use these words exactly; the "avoid" notes are there because the
synonym means something else in this repo.

This file is grown lazily — a term is added when a piece of work actually
resolves it, not upfront. Architecture invariants live in `CLAUDE.md`; wire
contracts live in `protocol/`.

## Session launch

**Launch profile (a "chain")** — an ordered list of rungs offered to a user as
one choice ("1080p60"). It is a chain, not a setting: the session starts at the
rung the resolution walk picks, which may not be the top one. Persisted in
`launch_profiles`; the id is also the key the cert-cap downgrade ladder hops
between. _Avoid_: "quality preset", "tier" (tier is the separate pre-UI-P4
legacy path).

**Rung** — one concrete, dispatchable point on a chain: resolution, fps, codec,
nominal bitrate, playout₀. Persisted in `stream_profiles`. A cert measures a
rung, because a rung is the only thing that has a single encode cost; a chain
does not. _Avoid_: "stream profile" in prose (it is the table name, not the
concept), "quality level".

**Rung resolution** — the post-placement walk that picks which rung of the
selected chain a placed session actually starts at, given the placed GPU's codec
set, the device's decode capability, and the failure history. The GPU is still
chosen first and the rung second; an explicit codec is a *codec constraint* on
that choice, and an Auto launch's *codec preference* orders it.

**Cert cap** — the encoder-certification downgrade. A host's measured verdict
for the resolved rung can be `unsafe` (or `capped` without stable live writes),
in which case the launch hops once to the next-lower *chain* and re-resolves.
The lookup is per rung; the remedy is per chain. A missing cert row is
optimism, not a refusal.

**Driver identity** — the opaque per-GPU fingerprint of the driver and encode
stack a host is running (`nvidia:610.57.04`, `vk:radv:Mesa 25.3.6`), reported on
`capacity` and stamped onto every cert row written for that GPU. It says which
measurements still describe reality: a cert measured before a driver change
describes software that is no longer installed. Compared for equality, never
parsed. _Unknown is a value_ — an agent that reports none, and every row written
before it existed, keep their measurements applicable.

**Stream plan** — the whole post-placement decision as one value: the resolved
chain and rung, whether the cap fired, the decision record, and exactly what to
persist. Computed from gathered inputs with no I/O, so the decision is
separable from the reads that feed it and the write that records it.

**Device probe** — a device-capability measurement reported by a client
(bandwidth, RTT, max decode height, refresh rate, decode matrix). Per account,
not per launching client: the latest device probe may describe a different
device than the one launching now, which is why the H.264 lift is keyed on the
request's declared client type as well as the device probe. _Avoid_: bare
"probe" in new prose (a *host probe* is a different thing, see Host readiness).

**Envelope** — the conservative ceiling derived from a device probe: a safe bitrate
cap and a playout₀ bump. It only ever lowers. It is applied to the *final*
rung's bitrate, not just the pre-placement one, or a fall-through to a lower
rung would restore an unclamped number.

**Entitlement** — whether a caller may launch an app. The subject is `all` or a
specific user; there is deliberately no admin arm. The authorization boundary
is the transactional check inside scheduling, not the pre-check on the launch
path — the pre-check exists only so an unentitled caller gets 403 before any
other gate can leak a 409.

**Home** — the per-(user, app) persistent storage a managed-home app mounts. A
derived tile borrows its parent's home, which is why a tile is placed with a
hard host pin rather than a locality preference.

**Derived tile** — an app row whose `parent_app_id` is set: a per-user game
tile discovered inside a parent app's library. It inherits the parent's
runtime, image, and resource demand; a handful of fields (default profile,
profile policy) stay on the tile.

**Generation** — one app container launched into one session: the gen-0
container the session boots with, and each replacement a swap launches after
it. Its container name carries the number (`quasar-sess-<sid>-g<n>`), and its
exit slot, log ring, presented-baseline and intentional-stop marker belong to
it alone, so nothing an earlier generation's observer reports late can be
attributed to its replacement. The runtime API knows nothing of generations:
each launch attempt is its own durable operation, and stop/cleanup act only on
that exact identity. _Avoid_: "retry count" (a rolled-back swap relaunches the
previous app under the previous generation's name; the counter is a name, not
a tally of what is running), and the unrelated policy epoch in `source_policy`.

**Intentional stop** — the agent's own teardown of a generation (a swap's step
2, a session stop, a drop). The marker is set on the container handle BEFORE
the engine sees the stop, so an exit the observer sees afterwards is discarded
rather than reported as an app failure. It is not proof of removal: a stop or
cleanup whose reply was lost keeps the handle and its durable operation pending
until that exact identity is reconciled. _Avoid_: "user stop" (a user can only
ask the control plane; the agent stops).

**Scratch home** — the empty, throwaway directory a warm-up bind-mounts at the
image's own home path so the app can populate it. It lives inside the staging
tree, is never seeded from a template, and becomes the template only after the
warm-up container's teardown has been proven; an unproven teardown fails the
build and publishes nothing. _Avoid_: "throwaway home" (the `agent-…` per-user
homes the homes GC reaps) and the `scratch_mount()` tempdir in the home tests.

**Legacy container** — an app or audio sibling left behind by a *pre-API* agent,
one that shell-launched `docker run --rm`. It carries this agent's owner label
and an allowed name prefix but has no durable operation journal, so it can only
be identified, never reconciled: the boot sweep re-inspects each candidate by
its immutable ID and removes it only when the exact owner label AND an allowed
prefix both hold. Everything else — a foreign owner, an unlabelled container, a
name that merely contains the prefix, an API-owned application, an audio sidecar
— is *preserved and counted* for operator review, and a removal this pass cannot
prove is left to the next boot rather than retried immediately. Removal is
boot-only, behind the persistent owner lease. _Avoid_: "orphan" (the old CLI
sweep's word; it suggested anything unclaimed was ours to delete), and "adoption"
— nothing here resumes or observes a prior session.

## Stream health

**Verdict** — the single stream-health judgement, as a value: the state (a
string the control plane owns and grows), the prose evidence, and the
falsifiers that would overturn it, plus the window it was computed over (with
per-source sample counts), the clock quality, and the evidence tier. Computed
by the classifier, returned by `GET /v1/{admin/}sessions/{id}/verdict` and
carried by the diagnostic bundle as `classifier`. Observational — it never has
session authority. A consumer that does not recognise the state string reports
it verbatim; the string is data, not a contract. _Avoid_: "health status",
"classification", "the classifier's answer" — those name the mechanism, and
they invite each consumer to grow its own.

**Capture** — a bounded, admin-triggered observation of a live session: arm it,
the agent observes within a byte *and* time budget, and reports once as a single
`diag.*` trace event. Single-flight per session — a second is refused, never
queued — and never a probe on the media path: it reads what the pipeline already
is, inserting nothing. Observational, like a verdict: arming, polling, or reading
one never moves a session. Exempt from both the rolling window and the
post-mortem retention, because it is the one thing on the timeline a human asked
for rather than a clock emitted; it leaves only with the session row.
_Avoid_: "dump" (a capture is bounded and knows what it may not contain),
"debug mode" (nothing is switched on and left on), "trace" (that is the
continuous record this rides in).

**Negotiated caps** — the caps the encode branch actually agreed, as opposed to the
caps it was asked for. They are re-stated after **every** renegotiation
(`caps.negotiated`), not captured once: a scale-stage rebuild renegotiates the branch,
and on the Vulkan path every resolution rung step is an encoder restart, so the launch
snapshot (`session.effective_media`) stops being true the first time the ladder moves.
The live `profile` is the field this exists for — a probe that let the encoder choose
negotiated `main-444` where every production session pins `main`, and the difference read
as a driver regression. _Avoid_: "configured caps" (that is the request, and the whole
point is that the two can differ), "encoder settings" (properties, not an agreement).

**Stall** — encoder OUTPUT silence at or beyond the threshold while INPUT keeps
arriving. It always carries a reason, because the same silence means three different
things: `no_output` (the encoder itself), `input_starved` (nothing is being fed to it —
look upstream), `negotiation` (the graph cannot agree a format). One open stall at a
time, reported on entry and on recovery. _Avoid_: "freeze" — that is the CLIENT-side
RVFC term for a presentation gap and can happen with a perfectly healthy encoder;
_avoid_ "hang" and "ring stall" (the second names a mechanism that has been guessed at
more often than observed).

**Xid** — an NVIDIA kernel fault record: a numbered fault the driver wrote to the kernel
ring buffer. It is a fact the host **reports**, not an inference from a failure string —
that distinction is the term's whole job, because the agent also infers device-loss from
error text and the two must not be confused. An Xid belongs to the GPU, not to a session
(the kernel does not know whose work faulted), so it is reported to every session running
at that instant. Its absence is only evidence when `/dev/kmsg` is readable — see the
`xid_visibility` readiness check. _Avoid_: "GPU crash" (most Xids are not fatal),
"driver error" (an Xid is a class of record, not a diagnosis).

**Metric manifest** — the one dictionary of every metric key on either
telemetry wire, and of the four things a number needs before it can be read: its
unit, the clock its value sits on, the window it summarises, and the estimator
that produced it — plus the key carrying its sample count, and whether the key
is stored at all. It lives at `docs/session-trace/metrics.json`, beside the
golden threshold file and for the same reason. The taxonomy, the browser ingest
allow-list, the diagnostics-panel labels and the `trace-format.md` §2 table are
all **derived** from it, mechanically, so none of them can drift from it; adding
a key means editing the manifest. It is also where a key that is posted and then
dropped, or declared and never produced, is named as such — an absent series and
an empty one look identical, and only the manifest says which is which.
_Avoid_: "metrics list" (it is not an inventory, it is a semantics table),
"metrics schema" (that is the OpenAPI type, which says shape and not meaning),
"field dictionary" alone (`schema.md` keeps that name for storage shape).

**Falsifier** — one named, estimator-qualified number a verdict relies on: a
taxonomy series name, the estimator applied over the window (`p10`, `p95`,
`max`, `delta`, `mean`, `any`), the value, the op/threshold/unit of the
condition, the sample count, and whether it holds. To overturn a verdict,
overturn a falsifier. A series with no samples reports a null value and
`holds: false`, never a silent pass. _Avoid_: "evidence" for the numeric kind —
`evidence` stays the prose list beside it, and the two are deliberately
separate.

**Present cadence** — the distribution of RVFC frame-to-frame presentation
intervals over a one-second window, and its summary: median and mean, p95 and
max, σ, the doubled share, the long-frame count, drift against the display, and
the sample count. The distribution is the measurement; every scalar is a view
onto it, and each one is reported with the estimator that produced it. _Avoid_:
"present fps" as the name of the concept — that is one estimator of it, and for
years it was the mean, which is how a healthy 1440p120 session got investigated
on 2026-08-22.

**Vsync beat** — the doubled-interval pattern that appears inherently when the
source frame rate equals the client's display refresh rate: two free-running
clocks, so the renderer occasionally misses one vsync and the next frame lands
on schedule again. Nothing is dropped and nothing freezes; only a mean-derived
fps moves. It is read together with the long-frame count, which the beat never
produces: beat with zero long frames is a healthy stream, a long frame is a
stall. _Avoid_: "stutter", "judder", "micro-drop" — those name a defect, and
this is not one.

**Clock alignment** — the act of putting browser-clock points on the host clock
using the measured offset, so a cross-source claim can be made at all. The
uncertainty travels with every aligned point: a coincidence is asserted with a
tolerance (never tighter than the reporting cadence), not by comparing two
timestamps as if they were exact. An **unmeasured clock never produces a
cross-source coincidence claim** — the claim is downgraded to what one source
supports on its own and labelled as such, rather than made quietly on two
unaligned axes. The sign convention lives in one place
(`control-plane/internal/telemetry/align.go`); a verdict reports whether the
offset was `applied`, not merely measured. _Avoid_: "clock sync" (that is the
ping/pong that produces the offset, not the act of using it), "correcting the
timestamp" (the reported stamp is kept beside the aligned one, never overwritten).

**Warm-up exclusion** — the first seconds after a session reaches running, left
out of the two rules that the ramp would otherwise decide: hitch detection and
the host frame-rate floor. The pipeline is filling, the receiver buffer is
inflating and the encoder has not reached its rate, so those samples describe
the start-up, not the session. They are still **served** — every point is in the
bundle — they are not *judged*, and how much was excluded is reported
(`window.warmup_excluded_ms`) so the exclusion is visible. _Avoid_: "ignoring the
first samples" (nothing is discarded), "settling time" (that names the physical
ramp; this names the rule about it).

**Rolling window** — the live per-session telemetry retention: while a session is
non-terminal, samples and trace events older than the window are swept, so a
long-lived session has bounded rows. Measured against the server-side ingestion
clock, never a reporter's timestamp. It is **not** a read window — the 2-10
minute span a trace or bundle request asks for is what a caller wants to see; the
rolling window is what the server still has. _Avoid_: "retention window" alone
(there are two), "the prune" (that named a DELETE on the ingest path, which no
longer exists).

**Post-mortem retention** — what a session keeps once it is terminal, and for how
long. Reaching a terminal state **freezes** telemetry rather than deleting it:
the rolling window stops being applied, and whatever the session had is kept for
the post-mortem retention so a verdict or a bundle still answers on a session
that failed hours ago. After it expires the samples, the non-capture events and
the clock row are swept. Captures are outside it. _Avoid_: "terminal prune" —
that named the opposite behaviour, deleting a session's evidence at the moment an
operator would go looking for it.

**Log token** — the stable, kebab-case name a node-agent WARN or ERROR line
carries as its first field (`token = "encoder-stall"`), naming the *condition*
rather than the sentence. It exists so a cause can be found again: prose gets
reworded, translated into a better explanation, or split across two arms, and
every grep pattern built on it rots. A token names one condition — two call
sites may share one only when they mean literally the same thing, and
`node-agent/tests/log_convention.rs` fails the build otherwise, as it does for a
WARN or ERROR that carries no token at all. The convention (levels, naming, how
to grep) is `.claude/rules/agent-logging.md`. _Avoid_: "error code" (a token has
no numbering and no stability guarantee to any client — it is for humans and
agents reading logs, never for a wire contract), "log tag" (that reads like the
tracing `target`, which is the module path and a different axis).

## Images

**Platform image** — a container image that runs Quasar itself, or builds it:
the control plane, the node agent, the build/test environment, the GStreamer
toolchain artefact. Built from this repo by `deploy/build-images.sh` against
`deploy/image-contract.json`, published by `.github/workflows/images.yml`.
_Avoid_: "our images" as a category — it hides the split from session images,
and the two have different owners, cadences, and validation.

**Session image** (also **app image**) — the container image a *session* runs:
the game, the desktop, the launcher. Built in the separate `quasar-images`
repo, named per app, referenced from the catalog's `runtime_spec.image` and by
`QUASAR_APP_IMAGE`. It is never validated against the platform image contract
and never renamed by platform work. _Avoid_: "runtime image" for this — that
phrase names the node agent's own image in `hosts.json` and in
`build-images.sh`'s `runtime` role.

**Role, not implementation** — the naming rule for platform images: an image is
named for the job it does, never for the technology that happens to be inside
it. `quasar-vulkan` broke this (it described an encoder path, so it went stale
the moment a second encode path shipped in the same image and misled anyone
choosing between it and `quasar-nv`). Current names:

| Role | Image | What it is |
| --- | --- | --- |
| `control` | `quasar-control-plane` | Control-plane production image |
| `runtime` | `quasar-node-agent` | Vendor-neutral node agent (AMD/Intel VA + Vulkan) |
| `nv` | `quasar-nv` | `runtime` + NVIDIA CUDA runtime libs. **Deprecated pending #545** — being retired, not renamed |
| `dev` | `quasar-agent-dev` | Build/test environment; never deployed as an agent |
| `toolchain` | `quasar-gst-toolchain` | Patched-GStreamer build artefact, tagged by content hash |
| `profiling` | `quasar-profiling` | PROF-02 capture variant; never validated, never promoted |

The pre-2026-08-26 names (`quasar-control`, `quasar-vulkan`, `quasar-toolchain`,
`quasar-dev`) are published and locally tagged as deprecated aliases for one
transition window; the removal condition is recorded in
`.github/workflows/images.yml` and on `role_legacy_image()` in
`deploy/build-images.sh`. _Avoid_: naming a future image after its encoder,
GPU vendor, or library — that is the mistake this rule exists to stop.

## App catalog

**Manifest provenance** — where the served app catalog came from: the sha256 of
the manifest bytes it was parsed from, the ref and the exact URL fetched, the
upstream commit that ref resolved to, and the digest this one replaced. Recorded
on every successful sync, in the same transaction as the catalog rows, so it can
never describe a manifest other than the one being served. It authenticates
nothing — the manifest is fetched by unauthenticated HTTPS GET from a mutable
ref, and signing it was ruled out for a self-hosted product (#548) — so its one
job is making a silent swap visible: a change is flagged on the admin catalog
page and logged (`token=catalog-manifest-changed`). _Avoid_: "manifest
signature" and "manifest verification" (nothing is verified), "manifest digest"
on its own when the ref/commit/URL are also meant (the digest is one field of
the record).

## Host management (RH05 proposal)

**Setting source** — how a supported host setting is chosen: Automatic,
deployment baseline or explicit value. Clearing a legacy override selects the
deployment baseline; it does not request Automatic. _Avoid_: "default" without
naming the source.

**Configuration applied** — verified evidence that a requested setting group is
active for its declared scope. A saved edit or accepted command is not application.
Next-session application leaves existing sessions on their previous values.
_Avoid_: "saved" or "received" as synonyms.

**Idle apply** — an operator-approved disruptive setting group that bars new
assignments, waits for active and local work to finish, then applies and verifies.
Waiting never authorizes ending a session. _Avoid_: "automatic restart".

**Admission restriction** — one named owner's reason a host cannot take new
assignments. Several owners can restrict the same host; each releases only its
own restriction. _Avoid_: "the cordon" when ownership matters.

**Canonical home claim** — one user's location for a managed app home, keyed by
the executable parent app when a derived tile is launched. It may be reserved,
materialized or in conflict; uncertainty never licenses a second home. _Avoid_:
"preferred host" (an existing home is a constraint).

**App placement** — the operator's selection of hosts where a canonical app may
run and be prepared. Derived tiles inherit it. A cached image does not grant
eligibility. _Avoid_: "image cache policy".

**App prepared** — required local image and preparation work have completed on a
host. It does not establish placement, readiness or browser reachability.
_Avoid_: "downloaded" when additional preparation is required.

**Home template** — an authorized prepared app home from which a new empty user
home can be initialized. Existing user homes are preserved. _Avoid_: "backup".

## Host readiness

**Host fact** — something observed about a host, carried with where the
observation came from and when it was made. A fact states what is there; it
never says what should be done about it. _Avoid_: "capability" (that is what a
runtime or encoder advertises), "setting" (that is policy).

**Readiness check** — a named verdict over one or more host facts, worded for
the operator, with the fix when it fails. It is the unit the console shows and
the only thing that can block a launch. _Avoid_: "health check" (the
container's), "preflight check" (preflight is the release evaluation that reads
some readiness checks).

**Host probe** — a bounded, disposable job the host agent runs to exercise a
real path (compositing and encoding, application GPU access, audio, virtual
input) where that path really runs, producing host facts. Distinct from a
*device probe*, which measures a client. _Avoid_: "preflight" (releases),
"self-test" (that is one process checking itself), bare "probe".

**Codec probe** — the media host probe run on one GPU for one codec above the H.264
floor (HEVC, AV1), after that GPU's own media probe has passed on the current agent
image, driver and media settings. Its pass is the evidence that admits the codec to the
GPU's codec set. A failure never blocks the GPU. When the encoder cannot open at all
(the encoder element's own open failure: the GPU has no encoder for that codec) the check
reports `unsupported`, a hardware fact that asks for no attention; any other failure reports
`fail`. `unsupported` is sticky by design: it is re-proven only when the agent image, driver,
media settings or GPU identity change, or the agent restarts. Its check is
`media_probe_gpu<N>_<codec>`. It is a host probe, not a device probe.

**GPU codec set** — the codecs one usable GPU has been shown to encode: H.264 always
(the floor), and every other codec only when its registry plan builds it (the
encoder-candidate resolution run on that GPU's own render node), no driver-compatibility
exclusion rules it out, and a codec probe on that GPU passed under the current agent
image, driver, media settings and GPU identity. The AV1 exclusion is still host-wide in
practice: one known-corrupt GPU keeps AV1 out of every GPU's Vulkan/NVENC plan, as
sessions are built.
The **host codec set** (`capacity.codecs`, `hosts.codecs`) is the union of every *usable*
GPU's set (one with `encode_slots_total > 0` — a render-node pin zeroes every other GPU,
dropping it from the union), and is never empty: H.264 with no usable GPU or no
registry. Each GPU's set is reported as `capacity.gpus[].codecs` and stored as `gpus.codecs`;
a GPU with none stored (an older agent) inherits its host's set. _Avoid_: "host codec set" for a single GPU's set, or
vice versa.

**Codec constraint** — an explicit codec on a launch (`stream.codec`), applied at
placement as a candidacy gate: only a GPU whose codec set contains it is a candidate, in
the pick, the re-check, the totals probe and every refusal diagnosis alike. All capable
GPUs busy is `capacity_exhausted`; none online is `no_host_available`; nothing is ever
downgraded to another codec. An Auto launch carries no constraint. _Avoid_: "codec
override" for the placement meaning.

**Codec preference** — what an Auto launch brings to placement instead of a constraint:
the distinct codecs of the chain's rungs in chain order, keeping only rungs the launching
device can take (decode capability, decode height, decode-failure history). It is the
order rung resolution would follow if every GPU could encode everything. Placement uses it
as a sort key only, after home locality and before load spread, so a free GPU that
encodes a better codec beats a freer one that does not; it never excludes a GPU. A
preference of H.264 alone (every usable GPU encodes it, and it is all a device with no
probe can take) is empty, and an empty preference orders nothing. The legacy tier launch
has none. It weighs only the device side: a GPU whose host clamps (hardware encoder
required, encoder throughput) rule out its best codec still ranks by that codec, so the
result can be a lesser codec than another GPU offered, never a failure. _Avoid_: "codec priority", or calling it a constraint.

**Evidence** — a host fact that came from exercising the real path, or a
definitive local observation such as an unreachable container runtime. Only
evidence may block a launch; a *proxy* (a file that exists, a firewall rule
that parses) informs the operator and never blocks.

**Indeterminate** — the outcome of a host probe that could not be concluded: a
deadline passed, a reply was lost, the runtime went away. It neither sets nor
clears a block; the last definitive result stands, with its own observation
time. _Avoid_: reporting it as a failure, or as "skip" (skip means not
applicable to this host).

**Readiness override** — an admin's recorded decision to launch on a host
despite one named failing readiness check. It never hides the check, applies to
that check only, and ends when the check next passes. _Avoid_: "ignore",
"suppress", "acknowledge".

**Readiness gate** — the control plane's use of a host's readiness report at
admission: a failing check that carries `blocks` excludes the host, the launches
that mount a home, or one GPU, and only while the report is fresh. On a stale or
absent report the gate *abstains* and excludes nothing. The blocked scopes are
derived once per report and per override change, never parsed at launch time.
_Avoid_: "readiness check" for the gate (a check is one verdict; the gate is
what admission does with them), "health gate".

**Diagnostic registration** — a host that is connected and visible in the
console while it refuses every launch, because its container runtime is
unusable or its startup cleanup has not yet succeeded.

**Host readiness vs browser reachability** — readiness is what the host can
establish about itself. Whether a given browser can reach the host's media path
is evidence only that browser can supply; no readiness check claims it.

## Platform releases

**Platform release** — a matched set of Quasar's own images (control plane, which
carries the web client, and node agent; from RH06 also the recovery actor) built
from one commit and published together. It is Quasar updating Quasar, and it never reaches the app catalog:
catalog images have their own version and push machinery. _Avoid_: "update"
(overloaded — catalog images are also "updated", and `redeploy.sh` "updates" a
source checkout), "image version" (that is the catalog term), "build" (a build
may never be published).

**Channel** — which platform releases an admin is shown. `stable` is a tagged,
noted release; `beta` is those and the prereleases among them; `edge` is
whatever was last published from a branch, with no notes. An instance follows
one channel at a time. Beta stores no releases of its own — it lists the ones
stable hides — so a switch selects differently rather than fetching again.
_Avoid_: "track", "branch" (edge follows a branch, but a channel is the
admin-facing choice, not the git object), "unstable" (that is `develop`, which
is what `edge` follows).

**Release manifest** — the machine-readable description of one stable platform
release: which component images it contains, by digest, and the commit they were
built from. Published with the release, from the same tag, so the human-readable
notes and the digests cannot disagree. _Avoid_: "release body" (the notes are
for people; the manifest is what the control plane reads), "catalog manifest"
(that is the app catalog's file).

**Release signature** — a detached signature over a release manifest's bytes,
published beside it. It covers the images through the digests the manifest
already names, so there is no per-image signature. Optional on both sides: a
release may carry none, and a host may check none. _Avoid_: "signed image" (no
image is signed), "attestation" (that is a different artifact with a different
producer).

**Trusted release key** — a public key a HOST has been configured to accept
release signatures from. A host may trust several at once, which is what makes a
key rotation a period rather than a flag day. The label beside a key is for
people; any signature by any trusted key verifies. _Avoid_: "signing key" for
the public half (the signing key is private and lives only in the release
pipeline), "certificate" (there is no chain and no expiry).

**Updater** — the per-host actor on a Compose install that pulls a platform
release and recreates the containers it replaces, because a container cannot
recreate itself. It acts only when told to, and only on the stack it sits beside.
On an owned machine the recovery actor does this job instead, and the updater
retires with RH06 (in shaping). _Avoid_: "sidecar" in prose (that is how it is
deployed, not what it is), "agent" (the agent asks; the updater acts).

**Install mode** — how a host got its platform images: from the registry, built
from source on the host, or **owned** — created and replaced by the machine's
recovery actor. A source-built host can be told about a release but not given one.
_Avoid_: "dev host" (a source-built host may be production), "pinned" (a registry
install is always pinned; the word adds nothing).

**Attempt** — one target's move to one digest set: the control plane, or one
host. Every apply produces one, whether it succeeded or failed, and it is the
only durable record of what that target was on before. Every attempt ends in a
stated outcome: succeeded, failed (restored or not), or interrupted with nothing
changed. _Avoid_: "job" (an attempt is operator-initiated and rides no schedule),
"task".

**Preflight** — the per-target evaluation, on the release view, of whether the
machinery around a target is shaped so an apply can be carried out at all: the
updater or recovery actor reachable, on a Compose install the stack directory and
overlays it will act on the ones the target was started with, on an owned machine
no conflicting container and room for a pre-update dump, the health port the next
agent start needs, the release's images resolvable. Distinct from *eligibility* (may this target take
the release) and from a host's *readiness* (can it run sessions); a host's own
readiness checks are inputs to its preflight. A blocked preflight is one
eligibility reason among the others. _Avoid_: "conformance check" (the checks
are readiness checks; preflight is the evaluation that reads them), "health
check" (that is the container's), "precheck".

**Release notification** — one outbound message announcing that a platform
release this instance could move to has appeared. Sent once per release, to an
admin-configured webhook URL, after a detection pass. It is a delivery, not a
decision: nothing it does changes what is offered, and its failure is recorded
rather than escalated. _Avoid_: "alert" (nothing is wrong), "announcement" (that
is the upstream publish), "notification" unqualified (the console banner is also
a notification, and it is the in-product one).

**Fleet run** — one release applied across the whole instance: the control plane
first, then every eligible host in sequence. At most one is active. A host that
cannot take the release at its turn is **skipped**, which is not a failure; a
target that fails stops the run where it stands. _Avoid_: "rollout" (implies
staging and percentages, of which there are none), "batch" (the run is strictly
sequential), "deployment". It cordons the whole instance for its whole life, but
its control-plane step drains the instance first ONLY when the release carries a
migration (`ReleaseRunsAMigration`): since #128 a recreate no longer ends a
`running` session, so a non-migrating step lets live sessions ride through it and
waits only for in-flight launches to settle (#153). Host steps drain as they
always did — recreating an agent does end that host's sessions.

## Deployment ownership (RH06, in shaping)

These terms describe RH06 as specified (#352) and as the contract amendment (amendment 14,
#353) spells it; that amendment awaits sign-off and nothing here is built yet.

**Platform service** — one long-running container that runs Quasar itself on a
machine: the control plane, a node agent, Postgres, the recovery actor, and later
an optional TURN relay. Distinct from a session's containers, which the node agent
creates and owns. _Avoid_: "stack" for a single service (a stack is the set a
manager groups together), "platform image" (that is what a service runs, not the
service).

**Service owner** — the one actor allowed to create, replace and remove a platform
service's container and to decide what image it runs. Every platform service has
exactly one at a time. A restart policy restarts a container; it is never an
owner. _Avoid_: "manager" unqualified, "supervisor".

**External manager** — software other than Quasar that starts containers from its
own definitions: the Compose CLI with the operator's files, a stack UI, an
appliance's container templates. On a Quasar-owned machine it holds exactly one
definition, the seed; it never holds one for a Quasar service it could redeploy.
Quasar never edits a manager's files. _Avoid_: "orchestrator" (implies scheduling
Quasar does not delegate), "compose" as a synonym (Compose is one external
manager).

**Seed** — the one container an external manager (or a single `docker run`)
declares for Quasar on a machine. It only ensures the recovery actor exists, and
is built to be stable for years; the manager, not Quasar, updates it. _Avoid_:
"installer" (that is a script run once), "bootstrap container" once the machine
is running (bootstrap is what the seed does the first time).

**Recovery actor** — the Quasar-owned container on each machine that creates and
replaces that machine's other platform services, and replaces itself by handing
over to a successor. It exists because a container cannot replace itself, and it
is what keeps a control plane recoverable while the control plane is down.
_Avoid_: "sidecar", "watchdog", "agent" (the node agent is a different service).

**Enrollment** — a node agent joining a control plane for the first time, by
redeeming an enrollment token for its host identity. Reconnecting with an
existing identity is not enrollment. _Avoid_: "registration" for this (every
connection registers; only the first enrolls), "bootstrap" (that is standing up
the first control plane).

**Enrollment token** — a secret an admin mints to let one agent enroll: single use
by default, short-lived, optionally bound to one node name, revocable, stored only
as a hash. A combined or control-only machine's own agent uses a single-use local
enrollment token its recovery actor generates at install. The static
deployment-wide token is deprecated and retires with RH06; it is no exception to
keep. _Avoid_: "join token", "API key", "break-glass token".

**Host identity** — the node name and node secret by which the control plane
recognises a host across reconnects. The agent's local state beside the secret
(its pinned control-plane certificate, container-ownership lease and
configuration journal) belongs to the same identity and moves with it. A new
node name is a new host. _Avoid_: "host id" (the database key), "hostname" (the
machine's name, which the node name only defaults to).

**Combined host** — one machine running the control plane, Postgres and a node
agent. A **GPU host** runs a node agent (and its recovery actor) only; a
**control-only host** runs the control plane and Postgres with no agent. Every
owned machine also runs its recovery actor. _Avoid_: "all-in-one", "head node",
"worker node".

**Replacement** — moving one platform service to a new specification (usually a new
image digest): stop the old container and keep it, start the new one, verify it, then
discard the old one — or restore the old one if verification fails. An attempt may
replace several services in order, the recovery actor first. _Avoid_: "recreate" (the
Compose mechanism), "upgrade" (a replacement can also revert), "rollout".

**Kept container** — the old container a replacement has stopped, with its restart
policy disabled, and holds until the new one is verified. Restoring is starting it
again, with no pull. It is the recovery actor's own, never an owner conflict.
_Avoid_: "backup container", "previous container" when the kept one is meant.

**Hand-over** — the recovery actor replacing itself: a successor starts beside it,
takes the machine's single lease only when the current actor releases it, and
verifies itself before the old actor is discarded. A successor that never verifies
is removed and the previous actor re-enabled. _Avoid_: "self-update" (that names
the whole platform feature), "restart".

**Recipe** — the container shape for one role (control plane, node agent, Postgres,
recovery actor) compiled into the recovery actor. A **recipe revision** numbers one
shape; each platform image names the revision it needs, and an actor refuses a
revision it does not carry before anything stops. _Avoid_: "template" (that was the
rejected image-carried alternative), "compose service".

**Machine inputs** — the few install-time facts a machine's recipes are rendered
with: role, node name, home and template roots, public host, ports, control URL,
detected GPU facts and database mode. They change only by a reconfigure, which is a
replacement with the same image and new inputs. _Avoid_: "settings" (agent settings
are host policy), "config".

**Machine state** — what the recovery actor keeps on its machine and nowhere else:
the machine inputs, the generated secrets, the attempt journal, the last verified
specification of each service, the pre-update dumps and the seed's state file. It
is what lets the actor act while the control plane is down. _Avoid_: "machine
config", "the volume" unqualified.

**Floor** — the oldest node-agent and recovery-actor release a control-plane release
still manages, published with the release. A host **below the floor** is not failed:
it is offered only an update. _Avoid_: "minimum version" (the floor is per release
and per component), "compatibility level".

**Developer apply** — an admin applying an arbitrary digest set, typically a branch
build from an allowlisted registry namespace, to one owned target without it being
published as a release. It is the product lane's way to test on the path users run;
it is never offered, never unattended, and bound by the same digest, namespace and
ordering rules as any apply. _Avoid_: "manual update" (that is the recipe a source or
Compose install is shown), "custom release".

**Pre-update dump** — the database dump the recovery actor takes before replacing
the control plane with a migrating release, when the database is Quasar's own. It is
the way back from a failed migration, through one printed `restore` command; the last
three are kept on the machine. An operator's own database gets no dump: its backup
is the operator's, confirmed before the update. _Avoid_: "backup" unqualified,
"recovery bundle" (withdrawn with RH06's review).

**Owner conflict** — a container on an owned machine that looks like a Quasar
platform service but lacks the installation's labels: a leftover Compose stack, a
definition a manager still holds. The recovery actor never acts on it and says so.
_Avoid_: "orphan", "foreign service".
