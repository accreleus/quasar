//! Steam library discovery: the agent-side scanner and ACF manifest parser
//! (Phase 4 of `docs/design/plans/2026-07-29-steam-library-discovery-spec.md`).
//!
//! Walks a control-plane-supplied home directory for Steam `appmanifest_*.acf`
//! files and reports what it finds. Follows #175's `session::gc` shape: an HTTP
//! pull/report pair reusing the agent's node-secret auth, a bounded blocking
//! pass that never fails the agent, and a scan that swallows its own errors.
//!
//! **The agent never learns a user.** The wire payload (`GET
//! /v1/agent/library/scan-pending`, `POST /v1/agent/library/scan-report`) has no
//! user id, username, or user-derived field — the control plane holds the
//! `scan_id -> (user_id, app_id, host_id)` mapping and resolves it on receipt
//! (spec §7.3).
//!
//! ## The PII rule (spec §9)
//!
//! `appmanifest_*.acf` carries `LastOwner`, a SteamID64 — a persistent,
//! globally unique, externally resolvable identifier for a real Steam account.
//! **`LastOwner` is never read, logged, transmitted or persisted.** The
//! mechanism is a parser **key allow-list, not a denylist**, applied at parse
//! time before any value crosses a function boundary: `appid | name |
//! installdir | SizeOnDisk | StateFlags`. Everything else — including
//! `LastOwner` and any future Valve key — is discarded while tokenizing, inside
//! [`parse_acf`], and never reaches [`ManifestEntry`]. That struct has exactly
//! five fields, so there is nowhere for a sixth value to travel even if the
//! parser were wrong.
//!
//! ## Security of the appid (spec §10)
//!
//! `appid` is validated as a bare positive integer (`^[1-9][0-9]{0,9}$`,
//! [`is_valid_appid`]) before it may leave [`parse_acf`]. A manifest whose
//! `appid` fails that check is dropped, with a debug log that does not echo the
//! value — validation point 1 of 4 (the other three: control-plane ingest, a
//! database CHECK, and the launch-time render).

use crate::cp_http::CpClient;
use std::collections::HashSet;
use std::path::{Path, PathBuf};
use std::time::{Duration, Instant};

use serde::{Deserialize, Serialize};
use tracing::debug;

use crate::session::home;

/// Wall-clock budget for one scan's filesystem walk. Smaller than
/// `session::home::DU_TIMEOUT` (10s) because this glob is shallow — a handful
/// of `steamapps` directories and a direct listing of `appmanifest_*.acf`
/// inside each, never a recursive size summation.
const SCAN_WALK_TIMEOUT: Duration = Duration::from_secs(5);

/// Max depth under `root_path` to search for a directory literally named
/// `steamapps` that isn't already named in `relative_roots` (spec §7.4 item
/// 1). Mirrors `session::home::DU_DEPTH_CAP`'s discipline of a fixed cap, just
/// a much shallower one — this walk is looking for one directory name, not
/// summing an entire tree.
const SCAN_STEAMAPPS_MAX_DEPTH: u32 = 4;

// ── Wire shapes (protocol/control-api.md, additive HTTP surface) ───────────

#[derive(Debug, Deserialize)]
struct ScanPendingResp {
    scans: Vec<ScanTask>,
}

/// One scan the control plane wants this host to run. `root_path` is
/// control-plane-supplied, so it is validated for containment before anything
/// under it is touched (spec §7.3) — the agent trusts it no more than a
/// client's path.
#[derive(Debug, Deserialize, Clone)]
pub struct ScanTask {
    pub scan_id: String,
    pub root_path: String,
    pub relative_roots: Vec<String>,
    pub max_entries: usize,
    pub max_manifest_bytes: u64,
}

/// The exactly-five fields extractable from an ACF manifest under the §9
/// allow-list. **No field here can hold `LastOwner`** — a parser bug cannot
/// make a SteamID64 cross this boundary because there is nowhere for it to live.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize)]
pub struct ManifestEntry {
    pub external_id: String,
    pub name: String,
    pub install_dir: String,
    pub size_on_disk: u64,
    pub state_flags: i64,
}

/// The report POSTed for one scan. A failure is reported with `ok: false` and a
/// non-empty `error`; never fatal to the agent (spec §7.4 step 4).
#[derive(Debug, Serialize)]
struct ScanReport {
    scan_id: String,
    ok: bool,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    entries: Vec<ManifestEntry>,
    #[serde(skip_serializing_if = "String::is_empty")]
    error: String,
}

impl ScanReport {
    fn ok(scan_id: String, entries: Vec<ManifestEntry>) -> Self {
        ScanReport {
            scan_id,
            ok: true,
            entries,
            error: String::new(),
        }
    }

    fn failed(scan_id: String, error: String) -> Self {
        ScanReport {
            scan_id,
            ok: false,
            entries: Vec::new(),
            error,
        }
    }
}

// ── HTTP client (mirrors session::gc::GcClient) ─────────────────────────────

/// Configuration the scanner needs, captured once per pass. Unlike `GcClient`
/// it needs no `ContainerRuntime` or `LiveRefs` — a filesystem walk has no
/// container or live-session interaction.
pub struct LibraryScanClient {
    cp: CpClient,
}

