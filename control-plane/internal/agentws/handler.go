// Package agentws implements the control-plane side of the P1-A agent WebSocket protocol.
// Each node agent dials this endpoint, registers, reports capacity, then sends heartbeats.
// Session commands (assign/start/stop) are added in P1-6.
package agentws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/console"
	"github.com/accreleus/quasar/control-plane/internal/hostcfg"
	"github.com/accreleus/quasar/control-plane/internal/hostenroll"
	"github.com/accreleus/quasar/control-plane/internal/httpx"
	"github.com/accreleus/quasar/control-plane/internal/ratelimit"
)

const (
	heartbeatIntervalMs = 5000
	// Allow 4× heartbeat_interval before declaring the read stale.
	readDeadlineDur = time.Duration(heartbeatIntervalMs*4) * time.Millisecond
	// How long to wait for the initial register / capacity messages.
	handshakeTimeout = 15 * time.Second
	// Bounds the read loop's per-message DB work (#416) so a stalled store
	// surfaces as a dropped agent connection instead of parking the loop. Must
	// stay below db.DefaultStatementTimeout (30s) or the DB's own timeout wins
	// and this bound never trips.
	agentDBCallTimeout      = 25 * time.Second
	agentReadLimit          = 1 << 20
	enrollmentFailureLimit  = 10
	enrollmentFailureTTL    = time.Minute
	enrollmentFailureMaxIPs = 4096
	enrollmentMaxInFlightIP = 10
	enrollmentMaxInFlight   = 256
)

var upgrader = websocket.Upgrader{
	// Agent endpoint — no browser, no CORS check needed.
	CheckOrigin: func(r *http.Request) bool { return true },
}

// Handler is the HTTP handler for the agent WebSocket endpoint (GET /agent/ws).
type Handler struct {
	store           *agentStore
	log             *slog.Logger
	enrollmentToken string
	registry        *Registry
	events          Events
	relay           *RelayBus
	cfgStore        *hostcfg.Store
	consoleStore    *console.Store
	consoleAuto     *consoleAutoState
	failures        *ratelimit.FailureLimiter
	diagnostics     *diagnosticQueue
	vram            *vramQueue
	// Image-management P2 callback surface (images.go). Never nil — NewHandler
	// installs a no-op — so dispatch needs no guard.
	imageEvents ImageEvents
	// Platform-release apply callback surface (release.go, amendment 2). Never
	// nil — NewHandler installs a no-op — so dispatch needs no guard.
	releaseEvents ReleaseEvents
	// Per-host image_state token bucket; bounded by the live connection set
	// (see imageStateLimiter).
	imageLimiter *imageStateLimiter

	// #438 trusted-proxy policy; nil keys on the direct peer. Without it, the
	// hardened topology (deploy/Caddyfile.hardened proxies /agent/ws) makes
	// every fleet host share one enrollment budget.
	trustedProxies []*net.IPNet
}

// WithTrustedProxies configures which direct peers are reverse proxies whose
// X-Forwarded-For may be believed when keying the enrollment failure limiter
// (#438).
func (h *Handler) WithTrustedProxies(nets []*net.IPNet) *Handler {
	h.trustedProxies = nets
	return h
}

// consoleAutoState is the CM-06/CM-09 in-memory state for console auto-start:
// per host, the auto-launched session, the last-reported connector list, and
// crash-loop backoff. In-memory only — a control-plane restart just makes the
// next capacity report a fresh baseline.
//
// Accepted growth (#406): lastConnectors and backoff are never deleted (one
// small entry per host ever enrolled). No eviction on disconnect, on purpose:
// console state must survive a reconnect, and clearing the crash-loop counter
// there would let a hot-looping host reset its backoff by reconnecting. The
// bound is distinct-hosts-per-process-lifetime, fleet-sized.
type consoleAutoState struct {
	mu             sync.Mutex
	sessions       map[string]string          // hostID → auto-started console session id
	launching      map[string]bool            // hostID → launch currently being scheduled
	lastConnectors map[string][]string        // hostID → last-reported connector list
	backoff        map[string]*consoleBackoff // hostID → crash-loop backoff state
}

func newConsoleAutoState() *consoleAutoState {
	return &consoleAutoState{
		sessions:       make(map[string]string),
		launching:      make(map[string]bool),
		lastConnectors: make(map[string][]string),
		backoff:        make(map[string]*consoleBackoff),
	}
}

// consoleBackoff is the CM-09 per-host crash-loop guard: fast terminations
// grow the relaunch delay; after consoleBackoffMaxRetries relaunching stops
// until a fresh capacity report re-primes it.
//
// pendingTimer is the scheduled retry (retryConsoleAfterBackoff): a static
// display never sends another capacity report, so without it the schedule and
// the give-up would never run. At most one per host — ConsoleSessionTerminated
// stops the previous timer before arming a new one.
type consoleBackoff struct {
	consecutiveFailures int
	nextEligibleAt      time.Time
	// Stamped at accepted dispatch, not confirmed `running` — a session failing
	// in between still counts as "fast", which only ever under-counts stability
	// (the safe direction for a crash-loop guard).
	lastLaunchAt time.Time
	gaveUp       bool
	pendingTimer *time.Timer
}

// Exponential backoff bounds (2s doubling, capped 60s) between console
// relaunch attempts. Vars so tests can scale the schedule down.
var (
	consoleBackoffBase = 2 * time.Second
	consoleBackoffMax  = 60 * time.Second
)

