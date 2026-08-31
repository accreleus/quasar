package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
	"github.com/accreleus/quasar/control-plane/internal/audit"
)

// Dispatch timeouts for the assign→start handshake; a missing ack fails the
// session (schema.md).
const (
	assignAckTimeout = 10 * time.Second
	startAckTimeout  = 10 * time.Second
	stopAckTimeout   = 10 * time.Second
)

// defaultStartToRunningTimeout bounds PROGRESS from acked-start to running,
// unlike startAckTimeout which bounds the ack itself. On expiry the watchdog
// fails the session, releasing its reservation, and tells the agent to tear down.
const defaultStartToRunningTimeout = 90 * time.Second

// swapAckTimeout bounds the agent's ack of a session_swap_app command.
const swapAckTimeout = 10 * time.Second

// traceAckTimeout bounds the agent's ack of a session_trace command. The admin
// trace toggle is synchronous, so it also bounds the HTTP request.
const traceAckTimeout = 10 * time.Second

// Swap state_detail tokens. The agent emits the matching detail in its
// session_state callbacks; the control plane recognises them to drive app_id.
const (
	swapDetailInProgress = "swapping"      // swap underway (top-level state stays running)
	swapDetailComplete   = "swap complete" // agent: new app is live ⇒ commit app_id
	swapDetailRejected   = "swap rejected" // agent ack:false / undeliverable ⇒ no-op
	swapDetailRolledBack = "rolled back"   // substring of the agent's rollback detail
)

// App-boot state_detail tokens (#484), passed straight through to state_detail.
// Neither string may ever equal a swapDetail* constant: that is what keeps them
// from being mistaken for a swap commit and moving app_id.
const (
	appDetailBooting   = "app booting"   // agent: container up, app has not drawn yet
	appDetailPresented = "app presented" // agent: app committed its first top-level surface
)

// Dispatcher is the subset of agentws.Registry the coordinator needs to reach an
// agent. An interface (not the concrete type) keeps the coordinator testable.
type Dispatcher interface {
	Send(hostID string, v any) error
	SendWithAck(ctx context.Context, hostID, id string, v any) (agentws.AckResult, error)
}

// Coordinator owns the session lifecycle: launches (schedule, assign, start),
// mapping the agent's session_state callbacks onto the authoritative state
// machine, and reaping a host's sessions when its agent drops. Implements
// agentws.Events.
type Coordinator struct {
	store      *Store
	dispatcher Dispatcher
	log        *slog.Logger
	// startToRunningTimeout is the stuck-start watchdog window; a field, not the
	// const, so tests can shorten it.
	startToRunningTimeout time.Duration

	// The coordinator's lifecycle context. Background watchdogs derive from ctx so
	// they cancel on shutdown; Close() cancels it.
	ctx    context.Context
	cancel context.CancelFunc

	// auditor records the terminal-failure edge; nil in tests.
	auditor audit.Recorder

	// swapper owns the app-swap lifecycle and its pendingSwaps map + mutex.
	swapper *swapper
	// health owns the health-run tracking maps + mutex.
	health *healthEvaluator
	// display owns the per-session external-resolution cache and its mutex.
	// Ephemeral by design — see displayState.
	display *displayState

	// homes is the P5-02 storage seam; nil means no provider, and managed-home
	// launches then fail loudly (WithHomeProvider).
	homes HomeProvider

	// micSettings is the mic-capture instance gate (migration 0049). Unlike
	// HomeProvider, nil is NOT loud: it resolves fail-closed to "never granted",
	// so an unwired deployment behaves as if the setting were off.
	micSettings MicCaptureProvider

	// jobs closes the job runs the host's previous agent process was executing,
	// on re-register (#492). nil is a quiet no-op.
	jobs JobReclaimer

	// certRuns tracks in-flight certification runs; one per host at a time.
	certRuns *certRunManager

	// forgetters hold per-session in-memory state to drop when a session goes
	// terminal (#402). Written once at construction, read-only after, so no lock
	// here; each implementation owns its own.
	forgetters []SessionForgetter

	// ConsoleReeval fires (hostID, sessionID) whenever a session goes terminal,
	// wired to agentws.Handler.ConsoleSessionTerminated. A plain func field, not
	// an agentws import: session implements agentws.Events, so the reverse edge
	// would cycle. nil is a valid no-op, and it is only ever called after the
	// transition commits — never on the lifecycle transaction's critical path.
	ConsoleReeval func(ctx context.Context, hostID, sessionID string)
}

