//! Host-path policy for the bind mounts an assign carries.
//!
//! The wire is untrusted, exactly as for [`super::container::resolve_network`]: an
//! `AppSpec.mounts` entry originates in a catalog manifest authored on another
//! machine, and reaches `docker run -v` verbatim. A source of `/var/run`,
//! `/var/lib/docker` or `/proc/1/root` hands the tenant container the host daemon
//! and therefore host root, so the host — not the control plane — decides which of
//! its own paths a session may see.
//!
//! Default-deny. The managed-home root is allowed read-write because the agent's
//! own home provisioning owns it; everything else must be named by the operator in
//! `QUASAR_APP_MOUNT_ALLOW` and is read-only unless that entry says `:rw`.
//!
//! Symlink resolution is advisory, never an approval: the agent runs in a
//! container, so `/etc` or `/proc/1/root` resolve against ITS rootfs while dockerd
//! binds the source from the HOST's. A canonical path is therefore only ever used
//! to reject; acceptance rests on the lexical path, which `..` rejection keeps
//! honest.

use anyhow::{bail, Result};
use std::path::{Component, Path, PathBuf};

/// Host trees a session may never bind, whatever the allowlist says. Deny beats
/// allow so an operator typo (`QUASAR_APP_MOUNT_ALLOW=/`) cannot reopen the hole.
const DENIED_ROOTS: &[&str] = &[
    "/proc",
    "/sys",
    "/dev",
    "/boot",
    "/etc",
    "/root",
    "/run",
    "/var/run",
    "/var/lib/docker",
    "/var/lib/containerd",
    "/var/lib/containers",
    "/var/lib/kubelet",
    "/var/lib/rancher",
    "/lib/modules",
    "/usr/lib/modules",
];

/// Container-runtime control sockets. A source equal to, or an ANCESTOR of, any of
/// these is refused: `/var/run/docker.sock` was never the whole hole, `/var/run`
/// carries the same socket one directory up.
const RUNTIME_SOCKETS: &[&str] = &[
    "/var/run/docker.sock",
    "/run/docker.sock",
    "/run/containerd/containerd.sock",
    "/var/run/containerd/containerd.sock",
    "/run/crio/crio.sock",
    "/run/podman/podman.sock",
    "/var/run/podman/podman.sock",
    "/var/run/crio/crio.sock",
];

/// Bind options the agent will pass through. `rshared`/`bind-propagation` are
/// absent on purpose: mount propagation out of a tenant container is not a
/// capability a manifest gets to ask for.
const ALLOWED_OPTS: &[&str] = &[
    "ro",
    "rw",
    "z",
    "Z",
    "nocopy",
    "cached",
    "delegated",
    "consistent",
];

/// One operator-named host tree a session may bind.
#[derive(Debug, Clone, PartialEq, Eq)]
struct AllowedRoot {
    path: PathBuf,
    writable: bool,
}

#[derive(Debug, Clone, Default)]
pub struct MountPolicy {
    allowed: Vec<AllowedRoot>,
}

impl MountPolicy {
    /// Build from the agent's live settings plus `QUASAR_APP_MOUNT_ALLOW`.
    pub fn from_env(home_root: &str) -> Self {
        Self::new(
            home_root,
            &std::env::var("QUASAR_APP_MOUNT_ALLOW").unwrap_or_default(),
        )
    }

    /// `allow_spec` is comma-separated `path` or `path:rw`; `home_root` (empty when
    /// the host configures none) is always allowed read-write.
    pub fn new(home_root: &str, allow_spec: &str) -> Self {
        let mut allowed = Vec::new();
        let home = home_root.trim();
        if !home.is_empty() && Path::new(home).is_absolute() {
            allowed.push(AllowedRoot {
                path: lexical_normalize(Path::new(home)),
                writable: true,
            });
        }
        for entry in allow_spec.split(',') {
            let entry = entry.trim();
            if entry.is_empty() {
                continue;
            }
            let (raw, writable) = match entry.strip_suffix(":rw") {
                Some(p) => (p.trim(), true),
                None => (entry.strip_suffix(":ro").unwrap_or(entry).trim(), false),
            };
            let p = Path::new(raw);
            if raw.is_empty() || !p.is_absolute() {
                tracing::warn!(
                    token = "mount-allow-entry-ignored",
                    "QUASAR_APP_MOUNT_ALLOW entry {entry:?} ignored: not an absolute path"
                );
                continue;
            }
            allowed.push(AllowedRoot {
                path: lexical_normalize(p),
                writable,
            });
        }
        Self { allowed }
    }

