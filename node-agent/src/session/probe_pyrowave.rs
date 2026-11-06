//! `media-probe --codec pyrowave`: the PyroWave codec probe (#1056). Encodes a test picture
//! with `pyrowaveenc` on the bound GPU and checks the reference decoder reads it back.
//! A pass adds `pyrowave` to that GPU's wire codec set (codec-pyrowave.md §Wire vocabulary).

use std::path::Path;
use std::time::Instant;

use gstreamer as gst;
use gstreamer::prelude::*;
use gstreamer_app as gst_app;

use super::probe_media::{MediaProbeRequest, ProbeVerdict};
use super::SessionConfig;

/// A broken encode reads back far below this; a working one at the probe's rate is well
/// above 40 dB on this picture.
const MIN_PSNR_DB: f64 = 30.0;
/// codec-pyrowave.md §Rate: the default target up to 1080p.
const PROBE_KBPS: u32 = 200_000;

/// Deterministic I420: luma ramps with a fine checker, chroma ramps. Even sizes only.
fn test_picture(w: usize, h: usize) -> Vec<u8> {
    let (cw, ch) = (w / 2, h / 2);
    let mut pic = Vec::with_capacity(w * h + 2 * cw * ch);
    for y in 0..h {
        for x in 0..w {
            let checker = if (x / 4 + y / 4) % 2 == 0 { 24 } else { 0 };
            pic.push((16 + (x * 160 / w) + (y * 40 / h) + checker) as u8);
        }
    }
    for y in 0..ch {
        pic.extend((0..cw).map(|x| (16 + x * 224 / cw + y % 2) as u8));
    }
    for y in 0..ch {
        pic.extend((0..cw).map(|_| (16 + y * 224 / ch) as u8));
    }
    pic
}

pub fn run(request: &MediaProbeRequest) -> ProbeVerdict {
    let (w, h, fps) = (request.width, request.height, request.fps);
    if request.frames == 0 || w <= 0 || h <= 0 || w % 2 != 0 || h % 2 != 0 || fps <= 0 {
        return ProbeVerdict::Usage(format!(
            "media-probe --codec pyrowave: --frames must be > 0 and --size an even WxH@FPS \
             (got {w}x{h}@{fps}, {} frames)",
            request.frames
        ));
    }
    let cfg = SessionConfig::from_env(false, None);
    if let Err(e) = super::init_gstreamer(&cfg) {
        return ProbeVerdict::Fail(format!("gstreamer init failed: {e:#}"));
    }
    let node = cfg.render_node.clone();
    if node.is_empty() || node == "software" {
        return ProbeVerdict::Unsupported("PyroWave needs a GPU; this probe has none".into());
    }
    if gst::ElementFactory::find("pyrowaveenc").is_none() {
        return ProbeVerdict::Unsupported("pyrowaveenc is not in this image".into());
    }
    // The element opens its own device; this one is the reference decoder's.
    let device = match gstpyrowave::ffi::Device::open(Some(Path::new(&node))) {
        Ok(d) => d,
        Err(e) => return ProbeVerdict::Unsupported(format!("{node}: {e}")),
    };
    tracing::info!(
        "pyrowave probe: gpu={} {w}x{h}@{fps} render_node={node}, want {} frames within {:?}",
        request.gpu,
        request.frames,
        request.budget
    );
    match encode_and_check(request, &node, &device) {
        Ok(verdict) | Err(verdict) => verdict,
    }
}

