// Command udprelay is a user-space UDP impairment relay for the lab hosts, whose kernel has no netem/tbf/ifb.
// It relays UDP (QUIC/WebTransport) between local clients and one upstream and impairs each direction:
// down = upstream -> client (the media), up = client -> upstream. TLS is untouched: it only sees datagrams.
//
//	udprelay -listen 127.0.0.1:9443 -upstream <agent-ip>:<wt-port> -control 127.0.0.1:9199 > relay.jsonl
//	curl -d '{"rate_bps":5000000,"burst_bytes":15360,"latency_ms":10}' 'localhost:9199/shape?dir=down'
//	curl -X POST 'localhost:9199/clear'
//
// Getting a browser's packets here is redirect.sh. Shapes: docs/testing-e2e-suite.md.
package main

import (
	"container/heap"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"time"
)

type client struct {
	addr *net.UDPAddr
	up   *net.UDPConn // connected to the upstream; its source port identifies the client to the agent
}

type pkt struct {
	at  time.Time
	seq uint64
	b   []byte
	c   *client
}

type pktHeap []*pkt

func (h pktHeap) Len() int { return len(h) }
func (h pktHeap) Less(i, j int) bool {
	if h[i].at.Equal(h[j].at) {
		return h[i].seq < h[j].seq
	}
	return h[i].at.Before(h[j].at)
}
func (h pktHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *pktHeap) Push(x any)   { *h = append(*h, x.(*pkt)) }
func (h *pktHeap) Pop() any     { o := *h; x := o[len(o)-1]; *h = o[:len(o)-1]; return x }

// line is one direction: Offer decides at arrival, a goroutine releases at the decided time.
type line struct {
	name string
	sh   *Shaper
	send func(*pkt)
	mu   sync.Mutex
	h    pktHeap
	seq  uint64
	wake chan struct{}
}

func newLine(name string, seed int64, send func(*pkt)) *line {
	return &line{name: name, sh: NewShaper(seed), send: send, wake: make(chan struct{}, 1)}
}

func (l *line) put(c *client, b []byte) {
	now := time.Now()
	rel, why := l.sh.Offer(now, len(b))
	if why != "" {
		return
	}
	l.mu.Lock()
	l.seq++
	heap.Push(&l.h, &pkt{at: rel, seq: l.seq, b: b, c: c})
	l.mu.Unlock()
	select {
	case l.wake <- struct{}{}:
	default:
	}
}

func (l *line) run(ctx context.Context) {
	t := time.NewTimer(time.Hour)
	for {
		var due []*pkt
		l.mu.Lock()
		now := time.Now()
		for len(l.h) > 0 && !l.h[0].at.After(now) {
			due = append(due, heap.Pop(&l.h).(*pkt))
		}
		wait := time.Hour
		if len(l.h) > 0 {
			wait = l.h[0].at.Sub(now)
		}
		l.mu.Unlock()
		for _, p := range due {
			l.send(p)
			l.sh.NoteOut(len(p.b))
		}
		if len(due) > 0 {
			continue
		}
		t.Reset(wait)
		select {
		case <-ctx.Done():
			return
		case <-l.wake:
		case <-t.C:
		}
		if !t.Stop() {
			select {
			case <-t.C:
			default:
			}
		}
	}
}

type relay struct {
	lc       *net.UDPConn
	upstream *net.UDPAddr
	down, up *line
	mu       sync.Mutex
	clients  map[string]*client // shortcut: never reaped, one browser per relay run
	logMu    sync.Mutex
	out      *json.Encoder
}

func (r *relay) logEvent(v any) { r.logMu.Lock(); r.out.Encode(v); r.logMu.Unlock() }

func (r *relay) clientFor(a *net.UDPAddr) (*client, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c := r.clients[a.String()]; c != nil {
		return c, nil
	}
	uc, err := net.DialUDP("udp", nil, r.upstream)
	if err != nil {
		return nil, err
	}
	uc.SetReadBuffer(4 << 20)
	c := &client{addr: a, up: uc}
	r.clients[a.String()] = c
	go func() {
		buf := make([]byte, 65536)
		for {
			n, err := uc.Read(buf)
			if err != nil {
				return
			}
			r.down.put(c, append([]byte(nil), buf[:n]...))
		}
	}()
	return c, nil
}

func (r *relay) readClients() {
	buf := make([]byte, 65536)
	for {
		n, a, err := r.lc.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if c, err := r.clientFor(a); err == nil {
			r.up.put(c, append([]byte(nil), buf[:n]...))
		}
	}
}

