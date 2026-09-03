//! The one-paste enrollment string and the control-plane transport policy (#12).
//!
//! `qenr1.<FINGERPRINT>.<base64url-nopad(wss-url)>.<token>` packs the three things a second
//! host needs to join — where the control plane is, how to recognise it, and the credential
//! to present — into one value the operator copies once, in the spirit of k3s / Tailscale
//! join keys. The fingerprint comes first and verbatim (uppercase colon-separated SHA-256,
//! exactly as the control plane logs it) so the operator can compare it by eye.
//!
//! Why pin rather than trust-on-first-use: the first connection is the one carrying the
//! enrollment token, so TOFU leaves exactly the credential this exists to protect exposed.
//! Why not a shared secret: see `hostenroll` on the control plane — a minted token is
//! per-host, single-use and expiring, so the blob is disposable.
//!
//! Everything here is pure and unit-tested; `config.rs` feeds it the environment.

use std::fmt;

use base64::Engine as _;
use sha2::{Digest, Sha256};

/// Version prefix. A decoder that sees anything else refuses rather than guessing.
pub const BLOB_PREFIX: &str = "qenr1";

/// The persisted pin lives beside the node secret so the blob can be deleted from the
/// environment once enrolled: `<NODE_SECRET_PATH>.tls`.
pub fn pin_path(node_secret_path: &str) -> String {
    format!("{node_secret_path}.tls")
}

/// SHA-256 of the control plane's leaf certificate, DER-encoded.
#[derive(Clone, Copy, PartialEq, Eq)]
pub struct Fingerprint(pub [u8; 32]);

impl Fingerprint {
    /// Accepts the control plane's own form (`AB:CD:…`, uppercase), lowercase, bare 64-hex,
    /// and an optional `sha256:` prefix. Anything else is an error naming the expected shape.
    pub fn parse(raw: &str) -> Result<Self, String> {
        let s = raw.trim();
        let s = s
            .strip_prefix("sha256:")
            .or_else(|| s.strip_prefix("SHA256:"))
            .unwrap_or(s);
        let hex: String = s.chars().filter(|c| *c != ':').collect();
        if hex.len() != 64 || !hex.chars().all(|c| c.is_ascii_hexdigit()) {
            return Err(format!(
                "fingerprint must be a SHA-256 as 32 colon-separated hex pairs (as the control \
                 plane logs it), got {raw:?}"
            ));
        }
        let mut out = [0u8; 32];
        for (i, chunk) in hex.as_bytes().chunks(2).enumerate() {
            out[i] = u8::from_str_radix(std::str::from_utf8(chunk).unwrap(), 16).unwrap();
        }
        Ok(Fingerprint(out))
    }

    pub fn of_der(der: &[u8]) -> Self {
        let sum = Sha256::digest(der);
        let mut out = [0u8; 32];
        out.copy_from_slice(&sum);
        Fingerprint(out)
    }

    /// The control plane's form: uppercase, colon-separated.
    pub fn to_colon_hex(&self) -> String {
        self.0
            .iter()
            .map(|b| format!("{b:02X}"))
            .collect::<Vec<_>>()
            .join(":")
    }
}

impl fmt::Display for Fingerprint {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(&self.to_colon_hex())
    }
}

impl fmt::Debug for Fingerprint {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "Fingerprint({})", self.to_colon_hex())
    }
}

/// The decoded blob.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Blob {
    pub url: String,
    pub fingerprint: Fingerprint,
    pub token: String,
}

