package agentws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/hostcfg"
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
	mu         sync.Mutex
	conns      map[string]*conn // hostID → live connection
	lifecycles map[string]*hostLifecycle
	log        *slog.Logger
}

// hostLifecycle serializes only callbacks which can change a host's session
// lifecycle. It is reference-counted so a queued stale callback cannot race a
// replacement through a newly allocated lock after the old connection leaves.
type hostLifecycle struct {
	mu    sync.Mutex
	users int
}

// NewRegistry builds an empty Registry.
func NewRegistry(log *slog.Logger) *Registry {
	return &Registry{conns: make(map[string]*conn), lifecycles: make(map[string]*hostLifecycle), log: log}
}

// conn is one live agent connection. A single writer goroutine drains out; all
// sends enqueue onto it (gorilla allows only one concurrent writer).
type conn struct {
	hostID                    string
	terminalHomeCleanupV1     bool
	policyTyped               bool
	policyIdle                bool
	policyAccepted            []string
	policyAcknowledged        atomic.Bool
	policyInitialMapApplied   atomic.Bool
	policyInventoryDone       atomic.Bool
	policyInventoryBlocked    atomic.Bool
	policyInventoryUnknown    bool
	policyAttemptOutstanding  atomic.Bool
	policyInventoryID         string
	policyInventorySnapshotID string
	policyInventoryCursor     *string
	policyInventoryHeader     []byte
	rh05RestartEntries        []hostcfg.JournalInventoryEntry
	rh05Snapshots             map[string]hostcfg.PolicySnapshot
	policyActiveSnapshots     atomic.Pointer[map[string]hostcfg.PolicySnapshot]
	policyOutstanding         map[string]ConfigPolicyStateMsg
	policySequence            map[string]uint64
	policySequenceContent     map[string][]byte
	policyUncertain           bool
	policyRefreshPending      bool
	policyDeliveryID          string
	policyDeliverySentAt      time.Time
	bootIncarnation           string
	connectionIncarnation     string
	ws                        *websocket.Conn
	out                       chan []byte
	done                      chan struct{}

	mu                      sync.Mutex
	closed                  bool
	acks                    map[string]chan AckResult
	imageCleanupV1          bool
	imageVersionsComplete   bool
	imageVersionsRevision   uint64
	imageVersionsObservedAt time.Time
	imageVersions           []ImageVersionEntry
	imageReconcilePending   map[string]bool
	imageReconcileAwaiting  bool
	imageReconciledRevision uint64
}

// SupportsTypedSettings describes the current authenticated connection only.
// A reconnect with an older agent replaces the capability immediately.
func (r *Registry) SupportsTypedSettings(hostID string) bool {
	c, ok := r.get(hostID)
	return ok && c.policyTyped
}

func (r *Registry) PolicyIdentity(hostID string) (string, string, bool) {
	c, ok := r.get(hostID)
	if !ok || !c.policyTyped {
		return "", "", false
	}
	return c.bootIncarnation, c.connectionIncarnation, true
}

// PolicyActiveSnapshots returns only the authenticated current connection's
// completed journal inventory snapshots, keyed by group. The map is never
// mutated after publication.
func (r *Registry) PolicyActiveSnapshots(hostID, connectionID string) map[string]hostcfg.PolicySnapshot {
	c, ok := r.get(hostID)
	if !ok || !c.policyTyped || c.connectionIncarnation != connectionID || !c.policyInventoryDone.Load() || c.policyInventoryBlocked.Load() || c.policyAttemptOutstanding.Load() {
		return nil
	}
	return c.activePolicySnapshots()
}

func (c *conn) activePolicySnapshots() map[string]hostcfg.PolicySnapshot {
	if snapshots := c.policyActiveSnapshots.Load(); snapshots != nil {
		return *snapshots
	}
	return nil
}

// setActivePolicySnapshot publishes a copy with group replaced; readers on
// other goroutines keep the map they loaded.
func (c *conn) setActivePolicySnapshot(group string, snapshot hostcfg.PolicySnapshot) {
	next := map[string]hostcfg.PolicySnapshot{}
	for key, value := range c.activePolicySnapshots() {
		next[key] = value
	}
	next[group] = snapshot
	c.policyActiveSnapshots.Store(&next)
}

func (r *Registry) PolicyRestartConflict(hostID string) bool {
	c, ok := r.get(hostID)
	return ok && c.policyTyped && (!c.policyInventoryDone.Load() || c.policyInventoryBlocked.Load() || c.policyAttemptOutstanding.Load())
}

