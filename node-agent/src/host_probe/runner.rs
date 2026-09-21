//! The production [`ProbeRunner`]: one probe target to one bounded run.

use std::path::PathBuf;
use std::sync::{Arc, Mutex};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use tokio::sync::watch;

use crate::messages::GpuCapacity;
use crate::session::settings::RuntimeSettings;
use crate::session::warmup::gate::{GateRefusal, WarmupControl};

use super::app_gpu;
use super::audio;
use super::child::{run_child, ChildSpec};
use super::container::{ContainerProbeEnd, Observed};
use super::media;
use super::orchestrator::{BoxFuture, ProbeRunner, RunEnd};
use super::outcome::{child_outcome, remediation, ChildEnd, ProbeOutcome};
use super::{ProbeKind, ProbeTarget};

const INPUT_DEADLINE: Duration = Duration::from_secs(20);
/// Must exceed the media child's own 15 s encode budget plus GStreamer init.
const MEDIA_DEADLINE: Duration = Duration::from_secs(45);
/// Must exceed the application-GPU probe's own 20 s in-container timeout.
const APPLICATION_GPU_DEADLINE: Duration = Duration::from_secs(30);

/// A fresh identity for one probe run: never minted over an unreconciled one, since the
/// runner never starts a container probe of a kind still waiting on `reconcile`.
fn probe_nonce() -> String {
    format!(
        "{}-{}",
        std::process::id(),
        SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap_or_default()
            .as_nanos()
    )
}

/// `None` when the host is unpinned or pinned to `gpu` itself; `Some(summary)` when a
/// probe there would be a false alarm because the scheduler never places a session on
/// any other GPU.
fn pin_mismatch(settings: &RuntimeSettings, inventory: &[GpuCapacity], gpu: i32) -> Option<String> {
    let gpu_cap = inventory.iter().find(|g| g.index == gpu)?;
    let pinned = &settings.render_node;
    if pinned.is_empty() || pinned == "software" {
        return None;
    }
    let matches_pin = gpu_cap.render_node.as_deref() == Some(pinned.as_str())
        || gpu_cap.device_path.as_deref() == Some(pinned.as_str());
    (!matches_pin)
        .then(|| format!("This host is pinned to {pinned}; sessions are never placed on GPU {gpu}"))
}

/// What a probe run needs from the agent loop. Refreshed before every `registered` /
/// `inputs_observed` send, so a `Start` the orchestrator issues right after always sees
/// current settings and inventory.
pub struct ProbeContext {
    pub settings: RuntimeSettings,
    pub inventory: Vec<GpuCapacity>,
    pub warmup: Option<Arc<WarmupControl>>,
}

/// The seam a test replaces to avoid spawning a real child process, while the
/// production path (`RealExec`) is the actual `run_child`/`media::run` calls a session
/// launch would use. Kept at the exec boundary rather than higher up so the gate
/// discipline in `media::run` (hold across the whole child life) is exercised for real
/// whenever a probe reaches it.
trait ProbeExec: Send + Sync {
    fn run_child(&self, spec: ChildSpec, preempt: watch::Receiver<bool>) -> BoxFuture<ChildEnd>;
    fn run_media(
        &self,
        warmup: Option<Arc<WarmupControl>>,
        spec: ChildSpec,
        preempt: watch::Receiver<bool>,
    ) -> BoxFuture<Result<ChildEnd, GateRefusal>>;
}

struct RealExec;

impl ProbeExec for RealExec {
    fn run_child(&self, spec: ChildSpec, preempt: watch::Receiver<bool>) -> BoxFuture<ChildEnd> {
        Box::pin(run_child(spec, preempt))
    }

    fn run_media(
        &self,
        warmup: Option<Arc<WarmupControl>>,
        spec: ChildSpec,
        preempt: watch::Receiver<bool>,
    ) -> BoxFuture<Result<ChildEnd, GateRefusal>> {
        Box::pin(async move { media::run(warmup.as_deref(), spec, preempt).await })
    }
}

