# RH-01 promotion to develop

The owner approved promotion of #240 candidate
`f9ecc701f19820744ad058fdb3cc2af05e642304` from
`initiative/resilient-host-architecture` into `develop` after manual Steam testing.
The [integrated acceptance report](2026-09-17-rh01-240-integrated-acceptance.md)
remains the record of automated AMD/NVIDIA validation and exact deployed images.

## Integration review

At promotion preparation, remote `develop` was `5998fc5`, an ancestor of the
approved candidate, with zero divergent commits and 24 incoming commits. The
initiative branch still pointed at the exact approved candidate. No conflict
resolution or production-code adjustment was required. Promotion is a
fast-forward, with this documentation-only addendum after the accepted candidate.
The runtime, control-plane, deployment and protocol trees remain exactly those
accepted under #240. No additional GPU acceptance is required for an unchanged
production tree; existing live sessions must not be interrupted for this promotion.

The exact resulting commit is validated before pushing. The promotion completion
record on #240 names that commit and its check results, avoiding a self-referential
commit ID in this document. The original worktree and its unrelated uncommitted
change are preserved by preparing the promotion in an isolated checkout.

## Owner's manual Steam validation

After automated acceptance, the owner registered on the existing gpu-test stack,
launched Steam and reported actively playing **Redout** through the browser. This
establishes a real Steam/game launch and user-observed gameplay in addition to the
controlled application fixtures used for #240. It does not certify every Steam
game or Proton configuration.

A read-only five-minute telemetry observation during gameplay recorded:

| Measurement | Observation |
| --- | --- |
| Profile / actual encoder | 1440p120 / Vulkan AV1 `vulkanav1enc` |
| Encoder / browser decode rate | Approximately 120 fps |
| Median encode time | 2.30 ms |
| Encoder per-sample p95 time, median / p95 across window | 2.52 / 2.68 ms |
| Network RTT, median / p95 | 3 / 5 ms |
| Encoder bitrate, median / target | 8.97 / 10 Mbps |
| Reported packet loss, dropped frames and freezes | Zero in the observed window |
| Source frame rate | Approximately 60 fps |
| Browser presentation | Occasional stalls; maximum interval 76.2 ms |

The diagnostic classifier reported nominal operation, but its long-frame
condition was unsatisfied. The presentation outlier is retained rather than
describing the stream as perfectly smooth. No session warnings/errors appeared
in the bounded captured agent log.

The owner confirmed that Redout was capped at 60 fps and subsequently reported
changing its cap to 120 fps and testing again. No fresh measured window after
that change is included here: 120 encoded/decoded frames per second is not proof
of 120 unique game frames. Telemetry was collected without modifying session
settings, injecting input or restarting services.

## Scope of approval

This approval covers promotion to **develop only**. It does not authorize a main
merge, release tag, image publication, Actions image build, deployment change or
another issue. Existing test deployments and active sessions remain in place.

Publishing an edge build is a separate decision. Before claiming the public
upgrade path verified for this candidate, rehearse the current public release's
existing updater applying the published candidate images on a disposable stack.
#240's deployment/rollback evidence does not substitute for that release-specific
updater rehearsal. Preserve the schema-84 compatibility restriction documented in
the [runtime recovery guide](../runtime-api-recovery.md).
