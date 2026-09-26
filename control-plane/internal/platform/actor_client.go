package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/actorsocket"
)

// ActorClient is the self-apply seam (UpdaterAPI) over an owned machine's
// control socket: the recovery actor beside this control plane replaces it
// (control-api.md §"Components of an apply on an owned machine"). The socket's
// shapes are internal/actorsocket, pinned by testdata/recovery/socket.
type ActorClient struct {
	socket string
	http   *http.Client
}

// NewActorClient dials socketPath (QUASAR_RECOVERY_CONTROL_SOCKET).
func NewActorClient(socketPath string) *ActorClient {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	return &ActorClient{
		socket: socketPath,
		http: &http.Client{
			// A submit answers after admission, which may fetch a signature.
			Timeout: 60 * time.Second,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return dialer.DialContext(ctx, "unix", socketPath)
				},
				// The actor closes every connection after one answer.
				DisableKeepAlives: true,
			},
		},
	}
}

func (c *ActorClient) SocketPath() string { return c.socket }

func (c *ActorClient) Present() bool { return c.SocketState().SocketExists }

func (c *ActorClient) SocketState() SocketState {
	var st SocketState
	if c == nil || c.socket == "" {
		return st
	}
	if _, err := os.Stat(filepath.Dir(c.socket)); err == nil {
		st.DirExists = true
	}
	if _, err := os.Stat(c.socket); err == nil {
		st.SocketExists = true
	}
	return st
}

// ActorRequest is the control-socket submit for one control-plane step. It
// leaves wait_timeout_s unset, so the actor's own verify timeout (300 s) is the
// new control plane's health budget; DefaultApplyDeadline bounds the attempt
// only while the actor is silent (apply_self.go followVerdict).
func ActorRequest(req SelfRequest) actorsocket.Request {
	out := actorsocket.Request{
		RequestID: req.RequestID,
		Kind:      actorsocket.KindReplace,
		Release: actorsocket.Release{
			ID: req.Release.ID, Version: req.Release.Version, SourceCommit: req.Release.SourceCommit,
		},
		Migrates:                req.Migrates,
		ExternalBackupConfirmed: req.ExternalBackupConfirmed,
		Components:              make([]actorsocket.Component, 0, len(req.Components)),
	}
	if req.SchemaVersion > 0 {
		v := int64(req.SchemaVersion)
		out.SchemaVersion = &v
	}
	if req.Migrates && req.FromVersion != "" {
		v := req.FromVersion
		out.FromVersion = &v
	}
	for _, comp := range req.Components {
		out.Components = append(out.Components, actorsocket.Component{Name: comp.Name, Image: comp.Image, Digest: comp.Digest})
	}
	return out
}

func (c *ActorClient) Apply(ctx context.Context, req SelfRequest) (SelfAccepted, error) {
	body, err := json.Marshal(ActorRequest(req))
	if err != nil {
		return SelfAccepted{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://recovery/v1/submit", bytes.NewReader(body))
	if err != nil {
		return SelfAccepted{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return SelfAccepted{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, ownMachineMaxBody))
	if resp.StatusCode != http.StatusAccepted {
		var rej actorsocket.Rejection
		if json.Unmarshal(raw, &rej) == nil && rej.Reason != "" {
			return SelfAccepted{}, &actorRefusal{Reason: string(rej.Reason), Message: rej.Message}
		}
		return SelfAccepted{}, fmt.Errorf("the recovery actor answered %d: %s", resp.StatusCode, string(raw))
	}
	var acc actorsocket.Accepted
	if err := json.Unmarshal(raw, &acc); err != nil {
		return SelfAccepted{}, fmt.Errorf("decode the recovery actor's 202: %w", err)
	}
	return SelfAccepted{RequestID: acc.RequestID, Previous: previousOfActor(acc.Previous)}, nil
}

// Result is the attempt's result from `GET /v1/status?request_id=`; the
// control socket answers only for attempts submitted on it.
func (c *ActorClient) Result(ctx context.Context, requestID string) (SelfResult, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://recovery/v1/status?request_id="+url.QueryEscape(requestID), nil)
	if err != nil {
		return SelfResult{}, err
	}
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return SelfResult{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return SelfResult{}, fmt.Errorf("the recovery actor answered %d", resp.StatusCode)
	}
	var st actorsocket.Status
	if err := json.NewDecoder(io.LimitReader(resp.Body, ownMachineMaxBody)).Decode(&st); err != nil {
		return SelfResult{}, fmt.Errorf("decode the recovery actor's status: %w", err)
	}
	if st.Result == nil || st.Result.RequestID != requestID {
		return SelfResult{}, ErrNoResult
	}
	return resultOfActor(*st.Result), nil
}

// resultOfActor re-frames the actor's result in `release_state`'s spellings, which
// it keeps by design.
func resultOfActor(r actorsocket.Result) SelfResult {
	out := SelfResult{
		RequestID:     r.RequestID,
		State:         string(r.State),
		Previous:      previousOfActor(r.Previous),
		Output:        r.Output,
		StartedAt:     r.StartedAt,
		UpdatedAt:     r.UpdatedAt,
		FinishedAt:    r.FinishedAt,
		Restored:      r.Restored,
		PreUpdateDump: r.Dump,
		Release: ReleaseRef{
			ID: r.Release.ID, Version: r.Release.Version, SourceCommit: r.Release.SourceCommit,
		},
	}
	if r.Reason != nil {
		reason := string(*r.Reason)
		out.Reason = &reason
	}
	for _, c := range r.Components {
		out.Components = append(out.Components, ComponentDigest{Name: c.Name, Image: c.Image, Digest: c.Digest})
	}
	return out
}

func previousOfActor(in []actorsocket.Previous) []PreviousDigest {
	out := make([]PreviousDigest, 0, len(in))
	for _, p := range in {
		out = append(out, PreviousDigest{Name: p.Name, Digest: p.Digest})
	}
	return out
}
