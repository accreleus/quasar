//! PyroWave on the GStreamer Vulkan device its input frames already live on (#1104): no
//! instance or device of its own, and no copy through system memory.
//!
//! `pyrowave_create_device` borrows a `VkDevice` but needs the create infos it was made with.
//! `GstVulkanDevice` keeps neither, so they are rebuilt here as a subset of what GStreamer
//! enabled: GStreamer 1.28 (`gst_vulkan_device_open`) passes the physical device's queried
//! `VkPhysicalDeviceFeatures2` chain back verbatim, so the same query names enabled features
//! only; the extension lists keep only what GStreamer reports enabled; one queue, GStreamer's
//! graphics+compute queue, with its submit lock as PyroWave's queue lock.
//!
//! The GStreamer Vulkan library is loaded at run time like PyroWave's, and the few public
//! struct fields read here (handles, an image's create info and tracked layout) are
//! `repr(C)` overlays of the 1.28 headers. An image whose overlay does not read back as a
//! sampled 4:2:0 image is refused, never used.

use std::ffi::{c_char, c_int, c_void, CStr, CString};
use std::ptr::{self, NonNull};
use std::sync::{Mutex, OnceLock};

use ash::vk;
use gstreamer as gst;

const SONAME: &CStr = c"libgstvulkan-1.0.so.0";

struct GstVk {
    is_image_memory: unsafe extern "C" fn(*mut gst::ffi::GstMemory) -> c_int,
    device_physical_device: unsafe extern "C" fn(*mut GstVkDevice) -> vk::PhysicalDevice,
    device_extension_enabled: unsafe extern "C" fn(*mut GstVkDevice, *const c_char) -> c_int,
    device_select_queue: unsafe extern "C" fn(*mut GstVkDevice, vk::QueueFlags) -> *mut GstVkQueue,
    instance_extension_enabled: unsafe extern "C" fn(*mut GstVkInstance, *const c_char) -> c_int,
    instance_check_api_version: unsafe extern "C" fn(*mut GstVkInstance, u32, u32, u32) -> c_int,
    physical_check_api_version: unsafe extern "C" fn(*mut c_void, u32, u32, u32) -> c_int,
    queue_submit_lock: unsafe extern "C" fn(*mut GstVkQueue),
    queue_submit_unlock: unsafe extern "C" fn(*mut GstVkQueue),
}

fn load() -> Result<GstVk, String> {
    // SAFETY: as ffi::load: NUL-terminated names, each cast to its gstvk*.h signature.
    unsafe {
        let lib = libc::dlopen(SONAME.as_ptr(), libc::RTLD_NOW | libc::RTLD_LOCAL);
        if lib.is_null() {
            return Err("cannot load libgstvulkan-1.0.so.0".into());
        }
        macro_rules! sym {
            ($name:literal) => {{
                let p = libc::dlsym(lib, concat!($name, "\0").as_ptr().cast());
                if p.is_null() {
                    return Err(format!("libgstvulkan has no {}", $name));
                }
                std::mem::transmute_copy::<*mut c_void, _>(&p)
            }};
        }
        Ok(GstVk {
            is_image_memory: sym!("gst_is_vulkan_image_memory"),
            device_physical_device: sym!("gst_vulkan_device_get_physical_device"),
            device_extension_enabled: sym!("gst_vulkan_device_is_extension_enabled"),
            device_select_queue: sym!("gst_vulkan_device_select_queue"),
            instance_extension_enabled: sym!("gst_vulkan_instance_is_extension_enabled"),
            instance_check_api_version: sym!("gst_vulkan_instance_check_api_version"),
            physical_check_api_version: sym!("gst_vulkan_physical_device_check_api_version"),
            queue_submit_lock: sym!("gst_vulkan_queue_submit_lock"),
            queue_submit_unlock: sym!("gst_vulkan_queue_submit_unlock"),
        })
    }
}

