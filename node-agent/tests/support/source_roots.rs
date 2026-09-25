//! Every source tree linked into the agent, for the source-scanning convention tests.

use std::path::{Path, PathBuf};

/// The agent's own `src/`, reported as `<path>`, then the shared runtime crate's
/// (#355), reported as `quasar-runtime/<path>`.
pub fn source_roots() -> [(PathBuf, &'static str); 2] {
    let manifest = Path::new(env!("CARGO_MANIFEST_DIR"));
    [
        (manifest.join("src"), ""),
        (
            manifest.join("crates/quasar-runtime/src"),
            "quasar-runtime/",
        ),
    ]
}
