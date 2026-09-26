package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/buildinfo"
	"github.com/accreleus/quasar/control-plane/internal/updater"
)

// The control plane applying ITSELF, over the updater socket beside it on its
// own host — never over an agent connection (agent-api.md §release_apply: "the
// control plane NEVER asks an agent to update the control plane").
//
// The rule the whole file is shaped by: this process cannot report its own
// success, because carrying the apply out destroys it. So the request id is
// persisted BEFORE the socket call, and the evidence of success is this
// binary's own liveness on the release's commit after it reboots — whatever a
// late result file says (control-api.md §"The shape of an apply, once").

// UpdaterSocketPath is where the updater's socket is mounted in this container.
// Twin of the agent's release::DEFAULT_SOCKET and of the compose mount.
const UpdaterSocketPath = "/run/quasar-updater/updater.sock"

// DefaultInstallModeTTL bounds how stale this control plane's own install mode
// may be. Seconds, not a boot-time read: the stack can be re-composed under it.
const DefaultInstallModeTTL = 30 * time.Second

// ConfiguredUpdaterSocket resolves the socket path; docs/configuration.md.
func ConfiguredUpdaterSocket() string {
	if v := os.Getenv("QUASAR_UPDATER_SOCKET"); v != "" {
		return v
	}
	return UpdaterSocketPath
}

// SelfRequest is one control-plane apply as the self-applier hands it to the
// actor beside it: the Compose updater, or on an owned machine the recovery
// actor (actor_client.go). Components are in replacement order.
type SelfRequest struct {
	RequestID  string
	Components []ComponentDigest
	Release    ReleaseRef
	// Migrates is the release's schema above this binary's; SchemaVersion is the
	// schema it moves to, 0 when unknown.
	Migrates                bool
	SchemaVersion           int
	ExternalBackupConfirmed bool
}

// UpdaterAPI is the sliver of the local socket this package uses, as an
// interface so the self-apply is testable over a temp socket or a fake.
type UpdaterAPI interface {
	// Present reports whether an updater is installed beside this control
	// plane. False makes the control-plane target ineligible (`updater_absent`)
	// rather than an apply that fails halfway.
	Present() bool
	// SocketState is the three-way #184 diagnosis behind Present.
	SocketState() SocketState
	// Self is what the updater discovered about the stack it sits beside.
	Self(ctx context.Context) (UpdaterSelf, error)
	Apply(ctx context.Context, req SelfRequest) (updater.Accepted, error)
	Result(ctx context.Context, requestID string) (updater.Result, error)
	// SocketPath names the socket in an operator-facing failure.
	SocketPath() string
}

// verdictExecutor is an actor whose result is the verdict: the recovery actor
// keeps the old control plane until the new one passes its health check and
// restores it when it does not (ADR 0004 amendment), so the booted binary is
// the evidence only once that result is terminal. The Compose updater's result
// file lags its own recreate (#113 finding 2) and is not one.
type verdictExecutor interface{ ResultIsVerdict() bool }

// UpdaterSelf is the sliver of `GET /v1/self` this package reads. Declared here
// rather than imported so an older updater, whose answer carries no `images`,
// decodes to "unknown" instead of failing.
type UpdaterSelf struct {
	Version     string   `json:"version"`
	WorkingDir  string   `json:"working_dir"`
	ConfigFiles []string `json:"config_files"`
	// Per compose service, the compose-file set its running container carries
	// in its own labels (preflight `updater_overlays`). Absent on an older
	// updater, which preflight reads as unknown.
	ServiceConfigFiles map[string][]string `json:"service_config_files"`
	// Component name → the effective image reference compose would use for its
	// service, defaults included.
	Images map[string]string `json:"images"`
}

