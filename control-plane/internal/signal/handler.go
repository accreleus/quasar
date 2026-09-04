// Package signal implements the P1-D authenticated WebRTC signaling endpoint.
//
// The browser connects to GET /v1/signal?token=<single-use>. The control plane
// validates and atomically consumes the token, then bridges Phase 0 signaling
// messages (offer/answer/ice/bye/error) between the browser WebSocket and the
// node agent via agentws.RelayBus / agentws.Registry. Neither end sees the relay.
//
// WS close codes (per signaling.md P1-D):
//
//	4401  token invalid / expired / already consumed
//	4404  session not found or terminal — at attach, and (#93) mid-session the
//	      moment the session reaches a terminal state
//	4409  session not yet assigned to a host (retry shortly)
//	4410  this attachment was taken over by a later attach (#526)
//	4500  relay to agent unavailable (host offline, or its inbound queue is
//	      saturated — #93)
//
// 4410 is an additive application close code, not a row in signaling.md's
// frozen table — no shape or meaning changes, and an unaware client lands in
// the "unrecognised code" arm it already had. The signaling.md amendment is a
// sign-off-gated follow-up; 1000 could never express it, being
// indistinguishable from an ordinary hang-up (which is why the displaced tab
// treated it as recoverable and re-minted).
package signal

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
	"github.com/accreleus/quasar/control-plane/internal/httpx"
	"github.com/accreleus/quasar/control-plane/internal/origins"
	"github.com/accreleus/quasar/control-plane/internal/ratelimit"
	"github.com/accreleus/quasar/control-plane/internal/session"
)

const (
	wsCloseTokenInvalid = 4401
	wsCloseNotFound     = 4404
	wsCloseNotAssigned  = 4409
	// wsCloseTakenOver tells a displaced browser that a LATER attach owns this
	// session's signaling now (#526). It is terminal by construction: the client
	// must render "taken over elsewhere" and must NOT mint a replacement token,
	// because minting is what makes two tabs evict each other forever.
	wsCloseTakenOver    = 4410
	wsCloseRelayUnavail = 4500

	browserWriteTimeout = 10 * time.Second
	browserReadTimeout  = 120 * time.Second
	browserPingPeriod   = 30 * time.Second
	// Must be >= agentws relayBufMax so Register can drain every pre-browser
	// signaling frame without its nonblocking send dropping the tail.
	relayBufSize            = 256
	browserReadLimit        = 256 << 10
	signalFailureLimit      = 10
	signalFailureTTL        = time.Minute
	signalFailureMaxIPs     = 4096
	signalMaxInFlightIP     = 10
	signalMaxInFlight       = 256
	signalValidationTimeout = 15 * time.Second
)

// validateBrowserFrame is deliberately small and side-effect free: it is used
// by the socket reader before any relay work and is also the boundary exercised
// by the deterministic fuzz regression. Gorilla enforces browserReadLimit while
// reading, but retain the length check here so callers of this boundary cannot
// accidentally bypass the cap later.
func validateBrowserFrame(messageType int, frame []byte) (int, string, error) {
	if messageType != websocket.TextMessage {
		return websocket.CloseUnsupportedData, "text JSON messages required", errors.New("non-text WebSocket message rejected")
	}
	if len(frame) > browserReadLimit {
		return websocket.CloseMessageTooBig, "message too large", errors.New("WebSocket message exceeds browser read limit")
	}
	if !utf8.Valid(frame) {
		return websocket.CloseUnsupportedData, "UTF-8 text required", errors.New("invalid UTF-8 WebSocket text rejected")
	}
	if !json.Valid(frame) {
		return websocket.CloseUnsupportedData, "valid JSON required", errors.New("invalid JSON WebSocket message rejected")
	}
	return 0, "", nil
}

// agentSender is the browser→agent half of the relay: everything this handler
// needs from agentws.Registry. Narrow on purpose — it is also the seam the
// tests use to observe exactly which browser frames reach an agent (the #505
// suppression is invisible from the browser side by construction).
type agentSender interface {
	SendSignaling(hostID, sessionID string, innerMsg json.RawMessage) error
}

