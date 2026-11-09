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
//!   anywhere in the PyroWave pipeline, up to the encoder's output (the 2-slot ring when HEVC
//!   is on), and a client's detach flushes what that pipeline holds.
//! - `pyrowaveenc` runs in a pipeline of its own, started off the encode thread with the
//!   encode pipeline's GPU contexts; its errors switch PyroWave off.
//! - Zero copy (#1104): when the frames are BT.709 NV12 Vulkan images (the compositor
//!   converts with that matrix for a PyroWave grant, `caps::source_caps`), `pyrowaveenc`
//!   takes them as they are and encodes on their own device.
//! - Otherwise (a compositor that predates the grant, CUDA or system memory) frames are
//!   downloaded and converted on the CPU first. `vulkandownload` adopts only the instance
//!   through `set_context` and asks its neighbours for the device, so the appsrc answers that
//!   query with the encode pipeline's device, and the download's resolved device is checked
//!   against it before any frame is copied.

use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, Mutex};

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

    /// Clear, and return when the frame was admitted (0 when none was).
    fn take(&self) -> u64 {
        self.0.swap(0, Ordering::AcqRel)
    }
}

/// The most the encode thread waits for PyroWave to release a frame. Over it PyroWave is
/// switched off: a GPU that slow is not one to stream PyroWave from.
const HANDOFF_MAX: std::time::Duration = std::time::Duration::from_millis(200);
/// The same for the first frame, which also opens PyroWave on the frames' device and makes the
/// encoder (about 230 ms on an RTX 5090). The client is already on PyroWave by then, so the
/// standard stream it pauses once is not the one on screen.
const HANDOFF_FIRST_MAX: std::time::Duration = std::time::Duration::from_secs(2);

/// The ordered hand-off of a Vulkan frame between PyroWave and the standard encoder, which
/// read the same image and each move its layout (#1104 review). The encode thread holds the
/// frame until PyroWave releases it, after PyroWave's GPU work is waited and the image is back
/// in its tracked layout, so the two never touch the image at once.
#[derive(Default)]
struct Handoff {
    held: Mutex<bool>,
    released: std::sync::Condvar,
}

impl Handoff {
    fn hold(&self) {
        *self.held.lock().unwrap_or_else(|e| e.into_inner()) = true;
    }

    fn release(&self) {
        *self.held.lock().unwrap_or_else(|e| e.into_inner()) = false;
        self.released.notify_all();
    }

    /// Wait for the release; false when it did not come within `max` (the hold is dropped).
    fn wait(&self, max: std::time::Duration) -> bool {
        let held = self.held.lock().unwrap_or_else(|e| e.into_inner());
        let (mut held, result) = self
            .released
            .wait_timeout_while(held, max, |h| *h)
            .unwrap_or_else(|e| e.into_inner());
        *held = false;
        !result.timed_out()
    }
}

/// How often the encode summary is logged.
const STATS_EVERY: std::time::Duration = std::time::Duration::from_secs(10);

/// Admission-to-encoded times of one window: frames, the window, the times in ns.
struct EncodeStats {
    since: std::time::Instant,
    times: Vec<u64>,
}

