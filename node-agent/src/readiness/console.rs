//! Console mode's grants for direct display (agent-api.md amendment 19, "Console checks";
//! #460): `console_card`, `console_input`, `console_sound`, `console_terminal`,
//! `console_udev` and `console_ddc`. Reported on every host, `skip` while console mode is
//! off, never `blocks`: a console session's container is given the console GPU's card
//! node, the input devices, the sound device and the host's udev data, and the agent holds
//! the console terminal. What the agent cannot grant fails here, naming the grant and the
//! fix, so a half-configured host explains itself instead of failing a launch.
//!
//! Each check judges facts gathered once per readiness probe ([`ConsoleView::live`]); the
//! judging is pure. Nothing here takes the display: the card is read the way the DRM
//! inventory reads it (read-only, mastership given back at once, never while a console
//! session holds the card), and the terminal is judged from what startup found taking it.

use std::path::{Path, PathBuf};

use crate::capacity::CardAccess;
use crate::messages::{DrmOutputCapability, ReadinessCheck};
use crate::session::console_plan::InputGrant;

pub const CHECK_CARD: &str = "console_card";
pub const CHECK_INPUT: &str = "console_input";
pub const CHECK_SOUND: &str = "console_sound";
pub const CHECK_TERMINAL: &str = "console_terminal";
pub const CHECK_UDEV: &str = "console_udev";
pub const CHECK_DDC: &str = "console_ddc";

const OFF_SUMMARY: &str = "Console mode is off for this host.";

/// How the agent is re-created with its console grants, said once for every check.
const RECREATE: &str = "then turn console mode off and on again from the host's Local \
                        console page, so the recovery actor re-creates the node agent with \
                        the host's devices (a Compose install: \
                        deploy/overlays/docker-compose.console.yml)";

/// The console settings the checks read, latched from `config_update` (the agent keeps
/// the full config for the session build; readiness only needs these two).
#[derive(Debug, Clone, Default)]
struct LatchedConfig {
    output_id: Option<String>,
    input_devices: serde_json::Value,
}

static CONFIG: std::sync::RwLock<Option<LatchedConfig>> = std::sync::RwLock::new(None);

/// Called from the agent's `config_update` handler with the host's console config.
pub(crate) fn set_config(output_id: Option<String>, input_devices: serde_json::Value) {
    if let Ok(mut slot) = CONFIG.write() {
        *slot = Some(LatchedConfig {
            output_id,
            input_devices,
        });
    }
}

fn config() -> LatchedConfig {
    CONFIG
        .read()
        .ok()
        .and_then(|g| g.clone())
        .unwrap_or_default()
}

/// What each check reads, gathered once per readiness probe.
#[derive(Debug, Clone, Default)]
pub struct ConsoleView {
    pub enabled: bool,
    /// The engine is rootless: host preparation runs with `--mode rootless`.
    pub rootless: bool,
    pub card: CardView,
    pub input: InputView,
    pub sound: SoundView,
    pub terminal: TerminalView,
    pub udev: UdevView,
    pub ddc: crate::ddc::DdcSummary,
}

/// The console card(s) judged and what reading each found.
#[derive(Debug, Clone, Default)]
pub struct CardView {
    /// `(node, access)`. Empty: no card node is visible to the agent at all.
    pub cards: Vec<(String, CardAccess)>,
    /// Who holds the display, from logind's state, when a card is held.
    pub holder: Option<String>,
}

/// The input devices a console session would be given.
#[derive(Debug, Clone, Default)]
pub struct InputView {
    /// The `input_devices` setting read as a grant.
    pub grant: Option<Result<InputGrant, String>>,
    /// `/dev/input` lists for the agent.
    pub dir: Option<Result<(), String>>,
    /// The nodes the grant passes in: an allowlist's nodes, or every physical input
    /// device on the host for `auto`.
    pub nodes: Vec<InputNode>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct InputNode {
    pub path: String,
    pub present: bool,
    /// The Quasar account can open it read-write (`access(2)`, which honours the host's
    /// ACLs): what the desktop does with it.
    pub openable: bool,
}

/// The sound device a console session would be given.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub enum SoundView {
    #[default]
    Missing,
    /// `/dev/snd` is there; the nodes in it the Quasar account cannot open read-write.
    Present { denied: Vec<String> },
}

