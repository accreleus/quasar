//! Where console mode's local audio plays (RH-07 #407, D13): the host desktop user's
//! PipeWire when it answers, the host's ALSA device otherwise, and never a device someone
//! else holds.
//!
//! Host preparation's `--console-audio-user USER` makes that user's `pipewire-pulse` listen
//! on a Quasar-only socket, [`PIPEWIRE_SOCKET`], in a directory only the user and the Quasar
//! group can enter; the recovery actor binds the directory into a console agent when the
//! host has it. While that socket answers, PipeWire owns the host's sound: console audio
//! goes to it (`pulsesink`), sink discovery reports only its sinks (`pipewire:default`,
//! `pipewire:<sink name>`), and an explicit `hw:*` choice is refused rather than opened
//! underneath it. With no PipeWire answering, ALSA is used only when the chosen PCM is not
//! already open (`/proc/asound/cardN/pcmMp/sub0/status` reads `closed`); an open one is a
//! named refusal, never a fight.
//!
//! ALSA sinks are reported by card id, `hw:CARD=<id>,DEV=<device>` (the id is
//! `/proc/asound/cardN/id`): a card's index moves when a driver reloads, its id does not,
//! so an operator's stored choice keeps naming the same device (#407 live). An id is
//! mapped to the card's current index only where an index is needed (the open-PCM check).
//! A stored legacy `hw:<card>,<device>` is still accepted and read by index, as before.
//!
//! [`choose_route`] is the whole decision and does no I/O of its own: what it reads of the
//! host comes through [`HostAudio`], whose live implementation ([`LiveHostAudio`]) takes
//! its paths as fields so tests can point it at a temporary `/proc/asound` and a real
//! socket. `capacity::detect_audio_sinks` and `pipeline::build_local_audio_pipeline` go
//! through it, so they cannot disagree.

use std::io::Read;
use std::os::unix::net::UnixStream;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::Mutex;
use std::time::{Duration, Instant};

use crate::messages::AudioSink;

/// The desktop user's `pipewire-pulse` listen socket, in the directory host preparation
/// creates and the recipe binds at the same path (`recipe::CONSOLE_AUDIO_DIR`).
pub const PIPEWIRE_SOCKET: &str = "/run/quasar-console-audio/native";
/// `pulsesink`'s `server` for [`PIPEWIRE_SOCKET`].
pub const PIPEWIRE_SERVER: &str = "unix:/run/quasar-console-audio/native";
/// Sink ids naming the host's PipeWire: `pipewire:default` or `pipewire:<sink name>`.
pub const PIPEWIRE_PREFIX: &str = "pipewire:";
#[allow(dead_code)]
pub const PIPEWIRE_DEFAULT: &str = "pipewire:default";
#[allow(dead_code)]
const LABEL_PREFIX: &str = "Host PipeWire · ";

/// What an ALSA PCM's `sub0/status` says.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum PcmStatus {
    Closed,
    /// Open by some process; the kernel names its pid (in the reader's pid namespace, so a
    /// host pid seen through the host's `/proc/asound` bind).
    Open {
        owner_pid: Option<u32>,
    },
    /// No status to read (a card-level id, a PCM the host does not list): not provably held.
    Unknown,
}

/// A sink the host's PipeWire listed over the pulse protocol.
#[allow(dead_code)]
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct PipeWireSink {
    pub name: String,
    pub description: String,
}

/// What [`choose_route`] reads of the host.
pub trait HostAudio {
    /// The Quasar console-audio socket accepts a connection.
    fn pipewire_answers(&self) -> bool;
    /// The PipeWire sinks, when they can be listed; empty when they cannot (restricted
    /// access may hide them), which leaves `pipewire:default` alone.
    #[allow(dead_code)]
    fn pipewire_sinks(&self) -> Vec<PipeWireSink>;
    /// The ALSA playback sinks this agent can open (`hw:CARD=<id>,DEV=<device>`, or
    /// `hw:CARD=<id>` when the host lists no PCMs).
    fn alsa_sinks(&self) -> Vec<AudioSink>;
    /// The current index of the card whose id is `id`, if the host has it.
    fn card_index(&self, id: &str) -> Option<u32>;
    fn pcm_status(&self, card: u32, device: u32) -> PcmStatus;
    /// Host preparation made the socket's directory, but no socket is in it: the desktop
    /// user's `pipewire-pulse` has not restarted since its drop-in was written (#433).
    #[allow(dead_code)]
    fn socket_missing(&self) -> bool {
        false
    }
}

/// What to run, as root on the host, so the console-audio user's `pipewire-pulse` reads
/// the drop-in and opens [`PIPEWIRE_SOCKET`] (#433). Through `runuser`: the `-M USER@`
/// form needs machined's transient units, which fail on Fedora CoreOS (uCore).
#[allow(dead_code)]
pub const RESTART_PIPEWIRE_PULSE: &str =
    "runuser -u USER -- env XDG_RUNTIME_DIR=/run/user/$(id -u USER) systemctl --user restart pipewire-pulse.service";

