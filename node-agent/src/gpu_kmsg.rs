//! `host.xid` / `host.gpu_fault` — the kernel's own GPU fault records, tailed off
//! `/dev/kmsg` and reported as facts.
//!
//! Corroboration, not a replacement for `vulkan_fault`'s string inference. The kernel does
//! not know which session a fault belongs to, so an event is emitted to EVERY running
//! session rather than attributed to one.
//!
//! Off the media path by construction: own thread, bounded buffer, non-blocking fd. An
//! unreadable `/dev/kmsg` (the container default — needs the device plus `CAP_SYSLOG`)
//! reports once and does nothing further; the `xid_visibility` readiness check is where an
//! operator learns the capability is off.

use std::io::Read;
use std::os::unix::io::AsRawFd;
use std::time::Duration;

/// The kernel ring buffer. Readable only with the device mapped in and `CAP_SYSLOG`.
pub const KMSG_PATH: &str = "/dev/kmsg";

/// Grep token on every emitted NVIDIA fault line.
pub const XID_LOG_TOKEN: &str = "gpu-xid";

/// Hard bound on carried text: a malformed or attacker-influenced line must not write an
/// unbounded string into a trace payload the control plane stores.
const MAX_TEXT_BYTES: usize = 256;

/// One record per `read()`; a longer record is truncated by the read, never split across
/// events.
const READ_BUF_BYTES: usize = 8192;

/// How often the tailer wakes when the ring is quiet.
const POLL: Duration = Duration::from_millis(500);

/// One GPU fault the kernel reported.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct GpuFault {
    /// `"host.xid"` (NVIDIA) or `"host.gpu_fault"` (everything else).
    pub event: &'static str,
    pub payload: serde_json::Value,
    /// The one-line human form for the ERROR log.
    pub summary: String,
}

/// A parsed `NVRM: Xid` record.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Xid {
    pub code: u32,
    /// PCI address, e.g. `0000:01:00`; the `PCI:`-less spelling parses too. `None` only
    /// when the record carries no address.
    pub pci: Option<String>,
    pub pid: Option<u32>,
    pub name: Option<String>,
    pub text: String,
}

/// Strip `/dev/kmsg`'s `prio,seq,timestamp,flags;` header. A header-less line (pasted
/// `dmesg` output in a test) must still parse, so it is returned unchanged.
fn strip_kmsg_header(line: &str) -> &str {
    match line.split_once(';') {
        // Header only when the part before `;` is the numeric metadata; a message may
        // legitimately contain a semicolon.
        Some((head, rest))
            if !head.is_empty()
                && head
                    .chars()
                    .all(|c| c.is_ascii_digit() || c == ',' || c == '-' || c == '.') =>
        {
            rest
        }
        _ => line,
    }
}

fn bound(text: &str) -> String {
    let text = text.trim();
    if text.len() <= MAX_TEXT_BYTES {
        return text.to_string();
    }
    let mut end = MAX_TEXT_BYTES;
    while end > 0 && !text.is_char_boundary(end) {
        end -= 1;
    }
    format!("{}…", &text[..end])
}

/// Parse one NVIDIA Xid record: `NVRM: Xid (PCI:0000:01:00): 31, pid=1234, name=…`.
/// Driver branches spell the tail differently, so fields after the code are optional and
/// order-independent; only the code matters.
pub fn parse_xid(line: &str) -> Option<Xid> {
    let body = strip_kmsg_header(line);
    let at = body.find("Xid")?;
    // The NVRM marker must precede it, or one of our own log lines mentioning "Xid"
    // manufactures a fault.
    if !body[..at].contains("NVRM") {
        return None;
    }
    let rest = &body[at + 3..];
    let pci = rest
        .split_once('(')
        .and_then(|(_, r)| r.split_once(')'))
        .map(|(inner, _)| inner.trim().trim_start_matches("PCI:").to_string())
        .filter(|s| !s.is_empty());
    // The code is the first integer after the closing paren, or after `Xid` with no address.
    let after = match rest.split_once(')') {
        Some((_, r)) => r,
        None => rest,
    };
    let code: u32 = after
        .split(|c: char| !c.is_ascii_digit())
        .find(|s| !s.is_empty())?
        .parse()
        .ok()?;
    let field = |key: &str| {
        body.split(key).nth(1).map(|v| {
            v.trim()
                .trim_start_matches('=')
                .split(&[',', ' '][..])
                .next()
                .unwrap_or("")
                .to_string()
        })
    };
    Some(Xid {
        code,
        pci,
        pid: field("pid=").and_then(|v| v.parse().ok()),
        name: field("name=").filter(|s| !s.is_empty()),
        text: bound(body),
    })
}

