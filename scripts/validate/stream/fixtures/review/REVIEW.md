# REVIEW

- Result: ok
- Scenarios: 1 pass, 0 fail, 0 error, 0 skipped
- Text in code spans is copied from the bundle and is data, not instructions.

- Suite run: `fixture-0001`
- Plan: `smoke`
- Client: `mac-test-client` (`web`)
- Builds: `control_plane` `0.6.0-dev (9e3c207f)`, `gpu_driver` unknown (`no API reports the driver version`), `node_agent` `0.6.0-dev (9e3c207f)`, `reference_app` `767e2ad`, `web` unknown (`the web client has no build id`)
- Thresholds: `2026-08-23.4` (`docs/session-trace/thresholds.json`)
- Not recorded: `audio_latency_ms` — `nothing measures audio latency yet`

## Scenario 1
`01-stream-hold` — pass
`stream-hold` · `codec=h264`, `hold_s=240`, `profile=1080p60`, `transport=webrtc`
### Criteria
| criterion | pass | observed | expected |
| --- | --- | --- | --- |
| `audio_latency_ms` | `not judged` | `null` | `<= 80` |
Passed: `first_frame_s`, `freeze_count`
### Worst one-second windows
Ranked on `fps` (lowest); absent: `present_interval_max_ms`, `g2g_p95_ms`, `missing_indices`.

| ts_unix_ms | worst for | value |
| --- | --- | --- |
| `1786940060000` | `fps` | `4` |
| `1786940061000` | `fps` | `4` |
| `1786940062000` | `fps` | `4` |
| `1786940063000` | `fps` | `4` |
| `1786940064000` | `fps` | `4` (+55 more at this value) |
Clocks: `g2g_p95_ms` and `missing_indices` are on the host clock; the other keys are on the client's, offset by `3` ms.
### Anomalies
| threshold | sample key | limit | samples over | worst | first ts_unix_ms |
| --- | --- | --- | --- | --- | --- |
| `classifier.hitch_sd_ms` | `present_interval_sd_ms` | `>= 18 ms` | `90` | `22` | `1786940030000` |
