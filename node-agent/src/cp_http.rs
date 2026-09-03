//! The node-secret HTTP client for the control plane's agent pull channels (#12):
//! `GET/POST /v1/agent/*` for GC, jobs and library scans.
//!
//! One client, built once from the same [`TransportPolicy`] as the websocket, so the two
//! can never disagree about who the control plane is. Under a pin this supplies its own
//! TLS connector: ureq 3 exposes no custom-verifier hook (`TlsConfig` offers root
//! certificates and `disable_verification` only, and its root path enforces SANs — which
//! would make the HTTP polls fail on exactly the LAN-IP URLs the pinned websocket
//! accepts). The connector mirrors ureq's own `RustlsConnector` minus its config source.
//!
//! The external artifact downloads (`artifact.rs`, image builds) are NOT this client: they
//! talk to public hosts and keep ureq's default WebPKI verification.

use std::io::{Read, Write};
use std::sync::Arc;
use std::time::Duration;

use rustls::pki_types::ServerName;
use rustls::{ClientConnection, StreamOwned};
use serde::de::DeserializeOwned;
use ureq::unversioned::resolver::DefaultResolver;
use ureq::unversioned::transport::{
    Buffers, ConnectionDetails, Connector, Either, LazyBuffers, NextTimeout, TcpConnector,
    Transport, TransportAdapter,
};

use crate::enrollment::TransportPolicy;

/// Per-request budget shared by every pull channel. Consolidates the three separate 15 s
/// constants that GC, jobs and library-scan each carried, raised to 20 s.
pub const HTTP_TIMEOUT: Duration = Duration::from_secs(20);

/// A TLS connector that uses OUR `rustls::ClientConfig` — pinned or WebPKI — instead of
/// the one ureq builds from `TlsConfig`.
struct PolicyTlsConnector {
    config: Arc<rustls::ClientConfig>,
}

impl<In: Transport> Connector<In> for PolicyTlsConnector {
    type Out = Either<In, PolicyTlsTransport>;

    fn connect(
        &self,
        details: &ConnectionDetails,
        chained: Option<In>,
    ) -> Result<Option<Self::Out>, ureq::Error> {
        let Some(transport) = chained else {
            return Err(ureq::Error::Tls(
                "policy TLS connector needs a chained TCP transport",
            ));
        };
        // Fail closed. This connector is only installed under a TLS policy, so a plain
        // request reaching it means the base URL and the policy disagree and the node
        // secret is one `Ok` away from crossing the wire in cleartext. `CpClient::new`
        // rejects that mismatch at construction; this is the second gate, not the only one.
        if !details.needs_tls() {
            return Err(ureq::Error::Tls(
                "control-plane URL is not https:// but the transport policy requires TLS",
            ));
        }
        if transport.is_tls() {
            return Ok(Some(Either::A(transport)));
        }
        // `ServerName` from the bare host: a DNS name OR an IP literal (rustls-pki-types 1.x
        // parses both). Under a pin the name is not verified against the certificate at all;
        // it only selects SNI, which rustls omits for IP literals.
        let host = details
            .uri
            .authority()
            .ok_or(ureq::Error::Tls("control-plane URL has no authority"))?
            .host();
        // `http`'s `Authority::host()` keeps the brackets on an IPv6 literal; `ServerName`
        // wants them off.
        let host = host
            .strip_prefix('[')
            .and_then(|h| h.strip_suffix(']'))
            .unwrap_or(host);
        let name: ServerName<'_> = host
            .try_into()
            .map_err(|_| ureq::Error::Tls("control-plane host is not a valid server name"))?;
        let conn = ClientConnection::new(self.config.clone(), name.to_owned())?;
        let stream = StreamOwned {
            conn,
            sock: TransportAdapter::new(transport.boxed()),
        };
        let buffers = LazyBuffers::new(
            details.config.input_buffer_size(),
            details.config.output_buffer_size(),
        );
        Ok(Some(Either::B(PolicyTlsTransport { buffers, stream })))
    }
}

impl std::fmt::Debug for PolicyTlsConnector {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("PolicyTlsConnector")
    }
}

/// Byte-for-byte the shape of ureq's `RustlsTransport`, which is not exported.
pub struct PolicyTlsTransport {
    buffers: LazyBuffers,
    stream: StreamOwned<ClientConnection, TransportAdapter>,
}

impl Transport for PolicyTlsTransport {
    fn buffers(&mut self) -> &mut dyn Buffers {
        &mut self.buffers
    }

    fn transmit_output(&mut self, amount: usize, timeout: NextTimeout) -> Result<(), ureq::Error> {
        self.stream.get_mut().set_timeout(timeout);
        let output = &self.buffers.output()[..amount];
        self.stream.write_all(output)?;
        Ok(())
    }

    fn await_input(&mut self, timeout: NextTimeout) -> Result<bool, ureq::Error> {
        self.stream.get_mut().set_timeout(timeout);
        let input = self.buffers.input_append_buf();
        let amount = self.stream.read(input)?;
        self.buffers.input_appended(amount);
        Ok(amount > 0)
    }

    fn is_open(&mut self) -> bool {
        self.stream.get_mut().get_mut().is_open()
    }

    fn is_tls(&self) -> bool {
        true
    }
}

impl std::fmt::Debug for PolicyTlsTransport {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("PolicyTlsTransport")
    }
}

/// The control-plane HTTP client: base URL, node identity, bearer secret, and an agent
/// whose TLS verification is the transport policy's.
#[derive(Clone)]
pub struct CpClient {
    agent: ureq::Agent,
    http_base: String,
    node_name: String,
    node_secret: String,
}

