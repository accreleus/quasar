# #282 — the media probe verifies pixels: acceptance

| | |
|---|---|
| Ticket | #282. Companion to #281; parent spec #252, ADR 0005 |
| Branch | `fix/282-media-probe-pixels`, from the #281 branch |
| Not on | `develop`, `main`, `initiative/resilient-host-architecture`. No tag, no published image |
| Design | [`docs/superpowers/plans/2026-09-20-282-media-probe-pixel-check.md`](../superpowers/plans/2026-09-20-282-media-probe-pixel-check.md) — written before any code, including the Fable review and what was done with it |
| Images | `quasar-node-agent` from source `2cbc009` (contract **148 passed / 0 failed** / 2 GPU-gated skips), control plane `dev-901058f`. Nothing published |
| Contract | Unchanged. No frozen interface is touched |

**This does not merge.** #282 stays open for the owner, with one discrepancy for them to rule on
(below).

## What changed

`media_probe_gpu<N>` passed on the **count** of encoded frames and never looked at what the
frames contained. It now tees its own encoded output into `openh264dec` and scores the decoded
planes against the picture the compositor actually fed in.

The reference needs no capture: with no client connected the compositor clears to opaque black
(`render_output(..., [0.0, 0.0, 0.0, 1.0], ...)`), so a probe frame is a flat field whose
correct value is known per plane.

### The measurement that decided the design

The corruption **is** visible on an all-black picture, which is why this was worth building.
Reproduced as a real session with an application that maps no window — same compositor, same
1280x720 h264 path as the probe — on the AMD test host with the corruption present:

| frame | distinct colours | dominant | share | second colour | share |
|---|---|---|---|---|---|
| 0–2 | 2 | RGB(0,0,0) | 0.7467 | **RGB(0,96,0)** | **0.2533** |
| 3 | 2 | RGB(0,0,0) | 0.9333 | RGB(0,96,0) | 0.0667 |

