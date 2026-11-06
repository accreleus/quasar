package agentws

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func b64(b byte) string {
	return base64.StdEncoding.EncodeToString([]byte(strings.Repeat(string([]byte{b}), 32)))
}

func capWithWT(t *testing.T, transports []string, wt string) CapacityMsg {
	t.Helper()
	msg := `{"type":"capacity","transports":` + mustJSON(t, transports)
	if wt != "" {
		msg += `,"webtransport":` + wt
	}
	var c CapacityMsg
	if err := json.Unmarshal([]byte(msg+`}`), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return c
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestWebTransportReport(t *testing.T) {
	good := `{"port":4433,"addresses":["192.0.2.10","2001:db8::10"],"protos":[1],"certificates":[
		{"sha256":"` + b64('a') + `","not_before":"2026-11-06T00:00:00Z","not_after":"2026-11-19T00:00:00Z"},
		{"sha256":"` + b64('b') + `","not_before":"2026-11-16T00:00:00Z","not_after":"2026-11-29T00:00:00Z"}]}`
	r := webTransportReport(capWithWT(t, []string{"webrtc", "webtransport"}, good))
	if r == nil || r.Port != 4433 || len(r.Addresses) != 2 || len(r.Certificates) != 2 {
		t.Fatalf("report = %+v", r)
	}
	if r.Certificates[0].SHA256 != b64('a') || !r.Certificates[1].NotAfter.Equal(time.Date(2026, 11, 29, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("certificates not carried in order: %+v", r.Certificates)
	}

	for name, c := range map[string]CapacityMsg{
		"transports absent":        capWithWT(t, nil, good),
		"webrtc only":              capWithWT(t, []string{"webrtc"}, good),
		"listed but no block":      capWithWT(t, []string{"webrtc", "webtransport"}, ""),
		"port out of range":        capWithWT(t, []string{"webtransport"}, `{"port":70000}`),
		"port zero":                capWithWT(t, []string{"webtransport"}, `{"port":0}`),
		"block of the wrong shape": capWithWT(t, []string{"webtransport"}, `"yes"`),
		"port of the wrong type":   capWithWT(t, []string{"webtransport"}, `{"port":"4433"}`),
	} {
		if got := webTransportReport(c); got != nil {
			t.Errorf("%s: report = %+v, want nil (the columns are cleared)", name, got)
		}
	}
}

// The address becomes part of a URL the browser opens: only literal, routable IPs get through.
func TestWebTransportReportKeepsOnlyUsableAddressesAndCertificates(t *testing.T) {
	var many []string
	for i := 1; i <= 12; i++ {
		many = append(many, "192.0.2."+string(rune('0'+i%10))+"0")
	}
	addrs := mustJSON(t, append([]string{
		"evil.example.com/x?", "127.0.0.1", "::1", "0.0.0.0", "169.254.1.1", "fe80::1", "not an ip", "", "https://x",
	}, many...))
	certs := `[{"sha256":"` + b64('a') + `","not_after":"2026-11-19T00:00:00Z"},
		{"sha256":"short","not_after":"2026-11-19T00:00:00Z"},
		{"sha256":"` + b64('b') + `","not_after":"nonsense"},
		{"sha256":"` + b64('c') + `"},
		{"sha256":"` + b64('d') + `","not_after":"2026-11-20T00:00:00Z"},
		{"sha256":"` + b64('e') + `","not_after":"2026-11-21T00:00:00Z"}]`
	r := webTransportReport(capWithWT(t, []string{"webtransport"}, `{"port":4433,"addresses":`+addrs+`,"certificates":`+certs+`}`))
	if r == nil {
		t.Fatal("nil report")
	}
	if len(r.Addresses) != maxWTAddresses {
		t.Errorf("addresses = %v, want exactly %d usable ones", r.Addresses, maxWTAddresses)
	}
	for _, a := range r.Addresses {
		if !strings.HasPrefix(a, "192.0.2.") {
			t.Errorf("address %q should have been dropped", a)
		}
	}
	if len(r.Certificates) != maxWTCertificates || r.Certificates[0].SHA256 != b64('a') || r.Certificates[1].SHA256 != b64('d') {
		t.Errorf("certificates = %+v, want the first two valid ones (a, d)", r.Certificates)
	}
}

// A report without WebTransport clears what an earlier one stored: replaced wholesale.
func TestUpsertHostWebTransportReplacesWholesale(t *testing.T) {
	pool := testPool(t)
	s := storeWithMintedTokens(pool, nil)
	hostID := seedHost(t, pool)
	ctx := context.Background()

	read := func() (transports, addrs, certs []byte, port *int) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT transports, wt_addresses, wt_certificates, wt_port FROM hosts WHERE id = $1::uuid`,
			hostID).Scan(&transports, &addrs, &certs, &port); err != nil {
			t.Fatal(err)
		}
		return
	}
	if tr, _, _, port := read(); tr != nil || port != nil {
		t.Fatalf("fresh host: transports %s port %v, want NULL", tr, port)
	}

	full := webTransportReport(capWithWT(t, []string{"webtransport"},
		`{"port":4433,"addresses":["192.0.2.10"],"certificates":[{"sha256":"`+b64('a')+`","not_after":"2026-11-19T00:00:00Z"}]}`))
	if err := s.upsertHostWebTransport(ctx, hostID, full); err != nil {
		t.Fatal(err)
	}
	tr, addrs, certs, port := read()
	if string(tr) != `["webrtc", "webtransport"]` || port == nil || *port != 4433 ||
		string(addrs) != `["192.0.2.10"]` || !strings.Contains(string(certs), b64('a')) {
		t.Fatalf("stored: %s %s %s %v", tr, addrs, certs, port)
	}

	// The next capacity has no WebTransport: everything goes back to NULL.
	if err := s.upsertHostWebTransport(ctx, hostID, webTransportReport(capWithWT(t, nil, ""))); err != nil {
		t.Fatal(err)
	}
	if tr, addrs, certs, port := read(); tr != nil || addrs != nil || certs != nil || port != nil {
		t.Errorf("after a report without webtransport: %s %s %s %v, want all NULL", tr, addrs, certs, port)
	}
}