fn gstvk() -> Result<&'static GstVk, String> {
    static API: OnceLock<Result<GstVk, String>> = OnceLock::new();
    API.get_or_init(load).as_ref().map_err(Clone::clone)
}

// Public fields of the 1.28 structs (gstvkinstance.h, gstvkdevice.h, gstvkqueue.h,
// gstvkbarrier.h, gstvkimagememory.h), up to the last one read.
#[repr(C)]
struct GstVkInstance {
    parent: gst::ffi::GstObject,
    instance: vk::Instance,
}

#[repr(C)]
pub(crate) struct GstVkDevice {
    parent: gst::ffi::GstObject,
    instance: *mut GstVkInstance,
    physical_device: *mut c_void,
    device: vk::Device,
}

#[repr(C)]
struct GstVkQueue {
    parent: gst::ffi::GstObject,
    device: *mut GstVkDevice,
    queue: vk::Queue,
    family: u32,
    index: u32,
}

#[repr(C)]
struct BarrierMemoryInfo {
    kind: c_int,
    flags: c_int,
    queue: *mut GstVkQueue,
    pipeline_stages: u64,
    access_flags: u64,
    semaphore: vk::Semaphore,
    semaphore_value: u64,
    _reserved: [*mut c_void; 4],
}

#[repr(C)]
struct BarrierImageInfo {
    parent: BarrierMemoryInfo,
    image_layout: vk::ImageLayout,
    subresource_range: vk::ImageSubresourceRange,
}

#[repr(C)]
struct GstVkImageMemory {
    parent: gst::ffi::GstMemory,
    device: *mut GstVkDevice,
    image: vk::Image,
    vk_mem: *mut c_void,
    create_info: vk::ImageCreateInfo<'static>,
    requirements: vk::MemoryRequirements,
    format_properties: vk::ImageFormatProperties,
    usage: vk::ImageUsageFlags,
    barrier: BarrierImageInfo,
}

/// One Vulkan image of an input frame.
#[derive(Clone, Copy, Debug)]
pub struct Plane {
    pub image: vk::Image,
    pub format: vk::Format,
    pub extent: vk::Extent2D,
    /// The layout GStreamer tracks the image in; the encode restores it.
    pub layout: vk::ImageLayout,
}

/// An NV12 frame in `GstVulkanImageMemory`: one 2-plane image (the compositor's encode
/// ring), or an R8 and an R8G8 image (GStreamer's own image pools).
pub struct Frame {
    pub(crate) device: NonNull<GstVkDevice>,
    pub planes: Vec<Plane>,
}

impl Frame {
    /// The `GstVulkanDevice` the frame's images belong to, as an identity.
    pub fn device_key(&self) -> usize {
        self.device.as_ptr() as usize
    }
}

