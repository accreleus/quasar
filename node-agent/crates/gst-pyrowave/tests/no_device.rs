//! `pyrowaveenc` behaviour that needs no GPU and no library, so it runs in CI.

use std::sync::Once;

use gstreamer as gst;
use gstreamer::prelude::*;

fn init() {
    static INIT: Once = Once::new();
    INIT.call_once(|| {
        gst::init().unwrap();
        gstpyrowave::plugin_register_static().unwrap();
    });
}

#[test]
fn the_sink_pad_accepts_only_bt709_4_2_0_yuv() {
    init();
    let enc = gst::ElementFactory::make("pyrowaveenc").build().unwrap();
    let sink = enc.static_pad("sink").unwrap();
    let accepts = |caps: &str| sink.query_accept_caps(&caps.parse::<gst::Caps>().unwrap());
    let size = "width=320,height=192,framerate=60/1";
    assert!(accepts(&format!(
        "video/x-raw,format=I420,{size},colorimetry=bt709"
    )));
    assert!(accepts(&format!(
        "video/x-raw,format=NV12,{size},colorimetry=1:3:5:1"
    )));
    for refused in [
        format!("video/x-raw,format=Y444,{size},colorimetry=bt709"),
        format!("video/x-raw,format=RGBA,{size},colorimetry=bt709"),
        format!("video/x-raw,format=I420_10LE,{size},colorimetry=bt709"),
        format!("video/x-raw,format=I420,{size},colorimetry=bt601"),
        format!("video/x-raw(memory:VulkanImage),format=NV12,{size},colorimetry=bt709"),
    ] {
        assert!(!accepts(&refused), "{refused}");
    }
}

/// Where this machine cannot open a PyroWave device (CI has no library, a GPU-less
/// container has no Vulkan device), the element fails to start with the reason on the
/// bus, and nothing panics.
#[test]
fn without_a_device_the_element_fails_to_start_and_says_why() {
    init();
    if gstpyrowave::ffi::Device::open(None).is_ok() {
        eprintln!("skipping: this machine has a PyroWave device");
        return;
    }
    let pipeline = gst::parse::launch(
        "videotestsrc num-buffers=1 ! video/x-raw,format=I420,width=64,height=64,\
         colorimetry=bt709 ! pyrowaveenc ! fakesink",
    )
    .unwrap();
    assert!(pipeline.set_state(gst::State::Paused).is_err());
    let msg = pipeline
        .bus()
        .unwrap()
        .pop_filtered(&[gst::MessageType::Error])
        .expect("an error message on the bus");
    let gst::MessageView::Error(err) = msg.view() else {
        unreachable!()
    };
    // The base class posts the domain's message; the element's reason is in the debug text.
    let text = format!("{} {:?}", err.error(), err.debug());
    assert!(text.contains("PyroWave device:"), "{text}");
    pipeline.set_state(gst::State::Null).unwrap();
}