const (
	// Consecutive fast terminations tolerated before auto-relaunching stops.
	consoleBackoffMaxRetries = 6
	// How long a relaunched session must stay non-terminal to reset the
	// crash-loop counter.
	consoleStabilityWindow = 30 * time.Second
)

// consoleBackoffDelay returns the exponential backoff delay for the nth
// (1-indexed) consecutive fast-termination failure.
func consoleBackoffDelay(consecutiveFailures int) time.Duration {
	d := consoleBackoffBase
	for i := 1; i < consecutiveFailures; i++ {
		d *= 2
		if d >= consoleBackoffMax {
			return consoleBackoffMax
		}
	}
	return d
}

// backoffFor returns (creating if absent) the crash-loop backoff state for
// hostID. Callers must hold s.mu.
func (s *consoleAutoState) backoffFor(hostID string) *consoleBackoff {
	bo, ok := s.backoff[hostID]
	if !ok {
		bo = &consoleBackoff{}
		s.backoff[hostID] = bo
	}
	return bo
}

// claimLaunch atomically reserves the right to schedule a console session for
// hostID. Capacity reports can arrive concurrently (the initial report and a
// hotplug/config-triggered refresh); without this in-flight claim both reports
// can observe no recorded session and launch overlapping GPU compositors.
func (s *consoleAutoState) claimLaunch(hostID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.sessions[hostID]; exists || s.launching[hostID] {
		return false
	}
	s.launching[hostID] = true
	return true
}

func (s *consoleAutoState) finishLaunch(hostID, sessionID string, launched bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.launching, hostID)
	if launched {
		s.sessions[hostID] = sessionID
	}
}

// NewHandler constructs a Handler. enrollmentToken is the pre-shared token from
// ENROLLMENT_TOKEN; pool is the Postgres connection pool; registry tracks live
// agent connections; events is the coordinator's callback surface; relay routes
// agent→browser signaling; consoleStore backs the CM-01 console-config
// snapshot push + reported-capabilities upsert. Any nil argument uses a safe
// no-op default (consoleStore nil simply skips console_config / capabilities).
func NewHandler(pool *pgxpool.Pool, enrollmentToken string, log *slog.Logger, registry *Registry, events Events, relay *RelayBus, cfgStore *hostcfg.Store, consoleStore *console.Store) *Handler {
	if registry == nil {
		registry = NewRegistry(log)
	}
	if events == nil {
		events = noopEvents{}
	}
	if relay == nil {
		relay = NewRelayBus(log)
	}
	h := &Handler{
		store: &agentStore{
			pool: pool,
			// The local half of the #96 liveness answer; the DB half is in enrollHost.
			isAgentConnected: registry.IsConnected,
			redeemEnrollment: hostenroll.Redeem,
		},
		log:             log,
		enrollmentToken: enrollmentToken,
		registry:        registry,
		events:          events,
		relay:           relay,
		cfgStore:        cfgStore,
		consoleStore:    consoleStore,
		consoleAuto:     newConsoleAutoState(),
		failures:        ratelimit.NewFailureLimiter(enrollmentFailureLimit, enrollmentFailureTTL, enrollmentFailureMaxIPs),
		imageEvents:     noopImageEvents{},
		releaseEvents:   noopReleaseEvents{},
		imageLimiter:    newImageStateLimiter(),
	}
	h.diagnostics = newDiagnosticQueue(events, log)
	h.vram = newVramQueue(h.store, log)
	return h
}

// SetImageEvents wires the image-management P2 callback surface (image_state
// ingest + register reconciliation + ensure-on-connect). A setter rather than a
// NewHandler parameter — the constructor already carries eight, and the ensure
// orchestrator is optional wiring, exactly like crud.Handler.SetRegistry. A nil
// argument restores the no-op.
func (h *Handler) SetImageEvents(ev ImageEvents) {
	if ev == nil {
		ev = noopImageEvents{}
	}
	h.imageEvents = ev
}

// SetReleaseEvents wires the platform-apply callback surface (release_state
// relay + the register success-evidence hook, #116). A setter for the same
// reason SetImageEvents is one: the apply runner is constructed after this
// handler. A nil argument restores the no-op.
func (h *Handler) SetReleaseEvents(ev ReleaseEvents) {
	if ev == nil {
		ev = noopReleaseEvents{}
	}
	h.releaseEvents = ev
}

// Close stops the handler's background queues. Optional for the production
// server (one Handler per process, which exits anyway), but tests build many —
// without it each leaks its drain goroutine for the life of the test binary.
func (h *Handler) Close() {
	if h.vram != nil {
		h.vram.close()
	}
	if h.diagnostics != nil {
		h.diagnostics.close()
	}
}

// Register wires the handler into mux at GET /agent/ws.
func (h *Handler) Register(mux httpx.Router) {
	mux.HandleFunc("GET /agent/ws", h.ServeHTTP)
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	clientIP := ratelimit.ClientIP(r, h.trustedProxies)
	if !h.failures.Reserve(clientIP, enrollmentMaxInFlightIP, enrollmentMaxInFlight) {
		http.Error(w, "too many failed enrollment attempts; try again later", http.StatusTooManyRequests)
		return
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		h.failures.Release(clientIP)
		// Upgrade already wrote the HTTP error response.
		return
	}
	defer conn.Close()
	conn.SetReadLimit(agentReadLimit)

	if err := h.handleConn(r.Context(), conn, clientIP); err != nil {
		// #496: agent self-restarts (driver-volume provision, GPU-fault
		// recovery, admin restart) hard-exit with no close frame, so they
		// arrive as code 1006 — indistinguishable from a real crash. The agent
		// logs its own reason and reconnects; only a host that stays offline
		// is actionable.
		if websocket.IsCloseError(err, websocket.CloseAbnormalClosure) {
			h.log.Warn("agent connection closed abnormally (code 1006) — "+
				"expected if the agent just restarted itself (driver-volume "+
				"provision, GPU-fault recovery, or an admin restart command); "+
				"only investigate if the host does not reconnect", "err", err)
		} else {
			h.log.Warn("agent connection closed", "err", err)
		}
	}
}

