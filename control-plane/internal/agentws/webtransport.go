package agentws

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"slices"

	"github.com/accreleus/quasar/control-plane/internal/wtendpoint"
)

const (
	maxWTAddresses    = 8
	maxWTCertificates = 2
)

// webTransportReport projects the capacity report's transports onto what is stored, or nil when
// the agent serves no WebTransport (the columns are then cleared: replaced wholesale, not
// keep-if-absent). The address goes into a URL a browser opens, so only literal IPs survive; a
// block that cannot be used is dropped whole rather than failing the report.
func webTransportReport(cap CapacityMsg) *wtendpoint.Reported {
	if !slices.Contains(cap.Transports, "webtransport") || len(cap.WebTransport) == 0 {
		return nil
	}
	var wt struct {
		Port         int               `json:"port"`
		Addresses    []string          `json:"addresses"`
		Certificates []json.RawMessage `json:"certificates"`
	}
	if json.Unmarshal(cap.WebTransport, &wt) != nil || wt.Port < 1 || wt.Port > 65535 {
		return nil
	}
	r := &wtendpoint.Reported{
		Transports:   []string{"webrtc", "webtransport"},
		Port:         wt.Port,
		Addresses:    []string{},
		Certificates: []wtendpoint.Certificate{},
	}
	for _, a := range wt.Addresses {
		ip := net.ParseIP(a)
		if ip == nil || ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || len(r.Addresses) == maxWTAddresses {
			continue
		}
		r.Addresses = append(r.Addresses, ip.String())
	}
	for _, raw := range wt.Certificates {
		var c wtendpoint.Certificate
		if json.Unmarshal(raw, &c) != nil || c.NotAfter.IsZero() || len(r.Certificates) == maxWTCertificates {
			continue
		}
		if sum, err := base64.StdEncoding.DecodeString(c.SHA256); err != nil || len(sum) != 32 {
			continue
		}
		r.Certificates = append(r.Certificates, c)
	}
	return r
}

func (s *agentStore) upsertHostWebTransport(ctx context.Context, hostID string, r *wtendpoint.Reported) error {
	return wtendpoint.Record(ctx, s.pool, hostID, r)
}