type statLine struct {
	T           string            `json:"t"`
	Dir         string            `json:"dir"`
	In          Count             `json:"in"`
	Out         Count             `json:"out"`
	Drop        map[string]*Count `json:"drop"`
	QueueBytes  int64             `json:"queue_bytes"`
	QueueMs     float64           `json:"queue_ms"`
	InflightPkt int64             `json:"inflight_pkts"`
}

// stats logs cumulative counters every second; the analysis differences consecutive lines.
func (r *relay) stats(ctx context.Context, every time.Duration) {
	tk := time.NewTicker(every)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tk.C:
			for _, l := range []*line{r.down, r.up} {
				c, qb, qm := l.sh.Snapshot(now)
				var dropped int64
				for _, d := range c.Drop {
					dropped += d.Pkts
				}
				r.logEvent(statLine{now.UTC().Format(time.RFC3339Nano), l.name, c.In, c.Out, c.Drop, qb, qm, c.In.Pkts - c.Out.Pkts - dropped})
			}
		}
	}
}

func (r *relay) control(ctx context.Context, addr string) error {
	pick := func(req *http.Request) []*line {
		switch req.URL.Query().Get("dir") {
		case "up":
			return []*line{r.up}
		case "both":
			return []*line{r.down, r.up}
		}
		return []*line{r.down}
	}
	apply := func(w http.ResponseWriter, req *http.Request, sh Shape) {
		if err := sh.Validate(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		now := time.Now()
		for _, l := range pick(req) {
			l.sh.Apply(now, sh)
			r.logEvent(map[string]any{"t": now.UTC().Format(time.RFC3339Nano), "event": "shape", "dir": l.name, "shape": sh})
		}
		fmt.Fprintln(w, "ok")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /shape", func(w http.ResponseWriter, req *http.Request) {
		var sh Shape
		d := json.NewDecoder(http.MaxBytesReader(w, req.Body, 4096))
		d.DisallowUnknownFields()
		if err := d.Decode(&sh); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		apply(w, req, sh)
	})
	mux.HandleFunc("POST /clear", func(w http.ResponseWriter, req *http.Request) { apply(w, req, Shape{}) })
	mux.HandleFunc("GET /shape", func(w http.ResponseWriter, req *http.Request) {
		json.NewEncoder(w).Encode(map[string]Shape{"down": r.down.sh.Shape(), "up": r.up.sh.Shape()})
	})
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { <-ctx.Done(); srv.Close() }()
	return srv.ListenAndServe()
}

func main() {
	listen := flag.String("listen", "127.0.0.1:9443", "UDP address clients are redirected to (keep it on 127.0.0.1: see redirect.sh)")
	upstream := flag.String("upstream", "", "host:port of the real server (required)")
	ctl := flag.String("control", "127.0.0.1:9199", "HTTP control address (loopback only)")
	shapeDown := flag.String("shape-down", "", "initial down shape, JSON")
	shapeUp := flag.String("shape-up", "", "initial up shape, JSON")
	every := flag.Duration("stats", time.Second, "stats interval")
	seed := flag.Int64("seed", time.Now().UnixNano(), "random seed for loss and jitter")
	flag.Parse()
	if *upstream == "" {
		log.Fatal("-upstream is required")
	}
	ua, err := net.ResolveUDPAddr("udp", *upstream)
	if err != nil {
		log.Fatal(err)
	}
	la, err := net.ResolveUDPAddr("udp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	lc, err := net.ListenUDP("udp", la)
	if err != nil {
		log.Fatal(err)
	}
	lc.SetReadBuffer(4 << 20)
	r := &relay{lc: lc, upstream: ua, clients: map[string]*client{}, out: json.NewEncoder(os.Stdout)}
	r.down = newLine("down", *seed, func(p *pkt) { lc.WriteToUDP(p.b, p.c.addr) })
	r.up = newLine("up", *seed+1, func(p *pkt) { p.c.up.Write(p.b) })
	for dir, js := range map[*line]string{r.down: *shapeDown, r.up: *shapeUp} {
		var sh Shape
		if js != "" {
			if err := json.Unmarshal([]byte(js), &sh); err != nil || sh.Validate() != nil {
				log.Fatalf("bad initial shape %q: %v %v", js, err, sh.Validate())
			}
		}
		dir.sh.Apply(time.Now(), sh)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	go r.down.run(ctx)
	go r.up.run(ctx)
	go r.readClients()
	go r.stats(ctx, *every)
	log.Printf("relay %s -> %s, control http://%s", lc.LocalAddr(), ua, *ctl)
	if err := r.control(ctx, *ctl); err != nil && ctx.Err() == nil {
		log.Fatal(err)
	}
	lc.Close()
}