/// The console terminal.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub enum TerminalView {
    /// A console session holds it now.
    Held,
    /// The host has no virtual terminals; there is nothing to hold.
    NoVts,
    /// The host has VTs and the agent cannot use the console one (the reason).
    Unusable(String),
    /// Not read yet.
    #[default]
    Unknown,
    /// The node is in the agent's container.
    Present {
        /// The agent can open it read-write.
        openable: bool,
        /// What startup found taking it; `None` when startup did not try.
        startup: Option<Result<(), String>>,
    },
}

/// The host's udev data, as the agent sees it.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub enum UdevView {
    #[default]
    Missing,
    Unreadable(String),
    Readable,
}

/// The cards a console session would use: the one the output pick names, or for `auto`
/// the card of the first connected output. With `auto` and no monitor, every card the
/// agent can see, since any of them may be the one.
pub fn console_cards(
    output_id: Option<&str>,
    outputs: &[DrmOutputCapability],
    visible_cards: &[String],
) -> Vec<String> {
    if let Some(card) = output_id
        .filter(|id| *id != "auto")
        .and_then(|id| id.split_once(':'))
        .map(|(card, _)| card)
    {
        return vec![card.to_string()];
    }
    match outputs.iter().find(|o| o.connected) {
        Some(output) => vec![output.card.clone()],
        None => visible_cards.to_vec(),
    }
}

/// The host's `/dev` as the agent sees it: `/host/dev` where a Compose install binds it,
/// otherwise its own.
fn host_dev() -> PathBuf {
    let host_dev = Path::new("/host/dev");
    if host_dev.is_dir() {
        host_dev.to_path_buf()
    } else {
        PathBuf::from("/dev")
    }
}

/// `access(2)` for read and write, which judges the caller's real ids and the node's ACL.
fn openable_rw(path: &Path) -> bool {
    use std::os::unix::ffi::OsStrExt as _;
    let Ok(c) = std::ffi::CString::new(path.as_os_str().as_bytes()) else {
        return false;
    };
    // SAFETY: a valid NUL-terminated path, no other pointers.
    unsafe { libc::access(c.as_ptr(), libc::R_OK | libc::W_OK) == 0 }
}

fn card_names(dri: &Path) -> Vec<String> {
    let mut cards: Vec<String> = std::fs::read_dir(dri)
        .into_iter()
        .flatten()
        .flatten()
        .filter_map(|e| {
            let name = e.file_name().to_string_lossy().into_owned();
            let digits = name.strip_prefix("card")?;
            (!digits.is_empty() && digits.bytes().all(|b| b.is_ascii_digit())).then_some(name)
        })
        .collect();
    cards.sort();
    cards
}

impl CardView {
    fn observe(output_id: Option<&str>) -> Self {
        let outputs = crate::capacity::detect_drm_outputs();
        let cards: Vec<(String, CardAccess)> =
            console_cards(output_id, &outputs, &card_names(Path::new("/dev/dri")))
                .into_iter()
                .map(|card| {
                    let access = crate::capacity::console_card_access(&card);
                    (format!("/dev/dri/{card}"), access)
                })
                .collect();
        let holder = cards
            .iter()
            .any(|(_, a)| *a == CardAccess::Held)
            .then(|| crate::session::console_preflight::name_holder(Path::new("/host/run")))
            .flatten();
        CardView { cards, holder }
    }
}

impl InputView {
    fn observe(input_devices: &serde_json::Value) -> Self {
        let grant = crate::session::console_plan::input_grant(input_devices);
        let dir = std::fs::read_dir("/dev/input")
            .map(|_| ())
            .map_err(|e| e.to_string());
        let paths: Vec<String> = match &grant {
            Ok(InputGrant::Nodes(nodes)) => nodes.clone(),
            Ok(InputGrant::All) => crate::capacity::detect_input_devices()
                .into_iter()
                .filter(|d| !d.label.starts_with("Quasar Virtual"))
                .map(|d| d.path)
                .collect(),
            Err(_) => Vec::new(),
        };
        let nodes = paths
            .into_iter()
            .map(|path| {
                let p = Path::new(&path);
                InputNode {
                    present: p.exists(),
                    openable: openable_rw(p),
                    path,
                }
            })
            .collect();
        InputView {
            grant: Some(grant),
            dir: Some(dir),
            nodes,
        }
    }
}