const SCAN_PENDING_PATH: &str = "/v1/agent/library/scan-pending";
const SCAN_REPORT_PATH: &str = "/v1/agent/library/scan-report";

impl LibraryScanClient {
    pub fn new(cp: CpClient) -> Self {
        LibraryScanClient { cp }
    }

    /// Run one full pass: pull pending scans, run each, report the result.
    /// Blocking; call from a spawned thread. Logs and swallows all errors —
    /// never panics, never returns Err (a scan failure must never be fatal to
    /// the agent), matching the GC reaper's posture.
    pub fn run_pass(&self) {
        let scans = match self.fetch_pending() {
            Ok(s) => s,
            Err(e) => {
                tracing::warn!(
                    token = "library-scan-fetch-failed",
                    "library-scan: fetch pending failed: {e}"
                );
                return;
            }
        };
        if scans.is_empty() {
            debug!("library-scan: no pending scans");
            return;
        }
        tracing::info!("library-scan: {} pending scan(s)", scans.len());
        for scan in &scans {
            let report = run_scan(scan);
            if let Err(e) = self.post_report(&report) {
                tracing::warn!(
                    token = "library-scan-report-failed",
                    "library-scan: scan {} report post failed (will be re-pulled next pass): {e}",
                    scan.scan_id
                );
            }
        }
    }

    fn fetch_pending(&self) -> Result<Vec<ScanTask>, String> {
        let parsed: ScanPendingResp = self.cp.get_json(SCAN_PENDING_PATH)?;
        Ok(parsed.scans)
    }

    fn post_report(&self, report: &ScanReport) -> Result<(), String> {
        self.cp.post_json_no_body(SCAN_REPORT_PATH, report)
    }
}

// ── The walk (spec §7.4) ────────────────────────────────────────────────────

/// Run one scan end to end: validate containment, walk, parse, build the
/// report. Never panics — every filesystem error becomes a skipped entry or a
/// `{ok: false, error}` report.
fn run_scan(scan: &ScanTask) -> ScanReport {
    match scan_root_path(scan, home::configured_home_root()) {
        Ok(entries) => ScanReport::ok(scan.scan_id.clone(), entries),
        Err(e) => ScanReport::failed(scan.scan_id.clone(), e),
    }
}

/// `configured_root` is a parameter, not an env read, purely for test
/// determinism — `run_scan` is the only real caller and always passes
/// `home::configured_home_root()`. Mirrors `home::measure_home_dirs`'s split
/// between pure logic and the env-reading wrapper.
fn scan_root_path(
    scan: &ScanTask,
    configured_root: Option<PathBuf>,
) -> Result<Vec<ManifestEntry>, String> {
    let Some(configured_root) = configured_root else {
        return Err("agent has no configured home root".to_string());
    };
    let root_path = PathBuf::from(&scan.root_path);

    // Containment BEFORE anything under root_path is touched (spec §7.3).
    // `is_under_root` is component-aware but does not resolve `..`, so a
    // literal `..` must be rejected first or `<root>/../../etc` would
    // lexically "start with" root while escaping it once resolved.
    if has_traversal(&root_path) {
        tracing::warn!(
            token = "library-scan-path-traversal",
            "library-scan: scan {} root_path contains a '..' component — refusing",
            scan.scan_id
        );
        return Err("root_path outside configured home root".to_string());
    }
    if !home::is_under_root(&configured_root, &root_path) {
        tracing::warn!(
            token = "library-scan-path-outside-root",
            "library-scan: scan {} root_path is outside the configured home root — refusing",
            scan.scan_id
        );
        return Err("root_path outside configured home root".to_string());
    }

    match probe_dir(&root_path) {
        DirProbe::Missing => return Ok(Vec::new()), // home not provisioned yet — nothing to scan, not an error
        DirProbe::Real => {}
        DirProbe::SymlinkOrNotDir => {
            tracing::warn!(
                token = "library-scan-path-not-a-dir",
                "library-scan: scan {} root_path is not a real directory — refusing",
                scan.scan_id
            );
            return Err("root_path is not a real directory".to_string());
        }
    }

    Ok(walk_and_parse(
        &root_path,
        &scan.relative_roots,
        scan.max_entries,
        scan.max_manifest_bytes,
    ))
}

/// Reject any path with a literal `..` component (mirrors
/// `home::host_path_of`'s traversal rejection).
fn has_traversal(p: &Path) -> bool {
    p.components()
        .any(|c| matches!(c, std::path::Component::ParentDir))
}

enum DirProbe {
    Missing,
    Real,
    SymlinkOrNotDir,
}

/// Probe a path without following a final symlink. Distinguishes "doesn't
/// exist yet" (nothing to scan) from "exists but is a symlink or not a
/// directory" (refused — no more trusted than the control plane's path itself).
fn probe_dir(path: &Path) -> DirProbe {
    match std::fs::symlink_metadata(path) {
        Err(_) => DirProbe::Missing,
        Ok(m) if m.file_type().is_symlink() => DirProbe::SymlinkOrNotDir,
        Ok(m) if m.is_dir() => DirProbe::Real,
        Ok(_) => DirProbe::SymlinkOrNotDir,
    }
}

