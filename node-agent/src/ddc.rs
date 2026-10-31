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
//! A monitor shared between machines powers on to whichever input woke it, so power alone
//! is not "this host can be seen". The host's own input (VCP `0x60`) is
//! `QUASAR_CONSOLE_INPUT_SOURCE` when set, else learned: read once, from the first local
//! input while a console session displays, and kept per connector in a durable file.
//! Until one is known power alone decides, so a dedicated monitor needs no setting.
//!
//! Deploy requirements: host `modprobe i2c-dev`, `ddcutil` in the agent image, and
//! the `c 89:* rmw` device cgroup rule. `/dev/i2c-N` nodes are mknod'd here
//! (mirrors `session::virtual_input`'s fake-udev pattern) so compose need not
//! enumerate host-varying bus numbers.

use std::collections::{BTreeMap, HashMap};
use std::path::PathBuf;
use std::process::Command;
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
use std::sync::{Mutex, OnceLock};
use std::time::{Duration, Instant};

use quasar_runtime::DurableFile;

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
    /// Powered, but showing an input other than this host's (configured or learned): off
    /// as far as this host is concerned.
    OtherInput,
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

/// Whether a `config_update` has said if console mode is on. Until then the power
/// read cannot be trusted to have run, so no display is reported (see
/// `powered_connectors`).
static CONSOLE_CONFIG_SEEN: AtomicBool = AtomicBool::new(false);

/// Whether the "no console settings yet" line has been logged (once per process).
static CONFIG_WAIT_LOGGED: AtomicBool = AtomicBool::new(false);

/// How long a failed attempt to learn a connector's input waits before the next one. Short,
/// so a read caught while the monitor was still switching input is corrected at once.
const LEARN_RETRY: Duration = Duration::from_secs(8);

/// Attempts per connector before learning stops until console mode is toggled or the agent
/// restarts. Each attempt reads `0xd6` and, when the monitor is on, `0x60`: over a second of
/// i2c a desktop being played feels (#458). A monitor that answers neither would otherwise
/// be read for as long as someone plays.
const LEARN_ATTEMPTS: u8 = 3;

/// The inputs learned so far, per connector.
#[derive(Default)]
struct Learned {
    store: Option<DurableFile<BTreeMap<String, u8>>>,
    inputs: BTreeMap<String, u8>,
    /// When each connector's learning last ran and how many attempts it has had, so a
    /// monitor that will not answer is not read on every poll.
    tried: BTreeMap<String, (Instant, u8)>,
}

static LEARNED: Mutex<Learned> = Mutex::new(Learned {
    store: None,
    inputs: BTreeMap::new(),
    tried: BTreeMap::new(),
});

fn learned() -> std::sync::MutexGuard<'static, Learned> {
    LEARNED
        .lock()
        .unwrap_or_else(|poisoned| poisoned.into_inner())
}

impl Learned {
    /// The inputs saved at `path`; an unreadable file is learned afresh, not fatal.
    fn open(path: PathBuf) -> Self {
        let store = DurableFile::new(path, "console-input.tmp").with_mode(0o600);
        let loaded = store.load();
        let corrupt = loaded.is_err();
        let learned = Learned {
            inputs: loaded
                .unwrap_or_else(|err| {
                    tracing::warn!(token = "console-input-unreadable", %err,
                    "console: the saved monitor inputs cannot be read; learning them again");
                    None
                })
                .unwrap_or_default(),
            store: Some(store),
            tried: BTreeMap::new(),
        };
        // Replace a file that does not parse, or it warns on every restart.
        if corrupt {
            learned.persist();
        }
        learned
    }

    fn persist(&self) {
        if let Some(store) = &self.store {
            if let Err(err) = store.store(&self.inputs) {
                tracing::warn!(token = "console-input-persist-failed", %err,
                    "console: the learned monitor input could not be saved; it holds until restart");
            }
        }
    }

    /// Keeps `value` as `connector`'s input; false when it already was.
    fn record(&mut self, connector: &str, value: u8) -> bool {
        let changed = self.inputs.insert(connector.to_string(), value) != Some(value);
        if changed {
            self.persist();
        }
        changed
    }

    fn forget(&mut self) {
        if !self.inputs.is_empty() {
            self.inputs.clear();
            self.persist();
        }
        self.tried.clear();
    }