/// Where the console audio leg plays.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Route {
    /// `pulsesink server=`[`PIPEWIRE_SERVER`]; `device` unset for the default sink.
    PipeWire { device: Option<String> },
    /// `alsasink`; `device` unset for `auto` (the ALSA `default` PCM).
    Alsa { device: Option<String> },
}

impl Route {
    pub fn describe(&self) -> String {
        match self {
            Route::PipeWire { device: None } => "the host's PipeWire (default output)".into(),
            Route::PipeWire { device: Some(d) } => format!("the host's PipeWire ({d})"),
            Route::Alsa { device: None } => "the host's default ALSA device".into(),
            Route::Alsa { device: Some(d) } => format!("the host's ALSA device {d}"),
        }
    }
}

/// Why console audio will not play. The session still runs (console video plays quiet).
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Refusal {
    /// A `pipewire:*` sink is chosen, but the console-audio socket does not answer.
    PipeWireSilent { output: String },
    /// An ALSA sink is chosen while PipeWire answers: it owns the device, and opening it
    /// underneath would fight it.
    PipeWireOwnsSound { output: String },
    /// No PipeWire answers and the ALSA PCM is already open.
    DeviceHeld {
        device: String,
        owner_pid: Option<u32>,
    },
    /// The chosen ALSA card id names no card on the host now.
    DeviceAbsent { device: String },
}

impl Refusal {
    /// The refusal's name, logged as `reason` under the `console-audio-refused` token.
    pub fn reason(&self) -> &'static str {
        match self {
            Refusal::PipeWireSilent { .. } => "console-audio-pipewire-silent",
            Refusal::PipeWireOwnsSound { .. } => "console-audio-pipewire-owns-sound",
            Refusal::DeviceHeld { .. } => "console-audio-device-held",
            Refusal::DeviceAbsent { .. } => "console-audio-device-absent",
        }
    }

    #[allow(dead_code)]
    pub fn remediation(&self) -> &'static str {
        match self {
            Refusal::PipeWireSilent { .. } => {
                "Log the desktop user in (their PipeWire makes the socket), or run host \
                 preparation (deploy/prepare-host.sh --console --console-audio-user USER) \
                 as root and turn console mode off and on; or choose an ALSA output."
            }
            Refusal::PipeWireOwnsSound { .. } => {
                "Choose a Host PipeWire output (or auto) for console audio."
            }
            Refusal::DeviceHeld { .. } => {
                "Run host preparation with --console-audio-user for the desktop user whose \
                 PipeWire holds the device, so console audio plays through it, or close the \
                 program holding it."
            }
            Refusal::DeviceAbsent { .. } => {
                "Choose one of the host's current audio outputs (or auto) for console \
                 audio, or put the sound card back."
            }
        }
    }
}

impl std::fmt::Display for Refusal {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Refusal::PipeWireSilent { output } => write!(
                f,
                "console audio is set to the host's PipeWire ({output}), but its \
                 console-audio socket does not answer"
            ),
            Refusal::PipeWireOwnsSound { output } => write!(
                f,
                "the host's PipeWire answers on the console-audio socket, so the sound \
                 device {output} is not opened underneath it"
            ),
            Refusal::DeviceHeld { device, owner_pid } => {
                write!(
                    f,
                    "the sound device ({device}) is held by another program (PipeWire/pulse)"
                )?;
                if let Some(pid) = owner_pid {
                    write!(f, ", host process {pid}")?;
                }
                Ok(())
            }
            Refusal::DeviceAbsent { device } => write!(
                f,
                "console audio is set to the sound device {device}, which this host does \
                 not have now"
            ),
        }
    }
}

/// The sink id reported for an ALSA playback PCM: by card id, which survives a driver
/// reload that moves the card's index.
pub fn alsa_sink_id(card_id: &str, device: u32) -> String {
    format!("hw:CARD={card_id},DEV={device}")
}

/// The sink id reported for a card with no PCM detail.
pub fn alsa_card_sink_id(card_id: &str) -> String {
    format!("hw:CARD={card_id}")
}

/// An ALSA card as an id names it: by index (legacy `hw:1,3`) or by card id.
#[derive(Debug, Clone, PartialEq, Eq)]
enum CardRef {
    Index(u32),
    Id(String),
}

/// An ALSA `hw:` / `plughw:` address: `<card>[,<device>]` or `CARD=<card>[,DEV=<device>]`.
#[derive(Debug, Clone, PartialEq, Eq)]
struct AlsaAddress {
    card: CardRef,
    device: Option<u32>,
}

