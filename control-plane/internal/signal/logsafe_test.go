package signal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"testing"

	"github.com/gorilla/websocket"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
)

// The teardown lines log at Info, which is the default LOG_LEVEL, so a raw
// transport error there would put the client's address in every operator's log.
// net.OpError.Error() embeds Addr and Source verbatim.
func TestLogSafeErrOmitsPeerAddress(t *testing.T) {
	peer := &net.TCPAddr{IP: net.ParseIP("203.0.113.77"), Port: 51234}
	local := &net.TCPAddr{IP: net.ParseIP("198.51.100.9"), Port: 8443}
	opErr := &net.OpError{
		Op:     "read",
		Net:    "tcp",
		Source: local,
		Addr:   peer,
		Err:    os.ErrDeadlineExceeded,
	}
	// Guard the premise: if OpError ever stops embedding the address, this test
	// is no longer testing anything.
	if !strings.Contains(opErr.Error(), "203.0.113.77") {
		t.Fatalf("premise broken: net.OpError no longer embeds Addr: %v", opErr)
	}

	for _, err := range []error{opErr, fmt.Errorf("read from socket: %w", opErr)} {
		got := logSafeErr(err)
		for _, leaked := range []string{"203.0.113.77", "198.51.100.9", "51234", "8443"} {
			if strings.Contains(got, leaked) {
				t.Fatalf("logSafeErr(%v) = %q, leaks %q", err, got, leaked)
			}
		}
		if got == "" {
			t.Fatalf("logSafeErr(%v) rendered nothing; the line must still say something", err)
		}
	}
	if got := logSafeErr(opErr); got != "net read: deadline exceeded" {
		t.Fatalf("logSafeErr(opErr) = %q, want the Op plus the unwrapped cause", got)
	}
}

// A DNSError carries the looked-up name, which for a mDNS/ICE peer is the
// client's host name.
func TestLogSafeErrOmitsDNSName(t *testing.T) {
	err := &net.DNSError{Err: "no such host", Name: "abc-123.local", Server: "192.0.2.1"}
	got := logSafeErr(err)
	if strings.Contains(got, "abc-123.local") || strings.Contains(got, "192.0.2.1") {
		t.Fatalf("logSafeErr(%v) = %q, leaks the name or server", err, got)
	}
}

// CloseError.Text is peer-supplied, so only the code may be logged.
func TestLogSafeErrKeepsCloseCodeNotPeerText(t *testing.T) {
	err := &websocket.CloseError{Code: websocket.CloseAbnormalClosure, Text: "203.0.113.77 said so"}
	got := logSafeErr(err)
	if !strings.Contains(got, "1006") {
		t.Fatalf("logSafeErr(%v) = %q, want the close code", err, got)
	}
	if strings.Contains(got, "203.0.113.77") {
		t.Fatalf("logSafeErr(%v) = %q, leaks peer-supplied close text", err, got)
	}
}

// The categories an operator actually reads, and the fallback: an unrecognised
// error renders as its type, never its message.
func TestLogSafeErrCategories(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{nil, ""},
		{os.ErrDeadlineExceeded, "deadline exceeded"},
		{net.ErrClosed, "connection closed"},
		{io.EOF, "eof"},
		{io.ErrUnexpectedEOF, "unexpected eof"},
		{context.Canceled, "context canceled"},
		{agentws.ErrSendQueueFull, agentws.ErrSendQueueFull.Error()},
		{agentws.ErrAgentNotConnected, agentws.ErrAgentNotConnected.Error()},
		{errFrameNotJSON, errFrameNotJSON.Error()},
		{errors.New("peer 203.0.113.77:51234 went away"), "*errors.errorString"},
	}
	for _, c := range cases {
		if got := logSafeErr(c.err); got != c.want {
			t.Errorf("logSafeErr(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}