// Handler serves GET /v1/signal.
type Handler struct {
	store       *session.Store
	registry    agentSender
	relay       *agentws.RelayBus
	log         *slog.Logger
	failures    *ratelimit.FailureLimiter
	upgrader    websocket.Upgrader
	readTimeout time.Duration
	pingPeriod  time.Duration

	// resolver owns the allow-list (internal/origins); this handler only
	// enforces. internal/access presents the same instance's Decision, so the
	// diagnostic panel and the socket cannot disagree — they previously did,
	// over whether same-origin comparison includes the port. Never nil:
	// NewHandler panics otherwise.
	resolver *origins.Resolver

	// Test hook (#422): fires best-effort once one handshake's limiter
	// bookkeeping has been applied — the WS upgrade is observable before that
	// bookkeeping runs, so tests cannot infer settled state from Dial
	// returning. nil in production; the send is skipped.
	authSettled chan string

	// trustedProxies is the #438 trusted-proxy policy; nil means "key on the
	// direct peer" (today's behaviour, correct for a direct-LAN deployment).
	trustedProxies []*net.IPNet
}

// WithTrustedProxies configures which direct peers are reverse proxies whose
// X-Forwarded-For may be believed when keying the handshake failure limiter
// (#438). Unset, every browser behind the hardened Caddy overlay shares one
// signaling budget.
func (h *Handler) WithTrustedProxies(nets []*net.IPNet) *Handler {
	h.trustedProxies = nets
	return h
}

// NewHandler builds the signaling handler. The resolver is a required
// parameter, not an option: an optional wiring call was once forgotten and the
// socket silently enforced a different allow-list from the one internal/access
// reported. A nil resolver panics rather than defaulting to anything — a
// private fallback resolver would reintroduce the divergence, and an error
// here would be ignored into the same outcome. Fail loudly, at boot.
func NewHandler(store *session.Store, registry *agentws.Registry, relay *agentws.RelayBus,
	log *slog.Logger, resolver *origins.Resolver) *Handler {
	if resolver == nil {
		panic("signal.NewHandler: a shared *origins.Resolver is required — " +
			"internal/access must present decisions from the SAME instance this handler enforces with")
	}
	h := &Handler{store: store, registry: registry, relay: relay, log: log,
		failures:    ratelimit.NewFailureLimiter(signalFailureLimit, signalFailureTTL, signalFailureMaxIPs),
		readTimeout: browserReadTimeout,
		pingPeriod:  browserPingPeriod,
		resolver:    resolver}
	h.upgrader.CheckOrigin = h.originAllowed
	return h
}

// Register wires GET /v1/signal onto mux.
func (h *Handler) Register(mux httpx.Router) {
	mux.HandleFunc("GET /v1/signal", h.ServeHTTP)
}

// notifyAuthSettled is a best-effort, non-blocking test hook — see the
// authSettled field doc. It is a no-op unless a test has set authSettled.
func (h *Handler) notifyAuthSettled(clientIP string) {
	if h.authSettled == nil {
		return
	}
	select {
	case h.authSettled <- clientIP:
	default:
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.originAllowed(r) {
		http.Error(w, "signaling origin not allowed", http.StatusForbidden)
		return
	}
	clientIP := ratelimit.ClientIP(r, h.trustedProxies)
	if !h.failures.Reserve(clientIP, signalMaxInFlightIP, signalMaxInFlight) {
		http.Error(w, "too many failed signaling handshakes; try again later", http.StatusTooManyRequests)
		return
	}
	token := r.URL.Query().Get("token")
	if token == "" {
		h.failures.Release(clientIP)
		h.failures.Failure(clientIP)
		http.Error(w, "token query parameter required", http.StatusBadRequest)
		return
	}
	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		h.failures.Release(clientIP)
		return
	}
	defer conn.Close()
	conn.SetReadLimit(browserReadLimit)
	// Long-lived WS connections outlive the HTTP request context.
	h.handle(context.Background(), conn, token, clientIP)
}

// originAllowed is pure ENFORCEMENT: it asks the shared resolver and acts on
// the verdict. Every rule behind that verdict — precedence, normalization, the
// same-origin exemption, the no-Origin allowance — lives in internal/origins.
func (h *Handler) originAllowed(r *http.Request) bool {
	return h.resolver.Decide(r.Context(), r.Header.Get("Origin"), r.Host).Allowed
}