fn parse_alsa(id: &str) -> Option<AlsaAddress> {
    let address = id
        .strip_prefix("hw:")
        .or_else(|| id.strip_prefix("plughw:"))?;
    let (mut card, mut device) = (None, None);
    for (i, part) in address.split(',').map(str::trim).enumerate() {
        let (key, value) = match part.split_once('=') {
            Some((k, v)) => (k.trim(), v.trim()),
            None => (if i == 0 { "CARD" } else { "DEV" }, part),
        };
        match key {
            "CARD" if !value.is_empty() => {
                card = Some(match value.parse() {
                    Ok(n) => CardRef::Index(n),
                    Err(_) => CardRef::Id(value.to_string()),
                })
            }
            "DEV" => device = Some(value.parse().ok()?),
            // SUBDEV and the like say nothing about which PCM.
            _ => {}
        }
    }
    Some(AlsaAddress {
        card: card?,
        device,
    })
}

/// The card's current index; `None` when a card id names no card on the host now.
fn card_index(card: &CardRef, host: &dyn HostAudio) -> Option<u32> {
    match card {
        CardRef::Index(n) => Some(*n),
        CardRef::Id(id) => host.card_index(id),
    }
}

/// `(card index, device)` for a PCM address, read at use time.
fn pcm_of(id: &str, host: &dyn HostAudio) -> Option<(u32, u32)> {
    let address = parse_alsa(id)?;
    Some((card_index(&address.card, host)?, address.device?))
}

/// Where the console audio leg for `output` (`auto`, `pipewire:*`, `hw:*`) plays, or why
/// it does not.
///
/// `auto` follows PipeWire's default sink while PipeWire answers (the plan's "alsasink
/// becomes pulsesink whenever that socket accepts a connection"); otherwise the ALSA
/// default, refused if any playback PCM the agent can see is open — `default` names no
/// card, so which one it would open is not knowable here, and a held device is never
/// fought for.
pub fn choose_route(output: &str, host: &dyn HostAudio) -> Result<Route, Refusal> {
    let answers = host.pipewire_answers();
    if let Some(name) = output.strip_prefix(PIPEWIRE_PREFIX) {
        if !answers {
            return Err(Refusal::PipeWireSilent {
                output: output.into(),
            });
        }
        let device = (name != "default" && !name.is_empty()).then(|| name.to_string());
        return Ok(Route::PipeWire { device });
    }
    if answers {
        if output == "auto" {
            return Ok(Route::PipeWire { device: None });
        }
        return Err(Refusal::PipeWireOwnsSound {
            output: output.into(),
        });
    }
    let address = parse_alsa(output);
    let card = match &address {
        Some(a) => match card_index(&a.card, host) {
            Some(n) => Some(n),
            // A card id the host does not have now: say so, rather than let alsasink fail
            // to open it. (A legacy index is not checked here, as before.)
            None => {
                return Err(Refusal::DeviceAbsent {
                    device: output.into(),
                })
            }
        },
        None => None,
    };
    let pcms: Vec<(String, u32, u32)> = match (card, address.and_then(|a| a.device)) {
        (Some(card), Some(device)) => vec![(output.to_string(), card, device)],
        _ => host
            .alsa_sinks()
            .into_iter()
            .filter_map(|s| {
                let (c, d) = pcm_of(&s.id, host)?;
                Some((s.id, c, d))
            })
            .filter(|(_, c, _)| output == "auto" || card == Some(*c))
            .collect(),
    };
    for (name, card, device) in pcms {
        if let PcmStatus::Open { owner_pid } = host.pcm_status(card, device) {
            return Err(Refusal::DeviceHeld {
                device: name,
                owner_pid,
            });
        }
    }
    let device = (output != "auto").then(|| output.to_string());
    Ok(Route::Alsa { device })
}

/// The console sinks to offer: the host's PipeWire's while it answers (`hw:*` hidden, so
/// the operator is never offered a device PipeWire owns), its ALSA ones otherwise.
#[allow(dead_code)]
pub fn sinks(host: &dyn HostAudio) -> Vec<AudioSink> {
    if !host.pipewire_answers() {
        return host.alsa_sinks();
    }
    let mut out = vec![AudioSink {
        id: PIPEWIRE_DEFAULT.into(),
        label: format!("{LABEL_PREFIX}Default output"),
    }];
    for sink in host.pipewire_sinks() {
        if sink.name.is_empty() || sink.name == "default" {
            continue;
        }
        let label = if sink.description.is_empty() {
            &sink.name
        } else {
            &sink.description
        };
        out.push(AudioSink {
            id: format!("{PIPEWIRE_PREFIX}{}", sink.name),
            label: format!("{LABEL_PREFIX}{label}"),
        });
    }
    out
}

