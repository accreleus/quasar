//! The crate is GStreamer-free and CUDA-free (#356), like quasar-runtime: the recovery actor
//! is a slim static image. Walks the workspace Cargo.lock dependency closure of
//! `quasar-recovery` (normal, build and dev edges alike) and fails on any crate from the
//! gstreamer-rs/glib family or anything CUDA.

use std::collections::{BTreeMap, BTreeSet};
use std::path::Path;

/// `name` or `name version` → the dependency names of every locked package.
fn lock_graph() -> BTreeMap<String, Vec<String>> {
    let lock =
        std::fs::read_to_string(Path::new(env!("CARGO_MANIFEST_DIR")).join("../../Cargo.lock"))
            .expect("read the workspace Cargo.lock");
    let mut graph = BTreeMap::new();
    for block in lock.split("[[package]]").skip(1) {
        let field = |key: &str| {
            block
                .lines()
                .find_map(|l| l.strip_prefix(&format!("{key} = \"")))
                .map(|v| v.trim_end_matches('"').to_owned())
        };
        let (Some(name), Some(version)) = (field("name"), field("version")) else {
            continue;
        };
        let deps: Vec<String> = match block.split_once("dependencies = [") {
            Some((_, rest)) => rest
                .split(']')
                .next()
                .unwrap_or("")
                .lines()
                .map(|l| l.trim().trim_end_matches(',').trim_matches('"').to_owned())
                .filter(|l| !l.is_empty())
                .collect(),
            None => Vec::new(),
        };
        // A dependency is written `name` when one version is locked, `name version`
        // otherwise; register the package under both keys.
        graph.insert(format!("{name} {version}"), deps.clone());
        graph.entry(name).or_insert(deps);
    }
    graph
}

fn closure(root: &str) -> BTreeSet<String> {
    let graph = lock_graph();
    let mut seen = BTreeSet::new();
    let mut todo = vec![root.to_owned()];
    while let Some(key) = todo.pop() {
        let name = key.split(' ').next().unwrap().to_owned();
        if !seen.insert(name) {
            continue;
        }
        let deps = graph
            .get(&key)
            .unwrap_or_else(|| panic!("{key} is not in Cargo.lock"));
        todo.extend(deps.iter().cloned());
    }
    seen
}

fn forbidden(name: &str) -> bool {
    name.starts_with("gstreamer")
        || name.starts_with("gst")
        || name.starts_with("glib")
        || name.starts_with("gio")
        || name.starts_with("gobject")
        || name.contains("cuda")
}

#[test]
fn the_recovery_crate_links_nothing_from_gstreamer_glib_or_cuda() {
    let deps = closure("quasar-recovery");
    // Not vacuous: the walk reaches the verifier, the regex engine and the fetch stack.
    for crate_name in ["ring", "regex", "rustls", "hyper", "tokio", "webpki-roots"] {
        assert!(
            deps.contains(crate_name),
            "{crate_name} missing from {deps:?}"
        );
    }
    let bad: Vec<_> = deps.iter().filter(|name| forbidden(name)).collect();
    assert!(
        bad.is_empty(),
        "quasar-recovery must stay GStreamer/CUDA-free (the recovery actor is a slim static binary): {bad:?}"
    );
}

/// The release-asset fetch trusts webpki-roots over ring, never the OS store, OpenSSL or
/// aws-lc (whose build would also leave the slim static image).
#[test]
fn the_fetch_stack_is_ring_and_webpki_only() {
    let deps = closure("quasar-recovery");
    let bad: Vec<_> = deps
        .iter()
        .filter(|n| {
            n.starts_with("aws-lc")
                || n.starts_with("openssl")
                || n.contains("native-certs")
                || n == &"native-tls"
        })
        .collect();
    assert!(bad.is_empty(), "{bad:?}");
}

/// The detector itself: the agent's closure does carry GStreamer.
#[test]
fn the_same_walk_finds_gstreamer_in_the_agent() {
    assert!(closure("quasar-node-agent")
        .iter()
        .any(|name| name.starts_with("gstreamer")));
}