[The probe's own picture, corrupt](2026-09-20-281-282-validation/probe-picture-corrupt-black-field.png).
Had it come back uniformly black, this ticket would have parked.

### The correction that saved the check from being useless

The first design scored the **luma plane only**. Working the corrupt colour back through the
colour conversion:

| | Y | Cb | Cr | BT.709 limited → RGB |
|---|---|---|---|---|
| clean black (the clear colour) | 16 | 128 | 128 | (0, 0, 0) |
| the corruption | 16 | **0** | **0** | **(0, 95, 0)** — the measured (0,96,0) |

The luma is **identical to clean black**. A luma-only check would have scored ~0 on the exact
defect it exists for. The score is now the worst of the three planes, and the summary names the
plane that tripped — which turns out to do real diagnostic work: every corrupt run on hardware
named `u,v`, i.e. chroma-only, i.e. a plane offset/stride defect, which is what #272 is. Full
Fable-review record is §12 of the plan.

## The threshold was pre-registered, then measured

The tolerances were derived from the codec's own properties and **written down before the
campaign**, so the runs confirm or refute them instead of being tuned until the answer came out
right:

| constant | value | reason |
|---|---|---|
| `TOL_SAMPLE` | 8 codes | a flat field is DC-predicted with zero residual at any QP, so reconstruction error is 0–1 code; RGB→YUV of exact black is 16/128 ±1. 8 is >4× any rounding chain and half the 16-code pedestal |
| `OFF_FLOOR` | 0.01 | 1% of a 720p luma plane is 9,216 samples — a 96×96 block; orders of magnitude above rounding, ~7× below the weakest corruption measured |
| `SKIP_FRAMES` / `MIN_SCORED` | 5 / 5 | decoder warm-up, and a floor on the sample |

### The distributions

Each population is **10 consecutive agent restarts**, each triggering a fresh probe; the score is
the worst plane's `off`.

| population | host | n | score | spread | verdict |
|---|---|---|---|---|---|
| healthy, Vulkan (the #281 default) | AMD test host | 10 | **0.0003** | none — identical every run | pass ×10 |
| healthy, VA (`QUASAR_ENCODER=va`) | AMD test host | 10 | **0.0003** | none | pass ×10 |
| healthy, Vulkan | NVIDIA test host | 10 | **0.0003** | none | pass ×10 |
| **corrupt** (`WOLF_VULKAN_LINEAR_ENCSRC=0`) | AMD test host | 10 | **0.2533** | none | **fail ×10** |

| margin | value |
|---|---|
| healthy → floor | **33×** below (0.0003 against 0.01) |
| floor → corrupt | **25×** above (0.01 against 0.2533) |
| healthy → corrupt | **844×** |

The populations separate by nearly three orders of magnitude, far past the "at least an order of
magnitude on both sides" bar the design pre-committed to park on. **The pre-registered floor was
confirmed, not moved.** Every score was identical run to run, on every population — there is no
observed variance to threshold against.

### Restart loop — no false failures

**40 agent restarts** in total (30 on healthy configurations, 10 on the corrupt one). Healthy
configurations passed **30 / 30**. The corrupt configuration failed **10 / 10**, which is the
intended behaviour, not a false failure. Zero flakiness.

## Hardware validation

### The corrupt configuration takes the host out of service

```
STATUS : fail
SUMMARY: GPU 1 could not composite and encode: vulkanh264enc: the picture did not survive:
         25.33% of the decoded u,v samples do not match the picture the compositor fed in on
         /dev/dri/renderD129 (worst plane over 25 decoded frames). The GPU is working — the
         frames encoded. The fault is in the compositor-to-encoder handoff or in the encoder
         itself. Set QUASAR_ENCODER for this host to another encoder (va, nvenc or openh264)
         and recreate the agent: if the picture is then correct, the encode path is at fault.
BLOCKS : {"scope": "gpu", "gpu_index": 1, "enforced_by": "control_plane"}
GATE   : {"state":"active","blocking":[{"check_id":"media_probe_gpu1","scope":"gpu",
          "gpu_index":1,"enforced_by":"control_plane","overridden":false}]}
```

and the launch is refused:

```
{"error":{"code":"host_not_ready","message":"the host that would run this needs its
 administrator's attention; try again once they have looked at it"}}
```

### The healthy configuration is unaffected

- AMD test host, nothing set: check passes at 0.0003, launch succeeds, picture clean
  (`row_std` 0.000).
- Both vendors, a real session after the change: picture clean, **mouse** (right-click opened the
  desktop menu), **keyboard** (`Ctrl+Alt+T` opened a terminal, a typed command ran and printed),
  **audio** (energy 0 → 2.05 on AMD, 0.02 → 1.98 on NVIDIA during a 440 Hz tone), normal stop to
  `stopped`, and **cleanup complete** — no `quasar-sess-*` / `quasar-pulse-*` / `quasar-probe-*`
  container left and `/run/quasar-agent` empty on the AMD host.

### Cost

| | media probe | agent log start → last probe finished |
|---|---|---|
| AMD test host, before | 1.853 s | 3.01 s |
| AMD test host, after | **1.896 s** (+43 ms, +2.3%) | 2.91 s |
| NVIDIA test host, before | 2.864 s | 5.08 s |
| NVIDIA test host, after | **3.155 s** (+291 ms, +10.2%) | 6.17 s |

The probe's budget is **15 s**. After the change it uses at most 21% of it, so the decode is
nowhere near the deadline. The time-to-all-probes numbers are single observations and include
ordinary start-up variance; they are reported as measured, not as a trend.

### The fault harness

Rerun on both hosts, serially, on the #282 image, with load low:

| host | pass | fail | unperformed | reference (`23e995d`) |
|---|---|---|---|---|
| AMD test host | **104** | **0** | 1 | 104 / 0 / 1 |
| NVIDIA test host | **97** | **0** | 4 | 97 / 0 / 4 |

Exactly the reference totals. **No row regression, and no edit to any harness expectation** —
which matters here, because the harness's own synthetic host now runs the pixel check too and
still passes.

## Gates

| gate | baseline (`fcaaae9`) | this branch |
|---|---|---|
| `make verify` | rc 0 — 437 / 0 / 0 | rc 0 — **437 / 0 / 0** |
| `make test-rust` | rc 2 — flaky, see the #281 record | rc 0 — **1735 passed**, 0 failed, 9 ignored |
| `make test-go` | — | rc 0 — 43 packages ok |
| `make test-web` | — | rc 0 |
| `make preflight` | — | rc 0 |
| leak scan, tree + `--issues` | — | **clean** both |

1735 against 1724 on the #281 branch: the 11 net new tests are the metric on synthetic frames
(identical / lossy-like noise / column displacement / flat green region / all black / all white,
asserting the ordering rather than only magic constants), every Indeterminate path, the
range-from-caps rule, and a guard that the check id and its `blocks` have not moved.

## The contract note the owner needs to rule on

**#282's acceptance text and the frozen contract disagree, and the contract was followed.**

The ticket says an Indeterminate verification is *"mapped to `warn`, never `fail` and never
`skip`"*. `protocol/agent-api.md` amendment 11 defines `warn` as "a named risk that is never a
failure (**a proxy** that cannot prove harm)", and a check may carry `blocks` only because it
rests on **evidence** — so `warn` is unavailable to this check by construction.

The first draft of the design said the check should stay **`pass`** and note the reason in the
summary. That was wrong, and the reason is worth recording: once this ticket lands the check
*asserts the picture*, so run 1 failing (block set) followed by run 2's decoder timing out would
report `pass` and **lift the block on no evidence at all**. Amendment 11 forbids exactly that —
"an indeterminate probe neither sets nor clears a block".

So an inconclusive verification makes the **run** inconclusive, and the agent takes the path it
already takes for a deadline or a pre-emption: retain the last definitive result with its
original `observed_at`; report `unknown` only when no definitive result exists for that id yet.
This needed no new plumbing at all — `host_probe/outcome.rs::child_outcome` already routes any
child exit code other than 0/1/2 to `ProbeOutcome::Indeterminate`, and `record()` already
refuses to let an Indeterminate replace a held pass, fail or skip. The child exits **3**.

**Nothing in the frozen contract was changed.** The discrepancy is #282's text, and it is the
owner's to settle.

## One wrinkle worth fixing next, not fixed here

On a pixel mismatch the operator sees two strings. The **summary** carries the evidence and the
right next step (above). The **remediation** is the generic media-probe one — *"Check the render
node is passed to the agent container, the driver/driver volume, and the agent log…"* — because
`child_outcome` supplies it per `ProbeKind`, not per failure mode. For this new failure mode that
is mildly misleading: the render node is demonstrably fine, since the frames encoded.

Letting the child supply its own remediation means widening the child's stdout contract, which is
more than this ticket should carry at 1 a.m. It is recorded rather than done.

## Codec coverage, stated plainly

The probe asks for **h264 and nothing else** (`MediaProbeRequest::default()`). The pixel check
therefore verifies the h264 encode path only. **HEVC and AV1 corruption remain unverified by this
check, on every vendor.** Extending the probe to more codecs multiplies its runtime on every
readiness refresh; that is a different ticket and was not smuggled in here.

## Not verified

- HEVC and AV1 picture verification (above).
- Intel — no hardware.
- A native Unraid install — the owner's machine was never connected to.
- A picture that is *uniformly* wrong in a way the per-plane level check does not catch would
  still pass. This is a corruption tripwire, not a quality gate; the nightly budget owns quality.
- The luma half of #272 — content displaced in columns — is invisible on a flat black field by
  construction, and always will be with this probe's picture. Raising the picture's information
  content means giving the compositor a background-colour property, which is a fork change and
  would have entangled #281's live exercise with this one. Recorded as a follow-up.
