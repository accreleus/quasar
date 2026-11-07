//! The PyroWave encode branch of a granted WebTransport session (codec-pyrowave.md,
//! amendment 55). Invariant: nothing PyroWave does may fail, stall or alter the session's
//! standard stream.
//!
//! - The encode pipeline gains no element: a buffer probe on its queue's src pad hands a
//!   reference of each admitted frame to the PyroWave pipeline's appsrc and always passes the
//!   frame on. Not a tee: a tee merges every branch's allocation answer (one failed query
//!   fails the merge and clears its metas, an empty one intersects them away, and two pads add
//!   a buffer to the minimum), so a branch cannot leave the encoder's negotiation untouched.
//! - At most one compositor frame is held for PyroWave: the probe admits none while one is
//!   between it and the end of its download (the 2-slot ring when HEVC is on).
//! - Download, convert and `pyrowaveenc` run in a pipeline of their own, started off the
//!   encode thread with the encode pipeline's GPU contexts; its errors switch PyroWave off.
//!
//! shortcut: frames cross the CPU (download + convert) before a CPU-input encoder; zero-copy
//! Vulkan input is #1104.

use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::Arc;

use anyhow::{anyhow, Context, Result};
use gstreamer as gst;
use gstreamer::prelude::*;
use gstreamer_app as gst_app;

use crate::session::SessionConfig;

/// A frame in flight this long is taken as lost (a stopped or failed PyroWave pipeline), so
/// the gate cannot stay shut for good.
const IN_FLIGHT_STALE: gst::ClockTime = gst::ClockTime::from_seconds(1);

/// When the frame held for PyroWave was admitted (`gst::get_timestamp`, ns); 0 when none is.
#[derive(Default)]
struct InFlight(AtomicU64);

impl InFlight {
    /// Admit a frame when none is in flight, or the one that is has gone stale.
    fn admit(&self) -> bool {
        let now = gst::get_timestamp().nseconds().max(1);
        let held = self.0.load(Ordering::Acquire);
        (held == 0 || now.saturating_sub(held) > IN_FLIGHT_STALE.nseconds())
            && self
                .0
                .compare_exchange(held, now, Ordering::AcqRel, Ordering::Acquire)
                .is_ok()
    }

    fn clear(&self) {
        self.0.store(0, Ordering::Release);
    }
}

