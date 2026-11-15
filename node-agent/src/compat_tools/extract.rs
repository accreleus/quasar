//! T5 and T6: extracting a verified GE-Proton tarball and reading its `compatibilitytool.vdf`.
//! semantics: control-api.md §Compatibility tools, "Trust rules".
//!
//! Every entry is created relative to an open directory, one component at a time with
//! `O_NOFOLLOW`, so nothing is ever written through a symlink. Symlinks are resolved only after
//! the last entry, with `openat2(RESOLVE_BENEATH)` from the tool's top-level directory.

use std::collections::HashSet;
use std::ffi::{CString, OsStr};
use std::fs::File;
use std::io::{self, Read, Write};
use std::os::fd::{AsRawFd, FromRawFd};
use std::os::unix::ffi::OsStrExt;

use super::Fail;
use crate::nofollow_dir::{mkdir_at, open_dir_at};

pub const MAX_UNPACKED: u64 = 4 << 30;
pub const MAX_ENTRIES: u64 = 200_000;

/// Components and bytes in one entry path or link target. A deeper path would only cost file
/// descriptors and stack in every later walk of the tree; GE-Proton's deepest is far shallower.
pub const MAX_COMPONENTS: usize = 32;
pub const MAX_PATH: usize = 4096;

/// The T5 caps. Only tests pass anything but [`Limits::CONTRACT`].
#[derive(Debug, Clone, Copy)]
pub struct Limits {
    pub entries: u64,
    pub bytes: u64,
    /// Bytes written between two calls of the extraction's check (free space, abort).
    pub check_every: u64,
}

impl Limits {
    pub const CONTRACT: Limits = Limits {
        entries: MAX_ENTRIES,
        bytes: MAX_UNPACKED,
        check_every: SPACE_CHECK_EVERY,
    };
}
const MAX_VDF: u64 = 64 << 10;
pub const VDF: &str = "compatibilitytool.vdf";
/// How often (in bytes written) extraction re-checks free space against the reserve (T7).
const SPACE_CHECK_EVERY: u64 = 64 << 20;

pub struct Extracted {
    /// The top-level directory, created inside the staging directory passed in.
    pub top: Vec<u8>,
    /// Unpacked size: the sum of the regular files' sizes.
    pub bytes: u64,
}

fn unsafe_archive(detail: impl Into<String>) -> Fail {
    Fail::new("unsafe_archive", detail)
}

fn too_large(detail: impl Into<String>) -> Fail {
    Fail::new("archive_too_large", detail)
}

fn io_fail(what: &str, e: io::Error) -> Fail {
    Fail::new("io_error", format!("{what}: {e}"))
}

/// The components of an archive path: relative, no NUL, no empty, `.` or `..` component, at most
/// [`MAX_COMPONENTS`] of them and [`MAX_PATH`] bytes. One trailing `/` (a directory) is allowed.
fn components(path: &[u8]) -> Result<Vec<&[u8]>, Fail> {
    if path.len() > MAX_PATH {
        return Err(unsafe_archive(format!(
            "an entry path of {} bytes",
            path.len()
        )));
    }
    let shown = String::from_utf8_lossy(path);
    if path.is_empty() || path[0] == b'/' || path.contains(&0) {
        return Err(unsafe_archive(format!(
            "entry path {shown:?} is absolute or empty"
        )));
    }
    let trimmed = path.strip_suffix(b"/").unwrap_or(path);
    let comps: Vec<&[u8]> = trimmed.split(|&c| c == b'/').collect();
    if comps.len() > MAX_COMPONENTS {
        return Err(unsafe_archive(format!(
            "an entry path of {} components",
            comps.len()
        )));
    }
    if comps
        .iter()
        .any(|c| c.is_empty() || *c == b"." || *c == b"..")
    {
        return Err(unsafe_archive(format!(
            "entry path {shown:?} has an empty, `.` or `..` component"
        )));
    }
    Ok(comps)
}

fn os(b: &[u8]) -> &OsStr {
    OsStr::from_bytes(b)
}