func (h *Handler) handleConn(reqCtx context.Context, conn *websocket.Conn, clientIP string) error {
	// Long-lived WS connections outlive the HTTP request context lifetime, so we
	// use a background context for DB calls. The request context is used only to
	// detect server shutdown (the HTTP server closes idle connections on shutdown).
	bg := context.Background()

	// Step 1 — register
	registerCtx, cancelRegister := context.WithTimeout(bg, handshakeTimeout)
	hostID, regImages, regCommit, err := h.handleRegister(registerCtx, conn, clientIP)
	cancelRegister()
	h.failures.Release(clientIP)
	if err != nil {
		return fmt.Errorf("register: %w", err)
	}
	h.failures.Forget(clientIP)
	defer func() {
		if err := h.store.markOffline(bg, hostID); err != nil {
			h.log.Error("mark offline failed", "host_id", hostID, "err", err)
		}
	}()

	h.log.Info("agent registered", "host_id", hostID)

	// Platform-apply success evidence: a register resolves an in-flight apply,
	// because the recreate killed the agent that would have reported it.
	// Bounded and swallowed — a registration is never refused over an apply.
	regEvCtx, regEvCancel := context.WithTimeout(bg, agentDBCallTimeout)
	h.releaseEvents.AgentRegistered(regEvCtx, hostID, regCommit)
	regEvCancel()

	// Step 2 — register the connection + start the sole writer goroutine
	// (gorilla allows one concurrent writer) BEFORE the capacity handshake.
	// Ordering is load-bearing: the handshake's console auto-start dispatches a
	// SessionAssign, and if that reaches the agent before the config_update
	// queued below, the session builds with console_config=None and silently
	// skips the local-display leg. Queuing config_update first guarantees it
	// precedes any assign on the single writer.
	ac := newConn(hostID, conn)
	h.registry.add(ac)
	go ac.runWriter(h.log)

	defer func() {
		// schema.md invariant #3: a lost agent connection reaps the host's
		// non-terminal sessions to failed — but only if this connection is
		// still the current one. A displaced connection must not reap the live
		// sessions the newer connection now owns (P2-06 race).
		if h.registry.remove(ac) {
			h.events.HostDisconnected(bg, hostID)
			// Bounds the rate-limiter map by the live connection set.
			h.imageLimiter.evict(hostID)
		}
	}()

	// Push settings + console config (agent-api.md `config_update`) before the
	// capacity handshake can assign a session (see above). Fire-and-forget: a
	// failure must never fail registration — the agent keeps its env baseline.
	if h.cfgStore != nil || h.consoleStore != nil {
		cmd := ConfigUpdateCmd{Type: "config_update"}
		if h.cfgStore != nil {
			if overrides, err := h.cfgStore.Get(bg, hostID); err != nil {
				h.log.Warn("config_update snapshot: load host settings failed", "host_id", hostID, "err", err)
			} else {
				// #194: only explicit overrides — the full resolved map silently
				// clobbered a host's QUASAR_ENCODER with the catalog default.
				cmd.Settings = hostcfg.AgentOverrides(overrides)
			}
		}
		if h.consoleStore != nil {
			if sparse, err := h.consoleStore.Get(bg, hostID); err != nil {
				h.log.Warn("config_update snapshot: load console config failed", "host_id", hostID, "err", err)
			} else if resolved, err := console.Resolve(sparse); err != nil {
				h.log.Warn("config_update snapshot: resolve console config failed", "host_id", hostID, "err", err)
			} else {
				// Full resolved object, not sparse (agent-api.md).
				cmd.ConsoleConfig = resolved
			}
		}
		if err := h.registry.Send(hostID, cmd); err != nil {
			h.log.Warn("config_update snapshot: send failed", "host_id", hostID, "err", err)
		}
	}

	// Reconcile before processing capacity: handleCapacity may auto-start a
	// console session, and reaping after that launch would mark it stale and
	// let the next refresh launch a duplicate compositor.
	h.events.AgentReconnected(bg, hostID)

	// Image-management P2: reconcile host_images against the agent's snapshot,
	// then re-ensure what's missing. Fire-and-forget — an image problem must
	// never fail registration. Register has no token bucket like image_state's,
	// so the snapshot must be bounded/sanitized before it reaches ImageEvents
	// (one DB op per entry).
	sanitizedImages, imagesReported := sanitizeRegisterImages(regImages, h.log, hostID)
	h.imageEvents.AgentImagesRegistered(bg, hostID, sanitizedImages, imagesReported)

	// Step 3 — capacity (protocol requires it next). Its console auto-start
	// runs only after config_update is queued and stale sessions reconciled.
	if err := h.handleCapacity(bg, conn, hostID); err != nil {
		return fmt.Errorf("capacity: %w", err)
	}
	h.log.Info("agent capacity received", "host_id", hostID)

	// Step 4 — message loop: heartbeats, acks, and session_state callbacks.
	for {
		// Fail fast if the server is shutting down.
		select {
		case <-reqCtx.Done():
			return nil
		default:
		}

		conn.SetReadDeadline(time.Now().Add(readDeadlineDur))
		raw, err := readTextMessage(conn)
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}

		msgType, err := peekType(raw)
		if err != nil {
			return fmt.Errorf("peek type: %w", err)
		}

		switch msgType {
		case "heartbeat":
			var hb HeartbeatMsg
			if err := json.Unmarshal(raw, &hb); err != nil {
				return fmt.Errorf("decode heartbeat: %w", err)
			}
			// #416: derived from bg (connection-lifetime, not request), with a
			// deadline so a stalled store drops the connection instead of
			// parking this read loop forever.
			hbCtx, hbCancel := context.WithTimeout(bg, agentDBCallTimeout)
			err := h.store.updateHeartbeat(hbCtx, hostID)
			hbCancel()
			if err != nil {
				h.log.Error("heartbeat update failed", "host_id", hostID, "err", err)
			} else {
				h.log.Debug("heartbeat", "host_id", hostID, "running_sessions", len(hb.RunningSessions))
			}
			// #383: VRAM telemetry, off the read loop (vramQueue). An absent
			// gpu_vram key is a no-op — the stored sample ages out.
			h.vram.enqueue(vramSampleBatch{hostID: hostID, agentMs: hb.TsUnixMs, samples: hb.GPUVram})
		case "ack":
			var a AckMsg
			if err := json.Unmarshal(raw, &a); err != nil {
				return fmt.Errorf("decode ack: %w", err)
			}
			res := AckResult{OK: a.OK}
			if a.Error != nil {
				res.Error = *a.Error
			}
			h.registry.resolveAck(hostID, a.ID, res)
		case "session_state":
			var m SessionStateMsg
			if err := json.Unmarshal(raw, &m); err != nil {
				return fmt.Errorf("decode session_state: %w", err)
			}
			stateCtx, stateCancel := context.WithTimeout(bg, agentDBCallTimeout)
			h.events.AgentState(stateCtx, hostID, m)
			stateCancel()
		case "session_metrics":
			// Fire-and-forget; malformed drops the message, never the connection
			// (agent-api.md). The coordinator enforces host ownership.
			var m SessionMetricsMsg
			if err := json.Unmarshal(raw, &m); err != nil {
				h.log.Warn("decode session_metrics failed", "host_id", hostID, "err", err)
				continue
			}
			h.diagnostics.enqueue(diagnosticEvent{hostID: hostID, metric: &m})
		case "session_trace_event":
			// Fire-and-forget, same drop-not-disconnect posture as
			// session_metrics; the coordinator validates host ownership.
			var m SessionTraceEventMsg
			if err := json.Unmarshal(raw, &m); err != nil {
				h.log.Warn("decode session_trace_event failed", "host_id", hostID, "err", err)
				continue
			}
			if strings.HasPrefix(m.Event, diagEventPrefix) {
				if !validDiagEvent(m, h.log, hostID) {
					continue
				}
			}
			// effective_media and diag.* persist synchronously: queue saturation
			// must not erase the admin's only record of what ran, and a diag.*
			// result is being polled on right now — a coalescing-queue drop
			// would turn that poll into a timeout.
			if m.Event == "session.effective_media" || strings.HasPrefix(m.Event, diagEventPrefix) {
				h.events.AgentTraceEvent(bg, hostID, m)
			} else {
				h.diagnostics.enqueue(diagnosticEvent{hostID: hostID, trace: &m})
			}
		case "capacity":
			// CM-06/07 hotplug re-report. Fire-and-forget — a decode/upsert
			// failure must never drop the connection.
			capCtx, capCancel := context.WithTimeout(bg, agentDBCallTimeout)
			err := h.processCapacity(capCtx, hostID, raw)
			capCancel()
			if err != nil {
				h.log.Warn("capacity re-report failed", "host_id", hostID, "err", err)
			}
		case "image_state":
			// Image-management P2, fire-and-forget. Malformed drops the message,
			// never the connection — an image report must not cost a host its
			// agent connection (that would reap its sessions).
			var m ImageStateMsg
			if err := json.Unmarshal(raw, &m); err != nil {
				h.log.Warn("decode image_state failed", "host_id", hostID, "err", err)
				continue
			}
			// Rate-limit before any DB work — a runaway agent must not get a
			// free upsert attempt per message.
			if !h.imageLimiter.allow(hostID) {
				if h.imageLimiter.shouldLog(hostID) {
					h.log.Warn("image_state: rate limit exceeded; dropping excess messages", "host_id", hostID)
				}
				continue
			}
			// Bound/clamp fields before they reach a query.
			if !validateImageState(&m) {
				h.log.Warn("image_state: invalid message dropped", "host_id", hostID, "image_id", m.ImageID)
				continue
			}
			// Throttled by contract (≤ every 2 s per image during a pull), so a
			// synchronous bounded upsert cannot congest the read loop.
			imgCtx, imgCancel := context.WithTimeout(bg, agentDBCallTimeout)
			h.imageEvents.AgentImageState(imgCtx, hostID, m)
			imgCancel()
		case "release_state":
			// Fire-and-forget apply progress. No token bucket: the contract
			// throttles it to one message per 2 s and the database allows at
			// most one open apply per host. A malformed message drops the
			// message, never the connection.
			var rm ReleaseStateMsg
			if err := json.Unmarshal(raw, &rm); err != nil {
				logReleaseDrop(h.log, hostID, "could not be decoded")
				continue
			}
			if !validateReleaseState(&rm) {
				logReleaseDrop(h.log, hostID, "is missing a request id or a state")
				continue
			}
			relCtx, relCancel := context.WithTimeout(bg, agentDBCallTimeout)
			h.releaseEvents.AgentReleaseState(relCtx, hostID, rm)
			relCancel()
		case "signaling":
			// Relay agent→browser: deliver only the verbatim inner msg.
			var env SignalingEnvelope
			if err := json.Unmarshal(raw, &env); err != nil {
				h.log.Warn("decode signaling envelope failed", "err", err)
				continue
			}
			if err := h.relay.Deliver(env.SessionID, env.Msg); err != nil {
				h.log.Error("reliable signaling relay failed", "host_id", hostID, "session_id", env.SessionID, "err", err)
				h.events.AgentSignalingFailure(bg, hostID, env.SessionID, err.Error())
			}
		default:
			h.log.Warn("unknown agent message", "type", msgType, "host_id", hostID)
		}
	}
}

