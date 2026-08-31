//! #488 §6.3 — publish-time verification. Fail the template build, never a user
//! session.
//!
//! Every assertion here is a **tripwire, not a filter**: nothing is removed to
//! make the tree pass. §6.1 already stripped what a template must not carry,
//! and §6.2's first two safety layers (a warm-up cannot authenticate; no
//! account artifact exists in a settled warm home) say the tree should already
//! be clean. This gate exists so that if either layer is ever wrong, the build
//! aborts loudly rather than poisoning every clone made from it.

use std::fs;
use std::os::unix::fs::MetadataExt;
use std::path::{Path, PathBuf};

/// Directory names / file names that carry Steam account identity or refresh
/// tokens (§1.1 verified all three absent from a settled warm home).
const ACCOUNT_STATE_NAMES: &[&str] = &["userdata", "loginusers.vdf"];

/// Filename prefix for Steam's sentry files (`ssfn<digits>`), the third account
/// artifact.
const ACCOUNT_STATE_PREFIX: &str = "ssfn";

/// The `config.vdf` key that appears once an account has been persisted.
const ACCOUNTS_KEY: &str = "\"Accounts\"";

/// The file whose presence means the Steam client bootstrap actually happened.
const SANITY_MARKER: &str = ".local/share/Steam/steam.sh";

/// The §6.3 sanity floor, as a struct so tests exercise the gate over a
/// fixture tree without materializing 2.5 GiB.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct VerifyPolicy {
    pub min_bytes: u64,
    pub max_bytes: u64,
    pub min_files: u64,
    /// Files whose presence proves the bootstrap ran, relative to the tree root.
    pub marker: &'static str,
    /// Reject entries owned by uid 0. Skipped in tests (a fixture tree is owned
    /// by whoever runs the test, which on a CI box may well be root).
    pub reject_root_owned: bool,
}

impl Default for VerifyPolicy {
    /// The production floor from §6.3: `steam.sh` present, total size within
    /// [1 GiB, 8 GiB], file count ≥ 10 000.
    fn default() -> Self {
        VerifyPolicy {
            min_bytes: 1 << 30,
            max_bytes: 8u64 << 30,
            min_files: 10_000,
            marker: SANITY_MARKER,
            reject_root_owned: true,
        }
    }
}

/// What a verified tree measured — the numbers that go into `.meta.json`.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct TemplateStats {
    pub files: u64,
    pub dirs: u64,
    pub symlinks: u64,
    pub bytes: u64,
}

/// Run every §6.3 assertion over `root`. `Ok(stats)` means publish; `Err(v)`
/// carries every violation found — not just the first, so an operator reading
/// the ERROR line need not re-run to see the next one.
pub fn verify_template(root: &Path, policy: &VerifyPolicy) -> Result<TemplateStats, Vec<String>> {
    let mut stats = TemplateStats::default();
    let mut violations = Vec::new();
    if let Err(e) = walk(root, Path::new(""), policy, &mut stats, &mut violations) {
        violations.push(format!("template tree is unreadable: {e}"));
        return Err(violations);
    }

    if !root.join(policy.marker).exists() {
        violations.push(format!(
            "sanity floor: {} is absent — the warm-up bootstrapped nothing",
            policy.marker
        ));
    }
    if stats.bytes < policy.min_bytes || stats.bytes > policy.max_bytes {
        violations.push(format!(
            "sanity floor: total size {} B is outside [{}, {}]",
            stats.bytes, policy.min_bytes, policy.max_bytes
        ));
    }
    if stats.files < policy.min_files {
        violations.push(format!(
            "sanity floor: file count {} is below {}",
            stats.files, policy.min_files
        ));
    }

    if violations.is_empty() {
        Ok(stats)
    } else {
        Err(violations)
    }
}

fn walk(
    dir: &Path,
    rel: &Path,
    policy: &VerifyPolicy,
    stats: &mut TemplateStats,
    violations: &mut Vec<String>,
) -> std::io::Result<()> {
    let mut children: Vec<PathBuf> = Vec::new();
    for entry in fs::read_dir(dir)? {
        let entry = entry?;
        let name = entry.file_name();
        let name = name.to_string_lossy().to_string();
        let child_rel = rel.join(&name);
        let meta = fs::symlink_metadata(entry.path())?;
        let ft = meta.file_type();

        if ACCOUNT_STATE_NAMES.contains(&name.as_str()) || name.starts_with(ACCOUNT_STATE_PREFIX) {
            violations.push(format!(
                "account state: {} must never exist in a template",
                child_rel.display()
            ));
        }
        if policy.reject_root_owned && meta.uid() == 0 {
            violations.push(format!("root-owned entry: {}", child_rel.display()));
        }

        if ft.is_symlink() {
            stats.symlinks += 1;
        } else if ft.is_dir() {
            stats.dirs += 1;
            children.push(entry.path());
        } else if ft.is_file() {
            stats.files += 1;
            stats.bytes += meta.len();
            if name == "config.vdf" && config_vdf_has_accounts(&entry.path()) {
                violations.push(format!(
                    "account state: {} contains an {ACCOUNTS_KEY} block",
                    child_rel.display()
                ));
            }
        } else {
            violations.push(format!(
                "special file (socket/FIFO/device): {}",
                child_rel.display()
            ));
        }
    }
    for child in children {
        let name = child.file_name().unwrap_or_default();
        walk(&child, &rel.join(name), policy, stats, violations)?;
    }
    Ok(())
}

