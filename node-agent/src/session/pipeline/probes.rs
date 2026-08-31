//! Encode-path pad probes: the always-on P4-03 encode-metrics timing/counters and the
//! optional diagnostic traces. All attach to existing pads without changing the graph.

use std::sync::Arc;

use gstreamer as gst;
use gstreamer::prelude::*;

use crate::session::metrics::SessionMetrics;

/// #260: write the element's raw bitstream output to `path` for offline decode analysis
/// (root-causing `h264_decode_failed`, or the M2 codec-validate harness's ffmpeg decode
/// gate). Codec-agnostic — it captures whatever bytes cross the src pad. Knobs:
/// `QUASAR_CAPTURE_BITSTREAM` (alias `QUASAR_CAPTURE_H264`).
pub(super) fn attach_bitstream_capture(parser: &gst::Element, path: &str) {
    use std::io::Write;
    // Direct File, no BufWriter: the bytes must be on disk even if teardown races the flush.
    let file = match std::fs::File::create(path) {
        Ok(f) => std::sync::Arc::new(std::sync::Mutex::new(f)),
        Err(e) => {
            tracing::warn!(
                token = "capture-bitstream-open-failed",
                "QUASAR_CAPTURE_BITSTREAM: cannot create {path}: {e}"
            );
            return;
        }
    };
    let Some(pad) = parser.static_pad("src") else {
        tracing::warn!(
            token = "capture-bitstream-no-src-pad",
            "QUASAR_CAPTURE_BITSTREAM: parser has no src pad"
        );
        return;
    };
    tracing::info!("SPIKE: capturing encoder bitstream to {path}");
    pad.add_probe(gst::PadProbeType::BUFFER, move |_pad, info| {
        if let Some(buffer) = info.buffer() {
            if let Ok(map) = buffer.map_readable() {
                if let Ok(mut w) = file.lock() {
                    let _ = w.write_all(map.as_slice());
                }
            }
        }
        gst::PadProbeReturn::Ok
    });
}

/// Vulkan diagnostic: per RTP timestamp (= access unit), how many packets it spans and
/// whether the marker bit lands on the last packet of that run. libwebrtc's `PacketBuffer`
/// completes a frame by walking a contiguous seq-num run to the marker packet, so an AU
/// whose last packet has no marker never completes and is dropped pre-decode with
/// `packetsLost=0`. Send-side confirmation of that receiver symptom, on the real wire:
/// videotestsrc cannot reproduce rtph264pay's marker logic. Knob:
/// `QUASAR_TRACE_RTP_MARKER`. Rolling summary every `window` frames, target
/// `quasar.rtp.marker`.
pub(super) fn attach_rtp_marker_trace(pay: &gst::Element) {
    use gstreamer_rtp::RTPBuffer;
    use std::sync::Mutex;

    let Some(pad) = pay.static_pad("src") else {
        tracing::warn!(
            token = "trace-rtp-marker-no-src-pad",
            "QUASAR_TRACE_RTP_MARKER: rtph264pay has no src pad"
        );
        return;
    };
    struct St {
        cur_ts: Option<u32>,
        pkts_in_group: u32,
        last_pkt_marker: bool,
        frames: u64,
        frames_ending_in_marker: u64,
        total_pkts: u64,
        window: u64,
    }
    let st = std::sync::Arc::new(Mutex::new(St {
        cur_ts: None,
        pkts_in_group: 0,
        last_pkt_marker: false,
        frames: 0,
        frames_ending_in_marker: 0,
        total_pkts: 0,
        window: 120,
    }));
    tracing::info!(
        "QUASAR_TRACE_RTP_MARKER: RTP marker-layout trace attached on rtph264pay src \
         (per-AU marker-on-last-packet check; target quasar.rtp.marker)"
    );
    pad.add_probe(gst::PadProbeType::BUFFER, move |_pad, info| {
        let Some(buffer) = info.buffer() else {
            return gst::PadProbeReturn::Ok;
        };
        let Ok(rtp) = RTPBuffer::from_buffer_readable(buffer) else {
            return gst::PadProbeReturn::Ok;
        };
        let ts = rtp.timestamp();
        let marker = rtp.is_marker();
        let mut s = st.lock().unwrap();
        // One AU per timestamp: a change closes the previous group.
        if s.cur_ts.is_some() && s.cur_ts != Some(ts) {
            s.frames += 1;
            if s.last_pkt_marker {
                s.frames_ending_in_marker += 1;
            }
            let window = s.window;
            if s.frames.is_multiple_of(window) {
                let (f, ok, pk) = (s.frames, s.frames_ending_in_marker, s.total_pkts);
                let pct = if f > 0 {
                    100.0 * ok as f64 / f as f64
                } else {
                    0.0
                };
                tracing::info!(
                    target: "quasar.rtp.marker",
                    "RTP marker: {ok}/{f} frames end in marker ({pct:.1}% healthy), \
                     {pk} pkts, {:.2} pkts/frame",
                    if f > 0 { pk as f64 / f as f64 } else { 0.0 }
                );
            }
            s.pkts_in_group = 0;
        }
        s.cur_ts = Some(ts);
        s.pkts_in_group += 1;
        s.total_pkts += 1;
        s.last_pkt_marker = marker;
        gst::PadProbeReturn::Ok
    });
}

