//! The agent talks to the container engine only through its HTTP API (#239).
//!
//! The image ships no `docker`/`podman` executable, so a `Command::new("docker")`
//! would not fail loudly — it would fail at the exact moment a session or a boot
//! sweep needed it, on a host nobody is watching. This walks the real source tree
//! so a reintroduced shell-out fails on the commit that adds it.
//!
//! Scope, and why: the rules below apply to the agent's PRODUCTION paths. A file's
//! `#[cfg(test)]` module and the `*_tests.rs` modules are skipped — they legitimately
//! re-exec the test binary, run `sh` against a probe script under test, and (in the
//! opt-in `*_docker_tests.rs` fixtures) drive the HOST's docker CLI as test tooling.
//! What must never happen is the agent doing it at runtime.

use std::path::{Path, PathBuf};

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
    for path in entries {
        if path.is_dir() {
            rs_files(&path, out);
        } else if path.extension().is_some_and(|e| e == "rs") {
            out.push(path);
        }
    }
}

struct Source {
    rel: String,
    /// The file up to its first `#[cfg(test)]`, which is its production half.
    production: String,
    all: String,
}

fn sources() -> Vec<Source> {
    let root = src_root();
    let mut files = Vec::new();
    rs_files(&root, &mut files);
    files
        .into_iter()
        .map(|path| {
            let text = std::fs::read_to_string(&path).expect("read source file");
            let rel = path
                .strip_prefix(&root)
                .unwrap()
                .to_string_lossy()
                .into_owned();
            let production = if rel.ends_with("_tests.rs") {
                String::new()
            } else {
                text.split("#[cfg(test)]").next().unwrap_or("").to_owned()
            };
            Source {
                rel,
                production,
                all: text,
            }
        })
        .collect()
}

/// Lines of `text` carrying `needle`, as `file:line` with the line's own text.
fn hits(source: &Source, text: &str, needle: &str) -> Vec<String> {
    text.lines()
        .enumerate()
        .filter(|(_, line)| line.contains(needle))
        .map(|(i, line)| format!("{}:{}: {}", source.rel, i + 1, line.trim()))
        .collect()
}

/// No call site names an engine binary, in production or in a test. The opt-in
/// docker fixtures reach the CLI through an absolute `/usr/bin/timeout`, so this
/// stays true for them too.
#[test]
fn no_call_site_execs_docker_or_podman() {
    let sources = sources();
    assert!(sources.len() > 30, "the scanner found no sources");
    let mut bad = Vec::new();
    for source in &sources {
        for name in ["\"docker\"", "\"podman\"", "'docker'", "'podman'"] {
            for hit in hits(source, &source.all, &format!("Command::new({name}")) {
                bad.push(hit);
            }
        }
    }
    assert!(
        bad.is_empty(),
        "the agent image ships no engine CLI; talk to the engine through crate::runtime:\n{}",
        bad.join("\n")
    );
}

/// The launch path and the whole runtime module spawn no child process at all.
/// `runtime/docker/credentials.rs` is the documented exception: Docker-ecosystem
/// credential helpers are registry tooling, not the engine.
#[test]
fn the_launch_path_and_runtime_module_spawn_no_child_process() {
    let mut bad = Vec::new();
    for source in sources() {
        let scoped = source.rel == "session/container.rs" || source.rel.starts_with("runtime/");
        if !scoped || source.rel == "runtime/docker/credentials.rs" {
            continue;
        }
        for needle in ["Command::new", "process::Command"] {
            bad.extend(hits(&source, &source.production, needle));
        }
    }
    assert!(
        bad.is_empty(),
        "an un-owned mutation path: these modules own container lifecycle and must reach \
         the engine only through its API:\n{}",
        bad.join("\n")
    );
}

/// `QUASAR_CONTAINER_RUNTIME` is retired: it selects nothing, and the one place it
/// may still be named is the startup warning that says so.
#[test]
fn the_retired_runtime_knob_is_only_named_where_it_is_refused() {
    let mut sites = Vec::new();
    for source in sources() {
        sites.extend(hits(&source, &source.all, "QUASAR_CONTAINER_RUNTIME"));
    }
    let warning: Vec<_> = sites
        .iter()
        .filter(|site| site.starts_with("runtime.rs:"))
        .collect();
    assert_eq!(
        sites.len(),
        warning.len(),
        "the knob is ignored; no code may read it to choose anything:\n{}",
        sites.join("\n")
    );
    assert!(
        !warning.is_empty(),
        "the retired-knob warning has disappeared — an operator whose .env still \
         carries it would get no explanation"
    );
    let source = std::fs::read_to_string(src_root().join("runtime.rs")).unwrap();
    assert!(
        source.contains("token = \"runtime-cli-knob-retired\""),
        "the retired-knob site must keep its grep token"
    );
}