// ClassifyImageRef is the Go twin of node-agent buildinfo::classify_image_ref:
// registry when the reference names a registry host or pins a digest, source
// when it is a bare local tag like `quasar-control-plane:latest`. "" when
// nothing can be said.
//
// The host test is docker's own: the first path segment is a registry only if
// it contains a `.` or a `:`, or is exactly `localhost`.
func ClassifyImageRef(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	// A digest pin can only have come from a registry (ADR 0001).
	if strings.Contains(ref, "@sha256:") {
		return InstallRegistry
	}
	first, _, hasSlash := strings.Cut(ref, "/")
	if hasSlash && (first == "localhost" || strings.ContainsAny(first, ".:")) {
		return InstallRegistry
	}
	return InstallSource
}

// UpdaterClient speaks the local socket. Its request body and result file are
// NOT a frozen interface (schema.md §"Not frozen: the updater's local socket");
// both ends ship in the same release, which is why the types are imported from
// internal/updater rather than re-declared.
type UpdaterClient struct {
	socket string
	http   *http.Client
}

// NewUpdaterClient dials the unix socket. The host in the URL is a placeholder:
// the transport ignores it.
func NewUpdaterClient(socket string) *UpdaterClient {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	return &UpdaterClient{
		socket: socket,
		http: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return dialer.DialContext(ctx, "unix", socket)
				},
			},
		},
	}
}

func (c *UpdaterClient) Present() bool {
	return c.SocketState().SocketExists
}

// SocketState stats the mount directory and then the socket, so "volume not
// mounted" and "updater not running" are told apart (#184).
func (c *UpdaterClient) SocketState() SocketState {
	if c == nil || c.socket == "" {
		return SocketState{}
	}
	var st SocketState
	if _, err := os.Stat(filepath.Dir(c.socket)); err == nil {
		st.DirExists = true
	}
	if _, err := os.Stat(c.socket); err == nil {
		st.SocketExists = true
	}
	return st
}

