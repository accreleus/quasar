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
selected chain a placed session actually starts at, given the host's encoder
set, the device's decode capability, and the failure history. Codec-blind
placement is deliberate: the host is chosen first, the rung second.

**Cert cap** — the encoder-certification downgrade. A host's measured verdict
for the resolved rung can be `unsafe` (or `capped` without stable live writes),
in which case the launch hops once to the next-lower *chain* and re-resolves.
The lookup is per rung; the remedy is per chain. A missing cert row is
optimism, not a refusal.

**Stream plan** — the whole post-placement decision as one value: the resolved
chain and rung, whether the cap fired, the decision record, and exactly what to
persist. Computed from gathered inputs with no I/O, so the decision is
separable from the reads that feed it and the write that records it.

**Probe** — a device-capability measurement reported by a client (bandwidth,
RTT, max decode height, refresh rate, decode matrix). Per account, not per
launching client: the latest probe may describe a different device than the one
launching now, which is why the H.264 lift is keyed on the request's declared
client type as well as the probe.

**Envelope** — the conservative ceiling derived from a probe: a safe bitrate
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

## Platform releases

**Platform release** — a matched set of Quasar's own images (control plane, which
carries the web client, and node agent) built from one commit and published
together. It is Quasar updating Quasar, and it never reaches the app catalog:
catalog images have their own version and push machinery. _Avoid_: "update"
(overloaded — catalog images are also "updated", and `redeploy.sh` "updates" a
source checkout), "image version" (that is the catalog term), "build" (a build
may never be published).

**Channel** — which platform releases an admin is shown. `stable` is a tagged,
noted release; `edge` is whatever was last published from a branch, with no
notes. An instance follows one channel at a time. _Avoid_: "track", "branch"
(edge follows a branch, but a channel is the admin-facing choice, not the git
object).

**Release manifest** — the machine-readable description of one stable platform
release: which component images it contains, by digest, and the commit they were
built from. Published with the release, from the same tag, so the human-readable
notes and the digests cannot disagree. _Avoid_: "release body" (the notes are
for people; the manifest is what the control plane reads), "catalog manifest"
(that is the app catalog's file).

**Updater** — the per-host actor that pulls a platform release and recreates the
containers it replaces, because a container cannot recreate itself. It acts only
when told to, and only on the stack it sits beside. _Avoid_: "sidecar" in
prose (that is how it is deployed, not what it is), "agent" (the agent asks; the
updater acts).

**Install mode** — how a host got its platform images: from the registry, or
built from source on the host. A source-built host can be told about a release
but not given one. _Avoid_: "dev host" (a source-built host may be production),
"pinned" (a registry install is always pinned; the word adds nothing).

**Attempt** — one target's move to one digest set: the control plane, or one
host. Every apply produces one, whether it succeeded or failed, and it is the
only durable record of what that target was on before. _Avoid_: "job" (an
attempt is operator-initiated and rides no schedule), "task".

**Fleet run** — one release applied across the whole instance: the control plane
first, then every eligible host in sequence. At most one is active. A host that
cannot take the release at its turn is **skipped**, which is not a failure; a
target that fails stops the run where it stands. _Avoid_: "rollout" (implies
staging and percentages, of which there are none), "batch" (the run is strictly
sequential), "deployment".