// handleRegister performs the register handshake and returns the resolved host
// id plus the agent's optional image-management P2 `images` snapshot (nil when
// the agent sent no such field — see RegisterMsg.Images).
// The third return value is the identity commit the agent reported (nil when it
// reported none or one this build refused), which the caller hands to the
// platform-apply success-evidence hook.
func (h *Handler) handleRegister(ctx context.Context, conn *websocket.Conn, clientIP string) (string, []RegisterImage, *string, error) {
	fail := func(err error) (string, []RegisterImage, *string, error) {
		h.failures.Failure(clientIP)
		return "", nil, nil, err
	}
	conn.SetReadDeadline(time.Now().Add(handshakeTimeout))
	raw, err := readTextMessage(conn)
	if err != nil {
		return fail(fmt.Errorf("read: %w", err))
	}

	var reg RegisterMsg
	if err := json.Unmarshal(raw, &reg); err != nil {
		return fail(fmt.Errorf("decode: %w", err))
	}
	if reg.Type != "register" {
		h.writeError(conn, "protocol_error", "expected register as first message")
		return fail(fmt.Errorf("unexpected first message type %q", reg.Type))
	}
	if reg.NodeName == "" || reg.AgentVersion == "" {
		h.writeError(conn, "protocol_error", "node_name and agent_version are required")
		return fail(errors.New("missing required register fields"))
	}

	result, err := h.resolveAuth(ctx, reg)
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidEnrollmentToken), errors.Is(err, ErrInvalidNodeSecret):
			h.writeError(conn, "auth_failed", "authentication failed")
		case errors.Is(err, ErrHostAgentConnected):
			// Deliberately distinct from auth_failed: the credential was fine, the
			// REQUEST was refused. An operator re-enrolling a machine that is quietly
			// already running needs to be told that, not sent to check their token (#96).
			// The existing `auth_failed` code stays exactly as it was for bad credentials,
			// so this adds a case rather than changing one.
			h.writeError(conn, "auth_failed",
				"a live agent is already registered under this node name; stop it before re-enrolling, "+
					"or enroll under a different node_name")
		case errors.Is(err, ErrHostNotFound):
			h.writeError(conn, "host_not_found", "node not enrolled; use enrollment_token to enroll first")
		default:
			h.writeError(conn, "internal_error", "registration failed")
		}
		return fail(err)
	}
	// Platform-release identity (amendment 1): written AFTER auth succeeded, so
	// a failed registration never touches a host row, and wholesale — absent
	// fields become NULL (agent-api.md §register). A write failure is logged
	// and swallowed: the control plane never refuses a registration over these
	// fields, and a host that streams is worth more than a known build stamp.
	identity, droppedIdentity := identityFromRegister(reg)
	if len(droppedIdentity) > 0 {
		h.log.Warn("register: ignoring malformed identity fields",
			"host_id", result.HostID, "node_name", reg.NodeName, "fields", droppedIdentity)
	}
	if err := h.store.replaceHostIdentity(ctx, result.HostID, identity); err != nil {
		h.log.Warn("register: could not store host identity",
			"host_id", result.HostID, "node_name", reg.NodeName, "err", err)
	}

	if result.AgentRestarted {
		// #429: logged so an operator tailing logs sees it in real time, not
		// only on the next admin-panel poll.
		h.log.Warn("agent restart detected", "host_id", result.HostID, "node_name", reg.NodeName)
	}
	resp := RegisteredMsg{
		Type:                "registered",
		HostID:              result.HostID,
		NodeSecret:          result.NodeSecret,
		HeartbeatIntervalMs: heartbeatIntervalMs,
	}
	if err := conn.WriteJSON(resp); err != nil {
		return "", nil, nil, fmt.Errorf("write registered: %w", err)
	}
	return result.HostID, reg.Images, identity.SourceCommit, nil
}

