use std::path::Path;
use std::str::FromStr;
use std::sync::{Arc, LazyLock, Mutex};

use gstreamer as gst;
use gstreamer::glib;
use gstreamer::prelude::*;
use gstreamer::subclass::prelude::*;
use gstreamer_video as gst_video;
use gstreamer_video::prelude::*;
use gstreamer_video::subclass::prelude::*;

use crate::ffi::{Device, Encoder, Picture, BITSTREAM_SLACK};

static CAT: LazyLock<gst::DebugCategory> = LazyLock::new(|| {
    gst::DebugCategory::new(
        "pyrowaveenc",
        gst::DebugColorFlags::empty(),
        Some("PyroWave encoder"),
    )
});

/// codec-pyrowave.md §Rate: the 1080p default target.
const DEFAULT_KBPS: u32 = 200_000;

struct Settings {
    bitrate_kbps: u32,
    device: Option<String>,
}

struct State {
    device: Arc<Device>,
    encoder: Option<Encoder>,
    info: Option<gst_video::VideoInfo>,
}

pub struct PyroWaveEnc {
    settings: Mutex<Settings>,
    state: Mutex<Option<State>>,
}

impl Default for PyroWaveEnc {
    fn default() -> Self {
        PyroWaveEnc {
            settings: Mutex::new(Settings {
                bitrate_kbps: DEFAULT_KBPS,
                device: None,
            }),
            state: Mutex::new(None),
        }
    }
}

#[glib::object_subclass]
impl ObjectSubclass for PyroWaveEnc {
    const NAME: &'static str = "GstPyroWaveEnc";
    type Type = crate::PyroWaveEnc;
    type ParentType = gst_video::VideoEncoder;
}

impl ObjectImpl for PyroWaveEnc {
    fn properties() -> &'static [glib::ParamSpec] {
        static PROPS: LazyLock<Vec<glib::ParamSpec>> = LazyLock::new(|| {
            vec![
                glib::ParamSpecUInt::builder("bitrate")
                    .nick("Bitrate")
                    .blurb(
                        "Target in kbit/s; each frame's byte budget is this over the frame \
                         rate (codec-pyrowave.md §Rate)",
                    )
                    .minimum(1)
                    .maximum(10_000_000)
                    .default_value(DEFAULT_KBPS)
                    .mutable_playing()
                    .build(),
                glib::ParamSpecString::builder("device")
                    .nick("Device")
                    .blurb(
                        "DRM render node of the GPU to encode on (/dev/dri/renderD*, or a \
                         by-path link); unset picks PyroWave's default device",
                    )
                    .mutable_ready()
                    .build(),
            ]
        });
        PROPS.as_ref()
    }

    fn set_property(&self, _id: usize, value: &glib::Value, pspec: &glib::ParamSpec) {
        let mut s = self.settings.lock().unwrap();
        match pspec.name() {
            "bitrate" => s.bitrate_kbps = value.get().unwrap(),
            "device" => s.device = value.get().unwrap(),
            _ => unreachable!(),
        }
    }

    fn property(&self, _id: usize, pspec: &glib::ParamSpec) -> glib::Value {
        let s = self.settings.lock().unwrap();
        match pspec.name() {
            "bitrate" => s.bitrate_kbps.to_value(),
            "device" => s.device.to_value(),
            _ => unreachable!(),
        }
    }
}

impl GstObjectImpl for PyroWaveEnc {}

impl ElementImpl for PyroWaveEnc {
    fn metadata() -> Option<&'static gst::subclass::ElementMetadata> {
        static META: LazyLock<gst::subclass::ElementMetadata> = LazyLock::new(|| {
            gst::subclass::ElementMetadata::new(
                "PyroWave encoder",
                "Codec/Encoder/Video",
                "Intra-only wavelet video encoder in Vulkan compute (PyroWave bitstream v1)",
                "Quasar <https://github.com/accreleus/quasar>",
            )
        });
        Some(&*META)
    }

    fn pad_templates() -> &'static [gst::PadTemplate] {
        static TEMPLATES: LazyLock<Vec<gst::PadTemplate>> = LazyLock::new(|| {
            // BT.709 only, limited or full range: PyroWave has no BT.601 matrix
            // (codec-pyrowave.md §hello_ack), so upstream converts to this.
            let sink = gst::Caps::from_str(
                "video/x-raw, format=(string){ I420, NV12 }, \
                 width=(int)[ 2, 16384 ], height=(int)[ 2, 16384 ], \
                 framerate=(fraction)[ 0/1, 2147483647/1 ], \
                 colorimetry=(string){ bt709, 1:3:5:1 }",
            )
            .unwrap();
            let src = gst::Caps::from_str(
                "video/x-pyrowave, chroma-format=(string)4:2:0, \
                 width=(int)[ 2, 16384 ], height=(int)[ 2, 16384 ], \
                 framerate=(fraction)[ 0/1, 2147483647/1 ]",
            )
            .unwrap();
            vec![
                gst::PadTemplate::new(
                    "sink",
                    gst::PadDirection::Sink,
                    gst::PadPresence::Always,
                    &sink,
                )
                .unwrap(),
                gst::PadTemplate::new(
                    "src",
                    gst::PadDirection::Src,
                    gst::PadPresence::Always,
                    &src,
                )
                .unwrap(),
            ]
        });
        TEMPLATES.as_ref()
    }
}

