//! Log spans, the host identity every line carries, and the `token=` convention
//! for WARN/ERROR. Full contract: `.claude/rules/agent-logging.md`; enforced by
//! `tests/log_convention.rs`.
//!
//! The span is thread-local: session-scoped threads clone it (`Span::current()`)
//! and enter it themselves. GStreamer streaming threads never enter it — call
//! sites there keep an explicit `session_id = %…` field.

use std::sync::OnceLock;

/// Stable host identity for logs: the same value the agent registers under
/// (`NODE_NAME`, else the detected hostname). Cached — the detection may shell
/// out, and a log field must never be able to cost a subprocess per line.
pub fn host_name() -> &'static str {
    static HOST: OnceLock<String> = OnceLock::new();
    HOST.get_or_init(|| {
        std::env::var("NODE_NAME")
            .ok()
            .filter(|s| !s.trim().is_empty())
            .unwrap_or_else(crate::config::detect_hostname)
    })
}

/// The span every line belonging to `session_id` is emitted inside. INFO level
/// so it survives the default filter — DEBUG would drop the session id from
/// production logs.
pub fn session_span(session_id: &str) -> tracing::Span {
    tracing::info_span!("session", id = %session_id, host = host_name())
}

/// Install the process-wide subscriber. `QUASAR_LOG_FORMAT=text` (default) is
/// the human format; `json` flattens event fields with open spans under `spans`.
/// Logs go to stderr so a machine-readable subcommand (`probe-encoder --json`)
/// owns stdout.
pub fn init_subscriber() {
    let raw = std::env::var("QUASAR_LOG_FORMAT").unwrap_or_default();
    let want_json = match raw.trim().to_ascii_lowercase().as_str() {
        "" | "text" => false,
        "json" => true,
        _ => {
            // No subscriber yet to warn through — print on the same stream/shape.
            eprintln!(
                "WARN token=\"log-format-unrecognised\" QUASAR_LOG_FORMAT={raw:?} is not text|json; using text"
            );
            false
        }
    };

    let filter = tracing_subscriber::EnvFilter::try_from_default_env()
        .unwrap_or_else(|_| tracing_subscriber::EnvFilter::new("info"));
    // Colour only when a human is watching: ANSI escapes land between a field
    // name and its `=`, which breaks a `grep token=`.
    let ansi = std::io::IsTerminal::is_terminal(&std::io::stderr());
    let builder = tracing_subscriber::fmt()
        .with_writer(std::io::stderr)
        .with_ansi(ansi)
        .with_env_filter(filter);
    if want_json {
        builder.json().flatten_event(true).init();
    } else {
        builder.init();
    }
}