/// The input as Vulkan images of `width`x`height` NV12, checked against what the encode
/// needs: sampled, and plane views possible.
pub fn frame(buffer: &gst::BufferRef, width: u32, height: u32) -> Result<Frame, String> {
    let api = gstvk()?;
    let expected: &[(vk::Format, u32)] = match buffer.n_memory() {
        1 => &[(vk::Format::G8_B8R8_2PLANE_420_UNORM, 1)],
        2 => &[(vk::Format::R8_UNORM, 1), (vk::Format::R8G8_UNORM, 2)],
        n => return Err(format!("an NV12 frame in {n} Vulkan memories")),
    };
    let mut device = None;
    let mut planes = Vec::new();
    for (i, &(format, div)) in expected.iter().enumerate() {
        let mem = buffer.peek_memory(i).as_ptr() as *mut gst::ffi::GstMemory;
        // SAFETY: the type check comes first; the overlay is the 1.28 public layout, and the
        // buffer keeps the memory alive for this call.
        let m = unsafe {
            if (api.is_image_memory)(mem) == 0 {
                return Err(format!("memory {i} is not a GstVulkanImageMemory"));
            }
            &*(mem as *const GstVkImageMemory)
        };
        let info = &m.create_info;
        let size = vk::Extent2D {
            width: width.div_ceil(div),
            height: height.div_ceil(div),
        };
        let plane_views = div == 2 || info.flags.contains(vk::ImageCreateFlags::MUTABLE_FORMAT);
        if info.format != format
            || !info.usage.contains(vk::ImageUsageFlags::SAMPLED)
            || !plane_views
            || (info.extent.width, info.extent.height) != (size.width, size.height)
        {
            return Err(format!(
                "memory {i}: {:?} {}x{} usage {:?} flags {:?}, need sampled {format:?} {}x{}{}",
                info.format,
                info.extent.width,
                info.extent.height,
                info.usage,
                info.flags,
                size.width,
                size.height,
                if div == 1 { " with MUTABLE_FORMAT" } else { "" }
            ));
        }
        let layout = m.barrier.image_layout;
        if matches!(
            layout,
            vk::ImageLayout::UNDEFINED | vk::ImageLayout::PREINITIALIZED
        ) {
            return Err(format!(
                "memory {i}: image in {layout:?}, no picture to read"
            ));
        }
        let dev = NonNull::new(m.device).ok_or("an image memory without a device")?;
        if *device.get_or_insert(dev) != dev {
            return Err("the frame's planes are on two devices".into());
        }
        planes.push(Plane {
            image: m.image,
            format,
            extent: size,
            layout,
        });
    }
    Ok(Frame {
        device: device.expect("at least one plane"),
        planes,
    })
}

/// `pyrowave_device_create_queue_info`.
#[repr(C)]
pub(crate) struct QueueInfo {
    queue: vk::Queue,
    family_index: u32,
    index: u32,
}

/// `pyrowave_device_create_info`.
#[repr(C)]
pub(crate) struct DeviceCreateInfo {
    get_instance_proc_addr: vk::PFN_vkGetInstanceProcAddr,
    instance: vk::Instance,
    physical_device: vk::PhysicalDevice,
    device: vk::Device,
    instance_create_info: *const vk::InstanceCreateInfo<'static>,
    device_create_info: *const vk::DeviceCreateInfo<'static>,
    queue_info: *mut QueueInfo,
    queue_info_count: u32,
    queue_lock: Option<unsafe extern "C" fn(*mut c_void)>,
    queue_unlock: Option<unsafe extern "C" fn(*mut c_void)>,
    userdata: *mut c_void,
}

/// What `DeviceCreateInfo` points at, which pyrowave.h needs alive as long as its device.
/// Boxed, and its pointers set once boxed: it never moves.
struct CreateInfos {
    _instance_exts: Vec<CString>,
    instance_ext_ptrs: Vec<*const c_char>,
    _device_exts: Vec<CString>,
    device_ext_ptrs: Vec<*const c_char>,
    app: vk::ApplicationInfo<'static>,
    instance: vk::InstanceCreateInfo<'static>,
    f2: vk::PhysicalDeviceFeatures2<'static>,
    f11: vk::PhysicalDeviceVulkan11Features<'static>,
    f12: vk::PhysicalDeviceVulkan12Features<'static>,
    f13: vk::PhysicalDeviceVulkan13Features<'static>,
    priority: f32,
    queue: vk::DeviceQueueCreateInfo<'static>,
    device: vk::DeviceCreateInfo<'static>,
    queue_info: QueueInfo,
    pyrowave: DeviceCreateInfo,
}

unsafe extern "C" fn queue_lock(queue: *mut c_void) {
    if let Ok(api) = gstvk() {
        (api.queue_submit_lock)(queue.cast());
    }
}

unsafe extern "C" fn queue_unlock(queue: *mut c_void) {
    if let Ok(api) = gstvk() {
        (api.queue_submit_unlock)(queue.cast());
    }
}

struct Cmd {
    pool: vk::CommandPool,
    buffer: vk::CommandBuffer,
    fence: vk::Fence,
}