impl VideoEncoderImpl for PyroWaveEnc {
    fn start(&self) -> Result<(), gst::ErrorMessage> {
        let node = self.settings.lock().unwrap().device.clone();
        let device = Device::open(node.as_deref().map(Path::new))
            .map_err(|e| gst::error_msg!(gst::LibraryError::Init, ["PyroWave device: {e}"]))?;
        *self.state.lock().unwrap() = Some(State {
            device,
            encoder: None,
            info: None,
        });
        Ok(())
    }

    fn stop(&self) -> Result<(), gst::ErrorMessage> {
        // Encoder before device: Encoder holds an Arc, so the order is by construction.
        *self.state.lock().unwrap() = None;
        Ok(())
    }

    fn set_format(
        &self,
        input: &gst_video::VideoCodecState<'static, gst_video::video_codec_state::Readable>,
    ) -> Result<(), gst::LoggableError> {
        let info = input.info().clone();
        let (w, h) = (info.width(), info.height());
        if w % 2 != 0 || h % 2 != 0 {
            return Err(gst::loggable_error!(
                CAT,
                "4:2:0 needs an even size, got {w}x{h}"
            ));
        }
        if info.colorimetry().matrix() != gst_video::VideoColorMatrix::Bt709 {
            return Err(gst::loggable_error!(CAT, "PyroWave input must be BT.709"));
        }
        let mut guard = self.state.lock().unwrap();
        let state = guard
            .as_mut()
            .ok_or_else(|| gst::loggable_error!(CAT, "caps before start"))?;
        state.encoder = None;
        let encoder = Encoder::new(state.device.clone(), w, h)
            .map_err(|e| gst::loggable_error!(CAT, "{e}"))?;
        state.encoder = Some(encoder);
        state.info = Some(info);
        drop(guard);

        let caps = gst::Caps::builder("video/x-pyrowave")
            .field("chroma-format", "4:2:0")
            .build();
        let obj = self.obj();
        let output = obj
            .set_output_state(caps, Some(input))
            .map_err(|_| gst::loggable_error!(CAT, "cannot set the output state"))?;
        obj.negotiate(output)
            .map_err(|_| gst::loggable_error!(CAT, "cannot negotiate"))?;
        Ok(())
    }

    fn handle_frame(
        &self,
        mut frame: gst_video::VideoCodecFrame,
    ) -> Result<gst::FlowSuccess, gst::FlowError> {
        let kbps = self.settings.lock().unwrap().bitrate_kbps;
        let mut guard = self.state.lock().unwrap();
        let State { encoder, info, .. } = guard.as_mut().ok_or(gst::FlowError::Flushing)?;
        let (Some(encoder), Some(info)) = (encoder.as_mut(), info.as_ref()) else {
            return Err(gst::FlowError::NotNegotiated);
        };
        let fps = info.fps();
        let budget = crate::frame_budget(kbps, fps.numer(), fps.denom());
        let full_range = info.colorimetry().range() == gst_video::VideoColorRange::Range0_255;

        let input = frame.input_buffer().ok_or(gst::FlowError::Error)?;
        let vframe = gst_video::VideoFrameRef::from_buffer_ref_readable(input, info)
            .map_err(|_| gst::FlowError::Error)?;
        let nv12 = info.format() == gst_video::VideoFormat::Nv12;
        let plane = |i: u32| {
            if i < vframe.n_planes() {
                vframe.plane_data(i).unwrap_or(&[])
            } else {
                &[]
            }
        };
        let strides = vframe.plane_stride();
        let stride = |i: usize| strides.get(i).map_or(0, |&s| s as usize);
        let pic = Picture {
            width: info.width(),
            height: info.height(),
            nv12,
            planes: [plane(0), plane(1), plane(2)],
            strides: [stride(0), stride(1), stride(2)],
        };

        let mut out =
            gst::Buffer::with_size(budget + BITSTREAM_SLACK).map_err(|_| gst::FlowError::Error)?;
        let encoded = {
            let out = out.get_mut().unwrap();
            let mut map = out.map_writable().map_err(|_| gst::FlowError::Error)?;
            encoder
                .encode(&pic, budget, &mut map)
                .and_then(|len| crate::write_sof_colour(&mut map[..len], full_range).map(|_| len))
        };
        drop(vframe);
        drop(guard);
        let len = encoded.map_err(|e| {
            gst::element_imp_error!(self, gst::StreamError::Encode, ["PyroWave encode: {e}"]);
            gst::FlowError::Error
        })?;
        out.get_mut().unwrap().set_size(len);
        frame.set_output_buffer(out);
        // Every frame is intra: the base class then clears DELTA_UNIT.
        frame.set_flags(gst_video::VideoCodecFrameFlags::SYNC_POINT);
        self.obj().finish_frame(frame)
    }
}