/// A PCM's `sub0/status` under an asound root. The kernel writes `closed` when nothing
/// has it open, and a `state:` / `owner_pid` block otherwise.
pub fn read_pcm_status(asound: &Path, card: u32, device: u32) -> PcmStatus {
    let path = asound.join(format!("card{card}/pcm{device}p/sub0/status"));
    let Ok(text) = std::fs::read_to_string(path) else {
        return PcmStatus::Unknown;
    };
    if text.trim() == "closed" {
        return PcmStatus::Closed;
    }
    let owner_pid = text.lines().find_map(|l| {
        let (k, v) = l.split_once(':')?;
        (k.trim() == "owner_pid").then(|| v.trim().parse().ok())?
    });
    PcmStatus::Open { owner_pid }
}

/// The asound root sink discovery reads: the agent's own `/proc/asound` when it lists
/// cards, else the host's bind at `/host-proc/asound`. Docker creates an empty
/// `/proc/asound` even with no host ALSA metadata visible, so the first root that actually
/// has `cards` wins, or the host bind would be shadowed.
pub fn asound_root() -> PathBuf {
    ["/proc/asound", "/host-proc/asound"]
        .into_iter()
        .find(|root| Path::new(root).join("cards").is_file())
        .unwrap_or("/proc/asound")
        .into()
}

/// The host as this agent sees it. Paths are fields so tests can use temporary ones.
#[derive(Debug, Clone)]
pub struct LiveHostAudio {
    pub socket: PathBuf,
    pub asound: PathBuf,
    pub dev_snd: PathBuf,
}

impl LiveHostAudio {
    pub fn live() -> Self {
        LiveHostAudio {
            socket: PIPEWIRE_SOCKET.into(),
            asound: asound_root(),
            dev_snd: "/dev/snd".into(),
        }
    }
}

impl HostAudio for LiveHostAudio {
    fn pipewire_answers(&self) -> bool {
        UnixStream::connect(&self.socket).is_ok()
    }

    fn pipewire_sinks(&self) -> Vec<PipeWireSink> {
        cached_pipewire_sinks(&self.socket)
    }

    fn socket_missing(&self) -> bool {
        self.socket.parent().is_some_and(Path::is_dir)
            && std::fs::symlink_metadata(&self.socket).is_err()
    }

    fn alsa_sinks(&self) -> Vec<AudioSink> {
        crate::capacity::alsa_sinks_at(&self.asound, &self.dev_snd)
    }

    fn card_index(&self, id: &str) -> Option<u32> {
        read_card_ids(&self.asound)
            .into_iter()
            .find_map(|(index, card_id)| (card_id == id).then_some(index))
    }

    fn pcm_status(&self, card: u32, device: u32) -> PcmStatus {
        read_pcm_status(&self.asound, card, device)
    }
}

/// Each card's index and id under an asound root: the index and bracketed id from
/// `cards`, the id from `cardN/id` where it can be read (the kernel's own file for it).
pub fn read_card_ids(asound: &Path) -> std::collections::BTreeMap<u32, String> {
    let cards = std::fs::read_to_string(asound.join("cards")).unwrap_or_default();
    let mut out = std::collections::BTreeMap::new();
    for line in cards.lines() {
        // " 0 [NVidia         ]: HDA-Intel - HDA NVidia"; the card's second line has no index.
        let Some((index, rest)) = line.trim_start().split_once(' ') else {
            continue;
        };
        let Ok(index) = index.parse::<u32>() else {
            continue;
        };
        let bracketed = rest
            .trim_start()
            .strip_prefix('[')
            .and_then(|r| r.split_once(']'))
            .map(|(id, _)| id.trim().to_string());
        let id = std::fs::read_to_string(asound.join(format!("card{index}/id")))
            .ok()
            .map(|id| id.trim().to_string())
            .filter(|id| !id.is_empty())
            .or(bracketed.filter(|id| !id.is_empty()));
        if let Some(id) = id {
            out.insert(index, id);
        }
    }
    out
}

/// Sink discovery runs on the console hotplug poll; the listing forks `pactl`, so it is
/// reused for a while.
#[allow(dead_code)]
const LIST_TTL: Duration = Duration::from_secs(15);
#[allow(dead_code)]
const LIST_TIMEOUT: Duration = Duration::from_secs(2);

#[allow(dead_code)]
type SinkCache = Option<(PathBuf, Instant, Vec<PipeWireSink>)>;
#[allow(dead_code)]
static LIST_CACHE: Mutex<SinkCache> = Mutex::new(None);

#[allow(dead_code)]
fn cached_pipewire_sinks(socket: &Path) -> Vec<PipeWireSink> {
    if let Some((path, at, sinks)) = LIST_CACHE.lock().unwrap().as_ref() {
        if path == socket && at.elapsed() < LIST_TTL {
            return sinks.clone();
        }
    }
    let sinks = list_pipewire_sinks(socket);
    *LIST_CACHE.lock().unwrap() = Some((socket.to_path_buf(), Instant::now(), sinks.clone()));
    sinks
}

