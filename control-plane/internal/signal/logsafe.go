package signal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"syscall"

	"github.com/gorilla/websocket"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
)

// safeErrTexts are the errors whose Error() text this package authored, so it
// is known to carry no peer data. Everything else is rendered structurally.
var safeErrTexts = []error{
	errNonTextFrame, errFrameTooLarge, errFrameNotUTF8, errFrameNotJSON,
	agentws.ErrAgentNotConnected, agentws.ErrSendQueueFull,
}

// logSafeErr renders a socket error for a DEFAULT-VISIBLE log line without the
// addresses net.OpError, net.DNSError and friends embed in Error(): the raw
// string of a transport error carries the client's IP and port, and these lines
// log at Info. Raw errors stay at Debug.
//
// Unrecognised errors render as their type, never their message — a wrapped
// transport error can carry an address anywhere in its text.
func logSafeErr(err error) string {
	if err == nil {
		return ""
	}
	for _, safe := range safeErrTexts {
		if errors.Is(err, safe) {
			return safe.Error()
		}
	}
	// The structural cases come FIRST: net.OpError unwraps to its cause, so a
	// category check ahead of it would swallow the Op ("deadline exceeded"
	// instead of "net read: deadline exceeded").
	//
	// CloseError.Text is peer-supplied; only the code is ours to log.
	var ce *websocket.CloseError
	if errors.As(err, &ce) {
		return fmt.Sprintf("websocket close %d", ce.Code)
	}
	// Name is the looked-up host, Server the resolver.
	var de *net.DNSError
	if errors.As(err, &de) {
		return "dns error"
	}
	// Addr/Source hold the peer address; Op and the unwrapped cause do not.
	var oe *net.OpError
	if errors.As(err, &oe) {
		if oe.Err != nil {
			return "net " + oe.Op + ": " + logSafeErr(oe.Err)
		}
		return "net " + oe.Op
	}
	switch {
	case errors.Is(err, os.ErrDeadlineExceeded):
		return "deadline exceeded"
	case errors.Is(err, net.ErrClosed):
		return "connection closed"
	case errors.Is(err, io.ErrUnexpectedEOF):
		return "unexpected eof"
	case errors.Is(err, io.EOF):
		return "eof"
	case errors.Is(err, syscall.ECONNRESET):
		return "connection reset"
	case errors.Is(err, syscall.EPIPE):
		return "broken pipe"
	case errors.Is(err, context.Canceled):
		return "context canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "context deadline exceeded"
	}
	return fmt.Sprintf("%T", err)
}