// NewCoordinator builds a Coordinator.
func NewCoordinator(store *Store, dispatcher Dispatcher, log *slog.Logger, opts ...CoordinatorOption) *Coordinator {
	c := &Coordinator{
		store:                 store,
		dispatcher:            dispatcher,
		log:                   log,
		startToRunningTimeout: defaultStartToRunningTimeout,
	}
	c.ctx, c.cancel = context.WithCancel(context.Background())
	for _, o := range opts {
		o(c)
	}
	c.swapper = newSwapper(c.store, c.dispatcher, c.log, c.resolveHomeSpec)
	c.health = newHealthEvaluator(c.store, c.log, c.failSessionWithDetail)
	c.display = newDisplayState()
	c.certRuns = newCertRunManager()
	return c
}

// Close cancels the lifecycle context, stopping in-flight watchdogs. Idempotent.
func (c *Coordinator) Close() {
	if c.cancel != nil {
		c.cancel()
	}
}

// forgetTerminalSession drops per-session in-memory state from every registered
// forgetter (#402), at the same four terminal sites as healthEvaluator.forget:
// each collaborator's own eviction path is driven by its own protocol, so a
// session ending any other way leaks an entry. Takes no coordinator lock and is
// called with none held; each forgetter takes only its own mutex.
func (c *Coordinator) forgetTerminalSession(sessionID string) {
	for _, f := range c.forgetters {
		if f != nil {
			f.Forget(sessionID)
		}
	}
}

func (c *Coordinator) Swap(ctx context.Context, sessionID, newAppID string) (Session, error) {
	return c.swapper.Swap(ctx, sessionID, newAppID)
}

func (c *Coordinator) EvaluateClientHealth(ctx context.Context, sessionID string, sample ClientHealthSample) {
	c.health.EvaluateClientHealth(ctx, sessionID, sample)
}

// Launch is the profile-less entry point: tier selection capped by app defaults,
// beaten by explicit overrides. It returns as soon as the reservation commits
// (control-api.md); the client polls GET for starting→running. Errors:
// ErrNotFound (404), ErrNoHostAvailable / ErrCapacityExhausted (503, no row).
func (c *Coordinator) Launch(ctx context.Context, userID, appID string, ov StreamOverride) (LaunchResult, error) {
	return c.LaunchByProfile(ctx, userID, LaunchParams{AppID: appID, Override: ov})
}

// Stop transitions to stopping and tells the agent to tear down; the agent
// confirms via AgentState. Idempotent on a terminal session.
func (c *Coordinator) Stop(ctx context.Context, sessionID, reason string) (Session, error) {
	sess, err := c.store.Get(ctx, sessionID)
	if err != nil {
		return Session{}, err
	}
	if sess.State.IsTerminal() {
		return sess, nil
	}

	sess, err = c.store.Transition(ctx, sessionID, StateStopping, strptr("stop requested"), nil)
	if err != nil {
		return Session{}, err
	}

	if sess.HostID != nil {
		cmd := agentws.SessionStopCmd{Type: "session_stop", ID: newCmdID(), SessionID: sessionID, Reason: reason}
		// Best-effort: if the agent is gone the host-disconnect reaper already
		// drove this session terminal.
		actx, cancel := context.WithTimeout(ctx, stopAckTimeout)
		defer cancel()
		if _, err := c.dispatcher.SendWithAck(actx, *sess.HostID, cmd.ID, cmd); err != nil {
			c.log.Warn("session_stop dispatch failed", "session_id", sessionID, "err", err)
		}
	}
	return sess, nil
}