    /// The connectors still to learn: mapped to a bus, not learned, not tried lately.
    fn due(
        &mut self,
        connected: &[String],
        bus_of: impl Fn(&str) -> Option<u32>,
        now: Instant,
    ) -> Vec<(String, u32)> {
        let mut out = Vec::new();
        for conn in connected {
            let Some(bus) = bus_of(conn) else { continue };
            let (last, attempts) = self.tried.get(conn).copied().unzip();
            if self.inputs.contains_key(conn)
                || attempts.is_some_and(|n| n >= LEARN_ATTEMPTS)
                || last.is_some_and(|at| now.duration_since(at) < LEARN_RETRY)
            {
                continue;
            }
            self.tried
                .insert(conn.clone(), (now, attempts.unwrap_or(0) + 1));
            out.push((conn.clone(), bus));
        }
        out
    }
}

/// Where the learned inputs are kept, and what was kept. Called once per process,
/// before the first `config_update`.
pub(crate) fn set_state_path(path: impl Into<PathBuf>) {
    let opened = Learned::open(path.into());
    *learned() = opened;
}

/// `QUASAR_CONSOLE_INPUT_SOURCE`, parsed once.
static INPUT_PIN: OnceLock<Option<u8>> = OnceLock::new();

/// #407: whether this agent runs on a rootless container engine (`register`'s
/// `engine_mode`). A user namespace cannot `mknod`, so on rootless the recovery actor
/// passes each host `/dev/i2c-N` it found as a device instead of this module creating
/// them (`recipe::HostDevices::i2c`). Defaults `false` (rootful), matching every host
/// before the first `register` round-trip.
static ROOTLESS: AtomicBool = AtomicBool::new(false);

/// #411: count of `ddcutil` probe attempts, incremented before the fork so a test
/// can assert the console gate short-circuits before any subprocess work.
static PROBE_CALLS: AtomicUsize = AtomicUsize::new(0);

/// Latch the host's console-mode state. Called from the agent's `config_update`
/// handler; idempotent and lock-free (read on the hotplug watcher thread).
pub(crate) fn set_console_enabled(enabled: bool) {
    // Turning console mode off forgets the learned inputs: the way to learn again
    // after the host is cabled to another input.
    if !enabled {
        learned().forget();
    }
    CONSOLE_ENABLED.store(enabled, Ordering::Relaxed);
    CONSOLE_CONFIG_SEEN.store(true, Ordering::Relaxed);
}

fn console_enabled() -> bool {
    CONSOLE_ENABLED.load(Ordering::Relaxed)
}

/// Whether console mode is on, for `readiness::console`'s `skip`-while-off rule.
pub(crate) fn is_console_enabled() -> bool {
    console_enabled()
}

/// Latch the engine mode. Called once per connection alongside
/// `release::console::ConsoleAccessManager::set_engine_mode`.
pub(crate) fn set_rootless(rootless: bool) {
    ROOTLESS.store(rootless, Ordering::Relaxed);
}