/// Resolve `root.join(rel)`, checking **every** path component for a symlink —
/// not just the final one.
///
/// `symlink_metadata`/`read_dir` on a multi-component path only inspects the
/// *final* component; the kernel transparently resolves every earlier one, so
/// checking the fully-joined path says nothing about e.g. `.local` (mid-path in
/// `.local/share/Steam/steamapps`) being a symlink out of `root`. A user's home
/// is bind-mounted read-write into their own container so Steam can write
/// there, so they control every path component under it — swapping `.local`
/// for a symlink before the next scan is something any user can do. Unchecked,
/// this walks into another user's home and reports its manifests as this
/// user's — a cross-user privacy leak, and (once the `granted_by='provider'`
/// entitlement grant lands) an entitlement-forgery vector.
///
/// Fix: walk one component at a time with a per-child `is_symlink()` check
/// before recursing, same discipline as `find_steamapps_dirs`. Also refuses an
/// absolute `rel` (which `PathBuf::join` would let replace `root` wholesale)
/// and any `..` component.
fn resolve_relative_root(root: &Path, rel: &str) -> Option<PathBuf> {
    let relp = Path::new(rel);
    if relp.is_absolute() || has_traversal(relp) {
        return None;
    }

    let mut current = root.to_path_buf();
    for component in relp.components() {
        let std::path::Component::Normal(name) = component else {
            // `has_traversal` already rejected `..`; refuse any other
            // component defensively rather than guess what it means.
            return None;
        };
        // Refuse if `current` (about to be descended into) is itself a
        // symlink, missing, or not a directory — the check a single
        // `symlink_metadata` on the fully-joined path can never give.
        match std::fs::symlink_metadata(&current) {
            Ok(m) if m.file_type().is_symlink() || !m.is_dir() => return None,
            Ok(_) => {}
            Err(_) => return None,
        }
        current = current.join(name);
    }

    // The loop validated every directory BEFORE descending; the fully-joined
    // `current` (the target directory itself) still needs the same check.
    match std::fs::symlink_metadata(&current) {
        Ok(m) if m.file_type().is_symlink() || !m.is_dir() => None,
        Ok(_) => Some(current),
        Err(_) => None,
    }
}

/// The walk described in spec §7.4: `relative_roots` under `root_path`, plus
/// any directory named `steamapps` at depth `<= SCAN_STEAMAPPS_MAX_DEPTH`, each
/// globbed for `appmanifest_*.acf`, capped at `max_entries` total /
/// `max_manifest_bytes` per file.
fn walk_and_parse(
    root_path: &Path,
    relative_roots: &[String],
    max_entries: usize,
    max_manifest_bytes: u64,
) -> Vec<ManifestEntry> {
    let deadline = Instant::now() + SCAN_WALK_TIMEOUT;
    let mut dirs: Vec<PathBuf> = Vec::new();
    let mut seen_dirs: HashSet<PathBuf> = HashSet::new();

    for rel in relative_roots {
        // See resolve_relative_root's doc for why a naive root.join(rel) +
        // one symlink_metadata call would not be sufficient.
        if let Some(joined) = resolve_relative_root(root_path, rel) {
            if seen_dirs.insert(joined.clone()) {
                dirs.push(joined);
            }
        }
    }

    let mut discovered = Vec::new();
    find_steamapps_dirs(
        root_path,
        1,
        SCAN_STEAMAPPS_MAX_DEPTH,
        deadline,
        &mut discovered,
    );
    for d in discovered {
        if seen_dirs.insert(d.clone()) {
            dirs.push(d);
        }
    }

    let mut out = Vec::new();
    let mut seen_appids: HashSet<String> = HashSet::new();
    for dir in &dirs {
        if out.len() >= max_entries || Instant::now() >= deadline {
            break;
        }
        collect_manifests(
            dir,
            &mut out,
            &mut seen_appids,
            max_entries,
            max_manifest_bytes,
        );
    }
    out
}

/// Bounded, depth-capped, containment-checked search for `steamapps`
/// directories, modelled on `home::du`'s wall-clock + depth discipline.
/// `depth` is the depth of `dir`'s immediate children. Checks every directory
/// it descends into for a symlink before recursing — same discipline as
/// `resolve_relative_root` and for the same reason: a symlinked `steamapps` (or
/// an ancestor) could otherwise point outside `root_path`.
fn find_steamapps_dirs(
    dir: &Path,
    depth: u32,
    max_depth: u32,
    deadline: Instant,
    found: &mut Vec<PathBuf>,
) {
    if depth > max_depth || Instant::now() >= deadline {
        return;
    }
    let rd = match std::fs::read_dir(dir) {
        Ok(r) => r,
        Err(_) => return,
    };
    for entry in rd {
        if Instant::now() >= deadline {
            return;
        }
        let entry = match entry {
            Ok(e) => e,
            Err(_) => continue,
        };
        let path = entry.path();
        let meta = match std::fs::symlink_metadata(&path) {
            Ok(m) => m,
            Err(_) => continue,
        };
        if meta.file_type().is_symlink() || !meta.is_dir() {
            continue;
        }
        if path.file_name().and_then(|n| n.to_str()) == Some("steamapps") {
            found.push(path);
            continue; // steamapps is never nested inside steamapps
        }
        find_steamapps_dirs(&path, depth + 1, max_depth, deadline, found);
    }
}

