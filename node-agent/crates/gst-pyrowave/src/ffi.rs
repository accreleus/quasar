//! The few PyroWave C API calls Quasar uses (`pyrowave/pyrowave.h` at the pin in
//! `docs/third-party-pins.md`), loaded at run time so a build without the library works.

use std::collections::HashMap;
use std::ffi::{c_int, c_void, CStr};
use std::path::Path;
use std::ptr::{self, NonNull};
use std::sync::{Arc, Mutex, OnceLock};

use ash::vk;

use crate::gpu::{self, Shared};

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

/// `pyrowave_image_view`.
#[repr(C)]
#[derive(Clone, Copy)]
struct ImageView {
    image: vk::Image,
    width: u32,
    height: u32,
    image_format: vk::Format,
    view_format: vk::Format,
    mip_level: u32,
    layer: u32,
    aspect: vk::ImageAspectFlags,
    swizzle: vk::ComponentSwizzle,
    layout: vk::ImageLayout,
}

/// `pyrowave_gpu_buffers`: Y, Cb, Cr.
#[repr(C)]
struct GpuBuffers {
    planes: [ImageView; 3],
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
    create_device: unsafe extern "C" fn(*const gpu::DeviceCreateInfo, *mut Raw) -> c_int,
    device_set_command_buffer: unsafe extern "C" fn(Raw, vk::CommandBuffer),
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
    encoder_encode_gpu: unsafe extern "C" fn(
        Raw,
        *const c_void,
        *const c_void,
        *const GpuBuffers,
        *const RateControl,
    ) -> c_int,
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
            create_device: sym!("pyrowave_create_device"),
            device_set_command_buffer: sym!("pyrowave_device_set_command_buffer"),
            create_device_by_compat: sym!("pyrowave_create_device_by_compat"),
            device_destroy: sym!("pyrowave_device_destroy"),
            encoder_create: sym!("pyrowave_encoder_create"),
            encoder_encode_cpu: sym!("pyrowave_encoder_encode_cpu"),
            encoder_encode_gpu: sym!("pyrowave_encoder_encode_gpu"),
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

/// Appended to every error this module builds from a memory-shortage code (PyroWave's or
/// Vulkan's). Errors cross the element's bus as text, so the class travels as this tag.
pub const RESOURCE_TAG: &str = "[resource shortage]";

/// An error this module produced is a resource shortage (host or device memory), not a
/// missing capability: the probe calls it indeterminate.
pub fn is_resource_error(message: &str) -> bool {
    message.contains(RESOURCE_TAG)
}

/// A Vulkan call's error, tagged by its code. `ERROR_INITIALIZATION_FAILED` is not a
/// shortage: it is also what a missing or broken driver returns.
fn vk_error(what: &str, result: vk::Result) -> String {
    match result {
        vk::Result::ERROR_OUT_OF_HOST_MEMORY | vk::Result::ERROR_OUT_OF_DEVICE_MEMORY => {
            format!("{what}: {result:?} {RESOURCE_TAG}")
        }
        _ => format!("{what}: {result:?}"),
    }
}

fn check(what: &str, result: c_int) -> Result<(), String> {
    // pyrowave_result: negative is an error, 1 is TIMEOUT.
    let name = match result {
        -1 => "ERROR_GENERIC",
        -2 => "ERROR_INVALID_ARGUMENT",
        -3 => "ERROR_OUT_OF_HOST_MEMORY",
        -4 => "ERROR_OUT_OF_DEVICE_MEMORY",
        -5 => "ERROR_NO_VULKAN (no Vulkan device with the features PyroWave needs)",
        -6 => "ERROR_NOT_IMPLEMENTED",
        _ => "",
    };
    if result == -3 || result == -4 {
        Err(format!("{what} failed: {result} {name} {RESOURCE_TAG}"))
    } else if result < 0 {
        Err(format!("{what} failed: {result} {name}"))
    } else {
        Ok(())
    }
}

/// Granite, inside the library, keeps process-wide state: creating and destroying a
/// device, an encoder or a decoder is serialized across every element and probe in the
/// process.
static DEVICE_LOCK: Mutex<()> = Mutex::new(());

fn device_lock() -> std::sync::MutexGuard<'static, ()> {
    DEVICE_LOCK.lock().unwrap_or_else(|e| e.into_inner())
}

/// A PyroWave Vulkan device: PyroWave's own VkDevice on the chosen GPU, or a borrowed
/// GStreamer one (`shared`).
pub struct Device {
    raw: NonNull<c_void>,
    /// Declared after `raw`: dropped once PyroWave's device is destroyed.
    shared: Option<Shared>,
}

