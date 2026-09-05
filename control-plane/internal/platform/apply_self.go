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

// ConfiguredUpdaterSocket resolves the socket path; docs/configuration.md.
func ConfiguredUpdaterSocket() string {
	if v := os.Getenv("QUASAR_UPDATER_SOCKET"); v != "" {
		return v
	}
	return UpdaterSocketPath
}

// UpdaterAPI is the sliver of the local socket this package uses, as an
// interface so the self-apply is testable over a temp socket or a fake.
type UpdaterAPI interface {
	// Present reports whether an updater is installed beside this control
	// plane. False makes the control-plane target ineligible (`updater_absent`)
	// rather than an apply that fails halfway.
	Present() bool
	Apply(ctx context.Context, req updater.ApplyRequest) (updater.Accepted, error)
	Result(ctx context.Context, requestID string) (updater.Result, error)
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
	if c == nil || c.socket == "" {
		return false
	}
	_, err := os.Stat(c.socket)
	return err == nil
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

func (c *UpdaterClient) Apply(ctx context.Context, req updater.ApplyRequest) (updater.Accepted, error) {
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
}

// NewSelfApplier builds the control-plane applier with the contract's timings.
func NewSelfApplier(store selfStore, up UpdaterAPI, log logger) *SelfApplier {
	return &SelfApplier{
		store:        store,
		updater:      up,
		log:          log,
		Identity:     buildinfo.Get,
		Deadline:     DefaultApplyDeadline,
		PollInterval: DefaultApplyPoll,
	}
}

// UpdaterPresent reports whether this control plane could apply itself at all.
func (s *SelfApplier) UpdaterPresent() bool {
	return s.updater != nil && s.updater.Present()
}

// Apply drives one control-plane attempt to terminal — or, in the normal case,
// until the updater recreates this container and the process dies mid-poll.
// The next boot's Adopt is what resolves the row then.
func (s *SelfApplier) Apply(ctx context.Context, a Attempt) {
	if !s.UpdaterPresent() {
		// Refused rather than attempted: an apply with nothing to carry it out
		// is a failure with a name, not a timeout fifteen minutes later.
		s.fail(a.ID, ReasonUpdaterAbsentFailure, "no updater socket at "+ConfiguredUpdaterSocket())
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
		if err != nil {
			s.log.Error("self-apply: could not persist the request id", "attempt_id", a.ID, "err", err)
			s.fail(a.ID, ReasonUpdaterUnreachable, "")
			return
		}
		if !s.send(dctx, a, requestID) {
			return
		}
		s.poll(dctx, a.ID, requestID)
		return
	}
	requestID, err := s.store.AttemptRequestID(ctx, a.ID)
	if err != nil || requestID == "" {
		s.log.Error("self-apply: a sent attempt carries no request id", "attempt_id", a.ID, "err", err)
		s.fail(a.ID, ReasonUpdaterUnreachable, "")
		return
	}
	s.poll(dctx, a.ID, requestID)
}

func (s *SelfApplier) send(ctx context.Context, a Attempt, requestID string) bool {
	var ref updater.Release
	if a.ReleaseID != nil {
		rel, err := s.store.Release(ctx, *a.ReleaseID)
		if err != nil {
			s.log.Error("self-apply: could not read the release", "attempt_id", a.ID, "err", err)
			s.fail(a.ID, ReasonInvalid, "")
			return false
		}
		ref = updater.Release{ID: rel.ID, Version: rel.Version, SourceCommit: rel.SourceCommit}
	}
	components := make([]updater.Component, 0, len(a.RequestedDigests))
	for _, c := range a.RequestedDigests {
		components = append(components, updater.Component{Name: c.Name, Image: c.Image, Digest: c.Digest})
	}
	accepted, err := s.updater.Apply(ctx, updater.ApplyRequest{
		RequestID: requestID, Components: components, Release: ref,
	})
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

// poll relays the result file onto the attempt until it is terminal. It
// normally does not return: the recreate kills this process partway through,
// and Adopt finishes the row on the next boot.
func (s *SelfApplier) poll(ctx context.Context, attemptID, requestID string) {
	for {
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				s.log.Warn("self-apply: deadline expired with no terminal state", "attempt_id", attemptID)
				s.fail(attemptID, ReasonTimeout, "")
			}
			return
		case <-time.After(s.PollInterval):
		}
		if a, err := s.store.Attempt(ctx, attemptID); err == nil && TerminalAttemptState(a.State) {
			return
		}
		res, err := s.updater.Result(ctx, requestID)
		if err != nil {
			continue // no result file yet, or a socket that went away with us
		}
		if s.record(ctx, attemptID, res) {
			return
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
// Returns false when the attempt is still open: it was never sent, and the
// caller re-drives it.
func (s *SelfApplier) Adopt(ctx context.Context, a Attempt, wantCommit string) bool {
	id := s.Identity()
	if wantCommit != "" && id.SourceCommit != nil && commitsMatch(*id.SourceCommit, wantCommit) {
		if done, err := s.store.SucceedAttempt(ctx, a.ID); err != nil {
			s.log.Warn("self-apply: could not resolve the adopted attempt", "attempt_id", a.ID, "err", err)
			return false
		} else if done {
			s.log.Info("control-plane apply succeeded: this build is serving on the release's commit",
				"attempt_id", a.ID, "source_commit", *id.SourceCommit)
		}
		return true
	}
	requestID, err := s.store.AttemptRequestID(ctx, a.ID)
	if err != nil {
		s.log.Warn("self-apply: could not read the attempt's request id", "attempt_id", a.ID, "err", err)
		return false
	}
	if requestID == "" {
		return false // never sent; the run re-drives it
	}
	s.log.Warn("re-adopting a control-plane apply left in flight by a restart",
		"attempt_id", a.ID, "state", a.State)
	started := a.CreatedAt
	if a.StartedAt != nil {
		started = *a.StartedAt
	}
	dctx, cancel := context.WithDeadline(ctx, started.Add(s.Deadline))
	defer cancel()
	s.poll(dctx, a.ID, requestID)
	return true
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