    /// Vet every wire mount, returning the arguments to hand `docker run -v`.
    /// A rejection fails the assign; nothing is spawned.
    pub fn check_all(&self, mounts: &[String]) -> Result<Vec<String>> {
        mounts.iter().map(|m| self.check(m)).collect()
    }

    /// Vet one `src:dst[:opts]` entry, returning it with `ro` forced on when the
    /// matched rule is not writable.
    pub fn check(&self, mount: &str) -> Result<String> {
        let parts: Vec<&str> = mount.splitn(3, ':').collect();
        if parts.len() < 2 {
            bail!("mount {mount:?} is not in src:dst[:opts] form");
        }
        let (src, dst) = (parts[0], parts[1]);
        let opts_raw = parts.get(2).copied().unwrap_or("");

        let src_path = Path::new(src);
        if src.is_empty() || !src_path.is_absolute() {
            bail!("mount {mount:?}: source must be an absolute host path");
        }
        if dst.is_empty() || !Path::new(dst).is_absolute() {
            bail!("mount {mount:?}: destination must be an absolute container path");
        }
        if dst == "/" {
            bail!("mount {mount:?}: a destination of \"/\" (the container root) is not allowed");
        }
        if mount.contains(['\0', '\n']) {
            bail!("mount {mount:?}: control characters are not allowed");
        }
        if src_path
            .components()
            .any(|c| matches!(c, Component::ParentDir))
        {
            bail!("mount {mount:?}: a source containing \"..\" is not allowed");
        }

        let lexical = lexical_normalize(src_path);
        self.reject_if_sensitive(mount, &lexical)?;
        // Advisory only (see the module doc): a resolved path may name a tree
        // inside THIS container rather than on the host, so it can refuse a mount
        // but never bless one.
        let canonical = resolve_existing_prefix(&lexical);
        if canonical != lexical {
            self.reject_if_sensitive(mount, &canonical)?;
        }

        let Some(rule) = self.allowed.iter().find(|r| is_under(&r.path, &lexical)) else {
            bail!(
                "mount {mount:?}: host path {} is not under the managed-home root or any \
                 QUASAR_APP_MOUNT_ALLOW entry — this host does not let an app image choose \
                 which of its paths a session sees",
                lexical.display()
            );
        };

        let opts = normalize_opts(mount, opts_raw, rule.writable)?;
        Ok(if opts.is_empty() {
            format!("{}:{dst}", lexical.display())
        } else {
            format!("{}:{dst}:{opts}", lexical.display())
        })
    }

    fn reject_if_sensitive(&self, mount: &str, p: &Path) -> Result<()> {
        if p == Path::new("/") {
            bail!("mount {mount:?}: a source of \"/\" (the whole host) is not allowed");
        }
        for denied in DENIED_ROOTS {
            if is_under(Path::new(denied), p) {
                bail!("mount {mount:?}: host path {} is under {denied}, which a session may never bind", p.display());
            }
        }
        for sock in RUNTIME_SOCKETS {
            let sock = Path::new(sock);
            if is_under(p, sock) {
                bail!(
                    "mount {mount:?}: host path {} contains the container-runtime socket {} — \
                     binding it hands the session control of this host's daemon",
                    p.display(),
                    sock.display()
                );
            }
        }
        // Catches a socket the lists above do not name (a rootless or relocated
        // daemon). Absent/unreadable is not evidence, so this only ever adds a
        // rejection.
        if let Ok(md) = std::fs::symlink_metadata(p) {
            use std::os::unix::fs::FileTypeExt;
            let ft = md.file_type();
            if ft.is_socket() || ft.is_char_device() || ft.is_block_device() {
                bail!(
                    "mount {mount:?}: host path {} is a socket or device node, not a directory",
                    p.display()
                );
            }
        }
        Ok(())
    }
}

