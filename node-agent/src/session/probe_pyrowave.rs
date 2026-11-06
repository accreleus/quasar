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

/// A resource shortage says nothing about the GPU's capability: indeterminate, so the
/// last definitive result stands (agent-api.md amendment 11). Anything else is `definite`.
fn classify(message: String, definite: fn(String) -> ProbeVerdict) -> ProbeVerdict {
    if gstpyrowave::ffi::is_resource_error(&message) {
        ProbeVerdict::Indeterminate(message)
    } else {
        definite(message)
    }
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
    let decoder = gstpyrowave::ffi::Device::open(Some(Path::new(&node)))
        .and_then(|device| gstpyrowave::ffi::Decoder::new(device, w as u32, h as u32));
    let mut decoder = match decoder {
        Ok(d) => d,
        Err(e) => return classify(format!("{node}: {e}"), ProbeVerdict::Unsupported),
    };
    tracing::info!(
        "pyrowave probe: gpu={} {w}x{h}@{fps} render_node={node}, want {} frames within {:?}",
        request.gpu,
        request.frames,
        request.budget
    );
    encode_and_check(request, &node, &mut decoder)
}

fn encode_and_check(
    request: &MediaProbeRequest,
    node: &str,
    decoder: &mut gstpyrowave::ffi::Decoder,
) -> ProbeVerdict {
    let caps = gst::Caps::builder("video/x-raw")
        .field("format", "I420")
        .field("width", request.width)
        .field("height", request.height)
        .field("framerate", gst::Fraction::new(request.fps, 1))
        .field("colorimetry", "bt709")
        .build();
    let src = gst_app::AppSrc::builder()
        .caps(&caps)
        .format(gst::Format::Time)
        .build();
    let enc = match gst::ElementFactory::make("pyrowaveenc")
        .property("device", node)
        .property("bitrate", PROBE_KBPS)
        .build()
    {
        Ok(enc) => enc,
        Err(e) => return ProbeVerdict::Fail(format!("{node}: {e}")),
    };
    let sink = gst_app::AppSink::builder().sync(false).build();
    let pipeline = gst::Pipeline::new();
    if let Err(e) = pipeline
        .add_many([src.upcast_ref(), &enc, sink.upcast_ref()])
        .and_then(|_| gst::Element::link_many([src.upcast_ref(), &enc, sink.upcast_ref()]))
    {
        return ProbeVerdict::Fail(format!("{node}: {e}"));
    }
    let bus_error = || {
        pipeline
            .bus()
            .and_then(|bus| bus.pop_filtered(&[gst::MessageType::Error]))
            .and_then(|msg| match msg.view() {
                gst::MessageView::Error(e) => Some(format!("{} ({:?})", e.error(), e.debug())),
                _ => None,
            })
    };
    // READY→PAUSED opens the element's device: a refusal there is the GPU or image having
    // no PyroWave encoder, a hardware fact like a codec probe's READY failure.
    let started = pipeline.set_state(gst::State::Playing).is_ok();
    let outcome = if started {
        push_and_read_back(request, &src, &sink, decoder)
    } else {
        Err(String::new())
    };
    let error = bus_error();
    // Every path below has stopped the pipeline.
    let _ = pipeline.set_state(gst::State::Null);
    if !started {
        let why = error.unwrap_or_else(|| "no error message".into());
        return classify(
            format!("{node}: pyrowaveenc did not start: {why}"),
            ProbeVerdict::Unsupported,
        );
    }
    if let Some(e) = error {
        return classify(format!("{node}: encode failed: {e}"), ProbeVerdict::Fail);
    }
    let (frames, worst, worst_plane) = match outcome {
        Ok(read) => read,
        Err(e) => return classify(format!("{node}: {e}"), ProbeVerdict::Fail),
    };
    if frames < request.frames {
        // A deadline passed (a device lost mid-probe shows this way too): inconclusive.
        return ProbeVerdict::Indeterminate(format!(
            "{node}: pyrowaveenc produced {frames} of {} frames within {:?}",
            request.frames, request.budget
        ));
    }
    if worst < MIN_PSNR_DB {
        return ProbeVerdict::Fail(format!(
            "{node}: PyroWave frames decode, but the picture is wrong: {worst_plane} plane \
             {worst:.1} dB, under {MIN_PSNR_DB} dB"
        ));
    }
    ProbeVerdict::Pass(format!(
        "encoded {frames} frames with pyrowaveenc on {node} at {PROBE_KBPS} kbit/s; the \
         reference decoder read every one back (worst plane {worst_plane} {worst:.1} dB)"
    ))
}

/// Push the test picture `request.frames` times and decode what comes out within the
/// budget: frames read back, the worst plane PSNR and its plane.
fn push_and_read_back(
    request: &MediaProbeRequest,
    src: &gst_app::AppSrc,
    sink: &gst_app::AppSink,
    decoder: &mut gstpyrowave::ffi::Decoder,
) -> Result<(u64, f64, &'static str), String> {
    let (w, h) = (request.width as usize, request.height as usize);
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
    let (ysz, csz) = (w * h, w * h / 4);
    let (mut frames, mut worst, mut worst_plane) = (0, f64::INFINITY, "");
    while frames < request.frames {
        let left = deadline.saturating_duration_since(Instant::now());
        let Some(sample) =
            sink.try_pull_sample(gst::ClockTime::from_nseconds(left.as_nanos() as u64))
        else {
            break;
        };
        let buffer = sample.buffer().ok_or("a sample with no buffer")?;
        let map = buffer.map_readable().map_err(|e| e.to_string())?;
        let decoded = decoder
            .decode_i420(&map)
            .map_err(|e| format!("frame {frames} does not decode: {e}"))?;
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
    Ok((frames, worst, worst_plane))
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
    fn a_memory_shortage_is_indeterminate_and_anything_else_definite() {
        let oom =
            "/dev/dri/renderD128: pyrowave_encoder_create failed: -4 ERROR_OUT_OF_DEVICE_MEMORY";
        assert!(matches!(
            classify(oom.into(), ProbeVerdict::Unsupported),
            ProbeVerdict::Indeterminate(_)
        ));
        let novk = "pyrowave_create_device_by_compat failed: -5 ERROR_NO_VULKAN";
        assert!(matches!(
            classify(novk.into(), ProbeVerdict::Unsupported),
            ProbeVerdict::Unsupported(_)
        ));
        assert!(matches!(
            classify("x".into(), ProbeVerdict::Fail),
            ProbeVerdict::Fail(_)
        ));
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
