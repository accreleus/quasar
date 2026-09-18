//! The parent half of the media host probe; the child is `session::probe_media`.
//!
//! The child cannot see `config_update` overlays, so every setting that decides the
//! encoder, render node or CUDA ordinal travels in [`ChildSpec::env`]. The Vulkan
//! per-codec knobs are process env and are inherited.
//!
//! The encode gate must be held for the child's whole life: use [`run`].

use std::time::Duration;

use anyhow::{Context, Result};
use tokio::sync::watch;

use super::child::{run_child, ChildSpec};
use super::outcome::ChildEnd;
use crate::messages::GpuCapacity;
use crate::session::probe_media::MediaProbeRequest;
use crate::session::settings::RuntimeSettings;
use crate::session::warmup::gate::{GateRefusal, WarmupControl};
use crate::session::{SessionConfig, StreamParams};

/// The stream params a media probe asks for: fixed, small, and at the floor codec, so the
/// verdict is about the GPU and not about what a profile happened to configure.
fn probe_stream(request: &MediaProbeRequest) -> StreamParams {
    StreamParams {
        width: request.width,
        height: request.height,
        fps: request.fps,
        ..StreamParams::default()
    }
}

fn args_for(request: &MediaProbeRequest) -> Vec<String> {
    vec![
        "media-probe".into(),
        "--gpu".into(),
        request.gpu.to_string(),
        "--codec".into(),
        request.codec.clone(),
        "--size".into(),
        format!("{}x{}@{}", request.width, request.height, request.fps),
        "--frames".into(),
        request.frames.to_string(),
        "--budget-secs".into(),
        request.budget.as_secs().to_string(),
    ]
}

/// The bound config, as the env the child's `SessionConfig::from_env` reads back.
fn env_for(settings: &RuntimeSettings, cfg: &SessionConfig) -> Vec<(String, String)> {
    // The encoder vocabulary lives in one place (`settings::effective_map`); a second
    // `EncoderChoice → &str` match here could drift from it. `bind_gpu` never changes
    // `cfg.encoder`, so this is the encoder the child will build.
    let encoder = settings
        .effective_map()
        .get("encoder")
        .cloned()
        .unwrap_or_default();
    vec![
        ("QUASAR_ENCODER".into(), encoder),
        ("QUASAR_RENDER_NODE".into(), cfg.render_node.clone()),
        ("QUASAR_CUDA_DEVICE".into(), cfg.cuda_device_id.to_string()),
        ("QUASAR_ZEROCOPY".into(), u8::from(cfg.zerocopy).to_string()),
        ("QUASAR_GOP".into(), cfg.gop.to_string()),
        ("QUASAR_SLICES".into(), cfg.num_slices.to_string()),
        ("QUASAR_TARGET_USAGE".into(), cfg.target_usage.to_string()),
        (
            "QUASAR_BITRATE_KBPS".into(),
            cfg.stream.bitrate_kbps.to_string(),
        ),
    ]
}

/// The child process that probes `gpu_index`, bound through the production
/// [`crate::agent::bind_gpu`]. `Err` means this GPU cannot be probed at all (no render
/// node, vendor/encoder mismatch) — the caller reports it as a failing check.
pub fn child_spec(
    settings: &RuntimeSettings,
    inventory: &[GpuCapacity],
    gpu_index: i32,
    deadline: Duration,
) -> Result<ChildSpec> {
    let request = MediaProbeRequest {
        gpu: gpu_index,
        ..MediaProbeRequest::default()
    };
    let mut cfg = SessionConfig::for_assignment_with(settings, probe_stream(&request), None);
    crate::agent::bind_gpu(inventory, gpu_index, &mut cfg)
        .with_context(|| format!("GPU {gpu_index} cannot run a media probe"))?;
    let program = std::env::current_exe()
        .context("the agent cannot find its own binary to run the media probe")?;
    Ok(ChildSpec {
        program,
        args: args_for(&request),
        env: env_for(settings, &cfg),
        deadline,
    })
}