/// The borrowed device: references on the `GstVulkanDevice` and its queue, the create infos,
/// and a command buffer and fence for the encode.
pub struct Shared {
    gst_device: NonNull<GstVkDevice>,
    gst_queue: NonNull<GstVkQueue>,
    infos: Box<CreateInfos>,
    vk: ash::Device,
    queue: vk::Queue,
    cmd: Mutex<Cmd>,
    _entry: ash::Entry,
}

// SAFETY: the GStreamer objects are refcounted and thread safe; the queue is used only under
// its submit lock and the command buffer only under `cmd`.
unsafe impl Send for Shared {}
unsafe impl Sync for Shared {}

fn enabled_names(
    available: Vec<vk::ExtensionProperties>,
    enabled: impl Fn(*const c_char) -> bool,
) -> (Vec<CString>, Vec<*const c_char>) {
    let names: Vec<CString> = available
        .iter()
        .filter_map(|e| e.extension_name_as_c_str().ok())
        .filter(|n| enabled(n.as_ptr()))
        .map(CStr::to_owned)
        .collect();
    let ptrs = names.iter().map(|n| n.as_ptr()).collect();
    (names, ptrs)
}

impl Shared {
    /// Borrow `device` (a `GstVulkanDevice`). The returned create info is valid as long as
    /// this is.
    pub(crate) fn new(device: NonNull<GstVkDevice>) -> Result<Shared, String> {
        let api = gstvk()?;
        let dev = device.as_ptr();
        // SAFETY: a live GstVulkanDevice (the caller holds a buffer of its memory); every
        // handle read is a public field or getter; the Vulkan calls are instance-level, and
        // the device table never asks for vkGetDeviceQueue2 (the #267 loader race,
        // `.claude/rules/gstreamer-gotchas.md`).
        unsafe {
            let gst_instance = (*dev).instance;
            let physical_obj = (*dev).physical_device;
            if gst_instance.is_null() || physical_obj.is_null() {
                return Err("the GstVulkanDevice has no instance or physical device".into());
            }
            if (api.instance_check_api_version)(gst_instance, 1, 3, 0) == 0
                || (api.physical_check_api_version)(physical_obj, 1, 3, 0) == 0
            {
                return Err("PyroWave needs Vulkan 1.3 on the shared device".into());
            }
            let vk_instance = (*gst_instance).instance;
            let vk_device = (*dev).device;
            let physical = (api.device_physical_device)(dev);
            let entry = ash::Entry::load().map_err(|e| format!("Vulkan loader: {e}"))?;
            let instance = ash::Instance::load(entry.static_fn(), vk_instance);

            let queue = NonNull::new((api.device_select_queue)(
                dev,
                vk::QueueFlags::GRAPHICS | vk::QueueFlags::COMPUTE,
            ))
            .ok_or("the shared device has no graphics+compute queue")?;
            // From here the queue reference is ours: `Shared` (or this guard) releases it.
            gst::ffi::gst_object_ref(dev.cast());
            let mut guard = Refs {
                device,
                queue: Some(queue),
            };
            let q = &*queue.as_ptr();

            let (instance_exts, instance_ext_ptrs) = enabled_names(
                entry
                    .enumerate_instance_extension_properties(None)
                    .map_err(|e| format!("vkEnumerateInstanceExtensionProperties: {e:?}"))?,
                |n| (api.instance_extension_enabled)(gst_instance, n) != 0,
            );
            let (device_exts, device_ext_ptrs) = enabled_names(
                instance
                    .enumerate_device_extension_properties(physical)
                    .map_err(|e| format!("vkEnumerateDeviceExtensionProperties: {e:?}"))?,
                |n| (api.device_extension_enabled)(dev, n) != 0,
            );
            let mut infos = Box::new(CreateInfos {
                _instance_exts: instance_exts,
                instance_ext_ptrs,
                _device_exts: device_exts,
                device_ext_ptrs,
                app: vk::ApplicationInfo::default().api_version(vk::API_VERSION_1_3),
                instance: vk::InstanceCreateInfo::default(),
                f2: vk::PhysicalDeviceFeatures2::default(),
                f11: vk::PhysicalDeviceVulkan11Features::default(),
                f12: vk::PhysicalDeviceVulkan12Features::default(),
                f13: vk::PhysicalDeviceVulkan13Features::default(),
                priority: 1.0,
                queue: vk::DeviceQueueCreateInfo::default(),
                device: vk::DeviceCreateInfo::default(),
                queue_info: QueueInfo {
                    queue: q.queue,
                    family_index: q.family,
                    index: q.index,
                },
                pyrowave: DeviceCreateInfo {
                    get_instance_proc_addr: entry.static_fn().get_instance_proc_addr,
                    instance: vk_instance,
                    physical_device: physical,
                    device: vk_device,
                    instance_create_info: ptr::null(),
                    device_create_info: ptr::null(),
                    queue_info: ptr::null_mut(),
                    queue_info_count: 1,
                    queue_lock: Some(queue_lock),
                    queue_unlock: Some(queue_unlock),
                    userdata: queue.as_ptr().cast(),
                },
            });
            let i = &mut *infos;
            i.instance.p_application_info = &i.app;
            i.instance.enabled_extension_count = i.instance_ext_ptrs.len() as u32;
            i.instance.pp_enabled_extension_names = i.instance_ext_ptrs.as_ptr();
            i.f2.p_next = (&mut i.f11 as *mut vk::PhysicalDeviceVulkan11Features).cast();
            i.f11.p_next = (&mut i.f12 as *mut vk::PhysicalDeviceVulkan12Features).cast();
            i.f12.p_next = (&mut i.f13 as *mut vk::PhysicalDeviceVulkan13Features).cast();
            // GStreamer enabled exactly what this query reports (see the module comment).
            instance.get_physical_device_features2(physical, &mut i.f2);
            i.queue.queue_family_index = q.family;
            i.queue.queue_count = 1;
            i.queue.p_queue_priorities = &i.priority;
            i.device.p_next = (&i.f2 as *const vk::PhysicalDeviceFeatures2).cast();
            i.device.queue_create_info_count = 1;
            i.device.p_queue_create_infos = &i.queue;
            i.device.enabled_extension_count = i.device_ext_ptrs.len() as u32;
            i.device.pp_enabled_extension_names = i.device_ext_ptrs.as_ptr();
            i.pyrowave.instance_create_info = &i.instance;
            i.pyrowave.device_create_info = &i.device;
            i.pyrowave.queue_info = &mut i.queue_info;

            let get_device_proc_addr = instance.fp_v1_0().get_device_proc_addr;
            let vk = ash::Device::load_with(
                |name| {
                    if name == c"vkGetDeviceQueue2" {
                        ptr::null()
                    } else {
                        std::mem::transmute::<vk::PFN_vkVoidFunction, *const c_void>(
                            get_device_proc_addr(vk_device, name.as_ptr()),
                        )
                    }
                },
                vk_device,
            );
            let cmd = command_objects(&vk, q.family)?;
            guard.queue = None;
            std::mem::forget(guard);
            Ok(Shared {
                gst_device: device,
                gst_queue: queue,
                infos,
                vk,
                queue: q.queue,
                cmd: Mutex::new(cmd),
                _entry: entry,
            })
        }
    }

