//! Virtual input against the real kernel uinput, read back through evdev. A
//! mock cannot catch an ioctl the kernel rejects: the first #348 edge build's
//! `UI_SET_PHYS` number passed every unit test and failed every launch (EINVAL).
//!
//! `#[ignore]`d: needs `/dev/uinput` and root (`create` also mknods and writes
//! `/run/udev/data`). Run with `make test-uinput`; CI runs them on every PR.
//! Without uinput a test prints SKIP and passes; `QUASAR_REQUIRE_UINPUT=1`
//! makes that a failure. Each test uses a unique tag and drops its devices, so
//! a run on a shared host never touches a live session's.

use super::*;
use input_linux::EvdevHandle;
use std::os::fd::AsRawFd;
use std::os::unix::fs::OpenOptionsExt;
use std::sync::atomic::AtomicU32;

/// Unique per test and per process, and no longer than the session UUID a real
/// tag is (device names are capped at `UINPUT_MAX_NAME_SIZE`).
fn unique_tag() -> String {
    static N: AtomicU32 = AtomicU32::new(0);
    format!(
        "uinput-test-{:08x}-{:04}",
        std::process::id(),
        N.fetch_add(1, Ordering::Relaxed)
    )
}

/// The session's devices, or `None` (after printing SKIP) when this host has
/// no usable `/dev/uinput`.
fn devices(test: &str) -> Option<(VirtualDevices, String)> {
    if let Err(e) = OpenOptions::new().write(true).open("/dev/uinput") {
        let why = format!("/dev/uinput not usable ({e}); needs the uinput module and root");
        if std::env::var_os("QUASAR_REQUIRE_UINPUT").is_some() {
            panic!("{test}: {why}, and QUASAR_REQUIRE_UINPUT is set");
        }
        eprintln!("SKIP {test}: {why}");
        return None;
    }
    let tag = unique_tag();
    let devs = VirtualDevices::create(&tag)
        .unwrap_or_else(|e| panic!("VirtualDevices::create against the real kernel: {e:#}"));
    Some((devs, tag))
}

/// Open the device behind `path` through a node made for this call,
/// non-blocking so a missing event times out instead of hanging.
///
/// Not through `path` itself: in a container's private `/dev` that node outlives
/// the device, and its inode keeps pointing at the destroyed device with the same
/// minor while anything (the host's udev) still holds it, so opening it right
/// after the previous test's teardown fails ENODEV. A host devtmpfs `/dev/input`,
/// as compose mounts, gets a fresh inode per device and is not affected.
fn open_evdev(path: &Path) -> EvdevHandle<File> {
    let (maj, min) = dev_major_minor(path).unwrap();
    let dir = tempfile::tempdir().unwrap();
    let node = dir.path().join("evdev");
    let cnode = std::ffi::CString::new(node.as_os_str().as_bytes()).unwrap();
    // SAFETY: a NUL-terminated path in a directory this test owns.
    let rc = unsafe {
        libc::mknod(
            cnode.as_ptr(),
            libc::S_IFCHR | 0o600,
            libc::makedev(maj, min),
        )
    };
    assert_eq!(rc, 0, "mknod {node:?}: {}", std::io::Error::last_os_error());
    let f = OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_NONBLOCK)
        .open(&node)
        .unwrap_or_else(|e| panic!("open {path:?} (c {maj}:{min}): {e}"));
    EvdevHandle::new(f)
}

/// A NUL-padded string from an `EVIOCG*` buffer.
fn c_string(bytes: Vec<u8>) -> String {
    String::from_utf8_lossy(&bytes)
        .trim_end_matches('\0')
        .to_string()
}

/// Read one `SYN_REPORT`-terminated frame as `(type, code, value)`, without
/// the `EV_SYN` and `EV_MSC` bookkeeping. Panics after 2 s with nothing.
fn read_frame(h: &EvdevHandle<File>) -> Vec<(u16, u16, i32)> {
    let deadline = std::time::Instant::now() + std::time::Duration::from_secs(2);
    let mut frame = Vec::new();
    loop {
        let left = deadline.saturating_duration_since(std::time::Instant::now());
        assert!(
            !left.is_zero(),
            "no complete evdev frame within 2 s (got {frame:?})"
        );
        let mut pfd = libc::pollfd {
            fd: h.as_raw_fd(),
            events: libc::POLLIN,
            revents: 0,
        };
        // SAFETY: one valid pollfd for the duration of the call.
        unsafe { libc::poll(&mut pfd, 1, left.as_millis() as libc::c_int) };
        let mut buf = [ev(0, 0, 0); 32];
        let n = match h.read(&mut buf) {
            Ok(n) => n,
            Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => continue,
            Err(e) => panic!("read evdev: {e}"),
        };
        for e in &buf[..n] {
            match e.type_ as i32 {
                isys::EV_SYN if e.code as i32 == isys::SYN_REPORT => return frame,
                isys::EV_SYN | isys::EV_MSC => {}
                _ => frame.push((e.type_, e.code, e.value)),
            }
        }
    }
}