/// `config.vdf` is deliberately kept (its CM server ping table saves a round
/// of discovery), so the file itself is not evidence — an `Accounts` block
/// inside it is.
fn config_vdf_has_accounts(path: &Path) -> bool {
    // Bounded read: config.vdf is a few KiB, but a corrupt/huge file must not be
    // slurped into memory.
    const MAX: u64 = 4 << 20;
    match fs::metadata(path) {
        Ok(m) if m.len() <= MAX => {}
        _ => return false,
    }
    match fs::read_to_string(path) {
        Ok(s) => s.contains(ACCOUNTS_KEY),
        Err(_) => false,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::Write;

    fn test_policy() -> VerifyPolicy {
        VerifyPolicy {
            min_bytes: 1,
            max_bytes: 1 << 20,
            min_files: 1,
            marker: SANITY_MARKER,
            reject_root_owned: false,
        }
    }

    fn touch(root: &Path, rel: &str, body: &str) {
        let p = root.join(rel);
        fs::create_dir_all(p.parent().unwrap()).unwrap();
        fs::File::create(&p)
            .unwrap()
            .write_all(body.as_bytes())
            .unwrap();
    }

    fn good_tree() -> tempfile::TempDir {
        let d = tempfile::tempdir().unwrap();
        touch(d.path(), SANITY_MARKER, "#!/bin/sh\n");
        touch(
            d.path(),
            ".local/share/Steam/config/config.vdf",
            "\"InstallConfigStore\"\n{\n\t\"CM\"\t\"1\"\n}\n",
        );
        d
    }

    #[test]
    fn a_clean_tree_passes_and_reports_its_stats() {
        let d = good_tree();
        let stats = verify_template(d.path(), &test_policy()).unwrap();
        assert_eq!(stats.files, 2);
        assert!(stats.bytes > 0);
    }

    #[test]
    fn userdata_loginusers_and_ssfn_each_abort_the_build() {
        for artifact in [
            ".local/share/Steam/userdata/1234/config.vdf",
            ".local/share/Steam/config/loginusers.vdf",
            ".local/share/Steam/ssfn8123456789",
        ] {
            let d = good_tree();
            touch(d.path(), artifact, "x");
            let err = verify_template(d.path(), &test_policy()).unwrap_err();
            assert!(
                err.iter().any(|v| v.starts_with("account state:")),
                "{artifact} should have tripped the account-state assertion, got {err:?}"
            );
        }
    }

    #[test]
    fn an_accounts_block_in_config_vdf_aborts_the_build() {
        let d = good_tree();
        touch(
            d.path(),
            ".local/share/Steam/config/config.vdf",
            "\"InstallConfigStore\"\n{\n\t\"Accounts\"\n\t{\n\t}\n}\n",
        );
        let err = verify_template(d.path(), &test_policy()).unwrap_err();
        assert!(err.iter().any(|v| v.contains("Accounts")), "{err:?}");
    }

    #[test]
    fn a_fifo_aborts_the_build() {
        let d = good_tree();
        let fifo = d.path().join("steam.pipe");
        let c = std::ffi::CString::new(fifo.to_str().unwrap()).unwrap();
        assert_eq!(unsafe { libc::mkfifo(c.as_ptr(), 0o644) }, 0);
        let err = verify_template(d.path(), &test_policy()).unwrap_err();
        assert!(err.iter().any(|v| v.starts_with("special file")), "{err:?}");
    }

    /// The "succeeded but bootstrapped nothing" case, strictly worse than no
    /// template at all.
    #[test]
    fn a_tree_that_bootstrapped_nothing_is_refused() {
        let d = tempfile::tempdir().unwrap();
        touch(d.path(), "some/file", "x");
        let err = verify_template(d.path(), &test_policy()).unwrap_err();
        assert!(
            err.iter().any(|v| v.contains("steam.sh is absent")),
            "{err:?}"
        );
    }

    #[test]
    fn the_production_sanity_floor_rejects_a_tiny_tree() {
        let d = good_tree();
        let err = verify_template(d.path(), &VerifyPolicy::default()).unwrap_err();
        assert!(err.iter().any(|v| v.contains("total size")), "{err:?}");
        assert!(err.iter().any(|v| v.contains("file count")), "{err:?}");
    }
}
