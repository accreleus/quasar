// Package access implements the first-run-wizard v2 §S6 surface: certificate
// download (§S6a), the "why can't my browser reach this" diagnosis endpoint
// (§S6b), and operator certificate upload with hot-reload (§S6d).
//
// The one rule this package keeps: the private key is write-only. It is
// sealed into internal/secrets under QUASAR_SECRET_KEY on upload and never
// returned, logged, or written as plaintext PEM. The download path re-encodes
// PEM from the parsed leaf's DER, so it has no key bytes to emit.
package access

import (
	"encoding/base64"
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

// ErrInvalidCertificate wraps every operator-fixable upload rejection, so the
// handler can answer 400 validation_failed with the specific message rather
// than a generic parse error.
var ErrInvalidCertificate = errors.New("invalid certificate")

// Source names where the certificate currently being served came from.
const (
	// SourceSelfSigned is the pair tlsx generated into QUASAR_TLS_DIR on first boot.
	SourceSelfSigned = "self_signed"
	// SourceProvided is an operator-mounted pair named by QUASAR_TLS_CERT /
	// QUASAR_TLS_KEY.
	SourceProvided = "provided"
)

// Info is everything public about the certificate in force — all of it already
// disclosed by any TLS handshake. No field can hold key material.
type Info struct {
	Source            string `json:"source"`
	FingerprintSHA256 string `json:"fingerprint_sha256"`
	// SPKISHA256 is curl's --pinnedpubkey form (sha256//<this>), so the enroll-host
	// one-liner can pin the key instead of trusting a CA. Contract: control-api.md.
	SPKISHA256      string    `json:"spki_sha256"`
	Subject         string    `json:"subject"`
	Issuer          string    `json:"issuer"`
	NotBefore       time.Time `json:"not_before"`
	NotAfter        time.Time `json:"not_after"`
	DaysUntilExpiry int       `json:"days_until_expiry"`
	DNSNames        []string  `json:"dns_names"`
	IPAddresses     []string  `json:"ip_addresses"`
	// ChainLength counts the leaf plus any intermediates supplied with it.
	ChainLength int `json:"chain_length"`
	// SelfSigned reports whether the leaf issued itself — the reason a browser warns.
	SelfSigned bool `json:"self_signed"`

	// leafDER is what GET /v1/tls/certificate.pem re-encodes. Unexported and
	// untagged so the download path and the metadata path cannot be confused.
	leafDER []byte
}

// LeafPEM re-encodes the leaf certificate as PEM. It is the only byte source
// the public download route uses, constructed rather than read from a file so
// no path mistake or chain-concatenation slip can put key material into the
// response. Guarded by TestCertificateDownloadNeverLeaksKey.
func (i Info) LeafPEM() []byte {
	if len(i.leafDER) == 0 {
		return nil
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: i.leafDER})
}

// CoversHost reports whether the leaf's SANs cover host (may carry a port).
// A leaf with no SANs covers nothing: browsers no longer honour the legacy
// CommonName fallback, and reporting CN coverage would tell an operator their
// setup is fine while their browser refuses it.
func (i Info) CoversHost(host string) bool {
	name := hostOnly(host)
	if name == "" {
		return false
	}
	leaf, err := x509.ParseCertificate(i.leafDER)
	if err != nil {
		return false
	}
	return leaf.VerifyHostname(name) == nil
}

// hostOnly strips a port and surrounding brackets from an authority.
func hostOnly(host string) string {
	host = strings.TrimSpace(host)
	if host == "" {
		return ""
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.Trim(strings.ToLower(host), "[]")
}

// Fingerprint renders SHA-256 over the DER as uppercase colon-separated hex —
// the same form tlsx.Fingerprint logs at startup and a browser's certificate
// viewer shows, so an operator can compare literally (§S6a).
// SPKIPin is base64(SHA-256(DER SubjectPublicKeyInfo)) — the value after
// `sha256//` in curl's --pinnedpubkey.
func SPKIPin(spkiDER []byte) string {
	sum := sha256.Sum256(spkiDER)
	return base64.StdEncoding.EncodeToString(sum[:])
}

func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(parts, ":")
}

