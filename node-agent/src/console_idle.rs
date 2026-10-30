//! How long the host's local input has been quiet while a console desktop owns the
//! display. The DDC power read is a ~1 s i2c transfer a running desktop feels as a hitch
//! (#458), so while a console session runs it waits until nobody seems to be playing.
//!
//! Every `/dev/input/event*` node is opened read-only and drained on each poll; only the
//! time of the last user input is kept. User input is a key or button, relative motion,
//! or an absolute axis moving past the dead zone around its resting value and away from
//! where it last counted. Sync and misc events and accelerometer devices never count, so a
//! device that reports on its own cannot keep the console busy.
//!
//! Blind spots, each a read during play (a hitch, never a stop: the monitor reads on):
//! an axis pushed again to where it last counted, or moved within its dead zone, is quiet;
//! a device another program holds with `EVIOCGRAB` delivers nothing to this reader, and
//! evdev cannot tell a grab apart without grabbing, which would take the holder's events;
//! input that reaches the session only over hidraw is not seen either.

use std::collections::{BTreeMap, HashMap};
use std::fs::File;
use std::io::ErrorKind;
use std::os::unix::fs::OpenOptionsExt;
use std::path::Path;
use std::sync::Mutex;
use std::time::{Duration, Instant};

use input_linux::sys::{input_event, EV_ABS, EV_KEY, EV_REL};
use input_linux::{EvdevHandle, InputProperty};

/// One event node; `None` for a device that never counts (an accelerometer).
type Node = Option<Device>;

struct Device {
    handle: EvdevHandle<File>,
    axes: HashMap<u16, Axis>,
}

/// An absolute axis: its value at open (rest), its dead zone, and the value it last
/// counted at. Rest never moves, so noise wider than the zone cannot drag the zone along.
struct Axis {
    rest: i32,
    dead: i32,
    last: i32,
}

impl Axis {
    fn new(rest: i32, dead: i32) -> Self {
        Axis {
            rest,
            dead,
            last: rest,
        }
    }

    /// Noise inside the zone around rest is idle, and so is a value away from rest within
    /// a zone of where it last counted: a held or jittering deflection counts once. A
    /// value across rest from the last count must clear twice the zone, so noise flipping
    /// either side of rest goes quiet; a real push to the other side still counts.
    fn moved(&mut self, value: i32) -> bool {
        let gap = |a: i32, b: i32| (i64::from(a) - i64::from(b)).abs();
        let side = |v: i32| (i64::from(v) - i64::from(self.rest)).signum();
        let dead = i64::from(self.dead);
        let need = if side(value) * side(self.last) < 0 {
            2 * dead
        } else {
            dead
        };
        if gap(value, self.rest) <= need || gap(value, self.last) <= dead {
            return false;
        }
        self.last = value;
        true
    }
}

struct Watch {
    nodes: BTreeMap<String, Node>,
    last_input: Instant,
}

static WATCH: Mutex<Option<Watch>> = Mutex::new(None);

fn watch() -> std::sync::MutexGuard<'static, Option<Watch>> {
    WATCH
        .lock()
        .unwrap_or_else(|poisoned| poisoned.into_inner())
}

/// Closes the devices and restarts the quiet period at the next poll: called while no
/// console desktop owns a display, when one claims it, and while no read could happen.
pub(crate) fn reset() {
    *watch() = None;
}

/// How long no device under `dir` has had user input, or why that cannot be known
/// (input the agent cannot see may be someone playing).
pub(crate) fn idle_for(dir: &Path) -> Result<Duration, &'static str> {
    let now = Instant::now();
    watch()
        .get_or_insert_with(|| Watch::new(now))
        .poll(dir, now)
}

impl Device {
    fn open(path: &Path) -> std::io::Result<Node> {
        let file = std::fs::OpenOptions::new()
            .read(true)
            .custom_flags(libc::O_NONBLOCK)
            .open(path)?;
        let handle = EvdevHandle::new(file);
        // A node that answers no ioctl (a test FIFO) counts as an ordinary device.
        if handle
            .device_properties()
            .is_ok_and(|props| props.get(InputProperty::Accelerometer))
        {
            return Ok(None);
        }
        let mut axes = HashMap::new();
        for axis in handle.absolute_bits().iter().flat_map(|bits| bits.iter()) {
            if let Ok(info) = handle.absolute_info(axis) {
                let dead = dead_zone(info.minimum, info.maximum, info.flat, info.fuzz);
                axes.insert(axis as u16, Axis::new(info.value, dead));
            }
        }
        Ok(Some(Device { handle, axes }))
    }