impl SoundView {
    fn observe(snd: &Path) -> Self {
        if !snd.is_dir() {
            return SoundView::Missing;
        }
        let mut denied: Vec<String> = std::fs::read_dir(snd)
            .into_iter()
            .flatten()
            .flatten()
            .filter(|e| {
                let name = e.file_name().to_string_lossy().into_owned();
                name.starts_with("controlC") || name.starts_with("pcmC")
            })
            .filter(|e| !openable_rw(&e.path()))
            .map(|e| format!("/dev/snd/{}", e.file_name().to_string_lossy()))
            .collect();
        denied.sort();
        SoundView::Present { denied }
    }
}

impl TerminalView {
    fn observe() -> Self {
        use crate::session::console_vt::{self, Presence};
        if console_vt::held() {
            return TerminalView::Held;
        }
        let node = console_vt::console_node();
        match console_vt::presence(Path::new("/sys/class/tty"), &node) {
            Presence::NoVts => TerminalView::NoVts,
            Presence::Unusable(why) => TerminalView::Unusable(why),
            Presence::Present => TerminalView::Present {
                openable: openable_rw(&node),
                startup: crate::session::console_preflight::last_terminal(),
            },
        }
    }
}

impl UdevView {
    /// The recovery actor binds the host's `/run/udev/data` at `/host/run/udev/data`; an
    /// agent on the host itself (or a Compose bind at the same path) reads its own.
    fn observe() -> Self {
        let path = [
            Path::new("/host/run/udev/data"),
            Path::new(crate::session::console_plan::UDEV_DATA),
        ]
        .into_iter()
        .find(|p| p.is_dir());
        match path {
            None => UdevView::Missing,
            Some(p) => match std::fs::read_dir(p) {
                Ok(_) => UdevView::Readable,
                Err(e) => UdevView::Unreadable(e.to_string()),
            },
        }
    }
}

impl ConsoleView {
    /// The live host. Gathers nothing while console mode is off: every check skips.
    pub fn live() -> Self {
        let enabled = crate::ddc::is_console_enabled();
        let ddc = crate::ddc::summary();
        if !enabled {
            return ConsoleView {
                ddc,
                ..Default::default()
            };
        }
        let config = config();
        ConsoleView {
            enabled,
            rootless: crate::ddc::is_rootless(),
            card: CardView::observe(config.output_id.as_deref()),
            input: InputView::observe(&config.input_devices),
            sound: SoundView::observe(&host_dev().join("snd")),
            terminal: TerminalView::observe(),
            udev: UdevView::observe(),
            ddc,
        }
    }

    /// Host preparation's command for this engine mode.
    fn prepare_host(&self) -> String {
        format!(
            "run host preparation with console mode as root on the host (sudo sh \
             prepare-host.sh --mode {} --console)",
            if self.rootless { "rootless" } else { "rootful" }
        )
    }
}