/// Vet the option list, forcing `ro` when the matched rule is read-only.
fn normalize_opts(mount: &str, raw: &str, writable: bool) -> Result<String> {
    let mut opts: Vec<String> = Vec::new();
    for tok in raw.split(',') {
        let tok = tok.trim();
        if tok.is_empty() {
            continue;
        }
        if !ALLOWED_OPTS.contains(&tok) {
            bail!("mount {mount:?}: bind option {tok:?} is not allowed");
        }
        if !opts.iter().any(|o| o == tok) {
            opts.push(tok.to_string());
        }
    }
    if !writable {
        if opts.iter().any(|o| o == "rw") {
            bail!(
                "mount {mount:?}: this host allows that path read-only — add `:rw` to its \
                 QUASAR_APP_MOUNT_ALLOW entry to permit writes"
            );
        }
        if !opts.iter().any(|o| o == "ro") {
            opts.insert(0, "ro".to_string());
        }
    }
    Ok(opts.join(","))
}

/// Collapse `.` and duplicate separators. `..` is rejected before this runs, so
/// no component here can climb.
fn lexical_normalize(p: &Path) -> PathBuf {
    let mut out = PathBuf::from("/");
    for c in p.components() {
        if let Component::Normal(part) = c {
            out.push(part);
        }
    }
    out
}

/// True when `path` is `root` itself or lives under it. Component-wise, so
/// `/var/lib/dockerfoo` is not under `/var/lib/docker`.
fn is_under(root: &Path, path: &Path) -> bool {
    path == root || path.starts_with(root)
}

