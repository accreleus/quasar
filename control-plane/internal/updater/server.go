package updater

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// The local HTTP surface: four routes over a unix socket in a volume shared by
// one host's containers (protocol/schema.md §"Not frozen: the updater's local
// socket").
//
// Authorisation is the REQUEST, never the caller. SO_PEERCRED yields the
// caller's uid but pid 0 (another pid namespace), so it is a sanity check and
// not authentication; what constrains this program is the namespace allowlist
// and the digest rules in plan.go, which hold whoever connects.

// MaxRequestBytes bounds the body; a larger one is not a real request.
const MaxRequestBytes = 64 * 1024

// Server holds everything a request needs.
type Server struct {
	Store  *Store
	Docker Docker
	Cfg    Config

	// EnvPath is the stack's `.env`, at its host path.
	EnvPath string

	PullTimeout     time.Duration
	RecreateTimeout time.Duration

	// Reported by `/v1/self`.
	Version string
}

// Handler wires the routes, separate from Listen so a test can serve it over a
// temp socket.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.HandleFunc("GET /v1/self", s.handleSelf)
	mux.HandleFunc("POST /v1/apply", s.handleApply)
	mux.HandleFunc("GET /v1/results/{request_id}", s.handleResult)
	return mux
}

// SelfResponse is what the updater discovered about itself — the first thing to
// check on a host where an apply did not behave.
type SelfResponse struct {
	Version           string   `json:"version"`
	Project           string   `json:"project"`
	WorkingDir        string   `json:"working_dir"`
	ConfigFiles       []string `json:"config_files"`
	EnvPath           string   `json:"env_path"`
	AllowedNamespaces []string `json:"allowed_namespaces"`
	Components        []string `json:"components"`
	WaitTimeoutS      int      `json:"wait_timeout_s"`
	InFlight          *string  `json:"in_flight"`
	// The image reference compose resolves for each component right now, or
	// null when that component's service is not in this stack. Read by the
	// control plane to classify its own install mode: a bare local tag is a
	// source build, a `repo@sha256:…` is a registry install.
	Images map[string]*string `json:"images"`
}

func (s *Server) handleSelf(w http.ResponseWriter, r *http.Request) {
	resp := SelfResponse{
		Version:           s.Version,
		Project:           s.Cfg.Project,
		WorkingDir:        s.Cfg.WorkingDir,
		ConfigFiles:       s.Cfg.ConfigFiles,
		EnvPath:           s.EnvPath,
		AllowedNamespaces: s.Cfg.AllowedNamespaces,
		Components:        []string{"control-plane", "node-agent"},
		WaitTimeoutS:      s.Cfg.WaitTimeoutS,
		Images:            s.executor().EffectiveImages(r.Context()),
	}
	if id := s.Store.InFlight(); id != "" {
		resp.InFlight = &id
	}
	writeJSON(w, http.StatusOK, resp)
}

// Every rejection: one closed-vocabulary identifier (what a caller keys on)
// plus a message for a human.
type errorBody struct {
	RequestID string `json:"request_id,omitempty"`
	Reason    string `json:"reason"`
	Message   string `json:"message"`
}

// `busy` is 409 (a state that will change); request-shape rejections are 400;
// the two an operator fixes by CONFIGURATION rather than by editing the request
// are 422 — well-formed, and this host still will not do it.
func statusFor(reason string) int {
	switch reason {
	case ReasonBusy:
		return http.StatusConflict
	case ReasonNamespaceRejected, ReasonDigestMalformed:
		return http.StatusUnprocessableEntity
	default:
		return http.StatusBadRequest
	}
}

func (s *Server) handleApply(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxRequestBytes+1))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Reason: ReasonInvalid, Message: "could not read the request body"})
		return
	}
	if len(body) > MaxRequestBytes {
		writeJSON(w, http.StatusBadRequest, errorBody{Reason: ReasonInvalid, Message: "request body too large"})
		return
	}
	var req ApplyRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Reason: ReasonInvalid, Message: "malformed request: " + err.Error()})
		return
	}

	// A re-post of an accepted id is idempotent: same 202, no second apply.
	if a, ok := s.Store.AcceptedFor(req.RequestID); ok {
		writeJSON(w, http.StatusAccepted, a)
		return
	}

	env, err := os.ReadFile(s.EnvPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		writeJSON(w, http.StatusBadRequest, errorBody{RequestID: req.RequestID, Reason: ReasonInvalid,
			Message: fmt.Sprintf("cannot read %s: %v", s.EnvPath, err)})
		return
	}
	priorEnv := string(env)

	cfg := s.Cfg
	cfg.InFlightRequestID = s.Store.InFlight()
	plan, rej := Plan(req, priorEnv, cfg)
	if rej != nil {
		log.Printf("apply %s REJECTED: %s", req.RequestID, rej.Error())
		writeJSON(w, statusFor(rej.Reason), errorBody{RequestID: req.RequestID, Reason: rej.Reason, Message: rej.Message})
		return
	}

	accepted := &Accepted{RequestID: req.RequestID, Previous: plan.Previous, Commands: plan.Commands}
	// Two simultaneous posts of different ids both pass Plan; exactly one
	// passes here.
	if err := s.Store.Claim(req.RequestID, accepted); err != nil {
		var rj *Rejection
		if errors.As(err, &rj) {
			writeJSON(w, statusFor(rj.Reason), errorBody{RequestID: req.RequestID, Reason: rj.Reason, Message: rj.Message})
			return
		}
		writeJSON(w, http.StatusConflict, errorBody{RequestID: req.RequestID, Reason: ReasonBusy, Message: err.Error()})
		return
	}

	// 202 now, execute detached: the requester is normally destroyed by the
	// work it asked for, so the context is the process's, never the request's.
	go s.executor().Apply(context.Background(), req, plan, priorEnv)

	log.Printf("apply %s ACCEPTED: services=%v", req.RequestID, plan.Services)
	writeJSON(w, http.StatusAccepted, accepted)
}

func (s *Server) handleResult(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("request_id")
	res, err := s.Store.Read(id)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeJSON(w, http.StatusNotFound, errorBody{RequestID: id, Reason: ReasonInvalid, Message: "no result for this request id"})
			return
		}
		writeJSON(w, http.StatusBadRequest, errorBody{RequestID: id, Reason: ReasonInvalid, Message: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// executor is built per use rather than held: it carries no state of its own
// (the latch and the results live in the Store), so one construction site is
// simpler than one lifetime.
func (s *Server) executor() *Executor {
	return &Executor{
		Store: s.Store, Docker: s.Docker, Cfg: s.Cfg, EnvPath: s.EnvPath,
		PullTimeout: s.PullTimeout, RecreateTimeout: s.RecreateTimeout,
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Listen creates the unix socket at mode 0666, with no group mapping: the
// control plane runs as uid 1000 and the agent as root, sharing no user-
// namespace mapping worth relying on, and the volume is mountable only by this
// host's stack. Tightening it locks out the control plane and buys nothing.
func Listen(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	// A socket left by a killed predecessor is not a server, and bind would
	// fail "address already in use" forever.
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o666); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}