impl EncodeStats {
    /// Add one frame's time; every [`STATS_EVERY`] log the window and start a new one.
    fn add(&mut self, ns: u64) {
        self.times.push(ns);
        let span = self.since.elapsed();
        if span < STATS_EVERY {
            return;
        }
        self.times.sort_unstable();
        let at = |q: f64| self.times[((self.times.len() - 1) as f64 * q) as usize] as f64 / 1e6;
        tracing::info!(
            token = "pyrowave-encode-stats",
            frames = self.times.len(),
            fps = format!("{:.1}", self.times.len() as f64 / span.as_secs_f64()),
            p50_ms = format!("{:.2}", at(0.5)),
            p95_ms = format!("{:.2}", at(0.95)),
            max_ms = format!("{:.2}", at(1.0)),
            "PyroWave frames, admission to encoded"
        );
        self.times.clear();
        self.since = std::time::Instant::now();
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
    /// Set under its lock with the stop; the start thread reads it under the same lock, so a
    /// start cannot land after the stop.
    stopped: Arc<Mutex<bool>>,
    /// The appsrc pad answering context queries, and its probe.
    context_probe: (gst::Pad, Option<gst::PadProbeId>),
    handoff: Arc<Handoff>,
}

impl Drop for Encoder {
    fn drop(&mut self) {
        if let Some(probe) = self.probe.take() {
            self.pad.remove_probe(probe);
        }
        if let Some(probe) = self.context_probe.1.take() {
            self.context_probe.0.remove_probe(probe);
        }
        let mut stopped = self.stopped.lock().unwrap_or_else(|e| e.into_inner());
        *stopped = true;
        let _ = self.pipeline.set_state(gst::State::Null);
        // Only once stopped: an encode in flight may still read the held frame.
        self.handoff.release();
    }
}

type OnError = Arc<dyn Fn(String) + Send + Sync>;

/// The context query the encode pipeline asks across the interpipe for the colorimetry the
/// source negotiated: the encode pipeline's own caps are the interpipesrc's, which say none.
pub(crate) const SOURCE_COLORIMETRY: &str = "quasar.source-colorimetry";

/// Answer [`SOURCE_COLORIMETRY`] queries reaching `pad` (the source's last src pad) with its
/// negotiated colorimetry.
pub(crate) fn answer_colorimetry_queries(pad: &gst::Pad) {
    pad.add_probe(gst::PadProbeType::QUERY_UPSTREAM, |pad, info| {
        let Some(gst::PadProbeData::Query(q)) = info.data.as_mut() else {
            return gst::PadProbeReturn::Ok;
        };
        let gst::QueryViewMut::Context(q) = q.view_mut() else {
            return gst::PadProbeReturn::Ok;
        };
        if q.context_type() != SOURCE_COLORIMETRY {
            return gst::PadProbeReturn::Ok;
        }
        let colorimetry = pad
            .current_caps()
            .and_then(|c| c.structure(0)?.get::<String>("colorimetry").ok());
        let mut context = gst::Context::new(SOURCE_COLORIMETRY, false);
        if let Some(c) = colorimetry {
            context
                .get_mut()
                .unwrap()
                .structure_mut()
                .set("colorimetry", c);
        }
        q.set_context(&context);
        gst::PadProbeReturn::Handled
    });
}

/// The colorimetry of the source `pad`'s frames come from, when it says.
fn source_colorimetry(pad: &gst::Pad) -> Option<String> {
    let mut q = gst::query::Context::new(SOURCE_COLORIMETRY);
    if !pad.query(&mut q) {
        return None;
    }
    q.context()?.structure().get::<String>("colorimetry").ok()
}

/// Fill the colorimetry of caps events leaving `pad` (the encode pipeline's interpipesrc) from
/// the source it listens to: its own caps cannot carry it (an interpipesrc with fixed caps
/// refuses the source's), and the standard encoder writes it into its VUI (H.265).
pub(crate) fn tag_caps_with_source_colorimetry(pad: &gst::Pad) {
    pad.add_probe(gst::PadProbeType::EVENT_DOWNSTREAM, |pad, info| {
        let Some(gst::PadProbeData::Event(event)) = &info.data else {
            return gst::PadProbeReturn::Ok;
        };
        let gst::EventView::Caps(c) = event.view() else {
            return gst::PadProbeReturn::Ok;
        };
        let caps = c.caps_owned();
        if caps.structure(0).is_none_or(|s| s.has_field("colorimetry")) {
            return gst::PadProbeReturn::Ok;
        }
        if let Some(colorimetry) = source_colorimetry(pad) {
            let tagged = with_colorimetry(caps, Some(colorimetry));
            info.data = Some(gst::PadProbeData::Event(gst::event::Caps::new(&tagged)));
        }
        gst::PadProbeReturn::Ok
    });
}

/// `caps` with the source's colorimetry, when it said one.
fn with_colorimetry(caps: gst::Caps, colorimetry: Option<String>) -> gst::Caps {
    match colorimetry {
        Some(c) => {
            let mut caps = caps;
            caps.make_mut().set("colorimetry", c);
            caps
        }
        None => caps,
    }
}

/// The frames `pyrowaveenc` can take with no copy: NV12 Vulkan images it accepts as they are
/// (BT.709, centre-sited). The colorimetry must be stated: untagged NV12 reads as BT.709 at
/// HD sizes, but an untagged compositor converts with BT.601.
fn zero_copy(caps: Option<&gst::Caps>) -> bool {
    caps.is_some_and(|caps| {
        caps.features(0)
            .is_some_and(|f| f.contains("memory:VulkanImage"))
            && caps.structure(0).is_some_and(|s| {
                s.get::<&str>("format") == Ok("NV12") && s.has_field("colorimetry")
            })
            && gstpyrowave::check_input(caps).is_ok()
    })
}

/// appsrc → `pyrowaveenc` at `kbps` → fakesink when the frames are [`zero_copy`], else
/// appsrc → [download] → videoconvert → BT.709 centre-sited I420 → `pyrowaveenc`; fed while
/// `open` says so. Which, is decided on the first frame: this runs at pipeline build, before
/// the caps are known. `on_frame` gets each encoded frame, `on_error` each error.
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
    // The CPU path's elements, added and linked by the start thread only if needed.
    let mut cpu: Vec<gst::Element> = download.iter().cloned().collect();
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
    cpu.extend([convert, filter]);
    let sink = gst::ElementFactory::make("fakesink")
        .property("sync", false)
        .property("async", false)
        .build()?;
    pipeline.add_many([src.upcast_ref(), enc, &sink])?;
    enc.link(&sink)?;

    // The next frame is admitted once this one is encoded: until then it, or its copy, is
    // still in this pipeline, and admitting another would leave it waiting in appsrc on a
    // compositor buffer. A frame that never comes out goes stale (IN_FLIGHT_STALE).
    let in_flight = Arc::new(InFlight::default());
    let cleared = in_flight.clone();
    let handoff = Arc::new(Handoff::default());
    // The frame has left its Vulkan image once the download has copied it out (CPU path) or
    // the encoder has output it (zero copy): either way the standard encoder may read it now.
    if let Some(pad) = download.as_ref().and_then(|d| d.static_pad("src")) {
        let released = handoff.clone();
        pad.add_probe(gst::PadProbeType::BUFFER, move |_, _| {
            released.release();
            gst::PadProbeReturn::Ok
        });
    }
    let encoded = handoff.clone();
    enc.static_pad("src")
        .context("the encoder has no src pad")?
        .add_probe(gst::PadProbeType::BUFFER, {
            let stats = Mutex::new(EncodeStats {
                since: std::time::Instant::now(),
                times: Vec::new(),
            });
            move |_, info| {
                encoded.release();
                let admitted = cleared.take();
                if admitted > 0 {
                    let now = gst::get_timestamp().nseconds();
                    stats
                        .lock()
                        .unwrap_or_else(|e| e.into_inner())
                        .add(now.saturating_sub(admitted));
                }
                if let Some(buffer) = info.buffer() {
                    on_frame(buffer);
                }
                gst::PadProbeReturn::Ok
            }
        });
    let on_error: OnError = Arc::new(on_error);
    let bus_error = on_error.clone();
    pipeline
        .bus()
        .context("a pipeline has a bus")?
        .set_sync_handler(move |_, msg| {
            // The held frame is not released here: the encode thread's wait times out and stops
            // this pipeline first (this is its streaming thread, which cannot stop it).
            if let gst::MessageView::Error(err) = msg.view() {
                bus_error(format!(
                    "{}: {}",
                    err.error(),
                    err.debug().unwrap_or_default()
                ));
            }
            gst::BusSyncReply::Drop
        });