/// Opens the directory `comps` under `root`, creating missing components. A component that is a
/// symlink or a file is refused: nothing is ever created through a link.
fn open_parent(root: &File, comps: &[&[u8]]) -> Result<File, Fail> {
    let mut cur = root.try_clone().map_err(|e| io_fail("dup staging", e))?;
    for c in comps {
        let next = match open_dir_at(&cur, os(c)) {
            Ok(d) => d,
            Err(e) if e.kind() == io::ErrorKind::NotFound => {
                mkdir_at(&cur, os(c), 0o755).map_err(|e| io_fail("mkdir", e))?;
                open_dir_at(&cur, os(c)).map_err(|e| io_fail("open dir", e))?
            }
            Err(e) if crate::nofollow_dir::is_symlink_or_not_dir(&e) => {
                return Err(unsafe_archive(format!(
                    "an entry's parent {:?} is a symlink or a file",
                    String::from_utf8_lossy(c)
                )))
            }
            Err(e) => return Err(io_fail("open dir", e)),
        };
        cur = next;
    }
    Ok(cur)
}

fn cstr(b: &[u8]) -> Result<CString, Fail> {
    CString::new(b).map_err(|_| unsafe_archive("a name holds a NUL"))
}

fn create_file(dir: &File, name: &[u8], exec: bool) -> Result<File, Fail> {
    let c = cstr(name)?;
    // SAFETY: `dir` is open and `c` NUL-terminated for the call; the new fd is owned below.
    let fd = unsafe {
        libc::openat(
            dir.as_raw_fd(),
            c.as_ptr(),
            libc::O_WRONLY | libc::O_CREAT | libc::O_EXCL | libc::O_NOFOLLOW | libc::O_CLOEXEC,
            0o600,
        )
    };
    if fd < 0 {
        let e = io::Error::last_os_error();
        return Err(if e.kind() == io::ErrorKind::AlreadyExists {
            unsafe_archive(format!(
                "entry {:?} already exists",
                String::from_utf8_lossy(name)
            ))
        } else {
            io_fail("create file", e)
        });
    }
    // SAFETY: `fd` was just returned by openat and nothing else owns it.
    let f = unsafe { File::from_raw_fd(fd) };
    // On the open descriptor, so never through a link; setuid, setgid and sticky never set.
    let mode = if exec { 0o755 } else { 0o644 };
    // SAFETY: `f` is an open descriptor.
    if unsafe { libc::fchmod(f.as_raw_fd(), mode) } != 0 {
        return Err(io_fail("fchmod", io::Error::last_os_error()));
    }
    Ok(f)
}

/// How often an `EAGAIN` from `openat2` is retried before it counts as a refusal.
const EAGAIN_RETRIES: usize = 64;

/// Runs `f` again while it fails `EAGAIN`, up to [`EAGAIN_RETRIES`] times.
fn retry_eagain(mut f: impl FnMut() -> io::Result<()>) -> io::Result<()> {
    let mut tries = 0;
    loop {
        match f() {
            Err(e) if e.raw_os_error() == Some(libc::EAGAIN) && tries < EAGAIN_RETRIES => {
                tries += 1
            }
            other => return other,
        }
    }
}

/// `openat2(dir, rel, O_PATH, RESOLVE_BENEATH)`: follows `rel`'s links without leaving `dir`. The
/// kernel answers `EAGAIN` when any rename on the system races a `..` step, so that is retried.
pub(crate) fn resolve_beneath(dir: &File, rel: &[u8]) -> io::Result<()> {
    retry_eagain(|| resolve_beneath_once(dir, rel))
}

fn resolve_beneath_once(dir: &File, rel: &[u8]) -> io::Result<()> {
    let c = CString::new(rel).map_err(io::Error::other)?;
    // SAFETY: zeroed is a valid open_how (no flags, no resolve bits).
    let mut how: libc::open_how = unsafe { std::mem::zeroed() };
    how.flags = (libc::O_PATH | libc::O_CLOEXEC) as u64;
    how.resolve = libc::RESOLVE_BENEATH;
    // SAFETY: every pointer is valid for the call and the size is the struct's own.
    let fd = unsafe {
        libc::syscall(
            libc::SYS_openat2,
            dir.as_raw_fd(),
            c.as_ptr(),
            &how as *const libc::open_how,
            std::mem::size_of::<libc::open_how>(),
        )
    };
    if fd < 0 {
        return Err(io::Error::last_os_error());
    }
    // SAFETY: openat2 returned an fd we own.
    drop(unsafe { File::from_raw_fd(fd as i32) });
    Ok(())
}

