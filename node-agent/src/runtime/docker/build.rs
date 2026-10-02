//! Classic image builder adapter; SDK options never escape this module.
use super::*;
use crate::runtime::{builds, BuildRequest, ImageInfo, ImageProgress};
use tokio::io::AsyncReadExt;

pub(super) const BUILD_LABEL: &str = "io.quasar.build-operation";

pub(in crate::runtime) async fn build(
    config: &RuntimeConfig,
    request: BuildRequest,
    progress: tokio::sync::watch::Sender<ImageProgress>,
    budget: std::time::Duration,
) -> Result<ImageInfo, RuntimeError> {
    let deadline = std::time::Instant::now() + budget;
    let input = request.clone();
    let archive = tokio::task::spawn_blocking(move || builds::package(&input, deadline))
        .await
        .map_err(|_| ErrorKind::InvalidBuildContext)??;
    let (docker, journal, existing, recovered) = reconcile_image(config, &request.tag).await?;
    if recovered.as_deref() == Some(&archive.fingerprint) {
        return existing.ok_or(ErrorKind::UnknownOutcome.into());
    }
    // Upload/header latency belongs to the build budget, not the read-operation
    // timeout; the executor still enforces the original absolute job deadline.
    let docker = docker.with_timeout(budget);
    let credentials = credentials::load_all(config).await?;
    let build_id = builds::build_id();
    journal.begin(crate::runtime::images::Intent {
        image: request.tag.clone(),
        socket: config.socket.clone(),
        remove_id: None,
        build_id: Some(build_id.clone()),
        build_fingerprint: Some(archive.fingerprint.clone()),
    })?;
    let file = tokio::fs::File::from_std(
        archive
            .file
            .reopen()
            .map_err(|_| ErrorKind::InvalidBuildContext)?,
    );
    let chunks = futures_util::stream::try_unfold(file, |mut file| async move {
        let mut buf = vec![0u8; 64 * 1024];
        let n = file.read(&mut buf).await?;
        if n == 0 {
            return Ok::<_, std::io::Error>(None);
        }
        buf.truncate(n);
        Ok(Some((buf.into(), file)))
    });
    let options = bollard::query_parameters::BuildImageOptions {
        dockerfile: request
            .dockerfile
            .to_str()
            .ok_or(ErrorKind::InvalidBuildContext)?
            .into(),
        t: Some(request.tag.clone()),
        buildargs: Some(request.build_args.into_iter().collect()),
        labels: Some(std::collections::HashMap::from([(
            BUILD_LABEL.into(),
            build_id.clone(),
        )])),
        version: bollard::query_parameters::BuilderVersion::BuilderV1,
        rm: true,
        forcerm: false,
        ..Default::default()
    };
    let mut output = BuildOutput::default();
    output.image = request.tag.clone();
    let mut stream =
        docker.build_image(options, credentials, Some(bollard::body_try_stream(chunks)));
    while let Some(event) = stream.next().await {
        let event = match event {
            Ok(event) => event,
            Err(error) => {
                if let Error::DockerStreamError { error: message }
                | Error::DockerResponseServerError { message, .. } = &error
                {
                    output.push(message, &progress);
                }

                if matches!(error,Error::DockerStreamError {..}|Error::DockerResponseServerError {status_code:400..=407|409..=499,..})
                {
                    journal.clear()?;
                    let mapped = image_error(error);
                    return Err(match mapped.kind {
                        ErrorKind::InsufficientDisk
                        | ErrorKind::RegistryDenied
                        | ErrorKind::ManifestMissing => mapped,
                        _ => ErrorKind::BuildFailed.into(),
                    });
                }
                return Err(ErrorKind::UnknownOutcome.into());
            }
        };
        if event.error_detail.is_some() {
            journal.clear()?;
            return Err(ErrorKind::BuildFailed.into());
        }
        if let Some(text) = event.stream {
            output.push(&text, &progress);
        }
        // Yield even when a noisy stream is continuously ready: deadlines and other
        // runtime operations must still be polled.
        tokio::task::yield_now().await;
    }
    let info = verified(&docker, &request.tag, &build_id)
        .await?
        .ok_or(ErrorKind::UnknownOutcome)?;
    journal.clear()?;
    output.success = true;
    Ok(info)
}

pub(super) async fn verified(
    docker: &Docker,
    image: &str,
    id: &str,
) -> Result<Option<ImageInfo>, RuntimeError> {
    let state = image_state(docker, image)
        .await
        .map_err(|_| ErrorKind::UnknownOutcome)?;
    Ok(state
        .filter(|s| s.build_id.as_deref() == Some(id))
        .map(|s| s.info))
}

fn step(line: &str) -> Option<u8> {
    let fraction = line
        .trim()
        .strip_prefix("Step ")?
        .split_whitespace()
        .next()?;
    let (n, m) = fraction.split_once('/')?;
    let n: u64 = n.parse().ok()?;
    let m: u64 = m.parse().ok()?;
    if m == 0 {
        return None;
    }
    Some(((n.min(m) as f64 / m as f64) * 100.) as u8)
}

/// Retain only a bounded local diagnostic tail and a bounded partial progress line.
/// Dropping on deadline still logs the tail; nothing raw reaches the control plane.
#[derive(Default)]
struct BuildOutput {
    image: String,
    tail: std::collections::VecDeque<u8>,
    line: Vec<u8>,
    truncated: bool,
    success: bool,
}
impl BuildOutput {
    fn push(&mut self, text: &str, progress: &tokio::sync::watch::Sender<ImageProgress>) {
        for byte in text.bytes() {
            if self.tail.len() == 4096 {
                self.tail.pop_front();
            }
            self.tail.push_back(byte);
            if byte == b'\n' {
                if !self.truncated {
                    if let Ok(line) = std::str::from_utf8(&self.line) {
                        if let Some(percent) = step(line) {
                            progress.send_replace(ImageProgress { percent, bytes: 0 });
                        }
                    }
                }
                self.line.clear();
                self.truncated = false;
            } else if self.line.len() < 1024 {
                self.line.push(byte);
            } else {
                self.truncated = true;
            }
        }
    }
}
impl Drop for BuildOutput {
    fn drop(&mut self) {
        if !self.success && !self.tail.is_empty() {
            let bytes: Vec<_> = self.tail.iter().copied().collect();
            tracing::error!(
                token = "image-build-output", image=%self.image,
                "classic build failed or outcome unknown; output tail: {}",
                String::from_utf8_lossy(&bytes)
            );
        }
    }
}