    // The encode pipeline's GPU contexts, copied when the pipeline starts: what the appsrc
    // answers a context query with (`vulkandownload` takes its device only from a query).
    let contexts: Arc<Mutex<Vec<gst::Context>>> = Arc::default();
    let shared = contexts.clone();
    let context_pad = src.static_pad("src").context("appsrc has no src pad")?;
    let context_probe = context_pad
        .add_probe(gst::PadProbeType::QUERY_UPSTREAM, move |_, info| {
            let Some(gst::PadProbeData::Query(q)) = info.data.as_mut() else {
                return gst::PadProbeReturn::Ok;
            };
            let gst::QueryViewMut::Context(q) = q.view_mut() else {
                return gst::PadProbeReturn::Ok;
            };
            let shared = shared.lock().unwrap_or_else(|e| e.into_inner());
            match shared.iter().find(|c| c.context_type() == q.context_type()) {
                Some(context) => {
                    q.set_context(context);
                    gst::PadProbeReturn::Handled
                }
                None => gst::PadProbeReturn::Ok,
            }
        })
        .context("could not answer context queries")?;
    let download = download.map(|d| d.downgrade());
    let weak_enc = enc.downgrade();
    // The frames' caps with the source's colorimetry, decided once by the start thread. Safe to
    // keep: the interpipesrc's caps are fixed for the encode pipeline's life (no
    // renegotiation), a swap's compositor is built from the same request, and an adoption
    // that re-streams builds a new encode pipeline, and with it a new PyroWave branch.
    let sample_caps: Arc<Mutex<Option<gst::Caps>>> = Arc::default();
    let starting = Arc::new(AtomicBool::new(false));
    let started = Arc::new(AtomicBool::new(false));
    let was_open = AtomicBool::new(false);
    let gave_up = AtomicBool::new(false);
    let first_released = AtomicBool::new(false);
    let held = handoff.clone();
    let held_error = on_error.clone();
    let stopped = Arc::new(Mutex::new(false));
    let start_stopped = stopped.clone();
    let weak = pipeline.downgrade();
    let weak_src: glib::WeakRef<gst_app::AppSrc> = src.downgrade();
    // Runs on the encode pipeline's streaming thread: never blocks, never drops, never fails.
    let probe = feed
        .pad
        .add_probe(gst::PadProbeType::BUFFER, move |pad, info| {
            if !open() || gave_up.load(Ordering::Acquire) {
                if was_open.swap(false, Ordering::AcqRel) && started.load(Ordering::Acquire) {
                    flush_off_thread(&weak_src, &in_flight);
                }
                return gst::PadProbeReturn::Ok;
            }
            was_open.store(true, Ordering::Release);
            if !started.load(Ordering::Acquire) {
                if !starting.swap(true, Ordering::AcqRel) {
                    let start = Start {
                        pipeline: weak.clone(),
                        src: weak_src.clone(),
                        enc: weak_enc.clone(),
                        cpu: cpu.clone(),
                        download: download.clone(),
                        contexts: contexts.clone(),
                        started: started.clone(),
                        stopped: start_stopped.clone(),
                        on_error: on_error.clone(),
                        sample_caps: sample_caps.clone(),
                    };
                    start_off_thread(pad, start);
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
            let decided = sample_caps
                .lock()
                .unwrap_or_else(|e| e.into_inner())
                .clone();
            let caps = decided.or_else(|| pad.current_caps());
            if let Some(caps) = &caps {
                sample = sample.caps(caps);
            }
            let vulkan = caps
                .as_ref()
                .and_then(|c| c.features(0))
                .is_some_and(|f| f.contains("memory:VulkanImage"));
            if vulkan {
                held.hold();
            }
            // A push the PyroWave pipeline refuses (stopped, failed) is its own business.
            if src.push_sample(&sample.build()).is_err() {
                in_flight.clear();
                held.release();
                return gst::PadProbeReturn::Ok;
            }
            // The standard encoder reads this image next: not before PyroWave is done with it.
            if vulkan {
                let max = match first_released.load(Ordering::Acquire) {
                    true => HANDOFF_MAX,
                    false => HANDOFF_FIRST_MAX,
                };
                if held.wait(max) {
                    first_released.store(true, Ordering::Release);
                } else if !gave_up.swap(true, Ordering::AcqRel) {
                    // Stopped before the frame goes on: NULL waits out the encode still on it.
                    if let Some(p) = weak.upgrade() {
                        let _ = p.set_state(gst::State::Null);
                    }
                    held_error(format!(
                        "PyroWave held a frame over {max:?}; the standard stream goes on"
                    ));
                }
            }
            gst::PadProbeReturn::Ok
        })
        .context("could not watch the encode queue")?;
    Ok(Encoder {
        pipeline,
        pad: feed.pad,
        probe: Some(probe),
        stopped,
        context_probe: (context_pad, Some(context_probe)),
        handoff,
    })
}

/// The client left: flush what the PyroWave pipeline holds, so a wedged one does not keep a
/// compositor frame until the session ends. Off the encode thread: a flush-stop takes the
/// stream locks downstream.
fn flush_off_thread(weak_src: &glib::WeakRef<gst_app::AppSrc>, in_flight: &Arc<InFlight>) {
    let (weak_src, in_flight) = (weak_src.clone(), in_flight.clone());
    let _ = std::thread::Builder::new()
        .name("quasar-pw-flush".into())
        .spawn(move || {
            if let Some(src) = weak_src.upgrade() {
                src.send_event(gst::event::FlushStart::new());
                src.send_event(gst::event::FlushStop::new(true));
            }
            in_flight.clear();
        });
}

const VULKAN_DEVICE: &str = "gst.vulkan.device";

/// The `GstVulkanDevice` a device context holds: one owns one VkDevice, so equal pointers
/// mean one device (as `runner::assert_vulkan_encoder_device` checks the encoder).
fn vulkan_device(context: &gst::Context) -> Option<usize> {
    context
        .structure()
        .get::<glib::Object>(VULKAN_DEVICE)
        .ok()
        .map(|o| o.as_ptr() as usize)
}

/// A `vulkandownload` must copy with the encode pipeline's device: anything else copies a
/// VkImage another device owns. Checked after READY→PAUSED, when it has resolved its device.
fn check_download_device(download: &gst::Element, contexts: &[gst::Context]) -> Result<(), String> {
    let expected = contexts
        .iter()
        .find(|c| c.context_type() == VULKAN_DEVICE)
        .and_then(vulkan_device);
    let vulkan = download
        .factory()
        .is_some_and(|f| f.name() == "vulkandownload");
    let Some(expected) = expected else {
        return match vulkan {
            true => Err("no shared Vulkan device to download with".into()),
            false => Ok(()),
        };
    };
    let mut query = gst::query::Context::new(VULKAN_DEVICE);
    let pad = download
        .static_pad("src")
        .ok_or("the download has no src pad")?;
    let actual = (pad.query(&mut query))
        .then(|| query.context_owned())
        .flatten()
        .as_ref()
        .and_then(vulkan_device);
    match actual {
        Some(actual) if actual == expected => Ok(()),
        Some(actual) => Err(format!(
            "the download resolved GstVulkanDevice {actual:#x}, the encode pipeline's is {expected:#x}"
        )),
        None => Err("the download did not answer its device".into()),
    }
}

/// What the start thread needs.
struct Start {
    pipeline: glib::WeakRef<gst::Pipeline>,
    src: glib::WeakRef<gst_app::AppSrc>,
    enc: glib::WeakRef<gst::Element>,
    /// [download →] convert → caps, linked between `src` and `enc` unless the frames are
    /// [`zero_copy`].
    cpu: Vec<gst::Element>,
    sample_caps: Arc<Mutex<Option<gst::Caps>>>,
    download: Option<glib::WeakRef<gst::Element>>,
    contexts: Arc<Mutex<Vec<gst::Context>>>,
    started: Arc<AtomicBool>,
    stopped: Arc<Mutex<bool>>,
    on_error: OnError,
}

/// Start the PyroWave pipeline on a thread of its own: a state change and a device open must
/// not hold the encode thread. PAUSED first, so the download's device is checked before any
/// frame reaches it.
fn start_off_thread(pad: &gst::Pad, start: Start) {
    let (Some(pipeline), encode) = (start.pipeline.upgrade(), pipeline_of(pad)) else {
        return;
    };
    let pad = pad.clone();
    let fail = start.on_error.clone();
    let spawned = std::thread::Builder::new()
        .name("quasar-pw-start".into())
        .spawn(move || {
            let copied = encode.map(|e| e.contexts()).unwrap_or_default();
            for context in &copied {
                pipeline.set_context(context);
            }
            *start.contexts.lock().unwrap_or_else(|e| e.into_inner()) = copied.clone();
            let stopped = start.stopped.lock().unwrap_or_else(|e| e.into_inner());
            if *stopped {
                return;
            }
            let caps = pad
                .current_caps()
                .map(|c| with_colorimetry(c, source_colorimetry(&pad)));
            *start.sample_caps.lock().unwrap_or_else(|e| e.into_inner()) = caps.clone();
            // QUASAR_PYROWAVE_CPU_INPUT=1 keeps the CPU path: an escape hatch, and an A/B.
            let zero =
                zero_copy(caps.as_ref()) && !crate::session::env_bool("QUASAR_PYROWAVE_CPU_INPUT");
            let checked = link_chain(&pipeline, &start, zero)
                .and_then(|()| {
                    pipeline
                        .set_state(gst::State::Paused)
                        .map_err(|e| format!("the PyroWave pipeline did not start: {e}"))
                })
                .and_then(
                    |_| match start.download.as_ref().and_then(|d| d.upgrade()) {
                        Some(download) if !zero => check_download_device(&download, &copied),
                        _ => Ok(()),
                    },
                )
                .and_then(|()| {
                    pipeline
                        .set_state(gst::State::Playing)
                        .map_err(|e| format!("the PyroWave pipeline did not start: {e}"))
                });
            match checked {
                Ok(_) => {
                    tracing::info!(
                        zero_copy = zero,
                        caps = ?caps,
                        "PyroWave pipeline started"
                    );
                    start.started.store(true, Ordering::Release)
                }
                Err(e) => {
                    let _ = pipeline.set_state(gst::State::Null);
                    (start.on_error)(e);
                }
            }
        });
    if let Err(e) = spawned {
        fail(format!("no thread to start the PyroWave pipeline: {e}"));
    }
}

/// appsrc → `pyrowaveenc` for [`zero_copy`] frames, else appsrc → the CPU path → encoder.
fn link_chain(pipeline: &gst::Pipeline, start: &Start, zero: bool) -> Result<(), String> {
    let (Some(src), Some(enc)) = (start.src.upgrade(), start.enc.upgrade()) else {
        return Err("the PyroWave pipeline is gone".into());
    };
    let mut chain = vec![src.upcast::<gst::Element>()];
    if !zero {
        pipeline
            .add_many(&start.cpu)
            .map_err(|e| format!("the CPU path: {e}"))?;
        chain.extend(start.cpu.iter().cloned());
    }
    chain.push(enc);
    gst::Element::link_many(&chain).map_err(|e| format!("cannot link the PyroWave pipeline: {e}"))
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::AtomicUsize;
    use std::time::{Duration, Instant};

    #[test]
    fn the_feed_learns_the_colorimetry_the_source_negotiated() {
        gst::init().unwrap();
        let p = gst::parse::launch(
            "videotestsrc is-live=true ! capsfilter name=tail \
             caps=video/x-raw,format=NV12,width=64,height=48,colorimetry=bt601 ! \
             capsfilter caps=video/x-raw,format=NV12,width=64,height=48 ! queue name=q ! \
             fakesink",
        )
        .unwrap()
        .downcast::<gst::Pipeline>()
        .unwrap();
        let bin = p.upcast_ref::<gst::Bin>();
        let tail = bin.by_name("tail").unwrap().static_pad("src").unwrap();
        let feed = bin.by_name("q").unwrap().static_pad("src").unwrap();
        assert_eq!(source_colorimetry(&feed), None, "nobody answers yet");
        answer_colorimetry_queries(&tail);
        p.set_state(gst::State::Playing).unwrap();
        wait_until("negotiated", || feed.current_caps().is_some());
        let said = source_colorimetry(&feed);
        p.set_state(gst::State::Null).unwrap();
        assert_eq!(said.as_deref(), Some("bt601"));
        let caps = with_colorimetry(feed.current_caps().unwrap_or_else(raw), said);
        assert!(!zero_copy(Some(&caps)), "BT.601 takes the CPU path");
    }

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

    /// The interpipe shape: the encode side's source has fixed, untagged caps (as an
    /// interpipesrc with `caps` set) and its context queries reach the source pipeline.
    #[test]
    fn the_encoder_input_carries_the_colorimetry_the_source_negotiated() {
        gst::init().unwrap();
        let source = gst::parse::launch(
            "videotestsrc is-live=true ! capsfilter name=tail \
             caps=video/x-raw,format=NV12,width=64,height=48,colorimetry=bt709 ! fakesink",
        )
        .unwrap()
        .downcast::<gst::Pipeline>()
        .unwrap();
        let tail = source.by_name("tail").unwrap().static_pad("src").unwrap();
        answer_colorimetry_queries(&tail);
        source.set_state(gst::State::Playing).unwrap();
        wait_until("source negotiated", || tail.current_caps().is_some());

        let encode = gst::Pipeline::new();
        let src = gst_app::AppSrc::builder()
            .caps(&raw())
            .format(gst::Format::Time)
            .build();
        let sink = gst::ElementFactory::make("fakesink").build().unwrap();
        encode.add_many([src.upcast_ref(), &sink]).unwrap();
        src.link(&sink).unwrap();
        let src_pad = src.static_pad("src").unwrap();
        src_pad.add_probe(gst::PadProbeType::QUERY_UPSTREAM, move |_, info| {
            let Some(gst::PadProbeData::Query(q)) = info.data.as_mut() else {
                return gst::PadProbeReturn::Ok;
            };
            match tail.query(q) {
                true => gst::PadProbeReturn::Handled,
                false => gst::PadProbeReturn::Ok,
            }
        });
        tag_caps_with_source_colorimetry(&src_pad);
        encode.set_state(gst::State::Playing).unwrap();
        src.push_buffer(gst::Buffer::with_size(64 * 48 * 3 / 2).unwrap())
            .unwrap();
        let sink_pad = sink.static_pad("sink").unwrap();
        wait_until("encode side negotiated", || {
            sink_pad.current_caps().is_some()
        });
        let caps = sink_pad.current_caps().unwrap();
        encode.set_state(gst::State::Null).unwrap();
        source.set_state(gst::State::Null).unwrap();
        let s = caps.structure(0).unwrap();
        assert_eq!(s.get::<&str>("colorimetry").ok(), Some("bt709"), "{caps}");
    }

    /// A frame PyroWave overholds goes to the standard encoder only once PyroWave has been
    /// stopped (its encode of that frame finished), not at the timeout; the stream goes on.
    #[test]
    fn an_overheld_frame_goes_on_only_after_pyrowave_has_stopped() {
        gst::init().unwrap();
        let caps = gst::Caps::builder("video/x-raw")
            .features(["memory:VulkanImage"])
            .field("format", "NV12")
            .field("width", 64)
            .field("height", 48)
            .field("framerate", gst::Fraction::new(60, 1))
            .field("colorimetry", "bt709")
            .build();
        let main = gst::Pipeline::new();
        let src = gst_app::AppSrc::builder()
            .caps(&caps)
            .format(gst::Format::Time)
            .build();
        let queue = gst::ElementFactory::make("queue").build().unwrap();
        let sink = gst::ElementFactory::make("fakesink")
            .property("sync", false)
            .build()
            .unwrap();
        main.add_many([src.upcast_ref(), &queue, &sink]).unwrap();
        gst::Element::link_many([src.upcast_ref(), &queue, &sink]).unwrap();
        let arrivals = Arc::new(Mutex::new(Vec::<Instant>::new()));
        let seen = arrivals.clone();
        sink.static_pad("sink")
            .unwrap()
            .add_probe(gst::PadProbeType::BUFFER, move |_, _| {
                seen.lock().unwrap().push(Instant::now());
                gst::PadProbeReturn::Ok
            });
        let feed = feed_at(queue.static_pad("src").unwrap(), &caps).unwrap();
        // The PyroWave stand-in turns slow (500 ms a frame) after its first frame.
        let enc = gst::ElementFactory::make("identity").build().unwrap();
        let slow = enc.clone();
        let errors = Arc::new(Mutex::new(Vec::<String>::new()));
        let errs = errors.clone();
        let side = start_with(
            feed,
            None,
            &enc,
            || true,
            move |_| slow.set_property("sleep-time", 500_000u32),
            move |e| errs.lock().unwrap().push(e),
        )
        .unwrap();
        main.set_state(gst::State::Playing).unwrap();
        for i in 0..20u64 {
            let mut buf = gst::Buffer::with_size(64 * 48 * 3 / 2).unwrap();
            buf.get_mut()
                .unwrap()
                .set_pts(gst::ClockTime::from_mseconds(i * 50));
            src.push_buffer(buf).unwrap();
            std::thread::sleep(Duration::from_millis(50));
        }
        wait_until("every frame on the standard side", || {
            arrivals.lock().unwrap().len() == 20
        });
        let state = side.pipeline.current_state();
        drop(side);
        main.set_state(gst::State::Null).unwrap();
        let errors = errors.lock().unwrap();
        assert_eq!(errors.len(), 1, "{errors:?}");
        assert!(errors[0].contains("held a frame"), "{}", errors[0]);
        assert_eq!(state, gst::State::Null, "PyroWave stopped");
        let arrivals = arrivals.lock().unwrap();
        let gaps: Vec<Duration> = arrivals.windows(2).map(|w| w[1] - w[0]).collect();
        let longest = *gaps.iter().max().unwrap();
        assert!(
            longest >= Duration::from_millis(450),
            "the overheld frame went on at {longest:?}, before PyroWave finished it"
        );
        // After the give-up the standard stream runs at the push pace, with no waits.
        let after = gaps
            .iter()
            .skip_while(|g| **g < Duration::from_millis(450))
            .skip(1);
        assert!(after.clone().count() > 5);
        assert!(
            after.clone().all(|g| *g < Duration::from_millis(200)),
            "{gaps:?}"
        );
    }

    #[test]
    fn the_encode_thread_waits_for_pyrowave_to_release_the_frame() {
        let h = Arc::new(Handoff::default());
        h.hold();
        assert!(
            !h.wait(Duration::from_millis(20)),
            "no release: it gives up"
        );
        h.hold();
        let releaser = h.clone();
        let t = std::thread::spawn(move || {
            std::thread::sleep(Duration::from_millis(30));
            releaser.release();
        });
        let start = Instant::now();
        assert!(h.wait(Duration::from_secs(5)), "released");
        assert!(
            start.elapsed() >= Duration::from_millis(25),
            "it waited for the release"
        );
        t.join().unwrap();
        assert!(h.wait(Duration::from_millis(1)), "nothing held: no wait");
    }

    #[test]
    fn only_bt709_nv12_vulkan_images_skip_the_cpu() {
        gst::init().unwrap();
        let caps = |features: &str, format: &str, colorimetry: Option<&str>| {
            let mut b = gst::Caps::builder("video/x-raw")
                .features([features])
                .field("format", format)
                .field("width", 1920)
                .field("height", 1080)
                .field("framerate", gst::Fraction::new(60, 1));
            if let Some(c) = colorimetry {
                b = b.field("colorimetry", c);
            }
            b.build()
        };
        let vk = "memory:VulkanImage";
        assert!(zero_copy(Some(&caps(vk, "NV12", Some("bt709")))));
        // The compositor's BT.601 default, and HDR P010, take the CPU path.
        assert!(!zero_copy(Some(&caps(vk, "NV12", Some("bt601")))));
        assert!(
            !zero_copy(Some(&caps(vk, "NV12", None))),
            "untagged is BT.601 here"
        );
        assert!(!zero_copy(Some(&caps(vk, "P010_10LE", Some("bt709")))));
        assert!(!zero_copy(Some(&caps(
            "memory:SystemMemory",
            "NV12",
            Some("bt709")
        ))));
        assert!(!zero_copy(Some(&caps(
            "memory:CUDAMemory",
            "NV12",
            Some("bt709")
        ))));
        assert!(!zero_copy(None), "not negotiated yet");
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

    /// The ring: how many frames were held for PyroWave at once, with a 40 ms stage (60 fps
    /// input) at the download or at the encoder. Returns (worst, encoded, admitted, standard).
    fn held_at_once(slow_encoder: bool) -> (usize, usize, usize, usize) {
        let standard = Arc::new(AtomicUsize::new(0));
        let (main, feed) = encode_pipeline(&[&counting_sink(&standard)]);
        let queue_src = feed.pad.clone();
        let (admitted, encoded, worst) = (
            Arc::new(AtomicUsize::new(0)),
            Arc::new(AtomicUsize::new(0)),
            Arc::new(AtomicUsize::new(0)),
        );
        let identity = |sleep_us: u32| {
            gst::ElementFactory::make("identity")
                .property("sleep-time", sleep_us)
                .build()
                .unwrap()
        };
        let (download, enc) = if slow_encoder {
            (None, identity(40_000))
        } else {
            (Some(identity(40_000)), identity(0))
        };
        // Added before `start_with`'s own probe on this pad, so a frame is counted out before
        // the next can be admitted.
        let e = encoded.clone();
        enc.static_pad("src")
            .unwrap()
            .add_probe(gst::PadProbeType::BUFFER, move |_, _| {
                e.fetch_add(1, Ordering::SeqCst);
                gst::PadProbeReturn::Ok
            });
        let side = start_with(feed, download, &enc, || true, |_| {}, |_| {}).unwrap();
        // After the feed's probe: a frame it admitted is referenced by the PyroWave pipeline
        // too (a 40 ms stage is ahead of it, so it is still held here).
        let (a, e, w) = (admitted.clone(), encoded.clone(), worst.clone());
        queue_src.add_probe(gst::PadProbeType::BUFFER, move |_, info| {
            if info.buffer().is_some_and(|b| !b.is_writable()) {
                let held =
                    (a.fetch_add(1, Ordering::SeqCst) + 1).saturating_sub(e.load(Ordering::SeqCst));
                w.fetch_max(held, Ordering::SeqCst);
            }
            gst::PadProbeReturn::Ok
        });
        main.set_state(gst::State::Playing).unwrap();
        std::thread::sleep(Duration::from_millis(1500));
        drop(side);
        main.set_state(gst::State::Null).unwrap();
        let load = |n: &Arc<AtomicUsize>| n.load(Ordering::SeqCst);
        (
            load(&worst),
            load(&encoded),
            load(&admitted),
            load(&standard),
        )
    }

    #[test]
    fn one_frame_at_most_is_held_behind_a_slow_download() {
        let (worst, encoded, admitted, standard) = held_at_once(false);
        assert!(encoded >= 5, "frames went through");
        assert_eq!(worst, 1, "two frames were held at once");
        assert!(
            standard > admitted + 10,
            "the standard stream ran at its own rate"
        );
    }

    /// Seat A round 2: a slow encoder must not leave a second frame waiting in appsrc.
    #[test]
    fn one_frame_at_most_is_held_behind_a_slow_encoder() {
        let (worst, encoded, admitted, standard) = held_at_once(true);
        assert!(encoded >= 5, "frames went through");
        assert_eq!(worst, 1, "two frames were held at once");
        assert!(
            standard > admitted + 10,
            "the standard stream ran at its own rate"
        );
    }

    /// Frame memory that says when it is freed.
    struct Tracked(Vec<u8>, Arc<AtomicBool>);

    impl AsRef<[u8]> for Tracked {
        fn as_ref(&self) -> &[u8] {
            &self.0
        }
    }

    impl Drop for Tracked {
        fn drop(&mut self) {
            self.1.store(true, Ordering::SeqCst);
        }
    }

    /// Seat A round 2: a wedged PyroWave pipeline (here every push out of its appsrc blocks)
    /// gives the frame it holds back when the client leaves.
    #[test]
    fn a_client_leaving_flushes_the_frame_a_wedged_pyrowave_pipeline_holds() {
        gst::init().unwrap();
        let main = gst::Pipeline::new();
        let src = gst_app::AppSrc::builder()
            .caps(&raw())
            .is_live(true)
            .format(gst::Format::Time)
            .build();
        let queue = gst::ElementFactory::make("queue").build().unwrap();
        let sink = gst::ElementFactory::make("fakesink")
            .property("sync", false)
            .property("async", false)
            .build()
            .unwrap();
        main.add_many([src.upcast_ref(), &queue, &sink]).unwrap();
        gst::Element::link_many([src.upcast_ref(), &queue, &sink]).unwrap();
        let feed = feed_at(queue.static_pad("src").unwrap(), &raw()).unwrap();
        let open = Arc::new(AtomicBool::new(true));
        let gate = open.clone();
        let enc = gst::ElementFactory::make("identity").build().unwrap();
        let side = start_with(
            feed,
            None,
            &enc,
            move || gate.load(Ordering::SeqCst),
            |_| {},
            |_| {},
        )
        .unwrap();
        let side_src = side
            .pipeline
            .children()
            .into_iter()
            .find(|e| e.is::<gst_app::AppSrc>())
            .unwrap();
        side_src.static_pad("src").unwrap().add_probe(
            gst::PadProbeType::BLOCK | gst::PadProbeType::BUFFER,
            |_, _| gst::PadProbeReturn::Ok,
        );
        main.set_state(gst::State::Playing).unwrap();
        let push = |i: u64, freed: &Arc<AtomicBool>| {
            let mut b =
                gst::Buffer::from_slice(Tracked(vec![16u8; 64 * 48 * 3 / 2], freed.clone()));
            b.get_mut()
                .unwrap()
                .set_pts(gst::ClockTime::from_mseconds(16 * i));
            src.push_buffer(b).unwrap();
        };
        let ignored = Arc::new(AtomicBool::new(false));
        push(0, &ignored); // starts the PyroWave pipeline
        wait_until("the PyroWave pipeline started", || {
            side.pipeline.current_state() == gst::State::Playing
        });
        let held = Arc::new(AtomicBool::new(false));
        push(1, &held);
        std::thread::sleep(Duration::from_millis(300));
        assert!(
            !held.load(Ordering::SeqCst),
            "the wedged pipeline holds the frame"
        );

        open.store(false, Ordering::SeqCst);
        push(2, &ignored);
        wait_until("the held frame was given back", || {
            held.load(Ordering::SeqCst)
        });
        drop(side);
        main.set_state(gst::State::Null).unwrap();
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

    /// A stand-in device context: what matters is the object it carries.
    fn device_context(device: &gst::Bin) -> gst::Context {
        let mut context = gst::Context::new(VULKAN_DEVICE, true);
        context
            .get_mut()
            .unwrap()
            .structure_mut()
            .set(VULKAN_DEVICE, device);
        context
    }

    /// Starts a PyroWave pipeline off an encode pipeline that holds `shared` as its device
    /// context, with `download` in the download's place. Returns it once started or failed.
    fn start_with_device(
        shared: &gst::Context,
        download: &gst::Element,
    ) -> (gst::Pipeline, Encoder, Arc<Mutex<Option<String>>>) {
        let n = Arc::new(AtomicUsize::new(0));
        let (main, feed) = encode_pipeline(&[&counting_sink(&n)]);
        main.set_context(shared);
        let failed: Arc<Mutex<Option<String>>> = Arc::default();
        let f = failed.clone();
        let side = start_with(
            feed,
            Some(download.clone()),
            &gst::ElementFactory::make("identity").build().unwrap(),
            || true,
            |_| {},
            move |e| *f.lock().unwrap() = Some(e),
        )
        .unwrap();
        main.set_state(gst::State::Playing).unwrap();
        wait_until("started or refused", || {
            side.pipeline.current_state() == gst::State::Playing || failed.lock().unwrap().is_some()
        });
        (main, side, failed)
    }

    /// Seat B's MUST: the download asks its neighbours for the device; the appsrc answers with
    /// the encode pipeline's, and nothing else would.
    #[test]
    fn the_appsrc_answers_the_downloads_device_query_with_the_shared_device() {
        gst::init().unwrap();
        let device = gst::Bin::new();
        let shared = device_context(&device);
        let download = gst::ElementFactory::make("identity").build().unwrap();
        let (main, mut side, failed) = start_with_device(&shared, &download);
        assert_eq!(
            *failed.lock().unwrap(),
            None,
            "the shared device passed the check"
        );

        let ask = || {
            let mut q = gst::query::Context::new(VULKAN_DEVICE);
            download
                .static_pad("sink")
                .unwrap()
                .peer_query(&mut q)
                .then(|| q.context_owned())
                .flatten()
                .as_ref()
                .and_then(vulkan_device)
        };
        assert_eq!(
            ask(),
            Some(device.as_ptr() as usize),
            "the shared GstVulkanDevice"
        );
        let (pad, probe) = (
            side.context_probe.0.clone(),
            side.context_probe.1.take().unwrap(),
        );
        pad.remove_probe(probe);
        assert_eq!(ask(), None, "with the probe removed, nobody answers");
        drop(side);
        main.set_state(gst::State::Null).unwrap();
    }

    /// A download that resolved another device is refused before any frame reaches it.
    #[test]
    fn a_download_on_another_device_is_refused_before_any_frame() {
        gst::init().unwrap();
        let (ours, theirs) = (gst::Bin::new(), gst::Bin::new());
        let shared = device_context(&ours);
        let other = device_context(&theirs);
        let download = gst::ElementFactory::make("identity").build().unwrap();
        let frames = Arc::new(AtomicUsize::new(0));
        let seen = frames.clone();
        let src = download.static_pad("src").unwrap();
        src.add_probe(gst::PadProbeType::QUERY_UPSTREAM, move |_, info| {
            let Some(gst::PadProbeData::Query(q)) = info.data.as_mut() else {
                return gst::PadProbeReturn::Ok;
            };
            match q.view_mut() {
                gst::QueryViewMut::Context(q) if q.context_type() == VULKAN_DEVICE => {
                    q.set_context(&other);
                    gst::PadProbeReturn::Handled
                }
                _ => gst::PadProbeReturn::Ok,
            }
        });
        src.add_probe(gst::PadProbeType::BUFFER, move |_, _| {
            seen.fetch_add(1, Ordering::SeqCst);
            gst::PadProbeReturn::Ok
        });
        let (main, side, failed) = start_with_device(&shared, &download);
        std::thread::sleep(Duration::from_millis(300));
        let why = failed.lock().unwrap().clone();
        assert!(
            why.as_deref()
                .is_some_and(|w| w.contains("GstVulkanDevice")),
            "refused: {why:?}"
        );
        assert_ne!(side.pipeline.current_state(), gst::State::Playing);
        assert_eq!(
            frames.load(Ordering::SeqCst),
            0,
            "no frame reached the download"
        );
        drop(side);
        main.set_state(gst::State::Null).unwrap();
    }
}