// isSelfSigned reports whether the leaf issued itself.
//
// Not CheckSignatureFrom: that validates the parent as a CA, so an ordinary
// self-signed server certificate (IsCA=false — what tlsx emits) fails it and
// would be reported as not self-signed, suppressing the "download and trust
// this" guidance for exactly the operators who need it. The honest test is:
// names itself as issuer, and its own key verifies its own signature.
func isSelfSigned(leaf *x509.Certificate) bool {
	if !bytes.Equal(leaf.RawSubject, leaf.RawIssuer) {
		return false
	}
	return leaf.CheckSignature(leaf.SignatureAlgorithm, leaf.RawTBSCertificate, leaf.Signature) == nil
}

// describe builds Info from a parsed chain (leaf first).
func describe(source string, chain []*x509.Certificate) Info {
	leaf := chain[0]
	ips := make([]string, 0, len(leaf.IPAddresses))
	for _, ip := range leaf.IPAddresses {
		ips = append(ips, ip.String())
	}
	dns := leaf.DNSNames
	if dns == nil {
		dns = []string{}
	}
	return Info{
		Source:            source,
		FingerprintSHA256: Fingerprint(leaf.Raw),
		SPKISHA256:        SPKIPin(leaf.RawSubjectPublicKeyInfo),
		Subject:           leaf.Subject.String(),
		Issuer:            leaf.Issuer.String(),
		NotBefore:         leaf.NotBefore.UTC(),
		NotAfter:          leaf.NotAfter.UTC(),
		DaysUntilExpiry:   int(time.Until(leaf.NotAfter) / (24 * time.Hour)),
		DNSNames:          dns,
		IPAddresses:       ips,
		ChainLength:       len(chain),
		SelfSigned:        isSelfSigned(leaf),
		leafDER:           leaf.Raw,
	}
}

// parseChain decodes every CERTIFICATE block in certPEM, in file order, and
// requires the input to be nothing but CERTIFICATE blocks and whitespace.
//
// The strictness matters: pem.Decode silently skips unrecognised bytes, so a
// lenient parse would accept arbitrary material riding beside the certificates
// and report a clean chain. A private-key block is refused by name rather than
// skipped — pasting "fullchain.pem plus privkey.pem" into the certificate
// field is a real operator mistake and must not be silently accepted.
func parseChain(certPEM []byte) ([]*x509.Certificate, error) {
	var chain []*x509.Certificate
	rest := certPEM
	for {
		trimmed := bytes.TrimLeft(rest, " \t\r\n")
		if len(trimmed) == 0 {
			break
		}
		// pem.Decode would skip forward to the next BEGIN line; requiring the next
		// non-whitespace bytes to start a block turns "ignored junk" into a rejection.
		if !bytes.HasPrefix(trimmed, []byte("-----BEGIN ")) {
			return nil, fmt.Errorf("%w: the certificate field contains data that is not part of a PEM block. "+
				"It must contain only certificates — no comments, no headers, no key material",
				ErrInvalidCertificate)
		}
		var block *pem.Block
		block, rest = pem.Decode(trimmed)
		if block == nil {
			return nil, fmt.Errorf("%w: a PEM block in the certificate field is malformed and could not be decoded",
				ErrInvalidCertificate)
		}
		switch {
		case block.Type == "CERTIFICATE":
			c, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("%w: a CERTIFICATE block could not be parsed as X.509: %v",
					ErrInvalidCertificate, err)
			}
			chain = append(chain, c)
		case strings.Contains(block.Type, "PRIVATE KEY"):
			return nil, fmt.Errorf("%w: the certificate field contains a %s block. "+
				"Send the certificate (and any intermediates) in certificate_pem and the key in private_key_pem — never both in one field",
				ErrInvalidCertificate, block.Type)
		default:
			return nil, fmt.Errorf("%w: unexpected %q PEM block in the certificate field; expected CERTIFICATE",
				ErrInvalidCertificate, block.Type)
		}
	}
	if len(chain) == 0 {
		return nil, fmt.Errorf("%w: no CERTIFICATE PEM block found. "+
			"Paste the whole file including the -----BEGIN CERTIFICATE----- lines", ErrInvalidCertificate)
	}
	return chain, nil
}

