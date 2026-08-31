package agentws

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// relayBufMax is the per-session frame buffer while no browser is registered.
// A dual-PC session can emit >64 frames before the browser connects (one video
// offer + one audio offer plus ICE candidates for every host/bridge address).
// Keep the queue bounded but large enough for the observed 68-frame normal burst.
const relayBufMax = 256

const relayDeliveryTimeout = time.Second

// RelayBus routes signaling frames from the agent to the right browser WebSocket
// client, buffering frames that arrive before the browser connects (the agent's
// offer is ready as soon as the pipeline is live — often before the browser WS
// is established). Thread-safe; shared between the agent WS handler (writes) and
// the browser signal handler (reads/registers).
// browserReg is one registered browser channel, plus the signal RelayBus
// closes when a LATER Register call for the same session displaces it (#415).
type browserReg struct {
	ch        chan<- []byte
	displaced chan struct{}
}

type RelayBus struct {
	mu       sync.Mutex
	log      *slog.Logger
	pending  map[string][][]byte   // session_id → buffered frames (no browser yet)
	browsers map[string]browserReg // session_id → live browser registration
}

// NewRelayBus builds an empty relay bus.
func NewRelayBus(log *slog.Logger) *RelayBus {
	return &RelayBus{
		log:      log,
		pending:  make(map[string][][]byte),
		browsers: make(map[string]browserReg),
	}
}

// Register registers ch as the browser's receive channel for a session. Any
// frames buffered while the browser was absent are drained into ch immediately.
//
// It returns a "displaced" signal: closed once (if ever) a LATER Register call
// for the SAME session id supersedes this one — a client reconnect (WiFi→LTE,
// VPN flap) that mints a new signaling token and dials a new socket while the
// old one is still half-open.
//
// #415: before this, Unregister deleted by session id alone, so the OLD
// socket's deferred cleanup (firing up to browserReadTimeout later) deleted
// the NEW socket's channel — the live browser then received zero agent frames
// (no ICE, no renegotiation, no bye), silently, with the session still
// `running`. Mirrors agentws.Registry.add/remove's identity-checked
// displacement for agent connections: the caller (signal/handler.go) selects
// on the returned channel and tears its own connection down immediately on
// displacement, instead of waiting out a read deadline while deaf.
func (b *RelayBus) Register(sessionID string, ch chan<- []byte) <-chan struct{} {
	b.mu.Lock()
	old, hadOld := b.browsers[sessionID]
	reg := browserReg{ch: ch, displaced: make(chan struct{})}
	b.browsers[sessionID] = reg
	for _, frame := range b.pending[sessionID] {
		select {
		case ch <- frame:
		default:
		}
	}
	delete(b.pending, sessionID)
	b.mu.Unlock()
	if hadOld {
		close(old.displaced)
	}
	return reg.displaced
}

// Unregister drops ch's registration for sessionID iff it is still the
// CURRENT one — a stale connection's deferred Unregister must not evict a
// newer reconnect's channel (#415; mirrors agentws.Registry.remove's identity
// check). A call from an already-displaced connection is a no-op: the
// registration it would have removed is already gone.
func (b *RelayBus) Unregister(sessionID string, ch chan<- []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if reg, ok := b.browsers[sessionID]; ok && reg.ch == ch {
		delete(b.browsers, sessionID)
		delete(b.pending, sessionID)
	}
}

// Forget drops every trace of a session from the bus (#402). It is the
// SESSION-TERMINAL eviction path, and it is not the same thing as Unregister:
// Unregister is driven by a browser WebSocket closing, so a session whose
// browser never attached at all — every headless launch, every soak cycle —
// never reaches it, and any agent frame arriving after a browser detaches
// re-creates the pending entry that Unregister just deleted. Without this the
// buffered frames (up to relayBufMax each) are retained for the life of the
// process, one entry per session ever launched.
//
// Idempotent and safe to call for a session that never relayed anything, which
// is the common case: the coordinator fires it on every terminal transition.
// Takes only b.mu — callers must not hold another lock that could be taken
// under it (nothing here calls out).
func (b *RelayBus) Forget(sessionID string) {
	b.mu.Lock()
	delete(b.pending, sessionID)
	delete(b.browsers, sessionID)
	b.mu.Unlock()
}

// Deliver routes a raw inner-message frame (the Phase 0 signaling JSON) to the
// browser. If no browser is registered yet the frame is buffered up to
// relayBufMax; frames beyond that are dropped with a warning.
func (b *RelayBus) Deliver(sessionID string, frame []byte) error {
	b.mu.Lock()
	if reg, ok := b.browsers[sessionID]; ok {
		b.mu.Unlock()
		select {
		case reg.ch <- frame:
			return nil
		case <-time.After(relayDeliveryTimeout):
			return fmt.Errorf("relay browser queue blocked for session %s", sessionID)
		}
	}
	// No browser yet — buffer.
	if len(b.pending[sessionID]) >= relayBufMax {
		b.mu.Unlock()
		return fmt.Errorf("relay pending queue full for session %s", sessionID)
	}
	cp := make([]byte, len(frame))
	copy(cp, frame)
	b.pending[sessionID] = append(b.pending[sessionID], cp)
	b.mu.Unlock()
	return nil
}

// SendSignaling wraps innerMsg (raw Phase 0 JSON) in a signaling envelope and
// sends it to the agent for the given host. Called by the browser signal handler
// to forward answer/ICE to the node agent.
func (r *Registry) SendSignaling(hostID, sessionID string, innerMsg json.RawMessage) error {
	env := SignalingEnvelope{
		Type:      "signaling",
		SessionID: sessionID,
		Msg:       innerMsg,
	}
	frame, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal signaling envelope: %w", err)
	}
	c, ok := r.get(hostID)
	if !ok {
		return ErrAgentNotConnected
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
