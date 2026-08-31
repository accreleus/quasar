// Package tlsx provides the self-signed certificate generation, persistence,
// and SAN gathering for the control-plane's optional HTTPS listener (issue #376).
//
// The listener is "batteries-included": on first boot with no operator-provided
// cert it generates a long-lived self-signed pair and persists it under
// QUASAR_TLS_DIR so the browser's accepted-exception survives restarts.
// deploy/*.hardened remains the real-cert (Caddy/ACME) path.
package tlsx

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	certFileName = "cert.pem"
	keyFileName  = "key.pem"
	// selfSignedValidity is ~10 years: long enough that an operator who accepted
	// the browser exception is never re-prompted for the life of the deployment.
	selfSignedValidity = 10 * 365 * 24 * time.Hour
)

// SANs is the set of Subject Alternative Names baked into a self-signed cert.
type SANs struct {
	DNS []string
	IPs []net.IP
}

// GatherSANs assembles the SAN set for a generated cert: always localhost +
// loopback, plus every host/IP listed in QUASAR_TLS_HOSTS (comma-separated),
// QUASAR_PUBLIC_HOST when set, and this process's own non-loopback interface
// addresses when they can be enumerated cheaply.
//
// That last set is NOT the LAN story, despite reading like it. In a
// containerised deploy — which is the default (deploy/docker-compose*.yml) —
// the only address this process can enumerate is the container's own bridge
// address (e.g. 172.21.0.3). The host's LAN IP lives in a network namespace
// this process cannot see, so it never lands in the cert, and a browser hitting
// https://<host-lan-ip>:8443 fails hostname validation
// (ERR_CERT_COMMON_NAME_INVALID) however many times the operator trusts the
// cert. LAN access therefore REQUIRES the host's LAN IP/hostname in
// QUASAR_TLS_HOSTS (deploy/seed-tls-hosts.sh seeds it from the host's primary
// LAN address; deploy/redeploy.sh calls that). The interface sweep only earns
// its keep when the binary runs on the host network directly — bare-metal,
// `go run`, or a `network_mode: host` container.
func GatherSANs(tlsHosts, publicHost string) SANs {
	var s SANs
	dnsSeen := map[string]struct{}{}
	ipSeen := map[string]struct{}{}

	addDNS := func(h string) {
		h = strings.TrimSpace(h)
		if h == "" {
			return
		}
		if ip := net.ParseIP(h); ip != nil {
			addIP(ip, &s, ipSeen)
			return
		}
		key := strings.ToLower(h)
		if _, ok := dnsSeen[key]; ok {
			return
		}
		dnsSeen[key] = struct{}{}
		s.DNS = append(s.DNS, h)
	}

	addDNS("localhost")
	addIP(net.IPv4(127, 0, 0, 1), &s, ipSeen)
	addIP(net.IPv6loopback, &s, ipSeen)

	for _, h := range strings.Split(tlsHosts, ",") {
		addDNS(h)
	}
	addDNS(publicHost)

	// This process's own non-loopback addresses. Useful only when it runs on the
	// host network: in a container these are the container's bridge addresses,
	// never the host's LAN IP, so this does NOT make https://<host-lan-ip>:8443
	// validate — QUASAR_TLS_HOSTS does (see the doc comment above).
	// Best-effort: ignore enumeration failure.
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipNet, ok := a.(*net.IPNet); ok && !ipNet.IP.IsLoopback() && !ipNet.IP.IsLinkLocalUnicast() {
				addIP(ipNet.IP, &s, ipSeen)
			}
		}
	}
	return s
}

func addIP(ip net.IP, s *SANs, seen map[string]struct{}) {
	if ip == nil {
		return
	}
	key := ip.String()
	if _, ok := seen[key]; ok {
		return
	}
	seen[key] = struct{}{}
	s.IPs = append(s.IPs, ip)
}

// EnsureSelfSigned returns the paths to a persisted self-signed cert/key pair
// under dir, generating them on first call and reusing them on every later call
// (so an accepted browser exception survives restarts). generated reports
// whether a new pair was written. It returns an error if dir is not writable —
// the caller treats that as fatal (mount a volume).
func EnsureSelfSigned(dir string, sans SANs) (certPath, keyPath string, generated bool, err error) {
	certPath = filepath.Join(dir, certFileName)
	keyPath = filepath.Join(dir, keyFileName)

	if fileExists(certPath) && fileExists(keyPath) {
		return certPath, keyPath, false, nil
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", false, fmt.Errorf("create TLS dir %q (mount a writable volume): %w", dir, err)
	}

	certPEM, keyPEM, err := generateSelfSigned(sans)
	if err != nil {
		return "", "", false, err
	}
	// Write the key first with 0600, then the cert. A partial write leaves the
	// pair incomplete; the next boot regenerates since fileExists checks both.
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return "", "", false, fmt.Errorf("write TLS key %q (mount a writable volume): %w", keyPath, err)
	}
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return "", "", false, fmt.Errorf("write TLS cert %q (mount a writable volume): %w", certPath, err)
	}
	return certPath, keyPath, true, nil
}

func generateSelfSigned(sans SANs) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate EC key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("generate serial: %w", err)
	}

	now := time.Now()
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "quasar-control"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(selfSignedValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              sans.DNS,
		IPAddresses:           sans.IPs,
	}

	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("create certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal key: %w", err)
	}

	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}

// Fingerprint returns the SHA-256 fingerprint (uppercase, colon-separated hex)
// of the leaf certificate in the PEM file at certPath — the value a browser
// shows so an operator can eyeball-verify the accepted exception.
func Fingerprint(certPath string) (string, error) {
	raw, err := os.ReadFile(certPath)
	if err != nil {
		return "", err
	}
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != "CERTIFICATE" {
		return "", fmt.Errorf("no CERTIFICATE PEM block in %q", certPath)
	}
	sum := sha256.Sum256(block.Bytes)
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(parts, ":"), nil
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}
