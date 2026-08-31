//! Template-build context handling: the `context_url` HTTPS + host-allowlist guard,
//! the size-capped download, and tar extraction with a zip-slip guard.
//!
//! The SSRF/containment half of `image_build` (agent-api.md), kept out of the op state
//! machine in `mod.rs` so the pure parts are unit-testable with no daemon and no
//! network. The `docker build` invocation itself stays in `mod.rs`.

use std::collections::BTreeSet;
use std::io::{Read, Write};
use std::path::{Component, Path, PathBuf};
use std::time::{Duration, Instant};

use flate2::read::GzDecoder;
use tar::Archive;

/// `context_url` host allowlist (agent-api.md `image_build` SSRF containment).
pub const SOURCE_HOSTS_ENV: &str = "QUASAR_IMAGE_SOURCE_HOSTS";

pub const DEFAULT_SOURCE_HOSTS: &str = "codeload.github.com,github.com";

/// Cap on the COMPRESSED download, so a hostile source cannot fill the disk through
/// the download itself. The pre-build disk guard covers the built image separately.
pub const MAX_CONTEXT_BYTES: u64 = 512 * 1024 * 1024;

/// Decompression-bomb guard: cap on the cumulative *declared* uncompressed size,
/// since a small gzip can declare a disk-filling payload under [`MAX_CONTEXT_BYTES`].
pub const MAX_EXTRACTED_BYTES: u64 = 2 * 1024 * 1024 * 1024;

/// Inode-exhaustion guard: a many-tiny-files bomb exhausts inodes below any byte cap.
pub const MAX_ENTRIES: u64 = 100_000;

/// Per-read timeouts, so a source that connects then goes silent trips rather than
/// hanging the build worker. [`download_context`]'s whole-op deadline caps the total
/// transfer too, so a slow-drip source cannot pin a shared build permit by staying
/// just under the per-read timeout.
const CONNECT_TIMEOUT: Duration = Duration::from_secs(30);
const READ_TIMEOUT: Duration = Duration::from_secs(300);

/// Comma-separated, trimmed, lowercased; empties dropped.
pub fn allowed_source_hosts() -> BTreeSet<String> {
    let raw = std::env::var(SOURCE_HOSTS_ENV).unwrap_or_else(|_| DEFAULT_SOURCE_HOSTS.to_string());
    parse_hosts(&raw)
}

fn parse_hosts(raw: &str) -> BTreeSet<String> {
    raw.split(',')
        .map(|h| h.trim().to_ascii_lowercase())
        .filter(|h| !h.is_empty())
        .collect()
}

/// HTTPS scheme + allowlisted host, returning the host or a short rejection reason
/// fit for `ack{ok:false}` / `image_state{failed}`.
///
/// Hand-rolled rather than the `url` crate: accepted shapes are narrow, and userinfo
/// must be rejected outright, not parsed (`https://codeload.github.com@evil.example/`).
pub fn validate_context_url<'a>(
    url: &'a str,
    allowed: &BTreeSet<String>,
) -> Result<&'a str, &'static str> {
    let rest = url
        .strip_prefix("https://")
        .ok_or("build source must be HTTPS")?;
    // Authority ends at the first '/', '?' or '#'.
    let authority_end = rest.find(['/', '?', '#']).unwrap_or(rest.len());
    let authority = &rest[..authority_end];
    if authority.is_empty() {
        return Err("build source URL has no host");
    }
    // Userinfo: an '@' before the host is an allowlist-bypass vector.
    if authority.contains('@') {
        return Err("build source URL host is not allowed");
    }
    // Split host[:port]. A host holds no other ':'.
    let (host, port) = match authority.split_once(':') {
        Some((h, p)) => (h, Some(p)),
        None => (authority, None),
    };
    if host.is_empty() {
        return Err("build source URL has no host");
    }
    // Only 443 may be named: an odd explicit port aims an allowlisted host at an
    // internal service (SSRF).
    if let Some(p) = port {
        if p != "443" {
            return Err("build source URL port is not allowed");
        }
    }
    if allowed.contains(&host.to_ascii_lowercase()) {
        Ok(&authority[..host.len()])
    } else {
        Err("build source URL host is not allowed")
    }
}