pub fn check_card(v: &ConsoleView) -> ReadinessCheck {
    if !v.enabled {
        return super::skip(CHECK_CARD, OFF_SUMMARY);
    }
    if v.card.cards.is_empty() {
        return super::fail(
            CHECK_CARD,
            "no display card node (/dev/dri/card*) is visible to this agent".into(),
            format!("Check the host has a GPU driving a display, {RECREATE}."),
        );
    }
    for (node, access) in &v.card.cards {
        match access {
            CardAccess::Missing => {
                return super::fail(
                    CHECK_CARD,
                    format!("the console card node {node} is not in this agent's container"),
                    format!("Check the console output names a card this host has, {RECREATE}."),
                )
            }
            CardAccess::Unopenable(err) => {
                return super::fail(
                    CHECK_CARD,
                    format!("the console card node {node} cannot be opened ({err})"),
                    format!(
                        "Grant the Quasar account the display cards: {}, {RECREATE}.",
                        v.prepare_host()
                    ),
                )
            }
            CardAccess::Held => {
                let who = v
                    .card
                    .holder
                    .clone()
                    .unwrap_or_else(|| "another program".into());
                return super::fail(
                    CHECK_CARD,
                    format!("{who} holds DRM master on {node}, so a console desktop cannot take the display"),
                    "Stop the desktop or login screen driving that card (for example its \
                     display manager), or pick an output on another card. On an NVIDIA host \
                     whose GPU reaches containers through CDI, a streamed session's app is \
                     given the card node too and may hold it: end that session."
                        .into(),
                );
            }
            CardAccess::Free | CardAccess::Claimed => {}
        }
    }
    let nodes: Vec<&str> = v.card.cards.iter().map(|(n, _)| n.as_str()).collect();
    if v.card.cards.iter().any(|(_, a)| *a == CardAccess::Claimed) {
        return super::pass(
            CHECK_CARD,
            format!(
                "the console session's desktop holds the display on {}",
                nodes.join(", ")
            ),
        );
    }
    super::pass(
        CHECK_CARD,
        format!(
            "{} can be passed to the console desktop, and no other program holds the display",
            nodes.join(", ")
        ),
    )
}

pub fn check_input(v: &ConsoleView) -> ReadinessCheck {
    if !v.enabled {
        return super::skip(CHECK_INPUT, OFF_SUMMARY);
    }
    let input = &v.input;
    let grant = match &input.grant {
        None => return super::unknown(CHECK_INPUT, "the input devices have not been read yet"),
        Some(Err(why)) => {
            return super::fail(
                CHECK_INPUT,
                format!("the console's input devices setting cannot be used: {why}"),
                "Set Input devices on the host's Local console page to Auto or to a list of \
                 /dev/input/event devices."
                    .into(),
            )
        }
        Some(Ok(grant)) => grant,
    };
    if let Some(Err(why)) = &input.dir {
        return super::fail(
            CHECK_INPUT,
            format!("/dev/input cannot be read by this agent ({why})"),
            format!("Check the host has /dev/input, {RECREATE}."),
        );
    }
    if let Some(gone) = input.nodes.iter().find(|n| !n.present) {
        return super::fail(
            CHECK_INPUT,
            format!("the listed input device {} is not on this host", gone.path),
            "Plug it in, or change Input devices on the host's Local console page \
             (device numbers can change when devices are replugged)."
                .into(),
        );
    }
    let denied: Vec<&str> = input
        .nodes
        .iter()
        .filter(|n| !n.openable)
        .map(|n| n.path.as_str())
        .collect();
    if let Some(first) = denied.first() {
        return super::fail(
            CHECK_INPUT,
            format!(
                "the Quasar account cannot open {} of the input devices the console desktop \
                 would get (first: {first})",
                denied.len()
            ),
            format!(
                "Grant the Quasar account the host's input devices: {}. The rule applies to \
                 devices as they appear, so replug a device it missed.",
                v.prepare_host()
            ),
        );
    }
    let later = if v.rootless {
        "a device plugged in later opens through host preparation's access rule"
    } else {
        "a device plugged in later opens through the input device-cgroup rule"
    };
    super::pass(
        CHECK_INPUT,
        match grant {
            InputGrant::All => format!(
                "{} input devices can be passed in with the whole /dev/input; {later}",
                input.nodes.len()
            ),
            InputGrant::Nodes(nodes) => {
                format!("the {} listed input devices can be passed in", nodes.len())
            }
        },
    )
}

