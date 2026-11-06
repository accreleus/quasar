//! The few PyroWave C API calls Quasar uses (`pyrowave/pyrowave.h` at the pin in
//! `docs/third-party-pins.md`), loaded at run time so a build without the library works.

use std::ffi::{c_int, c_void, CStr};
use std::path::Path;
use std::ptr::{self, NonNull};
use std::sync::{Arc, Mutex, OnceLock};

use ash::vk;

const SONAME: &CStr = c"libpyrowave-shared.so.1";
/// The pin's API version. The structs below mirror pyrowave.h by hand, so a library with
/// any other major.minor (a minor may append fields) is refused, not trusted.
const API_MAJOR: u32 = 1;
const API_MINOR: u32 = 0;
const CHROMA_420: c_int = 0;
const FORMAT_NV12: c_int = 0;
const FORMAT_YUV420P: c_int = 1;
const VK_QUEUE_GLOBAL_PRIORITY_MEDIUM: c_int = 256;

#[repr(C)]
struct EncoderCreateInfo {
    device: *mut c_void,
    width: c_int,
    height: c_int,
    chroma: c_int,
}

#[repr(C)]
struct DecoderCreateInfo {
    device: *mut c_void,
    width: c_int,
    height: c_int,
    chroma: c_int,
    fragment_path: bool,
}

#[repr(C)]
struct CpuBuffer {
    data: [*mut c_void; 3],
    row_stride_in_bytes: [usize; 3],
    plane_size_in_bytes: [usize; 3],
    width: c_int,
    height: c_int,
    format: c_int,
}

#[repr(C)]
struct RateControl {
    maximum_bitstream_size: usize,
}

#[repr(C)]
#[derive(Default, Clone, Copy)]
struct Packet {
    offset: usize,
    size: usize,
}

type Raw = *mut c_void;

struct Api {
    get_api_version: unsafe extern "C" fn(*mut u32, *mut u32, *mut u32),
    create_default_device: unsafe extern "C" fn(*mut Raw) -> c_int,
    create_device_by_compat: unsafe extern "C" fn(
        u32,
        u32,
        *const c_void,
        *const c_void,
        *const c_void,
        c_int,
        *mut Raw,
    ) -> c_int,
    device_destroy: unsafe extern "C" fn(Raw),
    encoder_create: unsafe extern "C" fn(*const EncoderCreateInfo, *mut Raw) -> c_int,
    encoder_encode_cpu: unsafe extern "C" fn(Raw, *const CpuBuffer, *const RateControl) -> c_int,
    encoder_compute_num_packets: unsafe extern "C" fn(Raw, usize, *mut usize) -> c_int,
    encoder_packetize:
        unsafe extern "C" fn(Raw, *mut Packet, usize, *mut usize, *mut c_void, usize) -> c_int,
    encoder_destroy: unsafe extern "C" fn(Raw),
    decoder_create: unsafe extern "C" fn(*const DecoderCreateInfo, *mut Raw) -> c_int,
    decoder_push_packet: unsafe extern "C" fn(Raw, *const c_void, usize) -> c_int,
    decoder_decode_is_ready: unsafe extern "C" fn(Raw, bool) -> bool,
    decoder_decode_cpu_buffer_synchronous: unsafe extern "C" fn(Raw, *const CpuBuffer) -> c_int,
    decoder_destroy: unsafe extern "C" fn(Raw),
}

