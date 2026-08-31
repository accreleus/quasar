//! CM-09 monitor **power** detection via DDC/CI (VCP feature `0xd6`).
//!
//! A monitor going to standby leaves its DRM connector `status=connected` with the
//! link up, so `/sys/class/drm/*/status` (capacity::detect_drm_connectors) can't
//! tell "on" from "off, still plugged" (confirmed on the 5090/DP-4: no OS-visible
//! signal on power-off). VCP `0xd6` reports the panel's own DPM state over I2C
//! instead: `0x01` = On, anything else = standby/off. We shell out to `ddcutil`:
//! map each connector to its I2C bus via `ddcutil detect` (nvidia exposes no
//! `/sys/.../ddc` symlink), then `getvcp d6 --bus N`, cached with a short TTL so
//! the 2s console hotplug poll doesn't spawn a probe storm.
//!
//! Graceful downgrade is the contract: `ddcutil` absent, bus unmapped, or a read
//! failing/parsing oddly all resolve to `Unknown` → treated as powered, i.e.
//! byte-identical to the pre-DDC always-on model. Gating is purely additive.
//!
//! Deploy requirements: host `modprobe i2c-dev`, `ddcutil` in the agent image, and
//! the `c 89:* rmw` device cgroup rule. `/dev/i2c-N` nodes are mknod'd here
//! (mirrors `session::virtual_input`'s fake-udev pattern) so compose need not
//! enumerate host-varying bus numbers.

use std::collections::HashMap;
use std::process::Command;
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
use std::sync::{Mutex, OnceLock};
use std::time::{Duration, Instant};

/// Per-connector power reading cache TTL. #411: must exceed the console hotplug
/// watcher's 2000ms poll interval or the cache expires every tick (was 1500ms,
/// forking ~43,200 real I2C transactions/connector/day). 10s coalesces ~5 polls
/// per read; console auto-start is not latency-sensitive.
const POWER_TTL: Duration = Duration::from_secs(10);

/// Connector→I2C-bus map reuse TTL. `ddcutil detect` is slow (~1-2s, probes all
/// buses) but stable for fixed cabling.
const BUS_MAP_TTL: Duration = Duration::from_secs(30);

/// Highest `/dev/i2c-N` minor pre-created. GPU DDC buses sit well under this;
/// a fixed span resolves the chicken-and-egg (detect needs the nodes, but the
/// per-connector bus is unknown until detect runs).
const MAX_I2C_MINOR: u32 = 31;

/// The I2C major number (`i2c-dev`) — `/dev/i2c-N` are `c 89:N`.
const I2C_MAJOR: u32 = 89;

#[derive(Clone, Copy, PartialEq, Eq, Debug)]
enum Power {
    On,
    Off,
    /// ddcutil missing / bus unknown / read failed → treated as powered (not gated).
    Unknown,
}

struct Cache {
    bus_map: HashMap<String, u32>,
    bus_map_at: Option<Instant>,
    power: HashMap<String, (Power, Instant)>,
}

static CACHE: OnceLock<Mutex<Cache>> = OnceLock::new();
static AVAILABLE: OnceLock<bool> = OnceLock::new();
static NODES_READY: OnceLock<bool> = OnceLock::new();

/// #411: whether console mode is enabled, latched from the control plane's
/// `config_update` (`console_config.enabled`). Gating exists only to decide
/// whether a console display counts as live, so with console mode off the probe
/// must not run at all. Defaults `false`, matching graceful-downgrade behaviour
/// before the first `config_update` arrives.
static CONSOLE_ENABLED: AtomicBool = AtomicBool::new(false);

/// #411: count of `ddcutil` probe attempts, incremented before the fork so a test
/// can assert the console gate short-circuits before any subprocess work.
static PROBE_CALLS: AtomicUsize = AtomicUsize::new(0);

/// Latch the host's console-mode state. Called from the agent's `config_update`
/// handler; idempotent and lock-free (read on the hotplug watcher thread).
pub(crate) fn set_console_enabled(enabled: bool) {
    CONSOLE_ENABLED.store(enabled, Ordering::Relaxed);
}

