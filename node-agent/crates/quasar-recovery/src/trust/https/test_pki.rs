//! A throwaway CA and server certificate, generated per test run with ring and encoded
//! by hand (the lockfile carries no certificate generator). No key material is committed.

use ring::rand::SystemRandom;
use ring::signature::{EcdsaKeyPair, KeyPair, ECDSA_P256_SHA256_ASN1_SIGNING};
use rustls::pki_types::{CertificateDer, PrivateKeyDer, PrivatePkcs8KeyDer};

const OID_ECDSA_SHA256: &[u8] = &[0x2a, 0x86, 0x48, 0xce, 0x3d, 0x04, 0x03, 0x02];
const OID_EC_PUBLIC_KEY: &[u8] = &[0x2a, 0x86, 0x48, 0xce, 0x3d, 0x02, 0x01];
const OID_P256: &[u8] = &[0x2a, 0x86, 0x48, 0xce, 0x3d, 0x03, 0x01, 0x07];
const OID_COMMON_NAME: &[u8] = &[0x55, 0x04, 0x03];
const OID_BASIC_CONSTRAINTS: &[u8] = &[0x55, 0x1d, 0x13];
const OID_KEY_USAGE: &[u8] = &[0x55, 0x1d, 0x0f];
const OID_SUBJECT_ALT_NAME: &[u8] = &[0x55, 0x1d, 0x11];
const OID_EXT_KEY_USAGE: &[u8] = &[0x55, 0x1d, 0x25];
const OID_SERVER_AUTH: &[u8] = &[0x2b, 0x06, 0x01, 0x05, 0x05, 0x07, 0x03, 0x01];

fn tlv(tag: u8, content: &[u8]) -> Vec<u8> {
    let mut out = vec![tag];
    match content.len() {
        n if n < 0x80 => out.push(n as u8),
        n if n < 0x100 => out.extend([0x81, n as u8]),
        n => out.extend([0x82, (n >> 8) as u8, n as u8]),
    }
    out.extend_from_slice(content);
    out
}

fn seq(parts: &[Vec<u8>]) -> Vec<u8> {
    tlv(0x30, &parts.concat())
}

fn oid(bytes: &[u8]) -> Vec<u8> {
    tlv(0x06, bytes)
}

fn name(common_name: &str) -> Vec<u8> {
    seq(&[tlv(
        0x31,
        &seq(&[oid(OID_COMMON_NAME), tlv(0x0c, common_name.as_bytes())]),
    )])
}

fn extension(id: &[u8], critical: bool, value: Vec<u8>) -> Vec<u8> {
    let mut parts = vec![oid(id)];
    if critical {
        parts.push(tlv(0x01, &[0xff]));
    }
    parts.push(tlv(0x04, &value));
    seq(&parts)
}

pub(super) struct Issued {
    pub(super) cert: CertificateDer<'static>,
    pub(super) key: PrivateKeyDer<'static>,
    signer: EcdsaKeyPair,
    subject: Vec<u8>,
}

fn keypair() -> (EcdsaKeyPair, Vec<u8>) {
    let rng = SystemRandom::new();
    let pkcs8 =
        EcdsaKeyPair::generate_pkcs8(&ECDSA_P256_SHA256_ASN1_SIGNING, &rng).expect("keygen");
    let pair = EcdsaKeyPair::from_pkcs8(&ECDSA_P256_SHA256_ASN1_SIGNING, pkcs8.as_ref(), &rng)
        .expect("key");
    (pair, pkcs8.as_ref().to_vec())
}

fn certificate(
    serial: u8,
    issuer: &[u8],
    subject: &[u8],
    public_key: &[u8],
    extensions: Vec<Vec<u8>>,
    signer: &EcdsaKeyPair,
) -> Vec<u8> {
    let algorithm = seq(&[oid(OID_ECDSA_SHA256)]);
    let spki = seq(&[
        seq(&[oid(OID_EC_PUBLIC_KEY), oid(OID_P256)]),
        tlv(0x03, &[&[0u8][..], public_key].concat()),
    ]);
    let tbs = seq(&[
        tlv(0xa0, &tlv(0x02, &[2])),
        tlv(0x02, &[serial]),
        algorithm.clone(),
        issuer.to_vec(),
        seq(&[tlv(0x17, b"200101000000Z"), tlv(0x17, b"491231235959Z")]),
        subject.to_vec(),
        spki,
        tlv(0xa3, &seq(&extensions)),
    ]);
    let signature = signer.sign(&SystemRandom::new(), &tbs).expect("sign");
    seq(&[
        tbs,
        algorithm,
        tlv(0x03, &[&[0u8][..], signature.as_ref()].concat()),
    ])
}

/// A self-signed CA.
pub(super) fn ca(common_name: &str) -> Issued {
    let (pair, pkcs8) = keypair();
    let subject = name(common_name);
    let extensions = vec![
        extension(OID_BASIC_CONSTRAINTS, true, seq(&[tlv(0x01, &[0xff])])),
        extension(OID_KEY_USAGE, true, tlv(0x03, &[0x01, 0x06])),
    ];
    let der = certificate(
        1,
        &subject,
        &subject,
        pair.public_key().as_ref(),
        extensions,
        &pair,
    );
    Issued {
        cert: CertificateDer::from(der),
        key: PrivateKeyDer::Pkcs8(PrivatePkcs8KeyDer::from(pkcs8)),
        signer: pair,
        subject,
    }
}

/// A server certificate for `localhost` and 127.0.0.1, issued by `ca`.
pub(super) fn server(ca: &Issued) -> Issued {
    let (pair, pkcs8) = keypair();
    let subject = name("localhost");
    let san = seq(&[tlv(0x82, b"localhost"), tlv(0x87, &[127, 0, 0, 1])]);
    let extensions = vec![
        extension(OID_KEY_USAGE, true, tlv(0x03, &[0x07, 0x80])),
        extension(OID_EXT_KEY_USAGE, false, seq(&[oid(OID_SERVER_AUTH)])),
        extension(OID_SUBJECT_ALT_NAME, false, san),
    ];
    let der = certificate(
        2,
        &ca.subject,
        &subject,
        pair.public_key().as_ref(),
        extensions,
        &ca.signer,
    );
    Issued {
        cert: CertificateDer::from(der),
        key: PrivateKeyDer::Pkcs8(PrivatePkcs8KeyDer::from(pkcs8)),
        signer: pair,
        subject,
    }
}
