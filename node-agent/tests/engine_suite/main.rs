//! One behavioural suite for every engine mode (RH-07 #408): the runtime's external
//! interface against a real engine, the same assertions for Docker and Podman, rootful and
//! rootless. How to run it, and which modes run where: `docs/testing-engine-suite.md`.
//!
//! Without `QUASAR_ENGINE_SUITE_TARGETS` it checks only its own tables, so `cargo test`
//! stays hermetic. Every container, volume and fixture it creates carries this run's id,
//! and only those are removed.

mod cases;
mod target;

use quasar_node_agent::runtime::{
    ApplicationRequest, ApplicationResult, RuntimeClient, RuntimeConfig, RuntimeError,
};
use quasar_runtime::platform::{ContainerSpec, PlatformContainer, RestartPolicy};
use std::cell::RefCell;
use std::collections::BTreeMap;
use std::os::unix::fs::{MetadataExt, PermissionsExt};
use std::path::{Path, PathBuf};
use std::process::ExitCode;
use std::time::{Duration, Instant};
use target::Target;

/// busybox 1.38.0 (glibc), a multi-arch index: `sh`, `su` and `df`, nothing else needed.
const DEFAULT_IMAGE: &str =
    "docker.io/library/busybox@sha256:dc2d74b28e4cf8984fa52af1f39bc7c3d9c73760b41a74d629f5d11b1ab28616";
const LABEL: &str = "io.quasar.engine-suite";

/// Everything one case may touch on one target, and the record of what it created.
pub struct Ctx {
    pub target: Target,
    pub runtime: RuntimeClient,
    pub run: String,
    pub image: String,
    /// The engine's own user on its host: the socket's owner.
    pub engine_uid: u32,
    dir: PathBuf,
    case: RefCell<&'static str>,
    applications: RefCell<Vec<String>>,
    containers: RefCell<Vec<String>>,
    volumes: RefCell<Vec<String>>,
}

impl Ctx {
    fn stem(&self, what: &str) -> String {
        format!("{}-{}-{what}", self.run, self.case.borrow())
    }

    /// A session application request named and tracked for this case.
    pub fn application(&self, what: &str) -> ApplicationRequest {
        let stem = self.stem(what);
        ApplicationRequest {
            operation: format!("engine-suite-{stem}"),
            name: format!("quasar-sess-suite-{stem}"),
            image: self.image.clone(),
            pull_never: true,
            ..Default::default()
        }
    }

    fn start_application(
        &self,
        request: ApplicationRequest,
    ) -> Result<quasar_node_agent::runtime::ApplicationId, RuntimeError> {
        assert!(request.is_valid(), "invalid request: {request:?}");
        self.applications
            .borrow_mut()
            .push(request.operation.clone());
        self.runtime.start_application(request).wait()
    }

    /// Start, wait for exit, then remove through the runtime; the container must be gone.
    pub fn run_application(&self, request: ApplicationRequest) -> ApplicationResult {
        let id = self
            .start_application(request)
            .expect("the runtime starts it and its read-back passes");
        let result = self
            .runtime
            .observe_application(id.clone())
            .wait()
            .expect("observe to exit");
        self.runtime
            .cleanup_application(id.clone())
            .wait()
            .expect("the runtime removes it");
        assert!(self.inspect(id.as_str()).is_none(), "removed by cleanup");
        result
    }

    pub fn start_application_err(&self, request: ApplicationRequest) -> RuntimeError {
        match self.start_application(request) {
            Ok(id) => panic!("started {} but should have been refused", id.as_str()),
            Err(error) => error,
        }
    }

    /// `Err(id)` when it started; the case's cleanup removes it.
    pub fn start_application_result(
        &self,
        request: ApplicationRequest,
    ) -> Result<RuntimeError, String> {
        match self.start_application(request) {
            Ok(id) => Err(id.as_str().to_string()),
            Err(error) => Ok(error),
        }
    }

    pub fn labels(&self) -> BTreeMap<String, String> {
        BTreeMap::from([(LABEL.to_string(), self.run.clone())])
    }

    pub fn volume_name(&self, what: &str) -> String {
        let name = format!("quasar-engine-suite-{}", self.stem(what));
        self.volumes.borrow_mut().push(name.clone());
        name
    }