func (h *Handler) handleCapacity(ctx context.Context, conn *websocket.Conn, hostID string) error {
	conn.SetReadDeadline(time.Now().Add(handshakeTimeout))
	raw, err := readTextMessage(conn)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}

	msgType, err := peekType(raw)
	if err != nil {
		return fmt.Errorf("peek type: %w", err)
	}
	if msgType != "capacity" {
		return fmt.Errorf("expected capacity, got %q", msgType)
	}

	return h.processCapacity(ctx, hostID, raw)
}

func readTextMessage(conn *websocket.Conn) ([]byte, error) {
	messageType, raw, err := conn.ReadMessage()
	if err != nil {
		return nil, err
	}
	if messageType != websocket.TextMessage {
		_ = conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseUnsupportedData, "text JSON messages required"),
			time.Now().Add(time.Second))
		return nil, errors.New("binary WebSocket message rejected")
	}
	return raw, nil
}

// processCapacity decodes and applies one capacity report (host/GPU capacity,
// console capabilities, CM-06 auto-start diffing). Called once at the
// handshake (handleCapacity) and again on every re-sent "capacity" message in
// the connection's message loop (CM-06/07 hotplug re-report).
func (h *Handler) processCapacity(ctx context.Context, hostID string, raw []byte) error {
	var cap CapacityMsg
	if err := json.Unmarshal(raw, &cap); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	detection, reason, gpus, err := normalizeCapacityReport(cap)
	if err != nil {
		return err
	}
	if err := h.store.upsertCapacityWithDetection(ctx, hostID, cap.Host, cap.EffectiveSettings, gpus, detection, reason); err != nil {
		return err
	}
	// The remaining stores are all fire-and-forget: a failure is logged and
	// must never break capacity handling.
	if err := h.store.upsertHostCodecs(ctx, hostID, cap.Codecs); err != nil {
		h.log.Warn("host codecs upsert failed", "host_id", hostID, "err", err)
	}
	// #506: an unstored throughput hint degrades to "unknown", which gates
	// nothing — a failure costs a tier decision, never a launch.
	if err := h.store.upsertHostCodecPixelRates(ctx, hostID, cap.CodecThroughput); err != nil {
		h.log.Warn("host codec throughput upsert failed", "host_id", hostID, "err", err)
	}
	if err := h.store.upsertHostReadiness(ctx, hostID, cap.Readiness); err != nil {
		h.log.Warn("host readiness upsert failed", "host_id", hostID, "err", err)
	}
	if h.consoleStore != nil && cap.ConsoleCapabilities != nil {
		if err := h.consoleStore.UpsertCapabilities(ctx, hostID, *cap.ConsoleCapabilities); err != nil {
			h.log.Warn("console capabilities upsert failed", "host_id", hostID, "err", err)
		}
		h.handleConsoleAutoStart(ctx, hostID, cap.ConsoleCapabilities.Connectors)
	}
	return nil
}