pub fn check_sound(v: &ConsoleView) -> ReadinessCheck {
    if !v.enabled {
        return super::skip(CHECK_SOUND, OFF_SUMMARY);
    }
    match &v.sound {
        SoundView::Missing => super::fail(
            CHECK_SOUND,
            "no sound device (/dev/snd) is visible to this agent, so the console desktop \
             would have no sound"
                .into(),
            format!(
                "If the host has a sound card, check its driver is loaded, {RECREATE} (the \
                 sound device is read again each time)."
            ),
        ),
        SoundView::Present { denied } if !denied.is_empty() => super::fail(
            CHECK_SOUND,
            format!(
                "the Quasar account cannot open {} of the host's sound devices (first: {})",
                denied.len(),
                denied[0]
            ),
            format!(
                "Grant the Quasar account the sound devices: {}, {RECREATE}.",
                v.prepare_host()
            ),
        ),
        SoundView::Present { .. } => super::pass(
            CHECK_SOUND,
            "the host's sound device (/dev/snd) can be passed in".into(),
        ),
    }
}

pub fn check_terminal(v: &ConsoleView) -> ReadinessCheck {
    use crate::session::console_vt::CONSOLE_VT;
    if !v.enabled {
        return super::skip(CHECK_TERMINAL, OFF_SUMMARY);
    }
    let fix = |what: &str| {
        format!(
            "{what}: {} (it grants tty{CONSOLE_VT} and keeps login prompts off it), {RECREATE}.",
            v.prepare_host()
        )
    };
    match &v.terminal {
        TerminalView::Held => super::pass(
            CHECK_TERMINAL,
            format!("a console session holds tty{CONSOLE_VT}"),
        ),
        TerminalView::NoVts => super::pass(
            CHECK_TERMINAL,
            "this host has no virtual terminals, so there is none to hold".into(),
        ),
        TerminalView::Unknown => {
            super::unknown(CHECK_TERMINAL, "the console terminal has not been read yet")
        }
        TerminalView::Unusable(why) => super::fail(
            CHECK_TERMINAL,
            why.clone(),
            fix("Give the agent the terminal"),
        ),
        TerminalView::Present {
            openable: false, ..
        } => super::fail(
            CHECK_TERMINAL,
            format!("tty{CONSOLE_VT} cannot be opened read-write by this agent"),
            fix("Grant the Quasar account the console terminal"),
        ),
        TerminalView::Present {
            startup: Some(Err(why)),
            ..
        } => super::fail(
            CHECK_TERMINAL,
            format!("console terminal: {why}"),
            fix("Stop any login prompt on the console terminal"),
        ),
        TerminalView::Present { .. } => super::pass(
            CHECK_TERMINAL,
            format!("tty{CONSOLE_VT} can be held in graphics mode for a console session"),
        ),
    }
}

pub fn check_udev(v: &ConsoleView) -> ReadinessCheck {
    if !v.enabled {
        return super::skip(CHECK_UDEV, OFF_SUMMARY);
    }
    match &v.udev {
        UdevView::Readable => super::pass(
            CHECK_UDEV,
            "the host's udev data (/run/udev/data) can be passed in, so devices plugged in \
             later reach the desktop"
                .into(),
        ),
        UdevView::Missing => super::fail(
            CHECK_UDEV,
            "the host's udev data (/run/udev/data) is not visible to this agent, so the \
             console desktop would not know its input devices"
                .into(),
            format!("Check the host runs systemd-udevd, {RECREATE}."),
        ),
        UdevView::Unreadable(err) => super::fail(
            CHECK_UDEV,
            format!("the host's udev data (/run/udev/data) cannot be read ({err})"),
            format!("Check /run/udev/data on the host is readable by everyone, as udev makes it, {RECREATE}."),
        ),
    }
}