fn encode_and_check(
    request: &MediaProbeRequest,
    node: &str,
    device: &std::sync::Arc<gstpyrowave::ffi::Device>,
) -> Result<ProbeVerdict, ProbeVerdict> {
    let (w, h) = (request.width as usize, request.height as usize);
    let caps = gst::Caps::builder("video/x-raw")
        .field("format", "I420")
        .field("width", request.width)
        .field("height", request.height)
        .field("framerate", gst::Fraction::new(request.fps, 1))
        .field("colorimetry", "bt709")
        .build();
    let fail = |e: &dyn std::fmt::Display| ProbeVerdict::Fail(format!("{node}: {e}"));
    let src = gst_app::AppSrc::builder()
        .caps(&caps)
        .format(gst::Format::Time)
        .build();
    let enc = gst::ElementFactory::make("pyrowaveenc")
        .property("device", node)
        .property("bitrate", PROBE_KBPS)
        .build()
        .map_err(|e| fail(&e))?;
    let sink = gst_app::AppSink::builder().sync(false).build();
    let pipeline = gst::Pipeline::new();
    pipeline
        .add_many([src.upcast_ref(), &enc, sink.upcast_ref()])
        .and_then(|_| gst::Element::link_many([src.upcast_ref(), &enc, sink.upcast_ref()]))
        .map_err(|e| fail(&e))?;
    let bus_error = |pipeline: &gst::Pipeline| {
        pipeline
            .bus()
            .and_then(|bus| bus.pop_filtered(&[gst::MessageType::Error]))
            .and_then(|msg| match msg.view() {
                gst::MessageView::Error(e) => Some(format!("{} ({:?})", e.error(), e.debug())),
                _ => None,
            })
    };
    let stop = |pipeline: &gst::Pipeline| {
        let _ = pipeline.set_state(gst::State::Null);
    };
    // READY→PAUSED opens the device and loads the library: a refusal there is the GPU or
    // image having no PyroWave encoder, a hardware fact like a codec probe's READY failure.
    if pipeline.set_state(gst::State::Playing).is_err() {
        let why = bus_error(&pipeline).unwrap_or_else(|| "no error message".into());
        stop(&pipeline);
        return Err(ProbeVerdict::Unsupported(format!(
            "{node}: pyrowaveenc did not start: {why}"
        )));
    }

    let picture = test_picture(w, h);
    let frame_ns = 1_000_000_000 / request.fps as u64;
    for i in 0..request.frames {
        let mut buf = gst::Buffer::from_slice(picture.clone());
        buf.get_mut()
            .unwrap()
            .set_pts(gst::ClockTime::from_nseconds(i * frame_ns));
        if src.push_buffer(buf).is_err() {
            break;
        }
    }
    let _ = src.end_of_stream();

    let deadline = Instant::now() + request.budget;
    let mut decoder =
        gstpyrowave::ffi::Decoder::new(device.clone(), w as u32, h as u32).map_err(|e| {
            stop(&pipeline);
            ProbeVerdict::Unsupported(format!("{node}: reference decoder: {e}"))
        })?;
    let (ysz, csz) = (w * h, w * h / 4);
    let mut frames = 0;
    let mut worst = f64::INFINITY;
    let mut worst_plane = "";
    while frames < request.frames {
        let left = deadline.saturating_duration_since(Instant::now());
        let Some(sample) =
            sink.try_pull_sample(gst::ClockTime::from_nseconds(left.as_nanos() as u64))
        else {
            break;
        };
        let Some(buffer) = sample.buffer() else { break };
        let map = buffer.map_readable().map_err(|e| fail(&e))?;
        let decoded = decoder.decode_i420(&map).map_err(|e| {
            stop(&pipeline);
            fail(&format!("frame {frames} does not decode: {e}"))
        })?;
        for (range, plane) in [
            (0..ysz, "y"),
            (ysz..ysz + csz, "cb"),
            (ysz + csz..ysz + 2 * csz, "cr"),
        ] {
            let db = gstpyrowave::psnr(&picture[range.clone()], &decoded[range]);
            if db < worst {
                (worst, worst_plane) = (db, plane);
            }
        }
        frames += 1;
    }
    let error = bus_error(&pipeline);
    stop(&pipeline);
    if let Some(e) = error {
        return Err(fail(&format!("encode failed after {frames} frames: {e}")));
    }
    if frames < request.frames {
        return Err(fail(&format!(
            "pyrowaveenc produced {frames} of {} frames within {:?}",
            request.frames, request.budget
        )));
    }
    if worst < MIN_PSNR_DB {
        return Err(ProbeVerdict::Fail(format!(
            "{node}: PyroWave frames decode, but the picture is wrong: {worst_plane} plane \
             {worst:.1} dB, under {MIN_PSNR_DB} dB"
        )));
    }
    Ok(ProbeVerdict::Pass(format!(
        "encoded {frames} frames with pyrowaveenc on {node} at {PROBE_KBPS} kbit/s; the \
         reference decoder read every one back (worst plane {worst_plane} {worst:.1} dB)"
    )))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_test_picture_is_i420_sized_and_in_limited_range() {
        let pic = test_picture(64, 32);
        assert_eq!(pic.len(), 64 * 32 * 3 / 2);
        assert!(pic.iter().all(|&v| (16..=240).contains(&v)));
    }

    #[test]
    fn an_odd_size_is_a_usage_error() {
        let request = MediaProbeRequest {
            width: 1279,
            ..MediaProbeRequest::codec_probe(0, super::super::probe_media::PYROWAVE)
        };
        assert!(matches!(run(&request), ProbeVerdict::Usage(_)));
    }
}
