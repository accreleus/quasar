//! What this recovery-actor binary is: stamped at build time by `deploy/Dockerfile.recovery`
//! through `QUASAR_VERSION` and `QUASAR_SOURCE_COMMIT`, as the agent's build stamps.

/// The release version without a leading `v`, or `dev` for a branch build. The control
/// plane stores a value that is not `MAJOR.MINOR.PATCH[-pre]` as unknown (amendment 14).
pub fn version() -> &'static str {
    normalized_version(option_env!("QUASAR_VERSION").unwrap_or(""))
}

/// `v0.5.0` and `0.5.0` read `0.5.0`; empty or `unknown` reads `dev`.
pub fn normalized_version(raw: &str) -> &str {
    let value = raw.trim();
    let value = value.strip_prefix('v').unwrap_or(value);
    if value.is_empty() || value == "unknown" {
        "dev"
    } else {
        value
    }
}

/// The commit this binary was built from: 7-40 lowercase hex, else `unknown`.
pub fn source_commit() -> &'static str {
    normalized_commit(option_env!("QUASAR_SOURCE_COMMIT").unwrap_or(""))
}

fn normalized_commit(raw: &str) -> &str {
    let hex = (7..=40).contains(&raw.len())
        && raw
            .bytes()
            .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b));
    if hex {
        raw
    } else {
        "unknown"
    }
}