func (h *Handler) handle(ctx context.Context, conn *websocket.Conn, token, clientIP string) {
	// Empty until the token resolves; wsClose closes over it so every close
	// line names its session (#93 — a close log with no session id cannot be
	// correlated with a client's report).
	var sessionID string
	// Every socket teardown logs at Info with a `reason` naming WHO closed and
	// WHY. #93 was undiagnosable because the relay's failure exits logged at
	// Debug (invisible under the default LOG_LEVEL=info) and sent no close
	// frame, so the client saw only a transport close with no closing handshake.
	wsClose := func(code int, msg, reason string) {
		_ = conn.SetWriteDeadline(time.Now().Add(browserWriteTimeout))
		_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(code, msg))
		h.log.Info("signaling WS closed by control plane",
			"token", "signal-ws-closed", "reason", reason,
			"session_id", sessionID, "code", code, "detail", msg)
	}
	// closedNoFrame is the other kind of exit: the transport already failed, so
	// no close frame can reach the client. Logged at the same level, because
	// telling these two apart from the host side is the whole point.
	closedNoFrame := func(reason string, err error) {
		h.log.Info("signaling WS dropped without a close frame",
			"token", "signal-ws-dropped", "reason", reason,
			"session_id", sessionID, "err", err)
	}

	// --- 1. Validate + atomically consume the token ---
	validationCtx, cancelValidation := context.WithTimeout(ctx, signalValidationTimeout)
	sess, err := h.store.ConsumeSignalingToken(validationCtx, token)
	cancelValidation()
	h.failures.Release(clientIP)
	if errors.Is(err, session.ErrTokenInvalid) {
		// The only outcome that is actual evidence of a bad/guessed
		// credential — count it against the limiter.
		h.failures.Failure(clientIP)
		h.notifyAuthSettled(clientIP)
		wsClose(wsCloseTokenInvalid, "token invalid, expired, or already used", "token-invalid")
		return
	}
	// Every other outcome means the token was not proven invalid — not
	// brute-force evidence. Forget prior failures so a legitimate client is
	// never left rate-limited by an unrelated earlier guess or an infra
	// hiccup (#422: skipping Forget on a generic error left a stale count).
	h.failures.Forget(clientIP)
	h.notifyAuthSettled(clientIP)
	switch {
	case errors.Is(err, session.ErrSessionTerminal):
		wsClose(wsCloseNotFound, "session is terminal", "terminal-at-attach")
		return
	case errors.Is(err, session.ErrSessionNotReady):
		wsClose(wsCloseNotAssigned, "session not yet assigned to a host", "not-assigned")
		return
	case err != nil:
		h.log.Error("consume signaling token", "err", err)
		wsClose(wsCloseNotFound, "internal error", "token-lookup-failed")
		return
	}

	if sess.HostID == nil {
		wsClose(wsCloseNotAssigned, "session has no host", "no-host")
		return
	}
	hostID := *sess.HostID
	sessionID = sess.ID
	h.log.Info("signaling WS established", "session_id", sess.ID, "host_id", hostID)

	// A healthy established session can be completely quiet on the signaling
	// socket after SDP/ICE exchange: media and input flow peer-to-peer. Keep the
	// relay alive with protocol-level Ping/Pong frames so the read deadline is a
	// dead-peer detector, not an unconditional two-minute session lifetime.
	_ = conn.SetReadDeadline(time.Now().Add(h.readTimeout))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(h.readTimeout))
	})
	ping := time.NewTicker(h.pingPeriod)
	defer ping.Stop()

	// --- 2. Register on relay bus; any buffered agent frames are drained on
	//        register (the offer may already be waiting from pipeline start) ---
	fromAgent := make(chan []byte, relayBufSize)
	signals := h.relay.Register(sess.ID, fromAgent)
	defer h.relay.Unregister(sess.ID, fromAgent)

	// #505: which PCs have an offer outstanding on THIS socket. See
	// negotiation.go for why the relay tracks it.
	negotiations := newNegotiationState()
	writeToBrowser := func(frame []byte) error {
		negotiations.noteAgentFrame(frame)
		_ = conn.SetWriteDeadline(time.Now().Add(browserWriteTimeout))
		return conn.WriteMessage(websocket.TextMessage, frame)
	}

	// drainAgentFrames writes every frame the bus has already handed us and
	// returns once the queue is momentarily empty.
	drainAgentFrames := func() error {
		for {
			select {
			case frame := <-fromAgent:
				if err := writeToBrowser(frame); err != nil {
					return err
				}
			default:
				return nil
			}
		}
	}

	// --- 2a. Flush the frames Register just drained, before the browser
	// reader exists (#505 ordering): this makes "was an offer outstanding when
	// the browser's first frame arrived?" a fact rather than Go's uniform
	// select choice between two ready cases — precisely the coin-flip #505 is
	// about.
	if err := drainAgentFrames(); err != nil {
		closedNoFrame("buffered-write-failed", err)
		return
	}

	// --- 3. Goroutine: read from browser, validate JSON, forward to agent ---
	type readResult struct {
		frame []byte
		err   error
	}
	browserIn := make(chan readResult, 1)
	// done unblocks the reader if the pump loop exits while the reader is
	// parked on the channel send (#148) — otherwise it lingers until the
	// deferred conn.Close() errors the *next* read, which never comes because
	// it is blocked on the send, not on I/O.
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			conn.SetReadDeadline(time.Now().Add(h.readTimeout))
			messageType, frame, err := conn.ReadMessage()
			if err == nil {
				closeCode, closeMessage, validationErr := validateBrowserFrame(messageType, frame)
				if validationErr != nil {
					_ = conn.WriteControl(websocket.CloseMessage,
						websocket.FormatCloseMessage(closeCode, closeMessage),
						time.Now().Add(browserWriteTimeout))
					err = validationErr
				}
			}
			select {
			case browserIn <- readResult{frame, err}:
			case <-done:
				return
			}
			if err != nil {
				return
			}
		}
	}()

	// --- 4. Pump: agent→browser and browser→agent ---
	for {
		select {
		case frame := <-fromAgent:
			// Agent → browser: raw inner Phase 0 JSON (offer/ice/…).
			if err := writeToBrowser(frame); err != nil {
				closedNoFrame("browser-write-failed", err)
				return
			}
		case res := <-browserIn:
			if res.err != nil {
				reason := "client-hung-up"
				if !websocket.IsCloseError(res.err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
					reason = "browser-read-failed"
				}
				closedNoFrame(reason, res.err)
				return
			}
			// #505: an ICE restart for a PC whose offer is still outstanding is
			// the redundant request that made the client answer two offers and
			// lose the restart. signaling.md already calls duplicate requests
			// idempotent while negotiation is in progress — this is where that
			// becomes true.
			if allow, pc := negotiations.allowBrowserFrame(res.frame); !allow {
				h.log.Info("dropped redundant ICE restart (offer already outstanding)",
					"token", "signal-restart-ice-redundant",
					"session_id", sess.ID, "pc", pc)
				continue
			}
			// Browser → agent: wrap in signaling envelope and send.
			if err := h.registry.SendSignaling(hostID, sess.ID, res.frame); err != nil {
				if errors.Is(err, agentws.ErrAgentNotConnected) {
					wsClose(wsCloseRelayUnavail, "agent unavailable", "agent-not-connected")
					return
				}
				// #93: this exit used to return with no close frame, one frame
				// after the client's own write — which the client reports as
				// "connection reset without closing handshake right after the
				// answer", with that answer silently never reaching the agent.
				// A saturated agent queue IS signaling.md's 4500 "relay to node
				// agent unavailable": no new code, no shape change.
				h.log.Warn("relay to agent failed",
					"token", "signal-relay-send-failed", "session_id", sess.ID, "err", err)
				wsClose(wsCloseRelayUnavail, "relay to agent failed", "relay-send-failed")
				return
			}
		case <-ping.C:
			if err := conn.WriteControl(websocket.PingMessage, nil,
				time.Now().Add(browserWriteTimeout)); err != nil {
				closedNoFrame("ping-failed", err)
				return
			}
		case <-signals.Terminal:
			// #93: the session went terminal, so the bus dropped this
			// registration and will never deliver another agent frame. Hand
			// over whatever it already queued — the agent's `bye` is typically
			// the last frame in flight — then close with the code signaling.md
			// already defines for a terminal session. Before this the socket
			// stayed open and deaf, and the client learned of the teardown only
			// when its own transport died, with no closing handshake.
			if err := drainAgentFrames(); err != nil {
				closedNoFrame("browser-write-failed", err)
				return
			}
			wsClose(wsCloseNotFound, "session is terminal", "session-ended")
			return
		case <-signals.Displaced:
			// #415: a later attach owns this session's signaling — this
			// connection is deaf to agent frames and must tear down now.
			// #526: close with a code the client can act on; 1000 looked like
			// an ordinary hang-up, so the displaced tab re-minted and displaced
			// the displacer, ping-pong forever. Last attach wins and the loser
			// is told it lost. A client's own reconnect also lands here,
			// harmlessly: its old transport is already torn down, so only a
			// still-live socket — a different tab — observes this frame.
			h.log.Info("signaling WS taken over by a later attach",
				"token", "signal-taken-over", "session_id", sess.ID)
			wsClose(wsCloseTakenOver, "session attached from another client", "taken-over")
			return
		case <-ctx.Done():
			closedNoFrame("context-cancelled", ctx.Err())
			return
		}
	}
}