fn console_enabled() -> bool {
    CONSOLE_ENABLED.load(Ordering::Relaxed)
}

fn cache() -> &'static Mutex<Cache> {
    CACHE.get_or_init(|| {
        Mutex::new(Cache {
            bus_map: HashMap::new(),
            bus_map_at: None,
            power: HashMap::new(),
        })
    })
}

/// DDC gating is disabled by `QUASAR_CONSOLE_DDC=0`; any other value (or unset)
/// enables it *if* `ddcutil` is present.
fn enabled() -> bool {
    !matches!(
        std::env::var("QUASAR_CONSOLE_DDC").ok().as_deref(),
        Some("0")
    )
}

/// Whether `ddcutil` can be invoked (cached). A missing binary silently degrades
/// the whole module to the physical-connected behaviour.
fn ddcutil_available() -> bool {
    *AVAILABLE.get_or_init(|| match Command::new("ddcutil").arg("--version").output() {
        Ok(o) => o.status.success(),
        Err(_) => {
            tracing::debug!(
                "ddcutil not present — console DDC monitor-power gating disabled \
                     (connectors reported as physically connected)"
            );
            false
        }
    })
}

/// mknod `/dev/i2c-0..=MAX_I2C_MINOR` (major 89) if missing, once. Best-effort:
/// a failed/missing node just yields `Unknown` for that bus (not gated). Mirrors
/// `session::virtual_input::ensure_dev_node`.
fn ensure_i2c_nodes() {
    NODES_READY.get_or_init(|| {
        for minor in 0..=MAX_I2C_MINOR {
            let path = format!("/dev/i2c-{minor}");
            if std::path::Path::new(&path).exists() {
                continue;
            }
            let Ok(cpath) = std::ffi::CString::new(path.clone()) else {
                continue;
            };
            let mode = libc::S_IFCHR | 0o600;
            // Return value ignored: any failure leaves the node absent → Unknown.
            unsafe {
                libc::mknod(
                    cpath.as_ptr(),
                    mode,
                    libc::makedev(I2C_MAJOR as _, minor as _),
                )
            };
        }
        true
    });
}

/// Given the physically-connected DRM connector suffixes (e.g. `["DP-4"]`),
/// return only those whose monitor is not reporting DDC power **Off**. A
/// connector with power `On` or `Unknown` is kept. No-op (identity) when gating
/// is disabled, `ddcutil` is unavailable, or the input is empty.
pub(crate) fn powered_connectors(connected: Vec<String>) -> Vec<String> {
    // #411: console gate first — with console mode off nothing consumes the
    // reading, so no ddcutil fork is justified; return the raw connected list.
    if connected.is_empty() || !console_enabled() || !enabled() || !ddcutil_available() {
        return connected;
    }
    ensure_i2c_nodes();

    let now = Instant::now();

    // #411: ddcutil forks below must never run while holding the CACHE mutex —
    // getvcp with --sleep-multiplier 2 is hundreds of ms of real I2C and would
    // serialise every other capacity path behind it. Phases: read cache → fork
    // unlocked → commit; a concurrent caller may duplicate a probe in the seam,
    // which the TTL absorbs.

    // Phase 1 (locked): what is still fresh, and does the bus map need a refresh?
    let (need_map, fresh) = {
        let cache = cache().lock().unwrap();
        let stale = match cache.bus_map_at {
            Some(t) => now.duration_since(t) > BUS_MAP_TTL,
            None => true,
        };
        // Refresh if stale, or a requested connector has no mapping yet.
        let missing = connected.iter().any(|c| !cache.bus_map.contains_key(c));
        let fresh: HashMap<String, Power> = connected
            .iter()
            .filter_map(|conn| {
                cache
                    .power
                    .get(conn)
                    .filter(|(_, at)| now.duration_since(*at) < POWER_TTL)
                    .map(|(p, _)| (conn.clone(), *p))
            })
            .collect();
        (stale || missing, fresh)
    };

    // Phase 2 (unlocked): the `ddcutil detect` fork.
    let refreshed = if need_map { detect_bus_map() } else { None };

    // Phase 3 (locked): commit the map and snapshot it for the reads below.
    let bus_map = {
        let mut cache = cache().lock().unwrap();
        if need_map {
            cache.bus_map_at = Some(Instant::now());
            // Leave the previous map intact on a failed/empty detect.
            if let Some(map) = refreshed {
                if !map.is_empty() {
                    tracing::debug!(?map, "DDC: connector→i2c-bus map refreshed");
                    cache.bus_map = map;
                }
            }
        }
        cache.bus_map.clone()
    };

    // Phase 4 (unlocked): one `getvcp` fork per connector whose reading expired.
    let reads: Vec<(String, Power)> = connected
        .iter()
        .filter(|conn| !fresh.contains_key(*conn))
        .map(|conn| {
            let power = match bus_map.get(conn) {
                Some(&bus) => read_power(bus),
                None => Power::Unknown,
            };
            (conn.clone(), power)
        })
        .collect();

    // Phase 5 (locked): commit the readings.
    if !reads.is_empty() {
        let at = Instant::now();
        let mut cache = cache().lock().unwrap();
        for (conn, power) in &reads {
            cache.power.insert(conn.clone(), (*power, at));
        }
    }

    connected
        .into_iter()
        .filter(|conn| {
            let power = fresh
                .get(conn)
                .copied()
                .or_else(|| {
                    reads
                        .iter()
                        .find(|(c, _)| c == conn)
                        .map(|(_, power)| *power)
                })
                .unwrap_or(Power::Unknown);
            if power == Power::Off {
                tracing::debug!(connector = %conn, "DDC: monitor power Off — connector gated out");
                false
            } else {
                true
            }
        })
        .collect()
}