fn load() -> Result<Api, String> {
    // SAFETY: dlopen/dlsym with NUL-terminated names; each symbol is cast to the type
    // pyrowave.h declares for it. The handle is never closed.
    unsafe {
        let lib = libc::dlopen(SONAME.as_ptr(), libc::RTLD_NOW | libc::RTLD_LOCAL);
        if lib.is_null() {
            let why = libc::dlerror();
            let why = if why.is_null() {
                "no reason given".into()
            } else {
                CStr::from_ptr(why).to_string_lossy()
            };
            return Err(format!("cannot load libpyrowave-shared.so.1: {why}"));
        }
        macro_rules! sym {
            ($name:literal) => {{
                let p = libc::dlsym(lib, concat!($name, "\0").as_ptr().cast());
                if p.is_null() {
                    return Err(format!("libpyrowave-shared has no {}", $name));
                }
                std::mem::transmute_copy::<*mut c_void, _>(&p)
            }};
        }
        let api = Api {
            get_api_version: sym!("pyrowave_get_api_version"),
            create_default_device: sym!("pyrowave_create_default_device"),
            create_device_by_compat: sym!("pyrowave_create_device_by_compat"),
            device_destroy: sym!("pyrowave_device_destroy"),
            encoder_create: sym!("pyrowave_encoder_create"),
            encoder_encode_cpu: sym!("pyrowave_encoder_encode_cpu"),
            encoder_compute_num_packets: sym!("pyrowave_encoder_compute_num_packets"),
            encoder_packetize: sym!("pyrowave_encoder_packetize"),
            encoder_destroy: sym!("pyrowave_encoder_destroy"),
            decoder_create: sym!("pyrowave_decoder_create"),
            decoder_push_packet: sym!("pyrowave_decoder_push_packet"),
            decoder_decode_is_ready: sym!("pyrowave_decoder_decode_is_ready"),
            decoder_decode_cpu_buffer_synchronous: sym!(
                "pyrowave_decoder_decode_cpu_buffer_synchronous"
            ),
            decoder_destroy: sym!("pyrowave_decoder_destroy"),
        };
        let (mut major, mut minor, mut patch) = (0, 0, 0);
        (api.get_api_version)(&mut major, &mut minor, &mut patch);
        if (major, minor) != (API_MAJOR, API_MINOR) {
            return Err(format!(
                "libpyrowave-shared API {major}.{minor}.{patch}, this element speaks \
                 {API_MAJOR}.{API_MINOR}"
            ));
        }
        Ok(api)
    }
}

fn api() -> Result<&'static Api, String> {
    // A load failure is kept for the life of the process: the library ships in the image,
    // so it cannot appear later.
    static API: OnceLock<Result<Api, String>> = OnceLock::new();
    API.get_or_init(load).as_ref().map_err(Clone::clone)
}

/// The library is present and speaks this API version. Says nothing about a GPU.
pub fn available() -> Result<(), String> {
    api().map(|_| ())
}

const OUT_OF_HOST_MEMORY: &str = "ERROR_OUT_OF_HOST_MEMORY";
const OUT_OF_DEVICE_MEMORY: &str = "ERROR_OUT_OF_DEVICE_MEMORY";

/// An error this module produced is a resource shortage (host or device memory), not a
/// missing capability: the probe calls it indeterminate.
pub fn is_resource_error(message: &str) -> bool {
    message.contains(OUT_OF_HOST_MEMORY) || message.contains(OUT_OF_DEVICE_MEMORY)
}

fn check(what: &str, result: c_int) -> Result<(), String> {
    // pyrowave_result: negative is an error, 1 is TIMEOUT.
    let name = match result {
        -1 => "ERROR_GENERIC",
        -2 => "ERROR_INVALID_ARGUMENT",
        -3 => OUT_OF_HOST_MEMORY,
        -4 => OUT_OF_DEVICE_MEMORY,
        -5 => "ERROR_NO_VULKAN (no Vulkan device with the features PyroWave needs)",
        -6 => "ERROR_NOT_IMPLEMENTED",
        _ => "",
    };
    if result < 0 {
        Err(format!("{what} failed: {result} {name}"))
    } else {
        Ok(())
    }
}

/// Granite, inside the library, keeps process-wide state: device creation and teardown
/// are serialized across every element and probe in the process.
static DEVICE_LOCK: Mutex<()> = Mutex::new(());

/// A PyroWave Vulkan device: PyroWave's own VkDevice on the chosen GPU.
pub struct Device(NonNull<c_void>);

// SAFETY: create and destroy run under DEVICE_LOCK; between them the handle is only
// passed to encoder/decoder creation, each of which its owner serializes. Nothing more
// is shown about the library's thread safety.
unsafe impl Send for Device {}
unsafe impl Sync for Device {}

