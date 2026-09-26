package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/actorsocket"
	"github.com/accreleus/quasar/control-plane/internal/agentws"
	"github.com/accreleus/quasar/control-plane/internal/buildinfo"
)

// The control plane's own machine, read from that machine's recovery actor over
// its control socket (QUASAR_RECOVERY_CONTROL_SOCKET). The socket is not frozen:
// its shapes are internal/actorsocket, pinned by testdata/recovery/socket.
// semantics: control-api.md §"The control plane's own machine" (amendment 14).

// DefaultInstallModeTTL bounds how stale this control plane's own install mode
// and preflight facts may be: seconds, not a boot-time read.
const DefaultInstallModeTTL = 30 * time.Second

const (
	ownMachineReadTimeout = 3 * time.Second
	// A status body is a few KiB; the cap only stops a runaway peer.
	ownMachineMaxBody = 1 << 20
)

// `database_mode` values.
const (
	DatabaseModeOwned    = "owned"
	DatabaseModeExternal = "external"
)

// MachineIdentity is the seven optional `PlatformIdentity` fields. nil is
// unknown, and all seven are always serialized. MachineRole and MachineNodeName
// come from this control plane's configuration (MachineShape), the rest from
// its recovery actor.
type MachineIdentity struct {
	InstallMode               *string `json:"install_mode"`
	RecoveryActorVersion      *string `json:"recovery_actor_version"`
	RecoveryActorSourceCommit *string `json:"recovery_actor_source_commit"`
	SeedVersion               *string `json:"seed_version"`
	DatabaseMode              *string `json:"database_mode"`
	MachineRole               *string `json:"machine_role"`
	MachineNodeName           *string `json:"machine_node_name"`
}

// `machine_role` values.
const (
	MachineRoleCombined    = "combined"
	MachineRoleControlOnly = "control_only"
)

// MachineShape is what the recovery actor that created this control plane
// wrote into its configuration (QUASAR_MACHINE_ROLE, QUASAR_MACHINE_NODE_NAME).
// Known without asking the actor; the zero value is not an owned machine.
type MachineShape struct {
	Role     string
	NodeName string
}

// Apply fills the two fields it owns; the others are left as they are.
func (s MachineShape) Apply(m MachineIdentity) MachineIdentity {
	if s.Role == "" || s.NodeName == "" {
		return m
	}
	role, node := s.Role, s.NodeName
	m.MachineRole, m.MachineNodeName = &role, &node
	return m
}

// CombinedNodeName is the node name of the agent sharing this control plane's
// machine, when it is a combined host.
func (s MachineShape) CombinedNodeName() (string, bool) {
	if s.Role != MachineRoleCombined || s.NodeName == "" {
		return "", false
	}
	return s.NodeName, true
}

// SharesMachineWith is whether the host named nodeName is the agent on this
// control plane's own machine, whose recovery actor moves in the control-plane
// step (A1, ADR 0008), never in a host step.
func (s MachineShape) SharesMachineWith(nodeName string) bool {
	own, ok := s.CombinedNodeName()
	return ok && own == nodeName
}

// PlatformIdentity is the wire `PlatformIdentity`: the binary's own stamps,
// plus the machine it runs on.
type PlatformIdentity struct {
	buildinfo.Identity
	MachineIdentity
}

// OwnMachine is what the recovery actor on this control plane's machine said.
type OwnMachine struct {
	Identity MachineIdentity
	// ActorVersion is as reported, for operator prose only.
	ActorVersion string
	// Conflicts are the race guard's owner conflicts, for the preflight.
	Conflicts []actorsocket.Conflict
}

// OwnMachineFromStatus derives the identity from one status answer. A value
// the contract cannot use is null, never passed through.
func OwnMachineFromStatus(st actorsocket.Status) OwnMachine {
	owned := InstallOwned
	m := OwnMachine{ActorVersion: st.Actor.Version, Conflicts: st.Conflicts}
	m.Identity.InstallMode = &owned
	if agentws.ValidRecoveryActorVersion(st.Actor.Version) {
		v := st.Actor.Version
		m.Identity.RecoveryActorVersion = &v
	}
	if agentws.ValidSourceCommit(st.Actor.Commit) {
		c := st.Actor.Commit
		m.Identity.RecoveryActorSourceCommit = &c
	}
	if st.Seed != nil && st.Seed.Version != "" {
		v := st.Seed.Version
		m.Identity.SeedVersion = &v
	}
	switch st.Database {
	case actorsocket.DatabaseOwned:
		v := DatabaseModeOwned
		m.Identity.DatabaseMode = &v
	case actorsocket.DatabaseExternal:
		v := DatabaseModeExternal
		m.Identity.DatabaseMode = &v
	}
	return m
}

