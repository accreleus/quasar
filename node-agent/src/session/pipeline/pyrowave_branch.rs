//! The PyroWave encode branch of a granted WebTransport session (codec-pyrowave.md,
//! amendment 55). A tee after the encode pipeline's queue feeds raw frames, downloaded to
//! system memory, to an appsink; a separate pipeline (appsrc → convert → `pyrowaveenc`)
//! encodes them. Its own pipeline keeps a PyroWave failure off the encode pipeline's bus
//! and out of its flow: it switches PyroWave off (`encoder_error`), never ends the session.
//!
//! shortcut: frames cross the CPU (download + convert) before a CPU-input encoder; zero-copy
//! Vulkan input is #1104.

use anyhow::{anyhow, Context, Result};
use gstreamer as gst;
use gstreamer::prelude::*;
use gstreamer_app as gst_app;

use crate::session::SessionConfig;

/// The encode-pipeline half: a tee pad's leaky queue, a download when the raw caps are GPU
/// memory, and the appsink the PyroWave pipeline is fed from.
pub(crate) struct Feed {
    pub(crate) queue: gst::Element,
    pub(crate) sink: gst_app::AppSink,
}

/// The element that brings `raw` (the interpipe's caps) to system memory, `None` when it is
/// already there. DMABuf has none here.
fn download_for(raw: &gst::Caps) -> Result<Option<&'static str>> {
    let features = raw.features(0).map(|f| f.to_string()).unwrap_or_default();
    match features.as_str() {
        "" | "memory:SystemMemory" => Ok(None),
        "memory:VulkanImage" => Ok(Some("vulkandownload")),
        "memory:CUDAMemory" => Ok(Some("cudadownload")),
        other => Err(anyhow!("no PyroWave feed from {other} frames")),
    }
}

/// Add the feed to `pipeline` off `tee`. Its queue's src pad is where the caller gates it.
pub(crate) fn add_feed(
    pipeline: &gst::Pipeline,
    tee: &gst::Element,
    raw: &gst::Caps,
) -> Result<Feed> {
    let download = download_for(raw)?
        .map(|name| {
            gst::ElementFactory::make(name)
                .build()
                .with_context(|| format!("{name} not found"))
        })
        .transpose()?;
    let queue = gst::ElementFactory::make("queue")
        .name("pyrowave-feed-queue")
        .property("max-size-buffers", 1u32)
        .property("max-size-bytes", 0u32)
        .property("max-size-time", 0u64)
        .property_from_str("leaky", "downstream")
        .build()?;
    // async=false: the gate drops every frame while no PyroWave client is attached, and a
    // sink that never prerolls must not hold the encode pipeline's state change.
    let sink = gst_app::AppSink::builder()
        .name("pyrowave-feed")
        .sync(false)
        .max_buffers(1)
        .drop(true)
        .enable_last_sample(false)
        .build();
    sink.set_property("async", false);
    let mut chain: Vec<&gst::Element> = vec![&queue];
    chain.extend(download.as_ref());
    chain.push(sink.upcast_ref());
    pipeline.add_many(chain.iter().copied())?;
    gst::Element::link_many(chain.iter().copied())?;
    tee.link(&queue)
        .context("link the PyroWave feed to the tee")?;
    Ok(Feed { queue, sink })
}

/// The PyroWave pipeline: appsrc → videoconvert → BT.709 centre-sited I420 → `pyrowaveenc`
/// at `kbps` → fakesink. `on_frame` gets each encoded frame, `on_error` the first error.
/// The returned pipeline is PLAYING; the caller sets it to NULL.
pub(crate) fn start_encoder(
    cfg: &SessionConfig,
    feed: &Feed,
    kbps: u32,
    on_frame: impl Fn(&gst::BufferRef) + Send + Sync + 'static,
    on_error: impl Fn(String) + Send + Sync + 'static,
) -> Result<(gst::Pipeline, gst::Element)> {
    let mut enc = gst::ElementFactory::make("pyrowaveenc").property("bitrate", kbps);
    if !cfg.render_node.is_empty() && cfg.render_node != "software" {
        enc = enc.property("device", cfg.render_node.as_str());
    }
    let enc = enc.build().context("pyrowaveenc not found")?;
    let pipeline = start_with(feed, &enc, on_frame, on_error)?;
    Ok((pipeline, enc))
}

