# #282 — the media probe verifies pixels, not frame counts

Written before any code, per the ticket's own risk note: *"The tolerance is the whole
design."* Read with `CONTEXT.md` "Host readiness", `docs/adr/0005-only-evidence-gates-a-launch.md`
and the `readiness` paragraph of `protocol/agent-api.md` (frozen; nothing here changes it).

Companion: #281, which removes the known cause of #272. This ticket makes the next member of
that class impossible to ship green, whatever its cause.

## 1. What the probe does today, read from the source

`node-agent/src/session/probe_media.rs`, child of `crate::host_probe::media`.

| Fact | Value | Where |
|---|---|---|
| Source | **the compositor**, not a test source | `build_video_source(&pipeline, cfg, None)`, and the comment "The compositor source, not `use_test_src`: the probe exists to prove the compositor arm the session builds" |
| Size / rate | 1280x720 @ 60 | `MediaProbeRequest::default()` |
| Codec | **`h264` only** | `MediaProbeRequest::default()`, "h264 is the floor codec every host must produce, so it is what a probe asks for" |
| Frames wanted | 30 | `DEFAULT_FRAMES` |
| Budget | 15 s | `DEFAULT_BUDGET_SECS` |
| Sink | `fakesink`, with a BUFFER pad probe counting buffers **after the parser** | `build_and_run` |
| Verdict | pure fn `verdict(wanted, &Observed)`: bus error ⇒ fail, `frames < wanted` ⇒ fail, else pass | `probe_media.rs` |
| Observed elapsed | ~1.7 s of the 15 s budget (AMD test host, 2026-09-20 agent log) | live |

### The ticket's premise is wrong on one point, and it matters

#282 says: *"The probe already substitutes a deterministic source for the compositor, so the
reference frame is known."* It does not. It runs the real compositor.

That turns out to be **better**, not worse, because the compositor with no client connected is
still perfectly deterministic: `State::create_frame` calls `render_output(...)` with the clear
colour `[0.0, 0.0, 0.0, 1.0]` (`wayland-display-core/src/comp/rendering.rs`). A probe frame is
therefore a **flat, opaque black field** — a reference that needs no capture, no golden file and
no assumption about the encoder's rate control.

### Measured, before choosing anything

A session with an application that maps no window (`sleep`) reproduces the probe's picture
exactly: same compositor, same clear colour, same 1280x720 h264 encode path. Decoded in a real
browser on the **AMD test host with the #272 corruption present**:

| frame | distinct colours | dominant | share | second colour | share |
|---|---|---|---|---|---|
| 0 | 2 | RGB(0,0,0) | 0.7467 | **RGB(0,96,0)** | **0.2533** |
| 1 | 2 | RGB(0,0,0) | 0.7467 | RGB(0,96,0) | 0.2533 |
| 2 | 2 | RGB(0,0,0) | 0.7467 | RGB(0,96,0) | 0.2533 |
| 3 | 2 | RGB(0,0,0) | 0.9333 | RGB(0,96,0) | 0.0667 |

So **the corruption is visible even on an all-black picture**: between 6.7% and 25.3% of the
frame is the flat green the #272 signature paints when the encoder reads past the end of the
valid data. That is the single measurement this whole design rests on, and it is why a pixel
check on the probe's existing source is worth building at all. Had the corrupt black frame come
back uniformly black, the honest answer would have been to park this ticket.

## 2. The metric

### The correction that changed this design

The first draft scored the **luma plane only**. That would have scored **zero on #272** and
shipped a check blind to the defect it exists for. Work the observed corrupt colour back
through the colour conversion:

| what | Y | Cb | Cr | BT.709 limited → RGB |
|---|---|---|---|---|
| clean black (the clear colour) | 16 | 128 | 128 | (0, 0, 0) |
| the corruption | 16 | **0** | **0** | **(0, 95, 0)** — the measured (0,96,0) |

Only one `(Y,Cb,Cr)` produces that green, and its **luma is identical to clean black**. The
green is the *chroma* planes read at the wrong offset, so the zeros past the end of the real
data land at the right and bottom edges. (Full range gives (0,84,0), which does not match the
measurement, so the path is BT.709 limited — but the luma is identical under that convention
too, so the conclusion does not depend on which it is.)

The luma half of #272 — content displaced in columns — is invisible on a flat field by
construction. **On this probe's picture the defect lives entirely in chroma.** The metric must
therefore score all three planes.

### The score

Per plane `p ∈ {Y, Cb, Cr}`, against the **known** reference for a flat clear-colour field —
no median, no golden file:

```
ref(Y)  = 16 if the decoder's caps say limited range, 0 if full; either accepted if absent
ref(Cb) = ref(Cr) = 128
off(p)  = fraction of that plane's samples with |sample - ref(p)| > TOL_SAMPLE
off     = max over p of off(p)          # and the summary names the plane that tripped
```

Naming the plane is diagnostic and costs nothing: **chroma-only** ⇒ a plane offset or stride
defect (what #272 is); **luma** ⇒ a layout or modifier defect.

A second, range-agnostic uniformity term was designed as a cross-check — scoring each plane
against its own median rather than a known reference, so it needs no assumption about range or
clear colour at all. **It was dropped during implementation.** `luma_reference`'s
nearest-median fallback (§2, "range from caps … accept either") already gives the luma
reference the range-agnostic behaviour the cross-check existed to provide, and a second
full-plane pass per frame that nothing reads is waste on the probe's own path. `TOL_LEVEL` went
with it.

Two alternatives were measured and rejected:

- **PSNR against a captured clean frame.** Between two *different* encoders it bottoms out
  around 24 dB purely from edge ringing — the same value a genuinely corrupt frame scores. It
  only separates when both sides came from the same encoder, which a readiness probe cannot
  guarantee.
- **16x16 block-mean correlation.** 0.9825 for a clean frame against 0.9556–0.9795 for corrupt
  ones. A knife edge; rejected on the ticket's own rule.

### Reading the planes correctly

Every one of these is a false-fail source on a healthy host, and each gets a test:

- **Stride, not width.** Map through `gst_video::VideoFrameRef` and walk `plane_stride()` /
  `comp_height()`. Reading `width` bytes per row out of a padded buffer scores the padding.
- **Chroma plane geometry.** I420 chroma is `w/2 × h/2` — 640×360 here, not 1280×720.
- **Range from caps**, never hard-coded; if the caps carry no range, accept either 0 or 16 for
  luma.
- **Size from `VideoInfo`**, not from the request.

## 3. Which frames

- **Skip** the first `SKIP_FRAMES = 5` decoded frames — for first-buffer and decoder warm-up,
  *not* for rate control: a flat field is DC-predicted with no residual at any QP, so rate
  control has nothing to converge.
- **Score every decoded frame after that** (~20 of the 30), not a fixed five. There is ~13 s of
  spare budget and no reason to throw the sample away.
- **Fail iff at least half the scored frames exceed the floor.** Sustained, not one-off: a
  single decoder hiccup must not take a working host out of service, and by the bias rule a
  corruption present on fewer than half the frames is correctly a non-fail. The measured #272
  frames vary in magnitude (0.067 to 0.253) but are corrupt in **every** frame, which is what
  this rule keys on.
- **Gate on the decoded count, and drain.** The existing loop breaks at `frames >= wanted` and
  goes straight to NULL with no EOS; a decoder holds frames in its DPB, so gating on the
  *encoded* count would routinely leave too few decoded frames and trip our own Indeterminate.

## 4. The threshold — pre-registered, then measured

The tolerances are **derived from the codec and written down before the campaign**, so the
measurement confirms or refutes them rather than being tuned until the answer comes out right:

| constant | value | why this number |
|---|---|---|
| `TOL_SAMPLE` | **8** | A flat field is DC-predicted with zero residual at any QP, so reconstruction error is 0–1 code. The RGB→YUV of exact black is 16/128 ±1. 8 is more than 4× any rounding chain and half the 16-code pedestal, so it cannot be crossed by the range ambiguity either. |
| `OFF_FLOOR` | **0.01** | 1% of a 720p luma plane is 9,216 samples — a 96×96 block. Orders of magnitude above any rounding artefact, and ~7× below the weakest corruption measured (0.067). |

The instrument is built first, with `QUASAR_MEDIA_PROBE_DUMP=<dir>` writing per-frame per-plane
scores (and the decoded planes when asked). Only then the campaign, ≥10 runs each:

| population | host | how |
|---|---|---|
| healthy AMD, VA encoder | AMD test host | `QUASAR_ENCODER=va` |
| healthy AMD, Vulkan encoder (the #281 default) | AMD test host | nothing set |
| healthy NVIDIA, Vulkan encoder | NVIDIA test host | nothing set |
| **corrupt** AMD | AMD test host | `WOLF_VULKAN_LINEAR_ENCSRC=0`, the #281 diagnostic knob |

The corrupt distribution is measured **on the probe pipeline itself**, not on a session — the
0.067–0.253 figures above are session frames and do not automatically transfer.

`OFF_FLOOR = 0.01` then has to sit in the gap with a stated margin on both sides. **If the
distributions do not separate by at least an order of magnitude on both sides, this ticket
parks with the measurements and nothing ships** — a check that carries `blocks` and is merely
"probably fine" is worse than no check. Moving the pre-registered floor is a documented
decision, not a tuning step.

**A precondition to check before the campaign, not after:** that `openh264dec` actually decodes
the probe's pinned `profile` on all three configurations. This repo has a known
encoder-probe caps-negotiation artifact around an unpinned `profile`; if the decoder cannot
take the probe's bitstream every run would be Indeterminate and the campaign would silently
measure nothing.

## 5. Codec coverage — stated, not quietly widened

The probe asks for **h264 and nothing else**. The pixel check therefore verifies the h264
encode path only. **HEVC and AV1 corruption remain unverified by this check**, on every vendor.
Extending the probe to more codecs is a different ticket with a different cost (it multiplies
the probe's runtime by the codec count on every readiness refresh); it is not smuggled in here.

## 6. The deadline budget

The decode runs inside the probe's existing 15 s budget and must not extend it.

- The probe currently finishes in ~1.7 s, so the headroom is ~13 s.
- Decoding 15 frames of 1280x720 h264 with `openh264dec` is tens of milliseconds.
- The implementation still takes the remaining budget as a hard deadline. If the decode has not
  produced `CHECK_FRAMES` scored frames when the deadline arrives, the verification is
  **Indeterminate** — it is *not* a failed probe, and the probe's own frame-count verdict
  stands.

`openh264dec` is already in the agent image (confirmed live: `gst-inspect-1.0 openh264dec` →
"OpenH264 video decoder", rank marginal). **No new dependency, no image change.**

## 7. Indeterminate

The verification is Indeterminate when, and only when:

- `openh264dec` is not in the registry;
- the decode leg errored or never reached PLAYING;
- fewer than `MIN_SCORED = 5` frames could be scored;
- the remaining budget in §6 was exceeded;
- the probe's codec is not h264 (see §5) — Indeterminate, never a fail.

**Errors must be attributed by source element.** The existing loop treats *any* bus ERROR as a
probe `Fail`. Once a decoder leg is teed in, an error whose `msg.src()` is the decoder maps to
Indeterminate, not Fail — otherwise a decoder bug takes a healthy host out of service, which is
the one outcome this design exists to avoid.

### What an Indeterminate run reports — the contract ruling

#282's acceptance text says an Indeterminate verification is *"mapped to `warn`, never `fail`
and never `skip`"*. **That is not what the frozen contract says**, and the contract wins.

`warn` is wrong on the letter of `protocol/agent-api.md` (amendment 11): `warn` is "a named risk
that is never a failure (**a proxy** that cannot prove harm)", and a check carrying `blocks` may
only do so because it rests on **evidence**, never a proxy. So `warn` is unavailable to this
check by construction.

The first draft of this plan said the check should stay **`pass`** with the reason in the
summary. **That was wrong, and the reason is worth writing down.** Once this ticket lands, the
check *asserts the picture*. Consider: run 1 on a corrupt host reports `fail` and the gate sets
a block; run 2's decoder leg times out. Under "stays `pass`" the block would be **lifted on no
evidence at all**, and the corrupt host admitted again. Amendment 11 forbids exactly that, in
terms:

> Either way an indeterminate probe **neither sets nor clears a block**.

> When a host probe is inconclusive the agent keeps reporting that check's **last definitive
> result**, with its original `observed_at` … it reports `unknown` only while no definitive
> result exists for that `id`.

So: an inconclusive pixel verification makes **the run** inconclusive, and the agent takes the
path it already takes for a deadline or a pre-emption — retain the last definitive result with
its original `observed_at`, and say in `summary` that this attempt could not verify the picture
and why. `unknown` appears only when no definitive result exists for that id yet, which is also
the right answer for an image that has somehow lost `openh264dec`: `unknown` blocks nothing, so
the host loses a green tile and nothing else, while the missing decoder becomes visible instead
of passing silently.

Implementation: a fourth child exit code (Indeterminate) beside the existing `Pass`/`Fail`/
`Usage`, routed by the parent into the inconclusive path that already exists.

**One thing to verify before committing to this**, because the ticket forbids editing the
harness's expectations: that no fixture in the #264 fault harness or the console group pin test
depends on a *first-ever* result being `pass` where it would now be `unknown`. If one does, the
fallback is `pass` + summary, and then the block-lifting hole above must be closed another way —
by never letting an Indeterminate run overwrite a retained `fail`.

No frozen interface is changed either way. The discrepancy is reported on #282.

## 8. What does not change

- The check id stays `media_probe_gpu<N>`. The release preflight, the console group pin test and
  the #264 harness all key on it.
- Its `blocks` stays exactly as it is — `{scope: "gpu", gpu_index: N, enforced_by: "control_plane"}`.
- Its `source` stays `host_probe`.
- No new check id. No change to which checks block. No change to the gate's scope rules.
- The probe's source, size, rate, codec, frame count and budget are all unchanged.

## 9. When the check fails

Status `fail`. The wording stays with the **evidence** and does not over-claim a cause: #272's
cause was the compositor→encoder buffer handoff, not the encoder element itself, so naming
`QUASAR_ENCODER` as *the fix* would be wrong. It is named as the way to isolate which side is
at fault:

> summary: `GPU <N> encoded frames but the picture did not survive: <P>% of the decoded
> <planes> samples do not match the picture the compositor fed in (<E> on <node>).`
>
> remediation: `The GPU is working — the frames encoded. The fault is in the compositor-to-encoder
> handoff or in the encoder itself. Set QUASAR_ENCODER for this host to another encoder (va,
> nvenc or openh264) and recreate the agent: if the picture is then correct, the encode path is
> at fault.`

## 10. Tests

Designed here, written with the code:

- **The metric as a pure function on synthetic frames** — identical; lossy-like additive noise;
  a column displacement; a flat green region; all black; all white. Asserts the ordering
  (identical ≤ noise ≪ displacement, green region) rather than magic constants alone.
- **The verdict mapping**, including every Indeterminate case in §7, asserting that each one
  leaves the check `pass` with the reason in the summary.
- **A guard test that the check id and its `blocks` are unchanged**, so a later refactor cannot
  silently move either.
- The #264 fault harness and the console group pin test must pass **with no edits to their
  expectations**. If either needs an edit, that is a signal the change is larger than the
  ticket allows, and it stops.

## 11. The risk this design is most exposed to

A false `fail` on a healthy host takes that host out of service, because this check carries
`blocks`. Everything above is biased accordingly: a reference-free metric, a median over frames
rather than a max, a floor set from measured distributions with a stated margin, and every
inconclusive path resolving to "pass, and say so" rather than "fail".

The limitation that remains, stated plainly: a picture that is *uniformly* wrong in a way the
level check in §2 does not catch would still pass. This is a corruption tripwire, not a quality
gate — the nightly budget owns quality, per the ticket's own non-goals.

## 12. The Fable consult, and what was done with it

The first draft of this plan was sent to Claude Fable 5.1 for review before any code was
written. Six things came back; five changed the design and are already folded in above. Recorded
here because two of them were corrections of substance, not polish.

| # | What it said | What was done |
|---|---|---|
| 1 | **"Your measured corruption is almost certainly chroma-only, and your metric is Y-only. It would score `off ≈ 0` on #272."** With the arithmetic: RGB(0,96,0) is Y=16, Cb=Cr=0. | **Accepted, after verifying the arithmetic independently** (both range conventions: limited gives (0,95,0), full gives (0,84,0); the luma is identical to clean black either way). §2 was rewritten to score all three planes against a known per-plane reference. This was a design-killing flaw and it is the single most valuable thing in the review. |
| 2 | Median-of-5 is too thin a sample; score every decoded frame after a small skip and fail iff at least half exceed the floor. | Accepted. §3. There was no budget reason for five. |
| 3 | The "skip 10 for rate control" rationale is wrong — a flat field is DC-predicted with no residual at any QP. Skip a few for first-buffer / decoder warm-up instead. | Accepted. §3. |
| 4 | **The `pass`-on-Indeterminate ruling has a block-lifting hole**: run 1 `fail` sets a block, run 2 Indeterminate reports `pass`, the block is cleared on no evidence — which amendment 11 forbids in terms. | **Accepted, reversing the first draft.** §7 now retains the last definitive result, exactly as the contract prescribes, with `unknown` only when none exists. The contract is still untouched. |
| 5 | Pre-register `TOL_SAMPLE`/`TOL_LEVEL`/`OFF_FLOOR` from the codec's own properties so the campaign confirms or refutes rather than tunes. | Accepted, with its numbers (8 / 8 / 0.01) and its reasoning. §4. |
| 6 | Implementation false-fail sources: stride vs width, chroma plane geometry, range from caps, size from `VideoInfo`, bus-error attribution by source element, EOS/drain before teardown. | Accepted; each is listed in §2/§3/§7 and each gets a test. |
| 7 | Raise the picture's information content by setting a compositor background colour property, if one exists. | **Not done in this ticket.** It is a fork change and it would make #281's live exercise and this one share a cause. Recorded as a follow-up; the flat-black limitation is stated instead (§11). |

Where it was not followed: item 7 only. Everything it claimed about this repository's code was
re-read and confirmed before being acted on, and the one claim it could not check itself (the
compositor's clear colour, which lives in the fork) was verified here.
