//! Engine targets: the mode table, and the targets and declared gaps the environment names.

use quasar_node_agent::runtime::{EngineKind, EngineMode};
use std::collections::BTreeMap;
use std::path::PathBuf;

// Embedded so a suite binary copied to a lab host needs no checkout beside it.
const TARGETS: &str = include_str!("../../../testdata/engine-suite/targets.json");
const PROFILES: &str = include_str!("../../../testdata/engine-profiles/profiles.json");

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum HomeOwner {
    AppId,
    EngineUser,
    SubordinateId,
}

/// One row of `targets.json`.
#[derive(Debug, Clone)]
pub struct Mode {
    pub name: String,
    pub engine: EngineKind,
    pub mode: EngineMode,
    pub home_owner: HomeOwner,
}

/// What a host may be unable to give, declared per run so an absent capability is a
/// reported skip and never a silent pass.
#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord)]
pub enum Capability {
    /// An NVIDIA GPU injected through CDI.
    Cdi,
    /// The host's DRM nodes, passed through by group.
    Dri,
    /// `/dev/uinput`, passed through by group.
    Uinput,
    /// Container healthchecks that the engine runs (Podman needs a systemd session).
    Health,
}

impl Capability {
    pub const ALL: [Capability; 4] = [
        Capability::Cdi,
        Capability::Dri,
        Capability::Uinput,
        Capability::Health,
    ];
    pub fn name(self) -> &'static str {
        match self {
            Capability::Cdi => "cdi",
            Capability::Dri => "dri",
            Capability::Uinput => "uinput",
            Capability::Health => "health",
        }
    }
    fn parse(name: &str) -> Result<Self, String> {
        Self::ALL
            .into_iter()
            .find(|c| c.name() == name)
            .ok_or_else(|| {
                format!(
                    "unknown capability {name:?} (known: {})",
                    Self::ALL.map(Capability::name).join(", ")
                )
            })
    }
}

#[derive(Debug, Clone)]
pub struct Target {
    pub mode: Mode,
    pub socket: PathBuf,
    /// Capability -> the reason this host cannot give it.
    pub lacks: BTreeMap<Capability, String>,
    /// Case -> the recorded finding it fails on for this engine mode.
    pub known: BTreeMap<String, String>,
}

fn engine(value: &str) -> Result<EngineKind, String> {
    match value {
        "docker" => Ok(EngineKind::Docker),
        "podman" => Ok(EngineKind::Podman),
        other => Err(format!("unknown engine {other:?}")),
    }
}

fn engine_mode(value: &str) -> Result<EngineMode, String> {
    match value {
        "rootful" => Ok(EngineMode::Rootful),
        "rootless" => Ok(EngineMode::Rootless),
        other => Err(format!("unknown mode {other:?}")),
    }
}

/// The mode table, held to the published engine profiles: one target per engine and mode
/// there, and no other.
pub fn modes() -> Result<Vec<Mode>, String> {
    let table: serde_json::Value =
        serde_json::from_str(TARGETS).map_err(|e| format!("targets.json: {e}"))?;
    let owners = table["homeOwners"]
        .as_object()
        .ok_or("targets.json: no homeOwners")?;
    let mut modes = Vec::new();
    for row in table["targets"]
        .as_array()
        .ok_or("targets.json: no targets")?
    {
        let field = |key: &str| {
            row[key]
                .as_str()
                .ok_or_else(|| format!("targets.json: a target without {key}"))
        };
        let (name, e, m, owner) = (
            field("name")?,
            field("engine")?,
            field("mode")?,
            field("homeOwner")?,
        );
        if name != format!("{e}-{m}") {
            return Err(format!("targets.json: {name} must be named {e}-{m}"));
        }
        if !owners.contains_key(owner) {
            return Err(format!("targets.json: {name}: unknown homeOwner {owner:?}"));
        }
        modes.push(Mode {
            name: name.to_string(),
            engine: engine(e)?,
            mode: engine_mode(m)?,
            home_owner: match owner {
                "app-id" => HomeOwner::AppId,
                "engine-user" => HomeOwner::EngineUser,
                "subordinate-id" => HomeOwner::SubordinateId,
                other => return Err(format!("targets.json: {name}: no rule for {other:?}")),
            },
        });
    }

    let profiles: serde_json::Value =
        serde_json::from_str(PROFILES).map_err(|e| format!("profiles.json: {e}"))?;
    let mut published = Vec::new();
    for e in profiles["engines"]
        .as_object()
        .ok_or("profiles.json: no engines")?
        .keys()
    {
        for m in profiles["modes"]
            .as_array()
            .ok_or("profiles.json: no modes")?
        {
            published.push(format!("{e}-{}", m.as_str().unwrap_or_default()));
        }
    }
    let mut ours: Vec<String> = modes.iter().map(|m| m.name.clone()).collect();
    published.sort();
    ours.sort();
    if published != ours {
        return Err(format!(
            "testdata/engine-suite/targets.json has {ours:?}, but the engine profiles publish \
             {published:?}: add or remove the target, not assertions"
        ));
    }
    Ok(modes)
}

