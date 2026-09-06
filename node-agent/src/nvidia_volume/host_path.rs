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

pub fn resolve(docker: &str, force: bool) -> Option<Result<PathBuf, String>> {
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
        verify_with(Path::new(super::VOLUME_MOUNT), &host, &image, |args| {
            let timeout = if args.first().is_some_and(|arg| arg == "rm") {
                Duration::from_secs(5)
            } else {
                Duration::from_secs(20)
            };
            let output = crate::session::container::output_with_deadline(
                std::process::Command::new(docker).args(args),
                "driver host-path validation", timeout,
            ).map_err(|error| format!("{ENV}: Docker host-bind validation failed: {error}"))?;
            if !output.status.success() {
                let detail = String::from_utf8_lossy(&output.stderr).chars().take(1024).collect::<String>();
                return Err(format!("{ENV}: Docker could not validate the same-directory bind (check the host path, local QUASAR_PULSE_IMAGE and Docker access). No directory is created automatically: {}", detail.trim()));
            }
            Ok(String::from_utf8_lossy(&output.stdout).into_owned())
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

/// The runner is injectable so regression tests can model a Docker host namespace
/// different from the agent's without depending on Docker or process environment.
fn verify_with(
    local: &Path,
    host: &Path,
    image: &str,
    mut run: impl FnMut(&[String]) -> Result<String, String>,
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
    let container = format!("quasar-driver-path-{nonce}");
    let args = vec![
        "run".into(),
        "--rm".into(),
        "--pull=never".into(),
        "--name".into(),
        container.clone(),
        "--network=none".into(),
        "--read-only".into(),
        "--cap-drop=ALL".into(),
        "--security-opt=no-new-privileges".into(),
        "--user=0:0".into(),
        "--entrypoint=/usr/bin/timeout".into(),
        "--mount".into(),
        format!("type=bind,src={},dst={PROBE_DST},readonly", host.display()),
        image.into(),
        "5s".into(),
        "/bin/sh".into(),
        "-c".into(),
        "cat \"$1\"".into(),
        "sh".into(),
        format!("{PROBE_DST}/{name}"),
    ];
    let result = run(&args);
    // The client may time out after Docker starts the child. Clean up our exact
    // generated name on every path; never stop an unrelated container.
    let cleanup = run(&["rm".into(), "-f".into(), container]);
    let observed = result?;
    // --rm can race this idempotent backstop, so a failed rm after a completed
    // child is harmless. A failed run was already returned above.
    let _ = cleanup;
    if observed.trim() != nonce {
        return Err(format!("{ENV} does not point to the same directory mounted at {}; the Docker sibling could not read the agent's fresh marker. App launch is blocked; correct the host path instead of reinstalling drivers.", super::VOLUME_MOUNT));
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

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
    fn explicit_host_path_works_without_hostname_or_container_inspection() {
        let local = tempfile::tempdir().unwrap();
        let host = Path::new("/mnt/user/appdata/driver files");
        let mut calls = 0;
        verify_with(local.path(), host, "quasar-agent:test", |args| {
            calls += 1;
            if args[0] == "rm" { return Ok(String::new()); }
            assert_eq!(args[0], "run");
            assert!(args.iter().any(|arg| arg == "--pull=never"));
            assert!(args.iter().any(|arg| arg == "type=bind,src=/mnt/user/appdata/driver files,dst=/quasar-driver-host-check,readonly"));
            let marker = Path::new(args.last().unwrap()).file_name().unwrap();
            Ok(std::fs::read_to_string(local.path().join(marker)).unwrap())
        }).unwrap();
        assert_eq!(calls, 2);
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
            |_| panic!("must not launch a Docker probe without the local mounted directory"),
        );
        assert!(result.unwrap_err().contains("mounted and writable"));
        assert!(!local.exists());
    }

    #[test]
    fn wrong_host_directory_and_failed_docker_probe_are_not_accepted() {
        let local = tempfile::tempdir().unwrap();
        for response in [
            Ok("old unrelated marker".into()),
            Err("missing host directory".into()),
        ] {
            let mut removed = false;
            let result = verify_with(
                local.path(),
                Path::new("/wrong"),
                "quasar-agent:test",
                |args| {
                    if args[0] == "rm" {
                        removed = true;
                        return Ok(String::new());
                    }
                    response.clone()
                },
            );
            assert!(result.is_err());
            assert!(removed);
            assert_eq!(std::fs::read_dir(local.path()).unwrap().count(), 0);
        }
    }
}
