//! #488 §6.1 — the golden-home strip list, and the sanitizing tree copy that
//! applies it.
//!
//! The design doc specifies the snapshot as
//! `rsync -aHAX --numeric-ids --exclude=…`. This module keeps that exclude
//! list as the single source of truth ([`STRIP_RULES`]) and renders it back
//! out as rsync arguments ([`rsync_excludes`], asserted verbatim against the
//! doc) — but performs the copy itself in Rust ([`copy_tree_sanitized`]).
//!
//! Why not shell out to rsync:
//!
//! 1. **No new image dependency.** Agent images are not contracted to carry
//!    `rsync`, so a shell-out would silently never build a template on a host
//!    whose image lacks the binary.
//! 2. **The strip list becomes testable** over a fixture tree, rather than
//!    delegated to a binary whose flags differ by platform (macOS's rsync
//!    2.6.9 has no `-A`/`-X`).
//! 3. **Sockets and FIFOs are refused, not reproduced.** §1.1 found four
//!    AF_UNIX sockets and two FIFOs in a settled home, all inside the strip
//!    list — but rsync would faithfully recreate any *new* special file a
//!    future Steam version puts outside it, which §6.3 would then abort the
//!    build over. Skipping them here (loudly) keeps that from costing a whole
//!    35s warm-up; §6.3 still asserts the invariant independently.
//!
//! The result is byte-identical to the doc's rsync command for the measured
//! tree: same excludes, symlinks-as-symlinks, ownership, modes.

use std::collections::BTreeMap;
use std::fs;
use std::io;
use std::os::unix::fs::{MetadataExt, PermissionsExt};
use std::path::{Path, PathBuf};

/// One entry of the §6.1 strip list.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum StripRule {
    /// A directory and everything under it (rsync `--exclude="p/"`).
    Dir(&'static str),
    /// One exact file (rsync `--exclude="p"`).
    File(&'static str),
}

/// The §6.1 strip list, verbatim and in the doc's order. Every path is
/// relative to the app's home directory. This list — not the rendered rsync
/// arguments, not the verification gate — is the single definition of what a
/// template does not contain.
pub const STRIP_RULES: &[StripRule] = &[
    // gamescope/dbus/dconf/at-spi runtime dir; 4 sockets + 2 FIFOs.
    StripRule::Dir(".runtime"),
    // nvidia GLCache + mesa_shader_cache; GPU-specific; the 25 root-owned entries.
    StripRule::Dir(".cache"),
    // NSS cert db, regenerated.
    StripRule::Dir(".pki"),
    // per-machine PulseAudio auth cookie.
    StripRule::File(".config/pulse/cookie"),
    // socket dir named from /etc/machine-id.
    StripRule::Dir(".config/ibus"),
    StripRule::File(".steam/steam.pid"),
    // FIFO.
    StripRule::File(".steam/steam.pipe"),
    // 16-byte per-run token.
    StripRule::File(".steam/steam.token"),
    StripRule::File(".steampid"),
    StripRule::File(".steampath"),
    StripRule::Dir(".local/share/Steam/logs"),
    // 4.5 MiB CEF profile with live LevelDB WALs.
    StripRule::Dir(".local/share/Steam/config/htmlcache"),
];

/// Does `rel` (relative to the home root, no leading `/`) fall inside the strip
/// list? A [`StripRule::Dir`] matches the directory and everything beneath it;
/// a [`StripRule::File`] matches only that exact path. Component-wise
/// comparison, so `.cachexyz` is never mistaken for `.cache`.
pub fn is_stripped(rel: &Path) -> bool {
    STRIP_RULES.iter().any(|rule| match rule {
        StripRule::Dir(p) => rel == Path::new(p) || rel.starts_with(p),
        StripRule::File(p) => rel == Path::new(p),
    })
}

/// [`STRIP_RULES`] rendered as the `--exclude=` arguments of the §6.1 rsync
/// command. Not used to run rsync — kept so the list can be compared against
/// the design doc mechanically, and so an operator reproducing a template by
/// hand has the exact command.
pub fn rsync_excludes() -> Vec<String> {
    STRIP_RULES
        .iter()
        .map(|rule| match rule {
            StripRule::Dir(p) => format!("--exclude=\"{p}/\""),
            StripRule::File(p) => format!("--exclude=\"{p}\""),
        })
        .collect()
}

/// What a sanitizing copy produced.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct TreeStats {
    pub files: u64,
    pub dirs: u64,
    pub symlinks: u64,
    pub bytes: u64,
    /// Paths skipped because they matched [`STRIP_RULES`].
    pub stripped: u64,
    /// Sockets/FIFOs skipped (see the module docs). Non-zero is worth a log
    /// line: it means the strip list has drifted from what the app writes.
    pub special_skipped: Vec<PathBuf>,
}