pub struct HostProbeRunner {
    context: Mutex<Option<ProbeContext>>,
    exec: Arc<dyn ProbeExec>,
}

impl Default for HostProbeRunner {
    fn default() -> Self {
        Self::new()
    }
}

impl HostProbeRunner {
    pub fn new() -> Self {
        Self::with_exec(Arc::new(RealExec))
    }

    fn with_exec(exec: Arc<dyn ProbeExec>) -> Self {
        HostProbeRunner {
            context: Mutex::new(None),
            exec,
        }
    }

    pub fn set_context(&self, ctx: ProbeContext) {
        *self.context.lock().unwrap_or_else(|e| e.into_inner()) = Some(ctx);
    }

    fn snapshot(
        &self,
    ) -> Option<(
        RuntimeSettings,
        Vec<GpuCapacity>,
        Option<Arc<WarmupControl>>,
    )> {
        self.context
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .as_ref()
            .map(|c| (c.settings.clone(), c.inventory.clone(), c.warmup.clone()))
    }
}

/// Free functions, not methods: the returned future must be `'static` (it is spawned
/// on its own task), so each takes everything it needs by value rather than borrowing
/// `&HostProbeRunner` across the `.await`.
async fn run_input(exec: Arc<dyn ProbeExec>, preempt: watch::Receiver<bool>) -> RunEnd {
    let program = match std::env::current_exe() {
        Ok(p) => p,
        Err(e) => {
            return RunEnd::Concluded {
                outcome: ProbeOutcome::Indeterminate {
                    reason: format!(
                        "the agent cannot find its own binary to run the input probe: {e}"
                    ),
                },
                reconciled: true,
            };
        }
    };
    run_input_at(exec, program, preempt).await
}

async fn run_input_at(
    exec: Arc<dyn ProbeExec>,
    program: PathBuf,
    preempt: watch::Receiver<bool>,
) -> RunEnd {
    let spec = ChildSpec {
        program,
        args: vec!["input-probe".into()],
        env: Vec::new(),
        deadline: INPUT_DEADLINE,
    };
    let end = exec.run_child(spec, preempt).await;
    RunEnd::Concluded {
        outcome: child_outcome(ProbeTarget::host(ProbeKind::Input), end),
        reconciled: true,
    }
}

async fn run_media(
    exec: Arc<dyn ProbeExec>,
    snapshot: Option<(
        RuntimeSettings,
        Vec<GpuCapacity>,
        Option<Arc<WarmupControl>>,
    )>,
    gpu: i32,
    preempt: watch::Receiver<bool>,
) -> RunEnd {
    let target = ProbeTarget::gpu(ProbeKind::Media, gpu);
    let Some((settings, inventory, warmup)) = snapshot else {
        return RunEnd::Concluded {
            outcome: ProbeOutcome::Indeterminate {
                reason: "The agent has no GPU inventory yet".into(),
            },
            reconciled: true,
        };
    };

    // The host pins one render node: the scheduler never places a session on any
    // other GPU, so a bind failure there would be a false alarm, not evidence.
    if let Some(summary) = pin_mismatch(&settings, &inventory, gpu) {
        return RunEnd::Concluded {
            outcome: ProbeOutcome::NotApplicable { summary },
            reconciled: true,
        };
    }

    let spec = match media::child_spec(&settings, &inventory, gpu, MEDIA_DEADLINE) {
        Ok(spec) => spec,
        Err(e) => {
            return RunEnd::Concluded {
                outcome: ProbeOutcome::Fail {
                    summary: format!("GPU {gpu} cannot be bound for encoding: {e:#}"),
                    remediation: remediation(ProbeKind::Media),
                },
                reconciled: true,
            };
        }
    };

    match exec.run_media(warmup, spec, preempt).await {
        Err(GateRefusal::Busy) | Err(GateRefusal::HostBusy { .. }) => RunEnd::Deferred,
        Ok(end) => RunEnd::Concluded {
            outcome: child_outcome(target, end),
            reconciled: true,
        },
    }
}