    /// A platform-service shape (the recovery actor's lifecycle) running `script`.
    pub fn service(&self, what: &str, script: &str) -> ContainerSpec {
        let name = format!("quasar-engine-suite-{}", self.stem(what));
        self.containers.borrow_mut().push(name.clone());
        ContainerSpec {
            name,
            image: self.image.clone(),
            entrypoint: Some(vec!["/bin/sh".into(), "-c".into()]),
            cmd: Some(vec![script.into()]),
            env: BTreeMap::new(),
            labels: self.labels(),
            network_mode: Some("none".into()),
            binds: Vec::new(),
            devices: Vec::new(),
            device_cgroup_rules: Vec::new(),
            gpus: Vec::new(),
            cap_add: Vec::new(),
            security_opt: Vec::new(),
            init: true,
            restart: RestartPolicy::No,
            ports: Vec::new(),
            healthcheck: None,
        }
    }

    pub fn create(&self, spec: ContainerSpec) -> String {
        self.runtime
            .create_container(spec)
            .wait()
            .expect("the engine answers")
            .unwrap_or_else(|refused| panic!("create refused: {refused:?}"))
    }

    pub fn start(&self, id: &str) {
        self.runtime
            .start_container(id)
            .wait()
            .expect("the engine answers")
            .unwrap_or_else(|refused| panic!("start refused: {refused:?}"));
    }

    pub fn inspect(&self, name_or_id: &str) -> Option<PlatformContainer> {
        self.runtime
            .inspect_platform_container(name_or_id)
            .wait()
            .expect("inspect")
    }

    /// A path under this run's host-backed directory, not created.
    pub fn fixture_path(&self, what: &str) -> PathBuf {
        self.dir.join(self.stem(what))
    }

    /// A fresh directory any mapped id may write to: this run's own fixture only.
    pub fn fixture_dir(&self, what: &str) -> PathBuf {
        let path = self.fixture_path(what);
        std::fs::create_dir(&path).expect("create the fixture directory");
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o777)).unwrap();
        path
    }

    /// Remove what this case created. Everything is attempted; what could not be proven
    /// gone is returned.
    fn clean_case(&self) -> Vec<String> {
        let mut left = Vec::new();
        for operation in self.applications.borrow_mut().drain(..) {
            if let Err(e) = self.runtime.abandon_application(operation.clone()).wait() {
                left.push(format!("application {operation}: {e}"));
            }
        }
        for name in self.containers.borrow_mut().drain(..) {
            if let Err(e) = self.runtime.remove_container(name.clone()).wait() {
                left.push(format!("container {name}: {e}"));
            }
        }
        for name in self.volumes.borrow_mut().drain(..) {
            if let Err(e) = self.runtime.remove_volume(name.clone()).wait() {
                left.push(format!("volume {name}: {e}"));
            }
        }
        left
    }

    /// Anything labelled with this run that survived its case.
    fn leftovers(&self) -> Vec<String> {
        match self.runtime.platform_containers().wait() {
            Ok(all) => all
                .into_iter()
                .filter(|c| c.labels.get(LABEL) == Some(&self.run))
                .map(|c| {
                    let _ = self.runtime.remove_container(c.id.clone()).wait();
                    format!("container {}", c.name)
                })
                .collect(),
            Err(e) => vec![format!("cannot list containers: {e}")],
        }
    }
}

enum Verdict {
    Pass(String),
    Skip(String),
    /// Failed on a finding recorded for this engine mode.
    Known(String),
    Fail(String),
}

fn report(target: &str, case: &str, verdict: &Verdict, took: Duration) {
    let (word, detail) = match verdict {
        Verdict::Pass(d) => ("PASS", d),
        Verdict::Skip(d) => ("SKIP", d),
        Verdict::Known(d) => ("KNOWN", d),
        Verdict::Fail(d) => ("FAIL", d),
    };
    println!(
        "engine-suite {target:<16} {case:<19} {word}  {detail} ({:.1}s)",
        took.as_secs_f64()
    );
}

static PANIC_AT: std::sync::Mutex<Option<String>> = std::sync::Mutex::new(None);

fn panic_text(payload: Box<dyn std::any::Any + Send>) -> String {
    let text = payload
        .downcast_ref::<String>()
        .cloned()
        .or_else(|| payload.downcast_ref::<&str>().map(|s| s.to_string()))
        .unwrap_or_else(|| "panicked".into());
    match PANIC_AT.lock().unwrap().take() {
        Some(at) => format!("{text} [{at}]"),
        None => text,
    }
}