    pub(crate) fn device_create_info(&self) -> *const DeviceCreateInfo {
        &self.infos.pyrowave
    }

    /// The `GstVulkanDevice` this borrows, as an identity.
    pub fn device_key(&self) -> usize {
        self.gst_device.as_ptr() as usize
    }

    /// Record `record` into the command buffer between barriers that put `planes` in
    /// `SHADER_READ_ONLY_OPTIMAL` and back in their tracked layouts, submit it on the queue
    /// under its lock, and wait for it.
    ///
    /// shortcut: the layout round trip races any other reader of the same image (the
    /// session's standard encoder reads it at the same time); harmless while encode-src
    /// images are uncompressed (LINEAR on AMD; NVIDIA ignores layouts). Upgrade to an
    /// ordered hand-off from the standard encoder if a driver compresses them.
    pub(crate) fn submit(
        &self,
        planes: &[Plane],
        record: impl FnOnce(vk::CommandBuffer) -> Result<(), String>,
    ) -> Result<(), String> {
        let cmd = self.cmd.lock().unwrap_or_else(|e| e.into_inner());
        let vk = &self.vk;
        let read = vk::ImageLayout::SHADER_READ_ONLY_OPTIMAL;
        let barriers = |to_read: bool| -> Vec<vk::ImageMemoryBarrier<'static>> {
            planes
                .iter()
                .map(|p| {
                    let b = vk::ImageMemoryBarrier::default()
                        .image(p.image)
                        .src_queue_family_index(vk::QUEUE_FAMILY_IGNORED)
                        .dst_queue_family_index(vk::QUEUE_FAMILY_IGNORED)
                        .subresource_range(vk::ImageSubresourceRange {
                            aspect_mask: vk::ImageAspectFlags::COLOR,
                            base_mip_level: 0,
                            level_count: 1,
                            base_array_layer: 0,
                            layer_count: 1,
                        });
                    if to_read {
                        b.old_layout(p.layout)
                            .new_layout(read)
                            .src_access_mask(vk::AccessFlags::MEMORY_WRITE)
                            .dst_access_mask(vk::AccessFlags::SHADER_READ)
                    } else {
                        b.old_layout(read)
                            .new_layout(p.layout)
                            .src_access_mask(vk::AccessFlags::empty())
                            .dst_access_mask(
                                vk::AccessFlags::MEMORY_READ | vk::AccessFlags::MEMORY_WRITE,
                            )
                    }
                })
                .collect()
        };
        let err = |what: &'static str| move |e: vk::Result| format!("{what}: {e:?}");
        // SAFETY: the command buffer and fence are ours, idle (every submit is waited), and
        // used only under `cmd`; the images are kept alive by the caller's buffer.
        unsafe {
            vk.reset_command_buffer(cmd.buffer, vk::CommandBufferResetFlags::empty())
                .map_err(err("vkResetCommandBuffer"))?;
            vk.begin_command_buffer(
                cmd.buffer,
                &vk::CommandBufferBeginInfo::default()
                    .flags(vk::CommandBufferUsageFlags::ONE_TIME_SUBMIT),
            )
            .map_err(err("vkBeginCommandBuffer"))?;
            vk.cmd_pipeline_barrier(
                cmd.buffer,
                vk::PipelineStageFlags::ALL_COMMANDS,
                vk::PipelineStageFlags::COMPUTE_SHADER,
                vk::DependencyFlags::empty(),
                &[],
                &[],
                &barriers(true),
            );
            let recorded = record(cmd.buffer);
            vk.cmd_pipeline_barrier(
                cmd.buffer,
                vk::PipelineStageFlags::COMPUTE_SHADER,
                vk::PipelineStageFlags::ALL_COMMANDS,
                vk::DependencyFlags::empty(),
                &[],
                &[],
                &barriers(false),
            );
            vk.end_command_buffer(cmd.buffer)
                .map_err(err("vkEndCommandBuffer"))?;
            // A failed record still submits: the barriers leave the images as they were.
            let buffers = [cmd.buffer];
            let submit = vk::SubmitInfo::default().command_buffers(&buffers);
            let api = gstvk()?;
            (api.queue_submit_lock)(self.gst_queue.as_ptr());
            let submitted = vk.queue_submit(self.queue, &[submit], cmd.fence);
            (api.queue_submit_unlock)(self.gst_queue.as_ptr());
            submitted.map_err(err("vkQueueSubmit"))?;
            vk.wait_for_fences(&[cmd.fence], true, u64::MAX)
                .map_err(err("vkWaitForFences"))?;
            vk.reset_fences(&[cmd.fence])
                .map_err(err("vkResetFences"))?;
            recorded
        }
    }
}