/// Everything blocking (`own_image`, `app_gpu_access_live`, `runtime::configured`) runs
/// inside `spawn_blocking`, never on the orchestrator loop.
async fn run_application_gpu(
    snapshot: Option<(
        RuntimeSettings,
        Vec<GpuCapacity>,
        Option<Arc<WarmupControl>>,
    )>,
    gpu: i32,
    preempt: watch::Receiver<bool>,
) -> RunEnd {
    let target = ProbeTarget::gpu(ProbeKind::ApplicationGpu, gpu);
    let Some((settings, inventory, _warmup)) = snapshot else {
        return RunEnd::Concluded {
            outcome: ProbeOutcome::Indeterminate {
                reason: "The agent has no GPU inventory yet".into(),
            },
            reconciled: true,
        };
    };
    let Some(gpu_cap) = inventory.iter().find(|g| g.index == gpu) else {
        return RunEnd::Concluded {
            outcome: ProbeOutcome::Indeterminate {
                reason: format!("GPU {gpu} is absent from the agent's latest capacity report"),
            },
            reconciled: true,
        };
    };
    if let Some(summary) = pin_mismatch(&settings, &inventory, gpu) {
        return RunEnd::Concluded {
            outcome: ProbeOutcome::NotApplicable { summary },
            reconciled: true,
        };
    }
    let Some(device_path) = gpu_cap.device_path.clone() else {
        return RunEnd::Concluded {
            outcome: ProbeOutcome::Indeterminate {
                reason: format!("GPU {gpu} has no reported device path"),
            },
            reconciled: true,
        };
    };

    let watch_preempt = preempt.clone();
    let end = tokio::task::spawn_blocking(move || {
        let is_preempted = || *watch_preempt.borrow();
        let api = crate::runtime::configured().ok()?;
        let runtime = crate::session::container::ContainerRuntime::from_env();
        let access = runtime.app_gpu_access_live();
        let image = match runtime.own_image() {
            Ok(image) => image,
            Err(error) => {
                return Some(ContainerProbeEnd {
                    observed: Observed::RuntimeError(format!(
                        "identifying the agent's own image: {error}"
                    )),
                    reconciled: true,
                });
            }
        };
        let mut command = vec![
            "20s".to_string(),
            "/usr/local/bin/quasar-node-agent".to_string(),
            "egl-selftest".to_string(),
        ];
        if access.carries_driver_volume() {
            command.push(format!(
                "{}/lib64/libEGL_nvidia.so.0",
                crate::nvidia_volume::VOLUME_MOUNT
            ));
        }
        command.push("--open-device".to_string());
        command.push("--render-node".to_string());
        command.push(device_path.clone());
        let gpu_run = access.probe_run(vec!["/usr/bin/timeout".to_string()], command);
        let nonce = probe_nonce();
        Some(app_gpu::run(
            api,
            &image,
            gpu_run,
            &nonce,
            APPLICATION_GPU_DEADLINE,
            &is_preempted,
        ))
    })
    .await;

    match end {
        Ok(Some(end)) => RunEnd::Concluded {
            outcome: app_gpu::outcome(target, &end),
            reconciled: end.reconciled,
        },
        Ok(None) => RunEnd::Concluded {
            outcome: ProbeOutcome::Indeterminate {
                reason: "The agent's container runtime is not configured".into(),
            },
            reconciled: true,
        },
        Err(_) => RunEnd::Concluded {
            outcome: ProbeOutcome::Indeterminate {
                reason: "The host probe task ended unexpectedly".into(),
            },
            reconciled: false,
        },
    }
}