// commandOK sends a command and waits for its ack, failing the session on
// rejection/timeout/disconnect. Returns true iff the agent accepted.
func (c *Coordinator) commandOK(hostID, id string, cmd any, timeout time.Duration, sessionID, what string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	res, err := c.dispatcher.SendWithAck(ctx, hostID, id, cmd)
	if err != nil {
		c.failSession(sessionID, fmt.Sprintf("%s dispatch failed: %v", what, err))
		return false
	}
	if !res.OK {
		c.failSession(sessionID, fmt.Sprintf("agent rejected %s: %s", what, res.Error))
		return false
	}
	return true
}

// failSession records reason as error_message, leaving state_detail unchanged.
// A transition error (already terminal) is logged, not propagated.
func (c *Coordinator) failSession(sessionID, reason string) {
	c.failSessionWithDetail(sessionID, reason, nil)
}

// failSessionWithDetail also stamps state_detail, as the host_lost reap edge
// does: web/src/lib/streamHealth.ts keys the client banner on a state_detail
// prefix, not on error_message. A nil detail leaves it untouched (COALESCE).
func (c *Coordinator) failSessionWithDetail(sessionID, reason string, detail *string) {
	sess, err := c.store.Transition(context.Background(), sessionID, StateFailed, detail, &reason)
	if err != nil && !errors.Is(err, ErrInvalidTransition) {
		c.log.Error("fail session", "session_id", sessionID, "reason", reason, "err", err)
		return
	}
	c.log.Warn("session failed", "session_id", sessionID, "reason", reason)
	if err == nil {
		c.recordSessionFailed(context.Background(), sess, "control_plane", nil, reason)
		// No telemetry prune: terminal FREEZES a failed session's last window for
		// post-mortem retention (internal/telemetry) rather than deleting it.
		// Drop every per-session in-memory map so none leaks an entry.
		c.health.forget(sessionID)
		c.display.forget(sessionID)
		c.swapper.forget(sessionID) // #405: any orphaned pending swap
		// #402: a failed session's browser may never have attached, so nothing else
		// evicts the relay's buffered signaling frames.
		c.forgetTerminalSession(sessionID)
		// A CP-internal fault reached the same terminal state an agent-reported
		// failure would, so re-eval console auto-start now rather than at the next
		// capacity report. After the transition commits.
		c.fireConsoleReeval(sess.HostID, sessionID)
	}
}

// WithAuditor wires the audit store so the terminal-failure edge is recorded.
func WithAuditor(rec audit.Recorder) CoordinatorOption {
	return func(c *Coordinator) { c.auditor = rec }
}

// Bounds on the two free-text fields that can reach a session.failed row.
// admin_activity.details has a 4096-byte CHECK and both of these are strings the
// agent (or a wrapped Go error) chose the length of, so a single unbounded one
// could cost the row its whole reason for existing. Cut here rather than at the
// store: truncating there would silently mangle a details map some other caller
// built deliberately.
const (
	maxAuditStateDetail = 512
	maxAuditReason      = 200
)

// truncateBytes cuts s to at most n BYTES (the CHECK counts bytes), stepping
// back off a partial UTF-8 sequence so the result is still valid JSON text.
func truncateBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}