fn command_objects(vk: &ash::Device, family: u32) -> Result<Cmd, String> {
    // SAFETY: plain object creation on a live device; destroyed in `Shared::drop`.
    unsafe {
        let pool = vk
            .create_command_pool(
                &vk::CommandPoolCreateInfo::default()
                    .queue_family_index(family)
                    .flags(vk::CommandPoolCreateFlags::RESET_COMMAND_BUFFER),
                None,
            )
            .map_err(|e| format!("vkCreateCommandPool: {e:?}"))?;
        let made = vk
            .allocate_command_buffers(
                &vk::CommandBufferAllocateInfo::default()
                    .command_pool(pool)
                    .level(vk::CommandBufferLevel::PRIMARY)
                    .command_buffer_count(1),
            )
            .and_then(|b| {
                vk.create_fence(&vk::FenceCreateInfo::default(), None)
                    .map(|f| (b[0], f))
            });
        match made {
            Ok((buffer, fence)) => Ok(Cmd {
                pool,
                buffer,
                fence,
            }),
            Err(e) => {
                vk.destroy_command_pool(pool, None);
                Err(format!("command buffer or fence: {e:?}"))
            }
        }
    }
}

/// Releases the GStreamer references if `Shared::new` fails part way.
struct Refs {
    device: NonNull<GstVkDevice>,
    queue: Option<NonNull<GstVkQueue>>,
}

