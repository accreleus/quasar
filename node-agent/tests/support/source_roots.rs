//! Every source tree linked into the agent, for the source-scanning convention tests.

use std::path::{Path, PathBuf};

/// The agent's own `src/`, reported as `<path>`, then the shared runtime crate's
/// (#355) as `quasar-runtime/<path>`, then the recovery actor's as `quasar-recovery/<path>`.
pub fn source_roots() -> [(PathBuf, &'static str); 3] {
    let manifest = Path::new(env!("CARGO_MANIFEST_DIR"));
    [
        (manifest.join("src"), ""),
        (
            manifest.join("crates/quasar-runtime/src"),
            "quasar-runtime/",
        ),
        (
            manifest.join("crates/quasar-recovery/src"),
            "quasar-recovery/",
        ),
    ]
}