fn key(code: i32, value: i32) -> (u16, u16, i32) {
    (isys::EV_KEY as u16, code as u16, value)
}

fn abs(code: i32, value: i32) -> (u16, u16, i32) {
    (isys::EV_ABS as u16, code as u16, value)
}

fn rel(code: i32, value: i32) -> (u16, u16, i32) {
    (isys::EV_REL as u16, code as u16, value)
}

/// The launch path itself: this is the call the `UI_SET_PHYS` bug failed, and
/// what `session/host.rs` treats as fatal.
#[test]
#[ignore = "needs /dev/uinput and root: make test-uinput"]
fn uinput_create_yields_three_real_evdev_nodes() {
    let Some((devs, _)) = devices("uinput_create_yields_three_real_evdev_nodes") else {
        return;
    };
    for path in [&devs.keyboard_path, &devs.mouse_path, &devs.gamepad_path] {
        let meta = std::fs::metadata(path).unwrap_or_else(|e| panic!("stat {path:?}: {e}"));
        assert!(
            meta.file_type().is_char_device(),
            "{path:?} is not a char device"
        );
    }
    assert_ne!(devs.keyboard_path, devs.mouse_path);
    assert_ne!(devs.mouse_path, devs.gamepad_path);
}

/// Issue #348: the kernel really reports the wired Xbox 360 identity, and the
/// session tag reached `phys` (which is exactly what the bad request number lost).
#[test]
#[ignore = "needs /dev/uinput and root: make test-uinput"]
fn uinput_gamepad_reports_the_xbox360_identity() {
    let Some((devs, tag)) = devices("uinput_gamepad_reports_the_xbox360_identity") else {
        return;
    };
    let pad = open_evdev(&devs.gamepad_path);
    let id = pad.device_id().expect("EVIOCGID");
    assert_eq!(
        (id.bustype, id.vendor, id.product, id.version),
        (isys::BUS_USB, 0x045e, 0x028e, 0x0114),
        "gamepad must present as a wired Xbox 360 pad on BUS_USB"
    );
    let name = c_string(pad.device_name().expect("EVIOCGNAME"));
    assert_eq!(name, "Quasar Virtual Gamepad");
    let phys = c_string(pad.physical_location().expect("EVIOCGPHYS"));
    assert_eq!(phys, format!("quasar/{tag}/input0"));

    // The keyboard and mouse carry the tag in their names instead.
    for path in [&devs.keyboard_path, &devs.mouse_path] {
        let name = c_string(open_evdev(path).device_name().expect("EVIOCGNAME"));
        assert!(name.contains(&tag), "{path:?} name {name:?} lacks the tag");
    }
}

/// The key set exactly as xpad exposes it, in the code order SDL indexes
/// buttons by (a:b0 b:b1 x:b2 y:b3 ... guide:b8 leftstick:b9 rightstick:b10).
/// Written out literally rather than derived from `PAD_BUTTONS`, so a change to
/// the table cannot make this pass by agreeing with itself.
#[test]
#[ignore = "needs /dev/uinput and root: make test-uinput"]
fn uinput_gamepad_exposes_exactly_the_xpad_key_set() {
    let Some((devs, _)) = devices("uinput_gamepad_exposes_exactly_the_xpad_key_set") else {
        return;
    };
    let pad = open_evdev(&devs.gamepad_path);
    let exposed: Vec<u16> = pad
        .key_bits()
        .expect("EVIOCGBIT(EV_KEY)")
        .iter()
        .map(|k| k as u16)
        .collect();
    let xpad: Vec<u16> = [
        isys::BTN_SOUTH,
        isys::BTN_EAST,
        isys::BTN_NORTH,
        isys::BTN_WEST,
        isys::BTN_TL,
        isys::BTN_TR,
        isys::BTN_SELECT,
        isys::BTN_START,
        isys::BTN_MODE,
        isys::BTN_THUMBL,
        isys::BTN_THUMBR,
    ]
    .iter()
    .map(|&c| c as u16)
    .collect();
    assert_eq!(exposed, xpad);

    let kinds: Vec<EventKind> = pad.event_bits().expect("EVIOCGBIT(0)").iter().collect();
    assert!(kinds.contains(&EventKind::Key) && kinds.contains(&EventKind::Absolute));
    assert!(
        !kinds.contains(&EventKind::Relative),
        "a pad with REL axes is not xpad"
    );
}