impl Device {
    /// The GPU behind `render_node` (`/dev/dri/renderD*` or a link to one), or the
    /// library's default device when `None`.
    pub fn open(render_node: Option<&Path>) -> Result<Arc<Device>, String> {
        let api = api()?;
        let _lock = DEVICE_LOCK.lock().unwrap_or_else(|e| e.into_inner());
        let mut raw = ptr::null_mut();
        match render_node {
            None => check("pyrowave_create_default_device", unsafe {
                (api.create_default_device)(&mut raw)
            })?,
            Some(node) => {
                let uuid = device_uuid(render_minor(node)?)
                    .map_err(|e| format!("{}: {e}", node.display()))?;
                check("pyrowave_create_device_by_compat", unsafe {
                    (api.create_device_by_compat)(
                        0,
                        0,
                        uuid.as_ptr().cast(),
                        ptr::null(),
                        ptr::null(),
                        VK_QUEUE_GLOBAL_PRIORITY_MEDIUM,
                        &mut raw,
                    )
                })
                .map_err(|e| format!("{e} for {}", node.display()))?
            }
        }
        NonNull::new(raw)
            .map(|p| Arc::new(Device(p)))
            .ok_or_else(|| "pyrowave returned no device".into())
    }
}

impl Drop for Device {
    fn drop(&mut self) {
        if let Ok(api) = api() {
            let _lock = DEVICE_LOCK.lock().unwrap_or_else(|e| e.into_inner());
            unsafe { (api.device_destroy)(self.0.as_ptr()) }
        }
    }
}

/// The DRM minor of a render node (a link is followed).
fn render_minor(node: &Path) -> Result<u32, String> {
    use std::os::unix::fs::{FileTypeExt, MetadataExt};
    let meta = std::fs::metadata(node).map_err(|e| format!("{}: {e}", node.display()))?;
    if !meta.file_type().is_char_device() {
        return Err(format!("{} is not a device node", node.display()));
    }
    Ok(libc::minor(meta.rdev()))
}

/// The one `deviceUUID` among `devices` (render minor, UUID) whose render node is `minor`.
fn pick_uuid(devices: &[(Option<u32>, [u8; 16])], minor: u32) -> Result<[u8; 16], String> {
    let mut found = devices.iter().filter(|(m, _)| *m == Some(minor));
    match (found.next(), found.next()) {
        (Some((_, uuid)), None) => Ok(*uuid),
        (None, _) => Err(format!("no Vulkan device has render minor {minor}")),
        (Some(_), Some(_)) => Err(format!("two Vulkan devices claim render minor {minor}")),
    }
}

/// `VkPhysicalDeviceIDProperties::deviceUUID` of the Vulkan device on render minor `minor`,
/// matched through `VK_EXT_physical_device_drm`. Instance-level calls only: never
/// `ash::Device::load` (the #267 loader race, `.claude/rules/gstreamer-gotchas.md`).
fn device_uuid(minor: u32) -> Result<[u8; 16], String> {
    let entry = unsafe { ash::Entry::load() }.map_err(|e| format!("Vulkan loader: {e}"))?;
    let app = vk::ApplicationInfo::default().api_version(vk::API_VERSION_1_1);
    let instance = unsafe {
        entry.create_instance(
            &vk::InstanceCreateInfo::default().application_info(&app),
            None,
        )
    }
    .map_err(|e| format!("vkCreateInstance: {e}"))?;
    let devices = unsafe { instance.enumerate_physical_devices() }
        .map_err(|e| format!("vkEnumeratePhysicalDevices: {e}"));
    let devices = devices.map(|list| {
        list.into_iter()
            .map(|pd| unsafe {
                let has_drm = instance
                    .enumerate_device_extension_properties(pd)
                    .unwrap_or_default()
                    .iter()
                    .any(|e| {
                        e.extension_name_as_c_str() == Ok(ash::ext::physical_device_drm::NAME)
                    });
                let mut id = vk::PhysicalDeviceIDProperties::default();
                let mut drm = vk::PhysicalDeviceDrmPropertiesEXT::default();
                let mut props = vk::PhysicalDeviceProperties2::default().push_next(&mut id);
                if has_drm {
                    props = props.push_next(&mut drm);
                }
                instance.get_physical_device_properties2(pd, &mut props);
                let render =
                    (has_drm && drm.has_render == vk::TRUE).then_some(drm.render_minor as u32);
                (render, id.device_uuid)
            })
            .collect::<Vec<_>>()
    });
    unsafe { instance.destroy_instance(None) };
    pick_uuid(&devices?, minor)
}

/// An 8-bit 4:2:0 picture in CPU memory.
pub struct Picture<'a> {
    pub width: u32,
    pub height: u32,
    /// NV12 (Y, interleaved CbCr) when true, I420 (Y, Cb, Cr) otherwise.
    pub nv12: bool,
    /// Plane data from each plane's first byte; NV12 leaves the third empty.
    pub planes: [&'a [u8]; 3],
    pub strides: [usize; 3],
}