/// Copy `src` to `dst`, applying [`STRIP_RULES`] and refusing special files.
/// Preserves mode, ownership (best effort — a non-root agent cannot `chown`,
/// not fatal since §6.3 asserts the result and WP3's seeding step `chown -R`s
/// anyway), and symlinks as symlinks. Hard links are NOT preserved: the
/// measured tree has none outside `.local/share/Steam`, and a template is a
/// clone source, not an archive.
///
/// `dst` must already exist.
pub fn copy_tree_sanitized(src: &Path, dst: &Path) -> io::Result<TreeStats> {
    let mut stats = TreeStats::default();
    copy_dir(src, dst, Path::new(""), &mut stats)?;
    Ok(stats)
}

fn copy_dir(src: &Path, dst: &Path, rel: &Path, stats: &mut TreeStats) -> io::Result<()> {
    // Deterministic order: two runs over the same tree produce the same log
    // and `special_skipped` ordering.
    let mut entries: BTreeMap<std::ffi::OsString, fs::DirEntry> = BTreeMap::new();
    for entry in fs::read_dir(src)? {
        let entry = entry?;
        entries.insert(entry.file_name(), entry);
    }
    for (name, entry) in entries {
        let child_rel = rel.join(&name);
        if is_stripped(&child_rel) {
            stats.stripped += 1;
            continue;
        }
        let src_path = entry.path();
        let dst_path = dst.join(&name);
        let meta = fs::symlink_metadata(&src_path)?;
        let ft = meta.file_type();
        if ft.is_symlink() {
            let target = fs::read_link(&src_path)?;
            std::os::unix::fs::symlink(&target, &dst_path)?;
            let _ = std::os::unix::fs::lchown(&dst_path, Some(meta.uid()), Some(meta.gid()));
            stats.symlinks += 1;
        } else if ft.is_dir() {
            fs::create_dir_all(&dst_path)?;
            copy_dir(&src_path, &dst_path, &child_rel, stats)?;
            // Mode + ownership AFTER recursion: an unwritable source dir would
            // otherwise block its own children.
            fs::set_permissions(&dst_path, fs::Permissions::from_mode(meta.mode() & 0o7777))?;
            let _ = std::os::unix::fs::chown(&dst_path, Some(meta.uid()), Some(meta.gid()));
            stats.dirs += 1;
        } else if ft.is_file() {
            fs::copy(&src_path, &dst_path)?;
            fs::set_permissions(&dst_path, fs::Permissions::from_mode(meta.mode() & 0o7777))?;
            let _ = std::os::unix::fs::chown(&dst_path, Some(meta.uid()), Some(meta.gid()));
            stats.files += 1;
            stats.bytes += meta.len();
        } else {
            // Socket, FIFO, block/char device. Never copied, never recreated.
            stats.special_skipped.push(child_rel.clone());
        }
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::Write;

    fn touch(root: &Path, rel: &str) {
        let p = root.join(rel);
        fs::create_dir_all(p.parent().unwrap()).unwrap();
        let mut f = fs::File::create(&p).unwrap();
        f.write_all(rel.as_bytes()).unwrap();
    }

    /// Rendered rsync arguments must equal the design doc's §6.1 block
    /// verbatim — the test that catches STRIP_RULES and the doc diverging.
    #[test]
    fn rsync_excludes_match_the_design_doc() {
        assert_eq!(
            rsync_excludes(),
            vec![
                r#"--exclude=".runtime/""#,
                r#"--exclude=".cache/""#,
                r#"--exclude=".pki/""#,
                r#"--exclude=".config/pulse/cookie""#,
                r#"--exclude=".config/ibus/""#,
                r#"--exclude=".steam/steam.pid""#,
                r#"--exclude=".steam/steam.pipe""#,
                r#"--exclude=".steam/steam.token""#,
                r#"--exclude=".steampid""#,
                r#"--exclude=".steampath""#,
                r#"--exclude=".local/share/Steam/logs/""#,
                r#"--exclude=".local/share/Steam/config/htmlcache/""#,
            ]
        );
    }

    #[test]
    fn strip_predicate_matches_dirs_recursively_and_files_exactly() {
        assert!(is_stripped(Path::new(".cache")));
        assert!(is_stripped(Path::new(".cache/nvidia/GLCache/x")));
        assert!(is_stripped(Path::new(".runtime/gamescope-0")));
        assert!(is_stripped(Path::new(".config/ibus/bus/abc-unix-0")));
        assert!(is_stripped(Path::new(".config/pulse/cookie")));
        assert!(is_stripped(Path::new(".steam/steam.token")));
        assert!(is_stripped(Path::new(".local/share/Steam/logs/x.txt")));

        // Near misses that must survive.
        assert!(!is_stripped(Path::new(".cachexyz/f")));
        assert!(!is_stripped(Path::new(".config/pulse/other")));
        assert!(!is_stripped(Path::new(".steam/registry.vdf")));
        assert!(!is_stripped(Path::new(".steam/steam")));
        assert!(!is_stripped(Path::new(".local/share/Steam/steam.sh")));
        assert!(!is_stripped(Path::new(
            ".local/share/Steam/config/config.vdf"
        )));
        // A `logs` dir elsewhere is not the Steam one.
        assert!(!is_stripped(Path::new("logs/x")));
    }

    #[test]
    fn sanitizing_copy_applies_the_strip_list_over_a_fixture_tree() {
        let src = tempfile::tempdir().unwrap();
        let dst = tempfile::tempdir().unwrap();
        let s = src.path();

        // Kept — the value of the feature.
        touch(s, ".local/share/Steam/steam.sh");
        touch(s, ".local/share/Steam/config/config.vdf");
        touch(s, ".local/share/Steam/ubuntu12_32/steam");
        touch(s, ".steam/registry.vdf");
        touch(s, ".steam/exportedsettings.json");
        // Stripped.
        touch(s, ".cache/nvidia/GLCache/blob");
        touch(s, ".cache/mesa_shader_cache/index");
        touch(s, ".pki/nssdb/cert9.db");
        touch(s, ".runtime/gamescope.x/stats");
        touch(s, ".config/pulse/cookie");
        touch(s, ".config/ibus/bus/9bc-unix-0");
        touch(s, ".steam/steam.pid");
        touch(s, ".steam/steam.token");
        touch(s, ".steampid");
        touch(s, ".steampath");
        touch(s, ".local/share/Steam/logs/bootstrap.txt");
        touch(s, ".local/share/Steam/config/htmlcache/000003.log");
        // §1.1 counted 2,563 symlinks of this shape.
        std::os::unix::fs::symlink("/home/quasar/.local/share/Steam", s.join(".steam/steam"))
            .unwrap();

        let stats = copy_tree_sanitized(s, dst.path()).unwrap();
        let d = dst.path();

        for kept in [
            ".local/share/Steam/steam.sh",
            ".local/share/Steam/config/config.vdf",
            ".local/share/Steam/ubuntu12_32/steam",
            ".steam/registry.vdf",
            ".steam/exportedsettings.json",
        ] {
            assert!(d.join(kept).is_file(), "{kept} should have been kept");
        }
        for gone in [
            ".cache",
            ".pki",
            ".runtime",
            ".config/pulse/cookie",
            ".config/ibus",
            ".steam/steam.pid",
            ".steam/steam.token",
            ".steampid",
            ".steampath",
            ".local/share/Steam/logs",
            ".local/share/Steam/config/htmlcache",
        ] {
            assert!(
                fs::symlink_metadata(d.join(gone)).is_err(),
                "{gone} should have been stripped"
            );
        }
        // The symlink is a symlink, not a copy of its target.
        let link = fs::symlink_metadata(d.join(".steam/steam")).unwrap();
        assert!(link.file_type().is_symlink());
        assert_eq!(
            fs::read_link(d.join(".steam/steam")).unwrap(),
            Path::new("/home/quasar/.local/share/Steam")
        );

        assert_eq!(stats.files, 5);
        assert_eq!(stats.symlinks, 1);
        assert!(stats.stripped >= 11, "stats: {stats:?}");
        assert!(stats.special_skipped.is_empty());
        // `.config` survives as an (empty) directory once its two children are
        // stripped — that is what rsync's exclude semantics do too.
        assert!(d.join(".config").is_dir());
    }

    #[test]
    fn sanitizing_copy_refuses_a_special_file_outside_the_strip_list() {
        let src = tempfile::tempdir().unwrap();
        let dst = tempfile::tempdir().unwrap();
        touch(src.path(), ".local/share/Steam/steam.sh");
        let fifo = src.path().join("surprise.pipe");
        let c = std::ffi::CString::new(fifo.to_str().unwrap()).unwrap();
        assert_eq!(unsafe { libc::mkfifo(c.as_ptr(), 0o644) }, 0);

        let stats = copy_tree_sanitized(src.path(), dst.path()).unwrap();
        assert!(fs::symlink_metadata(dst.path().join("surprise.pipe")).is_err());
        assert_eq!(stats.special_skipped, vec![PathBuf::from("surprise.pipe")]);
    }
}
