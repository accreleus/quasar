//! Explicit host-path escape hatch. A path that merely exists is insufficient:
//! prove that Docker sees the same backing directory the agent will provision.
use std::fs::OpenOptions;
use std::io::{Read, Write};
use std::path::{Component, Path, PathBuf};
use std::sync::Mutex;
use std::time::{Duration, Instant};

pub const ENV: &str = "QUASAR_NVIDIA_DRIVER_HOST_PATH";
const PROBE_DST: &str = "/quasar-driver-host-check";
type Cached = (String, Instant, Result<PathBuf, String>);
static CACHE: Mutex<Option<Cached>> = Mutex::new(None);

pub fn configured() -> bool {
    std::env::var(ENV).is_ok_and(|value| !value.is_empty())
}

pub fn resolve(force: bool) -> Option<Result<PathBuf, String>> {
    let raw = std::env::var(ENV).ok().filter(|value| !value.is_empty())?;
    let mut cache = match CACHE.lock() {
        Ok(cache) => cache,
        Err(_) => return Some(Err(format!("{ENV}: validation cache is unavailable"))),
    };
    if !force {
        if let Some((key, at, result)) = &*cache {
            if key == &raw && at.elapsed() < Duration::from_secs(60) {
                return Some(result.clone());
            }
        }
    }
    let result = validate_path(&raw).and_then(|host| {
        // The generated Compose already supplies this image for the audio
        // sidecar. It makes explicit mount validation independent of our ID.
        let runtime = crate::session::container::ContainerRuntime::from_env();
        let image = std::env::var("QUASAR_PULSE_IMAGE")
            .ok()
            .filter(|value| !value.trim().is_empty())
            .map(Ok)
            .unwrap_or_else(|| runtime.own_image().map_err(|error| format!(
                "{ENV}: cannot select the validation image ({error}); set QUASAR_PULSE_IMAGE to the locally installed Quasar agent image"
            )))?;
        let api = crate::runtime::configured().map_err(|error| format!("{ENV}: runtime unavailable: {error}"))?;
        api.recover_diagnostics().wait().map_err(|error| format!("{ENV}: previous diagnostic recovery is pending ({error}); check Docker access and retry"))?;
        verify_with(Path::new(super::VOLUME_MOUNT), &host, &image, |helper, run| {
            run_probe(api, helper, run)
        })?;
        Ok(host)
    });
    *cache = Some((raw, Instant::now(), result.clone()));
    Some(result)
}

fn validate_path(raw: &str) -> Result<PathBuf, String> {
    let path = Path::new(raw);
    if !path.is_absolute()
        || path == Path::new("/")
        || path
            .components()
            .any(|part| matches!(part, Component::ParentDir))
        || raw.contains(',')
        || raw.chars().any(char::is_control)
    {
        return Err(format!("{ENV} must be an absolute host directory other than /, with no parent traversal, commas or control characters"));
    }
    Ok(path.to_owned())
}

struct Marker(PathBuf);
impl Drop for Marker {
    fn drop(&mut self) {
        let _ = std::fs::remove_file(&self.0);
    }
}

fn run_probe(
    api: &crate::runtime::RuntimeClient,
    helper: crate::runtime::DiagnosticHelper,
    run: crate::runtime::DiagnosticRun,
) -> Result<crate::runtime::HelperResult, String> {
    let id = api.run_diagnostic(helper, run).wait().map_err(|error| format!("{ENV}: diagnostic launch failed ({error}); check the existing local image, host path and Docker access"))?;
    let observed = api.observe_diagnostic(id.clone()).wait();
    if observed.is_err() {
        // Termination is explicit; an observation error alone does not stop the helper.
        if api.stop_diagnostic(id.clone()).wait().is_ok() {
            let _ = api.observe_diagnostic(id.clone()).wait();
        }
    }
    let cleanup = api.cleanup_diagnostic(id).wait();
    let result = observed.map_err(|error| format!("{ENV}: diagnostic outcome is unknown ({error}); validation is blocked; check Docker access before retrying"))?;
    cleanup.map_err(|error| format!("{ENV}: diagnostic cleanup remains pending ({error}); retry after Docker access is restored"))?;
    Ok(result)
}

