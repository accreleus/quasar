package agentws

import (
	"errors"
	"strings"
	"testing"
)

type fakeTimeout struct{}

func (fakeTimeout) Error() string   { return "read tcp: i/o timeout" }
func (fakeTimeout) Timeout() bool   { return true }
func (fakeTimeout) Temporary() bool { return true }

// TestDescribeHandshakeReadNamesTheTimeout (#191): a handshake read that timed out
// is explained in words an operator can act on; any other read error is untouched.
func TestDescribeHandshakeReadNamesTheTimeout(t *testing.T) {
	got := describeHandshakeRead(fakeTimeout{})
	if !strings.Contains(got.Error(), "no register within") || !strings.Contains(got.Error(), "#191") {
		t.Fatalf("timeout not described: %v", got)
	}
	var ne fakeTimeout
	if !errors.As(got, &ne) {
		t.Fatalf("the original error must stay in the chain: %v", got)
	}
	plain := errors.New("websocket: close 1006 (abnormal closure)")
	if describeHandshakeRead(plain) != plain {
		t.Fatalf("a non-timeout error must pass through unchanged")
	}
}
