//! Persistent ownership of session/audio siblings on a shared Docker daemon.
//! The lease file is never renamed or unlinked: its inode is the process lock.
//! The label, the owned-container proof and the lease itself are the shared
//! `quasar-runtime` crate's; the identity file and its recovery wording are ours.
use quasar_runtime::{LeaseError, StateLease};
use std::fs::File;
use std::io::{Read, Write};
use std::path::Path;
use std::sync::OnceLock;

pub(crate) use quasar_runtime::ownership::{owned_id, LABEL};
/// Name prefix of every host-probe container (#258). Alongside the session
/// (`quasar-sess-`) and audio (`quasar-pulse-`) prefixes, it is one of the
/// three owned prefixes: the runtime refuses a probe request outside it, and
/// the boot legacy sweep never removes a container carrying it, because probe
/// teardown belongs to the durable helper journal.
pub(crate) const PROBE_NAME_PREFIX: &str = "quasar-probe-";
static OWNER: OnceLock<Result<Owner, String>> = OnceLock::new();

struct Owner {
    token: String,
    _lease: StateLease,
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
        let path = standalone_secret_path();
        initialize(&path)?;
    }
    OWNER
        .get()
        .expect("ownership initialized")
        .as_ref()
        .map(|owner| owner.token.clone())
        .map_err(Clone::clone)
}

/// Standalone session tools use the same identity and runtime-state namespace
/// as their ownership lease. The normal agent supplies its configured path.
pub(crate) fn standalone_secret_path() -> String {
    std::env::var("NODE_SECRET_PATH").unwrap_or_else(|_| {
        let name = std::env::var("NODE_NAME").unwrap_or_else(|_| crate::config::detect_hostname());
        format!("/tmp/quasar-{name}-secret")
    })
}

fn acquire(path: &Path) -> Result<Owner, String> {
    let error = |detail: String| {
        format!(
        "Cannot acquire agent container ownership at {}: {detail}. Stop any other agent using the same NODE_SECRET_PATH and check persistent-state permissions. Preserve the existing owner file; deleting it loses ownership of prior session containers.", path.display())
    };
    if let Some(parent) = path.parent().filter(|p| !p.as_os_str().is_empty()) {
        std::fs::create_dir_all(parent).map_err(|e| error(e.to_string()))?;
    }
    let lease = StateLease::acquire(path).map_err(|e| match e {
        LeaseError::Open(e) => error(e.to_string()),
        LeaseError::Held(e) => error(format!("another agent holds the state lease ({e})")),
    })?;
    let created = lease.created();
    let mut file = lease.file();
    let token = if created {
        let mut random = [0u8; 32];
        File::open("/dev/urandom")
            .and_then(|mut file| file.read_exact(&mut random))
            .map_err(|e| error(e.to_string()))?;
        let token = random
            .iter()
            .map(|byte| format!("{byte:02x}"))
            .collect::<String>();
        writeln!(file, "{token}")
            .and_then(|_| file.sync_all())
            .map_err(|e| error(e.to_string()))?;
        token
    } else {
        if file.metadata().map_err(|e| error(e.to_string()))?.len() > 65 {
            return Err(error(
                "existing owner identity is oversized or malformed".into(),
            ));
        }
        let mut raw = String::new();
        file.take(256)
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

#[cfg(test)]
mod tests {
    use super::*;

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
        let again = crate::test_lease::reacquire(
            &first_path,
            || acquire(&first_path),
            |e| e.contains("another agent holds the state lease"),
        );
        assert_eq!(again.token, saved);
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
}