/// The ABS set and ranges xpad uses: sticks signed 16-bit, triggers 0..255,
/// d-pad as a -1..1 hat.
#[test]
#[ignore = "needs /dev/uinput and root: make test-uinput"]
fn uinput_gamepad_exposes_the_xpad_axes_and_ranges() {
    let Some((devs, _)) = devices("uinput_gamepad_exposes_the_xpad_axes_and_ranges") else {
        return;
    };
    let pad = open_evdev(&devs.gamepad_path);
    let axes: Vec<AbsoluteAxis> = pad
        .absolute_bits()
        .expect("EVIOCGBIT(EV_ABS)")
        .iter()
        .collect();
    assert_eq!(
        axes,
        vec![
            AbsoluteAxis::X,
            AbsoluteAxis::Y,
            AbsoluteAxis::Z,
            AbsoluteAxis::RX,
            AbsoluteAxis::RY,
            AbsoluteAxis::RZ,
            AbsoluteAxis::Hat0X,
            AbsoluteAxis::Hat0Y,
        ]
    );
    let range = |a: AbsoluteAxis| {
        let info = pad.absolute_info(a).expect("EVIOCGABS");
        (info.minimum, info.maximum)
    };
    for a in [
        AbsoluteAxis::X,
        AbsoluteAxis::Y,
        AbsoluteAxis::RX,
        AbsoluteAxis::RY,
    ] {
        assert_eq!(range(a), (-32768, 32767), "{a:?}");
    }
    for a in [AbsoluteAxis::Z, AbsoluteAxis::RZ] {
        assert_eq!(range(a), (0, 255), "{a:?}");
    }
    for a in [AbsoluteAxis::Hat0X, AbsoluteAxis::Hat0Y] {
        assert_eq!(range(a), (-1, 1), "{a:?}");
    }
}

/// W3C Standard Gamepad input written through the public API arrives as the
/// xpad events: X on BTN_NORTH (#348), d-pad on the hat, triggers and sticks
/// on their ABS axes.
#[test]
#[ignore = "needs /dev/uinput and root: make test-uinput"]
fn uinput_gamepad_injection_reads_back_as_xpad_events() {
    let Some((devs, _)) = devices("uinput_gamepad_injection_reads_back_as_xpad_events") else {
        return;
    };
    let pad = open_evdev(&devs.gamepad_path);
    let mut buttons = vec![0.0; 17];
    let mut axes = vec![0.0; 4];

    buttons[2] = 1.0; // W3C X
    devs.gamepad(&buttons, &axes).unwrap();
    assert_eq!(read_frame(&pad), vec![key(isys::BTN_NORTH, 1)]);

    buttons[2] = 0.0;
    buttons[12] = 1.0; // d-pad up
    devs.gamepad(&buttons, &axes).unwrap();
    let frame = read_frame(&pad);
    assert!(frame.contains(&key(isys::BTN_NORTH, 0)), "{frame:?}");
    assert!(frame.contains(&abs(isys::ABS_HAT0Y, -1)), "{frame:?}");

    buttons[12] = 0.0;
    buttons[6] = 1.0; // LT
    buttons[16] = 1.0; // Guide
    axes[0] = -1.0; // left stick full left
    devs.gamepad(&buttons, &axes).unwrap();
    let frame = read_frame(&pad);
    for want in [
        abs(isys::ABS_HAT0Y, 0),
        abs(isys::ABS_Z, 255),
        abs(isys::ABS_X, -32768),
        key(isys::BTN_MODE, 1),
    ] {
        assert!(frame.contains(&want), "missing {want:?} in {frame:?}");
    }
}

/// A forwarded physical pad's `BTN_DPAD_*` is folded onto the hat; without
/// that the kernel would drop it, since the virtual pad declares no such key.
#[test]
#[ignore = "needs /dev/uinput and root: make test-uinput"]
fn uinput_forwarded_dpad_button_arrives_as_hat() {
    let Some((devs, _)) = devices("uinput_forwarded_dpad_button_arrives_as_hat") else {
        return;
    };
    let pad = open_evdev(&devs.gamepad_path);
    devs.forward_gamepad_frame(&[ev(isys::EV_KEY as u16, isys::BTN_DPAD_UP as u16, 1), syn()])
        .unwrap();
    assert_eq!(read_frame(&pad), vec![abs(isys::ABS_HAT0Y, -1)]);
    devs.forward_gamepad_frame(&[ev(isys::EV_KEY as u16, isys::BTN_DPAD_UP as u16, 0), syn()])
        .unwrap();
    assert_eq!(read_frame(&pad), vec![abs(isys::ABS_HAT0Y, 0)]);
}