impl Picture<'_> {
    fn cpu_buffer(&self) -> Result<CpuBuffer, String> {
        let chroma_rows = self.height.div_ceil(2) as usize;
        let rows = [self.height as usize, chroma_rows, chroma_rows];
        let (count, row_bytes) = if self.nv12 {
            (
                2,
                [self.width as usize, self.width.div_ceil(2) as usize * 2, 0],
            )
        } else {
            let cw = self.width.div_ceil(2) as usize;
            (3, [self.width as usize, cw, cw])
        };
        let mut buf = CpuBuffer {
            data: [ptr::null_mut(); 3],
            row_stride_in_bytes: [0; 3],
            plane_size_in_bytes: [0; 3],
            width: self.width as c_int,
            height: self.height as c_int,
            format: if self.nv12 {
                FORMAT_NV12
            } else {
                FORMAT_YUV420P
            },
        };
        for i in 0..count {
            // pyrowave.h: stride ≥ the row's bytes, plane size ≥ stride × rows.
            let size = self.strides[i] * rows[i];
            if self.strides[i] < row_bytes[i] || self.planes[i].len() < size {
                return Err(format!(
                    "plane {i}: {} bytes at stride {}, need {size}",
                    self.planes[i].len(),
                    self.strides[i]
                ));
            }
            // The encoder only reads (pyrowave.h: "read-only in encoder").
            buf.data[i] = self.planes[i].as_ptr() as *mut c_void;
            buf.row_stride_in_bytes[i] = self.strides[i];
            buf.plane_size_in_bytes[i] = size;
        }
        Ok(buf)
    }
}

/// Upstream's own slack over the per-frame budget for the bitstream buffer.
pub const BITSTREAM_SLACK: usize = 65536;

pub struct Encoder {
    raw: NonNull<c_void>,
    width: u32,
    height: u32,
    _device: Arc<Device>,
}

// SAFETY: pyrowave encoder entry points are not thread safe; the owner serializes them.
unsafe impl Send for Encoder {}

impl Encoder {
    pub fn new(device: Arc<Device>, width: u32, height: u32) -> Result<Encoder, String> {
        let api = api()?;
        let info = EncoderCreateInfo {
            device: device.0.as_ptr(),
            width: width as c_int,
            height: height as c_int,
            chroma: CHROMA_420,
        };
        let mut raw = ptr::null_mut();
        check("pyrowave_encoder_create", unsafe {
            (api.encoder_create)(&info, &mut raw)
        })?;
        Ok(Encoder {
            raw: NonNull::new(raw).ok_or("pyrowave returned no encoder")?,
            width,
            height,
            _device: device,
        })
    }

    /// Encode one picture within `budget` bytes into `out` (at least `budget +
    /// BITSTREAM_SLACK` long) and return the frame's length: the SOF, then the blocks.
    pub fn encode(
        &mut self,
        pic: &Picture,
        budget: usize,
        out: &mut [u8],
    ) -> Result<usize, String> {
        let api = api()?;
        if (pic.width, pic.height) != (self.width, self.height) {
            return Err(format!(
                "picture {}x{} on a {}x{} encoder",
                pic.width, pic.height, self.width, self.height
            ));
        }
        let buf = pic.cpu_buffer()?;
        let rc = RateControl {
            maximum_bitstream_size: budget,
        };
        let raw = self.raw.as_ptr();
        check("pyrowave_encoder_encode_cpu", unsafe {
            (api.encoder_encode_cpu)(raw, &buf, &rc)
        })?;
        // One packet as large as the buffer: the whole frame, contiguous. Datagram
        // packing (codec-pyrowave.md §Packing) is the transport's.
        let mut count = 0;
        check("pyrowave_encoder_compute_num_packets", unsafe {
            (api.encoder_compute_num_packets)(raw, out.len(), &mut count)
        })?;
        if count != 1 {
            return Err(format!(
                "frame needs {count} packets of {} bytes",
                out.len()
            ));
        }
        let mut packet = Packet::default();
        check("pyrowave_encoder_packetize", unsafe {
            (api.encoder_packetize)(
                raw,
                &mut packet,
                out.len(),
                &mut count,
                out.as_mut_ptr().cast(),
                out.len(),
            )
        })?;
        if count != 1 || packet.offset != 0 || packet.size < 8 || packet.size > out.len() {
            return Err(format!(
                "bad packetization: {count} packets, offset {}, size {}",
                packet.offset, packet.size
            ));
        }
        Ok(packet.size)
    }
}

