//! #267 against PyroWave: building a device's function table while another thread creates
//! Vulkan instances. On Vulkan-Loader before 1.4.345 the table's `vkGetDeviceQueue2` lookup
//! walks every instance unlocked while `vkEnumeratePhysicalDevices` frees driver records:
//! SIGSEGV in `libvulkan.so.1`, as do lookups of device commands newer than the loader. The
//! image ships Vulkan-Loader 1.4.345 (`deploy/pins.env` VULKAN_LOADER_REF), which locks that
//! walk. On a GPU host, in the image:
//! `QUASAR_REQUIRE_PYROWAVE=1 <test binary> --ignored --nocapture`; a crash is the failure.
//! `QUASAR_RACE_SECS` sets the length (default 60). Only the runtime image carries the pinned
//! loader: the dev and devtools images keep Fedora's 1.4.341, so it crashes there by design.

use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::Arc;
use std::time::{Duration, Instant};

use ash::vk;
use gstpyrowave::ffi::Device;

#[test]
#[ignore = "a GPU host with libpyrowave-shared; minutes"]
fn pyrowave_device_tables_survive_instance_churn() {
    let node = std::env::var("QUASAR_PYROWAVE_DEVICE")
        .ok()
        .filter(|s| !s.is_empty());
    let secs = std::env::var("QUASAR_RACE_SECS")
        .ok()
        .and_then(|s| s.parse().ok())
        .unwrap_or(60);
    if let Err(e) = Device::open(node.as_deref().map(std::path::Path::new)) {
        assert_ne!(
            std::env::var("QUASAR_REQUIRE_PYROWAVE").as_deref(),
            Ok("1"),
            "no PyroWave device: {e}"
        );
        eprintln!("skipping: no PyroWave device: {e}");
        return;
    }
    let stop = Arc::new(AtomicBool::new(false));
    let instances = Arc::new(AtomicU64::new(0));
    let churn = {
        let (stop, instances) = (stop.clone(), instances.clone());
        std::thread::spawn(move || {
            let entry = unsafe { ash::Entry::load() }.expect("Vulkan loader");
            let app = vk::ApplicationInfo::default().api_version(vk::API_VERSION_1_1);
            let info = vk::InstanceCreateInfo::default().application_info(&app);
            while !stop.load(Ordering::Relaxed) {
                let instance = unsafe { entry.create_instance(&info, None) }.expect("instance");
                let _ = unsafe { instance.enumerate_physical_devices() };
                unsafe { instance.destroy_instance(None) };
                instances.fetch_add(1, Ordering::Relaxed);
            }
        })
    };
    let deadline = Instant::now() + Duration::from_secs(secs);
    let mut devices = 0u64;
    while Instant::now() < deadline {
        // Each open builds PyroWave's device table (volkLoadDeviceTable); each drop destroys it.
        drop(Device::open(node.as_deref().map(std::path::Path::new)).expect("device"));
        devices += 1;
    }
    stop.store(true, Ordering::Relaxed);
    churn.join().unwrap();
    println!(
        "{devices} PyroWave device tables built beside {} instance creations, no crash",
        instances.load(Ordering::Relaxed)
    );
    assert!(devices > 0);
}
