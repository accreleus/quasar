package access

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"
)

// Manager owns the certificate the HTTPS listener actually serves. It is the
// tls.Config.GetCertificate callback, so it is the single place that knows
// what is being served — what the download and access-check routes report on
// without re-reading a file.
type Manager struct {
	log *slog.Logger

	// mu guards the pair. Loaded once at startup today, but runtime replacement
	// is the point of the GetCertificate callback; keep the lock.
	mu     sync.RWMutex
	active *tls.Certificate
	info   Info
}

// NewManager loads the on-disk pair (self-signed or operator-provided) and
// makes it active. The key is read here, once; nothing else in this package
// reads a key path and no method exposes it.
func NewManager(certPath, keyPath, source string, log *slog.Logger) (*Manager, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, fmt.Errorf("read TLS certificate %q: %w", certPath, err)
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("read TLS key %q: %w", keyPath, err)
	}
	pair, info, err := Validate(certPEM, keyPEM, source)
	if err != nil {
		// Validate's strictness belongs on the upload path, where the operator is
		// standing there to fix it. A file that already works is different: real
		// cert.pems carry bag attributes or text dumps ServeTLS accepted, and
		// refusing to boot on one turns an upgrade into an outage on a deployment
		// that changed nothing. So fall back to what crypto/tls itself accepts.
		fallbackPair, ferr := tls.X509KeyPair(certPEM, keyPEM)
		if ferr != nil {
			return nil, fmt.Errorf("load TLS keypair: %w", ferr)
		}
		chain := make([]*x509.Certificate, 0, len(fallbackPair.Certificate))
		for i, der := range fallbackPair.Certificate {
			c, perr := x509.ParseCertificate(der)
			if perr != nil {
				return nil, fmt.Errorf("parse TLS certificate %q (entry %d): %w", certPath, i, perr)
			}
			chain = append(chain, c)
		}
		if len(chain) == 0 {
			return nil, fmt.Errorf("TLS certificate %q contains no certificate", certPath)
		}
		// The fallback tolerates only encoding looseness and dates/SANs (which
		// access-check diagnoses). A leaf that cannot serve TLS at all — CA cert,
		// KU/EKU forbidding serverAuth, reversed chain — must still refuse to
		// boot, or a clientAuth-only certificate goes active with access-check
		// reporting no blocking problem.
		if uerr := checkServerUsable(chain[0]); uerr != nil {
			return nil, fmt.Errorf("TLS certificate %q cannot serve HTTPS: %w", certPath, uerr)
		}
		if uerr := checkChainOrder(chain); uerr != nil {
			return nil, fmt.Errorf("TLS certificate %q: %w", certPath, uerr)
		}
		log.Warn("the TLS certificate on disk does not meet the criteria applied to UPLOADED certificates; "+
			"serving it anyway (it was already in use and crypto/tls accepts it), and "+
			"GET /v1/admin/access-check will report the problem",
			"cert", certPath, "problem", err)
		info = describe(source, chain)
		fallbackPair.Leaf = chain[0]
		pair = &fallbackPair
	}
	return &Manager{log: log, active: pair, info: info}, nil
}

// NewManagerFromPair builds a Manager around an already-validated pair. Used by
// tests; production goes through NewManager.
func NewManagerFromPair(pair *tls.Certificate, info Info, log *slog.Logger) *Manager {
	return &Manager{log: log, active: pair, info: info}
}

// GetCertificate satisfies tls.Config.GetCertificate. SNI is ignored: this
// listener serves exactly one certificate, and a hello for an uncovered name
// still gets it — the browser's name mismatch is the honest failure, and
// access-check explains it.
func (m *Manager) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.active == nil {
		return nil, fmt.Errorf("no TLS certificate is loaded")
	}
	return m.active, nil
}

// TLSConfig returns the config to hand to http.Server.ServeTLS.
func (m *Manager) TLSConfig() *tls.Config {
	return &tls.Config{
		GetCertificate: m.GetCertificate,
		MinVersion:     tls.VersionTLS12,
	}
}

// Current returns the public metadata of the certificate in force. It can never
// return key material: Info has no field that could hold any.
func (m *Manager) Current() Info {
	m.mu.RLock()
	defer m.mu.RUnlock()
	info := m.info
	// Recompute on read, not frozen at install time: days_until_expiry is what
	// catches the 90-day Let's Encrypt trap (§S6d).
	info.DaysUntilExpiry = int(time.Until(info.NotAfter) / (24 * time.Hour))
	return info
}