// OwnMachineReader asks the recovery actor for `GET /v1/status` and reuses the
// answer, or the failure, for TTL (DefaultInstallModeTTL).
type OwnMachineReader struct {
	socket string
	http   *http.Client
	TTL    time.Duration
	// Log records a failed read; nil is slog.Default().
	Log *slog.Logger

	mu     sync.Mutex
	set    bool
	at     time.Time
	last   OwnMachine
	lastOK bool
	err    error
}

// NewOwnMachineReader reads socketPath; "" returns nil, which reads as not owned.
func NewOwnMachineReader(socketPath string) *OwnMachineReader {
	if socketPath == "" {
		return nil
	}
	dialer := &net.Dialer{Timeout: ownMachineReadTimeout}
	return &OwnMachineReader{
		socket: socketPath,
		TTL:    DefaultInstallModeTTL,
		http: &http.Client{
			Timeout: ownMachineReadTimeout,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return dialer.DialContext(ctx, "unix", socketPath)
				},
				DisableKeepAlives: true,
			},
		},
	}
}

// Read is the machine as last seen; ok false when the actor did not answer or
// its answer did not decode, and then every identity field is null.
func (r *OwnMachineReader) Read(ctx context.Context) (OwnMachine, bool) {
	m, ok, _, _ := r.read(ctx)
	return m, ok
}

// Identity is the five actor-read `PlatformIdentity` fields; all null when not owned.
func (r *OwnMachineReader) Identity(ctx context.Context) MachineIdentity {
	m, _ := r.Read(ctx)
	return m.Identity
}

// InstallMode is this control plane's own install mode on an owned machine:
// `owned` while its recovery actor answers, else nil ("nobody could say"),
// which the plan never treats as eligible.
func (r *OwnMachineReader) InstallMode() *string {
	return r.Identity(context.Background()).InstallMode
}

// Invalidate drops the cached answer: the apply endpoints call it before deciding.
func (r *OwnMachineReader) Invalidate() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.set = false
	r.mu.Unlock()
}

// PreflightFacts is the control-plane target's facts on an owned machine.
func (r *OwnMachineReader) PreflightFacts(ctx context.Context) PreflightFacts {
	if r == nil {
		return PreflightFacts{}
	}
	m, ok, at, err := r.read(ctx)
	fact := &OwnedActorFact{Socket: r.socket, Answered: ok, Version: m.ActorVersion, Conflicts: m.Conflicts}
	if err != nil {
		fact.Err = err.Error()
	}
	return PreflightFacts{CheckedAt: &at, OwnedActor: fact}
}

func (r *OwnMachineReader) read(ctx context.Context) (OwnMachine, bool, time.Time, error) {
	if r == nil {
		return OwnMachine{}, false, time.Time{}, errors.New("not an owned machine")
	}
	r.mu.Lock()
	if r.set && time.Since(r.at) < r.TTL {
		defer r.mu.Unlock()
		return r.last, r.lastOK, r.at, r.err
	}
	r.mu.Unlock()

	// Detached from the request: the answer is shared for TTL, so one caller's
	// cancellation (an admin navigating away) must not be cached for everyone.
	st, err := r.status(context.WithoutCancel(ctx))
	var m OwnMachine
	if err == nil {
		m = OwnMachineFromStatus(st)
		if st.Actor.Commit != "" && m.Identity.RecoveryActorSourceCommit == nil {
			log := r.Log
			if log == nil {
				log = slog.Default()
			}
			log.Warn("the recovery actor reports a commit that is not a commit; its actor-first ordering treats it as behind",
				"socket", r.socket, "commit", st.Actor.Commit)
		}
	} else {
		log := r.Log
		if log == nil {
			log = slog.Default()
		}
		log.Warn("recovery actor status read failed", "socket", r.socket, "err", err)
	}
	now := time.Now()
	r.mu.Lock()
	r.set, r.at, r.last, r.lastOK, r.err = true, now, m, err == nil, err
	r.mu.Unlock()
	return m, err == nil, now, err
}

func (r *OwnMachineReader) status(ctx context.Context) (actorsocket.Status, error) {
	cctx, cancel := context.WithTimeout(ctx, ownMachineReadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, "http://recovery/v1/status", nil)
	if err != nil {
		return actorsocket.Status{}, err
	}
	resp, err := r.http.Do(req)
	if err != nil {
		return actorsocket.Status{}, fmt.Errorf("no answer: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return actorsocket.Status{}, fmt.Errorf("status %d", resp.StatusCode)
	}
	var st actorsocket.Status
	if err := json.NewDecoder(io.LimitReader(resp.Body, ownMachineMaxBody)).Decode(&st); err != nil {
		return actorsocket.Status{}, fmt.Errorf("undecodable status: %w", err)
	}
	return st, nil
}
