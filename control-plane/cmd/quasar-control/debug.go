package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"runtime"
	rtdebug "runtime/debug"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// newDebugServer builds the debug/profiling listener (PROF-01, #388): its own
// *http.Server and mux, not a /debug/* route on the application router.
// Reasons: blast radius (127.0.0.1 bind is a network-level guarantee no later
// middleware mistake can widen), the route contract (TestOpenAPIDrift asserts
// the /v1 surface), and the admin gate stays untouched — pprof is operator
// surface, and the operator has shell.
//
// Threat model: a heap profile cannot leak a token (aggregated stacks and
// counts only); what does disclose is cmdline, goroutine?debug=2 and trace,
// and the availability risk is an unbounded ?seconds=. Loopback binding
// answers all four. pool may be nil; the pool-stats endpoint then 503s.
func newDebugServer(addr string, pool *pgxpool.Pool, log *slog.Logger) *http.Server {
	mux := http.NewServeMux()

	// pprof's init() registers onto http.DefaultServeMux, which we never serve;
	// register explicitly so the debug surface is exactly what this function says.
	mux.HandleFunc("/debug/pprof/", pprof.Index) // also serves /heap, /goroutine, /allocs, /block, /mutex, ...
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	// Mutex/block profiling are off at boot and not boot config: both carry a
	// steady-state cost, so an operator arms a window and disarms.
	mux.HandleFunc("POST /debug/quasar/mutexprofile", mutexProfileHandler(log))
	mux.HandleFunc("POST /debug/quasar/blockprofile", blockProfileHandler(log))

	mux.HandleFunc("GET /debug/quasar/pool", poolStatsHandler(pool))
	mux.HandleFunc("GET /debug/quasar/runtime", runtimeStatsHandler())

	return &http.Server{
		Addr:    addr,
		Handler: mux,
		// ReadHeaderTimeout, not ReadTimeout: bounded so a stuck client cannot
		// pin a connection, but the body is irrelevant here.
		ReadHeaderTimeout: 5 * time.Second,
		// WriteTimeout zero on purpose: the main servers' 10s would truncate
		// every profile?seconds=30 and trace capture. The unbounded-hold risk is
		// answered by the loopback bind, not a timeout.
		WriteTimeout: 0,
		IdleTimeout:  60 * time.Second,
	}
}

// mutexProfileHandler arms or disarms mutex profiling.
// POST /debug/quasar/mutexprofile?fraction=N — N=0 disables, N=1 samples every
// event, N>1 samples 1/N. Responds with the fraction that was in force before.
func mutexProfileHandler(log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		n, err := intParam(r, "fraction")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if n < 0 {
			// runtime treats a negative fraction as "read the current value
			// without changing it", which would make an operator typo look like
			// a successful arm. Reject it.
			http.Error(w, "fraction must be >= 0 (0 disables)", http.StatusBadRequest)
			return
		}
		prev := runtime.SetMutexProfileFraction(n)
		log.Warn("mutex profiling changed", "fraction", n, "previous", prev)
		writeJSON(w, map[string]int{"fraction": n, "previous": prev})
	}
}

// blockProfileHandler arms or disarms block profiling.
// POST /debug/quasar/blockprofile?rate=N — N=0 disables, N=1 tracks every
// blocking event, N>1 samples one blocking event per N nanoseconds blocked.
// The runtime exposes no getter, so no previous value is reported.
func blockProfileHandler(log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		n, err := intParam(r, "rate")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if n < 0 {
			http.Error(w, "rate must be >= 0 (0 disables)", http.StatusBadRequest)
			return
		}
		runtime.SetBlockProfileRate(n)
		log.Warn("block profiling changed", "rate", n)
		writeJSON(w, map[string]int{"rate": n})
	}
}