/// Resolve symlinks on the longest existing prefix, re-appending the rest. The
/// agent's filesystem view is not dockerd's, so the result is a rejection signal
/// only.
fn resolve_existing_prefix(p: &Path) -> PathBuf {
    let mut suffix: Vec<std::ffi::OsString> = Vec::new();
    let mut cur = p.to_path_buf();
    loop {
        if let Ok(mut real) = cur.canonicalize() {
            for part in suffix.iter().rev() {
                real.push(part);
            }
            return real;
        }
        match (cur.file_name().map(|s| s.to_os_string()), cur.parent()) {
            (Some(name), Some(parent)) if parent != cur => {
                suffix.push(name);
                cur = parent.to_path_buf();
            }
            _ => return p.to_path_buf(),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn policy() -> MountPolicy {
        MountPolicy::new("/var/lib/quasar/homes", "/opt/games,/srv/shared:rw")
    }

    #[test]
    fn managed_home_mount_is_allowed_writable() {
        let m = policy()
            .check("/var/lib/quasar/homes/alice/steam:/home/quasar")
            .expect("managed home must launch");
        assert_eq!(m, "/var/lib/quasar/homes/alice/steam:/home/quasar");
    }

    #[test]
    fn allowlisted_root_defaults_to_read_only() {
        assert_eq!(
            policy().check("/opt/games/lib:/games").unwrap(),
            "/opt/games/lib:/games:ro"
        );
        assert_eq!(
            policy().check("/opt/games:/games:ro").unwrap(),
            "/opt/games:/games:ro"
        );
    }

    #[test]
    fn read_only_root_refuses_an_explicit_rw() {
        assert!(policy().check("/opt/games:/games:rw").is_err());
    }

    #[test]
    fn rw_allowlist_entry_keeps_write_access() {
        assert_eq!(
            policy().check("/srv/shared:/shared").unwrap(),
            "/srv/shared:/shared"
        );
        assert_eq!(
            policy().check("/srv/shared:/shared:rw").unwrap(),
            "/srv/shared:/shared:rw"
        );
    }

    // The classic escape and the three the narrow docker.sock check let through.
    #[test]
    fn escape_sources_are_refused() {
        let p = MountPolicy::new("/var/lib/quasar/homes", "/");
        for m in [
            "/var/run/docker.sock:/var/run/docker.sock",
            "/run/docker.sock:/run/docker.sock",
            "/var/run:/hostrun",
            "/run:/hostrun",
            "/var/lib/docker:/hostdocker",
            "/proc/1/root:/host",
            "/proc:/hostproc",
            "/sys:/hostsys",
            "/dev:/hostdev",
            "/etc:/hostetc",
            "/root:/hostroot",
            "/:/host",
            "/var/lib/containerd:/hostcd",
            "/lib/modules:/lib/modules",
        ] {
            assert!(p.check(m).is_err(), "must reject {m}");
        }
    }

    // Deny beats allow: an operator naming `/` does not reopen the socket.
    #[test]
    fn allowlisting_root_does_not_reopen_denied_trees() {
        let p = MountPolicy::new("", "/:rw");
        assert!(p.check("/var/run/docker.sock:/s").is_err());
        assert!(p.check("/etc/shadow:/x").is_err());
        assert!(p.check("/opt/anything:/x").is_ok());
    }

    #[test]
    fn traversal_and_malformed_entries_are_refused() {
        let p = policy();
        for m in [
            "/var/lib/quasar/homes/../../../var/run/docker.sock:/s",
            "/var/lib/quasar/homes/a/../../../..:/host",
            "relative/path:/x",
            "/var/lib/quasar/homes/a",
            "",
            "/var/lib/quasar/homes/a:relative",
            "/var/lib/quasar/homes/a:/",
        ] {
            assert!(p.check(m).is_err(), "must reject {m}");
        }
    }

    #[test]
    fn unlisted_host_paths_are_refused_by_default() {
        let p = MountPolicy::new("/var/lib/quasar/homes", "");
        assert!(p.check("/opt/games:/games").is_err());
        assert!(p.check("/srv/media:/media:ro").is_err());
        assert!(p.check("/var/lib/quasar/homes/u/a:/home/quasar").is_ok());
    }

    // A component-wise prefix, not a string prefix.
    #[test]
    fn sibling_paths_do_not_inherit_an_allow_or_a_deny() {
        let p = MountPolicy::new("", "/opt/games");
        assert!(p.check("/opt/gamesecret:/x").is_err());
        let q = MountPolicy::new("", "/var/lib");
        assert!(q.check("/var/lib/dockerfoo:/x").is_ok());
        assert!(q.check("/var/lib/docker:/x").is_err());
    }

    #[test]
    fn dangerous_bind_options_are_refused() {
        let p = policy();
        assert!(p.check("/srv/shared:/s:rshared").is_err());
        assert!(p.check("/srv/shared:/s:bind-propagation=rshared").is_err());
        assert!(p.check("/srv/shared:/s:ro,z").is_ok());
    }

    #[test]
    fn an_empty_policy_allows_nothing() {
        let p = MountPolicy::new("", "");
        assert!(p.check("/var/lib/quasar/homes/u/a:/home/quasar").is_err());
        assert!(p.check_all(&[]).unwrap().is_empty());
    }

    #[test]
    fn check_all_fails_the_whole_set_on_one_bad_entry() {
        let p = policy();
        let mounts = vec![
            "/var/lib/quasar/homes/u/a:/home/quasar".to_string(),
            "/var/run:/hostrun".to_string(),
        ];
        assert!(p.check_all(&mounts).is_err());
    }

    #[test]
    fn a_socket_source_is_refused_even_when_allowlisted() {
        let dir = std::env::temp_dir().join(format!("quasar-mp-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let sock = dir.join("some.sock");
        let _l = std::os::unix::net::UnixListener::bind(&sock).unwrap();
        let p = MountPolicy::new("", &format!("{}:rw", dir.display()));
        assert!(p.check(&format!("{}:/s", sock.display())).is_err());
        std::fs::remove_dir_all(&dir).ok();
    }
}
