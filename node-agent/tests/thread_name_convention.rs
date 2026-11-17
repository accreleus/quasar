//! Every named thread keeps its whole name (#573).
//!
//! The kernel keeps 15 bytes of a thread's name, and the crash report prints what the
//! kernel kept. This walks the real source tree, so a new `thread::Builder` has to fit on
//! the commit that adds it.

use std::path::{Path, PathBuf};

#[path = "support/source_roots.rs"]
mod source_roots;
use source_roots::source_roots;

const KERNEL_NAME_BYTES: usize = 15;

fn rs_files(dir: &Path, out: &mut Vec<PathBuf>) {
    for entry in std::fs::read_dir(dir).unwrap().filter_map(Result::ok) {
        let path = entry.path();
        if path.is_dir() {
            rs_files(&path, out);
        } else if path.extension().is_some_and(|e| e == "rs") {
            out.push(path);
        }
    }
}

#[test]
fn no_thread_name_is_longer_than_the_kernel_keeps() {
    let builder =
        regex::Regex::new(r"(?s)thread::Builder::new\(\)\s*\.name\(\s*(.{0,80})").unwrap();
    let runtime = regex::Regex::new(r#"\.thread_name\(\s*"([^"]*)""#).unwrap();
    let literal = regex::Regex::new(r#"^"([^"]*)""#).unwrap();
    let constant = regex::Regex::new(r"^([A-Z][A-Z0-9_]*)").unwrap();
    let mut seen = 0;
    let mut too_long = Vec::new();
    for (root, _) in source_roots() {
        let mut files = Vec::new();
        rs_files(&root, &mut files);
        for file in files {
            let text = std::fs::read_to_string(&file).unwrap();
            let mut names: Vec<String> =
                runtime.captures_iter(&text).map(|c| c[1].into()).collect();
            for site in builder.captures_iter(&text) {
                let arg = &site[1];
                let name = if let Some(c) = literal.captures(arg) {
                    c[1].to_string()
                } else if let Some(c) = constant.captures(arg) {
                    let decl = format!(r#"const {}: &str = "([^"]*)""#, &c[1]);
                    let found = regex::Regex::new(&decl).unwrap().captures(&text);
                    found.unwrap_or_else(|| panic!("{}: no `const {}`", file.display(), &c[1]))[1]
                        .to_string()
                } else {
                    panic!(
                        "{}: a thread name that is neither a literal nor a const here: {arg}",
                        file.display()
                    );
                };
                names.push(name);
            }
            seen += names.len();
            too_long.extend(
                names
                    .into_iter()
                    .filter(|n| n.len() > KERNEL_NAME_BYTES)
                    .map(|n| format!("{} ({} bytes): {n}", file.display(), n.len())),
            );
        }
    }
    assert!(
        seen >= 15,
        "found only {seen} thread names: the scan lost its way"
    );
    assert!(
        too_long.is_empty(),
        "over {KERNEL_NAME_BYTES} bytes:\n{}",
        too_long.join("\n")
    );
}