/// Strict: unknown version, a non-`wss://` URL, a malformed fingerprint or an empty token
/// are each a distinct error. A token containing `.` survives (max-3 split).
pub fn parse_blob(raw: &str) -> Result<Blob, String> {
    let s = raw.trim();
    let mut parts = s.splitn(4, '.');
    let prefix = parts.next().unwrap_or("");
    if prefix != BLOB_PREFIX {
        return Err(format!(
            "enrollment string must start with `{BLOB_PREFIX}.` (got {:?}); if the control \
             plane is newer than this agent, update the agent",
            prefix.chars().take(16).collect::<String>()
        ));
    }
    let fp = parts
        .next()
        .ok_or("enrollment string is missing its fingerprint segment")?;
    let url_b64 = parts
        .next()
        .ok_or("enrollment string is missing its URL segment")?;
    let token = parts
        .next()
        .ok_or("enrollment string is missing its token segment")?;

    let fingerprint = Fingerprint::parse(fp)?;
    let url_bytes = base64::engine::general_purpose::URL_SAFE_NO_PAD
        .decode(url_b64)
        .map_err(|e| format!("enrollment string URL segment is not base64url: {e}"))?;
    let url = String::from_utf8(url_bytes)
        .map_err(|_| "enrollment string URL segment is not UTF-8".to_string())?;
    if !url.starts_with("wss://") {
        return Err(format!(
            "enrollment string carries a non-TLS control-plane URL ({url:?}); a pin only \
             means something over wss://, so the admin console must be opened over https to \
             mint one"
        ));
    }
    if token.trim().is_empty() {
        return Err("enrollment string has an empty token".into());
    }
    Ok(Blob {
        url,
        fingerprint,
        token: token.to_string(),
    })
}

/// Compose a blob — the agent never does this in production (the admin UI does), but the
/// round-trip is what the tests pin, and a harness minting its own is welcome to use it.
pub fn compose_blob(url: &str, fingerprint: &Fingerprint, token: &str) -> String {
    format!(
        "{BLOB_PREFIX}.{}.{}.{token}",
        fingerprint.to_colon_hex(),
        base64::engine::general_purpose::URL_SAFE_NO_PAD.encode(url.as_bytes())
    )
}

/// How the agent's two control-plane clients (the websocket and the node-secret HTTP
/// polls) verify the peer. Derived once; both clients consume the same value, so they can
/// never disagree about who the control plane is.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum TransportPolicy {
    /// `wss://` with the control plane's self-signed leaf pinned by SHA-256. SAN and
    /// expiry are NOT checked: the pin is the identity, and the deployment's self-signed
    /// cert routinely lacks the LAN name or IP the agent dials.
    Pinned(Fingerprint),
    /// `wss://` verified against the bundled WebPKI roots — a real CA (the Caddy overlay).
    WebPki,
    /// `ws://`, cleartext. Loopback needs no ceremony (the single-host compose default);
    /// anywhere else requires an explicit opt-in, because the enrollment token and the
    /// node secret cross this link as plain JSON.
    Plaintext,
}

/// Everything the environment can say about the transport, already read.
#[derive(Default)]
pub struct Inputs<'a> {
    pub blob: Option<&'a str>,
    pub url: Option<&'a str>,
    pub fingerprint: Option<&'a str>,
    pub token: Option<&'a str>,
    pub persisted_pin: Option<&'a str>,
    pub allow_plaintext: bool,
}

#[derive(Debug, PartialEq, Eq)]
pub struct Resolved {
    pub url: String,
    pub token: Option<String>,
    pub policy: TransportPolicy,
    /// Precedence decisions worth a WARN line — logged by the caller once tracing is up.
    pub warnings: Vec<String>,
}