/// Vulkan diagnostic. libwebrtc's `PacketBuffer` keys an H.264 AU boundary on the RTP
/// timestamp change and requires the AU's packets to form a gapless seq-num run, so a frame
/// never completes (dropped pre-decode, `packetsLost=0`) if the send side breaks either:
///   - a duplicate RTP TS across two distinct AUs (the assembler merges them, both
///     incomplete; a 42% / every-other-frame pattern fits this),
///   - a non-monotonic TS, or a delta other than the expected 1500 @60fps,
///   - a seq-num gap inside an AU's run or between adjacent AUs.
///
/// Aggregates in-process (counters + first 20 exemplars, no per-packet flood). Knob:
/// `QUASAR_TRACE_RTP_TS`. Rolling summary every `window` AUs, target `quasar.rtp.ts`.
pub(super) fn attach_rtp_ts_trace(pay: &gst::Element) {
    use gstreamer_rtp::RTPBuffer;
    use std::sync::Mutex;

    let Some(pad) = pay.static_pad("src") else {
        tracing::warn!(
            token = "trace-rtp-ts-no-src-pad",
            "QUASAR_TRACE_RTP_TS: rtph264pay has no src pad"
        );
        return;
    };

    // Per-AU accumulator. An AU = a maximal run of packets sharing one RTP timestamp.
    struct St {
        // Current (open) AU.
        cur_ts: Option<u32>,
        cur_first_seq: Option<u16>,
        cur_last_seq: Option<u16>,
        cur_pkts: u32,
        cur_intra_gap: bool,   // any seq gap WITHIN the current AU's packet run
        cur_pts: Option<u64>,  // first gst-buffer PTS seen in this AU (ns)
        cur_distinct_pts: u32, // distinct gst-buffer PTS values mapped to this one RTP TS
        // Cross-AU history.
        prev_ts: Option<u32>,       // TS of the previous completed AU
        prev_last_seq: Option<u16>, // last seq of the previous completed AU
        seen_ts: std::collections::HashSet<u32>, // every TS that has closed an AU (dup-across-AU)
        // Counters (over completed AUs).
        aus: u64,
        delta_1500: u64,  // TS-delta vs prev AU == expected 1500 (60fps 90kHz)
        delta_zero: u64,  // TS-delta == 0 (DUPLICATE TS on adjacent AUs)
        delta_other: u64, // any other delta (incl. negative/non-monotonic)
        delta_hist: std::collections::BTreeMap<i64, u64>, // TS-delta -> count
        dup_ts_across: u64, // AU whose TS was already seen on an EARLIER (non-adjacent) AU
        nonmono: u64,     // TS strictly less than prev (wrap-aware small window)
        intra_gaps: u64,  // AUs with a seq gap within their own packet run
        inter_gaps: u64,  // AUs whose first seq != prev AU last seq + 1
        pts_collisions: u64, // AUs where >1 distinct gst PTS shared one RTP TS (the
        // clock-rounding / duplicate-TS merge signature)
        pkts_total: u64,
        pkts_min: u32,
        pkts_max: u32,
        // First 20 anomaly exemplars (delta!=1500 OR any gap OR dup-across).
        exemplars: Vec<String>,
        window: u64,
    }

    let st = std::sync::Arc::new(Mutex::new(St {
        cur_ts: None,
        cur_first_seq: None,
        cur_last_seq: None,
        cur_pkts: 0,
        cur_intra_gap: false,
        cur_pts: None,
        cur_distinct_pts: 0,
        prev_ts: None,
        prev_last_seq: None,
        seen_ts: std::collections::HashSet::new(),
        aus: 0,
        delta_1500: 0,
        delta_zero: 0,
        delta_other: 0,
        delta_hist: std::collections::BTreeMap::new(),
        dup_ts_across: 0,
        nonmono: 0,
        intra_gaps: 0,
        inter_gaps: 0,
        pts_collisions: 0,
        pkts_total: 0,
        pkts_min: u32::MAX,
        pkts_max: 0,
        exemplars: Vec::new(),
        window: 120,
    }));

    tracing::info!(
        "QUASAR_TRACE_RTP_TS: RTP timestamp/seqnum continuity trace attached on rtph264pay src \
         (per-AU TS-delta, dup-TS-across-AU, seq contiguity; target quasar.rtp.ts)"
    );

    pad.add_probe(gst::PadProbeType::BUFFER, move |_pad, info| {
        let Some(buffer) = info.buffer() else {
            return gst::PadProbeReturn::Ok;
        };
        let Ok(rtp) = RTPBuffer::from_buffer_readable(buffer) else {
            return gst::PadProbeReturn::Ok;
        };
        let ts = rtp.timestamp();
        let seq = rtp.seq();
        // rtph264pay derives the RTP TS from this PTS; two distinct PTS collapsing onto one
        // RTP TS is the clock-rounding merge.
        let pts = buffer.pts().map(|p| p.nseconds());
        let mut s = st.lock().unwrap();

        if let Some(cur) = s.cur_ts {
            if cur != ts {
                close_au(&mut s);
            }
        }

        if s.cur_ts != Some(ts) {
            s.cur_ts = Some(ts);
            s.cur_first_seq = Some(seq);
            s.cur_last_seq = Some(seq);
            s.cur_pkts = 1;
            s.cur_intra_gap = false;
            s.cur_pts = pts;
            s.cur_distinct_pts = 1;
        } else {
            // Same AU: seq contiguity within the packet run.
            if let Some(prev) = s.cur_last_seq {
                if seq != prev.wrapping_add(1) {
                    s.cur_intra_gap = true;
                }
            }
            s.cur_last_seq = Some(seq);
            s.cur_pkts += 1;
            // A distinct PTS under one RTP TS = two encoder frames rounded onto it.
            if let (Some(cur), Some(p)) = (s.cur_pts, pts) {
                if cur != p {
                    s.cur_distinct_pts += 1;
                    s.cur_pts = Some(p);
                }
            }
        }
        gst::PadProbeReturn::Ok
    });

    /// Finalize the open AU: fold TS-delta / dup / seq-gap stats and maybe emit a summary.
    fn close_au(s: &mut St) {
        let ts = match s.cur_ts {
            Some(t) => t,
            None => return,
        };
        let first_seq = s.cur_first_seq.unwrap_or(0);
        let last_seq = s.cur_last_seq.unwrap_or(0);
        let pkts = s.cur_pkts;

        s.aus += 1;
        s.pkts_total += pkts as u64;
        s.pkts_min = s.pkts_min.min(pkts);
        s.pkts_max = s.pkts_max.max(pkts);

        let mut anomaly: Option<String> = None;
        if let Some(prev_ts) = s.prev_ts {
            // 32-bit wrap-aware signed delta.
            let delta = (ts.wrapping_sub(prev_ts)) as i32 as i64;
            *s.delta_hist.entry(delta).or_insert(0) += 1;
            if delta == 1500 {
                s.delta_1500 += 1;
            } else if delta == 0 {
                s.delta_zero += 1;
                anomaly = Some(format!(
                    "DUP-TS-ADJACENT ts={ts} (prev AU had same TS) seq=[{first_seq}..{last_seq}] pkts={pkts}"
                ));
            } else {
                s.delta_other += 1;
                if delta < 0 {
                    s.nonmono += 1;
                }
                if anomaly.is_none() {
                    anomaly = Some(format!(
                        "TS-DELTA={delta} (expected 1500) ts={ts} prev_ts={prev_ts} seq=[{first_seq}..{last_seq}] pkts={pkts}"
                    ));
                }
            }
        }

        // Duplicate TS across a non-adjacent earlier AU (the prime PacketBuffer-merge suspect).
        if s.seen_ts.contains(&ts) {
            s.dup_ts_across += 1;
            if anomaly.is_none() {
                anomaly = Some(format!(
                    "DUP-TS-ACROSS ts={ts} (seen on an earlier AU) seq=[{first_seq}..{last_seq}] pkts={pkts}"
                ));
            }
        }
        s.seen_ts.insert(ts);

        if s.cur_intra_gap {
            s.intra_gaps += 1;
            if anomaly.is_none() {
                anomaly = Some(format!(
                    "INTRA-SEQ-GAP ts={ts} seq=[{first_seq}..{last_seq}] pkts={pkts} (non-contiguous within AU)"
                ));
            }
        }

        // Inter-AU: this AU's first seq must be the prev AU's last seq + 1.
        if let Some(prev_last) = s.prev_last_seq {
            if first_seq != prev_last.wrapping_add(1) {
                s.inter_gaps += 1;
                if anomaly.is_none() {
                    anomaly = Some(format!(
                        "INTER-SEQ-GAP first_seq={first_seq} prev_last_seq={prev_last} (expected {}) ts={ts}",
                        prev_last.wrapping_add(1)
                    ));
                }
            }
        }

        // Two encoder frames rounded to the same 90 kHz timestamp: the merge that makes
        // libwebrtc's PacketBuffer treat two AUs as one incomplete frame.
        if s.cur_distinct_pts > 1 {
            s.pts_collisions += 1;
            if anomaly.is_none() {
                anomaly = Some(format!(
                    "PTS-COLLISION ts={ts} carries {} distinct gst PTS in one RTP TS (seq=[{first_seq}..{last_seq}] pkts={pkts})",
                    s.cur_distinct_pts
                ));
            }
        }

        if let Some(ex) = anomaly {
            if s.exemplars.len() < 20 {
                s.exemplars.push(ex);
            }
        }

        s.prev_ts = Some(ts);
        s.prev_last_seq = Some(last_seq);

        let window = s.window;
        if s.aus.is_multiple_of(window) {
            // ~2 s @60fps. Bounded over a long arm (20 exemplars, 12 histogram rows), and
            // since there is no probe teardown hook to defer a final dump to, the freshest
            // window snapshot IS the result table.
            emit_summary(s, true);
        }
    }

    /// Roll up the aggregate table. `final_` adds the exemplars + histogram; otherwise one
    /// dense counter line, so the log doesn't flood.
    fn emit_summary(s: &St, final_: bool) {
        let n = s.aus.max(1);
        let pct = |x: u64| 100.0 * x as f64 / n as f64;
        let pkts_mean = s.pkts_total as f64 / n as f64;
        tracing::info!(
            target: "quasar.rtp.ts",
            "RTP-TS: aus={} | delta==1500 {} ({:.1}%) | delta==0(DUP-adj) {} ({:.1}%) | delta-other {} ({:.1}%) | dup-TS-across {} | pts-collisions {} | nonmono {} | intra-seq-gaps {} | inter-seq-gaps {} | pkts/AU mean {:.2} min {} max {}",
            s.aus,
            s.delta_1500, pct(s.delta_1500),
            s.delta_zero, pct(s.delta_zero),
            s.delta_other, pct(s.delta_other),
            s.dup_ts_across, s.pts_collisions, s.nonmono, s.intra_gaps, s.inter_gaps,
            pkts_mean,
            if s.pkts_min == u32::MAX { 0 } else { s.pkts_min }, s.pkts_max,
        );
        if final_ {
            let mut hist: Vec<(i64, u64)> = s.delta_hist.iter().map(|(k, v)| (*k, *v)).collect();
            hist.sort_by_key(|b| std::cmp::Reverse(b.1));
            let top: Vec<String> = hist
                .iter()
                .take(12)
                .map(|(d, c)| format!("{d}:{c}"))
                .collect();
            tracing::info!(target: "quasar.rtp.ts", "RTP-TS delta histogram (top): {}", top.join(" "));
            for (i, ex) in s.exemplars.iter().enumerate() {
                tracing::info!(target: "quasar.rtp.ts", "RTP-TS exemplar[{i}]: {ex}");
            }
        }
    }
}