/// Prepare one target: the fixture image present (pulled through the runtime when it was
/// not), and whether this run pulled it.
fn prepare(target: &Target, run: &str, root: &Path, image: &str) -> Result<(Ctx, bool), String> {
    let dir = root.join(&target.mode.name);
    std::fs::create_dir(&dir).map_err(|e| format!("{}: {e}", dir.display()))?;
    std::fs::set_permissions(&dir, std::fs::Permissions::from_mode(0o755)).unwrap();
    let engine_uid = std::fs::metadata(&target.socket)
        .map_err(|e| format!("socket {}: {e}", target.socket.display()))?
        .uid();
    let mut config = RuntimeConfig::unix(&target.socket);
    config.image_state_path = Some(dir.join("operations"));
    config.deadline = Duration::from_secs(30);
    let runtime = RuntimeClient::new(config).map_err(|e| e.to_string())?;
    let present = runtime
        .image_present(image)
        .wait()
        .map_err(|e| format!("engine: {e}"))?;
    if !present {
        runtime
            .ensure_image(image, Duration::from_secs(300))
            .wait(|_| {})
            .map_err(|e| format!("pull {image}: {e}"))?;
    }
    Ok((
        Ctx {
            target: target.clone(),
            runtime,
            run: run.to_string(),
            image: image.to_string(),
            engine_uid,
            dir,
            case: RefCell::new(""),
            applications: RefCell::default(),
            containers: RefCell::default(),
            volumes: RefCell::default(),
        },
        !present,
    ))
}

