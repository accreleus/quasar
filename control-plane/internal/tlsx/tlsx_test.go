package tlsx

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureSelfSignedGeneratesAndReuses(t *testing.T) {
	dir := t.TempDir()
	sans := GatherSANs("game.lan, 10.0.0.5", "play.example.com")

	certPath, keyPath, generated, err := EnsureSelfSigned(dir, sans)
	if err != nil {
		t.Fatalf("EnsureSelfSigned: %v", err)
	}
	if !generated {
		t.Fatalf("first call should generate a new pair")
	}
	if certPath != filepath.Join(dir, certFileName) || keyPath != filepath.Join(dir, keyFileName) {
		t.Fatalf("unexpected paths: %q %q", certPath, keyPath)
	}

	// The pair must load as a valid TLS keypair.
	if _, err := tls.LoadX509KeyPair(certPath, keyPath); err != nil {
		t.Fatalf("LoadX509KeyPair: %v", err)
	}

	// Key file must not be world-readable.
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("key perms = %o, want 600", perm)
	}

	// Parse the cert and assert the SANs are present.
	cert := parseLeaf(t, certPath)
	if cert.Subject.CommonName != "quasar-control" {
		t.Fatalf("CN = %q, want quasar-control", cert.Subject.CommonName)
	}
	assertDNS(t, cert, "localhost")
	assertDNS(t, cert, "game.lan")
	assertDNS(t, cert, "play.example.com")
	assertIP(t, cert, net.IPv4(127, 0, 0, 1))
	assertIP(t, cert, net.IPv6loopback)
	assertIP(t, cert, net.ParseIP("10.0.0.5"))

	// Validity should be roughly a decade (well beyond a year).
	if got := cert.NotAfter.Sub(cert.NotBefore); got < 9*365*24*60*60*1e9 {
		t.Fatalf("validity too short: %v", got)
	}

	// Read the original bytes so we can prove reuse doesn't rewrite them.
	origCert, _ := os.ReadFile(certPath)

	certPath2, keyPath2, generated2, err := EnsureSelfSigned(dir, sans)
	if err != nil {
		t.Fatalf("second EnsureSelfSigned: %v", err)
	}
	if generated2 {
		t.Fatalf("second call should REUSE the persisted pair, not regenerate")
	}
	if certPath2 != certPath || keyPath2 != keyPath {
		t.Fatalf("reuse returned different paths")
	}
	reCert, _ := os.ReadFile(certPath)
	if string(origCert) != string(reCert) {
		t.Fatalf("cert file was rewritten on reuse (browser exception would be invalidated)")
	}
}

func TestEnsureSelfSignedUnwritableDir(t *testing.T) {
	// A path whose parent is a regular file cannot be MkdirAll'd.
	base := t.TempDir()
	notADir := filepath.Join(base, "afile")
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, _, err := EnsureSelfSigned(filepath.Join(notADir, "tls"), GatherSANs("", ""))
	if err == nil {
		t.Fatalf("expected an error for an unwritable TLS dir")
	}
}

func TestFingerprintStable(t *testing.T) {
	dir := t.TempDir()
	certPath, _, _, err := EnsureSelfSigned(dir, GatherSANs("", ""))
	if err != nil {
		t.Fatal(err)
	}
	fp1, err := Fingerprint(certPath)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	fp2, err := Fingerprint(certPath)
	if err != nil {
		t.Fatal(err)
	}
	if fp1 != fp2 {
		t.Fatalf("fingerprint not stable: %q vs %q", fp1, fp2)
	}
	if len(fp1) != 32*3-1 { // 32 bytes -> 32 hex pairs joined by 31 colons
		t.Fatalf("unexpected fingerprint length %d: %q", len(fp1), fp1)
	}
}

func TestGatherSANsDedupesAndSeparatesIPs(t *testing.T) {
	s := GatherSANs("localhost, 127.0.0.1, example.com, EXAMPLE.com", "127.0.0.1")
	// localhost must appear once.
	count := 0
	for _, d := range s.DNS {
		if d == "localhost" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("localhost appears %d times, want 1", count)
	}
	// A value that parses as an IP must land in IPs, not DNS.
	for _, d := range s.DNS {
		if net.ParseIP(d) != nil {
			t.Fatalf("IP %q leaked into DNS SANs", d)
		}
	}
	// 127.0.0.1 (given twice + loopback default) must be deduped.
	loopCount := 0
	for _, ip := range s.IPs {
		if ip.Equal(net.IPv4(127, 0, 0, 1)) {
			loopCount++
		}
	}
	if loopCount != 1 {
		t.Fatalf("127.0.0.1 appears %d times in IPs, want 1", loopCount)
	}
}

func parseLeaf(t *testing.T, certPath string) *x509.Certificate {
	t.Helper()
	raw, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		t.Fatal("no PEM block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func assertDNS(t *testing.T, cert *x509.Certificate, name string) {
	t.Helper()
	for _, d := range cert.DNSNames {
		if d == name {
			return
		}
	}
	t.Fatalf("DNS SAN %q missing from %v", name, cert.DNSNames)
}

func assertIP(t *testing.T, cert *x509.Certificate, ip net.IP) {
	t.Helper()
	for _, got := range cert.IPAddresses {
		if got.Equal(ip) {
			return
		}
	}
	t.Fatalf("IP SAN %v missing from %v", ip, cert.IPAddresses)
}
