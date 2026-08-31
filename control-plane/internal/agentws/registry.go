package agentws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// writeTimeout bounds a single frame write to a (possibly wedged) agent.
const writeTimeout = 10 * time.Second

// outBuffer is the per-connection outbound queue depth. Small: at N=1 the
// command rate is trivial; a full queue means a stuck agent and surfaces as an
// error the caller maps to a session failure.
const outBuffer = 32

var (
	// ErrAgentNotConnected is returned when no live agent connection exists for a
	// host (e.g. it dropped between scheduling and dispatch).
	ErrAgentNotConnected = errors.New("agent not connected")
	// ErrSendQueueFull is returned when an agent's outbound queue is saturated.
	ErrSendQueueFull = errors.New("agent send queue full")
)

// AckResult is the outcome of a command that requested an ack.
type AckResult struct {
	OK    bool
	Error string
}

// Registry tracks the live agent connections (one per host) and routes
// downstream commands to them, correlating acks back to waiters. It is the seam
// the session coordinator dispatches through; the coordinator never touches a
// websocket directly.
type Registry struct {
	mu    sync.Mutex
	conns map[string]*conn // hostID → live connection
	log   *slog.Logger
}

// NewRegistry builds an empty Registry.
func NewRegistry(log *slog.Logger) *Registry {
	return &Registry{conns: make(map[string]*conn), log: log}
}

// conn is one live agent connection. A single writer goroutine drains out; all
// sends enqueue onto it (gorilla allows only one concurrent writer).
type conn struct {
	hostID string
	ws     *websocket.Conn
	out    chan []byte
	done   chan struct{}

	mu     sync.Mutex
	closed bool
	acks   map[string]chan AckResult
}

func newConn(hostID string, ws *websocket.Conn) *conn {
	return &conn{
		hostID: hostID,
		ws:     ws,
		out:    make(chan []byte, outBuffer),
		done:   make(chan struct{}),
		acks:   make(map[string]chan AckResult),
	}
}

// add registers c as the live connection for its host, displacing (and closing)
// any prior one — a reconnect supersedes the stale connection.
func (r *Registry) add(c *conn) {
	r.mu.Lock()
	old := r.conns[c.hostID]
	r.conns[c.hostID] = c
	r.mu.Unlock()
	if old != nil {
		old.close()
	}
}

// remove drops c iff it is still the registered connection for its host (a
// newer reconnect must not be evicted by an older connection's teardown). It
// returns whether c was the current connection: false means c was already
// displaced by a reconnect, so its teardown must NOT trigger the host-disconnect
// reaper — the newer connection now owns the host's sessions (P2-06 race fix).
func (r *Registry) remove(c *conn) bool {
	r.mu.Lock()
	current := r.conns[c.hostID] == c
	if current {
		delete(r.conns, c.hostID)
	}
	r.mu.Unlock()
	c.close()
	return current
}

func (r *Registry) get(hostID string) (*conn, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.conns[hostID]
	return c, ok
}

// IsConnected reports whether the host currently has a live agent connection.
// Use this for the online-ness guard before deleting a host record — it reads
// the live connection map rather than the DB status column, so it correctly
// handles a reconnect race (a host that reconnects mid-delete attempt appears
// connected here even if the DB row still says "offline").
func (r *Registry) IsConnected(hostID string) bool {
	_, ok := r.get(hostID)
	return ok
}

// Send marshals v and enqueues it to the host's agent (fire-and-forget).
func (r *Registry) Send(hostID string, v any) error {
	c, ok := r.get(hostID)
	if !ok {
		return ErrAgentNotConnected
	}
	return c.enqueue(v)
}

// SendWithAck sends v (which must carry the given command id) and waits for the
// agent's ack, the context deadline, or the connection closing. The boolean in
// AckResult is the agent's accept/reject; a returned error means the command
// could not be delivered or no ack arrived in time.
func (r *Registry) SendWithAck(ctx context.Context, hostID, id string, v any) (AckResult, error) {
	c, ok := r.get(hostID)
	if !ok {
		return AckResult{}, ErrAgentNotConnected
	}
	ch := make(chan AckResult, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return AckResult{}, ErrAgentNotConnected
	}
	c.acks[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.acks, id)
		c.mu.Unlock()
	}()

	if err := c.enqueue(v); err != nil {
		return AckResult{}, err
	}

	select {
	case res := <-ch:
		return res, nil
	case <-ctx.Done():
		return AckResult{}, fmt.Errorf("ack wait for %s: %w", id, ctx.Err())
	case <-c.done:
		return AckResult{}, ErrAgentNotConnected
	}
}

// resolveAck delivers an ack to a waiting SendWithAck, if any.
func (r *Registry) resolveAck(hostID, id string, res AckResult) {
	c, ok := r.get(hostID)
	if !ok {
		return
	}
	c.mu.Lock()
	ch := c.acks[id]
	c.mu.Unlock()
	if ch != nil {
		select {
		case ch <- res:
		default:
		}
	}
}

// enqueue marshals v and pushes it onto the writer queue without blocking.
func (c *conn) enqueue(v any) error {
	frame, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal command: %w", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ErrAgentNotConnected
	}
	select {
	case c.out <- frame:
		return nil
	default:
		return ErrSendQueueFull
	}
}

// close marks the connection closed and stops its writer. Idempotent. Pending
// ack waiters unblock via the done channel.
func (c *conn) close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	close(c.done)
	c.mu.Unlock()
}

// runWriter is the sole writer for the connection: it drains the outbound queue
// to the websocket until the connection closes.
func (c *conn) runWriter(log *slog.Logger) {
	for {
		select {
		case frame := <-c.out:
			_ = c.ws.SetWriteDeadline(time.Now().Add(writeTimeout))
			if err := c.ws.WriteMessage(websocket.TextMessage, frame); err != nil {
				log.Warn("agent write failed", "host_id", c.hostID, "err", err)
				c.close()
				return
			}
		case <-c.done:
			return
		}
	}
}