/// The P4-03 encode metrics probes: the sink probe times each raw frame into the encoder
/// (FIFO-paired with the src side); the src probe counts each encoded buffer + its byte
/// size and closes the pair. The encode pipeline never restarts, so these survive a
/// launcher↔game swap.
///
/// `latency_probe` (`QUASAR_LATENCY_PROBE`) also records the sink-side PTS, closing the S1
/// pair opened at the compositor src pad, and queues the src-side instant for the S3 pair.
/// It adds no probe — the work rides these two callbacks, behind the `if latency_probe`
/// branch. Design: `docs/superpowers/specs/2026-08-18-latency-probe-design.md`.
pub(super) fn attach_encode_probes(
    encoder: &gst::Element,
    state: Arc<SessionMetrics>,
    latency_probe: bool,
) {
    let Some(sink) = encoder.static_pad("sink") else {
        tracing::warn!(
            token = "encode-metrics-no-sink-pad",
            "encoder has no sink pad — encode metrics probe not attached"
        );
        return;
    };
    let Some(src) = encoder.static_pad("src") else {
        tracing::warn!(
            token = "encode-metrics-no-src-pad",
            "encoder has no src pad — encode metrics probe not attached"
        );
        return;
    };

    // The encoder-input instant, FIFO-paired with the next encoded buffer on the src pad.
    // One clock read per frame (SO-03).
    let in_state = state.clone();
    sink.add_probe(gst::PadProbeType::BUFFER, move |_pad, info| {
        let now = std::time::Instant::now();
        in_state.record_encode_in(now);
        if latency_probe {
            // Metadata only: the PTS header field, never a pixel map (#270).
            let pts = info.buffer().and_then(|b| b.pts()).map(|p| p.nseconds());
            in_state.probe_record_encode_in(pts, now);
        }
        gst::PadProbeReturn::Ok
    });

    // One encoded frame out: count it, add its byte size (bitrate), close the FIFO pair
    // (out − in = encode_ms).
    src.add_probe(gst::PadProbeType::BUFFER, move |_pad, info| {
        if let Some(buffer) = info.buffer() {
            let now = std::time::Instant::now();
            state.record_encode_out(now, buffer.size() as u64);
            if latency_probe {
                state.probe_record_enc_out(now);
            }
        }
        gst::PadProbeReturn::Ok
    });
}