func normalizeCapacityReport(cap CapacityMsg) (string, string, []GPUCapacity, error) {
	detection := cap.GPUDetection
	if detection == "" {
		if len(cap.GPUs) > 0 {
			detection = "ok"
		} else {
			detection = "unavailable"
		}
	}
	if detection != "ok" && detection != "unavailable" && detection != "failed" {
		return "", "", nil, fmt.Errorf("invalid gpu_detection %q", detection)
	}
	reason := sanitizeCapacityReason(cap.GPUDetectionReason)
	if detection == "ok" && len(cap.GPUs) == 0 {
		detection = "unavailable"
		if reason == "" {
			reason = "agent reported no usable GPUs"
		}
	}
	if detection != "ok" {
		// A failed or unavailable probe cannot authoritatively update GPU rows.
		cap.GPUs = nil
	}
	return detection, reason, cap.GPUs, nil
}

func sanitizeCapacityReason(reason string) string {
	reason = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, reason))
	for len(reason) > 512 {
		_, size := utf8.DecodeLastRuneInString(reason)
		reason = reason[:len(reason)-size]
	}
	return reason
}

// handleConsoleAutoStart is the capacity-report entry to console auto-start
// (see reevalConsole for the mechanism and design-doc pointer).
func (h *Handler) handleConsoleAutoStart(ctx context.Context, hostID string, connectors []string) {
	if h.consoleStore == nil {
		return
	}

	// Cache the connector list so the session-terminal re-eval hook can answer
	// "is the display still present" without waiting for a capacity report —
	// a static display never produces one.
	h.consoleAuto.mu.Lock()
	h.consoleAuto.lastConnectors[hostID] = connectors
	h.consoleAuto.mu.Unlock()

	h.reevalConsole(ctx, hostID, connectors, true)
}

