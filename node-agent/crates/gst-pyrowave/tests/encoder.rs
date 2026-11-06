//! `pyrowaveenc` against the golden fixtures (`testdata/pyrowave/README.md` "Bars"): encode
//! each source at its budget, decode with the reference decoder, compare with the SOURCE.
//! Needs libpyrowave-shared and a Vulkan GPU; without them it skips, unless
//! `QUASAR_REQUIRE_PYROWAVE=1` makes that a failure. `QUASAR_PYROWAVE_DEVICE` picks the
//! render node.

use std::path::PathBuf;
use std::sync::{Arc, Once};
use std::time::{Duration, Instant};

use gstpyrowave::ffi::{Decoder, Device};
use gstreamer as gst;
use gstreamer::prelude::*;
use gstreamer_app as gst_app;

fn fixtures() -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../../../testdata/pyrowave")
}

fn device_node() -> Option<String> {
    std::env::var("QUASAR_PYROWAVE_DEVICE")
        .ok()
        .filter(|s| !s.is_empty())
}

/// The reference device, or `None` (skip) when this machine cannot encode PyroWave.
fn device() -> Option<Arc<Device>> {
    static INIT: Once = Once::new();
    INIT.call_once(|| {
        gst::init().unwrap();
        gstpyrowave::plugin_register_static().unwrap();
    });
    match Device::open(device_node().as_deref().map(std::path::Path::new)) {
        Ok(d) => Some(d),
        Err(e) if std::env::var("QUASAR_REQUIRE_PYROWAVE").as_deref() == Ok("1") => {
            panic!("QUASAR_REQUIRE_PYROWAVE=1 and no PyroWave device: {e}")
        }
        Err(e) => {
            eprintln!("skipping: no PyroWave device: {e}");
            None
        }
    }
}

/// Push `frames` through `appsrc ! pyrowaveenc ! appsink` and return the output buffers.
fn encode(caps: &str, kbps: u32, frames: &[Vec<u8>]) -> Vec<gst::Buffer> {
    let mut desc = format!(
        "appsrc name=src format=time caps=\"{caps}\" ! pyrowaveenc name=enc bitrate={kbps} "
    );
    if let Some(node) = device_node() {
        desc += &format!("device={node} ");
    }
    desc += "! appsink name=sink sync=false";
    let pipeline = gst::parse::launch(&desc)
        .unwrap()
        .downcast::<gst::Pipeline>()
        .unwrap();
    let src = pipeline
        .by_name("src")
        .unwrap()
        .downcast::<gst_app::AppSrc>()
        .unwrap();
    let sink = pipeline
        .by_name("sink")
        .unwrap()
        .downcast::<gst_app::AppSink>()
        .unwrap();
    pipeline.set_state(gst::State::Playing).unwrap();
    for (i, f) in frames.iter().enumerate() {
        let mut buf = gst::Buffer::from_slice(f.clone());
        buf.get_mut()
            .unwrap()
            .set_pts(gst::ClockTime::from_mseconds(i as u64 * 16));
        src.push_buffer(buf).unwrap();
    }
    src.end_of_stream().unwrap();
    let mut out = Vec::new();
    while let Some(sample) = sink.try_pull_sample(gst::ClockTime::from_seconds(10)) {
        out.push(sample.buffer_owned().unwrap());
    }
    let bus = pipeline.bus().unwrap();
    if let Some(msg) = bus.pop_filtered(&[gst::MessageType::Error]) {
        panic!("pipeline error: {msg:?}");
    }
    pipeline.set_state(gst::State::Null).unwrap();
    assert_eq!(out.len(), frames.len(), "one output per input frame");
    out
}

fn sof_words(frame: &[u8]) -> (u32, u32) {
    let w = |i: usize| u32::from_le_bytes(frame[i..i + 4].try_into().unwrap());
    (w(0), w(4))
}