/// G1 buffer-reuse gate verification (multi-session Vulkan spec 2c). The compositor hands
/// out a child buffer whose `GstParentBufferMeta` refs the encode-src ring slot, and that
/// meta MUST survive `interpipesink → interpipesrc → queue → encoder` or the slot-reuse
/// gate silently reopens — the slot can then be recycled under the encoder (the green-bars
/// / device-loss family) with no other symptom. Warns ONCE, not per frame. Attach only on
/// the Vulkan-image encode path (`caps::vulkan_image_transport`).
pub(super) fn attach_vulkan_parent_meta_probe(encoder: &gst::Element) {
    use std::sync::atomic::{AtomicBool, Ordering};
    let Some(sink) = encoder.static_pad("sink") else {
        tracing::warn!(
            token = "vulkan-gate-no-sink-pad",
            "encoder has no sink pad — Vulkan ParentBufferMeta gate probe not attached"
        );
        return;
    };
    let warned = Arc::new(AtomicBool::new(false));
    let seen_ok = Arc::new(AtomicBool::new(false));
    sink.add_probe(gst::PadProbeType::BUFFER, move |_pad, info| {
        if let Some(buffer) = info.buffer() {
            if buffer.meta::<gst::ParentBufferMeta>().is_some() {
                if !seen_ok.swap(true, Ordering::Relaxed) {
                    tracing::info!(
                        "Vulkan G1 gate: ParentBufferMeta present at encoder sink (buffer-reuse gate live)"
                    );
                }
            } else if !warned.swap(true, Ordering::Relaxed) {
                tracing::warn!(
                    token = "vulkan-gate-meta-stripped",
                    "Vulkan G1 gate: encode-src buffer reached the encoder WITHOUT ParentBufferMeta \
                     — a meta-stripping element on interpipesink→interpipesrc→queue→encoder has \
                     reopened the buffer-reuse gate (spec 2c); slots may be recycled under the encoder"
                );
            }
        }
        gst::PadProbeReturn::Ok
    });
}