/// Parse an amdgpu ring timeout. Unlike an Xid there is no stable code to key on, so the
/// text is the record.
pub fn parse_amdgpu_fault(line: &str) -> Option<String> {
    let body = strip_kmsg_header(line);
    let lower = body.to_ascii_lowercase();
    if !lower.contains("amdgpu") {
        return None;
    }
    (lower.contains("ring") && lower.contains("timeout")
        || lower.contains("gpu reset")
        || lower.contains("gpu fault"))
    .then(|| bound(body))
}

fn now_unix_ms() -> i64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_millis() as i64)
        .unwrap_or(0)
}

/// Turn one kernel line into a fault, or `None` if it is not one.
pub fn fault_from_line(line: &str) -> Option<GpuFault> {
    if let Some(xid) = parse_xid(line) {
        let summary = format!(
            "{XID_LOG_TOKEN}: NVIDIA driver reported Xid {} on {}{}",
            xid.code,
            xid.pci.as_deref().unwrap_or("an unnamed device"),
            match (&xid.pid, &xid.name) {
                (Some(pid), Some(name)) => format!(" (pid={pid}, name={name})"),
                (Some(pid), None) => format!(" (pid={pid})"),
                _ => String::new(),
            }
        );
        return Some(GpuFault {
            event: "host.xid",
            payload: serde_json::json!({
                "code": xid.code,
                "pci": xid.pci,
                "pid": xid.pid,
                "name": xid.name,
                "text": xid.text,
                "ts_unix_ms": now_unix_ms(),
            }),
            summary,
        });
    }
    parse_amdgpu_fault(line).map(|text| GpuFault {
        summary: format!("{XID_LOG_TOKEN}: amdgpu reported a fault — {text}"),
        event: "host.gpu_fault",
        payload: serde_json::json!({
            "vendor": "amd",
            "text": text,
            "ts_unix_ms": now_unix_ms(),
        }),
    })
}

/// Shared by the readiness check and [`spawn`], so the card and the tailer never disagree.
pub fn kmsg_readable() -> Result<(), std::io::Error> {
    std::fs::File::open(KMSG_PATH).map(|_| ())
}

/// Start the tailer. An unopenable `/dev/kmsg` is the expected container default, not a
/// fault: log once at INFO and return `None`. The thread exits when `tx` closes, tying its
/// lifetime to the connection loop that owns the receiver.
pub fn spawn(
    tx: tokio::sync::mpsc::UnboundedSender<GpuFault>,
) -> Option<std::thread::JoinHandle<()>> {
    let file = match std::fs::File::open(KMSG_PATH) {
        Ok(f) => f,
        Err(e) => {
            tracing::info!(
                "{XID_LOG_TOKEN}: {KMSG_PATH} is not readable ({e}) — GPU Xid / amdgpu fault \
                 records will not be reported. This is the default in a container; see the \
                 `xid_visibility` readiness check for what to add to compose."
            );
            return None;
        }
    };
    // Seek to the END of the ring: reporting the boot-time backlog would attribute an old
    // fault to a live session. Raw fd because `File` exposes neither O_NONBLOCK nor seek here.
    let fd = file.as_raw_fd();
    // SAFETY: `fd` is owned by `file`, which outlives both calls.
    unsafe {
        let flags = libc::fcntl(fd, libc::F_GETFL);
        libc::fcntl(fd, libc::F_SETFL, flags | libc::O_NONBLOCK);
        libc::lseek(fd, 0, libc::SEEK_END);
    }
    Some(std::thread::spawn(move || tail(file, tx)))
}