/// Local and daemon-host paths may name different directories. The fresh marker
/// is the proof; neither a successful bind nor a zero exit alone is sufficient.
fn verify_with(
    local: &Path,
    host: &Path,
    image: &str,
    mut run: impl FnMut(
        crate::runtime::DiagnosticHelper,
        crate::runtime::DiagnosticRun,
    ) -> Result<crate::runtime::HelperResult, String>,
) -> Result<(), String> {
    use std::os::unix::fs::OpenOptionsExt;
    let mut entropy = [0u8; 24];
    std::fs::File::open("/dev/urandom")
        .and_then(|mut file| file.read_exact(&mut entropy))
        .map_err(|error| format!("{ENV}: cannot create validation nonce: {error}"))?;
    let nonce = entropy
        .iter()
        .map(|byte| format!("{byte:02x}"))
        .collect::<String>();
    let name = format!(".quasar-host-path-{nonce}");
    let mut marker = OpenOptions::new()
        .write(true)
        .create_new(true)
        .mode(0o644)
        .open(local.join(&name))
        .map_err(|error| {
            format!(
                "{ENV}: agent driver directory {} must already be mounted and writable: {error}",
                local.display()
            )
        })?;
    let _cleanup = Marker(local.join(&name));
    marker
        .write_all(nonce.as_bytes())
        .map_err(|error| format!("{ENV}: cannot write validation marker: {error}"))?;
    drop(marker);
    let helper = crate::runtime::DiagnosticHelper {
        operation: format!("host-path-{nonce}"),
        name: format!("quasar-driver-path-{nonce}"),
        image: image.into(),
    };
    let request = crate::runtime::DiagnosticRun {
        entrypoint: vec!["/usr/bin/timeout".into()],
        command: vec![
            "5s".into(),
            "/bin/sh".into(),
            "-c".into(),
            "cat \"$1\"".into(),
            "sh".into(),
            format!("{PROBE_DST}/{name}"),
        ],
        bind: crate::runtime::ReadOnlyHostBind {
            source: host.into(),
            target: PROBE_DST.into(),
        },
    };
    let observed = run(helper, request)?;
    if observed.exit_code != Some(0) {
        return Err(format!("{ENV}: host-bind diagnostic did not complete successfully; check the existing local image, host path and Docker access"));
    }
    if observed.stdout.trim() != nonce {
        return Err(format!("{ENV} does not point to the same directory mounted at {}; the Docker sibling could not read the agent's fresh marker. App launch is blocked; correct the host path instead of reinstalling drivers.", super::VOLUME_MOUNT));
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    #[ignore = "requires explicit local Docker socket, existing image and daemon-host checkout path"]
    fn real_docker_host_path_nonce_validation() {
        let socket = std::env::var("QUASAR_TEST_RUNTIME_SOCKET").expect("set explicit test socket");
        let image = std::env::var("QUASAR_TEST_RUNTIME_IMAGE").expect("set existing image");
        let host_root = PathBuf::from(
            std::env::var("QUASAR_TEST_RUNTIME_HOST_ROOT").expect("set daemon-host checkout path"),
        );
        let local_root = Path::new(env!("CARGO_MANIFEST_DIR")).parent().unwrap();
        let scratch = tempfile::Builder::new()
            .prefix("host-path-acceptance-")
            .tempdir_in(local_root.join(".diagnostics"))
            .unwrap();
        let local = scratch.path().join("driver");
        let wrong = scratch.path().join("wrong");
        std::fs::create_dir(&local).unwrap();
        std::fs::create_dir(&wrong).unwrap();
        let daemon = host_root.join(local.strip_prefix(local_root).unwrap());
        let mut config = crate::runtime::RuntimeConfig::unix(socket);
        config.image_state_path = Some(scratch.path().join("operations"));
        config.diagnostic_owner = Some(format!(
            "host-path-{}",
            scratch.path().file_name().unwrap().to_string_lossy()
        ));
        let api = crate::runtime::RuntimeClient::new(config).unwrap();
        api.recover_diagnostics().wait().unwrap();
        let check = |host: &Path| {
            verify_with(&local, host, &image, |helper, run| {
                run_probe(&api, helper, run)
            })
        };
        let checked = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
            check(&daemon).unwrap();
            assert!(check(&host_root.join(wrong.strip_prefix(local_root).unwrap())).is_err());
            let missing = scratch.path().join("must-not-create");
            assert!(check(&host_root.join(missing.strip_prefix(local_root).unwrap())).is_err());
            assert!(!missing.exists());
            assert_eq!(std::fs::read_dir(&local).unwrap().count(), 0);
        }));
        if let Err(error) = api.recover_diagnostics().wait() {
            let _ = scratch.keep();
            panic!("diagnostic cleanup pending; acceptance state retained: {error}");
        }
        if let Err(panic) = checked {
            std::panic::resume_unwind(panic);
        }
        println!(
            "host-path same-directory nonce, mismatch, absent host bind and marker cleanup passed"
        );
    }

    #[test]
    fn matching_nonce_does_not_override_nonzero_or_unknown_exit() {
        for exit_code in [Some(7), None] {
            let local = tempfile::tempdir().unwrap();
            let result = verify_with(
                local.path(),
                Path::new("/daemon/driver"),
                "quasar-agent:test",
                |_helper, request: crate::runtime::DiagnosticRun| {
                    let marker = Path::new(request.command.last().unwrap())
                        .file_name()
                        .unwrap();
                    Ok(crate::runtime::HelperResult {
                        exit_code,
                        stdout: std::fs::read_to_string(local.path().join(marker)).unwrap(),
                        stderr: String::new(),
                    })
                },
            );
            assert!(
                result.is_err(),
                "matching marker is insufficient without a known successful exit"
            );
            assert_eq!(std::fs::read_dir(local.path()).unwrap().count(), 0);
        }
    }

    #[test]
    fn validates_docker_mount_syntax_without_rejecting_spaces() {
        for bad in [
            "relative",
            "/",
            "/host/../wrong",
            "/host,readonly=false",
            "/host\npath",
        ] {
            assert!(validate_path(bad).is_err(), "{bad:?}");
        }
        assert_eq!(
            validate_path("/mnt/user/appdata/driver files").unwrap(),
            Path::new("/mnt/user/appdata/driver files")
        );
    }

    #[test]
    fn explicit_host_path_preserves_distinct_agent_and_daemon_paths() {
        let local = tempfile::tempdir().unwrap();
        let host = Path::new("/mnt/user/appdata/driver files");
        let mut calls = 0;
        verify_with(
            local.path(),
            host,
            "quasar-agent:test",
            |helper, request| {
                calls += 1;
                assert_eq!(helper.image, "quasar-agent:test");
                assert_eq!(request.bind.source, host);
                assert_eq!(request.bind.target, PROBE_DST);
                assert_eq!(request.entrypoint, ["/usr/bin/timeout"]);
                assert_eq!(request.command[0], "5s");
                let marker = Path::new(request.command.last().unwrap())
                    .file_name()
                    .unwrap();
                Ok(crate::runtime::HelperResult {
                    exit_code: Some(0),
                    stdout: std::fs::read_to_string(local.path().join(marker)).unwrap(),
                    stderr: String::new(),
                })
            },
        )
        .unwrap();
        assert_eq!(calls, 1);
        assert_eq!(std::fs::read_dir(local.path()).unwrap().count(), 0);
    }

    #[test]
    fn missing_agent_mount_is_never_created_by_the_override() {
        let parent = tempfile::tempdir().unwrap();
        let local = parent.path().join("not-mounted");
        let result = verify_with(
            &local,
            Path::new("/host/driver"),
            "quasar-agent:test",
            |_, _| panic!("must not launch a Docker probe without the local mounted directory"),
        );
        assert!(result.unwrap_err().contains("mounted and writable"));
        assert!(!local.exists());
    }

    #[test]
    fn wrong_host_directory_and_failed_docker_probe_are_not_accepted() {
        let local = tempfile::tempdir().unwrap();
        for response in [
            Ok(crate::runtime::HelperResult {
                exit_code: Some(0),
                stdout: "old unrelated marker".into(),
                stderr: String::new(),
            }),
            Err("missing host directory".into()),
        ] {
            let result = verify_with(
                local.path(),
                Path::new("/wrong"),
                "quasar-agent:test",
                |_, _| response.clone(),
            );
            assert!(result.is_err());
            assert_eq!(std::fs::read_dir(local.path()).unwrap().count(), 0);
        }
    }
}