/// Wave 4.1 correlated stage probes. These observe existing pads only; they do
/// not add elements, alter caps, or participate in negotiation.
pub(super) fn attach_stage_probes(
    queue: &gst::Element,
    pay: &gst::Element,
    state: Arc<SessionMetrics>,
) {
    if let Some(sink) = queue.static_pad("sink") {
        let queue_state = state.clone();
        // Read the queue via `pad.parent_element()`, NEVER a captured strong clone: that is
        // a GObject ref cycle GLib never collects, and it leaked the queue, its probe-held
        // Arcs, and its cached GstContexts (~505 MiB VRAM/session).
        // `.claude/rules/gstreamer-gotchas.md`.
        sink.add_probe(gst::PadProbeType::BUFFER, move |pad, _info| {
            let level = pad
                .parent_element()
                .map(|queue| queue.property::<u32>("current-level-buffers") as u64)
                .unwrap_or(0);
            queue_state.record_queue_in(std::time::Instant::now(), level.saturating_add(1));
            gst::PadProbeReturn::Ok
        });
    }
    if let Some(src) = queue.static_pad("src") {
        let queue_state = state.clone();
        src.add_probe(gst::PadProbeType::BUFFER, move |_pad, _info| {
            queue_state.record_queue_out(std::time::Instant::now());
            gst::PadProbeReturn::Ok
        });
    }
    let overrun_state = state.clone();
    queue.connect("overrun", false, move |_| {
        overrun_state.record_queue_overrun();
        None
    });

    if let Some(src) = pay.static_pad("src") {
        // rtph264pay pushes a buffer LIST whenever an AU is fragmented or aggregated, and
        // a BUFFER probe never fires for a list, so a BUFFER-only probe saw only the rare
        // single-packet AU and `rtp_fps` / `rtp_bitrate_kbps` read ~0 against a real 60 fps
        // stream. Probe both; GStreamer fires exactly one per push, so no double count.
        src.add_probe(
            gst::PadProbeType::BUFFER | gst::PadProbeType::BUFFER_LIST,
            move |_pad, info| {
                if let Some(buffer) = info.buffer() {
                    let (bytes, marker) = rtp_packet_stats(buffer);
                    state.record_rtp_out(bytes, marker);
                } else if let Some(list) = info.buffer_list() {
                    for buffer in list.iter() {
                        let (bytes, marker) = rtp_packet_stats(buffer);
                        state.record_rtp_out(bytes, marker);
                    }
                }
                gst::PadProbeReturn::Ok
            },
        );
    }
}

/// Wire size of one RTP packet plus whether it closes an access unit (marker bit).
fn rtp_packet_stats(buffer: &gst::BufferRef) -> (u64, bool) {
    let marker = gstreamer_rtp::RTPBuffer::from_buffer_readable(buffer)
        .map(|rtp| rtp.is_marker())
        .unwrap_or(false);
    (buffer.size() as u64, marker)
}