    /// Drains the device; `None` once it is gone, else whether it had user input.
    fn drain(&mut self) -> Option<bool> {
        let mut events = [input_event {
            time: input_linux::sys::timeval {
                tv_sec: 0,
                tv_usec: 0,
            },
            type_: 0,
            code: 0,
            value: 0,
        }; 64];
        let mut input = false;
        // Bounded: a device streaming faster than this drains has input anyway.
        for _ in 0..16 {
            match self.handle.read(&mut events) {
                Ok(0) => return None,
                Ok(n) => {
                    for ev in &events[..n] {
                        input |= self.is_input(ev);
                    }
                }
                Err(e) if e.kind() == ErrorKind::WouldBlock => return Some(input),
                // ENODEV: unplugged; the next poll opens it again if the node is back.
                Err(_) => return None,
            }
        }
        Some(true)
    }

    fn is_input(&mut self, ev: &input_event) -> bool {
        match i32::from(ev.type_) {
            EV_KEY | EV_REL => true,
            // An axis open did not describe (a test FIFO) rests at its first value.
            EV_ABS => self
                .axes
                .entry(ev.code)
                .or_insert_with(|| Axis::new(ev.value, 0))
                .moved(ev.value),
            _ => false,
        }
    }
}

/// How far an absolute axis moves before it counts as someone touching it: past the
/// driver's own flat and fuzz, and past 1/32 of its range, so a resting stick's noise
/// is not play.
fn dead_zone(minimum: i32, maximum: i32, flat: i32, fuzz: i32) -> i32 {
    let span = (i64::from(maximum) - i64::from(minimum)) / 32;
    span.max(i64::from(flat))
        .max(i64::from(fuzz))
        .clamp(0, i64::from(i32::MAX)) as i32
}

impl Watch {
    fn new(now: Instant) -> Self {
        Watch {
            nodes: BTreeMap::new(),
            last_input: now,
        }
    }

    fn poll(&mut self, dir: &Path, now: Instant) -> Result<Duration, &'static str> {
        let names: Vec<String> = std::fs::read_dir(dir)
            .map_err(|_| "the input directory cannot be listed")?
            .flatten()
            .map(|e| e.file_name().to_string_lossy().into_owned())
            .filter(|name| name.starts_with("event"))
            .collect();
        self.nodes.retain(|name, _| names.contains(name));
        let mut unreadable = false;
        for name in names {
            if self.nodes.contains_key(&name) {
                continue;
            }
            match Device::open(&dir.join(&name)) {
                Ok(node) => {
                    self.nodes.insert(name, node);
                }
                Err(_) => unreadable = true,
            }
        }
        let mut input = false;
        self.nodes.retain(|_, node| match node {
            None => true,
            Some(device) => match device.drain() {
                Some(had) => {
                    input |= had;
                    true
                }
                None => false,
            },
        });
        if input {
            self.last_input = now;
        }
        if unreadable {
            return Err("an input device cannot be opened");
        }
        if !self.nodes.values().any(Option::is_some) {
            return Err("no input device");
        }
        Ok(now.saturating_duration_since(self.last_input))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::Write;

    fn fifo(dir: &Path, name: &str) -> File {
        let path = dir.join(name);
        let c = std::ffi::CString::new(path.to_str().unwrap()).unwrap();
        assert_eq!(unsafe { libc::mkfifo(c.as_ptr(), 0o600) }, 0);
        // The writer end keeps the read end from seeing EOF, like a live device.
        std::fs::OpenOptions::new()
            .read(true)
            .write(true)
            .open(path)
            .unwrap()
    }

    /// One `input_event` in the kernel's layout.
    fn event(type_: i32, code: u16, value: i32) -> Vec<u8> {
        let ev = input_event {
            time: input_linux::sys::timeval {
                tv_sec: 0,
                tv_usec: 0,
            },
            type_: type_ as u16,
            code,
            value,
        };
        let size = std::mem::size_of::<input_event>();
        unsafe { std::slice::from_raw_parts(&ev as *const input_event as *const u8, size) }.to_vec()
    }

    const SYN: i32 = input_linux::sys::EV_SYN;
    const MSC: i32 = input_linux::sys::EV_MSC;

    #[test]
    fn quiet_grows_until_a_device_has_input() {
        let dir = tempfile::tempdir().unwrap();
        let mut keyboard = fifo(dir.path(), "event3");
        let _pad = fifo(dir.path(), "event7");
        std::fs::write(dir.path().join("mouse0"), b"").unwrap();
        let start = Instant::now();
        let mut watch = Watch::new(start);

        assert_eq!(watch.poll(dir.path(), start), Ok(Duration::ZERO));
        let later = start + Duration::from_secs(600);
        assert_eq!(watch.poll(dir.path(), later), Ok(Duration::from_secs(600)));

        keyboard.write_all(&event(EV_KEY, 30, 1)).unwrap();
        assert_eq!(watch.poll(dir.path(), later), Ok(Duration::ZERO));
        let after = later + Duration::from_secs(5);
        assert_eq!(watch.poll(dir.path(), after), Ok(Duration::from_secs(5)));
    }