pub fn check_ddc(v: &ConsoleView) -> ReadinessCheck {
    if !v.enabled {
        return super::skip(CHECK_DDC, OFF_SUMMARY);
    }
    let d = &v.ddc;
    if !d.available {
        return super::warn_check(
            CHECK_DDC,
            "ddcutil is not present in this agent image; monitor-power detection is off \
             (every connected display is treated as powered on)"
                .into(),
            "Rebuild the node agent image with ddcutil, or ignore it: display detection \
             and hotplug still work without it."
                .into(),
        );
    }
    if !d.bus_mapped {
        return super::warn_check(
            CHECK_DDC,
            "no I2C bus is mapped to a display connector yet".into(),
            "Check /dev/i2c-* is present (host preparation grants it on a rootless engine) \
             and that a monitor is connected. On a rootless engine the recovery actor passes \
             only the /dev/i2c-N that are character devices: one that is a plain file (a \
             stale placeholder) is skipped and named in its log. Remove that file, load \
             i2c-dev, then turn console mode off and on again."
                .into(),
        );
    }
    if !d.any_read {
        return super::unknown(
            CHECK_DDC,
            "an I2C bus is mapped to a connector, but no monitor-power read has completed yet",
        );
    }
    if d.all_off {
        return super::warn_check(
            CHECK_DDC,
            "every mapped monitor currently reads DDC power off/standby".into(),
            "Turn the monitor on.".into(),
        );
    }
    super::pass(
        CHECK_DDC,
        "read at least one connected monitor's DDC power state".into(),
    )
}