/// Vulkan diagnostic: the gst-buffer PTS at the encoder sink and src for the first `N`
/// buffers of each, to localise a scrambled/non-monotonic output-PTS defect. The RTP-TS
/// trace showed Vulkan's rtph264pay timestamps cycling through 4 fixed values
/// (dup-TS-across ~100%, nonmono 25%) while VA is monotonic; the encode pipeline runs
/// `interpipesrc do-timestamp=false`, so the encoder passes source PTS straight through. A
/// monotonic sink with a cycling src means `vulkanh264enc` mangles the output timestamp.
/// Knob: `QUASAR_TRACE_ENC_PTS`. Target `quasar.enc.pts`.
pub(super) fn attach_encoder_pts_trace(encoder: &gst::Element) {
    use std::sync::atomic::{AtomicU64, Ordering};
    const N: u64 = 40;
    for (padname, tag) in [("sink", "IN "), ("src", "OUT")] {
        let Some(pad) = encoder.static_pad(padname) else {
            continue;
        };
        let count = std::sync::Arc::new(AtomicU64::new(0));
        let prev = std::sync::Arc::new(AtomicU64::new(u64::MAX));
        pad.add_probe(gst::PadProbeType::BUFFER, move |_pad, info| {
            let n = count.load(Ordering::Relaxed);
            if n >= N {
                return gst::PadProbeReturn::Ok;
            }
            if let Some(buffer) = info.buffer() {
                let pts = buffer.pts().map(|p| p.nseconds());
                let dts = buffer.dts().map(|d| d.nseconds());
                let p = prev.swap(pts.unwrap_or(u64::MAX), Ordering::Relaxed);
                let delta = match (pts, p) {
                    (Some(cur), old) if old != u64::MAX => cur as i128 - old as i128,
                    _ => 0,
                };
                tracing::info!(
                    target: "quasar.enc.pts",
                    "ENC-PTS[{tag}] #{n} pts={pts:?} dts={dts:?} delta_ns={delta}"
                );
                count.fetch_add(1, Ordering::Relaxed);
            }
            gst::PadProbeReturn::Ok
        });
    }
    tracing::info!(
        "QUASAR_TRACE_ENC_PTS: encoder sink/src PTS trace attached (first {N} each; target quasar.enc.pts)"
    );
}

#[cfg(test)]
mod tests {
    use super::*;

    fn init() {
        gst::init().unwrap();
    }

    /// The latency probe's two riskiest assumptions, on REAL buffers rather than hand-fed
    /// instants:
    ///
    ///  1. The PTS key survives to the encoder sink. S1 pairs the compositor's src pad
    ///     against the encoder's sink pad on buffer PTS across a queue and a converter; an
    ///     element re-stamping anywhere on that span silently omits every S1 sample.
    ///  2. The encoder pair survives a real encoder.
    ///
    /// S3's close is not exercised: it happens at the post-`rtpbin` egress seam, which
    /// exists only inside `webrtcbin`, and this pipeline stops at a fakesink. Software
    /// encoder + plain `videoconvert`, so it runs with no GPU and no interpipe.
    #[test]
    fn latency_probe_samples_real_buffer_flow() {
        init();
        let Ok(encoder) = gst::ElementFactory::make("openh264enc")
            .name("quasar-video-encoder")
            .build()
        else {
            return; // no software encoder in this image
        };
        let pipeline = gst::Pipeline::new();
        let src = gst::ElementFactory::make("videotestsrc")
            .property("is-live", true)
            .property("num-buffers", 90i32)
            .build()
            .unwrap();
        let caps = gst::ElementFactory::make("capsfilter")
            .property(
                "caps",
                gst::Caps::builder("video/x-raw")
                    .field("width", 320i32)
                    .field("height", 240i32)
                    .field("framerate", gst::Fraction::new(30, 1))
                    .build(),
            )
            .build()
            .unwrap();
        let queue = gst::ElementFactory::make("queue").build().unwrap();
        let convert = gst::ElementFactory::make("videoconvert").build().unwrap();
        let parser = gst::ElementFactory::make("h264parse").build().unwrap();
        let pay = gst::ElementFactory::make("rtph264pay").build().unwrap();
        let sink = gst::ElementFactory::make("fakesink")
            .property("sync", false)
            .build()
            .unwrap();
        let chain = [
            &src, &caps, &queue, &convert, &encoder, &parser, &pay, &sink,
        ];
        pipeline.add_many(chain).unwrap();
        gst::Element::link_many(chain).unwrap();

        let metrics = Arc::new(SessionMetrics::new("off", 30));
        // Stands in for `AppSource::attach_compositor_metrics`: same call, same pad
        // position (the source element's own src pad).
        let emit_state = metrics.clone();
        src.static_pad("src")
            .unwrap()
            .add_probe(gst::PadProbeType::BUFFER, move |_pad, info| {
                if let Some(buffer) = info.buffer() {
                    emit_state.probe_record_compositor_emit(
                        buffer.pts().map(|p| p.nseconds()),
                        std::time::Instant::now(),
                        None,
                    );
                }
                gst::PadProbeReturn::Ok
            });
        attach_encode_probes(&encoder, metrics.clone(), true);
        attach_stage_probes(&queue, &pay, metrics.clone());

        pipeline.set_state(gst::State::Playing).unwrap();
        let deadline = std::time::Instant::now() + std::time::Duration::from_secs(15);
        let mut encoded = 0u64;
        let mut cap_to_enc = 0usize;
        loop {
            std::thread::sleep(std::time::Duration::from_millis(200));
            let w = metrics.drain_window(std::time::Instant::now());
            encoded += w.frames_encoded;
            // Count windows that carried the stage, not the samples: the drain hands
            // back percentiles, not raw vectors.
            if w.probe_capture_to_enc_in_p50_ms.is_some() {
                cap_to_enc += 1;
            }
            if encoded >= 30 && cap_to_enc > 0 {
                break;
            }
            assert!(
                std::time::Instant::now() < deadline,
                "latency probe produced no capture→encoder samples over {encoded} encoded \
                 frames — the PTS key did not survive queue + videoconvert"
            );
        }
        pipeline.set_state(gst::State::Null).unwrap();
    }