impl Drop for Refs {
    fn drop(&mut self) {
        // SAFETY: each pointer holds one reference taken in `Shared::new`.
        unsafe {
            if let Some(q) = self.queue {
                gst::ffi::gst_object_unref(q.as_ptr().cast());
            }
            gst::ffi::gst_object_unref(self.device.as_ptr().cast());
        }
    }
}

impl Drop for Shared {
    fn drop(&mut self) {
        let cmd = self.cmd.get_mut().unwrap_or_else(|e| e.into_inner());
        // SAFETY: every submit was waited, so nothing is in flight; the pool frees its buffer.
        unsafe {
            self.vk.destroy_fence(cmd.fence, None);
            self.vk.destroy_command_pool(cmd.pool, None);
        }
        drop(Refs {
            device: self.gst_device,
            queue: Some(self.gst_queue),
        });
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::mem::{offset_of, size_of};

    /// The overlays against the 1.28 headers on x86_64/aarch64 (LP64): a field moved here
    /// would read the wrong bytes of a live image.
    #[test]
    fn the_overlays_match_the_gstreamer_1_28_layouts() {
        let object = size_of::<gst::ffi::GstObject>();
        assert_eq!(offset_of!(GstVkDevice, device), object + 16);
        assert_eq!(offset_of!(GstVkInstance, instance), object);
        assert_eq!(offset_of!(GstVkQueue, family), object + 16);
        // BarrierMemoryInfo: two enums, a pointer, three u64s, a u64, four pointers.
        assert_eq!(
            size_of::<BarrierMemoryInfo>(),
            4 + 4 + 8 + 8 + 8 + 8 + 8 + 32
        );
        assert_eq!(offset_of!(BarrierImageInfo, image_layout), 80);
        let memory = size_of::<gst::ffi::GstMemory>();
        assert_eq!(offset_of!(GstVkImageMemory, create_info), memory + 24);
        assert_eq!(size_of::<vk::ImageCreateInfo>(), 88);
        assert_eq!(
            offset_of!(GstVkImageMemory, barrier),
            // create info, requirements (24), format properties (32), usage, padding.
            memory + 24 + 88 + 24 + 32 + 4 + 4
        );
    }
}
