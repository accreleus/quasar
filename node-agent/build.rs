// Build script: link libgstcuda-1.0 only when the `cuda` feature is on (ZC-02 N-A).
//
// The node-agent is one source that builds for BOTH the AMD/hermes image (VA encode,
// no libgstcuda) and the NVIDIA/quasar-nv image (NVENC zero-copy, libgstcuda present).
// The full-zero-copy N-A path FFI-links libgstcuda-1.0 (gst_cuda_context_new etc.); that
// lib + its pkg-config (`gstreamer-cuda-1.0`) exist only in the quasar-nv image, so the
// link is gated on the `cuda` feature. Without the feature this build script is a no-op
// and the AMD build is unchanged.
fn main() {
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