/// Extracts the gzip tar `archive` into `staging` under T5. `check` (free space against the
/// reserve, T7, and the run's abort) runs every `limits.check_every` bytes written; its error stops
/// the extraction.
pub fn extract(
    archive: impl Read,
    staging: &File,
    check: &mut dyn FnMut() -> Result<(), Fail>,
    limits: Limits,
) -> Result<Extracted, Fail> {
    let mut ar = tar::Archive::new(flate2::read::GzDecoder::new(archive));
    let mut seen: HashSet<Vec<u8>> = HashSet::new();
    let mut links: Vec<Vec<u8>> = Vec::new();
    let mut top: Option<Vec<u8>> = None;
    let (mut entries, mut bytes, mut since_check) = (0u64, 0u64, 0u64);
    let iter = ar
        .entries()
        .map_err(|e| unsafe_archive(format!("unreadable archive: {e}")))?;
    for entry in iter {
        let mut entry = entry.map_err(|e| unsafe_archive(format!("unreadable archive: {e}")))?;
        entries += 1;
        if entries > limits.entries {
            return Err(too_large(format!("more than {} entries", limits.entries)));
        }
        let kind = entry.header().entry_type();
        if kind.is_pax_global_extensions() {
            continue;
        }
        let path = entry.path_bytes().into_owned();
        let comps = components(&path)?;
        match &top {
            None => top = Some(comps[0].to_vec()),
            Some(t) if t.as_slice() != comps[0] => {
                return Err(unsafe_archive(
                    "entries under more than one top-level directory",
                ))
            }
            Some(_) => {}
        }
        if !seen.insert(comps.join(&b'/')) {
            return Err(unsafe_archive(format!(
                "duplicate entry {:?}",
                String::from_utf8_lossy(&path)
            )));
        }
        let (name, parents) = comps.split_last().expect("components is never empty");
        if comps.len() == 1 && !kind.is_dir() {
            return Err(unsafe_archive("an entry outside the top-level directory"));
        }
        let dir = open_parent(staging, parents)?;
        if kind.is_dir() {
            match mkdir_at(&dir, os(name), 0o755) {
                Ok(()) => {}
                // Made earlier as a missing parent; refused if it is not a directory.
                Err(e) if e.kind() == io::ErrorKind::AlreadyExists => {
                    open_dir_at(&dir, os(name))
                        .map_err(|_| unsafe_archive("a directory entry over a file or link"))?;
                }
                Err(e) => return Err(io_fail("mkdir", e)),
            }
        } else if kind.is_file() || kind == tar::EntryType::Continuous {
            let size = entry
                .header()
                .size()
                .map_err(|e| unsafe_archive(e.to_string()))?;
            if bytes.saturating_add(size) > limits.bytes {
                return Err(too_large(format!(
                    "more than {} bytes unpacked",
                    limits.bytes
                )));
            }
            let exec = entry.header().mode().unwrap_or(0) & 0o111 != 0;
            let mut out = create_file(&dir, name, exec)?;
            let mut buf = vec![0u8; 1 << 16];
            loop {
                let n = entry
                    .read(&mut buf)
                    .map_err(|e| unsafe_archive(format!("unreadable archive: {e}")))?;
                if n == 0 {
                    break;
                }
                bytes += n as u64;
                since_check += n as u64;
                if bytes > limits.bytes {
                    return Err(too_large(format!(
                        "more than {} bytes unpacked",
                        limits.bytes
                    )));
                }
                out.write_all(&buf[..n]).map_err(|e| io_fail("write", e))?;
                if since_check >= limits.check_every {
                    since_check = 0;
                    check()?;
                }
            }
        } else if kind.is_symlink() {
            let target = entry
                .link_name_bytes()
                .map(|t| t.into_owned())
                .unwrap_or_default();
            if target.is_empty()
                || target.len() > MAX_PATH
                || target[0] == b'/'
                || target.contains(&0)
            {
                return Err(unsafe_archive(format!(
                    "symlink {:?} has an absolute or empty target",
                    String::from_utf8_lossy(&path)
                )));
            }
            let (t, n) = (cstr(&target)?, cstr(name)?);
            // SAFETY: both strings are NUL-terminated and `dir` is open for the call.
            if unsafe { libc::symlinkat(t.as_ptr(), dir.as_raw_fd(), n.as_ptr()) } != 0 {
                let e = io::Error::last_os_error();
                return Err(if e.kind() == io::ErrorKind::AlreadyExists {
                    unsafe_archive("a symlink entry over an existing entry")
                } else {
                    io_fail("symlinkat", e)
                });
            }
            links.push(comps[1..].join(&b'/'));
        } else {
            return Err(unsafe_archive(format!(
                "entry {:?} has refused type {kind:?}",
                String::from_utf8_lossy(&path)
            )));
        }
    }
    let top = top.ok_or_else(|| unsafe_archive("the archive is empty"))?;
    let topfd = open_dir_at(staging, os(&top)).map_err(|e| io_fail("open top", e))?;
    for link in &links {
        if let Err(e) = resolve_beneath(&topfd, link) {
            return Err(link_verdict(link, e));
        }
    }
    Ok(Extracted { top, bytes })
}

