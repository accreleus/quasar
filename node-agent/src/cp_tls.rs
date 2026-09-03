//! TLS for the control-plane link (#12): one `rustls::ClientConfig` per
//! [`TransportPolicy`], consumed by BOTH the websocket and the node-secret HTTP polls, so
//! the two clients can never disagree about who the control plane is.
//!
//! Under a pin, SAN and expiry are deliberately not checked — the pin is the identity, and
//! the deployment's self-signed certificate routinely lacks the LAN name or IP the agent
//! dials. Signatures ARE checked, through the crypto provider: a verifier that only compares
//! the certificate proves the peer *presented* the pinned cert, not that it holds the key,
//! and this deployment's certificate is publicly downloadable (`GET /v1/tls/certificate.pem`).

use std::sync::Arc;

use rustls::client::danger::{HandshakeSignatureValid, ServerCertVerified, ServerCertVerifier};
use rustls::crypto::CryptoProvider;
use rustls::pki_types::{CertificateDer, ServerName, UnixTime};
use rustls::{CertificateError, DigitallySignedStruct, Error, SignatureScheme};

use crate::enrollment::{Fingerprint, TransportPolicy};

/// Accepts exactly one leaf: the one whose DER hashes to the pin.
#[derive(Debug)]
pub struct PinnedVerifier {
    pin: Fingerprint,
    provider: Arc<CryptoProvider>,
}

impl PinnedVerifier {
    pub fn new(pin: Fingerprint, provider: Arc<CryptoProvider>) -> Self {
        Self { pin, provider }
    }

    /// The decision, kept separate so it is testable without a handshake.
    pub fn accepts(&self, end_entity_der: &[u8]) -> bool {
        Fingerprint::of_der(end_entity_der) == self.pin
    }
}

impl ServerCertVerifier for PinnedVerifier {
    fn verify_server_cert(
        &self,
        end_entity: &CertificateDer<'_>,
        _intermediates: &[CertificateDer<'_>],
        _server_name: &ServerName<'_>,
        _ocsp_response: &[u8],
        _now: UnixTime,
    ) -> Result<ServerCertVerified, Error> {
        if self.accepts(end_entity.as_ref()) {
            return Ok(ServerCertVerified::assertion());
        }
        let observed = Fingerprint::of_der(end_entity.as_ref());
        tracing::error!(
            token = "cp-tls-pin-mismatch",
            expected = %self.pin,
            observed = %observed,
            "the control plane presented a certificate that does not match the pinned \
             fingerprint. Either its certificate was re-issued (update \
             CONTROL_PLANE_FINGERPRINT / the enrollment string to the value in its startup \
             log) or something is intercepting this connection"
        );
        Err(Error::InvalidCertificate(
            CertificateError::ApplicationVerificationFailure,
        ))
    }

    fn verify_tls12_signature(
        &self,
        message: &[u8],
        cert: &CertificateDer<'_>,
        dss: &DigitallySignedStruct,
    ) -> Result<HandshakeSignatureValid, Error> {
        rustls::crypto::verify_tls12_signature(
            message,
            cert,
            dss,
            &self.provider.signature_verification_algorithms,
        )
    }

    fn verify_tls13_signature(
        &self,
        message: &[u8],
        cert: &CertificateDer<'_>,
        dss: &DigitallySignedStruct,
    ) -> Result<HandshakeSignatureValid, Error> {
        rustls::crypto::verify_tls13_signature(
            message,
            cert,
            dss,
            &self.provider.signature_verification_algorithms,
        )
    }

    fn supported_verify_schemes(&self) -> Vec<SignatureScheme> {
        self.provider
            .signature_verification_algorithms
            .supported_schemes()
    }
}

fn provider() -> Arc<CryptoProvider> {
    Arc::new(rustls::crypto::ring::default_provider())
}

/// The shared client configuration for a policy; `None` for plaintext.
pub fn client_config(policy: &TransportPolicy) -> Option<Arc<rustls::ClientConfig>> {
    let provider = provider();
    let builder = rustls::ClientConfig::builder_with_provider(provider.clone())
        .with_safe_default_protocol_versions()
        .expect("ring provider supports the default protocol versions");
    match policy {
        TransportPolicy::Pinned(fp) => Some(Arc::new(
            builder
                .dangerous()
                .with_custom_certificate_verifier(Arc::new(PinnedVerifier::new(*fp, provider)))
                .with_no_client_auth(),
        )),
        TransportPolicy::WebPki => {
            let mut roots = rustls::RootCertStore::empty();
            roots.extend(webpki_roots::TLS_SERVER_ROOTS.iter().cloned());
            Some(Arc::new(
                builder
                    .with_root_certificates(roots)
                    .with_no_client_auth(),
            ))
        }
        TransportPolicy::Plaintext => None,
    }
}

/// The websocket connector for a policy. `Plain` is explicit so a `wss://` URL can never
/// fall through to tokio-tungstenite's default (OS/bundled roots) by accident.
pub fn ws_connector(policy: &TransportPolicy) -> tokio_tungstenite::Connector {
    match client_config(policy) {
        Some(cfg) => tokio_tungstenite::Connector::Rustls(cfg),
        None => tokio_tungstenite::Connector::Plain,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_pinned_leaf_is_accepted_and_any_other_is_refused() {
        let der = b"not really a certificate, but the hash does not care";
        let v = PinnedVerifier::new(Fingerprint::of_der(der), provider());
        assert!(v.accepts(der));
        assert!(!v.accepts(b"a different certificate"));
        assert!(!v.accepts(b""));
    }

    #[test]
    fn signature_schemes_come_from_the_provider_not_a_hand_list() {
        // An empty list here is the classic copy-paste failure: every handshake then fails
        // in a way that looks like a server bug.
        let v = PinnedVerifier::new(Fingerprint([0; 32]), provider());
        assert!(!v.supported_verify_schemes().is_empty());
    }

    #[test]
    fn every_policy_yields_the_right_client_shape() {
        assert!(client_config(&TransportPolicy::Plaintext).is_none());
        assert!(client_config(&TransportPolicy::WebPki).is_some());
        assert!(client_config(&TransportPolicy::Pinned(Fingerprint([7; 32]))).is_some());
        assert!(matches!(
            ws_connector(&TransportPolicy::Plaintext),
            tokio_tungstenite::Connector::Plain
        ));
        assert!(matches!(
            ws_connector(&TransportPolicy::Pinned(Fingerprint([7; 32]))),
            tokio_tungstenite::Connector::Rustls(_)
        ));
    }
}