// updaterError carries the socket's rejection identifier, which is already the
// closed `release_state.reason` vocabulary.
type updaterError struct {
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

func (e *updaterError) Error() string {
	if e.Message == "" {
		return e.Reason
	}
	return e.Reason + ": " + e.Message
}

// SocketPath is where this client dials.
func (c *UpdaterClient) SocketPath() string {
	if c == nil {
		return ""
	}
	return c.socket
}

// Apply sends the request in the updater's own shape, which has no notion of
// a migration: the Compose updater recreates whatever it is given.
func (c *UpdaterClient) Apply(ctx context.Context, sr SelfRequest) (updater.Accepted, error) {
	req := updater.ApplyRequest{RequestID: sr.RequestID, Release: updater.Release{
		ID: sr.Release.ID, Version: sr.Release.Version, SourceCommit: sr.Release.SourceCommit,
	}}
	for _, c := range sr.Components {
		req.Components = append(req.Components, updater.Component{Name: c.Name, Image: c.Image, Digest: c.Digest})
	}
	body, err := json.Marshal(req)
	if err != nil {
		return updater.Accepted{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://updater/v1/apply", bytes.NewReader(body))
	if err != nil {
		return updater.Accepted{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return updater.Accepted{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, updater.MaxRequestBytes))
	if resp.StatusCode != http.StatusAccepted {
		var e updaterError
		if json.Unmarshal(raw, &e) == nil && e.Reason != "" {
			return updater.Accepted{}, &e
		}
		return updater.Accepted{}, fmt.Errorf("updater answered %d: %s", resp.StatusCode, string(raw))
	}
	var out updater.Accepted
	if err := json.Unmarshal(raw, &out); err != nil {
		return updater.Accepted{}, fmt.Errorf("decode updater 202: %w", err)
	}
	return out, nil
}

func (c *UpdaterClient) Self(ctx context.Context) (UpdaterSelf, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://updater/v1/self", nil)
	if err != nil {
		return UpdaterSelf{}, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return UpdaterSelf{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return UpdaterSelf{}, fmt.Errorf("updater answered %d: %s", resp.StatusCode, string(raw))
	}
	var out UpdaterSelf
	if err := json.Unmarshal(raw, &out); err != nil {
		return UpdaterSelf{}, fmt.Errorf("decode updater self: %w", err)
	}
	return out, nil
}

// ErrNoResult is a request id the updater has written no result file for yet —
// normal for the first seconds of an apply, and on boot before the executor
// has re-stamped it.
var ErrNoResult = errors.New("no result for this request id")

func (c *UpdaterClient) Result(ctx context.Context, requestID string) (updater.Result, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://updater/v1/results/"+requestID, nil)
	if err != nil {
		return updater.Result{}, err
	}
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return updater.Result{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusNotFound {
		return updater.Result{}, ErrNoResult
	}
	if resp.StatusCode != http.StatusOK {
		return updater.Result{}, fmt.Errorf("updater answered %d: %s", resp.StatusCode, string(raw))
	}
	var out updater.Result
	if err := json.Unmarshal(raw, &out); err != nil {
		return updater.Result{}, fmt.Errorf("decode updater result: %w", err)
	}
	return out, nil
}

// selfStore is the persistence the self-apply needs.
type selfStore interface {
	MintRequestID(ctx context.Context, attemptID string) (string, error)
	FailAttempt(ctx context.Context, attemptID, reason, output string) error
	SucceedAttempt(ctx context.Context, attemptID string) (bool, error)
	Attempt(ctx context.Context, attemptID string) (Attempt, error)
	AttemptRequestID(ctx context.Context, attemptID string) (string, error)
	RecordReleaseState(ctx context.Context, attemptID, state string, previous []PreviousDigest, output string) error
	SetPreviousDigests(ctx context.Context, attemptID string, previous []PreviousDigest) error
	Release(ctx context.Context, id string) (Release, error)
}

// SelfApplier drives the control-plane target of a fleet run.
type SelfApplier struct {
	store   selfStore
	updater UpdaterAPI
	log     logger

	// Identity is this binary's own build stamps; a field so a test can supply
	// the "booted on the new release" case a test binary cannot have.
	Identity     func() buildinfo.Identity
	Deadline     time.Duration
	PollInterval time.Duration
	// How long a read of the updater's self-report is reused. Short rather
	// than cached at boot: the stack can be re-composed under a running control
	// plane, and a report read once at start would then be a lie for its whole
	// life.
	InstallModeTTL time.Duration
	// DeveloperCommit reads the commit a developer apply's images carry: its
	// release_apply provenance and its success evidence, since the row names no
	// release. Nil fails such an attempt before the send.
	DeveloperCommit func(ctx context.Context, components []ComponentDigest) (string, error)
	// VerdictSilence is how long, from the apply deadline on, a verdict executor
	// may give no answer for a request before the attempt times out.
	VerdictSilence time.Duration

	mu      sync.Mutex
	self    UpdaterSelf
	selfErr error
	selfAt  time.Time
	selfSet bool
}

// NewSelfApplier builds the control-plane applier with the contract's timings.
func NewSelfApplier(store selfStore, up UpdaterAPI, log logger) *SelfApplier {
	return &SelfApplier{
		store:          store,
		updater:        up,
		log:            log,
		Identity:       buildinfo.Get,
		Deadline:       DefaultApplyDeadline,
		PollInterval:   DefaultApplyPoll,
		InstallModeTTL: DefaultInstallModeTTL,
		VerdictSilence: DefaultVerdictSilence,
	}
}

// DefaultVerdictSilence outlasts a hand-over's control-socket gap (the successor
// re-binds it) with room to spare.
const DefaultVerdictSilence = 2 * time.Minute

// UpdaterPresent reports whether this control plane could apply itself at all.
func (s *SelfApplier) UpdaterPresent() bool {
	return s.updater != nil && s.updater.Present()
}

// InstallMode is how THIS control plane got its image, learned from the updater
// beside it: it reads the compose config, so a default like
// `quasar-control-plane:latest` is seen as the source install it is. nil is
// "nobody could say", which is never treated as registry.
//
// A source-built control plane must never be offered a registry image: the two
// images are different builds with different uids, and replacing one with the
// other leaves a container that starts and then cannot write its own TLS volume
// — a crash-loop with no console left to fix it from.
func (s *SelfApplier) InstallMode() *string {
	self, _, err := s.selfReport(context.Background())
	if err != nil {
		return nil
	}
	mode := ClassifyImageRef(self.Images[ComponentControlPlane])
	if mode == "" {
		return nil
	}
	return &mode
}

// selfReport is `GET /v1/self` over the socket, reused for InstallModeTTL. The
// error is cached too: a failing updater is asked once per TTL, not once per
// view read.
func (s *SelfApplier) selfReport(ctx context.Context) (UpdaterSelf, time.Time, error) {
	s.mu.Lock()
	if s.selfSet && time.Since(s.selfAt) < s.InstallModeTTL {
		defer s.mu.Unlock()
		return s.self, s.selfAt, s.selfErr
	}
	s.mu.Unlock()

	var self UpdaterSelf
	var err error
	if s.updater == nil || !s.updater.Present() {
		err = errors.New("no updater socket")
	} else {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		self, err = s.updater.Self(cctx)
		cancel()
		if err != nil {
			s.log.Warn("could not read this control plane's updater self-report", "err", err)
		}
	}
	now := time.Now()
	s.mu.Lock()
	s.self, s.selfErr, s.selfAt, s.selfSet = self, err, now, true
	s.mu.Unlock()
	return self, now, err
}

// InvalidateSelf drops the cached self-report so the next read asks the
// updater again: the apply endpoints call it before deciding.
func (s *SelfApplier) InvalidateSelf() {
	s.mu.Lock()
	s.selfSet = false
	s.mu.Unlock()
}

// PreflightFacts is what preflight can learn about this control plane's own
// stack: the socket three-way, and the updater's self-report when it answers.
func (s *SelfApplier) PreflightFacts(ctx context.Context) PreflightFacts {
	f := PreflightFacts{}
	if s.updater == nil {
		return f
	}
	st := s.updater.SocketState()
	f.Socket = &st
	if !st.SocketExists {
		return f
	}
	self, at, err := s.selfReport(ctx)
	f.CheckedAt = &at
	facts := &UpdaterSelfFacts{}
	if err != nil {
		facts.Err = err.Error()
	} else {
		facts.Version = self.Version
		facts.StackDir = self.WorkingDir
		facts.ConfigFiles = self.ConfigFiles
		facts.ServiceConfigFiles = self.ServiceConfigFiles
	}
	f.Self = facts
	return f
}

// Apply drives one control-plane attempt to terminal — or, in the normal case,
// until the updater recreates this container and the process dies mid-poll.
// The next boot's Adopt is what resolves the row then.
func (s *SelfApplier) Apply(ctx context.Context, a Attempt) {
	if !s.UpdaterPresent() {
		// Refused rather than attempted: an apply with nothing to carry it out
		// is a failure with a name, not a timeout fifteen minutes later.
		s.fail(a.ID, ReasonUpdaterAbsentFailure, "no socket to apply the control plane over at "+s.socketPath())
		return
	}

	started := a.CreatedAt
	if a.StartedAt != nil {
		started = *a.StartedAt
	}
	dctx, cancel := context.WithDeadline(ctx, started.Add(s.Deadline))
	defer cancel()

	if a.State == AttemptQueued || a.State == AttemptWaitingSessions {
		// Persisted before the call, because the process that made it is
		// normally destroyed by carrying it out.
		requestID, err := s.store.MintRequestID(dctx, a.ID)
		if errors.Is(err, ErrAttemptNotFound) {
			return // resolved underneath us: a cancel, or a boot-adopted terminal state
		}
		if err != nil && ctx.Err() != nil {
			return // shutting down, not a failure of the apply
		}
		if err != nil {
			s.log.Error("self-apply: could not persist the request id", "attempt_id", a.ID, "err", err)
			s.fail(a.ID, ReasonUpdaterUnreachable, "")
			return
		}
		if !s.send(dctx, a, requestID) {
			return
		}
		s.poll(ctx, a, requestID)
		return
	}
	requestID, err := s.store.AttemptRequestID(ctx, a.ID)
	if ctx.Err() != nil {
		return // shutting down: the next boot's Adopt resolves the row
	}
	if err != nil || requestID == "" {
		s.log.Error("self-apply: a sent attempt carries no request id", "attempt_id", a.ID, "err", err)
		s.fail(a.ID, ReasonUpdaterUnreachable, "")
		return
	}
	s.poll(ctx, a, requestID)
}

func (s *SelfApplier) send(ctx context.Context, a Attempt, requestID string) bool {
	req := SelfRequest{RequestID: requestID, Components: a.RequestedDigests}
	if a.ReleaseID != nil {
		rel, err := s.store.Release(ctx, *a.ReleaseID)
		if err != nil {
			s.log.Error("self-apply: could not read the release", "attempt_id", a.ID, "err", err)
			s.fail(a.ID, ReasonInvalid, "")
			return false
		}
		req.Release = ReleaseRef{ID: rel.ID, Version: rel.Version, SourceCommit: rel.SourceCommit}
		req.SchemaVersion = rel.SchemaVersion
		req.Migrates = rel.SchemaVersion > s.Identity().SchemaVersion
	} else {
		// A developer apply: its provenance is the commit its images carry
		// (control-api.md §"Developer apply"). The endpoint refuses a migrating
		// one, so what reaches here never migrates.
		commit, err := s.developerCommit(ctx, a)
		if err != nil {
			s.log.Error("self-apply: could not read the developer apply's commit", "attempt_id", a.ID, "err", err)
			s.fail(a.ID, ReasonInvalid, "the images' build identity could not be read: "+err.Error())
			return false
		}
		req.Release = ReleaseRef{SourceCommit: commit}
	}
	accepted, err := s.updater.Apply(ctx, req)
	if err != nil {
		var rej *updaterError
		if errors.As(err, &rej) {
			// The socket's identifiers are already the closed reason
			// vocabulary; an unrecognised one is stored verbatim.
			s.log.Warn("self-apply: the updater refused", "attempt_id", a.ID, "reason", rej.Reason)
			s.fail(a.ID, rej.Reason, rej.Message)
			return false
		}
		s.log.Error("self-apply: could not reach the updater", "attempt_id", a.ID, "err", err)
		s.fail(a.ID, ReasonUpdaterUnreachable, err.Error())
		return false
	}
	// The 202 already knows what this control plane was on; recording it now
	// means a failure's manual restore is copy-paste even if nothing else is
	// ever reported.
	if prev := previousFromUpdater(accepted.Previous); len(prev) > 0 {
		if err := s.store.SetPreviousDigests(ctx, a.ID, prev); err != nil {
			s.log.Warn("self-apply: could not record previous digests", "attempt_id", a.ID, "err", err)
		}
	}
	s.log.Info("self-apply: accepted by the updater", "attempt_id", a.ID, "request_id", requestID)
	return true
}

// poll relays the result onto the attempt until it is terminal. It normally
// does not return: the recreate kills this process partway through, and Adopt
// finishes the row on the next boot. ctx carries no deadline: the apply
// deadline is measured here from the attempt's start. A cancelled ctx is this
// process shutting down, which leaves the row open for the next boot.
func (s *SelfApplier) poll(ctx context.Context, a Attempt, requestID string) {
	if s.verdicts() {
		if s.followVerdict(ctx, a, requestID) == verdictTimeout {
			s.fail(a.ID, ReasonTimeout, "the recovery actor did not answer for this request within the apply deadline")
		}
		return
	}
	dctx, cancel := context.WithDeadline(ctx, attemptStart(a).Add(s.Deadline))
	defer cancel()
	for {
		// Read before honouring the deadline: a restored control plane may boot
		// after it, onto a result that is already terminal.
		// The reads take ctx, not dctx: an expired deadline must not fail them.
		if cur, err := s.store.Attempt(ctx, a.ID); err == nil && TerminalAttemptState(cur.State) {
			return
		}
		if res, err := s.updater.Result(ctx, requestID); err == nil && s.record(ctx, a.ID, res) {
			return
		}
		select {
		case <-dctx.Done():
			if errors.Is(dctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
				s.log.Warn("self-apply: deadline expired with no terminal state", "attempt_id", a.ID)
				s.fail(a.ID, ReasonTimeout, "")
			}
			return
		case <-time.After(s.PollInterval):
		}
	}
}

// verdict is how following a verdict executor's result ended.
type verdict int

const (
	// verdictResolved: the attempt is terminal.
	verdictResolved verdict = iota
	// verdictShutdown: this process is stopping (the actor stops the control
	// plane it replaces or restores). Nothing was written, so the row stays
	// open and the next boot's Adopt records the actor's real verdict.
	verdictShutdown
	// verdictTimeout: from the apply deadline on, the actor gave no answer for
	// this request for VerdictSilence. Fail closed, never success.
	verdictTimeout
)

func (s *SelfApplier) verdicts() bool {
	v, ok := s.updater.(verdictExecutor)
	return ok && v.ResultIsVerdict()
}

func attemptStart(a Attempt) time.Time {
	if a.StartedAt != nil {
		return *a.StartedAt
	}
	return a.CreatedAt
}

// followVerdict relays the recovery actor's result for requestID until it is
// terminal. While the actor answers a non-terminal state it is waited for past
// the apply deadline: its own verify and restore timeouts bound the attempt, and
// a control plane on a slow link may boot after the deadline and still be put
// back. Only an actor that has not answered for this request, from the deadline
// on, for VerdictSilence times the attempt out.
func (s *SelfApplier) followVerdict(ctx context.Context, a Attempt, requestID string) verdict {
	deadline := attemptStart(a).Add(s.Deadline)
	var answered time.Time
	for {
		if cur, err := s.store.Attempt(ctx, a.ID); err == nil && TerminalAttemptState(cur.State) {
			return verdictResolved
		}
		if res, err := s.updater.Result(ctx, requestID); err == nil {
			if s.record(ctx, a.ID, res) {
				return verdictResolved
			}
			if res.State != updater.StateSucceeded && res.State != updater.StateFailed {
				answered = time.Now()
			}
		}
		if ctx.Err() != nil {
			return verdictShutdown
		}
		if now := time.Now(); !now.Before(deadline) && now.Sub(answered) >= s.VerdictSilence {
			s.log.Warn("self-apply: the recovery actor gave no verdict by the apply deadline",
				"attempt_id", a.ID, "request_id", requestID)
			return verdictTimeout
		}
		select {
		case <-ctx.Done():
			return verdictShutdown
		case <-time.After(s.PollInterval):
		}
	}
}

// record writes one result onto the attempt and reports whether it resolved it.
func (s *SelfApplier) record(ctx context.Context, attemptID string, res updater.Result) bool {
	prev := previousFromUpdater(res.Previous)
	switch res.State {
	case updater.StateSucceeded:
		if _, err := s.store.SucceedAttempt(ctx, attemptID); err != nil {
			s.log.Warn("self-apply: could not record success", "attempt_id", attemptID, "err", err)
			return false
		}
		return true
	case updater.StateFailed:
		reason := ReasonInvalid
		if res.Reason != nil && *res.Reason != "" {
			reason = *res.Reason
		}
		if len(prev) > 0 {
			_ = s.store.SetPreviousDigests(ctx, attemptID, prev)
		}
		if err := s.store.FailAttempt(ctx, attemptID, reason, res.Output); err != nil {
			s.log.Warn("self-apply: could not record failure", "attempt_id", attemptID, "err", err)
			return false
		}
		s.log.Warn("control-plane apply failed", "attempt_id", attemptID, "reason", reason, "restored", res.Restored)
		return true
	default:
		if !wireAttemptStates[res.State] {
			return false
		}
		if err := s.store.RecordReleaseState(ctx, attemptID, res.State, prev, res.Output); err != nil {
			s.log.Warn("self-apply: could not record progress", "attempt_id", attemptID, "err", err)
		}
		return false
	}
}

// Adopt resolves a control-plane attempt left non-terminal by the restart it
// caused. Order matters and is the contract's:
//
//  1. This binary is serving on the release's commit — the attempt succeeded,
//     whatever a late result file says. Reading the result once at boot is
//     wrong: the new container is up while the executor is still recreating
//     (#113 finding 2).
//  2. Otherwise poll the result id until it is terminal. A never-started apply
//     is auto-restored by the updater, which brings the OLD build back — so
//     this is exactly the branch a restore lands in, and it is recorded failed
//     with its reason (#113 finding 5).
//
// On an owned machine (a verdict executor) step 1 waits for the recovery
// actor's terminal result instead: the actor may still put the old control
// plane back, stopping this one first.
//
// Returns false when the attempt is still open: it was never sent, and the
// caller re-drives it. A cancelled ctx (this process shutting down) returns
// true with nothing written; the caller must not re-drive then either.
func (s *SelfApplier) Adopt(ctx context.Context, a Attempt, wantCommit string) bool {
	id := s.Identity()
	if wantCommit != "" && id.SourceCommit != nil && commitsMatch(*id.SourceCommit, wantCommit) {
		// On an owned machine this build may yet be put back: it is the evidence
		// only once the recovery actor has verified it.
		if s.verdicts() {
			requestID, err := s.store.AttemptRequestID(ctx, a.ID)
			if ctx.Err() != nil {
				return true // shutting down: nothing is decided, the next boot adopts
			}
			if err == nil && requestID != "" {
				if s.followVerdict(ctx, a, requestID) == verdictTimeout {
					s.fail(a.ID, ReasonTimeout, "the recovery actor gave no verdict within the apply deadline; this build is serving but was never verified")
				}
				return true
			}
		}
		if done, err := s.store.SucceedAttempt(ctx, a.ID); err != nil {
			if ctx.Err() != nil {
				return true
			}
			s.log.Warn("self-apply: could not resolve the adopted attempt", "attempt_id", a.ID, "err", err)
			return false
		} else if done {
			s.log.Info("control-plane apply succeeded: this build is serving on the release's commit",
				"attempt_id", a.ID, "source_commit", *id.SourceCommit)
		}
		return true
	}
	requestID, err := s.store.AttemptRequestID(ctx, a.ID)
	if ctx.Err() != nil {
		return true // shutting down: nothing is decided, the next boot adopts
	}
	if err != nil {
		s.log.Warn("self-apply: could not read the attempt's request id", "attempt_id", a.ID, "err", err)
		return false
	}
	if requestID == "" {
		return false // never sent; the run re-drives it
	}
	s.log.Warn("re-adopting a control-plane apply left in flight by a restart",
		"attempt_id", a.ID, "state", a.State)
	s.poll(ctx, a, requestID)
	return true
}

// developerCommit is the commit a developer apply's images carry.
func (s *SelfApplier) developerCommit(ctx context.Context, a Attempt) (string, error) {
	if s.DeveloperCommit == nil {
		return "", errors.New("no registry reader is wired to read the images' commit")
	}
	return s.DeveloperCommit(ctx, a.RequestedDigests)
}

func (s *SelfApplier) socketPath() string {
	if s.updater == nil {
		return ConfiguredUpdaterSocket()
	}
	return s.updater.SocketPath()
}

func (s *SelfApplier) fail(attemptID, reason, output string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.store.FailAttempt(ctx, attemptID, reason, output); err != nil {
		s.log.Error("self-apply: could not record the failure", "attempt_id", attemptID, "reason", reason, "err", err)
	}
}

func previousFromUpdater(in []updater.PreviousComponent) []PreviousDigest {
	if len(in) == 0 {
		return nil
	}
	out := make([]PreviousDigest, 0, len(in))
	for _, p := range in {
		out = append(out, PreviousDigest{Name: p.Name, Digest: p.Digest})
	}
	return out
}