/// `pactl --server=unix:<socket> -f json list sinks`, bounded. Any failure (no `pactl`,
/// access restricted, a slow server) lists nothing, which leaves `pipewire:default`.
#[allow(dead_code)]
fn list_pipewire_sinks(socket: &Path) -> Vec<PipeWireSink> {
    let mut child = match std::process::Command::new("pactl")
        .arg(format!("--server=unix:{}", socket.display()))
        .args(["-f", "json", "list", "sinks"])
        .stdin(std::process::Stdio::null())
        .stdout(std::process::Stdio::piped())
        .stderr(std::process::Stdio::null())
        .spawn()
    {
        Ok(c) => c,
        Err(e) => {
            tracing::debug!("pactl not runnable for PipeWire sink listing: {e}");
            return Vec::new();
        }
    };
    let mut stdout = child.stdout.take();
    let reader = std::thread::spawn(move || {
        let mut out = String::new();
        if let Some(s) = stdout.as_mut() {
            let _ = s.read_to_string(&mut out);
        }
        out
    });
    let deadline = Instant::now() + LIST_TIMEOUT;
    let status = loop {
        match child.try_wait() {
            Ok(Some(status)) => break Some(status),
            Ok(None) if Instant::now() < deadline => std::thread::sleep(Duration::from_millis(20)),
            _ => {
                let _ = child.kill();
                let _ = child.wait();
                break None;
            }
        }
    };
    let out = reader.join().unwrap_or_default();
    match status {
        Some(s) if s.success() => parse_pactl_sinks(&out),
        _ => {
            tracing::debug!(
                token = "console-audio-pipewire-list-failed",
                "could not list the host PipeWire's sinks; offering its default output only"
            );
            Vec::new()
        }
    }
}

/// `pactl -f json list sinks`: an array of objects carrying `name` and `description`.
#[allow(dead_code)]
pub fn parse_pactl_sinks(json: &str) -> Vec<PipeWireSink> {
    let Ok(serde_json::Value::Array(items)) = serde_json::from_str(json) else {
        return Vec::new();
    };
    items
        .iter()
        .filter_map(|item| {
            let name = item.get("name")?.as_str()?.trim();
            if name.is_empty() {
                return None;
            }
            Some(PipeWireSink {
                name: name.to_string(),
                description: item
                    .get("description")
                    .and_then(|d| d.as_str())
                    .unwrap_or_default()
                    .trim()
                    .to_string(),
            })
        })
        .collect()
}

/// How many console ALSA legs this agent has open. The readiness check reads an open PCM
/// as held by someone else only while it is zero: otherwise the holder is us.
static ALSA_LEGS: AtomicUsize = AtomicUsize::new(0);

/// Held by a live ALSA console-audio leg; counts it for [`alsa_leg_live`].
#[derive(Debug)]
pub struct AlsaLeg(());

impl AlsaLeg {
    pub fn open() -> Self {
        ALSA_LEGS.fetch_add(1, Ordering::Relaxed);
        AlsaLeg(())
    }
}

impl Drop for AlsaLeg {
    fn drop(&mut self) {
        ALSA_LEGS.fetch_sub(1, Ordering::Relaxed);
    }
}

#[allow(dead_code)]
pub fn alsa_leg_live() -> bool {
    ALSA_LEGS.load(Ordering::Relaxed) > 0
}

/// The console config's `audio_output`, latched when the control plane sends it, for the
/// readiness check to judge the route a session would take. `None` (console video only, or
/// not yet sent) is judged as `auto`.
static CONFIGURED_OUTPUT: Mutex<Option<String>> = Mutex::new(None);

pub fn set_configured_output(output: Option<&str>) {
    *CONFIGURED_OUTPUT.lock().unwrap() = output.map(str::to_string);
}