// PolicyLegacyDelivery exposes only current-connection writer ownership and
// whether the initial full-map inventory handshake permits later maps.
func (r *Registry) PolicyLegacyDelivery(hostID string) (string, []string, bool, bool) {
	c, ok := r.get(hostID)
	if !ok || !c.policyTyped {
		return "", nil, false, false
	}
	return c.connectionIncarnation, append([]string(nil), c.policyAccepted...), c.policyAcknowledged.Load() && c.policyInventoryDone.Load() && !c.policyInventoryBlocked.Load(), true
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

// acquireLifecycle holds a per-host lifecycle callback gate without retaining
// Registry.mu while the callback can dispatch.
func (r *Registry) acquireLifecycle(hostID string) *hostLifecycle {
	r.mu.Lock()
	gate := r.lifecycles[hostID]
	if gate == nil {
		gate = &hostLifecycle{}
		r.lifecycles[hostID] = gate
	}
	gate.users++
	r.mu.Unlock()
	gate.mu.Lock()
	return gate
}

func (r *Registry) releaseLifecycle(hostID string, gate *hostLifecycle) {
	gate.mu.Unlock()
	r.mu.Lock()
	gate.users--
	if gate.users == 0 && r.lifecycles[hostID] == gate {
		delete(r.lifecycles, hostID)
	}
	r.mu.Unlock()
}

// add registers c as the live connection for its host, displacing (and closing)
// any prior one — a reconnect supersedes the stale connection.
func (r *Registry) add(c *conn) {
	gate := r.acquireLifecycle(c.hostID)
	defer r.releaseLifecycle(c.hostID, gate)
	r.mu.Lock()
	old := r.conns[c.hostID]
	r.conns[c.hostID] = c
	r.mu.Unlock()
	if old != nil {
		old.close()
	}
}

// withCurrent serializes lifecycle callbacks with reconnect replacement. It
// never holds Registry.mu across callback work, so callbacks may dispatch.
func (r *Registry) withCurrent(c *conn, callback func()) bool {
	gate := r.acquireLifecycle(c.hostID)
	defer r.releaseLifecycle(c.hostID, gate)
	r.mu.Lock()
	current := r.conns[c.hostID] == c
	r.mu.Unlock()
	if !current {
		return false
	}
	callback()
	return true
}

// remove drops c iff it is still the registered connection for its host (a
// newer reconnect must not be evicted by an older connection's teardown). It
// returns whether c was the current connection: false means c was already
// displaced by a reconnect, so its teardown must NOT trigger the host-disconnect
// reaper — the newer connection now owns the host's sessions (P2-06 race fix).
func (r *Registry) remove(c *conn) bool {
	return r.removeWithLifecycle(c, func() {})
}

// removeWithLifecycle linearizes removal and its HostDisconnected callback with
// reconnect and inbound lifecycle work for this host.
func (r *Registry) removeWithLifecycle(c *conn, callback func()) bool {
	gate := r.acquireLifecycle(c.hostID)
	defer r.releaseLifecycle(c.hostID, gate)
	r.mu.Lock()
	current := r.conns[c.hostID] == c
	if current {
		delete(r.conns, c.hostID)
	}
	r.mu.Unlock()
	c.close()
	if current {
		callback()
	}
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

// CurrentHomeCleanupCapability describes only the authenticated connection
// serving this host right now. A disconnected host has no known capability.
func (r *Registry) CurrentHomeCleanupCapability(hostID string) string {
	c, ok := r.get(hostID)
	if !ok {
		return "unknown"
	}
	if c.terminalHomeCleanupV1 {
		return "supported"
	}
	return "unsupported"
}

// HomeCommandEpoch binds a managed-home command to the exact authenticated
// agent connection whose cleanup capability was used for the DB decision.
// The boolean from SendWithAck says a frame entered the socket writer queue;
// from that point delivery is uncertain even on timeout or disconnect.
type HomeCommandEpoch interface {
	SupportsHomeCleanup() bool
	Send(any) (bool, error)
	SendWithAck(context.Context, string, any) (AckResult, bool, error)
}

type homeCommandEpoch struct {
	registry *Registry
	conn     *conn
}

func (e *homeCommandEpoch) SupportsHomeCleanup() bool { return e.conn.terminalHomeCleanupV1 }

func (r *Registry) CurrentHomeCommandEpoch(hostID string) (HomeCommandEpoch, bool) {
	c, ok := r.get(hostID)
	if !ok {
		return nil, false
	}
	return &homeCommandEpoch{registry: r, conn: c}, true
}

func (e *homeCommandEpoch) Send(v any) (bool, error) {
	var queued bool
	var sendErr error
	current := e.registry.withCurrent(e.conn, func() {
		sendErr = e.conn.enqueue(v)
		queued = sendErr == nil
	})
	if !current {
		return false, ErrAgentNotConnected
	}
	return queued, sendErr
}

func (e *homeCommandEpoch) SendWithAck(ctx context.Context, id string, v any) (AckResult, bool, error) {
	c := e.conn
	r := e.registry
	ch := make(chan AckResult, 1)
	var queued bool
	var enqueueErr error
	current := r.withCurrent(c, func() {
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			enqueueErr = ErrAgentNotConnected
			return
		}
		c.acks[id] = ch
		c.mu.Unlock()
		if err := c.enqueue(v); err != nil {
			c.mu.Lock()
			delete(c.acks, id)
			c.mu.Unlock()
			enqueueErr = err
			return
		}
		queued = true
	})
	if !current {
		return AckResult{}, false, ErrAgentNotConnected
	}
	if enqueueErr != nil {
		return AckResult{}, false, enqueueErr
	}
	defer func() {
		c.mu.Lock()
		delete(c.acks, id)
		c.mu.Unlock()
	}()
	select {
	case result := <-ch:
		if got, ok := r.get(c.hostID); !ok || got != c {
			return AckResult{}, queued, ErrAgentNotConnected
		}
		return result, queued, nil
	case <-ctx.Done():
		return AckResult{}, queued, fmt.Errorf("ack wait for %s: %w", id, ctx.Err())
	case <-c.done:
		return AckResult{}, queued, ErrAgentNotConnected
	}
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
	r.resolveAckFromConn(c, id, res)
}

func (r *Registry) resolveAckFromConn(c *conn, id string, res AckResult) {
	if current, ok := r.get(c.hostID); !ok || current != c {
		return
	}
	c.mu.Lock()
	if c.imageReconcilePending[id] {
		delete(c.imageReconcilePending, id)
		if res.OK {
			// The agent updates its scanned inventory before sending this ack.
			// Its following image_versions_state is ordered on this connection.
			c.imageReconcileAwaiting = true
		}
	}
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