    /// A device that reports on its own (sync, misc, a resting stick's noise) never
    /// resets the quiet period; motion past the dead zone does.
    #[test]
    fn only_user_input_resets_the_quiet_period() {
        let dir = tempfile::tempdir().unwrap();
        let mut pad = fifo(dir.path(), "event5");
        let start = Instant::now();
        let mut watch = Watch::new(start);
        let quiet = |s| Ok(Duration::from_secs(s));

        let mut t = start;
        for step in 1..=3 {
            t = start + Duration::from_secs(step * 100);
            pad.write_all(&event(SYN, 0, 0)).unwrap();
            pad.write_all(&event(MSC, 4, 7)).unwrap();
            assert_eq!(watch.poll(dir.path(), t), quiet(step * 100), "sync/misc");
        }
        // A FIFO answers no EVIOCGABS, so the first value is the anchor, dead zone 0.
        pad.write_all(&event(EV_ABS, 0, 128)).unwrap();
        pad.write_all(&event(EV_ABS, 0, 128)).unwrap();
        assert_eq!(watch.poll(dir.path(), t), quiet(300), "an axis at rest");
        pad.write_all(&event(EV_ABS, 0, 200)).unwrap();
        assert_eq!(watch.poll(dir.path(), t), quiet(0), "an axis moved");
        pad.write_all(&event(EV_REL, 0, 1)).unwrap();
        let t2 = t + Duration::from_secs(9);
        assert_eq!(watch.poll(dir.path(), t2), quiet(0), "relative motion");
    }

    /// Noise wider than the dead zone, around rest or around a held deflection, keeps
    /// counting no longer than its first step; real motion still counts.
    #[test]
    fn an_axis_counts_motion_not_noise() {
        let mut trigger = Axis::new(3, dead_zone(0, 255, 0, 0));
        let busy: Vec<bool> = [3, 12, 2, 13, 1, 12, 3, 13]
            .iter()
            .map(|&v| trigger.moved(v))
            .collect();
        assert_eq!(busy.iter().filter(|&&b| b).count(), 1, "{busy:?}");
        assert!(trigger.moved(200), "a pull");
        assert!(!trigger.moved(195), "held");
        assert!(trigger.moved(120), "eased off");

        let mut hat = Axis::new(0, dead_zone(-1, 1, 0, 0));
        assert!(
            hat.moved(-1) && !hat.moved(0) && hat.moved(1),
            "any hat step"
        );
    }

    /// #811: noise wider than the zone on both sides of rest counts once, not on every
    /// flip; a real push to the other side still counts.
    #[test]
    fn two_sided_axis_noise_goes_quiet() {
        let mut stick = Axis::new(2048, dead_zone(0, 4095, 128, 0));
        let busy = [2248, 1848, 2248, 1848, 2248, 1848]
            .iter()
            .filter(|&&v| stick.moved(v))
            .count();
        assert_eq!(busy, 1);
        assert!(stick.moved(1700), "a push to the other side");
    }

    /// An axis that bounces across its zone for longer than the quiet window lets the
    /// quiet period grow.
    #[test]
    fn a_jittering_axis_goes_quiet() {
        let dir = tempfile::tempdir().unwrap();
        let mut pad = fifo(dir.path(), "event6");
        let start = Instant::now();
        let mut watch = Watch::new(start);
        // A FIFO answers no EVIOCGABS: rest is the first value, dead zone 0.
        pad.write_all(&event(EV_ABS, 2, 0)).unwrap();
        assert_eq!(watch.poll(dir.path(), start), Ok(Duration::ZERO));
        let mut t = start;
        for step in 1..=700u64 {
            t = start + Duration::from_secs(step);
            let value = if step % 2 == 0 { 0 } else { 9 };
            pad.write_all(&event(EV_ABS, 2, value)).unwrap();
            watch.poll(dir.path(), t).unwrap();
        }
        assert!(
            watch.poll(dir.path(), t).unwrap() >= Duration::from_secs(600),
            "a bouncing axis kept the console busy"
        );
    }

    #[test]
    fn a_dead_zone_covers_noise_and_scales_with_the_range() {
        assert_eq!(dead_zone(0, 255, 0, 0), 7);
        assert_eq!(dead_zone(-32768, 32767, 128, 16), 2047);
        assert_eq!(dead_zone(0, 255, 15, 0), 15);
        assert_eq!(dead_zone(-1, 1, 0, 0), 0, "a hat counts on any step");
        assert_eq!(dead_zone(i32::MIN, i32::MAX, 0, 0), i32::MAX / 16);
    }

    #[test]
    fn no_readable_device_is_never_quiet() {
        let dir = tempfile::tempdir().unwrap();
        let start = Instant::now();
        let mut watch = Watch::new(start - Duration::from_secs(3600));
        assert_eq!(watch.poll(dir.path(), start), Err("no input device"));
        assert!(watch.poll(&dir.path().join("missing"), start).is_err());
    }
}