/// Precedence, stated once:
/// - `CONTROL_PLANE_URL` beats the blob's URL (WARN) — split-horizon deployments dial a
///   different address than the admin console was opened on.
/// - `CONTROL_PLANE_FINGERPRINT` beats the blob's fingerprint (WARN) — this is the
///   certificate-rotation path; the blob is not the rotation vehicle.
/// - `ENROLLMENT_TOKEN` and the blob's token that DIFFER are fatal. A credential ambiguity
///   is never resolved silently.
/// - A persisted pin is used when nothing in the environment names one.
/// - `ws://` to a non-loopback host is fatal without `QUASAR_ALLOW_PLAINTEXT_AGENT=1`.
pub fn resolve(inputs: Inputs<'_>) -> Result<Resolved, String> {
    let mut warnings = Vec::new();
    let blob = match inputs.blob.map(str::trim).filter(|s| !s.is_empty()) {
        Some(raw) => Some(parse_blob(raw)?),
        None => None,
    };

    let url = match (inputs.url.map(str::trim).filter(|s| !s.is_empty()), &blob) {
        (Some(explicit), Some(b)) => {
            if explicit.trim_end_matches('/') != b.url.trim_end_matches('/') {
                warnings.push(format!(
                    "CONTROL_PLANE_URL ({explicit}) overrides the URL inside QUASAR_ENROLLMENT \
                     ({}); the pin inside the enrollment string still applies",
                    b.url
                ));
            }
            explicit.to_string()
        }
        (Some(explicit), None) => explicit.to_string(),
        (None, Some(b)) => b.url.clone(),
        (None, None) => {
            return Err(
                "CONTROL_PLANE_URL is required (e.g. ws://localhost:8080), or set \
                 QUASAR_ENROLLMENT to the enrollment string from the admin console"
                    .into(),
            )
        }
    };

    let token = match (
        inputs.token.map(str::trim).filter(|s| !s.is_empty()),
        blob.as_ref().map(|b| b.token.as_str()),
    ) {
        (Some(explicit), Some(inner)) if explicit != inner => {
            return Err(
                "ENROLLMENT_TOKEN and the token inside QUASAR_ENROLLMENT differ; refusing to \
                 guess which credential to present — unset one of them"
                    .into(),
            )
        }
        (Some(explicit), _) => Some(explicit.to_string()),
        (None, Some(inner)) => Some(inner.to_string()),
        (None, None) => None,
    };

    let env_pin = match inputs.fingerprint.map(str::trim).filter(|s| !s.is_empty()) {
        Some(raw) => Some(Fingerprint::parse(raw)?),
        None => None,
    };
    let persisted_pin = match inputs
        .persisted_pin
        .map(str::trim)
        .filter(|s| !s.is_empty())
    {
        Some(raw) => Some(Fingerprint::parse(raw).map_err(|e| format!("persisted pin file: {e}"))?),
        None => None,
    };
    let pin = match (env_pin, blob.as_ref().map(|b| b.fingerprint), persisted_pin) {
        (Some(env), Some(inner), _) => {
            if env != inner {
                warnings.push(format!(
                    "CONTROL_PLANE_FINGERPRINT ({env}) overrides the fingerprint inside \
                     QUASAR_ENROLLMENT ({inner}) — expected only during a certificate rotation"
                ));
            }
            Some(env)
        }
        (Some(env), None, _) => Some(env),
        (None, Some(inner), _) => Some(inner),
        (None, None, Some(persisted)) => Some(persisted),
        (None, None, None) => None,
    };

    let policy = if url.starts_with("wss://") {
        match pin {
            Some(fp) => TransportPolicy::Pinned(fp),
            None => TransportPolicy::WebPki,
        }
    } else if url.starts_with("ws://") {
        if !is_loopback_ws_url(&url) && !inputs.allow_plaintext {
            return Err(format!(
                "CONTROL_PLANE_URL is cleartext ({url}) to a host that is not this machine: the \
                 enrollment token and the node secret would cross the network as plain JSON. \
                 Use the wss:// enrollment string from the admin console (QUASAR_ENROLLMENT), \
                 or — only on a network you own end to end — set QUASAR_ALLOW_PLAINTEXT_AGENT=1"
            ));
        }
        if pin.is_some() {
            warnings.push(
                "a certificate fingerprint is configured but the control-plane URL is ws://; \
                 the pin does nothing on a cleartext link"
                    .into(),
            );
        }
        TransportPolicy::Plaintext
    } else {
        return Err(format!(
            "CONTROL_PLANE_URL must start with ws:// or wss://, got {url:?}"
        ));
    };

    Ok(Resolved {
        url,
        token,
        policy,
        warnings,
    })
}

