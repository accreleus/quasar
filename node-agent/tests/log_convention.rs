//! The WARN/ERROR `token=` convention, enforced.
//!
//! Prose changes; a grep pattern built on prose rots. Every `warn!`/`error!` in
//! the node agent therefore carries a stable `token = "<area>-<condition>"` as
//! its first field, so an operator (or an agent) can find a cause by token
//! instead of by remembering how the sentence was worded. The convention itself
//! — levels, naming, how to grep — is `.claude/rules/agent-logging.md`.
//!
//! This walks the real source tree rather than a fixture: a new call site has to
//! satisfy the convention on the commit that introduces it, not whenever someone
//! next remembers to update a list.

use std::collections::BTreeMap;
use std::path::{Path, PathBuf};

/// Tokens that legitimately appear at more than one call site because the sites
/// mean the *same* condition (the same failure reachable from two arms, or the
/// same fact discovered by the session runner and by the standalone session
/// server). Anything not listed here must be unique: two different conditions
/// sharing a token makes the token useless for finding either one.
const SHARED_TOKENS: &[&str] = &[
    "app-exit-disposition-unrecognized",
    "audio-fallback-silent",
    "audio-unavailable-silent",
    "console-weston-exit-timeout",
    "datachannel-create-returned-nothing",
    "image-op-error",
    "knob-invalid-nvenc-max-sessions",
    "knob-invalid-vulkan-max-sessions",
    "session-assign-rejected",
    "udev-export-failed",
    "vulkan-encoder-rearm-not-found",
    "webrtc-duplicate-answer",
    "webrtc-set-remote-failed",
];

/// Call sites deliberately exempt from the convention. Empty, and meant to stay
/// that way: an exemption is a hole in the grep surface, so add a token instead.
const ALLOWED_UNTOKENED: &[&str] = &[];

struct Site {
    file: String,
    line: usize,
    token: Option<String>,
}

fn src_root() -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR")).join("src")
}

fn rs_files(dir: &Path, out: &mut Vec<PathBuf>) {
    let mut entries: Vec<_> = std::fs::read_dir(dir)
        .unwrap_or_else(|e| panic!("read_dir {}: {e}", dir.display()))
        .filter_map(Result::ok)
        .map(|e| e.path())
        .collect();
    entries.sort();
    for p in entries {
        if p.is_dir() {
            rs_files(&p, out);
        } else if p.extension().is_some_and(|e| e == "rs") {
            out.push(p);
        }
    }
}

/// True when the `warn!`/`error!` at `start` is real code rather than a mention
/// inside a comment or a string literal. Cheap and line-local, which is enough:
/// the macro name always starts on the line it is invoked from.
fn is_code(line: &str, start: usize) -> bool {
    let before = &line[..start];
    if before.contains("//") {
        return false;
    }
    // An odd number of unescaped quotes before it means we are inside a string.
    let mut in_string = false;
    let mut escaped = false;
    for c in before.chars() {
        match c {
            _ if escaped => escaped = false,
            '\\' => escaped = true,
            '"' => in_string = !in_string,
            _ => {}
        }
    }
    !in_string
}

/// Find every `warn!(` / `error!(` (bare or `tracing::`-qualified) and read back
/// the first argument. `gst::loggable_error!` and friends are not tracing macros
/// and are skipped — the `!` must be preceded by exactly `warn` or `error`.
fn collect_sites() -> Vec<Site> {
    let root = src_root();
    let mut files = Vec::new();
    rs_files(&root, &mut files);
    let mut sites = Vec::new();
    for path in files {
        let text = std::fs::read_to_string(&path).expect("read source file");
        let rel = path
            .strip_prefix(&root)
            .unwrap()
            .to_string_lossy()
            .into_owned();
        for (idx, line) in text.lines().enumerate() {
            for name in ["warn", "error"] {
                let pat = format!("{name}!(");
                let mut from = 0usize;
                while let Some(rel_at) = line[from..].find(&pat) {
                    let at = from + rel_at;
                    from = at + pat.len();
                    // `loggable_error!`, `my_warn!`: the identifier must end here.
                    let prev_ok = line[..at]
                        .chars()
                        .next_back()
                        .is_none_or(|c| !c.is_alphanumeric() && c != '_');
                    if !prev_ok || !is_code(line, at) {
                        continue;
                    }
                    let after: String = text
                        .lines()
                        .skip(idx)
                        .collect::<Vec<_>>()
                        .join("\n")
                        .chars()
                        .skip(at + pat.len())
                        .collect();
                    sites.push(Site {
                        file: rel.clone(),
                        line: idx + 1,
                        token: first_token(&after),
                    });
                }
            }
        }
    }
    sites
}

