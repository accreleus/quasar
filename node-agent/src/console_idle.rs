//! How long the host's local input has been quiet while a console desktop owns the
//! display. The DDC power read is a ~1 s i2c transfer a running desktop feels as a hitch
//! (#458), so while a console session runs it waits until nobody can be playing.
//!
//! Every `/dev/input/event*` node is opened read-only and drained on each poll; the events
//! are discarded, only their arrival counts. Virtual devices count too: a game controller
//! another program holds exclusively still drives the virtual pad it feeds.

use std::collections::BTreeMap;
use std::fs::File;
use std::io::{ErrorKind, Read};
use std::os::unix::fs::OpenOptionsExt;
use std::path::Path;
use std::sync::Mutex;
use std::time::{Duration, Instant};

struct Watch {
    devices: BTreeMap<String, File>,
    last_input: Instant,
}

static WATCH: Mutex<Option<Watch>> = Mutex::new(None);

fn watch() -> std::sync::MutexGuard<'static, Option<Watch>> {
    WATCH
        .lock()
        .unwrap_or_else(|poisoned| poisoned.into_inner())
}

/// Closes the devices and restarts the quiet period at the next poll: called while no
/// console desktop owns a display, and when one claims it.
pub(crate) fn reset() {
    *watch() = None;
}

/// How long no device under `dir` has had an event, or `None` while some device cannot
/// be read: input the agent cannot see may be someone playing.
pub(crate) fn idle_for(dir: &Path) -> Option<Duration> {
    let now = Instant::now();
    watch()
        .get_or_insert_with(|| Watch::new(now))
        .poll(dir, now)
}

impl Watch {
    fn new(now: Instant) -> Self {
        Watch {
            devices: BTreeMap::new(),
            last_input: now,
        }
    }

    fn poll(&mut self, dir: &Path, now: Instant) -> Option<Duration> {
        let names: Vec<String> = std::fs::read_dir(dir)
            .ok()?
            .flatten()
            .map(|e| e.file_name().to_string_lossy().into_owned())
            .filter(|name| name.starts_with("event"))
            .collect();
        self.devices.retain(|name, _| names.contains(name));
        let mut unreadable = false;
        for name in names {
            if self.devices.contains_key(&name) {
                continue;
            }
            match std::fs::OpenOptions::new()
                .read(true)
                .custom_flags(libc::O_NONBLOCK)
                .open(dir.join(&name))
            {
                Ok(file) => {
                    self.devices.insert(name, file);
                }
                Err(_) => unreadable = true,
            }
        }
        let mut input = false;
        let mut buf = [0u8; 4096];
        self.devices.retain(|_, file| {
            // Bounded: a device streaming faster than this drains counts as input anyway.
            for _ in 0..64 {
                match file.read(&mut buf) {
                    Ok(0) => return false,
                    Ok(_) => input = true,
                    Err(e) if e.kind() == ErrorKind::WouldBlock => return true,
                    // ENODEV: unplugged; the next poll opens it again if the node is back.
                    Err(_) => return false,
                }
            }
            true
        });
        if input {
            self.last_input = now;
        }
        if unreadable || self.devices.is_empty() {
            return None;
        }
        Some(now.saturating_duration_since(self.last_input))
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

    #[test]
    fn quiet_grows_until_a_device_has_input() {
        let dir = tempfile::tempdir().unwrap();
        let mut keyboard = fifo(dir.path(), "event3");
        let _pad = fifo(dir.path(), "event7");
        std::fs::write(dir.path().join("mouse0"), b"").unwrap();
        let start = Instant::now();
        let mut watch = Watch::new(start);

        assert_eq!(watch.poll(dir.path(), start), Some(Duration::ZERO));
        let later = start + Duration::from_secs(600);
        assert_eq!(
            watch.poll(dir.path(), later),
            Some(Duration::from_secs(600))
        );

        keyboard.write_all(&[0u8; 24]).unwrap();
        assert_eq!(watch.poll(dir.path(), later), Some(Duration::ZERO));
        let after = later + Duration::from_secs(5);
        assert_eq!(watch.poll(dir.path(), after), Some(Duration::from_secs(5)));
    }

    #[test]
    fn no_readable_device_is_never_quiet() {
        let dir = tempfile::tempdir().unwrap();
        let start = Instant::now();
        let mut watch = Watch::new(start - Duration::from_secs(3600));
        assert_eq!(watch.poll(dir.path(), start), None, "no device");
        assert_eq!(watch.poll(&dir.path().join("missing"), start), None);
    }
}