/// Where a granted session's raw frames are taken: the encode queue's src pad.
pub(crate) struct Feed {
    pad: gst::Pad,
    /// The element that brings the raw caps to system memory, built in the PyroWave pipeline.
    download: Option<&'static str>,
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

/// A feed off `pad`, whose frames have `raw` caps. Adds nothing until `start_encoder`.
pub(crate) fn feed_at(pad: gst::Pad, raw: &gst::Caps) -> Result<Feed> {
    Ok(Feed {
        pad,
        download: download_for(raw)?,
    })
}

/// The PyroWave pipeline and the probe that feeds it; dropping it removes the probe, then
/// stops the pipeline.
pub(crate) struct Encoder {
    pub(crate) pipeline: gst::Pipeline,
    pad: gst::Pad,
    probe: Option<gst::PadProbeId>,
}

impl Drop for Encoder {
    fn drop(&mut self) {
        if let Some(probe) = self.probe.take() {
            self.pad.remove_probe(probe);
        }
        let _ = self.pipeline.set_state(gst::State::Null);
    }
}

type OnError = Arc<dyn Fn(String) + Send + Sync>;

/// appsrc → [download] → videoconvert → BT.709 centre-sited I420 → `pyrowaveenc` at `kbps`
/// → fakesink, fed while `open` says so. `on_frame` gets each encoded frame, `on_error` each
/// error.
pub(crate) fn start_encoder(
    cfg: &SessionConfig,
    feed: Feed,
    kbps: u32,
    open: impl Fn() -> bool + Send + Sync + 'static,
    on_frame: impl Fn(&gst::BufferRef) + Send + Sync + 'static,
    on_error: impl Fn(String) + Send + Sync + 'static,
) -> Result<Encoder> {
    let mut enc = gst::ElementFactory::make("pyrowaveenc").property("bitrate", kbps);
    if !cfg.render_node.is_empty() && cfg.render_node != "software" {
        enc = enc.property("device", cfg.render_node.as_str());
    }
    let enc = enc.build().context("pyrowaveenc not found")?;
    let download = feed
        .download
        .map(|name| {
            gst::ElementFactory::make(name)
                .build()
                .with_context(|| format!("{name} not found"))
        })
        .transpose()?;
    start_with(feed, download, &enc, open, on_frame, on_error)
}

/// Threads for the convert: a small fixed share of the host, never all of it.
fn convert_threads() -> u32 {
    std::thread::available_parallelism()
        .map_or(2, |n| n.get() as u32 / 4)
        .clamp(2, 4)
}

/// The top-level pipeline `pad` is in.
fn pipeline_of(pad: &gst::Pad) -> Option<gst::Element> {
    let mut element = pad.parent_element()?;
    while let Some(parent) = element.parent().and_downcast::<gst::Element>() {
        element = parent;
    }
    Some(element)
}

fn start_with(
    feed: Feed,
    download: Option<gst::Element>,
    enc: &gst::Element,
    open: impl Fn() -> bool + Send + Sync + 'static,
    on_frame: impl Fn(&gst::BufferRef) + Send + Sync + 'static,
    on_error: impl Fn(String) + Send + Sync + 'static,
) -> Result<Encoder> {
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
        convert.set_property("n-threads", convert_threads());
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
    let mut chain: Vec<&gst::Element> = vec![src.upcast_ref()];
    chain.extend(download.as_ref());
    chain.extend([&convert, &filter, enc, &sink]);
    pipeline.add_many(chain.iter().copied())?;
    gst::Element::link_many(chain.iter().copied())?;

    // The compositor frame is released once its download is done (or, in system memory,
    // once converted): the next one may be admitted.
    let in_flight = Arc::new(InFlight::default());
    let released = download.as_ref().unwrap_or(&convert);
    let cleared = in_flight.clone();
    released
        .static_pad("src")
        .context("no src pad to release frames at")?
        .add_probe(gst::PadProbeType::BUFFER, move |_, _| {
            cleared.clear();
            gst::PadProbeReturn::Ok
        });
    enc.static_pad("src")
        .context("the encoder has no src pad")?
        .add_probe(gst::PadProbeType::BUFFER, move |_, info| {
            if let Some(buffer) = info.buffer() {
                on_frame(buffer);
            }
            gst::PadProbeReturn::Ok
        });
    let on_error: OnError = Arc::new(on_error);
    let bus_error = on_error.clone();
    pipeline
        .bus()
        .context("a pipeline has a bus")?
        .set_sync_handler(move |_, msg| {
            if let gst::MessageView::Error(err) = msg.view() {
                bus_error(format!(
                    "{}: {}",
                    err.error(),
                    err.debug().unwrap_or_default()
                ));
            }
            gst::BusSyncReply::Drop
        });

    let starting = Arc::new(AtomicBool::new(false));
    let started = Arc::new(AtomicBool::new(false));
    let weak = pipeline.downgrade();
    let weak_src: glib::WeakRef<gst_app::AppSrc> = src.downgrade();
    // Runs on the encode pipeline's streaming thread: never blocks, never drops, never fails.
    let probe = feed
        .pad
        .add_probe(gst::PadProbeType::BUFFER, move |pad, info| {
            if !open() {
                return gst::PadProbeReturn::Ok;
            }
            if !started.load(Ordering::Acquire) {
                if !starting.swap(true, Ordering::AcqRel) {
                    start_off_thread(pad, &weak, &started, &on_error);
                }
                return gst::PadProbeReturn::Ok;
            }
            let (Some(buffer), Some(src)) = (info.buffer(), weak_src.upgrade()) else {
                return gst::PadProbeReturn::Ok;
            };
            if !in_flight.admit() {
                return gst::PadProbeReturn::Ok;
            }
            let mut sample = gst::Sample::builder().buffer(buffer);
            let caps = pad.current_caps();
            if let Some(caps) = &caps {
                sample = sample.caps(caps);
            }
            // A push the PyroWave pipeline refuses (stopped, failed) is its own business.
            if src.push_sample(&sample.build()).is_err() {
                in_flight.clear();
            }
            gst::PadProbeReturn::Ok
        })
        .context("could not watch the encode queue")?;
    Ok(Encoder {
        pipeline,
        pad: feed.pad,
        probe: Some(probe),
    })
}

/// Start the PyroWave pipeline on a thread of its own: a state change and a device open must
/// not hold the encode thread.
fn start_off_thread(
    pad: &gst::Pad,
    weak: &glib::WeakRef<gst::Pipeline>,
    started: &Arc<AtomicBool>,
    on_error: &OnError,
) {
    let (Some(pipeline), encode) = (weak.upgrade(), pipeline_of(pad)) else {
        return;
    };
    let (started, on_error) = (started.clone(), on_error.clone());
    let fail = on_error.clone();
    let spawned = std::thread::Builder::new()
        .name("quasar-pw-start".into())
        .spawn(move || {
            // The encode pipeline's GPU contexts are set on it before READY; a download must
            // use that device, not make its own.
            for context in encode.map(|e| e.contexts()).unwrap_or_default() {
                pipeline.set_context(&context);
            }
            match pipeline.set_state(gst::State::Playing) {
                Ok(_) => started.store(true, Ordering::Release),
                Err(e) => on_error(format!("the PyroWave pipeline did not start: {e}")),
            }
        });
    if let Err(e) = spawned {
        fail(format!("no thread to start the PyroWave pipeline: {e}"));
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::AtomicUsize;
    use std::sync::Mutex;
    use std::time::{Duration, Instant};

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

    #[test]
    fn convert_threads_are_a_small_share() {
        assert!((2..=4).contains(&convert_threads()));
    }

    fn raw() -> gst::Caps {
        gst::Caps::builder("video/x-raw")
            .field("format", "NV12")
            .field("width", 64)
            .field("height", 48)
            .field("framerate", gst::Fraction::new(60, 1))
            .build()
    }

    /// A live source → queue → `standard`, like the encode pipeline, and the feed off the
    /// queue's src pad.
    fn encode_pipeline(standard: &[&gst::Element]) -> (gst::Pipeline, Feed) {
        gst::init().unwrap();
        let main = gst::Pipeline::new();
        let src = gst::ElementFactory::make("videotestsrc")
            .property("is-live", true)
            .build()
            .unwrap();
        let filter = gst::ElementFactory::make("capsfilter")
            .property("caps", raw())
            .build()
            .unwrap();
        let queue = gst::ElementFactory::make("queue").build().unwrap();
        main.add_many([&src, &filter, &queue]).unwrap();
        main.add_many(standard.iter().copied()).unwrap();
        let mut chain = vec![&src, &filter, &queue];
        chain.extend(standard.iter().copied());
        gst::Element::link_many(chain).unwrap();
        let feed = feed_at(queue.static_pad("src").unwrap(), &raw()).unwrap();
        (main, feed)
    }

    fn counting_sink(count: &Arc<AtomicUsize>) -> gst::Element {
        gst::init().unwrap();
        let sink = gst::ElementFactory::make("fakesink")
            .property("sync", false)
            .build()
            .unwrap();
        let seen = count.clone();
        sink.static_pad("sink")
            .unwrap()
            .add_probe(gst::PadProbeType::BUFFER, move |_, _| {
                seen.fetch_add(1, Ordering::SeqCst);
                gst::PadProbeReturn::Ok
            });
        sink
    }

    fn errors_on(bus: &gst::Bus) -> Vec<String> {
        std::iter::from_fn(|| bus.pop_filtered(&[gst::MessageType::Error]))
            .map(|m| format!("{m:?}"))
            .collect()
    }

    fn wait_until(what: &str, cond: impl Fn() -> bool) {
        let deadline = Instant::now() + Duration::from_secs(10);
        while !cond() {
            assert!(Instant::now() < deadline, "timed out: {what}");
            std::thread::sleep(Duration::from_millis(20));
        }
    }

    /// A frame reaches the encoder as BT.709 centre-sited I420, and only while the gate is
    /// open; the PyroWave pipeline starts with the first one. No GPU: `identity` stands in for
    /// `pyrowaveenc`.
    #[test]
    fn frames_cross_to_the_encoder_converted_and_only_while_the_gate_is_open() {
        let n = Arc::new(AtomicUsize::new(0));
        let (main, feed) = encode_pipeline(&[&counting_sink(&n)]);
        let open = Arc::new(AtomicBool::new(false));
        let gate = open.clone();
        let enc = gst::ElementFactory::make("identity").build().unwrap();
        let (tx, rx) = std::sync::mpsc::channel::<()>();
        let tx = Mutex::new(tx);
        let side = start_with(
            feed,
            None,
            &enc,
            move || gate.load(Ordering::SeqCst),
            move |_| {
                let _ = tx.lock().unwrap().send(());
            },
            |_| {},
        )
        .unwrap();
        main.set_state(gst::State::Playing).unwrap();
        wait_until("standard frames", || n.load(Ordering::SeqCst) > 5);
        assert!(rx.try_recv().is_err(), "gated");
        assert_eq!(
            side.pipeline.current_state(),
            gst::State::Null,
            "not started before a frame"
        );

        open.store(true, Ordering::SeqCst);
        rx.recv_timeout(Duration::from_secs(10))
            .expect("a frame reached the encoder");
        let caps = enc.static_pad("sink").unwrap().current_caps().unwrap();
        let s = caps.structure(0).unwrap();
        assert_eq!(s.get::<&str>("format").unwrap(), "I420");
        assert_eq!(s.get::<&str>("colorimetry").unwrap(), "bt709");
        assert_eq!(s.get::<&str>("chroma-site").unwrap(), "jpeg");
        drop(side);
        main.set_state(gst::State::Null).unwrap();
    }

    /// Seat A's MUST: a failure in the feed's download (here an element that errors after two
    /// buffers) reaches only the PyroWave pipeline. The encode pipeline's bus stays clean and
    /// its standard H.264 stream keeps flowing.
    #[test]
    fn a_feed_error_leaves_the_standard_stream_running() {
        gst::init().unwrap();
        let standard = Arc::new(AtomicUsize::new(0));
        let convert = gst::ElementFactory::make("videoconvert").build().unwrap();
        let enc = gst::ElementFactory::make("openh264enc").build().unwrap();
        let parse = gst::ElementFactory::make("h264parse").build().unwrap();
        let (main, feed) = encode_pipeline(&[&convert, &enc, &parse, &counting_sink(&standard)]);
        let failing = gst::ElementFactory::make("identity")
            .property("error-after", 2i32)
            .build()
            .unwrap();
        let errored = Arc::new(AtomicBool::new(false));
        let seen = errored.clone();
        let stand_in = gst::ElementFactory::make("identity").build().unwrap();
        let side = start_with(
            feed,
            Some(failing),
            &stand_in,
            || true,
            |_| {},
            move |_| seen.store(true, Ordering::SeqCst),
        )
        .unwrap();
        main.set_state(gst::State::Playing).unwrap();
        wait_until("the failing download failed", || {
            errored.load(Ordering::SeqCst)
        });
        let at_error = standard.load(Ordering::SeqCst);
        std::thread::sleep(Duration::from_millis(500));
        let after = standard.load(Ordering::SeqCst);
        assert!(
            after >= at_error + 10,
            "the standard stream stalled after the feed failed ({at_error} -> {after})"
        );
        assert_eq!(
            errors_on(&main.bus().unwrap()),
            Vec::<String>::new(),
            "the encode bus saw the feed's error"
        );
        drop(side);
        main.set_state(gst::State::Null).unwrap();
    }

    /// The ring: never two frames held for PyroWave. A slow download (40 ms a frame at 60 fps)
    /// makes the probe admit no second frame while one is held.
    #[test]
    fn one_frame_at_most_is_held_until_its_download_is_done() {
        let standard = Arc::new(AtomicUsize::new(0));
        let (main, feed) = encode_pipeline(&[&counting_sink(&standard)]);
        let queue_src = feed.pad.clone();
        let (admitted, left, worst) = (
            Arc::new(AtomicUsize::new(0)),
            Arc::new(AtomicUsize::new(0)),
            Arc::new(AtomicUsize::new(0)),
        );
        let slow = gst::ElementFactory::make("identity")
            .property("sleep-time", 40_000u32)
            .build()
            .unwrap();
        // Added before `start_with`'s own probe on this pad, so a frame is counted out before
        // the next can be admitted.
        let l = left.clone();
        slow.static_pad("src")
            .unwrap()
            .add_probe(gst::PadProbeType::BUFFER, move |_, _| {
                l.fetch_add(1, Ordering::SeqCst);
                gst::PadProbeReturn::Ok
            });
        let stand_in = gst::ElementFactory::make("identity").build().unwrap();
        let side = start_with(feed, Some(slow), &stand_in, || true, |_| {}, |_| {}).unwrap();
        // After the feed's probe: a frame it admitted is referenced by the PyroWave pipeline
        // too (it sleeps 40 ms in the download, so it is still held here).
        let (a, l, w) = (admitted.clone(), left.clone(), worst.clone());
        queue_src.add_probe(gst::PadProbeType::BUFFER, move |_, info| {
            if info.buffer().is_some_and(|b| !b.is_writable()) {
                let held = a.fetch_add(1, Ordering::SeqCst) + 1 - l.load(Ordering::SeqCst);
                w.fetch_max(held, Ordering::SeqCst);
            }
            gst::PadProbeReturn::Ok
        });
        main.set_state(gst::State::Playing).unwrap();
        std::thread::sleep(Duration::from_millis(1500));
        drop(side);
        main.set_state(gst::State::Null).unwrap();
        assert!(
            left.load(Ordering::SeqCst) >= 5,
            "frames went through the download"
        );
        assert_eq!(
            worst.load(Ordering::SeqCst),
            1,
            "two frames were held at once"
        );
        assert!(
            standard.load(Ordering::SeqCst) > admitted.load(Ordering::SeqCst) + 10,
            "the standard stream ran at its own rate; PyroWave admitted fewer"
        );
    }

    /// The feed changes nothing the standard branch negotiates: the encode pipeline has the
    /// same elements, and the queue's allocation and caps answers are the standard branch's.
    #[test]
    fn the_feed_leaves_the_encode_pipelines_graph_and_negotiation_alone() {
        gst::init().unwrap();
        // The standard branch answers allocation with a video meta and a 3-buffer minimum.
        let answering = || {
            let sink = gst::ElementFactory::make("fakesink").build().unwrap();
            sink.static_pad("sink").unwrap().add_probe(
                gst::PadProbeType::QUERY_DOWNSTREAM,
                |_, info| {
                    let Some(gst::PadProbeData::Query(q)) = info.data.as_mut() else {
                        return gst::PadProbeReturn::Ok;
                    };
                    if let gst::QueryViewMut::Allocation(a) = q.view_mut() {
                        a.add_allocation_meta::<gstreamer_video::VideoMeta>(None);
                        a.add_allocation_pool(None::<&gst::BufferPool>, 4608, 3, 0);
                        return gst::PadProbeReturn::Handled;
                    }
                    gst::PadProbeReturn::Ok
                },
            );
            sink
        };
        let ask = |main: &gst::Pipeline| {
            let _ = main.set_state(gst::State::Paused);
            let queue = main
                .children()
                .into_iter()
                .find(|e| e.factory().is_some_and(|f| f.name() == "queue"))
                .unwrap();
            let pad = queue.static_pad("src").unwrap();
            let mut q = gst::query::Allocation::new(Some(&raw()), true);
            assert!(pad.peer_query(&mut q), "allocation answered");
            let pools: Vec<u32> = q.allocation_pools().map(|(_, _, min, _)| min).collect();
            let answer = (
                main.children().len(),
                q.allocation_metas().len(),
                pools,
                pad.peer_query_caps(None),
            );
            main.set_state(gst::State::Null).unwrap();
            answer
        };
        let (plain, _unused) = encode_pipeline(&[&answering()]);
        let without = ask(&plain);
        let (main, feed) = encode_pipeline(&[&answering()]);
        let side = start_with(
            feed,
            None,
            &gst::ElementFactory::make("identity").build().unwrap(),
            || true,
            |_| {},
            |_| {},
        )
        .unwrap();
        let with = ask(&main);
        drop(side);
        assert_eq!((without.1, &without.2), (1, &vec![3]));
        assert_eq!(with, without, "the feed changed the encode pipeline");
    }
}