// SAFETY: create and destroy, and those of every encoder and decoder on it, run under
// DEVICE_LOCK; between them the handle is only passed to those creations. Nothing more is
// shown about the library's thread safety.
unsafe impl Send for Device {}
unsafe impl Sync for Device {}

impl Device {
    /// The GPU behind `render_node` (`/dev/dri/renderD*` or a link to one), or the
    /// library's default device when `None`.
    pub fn open(render_node: Option<&Path>) -> Result<Arc<Device>, String> {
        let api = api()?;
        let _lock = device_lock();
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
            .map(|raw| Arc::new(Device { raw, shared: None }))
            .ok_or_else(|| "pyrowave returned no device".into())
    }

    /// PyroWave on the `GstVulkanDevice` `frame`'s images belong to: no instance or device
    /// of its own (#1104). Fails, with the reason, where that device lacks what PyroWave needs.
    pub fn share(frame: &gpu::Frame) -> Result<Arc<Device>, String> {
        let api = api()?;
        let shared = Shared::new(frame.device)?;
        let _lock = device_lock();
        let mut raw = ptr::null_mut();
        check("pyrowave_create_device", unsafe {
            (api.create_device)(shared.device_create_info(), &mut raw)
        })?;
        NonNull::new(raw)
            .map(|raw| {
                Arc::new(Device {
                    raw,
                    shared: Some(shared),
                })
            })
            .ok_or_else(|| "pyrowave returned no device".into())
    }

    /// The `GstVulkanDevice` a shared device borrows, as an identity.
    pub fn shared_key(&self) -> Option<usize> {
        self.shared.as_ref().map(Shared::device_key)
    }
}

impl Drop for Device {
    fn drop(&mut self) {
        if let Ok(api) = api() {
            let _lock = device_lock();
            unsafe { (api.device_destroy)(self.raw.as_ptr()) }
        }
    }
}

const DRM_MAJOR: u32 = 226;

/// The DRM minor of a render node (a link is followed).
fn render_minor(node: &Path) -> Result<u32, String> {
    use std::os::unix::fs::{FileTypeExt, MetadataExt};
    let meta = std::fs::metadata(node).map_err(|e| format!("{}: {e}", node.display()))?;
    if !meta.file_type().is_char_device() {
        return Err(format!("{} is not a device node", node.display()));
    }
    drm_minor(meta.rdev()).map_err(|e| format!("{}: {e}", node.display()))
}

fn drm_minor(rdev: u64) -> Result<u32, String> {
    let (major, minor) = (libc::major(rdev), libc::minor(rdev));
    if major != DRM_MAJOR {
        return Err(format!(
            "device {major}:{minor} is not a DRM node (major {DRM_MAJOR})"
        ));
    }
    Ok(minor)
}

/// One Vulkan physical device as the render-node match reads it.
struct VkGpu {
    /// It offers `VK_EXT_physical_device_drm`.
    drm: bool,
    render_minor: Option<u32>,
    uuid: [u8; 16],
}

/// The one `deviceUUID` among `devices` whose render node is `minor`.
fn pick_uuid(devices: &[VkGpu], minor: u32) -> Result<[u8; 16], String> {
    if !devices.iter().any(|d| d.drm) {
        return Err(format!(
            "none of {} Vulkan devices offers VK_EXT_physical_device_drm, so no render node \
             can be matched to one",
            devices.len()
        ));
    }
    let mut found = devices.iter().filter(|d| d.render_minor == Some(minor));
    match (found.next(), found.next()) {
        (Some(d), None) => Ok(d.uuid),
        (None, _) => Err(format!("no Vulkan device has render minor {minor}")),
        (Some(_), Some(_)) => Err(format!("two Vulkan devices claim render minor {minor}")),
    }
}

/// `VkPhysicalDeviceIDProperties::deviceUUID` of the Vulkan device on render minor `minor`,
/// looked up once per minor: each lookup is a Vulkan instance of its own.
fn device_uuid(minor: u32) -> Result<[u8; 16], String> {
    static CACHE: Mutex<Option<HashMap<u32, [u8; 16]>>> = Mutex::new(None);
    let mut cache = CACHE.lock().unwrap_or_else(|e| e.into_inner());
    if let Some(uuid) = cache.get_or_insert_with(HashMap::new).get(&minor) {
        return Ok(*uuid);
    }
    let uuid = lookup_device_uuid(minor)?;
    cache.get_or_insert_with(HashMap::new).insert(minor, uuid);
    Ok(uuid)
}