/// A failed link resolution: an escape, a loop or a missing target is the archive's fault
/// (`unsafe_archive`); anything else (openat2 missing or filtered, EAGAIN past its retries, out
/// of memory) is this host's, and must not hold the release back as if its bytes were bad.
fn link_verdict(link: &[u8], e: io::Error) -> Fail {
    let shown = String::from_utf8_lossy(link);
    match e.raw_os_error() {
        Some(libc::EXDEV | libc::ELOOP | libc::ENOENT | libc::ENOTDIR) => {
            unsafe_archive(format!("symlink {shown:?} escapes, loops or dangles: {e}"))
        }
        _ => Fail::new("io_error", format!("resolving symlink {shown:?}: {e}")),
    }
}

/// Reads `compatibilitytool.vdf` from the open top-level directory and returns the one tool name
/// it registers (T6). Not a symlink, at most 64 KiB, strict grammar.
pub fn read_vdf_name(top: &File) -> Result<String, Fail> {
    let c = CString::new(VDF).expect("constant");
    // SAFETY: `top` is open and `c` NUL-terminated; the new fd is owned below.
    let fd = unsafe {
        libc::openat(
            top.as_raw_fd(),
            c.as_ptr(),
            libc::O_RDONLY | libc::O_NOFOLLOW | libc::O_CLOEXEC | libc::O_NONBLOCK,
        )
    };
    if fd < 0 {
        return Err(Fail::new(
            "vdf_invalid",
            format!("{VDF}: {}", io::Error::last_os_error()),
        ));
    }
    // SAFETY: `fd` was just returned by openat.
    let f = unsafe { File::from_raw_fd(fd) };
    let meta = f.metadata().map_err(|e| io_fail("stat vdf", e))?;
    if !meta.is_file() || meta.len() > MAX_VDF {
        return Err(Fail::new(
            "vdf_invalid",
            format!("{VDF} is not a file of at most 64 KiB"),
        ));
    }
    let mut body = Vec::new();
    f.take(MAX_VDF + 1)
        .read_to_end(&mut body)
        .map_err(|e| io_fail("read vdf", e))?;
    parse_vdf_name(&body).map_err(|e| Fail::new("vdf_invalid", format!("{VDF}: {e}")))
}

#[derive(Debug, PartialEq)]
enum Tok {
    Str(String),
    Open,
    Close,
}