/// Issue #350: a browser scroll-down (positive `dy`) is a NEGATIVE evdev wheel,
/// and fractional steps add up to whole detents.
#[test]
#[ignore = "needs /dev/uinput and root: make test-uinput"]
fn uinput_scroll_down_reads_back_as_negative_wheel() {
    let Some((devs, _)) = devices("uinput_scroll_down_reads_back_as_negative_wheel") else {
        return;
    };
    let mouse = open_evdev(&devs.mouse_path);

    devs.scroll(0.0, 120.0).unwrap();
    let frame = read_frame(&mouse);
    assert!(
        frame.contains(&rel(isys::REL_WHEEL_HI_RES, -120)),
        "{frame:?}"
    );
    assert!(frame.contains(&rel(isys::REL_WHEEL, -1)), "{frame:?}");

    // Firefox-sized steps: five of 48 units are 240 hi-res units, two detents.
    let (mut hi_res, mut detents) = (0, 0);
    for _ in 0..5 {
        devs.scroll(0.0, 48.0).unwrap();
        for (_, code, value) in read_frame(&mouse) {
            match code as i32 {
                isys::REL_WHEEL_HI_RES => hi_res += value,
                isys::REL_WHEEL => detents += value,
                _ => {}
            }
        }
    }
    assert_eq!((hi_res, detents), (-240, -2));

    // Horizontal keeps the browser's sign (positive = right).
    devs.scroll(120.0, 0.0).unwrap();
    let frame = read_frame(&mouse);
    assert!(frame.contains(&rel(isys::REL_HWHEEL, 1)), "{frame:?}");
}

#[test]
#[ignore = "needs /dev/uinput and root: make test-uinput"]
fn uinput_keyboard_and_mouse_buttons_read_back() {
    let Some((devs, _)) = devices("uinput_keyboard_and_mouse_buttons_read_back") else {
        return;
    };
    let kb = open_evdev(&devs.keyboard_path);
    devs.key(isys::KEY_A as u32, true).unwrap();
    assert_eq!(read_frame(&kb), vec![key(isys::KEY_A, 1)]);
    devs.key(isys::KEY_A as u32, false).unwrap();
    assert_eq!(read_frame(&kb), vec![key(isys::KEY_A, 0)]);

    let mouse = open_evdev(&devs.mouse_path);
    devs.mouse_button(isys::BTN_LEFT as u32, true).unwrap();
    assert_eq!(read_frame(&mouse), vec![key(isys::BTN_LEFT, 1)]);
    devs.release_all().unwrap();
    assert_eq!(read_frame(&mouse), vec![key(isys::BTN_LEFT, 0)]);
}

/// Dropping the devices removes them from the kernel and removes the exported
/// fake-udev directory, so a test run (or a session) leaves nothing behind.
#[test]
#[ignore = "needs /dev/uinput and root: make test-uinput"]
fn uinput_drop_removes_the_devices_and_udev_export() {
    let Some((devs, _)) = devices("uinput_drop_removes_the_devices_and_udev_export") else {
        return;
    };
    let export = tempfile::tempdir().unwrap();
    let dir = export.path().join("udev");
    devs.export_udev_data(&dir).unwrap();
    assert_eq!(std::fs::read_dir(&dir).unwrap().count(), 3);

    let sysfs: Vec<PathBuf> = [&devs.keyboard_path, &devs.mouse_path, &devs.gamepad_path]
        .iter()
        .map(|p| Path::new("/sys/class/input").join(p.file_name().unwrap()))
        .collect();
    assert!(sysfs.iter().all(|p| p.exists()));
    drop(devs);

    assert!(!dir.exists(), "udev export dir survived Drop");
    let deadline = std::time::Instant::now() + std::time::Duration::from_secs(2);
    while sysfs.iter().any(|p| p.exists()) {
        assert!(
            std::time::Instant::now() < deadline,
            "devices still registered after Drop: {sysfs:?}"
        );
        std::thread::sleep(std::time::Duration::from_millis(20));
    }
}