// poolStats mirrors pgxpool.Stat. Snake-cased because the soak harness parses it
// with jq alongside the /proc-derived series.
type poolStats struct {
	AcquireCount            int64  `json:"acquire_count"`
	AcquireDurationMS       int64  `json:"acquire_duration_ms"`
	AcquiredConns           int32  `json:"acquired_conns"`
	CanceledAcquireCount    int64  `json:"canceled_acquire_count"`
	ConstructingConns       int32  `json:"constructing_conns"`
	EmptyAcquireCount       int64  `json:"empty_acquire_count"`
	IdleConns               int32  `json:"idle_conns"`
	MaxConns                int32  `json:"max_conns"`
	MaxIdleDestroyCount     int64  `json:"max_idle_destroy_count"`
	MaxLifetimeDestroyCount int64  `json:"max_lifetime_destroy_count"`
	NewConnsCount           int64  `json:"new_conns_count"`
	TotalConns              int32  `json:"total_conns"`
	Note                    string `json:"note,omitempty"`
}

func poolStatsHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if pool == nil {
			http.Error(w, "no database pool wired", http.StatusServiceUnavailable)
			return
		}
		s := pool.Stat()
		writeJSON(w, poolStats{
			AcquireCount:            s.AcquireCount(),
			AcquireDurationMS:       s.AcquireDuration().Milliseconds(),
			AcquiredConns:           s.AcquiredConns(),
			CanceledAcquireCount:    s.CanceledAcquireCount(),
			ConstructingConns:       s.ConstructingConns(),
			EmptyAcquireCount:       s.EmptyAcquireCount(),
			IdleConns:               s.IdleConns(),
			MaxConns:                s.MaxConns(),
			MaxIdleDestroyCount:     s.MaxIdleDestroyCount(),
			MaxLifetimeDestroyCount: s.MaxLifetimeDestroyCount(),
			NewConnsCount:           s.NewConnsCount(),
			TotalConns:              s.TotalConns(),
			// acquired_conns pinned at max_conns with a climbing
			// empty_acquire_count is the pool-exhaustion signature.
			Note: "acquired_conns == max_conns with a rising empty_acquire_count means pool exhaustion",
		})
	}
}

// runtimeStats is the per-cycle sample the soak harness (PROF-03) takes without
// downloading and parsing a profile.
type runtimeStats struct {
	Goroutines   int    `json:"goroutines"`
	GOMAXPROCS   int    `json:"gomaxprocs"`
	NumCPU       int    `json:"num_cpu"`
	MemLimit     int64  `json:"gomemlimit_bytes"`
	HeapAlloc    uint64 `json:"heap_alloc_bytes"`
	HeapInuse    uint64 `json:"heap_inuse_bytes"`
	HeapObjects  uint64 `json:"heap_objects"`
	StackInuse   uint64 `json:"stack_inuse_bytes"`
	Sys          uint64 `json:"sys_bytes"`
	NumGC        uint32 `json:"num_gc"`
	MutexProfile bool   `json:"mutex_profiling_armed"`
}

func runtimeStatsHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		// SetMutexProfileFraction(-1) reads the current value without changing
		// it — the one documented use of a negative argument.
		armed := runtime.SetMutexProfileFraction(-1) > 0
		writeJSON(w, runtimeStats{
			Goroutines: runtime.NumGoroutine(),
			GOMAXPROCS: runtime.GOMAXPROCS(0),
			NumCPU:     runtime.NumCPU(),
			// SetMemoryLimit(-1) likewise reads GOMEMLIMIT without setting it.
			MemLimit:     rtdebug.SetMemoryLimit(-1),
			HeapAlloc:    m.HeapAlloc,
			HeapInuse:    m.HeapInuse,
			HeapObjects:  m.HeapObjects,
			StackInuse:   m.StackInuse,
			Sys:          m.Sys,
			NumGC:        m.NumGC,
			MutexProfile: armed,
		})
	}
}

func intParam(r *http.Request, name string) (int, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return 0, errMissingParam(name)
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, errBadParam(name, raw)
	}
	return n, nil
}

type paramError string

func (e paramError) Error() string { return string(e) }

func errMissingParam(name string) error { return paramError(name + " is required") }
func errBadParam(name, raw string) error {
	return paramError(name + " " + strconv.Quote(raw) + ": must be an integer")
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