// ConsoleSessionTerminated is the CM-09 session-terminal re-eval hook, wired
// to Coordinator.ConsoleReeval (the callback inversion that keeps session from
// importing agentws). Without it, a console session crashing under a static
// display would wait forever for a capacity re-send that never comes. A
// session this handler did not auto-start (or a stale sessionID) is a no-op.
func (h *Handler) ConsoleSessionTerminated(ctx context.Context, hostID, sessionID string) {
	if h.consoleStore == nil {
		return
	}

	h.consoleAuto.mu.Lock()
	recordedID, tracked := h.consoleAuto.sessions[hostID]
	if !tracked || recordedID != sessionID {
		h.consoleAuto.mu.Unlock()
		return
	}
	delete(h.consoleAuto.sessions, hostID)
	connectors := h.consoleAuto.lastConnectors[hostID]
	bo := h.consoleAuto.backoffFor(hostID)
	now := time.Now()
	// At most one pending retry timer per host.
	if bo.pendingTimer != nil {
		bo.pendingTimer.Stop()
		bo.pendingTimer = nil
	}
	var retryDelay time.Duration
	armRetry := false
	if bo.lastLaunchAt.IsZero() || now.Sub(bo.lastLaunchAt) >= consoleStabilityWindow {
		// Survived the stability window (or no recorded launch time, e.g.
		// after a control-plane restart) — clean end, reset the guard.
		bo.consecutiveFailures = 0
		bo.gaveUp = false
	} else {
		bo.consecutiveFailures++
		if bo.consecutiveFailures >= consoleBackoffMaxRetries {
			if !bo.gaveUp {
				h.log.Error("console self-heal: host auto-start crash-looping, giving up until next display/config change",
					"host_id", hostID, "consecutive_failures", bo.consecutiveFailures)
			}
			bo.gaveUp = true
		} else {
			retryDelay = consoleBackoffDelay(bo.consecutiveFailures)
			bo.nextEligibleAt = now.Add(retryDelay)
			armRetry = true
			h.log.Warn("console self-heal: session terminated quickly, backing off before relaunch",
				"host_id", hostID, "session_id", sessionID, "consecutive_failures", bo.consecutiveFailures,
				"next_eligible_at", bo.nextEligibleAt)
		}
	}
	if armRetry {
		// Without this timer nothing re-checks after the window elapses (a
		// static display never re-sends capacity), so the schedule and the
		// give-up would be unreachable. retryConsoleAfterBackoff re-reads
		// state fresh; it closes over only hostID.
		bo.pendingTimer = time.AfterFunc(retryDelay, func() {
			h.retryConsoleAfterBackoff(hostID)
		})
	}
	h.consoleAuto.mu.Unlock()

	h.log.Info("console self-heal: tracked session terminated, re-evaluating",
		"host_id", hostID, "session_id", sessionID)
	// connectors is the last-reported list and can be stale (a real unplug
	// racing a crash). Tolerated: a launch on stale "present" data is
	// self-correcting — the next capacity report triggers the auto-stop path.
	h.reevalConsole(ctx, hostID, connectors, false)
}

// retryConsoleAfterBackoff is the backoff-timer callback: it re-evaluates
// console auto-start without waiting for a capacity report, re-reading state
// fresh under the lock rather than a snapshot from when the timer was armed.
func (h *Handler) retryConsoleAfterBackoff(hostID string) {
	h.consoleAuto.mu.Lock()
	if bo, ok := h.consoleAuto.backoff[hostID]; ok {
		bo.pendingTimer = nil
	}
	connectors := h.consoleAuto.lastConnectors[hostID]
	h.consoleAuto.mu.Unlock()

	h.reevalConsole(context.Background(), hostID, connectors, false)
}

// reevalConsole is CM-06 console auto-start: no agent-api change, presence is
// inferred from capacity reports' connector lists (design doc
// docs/design/plans/cm-06-07-autostart-design.md), also run from the
// session-terminal hook (CM-09). Dormant unless the resolved console_config
// has enabled && auto_start_on_display && default_app && default_user.
//
// isCapacityPath: only a fresh capacity report re-primes a host that gave up
// after consoleBackoffMaxRetries; the terminal hook never does. Both callers
// respect the time-based backoff window. State is in-memory only — a restart
// resets the baseline and forgets the launched-session record (consoleAutoState).
func (h *Handler) reevalConsole(ctx context.Context, hostID string, connectors []string, isCapacityPath bool) {
	h.consoleAuto.mu.Lock()
	recordedID, alreadyLaunched := h.consoleAuto.sessions[hostID]
	h.consoleAuto.mu.Unlock()

	sparse, err := h.consoleStore.Get(ctx, hostID)
	if err != nil {
		h.log.Warn("console auto-start: load console config failed", "host_id", hostID, "err", err)
		return
	}
	cfg, err := console.Resolve(sparse)
	if err != nil {
		h.log.Warn("console auto-start: resolve console config failed", "host_id", hostID, "err", err)
		return
	}
	// A recorded session that terminated on its own must be cleared here or it
	// blocks all future relaunches (the tracker is only otherwise cleared on
	// auto-stop). Checked before the eligibility gate so a stale entry never
	// triggers a spurious teardown-on-disable stop.
	if alreadyLaunched && !h.events.ConsoleSessionActive(ctx, recordedID) {
		h.consoleAuto.mu.Lock()
		delete(h.consoleAuto.sessions, hostID)
		h.consoleAuto.mu.Unlock()
		alreadyLaunched = false
		h.log.Info("console auto-start: prior session terminated, will relaunch",
			"host_id", hostID, "prior_session_id", recordedID)
	}

	if !cfg.Enabled || !cfg.AutoStartOnDisplay || cfg.DefaultApp == nil || cfg.DefaultUser == nil {
		// Teardown-on-disable: a session auto-started while eligible must be
		// stopped when console mode is disabled (the agent re-sends capacity
		// after every config_update, which re-runs this path). It is invisible
		// to the session API, so there is no other lever.
		if !alreadyLaunched {
			return
		}
		h.consoleAuto.mu.Lock()
		sessionID, ok := h.consoleAuto.sessions[hostID]
		delete(h.consoleAuto.sessions, hostID)
		h.consoleAuto.mu.Unlock()
		if !ok {
			return
		}
		if err := h.events.StopConsoleSession(ctx, sessionID, "console_disabled"); err != nil {
			h.log.Warn("console teardown-on-disable: stop failed", "host_id", hostID, "session_id", sessionID, "err", err)
			return
		}
		h.log.Info("console teardown-on-disable: session stopped", "host_id", hostID, "session_id", sessionID)
		return
	}

	// Level-triggered, not edge-triggered: a console session runs whenever the
	// display is present, so it is (re)started on agent connect and boot-with-
	// display (monitor power off/on is invisible to the OS).
	//
	// Presence keys on the pinned connector (console.ConsoleConfig.PinnedConnector).
	// A pinned connector missing from the report is simply not-present — never a
	// silent fallback to a different monitor.
	nowPresent := connectorPresent(connectors, cfg.PinnedConnector())

	switch {
	case nowPresent && !alreadyLaunched:
		h.attemptConsoleLaunch(ctx, hostID, cfg, isCapacityPath)
	case !nowPresent && alreadyLaunched:
		h.consoleAuto.mu.Lock()
		sessionID, ok := h.consoleAuto.sessions[hostID]
		delete(h.consoleAuto.sessions, hostID)
		h.consoleAuto.mu.Unlock()
		if !ok {
			h.log.Debug("console auto-stop: no session recorded for host, nothing to stop", "host_id", hostID)
			return
		}
		if err := h.events.StopConsoleSession(ctx, sessionID, "console_display_disconnected"); err != nil {
			h.log.Warn("console auto-stop: stop failed", "host_id", hostID, "session_id", sessionID, "err", err)
			return
		}
		h.log.Info("console auto-stop: session stopped", "host_id", hostID, "session_id", sessionID, "connector", cfg.PinnedConnector())
	default:
		// Signal a connector mismatch on an eligible host — otherwise an admin
		// staring at a dead console has no clue the cause is the pin. "auto"
		// with no connectors stays quiet (a host with no display at all).
		if pinned := cfg.PinnedConnector(); pinned != "auto" && !nowPresent {
			h.log.Info("console auto-start: pinned connector not present, waiting",
				"host_id", hostID, "pinned_connector", pinned, "reported_connectors", connectors)
		}
	}
}

