# REVIEW — fixture-0001

- Plan: `smoke`
- Client: mac-test-client (web)
- Builds: control_plane 0.6.0-dev (9e3c207f), gpu_driver unknown (no API reports the driver version), node_agent 0.6.0-dev (9e3c207f), reference_app 767e2ad, web unknown (the web client has no build id)
- Scenarios: 1 pass, 0 fail, 0 error, 0 skipped
- Thresholds: `2026-08-23.4` (`docs/session-trace/thresholds.json`)
- Not recorded: `audio_latency_ms` — nothing measures audio latency yet

## 01-stream-hold — pass

`stream-hold` · transport=webrtc, codec=h264, profile=1080p60, hold_s=240

### Criteria

| criterion | pass | observed | expected |
| --- | --- | --- | --- |
| audio_latency_ms | not judged | null | <= 80 |
| first_frame_s | pass | 4.2 | <= 20 |
| freeze_count | pass | 0 | 0 |

### Worst one-second windows

Ranked on `fps` (lowest); absent: `present_interval_max_ms`, `g2g_p95_ms`, `missing_indices`.

| ts_unix_ms | worst for | value |
| --- | --- | --- |
| 1786940060000 | fps | 4 |
| 1786940061000 | fps | 4 |
| 1786940062000 | fps | 4 |
| 1786940063000 | fps | 4 |
| 1786940064000 | fps | 4 |

### Anomalies

| threshold | sample key | limit | windows over | worst | first ts_unix_ms |
| --- | --- | --- | --- | --- | --- |
| classifier.hitch_sd_ms | present_interval_sd_ms | >= 18 ms | 90 | 22 | 1786940030000 |
| web.signal.packets_lost_fair | packets_lost | >= 3 count | 178 | 358 | 1786940002000 |
| web.signal.packets_lost_poor | packets_lost | >= 15 count | 172 | 358 | 1786940008000 |
