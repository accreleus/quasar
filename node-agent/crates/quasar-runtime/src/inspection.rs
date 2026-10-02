//! Read-only runtime facts. Host paths below name the engine daemon's host,
//! never the node-agent container filesystem.

use std::collections::BTreeMap;
use std::path::{Path, PathBuf};

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct DaemonHostPath(pub PathBuf);

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum MountKind {
    Bind,
    Volume,
    Tmpfs,
    Other(String),
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Mount {
    pub kind: MountKind,
    /// Source in the engine daemon host namespace, when this mount has one.
    pub source: Option<DaemonHostPath>,
    /// Docker volume name, when `kind` is [`MountKind::Volume`].
    pub name: Option<String>,
    pub destination: String,
    /// `None` means the engine did not provide an access-mode fact.
    pub read_only: Option<bool>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ContainerInspection {
    pub id: String,
    pub image_id: String,
    pub configured_image: String,
    pub labels: BTreeMap<String, String>,
    pub mounts: Vec<Mount>,
    pub network_mode: Option<String>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct EngineStorage {
    pub root: DaemonHostPath,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ImageMetadata {
    pub id: String,
    pub baked_env: Vec<String>,
    /// Image-configured working directory, when Docker reports a usable value.
    pub working_dir: Option<String>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct DaemonImage {
    pub id: String,
    pub refs: Vec<String>,
}

/// Translate a path across the agent/daemon namespace boundary. A result is
/// returned only when the longest component-prefix bind mapping round-trips;
/// overlapping mounts therefore cannot make a hidden daemon path look safe.
pub fn daemon_path_for_agent_path(mounts: &[Mount], agent: &Path) -> Option<DaemonHostPath> {
    translate(mounts, agent, true).map(DaemonHostPath)
}

pub fn agent_path_for_daemon_path(mounts: &[Mount], daemon: &Path) -> Option<PathBuf> {
    let candidate = translate(mounts, daemon, false)?;
    (daemon_path_for_agent_path(mounts, &candidate)?.0 == daemon).then_some(candidate)
}

fn translate(mounts: &[Mount], path: &Path, agent_to_daemon: bool) -> Option<PathBuf> {
    if !path.is_absolute()
        || path
            .components()
            .any(|part| matches!(part, std::path::Component::ParentDir))
    {
        return None;
    }
    if mounts.iter().any(|mount| {
        let destination = Path::new(&mount.destination);
        !destination.is_absolute()
            || destination
                .components()
                .any(|part| matches!(part, std::path::Component::ParentDir))
            || mount.source.as_ref().is_some_and(|source| {
                !source.0.is_absolute()
                    || source
                        .0
                        .components()
                        .any(|part| matches!(part, std::path::Component::ParentDir))
            })
    }) {
        return None;
    }
    let candidates = mounts
        .iter()
        .filter_map(|mount| {
            let destination = Path::new(&mount.destination);
            let from = if agent_to_daemon {
                destination
            } else {
                mount.source.as_ref()?.0.as_path()
            };
            let suffix = path.strip_prefix(from).ok()?;
            Some((from.components().count(), mount, suffix))
        })
        .collect::<Vec<_>>();
    let longest = candidates.iter().map(|(len, _, _)| *len).max()?;
    if candidates
        .iter()
        .filter(|(len, _, _)| *len == longest)
        .count()
        != 1
    {
        return None;
    }
    let (_, mount, suffix) = candidates.into_iter().find(|(len, _, _)| *len == longest)?;
    if mount.kind != MountKind::Bind {
        return None;
    }
    let to = if agent_to_daemon {
        mount.source.as_ref()?.0.as_path()
    } else {
        Path::new(&mount.destination)
    };
    Some(to.join(suffix))
}