#[test]
fn every_fixture_meets_the_encoder_bar_with_the_configured_sof_colour() {
    let Some(device) = device() else { return };
    let manifest: serde_json::Value =
        serde_json::from_slice(&std::fs::read(fixtures().join("manifest.json")).unwrap()).unwrap();
    let list = manifest["fixtures"].as_array().unwrap();
    assert!(!list.is_empty());
    for fx in list {
        let name = fx["name"].as_str().unwrap();
        let (w, h) = (
            fx["width"].as_u64().unwrap(),
            fx["height"].as_u64().unwrap(),
        );
        let budget = fx["budget_bytes"].as_u64().unwrap();
        let src = std::fs::read(fixtures().join(fx["src"].as_str().unwrap())).unwrap();
        // The budget is bitrate / fps: the largest kbit/s whose 60 fps budget fits.
        let kbps = (budget * 8 * 60 / 1000) as u32;
        let caps = format!(
            "video/x-raw,format=I420,width={w},height={h},framerate=60/1,colorimetry=bt709"
        );
        let out = encode(&caps, kbps, std::slice::from_ref(&src));
        let buf = &out[0];
        assert!(
            !buf.flags().contains(gst::BufferFlags::DELTA_UNIT),
            "{name}: a keyframe"
        );
        let frame = buf.map_readable().unwrap();
        assert!(
            frame.len() as u64 <= budget,
            "{name}: {} bytes over {budget}",
            frame.len()
        );

        let (w0, w1) = sof_words(&frame);
        let sof = &fx["sof"];
        assert_eq!(w0 >> 31, 1, "{name}: SOF first");
        assert_eq!((w0 & 0x3FFF) + 1, w as u32, "{name}: SOF width");
        assert_eq!(((w0 >> 14) & 0x3FFF) + 1, h as u32, "{name}: SOF height");
        let field = |bit: u32| u64::from((w1 >> bit) & 1);
        for (key, bit) in [
            ("chroma_resolution", 26),
            ("color_primaries", 27),
            ("transfer_function", 28),
            ("ycbcr_transform", 29),
            ("ycbcr_range", 30),
            ("chroma_siting", 31),
        ] {
            assert_eq!(field(bit), sof[key].as_u64().unwrap(), "{name}: SOF {key}");
        }
        assert_eq!(field(30), 1, "{name}: limited range");
        assert_eq!(field(31), 0, "{name}: centre chroma siting");

        let decoded = Decoder::new(device.clone(), w as u32, h as u32)
            .unwrap()
            .decode_i420(&frame)
            .unwrap();
        let ysz = (w * h) as usize;
        let csz = ((w / 2) * h.div_ceil(2)) as usize;
        let planes = [(0, ysz, "y"), (ysz, csz, "cb"), (ysz + csz, csz, "cr")];
        let mut line = format!("{name}: {} B of {budget};", frame.len());
        for (off, len, plane) in planes {
            let got = gstpyrowave::psnr(&src[off..off + len], &decoded[off..off + len]);
            let bar = fx["encoder_bar"]["min_psnr_db"][plane].as_f64().unwrap();
            line += &format!(" {plane} {got:.2} dB (bar {bar:.2})");
            assert!(
                got >= bar,
                "{name}: {plane} {got:.2} dB under the bar {bar:.2}"
            );
        }
        println!("{line}");
    }
}

#[test]
fn full_range_input_says_full_range_in_the_sof() {
    if device().is_none() {
        return;
    }
    let (w, h) = (320, 192);
    let src = std::fs::read(fixtures().join("detail_320x192.src.yuv")).unwrap();
    let caps =
        format!("video/x-raw,format=I420,width={w},height={h},framerate=60/1,colorimetry=1:3:5:1");
    let out = encode(&caps, 20_000, &[src]);
    let frame = out[0].map_readable().unwrap();
    assert_eq!(
        sof_words(&frame).1 >> 27,
        0,
        "sRGB, sRGB, BT.709, FULL, centre siting"
    );
}