impl Drop for Encoder {
    fn drop(&mut self) {
        if let Ok(api) = api() {
            unsafe { (api.encoder_destroy)(self.raw.as_ptr()) }
        }
    }
}

/// The reference decoder, for probes and tests.
pub struct Decoder {
    raw: NonNull<c_void>,
    width: u32,
    height: u32,
    _device: Arc<Device>,
}

// SAFETY: as for Encoder.
unsafe impl Send for Decoder {}

impl Decoder {
    pub fn new(device: Arc<Device>, width: u32, height: u32) -> Result<Decoder, String> {
        let api = api()?;
        let info = DecoderCreateInfo {
            device: device.0.as_ptr(),
            width: width as c_int,
            height: height as c_int,
            chroma: CHROMA_420,
            fragment_path: false,
        };
        let mut raw = ptr::null_mut();
        check("pyrowave_decoder_create", unsafe {
            (api.decoder_create)(&info, &mut raw)
        })?;
        Ok(Decoder {
            raw: NonNull::new(raw).ok_or("pyrowave returned no decoder")?,
            width,
            height,
            _device: device,
        })
    }

    /// Decode one whole frame (SOF + blocks) to tightly packed I420.
    pub fn decode_i420(&mut self, frame: &[u8]) -> Result<Vec<u8>, String> {
        let api = api()?;
        let raw = self.raw.as_ptr();
        check("pyrowave_decoder_push_packet", unsafe {
            (api.decoder_push_packet)(raw, frame.as_ptr().cast(), frame.len())
        })?;
        if !unsafe { (api.decoder_decode_is_ready)(raw, false) } {
            return Err("the decoder is not ready after the whole frame".into());
        }
        let (w, h) = (self.width as usize, self.height as usize);
        let (cw, ch) = (w.div_ceil(2), h.div_ceil(2));
        let mut out = vec![0u8; w * h + 2 * cw * ch];
        let (y, c) = out.split_at_mut(w * h);
        let (cb, cr) = c.split_at_mut(cw * ch);
        let buf = CpuBuffer {
            data: [
                y.as_mut_ptr().cast(),
                cb.as_mut_ptr().cast(),
                cr.as_mut_ptr().cast(),
            ],
            row_stride_in_bytes: [w, cw, cw],
            plane_size_in_bytes: [w * h, cw * ch, cw * ch],
            width: w as c_int,
            height: h as c_int,
            format: FORMAT_YUV420P,
        };
        check("pyrowave_decoder_decode_cpu_buffer_synchronous", unsafe {
            (api.decoder_decode_cpu_buffer_synchronous)(raw, &buf)
        })?;
        Ok(out)
    }
}

impl Drop for Decoder {
    fn drop(&mut self) {
        if let Ok(api) = api() {
            unsafe { (api.decoder_destroy)(self.raw.as_ptr()) }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_render_node_picks_exactly_one_device_uuid() {
        let a = [1u8; 16];
        let b = [2u8; 16];
        // Two identical GPUs: same PCI ids, different render minors and UUIDs.
        let devices = [(Some(128), a), (Some(129), b), (None, [3u8; 16])];
        assert_eq!(pick_uuid(&devices, 129), Ok(b));
        assert_eq!(pick_uuid(&devices, 128), Ok(a));
        assert!(pick_uuid(&devices, 130).is_err(), "no such node");
        assert!(
            pick_uuid(&[(Some(128), a), (Some(128), b)], 128).is_err(),
            "ambiguous"
        );
    }

    #[test]
    fn the_render_minor_comes_from_the_device_number() {
        // /dev/null is char device 1:3 on Linux.
        assert_eq!(render_minor(Path::new("/dev/null")), Ok(3));
        assert!(render_minor(Path::new("/")).is_err());
    }

    #[test]
    fn only_memory_shortages_are_resource_errors() {
        assert!(is_resource_error(&check("x", -4).unwrap_err()));
        assert!(is_resource_error(&check("x", -3).unwrap_err()));
        assert!(!is_resource_error(&check("x", -5).unwrap_err()));
        assert!(check("x", 1).is_ok(), "TIMEOUT is not an error");
    }
}