async fn run_audio(preempt: watch::Receiver<bool>) -> RunEnd {
    let watch_preempt = preempt.clone();
    let end = tokio::task::spawn_blocking(move || {
        let is_preempted = || *watch_preempt.borrow();
        let api = crate::runtime::configured().ok()?;
        let runtime = crate::session::container::ContainerRuntime::from_env();
        let image = match crate::session::audio::sidecar_image(&runtime) {
            Ok(image) => image,
            Err(error) => {
                return Some(ContainerProbeEnd {
                    observed: Observed::RuntimeError(format!(
                        "selecting the audio probe image: {error}"
                    )),
                    reconciled: true,
                });
            }
        };
        let runtime_dir = crate::session::default_runtime_dir();
        let nonce = probe_nonce();
        Some(audio::run(api, &image, &runtime_dir, &nonce, &is_preempted))
    })
    .await;

    match end {
        Ok(Some(end)) => RunEnd::Concluded {
            outcome: audio::outcome(&end),
            reconciled: end.reconciled,
        },
        Ok(None) => RunEnd::Concluded {
            outcome: ProbeOutcome::Indeterminate {
                reason: "The agent's container runtime is not configured".into(),
            },
            reconciled: true,
        },
        Err(_) => RunEnd::Concluded {
            outcome: ProbeOutcome::Indeterminate {
                reason: "The host probe task ended unexpectedly".into(),
            },
            reconciled: false,
        },
    }
}

impl ProbeRunner for HostProbeRunner {
    fn run(&self, target: ProbeTarget, preempt: watch::Receiver<bool>) -> BoxFuture<RunEnd> {
        match target.kind {
            ProbeKind::Input => {
                let exec = self.exec.clone();
                Box::pin(run_input(exec, preempt))
            }
            ProbeKind::Media => {
                let gpu = target
                    .gpu
                    .expect("a Media ProbeTarget always carries a GPU index");
                let exec = self.exec.clone();
                let snapshot = self.snapshot();
                Box::pin(run_media(exec, snapshot, gpu, preempt))
            }
            ProbeKind::ApplicationGpu => {
                let gpu = target
                    .gpu
                    .expect("an ApplicationGpu ProbeTarget always carries a GPU index");
                let snapshot = self.snapshot();
                Box::pin(run_application_gpu(snapshot, gpu, preempt))
            }
            ProbeKind::Audio => Box::pin(run_audio(preempt)),
        }
    }