fn start_with(
    feed: &Feed,
    enc: &gst::Element,
    on_frame: impl Fn(&gst::BufferRef) + Send + Sync + 'static,
    on_error: impl Fn(String) + Send + Sync + 'static,
) -> Result<gst::Pipeline> {
    let pipeline = gst::Pipeline::with_name("pyrowave");
    let src = gst_app::AppSrc::builder()
        .is_live(true)
        .format(gst::Format::Time)
        .block(false)
        .build();
    src.set_property("max-buffers", 1u64);
    if src.find_property("leaky-type").is_some() {
        src.set_property_from_str("leaky-type", "downstream");
    }
    let convert = gst::ElementFactory::make("videoconvert").build()?;
    if convert.find_property("n-threads").is_some() {
        convert.set_property("n-threads", 0u32);
    }
    // codec-pyrowave.md §`hello_ack`: BT.709 always, limited range here, centre chroma
    // siting (erratum quasar-protocol#29 item 4).
    let caps = gst::Caps::builder("video/x-raw")
        .field("format", "I420")
        .field("colorimetry", "bt709")
        .field("chroma-site", "jpeg")
        .build();
    let filter = gst::ElementFactory::make("capsfilter")
        .property("caps", &caps)
        .build()?;
    let sink = gst::ElementFactory::make("fakesink")
        .property("sync", false)
        .property("async", false)
        .build()?;
    pipeline.add_many([src.upcast_ref(), &convert, &filter, enc, &sink])?;
    gst::Element::link_many([src.upcast_ref(), &convert, &filter, enc, &sink])?;

    enc.static_pad("src")
        .context("pyrowaveenc has no src pad")?
        .add_probe(gst::PadProbeType::BUFFER, move |_, info| {
            if let Some(buffer) = info.buffer() {
                on_frame(buffer);
            }
            gst::PadProbeReturn::Ok
        });
    let bus = pipeline.bus().context("a pipeline has a bus")?;
    bus.set_sync_handler(move |_, msg| {
        if let gst::MessageView::Error(err) = msg.view() {
            on_error(format!(
                "{}: {}",
                err.error(),
                err.debug().unwrap_or_default()
            ));
        }
        gst::BusSyncReply::Drop
    });

    let weak: glib::WeakRef<gst_app::AppSrc> = src.downgrade();
    feed.sink.set_callbacks(
        gst_app::AppSinkCallbacks::builder()
            .new_sample(move |sink| {
                let sample = sink.pull_sample().map_err(|_| gst::FlowError::Eos)?;
                // A push the PyroWave pipeline refuses (stopped, failed) is its own business.
                if let Some(src) = weak.upgrade() {
                    let _ = src.push_sample(&sample);
                }
                Ok(gst::FlowSuccess::Ok)
            })
            .build(),
    );
    pipeline
        .set_state(gst::State::Playing)
        .context("start the PyroWave pipeline")?;
    Ok(pipeline)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_feed_downloads_gpu_memory_and_refuses_what_it_cannot() {
        gst::init().unwrap();
        let caps = |f: Option<&str>| {
            let b = gst::Caps::builder("video/x-raw").field("format", "NV12");
            match f {
                Some(f) => b.features([f]).build(),
                None => b.build(),
            }
        };
        assert_eq!(download_for(&caps(None)).unwrap(), None);
        assert_eq!(
            download_for(&caps(Some("memory:VulkanImage"))).unwrap(),
            Some("vulkandownload")
        );
        assert_eq!(
            download_for(&caps(Some("memory:CUDAMemory"))).unwrap(),
            Some("cudadownload")
        );
        assert!(download_for(&caps(Some("memory:DMABuf"))).is_err());
    }

    /// A system-memory feed off a live tee: frames reach the PyroWave pipeline's encoder
    /// as BT.709 centre-sited I420, and a feed nobody reads never holds the encode
    /// pipeline's state change. No GPU: `identity` stands in for `pyrowaveenc`.
    #[test]
    fn frames_cross_from_the_tee_to_the_encoder_converted() {
        gst::init().unwrap();
        let main = gst::Pipeline::new();
        let src = gst::ElementFactory::make("videotestsrc")
            .property("is-live", true)
            .build()
            .unwrap();
        let raw = gst::Caps::builder("video/x-raw")
            .field("format", "NV12")
            .field("width", 64)
            .field("height", 48)
            .field("framerate", gst::Fraction::new(30, 1))
            .build();
        let filter = gst::ElementFactory::make("capsfilter")
            .property("caps", &raw)
            .build()
            .unwrap();
        let tee = gst::ElementFactory::make("tee")
            .property("allow-not-linked", true)
            .build()
            .unwrap();
        let fake = gst::ElementFactory::make("fakesink").build().unwrap();
        main.add_many([&src, &filter, &tee, &fake]).unwrap();
        gst::Element::link_many([&src, &filter, &tee, &fake]).unwrap();
        let feed = add_feed(&main, &tee, &raw).unwrap();
        // The gate closed: nothing reaches the appsink, and PLAYING still completes.
        let open = std::sync::Arc::new(std::sync::atomic::AtomicBool::new(false));
        let gate = open.clone();
        feed.queue
            .static_pad("src")
            .unwrap()
            .add_probe(gst::PadProbeType::BUFFER, move |_, _| {
                if gate.load(std::sync::atomic::Ordering::SeqCst) {
                    gst::PadProbeReturn::Ok
                } else {
                    gst::PadProbeReturn::Drop
                }
            });

        let enc = gst::ElementFactory::make("identity").build().unwrap();
        let (tx, rx) = std::sync::mpsc::channel::<()>();
        let tx = std::sync::Mutex::new(tx);
        let side = start_with(
            &feed,
            &enc,
            move |_| {
                let _ = tx.lock().unwrap().send(());
            },
            |_| {},
        )
        .unwrap();
        main.set_state(gst::State::Playing).unwrap();
        let (change, state, _) = main.state(gst::ClockTime::from_seconds(5));
        assert_eq!(
            (change, state),
            (Ok(gst::StateChangeSuccess::Success), gst::State::Playing)
        );
        assert!(
            rx.recv_timeout(std::time::Duration::from_millis(300))
                .is_err(),
            "gated"
        );

        open.store(true, std::sync::atomic::Ordering::SeqCst);
        rx.recv_timeout(std::time::Duration::from_secs(10))
            .expect("a frame reached the encoder");
        let caps = enc.static_pad("sink").unwrap().current_caps().unwrap();
        let s = caps.structure(0).unwrap();
        assert_eq!(s.get::<&str>("format").unwrap(), "I420");
        assert_eq!(s.get::<&str>("colorimetry").unwrap(), "bt709");
        assert_eq!(s.get::<&str>("chroma-site").unwrap(), "jpeg");
        main.set_state(gst::State::Null).unwrap();
        side.set_state(gst::State::Null).unwrap();
    }
}