/// Glob `appmanifest_*.acf` directly inside `dir` (Steam never nests them
/// further), enforcing the symlink refusal and the two caps from spec §7.4
/// step 2, and deduplicating by appid — a directory reachable via both
/// `relative_roots` and the depth walk must not double an entry.
fn collect_manifests(
    dir: &Path,
    out: &mut Vec<ManifestEntry>,
    seen_appids: &mut HashSet<String>,
    max_entries: usize,
    max_manifest_bytes: u64,
) {
    let rd = match std::fs::read_dir(dir) {
        Ok(r) => r,
        Err(_) => return,
    };
    for entry in rd {
        if out.len() >= max_entries {
            return;
        }
        let entry = match entry {
            Ok(e) => e,
            Err(_) => continue,
        };
        let path = entry.path();
        let Some(fname) = path.file_name().and_then(|n| n.to_str()) else {
            continue;
        };
        if !(fname.starts_with("appmanifest_") && fname.ends_with(".acf")) {
            continue;
        }
        let meta = match std::fs::symlink_metadata(&path) {
            Ok(m) => m,
            Err(_) => continue,
        };
        if meta.file_type().is_symlink() {
            debug!(
                "library-scan: refusing to read symlinked manifest {}",
                path.display()
            );
            continue;
        }
        if !meta.is_file() {
            continue;
        }
        if meta.len() > max_manifest_bytes {
            debug!(
                "library-scan: manifest {} exceeds max_manifest_bytes ({} > {}) — skipping",
                path.display(),
                meta.len(),
                max_manifest_bytes
            );
            continue;
        }
        let bytes = match std::fs::read(&path) {
            Ok(b) => b,
            Err(e) => {
                debug!(
                    "library-scan: reading manifest {} failed: {e}",
                    path.display()
                );
                continue;
            }
        };
        let text = match std::str::from_utf8(&bytes) {
            Ok(t) => t,
            Err(_) => {
                debug!(
                    "library-scan: manifest {} is not valid UTF-8 — skipping",
                    path.display()
                );
                continue;
            }
        };
        match parse_acf(text) {
            Ok(parsed) => {
                if seen_appids.insert(parsed.external_id.clone()) {
                    out.push(parsed);
                }
            }
            Err(AcfParseError::InvalidAppid) => {
                // Validation point 1 of 4 (spec §10): drop the entry, and do
                // NOT echo the value — only the (trusted) file path is logged.
                debug!(
                    "library-scan: manifest {} has an appid that fails validation — dropped",
                    path.display()
                );
            }
            Err(AcfParseError::Malformed) => {
                // A torn read (Steam writing the file mid-scan) or any other
                // structurally invalid VDF. Skipped, not fatal (spec §7.4).
                debug!(
                    "library-scan: manifest {} failed to parse (malformed or torn) — skipped",
                    path.display()
                );
            }
        }
    }
}

// ── The parser (spec §9) ────────────────────────────────────────────────────

#[derive(Debug, PartialEq, Eq)]
enum AcfParseError {
    /// Structurally invalid VDF (unbalanced braces, missing `AppState`
    /// wrapper, a truncated/torn read, or no `appid` key at all).
    Malformed,
    /// `appid` was present but failed `^[1-9][0-9]{0,9}$`.
    InvalidAppid,
}

#[derive(Debug, PartialEq, Eq)]
enum Tok {
    Str(String),
    Open,
    Close,
}

/// Tokenize Valve KeyValues (VDF) text: quoted strings (with `\"`/`\\`
/// unescaping) and bare `{`/`}` as structural tokens. `//` starts a line
/// comment (not exercised by real manifests but cheap to honour). Any other
/// bare character is skipped — well-formed VDF never has one outside a string.
fn tokenize(input: &str) -> Vec<Tok> {
    let mut out = Vec::new();
    let mut chars = input.chars().peekable();
    while let Some(&c) = chars.peek() {
        if c.is_whitespace() {
            chars.next();
            continue;
        }
        if c == '/' {
            chars.next();
            if chars.peek() == Some(&'/') {
                for c2 in chars.by_ref() {
                    if c2 == '\n' {
                        break;
                    }
                }
            }
            continue;
        }
        if c == '{' {
            out.push(Tok::Open);
            chars.next();
            continue;
        }
        if c == '}' {
            out.push(Tok::Close);
            chars.next();
            continue;
        }
        if c == '"' {
            chars.next(); // consume opening quote
            let mut val = String::new();
            // `while let` (not `for chars.by_ref()`): each iteration's borrow
            // ends before the loop body runs, so `continue_escape` can take
            // its own `&mut chars` to consume the char after a backslash.
            while let Some(c2) = chars.next() {
                if c2 == '\\' {
                    continue_escape(&mut val, &mut chars);
                } else if c2 == '"' {
                    break;
                } else {
                    val.push(c2);
                }
            }
            // A torn read with no closing quote: emit the partial token so the
            // structural walk terminates by running out of tokens (Malformed).
            out.push(Tok::Str(val));
            continue;
        }
        // Unknown bare character outside a quoted string/brace — skip one
        // char at a time so a stray byte can never spin the loop forever.
        chars.next();
    }
    out
}