    /// Regression for the ~505 MiB/session VRAM leak: the stage probes captured a strong
    /// `queue` clone into a probe on the queue's own sink pad, a ref cycle GLib never
    /// collects, pinning the queue's probe-held Arcs and cached GstContexts.
    ///
    /// Both latency-probe states: the probe adds captured state to these closures, so "no
    /// ref cycle" must hold with it on too.
    #[test]
    fn stage_probes_release_queue_and_metrics_on_drop() {
        init();
        for latency_probe in [false, true] {
            let queue = gst::ElementFactory::make("queue").build().unwrap();
            let pay = gst::ElementFactory::make("identity").build().unwrap();
            let metrics = Arc::new(SessionMetrics::new("off", 60));
            attach_stage_probes(&queue, &pay, metrics.clone());
            let weak_queue = queue.downgrade();
            drop(queue);
            drop(pay);
            assert!(
                weak_queue.upgrade().is_none(),
                "queue leaked (latency_probe={latency_probe}): a stage-probe closure holds a \
                 strong ref to the queue itself"
            );
            assert_eq!(
                Arc::strong_count(&metrics),
                1,
                "SessionMetrics leaked via stage-probe closures (latency_probe={latency_probe})"
            );
        }
    }

    /// Flow-level regression for the same leak: real buffers through
    /// interpipe → queue → encoder → payloader with the production probe set attached, then
    /// assert every probed element finalizes and the SessionMetrics Arc is released. The
    /// in-repo stand-in for the live churn check (VRAM / RSS flat across relaunches).
    #[test]
    fn probed_elements_finalize_after_real_buffer_flow() {
        init();
        if !crate::session::pipeline::interpipe_available() {
            return;
        }
        let src_pipe = gst::parse::launch(
            "videotestsrc is-live=true num-buffers=90 ! video/x-raw,width=320,height=240,framerate=30/1 \
             ! interpipesink name=leaktest-src sync=false forward-eos=false async=false",
        )
        .expect("source pipeline")
        .downcast::<gst::Pipeline>()
        .unwrap();

        let enc_pipe = gst::Pipeline::new();
        let ipsrc = gst::ElementFactory::make("interpipesrc")
            .property("listen-to", "leaktest-src")
            .property("is-live", true)
            .property("do-timestamp", true)
            .property_from_str("format", "time")
            .build()
            .expect("interpipesrc (gst-interpipe missing?)");
        let queue = gst::ElementFactory::make("queue").build().unwrap();
        let convert = gst::ElementFactory::make("videoconvert").build().unwrap();
        let encoder = gst::ElementFactory::make("openh264enc")
            .name("quasar-video-encoder")
            .build()
            .expect("openh264enc");
        let parser = gst::ElementFactory::make("h264parse").build().unwrap();
        let pay = gst::ElementFactory::make("rtph264pay").build().unwrap();
        let sink = gst::ElementFactory::make("fakesink")
            .property("sync", false)
            .build()
            .unwrap();
        enc_pipe
            .add_many([&ipsrc, &queue, &convert, &encoder, &parser, &pay, &sink])
            .unwrap();
        gst::Element::link_many([&ipsrc, &queue, &convert, &encoder, &parser, &pay, &sink])
            .unwrap();

        let metrics = Arc::new(SessionMetrics::new("off", 30));
        attach_encode_probes(&encoder, metrics.clone(), true);
        attach_stage_probes(&queue, &pay, metrics.clone());

        enc_pipe.set_state(gst::State::Playing).unwrap();
        src_pipe.set_state(gst::State::Playing).unwrap();
        // 90 frames at 30 fps ⇒ ~3 s; poll the counter rather than sleeping blind.
        let deadline = std::time::Instant::now() + std::time::Duration::from_secs(10);
        let mut frames_total: u64 = 0;
        loop {
            std::thread::sleep(std::time::Duration::from_millis(200));
            frames_total += metrics
                .drain_window(std::time::Instant::now())
                .frames_encoded;
            if frames_total >= 30 {
                break;
            }
            assert!(
                std::time::Instant::now() < deadline,
                "no buffer flow through the encode chain (frames_encoded {frames_total} < 30)"
            );
        }

        src_pipe.set_state(gst::State::Null).unwrap();
        enc_pipe.set_state(gst::State::Null).unwrap();

        let weak_queue = queue.downgrade();
        let weak_encoder = encoder.downgrade();
        let weak_pay = pay.downgrade();
        drop(queue);
        drop(convert);
        drop(encoder);
        drop(parser);
        drop(pay);
        drop(sink);
        drop(ipsrc);
        drop(src_pipe);
        drop(enc_pipe);

        assert!(weak_queue.upgrade().is_none(), "queue leaked after flow");
        assert!(
            weak_encoder.upgrade().is_none(),
            "encoder leaked after flow"
        );
        assert!(weak_pay.upgrade().is_none(), "payloader leaked after flow");
        assert_eq!(
            Arc::strong_count(&metrics),
            1,
            "SessionMetrics leaked via probe closures after flow"
        );
    }