fn lex(s: &str) -> Result<Vec<Tok>, String> {
    let b = s.as_bytes();
    let (mut i, mut out) = (0, Vec::new());
    while i < b.len() {
        match b[i] {
            b' ' | b'\t' | b'\r' | b'\n' => i += 1,
            b'{' => {
                out.push(Tok::Open);
                i += 1;
            }
            b'}' => {
                out.push(Tok::Close);
                i += 1;
            }
            b'/' if b.get(i + 1) == Some(&b'/') => {
                while i < b.len() && b[i] != b'\n' {
                    i += 1;
                }
            }
            b'"' => {
                let start = i + 1;
                let end = b[start..]
                    .iter()
                    .position(|&c| c == b'"')
                    .map(|p| start + p)
                    .ok_or("unterminated string")?;
                let tok = &s[start..end];
                if tok.contains(['\\', '\n', '\r']) {
                    return Err("a string holds an escape or a line break".into());
                }
                out.push(Tok::Str(tok.to_owned()));
                i = end + 1;
            }
            _ => return Err(format!("unexpected character at byte {i}")),
        }
    }
    Ok(out)
}

#[derive(Debug)]
enum Node {
    Str(String),
    Block(Vec<(String, Node)>),
}

fn block(toks: &[Tok], i: &mut usize, nested: bool) -> Result<Vec<(String, Node)>, String> {
    let mut pairs: Vec<(String, Node)> = Vec::new();
    loop {
        match toks.get(*i) {
            None if !nested => return Ok(pairs),
            None => return Err("unclosed block".into()),
            Some(Tok::Close) if nested => {
                *i += 1;
                return Ok(pairs);
            }
            Some(Tok::Str(k)) => {
                *i += 1;
                let value = match toks.get(*i) {
                    Some(Tok::Str(v)) => {
                        *i += 1;
                        Node::Str(v.clone())
                    }
                    Some(Tok::Open) => {
                        *i += 1;
                        Node::Block(block(toks, i, true)?)
                    }
                    _ => return Err(format!("key {k:?} has no value")),
                };
                if pairs.iter().any(|(p, _)| p.eq_ignore_ascii_case(k)) {
                    return Err(format!("key {k:?} appears twice in one block"));
                }
                pairs.push((k.clone(), value));
            }
            Some(t) => return Err(format!("unexpected {t:?}")),
        }
    }
}

type Pairs = [(String, Node)];

fn only<'a>(pairs: &'a Pairs, key: Option<&str>) -> Result<(&'a str, &'a Pairs), String> {
    match pairs {
        [(k, Node::Block(inner))] if key.is_none_or(|want| k == want) => Ok((k, inner)),
        _ => Err(format!(
            "expected exactly one block{}",
            key.map(|k| format!(" {k:?}")).unwrap_or_default()
        )),
    }
}

/// The strict shape: `"compatibilitytools" { "compat_tools" { "<name>" { install_path ".",
/// from_oslist "windows", to_oslist "linux", optional display_name } } }`, nothing else.
pub fn parse_vdf_name(body: &[u8]) -> Result<String, String> {
    let s = std::str::from_utf8(body).map_err(|_| "not UTF-8".to_string())?;
    let toks = lex(s)?;
    let mut i = 0;
    let root = block(&toks, &mut i, false)?;
    let (_, tools) = only(&root, Some("compatibilitytools"))?;
    let (_, entries) = only(tools, Some("compat_tools"))?;
    let (name, keys) = only(entries, None)?;
    let mut need = [
        ("install_path", "."),
        ("from_oslist", "windows"),
        ("to_oslist", "linux"),
    ]
    .map(|(k, v)| (k, v, false));
    for (k, v) in keys {
        let Node::Str(v) = v else {
            return Err(format!("{k:?} is a block"));
        };
        if k == "display_name" {
            continue;
        }
        match need.iter_mut().find(|(want, _, _)| want == k) {
            Some((_, want, seen)) if v == want => *seen = true,
            Some(_) => return Err(format!("{k:?} is {v:?}")),
            None => return Err(format!("unknown key {k:?}")),
        }
    }
    if let Some((k, _, _)) = need.iter().find(|(_, _, seen)| !seen) {
        return Err(format!("{k:?} is missing"));
    }
    Ok(name.to_owned())
}

#[cfg(test)]
mod tests {
    use super::*;

    pub(crate) const GOOD_VDF: &str = r#""compatibilitytools"
{
  "compat_tools"
  {
    "GE-Proton11-12-x86_64" // Internal name of this tool
    {
      // Can register this tool with Steam in two ways:
      "install_path" "."
      "display_name" "GE-Proton11-12-x86_64"
      "from_oslist"  "windows"
      "to_oslist"    "linux"
    }
  }
}
"#;