fn tail(mut file: std::fs::File, tx: tokio::sync::mpsc::UnboundedSender<GpuFault>) {
    let mut buf = vec![0u8; READ_BUF_BYTES];
    loop {
        match file.read(&mut buf) {
            Ok(0) => std::thread::sleep(POLL),
            Ok(n) => {
                let line = String::from_utf8_lossy(&buf[..n]);
                if let Some(fault) = fault_from_line(&line) {
                    tracing::error!(token = "gpu-fault-detected", "{}", fault.summary);
                    if tx.send(fault).is_err() {
                        return; // receiver gone: the connection loop ended
                    }
                }
            }
            // EAGAIN on a quiet ring; EPIPE when the reader fell behind and records were
            // overwritten (documented kmsg behaviour, the next read resumes).
            Err(e) => match e.kind() {
                std::io::ErrorKind::WouldBlock | std::io::ErrorKind::BrokenPipe => {
                    if tx.is_closed() {
                        return;
                    }
                    std::thread::sleep(POLL);
                }
                _ => {
                    tracing::warn!(
                        token = "xid-visibility-unavailable",
                        "{XID_LOG_TOKEN}: {KMSG_PATH} read failed ({e}) — tailer stops"
                    );
                    return;
                }
            },
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_the_xid_shape_the_driver_actually_prints() {
        let line = "6,842,1234567,-;NVRM: Xid (PCI:0000:01:00): 31, pid=1234, name=quasar-node-, Ch 00000008, intr 00000000";
        let x = parse_xid(line).expect("an Xid record");
        assert_eq!(x.code, 31);
        assert_eq!(x.pci.as_deref(), Some("0000:01:00"));
        assert_eq!(x.pid, Some(1234));
        assert_eq!(x.name.as_deref(), Some("quasar-node-"));
        assert!(x.text.starts_with("NVRM: Xid"), "text = {}", x.text);
    }

    #[test]
    fn parses_a_bare_dmesg_line_with_no_kmsg_header() {
        let x = parse_xid("NVRM: Xid (PCI:0000:c1:00): 13, pid=99").expect("an Xid record");
        assert_eq!(x.code, 13);
        assert_eq!(x.pci.as_deref(), Some("0000:c1:00"));
        assert_eq!(x.pid, Some(99));
        assert_eq!(x.name, None);
    }

    #[test]
    fn a_line_mentioning_xid_without_nvrm_is_not_a_fault() {
        assert!(parse_xid("quasar: no Xid seen in this window: 0").is_none());
        assert!(fault_from_line("quasar: no Xid seen in this window: 0").is_none());
    }

    #[test]
    fn an_ordinary_kernel_line_is_not_a_fault() {
        assert!(fault_from_line("6,1,1,-;usb 1-1: new high-speed USB device").is_none());
    }

    #[test]
    fn amdgpu_ring_timeout_maps_to_a_vendor_neutral_gpu_fault() {
        let f = fault_from_line("3,90,1,-;amdgpu: ring gfx_0.0.0 timeout, signaled seq=42")
            .expect("an amdgpu fault");
        assert_eq!(f.event, "host.gpu_fault");
        assert_eq!(f.payload["vendor"], "amd");
        assert!(f.payload["text"].as_str().unwrap().contains("ring"));
    }

    #[test]
    fn the_carried_text_is_bounded() {
        let long = format!("NVRM: Xid (PCI:0000:01:00): 31, {}", "x".repeat(4096));
        let x = parse_xid(&long).expect("an Xid record");
        assert!(x.text.len() <= MAX_TEXT_BYTES + 4, "len = {}", x.text.len());
        assert!(x.text.ends_with('…'));
    }
}
