package wtendpoint

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

func cert(sha string, notAfter time.Time) Certificate {
	return Certificate{SHA256: sha, NotBefore: notAfter.Add(-14 * 24 * time.Hour), NotAfter: notAfter}
}

func reported(addrs []string, certs ...Certificate) *Reported {
	return &Reported{Transports: []string{"webrtc", "webtransport"}, Port: 4433, Addresses: addrs, Certificates: certs}
}

func TestResolve(t *testing.T) {
	live := cert("AAAA", now.Add(24*time.Hour))
	next := cert("BBBB", now.Add(10*24*time.Hour))
	dead := cert("CCCC", now.Add(-time.Minute))
	tests := []struct {
		name       string
		host       Host
		wantURL    string
		wantMode   string
		wantHashes []string
		wantReason string
	}{
		{"direct v4", Host{Online: true, Reported: reported([]string{"192.0.2.10"}, live)},
			"https://192.0.2.10:4433/wt", ModeDirect, []string{"AAAA"}, ""},
		{"direct v6 is bracketed", Host{Online: true, Reported: reported([]string{"2001:db8::10"}, live)},
			"https://[2001:db8::10]:4433/wt", ModeDirect, []string{"AAAA"}, ""},
		{"first address wins", Host{Online: true, Reported: reported([]string{"192.0.2.10", "2001:db8::10"}, live)},
			"https://192.0.2.10:4433/wt", ModeDirect, []string{"AAAA"}, ""},
		{"rotation overlap sends both, expired one dropped", Host{Online: true, Reported: reported([]string{"192.0.2.10"}, dead, live, next)},
			"https://192.0.2.10:4433/wt", ModeDirect, []string{"AAAA", "BBBB"}, ""},
		{"agent presents a public certificate: no hashes", Host{Online: true, Reported: reported([]string{"192.0.2.10"})},
			"https://192.0.2.10:4433/wt", ModeDirect, nil, ""},
		{"edge override: URL as set, no hashes", Host{Online: true, Reported: reported(nil, live),
			Override: &Override{PublicURL: "https://play.example.com/wt/a", Certificate: CertPublic}},
			"https://play.example.com/wt/a", ModeEdge, nil, ""},
		{"agent-certificate override keeps the hashes", Host{Online: true, Reported: reported(nil, live),
			Override: &Override{PublicURL: "https://play.example.com:9000/wt", Certificate: CertAgent}},
			"https://play.example.com:9000/wt", ModeDirect, []string{"AAAA"}, ""},
		{"never reported", Host{Online: true}, "", "", nil, ReasonNotReported},
		{"reported without webtransport", Host{Online: true, Reported: &Reported{Transports: []string{"webrtc"}}}, "", "", nil, ReasonNotReported},
		{"offline", Host{Online: false, Reported: reported([]string{"192.0.2.10"}, live)}, "", "", nil, ReasonHostOffline},
		{"no address and no override", Host{Online: true, Reported: reported(nil, live)}, "", "", nil, ReasonNoAddress},
		{"every certificate expired", Host{Online: true, Reported: reported([]string{"192.0.2.10"}, dead)}, "", "", nil, ReasonCertificateExpired},
		{"expired certificate does not matter at the edge", Host{Online: true, Reported: reported(nil, dead),
			Override: &Override{PublicURL: "https://play.example.com/wt", Certificate: CertPublic}},
			"https://play.example.com/wt", ModeEdge, nil, ""},
		{"expired certificate matters for an agent-certificate override", Host{Online: true, Reported: reported(nil, dead),
			Override: &Override{PublicURL: "https://play.example.com/wt", Certificate: CertAgent}}, "", "", nil, ReasonCertificateExpired},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Resolve(tc.host, now)
			if got.Reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", got.Reason, tc.wantReason)
			}
			if tc.wantReason != "" {
				if got.Endpoint != nil {
					t.Fatalf("endpoint = %+v on a refusal, want nil", got.Endpoint)
				}
				return
			}
			if got.Endpoint == nil {
				t.Fatal("endpoint nil, want usable")
			}
			if got.Endpoint.URL != tc.wantURL || got.Endpoint.Mode != tc.wantMode {
				t.Errorf("endpoint = %s (%s), want %s (%s)", got.Endpoint.URL, got.Endpoint.Mode, tc.wantURL, tc.wantMode)
			}
			var hashes []string
			for _, h := range got.Endpoint.CertHashes {
				if h.Algorithm != "sha-256" {
					t.Errorf("hash algorithm = %q, want sha-256", h.Algorithm)
				}
				hashes = append(hashes, h.Value)
			}
			if !reflect.DeepEqual(hashes, tc.wantHashes) {
				t.Errorf("hashes = %v, want %v", hashes, tc.wantHashes)
			}
			if got.Endpoint.CertHashes == nil {
				t.Error("CertHashes is nil: it must serialize as [] (control-api.md: always serialized)")
			}
		})
	}
}

func TestValidatePublicURL(t *testing.T) {
	ok := []string{"https://play.example.com/wt/host-a", "https://play.example.com:9000/", "https://[2001:db8::1]/wt"}
	bad := map[string]string{
		"http scheme":   "http://play.example.com/wt",
		"no host":       "https:///wt",
		"user info":     "https://u:p@play.example.com/wt",
		"fragment":      "https://play.example.com/wt#x",
		"relative":      "/wt",
		"not a url":     "://",
		"too long":      "https://play.example.com/" + strings.Repeat("a", 2048),
		"wss scheme":    "wss://play.example.com/wt",
		"empty":         "",
		"space in host": "https://play example.com/wt",
	}
	for _, u := range ok {
		if err := ValidatePublicURL(u); err != nil {
			t.Errorf("ValidatePublicURL(%q) = %v, want nil", u, err)
		}
	}
	for name, u := range bad {
		if err := ValidatePublicURL(u); err == nil {
			t.Errorf("%s: ValidatePublicURL(%q) = nil, want an error", name, u)
		}
	}
}