    #[test]
    fn the_real_vdf_shape_gives_its_name() {
        assert_eq!(
            parse_vdf_name(GOOD_VDF.as_bytes()).unwrap(),
            "GE-Proton11-12-x86_64"
        );
    }

    #[test]
    fn a_vdf_outside_the_grammar_is_refused() {
        let bad = [
            GOOD_VDF.replace("\"install_path\" \".\"", "\"install_path\" \"..\""),
            GOOD_VDF.replace(
                "\"to_oslist\"    \"linux\"",
                "\"to_oslist\" \"linux\" \"extra\" \"x\"",
            ),
            GOOD_VDF.replace(
                "\"display_name\"",
                "\"display_name\" \"a\" \"Display_Name\"",
            ),
            GOOD_VDF.replace("\"from_oslist\"  \"windows\"", ""),
            GOOD_VDF.replace("\"install_path\" \".\"", "install_path \".\""),
            GOOD_VDF.replace("\"install_path\" \".\"", "#base \"x\""),
            GOOD_VDF.replace(
                "\"display_name\" \"GE-Proton11-12-x86_64\"",
                "\"display_name\" \"a\\b\"",
            ),
            format!("{GOOD_VDF}\"second\" {{ }}"),
            GOOD_VDF.replace(
                "\"compat_tools\"",
                "\"compat_tools\" { \"a\" { } }\n\"other\"",
            ),
            "\u{feff}x".into(),
        ];
        for b in bad {
            assert!(parse_vdf_name(b.as_bytes()).is_err(), "{b}");
        }
        assert!(parse_vdf_name(&[0xff, 0xfe]).is_err());
    }

    #[test]
    fn only_a_resolution_verdict_is_the_archives_fault() {
        for errno in [libc::EXDEV, libc::ELOOP, libc::ENOENT, libc::ENOTDIR] {
            let f = link_verdict(b"l", io::Error::from_raw_os_error(errno));
            assert_eq!(f.reason, "unsafe_archive", "errno {errno}");
        }
        for errno in [libc::ENOSYS, libc::EPERM, libc::EAGAIN, libc::ENOMEM] {
            let f = link_verdict(b"l", io::Error::from_raw_os_error(errno));
            assert_eq!(
                f.reason, "io_error",
                "errno {errno}: an environment fault, not the bytes"
            );
        }
    }

    #[test]
    fn eagain_is_retried_and_then_given_up() {
        let mut n = 0;
        let flaky = retry_eagain(|| {
            n += 1;
            if n < 3 {
                Err(io::Error::from_raw_os_error(libc::EAGAIN))
            } else {
                Ok(())
            }
        });
        assert!(flaky.is_ok() && n == 3);
        let always = retry_eagain(|| Err(io::Error::from_raw_os_error(libc::EAGAIN)));
        assert_eq!(always.unwrap_err().raw_os_error(), Some(libc::EAGAIN));
        let mut calls = 0;
        let other = retry_eagain(|| {
            calls += 1;
            Err(io::Error::from_raw_os_error(libc::EXDEV))
        });
        assert!(other.is_err() && calls == 1, "an escape is never retried");
    }

    #[test]
    fn components_refuse_escapes() {
        for p in [
            &b"/abs"[..],
            b"a/../b",
            b"a//b",
            b"./a",
            b"a/./b",
            b"",
            b"a\0b",
        ] {
            assert!(components(p).is_err(), "{:?}", String::from_utf8_lossy(p));
        }
        assert_eq!(components(b"top/dir/").unwrap(), vec![&b"top"[..], b"dir"]);
        let deep = vec!["d"; MAX_COMPONENTS + 1].join("/");
        assert!(components(deep.as_bytes()).is_err(), "too many components");
        assert!(components(vec!["d"; MAX_COMPONENTS].join("/").as_bytes()).is_ok());
        let long = vec!["x".repeat(250); 17].join("/");
        assert!(
            long.len() > MAX_PATH && components(long.as_bytes()).is_err(),
            "too long"
        );
    }
}
