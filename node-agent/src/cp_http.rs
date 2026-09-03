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

/// Per-request budget shared by every pull channel (they each had their own copy of the
/// same 20 s; one definition now).
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
        if !details.needs_tls() || transport.is_tls() {
            return Ok(Some(Either::A(transport)));
        }
        // `ServerName` from the bare host: a DNS name OR an IP literal (rustls-pki-types 1.x
        // parses both). Under a pin the name is not verified against the certificate at all;
        // it only selects SNI, which rustls omits for IP literals.
        let name: ServerName<'_> = details
            .uri
            .authority()
            .ok_or(ureq::Error::Tls("control-plane URL has no authority"))?
            .host_bare()
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
    pub fn new(
        policy: &TransportPolicy,
        http_base: String,
        node_name: String,
        node_secret: String,
    ) -> Self {
        let config = ureq::Agent::config_builder()
            .timeout_global(Some(HTTP_TIMEOUT))
            .build();
        let agent = match crate::cp_tls::client_config(policy) {
            // Our verifier for both pinned and WebPKI: one code path, one answer.
            Some(tls) => ureq::Agent::with_parts(
                config,
                TcpConnector::default().chain(PolicyTlsConnector { config: tls }),
                DefaultResolver::default(),
            ),
            // Plaintext: the URL is http://, so TLS never enters the picture.
            None => ureq::Agent::new_with_config(config),
        };
        Self {
            agent,
            http_base: http_base.trim_end_matches('/').to_string(),
            node_name,
            node_secret,
        }
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

    #[test]
    fn a_client_builds_for_every_policy_and_never_prints_its_secret() {
        for policy in [
            TransportPolicy::Plaintext,
            TransportPolicy::WebPki,
            TransportPolicy::Pinned(Fingerprint([3; 32])),
        ] {
            let c = CpClient::new(
                &policy,
                "https://cp.example:8443/".into(),
                "n".into(),
                "s3cret".into(),
            );
            assert_eq!(c.http_base(), "https://cp.example:8443");
            let dbg = format!("{c:?}");
            assert!(!dbg.contains("s3cret"), "{dbg}");
        }
    }
}