#[allow(dead_code)]
pub fn configured_output() -> String {
    CONFIGURED_OUTPUT
        .lock()
        .unwrap()
        .clone()
        .unwrap_or_else(|| "auto".into())
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::BTreeMap;

    #[derive(Default)]
    struct FakeHost {
        answers: bool,
        listed: Vec<PipeWireSink>,
        alsa: Vec<AudioSink>,
        cards: BTreeMap<String, u32>,
        status: BTreeMap<(u32, u32), PcmStatus>,
    }

    impl HostAudio for FakeHost {
        fn pipewire_answers(&self) -> bool {
            self.answers
        }
        fn pipewire_sinks(&self) -> Vec<PipeWireSink> {
            self.listed.clone()
        }
        fn alsa_sinks(&self) -> Vec<AudioSink> {
            self.alsa.clone()
        }
        fn card_index(&self, id: &str) -> Option<u32> {
            self.cards.get(id).copied()
        }
        fn pcm_status(&self, card: u32, device: u32) -> PcmStatus {
            *self
                .status
                .get(&(card, device))
                .unwrap_or(&PcmStatus::Unknown)
        }
    }

    fn hdmi() -> FakeHost {
        FakeHost {
            alsa: vec![
                AudioSink {
                    id: "hw:CARD=NVidia,DEV=3".into(),
                    label: "HDA NVidia — HDMI 0".into(),
                },
                AudioSink {
                    id: "hw:CARD=Generic,DEV=0".into(),
                    label: "Generic — Analog".into(),
                },
            ],
            cards: BTreeMap::from([("NVidia".into(), 0), ("Generic".into(), 1)]),
            status: BTreeMap::from([((0, 3), PcmStatus::Closed), ((1, 0), PcmStatus::Closed)]),
            ..Default::default()
        }
    }

    #[test]
    fn pipewire_answering_takes_auto_and_named_sinks_and_refuses_alsa() {
        let mut host = hdmi();
        host.answers = true;
        assert_eq!(
            choose_route("auto", &host),
            Ok(Route::PipeWire { device: None })
        );
        assert_eq!(
            choose_route("pipewire:default", &host),
            Ok(Route::PipeWire { device: None })
        );
        assert_eq!(
            choose_route("pipewire:alsa_output.pci-0000_01_00.1.hdmi-stereo", &host),
            Ok(Route::PipeWire {
                device: Some("alsa_output.pci-0000_01_00.1.hdmi-stereo".into())
            })
        );
        let refused = choose_route("hw:0,3", &host).unwrap_err();
        assert_eq!(refused.reason(), "console-audio-pipewire-owns-sound");
        assert!(refused.to_string().contains("hw:0,3"), "{refused}");
    }

    #[test]
    fn pipewire_chosen_but_silent_is_a_named_refusal() {
        let host = hdmi();
        let refused = choose_route("pipewire:default", &host).unwrap_err();
        assert_eq!(refused.reason(), "console-audio-pipewire-silent");
    }

    #[test]
    fn alsa_is_used_only_when_the_pcm_is_closed() {
        let mut host = hdmi();
        assert_eq!(
            choose_route("hw:0,3", &host),
            Ok(Route::Alsa {
                device: Some("hw:0,3".into())
            })
        );
        assert_eq!(
            choose_route("auto", &host),
            Ok(Route::Alsa { device: None })
        );

        host.status.insert(
            (0, 3),
            PcmStatus::Open {
                owner_pid: Some(1234),
            },
        );
        let held = choose_route("hw:0,3", &host).unwrap_err();
        assert_eq!(held.reason(), "console-audio-device-held");
        let text = held.to_string();
        assert!(
            text.contains("held by another program (PipeWire/pulse)") && text.contains("1234"),
            "{text}"
        );
        // `auto` names no card: any open playback PCM refuses it.
        assert!(matches!(
            choose_route("auto", &host),
            Err(Refusal::DeviceHeld { .. })
        ));
        // Another card's PCM is not this one.
        assert!(choose_route("hw:1,0", &host).is_ok());
        // A card-level id checks that card's PCMs.
        assert!(choose_route("hw:0", &host).is_err());
        assert!(choose_route("hw:1", &host).is_ok());
    }

    /// A stable id follows its card to whatever index it has now (#407 live: a driver
    /// reload moved the NVIDIA HDA from card 4 to card 1); a legacy index id still reads
    /// by index; a card id the host no longer has is a named refusal.
    #[test]
    fn a_card_id_is_read_at_its_current_index_and_a_legacy_index_still_works() {
        let mut host = hdmi();
        assert_eq!(
            choose_route("hw:CARD=NVidia,DEV=3", &host),
            Ok(Route::Alsa {
                device: Some("hw:CARD=NVidia,DEV=3".into())
            })
        );
        // The driver reloads: NVidia is card 1 now, Generic card 0.
        host.cards = BTreeMap::from([("NVidia".into(), 1), ("Generic".into(), 0)]);
        host.status = BTreeMap::from([
            ((1, 3), PcmStatus::Open { owner_pid: Some(7) }),
            ((0, 0), PcmStatus::Closed),
        ]);
        let held = choose_route("hw:CARD=NVidia,DEV=3", &host).unwrap_err();
        assert_eq!(held.reason(), "console-audio-device-held");
        assert!(held.to_string().contains("hw:CARD=NVidia,DEV=3"), "{held}");
        assert!(choose_route("hw:CARD=Generic,DEV=0", &host).is_ok());
        // A card-level id checks that card's PCMs at its current index.
        assert!(choose_route("hw:CARD=NVidia", &host).is_err());
        assert!(choose_route("plughw:CARD=Generic", &host).is_ok());
        // A stored legacy id reads by index, as before.
        assert!(choose_route("hw:1,3", &host).is_err());
        assert_eq!(
            choose_route("hw:0,0", &host),
            Ok(Route::Alsa {
                device: Some("hw:0,0".into())
            })
        );
        // A card id the host does not have now.
        let absent = choose_route("hw:CARD=USB,DEV=0", &host).unwrap_err();
        assert_eq!(absent.reason(), "console-audio-device-absent");
        assert!(absent.to_string().contains("hw:CARD=USB,DEV=0"), "{absent}");
    }

    #[test]
    fn alsa_addresses_parse_positional_and_keyed_forms() {
        let at = |card: CardRef, device: Option<u32>| Some(AlsaAddress { card, device });
        assert_eq!(parse_alsa("hw:1,3"), at(CardRef::Index(1), Some(3)));
        assert_eq!(parse_alsa("plughw:2"), at(CardRef::Index(2), None));
        assert_eq!(
            parse_alsa("hw:CARD=NVidia,DEV=3"),
            at(CardRef::Id("NVidia".into()), Some(3))
        );
        assert_eq!(
            parse_alsa("hw:NVidia,7"),
            at(CardRef::Id("NVidia".into()), Some(7))
        );
        assert_eq!(
            parse_alsa("hw:CARD=Generic"),
            at(CardRef::Id("Generic".into()), None)
        );
        assert_eq!(parse_alsa("hw:"), None);
        assert_eq!(parse_alsa("hw:0,x"), None);
        assert_eq!(parse_alsa("auto"), None);
        assert_eq!(alsa_sink_id("NVidia", 3), "hw:CARD=NVidia,DEV=3");
        assert_eq!(alsa_card_sink_id("NVidia"), "hw:CARD=NVidia");
    }

    #[test]
    fn card_ids_come_from_the_card_id_file_then_the_cards_listing() {
        let dir = tempfile::tempdir().unwrap();
        std::fs::write(
            dir.path().join("cards"),
            " 0 [Generic        ]: HDA-Intel - HD-Audio Generic\n                      HD-Audio Generic at 0xfc\n 1 [NVidia         ]: HDA-Intel - HDA NVidia\n",
        )
        .unwrap();
        std::fs::create_dir_all(dir.path().join("card1")).unwrap();
        std::fs::write(dir.path().join("card1/id"), "NVidiaHDA\n").unwrap();
        assert_eq!(
            read_card_ids(dir.path()),
            BTreeMap::from([(0, "Generic".to_string()), (1, "NVidiaHDA".to_string())])
        );
        let host = LiveHostAudio {
            socket: dir.path().join("native"),
            asound: dir.path().into(),
            dev_snd: dir.path().join("snd"),
        };
        assert_eq!(host.card_index("NVidiaHDA"), Some(1));
        assert_eq!(host.card_index("Generic"), Some(0));
        assert_eq!(host.card_index("NVidia"), None);
    }

    #[test]
    fn sinks_are_pipewire_only_while_it_answers() {
        let mut host = hdmi();
        let alsa: Vec<String> = sinks(&host).into_iter().map(|s| s.id).collect();
        assert_eq!(alsa, vec!["hw:CARD=NVidia,DEV=3", "hw:CARD=Generic,DEV=0"]);

        host.answers = true;
        assert_eq!(
            sinks(&host),
            vec![AudioSink {
                id: "pipewire:default".into(),
                label: "Host PipeWire · Default output".into()
            }]
        );
        host.listed = vec![
            PipeWireSink {
                name: "alsa_output.hdmi".into(),
                description: "HDMI / DisplayPort".into(),
            },
            PipeWireSink {
                name: "bare".into(),
                description: String::new(),
            },
        ];
        let listed = sinks(&host);
        assert_eq!(listed.len(), 3);
        assert_eq!(listed[1].id, "pipewire:alsa_output.hdmi");
        assert_eq!(listed[1].label, "Host PipeWire · HDMI / DisplayPort");
        assert_eq!(listed[2].label, "Host PipeWire · bare");
        assert!(listed.iter().all(|s| !s.id.starts_with("hw:")));
    }

    #[test]
    fn pcm_status_reads_closed_open_and_missing_from_an_asound_tree() {
        let dir = tempfile::tempdir().unwrap();
        let sub = dir.path().join("card0/pcm3p/sub0");
        std::fs::create_dir_all(&sub).unwrap();
        std::fs::write(sub.join("status"), "closed\n").unwrap();
        assert_eq!(read_pcm_status(dir.path(), 0, 3), PcmStatus::Closed);
        std::fs::write(
            sub.join("status"),
            "state: RUNNING\nowner_pid   : 4321\ntrigger_time: 1.0\n",
        )
        .unwrap();
        assert_eq!(
            read_pcm_status(dir.path(), 0, 3),
            PcmStatus::Open {
                owner_pid: Some(4321)
            }
        );
        std::fs::write(sub.join("status"), "state: PREPARED\n").unwrap();
        assert_eq!(
            read_pcm_status(dir.path(), 0, 3),
            PcmStatus::Open { owner_pid: None }
        );
        assert_eq!(read_pcm_status(dir.path(), 0, 7), PcmStatus::Unknown);
    }

    /// The live host view against real files and a real socket: absent, then listening.
    #[test]
    fn live_host_audio_probes_a_real_socket_and_asound_fixture() {
        let dir = tempfile::tempdir().unwrap();
        let asound = dir.path().join("asound");
        let dev_snd = dir.path().join("snd");
        std::fs::create_dir_all(asound.join("card0/pcm3p/sub0")).unwrap();
        std::fs::create_dir_all(&dev_snd).unwrap();
        std::fs::write(
            asound.join("cards"),
            " 0 [NVidia         ]: HDA-Intel - HDA NVidia\n",
        )
        .unwrap();
        std::fs::write(asound.join("pcm"), "00-03: HDMI 0 : HDMI 0 : playback 1\n").unwrap();
        std::fs::write(dev_snd.join("pcmC0D3p"), "").unwrap();
        std::fs::write(
            asound.join("card0/pcm3p/sub0/status"),
            "state: RUNNING\nowner_pid   : 99\n",
        )
        .unwrap();
        let host = LiveHostAudio {
            socket: dir.path().join("native"),
            asound: asound.clone(),
            dev_snd,
        };

        assert!(!host.pipewire_answers());
        assert_eq!(
            sinks(&host).into_iter().map(|s| s.id).collect::<Vec<_>>(),
            vec!["hw:CARD=NVidia,DEV=3"]
        );
        assert!(matches!(
            choose_route("auto", &host),
            Err(Refusal::DeviceHeld {
                owner_pid: Some(99),
                ..
            })
        ));
        std::fs::write(asound.join("card0/pcm3p/sub0/status"), "closed\n").unwrap();
        assert_eq!(
            choose_route("auto", &host),
            Ok(Route::Alsa { device: None })
        );

        // A stale socket file with no listener does not answer.
        let listener = std::os::unix::net::UnixListener::bind(&host.socket).unwrap();
        drop(listener);
        assert!(!host.pipewire_answers());
        std::fs::remove_file(&host.socket).unwrap();

        let _listener = std::os::unix::net::UnixListener::bind(&host.socket).unwrap();
        assert!(host.pipewire_answers());
        assert_eq!(
            choose_route("auto", &host),
            Ok(Route::PipeWire { device: None })
        );
        assert_eq!(sinks(&host)[0].id, "pipewire:default");
        assert!(sinks(&host).iter().all(|s| !s.id.starts_with("hw:")));
    }

    /// #433: host preparation made the socket directory, but the desktop user's
    /// pipewire-pulse has not been restarted since, so nothing listens in it yet.
    #[test]
    fn live_host_audio_says_the_socket_is_missing_only_in_a_prepared_directory() {
        let dir = tempfile::tempdir().unwrap();
        let unprepared = LiveHostAudio {
            socket: dir.path().join("absent/native"),
            asound: dir.path().join("asound"),
            dev_snd: dir.path().join("snd"),
        };
        assert!(!unprepared.socket_missing(), "no directory: not prepared");

        let prepared = LiveHostAudio {
            socket: dir.path().join("native"),
            ..unprepared
        };
        assert!(prepared.socket_missing());

        // A socket file, listening or stale, is not missing (stale is PipeWireSilent).
        let listener = std::os::unix::net::UnixListener::bind(&prepared.socket).unwrap();
        assert!(!prepared.socket_missing());
        drop(listener);
        assert!(!prepared.socket_missing());
    }

    #[test]
    fn pactl_json_listing_parses_names_and_descriptions() {
        let json = r#"[{"index":55,"name":"alsa_output.pci-0000_01_00.1.hdmi-stereo","description":"GB202 High Definition Audio Controller Digital Stereo (HDMI)","driver":"PipeWire"},{"index":56,"description":"no name"},{"name":"","description":"empty"}]"#;
        assert_eq!(
            parse_pactl_sinks(json),
            vec![PipeWireSink {
                name: "alsa_output.pci-0000_01_00.1.hdmi-stereo".into(),
                description: "GB202 High Definition Audio Controller Digital Stereo (HDMI)".into(),
            }]
        );
        assert!(parse_pactl_sinks("Connection failure").is_empty());
    }

    #[test]
    fn an_alsa_leg_counts_while_it_lives() {
        let before = alsa_leg_live();
        let leg = AlsaLeg::open();
        assert!(alsa_leg_live());
        drop(leg);
        assert_eq!(alsa_leg_live(), before);
    }
}
