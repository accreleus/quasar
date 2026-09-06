//! Persistent ownership of session/audio siblings on a shared Docker daemon.
//! The lease file is never renamed or unlinked: its inode is the process lock.
use std::fs::{File, OpenOptions};
use std::io::{Read, Write};
use std::os::fd::AsRawFd;
use std::os::unix::fs::OpenOptionsExt;
use std::path::Path;
use std::sync::OnceLock;

pub(crate) const LABEL: &str = "io.quasar.agent-owner";
static OWNER: OnceLock<Result<Owner, String>> = OnceLock::new();

struct Owner {
    token: String,
    _lease: File,
}

pub(crate) fn initialize(secret_path: &str) -> Result<(), String> {
    OWNER
        .get_or_init(|| acquire(Path::new(&format!("{secret_path}.container-owner"))))
        .as_ref()
        .map(|_| ())
        .map_err(Clone::clone)
}

/// Standalone session harnesses use the same persisted state convention, but
/// diagnostic commands that launch no managed siblings never acquire ownership.
pub(crate) fn token() -> Result<String, String> {
    if OWNER.get().is_none() {
        let path = std::env::var("NODE_SECRET_PATH").unwrap_or_else(|_| {
            let name =
                std::env::var("NODE_NAME").unwrap_or_else(|_| crate::config::detect_hostname());
            format!("/tmp/quasar-{name}-secret")
        });
        initialize(&path)?;
    }
    OWNER
        .get()
        .expect("ownership initialized")
        .as_ref()
        .map(|owner| owner.token.clone())
        .map_err(Clone::clone)
}

fn acquire(path: &Path) -> Result<Owner, String> {
    let error = |detail: String| {
        format!(
        "Cannot acquire agent container ownership at {}: {detail}. Stop any other agent using the same NODE_SECRET_PATH and check persistent-state permissions. Preserve the existing owner file; deleting it loses ownership of prior session containers.", path.display())
    };
    if let Some(parent) = path.parent().filter(|p| !p.as_os_str().is_empty()) {
        std::fs::create_dir_all(parent).map_err(|e| error(e.to_string()))?;
    }
    let (mut lease, created) = match OpenOptions::new()
        .read(true)
        .write(true)
        .create_new(true)
        .mode(0o600)
        .custom_flags(libc::O_NOFOLLOW)
        .open(path)
    {
        Ok(file) => (file, true),
        Err(e) if e.kind() == std::io::ErrorKind::AlreadyExists => (
            OpenOptions::new()
                .read(true)
                .write(true)
                .custom_flags(libc::O_NOFOLLOW)
                .open(path)
                .map_err(|e| error(e.to_string()))?,
            false,
        ),
        Err(e) => return Err(error(e.to_string())),
    };
    // SAFETY: lease owns this live fd throughout the call and until Owner drops.
    if unsafe { libc::flock(lease.as_raw_fd(), libc::LOCK_EX | libc::LOCK_NB) } != 0 {
        return Err(error(format!(
            "another agent holds the state lease ({})",
            std::io::Error::last_os_error()
        )));
    }
    let token = if created {
        let mut random = [0u8; 32];
        File::open("/dev/urandom")
            .and_then(|mut file| file.read_exact(&mut random))
            .map_err(|e| error(e.to_string()))?;
        let token = random
            .iter()
            .map(|byte| format!("{byte:02x}"))
            .collect::<String>();
        writeln!(lease, "{token}")
            .and_then(|_| lease.sync_all())
            .map_err(|e| error(e.to_string()))?;
        token
    } else {
        if lease.metadata().map_err(|e| error(e.to_string()))?.len() > 65 {
            return Err(error(
                "existing owner identity is oversized or malformed".into(),
            ));
        }
        let mut raw = String::new();
        (&mut lease)
            .take(256)
            .read_to_string(&mut raw)
            .map_err(|e| error(e.to_string()))?;
        let token = raw.trim();
        if token.len() != 64 || !token.bytes().all(|b| b.is_ascii_hexdigit()) {
            return Err(error("existing owner identity is malformed; restore its original value rather than regenerating it".into()));
        }
        token.to_owned()
    };
    Ok(Owner {
        token,
        _lease: lease,
    })
}

pub(crate) fn managed_name(name: &str) -> bool {
    let name = name.strip_prefix('/').unwrap_or(name);
    name.starts_with(crate::session::container::SESSION_NAME_PREFIX)
        || name.starts_with(crate::session::audio::PULSE_NAME_PREFIX)
}

/// Inspect data is verified independently of Docker's listing filters. A label
/// alone never authorizes deletion of an unrelated prefix; a prefix alone never
/// authorizes deletion of another agent's or legacy unlabelled containers.
pub(crate) fn owned_id(
    value: &serde_json::Value,
    owner: &str,
    prefixes: &[&str],
) -> Option<String> {
    let id = value["Id"].as_str()?;
    let name = value["Name"].as_str()?.strip_prefix('/')?;
    if id.len() != 64
        || !id.bytes().all(|b| b.is_ascii_hexdigit())
        || !prefixes.iter().any(|prefix| name.starts_with(prefix))
        || value["Labels"][LABEL].as_str() != Some(owner)
    {
        return None;
    }
    Some(id.to_owned())
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn ownership_survives_restart_and_distinct_agents_are_isolated() {
        let dir = tempfile::tempdir().unwrap();
        let first_path = dir.path().join("one");
        let first = acquire(&first_path).unwrap();
        let second = acquire(&dir.path().join("two")).unwrap();
        assert_ne!(first.token, second.token);
        assert!(
            acquire(&first_path).is_err(),
            "simultaneous agent sharing state must not sweep"
        );
        let saved = first.token.clone();
        drop(first);
        assert_eq!(acquire(&first_path).unwrap().token, saved);
    }

    #[test]
    fn malformed_existing_owner_is_not_regenerated() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("owner");
        for content in ["", "broken identity\n"] {
            std::fs::write(&path, content).unwrap();
            assert!(acquire(&path).is_err());
            assert_eq!(std::fs::read_to_string(&path).unwrap(), content);
        }
    }

    #[test]
    fn oversized_and_symlinked_owner_files_fail_closed() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("owner");
        let oversized = format!("{}{}invalid", "a".repeat(64), " ".repeat(256));
        std::fs::write(&path, &oversized).unwrap();
        assert!(acquire(&path).is_err());
        assert_eq!(std::fs::read_to_string(&path).unwrap(), oversized);
        let link = dir.path().join("link");
        std::os::unix::fs::symlink(&path, &link).unwrap();
        assert!(acquire(&link).is_err());
    }

    #[test]
    fn cleanup_requires_both_exact_prefix_and_matching_owner_for_all_states() {
        let prefixes = ["quasar-sess-", "quasar-pulse-"];
        for running in [true, false] {
            for name in ["/quasar-sess-sid", "/quasar-pulse-sid"] {
                let mut value = json!({"Id": "a".repeat(64), "Name": name,
                    "Labels": {LABEL: "one"}, "State": {"Running": running}});
                assert!(owned_id(&value, "one", &prefixes).is_some());
                assert!(owned_id(&value, "two", &prefixes).is_none());
                value["Labels"] = json!({});
                assert!(owned_id(&value, "one", &prefixes).is_none());
            }
        }
        for name in ["/other-quasar-sess-sid", "/database", "/quasar-session"] {
            let value = json!({"Id": "b".repeat(64), "Name": name, "Labels": {LABEL: "one"}});
            assert!(owned_id(&value, "one", &prefixes).is_none());
        }
    }
}