/// Number of `ddcutil` probe attempts made so far (bus-map detect + power read).
#[cfg(test)]
fn probe_calls() -> usize {
    PROBE_CALLS.load(Ordering::Relaxed)
}

/// Read a fresh connector→bus map from `ddcutil detect`. Parses the paired
/// `I2C bus:  /dev/i2c-N` and `DRM connector:  cardX-<suffix>` lines. `None`
/// when the fork itself failed.
fn detect_bus_map() -> Option<HashMap<String, u32>> {
    PROBE_CALLS.fetch_add(1, Ordering::Relaxed);
    let out = Command::new("ddcutil").arg("detect").output().ok()?;
    let text = String::from_utf8_lossy(&out.stdout);
    let mut map = HashMap::new();
    let mut cur_bus: Option<u32> = None;
    for line in text.lines() {
        let l = line.trim();
        if let Some(rest) = l.strip_prefix("I2C bus:") {
            cur_bus = parse_i2c_bus(rest);
        } else if let Some(rest) = l
            .strip_prefix("DRM connector:")
            .or_else(|| l.strip_prefix("DRM_connector:"))
        {
            if let (Some(bus), Some(conn)) = (cur_bus, parse_connector(rest)) {
                map.insert(conn, bus);
            }
        }
    }
    Some(map)
}

/// Read VCP `0xd6` on a bus. `sl=0x01` → On; any other value → Off; a non-zero
/// exit or unparseable output → `Unknown` (not gated).
fn read_power(bus: u32) -> Power {
    // --sleep-multiplier 2: a bare read is flaky on this hardware ("DDC
    // communication failed" ~1-in-3), which would otherwise spuriously read Unknown.
    PROBE_CALLS.fetch_add(1, Ordering::Relaxed);
    let out = match Command::new("ddcutil")
        .args([
            "getvcp",
            "d6",
            "--bus",
            &bus.to_string(),
            "--sleep-multiplier",
            "2",
        ])
        .output()
    {
        Ok(o) => o,
        Err(_) => return Power::Unknown,
    };
    if !out.status.success() {
        return Power::Unknown;
    }
    let text = String::from_utf8_lossy(&out.stdout);
    match parse_sl(&text) {
        Some(0x01) => Power::On,
        Some(_) => Power::Off,
        None => Power::Unknown,
    }
}

/// Extract the `sl=0xNN` byte from a `ddcutil getvcp` line.
fn parse_sl(text: &str) -> Option<u8> {
    let idx = text.find("sl=0x")?;
    let hex: String = text[idx + 5..]
        .chars()
        .take(2)
        .take_while(|c| c.is_ascii_hexdigit())
        .collect();
    u8::from_str_radix(&hex, 16).ok()
}