fn rootless() -> bool {
    ROOTLESS.load(Ordering::Relaxed)
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

/// `QUASAR_CONSOLE_INPUT_SOURCE`: the monitor input (VCP `0x60` value) this host is
/// cabled to, for a monitor shared between machines. When set, a powered monitor
/// showing another input counts as off, and nothing is learned.
fn input_source_pin() -> Option<u8> {
    *INPUT_PIN.get_or_init(|| {
        parse_input_source(std::env::var("QUASAR_CONSOLE_INPUT_SOURCE").ok().as_deref())
    })
}

/// The input `connector` must show to count as this host's: the setting, else the one
/// learned. `None` until either is known: power alone decides.
fn input_for(connector: &str) -> Option<u8> {
    input_in(input_source_pin(), &learned(), connector)
}

fn input_in(setting: Option<u8>, learned: &Learned, connector: &str) -> Option<u8> {
    setting.or_else(|| learned.inputs.get(connector).copied())
}

/// `"0x10"` or `"16"` → `0x10`; empty → `None`; anything else warns and is `None`.
fn parse_input_source(value: Option<&str>) -> Option<u8> {
    let value = value?.trim();
    if value.is_empty() {
        return None;
    }
    let parsed = match value
        .strip_prefix("0x")
        .or_else(|| value.strip_prefix("0X"))
    {
        Some(hex) => u8::from_str_radix(hex, 16).ok(),
        None => value.parse::<u8>().ok(),
    };
    if parsed.is_none() {
        tracing::warn!(
            token = "console-input-source-invalid",
            value,
            "QUASAR_CONSOLE_INPUT_SOURCE is not a VCP 0x60 value (like 0x10 or 16); ignored"
        );
    }
    parsed
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

/// Whether this call should `mknod` — split out so the rootless/rootful choice is a
/// pure, directly testable decision rather than buried in the `mknod` loop.
fn should_mknod_i2c_nodes(rootless: bool) -> bool {
    !rootless
}

/// mknod `/dev/i2c-0..=MAX_I2C_MINOR` (major 89) if missing, once. Best-effort:
/// a failed/missing node just yields `Unknown` for that bus (not gated). Mirrors
/// `session::virtual_input::ensure_dev_node`.
///
/// On a rootless engine (#407) a user namespace cannot `mknod`: the recovery actor
/// already passed each host `/dev/i2c-N` it found as a device, so this is a no-op —
/// a bus the recipe did not pass through just yields `Unknown` for that connector,
/// same graceful downgrade as a failed mknod on a rootful host.
fn ensure_i2c_nodes() {
    NODES_READY.get_or_init(|| {
        if !should_mknod_i2c_nodes(rootless()) {
            return true;
        }
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
    powered(connected, true)
}

/// [`powered_connectors`] without `ddcutil detect`, which probes every bus: only the
/// connectors whose bus is already mapped are read, the rest count as powered. For a
/// read while a console desktop owns the display.
pub(crate) fn powered_connectors_on_known_bus(connected: Vec<String>) -> Vec<String> {
    powered(connected, false)
}

/// Why [`powered_connectors_on_known_bus`] could not read any of `connected`, or `None`
/// when it can. Forks nothing beyond the one `ddcutil --version` check.
pub(crate) fn known_bus_read_blocked(connected: &[String]) -> Option<&'static str> {
    if !CONSOLE_CONFIG_SEEN.load(Ordering::Relaxed) {
        return Some("no console settings yet");
    }
    if !console_enabled() {
        return Some("console mode is off");
    }
    if !enabled() {
        return Some("QUASAR_CONSOLE_DDC=0");
    }
    if !ddcutil_available() {
        return Some("ddcutil is not available");
    }
    let cache = cache().lock().unwrap();
    if !connected.iter().any(|c| cache.bus_map.contains_key(c)) {
        return Some("no i2c bus is mapped for the display");
    }
    None
}

/// A person is using this host's input devices while a console session displays on
/// `connector`, so the monitor is showing this host: remember which input that is. Needs
/// a mapped bus and no `QUASAR_CONSOLE_INPUT_SOURCE`; a connector already learned is not
/// read again, a failed attempt waits [`LEARN_RETRY`], and after [`LEARN_ATTEMPTS`] it stops.
/// The reads run on their own thread, off the hotplug poll.
pub(crate) fn learn_input(connector: &str) {
    if input_source_pin().is_some() || known_bus_read_blocked(&[connector.to_string()]).is_some() {
        return;
    }
    let due = {
        let cache = cache().lock().unwrap();
        learned().due(
            &[connector.to_string()],
            |c| cache.bus_map.get(c).copied(),
            Instant::now(),
        )
    };
    if due.is_empty() {
        return;
    }
    // Over a second of i2c per read: not on the 2 s hotplug poll's thread.
    let spawned = std::thread::Builder::new()
        .name("console-input-learn".into())
        .spawn(move || learn_from(&LEARNED, due, read_vcp_sl));
    if let Err(err) = spawned {
        tracing::warn!(token = "console-input-learn-spawn-failed", %err, "console: could not start the monitor input read");
    }
}

fn learn_from(
    state: &Mutex<Learned>,
    due: Vec<(String, u32)>,
    read: impl Fn(u32, &str) -> Option<u8>,
) {
    for (connector, bus) in due {
        let Some(value) = host_input(read(bus, "d6"), || read(bus, "60")) else {
            continue;
        };
        let changed = state
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner())
            .record(&connector, value);
        if changed {
            tracing::info!(
                token = "console-input-learned",
                %connector,
                input = format_args!("{value:#04x}"),
                "console: this host's monitor input learned from local input during a console session"
            );
        }
    }
}

/// The input to learn: a powered monitor's `0x60` reading. A monitor in standby may still
/// answer with the input it last had, which proves nothing about who is looking.
fn host_input(power_sl: Option<u8>, input: impl FnOnce() -> Option<u8>) -> Option<u8> {
    (power_sl == Some(0x01)).then(input).flatten()
}

fn powered(connected: Vec<String>, detect: bool) -> Vec<String> {
    powered_with(connected, detect, read_display)
}

/// [`powered`] with the per-connector read (`bus`, the input to compare) given.
fn powered_with(
    connected: Vec<String>,
    detect: bool,
    read: impl Fn(u32, Option<u8>) -> Power,
) -> Vec<String> {
    // Before the first config_update the agent does not know whether to read the
    // monitor's power, and an unread monitor must not look on: a report made then
    // started console mode on a switched-off monitor after an agent restart.
    // config_update follows register on every connection; the hotplug poll then
    // reports the display if it is on.
    if !CONSOLE_CONFIG_SEEN.load(Ordering::Relaxed) {
        if !connected.is_empty() && !CONFIG_WAIT_LOGGED.swap(true, Ordering::Relaxed) {
            tracing::info!(
                token = "console-display-awaits-config",
                "console: no console settings from the control plane yet; reporting no \
                 display until they arrive"
            );
        }
        return Vec::new();
    }
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
        (detect && (stale || missing), fresh)
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
                Some(&bus) => read(bus, input_for(conn)),
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
            let before = cache
                .power
                .insert(conn.clone(), (*power, at))
                .map(|(p, _)| p);
            if before != Some(*power) {
                tracing::info!(connector = %conn, power = ?power, before = ?before, "DDC: monitor state changed");
            }
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
            if matches!(power, Power::Off | Power::OtherInput) {
                tracing::debug!(connector = %conn, ?power, "DDC: monitor off for this host — connector gated out");
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

/// What `readiness::console::check_ddc` reads — a snapshot of what the hotplug poll has
/// already learned, not a fresh probe (a readiness check runs far more often than the
/// hotplug watcher's own cadence would justify forking `ddcutil` for).
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct DdcSummary {
    pub available: bool,
    pub bus_mapped: bool,
    pub any_read: bool,
    pub all_off: bool,
    /// A monitor reads on but shows another input than this host's.
    pub other_input: bool,
    /// This host's input (VCP `0x60`), when the setting or a learned reading gives one.
    pub input: Option<u8>,
    /// `input` was learned rather than set.
    pub input_learned: bool,
}

pub(crate) fn summary() -> DdcSummary {
    let available = ddcutil_available();
    let planned = crate::readiness::console::configured_output_id().and_then(|id| {
        id.split_once(':')
            .map(|(_, connector)| connector.to_string())
    });
    let cache = cache().lock().unwrap();
    let (other_input, input, input_learned) = input_summary(
        planned.as_deref(),
        &cache.power,
        &learned().inputs,
        input_source_pin(),
    );
    let any_read = !cache.power.is_empty();
    DdcSummary {
        available,
        bus_mapped: !cache.bus_map.is_empty(),
        any_read,
        other_input,
        input,
        input_learned,
        all_off: any_read
            && cache
                .power
                .values()
                .all(|(p, _)| matches!(p, Power::Off | Power::OtherInput)),
    }
}

/// Whether a monitor shows another input, and the host's input, for the connector the
/// console is planned on; without a pinned connector, for those that show another input.
fn input_summary(
    planned: Option<&str>,
    power: &HashMap<String, (Power, Instant)>,
    inputs: &BTreeMap<String, u8>,
    setting: Option<u8>,
) -> (bool, Option<u8>, bool) {
    let other: Vec<&String> = power
        .iter()
        .filter(|(c, (p, _))| matches!(p, Power::OtherInput) && planned.is_none_or(|n| *c == n))
        .map(|(c, _)| c)
        .collect();
    let learned_input = match planned {
        Some(c) => inputs.get(c).copied(),
        None => other
            .iter()
            .find_map(|c| inputs.get(*c).copied())
            .or_else(|| inputs.values().next().copied()),
    };
    (
        !other.is_empty(),
        setting.or(learned_input),
        setting.is_none() && learned_input.is_some(),
    )
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

/// The monitor's state on a bus: power from VCP `0xd6`, then, when an input is
/// pinned and the monitor is on, its input from VCP `0x60`.
fn read_display(bus: u32, pin: Option<u8>) -> Power {
    display_state(read_vcp_sl(bus, "d6"), pin, || read_vcp_sl(bus, "60"))
}

/// `0xd6` `sl=0x01` → On; any other value → Off; no reading → `Unknown` (not
/// gated). An On monitor whose `0x60` input is not the pin → `OtherInput`; an
/// input that cannot be read leaves it On (the same graceful downgrade).
fn display_state(
    power_sl: Option<u8>,
    pin: Option<u8>,
    input: impl FnOnce() -> Option<u8>,
) -> Power {
    match power_sl {
        Some(0x01) => match pin {
            Some(pin) => match input() {
                Some(shown) if shown != pin => {
                    tracing::debug!(shown, pin, "DDC: monitor on, showing another input");
                    Power::OtherInput
                }
                _ => Power::On,
            },
            None => Power::On,
        },
        Some(_) => Power::Off,
        None => Power::Unknown,
    }
}

/// Read one VCP feature's `sl` byte on a bus; a failed run or unparseable output
/// is `None`.
fn read_vcp_sl(bus: u32, code: &str) -> Option<u8> {
    // --sleep-multiplier 2: a bare read is flaky on this hardware ("DDC
    // communication failed" ~1-in-3), which would otherwise spuriously read Unknown.
    PROBE_CALLS.fetch_add(1, Ordering::Relaxed);
    let out = Command::new("ddcutil")
        .args([
            "getvcp",
            code,
            "--bus",
            &bus.to_string(),
            "--sleep-multiplier",
            "2",
        ])
        .output()
        .ok()?;
    if !out.status.success() {
        return None;
    }
    parse_sl(&String::from_utf8_lossy(&out.stdout))
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

        // No config_update yet: no display reported and no probe, whatever is
        // plugged in (an agent restart must not start console mode on its own).
        CONSOLE_CONFIG_SEEN.store(false, Ordering::Relaxed);
        let before_config = probe_calls();
        assert!(powered_connectors(vec!["DP-9".to_string()]).is_empty());
        assert_eq!(probe_calls(), before_config);

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

        // The read while a console desktop owns the display never runs `ddcutil
        // detect`: an unmapped connector is kept, with no fork at all.
        cache().lock().unwrap().bus_map.remove("DP-7");
        let before_known = probe_calls();
        assert_eq!(
            powered_connectors_on_known_bus(vec!["DP-7".to_string()]),
            vec!["DP-7".to_string()]
        );
        assert_eq!(
            probe_calls(),
            before_known,
            "ddcutil detect on the known-bus read"
        );
        if forced_available && enabled() {
            assert_eq!(
                known_bus_read_blocked(&["DP-7".to_string()]),
                Some("no i2c bus is mapped for the display")
            );
        }
        // The power read compares each connector with the input learned for it (not
        // another's), and the setting wins: what the read is handed is what the gate uses.
        if forced_available && enabled() && input_source_pin().is_none() {
            set_console_enabled(true);
            {
                let mut cache = cache().lock().unwrap();
                cache.bus_map.insert("DP-6".to_string(), 6);
                cache.bus_map.insert("DP-5".to_string(), 5);
                cache.power.remove("DP-6");
                cache.power.remove("DP-5");
            }
            learned().inputs.insert("DP-6".to_string(), 0x10);
            let seen = std::sync::Mutex::new(Vec::new());
            let kept = powered_with(
                vec!["DP-6".to_string(), "DP-5".to_string()],
                false,
                |bus, pin| {
                    seen.lock().unwrap().push((bus, pin));
                    if pin == Some(0x10) {
                        Power::OtherInput
                    } else {
                        Power::On
                    }
                },
            );
            let mut seen = seen.into_inner().unwrap();
            seen.sort();
            assert_eq!(seen, vec![(5, None), (6, Some(0x10))], "{seen:?}");
            assert_eq!(kept, vec!["DP-5".to_string()], "DP-6 shows another input");
            learned().inputs.remove("DP-6");
        }
        set_console_enabled(false);
        assert_eq!(
            known_bus_read_blocked(&["DP-7".to_string()]),
            Some("console mode is off")
        );
    }

    #[test]
    fn input_source_parses_hex_and_decimal() {
        assert_eq!(parse_input_source(Some("0x10")), Some(0x10));
        assert_eq!(parse_input_source(Some(" 0X0f ")), Some(0x0f));
        assert_eq!(parse_input_source(Some("17")), Some(17));
        assert_eq!(parse_input_source(Some("")), None);
        assert_eq!(parse_input_source(None), None);
        assert_eq!(parse_input_source(Some("DisplayPort-2")), None);
        assert_eq!(parse_input_source(Some("0x100")), None);
    }

    #[test]
    fn a_monitor_counts_only_when_on_and_showing_the_pinned_input() {
        let never = || -> Option<u8> { panic!("input read without a pin or while off") };
        assert_eq!(display_state(Some(0x01), None, never), Power::On);
        assert_eq!(display_state(Some(0x04), Some(0x10), never), Power::Off);
        assert_eq!(display_state(None, Some(0x10), never), Power::Unknown);
        assert_eq!(
            display_state(Some(0x01), Some(0x10), || Some(0x10)),
            Power::On
        );
        assert_eq!(
            display_state(Some(0x01), Some(0x10), || Some(0x0f)),
            Power::OtherInput
        );
        // An unreadable input keeps the monitor on, like an unreadable power state.
        assert_eq!(display_state(Some(0x01), Some(0x10), || None), Power::On);
    }

    fn learned_at(dir: &tempfile::TempDir) -> Learned {
        Learned::open(dir.path().join("agent.console-input.json"))
    }

    /// Only a powered monitor's input is this host's: standby or an unreadable power state
    /// reads nothing, and an unreadable input learns nothing.
    #[test]
    fn only_a_powered_monitors_input_is_learned() {
        let never = || -> Option<u8> { panic!("input read without power") };
        assert_eq!(host_input(Some(0x01), || Some(0x10)), Some(0x10));
        assert_eq!(host_input(Some(0x04), never), None);
        assert_eq!(host_input(None, never), None);
        assert_eq!(host_input(Some(0x01), || None), None);
    }

    /// The learned input survives a restart, is the one a later reading is compared with,
    /// and turning console mode off forgets it on disk too.
    #[test]
    fn a_learned_input_persists_gates_and_is_forgotten() {
        let dir = tempfile::tempdir().unwrap();
        let mut state = learned_at(&dir);
        assert!(state.record("DP-4", 0x10));
        assert!(!state.record("DP-4", 0x10), "same value is no change");

        let mut again = learned_at(&dir);
        // What the power read does with it: the other input counts as off, the setting
        // wins over what was learned, and an unlearned connector is gated by power alone.
        let pin = input_in(None, &again, "DP-4");
        assert_eq!(pin, Some(0x10));
        assert_eq!(
            display_state(Some(0x01), pin, || Some(0x0f)),
            Power::OtherInput
        );
        assert_eq!(display_state(Some(0x01), pin, || Some(0x10)), Power::On);
        assert_eq!(input_in(Some(0x11), &again, "DP-4"), Some(0x11));
        assert_eq!(input_in(None, &again, "HDMI-A-1"), None);

        again.forget();
        assert!(learned_at(&dir).inputs.is_empty());
    }

    #[test]
    fn an_unreadable_state_file_is_learned_afresh() {
        let dir = tempfile::tempdir().unwrap();
        std::fs::write(dir.path().join("agent.console-input.json"), b"{not json").unwrap();
        assert!(learned_at(&dir).inputs.is_empty());
    }

    /// Learning reads only a mapped, not-yet-learned connector, spaced by `LEARN_RETRY`, and
    /// after `LEARN_ATTEMPTS` nothing more is read until console mode is toggled.
    #[test]
    fn learning_gives_up_after_a_few_attempts() {
        let dir = tempfile::tempdir().unwrap();
        let mut state = learned_at(&dir);
        let bus_of = |c: &str| (c == "DP-4").then_some(5);
        let now = Instant::now();
        let conns = ["DP-4".to_string(), "HDMI-A-1".to_string()];

        let due = state.due(&conns, bus_of, now);
        assert_eq!(due, vec![("DP-4".to_string(), 5)], "unmapped is skipped");
        assert!(state
            .due(&conns, bus_of, now + LEARN_RETRY - Duration::from_secs(1))
            .is_empty());
        for attempt in 1..LEARN_ATTEMPTS {
            let at = now + LEARN_RETRY * u32::from(attempt);
            assert_eq!(state.due(&conns, bus_of, at).len(), 1, "attempt {attempt}");
        }
        for later in [u32::from(LEARN_ATTEMPTS), 10, 1000] {
            let at = now + LEARN_RETRY * later;
            assert!(
                state.due(&conns, bus_of, at).is_empty(),
                "no read after the cap"
            );
        }
        assert!(state
            .due(&conns, bus_of, now + Duration::from_secs(86_400))
            .is_empty());

        state.forget();
        assert_eq!(
            state.due(&conns, bus_of, now).len(),
            1,
            "a toggle starts afresh"
        );
        state.record("DP-4", 0x10);
        assert!(
            state.due(&conns, bus_of, now + LEARN_RETRY * 9).is_empty(),
            "learned"
        );
    }

    /// A file that does not parse is replaced, so it does not warn on every restart; the
    /// file is private to the agent.
    #[test]
    fn a_corrupt_state_file_is_rewritten_once_and_private() {
        use std::os::unix::fs::PermissionsExt;
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("agent.console-input.json");
        std::fs::write(&path, b"{not json").unwrap();
        let state = Learned::open(path.clone());
        assert!(state.inputs.is_empty());
        assert_eq!(std::fs::read_to_string(&path).unwrap(), "{}");
        let mut state = state;
        state.record("DP-4", 0x10);
        let mode = std::fs::metadata(&path).unwrap().permissions().mode();
        assert_eq!(mode & 0o777, 0o600);
    }

    /// Readiness names the input of the connector the console is planned on, not another
    /// monitor's.
    #[test]
    fn the_summary_names_the_planned_connectors_input() {
        let now = Instant::now();
        let power: HashMap<String, (Power, Instant)> = [
            ("DP-3".to_string(), (Power::On, now)),
            ("DP-4".to_string(), (Power::OtherInput, now)),
        ]
        .into();
        let inputs: BTreeMap<String, u8> = [("DP-3".into(), 0x0f), ("DP-4".into(), 0x10)].into();
        assert_eq!(
            input_summary(Some("DP-4"), &power, &inputs, None),
            (true, Some(0x10), true)
        );
        assert_eq!(
            input_summary(Some("DP-3"), &power, &inputs, None),
            (false, Some(0x0f), true)
        );
        assert_eq!(
            input_summary(None, &power, &inputs, None),
            (true, Some(0x10), true),
            "auto: the connector showing another input"
        );
        assert_eq!(
            input_summary(Some("DP-4"), &power, &inputs, Some(0x11)),
            (true, Some(0x11), false),
            "the setting wins"
        );
    }

    /// A monitor that is on teaches its 0x60 value; one in standby teaches nothing.
    #[test]
    fn learning_from_a_fake_monitor() {
        let fake = |power: u8, input: u8| {
            move |bus: u32, code: &str| {
                assert_eq!(bus, 5);
                Some(if code == "d6" { power } else { input })
            }
        };
        let state = Mutex::new(Learned::default());
        let due = || vec![("DP-4".to_string(), 5u32)];

        learn_from(&state, due(), fake(0x04, 0x0f));
        assert!(state.lock().unwrap().inputs.is_empty(), "standby");
        learn_from(&state, due(), fake(0x01, 0x10));
        assert_eq!(state.lock().unwrap().inputs.get("DP-4"), Some(&0x10));
    }

    /// #411: the TTL must exceed the console hotplug watcher's poll interval, or
    /// the cache expires on every tick and caches nothing.
    #[test]
    fn power_ttl_outlives_the_hotplug_poll_interval() {
        assert!(POWER_TTL > Duration::from_millis(2000));
    }

    #[test]
    fn rootless_never_mknods_i2c_nodes() {
        assert!(should_mknod_i2c_nodes(false), "rootful still creates them");
        assert!(
            !should_mknod_i2c_nodes(true),
            "rootless cannot mknod in a user namespace — the recipe passes the nodes instead"
        );
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