/// Consume the character after a `\` and push its unescaped form onto `val`.
/// `\"` -> `"`, `\\` -> `\`, anything else is pushed back as a literal
/// backslash-plus-char (defensive; the real manifests only use the first two).
fn continue_escape(val: &mut String, chars: &mut std::iter::Peekable<std::str::Chars<'_>>) {
    match chars.next() {
        Some('"') => val.push('"'),
        Some('\\') => val.push('\\'),
        Some('n') => val.push('\n'),
        Some('t') => val.push('\t'),
        Some(other) => {
            val.push('\\');
            val.push(other);
        }
        None => val.push('\\'),
    }
}

/// `^[1-9][0-9]{0,9}$` — a bare positive integer, 1 to 10 digits, no leading
/// zero. Matches spec §10 exactly (also enforced independently by the
/// control-plane ingest, a database CHECK, and the launch-time render).
fn is_valid_appid(s: &str) -> bool {
    let bytes = s.as_bytes();
    if bytes.is_empty() || bytes.len() > 10 {
        return false;
    }
    if !(b'1'..=b'9').contains(&bytes[0]) {
        return false;
    }
    bytes[1..].iter().all(|b| b.is_ascii_digit())
}

/// Parse one ACF manifest, extracting only the five allow-listed keys:
/// `appid | name | installdir | SizeOnDisk | StateFlags`. Every other key —
/// including `LastOwner` — is discarded while walking the token stream and
/// never touches [`ManifestEntry`].
///
/// The manifest is a flat `"AppState" { "key" "value" ... }` object with
/// occasional nested objects (`InstalledDepots`, `UserConfig`, ...), skipped
/// wholesale by brace-depth tracking — none of the five allow-listed keys
/// live inside one.
fn parse_acf(text: &str) -> Result<ManifestEntry, AcfParseError> {
    let toks = tokenize(text);
    let mut i = 0usize;

    match toks.first() {
        Some(Tok::Str(s)) if s == "AppState" => i += 1,
        _ => return Err(AcfParseError::Malformed),
    }
    match toks.get(i) {
        Some(Tok::Open) => i += 1,
        _ => return Err(AcfParseError::Malformed),
    }

    let mut entry = ManifestEntry::default();
    let mut appid_raw: Option<String> = None;
    let mut depth: u32 = 1;

    while depth > 0 {
        match toks.get(i) {
            None => return Err(AcfParseError::Malformed), // ran out of tokens: torn/truncated
            Some(Tok::Close) => {
                depth -= 1;
                i += 1;
            }
            Some(Tok::Open) => {
                // Unexpected without a preceding key; skip as an unlabeled
                // nested object rather than looping or panicking.
                i += 1;
                skip_nested(&toks, &mut i)?;
            }
            Some(Tok::Str(key)) => {
                let key = key.clone();
                i += 1;
                match toks.get(i) {
                    Some(Tok::Open) => {
                        i += 1;
                        skip_nested(&toks, &mut i)?;
                    }
                    Some(Tok::Str(val)) => {
                        let val = val.clone();
                        i += 1;
                        if depth == 1 {
                            match key.as_str() {
                                "appid" => appid_raw = Some(val),
                                "name" => entry.name = val,
                                "installdir" => entry.install_dir = val,
                                "SizeOnDisk" => entry.size_on_disk = val.parse().unwrap_or(0),
                                "StateFlags" => entry.state_flags = val.parse().unwrap_or(0),
                                // Allow-list: every other key, including
                                // `LastOwner`, is discarded here.
                                _ => {}
                            }
                        }
                    }
                    _ => return Err(AcfParseError::Malformed), // key with no value/object
                }
            }
        }
    }

    let Some(appid) = appid_raw else {
        return Err(AcfParseError::Malformed);
    };
    if !is_valid_appid(&appid) {
        return Err(AcfParseError::InvalidAppid);
    }
    entry.external_id = appid;
    Ok(entry)
}