impl CpClient {
    /// Errors when the base URL's scheme contradicts the policy. The two are derived from
    /// the same resolved `CONTROL_PLANE_URL`, so a disagreement is a bug — but a silent
    /// one would either send the node secret in cleartext or skip the pin, so it is
    /// refused rather than repaired.
    pub fn new(
        policy: &TransportPolicy,
        http_base: String,
        node_name: String,
        node_secret: String,
    ) -> Result<Self, String> {
        let https = http_base.starts_with("https://");
        match (policy, https) {
            (TransportPolicy::Pinned(_) | TransportPolicy::WebPki, false) => {
                return Err(format!(
                    "transport policy {policy:?} requires https:// but the control-plane HTTP \
                     base is {http_base:?}"
                ))
            }
            (TransportPolicy::Plaintext, true) => {
                return Err(format!(
                    "control-plane HTTP base {http_base:?} is https:// but the transport policy \
                     is Plaintext; no certificate would be verified"
                ))
            }
            _ => {}
        }
        let config = ureq::Agent::config_builder()
            .timeout_global(Some(HTTP_TIMEOUT))
            .build();
        let agent = match crate::cp_tls::client_config(policy) {
            // Our verifier for both pinned and WebPKI: one code path, one answer.
            // This chain REPLACES ureq's default connector stack, which means its SOCKS
            // and HTTP-CONNECT proxy connectors are gone: the agent's control-plane link
            // ignores `*_proxy` by construction. Restoring proxy support means chaining
            // them back in ahead of this one, not swapping the TLS layer out.
            Some(tls) => ureq::Agent::with_parts(
                config,
                TcpConnector::default().chain(PolicyTlsConnector { config: tls }),
                DefaultResolver::default(),
            ),
            // Plaintext: the URL is http://, so TLS never enters the picture.
            None => ureq::Agent::new_with_config(config),
        };
        Ok(Self {
            agent,
            http_base: http_base.trim_end_matches('/').to_string(),
            node_name,
            node_secret,
        })
    }

    pub fn http_base(&self) -> &str {
        &self.http_base
    }

    pub fn node_name(&self) -> &str {
        &self.node_name
    }

    fn url(&self, path: &str) -> String {
        format!("{}{}", self.http_base, path)
    }

    /// `GET {base}{path}` with the node-secret bearer, decoded as JSON.
    pub fn get_json<T: DeserializeOwned>(&self, path: &str) -> Result<T, String> {
        let mut resp = self
            .agent
            .get(&self.url(path))
            .header("Authorization", &format!("Bearer {}", self.node_secret))
            .header("X-Quasar-Node", &self.node_name)
            .call()
            .map_err(|e| format!("GET {path}: {e}"))?;
        resp.body_mut()
            .read_json()
            .map_err(|e| format!("decode {path}: {e}"))
    }

    /// `POST {base}{path}` with a JSON body and the node-secret bearer, decoded as JSON.
    pub fn post_json<B: serde::Serialize, T: DeserializeOwned>(
        &self,
        path: &str,
        body: &B,
    ) -> Result<T, String> {
        let mut resp = self
            .agent
            .post(&self.url(path))
            .header("Authorization", &format!("Bearer {}", self.node_secret))
            .header("X-Quasar-Node", &self.node_name)
            .send_json(body)
            .map_err(|e| format!("POST {path}: {e}"))?;
        resp.body_mut()
            .read_json()
            .map_err(|e| format!("decode {path}: {e}"))
    }

    /// `POST` whose response body is not needed.
    pub fn post_json_no_body<B: serde::Serialize>(
        &self,
        path: &str,
        body: &B,
    ) -> Result<(), String> {
        self.agent
            .post(&self.url(path))
            .header("Authorization", &format!("Bearer {}", self.node_secret))
            .header("X-Quasar-Node", &self.node_name)
            .send_json(body)
            .map_err(|e| format!("POST {path}: {e}"))?;
        Ok(())
    }
}

impl std::fmt::Debug for CpClient {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        // Never the secret.
        f.debug_struct("CpClient")
            .field("http_base", &self.http_base)
            .field("node_name", &self.node_name)
            .finish()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::enrollment::Fingerprint;

    fn client(policy: &TransportPolicy, base: &str) -> Result<CpClient, String> {
        CpClient::new(policy, base.into(), "n".into(), "s3cret".into())
    }

    #[test]
    fn a_client_builds_for_every_policy_and_never_prints_its_secret() {
        for (policy, base) in [
            (TransportPolicy::Plaintext, "http://127.0.0.1:8080/"),
            (TransportPolicy::WebPki, "https://cp.example:8443/"),
            (
                TransportPolicy::Pinned(Fingerprint([3; 32])),
                "https://cp.example:8443/",
            ),
        ] {
            let c = client(&policy, base).expect("matching scheme and policy");
            assert_eq!(c.http_base(), base.trim_end_matches('/'));
            let dbg = format!("{c:?}");
            assert!(!dbg.contains("s3cret"), "{dbg}");
        }
    }

    /// A policy/scheme mismatch would either leak the node secret or skip the pin, so it
    /// is refused at construction rather than at the first request.
    #[test]
    fn a_policy_that_contradicts_the_base_url_scheme_is_refused() {
        for policy in [
            TransportPolicy::WebPki,
            TransportPolicy::Pinned(Fingerprint([3; 32])),
        ] {
            let err = client(&policy, "http://cp.example:8080").unwrap_err();
            assert!(err.contains("https://"), "{err}");
        }
        let err = client(&TransportPolicy::Plaintext, "https://cp.example:8443").unwrap_err();
        assert!(err.contains("Plaintext"), "{err}");
    }
}