/// `QUASAR_ENGINE_SUITE_TARGETS`: whitespace- or comma-separated `<target>=<socket path>`.
/// `QUASAR_ENGINE_SUITE_LACKS`: `;`-separated `[<target>:]<capability>=<reason>`; without a
/// target the gap applies to every target of the run.
pub fn from_environment(
    modes: &[Mode],
    targets: &str,
    lacks: Option<&str>,
) -> Result<Vec<Target>, String> {
    let mut out: Vec<Target> = Vec::new();
    for item in targets
        .split(|c: char| c == ',' || c.is_whitespace())
        .filter(|s| !s.is_empty())
    {
        let (name, socket) = item
            .split_once('=')
            .ok_or_else(|| format!("target {item:?} is not <target>=<socket path>"))?;
        let socket = socket.strip_prefix("unix://").unwrap_or(socket);
        let mode = modes
            .iter()
            .find(|m| m.name == name)
            .ok_or_else(|| {
                format!(
                    "unknown target {name:?} (known: {})",
                    modes
                        .iter()
                        .map(|m| m.name.as_str())
                        .collect::<Vec<_>>()
                        .join(", ")
                )
            })?
            .clone();
        if !socket.starts_with('/') {
            return Err(format!("{name}: the socket must be an absolute path"));
        }
        if out.iter().any(|t| t.mode.name == name) {
            return Err(format!("{name} is named twice"));
        }
        out.push(Target {
            mode,
            socket: PathBuf::from(socket),
            lacks: BTreeMap::new(),
            known: BTreeMap::new(),
        });
    }
    if out.is_empty() {
        return Err("QUASAR_ENGINE_SUITE_TARGETS names no target".into());
    }
    for entry in lacks
        .unwrap_or_default()
        .split(';')
        .map(str::trim)
        .filter(|s| !s.is_empty())
    {
        let (scope, reason) = entry
            .split_once('=')
            .ok_or_else(|| format!("gap {entry:?} is not [<target>:]<capability>=<reason>"))?;
        let reason = reason.trim();
        if reason.is_empty() {
            return Err(format!("gap {entry:?} gives no reason"));
        }
        let (only, capability) = match scope.split_once(':') {
            Some((target, capability)) => (Some(target.trim()), capability),
            None => (None, scope),
        };
        let capability = Capability::parse(capability.trim())?;
        if let Some(name) = only {
            if !out.iter().any(|t| t.mode.name == name) {
                return Err(format!("gap for {name}, which this run does not target"));
            }
        }
        for target in out
            .iter_mut()
            .filter(|t| only.is_none_or(|name| t.mode.name == name))
        {
            target.lacks.insert(capability, reason.to_string());
        }
    }
    Ok(out)
}

/// `QUASAR_ENGINE_SUITE_KNOWN`: `;`-separated `<target>:<case>=<finding>`. Such a case
/// still runs: its failure reports KNOWN with the finding, and a pass fails the run so the
/// entry is removed once the finding is fixed.
pub fn known_failures(targets: &mut [Target], cases: &[&str], spec: &str) -> Result<(), String> {
    for entry in spec.split(';').map(str::trim).filter(|s| !s.is_empty()) {
        let (scope, finding) = entry
            .split_once('=')
            .ok_or_else(|| format!("known failure {entry:?} is not <target>:<case>=<finding>"))?;
        let (target, case) = scope
            .split_once(':')
            .ok_or_else(|| format!("known failure {entry:?} names no target"))?;
        let (target, case, finding) = (target.trim(), case.trim(), finding.trim());
        if finding.is_empty() {
            return Err(format!("known failure {entry:?} records no finding"));
        }
        if !cases.contains(&case) {
            return Err(format!("known failure for unknown case {case:?}"));
        }
        let Some(t) = targets.iter_mut().find(|t| t.mode.name == target) else {
            return Err(format!(
                "known failure for {target}, which this run does not target"
            ));
        };
        t.known.insert(case.to_string(), finding.to_string());
    }
    Ok(())
}

/// The mode table and the environment parser run on every `cargo test`, targets or not.
pub fn self_check() -> Result<(), String> {
    let modes = modes()?;
    let parsed = from_environment(
        &modes,
        "docker-rootful=/var/run/docker.sock, podman-rootless=unix:///run/user/1000/podman/podman.sock",
        Some("cdi=no GPU here; podman-rootless:health=no systemd user session"),
    )?;
    let expect = |ok: bool, what: &str| if ok { Ok(()) } else { Err(what.to_string()) };
    expect(parsed.len() == 2, "two targets parse")?;
    expect(
        parsed[1].socket == std::path::Path::new("/run/user/1000/podman/podman.sock"),
        "a unix:// socket is read as its path",
    )?;
    expect(
        parsed
            .iter()
            .all(|t| t.lacks.contains_key(&Capability::Cdi)),
        "an unscoped gap applies to every target",
    )?;
    expect(
        !parsed[0].lacks.contains_key(&Capability::Health)
            && parsed[1].lacks[&Capability::Health] == "no systemd user session",
        "a scoped gap applies to its target only",
    )?;
    for (targets, lacks) in [
        ("docker-rootful=/s", Some("gpu=typo")),
        ("docker-rootful=/s", Some("cdi=")),
        (
            "docker-rootful=/s",
            Some("podman-rootless:cdi=not targeted"),
        ),
        ("docker-rootful=relative", None),
        ("docker-rootfull=/s", None),
        ("docker-rootful=/a docker-rootful=/b", None),
        ("", None),
    ] {
        expect(
            from_environment(&modes, targets, lacks).is_err(),
            &format!("refused: {targets:?} with {lacks:?}"),
        )?;
    }
    let mut known = parsed;
    known_failures(&mut known, &["restart"], "podman-rootless:restart=finding")?;
    expect(
        known[1].known["restart"] == "finding" && known[0].known.is_empty(),
        "a known failure applies to its target only",
    )?;
    for spec in [
        "restart=no target",
        "podman-rootless:restart=",
        "podman-rootless:nosuchcase=finding",
        "podman-rootful:restart=not targeted",
    ] {
        expect(
            known_failures(&mut known, &["restart"], spec).is_err(),
            &format!("refused: {spec:?}"),
        )?;
    }
    Ok(())
}
