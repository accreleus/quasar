package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// relayReadLimit is deliberately generous: capacity messages can carry large
// console_capabilities/codec_throughput blocks, and this fixture must never be
// the thing that truncates a frame the real control plane would have accepted.
const relayReadLimit = 8 << 20 // 8 MiB

// Relay forwards the real node agent's WebSocket to the control plane
// verbatim, except that it may rewrite an upstream capacity message's
// readiness array per its current Rule (docs/superpowers/plans/
// 2026-09-19-rh02-264-harness-matrix.md "relay" fixture).
type Relay struct {
	ListenAddr  string
	UpstreamURL string
	ControlAddr string

	mu    sync.Mutex
	rule  Rule
	stats Stats

	proxyOnce sync.Once
	proxy     http.Handler
}

// Stats is what GET /stats reports — enough for the harness to know a
// rewritten report actually reached the control plane after a rule change.
type Stats struct {
	AgentConnections  int        `json:"agent_connections"`
	CapacitySeen      int        `json:"capacity_seen"`
	CapacityRewritten int        `json:"capacity_rewritten"`
	LastCapacityAt    *time.Time `json:"last_capacity_at"`
}

func NewRelay(listen, upstream, control string) *Relay {
	return &Relay{ListenAddr: listen, UpstreamURL: upstream, ControlAddr: control, rule: defaultRule()}
}

func (r *Relay) currentRule() Rule {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rule
}

func (r *Relay) setRule(rule Rule) { r.mu.Lock(); r.rule = rule; r.mu.Unlock() }

func (r *Relay) recordCapacity(rewritten bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now().UTC()
	r.stats.CapacitySeen++
	if rewritten {
		r.stats.CapacityRewritten++
	}
	r.stats.LastCapacityAt = &now
}

func (r *Relay) incConnections(delta int) {
	r.mu.Lock()
	r.stats.AgentConnections += delta
	r.mu.Unlock()
}

func (r *Relay) getStats() Stats {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.stats
	if s.LastCapacityAt != nil {
		t := *s.LastCapacityAt
		s.LastCapacityAt = &t
	}
	return s
}

// httpPassThrough is a plain reverse proxy to the control plane behind
// UpstreamURL (ws -> http, wss -> https), path and query preserved.
func (r *Relay) httpPassThrough() http.Handler {
	r.proxyOnce.Do(func() {
		target, err := url.Parse(r.UpstreamURL)
		if err != nil {
			r.proxy = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "readiness-fixture: bad upstream URL", http.StatusBadGateway)
			})
			return
		}
		switch target.Scheme {
		case "wss":
			target.Scheme = "https"
		default:
			target.Scheme = "http"
		}
		target.Path, target.RawQuery = "", ""
		r.proxy = httputil.NewSingleHostReverseProxy(target)
	})
	return r.proxy
}

var agentUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin:     func(*http.Request) bool { return true },
}

// AgentHandler accepts the real agent's connection on --listen and pumps it
// against a freshly dialed upstream connection to the control plane.
func (r *Relay) AgentHandler(w http.ResponseWriter, req *http.Request) {
	// The agent derives an HTTP base from the same control-plane address for
	// its /v1/agent/* calls. Those are not the relay's business: they pass
	// through to the control plane untouched, as the WebSocket frames do.
	if !websocket.IsWebSocketUpgrade(req) {
		r.httpPassThrough().ServeHTTP(w, req)
		return
	}
	downConn, err := agentUpgrader.Upgrade(w, req, nil)
	if err != nil {
		log.Printf("readiness-fixture: agent upgrade failed: %v", err)
		return
	}
	defer downConn.Close()
	downConn.SetReadLimit(relayReadLimit)

	upConn, _, err := websocket.DefaultDialer.DialContext(req.Context(), r.UpstreamURL, nil)
	if err != nil {
		log.Printf("readiness-fixture: upstream dial failed: %v", err)
		downConn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "upstream unreachable"),
			time.Now().Add(time.Second))
		return
	}
	defer upConn.Close()
	upConn.SetReadLimit(relayReadLimit)

	r.incConnections(1)
	defer r.incConnections(-1)

	var wg sync.WaitGroup
	wg.Add(2)
	// Whichever leg ends first takes the other down with it: a pump blocked in a
	// read on a peer that never answers the close would otherwise hold the
	// upstream socket open, and the control plane would keep the host online.
	closeBoth := func() { downConn.Close(); upConn.Close() }
	go func() { defer wg.Done(); defer closeBoth(); r.pumpAgentToControl(downConn, upConn) }()
	go func() { defer wg.Done(); defer closeBoth(); pumpVerbatim(upConn, downConn) }()
	wg.Wait()
}

