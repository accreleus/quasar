// guard_test.go — the dial-time guard, tested directly. These assertions moved
// verbatim from internal/images/digest_test.go with the code they cover (#105).
package outbound

import (
	"context"
	"net"
	"testing"
)

// TestGuardedDialRefusesNonPublicIP — the DNS-rebind guard: an allowlisted host
// that resolves to a non-public IP is refused at dial time, on every
// connection. The lookup is injected so no real DNS is needed.
func TestGuardedDialRefusesNonPublicIP(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "10.0.0.1", "192.168.1.1", "169.254.1.1", "::1", "0.0.0.0"} {
		lookup := func(context.Context, string) ([]net.IP, error) {
			return []net.IP{net.ParseIP(ip)}, nil
		}
		dial := guardedDialContext(lookup, nil)
		if _, err := dial(context.Background(), "tcp", "registry.example.com:443"); err == nil {
			t.Fatalf("guardedDialContext must refuse %s", ip)
		}
	}
}

// TestDisallowedIP pins the classification the dial guard depends on.
func TestDisallowedIP(t *testing.T) {
	disallowed := []string{"127.0.0.1", "::1", "10.1.2.3", "172.16.0.1", "192.168.9.9", "169.254.0.1", "224.0.0.1", "0.0.0.0"}
	for _, s := range disallowed {
		if !disallowedIP(net.ParseIP(s)) {
			t.Errorf("disallowedIP(%s) = false, want true", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "140.82.112.3"} {
		if disallowedIP(net.ParseIP(s)) {
			t.Errorf("disallowedIP(%s) = true, want false", s)
		}
	}
}