/// Consume tokens up to and including the `Close` balancing the `Open` already
/// consumed by the caller (`*i` points just past it). Skips a nested object
/// wholesale, regardless of its internal structure.
fn skip_nested(toks: &[Tok], i: &mut usize) -> Result<(), AcfParseError> {
    let mut nd: u32 = 1;
    while nd > 0 {
        match toks.get(*i) {
            None => return Err(AcfParseError::Malformed),
            Some(Tok::Open) => {
                nd += 1;
                *i += 1;
            }
            Some(Tok::Close) => {
                nd -= 1;
                *i += 1;
            }
            Some(Tok::Str(_)) => {
                *i += 1;
            }
        }
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::fs;

    fn fixture(name: &str) -> String {
        // CARGO_MANIFEST_DIR is node-agent/ (this crate's root).
        let path = format!(
            "{}/tests/fixtures/steam_acf/{name}",
            env!("CARGO_MANIFEST_DIR")
        );
        fs::read_to_string(&path).unwrap_or_else(|e| panic!("reading fixture {path}: {e}"))
    }

    // ── Parser: real Tower manifests ────────────────────────────────────────

    #[test]
    fn parses_the_five_allowlisted_keys() {
        let text = fixture("appmanifest_517710.acf");
        let entry = parse_acf(&text).expect("well-formed fixture must parse");
        assert_eq!(entry.external_id, "517710");
        assert_eq!(entry.name, "Redout: Enhanced Edition");
        assert_eq!(entry.install_dir, "Redout");
        assert_eq!(entry.size_on_disk, 4_895_251_090);
        assert_eq!(entry.state_flags, 4);
    }

    #[test]
    fn handles_comma_and_colon_in_name() {
        let text = fixture("appmanifest_2183900.acf");
        let entry = parse_acf(&text).unwrap();
        assert_eq!(entry.name, "Warhammer 40,000: Space Marine 2");
        assert_eq!(entry.external_id, "2183900");
        assert_eq!(entry.state_flags, 6); // the one 9-manifest outlier
    }

    #[test]
    fn handles_en_dash_in_name() {
        let text = fixture("appmanifest_2536520.acf");
        let entry = parse_acf(&text).unwrap();
        assert_eq!(
            entry.name,
            "Diablo II: Resurrected \u{2013} Infernal Edition"
        );
        assert_eq!(entry.external_id, "2536520");
    }

    #[test]
    fn last_owner_cannot_escape_the_parser() {
        // All 9 fixtures carry LastOwner; belt-and-braces over the type-level
        // guarantee, the serialized JSON must never contain it either.
        const SCRUBBED_STEAMID: &str = "76561190000000000";
        let names = [
            "appmanifest_1493710.acf",
            "appmanifest_1628350.acf",
            "appmanifest_2180100.acf",
            "appmanifest_2183900.acf",
            "appmanifest_228980.acf",
            "appmanifest_2536520.acf",
            "appmanifest_3179810.acf",
            "appmanifest_4183110.acf",
            "appmanifest_517710.acf",
        ];
        for name in names {
            let text = fixture(name);
            assert!(
                text.contains("LastOwner"),
                "fixture {name} must actually contain LastOwner for this test to mean anything"
            );
            let entry = parse_acf(&text).unwrap_or_else(|e| panic!("{name} must parse: {e:?}"));
            let json = serde_json::to_string(&entry).unwrap();
            assert!(
                !json.contains(SCRUBBED_STEAMID),
                "{name}: LastOwner's value leaked into the parsed struct: {json}"
            );
        }
    }

    #[test]
    fn all_nine_fixtures_parse_with_expected_appids() {
        let expected = [
            "1493710", "1628350", "2180100", "2183900", "228980", "2536520", "3179810", "4183110",
            "517710",
        ];
        for appid in expected {
            let text = fixture(&format!("appmanifest_{appid}.acf"));
            let entry = parse_acf(&text).unwrap();
            assert_eq!(entry.external_id, appid);
        }
    }

    // ── Parser: malformed / injection ───────────────────────────────────────

    #[test]
    fn empty_appid_is_dropped() {
        let text = r#""AppState" { "appid" "" "name" "x" }"#;
        assert_eq!(parse_acf(text), Err(AcfParseError::InvalidAppid));
    }

    #[test]
    fn zero_appid_is_dropped() {
        let text = r#""AppState" { "appid" "0" "name" "x" }"#;
        assert_eq!(parse_acf(text), Err(AcfParseError::InvalidAppid));
    }

    #[test]
    fn injection_appid_is_dropped() {
        let text = r#""AppState" { "appid" "1; rm -rf /" "name" "x" }"#;
        assert_eq!(parse_acf(text), Err(AcfParseError::InvalidAppid));
    }

    #[test]
    fn too_long_appid_is_dropped() {
        let text = r#""AppState" { "appid" "99999999999" "name" "x" }"#;
        assert_eq!(parse_acf(text), Err(AcfParseError::InvalidAppid));
    }

    #[test]
    fn ten_digit_appid_is_accepted() {
        let text = r#""AppState" { "appid" "1234567890" "name" "x" }"#;
        assert_eq!(parse_acf(text).unwrap().external_id, "1234567890");
    }

    #[test]
    fn torn_manifest_is_malformed_not_fatal() {
        let full = fixture("appmanifest_517710.acf");
        let torn = &full[..full.len() / 2];
        assert_eq!(parse_acf(torn), Err(AcfParseError::Malformed));
    }

    #[test]
    fn missing_appstate_wrapper_is_malformed() {
        assert_eq!(
            parse_acf(r#"{ "appid" "1" }"#),
            Err(AcfParseError::Malformed)
        );
    }

    #[test]
    fn missing_appid_key_is_malformed() {
        let text = r#""AppState" { "name" "no appid here" }"#;
        assert_eq!(parse_acf(text), Err(AcfParseError::Malformed));
    }

    #[test]
    fn nested_objects_are_skipped_wholesale() {
        let text = r#""AppState"
        {
            "appid" "42"
            "name" "Nested Test"
            "InstalledDepots"
            {
                "42001" { "manifest" "123" "size" "456" }
            }
            "UserConfig" {}
            "StateFlags" "4"
        }"#;
        let entry = parse_acf(text).unwrap();
        assert_eq!(entry.external_id, "42");
        assert_eq!(entry.name, "Nested Test");
        assert_eq!(entry.state_flags, 4);
    }

    // ── Walk: containment, symlinks, caps ───────────────────────────────────

    fn write(path: &Path, content: &str) {
        if let Some(parent) = path.parent() {
            fs::create_dir_all(parent).unwrap();
        }
        fs::write(path, content).unwrap();
    }

    fn synth_manifest(appid: &str) -> String {
        format!(
            r#""AppState" {{ "appid" "{appid}" "name" "Game {appid}" "installdir" "d{appid}" "SizeOnDisk" "1" "StateFlags" "4" "LastOwner" "76561190000000000" }}"#
        )
    }

    /// Named acceptance test: a root_path outside the agent's configured home
    /// root is refused and reported as an error, and the walk never runs.
    #[test]
    fn root_path_outside_configured_home_root_is_refused() {
        let scan = ScanTask {
            scan_id: "s1".into(),
            root_path: "/definitely/outside/anything".into(),
            relative_roots: vec![".local/share/Steam/steamapps".into()],
            max_entries: 512,
            max_manifest_bytes: 1_048_576,
        };
        let configured_root = Some(PathBuf::from("/mnt/user/appdata/quasar/homes"));
        let result = scan_root_path(&scan, configured_root);
        let err = result.expect_err("a root_path outside the configured root must be refused");
        assert!(!err.is_empty());
    }

    /// The positive case, proving the refusal above tests containment and not
    /// just "always errors": a contained root_path that doesn't exist yet is
    /// "nothing to scan", not an error.
    #[test]
    fn root_path_under_configured_home_root_is_walked() {
        let scan = ScanTask {
            scan_id: "s1".into(),
            root_path: "/mnt/user/appdata/quasar/homes/some-opaque-id".into(),
            relative_roots: vec![".local/share/Steam/steamapps".into()],
            max_entries: 512,
            max_manifest_bytes: 1_048_576,
        };
        let configured_root = Some(PathBuf::from("/mnt/user/appdata/quasar/homes"));
        let entries = scan_root_path(&scan, configured_root)
            .expect("a contained root_path must not be refused");
        assert!(entries.is_empty());
    }

    /// A volume-driver instance (or one that lost its home-root config) has
    /// nothing to walk against — refuse rather than no-op indistinguishably
    /// from "scanned, found nothing".
    #[test]
    fn no_configured_home_root_is_refused() {
        let scan = ScanTask {
            scan_id: "s1".into(),
            root_path: "/mnt/user/appdata/quasar/homes/some-opaque-id".into(),
            relative_roots: vec![],
            max_entries: 512,
            max_manifest_bytes: 1_048_576,
        };
        let result = scan_root_path(&scan, None);
        assert!(result.is_err());
    }

    #[test]
    fn traversal_in_root_path_is_refused() {
        assert!(has_traversal(Path::new("/data/homes/../../etc")));
        assert!(!has_traversal(Path::new("/data/homes/u/a")));
    }

    #[test]
    fn resolve_relative_root_refuses_absolute_relative_root() {
        assert_eq!(
            resolve_relative_root(Path::new("/root"), "/etc/passwd"),
            None
        );
    }

    #[test]
    fn resolve_relative_root_refuses_traversal() {
        assert_eq!(resolve_relative_root(Path::new("/root"), "../../etc"), None);
    }

    #[test]
    fn resolve_relative_root_accepts_plain_relative() {
        let dir = tempfile::tempdir().unwrap();
        let target = dir.path().join(".local/share/Steam/steamapps");
        fs::create_dir_all(&target).unwrap();
        assert_eq!(
            resolve_relative_root(dir.path(), ".local/share/Steam/steamapps"),
            Some(target)
        );
    }

    #[test]
    fn resolve_relative_root_refuses_missing_intermediate() {
        let dir = tempfile::tempdir().unwrap();
        assert_eq!(
            resolve_relative_root(dir.path(), ".local/share/Steam/steamapps"),
            None
        );
    }

    #[test]
    fn symlinked_manifest_is_refused() {
        let dir = tempfile::tempdir().unwrap();
        let steamapps = dir.path().join("steamapps");
        fs::create_dir_all(&steamapps).unwrap();
        let real = dir.path().join("real_manifest.acf");
        write(&real, &synth_manifest("111111"));
        let link = steamapps.join("appmanifest_111111.acf");
        std::os::unix::fs::symlink(&real, &link).unwrap();

        let mut out = Vec::new();
        let mut seen = HashSet::new();
        collect_manifests(&steamapps, &mut out, &mut seen, 512, 1_048_576);
        assert!(out.is_empty(), "a symlinked manifest must never be read");
    }

    /// An *intermediate* path component (`.local`, not the final `steamapps`
    /// directory) replaced with a symlink outside `root_path`. A naive
    /// `root.join(rel)` + one `symlink_metadata` call only checks the FINAL
    /// component — the kernel resolves every earlier one transparently — so
    /// it walks straight through and reads manifests outside the configured
    /// home root. A real user can trigger this: their Steam home is
    /// bind-mounted read-write into their own container, so they control
    /// every path component under it, including `.local`.
    #[test]
    fn intermediate_symlink_in_relative_root_is_refused() {
        let dir = tempfile::tempdir().unwrap();
        let root = dir.path().join("root");
        fs::create_dir_all(&root).unwrap();

        // Outside root — stands in for "another user's home".
        let outside = dir.path().join("outside-another-users-home");
        let outside_steamapps = outside.join("share/Steam/steamapps");
        fs::create_dir_all(&outside_steamapps).unwrap();
        write(
            &outside_steamapps.join("appmanifest_999999.acf"),
            &synth_manifest("999999"),
        );

        // `root/.local` is a symlink to `outside` — an ancestor of the final
        // `steamapps` component, not that component itself.
        std::os::unix::fs::symlink(&outside, root.join(".local")).unwrap();

        // The resolver must refuse outright...
        assert_eq!(
            resolve_relative_root(&root, ".local/share/Steam/steamapps"),
            None,
            "a symlinked intermediate component must not be walked through"
        );

        // ...and the full walk must find nothing, not the outside game.
        let entries = walk_and_parse(
            &root,
            &[".local/share/Steam/steamapps".to_string()],
            512,
            1_048_576,
        );
        assert!(
            entries.is_empty(),
            "the walk must not follow a symlinked intermediate directory out of root_path: got {entries:?}"
        );
    }

    #[test]
    fn max_entries_cap_is_enforced() {
        let dir = tempfile::tempdir().unwrap();
        let steamapps = dir.path().join("steamapps");
        fs::create_dir_all(&steamapps).unwrap();
        for n in 0..10 {
            let appid = format!("{}", 1000 + n);
            write(
                &steamapps.join(format!("appmanifest_{appid}.acf")),
                &synth_manifest(&appid),
            );
        }
        let mut out = Vec::new();
        let mut seen = HashSet::new();
        collect_manifests(&steamapps, &mut out, &mut seen, 3, 1_048_576);
        assert_eq!(
            out.len(),
            3,
            "max_entries must cap the number of parsed manifests"
        );
    }

    #[test]
    fn max_manifest_bytes_cap_is_enforced() {
        let dir = tempfile::tempdir().unwrap();
        let steamapps = dir.path().join("steamapps");
        fs::create_dir_all(&steamapps).unwrap();
        // A manifest padded well past a tiny cap via a long name value.
        let big_name = "x".repeat(2000);
        let content = format!(
            r#""AppState" {{ "appid" "222222" "name" "{big_name}" "installdir" "d" "SizeOnDisk" "1" "StateFlags" "4" }}"#
        );
        write(&steamapps.join("appmanifest_222222.acf"), &content);
        write(
            &steamapps.join("appmanifest_333333.acf"),
            &synth_manifest("333333"),
        );

        // The small (synthetic) manifest is ~140 bytes; the padded one is
        // ~2000+. A cap of 300 sits strictly between them.
        let mut out = Vec::new();
        let mut seen = HashSet::new();
        collect_manifests(&steamapps, &mut out, &mut seen, 512, 300);
        assert_eq!(
            out.len(),
            1,
            "the oversized manifest must be skipped, the small one kept"
        );
        assert_eq!(out[0].external_id, "333333");
    }

    #[test]
    fn steamapps_found_within_depth_cap_is_scanned() {
        let dir = tempfile::tempdir().unwrap();
        // depth: .local(1)/share(2)/Steam(3)/steamapps(4)
        let steamapps = dir
            .path()
            .join(".local")
            .join("share")
            .join("Steam")
            .join("steamapps");
        fs::create_dir_all(&steamapps).unwrap();
        write(
            &steamapps.join("appmanifest_444444.acf"),
            &synth_manifest("444444"),
        );

        let entries = walk_and_parse(dir.path(), &[], 512, 1_048_576);
        assert_eq!(entries.len(), 1);
        assert_eq!(entries[0].external_id, "444444");
    }

    #[test]
    fn steamapps_beyond_depth_cap_is_not_scanned() {
        let dir = tempfile::tempdir().unwrap();
        // depth 5 — one level deeper than SCAN_STEAMAPPS_MAX_DEPTH (4).
        let steamapps = dir
            .path()
            .join("a")
            .join("b")
            .join("c")
            .join("d")
            .join("steamapps");
        fs::create_dir_all(&steamapps).unwrap();
        write(
            &steamapps.join("appmanifest_555555.acf"),
            &synth_manifest("555555"),
        );

        let entries = walk_and_parse(dir.path(), &[], 512, 1_048_576);
        assert!(
            entries.is_empty(),
            "a steamapps dir past the depth cap must not be scanned"
        );
    }

    #[test]
    fn duplicate_dir_from_relative_root_and_walk_is_not_double_counted() {
        let dir = tempfile::tempdir().unwrap();
        let steamapps = dir.path().join("steamapps");
        fs::create_dir_all(&steamapps).unwrap();
        write(
            &steamapps.join("appmanifest_666666.acf"),
            &synth_manifest("666666"),
        );

        // "steamapps" is both an explicit relative_root AND directly
        // discoverable by the depth walk (depth 1) — must be scanned once.
        let entries = walk_and_parse(dir.path(), &["steamapps".to_string()], 512, 1_048_576);
        assert_eq!(entries.len(), 1);
    }
}
