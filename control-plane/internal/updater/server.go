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

// The local HTTP surface. Four routes, over a unix socket in a named volume
// shared by exactly one host's containers; nothing outside the host can reach
// it and nothing outside the host is allowed to know its shape
// (protocol/schema.md §"Not frozen: the updater's local socket").
//
// AUTHORISATION IS THE REQUEST, NOT THE CALLER. SO_PEERCRED yields the caller's
// uid but pid 0 — the caller is in another pid namespace — so it is a coarse
// sanity check and never authentication (prototype finding 1). What actually
// constrains this program is the namespace allowlist and the digest rules in
// plan.go, which hold no matter who connects.

// MaxRequestBytes bounds the body. Generous against any real request; a body
// larger than this is not one.
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

	// Version identifies this build in `/v1/self`, for the same reason every
	// other component reports one.
	Version string
}

// Handler wires the routes. Separate from Listen so tests can serve it over a
// temp socket without a process.
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

// SelfResponse is what the updater discovered about itself, which is the first
// thing to check on a host where an apply did not behave (and the acceptance
// check for this ticket).
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
}

func (s *Server) handleSelf(w http.ResponseWriter, _ *http.Request) {
	resp := SelfResponse{
		Version:           s.Version,
		Project:           s.Cfg.Project,
		WorkingDir:        s.Cfg.WorkingDir,
		ConfigFiles:       s.Cfg.ConfigFiles,
		EnvPath:           s.EnvPath,
		AllowedNamespaces: s.Cfg.AllowedNamespaces,
		Components:        []string{"control-plane", "node-agent"},
		WaitTimeoutS:      s.Cfg.WaitTimeoutS,
	}
	if id := s.Store.InFlight(); id != "" {
		resp.InFlight = &id
	}
	writeJSON(w, http.StatusOK, resp)
}

// errorBody is every rejection's shape: one closed-vocabulary identifier plus a
// message for a human. The identifier is what a caller keys on.
type errorBody struct {
	RequestID string `json:"request_id,omitempty"`
	Reason    string `json:"reason"`
	Message   string `json:"message"`
}

// statusFor maps a reason to its HTTP status. `busy` is 409 (a state, will
// change), the request-shape rejections are 400, and the two rejections an
// operator fixes by CONFIGURATION rather than by editing the request are 422 —
// the request is well-formed and this host still will not do it.
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

	// A re-post of an id already accepted is idempotent: the same 202, no
	// second apply. Same rule as an `image_ensure` for an image already ready.
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
	// Claim is the authoritative latch; Plan's `busy` above is only the early,
	// friendly answer. Two simultaneous posts of different ids both pass Plan
	// and exactly one passes here.
	if err := s.Store.Claim(req.RequestID, accepted); err != nil {
		var rj *Rejection
		if errors.As(err, &rj) {
			writeJSON(w, statusFor(rj.Reason), errorBody{RequestID: req.RequestID, Reason: rj.Reason, Message: rj.Message})
			return
		}
		writeJSON(w, http.StatusConflict, errorBody{RequestID: req.RequestID, Reason: ReasonBusy, Message: err.Error()})
		return
	}

	// 202 NOW, execute DETACHED. The requester is normally destroyed by the
	// work it asked for (prototype finding 2), so the job must not be tied to
	// the connection — the context is the process's, never the request's.
	exec := &Executor{
		Store: s.Store, Docker: s.Docker, Cfg: s.Cfg, EnvPath: s.EnvPath,
		PullTimeout: s.PullTimeout, RecreateTimeout: s.RecreateTimeout,
	}
	go exec.Apply(context.Background(), req, plan, priorEnv)

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

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Listen creates the unix socket at path with mode 0666 in its (named-volume)
// directory.
//
// 0666 AND NO GROUP MAPPING is the decision from prototype finding 1: the
// control plane runs as uid 1000 and the agent as root, in containers that
// share no user namespace mapping worth relying on, and the socket is inside a
// volume only this host's stack can mount. Tightening the mode here would buy
// nothing and lock out the control plane.
func Listen(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	// A socket left behind by a killed predecessor is not a running server;
	// bind would fail with "address already in use" forever.
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