/// Hold the probe gate for exactly the child's life. `Err(GateRefusal)` means a warm-up
/// (or another probe) holds it and nothing was spawned.
///
/// The compositor creates its Wayland socket under `XDG_RUNTIME_DIR`. The child gets a
/// private one, removed here on every path, so a child killed at its deadline leaves
/// no socket behind in the agent's own runtime dir.
pub async fn run(
    control: Option<&WarmupControl>,
    mut spec: ChildSpec,
    preempt: watch::Receiver<bool>,
) -> Result<ChildEnd, GateRefusal> {
    let _guard = match control {
        Some(control) => Some(control.try_acquire_probe()?),
        None => None,
    };
    let parent = std::env::var_os("XDG_RUNTIME_DIR")
        .map(std::path::PathBuf::from)
        .filter(|p| p.is_dir())
        .unwrap_or_else(std::env::temp_dir);
    let runtime_dir = match tempfile::Builder::new()
        .prefix("quasar-media-probe-")
        .tempdir_in(parent)
    {
        Ok(dir) => dir,
        Err(e) => {
            return Ok(ChildEnd::SpawnFailed(format!(
                "cannot create a runtime directory for the media probe: {e}"
            )))
        }
    };
    spec.env.push((
        "XDG_RUNTIME_DIR".into(),
        runtime_dir.path().to_string_lossy().into_owned(),
    ));
    Ok(run_child(spec, preempt).await)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::session::EncoderChoice;

    fn gpu(index: i32, vendor: &str, render_node: Option<&str>) -> GpuCapacity {
        GpuCapacity {
            index,
            vendor: vendor.into(),
            model: "test".into(),
            vram_mb_total: 8192,
            encode_slots_total: 2,
            render_node: render_node.map(str::to_string),
            device_path: render_node.map(str::to_string),
            driver_identity: None,
        }
    }

    fn settings(encoder: EncoderChoice) -> RuntimeSettings {
        let mut s = RuntimeSettings::baseline();
        s.encoder = encoder;
        // Unpinned: `bind_gpu` adopts the scheduled GPU's node, as a session does.
        s.render_node = String::new();
        s
    }

    fn env_of(spec: &ChildSpec, key: &str) -> Option<String> {
        spec.env
            .iter()
            .find(|(k, _)| k == key)
            .map(|(_, v)| v.clone())
    }

    #[test]
    fn the_chosen_gpus_render_node_and_the_settings_encoder_reach_the_child() {
        let inventory = [
            gpu(0, "amd", Some("/dev/dri/renderD128")),
            gpu(1, "amd", Some("/dev/dri/renderD129")),
        ];
        let spec = child_spec(
            &settings(EncoderChoice::Vulkan),
            &inventory,
            1,
            Duration::from_secs(30),
        )
        .expect("GPU 1 is bindable");

        assert_eq!(spec.program, std::env::current_exe().unwrap());
        assert_eq!(spec.args[0], "media-probe");
        assert!(
            spec.args.windows(2).any(|w| w == ["--gpu", "1"]),
            "{:?}",
            spec.args
        );
        assert_eq!(
            env_of(&spec, "QUASAR_RENDER_NODE").as_deref(),
            Some("/dev/dri/renderD129")
        );
        assert_eq!(env_of(&spec, "QUASAR_ENCODER").as_deref(), Some("vulkan"));
        assert_eq!(spec.deadline, Duration::from_secs(30));
    }

    /// The probe asks for the floor codec at a fixed small size, whatever the host's
    /// stream knobs say.
    #[test]
    fn the_request_travels_on_argv() {
        let inventory = [gpu(0, "amd", Some("/dev/dri/renderD128"))];
        let spec = child_spec(
            &settings(EncoderChoice::Vulkan),
            &inventory,
            0,
            Duration::from_secs(30),
        )
        .unwrap();
        for pair in [
            ["--codec", "h264"],
            ["--size", "1280x720@60"],
            ["--frames", "30"],
            ["--budget-secs", "15"],
        ] {
            assert!(spec.args.windows(2).any(|w| w == pair), "{:?}", spec.args);
        }
    }

    #[test]
    fn a_gpu_absent_from_the_inventory_is_an_error() {
        let inventory = [gpu(0, "amd", Some("/dev/dri/renderD128"))];
        let e = child_spec(
            &settings(EncoderChoice::Vulkan),
            &inventory,
            7,
            Duration::from_secs(30),
        )
        .expect_err("GPU 7 does not exist");
        let text = format!("{e:#}");
        assert!(text.contains("GPU 7 cannot run a media probe"), "{text}");
        assert!(
            text.contains("absent from the agent's latest capacity"),
            "{text}"
        );
    }

    #[test]
    fn a_gpu_with_no_render_node_cannot_be_probed_with_a_hardware_encoder() {
        let inventory = [gpu(0, "nvidia", None)];
        let e = child_spec(
            &settings(EncoderChoice::Vulkan),
            &inventory,
            0,
            Duration::from_secs(30),
        )
        .expect_err("a hardware encoder needs a render node");
        assert!(format!("{e:#}").contains("no reported render node"));
    }

    /// `render_node=software` with a hardware encoder is the other half of the same
    /// fault: a session refuses it, so a probe must not claim to have exercised it.
    #[test]
    fn a_software_render_node_cannot_be_probed_with_a_hardware_encoder() {
        let inventory = [gpu(0, "nvidia", Some("/dev/dri/renderD128"))];
        let mut s = settings(EncoderChoice::Vulkan);
        s.render_node = "software".into();
        let e = child_spec(&s, &inventory, 0, Duration::from_secs(30))
            .expect_err("software render node with a hardware encoder");
        assert!(format!("{e:#}").contains("render_node=software"));
    }

    fn sh(script: &str, deadline: Duration) -> ChildSpec {
        ChildSpec {
            program: "/bin/sh".into(),
            args: vec!["-c".into(), script.into()],
            env: Vec::new(),
            deadline,
        }
    }

    #[tokio::test]
    async fn the_gate_is_held_for_the_whole_run_and_free_afterwards() {
        let control = WarmupControl::new();
        let (_tx, rx) = watch::channel(false);
        let run = run(Some(&control), sh("sleep 0.3", Duration::from_secs(10)), rx);
        let watcher = async {
            tokio::time::sleep(Duration::from_millis(120)).await;
            assert!(
                control.active(),
                "the gate must be held while the child runs"
            );
        };
        let (end, ()) = tokio::join!(run, watcher);
        assert!(matches!(end, Ok(ChildEnd::Exited { code: 0, .. })));
        assert!(!control.active());
    }

    #[tokio::test]
    async fn the_gate_is_released_after_a_deadline_and_after_a_preemption() {
        let control = WarmupControl::new();
        let (_tx, rx) = watch::channel(false);
        let end = run(
            Some(&control),
            sh("sleep 30", Duration::from_millis(200)),
            rx,
        )
        .await;
        assert!(matches!(end, Ok(ChildEnd::Deadline(_))));
        assert!(!control.active());

        let (tx, rx) = watch::channel(true);
        drop(tx);
        let end = run(Some(&control), sh("sleep 30", Duration::from_secs(10)), rx).await;
        assert_eq!(end, Ok(ChildEnd::Preempted));
        assert!(!control.active());
    }

    #[tokio::test]
    async fn a_busy_gate_refuses_without_spawning() {
        let control = WarmupControl::new();
        let held = control.try_acquire_probe().unwrap();
        let dir = tempfile::tempdir().unwrap();
        let marker = dir.path().join("ran");
        let (_tx, rx) = watch::channel(false);
        let end = run(
            Some(&control),
            sh(
                &format!("touch {}", marker.display()),
                Duration::from_secs(10),
            ),
            rx,
        )
        .await;
        assert_eq!(end, Err(GateRefusal::Busy));
        assert!(!marker.exists(), "a refused probe must not spawn a child");
        drop(held);
    }

    #[tokio::test]
    async fn the_child_gets_a_private_runtime_dir_that_is_gone_afterwards_even_when_killed() {
        let out = tempfile::tempdir().unwrap();
        let note = out.path().join("dir");
        let (_tx, rx) = watch::channel(false);
        let script = format!(
            "echo \"$XDG_RUNTIME_DIR\" > {}; touch \"$XDG_RUNTIME_DIR/wayland-1\"; sleep 300",
            note.display()
        );
        let end = tokio::time::timeout(
            Duration::from_secs(20),
            run(None, sh(&script, Duration::from_millis(500)), rx),
        )
        .await
        .unwrap()
        .unwrap();
        assert_eq!(end, ChildEnd::Deadline(Duration::from_millis(500)));

        let dir = std::fs::read_to_string(&note).unwrap();
        let dir = std::path::Path::new(dir.trim());
        assert!(dir
            .file_name()
            .unwrap()
            .to_string_lossy()
            .starts_with("quasar-media-probe-"));
        assert_ne!(
            Some(dir),
            std::env::var_os("XDG_RUNTIME_DIR")
                .as_deref()
                .map(std::path::Path::new)
        );
        assert!(!dir.exists(), "{dir:?} was left behind");
    }
}
