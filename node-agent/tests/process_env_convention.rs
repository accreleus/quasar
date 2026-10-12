//! The agent writes its own environment only before its threads exist (#267).
//!
//! `setenv` beside a `getenv` on another thread is a use-after-free on a libc that
//! frees the old environment array, and GStreamer, Mesa, the Vulkan loader and the GPU
//! drivers all read the environment while a device comes up. glibc 2.41 stopped freeing
//! it, which is what the agent images ship; the rule keeps the agent right on any libc.
//!
//! Every write is in the table below. All but one are reached only from `main`'s
//! `prepare_environment`, which runs before the tokio runtime is built. The one that is
//! not, `nvidia_volume::apply_process_env`, breaks the rule and is left that way: it
//! writes from a blocking-pool thread at agent start, after the runtime's workers exist.
//!
//! A reviewer cannot see a new `set_var` on a session path by reading the diff that adds
//! it, so the table is enforced: put a new write in `prepare_environment` or
//! `session::prepare_gst_env`, then count it here.

use std::path::{Path, PathBuf};

/// (file, writes). `nvidia_volume.rs` is the write that still happens with other
/// threads running: `apply_process_env` needs the adopted driver volume, which is found
/// at agent start. It runs once, before GStreamer, EGL or a session exists, beside the
/// runtime's workers, the health server and the homes sweeper.
const WRITES: &[(&str, usize)] = &[
    ("main.rs", 2),
    ("nvidia_volume.rs", 1),
    ("session/mod.rs", 3),
    ("session/pipeline/source_branch.rs", 1),
];

fn rs_files(dir: &Path, out: &mut Vec<PathBuf>) {
    for entry in std::fs::read_dir(dir).unwrap() {
        let path = entry.unwrap().path();
        if path.is_dir() {
            rs_files(&path, out);
        } else if path.extension().is_some_and(|e| e == "rs") {
            out.push(path);
        }
    }
}

/// Test code may set what it likes: a `tests` directory, a `*tests.rs` file, and
/// everything from a file's `mod tests` on.
fn production(path: &Path, text: &str) -> Option<String> {
    let name = path.file_name()?.to_str()?;
    if name.ends_with("tests.rs") || path.components().any(|c| c.as_os_str() == "tests") {
        return None;
    }
    let end = text.find("#[cfg(test)]\nmod ").unwrap_or(text.len());
    Some(text[..end].to_string())
}

#[test]
fn every_environment_write_is_one_the_table_knows() {
    let src = Path::new(env!("CARGO_MANIFEST_DIR")).join("src");
    let mut files = Vec::new();
    rs_files(&src, &mut files);
    let mut found: Vec<(String, usize)> = files
        .iter()
        .filter_map(|path| {
            let text = production(path, &std::fs::read_to_string(path).unwrap())?;
            let writes = text
                .lines()
                .filter(|line| !line.trim_start().starts_with("//"))
                .filter(|line| line.contains("env::set_var(") || line.contains("env::remove_var("))
                .count();
            let rel = path
                .strip_prefix(&src)
                .unwrap()
                .to_str()
                .unwrap()
                .to_string();
            (writes > 0).then_some((rel, writes))
        })
        .collect();
    found.sort();
    let expected: Vec<(String, usize)> = WRITES.iter().map(|(f, n)| (f.to_string(), *n)).collect();
    assert_eq!(
        found, expected,
        "an environment write moved or appeared: it belongs in `prepare_environment` \
         (main.rs) or `session::prepare_gst_env`, before any thread exists"
    );
}
