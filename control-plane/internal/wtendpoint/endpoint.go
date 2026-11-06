// Package wtendpoint resolves where a browser reaches a host's WebTransport listener: the agent's
// own report, or an admin's public-URL override. One pure function serves the stream plan, the
// token mints and the admin read. semantics: control-api.md §WebTransport endpoint
package wtendpoint

import (
	"errors"
	"net"
	"net/url"
	"strconv"
	"time"
)

const (
	ModeDirect = "direct"
	ModeEdge   = "edge"

	// CertPublic: the override URL presents a publicly trusted certificate (no hashes sent).
	// CertAgent: it presents the agent's own (hashes sent).
	CertPublic = "public"
	CertAgent  = "agent"

	ReasonNotReported        = "not_reported"
	ReasonHostOffline        = "host_offline"
	ReasonNoAddress          = "no_address"
	ReasonCertificateExpired = "certificate_expired"

	maxURLLen = 2048
)

type Certificate struct {
	SHA256    string    `json:"sha256"` // base64 of the DER's SHA-256, the serverCertificateHashes value
	NotBefore time.Time `json:"not_before"`
	NotAfter  time.Time `json:"not_after"`
}

// Reported is the agent's last capacity report of its transports; nil on a Host means it never
// reported WebTransport.
type Reported struct {
	Transports   []string      `json:"transports"`
	Port         int           `json:"port"`
	Addresses    []string      `json:"addresses"`
	Certificates []Certificate `json:"certificates"`
}

// Serves reports whether the agent lists webtransport.
func (r *Reported) Serves() bool {
	if r == nil {
		return false
	}
	for _, t := range r.Transports {
		if t == "webtransport" {
			return true
		}
	}
	return false
}

type Override struct {
	PublicURL   string `json:"public_url"`
	Certificate string `json:"certificate"`
}

// Host is everything Resolve reads. Online is status='online' with a live agent connection.
type Host struct {
	Online   bool
	Reported *Reported
	Override *Override
}

type Hash struct {
	Algorithm string `json:"algorithm"`
	Value     string `json:"value"`
}

type Endpoint struct {
	URL        string `json:"url"`
	Mode       string `json:"mode"`
	CertHashes []Hash `json:"cert_hashes"`
}

// Resolution has exactly one of Endpoint (usable) or Reason (why not).
type Resolution struct {
	Endpoint *Endpoint
	Reason   string
}

func (r Resolution) Usable() bool { return r.Endpoint != nil }

func Resolve(h Host, now time.Time) Resolution {
	switch {
	case !h.Reported.Serves():
		return Resolution{Reason: ReasonNotReported}
	case !h.Online:
		return Resolution{Reason: ReasonHostOffline}
	}
	needHashes := h.Override == nil || h.Override.Certificate == CertAgent
	hashes := []Hash{}
	if needHashes {
		// An empty report means the agent presents a publicly trusted certificate: nothing to pin.
		for _, c := range h.Reported.Certificates {
			if c.NotAfter.After(now) {
				hashes = append(hashes, Hash{Algorithm: "sha-256", Value: c.SHA256})
			}
		}
		if len(h.Reported.Certificates) > 0 && len(hashes) == 0 {
			return Resolution{Reason: ReasonCertificateExpired}
		}
	}
	if h.Override != nil {
		mode := ModeDirect
		if h.Override.Certificate != CertAgent {
			mode = ModeEdge
		}
		return Resolution{Endpoint: &Endpoint{URL: h.Override.PublicURL, Mode: mode, CertHashes: hashes}}
	}
	if len(h.Reported.Addresses) == 0 || h.Reported.Port < 1 || h.Reported.Port > 65535 {
		return Resolution{Reason: ReasonNoAddress}
	}
	hostPort := net.JoinHostPort(h.Reported.Addresses[0], strconv.Itoa(h.Reported.Port))
	return Resolution{Endpoint: &Endpoint{URL: "https://" + hostPort + "/wt", Mode: ModeDirect, CertHashes: hashes}}
}

// ValidatePublicURL is the PATCH rule: absolute https with a host, no user info, no fragment.
func ValidatePublicURL(s string) error {
	if len(s) == 0 || len(s) > maxURLLen {
		return errors.New("public_url must be 1-2048 characters")
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return errors.New("public_url must be an absolute https URL with a host")
	}
	if u.User != nil || u.Fragment != "" || u.Opaque != "" {
		return errors.New("public_url must have no user info and no fragment")
	}
	return nil
}