/// The zip-slip guard (agent-api.md): normalize a tar-entry or wire-supplied path,
/// or `None` if it would escape (absolute, root/drive prefix, or any `..`).
///
/// The result holds only `Normal` components, so joining it onto a fixed root can
/// never leave that root. Extraction relies on that invariant.
pub fn sanitize_relative(path: &Path) -> Option<PathBuf> {
    let mut out = PathBuf::new();
    for comp in path.components() {
        match comp {
            Component::Normal(c) => out.push(c),
            Component::CurDir => {}
            // RootDir (absolute), Prefix (drive), ParentDir ("..") all escape.
            Component::RootDir | Component::Prefix(_) | Component::ParentDir => return None,
        }
    }
    Some(out)
}

/// Stream `url` to `dest` under [`MAX_CONTEXT_BYTES`]. The caller must apply the
/// host/HTTPS guard first, so a rejected URL never opens a socket; errors never echo
/// the URL.
///
/// Redirects are disabled and only a direct 200 is accepted: an allowlisted host
/// answering 30x would otherwise reach an arbitrary host with no re-validation.
/// `deadline` bounds the whole transfer on top of the per-read timeout.
pub fn download_context(url: &str, dest: &Path, deadline: Instant) -> Result<(), String> {
    let now = Instant::now();
    if now >= deadline {
        return Err("build context download timed out".to_string());
    }
    let agent = ureq::AgentBuilder::new()
        .timeout_connect(CONNECT_TIMEOUT)
        .timeout_read(READ_TIMEOUT)
        .timeout(deadline - now)
        .redirects(0)
        .build();
    let resp = agent
        .get(url)
        .call()
        .map_err(|e| format!("build context download failed: {}", download_err(&e)))?;
    // ureq returns a 3xx as-is with redirects disabled; anything but 200 is a failure.
    if resp.status() != 200 {
        return Err(format!(
            "build context download failed: HTTP {}",
            resp.status()
        ));
    }
    let mut reader = resp.into_reader().take(MAX_CONTEXT_BYTES + 1);
    let mut file = std::fs::File::create(dest)
        .map_err(|e| format!("could not create build context file: {e}"))?;
    // Manual loop, not io::copy: the deadline and the size cap must both be enforced
    // mid-transfer.
    let mut buf = [0u8; 64 * 1024];
    let mut written: u64 = 0;
    loop {
        if Instant::now() >= deadline {
            return Err("build context download timed out".to_string());
        }
        let n = reader
            .read(&mut buf)
            .map_err(|e| format!("build context read failed: {e}"))?;
        if n == 0 {
            break;
        }
        file.write_all(&buf[..n])
            .map_err(|e| format!("could not write build context file: {e}"))?;
        written += n as u64;
        if written > MAX_CONTEXT_BYTES {
            return Err(format!(
                "build context exceeds the {} MiB size cap",
                MAX_CONTEXT_BYTES / (1024 * 1024)
            ));
        }
    }
    Ok(())
}

/// Short classification; never the URL or body.
fn download_err(e: &ureq::Error) -> String {
    match e {
        ureq::Error::Status(code, _) => format!("HTTP {code}"),
        ureq::Error::Transport(_) => "transport error".to_string(),
    }
}