    /// Regression for the near-dead `rtp_fps` / `rtp_bitrate_kbps` keys: a `BUFFER`-only
    /// probe never fires for the buffer LISTS `rtph264pay` pushes on a fragmented AU, so
    /// the counters read ~0.2 fps against a real 60 fps stream. Force fragmentation with a
    /// tiny MTU; the counters must track the encoder — one marker per AU, and more wire
    /// bytes than encoded bytes (RTP headers).
    #[test]
    fn stage_probes_count_rtp_buffer_lists() {
        init();
        let pipe = gst::Pipeline::new();
        let src = gst::ElementFactory::make("videotestsrc")
            .property("num-buffers", 60i32)
            .build()
            .unwrap();
        let caps = gst::ElementFactory::make("capsfilter")
            .property(
                "caps",
                gst::Caps::builder("video/x-raw")
                    .field("width", 320i32)
                    .field("height", 240i32)
                    .field("framerate", gst::Fraction::new(30, 1))
                    .build(),
            )
            .build()
            .unwrap();
        let queue = gst::ElementFactory::make("queue").build().unwrap();
        let convert = gst::ElementFactory::make("videoconvert").build().unwrap();
        let encoder = gst::ElementFactory::make("openh264enc")
            .build()
            .expect("openh264enc");
        let parser = gst::ElementFactory::make("h264parse").build().unwrap();
        // MTU below any encoded frame ⇒ every AU is FU-A fragmented ⇒ pushed as a list.
        let pay = gst::ElementFactory::make("rtph264pay")
            .property("mtu", 200u32)
            .build()
            .unwrap();
        let sink = gst::ElementFactory::make("fakesink")
            .property("sync", false)
            .build()
            .unwrap();
        pipe.add_many([
            &src, &caps, &queue, &convert, &encoder, &parser, &pay, &sink,
        ])
        .unwrap();
        gst::Element::link_many([
            &src, &caps, &queue, &convert, &encoder, &parser, &pay, &sink,
        ])
        .unwrap();

        let metrics = Arc::new(SessionMetrics::new("off", 30));
        attach_encode_probes(&encoder, metrics.clone(), false);
        attach_stage_probes(&queue, &pay, metrics.clone());

        let t0 = std::time::Instant::now();
        metrics.drain_window(t0);
        pipe.set_state(gst::State::Playing).unwrap();
        let bus = pipe.bus().unwrap();
        let msg = bus.timed_pop_filtered(
            gst::ClockTime::from_seconds(20),
            &[gst::MessageType::Eos, gst::MessageType::Error],
        );
        assert!(
            matches!(
                msg.as_ref().map(|m| m.view()),
                Some(gst::MessageView::Eos(_))
            ),
            "pipeline did not reach EOS cleanly: {msg:?}"
        );
        pipe.set_state(gst::State::Null).unwrap();

        let t1 = std::time::Instant::now();
        let w = metrics.drain_window(t1);
        let secs = (t1 - t0).as_secs_f64();
        let rtp_frames = (w.rtp_fps * secs).round() as u64;
        let rtp_kbits = w.rtp_bitrate_kbps * secs;
        let enc_kbits = w.bitrate_kbps * secs;
        assert_eq!(w.frames_encoded, 60, "encoder did not emit every frame");
        assert_eq!(
            rtp_frames, w.frames_encoded,
            "rtp_fps must count one marker per AU (buffer lists not probed?)"
        );
        assert!(
            rtp_kbits > enc_kbits,
            "rtp_bitrate_kbps ({rtp_kbits:.1} kbit) must exceed encoded bytes \
             ({enc_kbits:.1} kbit) once RTP headers are counted"
        );
    }

    /// Cycle-B of the same leak: encoder → probe → metrics → hook → encoder, since the
    /// encoder's pad probes hold `Arc<SessionMetrics>` while the ABR hooks inside it hold
    /// the encoder strong. The runner's teardown guard breaks it via `clear_hooks`.
    #[test]
    fn clear_hooks_releases_encoder_held_by_metrics_hooks() {
        init();
        let encoder = gst::ElementFactory::make("identity").build().unwrap();
        let metrics = Arc::new(SessionMetrics::new("off", 60));
        attach_encode_probes(&encoder, metrics.clone(), true);
        let hook_encoder = encoder.clone();
        metrics.set_on_window(Box::new(move |_, _| {
            let _keepalive = &hook_encoder;
        }));
        let weak_encoder = encoder.downgrade();
        drop(encoder);
        assert!(
            weak_encoder.upgrade().is_some(),
            "sanity: the metrics hook must hold the encoder until hooks are cleared"
        );
        metrics.clear_hooks();
        assert!(
            weak_encoder.upgrade().is_none(),
            "encoder leaked: metrics hooks retained after clear_hooks"
        );
    }
}