fn main() -> ExitCode {
    let args: Vec<String> = std::env::args().skip(1).collect();
    if args.iter().any(|a| a == "--list") {
        for case in cases::CASES {
            println!("{}: test", case.name);
        }
        return ExitCode::SUCCESS;
    }
    // libtest flags cargo may forward are ignored; a bare word filters cases by name.
    let filters: Vec<&String> = args.iter().filter(|a| !a.starts_with('-')).collect();

    if let Err(e) = target::self_check() {
        eprintln!("engine-suite: its own tables are wrong: {e}");
        return ExitCode::FAILURE;
    }
    let Some(targets) = std::env::var("QUASAR_ENGINE_SUITE_TARGETS").ok() else {
        println!(
            "engine-suite: tables checked; no engine targeted (set QUASAR_ENGINE_SUITE_TARGETS, \
             see docs/testing-engine-suite.md)"
        );
        return ExitCode::SUCCESS;
    };
    let modes = target::modes().expect("checked above");
    let lacks = std::env::var("QUASAR_ENGINE_SUITE_LACKS").ok();
    let case_names: Vec<&str> = cases::CASES.iter().map(|c| c.name).collect();
    let known_spec = std::env::var("QUASAR_ENGINE_SUITE_KNOWN").unwrap_or_default();
    let targets = match target::from_environment(&modes, &targets, lacks.as_deref())
        .and_then(|mut t| target::known_failures(&mut t, &case_names, &known_spec).map(|()| t))
    {
        Ok(t) => t,
        Err(e) => {
            eprintln!("engine-suite: {e}");
            return ExitCode::FAILURE;
        }
    };
    let image =
        std::env::var("QUASAR_ENGINE_SUITE_IMAGE").unwrap_or_else(|_| DEFAULT_IMAGE.to_string());
    let run = format!(
        "{:x}{:x}",
        std::process::id(),
        std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap()
            .as_millis()
    );
    let parent = std::env::var_os("QUASAR_ENGINE_SUITE_STATE_DIR")
        .map(PathBuf::from)
        .unwrap_or_else(std::env::temp_dir);
    let root = match tempfile::Builder::new()
        .prefix(&format!("engine-suite-{run}-"))
        .tempdir_in(&parent)
    {
        Ok(dir) => dir,
        Err(e) => {
            eprintln!(
                "engine-suite: state directory under {}: {e}",
                parent.display()
            );
            return ExitCode::FAILURE;
        }
    };
    std::fs::set_permissions(root.path(), std::fs::Permissions::from_mode(0o755)).unwrap();
    // The runtime's own warnings (a refused read-back names the field) explain a FAIL.
    let _ = tracing_subscriber::fmt()
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_env("QUASAR_ENGINE_SUITE_LOG")
                .unwrap_or_else(|_| tracing_subscriber::EnvFilter::new("warn")),
        )
        .with_writer(std::io::stderr)
        .try_init();
    // The runtime's ownership lease, fresh per run: this run owns only what it creates.
    std::env::set_var("NODE_SECRET_PATH", root.path().join("node-secret"));
    // A failed assertion is reported on its case's line, with where it was raised.
    std::panic::set_hook(Box::new(|info| {
        *PANIC_AT.lock().unwrap() = info
            .location()
            .map(|l| format!("{}:{}", l.file(), l.line()));
    }));

    println!("engine-suite run {run}, image {image}");
    let (mut passed, mut skipped, mut known, mut failed) = (0, 0, 0, 0);
    let mut keep_state = false;
    for target in &targets {
        let name = target.mode.name.as_str();
        for (capability, reason) in &target.lacks {
            println!(
                "engine-suite {name:<16} lacks {}: {reason}",
                capability.name()
            );
        }
        let started = Instant::now();
        let (ctx, pulled) = match prepare(target, &run, root.path(), &image) {
            Ok(v) => v,
            Err(e) => {
                report(name, "prepare", &Verdict::Fail(e), started.elapsed());
                failed += 1;
                continue;
            }
        };
        for case in cases::CASES {
            if !filters.is_empty() && !filters.iter().any(|f| case.name.contains(f.as_str())) {
                continue;
            }
            let started = Instant::now();
            let lacking = case
                .needs
                .iter()
                .find_map(|c| target.lacks.get(c).map(|r| (c, r)));
            let verdict = if let Some((capability, reason)) = lacking {
                Verdict::Skip(format!("target lacks {}: {reason}", capability.name()))
            } else {
                *ctx.case.borrow_mut() = case.name;
                let outcome =
                    std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| (case.run)(&ctx)));
                let left = ctx.clean_case();
                let verdict = match (outcome, left.is_empty()) {
                    (Ok(detail), true) => Verdict::Pass(detail),
                    (Ok(_), false) => Verdict::Fail(format!("left behind: {left:?}")),
                    (Err(payload), _) => {
                        let mut text = panic_text(payload);
                        if !left.is_empty() {
                            text.push_str(&format!("; left behind: {left:?}"));
                        }
                        Verdict::Fail(text)
                    }
                };
                match (verdict, target.known.get(case.name)) {
                    (Verdict::Fail(text), Some(finding)) => {
                        Verdict::Known(format!("{finding} (this run: {text})"))
                    }
                    (Verdict::Pass(_), Some(finding)) => Verdict::Fail(format!(
                        "passed, but is recorded as failing ({finding}): drop it from \
                         QUASAR_ENGINE_SUITE_KNOWN"
                    )),
                    (verdict, _) => verdict,
                }
            };
            match verdict {
                Verdict::Pass(_) => passed += 1,
                Verdict::Skip(_) => skipped += 1,
                Verdict::Known(_) => known += 1,
                Verdict::Fail(_) => failed += 1,
            }
            report(name, case.name, &verdict, started.elapsed());
        }
        let left = ctx.leftovers();
        if !left.is_empty() {
            report(
                name,
                "leftovers",
                &Verdict::Fail(format!("removed late: {left:?}")),
                Duration::ZERO,
            );
            failed += 1;
        }
        if pulled {
            if let Err(e) = ctx
                .runtime
                .remove_image(&image, Duration::from_secs(60))
                .wait()
            {
                println!("engine-suite {name:<16} the image this run pulled stays: {e}");
            }
        }
        let journals = ctx.dir.join("operations");
        if ctx.runtime.recover_application_cleanup().wait().is_err() {
            keep_state = true;
            println!(
                "engine-suite {name:<16} application cleanup unproven; journals kept at {}",
                journals.display()
            );
        }
    }
    let names: Vec<&str> = targets.iter().map(|t| t.mode.name.as_str()).collect();
    println!(
        "RESULT engine-suite: {passed} passed, {skipped} skipped, {known} known, {failed} failed \
         (targets: {})",
        names.join(", ")
    );
    if keep_state {
        let _ = root.keep();
    }
    if failed == 0 {
        ExitCode::SUCCESS
    } else {
        ExitCode::FAILURE
    }
}