// recordSessionFailed writes the one audit row with NO acting admin — a failure
// is the instance's own event, which is what actor_user_id's nullability is for.
//
// reason is carried ONLY on the control-plane path, where nothing else records
// why: an agent-reported failure has failure_code plus the session row's
// error_message, but a dispatch rejection has only this string, and a
// session.failed with no explanation is a row an operator cannot act on.
func (c *Coordinator) recordSessionFailed(ctx context.Context, sess Session, source string, failureCode *string, reason string) {
	details := map[string]any{"reason_source": source}
	if sess.HostID != nil {
		details["host_id"] = *sess.HostID
	}
	if sess.StateDetail != nil {
		details["state_detail"] = truncateBytes(*sess.StateDetail, maxAuditStateDetail)
	}
	if failureCode != nil {
		details["failure_code"] = truncateBytes(*failureCode, maxAuditStateDetail)
	}
	if reason != "" {
		details["reason"] = truncateBytes(reason, maxAuditReason)
	}
	audit.TryRecord(ctx, c.auditor, "", "session.failed", "session", sess.ID, details)
}

// fireConsoleReeval invokes ConsoleReeval, if wired, for a terminal session that
// was placed on a host. Decoupled from the transaction it follows: the callback
// does its own DB read and must never block or fail the state machine.
func (c *Coordinator) fireConsoleReeval(hostID *string, sessionID string) {
	if c.ConsoleReeval == nil || hostID == nil {
		return
	}
	go c.ConsoleReeval(context.Background(), *hostID, sessionID)
}

// newCmdID generates a per-connection-unique command id for ack correlation.
func newCmdID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// capAndPick is the precedence for one stream parameter: an explicit override
// wins and is passed through UNCAPPED (operator intent); otherwise the tier,
// capped at the app default so a 720p app never launches 1080p.
func capAndPick(override *int32, tierVal, appDefault int32) int32 {
	if override != nil && *override > 0 {
		return *override
	}
	if tierVal <= 0 {
		return appDefault
	}
	if tierVal < appDefault {
		return tierVal
	}
	return appDefault
}

func pick(override *int32, def int32) int32 {
	if override != nil && *override > 0 {
		return *override
	}
	return def
}

// pickProfile returns the per-launch H.264 profile override, or the
// constrained-baseline floor when none is supplied. The handler has already
// validated a non-empty override against ValidH264Profile.
func pickProfile(override *string) string {
	if override != nil && *override != "" {
		return *override
	}
	return "constrained-baseline"
}

// nativeHighEligible reports whether the caller's latest probe describes a
// native client that can decode the rung's target H.264 profile. It never errors
// and never blocks a launch, and every uncertainty resolves to NOT eligible,
// keeping the constrained-baseline floor: a nil probe or a non-native
// client_type is ineligible; a constrained-baseline target is the floor itself;
// an explicit decode.h264.profiles list decides; and with no decode matrix,
// "main" only, NEVER "high".
func nativeHighEligible(dp *DeviceProbe, target string) bool {
	if dp == nil || dp.ClientType != "native" {
		return false
	}
	if target == "" || target == "constrained-baseline" {
		return true
	}
	if len(dp.H264DecodeProfiles) > 0 {
		for _, p := range dp.H264DecodeProfiles {
			if p == target {
				return true
			}
		}
		return false
	}
	return target == "main"
}

// formatRungVerdicts renders a rung walk for the launch log: "id:codec" for an
// accepted rung, "id:codec!reason" for a rejected one.
func formatRungVerdicts(vs []rungVerdict) []string {
	out := make([]string, 0, len(vs))
	for _, v := range vs {
		if v.Reject == "" {
			out = append(out, v.ID+":"+v.Codec)
			continue
		}
		out = append(out, v.ID+":"+v.Codec+"!"+v.Reject)
	}
	return out
}

// probeDecodeHeight is the probe's measured decode ceiling for logging; 0 when
// unmeasured (in which case the decode-height clamp does not fire).
func probeDecodeHeight(dp *DeviceProbe) int32 {
	if dp == nil {
		return 0
	}
	return dp.MaxDecodeHeight
}

// keys returns a map's keys for logging (order unspecified; nil-safe).
func keys(m map[string]bool) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func strptr(s string) *string { return &s }

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefI32(i *int32) int32 {
	if i == nil {
		return -1
	}
	return *i
}