/// Loopback by literal only — `localhost`, `127.0.0.0/8`, `::1`. A LAN name that happens to
/// resolve here is not loopback for this purpose: the operator typed a network address.
pub fn is_loopback_ws_url(url: &str) -> bool {
    let rest = url
        .strip_prefix("ws://")
        .or_else(|| url.strip_prefix("wss://"))
        .unwrap_or(url);
    let authority = rest.split(['/', '?', '#']).next().unwrap_or("");
    let authority = authority.rsplit('@').next().unwrap_or(authority);
    let host = if let Some(v6) = authority.strip_prefix('[') {
        v6.split(']').next().unwrap_or("")
    } else {
        authority
            .rsplit_once(':')
            .map(|(h, _)| h)
            .unwrap_or(authority)
    };
    let host = host.to_ascii_lowercase();
    host == "localhost"
        || host == "::1"
        || host
            .parse::<std::net::Ipv4Addr>()
            .map(|ip| ip.is_loopback())
            .unwrap_or(false)
}

/// Truthiness for `QUASAR_ALLOW_PLAINTEXT_AGENT`.
pub fn is_truthy(v: Option<&str>) -> bool {
    matches!(
        v.map(|s| s.trim().to_ascii_lowercase()).as_deref(),
        Some("1") | Some("true") | Some("yes") | Some("on")
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    const FP: &str = "0A:1B:2C:3D:4E:5F:60:71:82:93:A4:B5:C6:D7:E8:F9:0A:1B:2C:3D:4E:5F:60:71:82:93:A4:B5:C6:D7:E8:F9";

    fn fp() -> Fingerprint {
        Fingerprint::parse(FP).unwrap()
    }

    #[test]
    fn fingerprint_accepts_every_operator_facing_form_and_prints_the_control_planes() {
        let canonical = fp();
        for form in [
            FP,
            &FP.to_ascii_lowercase(),
            &FP.replace(':', ""),
            &format!("sha256:{FP}"),
            &format!("  {FP}  "),
        ] {
            assert_eq!(Fingerprint::parse(form).unwrap(), canonical, "{form}");
        }
        assert_eq!(canonical.to_colon_hex(), FP);
        for bad in ["", "abc", &FP[..len_minus(FP, 3)], "zz:zz"] {
            assert!(Fingerprint::parse(bad).is_err(), "{bad:?}");
        }
    }

    fn len_minus(s: &str, n: usize) -> usize {
        s.len() - n
    }

    #[test]
    fn blob_round_trips_and_a_token_with_dots_survives() {
        let token = "abc.def.ghi";
        let s = compose_blob("wss://cp.example:8443", &fp(), token);
        assert!(s.starts_with(&format!("qenr1.{FP}.")), "{s}");
        let b = parse_blob(&s).unwrap();
        assert_eq!(b.url, "wss://cp.example:8443");
        assert_eq!(b.fingerprint, fp());
        assert_eq!(b.token, token);
    }

    #[test]
    fn blob_decoder_is_strict() {
        let good = compose_blob("wss://cp.example", &fp(), "tok");
        assert!(parse_blob(&good.replacen("qenr1", "qenr2", 1))
            .unwrap_err()
            .contains("qenr1"));
        // A cleartext URL inside a blob is exactly the exposure the blob exists to close.
        let ws = compose_blob("ws://cp.example", &fp(), "tok");
        assert!(parse_blob(&ws).unwrap_err().contains("wss://"));
        assert!(parse_blob("qenr1.notafingerprint.d3NzOi8vY3A.tok").is_err());
        assert!(parse_blob("qenr1").unwrap_err().contains("fingerprint"));
        assert!(parse_blob(&format!("qenr1.{FP}.!!!.tok"))
            .unwrap_err()
            .contains("base64url"));
        assert!(parse_blob(&format!("qenr1.{FP}.d3NzOi8vY3A."))
            .unwrap_err()
            .contains("empty token"));
    }

    #[test]
    fn blob_alone_yields_a_pinned_wss_policy() {
        let s = compose_blob("wss://cp.example:8443", &fp(), "tok");
        let r = resolve(Inputs {
            blob: Some(&s),
            ..Default::default()
        })
        .unwrap();
        assert_eq!(r.url, "wss://cp.example:8443");
        assert_eq!(r.token.as_deref(), Some("tok"));
        assert_eq!(r.policy, TransportPolicy::Pinned(fp()));
        assert!(r.warnings.is_empty(), "{:?}", r.warnings);
    }

    #[test]
    fn explicit_url_and_fingerprint_override_the_blob_with_a_warning() {
        let s = compose_blob("wss://cp.example:8443", &fp(), "tok");
        let other = "11:".repeat(31) + "11";
        let r = resolve(Inputs {
            blob: Some(&s),
            url: Some("wss://cp.lan:8443"),
            fingerprint: Some(&other),
            ..Default::default()
        })
        .unwrap();
        assert_eq!(r.url, "wss://cp.lan:8443");
        assert_eq!(
            r.policy,
            TransportPolicy::Pinned(Fingerprint::parse(&other).unwrap())
        );
        assert_eq!(r.warnings.len(), 2, "{:?}", r.warnings);
        assert!(r.warnings[0].contains("CONTROL_PLANE_URL"));
        assert!(r.warnings[1].contains("CONTROL_PLANE_FINGERPRINT"));
    }

    #[test]
    fn a_differing_explicit_token_is_fatal_not_silently_resolved() {
        let s = compose_blob("wss://cp.example", &fp(), "tok-a");
        let err = resolve(Inputs {
            blob: Some(&s),
            token: Some("tok-b"),
            ..Default::default()
        })
        .unwrap_err();
        assert!(err.contains("differ"), "{err}");
        // Identical is fine — the operator pasted both.
        assert!(resolve(Inputs {
            blob: Some(&s),
            token: Some("tok-a"),
            ..Default::default()
        })
        .is_ok());
    }

    #[test]
    fn wss_without_any_pin_is_webpki_and_a_persisted_pin_is_used_when_nothing_else_names_one() {
        let r = resolve(Inputs {
            url: Some("wss://play.example.com"),
            ..Default::default()
        })
        .unwrap();
        assert_eq!(r.policy, TransportPolicy::WebPki);

        let r = resolve(Inputs {
            url: Some("wss://cp.lan:8443"),
            persisted_pin: Some(FP),
            ..Default::default()
        })
        .unwrap();
        assert_eq!(r.policy, TransportPolicy::Pinned(fp()));
    }

    #[test]
    fn cleartext_is_free_to_loopback_and_gated_everywhere_else() {
        for url in [
            "ws://localhost:8080",
            "ws://127.0.0.1:8080",
            "ws://[::1]:8080",
            "ws://127.9.9.9",
        ] {
            let r = resolve(Inputs {
                url: Some(url),
                ..Default::default()
            })
            .unwrap();
            assert_eq!(r.policy, TransportPolicy::Plaintext, "{url}");
        }
        let err = resolve(Inputs {
            url: Some("ws://cp.lan:8080"),
            ..Default::default()
        })
        .unwrap_err();
        assert!(err.contains("QUASAR_ALLOW_PLAINTEXT_AGENT"), "{err}");
        let r = resolve(Inputs {
            url: Some("ws://cp.lan:8080"),
            allow_plaintext: true,
            ..Default::default()
        })
        .unwrap();
        assert_eq!(r.policy, TransportPolicy::Plaintext);
        // A pin on a cleartext link is a misconfiguration worth saying out loud.
        let r = resolve(Inputs {
            url: Some("ws://localhost:8080"),
            fingerprint: Some(FP),
            ..Default::default()
        })
        .unwrap();
        assert!(
            r.warnings.iter().any(|w| w.contains("ws://")),
            "{:?}",
            r.warnings
        );
    }

    #[test]
    fn loopback_is_by_literal_only() {
        assert!(is_loopback_ws_url("ws://localhost:8080/agent/ws"));
        assert!(is_loopback_ws_url("ws://user@127.0.0.1:8080"));
        assert!(is_loopback_ws_url("ws://[::1]:8080"));
        assert!(!is_loopback_ws_url("ws://localhost.lan:8080"));
        assert!(!is_loopback_ws_url("ws://10.0.0.5:8080"));
        assert!(!is_loopback_ws_url("wss://cp.example"));
    }

    #[test]
    fn truthiness() {
        assert!(is_truthy(Some("1")) && is_truthy(Some("true")) && is_truthy(Some(" YES ")));
        assert!(!is_truthy(Some("0")) && !is_truthy(Some("")) && !is_truthy(None));
    }
}