// attemptConsoleLaunch launches the auto-start console session, gated by the
// crash-loop backoff (see reevalConsole for the isCapacityPath re-prime rule).
// Both paths respect nextEligibleAt — a hotplug re-send cannot bypass the
// spacing.
func (h *Handler) attemptConsoleLaunch(ctx context.Context, hostID string, cfg console.ConsoleConfig, isCapacityPath bool) {
	now := time.Now()
	h.consoleAuto.mu.Lock()
	bo := h.consoleAuto.backoffFor(hostID)
	if isCapacityPath && bo.gaveUp {
		h.log.Info("console self-heal: re-priming after fresh capacity report", "host_id", hostID)
		bo.gaveUp = false
	}
	if bo.gaveUp {
		h.consoleAuto.mu.Unlock()
		return
	}
	if now.Before(bo.nextEligibleAt) {
		h.consoleAuto.mu.Unlock()
		h.log.Warn("console self-heal: relaunch skipped, in backoff window",
			"host_id", hostID, "consecutive_failures", bo.consecutiveFailures, "next_eligible_at", bo.nextEligibleAt)
		return
	}
	h.consoleAuto.mu.Unlock()

	if !h.consoleAuto.claimLaunch(hostID) {
		return
	}
	var width, height, fps int32
	if cfg.Mode != nil {
		width = int32(cfg.Mode.Width)
		height = int32(cfg.Mode.Height)
		fps = int32((cfg.Mode.RefreshMillihz + 500) / 1000)
	}
	sessionID, err := h.events.LaunchConsoleSession(ctx, hostID, *cfg.DefaultUser, *cfg.DefaultApp, cfg.VideoTopology(), width, height, fps)
	if err != nil {
		h.consoleAuto.finishLaunch(hostID, "", false)
		h.log.Warn("console auto-start: launch failed", "host_id", hostID, "connector", cfg.PinnedConnector(), "err", err)
		return
	}
	h.consoleAuto.finishLaunch(hostID, sessionID, true)
	h.consoleAuto.mu.Lock()
	bo.lastLaunchAt = now
	h.consoleAuto.mu.Unlock()
	h.log.Info("console auto-start: session launched", "host_id", hostID, "session_id", sessionID, "connector", cfg.PinnedConnector())
}

// connectorPresent reports whether the configured connector is present in a
// reported connector list. "auto" (the default) matches any connector being
// present at all — the admin hasn't pinned to a specific port.
func connectorPresent(connectors []string, configured string) bool {
	if configured == "auto" {
		return len(connectors) > 0
	}
	for _, c := range connectors {
		if c == configured {
			return true
		}
	}
	return false
}

func (h *Handler) writeError(conn *websocket.Conn, code, msg string) {
	_ = conn.WriteJSON(ErrorMsg{Type: "error", Code: code, Message: msg})
}

// resolveAuth decodes the auth field and calls the appropriate store method.
func (h *Handler) resolveAuth(ctx context.Context, reg RegisterMsg) (registerResult, error) {
	var tryEnroll AuthEnrollment
	if json.Unmarshal(reg.Auth, &tryEnroll) == nil && tryEnroll.EnrollmentToken != "" {
		return h.store.enrollHost(ctx, reg.NodeName, reg.AgentVersion,
			tryEnroll.EnrollmentToken, h.enrollmentToken)
	}
	var tryReconnect AuthReconnect
	if json.Unmarshal(reg.Auth, &tryReconnect) == nil && tryReconnect.NodeSecret != "" {
		return h.store.reconnectHost(ctx, reg.NodeName, reg.AgentVersion,
			tryReconnect.NodeSecret)
	}
	return registerResult{}, errors.New("auth must contain enrollment_token or node_secret")
}