/// Extract a codeload `.tar.gz` into `dest`.
///
/// The artifact's single top-level `<repo>-<sha>/` prefix is stripped, then only
/// entries under `context_subdir` are unpacked, so `dest` holds that subdir's
/// contents. Every entry path goes through [`sanitize_relative`] first; one that
/// would escape fails the whole extraction.
pub fn extract_context(
    tarball: &Path,
    context_subdir: &str,
    dest: &Path,
    deadline: Instant,
) -> Result<(), String> {
    let subdir = sanitize_relative(Path::new(context_subdir))
        .ok_or("invalid context_subdir (path traversal)")?;

    let file = std::fs::File::open(tarball)
        .map_err(|e| format!("could not open build context tarball: {e}"))?;
    let mut archive = Archive::new(GzDecoder::new(file));
    let entries = archive
        .entries()
        .map_err(|e| format!("build context tarball is unreadable: {e}"))?;

    let mut found = false;
    let mut entry_count: u64 = 0;
    let mut total_declared: u64 = 0;
    for entry in entries {
        if Instant::now() >= deadline {
            return Err("build context extraction timed out".to_string());
        }
        let mut entry = entry.map_err(|e| format!("build context tarball is corrupt: {e}"))?;

        // Bomb guards must run on every entry at the header, before any body is read
        // or written: entry count (inodes) and cumulative declared size (disk).
        entry_count += 1;
        if entry_count > MAX_ENTRIES {
            return Err("build context has too many entries".to_string());
        }
        total_declared = total_declared
            .checked_add(entry.header().size().unwrap_or(0))
            .ok_or("build context declared size overflow")?;
        if total_declared > MAX_EXTRACTED_BYTES {
            return Err("build context uncompressed size exceeds the cap".to_string());
        }

        let entry_type = entry.header().entry_type();

        let raw = entry
            .path()
            .map_err(|e| format!("build context tarball has an unreadable entry path: {e}"))?
            .into_owned();

        // Zip-slip guard: reject BEFORE deciding where to write.
        let safe = sanitize_relative(&raw)
            .ok_or("unsafe tar entry path (path traversal) in build context")?;

        // Strip the codeload top-level <repo>-<sha>/ prefix.
        let mut comps = safe.components();
        comps.next();
        let below_top = comps.as_path();

        // Everything outside context_subdir (codeload's `pax_global_header`, PAX
        // extension headers, the rest of the repo) is skipped before touching disk,
        // so the link guard below only has to cover entries actually unpacked.
        let Ok(under) = below_top.strip_prefix(&subdir) else {
            continue;
        };
        found = true;

        // Accept only dirs and regular files. `unpack` validates no link TARGET (only
        // the entry NAME went through sanitize_relative), so a symlink/hardlink entry
        // could redirect a later write to an arbitrary host path.
        if !(entry_type.is_dir() || entry_type.is_file()) {
            return Err("build context contains a disallowed link or special file".to_string());
        }

        // `under` is Normal-only and `dest` fixed, so the join cannot escape `dest`.
        let target = dest.join(under);
        if entry_type.is_dir() {
            std::fs::create_dir_all(&target)
                .map_err(|e| format!("could not create build context dir: {e}"))?;
        } else {
            if let Some(parent) = target.parent() {
                std::fs::create_dir_all(parent)
                    .map_err(|e| format!("could not create build context dir: {e}"))?;
            }
            entry
                .unpack(&target)
                .map_err(|e| format!("could not unpack build context entry: {e}"))?;
        }
    }

    if !found {
        return Err("context_subdir not found in the build context tarball".to_string());
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::Write;

    fn hosts(raw: &str) -> BTreeSet<String> {
        parse_hosts(raw)
    }

    #[test]
    fn accepts_allowed_https_hosts() {
        let a = hosts(DEFAULT_SOURCE_HOSTS);
        for url in [
            "https://codeload.github.com/accreleus/quasar-images/tar.gz/deadbeef",
            "https://github.com/x/y",
            "https://CodeLoad.GitHub.com/x/y", // case-insensitive host
            "https://codeload.github.com:443/x/y", // explicit port
            "https://github.com",              // authority only
        ] {
            assert!(validate_context_url(url, &a).is_ok(), "should accept {url}");
        }
    }

    #[test]
    fn rejects_non_https_and_disallowed_and_bypass_hosts() {
        let a = hosts(DEFAULT_SOURCE_HOSTS);
        for url in [
            "http://codeload.github.com/x/y",               // not HTTPS
            "ftp://codeload.github.com/x/y",                // not HTTPS
            "https://evil.example/x/y",                     // not allowlisted
            "https://codeload.github.com.evil.example/x/y", // suffix trick
            "https://codeload.github.com@evil.example/x/y", // userinfo bypass
            "https:///x/y",                                 // no host
            "codeload.github.com/x/y",                      // no scheme
        ] {
            assert!(
                validate_context_url(url, &a).is_err(),
                "should reject {url}"
            );
        }
    }

    #[test]
    fn host_allowlist_parses_env_form() {
        let a = hosts(" codeload.github.com , GitHub.com ,,  ");
        assert!(a.contains("codeload.github.com"));
        assert!(a.contains("github.com"));
        assert_eq!(a.len(), 2);
        // A tightened allowlist excludes the default extra host.
        let only = hosts("codeload.github.com");
        assert!(validate_context_url("https://github.com/x", &only).is_err());
    }

    #[test]
    fn sanitize_accepts_plain_relative_paths() {
        assert_eq!(
            sanitize_relative(Path::new("repo-sha/xfce-desktop/Dockerfile")),
            Some(PathBuf::from("repo-sha/xfce-desktop/Dockerfile"))
        );
        // "." segments are dropped.
        assert_eq!(
            sanitize_relative(Path::new("a/./b")),
            Some(PathBuf::from("a/b"))
        );
    }

    #[test]
    fn sanitize_rejects_absolute_and_parent_escapes() {
        // Absolute path.
        assert_eq!(sanitize_relative(Path::new("/etc/passwd")), None);
        // Leading "..".
        assert_eq!(sanitize_relative(Path::new("../escape")), None);
        // Embedded "..".
        assert_eq!(sanitize_relative(Path::new("repo-sha/../../escape")), None);
        assert_eq!(sanitize_relative(Path::new("a/b/../../../c")), None);
    }

    /// Build a `.tar.gz` from `(path, is_dir, contents)` entries.
    fn write_targz(dest: &Path, entries: &[(&str, bool, &[u8])]) {
        let file = std::fs::File::create(dest).unwrap();
        let enc = flate2::write::GzEncoder::new(file, flate2::Compression::fast());
        let mut builder = tar::Builder::new(enc);
        for (path, is_dir, contents) in entries {
            let mut header = tar::Header::new_gnu();
            header.set_path(path).unwrap();
            if *is_dir {
                header.set_entry_type(tar::EntryType::Directory);
                header.set_size(0);
                header.set_mode(0o755);
                header.set_cksum();
                builder.append(&header, std::io::empty()).unwrap();
            } else {
                header.set_size(contents.len() as u64);
                header.set_mode(0o644);
                header.set_cksum();
                builder.append(&header, *contents).unwrap();
            }
        }
        let enc = builder.into_inner().unwrap();
        enc.finish().unwrap();
    }

    #[test]
    fn extracts_only_the_context_subdir_stripping_the_top_level_prefix() {
        let dir = tempfile::tempdir().unwrap();
        let tarball = dir.path().join("ctx.tar.gz");
        write_targz(
            &tarball,
            &[
                ("quasar-images-sha/", true, b""),
                ("quasar-images-sha/README.md", false, b"ignore me"),
                ("quasar-images-sha/xfce-desktop/", true, b""),
                (
                    "quasar-images-sha/xfce-desktop/Dockerfile",
                    false,
                    b"FROM alpine:3\n",
                ),
                (
                    "quasar-images-sha/xfce-desktop/sub/app.sh",
                    false,
                    b"#!/bin/sh\n",
                ),
            ],
        );
        let out = dir.path().join("out");
        std::fs::create_dir_all(&out).unwrap();
        extract_context(&tarball, "xfce-desktop", &out, far_future()).unwrap();

        assert_eq!(
            std::fs::read_to_string(out.join("Dockerfile")).unwrap(),
            "FROM alpine:3\n"
        );
        assert!(out.join("sub/app.sh").is_file());
        // Files outside the subdir are not extracted.
        assert!(!out.join("README.md").exists());
    }

    /// A raw ustar entry. Hand-rolled because `Builder::set_path` refuses to write a
    /// `..` entry, which is exactly what an attacker would send.
    fn raw_tar_block(name: &str, data: &[u8]) -> Vec<u8> {
        let mut h = [0u8; 512];
        let nb = name.as_bytes();
        h[..nb.len()].copy_from_slice(nb);
        h[100..107].copy_from_slice(b"0000644");
        h[108..115].copy_from_slice(b"0000000");
        h[116..123].copy_from_slice(b"0000000");
        h[124..135].copy_from_slice(format!("{:011o}", data.len()).as_bytes());
        h[136..147].copy_from_slice(b"00000000000");
        h[156] = b'0'; // typeflag: regular file
        h[257..262].copy_from_slice(b"ustar");
        h[263..265].copy_from_slice(b"00");
        for b in &mut h[148..156] {
            *b = b' ';
        }
        let sum: u32 = h.iter().map(|&b| b as u32).sum();
        h[148..156].copy_from_slice(format!("{sum:06o}\0 ").as_bytes());
        let mut out = h.to_vec();
        out.extend_from_slice(data);
        let pad = (512 - data.len() % 512) % 512;
        out.extend(std::iter::repeat_n(0u8, pad));
        out
    }

    fn write_raw_targz(dest: &Path, entries: &[(&str, &[u8])]) {
        let mut tar = Vec::new();
        for (name, data) in entries {
            tar.extend(raw_tar_block(name, data));
        }
        tar.extend(std::iter::repeat_n(0u8, 1024)); // two zero blocks: EOF
        let file = std::fs::File::create(dest).unwrap();
        let mut enc = flate2::write::GzEncoder::new(file, flate2::Compression::fast());
        enc.write_all(&tar).unwrap();
        enc.finish().unwrap();
    }

    #[test]
    fn extraction_rejects_a_parent_traversal_entry() {
        let dir = tempfile::tempdir().unwrap();
        let tarball = dir.path().join("evil.tar.gz");
        write_raw_targz(
            &tarball,
            &[
                (
                    "quasar-images-sha/xfce-desktop/Dockerfile",
                    b"FROM alpine\n",
                ),
                // Zip-slip: escape the extraction root via `..`.
                (
                    "quasar-images-sha/xfce-desktop/../../../../etc/pwned",
                    b"owned",
                ),
            ],
        );
        let out = dir.path().join("out");
        std::fs::create_dir_all(&out).unwrap();
        let err = extract_context(&tarball, "xfce-desktop", &out, far_future()).unwrap_err();
        assert!(err.contains("path traversal"), "unexpected error: {err}");
        // Nothing escaped the sandbox.
        assert!(!dir.path().join("etc/pwned").exists());
    }

    #[test]
    fn extraction_errors_when_the_subdir_is_missing() {
        let dir = tempfile::tempdir().unwrap();
        let tarball = dir.path().join("ctx.tar.gz");
        write_targz(
            &tarball,
            &[(
                "quasar-images-sha/other/Dockerfile",
                false,
                b"FROM alpine\n",
            )],
        );
        let out = dir.path().join("out");
        std::fs::create_dir_all(&out).unwrap();
        let err = extract_context(&tarball, "xfce-desktop", &out, far_future()).unwrap_err();
        assert!(err.contains("not found"), "unexpected error: {err}");
    }

    #[test]
    fn download_cap_constant_is_bytes() {
        // Guards an accidental MiB/bytes mixup in the cap.
        assert_eq!(MAX_CONTEXT_BYTES, 512 * 1024 * 1024);
        let _ = std::io::sink().write(b"");
    }

    fn far_future() -> Instant {
        Instant::now() + Duration::from_secs(3600)
    }

    /// A link/special entry, which the `tar` builder will write (unlike a `..` path).
    fn write_targz_with_link(dest: &Path, kind: tar::EntryType, name: &str, link_target: &str) {
        let file = std::fs::File::create(dest).unwrap();
        let enc = flate2::write::GzEncoder::new(file, flate2::Compression::fast());
        let mut builder = tar::Builder::new(enc);
        // A benign Dockerfile first, so the subdir is "found" and only the link entry
        // can abort the extraction.
        let mut df = tar::Header::new_gnu();
        df.set_path("quasar-images-sha/xfce-desktop/Dockerfile")
            .unwrap();
        df.set_size(11);
        df.set_mode(0o644);
        df.set_cksum();
        builder.append(&df, &b"FROM alpine\n"[..]).unwrap();

        let mut h = tar::Header::new_gnu();
        h.set_entry_type(kind);
        h.set_path(name).unwrap();
        h.set_link_name(link_target).unwrap();
        h.set_size(0);
        h.set_mode(0o777);
        h.set_cksum();
        builder.append(&h, std::io::empty()).unwrap();
        let enc = builder.into_inner().unwrap();
        enc.finish().unwrap();
    }

    #[test]
    fn extraction_rejects_a_symlink_entry() {
        let dir = tempfile::tempdir().unwrap();
        let tarball = dir.path().join("evil.tar.gz");
        write_targz_with_link(
            &tarball,
            tar::EntryType::Symlink,
            "quasar-images-sha/xfce-desktop/link",
            "/etc/cron.d",
        );
        let out = dir.path().join("out");
        std::fs::create_dir_all(&out).unwrap();
        let err = extract_context(&tarball, "xfce-desktop", &out, far_future()).unwrap_err();
        assert!(err.contains("disallowed link"), "unexpected error: {err}");
        assert!(!out.join("link").exists());
    }

    #[test]
    fn extraction_skips_a_link_outside_the_context_subdir() {
        // A symlink elsewhere in the repo, or codeload's `pax_global_header`, must be
        // skipped rather than fail the build: the link guard covers only entries
        // under context_subdir.
        let dir = tempfile::tempdir().unwrap();
        let tarball = dir.path().join("mixed.tar.gz");
        let file = std::fs::File::create(&tarball).unwrap();
        let enc = flate2::write::GzEncoder::new(file, flate2::Compression::fast());
        let mut builder = tar::Builder::new(enc);
        let mut link = tar::Header::new_gnu();
        link.set_entry_type(tar::EntryType::Symlink);
        link.set_path("quasar-images-sha/docs/latest").unwrap();
        link.set_link_name("/etc/passwd").unwrap();
        link.set_size(0);
        link.set_mode(0o777);
        link.set_cksum();
        builder.append(&link, std::io::empty()).unwrap();
        let mut df = tar::Header::new_gnu();
        df.set_path("quasar-images-sha/xfce-desktop/Dockerfile")
            .unwrap();
        df.set_size(12);
        df.set_mode(0o644);
        df.set_cksum();
        builder.append(&df, &b"FROM alpine\n"[..]).unwrap();
        let enc = builder.into_inner().unwrap();
        enc.finish().unwrap();

        let out = dir.path().join("out");
        std::fs::create_dir_all(&out).unwrap();
        extract_context(&tarball, "xfce-desktop", &out, far_future())
            .expect("a link outside the context subdir must be skipped, not fail the build");
        assert!(out.join("Dockerfile").exists());
    }

    #[test]
    fn extraction_rejects_a_hardlink_entry() {
        let dir = tempfile::tempdir().unwrap();
        let tarball = dir.path().join("evil.tar.gz");
        write_targz_with_link(
            &tarball,
            tar::EntryType::Link,
            "quasar-images-sha/xfce-desktop/hard",
            "quasar-images-sha/xfce-desktop/Dockerfile",
        );
        let out = dir.path().join("out");
        std::fs::create_dir_all(&out).unwrap();
        let err = extract_context(&tarball, "xfce-desktop", &out, far_future()).unwrap_err();
        assert!(err.contains("disallowed link"), "unexpected error: {err}");
    }

    /// A raw ustar entry whose declared size is independent of its data: the shape a
    /// decompression bomb uses.
    fn raw_tar_block_declared(name: &str, declared: u64, data: &[u8]) -> Vec<u8> {
        let mut h = [0u8; 512];
        let nb = name.as_bytes();
        h[..nb.len()].copy_from_slice(nb);
        h[100..107].copy_from_slice(b"0000644");
        h[108..115].copy_from_slice(b"0000000");
        h[116..123].copy_from_slice(b"0000000");
        h[124..135].copy_from_slice(format!("{declared:011o}").as_bytes());
        h[136..147].copy_from_slice(b"00000000000");
        h[156] = b'0'; // typeflag: regular file
        h[257..262].copy_from_slice(b"ustar");
        h[263..265].copy_from_slice(b"00");
        for b in &mut h[148..156] {
            *b = b' ';
        }
        let sum: u32 = h.iter().map(|&b| b as u32).sum();
        h[148..156].copy_from_slice(format!("{sum:06o}\0 ").as_bytes());
        let mut out = h.to_vec();
        out.extend_from_slice(data);
        let pad = (512 - data.len() % 512) % 512;
        out.extend(std::iter::repeat_n(0u8, pad));
        out
    }

    #[test]
    fn extraction_rejects_a_declared_size_bomb() {
        let dir = tempfile::tempdir().unwrap();
        let tarball = dir.path().join("bomb.tar.gz");
        // A few hundred compressed bytes declaring >2 GiB, with no actual body.
        let mut tar = Vec::new();
        tar.extend(raw_tar_block_declared(
            "quasar-images-sha/xfce-desktop/huge",
            MAX_EXTRACTED_BYTES + 1,
            b"",
        ));
        tar.extend(std::iter::repeat_n(0u8, 1024)); // EOF
        let file = std::fs::File::create(&tarball).unwrap();
        let mut enc = flate2::write::GzEncoder::new(file, flate2::Compression::fast());
        enc.write_all(&tar).unwrap();
        enc.finish().unwrap();

        let out = dir.path().join("out");
        std::fs::create_dir_all(&out).unwrap();
        let err = extract_context(&tarball, "xfce-desktop", &out, far_future()).unwrap_err();
        assert!(
            err.contains("uncompressed size exceeds"),
            "unexpected error: {err}"
        );
    }

    #[test]
    fn download_treats_a_redirect_as_failure() {
        use std::io::{Read as _, Write as _};
        use std::net::TcpListener;
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let addr = listener.local_addr().unwrap();
        let handle = std::thread::spawn(move || {
            if let Ok((mut stream, _)) = listener.accept() {
                let mut buf = [0u8; 1024];
                let _ = stream.read(&mut buf);
                let _ = stream.write_all(
                    b"HTTP/1.1 302 Found\r\nLocation: https://evil.example/\r\nContent-Length: 0\r\n\r\n",
                );
            }
        });
        let dir = tempfile::tempdir().unwrap();
        let dest = dir.path().join("ctx.tar.gz");
        let url = format!("http://{addr}/x");
        let err =
            download_context(&url, &dest, Instant::now() + Duration::from_secs(30)).unwrap_err();
        assert!(
            err.contains("HTTP 302") || err.to_lowercase().contains("download failed"),
            "unexpected error: {err}"
        );
        let _ = handle.join();
    }
}
