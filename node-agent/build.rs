// Build script: link libgstcuda-1.0 only when the `cuda` feature is on (ZC-02 N-A).
//
// The node-agent is one source that builds for BOTH the AMD/hermes image (VA encode,
// no libgstcuda) and the NVIDIA/quasar-nv image (NVENC zero-copy, libgstcuda present).
// The full-zero-copy N-A path FFI-links libgstcuda-1.0 (gst_cuda_context_new etc.); that
// lib + its pkg-config (`gstreamer-cuda-1.0`) exist only in the quasar-nv image, so the
// link is gated on the `cuda` feature. Without the feature this build script is a no-op
// and the AMD build is unchanged.
fn main() {
    stamp_build_identity();

    #[cfg(feature = "cuda")]
    {
        match pkg_config::Config::new().probe("gstreamer-cuda-1.0") {
            Ok(_) => {}
            Err(e) => {
                // The .pc may be absent even when the .so is present; fall back to a
                // direct link by name (the lib is on the default search path in the image).
                println!(
                    "cargo:warning=pkg-config gstreamer-cuda-1.0 failed ({e}); \
                     linking -lgstcuda-1.0 directly"
                );
                println!("cargo:rustc-link-lib=gstcuda-1.0");
            }
        }
    }
}

/// Bake the source commit and build time into the binary as `QUASAR_STAMP_*`
/// compile-time env vars, read back by `crate::buildinfo`.
///
/// Order: the `QUASAR_SOURCE_COMMIT` / `QUASAR_BUILT_AT` environment (which is
/// how the image build passes its `SOURCE_COMMIT`/`BUILT_AT` build args — the
/// build context carries no `.git`), then `git rev-parse HEAD` for a developer
/// build in a checkout, then `unknown`.
///
/// `unknown` is never papered over with the Cargo package version: a stale
/// stamp is worse than an absent one, because the control plane would store it
/// as this host's identity and offer platform releases against it.
fn stamp_build_identity() {
    println!("cargo:rerun-if-env-changed=QUASAR_SOURCE_COMMIT");
    println!("cargo:rerun-if-env-changed=QUASAR_BUILT_AT");

    let commit = std::env::var("QUASAR_SOURCE_COMMIT")
        .ok()
        .filter(|v| !v.trim().is_empty() && v.trim() != "unknown")
        .or_else(git_head)
        .unwrap_or_else(|| "unknown".to_string());

    let built_at = std::env::var("QUASAR_BUILT_AT")
        .ok()
        .filter(|v| !v.trim().is_empty() && v.trim() != "unknown")
        .unwrap_or_else(|| "unknown".to_string());

    println!(
        "cargo:rustc-env=QUASAR_STAMP_SOURCE_COMMIT={}",
        commit.trim()
    );
    println!("cargo:rustc-env=QUASAR_STAMP_BUILT_AT={}", built_at.trim());
}

/// `git rev-parse HEAD`, or None when git is absent, this is not a checkout, or
/// the command fails. A build script must never fail the build over provenance.
fn git_head() -> Option<String> {
    let out = std::process::Command::new("git")
        .args(["rev-parse", "HEAD"])
        .output()
        .ok()?;
    if !out.status.success() {
        return None;
    }
    let sha = String::from_utf8(out.stdout).ok()?.trim().to_string();
    if sha.is_empty() {
        None
    } else {
        Some(sha)
    }
}
