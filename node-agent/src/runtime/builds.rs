//! Quasar build requests and bounded, owned context packaging.
use super::*;
mod dockerignore;
use std::collections::BTreeMap;
use std::io::{Read, Seek, SeekFrom};
use std::time::Instant;

#[derive(Clone)]
pub struct BuildRequest {
    pub tag: String,
    pub context_dir: PathBuf,
    pub dockerfile: PathBuf,
    pub build_args: BTreeMap<String, String>,
}

pub(super) struct ContextArchive {
    pub file: tempfile::NamedTempFile,
    pub fingerprint: String,
}

pub(super) fn package(
    request: &BuildRequest,
    deadline: Instant,
) -> Result<ContextArchive, RuntimeError> {
    use std::os::unix::fs::OpenOptionsExt;
    let invalid = || RuntimeError::from(ErrorKind::InvalidBuildContext);
    let df = crate::images::build::sanitize_relative(&request.dockerfile).ok_or_else(invalid)?;
    if df.as_os_str().is_empty() {
        return Err(invalid());
    }
    let meta = std::fs::symlink_metadata(request.context_dir.join(&df)).map_err(|_| invalid())?;
    if !meta.is_file() || meta.file_type().is_symlink() {
        return Err(invalid());
    }
    if !std::fs::symlink_metadata(&request.context_dir)
        .map_err(|_| invalid())?
        .is_dir()
    {
        return Err(invalid());
    }
    let ignore_path = request.context_dir.join(".dockerignore");
    let rules = match std::fs::OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_NOFOLLOW | libc::O_NONBLOCK)
        .open(ignore_path)
    {
        Ok(file) => {
            if !file.metadata().map_err(|_| invalid())?.is_file() {
                return Err(invalid());
            }
            let mut text = String::new();
            file.take(1024 * 1024 + 1)
                .read_to_string(&mut text)
                .map_err(|_| invalid())?;
            if text.len() > 1024 * 1024 {
                return Err(invalid());
            }
            dockerignore::Ignore::parse(&text)?
        }
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => dockerignore::Ignore::parse("")?,
        Err(_) => return Err(invalid()),
    };
    let mut output = tempfile::NamedTempFile::new().map_err(|_| ErrorKind::Unavailable)?;
    let mut tar = tar::Builder::new(output.as_file_mut());
    tar.mode(tar::HeaderMode::Deterministic);
    let mut stack = vec![PathBuf::new()];
    let (mut count, mut size) = (0u64, 0u64);
    while let Some(relative) = stack.pop() {
        if Instant::now() >= deadline {
            return Err(ErrorKind::Timeout.into());
        }
        count += 1;
        if count > crate::images::build::MAX_ENTRIES + 1 {
            return Err(invalid());
        }
        let path = request.context_dir.join(&relative);
        let metadata = std::fs::symlink_metadata(&path).map_err(|_| invalid())?;
        if metadata.file_type().is_symlink() {
            return Err(invalid());
        }
        let name = relative.to_str().ok_or_else(invalid)?;
        let excluded = rules.excluded(name) && relative != df && name != ".dockerignore";
        if metadata.is_dir() {
            if !excluded && !relative.as_os_str().is_empty() {
                tar.append_dir(&relative, &path).map_err(|_| invalid())?;
            }
            let mut entries = Vec::new();
            for entry in std::fs::read_dir(path).map_err(|_| invalid())? {
                if Instant::now() >= deadline {
                    return Err(ErrorKind::Timeout.into());
                }
                if count + stack.len() as u64 + entries.len() as u64
                    >= crate::images::build::MAX_ENTRIES + 1
                {
                    return Err(invalid());
                }
                entries.push(relative.join(entry.map_err(|_| invalid())?.file_name()));
            }
            entries.sort();
            entries.reverse();
            stack.extend(entries);
        } else if metadata.is_file() {
            if excluded {
                continue;
            }
            size = size.checked_add(metadata.len()).ok_or_else(invalid)?;
            if size > crate::images::build::MAX_EXTRACTED_BYTES {
                return Err(invalid());
            }
            let file = std::fs::OpenOptions::new()
                .read(true)
                .custom_flags(libc::O_NOFOLLOW | libc::O_NONBLOCK)
                .open(path)
                .map_err(|_| invalid())?;
            let mut header = tar::Header::new_gnu();
            header.set_metadata_in_mode(&metadata, tar::HeaderMode::Deterministic);
            tar.append_data(
                &mut header,
                &relative,
                DeadlineReader {
                    inner: file.take(metadata.len()),
                    deadline,
                },
            )
            .map_err(|_| invalid())?;
        } else {
            return Err(invalid());
        }
    }
    tar.finish().map_err(|_| invalid())?;
    drop(tar);
    output
        .as_file_mut()
        .seek(SeekFrom::Start(0))
        .map_err(|_| invalid())?;
    use sha2::{Digest, Sha256};
    let mut hash = Sha256::new();
    hash.update(
        serde_json::to_vec(&(&request.tag, &request.dockerfile, &request.build_args))
            .map_err(|_| invalid())?,
    );
    let mut buf = [0u8; 64 * 1024];
    loop {
        if Instant::now() >= deadline {
            return Err(ErrorKind::Timeout.into());
        }
        let n = output.as_file_mut().read(&mut buf).map_err(|_| invalid())?;
        if n == 0 {
            break;
        }
        hash.update(&buf[..n]);
    }
    output
        .as_file_mut()
        .seek(SeekFrom::Start(0))
        .map_err(|_| invalid())?;
    let fingerprint = hash.finalize().iter().map(|b| format!("{b:02x}")).collect();
    Ok(ContextArchive {
        file: output,
        fingerprint,
    })
}
struct DeadlineReader<R> {
    inner: R,
    deadline: Instant,
}
impl<R: Read> Read for DeadlineReader<R> {
    fn read(&mut self, buf: &mut [u8]) -> std::io::Result<usize> {
        if Instant::now() >= self.deadline {
            return Err(std::io::ErrorKind::TimedOut.into());
        }
        self.inner.read(buf)
    }
}

pub(super) fn build_id() -> String {
    static SERIAL: std::sync::atomic::AtomicU64 = std::sync::atomic::AtomicU64::new(0);
    format!(
        "{}-{}-{}",
        std::process::id(),
        std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap_or_default()
            .as_nanos(),
        SERIAL.fetch_add(1, std::sync::atomic::Ordering::Relaxed)
    )
}
