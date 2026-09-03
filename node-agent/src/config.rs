use std::env;

use crate::enrollment::{self, TransportPolicy};

pub struct Config {
    /// WebSocket base URL of the control plane; `/agent/ws` is appended automatically.
    pub control_plane_url: String,

    /// Stable identity mapping reconnects to the same DB row. Default: system hostname.
    pub node_name: String,

    /// Pre-shared enrollment token. Required when no node-secret file exists.
    pub enrollment_token: Option<String>,

    /// Per-node secret persisted at first enrollment. Default path is scoped to
    /// NODE_NAME so two agents without an explicit NODE_SECRET_PATH never collide.
    pub node_secret_path: String,

    /// How both control-plane clients verify the peer (#12). Derived once from
    /// `QUASAR_ENROLLMENT` / `CONTROL_PLANE_FINGERPRINT` / the persisted pin / the URL scheme.
    pub transport: TransportPolicy,

    /// Precedence decisions made while resolving the transport, to be logged at WARN once
    /// tracing is up (config is read before the subscriber exists).
    pub startup_warnings: Vec<String>,
}

impl Config {
    pub fn from_env() -> Result<Self, String> {
        let node_name = env::var("NODE_NAME").unwrap_or_else(|_| detect_hostname());
        let node_secret_path = env::var("NODE_SECRET_PATH")
            .unwrap_or_else(|_| format!("/tmp/quasar-{node_name}-secret"));

        // #12: the one-paste enrollment string, the manual pin, the plaintext opt-in, and
        // the pin persisted at first verified connect all feed one resolver. #519's rule
        // that an empty-but-set ENROLLMENT_TOKEN folds to None is preserved by trimming.
        let blob = env::var("QUASAR_ENROLLMENT").ok();
        let url = env::var("CONTROL_PLANE_URL").ok();
        let fingerprint = env::var("CONTROL_PLANE_FINGERPRINT").ok();
        let token = normalize_enrollment_token(env::var("ENROLLMENT_TOKEN").ok());
        let allow_plaintext =
            enrollment::is_truthy(env::var("QUASAR_ALLOW_PLAINTEXT_AGENT").ok().as_deref());
        let persisted_pin = std::fs::read_to_string(enrollment::pin_path(&node_secret_path)).ok();

        let resolved = enrollment::resolve(enrollment::Inputs {
            blob: blob.as_deref(),
            url: url.as_deref(),
            fingerprint: fingerprint.as_deref(),
            token: token.as_deref(),
            persisted_pin: persisted_pin.as_deref(),
            allow_plaintext,
        })?;

        Ok(Config {
            control_plane_url: resolved.url,
            node_name,
            enrollment_token: resolved.token,
            node_secret_path,
            transport: resolved.policy,
            startup_warnings: resolved.warnings,
        })
    }

    /// Where the pin learned at first verified connect is kept (`<NODE_SECRET_PATH>.tls`),
    /// so the enrollment string can be removed from the environment afterwards.
    pub fn pin_path(&self) -> String {
        enrollment::pin_path(&self.node_secret_path)
    }

    pub fn ws_url(&self) -> String {
        let base = self.control_plane_url.trim_end_matches('/');
        format!("{base}/agent/ws")
    }

    /// HTTP base derived from the WS URL (#175 GC pull): scheme swapped, any
    /// `/agent/ws` suffix and trailing slashes stripped.
    pub fn http_base_url(&self) -> String {
        http_base_from_ws(&self.control_plane_url)
    }

    /// Managed-image state path (`image_id -> {registry_ref, version, state}`),
    /// co-located next to `node_secret_path` rather than a new env var.
    pub fn image_state_path(&self) -> String {
        format!("{}.images.json", self.node_secret_path)
    }
}

/// #519: fold an empty/whitespace-only `ENROLLMENT_TOKEN` to `None`, same as unset.
fn normalize_enrollment_token(raw: Option<String>) -> Option<String> {
    raw.map(|t| t.trim().to_string()).filter(|t| !t.is_empty())
}

/// Pure so it's unit-testable without GStreamer libs.
pub(crate) fn http_base_from_ws(url: &str) -> String {
    let mut base = url.trim().trim_end_matches('/').to_string();
    if let Some(stripped) = base.strip_suffix("/agent/ws") {
        base = stripped.trim_end_matches('/').to_string();
    }
    if let Some(rest) = base.strip_prefix("ws://") {
        format!("http://{rest}")
    } else if let Some(rest) = base.strip_prefix("wss://") {
        format!("https://{rest}")
    } else {
        base
    }
}

pub(crate) fn detect_hostname() -> String {
    if let Ok(h) = std::fs::read_to_string("/etc/hostname") {
        let h = h.trim().to_string();
        if !h.is_empty() {
            return h;
        }
    }
    std::process::Command::new("hostname")
        .output()
        .ok()
        .and_then(|o| String::from_utf8(o.stdout).ok())
        .map(|s| s.trim().to_string())
        .filter(|s| !s.is_empty())
        .unwrap_or_else(|| "quasar-node".to_string())
}

#[cfg(test)]
mod tests {
    use super::{http_base_from_ws, normalize_enrollment_token};

    #[test]
    fn normalize_enrollment_token_folds_empty_and_whitespace_to_none() {
        assert_eq!(normalize_enrollment_token(None), None);
        assert_eq!(normalize_enrollment_token(Some(String::new())), None);
        assert_eq!(normalize_enrollment_token(Some("   ".to_string())), None);
        assert_eq!(normalize_enrollment_token(Some("\t\n".to_string())), None);
    }

    #[test]
    fn normalize_enrollment_token_trims_and_keeps_real_tokens() {
        assert_eq!(
            normalize_enrollment_token(Some("  tok-123  ".to_string())),
            Some("tok-123".to_string())
        );
        assert_eq!(
            normalize_enrollment_token(Some("tok-123".to_string())),
            Some("tok-123".to_string())
        );
    }

    #[test]
    fn ws_to_http() {
        assert_eq!(
            http_base_from_ws("ws://localhost:8080"),
            "http://localhost:8080"
        );
    }

    #[test]
    fn wss_to_https() {
        assert_eq!(
            http_base_from_ws("wss://cp.example.com"),
            "https://cp.example.com"
        );
    }

    #[test]
    fn strips_agent_ws_path() {
        assert_eq!(
            http_base_from_ws("ws://localhost:8080/agent/ws"),
            "http://localhost:8080"
        );
    }

    #[test]
    fn strips_trailing_slash() {
        assert_eq!(
            http_base_from_ws("ws://localhost:8080/"),
            "http://localhost:8080"
        );
    }

    #[test]
    fn passthrough_http() {
        assert_eq!(
            http_base_from_ws("http://localhost:8080"),
            "http://localhost:8080"
        );
    }
}
