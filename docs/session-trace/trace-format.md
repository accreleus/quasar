# Session-trace format v1 (internal, client-agnostic)

**Status:** ST-01 (Observability v2 — Unified Session Tracer & Tuning). Internal
design doc, **not** a frozen `protocol/` wire contract. Kept in `docs/` deliberately
(see `ST-00-overview.md` design stance) so it does not require the quasar-protocol
submodule round-trip; if the Phase 9 native client later needs to emit traces on the
wire, this shape is ready to re-home to `protocol/` then (Opus + sign-off at that
point). Designing it client-agnostic now is what makes browser/native diagnosis parity
free later.

> This format is a **normalization-at-read view** over telemetry that already flows. It
> introduces **one** new write path (discrete events) and **one** new per-session record
> (clock metadata). It does **not** introduce a new samples table, a new sampling cadence,
> or new instrumentation on the hot path. The diagnostic bundle (`ST-06`,
> `contract-amendment.md`) is assembled directly from the existing `session_metrics` JSONB
> joined with the new events table — no double-write.

---

## 1. Trace lifecycle & identity

- **One trace per session. Identity = `session_id`.** There is **no separate `trace_id`**.
  A "trace" is not a new entity with its own lifecycle; it is the read-time union of
  everything already keyed by `session_id`. Reusing the session row (and its
  `ON DELETE CASCADE` reach) is what keeps retention, the trust boundary, and the admin
  read surface identical to the existing telemetry spine.
- **A trace comprises three things, all keyed by `session_id`:**
  1. **Periodic samples** — the existing `session_metrics` rows (`source ∈ {agent, browser,
     native}`), unchanged. These are the time-series.
  2. **Discrete events** — rows in the new `session_trace_events` table (`source ∈ {agent,
     browser}`). These are the markers: "what happened, when".
  3. **Clock metadata** — one optional record per session (the new `session_trace_clock`)
     carrying `client_offset_ms` + `uncertainty_ms` so the browser-clock series can be
     aligned against the host-clock series **honestly** (with uncertainty, or labelled
     unmeasured).
- **Trace start/end** are the session's own `started_at` / `ended_at` (from the `sessions`
  row). A trace exists for as long as the session row and its bounded telemetry window
  exist; it is reaped with the session (cascade) or pruned by the retention policy (§6).
- **No global trace registry, no cross-session join.** Every query is single-session,
  short-window, recent-data — the read pattern the existing `(session_id, ts_unix_ms DESC)`
  index already serves.

---

## 2. Metric taxonomy v1 (normalize-at-read)

The taxonomy is a **namespaced, read-time renaming** of keys that already live in the
`session_metrics.metrics` JSONB. The bundle assembler maps a raw JSONB key + its `source` to a
namespaced taxonomy name; it does **not** write new rows or re-instrument anything. A raw key with
no taxonomy mapping is still readable through the raw `session_metrics` surface — the taxonomy is
a curated diagnostic view, not an exhaustive one.

Four namespaces: `client.*` (browser/native presentation + receive), `transport.*`
(WebRTC wire + congestion), `encoder.*` (host encode), `abr.*` (the in-session governor).

### The table below is generated

**The source is the *metric manifest*, `docs/session-trace/metrics.json`** — the one dictionary of
every metric key on either wire. `taxonomyV1` in `control-plane/internal/session/classifier.go`,
the browser ingest allow-list in `internal/telemetry/filter.go`, the diagnostics-panel labels in
`web/src/lib/metricsManifest.ts` and this table are **all derived from it**. Edit the manifest and
run `make docs-trace`; a drift test fails if any consumer goes stale.

Why the extra columns exist: a key suffix carries the unit and nothing else, and the unit is the
least interesting of the four things a number needs before it can be read. `glass_to_glass_ms` was
a **median over a never-drained ring of up to 600 samples** sitting in the same panel as
`decode_ms`, a **one-second mean**, with nothing on either saying so. Both false alarms this
document records (§8, §9) were estimator confusion, one level apart.