// checkServerUsable rejects a leaf that parses and matches its key but that no
// browser would accept as a server certificate — an upload hot-swaps the live
// listener, so it must not be able to take HTTPS down with a certificate that
// was never usable here. Realistic cases: a CA certificate uploaded by
// accident, and a clientAuth-only certificate from an mTLS setup.
//
// An empty ExtKeyUsage means unrestricted and is accepted — that is what the
// self-signed pair and plenty of real certificates carry.
func checkServerUsable(leaf *x509.Certificate) error {
	if leaf.IsCA {
		return fmt.Errorf("%w: this is a CA certificate, not a server certificate. "+
			"Upload the leaf (server) certificate — the one issued for your hostname — with any intermediates after it",
			ErrInvalidCertificate)
	}
	// KeyUsage, when present (zero means absent, hence unrestricted), must permit
	// digitalSignature: every modern handshake (TLS 1.3, ECDHE in 1.2) has the
	// server sign, so a certificate without it cannot complete one.
	if leaf.KeyUsage != 0 && leaf.KeyUsage&x509.KeyUsageDigitalSignature == 0 {
		return fmt.Errorf("%w: this certificate's Key Usage extension does not permit digital signatures, "+
			"which every modern TLS handshake requires of a server certificate. It cannot serve HTTPS however it is installed",
			ErrInvalidCertificate)
	}
	if len(leaf.ExtKeyUsage) == 0 && len(leaf.UnknownExtKeyUsage) == 0 {
		return nil // unrestricted
	}
	for _, eku := range leaf.ExtKeyUsage {
		if eku == x509.ExtKeyUsageServerAuth || eku == x509.ExtKeyUsageAny {
			return nil
		}
	}
	return fmt.Errorf("%w: this certificate's Extended Key Usage does not permit server authentication, "+
		"so a browser will refuse it however it is installed. A client-authentication certificate cannot serve HTTPS",
		ErrInvalidCertificate)
}

// checkChainValidity checks NotBefore/NotAfter on every certificate in the
// chain, not just the leaf: a valid leaf bundled with an expired intermediate
// installs cleanly and then breaks most clients, and from the operator's side
// the leaf they were looking at is fine. The error names index and subject.
func checkChainValidity(chain []*x509.Certificate, now time.Time) error {
	for i, c := range chain {
		what := "the leaf (server) certificate"
		if i > 0 {
			what = fmt.Sprintf("intermediate certificate %d (%s)", i, c.Subject.CommonName)
		}
		if now.After(c.NotAfter) {
			return fmt.Errorf("%w: %s expired on %s. "+
				"Renew or replace it before uploading — a chain containing an expired certificate breaks clients that build a path through it, even when the leaf itself is valid",
				ErrInvalidCertificate, what, c.NotAfter.UTC().Format(time.RFC3339))
		}
		if now.Before(c.NotBefore) {
			return fmt.Errorf("%w: %s is not valid until %s. "+
				"Check the server clock, or wait until it becomes valid",
				ErrInvalidCertificate, what, c.NotBefore.UTC().Format(time.RFC3339))
		}
	}
	return nil
}

