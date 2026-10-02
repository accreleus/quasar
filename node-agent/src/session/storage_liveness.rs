//! A single fail-closed storage-liveness snapshot for both home reapers.
//!
//! Docker reports bind sources in the daemon host namespace while the agent
//! sees candidates in its own namespace.  They are deliberately translated
//! only through the agent container's inspected bind mounts.

use std::path::{Path, PathBuf};

use anyhow::{anyhow, Context, Result};

use crate::runtime::{self, daemon_path_for_agent_path, ContainerInspection, Mount, MountKind};

#[derive(Debug, Clone)]
pub(crate) struct Snapshot {
    /// `None` means a native agent sharing the daemon's filesystem namespace.
    pub own_mounts: Option<Vec<Mount>>,
    pub foreign: Vec<ContainerInspection>,
    pub proc_mounts: Vec<PathBuf>,
}

/// Point-in-time evidence used to decide whether a local home may be deleted.
#[derive(Debug, Clone)]
pub(crate) struct StorageLiveness(Snapshot);

impl StorageLiveness {
    /// Capture all live containers (including foreign workloads), the local
    /// mount namespace, and, when containerized, the agent's bind mapping.
    pub fn capture() -> Result<Self> {
        let runtime = runtime::configured().map_err(|e| anyhow!(e))?;
        let own = match crate::nvidia_volume::self_container_id() {
            Some(id) => runtime
                .inspect_container(id)
                .wait()
                .map_err(|e| anyhow!(e))?
                .ok_or_else(|| anyhow!("agent container disappeared during liveness capture"))?,
            None if inside_container() => {
                return Err(anyhow!(
                    "cannot identify containerized agent for storage liveness"
                ));
            }
            None => {
                // A native agent and daemon use the same path namespace.
                return Self::native(runtime.live_containers().wait().map_err(|e| anyhow!(e))?);
            }
        };
        let live = runtime.live_containers().wait().map_err(|e| anyhow!(e))?;
        let foreign = live.into_iter().filter(|item| item.id != own.id).collect();
        Ok(Self(Snapshot {
            own_mounts: Some(own.mounts),
            foreign,
            proc_mounts: read_proc_mounts()?,
        }))
    }

    fn native(foreign: Vec<ContainerInspection>) -> Result<Self> {
        Ok(Self(Snapshot {
            own_mounts: None,
            foreign,
            proc_mounts: read_proc_mounts()?,
        }))
    }

    /// Test boundary without Docker SDK mocks.
    #[cfg(test)]
    pub(crate) fn from_snapshot(snapshot: Snapshot) -> Self {
        Self(snapshot)
    }

    /// True when a mount proves that `candidate` (or one of its descendants)
    /// is in use.  Unknown namespace translation is an error, never a miss.
    pub fn protects(&self, candidate: &Path) -> Result<bool> {
        if !absolute_clean(candidate) {
            return Err(anyhow!("invalid storage candidate {}", candidate.display()));
        }
        // These are paths visible to this process.  A mount of `/` is not
        // evidence that every home is mounted; only mountpoints at/below the
        // candidate prove use.
        if self
            .0
            .proc_mounts
            .iter()
            .any(|mount| is_under(candidate, mount))
        {
            return Ok(true);
        }
        let daemon_candidate = match &self.0.own_mounts {
            Some(mounts) => daemon_path_for_agent_path(mounts, candidate)
                .map(|path| path.0)
                .ok_or_else(|| {
                    anyhow!("cannot map {} into daemon namespace", candidate.display())
                })?,
            None => candidate.to_path_buf(),
        };
        for container in &self.0.foreign {
            for mount in &container.mounts {
                if matches!(mount.kind, MountKind::Tmpfs) {
                    continue;
                }
                let Some(source) = &mount.source else {
                    return Err(anyhow!(
                        "live container {} has no usable mount source",
                        container.id
                    ));
                };
                if overlaps(&daemon_candidate, &source.0) {
                    return Ok(true);
                }
            }
        }
        Ok(false)
    }

    /// The in-process session set predates daemon inspection.  It may contain
    /// either agent-visible refs or daemon-visible refs, so compare it in both
    /// namespaces without guessing a conversion for an unrecognised value.
    pub fn matches_local_ref(&self, candidate: &Path, reference: &str) -> Result<bool> {
        let reference = Path::new(reference);
        if !absolute_clean(reference) {
            return Ok(false);
        }
        if overlaps(candidate, reference) {
            return Ok(true);
        }
        let Some(mounts) = &self.0.own_mounts else {
            return Ok(false);
        };
        let candidate = daemon_path_for_agent_path(mounts, candidate)
            .map(|path| path.0)
            .ok_or_else(|| anyhow!("cannot map local ref candidate into daemon namespace"))?;
        Ok(overlaps(&candidate, reference))
    }
}