- **clock** — which time base the value sits on. `host_monotonic` (a Rust `Instant` on the host),
  `host_wall` (the host's epoch clock — what agent sample stamps are), `gst_pts` (the GStreamer
  pipeline clock; **nominal**, it advances at the negotiated rate whatever the wall clock does),
  `rtp`, `client_performance` (the page's high-resolution clock, no epoch), `client_wall`
  (`Date.now()` in the browser — the one that needs §4's offset), and `none` for a quantity with
  no time base at all (a count, a flag, a setting). `none` never means "unknown".
- **window** — what span the value summarises. `heartbeat(~5s)` is the agent's drain cadence, and
  it equals the emit cadence so windows are contiguous. `1s` is the browser's poll. `cumulative`
  means the posted value is a **running total** and every rule must delta it — a single sample
  cannot, and reports `no samples` rather than 0. `rolling_600` is a ring of up to 600 samples
  that is **never drained**: minutes of history, drifting slowly, sitting beside 1 s numbers.
  `snapshot` is a level read at drain time, `event` is emitted once.
- **estimator** — the reduction. Part of the claim, not decoration: "present σ was 19 ms" means
  different things as a `mean`, a `p95` and a `max`.
- **n** — the key carrying this one's sample count. A number over 4 samples and one over 400 are
  not the same evidence, and the manifest names where to find the divisor.

<!-- metrics:begin -->
<!-- GENERATED from docs/session-trace/metrics.json by scripts/dx/gen_trace_docs.py.
     Do not edit between these markers; edit the manifest and run `make docs-trace`. -->

*Manifest version `2026-08-24.1` — 46 curated series of 124 documented keys.*

| taxonomy name | source | raw key | unit | clock | window | estimator | n | meaning / the trap it avoids |
|---|---|---|---|---|---|---|---|---|
| `abr.setpoint_kbps` | agent | `abr_setpoint_kbps` | kbps | `none` | `heartbeat(~5s)` | `last` | — | The in-session ABR governor's CURRENT CBR setpoint at drain time — a level, not a window average. Absent when ABR is disarmed. |
| `compositor.fps` | agent | `compositor_fps` | fps | `host_monotonic` | `heartbeat(~5s)` | `mean` | — | Frames emitted by waylanddisplaysrc per second, BEFORE caps normalisation and interpipe. The first stage where a rate can differ from what the app painted. |
| `compositor.pts_delta_p50_ms` | agent | `compositor_pts_delta_p50_ms` | ms | `gst_pts` | `heartbeat(~5s)` | `p50` | — | p50 spacing of consecutive compositor buffer PTS. NOMINAL, NOT REALIZED: PTS come off the shared pipeline clock at the negotiated rate, so this sits at a flat ~16.668 ms at 60 fps however badly the wall-clock cadence jitters. For realized emit spacing read probe_compositor_frame_interval_p95_ms. |
| `compositor.pts_delta_p95_ms` | agent | `compositor_pts_delta_p95_ms` | ms | `gst_pts` | `heartbeat(~5s)` | `p95` | — | p95 of the same NOMINAL PTS spacing. Same trap as the p50: a healthy-looking 16.7 here is not evidence the compositor emitted on time. |
| `encoder.bitrate_kbps` | agent | `bitrate_kbps` | kbps | `host_monotonic` | `heartbeat(~5s)` | `mean` | — | Encoder output bytes in the window as kbit/s. This is what the encoder produced, not what ABR asked for (abr_setpoint_kbps) and not what left the box (rtp_bitrate_kbps). |
| `encoder.encode_ms` | agent | `encode_ms` | ms | `host_monotonic` | `heartbeat(~5s)` | `mean` | — | MEAN per-frame encode time over the window, from the encode pad-probe FIFO (sink instant paired with the next src buffer). A mean hides the tail that matters; read encode_ms_p95 beside it. |
| `encoder.encode_ms_max` | agent | `encode_ms_max` | ms | `host_monotonic` | `heartbeat(~5s)` | `max` | — | The single worst per-frame encode in the window — the only agent key that can see a one-frame stall. Over a 5 s window at 60 fps one bad frame is the 99.7th percentile, so a 200 ms hiccup moves encode_ms_p95 by well under a millisecond and moves this by two hundred. Computed since P4-03 and discarded at the drain until C10 published it. Omitted when the window encoded nothing. |
| `encoder.encode_ms_p50` | agent | `encode_ms_p50` | ms | `host_monotonic` | `heartbeat(~5s)` | `p50` | — | Median per-frame encode time in the window. Omitted entirely when no frame was encoded — absent means not measured, never 0. |
| `encoder.encode_ms_p95` | agent | `encode_ms_p95` | ms | `host_monotonic` | `heartbeat(~5s)` | `p95` | — | p95 per-frame encode time — the encoder-headroom reading the verdict's encoder_saturation rule uses (classifier.encoder_ceiling_ms). Omitted when the window encoded nothing. |
| `encoder.fps` | agent | `fps` | fps | `host_monotonic` | `heartbeat(~5s)` | `mean` | — | Frames leaving the encoder in the window divided by the window — realized encode rate, not a requested one. Compare against source_fps/compositor_fps to see where a rate was lost. |
| `encoder.frames_dropped` | agent | `frames_dropped` | count | `none` | `heartbeat(~5s)` | `sum` | — | ENCODER-side drops in the window, detected by a pending-FIFO timeout (500 ms) plus FIFO-overflow resyncs, because no HW/SW H.264 encoder exposes a dropped-frame property. Same key name as the browser's presentation drops and a completely different quantity — the taxonomy keeps them in separate namespaces for exactly that reason. |
| `encoder.frames_encoded` | agent | `frames_encoded` | count | `none` | `heartbeat(~5s)` | `sum` | — | Frames the encoder emitted in the window. A per-window count, not a cumulative counter — do not delta it. |
| `interpipe.queue_drops` | agent | `interpipe_queue_drops` | count | `none` | `heartbeat(~5s)` | `sum` | — | Leaky-queue overruns in the window. Host-side loss BEFORE the encoder; distinct from frames_dropped (encoder) and from the client's frames_dropped (presentation). |
| `interpipe.queue_dwell_p50_ms` | agent | `interpipe_queue_dwell_p50_ms` | ms | `host_monotonic` | `heartbeat(~5s)` | `p50` | — | p50 time a buffer spent between the interpipe sink and src. Omitted when nothing traversed. |
| `interpipe.queue_dwell_p95_ms` | agent | `interpipe_queue_dwell_p95_ms` | ms | `host_monotonic` | `heartbeat(~5s)` | `p95` | — | p95 interpipe dwell — where a queue that is filling shows up before drops do. |
| `interpipe.queue_level_max` | agent | `interpipe_queue_level_max` | count | `none` | `heartbeat(~5s)` | `max` | — | Deepest the interpipe queue got, in buffers, since the previous drain reset it. A max, so one burst sets it for the whole window — which is the point. |
| `rtp.bitrate_kbps` | agent | `rtp_bitrate_kbps` | kbps | `host_monotonic` | `heartbeat(~5s)` | `mean` | — | RTP packet bitrate before WebRTC transport overhead (SRTP, RTX, FEC). Always a little above bitrate_kbps; a large gap is retransmission. |
| `rtp.fps` | agent | `rtp_fps` | fps | `host_monotonic` | `heartbeat(~5s)` | `mean` | — | RTP access units completed per second, counted on marker packets — the last host-side rate before the wire. |
| `source.fps` | agent | `source_fps` | fps | `host_monotonic` | `heartbeat(~5s)` | `mean` | — | New buffers committed by the MAPPED FULLSCREEN app top-level per second. Cursor, popup, subsurface and configure-only commits are excluded, so this is the app's own paint rate, not Wayland traffic. |
| `transport.gcc_estimate_kbps` | agent | `gcc_estimate_kbps` | kbps | `none` | `heartbeat(~5s)` | `last` | — | The raw rtpgccbwe bandwidth estimate BEFORE the governor's EWMA / deadband / step logic. The delta against abr_setpoint_kbps is the governor's own smoothing contribution. |
| `client.abs_capture_time_negotiated` | browser/native | `abs_capture_time_negotiated` | bool | `none` | `snapshot` | `last` | — | Receiver-parameter proof that the abs-capture-time RTP header extension was negotiated (0/1). Proven live on Tower 2026-08-05 to be 1 on hosts where captureTime is still absent — which is why both keys exist. |
| `client.bitrate_kbps` | browser/native | `bitrate_kbps` | kbps | `client_wall` | `1s` | `mean` | — | Receiver inbound bitrate, delta of bytesReceived over the poll interval (integer kbps). Computed since UI-09 but only posted since 2026-08-23: the manifest drift audit found the stored client.bitrate_kbps series empty for every earlier session. |
| `client.decode_display_ms` | browser/native | `decode_display_ms` | ms | `client_wall` | `rolling_600` | `median` | — | The residual glass_to_glass_ms - network_pacing_ms - jitter_buffer_ms. CAN BE NEGATIVE, DELIBERATELY: a rolling-600 median minus two 1 s numbers is not an identity, and the disagreement is the diagnosis. It used to be clamped to a confident 0.0. |
| `client.decode_ms` | browser/native | `decode_ms` | ms | `client_wall` | `1s` | `mean` | — | MEAN per-frame decode time this 1 s window, from the totalDecodeTime / framesDecoded counter delta. A 1 s mean sitting beside a rolling-600 g2g median in the same panel is exactly the confusion this manifest exists to stop. |
| `client.display_refresh_hz` | browser/native | `display_refresh_hz` | fps | `client_performance` | `snapshot` | `median` | — | The client monitor's refresh rate, measured from rAF vsync spacing with a MEDIAN (AS10-14) and re-measured when the tab becomes visible. Not RVFC-derived. |
| `client.fps` | browser/native | `fps` | fps | `client_wall` | `1s` | `mean` | — | Receiver-decoded fps from getStats framesPerSecond — frames the decoder produced, which is NOT the rate the viewer saw (that is present_fps_median). |
| `client.frames_dropped` | browser/native | `frames_dropped` | count | `none` | `cumulative` | `raw` | — | CUMULATIVE receiver-side presentation drops. Same key name as the agent's encoder drops and a different quantity; a hidden tab inflates it with frames it never tried to present, which is why is_hidden is read first. |
| `client.freeze_count` | browser/native | `freeze_count` | count | `none` | `cumulative` | `raw` | — | CUMULATIVE getStats freezeCount. Its advances drive the client.freeze_detected event; the freeze LENGTH is present_interval_max_ms, never 1000/freezeCount. |
| `client.glass_to_glass_ms` | browser/native | `glass_to_glass_ms` | ms | `client_wall` | `rolling_600` | `median` | — | **DEPRECATED — read `rvfc_capture_to_display_ms`.** MEDIAN over a ring of up to 600 RVFC samples that is never drained — so it is minutes of history, not this second, and it drifts slowly rather than reacting. It is also NOT true glass-to-glass: the measurement is RTP capture-time to browser present, which excludes app render and client display scan-out. Superseded by rvfc_capture_to_display_ms, which says what it measures; both are posted this release and the series is unchanged. |
| `client.is_hidden` | browser/native | `is_hidden` | bool | `none` | `snapshot` | `last` | — | Tab/window hidden at sample time (0/1). The #1 presentation false-positive guard: an occluded tab 'drops' frames it never tried to present. |
| `client.playout_target_ms` | browser/native | `playout_target_ms` | ms | `none` | `snapshot` | `last` | — | The playout/jitter-buffer target this sample was measured UNDER, so a stored sigma is self-describing. A setting, not a measurement. |
| `client.present_beat_fraction` | browser/native | `present_beat_fraction` | fraction | `client_performance` | `1s` | `count` | `present_n` | Share of intervals within +/-20% of exactly 2x the median — the inherent vsync beat of two free-running clocks, not stutter. Read it WITH present_long_frames: beat > 0 with long frames 0 is benign; long frames > 0 is a stall the beat does not excuse. |
| `client.present_fps` | browser/native | `present_fps` | fps | `client_performance` | `1s` | `mean` | `present_n` | **DEPRECATED — read `present_fps_median`.** fps from the MEAN RVFC interval. DEPRECATED AS A HEADLINE: at source fps == display Hz one missed vsync doubles one interval and drags the mean — a healthy 1440p120 session read 88-108 on 2026-08-22 and cost a day of encoder investigation. Kept unchanged so the stored series stays comparable. |
| `client.present_fps_median` | browser/native | `present_fps_median` | fps | `client_performance` | `1s` | `median` | `present_n` | THE READING. fps from the MEDIAN RVFC interval — the fps a viewer perceives, unmoved by an occasional doubled frame. Its input intervals are display-refresh independent since #263 (2026-08-24): RVFC ticks are deduped by metadata.mediaTime before summarising, so a display whose refresh rate is not an integer multiple of the source fps (e.g. 98 Hz) no longer pollutes the samples with vsync ticks that carry no new frame — a healthy 60 fps stream on such a display previously read ~43-49. |
| `client.present_interval_max_ms` | browser/native | `present_interval_max_ms` | ms | `client_performance` | `1s` | `max` | `present_n` | The longest interval in the window: the real stall length, and the source of client.freeze_detected's gap_ms. |
| `client.present_interval_median_ms` | browser/native | `present_interval_median_ms` | ms | `client_performance` | `1s` | `median` | `present_n` | The median frame-to-frame presentation interval itself. Every other cadence key is defined relative to it (doubled band, long-frame factor). |
| `client.present_interval_p95_ms` | browser/native | `present_interval_p95_ms` | ms | `client_performance` | `1s` | `p95` | `present_n` | p95 presentation interval — a sustained long-frame tail sigma can stay modest through. |
| `client.present_interval_sd_ms` | browser/native | `present_interval_sd_ms` | ms | `client_performance` | `1s` | `mean` | `present_n` | Standard deviation of this window's presentation intervals — the headline judder metric (#108). A hitch is a property of the WINDOW: two samples must reach classifier.hitch_sd_ms, because as a max one 18.058 ms sample out of 54 flipped a healthy session on 2026-08-23. |
| `client.present_long_frames` | browser/native | `present_long_frames` | count | `client_performance` | `1s` | `count` | `present_n` | Intervals above 2.5x the median — above the top of the doubled band, so genuine stalls. Zero is what makes a beat benign. |
| `client.present_n` | browser/native | `present_n` | count | `none` | `1s` | `count` | — | How many intervals the window actually held. Below 5 every other present_* key is OMITTED rather than computed from a fragment — this is the key that tells 'quiet' from 'unmeasured'. |
| `client.rvfc_capture_time_available` | browser/native | `rvfc_capture_time_available` | bool | `none` | `snapshot` | `last` | — | This browser is currently surfacing a valid RVFC captureTime (0/1). NOT proof the RTP extension was negotiated — that is abs_capture_time_negotiated, and the two differ on Chrome. |
| `client.rvfc_capture_to_display_ms` | browser/native | `rvfc_capture_to_display_ms` | ms | `client_wall` | `rolling_600` | `median` | — | The same number as glass_to_glass_ms under a name that does not overclaim: RTP capture-time to browser present, median over a rolling ring of up to 600 RVFC samples. Posted alongside the old key this release so no stored series breaks. |
| `transport.jitter_buffer_ms` | browser/native | `jitter_buffer_ms` | ms | `client_wall` | `1s` | `mean` | — | Receiver jitter-buffer depth for this window. The buffer the playout controller is steering, not the target it is steering toward. |
| `transport.network_pacing_ms` | browser/native | `network_pacing_ms` | ms | `client_wall` | `1s` | `last` | — | rtt_ms / 2 — a one-way estimate, not a measured pacing delay, and from THIS 1 s window rather than the rolling ring the g2g median comes from. |
| `transport.packets_lost` | browser/native | `packets_lost` | count | `none` | `cumulative` | `raw` | — | CUMULATIVE receiver packet loss, posted raw. Every rule that uses it takes a delta across the window (falsifier estimator 'delta'); a single sample cannot produce one and reports 'no samples' rather than 0. |
| `transport.rtt_ms` | browser/native | `rtt_ms` | ms | `client_wall` | `1s` | `last` | — | candidate-pair currentRoundTripTime at poll time. Half of it is what network_pacing_ms uses as the one-way estimate. |

<!-- metrics:end -->

> **`client.interactive_ms` removed from this table (IL-0 follow-up cleanup).** It was
> documented as a live ping-as-input-marker interactive-latency metric but never emitted
> by `buildMetrics()` in `web/src/webrtc/telemetry.ts` — a dead key, not a live metric.
> Removed rather than backfilled with a synthesized value (see
> `docs/research/input-latency-analysis.md` for the IL-0 finding, and §4 below for the
> parallel removal from the clock-fields note). A future re-derivation is new work
> against this doc, not a resurrection of this row.

> **`frames_dropped` is source-scoped, never blind-overlaid.** It exists under **both**
> `encoder.*` (encoder-side) and `client.*` (receiver-side) with **different meaning**,
> disambiguated by `source` exactly as `schema.md` requires. The taxonomy keeps them in
> separate namespaces so a bundle reader can never confuse one for the other.

> **`native` source.** Per `schema.md` (P9-01) the native client emits the **same
> receiver-side key set** as the browser. Every `client.*`/`transport.*` mapping above
> applies identically to `source='native'`. The taxonomy is the seam that makes
> browser/native diagnosis parity free.

---

## 3. Event taxonomy v1

Events are discrete markers — the things that make before/after reasoning possible — stored
in `session_trace_events` (DDL in `contract-amendment.md`). Each event row is `{ session_id,
source, ts_unix_ms, type, payload }`. `ts_unix_ms` is the **reporter's** wall-clock at the
event (same convention as `session_metrics.ts_unix_ms` and `agent-api.md heartbeat.ts_unix_ms`).

### 3.1 Naming rules

- **`namespace.verb_or_noun`**, lower snake-case, dot-separated. The leading namespace
  matches the metric taxonomy (`abr`, `pipeline`, `encoder`, `webrtc`, `playout`, `client`).
- A `type` is a **closed v1 allow-list** (below). The browser ingest path **drops unknown
  types, does not store them** — mirroring `FilterBrowserMetrics`'s key filtering for
  metrics (`metrics_store.go`). Agent events are from a trusted host-side reporter but are
  still validated for host ownership (§7).
- `payload` is a small JSONB object; keys are descriptive and unit-suffixed
  (`_ms`, `_kbps`). Absent optional payload keys are simply omitted.
- An unrecognized **future** type added by a newer reporter is forward-compatible: an older
  reader ignores types it does not know (the same posture as an unknown WS message type).

### 3.2 Agent-source events (`source='agent'`)

### The table below is generated

**The source is the event section of the same manifest, `docs/session-trace/metrics.json`**
(`events[]`, `source: "agent"`). Edit the manifest and run `make docs-trace`; the hand-written
table that used to live here drifted from the emitters, and the two facts a reader most needs —
what is in the payload, and whether a missing row means anything — were only in prose.

<!-- events:begin -->
<!-- GENERATED from docs/session-trace/metrics.json by scripts/dx/gen_trace_docs.py.
     Do not edit between these markers; edit the manifest and run `make docs-trace`. -->

*18 agent-source event types. `lane` is load-bearing: a **reliable** row is ordered, never coalesced and never dropped, so its absence is a defect; a **diagnostic** row rides the bounded droppable lane, so its absence is not evidence the event did not happen.*

| type | lane | payload keys | clock | meaning / the trap it avoids |
|---|---|---|---|---|
| `caps.negotiated` | reliable | `trigger`, `encoder_factory`, `codec`, `profile`, `level`, `encoder_sink_caps`, `encoder_src_caps`, `payloader_src_caps`, `size` | `host_wall` | What the encode branch agreed, re-stated on EVERY (re)negotiation: launch, rung_step, scale_rebuild, encoder_restart, source_swap. Exists because effective_media stops being true after the first scale-stage rebuild, and on Vulkan every rung step is an encoder restart. `profile` is the live value — a 2026-08-22 gst-launch probe negotiating main-444 read as a driver regression while production pinned main all along. |
| `diag.burst_stats` | reliable | `capture_id`, `kind`, `encoding`, `content_type`, `json`, `bytes`, `duration_ms`, `error` | `host_wall` | As diag.pipeline_dot: bitstream burst statistics for one armed capture. |
| `diag.encoder_props` | reliable | `capture_id`, `kind`, `encoding`, `content_type`, `json`, `bytes`, `duration_ms`, `error` | `host_wall` | As diag.pipeline_dot: live encoder property + scale-stage readback for one armed capture. |
| `diag.pipeline_dot` | reliable | `capture_id`, `kind`, `encoding`, `content_type`, `data`, `json`, `bytes`, `compressed_bytes`, `original_bytes`, `truncated`, `duration_ms`, `error` | `host_wall` | The single result of an admin-armed session_capture. Persisted synchronously and EXEMPT from the rolling prune: this row is the answer to a question a human asked thirty seconds ago and is polling for by capture_id. |
| `encoder.stall` | reliable | `phase`, `reason`, `since_ms`, `stalled_ms` | `host_wall` | Encoder output silence at or above agent.encoder_stall_ms while input keeps arriving, and its recovery. The reason is the point: no_output (the encoder), input_starved (upstream), negotiation (the graph cannot agree caps). Before this there was no stall event, metric or counter at all — interpipe_queue_level_max was the nearest signal and it is a queue depth. |
| `host.gpu_fault` | reliable | `vendor`, `text`, `ts_unix_ms` | `host_wall` | The non-NVIDIA half of host.xid: an amdgpu ring timeout, GPU reset or GPU fault. Text-only, because unlike an Xid there is no small stable code to key on. Same host-not-session emission rule. |
| `host.xid` | reliable | `code`, `pci`, `pid`, `name`, `text`, `ts_unix_ms` | `host_wall` | An NVIDIA Xid the kernel recorded, tailed off /dev/kmsg. Emitted to EVERY running session because the kernel does not know whose work faulted — two sessions reporting the same Xid are one fault seen twice. Corroborates vulkan_fault's failure-string inference; it does not replace it. Present by default: the shipped compose passes /dev/kmsg read-only and grants CAP_SYSLOG. Absent on a host whose compose predates that, dropped the entry, or runs a kernel without /dev/kmsg (see the xid_visibility readiness check), which is NOT evidence that no fault occurred. |
| `sdp.answer_applied` | reliable | `pc`, `m_lines`, `rejected_count`, `ice_ufrag_changed` | `host_wall` | The peer's answer APPLIED, and what it accepted. rejected_count > 0 means an m-line came back at port 0 or inactive, so that media will never flow even though nothing failed — the counterpart of webrtc.remote_description_failed, which only fires when webrtcbin REFUSES the answer. Read this before reading anything about the encoder: a headless Chrome peer always rejects h265, and that shape was misread as a ring stall for hours. |
| `session.effective_media` | reliable | `configured`, `resolved`, `actual`, `app_display`, `audio`, `mic` | `host_wall` | The one-shot, first-offer snapshot of what the session actually built: element readbacks and negotiated caps, kept separate from what was requested. ONE-SHOT BY CONTRACT — it is never re-emitted, which is why caps.negotiated exists. |
| `abr.retarget` | diagnostic | `from_kbps`, `to_kbps`, `reason` | `host_wall` | The in-session ABR governor moved its CBR setpoint. Pairs with the abr.* sample series: the samples say where the setpoint sat, this says when and why it moved. |
| `app.exited` | diagnostic | `status`, `policy` | `host_wall` | The session's app container exited, with the on_app_exit policy that decided what happened next. A Keep policy means the session continues with an empty compositor. |
| `app.launch_state` | diagnostic | `state`, `appid` | `host_wall` | Declared by agent-api.md as the event-driven counterpart of the app_launch_state sample key. Contract-defined; no node-agent emitter today. |
| `audio.degraded` | diagnostic | `reason`, `required` | `host_wall` | The per-session PulseAudio sidecar was wanted and unavailable, so the session is MUTE. Emitted at the first offer, not at detection: the control plane drops an event for a session that is not yet running, and the sidecar fails during resource prepare. |
| `encoder.drop_detected` | diagnostic | `frames_dropped`, `window_ms` | `host_wall` | A non-trivial frame-drop burst in one window, from the 500 ms per-frame drop scan. Deliberately independent of encoder.stall: this counts frames that produced no output, that reports the encoder having stopped. |
| `pipeline.source_swapped` | diagnostic | `from_app`, `to_app` | `host_wall` | A launcher-to-game source-pipeline swap completed. The encode pipeline and webrtcbin are NOT touched (P2-07), so this is a source-generation change, not a renegotiation — caps.negotiated{trigger:source_swap} follows it and reports the caps. |
| `transport.peer_disconnected` | diagnostic | `error`, `debug` | `host_wall` | The DataChannel's SCTP association died while nobody asked us to stop. The session ends on the CLEAN teardown path with a state_detail and no error_message — the peer went away, the agent did not fault. |
| `webrtc.remote_description_failed` | diagnostic | `pc`, `reason`, `kind`, `benign` | `host_wall` | A remote description webrtcbin REFUSED (#503). kind=rejected means that PeerConnection will never carry media and the session fails immediately after; kind=duplicate_answer is normal on the ICE-restart path and is recorded only. |
| `webrtc.state_changed` | diagnostic | `kind`, `from`, `to` | `host_wall` | An ICE/connection transition on the host webrtcbin. Connected and Completed BOTH mean established — ICE may jump Checking to Completed, and reading 'never reached connected' as a failure is a known misread. |

<!-- events:end -->

Browser-source events (§3.3) are still hand-written: they are emitted by the web client, not by
the agent, and share no manifest with it.


> **`webrtc.state_changed` — Connected/Completed both mean established.** ICE may jump
> `Checking → Completed`, skipping `Connected` (CLAUDE.md). A bundle reader treats **both**
> `connected` and `completed` as transport-established; a classifier must not read
> "never reached `connected`" as a failure when `completed` is present.

> **`kind: "duplicate_answer"` is normal, not a defect.** The control plane buffers the
> agent's offer for a session whose browser has not attached yet and drains it on register,
> and the web client asks for an ICE restart on websocket open — so a *reconnecting* client
> receives two offers per PeerConnection and answers both. webrtcbin does not match an
> answer to the offer generation it answers: it applies whichever arrives first and refuses
> the other. The session is connected on the answer that did apply. Only
> `kind: "rejected"` means a PeerConnection will never carry media.

> **Every agent event is ordered before a terminal `session_state`, and is still
> best-effort.** The control plane drops an `agent_trace_event` whose session is no longer
> `running` on the host (`agent-api.md`), and the diagnostic lane is separate from the
> lifecycle lane — so an event emitted microseconds before the failure that caused it used
> to lose the race and never be stored. The agent now drains the diagnostic lane and sends
> everything pending *before* it serialises a terminal `session_state`
> (`agent::flush_pending_diagnostics`), which makes the ordering deterministic rather than
> timing-dependent. It applies to the whole lane, so `audio.degraded` and
> `session.effective_media` on a short-lived session are covered by the same guarantee.
> The lane is still bounded and droppable by design: the authoritative record of a failure
> is the session's `error_message` plus the agent log, never a trace row, and a missing row
> is not evidence the failure did not happen.
>
> **Follow-up (not shipped):** a per-PC connection-state watchdog — "the video PC has not
> reached `connected` N s after its answer was applied while the audio PC has" — would catch
> the shape of this failure that webrtcbin accepts and then never connects. It does not fit
> any existing timeout cleanly (the idle reap watches only the video webrtcbin's ICE state
> and charges its window from first present, #484), and #503 also asks for per-PC connection
> state in `session_metrics`, which is an `agent-api.md` addition and so needs sign-off.

> **`diag.*` is the one event family that is not fire-and-forget in effect.** It is still
> ack-less on the wire like every other `session_trace_event`, but the control plane persists
> it **synchronously** (the treatment `session.effective_media` gets) and **exempts it from
> retention** (§6). Both follow from the same fact: this row is the answer to a question a
> human asked thirty seconds ago and is polling for by `capture_id` — a coalescing-queue drop
> or a window prune would turn that poll into a timeout with nothing to show for the capture.

### 3.3 Browser-source events (`source='browser'`)

| type | when | payload fields |
|---|---|---|
| `playout.changed` | the AS-05 adaptive playout controller retargets the jitter-buffer/playout hint | `from_ms` (float), `to_ms` (float), `reason` (string, optional — e.g. `"degrade"`, `"recover"`) |
| `client.freeze_detected` | `getStats` `freezeCount` advances for this receiver | `gap_ms` (float, **optional** — the window's `present_interval_max_ms`; **omitted** when the window produced no cadence, which is what a long freeze looks like. It was formerly `1000 / freezeCount`, a number derived from the counter and from no clock at all), `is_hidden` (bool, optional — the false-positive guard) |
| `client.visibility_changed` | tab/window visibility flips (pairs with the `client.is_hidden` sample key) | `hidden` (bool) |
| `webrtc.state_changed` | a client-side `RTCPeerConnection` ICE/connection state transition | `kind` (string: `"ice"` \| `"connection"`), `from` (string), `to` (string) |

> The two `webrtc.state_changed` sources are distinguished by the row's `source` column
> (agent = host webrtcbin; browser = client `RTCPeerConnection`). A bundle reader can place
> both on one timeline and see, e.g., the host reaching `completed` before the client does.

> **`client.visibility_changed` is the #1 false-positive guard.** A hidden tab throttles
> presentation and inflates "dropped"/judder numbers that are display throttling, not
> network or encoder loss (CLAUDE.md, #108). The classifier (`ST-06`) must consult the
> `client.is_hidden` sample / `client.visibility_changed` events before attributing a
> presentation symptom to the stream.

---

## 4. Clock fields — and what is done with them

Browser-clock and host-clock series cannot be placed on one timeline without an
offset, and the offset is an **estimate** with a real measurement spread. The format
carries three values per session (persisted in `session_trace_clock`, written by
`ST-05`):

| field | type | meaning |
|---|---|---|
| `client_offset_ms` | double | the measured **host-clock − client-clock** offset (ms). |
| `uncertainty_ms` | double | the spread of the offset estimate (min-RTT/2). The honest error bar on every cross-clock alignment. |
| `measured_at` | timestamp | when the offset was last measured. |

### The sign convention

```
client_offset_ms = host_clock − client_clock
host_ts_unix_ms  = client_ts_unix_ms + client_offset_ms
```

**ADD it to a browser timestamp to get the host clock.** The field is *named*
`client_offset_ms` — the client's offset, the correction the client's clock needs —
which reads as though it were client−host. It is not, and an earlier revision of
this section said it was while also saying "add it", which cannot both be true. The
producing code is the arbiter: `web/src/webrtc/telemetry.ts` `onPong` computes
`offset = hostTs − (sendTc + now)/2` from two `Date.now()` stamps, and
`clockOffset.ts` documents the result as host-minus-client.

The convention is written down in exactly **one** place in the tree —
`control-plane/internal/telemetry/align.go` — and nothing else may restate it.

### Who applies it

`telemetry.AlignSeries(samples, events, clock)` does, once, at read time. It returns
an `AlignedSet` in which every browser/native point sits on the host clock and every
agent point is untouched (agent stamps **are** the host clock — they are what the
exercise aligns *to*). `normalizeSeries`, `computeDerivedWindows` and `classify` all
consume that set, so there is no path on which a rule reads an unaligned point.

Until 2026-08-23 the offset was measured, stored, emitted in the bundle — and applied
nowhere. Agent points (host wall-clock at drain) and browser points (`Date.now()`)
went onto one `ts` axis, and every cross-source rule silently assumed the two clocks
agreed. `Verdict.clock.applied` exists so that can never be true again unnoticed:
a measured clock that was not applied is visible in the value itself.

### The tolerance rule

A cross-source coincidence claim — "the ABR downshift happened *during* the
congestion window", "the hitch landed *on* a host fps dip" — is never made by
comparing two timestamps as though they were exact. It is made with a tolerance:

```
tolerance = max(uncertainty_ms, 1000 ms)
```

The 1000 ms floor is the reporting cadence: both sides sample about once a second, so
two points inside one sampling window are indistinguishable in time however good the
offset is. Sub-cadence skew must not be able to flip a claim; only skew larger than
the sampling window can.

### The unmeasured downgrade

**Hard rule — no false precision.** *Never present an unaligned or unmeasured client
timestamp as authoritative.*

- A session whose clock sync never succeeded is reported as **`unmeasured`** — the
  clock object is `{ "unmeasured": true }`, **never** `client_offset_ms: 0`. Offset 0
  is a measured value; a default of 0 is a silent lie.
- With an unmeasured clock, a cross-source coincidence claim is **downgraded, not
  dropped**: the classifier still says what one source supports on its own (whole-window
  aggregates), and the `reason`/evidence carries
  `cross-source timing unverified (clock unmeasured)`. The falsifiers that span
  sources carry the same note. `evidence_tier` still caps below `full` (§8).
- Any aligned series in a bundle carries `uncertainty_ms` so a reader — human or agent
  — knows the error bar.
- Glass-to-glass (`client.glass_to_glass_ms`) needs the offset; when the clock is
  `unmeasured` it is reported with that caveat, never as if aligned.

### Re-posting cadence

v1 posted the offset **once** per session and latched. A clock that drifted after the
first seconds then invalidated every aligned series for the rest of the session, while
bench mode re-estimated live from the same window — two offsets coexisting, one stale,
neither labelled.

The client now posts the first stable estimate and **re-posts while it drifts**: at
most every `web.clock.repost_interval_s` (60 s), and only when the estimate moved by
more than `web.clock.repost_delta_ms` (5 ms) from the last posted value. Below that
delta the change is inside the min-RTT estimator's own noise, and re-posting would
churn `measured_at` — which is what staleness is read from — without improving the
offset. `clockOffsetMs()` (what bench mode reads) and the posted value come from the
same estimator over the same window; there is no live-vs-latched split.
`UpsertClock` refreshes `measured_at`, and `Verdict.clock.age_ms` (now − `measured_at`)
makes staleness visible.

### Ingest: an implausible timestamp is dropped, not stored

Ingest never validated `ts_unix_ms`. A stamp in seconds, a `performance.now()` reading
(~1e5), or nanoseconds was stored happily and then vanished, because no read window is
wide enough to contain it: the row existed, the data did not, and nothing anywhere
said so.

A **client** sample or trace event whose `ts_unix_ms` falls outside ±24 h of server
now is now **dropped** at ingest, counted per session (in memory), and named. The
WARN — at most once per session per minute — carries the offending value and the
domain it most likely came from ("looks like seconds", "looks like performance.now",
"looks like nanoseconds"). The batch still returns `202`: telemetry never fails a
client. The counters surface as the diagnostic bundle's optional `ingest` object
(`rejected_ts`, `last_rejected_ts_unix_ms`, `last_rejected_reason`), absent entirely
when nothing was rejected.

The **agent** is not validated. It is the trusted host reporter and its stamps *are*
the host clock.

---

## 5. Reporters & trust

- **Agent (trusted host reporter):** emits `source='agent'` events over the WS
  (`agent-api.md session_trace_event`, additive). Its event `type` is taken from the v1
  allow-list; the control plane validates **host ownership** before storing (§7). Like
  `session_metrics`, it is fire-and-forget — no ack, no session-state authority.
- **Browser (untrusted client reporter):** posts `source='browser'` events to the control
  plane (the trace-events ingest, `control-api.md`). Owner-or-admin auth; unknown `type`
  dropped; bounded batch size; `202` on accept.
- **Native (future):** rides the same browser ingest with `client: "native"` (the same
  discriminator P9-01 added to the stats POST), producing `source='browser'`-class
  receiver events. No new path.

---

## 6. Retention

Trace data reuses the **same** retention model as `session_metrics` — one policy over all
three tables, no new mechanism, and still **no long-term retention** (no Timescale, no
weeks-long history). It lives in `control-plane/internal/telemetry`.

*Rewritten 2026-08-23. The previous text described a rolling window pruned "on event
arrival" plus a terminal prune. Both were true and both were wrong in a way worth naming:
the prune ran inline on all four ingest paths (~0.4 near-always-empty DELETEs a second per
session, on the hot path), and the terminal prune deleted a session's trace at the exact
moment an operator would go looking for it.*

- **Rolling window (live sessions).** While a session is **non-terminal**, samples and
  events older than the window are swept. Default 1 hour, `QUASAR_TELEMETRY_ROLLING_WINDOW`.
  This is what bounds a long-lived session.
- **Post-mortem retention (terminal sessions).** When a session reaches a terminal state its
  telemetry is **frozen, not deleted** — the rolling window stops being applied to it — and
  kept for the post-mortem retention. Default 24 hours,
  `QUASAR_TELEMETRY_POSTMORTEM_RETENTION` (validated at boot to be >= the rolling window).
  This is the rule that makes `make session-verdict` / `make session-bundle` work on a
  session that failed last night. After it expires the session's samples, non-capture
  events, and its `session_trace_clock` row are swept.
- **Captures (`diag.*`) are exempt from both.** The rolling window and the post-mortem sweep
  reap what a clock emitted; a capture is what a human asked for, and "why did that session
  behave that way" is asked in the past tense — an admin who captures a pipeline graph and
  then stops the session must still be able to read it. Captures leave with the session
  row's `ON DELETE CASCADE` and by nothing else. What makes the exemption safe is that they
  are sparse (one per explicit request), single-flight per session, and bounded in bytes by
  their own budget and again by a wire cap. See §10.
- **Both durations are measured against the server-side `created_at`** — the ingestion clock
  — never a reporter's `ts_unix_ms`, so a skewed or hostile reporter clock cannot evade the
  cap.
- **One janitor, no inline prunes.** `telemetry.retain` runs on the job dispatcher (every 5
  minutes by default; `QUASAR_TELEMETRY_RETAIN_INTERVAL`), deletes in bounded batches so no
  statement holds a long lock on a table the ingest path is writing, and logs one INFO line
  per pass with the counts (plus a WARN if a pass exceeds 30s). **Nothing on an ingest path
  or a lifecycle transition deletes telemetry** — `TestIngestPathsDoNotPrune` in
  `internal/session` enforces that by reading the package's own source.
- **Bounded reads.** Every trace/bundle read is a **bounded window** (default 2–10 min,
  `contract-amendment.md`) — "recent history", never the full series. Note this is a
  different thing from the rolling window: the read window is what a caller asks to see, the
  rolling window is what the server still has. The clock metadata is one small row per
  session.

This is exactly the read/retention shape the existing `(session_id, ts_unix_ms DESC)` index
already serves — the retention statements need no index of their own (see the comment on
`Retain` in `internal/telemetry/postgres.go` for the reasoning). The schema is *designed so*
a later Timescale conversion is a clean, isolated migration **if** longitudinal fleet
profiling is ever wanted — designed-for, not built (`ST-00`).

---

## 7. Security model

The trace surface reuses the **existing** telemetry trust boundaries — it introduces no new
auth mechanism, and (per CLAUDE.md) **never** uses a client-side flag as access control.

- **Reads are admin-gated.** Every trace read (`GET .../trace`, `.../trace/window`,
  `.../trace/metrics`, `.../trace/events`, `.../diagnostic-bundle`) requires
  `RequireAuth → RequireAdmin` — a valid non-admin bearer is `403` before any resource
  lookup (so an admin endpoint never leaks existence), exactly like
  `GET /v1/admin/sessions/{id}/metrics`.
- **Browser event writes are owner-or-admin.** The trace-events ingest applies the same
  ownership check as `POST /v1/sessions/{id}/stats` and `DELETE /v1/sessions/{id}`:
  owner-or-admin, `403` for a non-owner, `404` for an unknown id, `202` on accept. Owner is
  the **bearer identity**, never a body field.
- **Browser event `type` allow-list.** An event whose `type` is not in the §3.3 v1
  allow-list is **dropped, not stored** — the event analogue of `FilterBrowserMetrics`
  dropping unknown metric keys. Batches are bounded (size + count) and rate-limited.
- **Agent event writes validate host ownership.** An agent event is stored only if the
  target session is placed on the reporting host — the `GetSessionHostState` trust boundary
  (`metrics_store.go` / `agent_state.go`): a cross-host event is dropped (and logged), a
  session not running on this host is dropped, an unknown session is dropped. This is the
  same boundary that guards `session_metrics` and `session_state`.
- **Telemetry is never authority, never the hot path.** A failed trace insert never fails a
  session lifecycle transition; a malformed event is dropped, never fatal to the WS or the
  request. Trace data is a diagnostic cache of reporter truth, never access control and
  never a session-state signal.

---

## 8. Verdict (ST-09)

**Status:** approved by Michael 2026-08-23, additive. Contract:
`protocol/control-api.md` (§Authorization + the ST-09 amendment note) and the
`Verdict` / `Falsifier` schemas in `protocol/openapi.yaml`. Implementation:
`control-plane/internal/session/verdict.go`.

### What it is, and why it is a value

"Is this session healthy" used to be decided in five vocabularies across eleven
places, and the authoritative answer — the classifier's — was a string plus a
list of prose sentences. Nothing in it said **which numbers would falsify it**,
over **what window**, from **how many samples**, on **which clock**. Two costs
followed. Consumers copied the enum and broke silently when it grew (a stale
four-string copy in the diagnosis tooling turned a healthy `nominal` session
into a hard error on 2026-08-22). And an operator reading
`likely_client_presentation_limit` had no way to check the claim short of a
`psql` session — which is how a 1440p120 h265 session with recv fps 120, encode
7.5 ms, zero drops and present σ 1.9 ms came to be investigated, because one
headline scalar (`present_fps`, a **mean**) read 88–108.

So the verdict is now one **value**, computed by the same pure classifier over
the same window, scope unchanged (stream health only, no session authority):

```json
{
  "verdict": "likely_network_congestion",
  "evidence": [ "network congestion: {\"packets_lost_delta\":14,\"rtt_ms_p95\":60}" ],
  "reason": "Packet loss and round-trip time both crossed their congestion thresholds over a 300 s window (612 host, 598 client samples).",
  "window": { "from_ms": 1735689300000, "to_ms": 1735689600000, "n_host": 612, "n_client": 598 },
  "clock":  { "quality": "measured", "offset_ms": -3.2, "uncertainty_ms": 1.8 },
  "evidence_tier": "full",
  "falsifiers": [
    { "name": "transport.packets_lost", "estimator": "delta", "value": 14, "op": ">", "threshold": 5, "unit": "count", "n": 300, "holds": true }
  ],
  "thresholds_version": "2026-08-23.1"
}
```

`verdict` and `evidence` are **byte-for-byte** what ST-01/ST-07 produced. Every
other key is additive, so a consumer that reads only those two is unaffected.

Where it appears:

- `GET /v1/admin/sessions/{id}/verdict` — admin.
- `GET /v1/sessions/{id}/verdict` — owner-or-admin, rate-limited per session.
- the diagnostic bundle's `classifier` key.

All three are built by one function (`verdictFrom`), so they cannot drift.

### Falsifier semantics

A **falsifier** is a named, estimator-qualified number the verdict relies on:

| field | meaning |
|---|---|
| `name` | a taxonomy series name (§3), e.g. `encoder.fps` — never a raw metric key |
| `estimator` | `p10` \| `p95` \| `max` \| `delta` \| `mean` \| `any` \| `count_ge_threshold` |
| `value` | the computed number, or **`null`** when the series could not support the estimator |
| `op` / `threshold` / `unit` | the condition the verdict relies on |
| `n` | samples the estimator consumed |
| `holds` | whether the data satisfies that condition |
| `note` | why, when something is off (`"no samples"`, "not assessable: …") |

Three rules that are easy to get wrong:

1. **`holds` is not "good".** It answers "does the data satisfy the condition the
   verdict relies on". For `nominal`, every falsifier holding is what makes it
   nominal. For `likely_network_congestion`, the loss and rtt falsifiers hold
   **because** they crossed their thresholds — the verdict relies on them having
   crossed. A `holds: false` is where the verdict is weakest and where to look
   first to overturn it; `reason` names those explicitly.
2. **A missing measurement never reads as a passing one.** `n = 0` produces
   `value: null`, `holds: false` and `note: "no samples"`. A cumulative counter
   with a single sample cannot produce a `delta` and is reported the same way.
3. **The estimator is part of the claim.** "present σ was 19 ms" means different
   things as a mean, a p95 and a max. The 2026-08-22 trap was precisely a mean
   being read as if it were a floor.

Each verdict's falsifier set is the argument it makes: the conditions that
**fired**, followed by the guard conditions it **ruled out** (for
`likely_client_presentation_limit`: judder fired; host fps steady, wire quiet and
tab visible are what leave the client's own present path as the explanation).
`indeterminate_client_hidden` states what could **not** be assessed as a
falsifier with a note, rather than omitting it — an absent number is the thing
that misleads.

Thresholds come from the one golden file, `docs/session-trace/thresholds.json`,
whose `version` is echoed as `thresholds_version` so a stored verdict can be read
against the thresholds it was computed under. Drift tests on both sides
(`verdict_thresholds_test.go`, `web/src/webrtc/thresholds.test.ts`) fail if a
number moves in only one place.

### Evidence tier and clock

`evidence_tier` is a **claim about coverage**, never a default:

| tier | meaning |
|---|---|
| `full` | both host and client contributed ≥3 samples **and** the clock was measured |
| `host_only` | the client did not report — **or** both sides did but the clock is unmeasured, so they cannot be aligned |
| `client_only` | the host did not report |
| `insufficient` | neither side reached 3 samples |

`clock.quality` comes from the session's `session_trace_clock` row (§1). An
unmeasured clock — the common case — **can never produce `full`**, and the
capping is called out in `reason` so `host_only` is never misread as "the client
sent nothing".

`clock` also carries **`applied`** and **`age_ms`** (2026-08-23). `applied` is
whether the offset was actually used to put the client series on the host clock
before the rules ran — a measured clock that is merely *reported* is the defect
those two fields exist to make impossible to have again (§4). `age_ms` is
now − `measured_at`: the client re-posts while the offset drifts, so a large age
means the estimate stopped being refreshed, not that the clock stopped moving.

### Cross-source falsifiers carry their tolerance

A verdict's argument usually spans both reporters: `likely_client_presentation_limit`
fires on a CLIENT series and is guarded by a HOST one. Each falsifier from the side
that did **not** fire therefore carries a `note` naming the tolerance the coincidence
was assessed with — or, when the clock is unmeasured, saying that it was not
assessed at all and the leg rests on whole-window aggregates (§4). `nominal` makes no
coincidence claim and is not annotated: an unmeasured clock does not weaken "quiet on
both sides".

### Warm-up exclusion

`window.warmup_excluded_ms` is how much of the head of the window was excluded from
the two **warm-up-sensitive** rules — hitch detection and the `encoder.fps` floor —
because it fell within `classifier.warmup_exclude_s` (20 s) of the session reaching
**running**. A session's first seconds judder and under-run by construction (pipeline
filling, receiver buffer inflating, encoder ramping); inside a 300 s window they
otherwise sit there as permanent unsatisfied falsifiers for the rest of the session's
life. The samples are still **served** — the bundle carries every point — they are
simply not judged, and the amount is reported so the exclusion is visible rather than
a silent trim. A falsifier left with `n = 0` purely because of the exclusion says so
in its `note`, which is a different answer from "no samples".

### A hitch is a property of the window, not of one sample

`classifier.hitch_min_samples` (2) samples must reach `classifier.hitch_sd_ms` inside
the window before it counts as a hitch. On 2026-08-23 a healthy 300 s session flipped
to `likely_client_presentation_limit` on **one** present-interval σ of 18.058 ms out
of 54, because the rule was a `max` — the same estimator trap as 2026-08-22, one level
up. The condition is now the `count_ge_threshold` row; the `max` row stays beside it
because the worst single σ is worth seeing, and for `nominal` it is **informative**
(compared against the floor of its own unit, like `client.present_beat_fraction`, so
it carries the number without being able to fail on its own).

### `ingest`

The diagnostic bundle carries an optional `ingest` object when this control plane
dropped client points for an implausible `ts_unix_ms` — see §4. It is in memory and
per-process; absent means nothing was rejected (or the process restarted).

### The rule for consumers

**An unknown `verdict` string is DATA.** The control plane owns that vocabulary
and grows it (ST-07 split the overloaded `unknown` three ways; ST-09 will not be
the last change). A consumer must report an unrecognised value **verbatim** and
carry on — never fail, never map it to an error state, never keep a local copy of
the enum to validate against. `scripts/dx/session.sh` and
`session_diagnose/runner.py` both do exactly this, and `scripts/dx/tests/run.sh`
holds the 2026-08-22 regression as a test.

The same defensiveness runs the other way: a consumer may be pointed at a control
plane that predates ST-09 and returns only `verdict` + `evidence`. Every new field
is therefore optional to read — the admin trace viewer, the DX verb and the
diagnosis runner all degrade to the old display rather than throwing.

---

## 9. Present cadence (2026-08-22)

### What happened

A 2560×1440@120 h265 session reported: receiver fps 120, host encode 7.5 ms,
zero drops, zero freezes, present σ 1.9 ms. It was smooth. It was investigated
as an encoder fault, because the diagnostics panel's headline **fps (shown)**
read 88-108.

`present_fps` was `1000 / mean(RVFC frame-to-frame intervals)`. When the source
frame rate equals the client's display refresh rate, the two clocks free-run
against each other: the renderer occasionally misses one vsync, one interval
becomes two, and the frame after it lands on schedule again. Nothing is lost —
`framesDropped` stays 0, `freezeCount` stays 0, σ barely moves — but the MEAN
does move, hard. At 120 Hz with 12 % of intervals doubled the mean interval is
9.3 ms and the mean-derived fps is **107**, while the median interval is still
8.33 ms and the median-derived fps is still **120**.

The repo already knew this: `displayRefreshEstimator` had used a median since
AS10-14, for exactly this reason. The headline did not, and nothing on the
number said which estimator produced it.

### The fix: ship the distribution

The one-second window of intervals is no longer reduced to three scalars and
discarded (`web/src/webrtc/presentCadence.ts`). It is summarised, and the
summary is what travels:

| key | what it says |
|---|---|
| `present_fps_median` | **the reading.** fps from the median interval — the fps a viewer perceives, unmoved by an occasional doubled frame. |
| `present_interval_median_ms` | the median interval itself (ms). |
| `present_fps` | fps from the MEAN interval. Kept, unchanged, so the stored series stays comparable across this change — and deprecated as a headline. |
| `present_interval_sd_ms` / `_p95_ms` | unchanged: σ and p95 of the same intervals. |
| `present_interval_max_ms` | the longest interval in the window — the real stall length. |
| `present_beat_fraction` | share of intervals within ±20 % of exactly 2× the median: **the vsync beat**. |
| `present_long_frames` | intervals above 2.5× the median — above the top of the doubled band, so genuine stalls. |
| `present_n` | how many intervals the window held. Below 5, every other `present_*` key is omitted rather than computed from a fragment. |

### The vsync beat

`present_beat_fraction` and `present_long_frames` are read **together**, and
that pair is the whole diagnosis:

- **beat > 0, long frames 0** — the inherent consequence of running the source
  at the display's own rate. Not stutter. This is the 1440p120 case.
- **long frames > 0** — a stall. The beat explanation does not cover it, and
  the Verdict's `client.present_long_frames` falsifier says so (§8).

The client also computes `inherentBeat`, a positive claim it makes only when
source fps and display Hz agree to within 1, the doubled share is at or below
25 %, and no interval is long. It is **false whenever it cannot be established**
— unknown display Hz, unknown source fps, too few samples — because "we could
not tell" must never render as "nothing to see here".

Every threshold above (`5`, `±20 %`, `2.5×`, `25 %`) lives in
`docs/session-trace/thresholds.json` under `web.present_cadence.*`, marked
web-only: the control plane stores and serves these keys, it does not recompute
the cadence.

### For readers of older sessions

All six keys are additive and optional. A session recorded by a client that
predates them simply has no such series, and the Verdict's cadence falsifiers
report `value: null`, `n: 0`, `note: "no samples"` — never a passing zero.

---

## 10. On-demand capture (session-capture, 2026-08-23)

**Status:** approved by Michael 2026-08-23, additive, admin-only. Contract:
`protocol/agent-api.md` §`session_capture` + the `diag.*` rules in
§`session_trace_event`; `protocol/control-api.md` §On-demand capture; the
`Capture` / `CaptureRequest` / `CaptureAccepted` schemas in
`protocol/openapi.yaml`. Implementation:
`control-plane/internal/session/capture_handler.go`,
`scripts/dx/session.sh capture`.

### Why it exists

Three questions cost more debugging time than anything else in August 2026, and
all three were answerable only by an ssh hop, a rebuild, or a guess:

- **What is the encode graph actually wired as, right now?** The 2026-08-22
  `vulkanscale` work spent hours on a resize failure that a live graph would have
  shown immediately — the encoder was holding its launch-size DPB, and the shape
  of the pipeline was the evidence.
- **What are the encoder's live properties?** The same campaign chased a "driver
  bug" that turned out to be a probe negotiating `profile=main-444` while the
  production path pinned `profile=main`. The two differed in one property, and
  nothing on the wire reported which one was live.
- **What do encode times look like at a finer grain than the heartbeat?** The
  present-cadence work (§9) had to prove a distribution, and a once-per-heartbeat
  scalar cannot carry one.

Each of those is a *bounded, one-off observation of a running system*. Adding
another always-on metric for each would pay their cost on every session forever;
a capture pays it once, when asked.

### What a capture is, and what it is not

A capture is **arm → observe within a budget → report once**. It is:

- **not a probe.** Nothing is inserted into the media path, no pad probe is added,
  no element is reconfigured. A capture reads what the pipeline already is. (The
  host-side deep-trace overlay was removed in #270 precisely because a probe on
  the media path could crash the stream; this must never become that.)
- **not session authority.** Arming, polling, or reading never moves a session; a
  refusal is a no-op on a still-`running` session.
- **not a subscription.** One command, one result, **single-flight per session** —
  a second request while one is in flight is refused, never queued. One
  observation at a time is the entire cost bound.

### The safety envelope

These hold for every kind, including kinds added later:

| never | why |
|---|---|
| pixels, audio samples, encoded bitstream | a capture is about the machinery, never the content |
| input events, microphone | same, and they are the user's |
| `node_secret`, enrollment tokens, bearers, the environment wholesale | a diagnostic must not become a credential path |
| any path outside the session's scratch directory | `pipeline_dot` therefore uses `CAPS_DETAILS \| STATES` and **not** `NON_DEFAULT_PARAMS`, which can print string properties carrying paths |
| unbounded output | `compressed_bytes ≤ budget.max_bytes` (256 KiB). Over it: truncate the uncompressed text at a line boundary, recompress, `truncated: true`, `original_bytes` set |

`encoder_props` reads an **allow-list** of property names — never "every property
on the element" — and elides string values longer than 256 characters.

### Reading one

```
make session-capture SID=latest HOST=devbox KIND=pipeline_dot
make session-capture SID=latest HOST=devbox KIND=all OUT=.diagnostics/
```

The verb arms, polls (a `404` while in flight is the poll **signal**, not an
error), decodes, and writes the artifact `0600` under `.diagnostics/`, rendering
an `.svg` beside a `.dot` when graphviz is present. Every refusal names its own
next command; the one worth knowing is `501`, which means the host's agent
predates captures and never acked — `make rebuild HOST=<h>`, because retrying
will never help.
