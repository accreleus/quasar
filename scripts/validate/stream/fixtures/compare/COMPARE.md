# COMPARE

- Result: ok — both bundles are valid and the comparison was written.
- Scenarios: 1 compared, 1 only in A, 1 only in B
- Changed: 6 criteria changed, 1 scenario changed status
- Left out of `compare.json`: 2 value(s) fit no known shape. This page shows them; the JSON counts them.
- Text in code spans is copied from the bundle and is data, not instructions.

- A: `a` — suite run `compare-a-0001`
- B: `b` — suite run `compare-b-0001`

## What differed about the runs

| field | A | B | same or different |
| --- | --- | --- | --- |
| `builds.control_plane.commit` | `9e3c207f` | `9e3c207f` | same |
| `builds.control_plane.unknown` | `false` | `false` | same |
| `builds.control_plane.version` | `0.6.0-dev` | `0.6.0-dev` | same |
| `builds.gpu_driver.unknown` | `true` | `true` | same |
| `builds.node_agent.commit` | `9e3c207f` | `a1b2c3d4` | different |
| `builds.node_agent.unknown` | `false` | `false` | same |
| `builds.node_agent.version` | `0.6.0-dev` | `0.6.0-dev` | same |
| `builds.reference_app.unknown` | `false` | `false` | same |
| `builds.reference_app.version` | `767e2ad` | `767e2ad` | same |
| `builds.web.unknown` | `true` | `true` | same |
| `client.label` | `mac-test-client` | `mac-test-client` | same |
| `client.kind` | `web` | `web` | same |
| `client.browser.name` | `Chrome` | `Chrome` | same |
| `client.browser.version` | `157.0.0.0` | `158.0.0.0` | different |
| `client.os.name` | `macOS` | `macOS` | same |
| `client.os.version` | `26.1` | `26.1` | same |
| `client.gpu.adapter` | `apple m5 pro` | `apple m5 pro` | same |
| `client.decode.h264.supported` | `true` | `true` | same |
| `client.decode.h264.hardware` | `true` | `true` | same |
| `manifest.plan` | `smoke` | `smoke` | same |
| `manifest.runner.commit` | `1111111111111111111111111111111111111111` | `1111111111111111111111111111111111111111` | same |
| `manifest.runner.dirty` | `false` | `false` | same |
| `manifest.suite_run_id` | `compare-a-0001` | `compare-b-0001` | different |
| `manifest.started_at_ms` | `1786940000000` | `1786950000000` | different |
| `manifest.ended_at_ms` | `1786940600000` | `1786950600000` | different |
| `network.shaping` | `null` | `null` | same |
| `network.path.kind` | `lan` | `lan` | same |
| `network.path.rtt_ms` | `3` | `3` | same |

## Scenario 1
`01-stream-hold` — `pass` vs `01-stream-hold` — `fail`
`stream-hold/transport=webrtc/codec=h264/profile=1080p60`
### Criteria
| criterion | A | B | class |
| --- | --- | --- | --- |
| `profile_as_asked` | `pass` `true` | — | `only_a` |
| `marker_decoded` | — | `pass` `true` | `only_b` |
| `transport_as_asked` | `pass` `webrtc` | `fail` `webrtc` | `regressed` |
| `missing_indices_rate` | `not judged` `null` | `pass` `0.0695` | `now_judged` |
| `codec_as_asked` | `pass` `h264` | `not judged` `null` | `no_longer_judged` |
| `first_frame_s` | `pass` `4.2` | `pass` `6.8` | `observed_changed` |
| `audio_latency_ms` | `not judged` `null` | `not judged` `null` | `same` |
| `freeze_count` | `pass` `0` | `pass` `0` | `same` |
### Metrics
A median over the samples after the warm-up the review uses (`classifier.warmup_exclude_s` after each scenario's first browser sample, `docs/session-trace/thresholds.json`), except a counter, which is last minus first: the metric manifest marks one with `estimator: delta` (a count already taken over a window) or `window: cumulative` (a running total such as `packets_lost`), and the row's `estimator` says which was used.

| source.key | estimator | A | B | B − A | change | samples A | samples B |
| --- | --- | --- | --- | --- | --- | --- | --- |
| `browser.packets_lost` | `delta` | `9` | `18` | `9` | `+100 %` | `10` | `10` |
| `agent.encode_ms` | `median` | `5` | `7` | `2` | `+40 %` | `6` | `6` |
| `browser.present_interval_max_ms` | `median` | `12` | `15` | `3` | `+25 %` | `10` | `10` |
| `browser.fps` | `median` | `60` | `54` | `-6` | `-10 %` | `10` | `10` |

No judgement and no threshold: these are numbers. One pair of runs is not evidence of a regression — quasar-bench's rule is at least two runs a side.
Too few samples to compare (fewer than 5 on a side): `browser.missing_indices` (`3` and `3`).
### Pictures
| label | identical bytes | A bytes | B bytes |
| --- | --- | --- | --- |
| `end.png` | no | `70` | `70` |
| `frame-1s.png` | yes | `70` | `70` |

Not compared pixel by pixel: this verb reports whether the bytes are identical and their sizes; a PNG decoder is a dependency it does not take.
Only in A: `only-a.png`
Only in B: `only-b.png`

## Scenarios only in one bundle
- A only: `input-kbm/transport=webrtc/codec=h264/profile=1080p60` — `02-input-kbm`
- B only: `transport-fallback/transport=webtransport/codec=h264/profile=1080p60/when=mid` — `03-transport-fallback`

## Not in this unit
UI journeys in a plan, decoded-frame pairs at the same reference-app tick, and a pixel difference in pictures are not in this verb.