/// [`device_uuid`] uncached, matched through `VK_EXT_physical_device_drm`. Instance-level
/// calls only: never `ash::Device::load` (the #267 loader race,
/// `.claude/rules/gstreamer-gotchas.md`).
fn lookup_device_uuid(minor: u32) -> Result<[u8; 16], String> {
    let entry = unsafe { ash::Entry::load() }.map_err(|e| format!("Vulkan loader: {e}"))?;
    let app = vk::ApplicationInfo::default().api_version(vk::API_VERSION_1_1);
    let instance = unsafe {
        entry.create_instance(
            &vk::InstanceCreateInfo::default().application_info(&app),
            None,
        )
    }
    .map_err(|e| vk_error("vkCreateInstance", e))?;
    let devices = gpus(&instance);
    unsafe { instance.destroy_instance(None) };
    pick_uuid(&devices?, minor)
}

fn gpus(instance: &ash::Instance) -> Result<Vec<VkGpu>, String> {
    let list = unsafe { instance.enumerate_physical_devices() }
        .map_err(|e| vk_error("vkEnumeratePhysicalDevices", e))?;
    list.into_iter()
        .map(|pd| unsafe {
            let drm = instance
                .enumerate_device_extension_properties(pd)
                .map_err(|e| vk_error("vkEnumerateDeviceExtensionProperties", e))?
                .iter()
                .any(|e| e.extension_name_as_c_str() == Ok(ash::ext::physical_device_drm::NAME));
            let mut id = vk::PhysicalDeviceIDProperties::default();
            let mut props_drm = vk::PhysicalDeviceDrmPropertiesEXT::default();
            let mut props = vk::PhysicalDeviceProperties2::default().push_next(&mut id);
            if drm {
                props = props.push_next(&mut props_drm);
            }
            instance.get_physical_device_properties2(pd, &mut props);
            let render_minor =
                (drm && props_drm.has_render == vk::TRUE).then_some(props_drm.render_minor as u32);
            Ok(VkGpu {
                drm,
                render_minor,
                uuid: id.device_uuid,
            })
        })
        .collect()
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
            device: device.raw.as_ptr(),
            width: width as c_int,
            height: height as c_int,
            chroma: CHROMA_420,
        };
        let mut raw = ptr::null_mut();
        let _lock = device_lock();
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
        self.packetize(out)
    }

    pub fn device(&self) -> &Arc<Device> {
        &self._device
    }

    /// As [`Encoder::encode`], from Vulkan images on the device this encoder's (shared)
    /// device borrows: no copy through system memory.
    pub fn encode_gpu(
        &mut self,
        frame: &gpu::Frame,
        budget: usize,
        out: &mut [u8],
    ) -> Result<usize, String> {
        let api = api()?;
        let device = &self._device;
        let shared = device
            .shared
            .as_ref()
            .ok_or("Vulkan input on a device of PyroWave's own")?;
        if shared.device_key() != frame.device_key() {
            return Err("the frame is on another Vulkan device".into());
        }
        let size = (self.width, self.height);
        let view = |p: &gpu::Plane, view_format, aspect, swizzle| ImageView {
            image: p.image,
            // pyrowave.h: a plane view of a 2-plane image gives the luma size.
            width: if frame.planes.len() == 1 {
                size.0
            } else {
                p.extent.width
            },
            height: if frame.planes.len() == 1 {
                size.1
            } else {
                p.extent.height
            },
            image_format: p.format,
            view_format,
            mip_level: 0,
            layer: 0,
            aspect,
            swizzle,
            layout: vk::ImageLayout::SHADER_READ_ONLY_OPTIMAL,
        };
        let (y, c, aspects) = match frame.planes.as_slice() {
            [p] => (
                p,
                p,
                [vk::ImageAspectFlags::PLANE_0, vk::ImageAspectFlags::PLANE_1],
            ),
            [y, c] => (y, c, [vk::ImageAspectFlags::COLOR; 2]),
            _ => return Err(format!("{} planes, need 1 or 2", frame.planes.len())),
        };
        // pyrowave.h: NV12 passes the CbCr plane twice, with swizzles picking Cb and Cr.
        let identity = vk::ComponentSwizzle::IDENTITY;
        let buffers = GpuBuffers {
            planes: [
                view(y, vk::Format::R8_UNORM, aspects[0], identity),
                view(
                    c,
                    vk::Format::R8G8_UNORM,
                    aspects[1],
                    vk::ComponentSwizzle::R,
                ),
                view(
                    c,
                    vk::Format::R8G8_UNORM,
                    aspects[1],
                    vk::ComponentSwizzle::G,
                ),
            ],
        };
        let rc = RateControl {
            maximum_bitstream_size: budget,
        };
        let (raw, pw_device) = (self.raw.as_ptr(), device.raw.as_ptr());
        shared.submit(&frame.planes, |cmd| unsafe {
            (api.device_set_command_buffer)(pw_device, cmd);
            let r = check("pyrowave_encoder_encode_gpu", {
                (api.encoder_encode_gpu)(raw, ptr::null(), ptr::null(), &buffers, &rc)
            });
            (api.device_set_command_buffer)(pw_device, vk::CommandBuffer::null());
            r
        })?;
        self.packetize(out)
    }

    /// The last encoded frame into `out`: its length, the SOF then the blocks.
    fn packetize(&mut self, out: &mut [u8]) -> Result<usize, String> {
        let api = api()?;
        let raw = self.raw.as_ptr();
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
            let _lock = device_lock();
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
            device: device.raw.as_ptr(),
            width: width as c_int,
            height: height as c_int,
            chroma: CHROMA_420,
            fragment_path: false,
        };
        let mut raw = ptr::null_mut();
        let _lock = device_lock();
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
            let _lock = device_lock();
            unsafe { (api.decoder_destroy)(self.raw.as_ptr()) }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn gpu(drm: bool, render_minor: Option<u32>, uuid: u8) -> VkGpu {
        VkGpu {
            drm,
            render_minor,
            uuid: [uuid; 16],
        }
    }

    #[test]
    fn the_render_node_picks_exactly_one_device_uuid() {
        // Two identical GPUs: same PCI ids, different render minors and UUIDs.
        let devices = [
            gpu(true, Some(128), 1),
            gpu(true, Some(129), 2),
            gpu(false, None, 3),
        ];
        assert_eq!(pick_uuid(&devices, 129), Ok([2; 16]));
        assert_eq!(pick_uuid(&devices, 128), Ok([1; 16]));
        let none = pick_uuid(&devices, 130).unwrap_err();
        assert!(none.contains("render minor 130"), "{none}");
        let ambiguous = [gpu(true, Some(128), 1), gpu(true, Some(128), 2)];
        assert!(pick_uuid(&ambiguous, 128)
            .unwrap_err()
            .contains("two Vulkan devices"));
    }

    #[test]
    fn no_device_with_the_drm_extension_names_the_extension() {
        let e = pick_uuid(&[gpu(false, None, 1), gpu(false, None, 2)], 128).unwrap_err();
        assert!(e.contains("VK_EXT_physical_device_drm"), "{e}");
        assert!(pick_uuid(&[], 128)
            .unwrap_err()
            .contains("VK_EXT_physical_device_drm"));
    }

    #[test]
    fn only_a_drm_char_device_gives_a_render_minor() {
        assert_eq!(drm_minor(libc::makedev(226, 128)), Ok(128));
        let null = drm_minor(libc::makedev(1, 3)).unwrap_err();
        assert!(null.contains("not a DRM node"), "{null}");
        // /dev/null is char device 1:3: a device node, but not DRM.
        assert!(render_minor(Path::new("/dev/null"))
            .unwrap_err()
            .contains("not a DRM node"));
        assert!(render_minor(Path::new("/"))
            .unwrap_err()
            .contains("not a device node"));
    }

    #[test]
    fn only_memory_shortages_are_resource_errors() {
        assert!(is_resource_error(&check("x", -4).unwrap_err()));
        assert!(is_resource_error(&check("x", -3).unwrap_err()));
        assert!(!is_resource_error(&check("x", -5).unwrap_err()));
        assert!(check("x", 1).is_ok(), "TIMEOUT is not an error");
        for r in [
            vk::Result::ERROR_OUT_OF_HOST_MEMORY,
            vk::Result::ERROR_OUT_OF_DEVICE_MEMORY,
        ] {
            assert!(is_resource_error(&vk_error("vkCreateInstance", r)), "{r:?}");
        }
        for r in [
            vk::Result::ERROR_INITIALIZATION_FAILED,
            vk::Result::ERROR_INCOMPATIBLE_DRIVER,
            vk::Result::ERROR_DEVICE_LOST,
        ] {
            assert!(
                !is_resource_error(&vk_error("vkCreateInstance", r)),
                "{r:?}"
            );
        }
    }
}