/// `"  /dev/i2c-5"` or `"5"` → `5`.
fn parse_i2c_bus(s: &str) -> Option<u32> {
    let s = s.trim();
    let tail = s.rsplit("i2c-").next().unwrap_or(s).trim();
    tail.parse().ok()
}

/// `"           card0-DP-4"` → `"DP-4"`.
fn parse_connector(s: &str) -> Option<String> {
    let s = s.trim();
    let (card, rest) = s.split_once('-')?;
    if !card.starts_with("card") || rest.is_empty() {
        return None;
    }
    Some(rest.to_string())
}

#[cfg(test)]
mod tests {
    use super::*;

    /// #411: with console mode disabled, must not reach a ddcutil fork at all —
    /// not even bus-map detect. Forces the availability latch so the console gate
    /// is the only thing standing between the call and a fork. Both phases live
    /// in ONE test since AVAILABLE/CONSOLE_ENABLED are process globals that would
    /// race across separate tests.
    #[test]
    fn console_gate_blocks_every_ddcutil_probe() {
        // Force "present" regardless of PATH; tolerate an already-latched false.
        let _ = AVAILABLE.set(true);
        let forced_available = *AVAILABLE.get().unwrap();

        set_console_enabled(false);
        let before = probe_calls();
        let input = vec!["DP-9".to_string(), "HDMI-A-9".to_string()];
        let out = powered_connectors(input.clone());
        assert_eq!(out, input, "console disabled must be an identity filter");
        assert_eq!(
            probe_calls(),
            before,
            "console mode is disabled — no ddcutil fork is justified"
        );

        // Console mode on: probe path reachable again (fork may fail in CI).
        set_console_enabled(true);
        let kept = powered_connectors(vec!["DP-8".to_string()]);
        if forced_available {
            assert!(
                probe_calls() > before,
                "console enabled must re-arm the DDC probe"
            );
        }
        // A failed/absent read is `Unknown` → connector kept (graceful downgrade).
        assert_eq!(kept, vec!["DP-8".to_string()]);
        set_console_enabled(false);
    }

    /// #411: the TTL must exceed the console hotplug watcher's poll interval, or
    /// the cache expires on every tick and caches nothing.
    #[test]
    fn power_ttl_outlives_the_hotplug_poll_interval() {
        assert!(POWER_TTL > Duration::from_millis(2000));
    }

    #[test]
    fn parses_power_byte() {
        assert_eq!(parse_sl("(sl=0x01)"), Some(0x01));
        assert_eq!(
            parse_sl("VCP code 0xd6 (Power mode): DPM: Off, DPMS: Off (sl=0x04)"),
            Some(0x04)
        );
        assert_eq!(parse_sl("no marker here"), None);
        assert_eq!(parse_sl("sl=0x"), None);
    }

    #[test]
    fn parses_i2c_bus() {
        assert_eq!(parse_i2c_bus("  /dev/i2c-5"), Some(5));
        assert_eq!(parse_i2c_bus("/dev/i2c-12"), Some(12));
        assert_eq!(parse_i2c_bus("7"), Some(7));
        assert_eq!(parse_i2c_bus("garbage"), None);
    }

    #[test]
    fn parses_connector() {
        assert_eq!(parse_connector("   card0-DP-4").as_deref(), Some("DP-4"));
        assert_eq!(
            parse_connector("card1-HDMI-A-1").as_deref(),
            Some("HDMI-A-1")
        );
        assert_eq!(parse_connector("renderD128"), None);
        assert_eq!(parse_connector("card0-"), None);
    }

    #[test]
    fn parses_ddcutil_detect_connector_labels() {
        for line in ["DRM connector: card0-DP-4", "DRM_connector: card0-DP-4"] {
            let rest = line
                .strip_prefix("DRM connector:")
                .or_else(|| line.strip_prefix("DRM_connector:"))
                .unwrap();
            assert_eq!(parse_connector(rest).as_deref(), Some("DP-4"));
        }
    }
}