#[test]
fn nv12_input_encodes_like_i420() {
    let Some(device) = device() else { return };
    let (w, h) = (320usize, 192usize);
    let src = std::fs::read(fixtures().join("detail_320x192.src.yuv")).unwrap();
    let (y, c) = src.split_at(w * h);
    let (cb, cr) = c.split_at(w * h / 4);
    let mut nv12 = y.to_vec();
    for (u, v) in cb.iter().zip(cr) {
        nv12.extend([*u, *v]);
    }
    let caps =
        format!("video/x-raw,format=NV12,width={w},height={h},framerate=60/1,colorimetry=bt709");
    let out = encode(&caps, 100_000, &[nv12]);
    let frame = out[0].map_readable().unwrap();
    let decoded = Decoder::new(device, w as u32, h as u32)
        .unwrap()
        .decode_i420(&frame)
        .unwrap();
    for (range, plane) in [(0..w * h, "y"), (w * h..w * h * 3 / 2, "cbcr")] {
        let got = gstpyrowave::psnr(&src[range.clone()], &decoded[range]);
        assert!(got > 40.0, "{plane}: {got:.2} dB");
    }
}

/// Encode time per frame in a running pipeline: from a buffer entering the element's sink
/// pad to its frame leaving the src pad. `cargo test -- --ignored encode_time` on a GPU host.
#[test]
#[ignore = "timing on a GPU host"]
fn encode_time() {
    if device().is_none() {
        return;
    }
    for (w, h, kbps) in [(1920, 1080, 200_000), (3840, 2160, 400_000)] {
        let mut desc = format!(
            "videotestsrc num-buffers=600 pattern=smpte ! \
             video/x-raw,format=I420,width={w},height={h},framerate=60/1,colorimetry=bt709 ! \
             pyrowaveenc name=enc bitrate={kbps} "
        );
        if let Some(node) = device_node() {
            desc += &format!("device={node} ");
        }
        desc += "! fakesink sync=false";
        let pipeline = gst::parse::launch(&desc).unwrap();
        let enc = pipeline
            .downcast_ref::<gst::Bin>()
            .unwrap()
            .by_name("enc")
            .unwrap();
        let start = Arc::new(std::sync::Mutex::new(None::<Instant>));
        let times = Arc::new(std::sync::Mutex::new(Vec::<Duration>::new()));
        let s = start.clone();
        enc.static_pad("sink")
            .unwrap()
            .add_probe(gst::PadProbeType::BUFFER, move |_, _| {
                *s.lock().unwrap() = Some(Instant::now());
                gst::PadProbeReturn::Ok
            });
        let (s, t) = (start.clone(), times.clone());
        enc.static_pad("src")
            .unwrap()
            .add_probe(gst::PadProbeType::BUFFER, move |_, _| {
                if let Some(at) = s.lock().unwrap().take() {
                    t.lock().unwrap().push(at.elapsed());
                }
                gst::PadProbeReturn::Ok
            });
        pipeline.set_state(gst::State::Playing).unwrap();
        let bus = pipeline.bus().unwrap();
        let msg = bus
            .timed_pop_filtered(
                gst::ClockTime::from_seconds(120),
                &[gst::MessageType::Eos, gst::MessageType::Error],
            )
            .unwrap();
        assert_eq!(msg.type_(), gst::MessageType::Eos, "{msg:?}");
        pipeline.set_state(gst::State::Null).unwrap();
        let mut t = times.lock().unwrap().split_off(30); // warm-up
        t.sort();
        let ms = |d: Duration| d.as_secs_f64() * 1000.0;
        println!(
            "{w}x{h} @ {kbps} kbit/s: {} frames, encode p50 {:.3} ms, p95 {:.3} ms, max {:.3} ms",
            t.len(),
            ms(t[t.len() / 2]),
            ms(t[t.len() * 95 / 100]),
            ms(t[t.len() - 1])
        );
    }
}