// checkChainOrder enforces leaf-first ordering, and must run before
// tls.X509KeyPair: a reversed chain fails X509KeyPair with "private key does
// not match public key" (the key is matched against whatever is first), which
// sends the operator hunting the wrong file. §S6d requires the specific
// diagnosis to win.
func checkChainOrder(chain []*x509.Certificate) error {
	if len(chain) < 2 {
		return nil
	}
	// The classic mistake: root/intermediate first, leaf last.
	if chain[0].IsCA {
		for _, c := range chain[1:] {
			if !c.IsCA {
				return fmt.Errorf("%w: the chain is in the wrong order — it starts with a CA certificate (%s) and the server (leaf) certificate appears later. "+
					"The leaf must come FIRST, followed by intermediates, root last or omitted",
					ErrInvalidCertificate, chain[0].Subject.CommonName)
			}
		}
	}
	for i := 0; i < len(chain)-1; i++ {
		if err := chain[i].CheckSignatureFrom(chain[i+1]); err != nil {
			// Only a reversed chain is fixable by re-ordering; an unrelated bundle
			// gets a different message.
			if reversedChainVerifies(chain) {
				return fmt.Errorf("%w: the chain is in the wrong order — each certificate must be signed by the NEXT one, and this bundle is reversed. "+
					"Put the leaf (server) certificate first, then each intermediate", ErrInvalidCertificate)
			}
			return fmt.Errorf("%w: certificate %d in the chain is not signed by certificate %d. "+
				"These certificates do not form a chain — check you pasted the right fullchain file",
				ErrInvalidCertificate, i+1, i+2)
		}
	}
	return nil
}

func reversedChainVerifies(chain []*x509.Certificate) bool {
	n := len(chain)
	for i := 0; i < n-1; i++ {
		if err := chain[n-1-i].CheckSignatureFrom(chain[n-2-i]); err != nil {
			return false
		}
	}
	return true
}

// Validate is the whole acceptance gate for a cert/key pair. It returns the
// usable tls.Certificate and its public Info, or an ErrInvalidCertificate
// written for an operator. tls.X509KeyPair is the only proof that the key
// matches the certificate.
func Validate(certPEM, keyPEM []byte, source string) (*tls.Certificate, Info, error) {
	chain, err := parseChain(certPEM)
	if err != nil {
		return nil, Info{}, err
	}
	if err := checkChainOrder(chain); err != nil {
		return nil, Info{}, err
	}
	if len(strings.TrimSpace(string(keyPEM))) == 0 {
		return nil, Info{}, fmt.Errorf("%w: no private key was supplied", ErrInvalidCertificate)
	}
	if block, _ := pem.Decode(keyPEM); block == nil {
		return nil, Info{}, fmt.Errorf("%w: the private key is not PEM. "+
			"Paste the whole key file including its -----BEGIN ...----- lines", ErrInvalidCertificate)
	} else if block.Type == "CERTIFICATE" {
		return nil, Info{}, fmt.Errorf("%w: the private-key field contains a CERTIFICATE. "+
			"The two fields are the other way round", ErrInvalidCertificate)
	}

	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		// Answer with our own sentence rather than echoing crypto/tls: nothing
		// about the key's contents may reach an HTTP response or audit row.
		return nil, Info{}, fmt.Errorf("%w: the private key does not match the certificate (or is of a type this build cannot use). "+
			"They must be the pair issued together", ErrInvalidCertificate)
	}

	leaf := chain[0]
	if err := checkServerUsable(leaf); err != nil {
		return nil, Info{}, err
	}
	if err := checkChainValidity(chain, time.Now()); err != nil {
		return nil, Info{}, err
	}
	if len(leaf.DNSNames) == 0 && len(leaf.IPAddresses) == 0 {
		return nil, Info{}, fmt.Errorf("%w: the certificate has no Subject Alternative Names. "+
			"Browsers have not honoured the legacy Common Name for years, so this certificate would be rejected by every client",
			ErrInvalidCertificate)
	}

	pair.Leaf = leaf
	return &pair, describe(source, chain), nil
}