    fn reconcile(&self, kind: ProbeKind) -> BoxFuture<bool> {
        Box::pin(async move {
            tokio::task::spawn_blocking(move || {
                let Ok(api) = crate::runtime::configured() else {
                    return false;
                };
                match kind {
                    ProbeKind::ApplicationGpu => api.recover_diagnostics().wait().is_ok(),
                    ProbeKind::Audio => api.recover_audio_sidecars().wait().is_ok(),
                    ProbeKind::Input | ProbeKind::Media => true,
                }
            })
            .await
            .unwrap_or(false)
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::time::Instant;

    use crate::session::warmup::gate::HostActivity;

    const BOUND: Duration = Duration::from_secs(5);

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

    fn settings() -> RuntimeSettings {
        let mut s = RuntimeSettings::baseline_with(&|_| None);
        s.encoder = crate::session::EncoderChoice::Vulkan;
        s.render_node = String::new();
        s
    }

    fn never() -> watch::Receiver<bool> {
        watch::channel(false).1
    }

    #[derive(Default)]
    struct FakeExec {
        child_calls: Mutex<Vec<ChildSpec>>,
        media_calls: Mutex<Vec<ChildSpec>>,
        result: Mutex<Option<ChildEnd>>,
    }

    impl FakeExec {
        fn returning(end: ChildEnd) -> Self {
            FakeExec {
                result: Mutex::new(Some(end)),
                ..Default::default()
            }
        }
    }

    impl ProbeExec for FakeExec {
        fn run_child(
            &self,
            spec: ChildSpec,
            _preempt: watch::Receiver<bool>,
        ) -> BoxFuture<ChildEnd> {
            self.child_calls.lock().unwrap().push(spec);
            let end = self.result.lock().unwrap().clone().unwrap();
            Box::pin(async move { end })
        }

        fn run_media(
            &self,
            _warmup: Option<Arc<WarmupControl>>,
            spec: ChildSpec,
            _preempt: watch::Receiver<bool>,
        ) -> BoxFuture<Result<ChildEnd, GateRefusal>> {
            self.media_calls.lock().unwrap().push(spec);
            let end = self.result.lock().unwrap().clone().unwrap();
            Box::pin(async move { Ok(end) })
        }
    }

    fn runner_with(exec: FakeExec) -> (HostProbeRunner, Arc<FakeExec>) {
        let exec = Arc::new(exec);
        (HostProbeRunner::with_exec(exec.clone()), exec)
    }

    #[tokio::test]
    async fn input_target_runs_the_agents_own_binary_with_the_input_probe_subcommand() {
        let (runner, exec) = runner_with(FakeExec::returning(ChildEnd::Exited {
            code: 0,
            stdout: "ok".into(),
        }));
        let end = tokio::time::timeout(
            BOUND,
            runner.run(ProbeTarget::host(ProbeKind::Input), never()),
        )
        .await
        .unwrap();
        assert!(matches!(
            end,
            RunEnd::Concluded {
                outcome: ProbeOutcome::Pass { .. },
                reconciled: true
            }
        ));
        let calls = exec.child_calls.lock().unwrap();
        assert_eq!(calls.len(), 1);
        assert_eq!(calls[0].args, vec!["input-probe".to_string()]);
        assert_eq!(calls[0].deadline, INPUT_DEADLINE);
    }

    #[tokio::test]
    async fn media_on_a_gpu_the_host_is_not_pinned_to_is_not_applicable_and_runs_no_child() {
        let (runner, exec) = runner_with(FakeExec::returning(ChildEnd::Exited {
            code: 0,
            stdout: "ok".into(),
        }));
        let mut s = settings();
        s.render_node = "/dev/dri/renderD128".into();
        runner.set_context(ProbeContext {
            settings: s,
            inventory: vec![
                gpu(0, "amd", Some("/dev/dri/renderD128")),
                gpu(1, "amd", Some("/dev/dri/renderD129")),
            ],
            warmup: None,
        });
        let end = tokio::time::timeout(
            BOUND,
            runner.run(ProbeTarget::gpu(ProbeKind::Media, 1), never()),
        )
        .await
        .unwrap();
        match end {
            RunEnd::Concluded {
                outcome: ProbeOutcome::NotApplicable { summary },
                reconciled: true,
            } => {
                assert!(summary.contains("/dev/dri/renderD128"), "{summary}");
                assert!(summary.contains("GPU 1"), "{summary}");
            }
            other => panic!("{other:?}"),
        }
        assert!(exec.media_calls.lock().unwrap().is_empty());
        assert!(exec.child_calls.lock().unwrap().is_empty());
    }

    #[tokio::test]
    async fn media_for_a_gpu_missing_from_the_inventory_fails_and_runs_no_child() {
        let (runner, exec) = runner_with(FakeExec::returning(ChildEnd::Exited {
            code: 0,
            stdout: "ok".into(),
        }));
        runner.set_context(ProbeContext {
            settings: settings(),
            inventory: vec![gpu(0, "amd", Some("/dev/dri/renderD128"))],
            warmup: None,
        });
        let end = tokio::time::timeout(
            BOUND,
            runner.run(ProbeTarget::gpu(ProbeKind::Media, 7), never()),
        )
        .await
        .unwrap();
        match end {
            RunEnd::Concluded {
                outcome: ProbeOutcome::Fail { summary, .. },
                reconciled: true,
            } => assert!(summary.contains("GPU 7"), "{summary}"),
            other => panic!("{other:?}"),
        }
        assert!(exec.media_calls.lock().unwrap().is_empty());
    }

    #[tokio::test]
    async fn media_with_no_context_is_indeterminate() {
        let (runner, _exec) = runner_with(FakeExec::returning(ChildEnd::Exited {
            code: 0,
            stdout: "ok".into(),
        }));
        let end = tokio::time::timeout(
            BOUND,
            runner.run(ProbeTarget::gpu(ProbeKind::Media, 0), never()),
        )
        .await
        .unwrap();
        assert!(matches!(
            end,
            RunEnd::Concluded {
                outcome: ProbeOutcome::Indeterminate { .. },
                ..
            }
        ));
    }

    #[tokio::test]
    async fn a_media_probe_deadline_is_the_real_gate_and_a_held_warmup_defers_it() {
        // No exec substitution: the gate check happens before any child spawn, so this
        // stays safe with the real `media::run` — RealExec.run_media never reaches
        // run_child while the gate refuses.
        let runner = HostProbeRunner::new();
        let control = Arc::new(WarmupControl::new());
        let activity = HostActivity::new();
        let held = control
            .try_acquire(&activity, Duration::ZERO, Instant::now())
            .unwrap();

        runner.set_context(ProbeContext {
            settings: settings(),
            inventory: vec![gpu(0, "amd", Some("/dev/dri/renderD128"))],
            warmup: Some(control.clone()),
        });
        let end = tokio::time::timeout(
            BOUND,
            runner.run(ProbeTarget::gpu(ProbeKind::Media, 0), never()),
        )
        .await
        .unwrap();
        assert_eq!(end, RunEnd::Deferred);
        drop(held);
    }

    #[tokio::test]
    async fn the_media_childs_env_carries_the_gpus_render_node() {
        let (runner, exec) = runner_with(FakeExec::returning(ChildEnd::Exited {
            code: 0,
            stdout: "ok".into(),
        }));
        let mut s = settings();
        s.render_node = String::new();
        runner.set_context(ProbeContext {
            settings: s,
            inventory: vec![
                gpu(0, "amd", Some("/dev/dri/renderD128")),
                gpu(1, "amd", Some("/dev/dri/renderD129")),
            ],
            warmup: None,
        });
        let end = tokio::time::timeout(
            BOUND,
            runner.run(ProbeTarget::gpu(ProbeKind::Media, 1), never()),
        )
        .await
        .unwrap();
        assert!(matches!(
            end,
            RunEnd::Concluded {
                outcome: ProbeOutcome::Pass { .. },
                ..
            }
        ));
        let calls = exec.media_calls.lock().unwrap();
        assert_eq!(calls.len(), 1);
        assert!(calls[0]
            .env
            .iter()
            .any(|(k, v)| k == "QUASAR_RENDER_NODE" && v == "/dev/dri/renderD129"));
    }

    #[tokio::test]
    async fn application_gpu_with_no_context_is_indeterminate_and_never_panics() {
        let (runner, _exec) = runner_with(FakeExec::returning(ChildEnd::Exited {
            code: 0,
            stdout: "ok".into(),
        }));
        let end = tokio::time::timeout(
            BOUND,
            runner.run(ProbeTarget::gpu(ProbeKind::ApplicationGpu, 0), never()),
        )
        .await
        .unwrap();
        assert!(matches!(
            end,
            RunEnd::Concluded {
                outcome: ProbeOutcome::Indeterminate { .. },
                reconciled: true,
            }
        ));
    }

    /// This process has no reachable container runtime, so the probe cannot get past
    /// its own recovery step — never a panic, always an indeterminate verdict.
    #[tokio::test]
    async fn audio_target_never_panics_with_no_reachable_runtime() {
        let (runner, _exec) = runner_with(FakeExec::returning(ChildEnd::Exited {
            code: 0,
            stdout: "ok".into(),
        }));
        let end = tokio::time::timeout(
            BOUND,
            runner.run(ProbeTarget::host(ProbeKind::Audio), never()),
        )
        .await
        .unwrap();
        assert!(matches!(
            end,
            RunEnd::Concluded {
                outcome: ProbeOutcome::Indeterminate { .. },
                ..
            }
        ));
    }

    #[tokio::test]
    async fn reconcile_never_panics_for_every_kind() {
        let runner = HostProbeRunner::new();
        // Child-process kinds have nothing to reconcile and always report done; the
        // container kinds depend on a reachable runtime, which this process has none
        // of — the assertion here is "never panics", not a specific verdict.
        assert!(runner.reconcile(ProbeKind::Input).await);
        assert!(runner.reconcile(ProbeKind::Media).await);
        for kind in [ProbeKind::Audio, ProbeKind::ApplicationGpu] {
            let _ = runner.reconcile(kind).await;
        }
    }
}