/// Every console check, in report order.
pub fn checks(v: &ConsoleView) -> [ReadinessCheck; 6] {
    [
        check_card(v),
        check_input(v),
        check_sound(v),
        check_terminal(v),
        check_udev(v),
        check_ddc(v),
    ]
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::readiness::{FAIL, PASS, SKIP, UNKNOWN, WARN};

    fn on() -> ConsoleView {
        ConsoleView {
            enabled: true,
            rootless: true,
            card: CardView {
                cards: vec![("/dev/dri/card1".into(), CardAccess::Free)],
                holder: None,
            },
            input: InputView {
                grant: Some(Ok(InputGrant::All)),
                dir: Some(Ok(())),
                nodes: vec![node("/dev/input/event3", true, true)],
            },
            sound: SoundView::Present { denied: Vec::new() },
            terminal: TerminalView::Present {
                openable: true,
                startup: Some(Ok(())),
            },
            udev: UdevView::Readable,
            ddc: crate::ddc::DdcSummary {
                available: true,
                bus_mapped: true,
                any_read: true,
                all_off: false,
            },
        }
    }

    fn node(path: &str, present: bool, openable: bool) -> InputNode {
        InputNode {
            path: path.into(),
            present,
            openable,
        }
    }

    fn output(id: &str, connected: bool) -> DrmOutputCapability {
        let (card, connector) = id.split_once(':').unwrap();
        DrmOutputCapability {
            id: id.into(),
            card: card.into(),
            render_node: None,
            connector: connector.into(),
            connected,
            active_mode: None,
            modes: Vec::new(),
        }
    }

    /// A failing check names the grant and says what to run; none ever blocks.
    fn assert_fail(check: &ReadinessCheck, summary: &str, fix: &str) {
        assert_eq!(check.status, FAIL, "{check:?}");
        assert!(check.summary.contains(summary), "{summary}: {check:?}");
        assert!(check.remediation.contains(fix), "{fix}: {check:?}");
        assert!(check.blocks.is_none(), "{check:?}");
    }

    #[test]
    fn every_check_skips_while_console_mode_is_off_and_none_blocks() {
        let off = ConsoleView::default();
        let ids: Vec<String> = checks(&off).iter().map(|c| c.id.clone()).collect();
        assert_eq!(
            ids,
            [
                "console_card",
                "console_input",
                "console_sound",
                "console_terminal",
                "console_udev",
                "console_ddc"
            ]
        );
        for check in checks(&off) {
            assert_eq!(check.status, SKIP, "{check:?}");
            assert!(check.blocks.is_none(), "{check:?}");
        }
        // A fully granted host passes every one.
        for check in checks(&on()) {
            assert_eq!(check.status, PASS, "{check:?}");
            assert!(check.blocks.is_none(), "{check:?}");
        }
    }

    #[test]
    fn the_console_card_is_the_picked_outputs_or_the_connected_one() {
        let outputs = [output("card0:DP-1", false), output("card1:HDMI-A-1", true)];
        let visible = ["card0".to_string(), "card1".to_string()];
        assert_eq!(
            console_cards(Some("card0:DP-1"), &outputs, &visible),
            ["card0"]
        );
        for auto in [None, Some("auto")] {
            assert_eq!(console_cards(auto, &outputs, &visible), ["card1"]);
        }
        // Auto with no monitor: any card could be the one.
        assert_eq!(
            console_cards(None, &[output("card0:DP-1", false)], &visible),
            visible
        );
    }

    #[test]
    fn card_fails_naming_the_missing_grant_or_the_holder() {
        let with = |access: CardAccess| {
            let mut v = on();
            v.card.cards = vec![("/dev/dri/card1".into(), access)];
            v
        };
        assert_fail(
            &check_card(&with(CardAccess::Unopenable(
                "Permission denied (os error 13)".into(),
            ))),
            "/dev/dri/card1 cannot be opened (Permission denied",
            "sudo sh prepare-host.sh --mode rootless --console",
        );
        assert_fail(
            &check_card(&with(CardAccess::Missing)),
            "/dev/dri/card1 is not in this agent's container",
            "recovery actor re-creates the node agent",
        );
        let mut held = with(CardAccess::Held);
        assert_fail(
            &check_card(&held),
            "another program holds DRM master on /dev/dri/card1",
            "Stop the desktop or login screen",
        );
        held.card.holder = Some("gdm, the login screen".into());
        assert_fail(
            &check_card(&held),
            "gdm, the login screen holds DRM master on /dev/dri/card1",
            "CDI",
        );
        let mut none = on();
        none.card.cards.clear();
        assert_fail(
            &check_card(&none),
            "no display card node",
            "turn console mode off and on again",
        );
        // The console session's own desktop holding it is the point, not a fault.
        let claimed = check_card(&with(CardAccess::Claimed));
        assert_eq!(claimed.status, PASS, "{claimed:?}");
        assert!(claimed.summary.contains("console session's desktop"));
    }

    #[test]
    fn the_rootful_fix_names_its_own_mode() {
        let mut v = on();
        v.rootless = false;
        v.card.cards = vec![("/dev/dri/card0".into(), CardAccess::Unopenable("x".into()))];
        assert_fail(
            &check_card(&v),
            "/dev/dri/card0",
            "prepare-host.sh --mode rootful --console",
        );
    }

    #[test]
    fn input_fails_on_a_bad_setting_an_unreadable_directory_a_gone_or_a_closed_device() {
        let mut bad = on();
        bad.input.grant = Some(Err("input_devices must be \"auto\"".into()));
        assert_fail(&check_input(&bad), "input devices setting", "Input devices");

        let mut no_dir = on();
        no_dir.input.dir = Some(Err("No such file or directory".into()));
        assert_fail(
            &check_input(&no_dir),
            "/dev/input cannot be read",
            "recovery actor re-creates the node agent",
        );

        let mut gone = on();
        gone.input.grant = Some(Ok(InputGrant::Nodes(vec!["/dev/input/event9".into()])));
        gone.input.nodes = vec![node("/dev/input/event9", false, false)];
        assert_fail(
            &check_input(&gone),
            "/dev/input/event9 is not on this host",
            "Plug it in",
        );

        let mut closed = on();
        closed.input.nodes = vec![
            node("/dev/input/event3", true, true),
            node("/dev/input/event5", true, false),
        ];
        assert_fail(
            &check_input(&closed),
            "cannot open 1 of the input devices the console desktop would get (first: /dev/input/event5)",
            "prepare-host.sh --mode rootless --console",
        );

        let mut unread = on();
        unread.input.grant = None;
        assert_eq!(check_input(&unread).status, UNKNOWN);
    }

    #[test]
    fn input_passes_saying_how_a_later_device_opens_on_each_engine_mode() {
        let rootless = check_input(&on());
        assert_eq!(rootless.status, PASS);
        assert!(
            rootless.summary.contains("host preparation's access rule"),
            "{rootless:?}"
        );
        let mut rootful = on();
        rootful.rootless = false;
        let rootful = check_input(&rootful);
        assert!(
            rootful.summary.contains("device-cgroup rule"),
            "{rootful:?}"
        );
        // A host with no physical input yet is not a missing grant.
        let mut empty = on();
        empty.input.nodes.clear();
        assert_eq!(check_input(&empty).status, PASS);
    }

    #[test]
    fn sound_fails_when_absent_or_closed() {
        let mut missing = on();
        missing.sound = SoundView::Missing;
        assert_fail(
            &check_sound(&missing),
            "no sound device (/dev/snd)",
            "recovery actor re-creates the node agent",
        );
        let mut closed = on();
        closed.sound = SoundView::Present {
            denied: vec!["/dev/snd/controlC0".into()],
        };
        assert_fail(
            &check_sound(&closed),
            "cannot open 1 of the host's sound devices (first: /dev/snd/controlC0)",
            "prepare-host.sh --mode rootless --console",
        );
    }

    #[test]
    fn terminal_passes_held_or_absent_and_fails_naming_why() {
        for view in [TerminalView::Held, TerminalView::NoVts] {
            let mut v = on();
            v.terminal = view;
            assert_eq!(check_terminal(&v).status, PASS);
        }
        let mut unusable = on();
        unusable.terminal =
            TerminalView::Unusable("/dev/tty8 is not in the agent's container".into());
        assert_fail(
            &check_terminal(&unusable),
            "/dev/tty8 is not in the agent's container",
            "--console",
        );
        let mut closed = on();
        closed.terminal = TerminalView::Present {
            openable: false,
            startup: None,
        };
        assert_fail(
            &check_terminal(&closed),
            "tty8 cannot be opened read-write",
            "prepare-host.sh --mode rootless --console",
        );
        let mut getty = on();
        getty.terminal = TerminalView::Present {
            openable: true,
            startup: Some(Err(
                "tty8 is another session's terminal (a login prompt on it?)".into(),
            )),
        };
        assert_fail(
            &check_terminal(&getty),
            "console terminal: tty8 is another session's terminal",
            "keeps login prompts off it",
        );
        // Startup never tried (console mode was turned on without a restart): judged by
        // the node alone.
        let mut untried = on();
        untried.terminal = TerminalView::Present {
            openable: true,
            startup: None,
        };
        assert_eq!(check_terminal(&untried).status, PASS);
    }

    #[test]
    fn udev_fails_when_missing_or_unreadable() {
        let mut missing = on();
        missing.udev = UdevView::Missing;
        assert_fail(
            &check_udev(&missing),
            "/run/udev/data) is not visible",
            "systemd-udevd",
        );
        let mut unreadable = on();
        unreadable.udev = UdevView::Unreadable("Permission denied".into());
        assert_fail(
            &check_udev(&unreadable),
            "cannot be read (Permission denied)",
            "readable",
        );
    }

    #[test]
    fn ddc_walks_available_bus_mapped_any_read_all_off_in_order() {
        let mut v = on();
        v.ddc = Default::default();
        assert_eq!(check_ddc(&v).status, WARN, "no ddcutil");

        v.ddc.available = true;
        assert_eq!(check_ddc(&v).status, WARN, "no bus mapped");

        v.ddc.bus_mapped = true;
        assert_eq!(check_ddc(&v).status, UNKNOWN, "no read yet");

        v.ddc.any_read = true;
        v.ddc.all_off = true;
        assert_eq!(check_ddc(&v).status, WARN, "all off");

        v.ddc.all_off = false;
        assert_eq!(check_ddc(&v).status, PASS);
    }

    /// Through the live reader: an absent directory, then one with a PCM-family node and
    /// a node the desktop does not need.
    #[test]
    fn the_sound_reader_sees_what_is_there() {
        let dir = tempfile::tempdir().unwrap();
        assert_eq!(
            SoundView::observe(&dir.path().join("snd")),
            SoundView::Missing
        );
        let snd = dir.path().join("snd");
        std::fs::create_dir_all(&snd).unwrap();
        std::fs::write(snd.join("controlC0"), "").unwrap();
        std::fs::write(snd.join("timer"), "").unwrap();
        assert_eq!(
            SoundView::observe(&snd),
            SoundView::Present { denied: Vec::new() },
            "a node this test may open read-write, and a non-PCM node ignored"
        );
    }
}