/// Read `token = "<value>"` off the front of a macro body, allowing a leading
/// `target: <expr>,` (which tracing requires to come first).
fn first_token(body: &str) -> Option<String> {
    let mut rest = body.trim_start();
    if rest.starts_with("target:") {
        let mut depth = 0i32;
        let mut in_str = false;
        let mut escaped = false;
        let mut cut = None;
        for (i, c) in rest.char_indices() {
            match c {
                _ if escaped => escaped = false,
                '\\' if in_str => escaped = true,
                '"' => in_str = !in_str,
                _ if in_str => {}
                '(' | '[' | '{' => depth += 1,
                ')' | ']' | '}' => depth -= 1,
                ',' if depth == 0 => {
                    cut = Some(i + 1);
                    break;
                }
                _ => {}
            }
        }
        rest = rest[cut?..].trim_start();
    }
    let rest = rest.strip_prefix("token")?.trim_start();
    let rest = rest.strip_prefix('=')?.trim_start();
    let rest = rest.strip_prefix('"')?;
    let end = rest.find('"')?;
    Some(rest[..end].to_string())
}

fn is_kebab(s: &str) -> bool {
    !s.is_empty()
        && !s.starts_with('-')
        && !s.ends_with('-')
        && !s.contains("--")
        && s.chars()
            .all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || c == '-')
        && s.contains('-')
}

#[test]
fn every_warn_and_error_carries_a_kebab_case_token() {
    let sites = collect_sites();
    assert!(
        sites.len() > 300,
        "only found {} warn/error sites — the scanner is broken, not the source",
        sites.len()
    );
    let mut bad = Vec::new();
    for s in &sites {
        let at = format!("{}:{}", s.file, s.line);
        if ALLOWED_UNTOKENED.contains(&at.as_str()) {
            continue;
        }
        match &s.token {
            None => bad.push(format!("{at}: no `token = \"…\"` as the first field")),
            Some(t) if !is_kebab(t) => bad.push(format!("{at}: token {t:?} is not kebab-case")),
            Some(_) => {}
        }
    }
    assert!(
        bad.is_empty(),
        "{} warn/error site(s) break the token convention \
         (see .claude/rules/agent-logging.md):\n{}",
        bad.len(),
        bad.join("\n")
    );
}

#[test]
fn a_token_names_one_condition() {
    let mut by_token: BTreeMap<String, Vec<String>> = BTreeMap::new();
    for s in collect_sites() {
        if let Some(t) = s.token {
            by_token
                .entry(t)
                .or_default()
                .push(format!("{}:{}", s.file, s.line));
        }
    }
    let mut bad = Vec::new();
    for (token, sites) in &by_token {
        if sites.len() > 1 && !SHARED_TOKENS.contains(&token.as_str()) {
            bad.push(format!("{token}: {}", sites.join(", ")));
        }
    }
    assert!(
        bad.is_empty(),
        "token(s) reused across call sites that are not listed as shared \
         (add to SHARED_TOKENS only if the sites mean the SAME condition):\n{}",
        bad.join("\n")
    );
    // The reverse: a shared token that stopped being shared is a stale entry.
    let stale: Vec<_> = SHARED_TOKENS
        .iter()
        .filter(|t| by_token.get(**t).is_none_or(|s| s.len() < 2))
        .collect();
    assert!(
        stale.is_empty(),
        "SHARED_TOKENS lists token(s) that no longer have two call sites: {stale:?}"
    );
}

/// The allow-list exists so a genuine exception has a home; it is empty on
/// purpose. This test is the tripwire that says so out loud if one is added.
#[test]
// The point of the assert is that it is const-true TODAY. It exists so that the
// day someone adds an exemption, a test fails and says why — clippy reading it
// as trivially true is the whole design.
#[allow(clippy::const_is_empty)]
fn the_exemption_list_is_empty() {
    assert!(
        ALLOWED_UNTOKENED.is_empty(),
        "an exemption is a hole in the grep surface — give the site a token instead: {ALLOWED_UNTOKENED:?}"
    );
}