// pumpAgentToControl forwards every frame from the real agent (down) to the
// control plane (up), rewriting capacity.readiness per the current rule.
func (r *Relay) pumpAgentToControl(down, up *websocket.Conn) {
	for {
		msgType, data, err := down.ReadMessage()
		if err != nil {
			forwardClose(up, down, err)
			return
		}

		out := data
		if msgType == websocket.TextMessage {
			rule := r.currentRule()
			var probe map[string]json.RawMessage
			isCapacity := json.Unmarshal(data, &probe) == nil && isCapacityType(probe)
			rewritten := false
			if isCapacity {
				rewrittenFrame, did, err := rewriteCapacity(data, rule)
				if err == nil {
					out = rewrittenFrame
					rewritten = did
				} else {
					log.Printf("readiness-fixture: capacity rewrite skipped, forwarding verbatim: %v", err)
				}
				r.recordCapacity(rewritten)
			}
		}

		if err := up.WriteMessage(msgType, out); err != nil {
			forwardClose(down, up, err)
			return
		}
	}
}

func isCapacityType(top map[string]json.RawMessage) bool {
	raw, ok := top["type"]
	if !ok {
		return false
	}
	var typ string
	if json.Unmarshal(raw, &typ) != nil {
		return false
	}
	return typ == "capacity"
}

// pumpVerbatim forwards every frame from src to dst with no inspection —
// downstream (control -> agent) traffic is never mutated.
func pumpVerbatim(src, dst *websocket.Conn) {
	for {
		msgType, data, err := src.ReadMessage()
		if err != nil {
			forwardClose(dst, src, err)
			return
		}
		if err := dst.WriteMessage(msgType, data); err != nil {
			forwardClose(src, dst, err)
			return
		}
	}
}

// forwardClose propagates a close from whichever side saw it first to the
// other side, so an agent disconnect closes the upstream leg and vice versa.
func forwardClose(peer, source *websocket.Conn, err error) {
	code := websocket.CloseNormalClosure
	text := "closed"
	var ce *websocket.CloseError
	if errors.As(err, &ce) {
		code = ce.Code
		text = ce.Text
	}
	_ = peer.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, text),
		time.Now().Add(2*time.Second))
}

// ControlMux builds the loopback HTTP control surface: GET /healthz, GET/PUT
// /rule, GET /stats.
func (r *Relay) ControlMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /rule", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, r.currentRule())
	})
	mux.HandleFunc("PUT /rule", func(w http.ResponseWriter, req *http.Request) {
		var rule Rule
		if err := json.NewDecoder(req.Body).Decode(&rule); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if err := rule.Validate(); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		r.setRule(rule)
		writeJSON(w, http.StatusOK, rule)
	})
	mux.HandleFunc("GET /stats", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, r.getStats())
	})
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// RunRelay starts both listeners and blocks until ctx is cancelled.
func RunRelay(ctx context.Context, r *Relay) error {
	agentMux := http.NewServeMux()
	agentMux.HandleFunc("/", r.AgentHandler)
	agentSrv := &http.Server{Addr: r.ListenAddr, Handler: agentMux}
	controlSrv := &http.Server{Addr: r.ControlAddr, Handler: r.ControlMux()}

	errCh := make(chan error, 2)
	go func() { errCh <- agentSrv.ListenAndServe() }()
	go func() { errCh <- controlSrv.ListenAndServe() }()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		agentSrv.Shutdown(shutdownCtx)
		controlSrv.Shutdown(shutdownCtx)
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