fn inside_container() -> bool {
    Path::new("/.dockerenv").exists()
        || Path::new("/run/.containerenv").exists()
        || std::fs::read_to_string("/proc/1/cgroup")
            .map(|body| {
                body.contains("docker") || body.contains("containerd") || body.contains("kubepods")
            })
            .unwrap_or(true)
}

fn read_proc_mounts() -> Result<Vec<PathBuf>> {
    let body = std::fs::read_to_string("/proc/mounts").context("read /proc/mounts")?;
    Ok(body
        .lines()
        .filter_map(|line| line.split_whitespace().nth(1))
        .map(|path| PathBuf::from(path.replace("\\040", " ")))
        .filter(|path| absolute_clean(path))
        .collect())
}

fn absolute_clean(path: &Path) -> bool {
    path.is_absolute()
        && !path
            .components()
            .any(|part| matches!(part, std::path::Component::ParentDir))
}

fn is_under(parent: &Path, child: &Path) -> bool {
    child == parent || child.starts_with(parent)
}

fn overlaps(left: &Path, right: &Path) -> bool {
    is_under(left, right) || is_under(right, left)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::runtime::{DaemonHostPath, Mount};

    fn bind(source: &str, destination: &str) -> Mount {
        Mount {
            kind: MountKind::Bind,
            source: Some(DaemonHostPath(source.into())),
            name: None,
            destination: destination.into(),
            read_only: Some(false),
        }
    }
    fn foreign(source: &str) -> ContainerInspection {
        ContainerInspection {
            id: "foreign".into(),
            image_id: "image".into(),
            configured_image: "image".into(),
            labels: Default::default(),
            mounts: vec![bind(source, "/home")],
            network_mode: None,
        }
    }

    fn foreign_with(kind: MountKind, source: Option<&str>) -> ContainerInspection {
        ContainerInspection {
            id: "foreign".into(),
            image_id: "image".into(),
            configured_image: "image".into(),
            labels: Default::default(),
            mounts: vec![Mount {
                kind,
                source: source.map(|value| DaemonHostPath(value.into())),
                name: None,
                destination: "/home".into(),
                read_only: Some(false),
            }],
            network_mode: None,
        }
    }

    #[test]
    fn translated_overlap_is_symmetric_and_component_aware() {
        let guard = StorageLiveness::from_snapshot(Snapshot {
            own_mounts: Some(vec![bind("/daemon/homes", "/agent/homes")]),
            foreign: vec![foreign("/daemon/homes/agent-a")],
            proc_mounts: vec![PathBuf::from("/")],
        });
        assert!(guard
            .protects(Path::new("/agent/homes/agent-a/app"))
            .unwrap());
        assert!(!guard.protects(Path::new("/agent/homes/agent-b")).unwrap());
        let parent = StorageLiveness::from_snapshot(Snapshot {
            own_mounts: Some(vec![bind("/daemon/homes", "/agent/homes")]),
            foreign: vec![foreign("/daemon")],
            proc_mounts: vec![],
        });
        assert!(parent.protects(Path::new("/agent/homes/agent-b")).unwrap());
    }

    #[test]
    fn unmapped_container_candidate_fails_closed() {
        let guard = StorageLiveness::from_snapshot(Snapshot {
            own_mounts: Some(vec![bind("/daemon/homes", "/agent/homes")]),
            foreign: vec![],
            proc_mounts: vec![],
        });
        assert!(guard.protects(Path::new("/unmapped/home")).is_err());
    }

    #[test]
    fn non_tmpfs_foreign_mount_without_source_is_unknown() {
        let guard = StorageLiveness::from_snapshot(Snapshot {
            own_mounts: None,
            foreign: vec![foreign_with(MountKind::Volume, None)],
            proc_mounts: vec![],
        });
        assert!(guard.protects(Path::new("/homes/agent-a")).is_err());
    }

    #[test]
    fn foreign_volume_host_source_protects_like_a_bind() {
        let guard = StorageLiveness::from_snapshot(Snapshot {
            own_mounts: None,
            foreign: vec![foreign_with(MountKind::Volume, Some("/homes/agent-a"))],
            proc_mounts: vec![],
        });
        assert!(guard.protects(Path::new("/homes/agent-a/app")).unwrap());
    }
}
