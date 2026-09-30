package httpx_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/httpx"
)

// #417 — a fresh gzip.Writer was allocated per compressible response
// (gzip.NewWriterLevel in decide()). flate's compressor state is ~800KB
// (hashHead 512KB + hashPrev 128KB + 64KB window), used once and dropped, on
// EVERY response over compressMinBytes — visible in the PROF-01 heap profile
// as compress/flate alloc_space dominance under browser polling. Before the
// pool this test measured ~800KB/request; the acceptance bound is 128KB.
func TestCompressPoolBoundsAllocationsPerRequest(t *testing.T) {
	if raceBuild {
		// The race detector's shadow-memory instrumentation inflates every
		// allocation; the byte budget below is only meaningful in a normal
		// build. gofix's go-check / go-test-db gates still exercise this file
		// under a plain (non -race) `go test`.
		t.Skip("allocation-bytes assertion is not meaningful under -race")
	}
	payload := strings.Repeat("export const a = 1;\n", 4000) // ~80KB, well over compressMinBytes
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		_, _ = w.Write([]byte(payload))
	})
	wrapped := httpx.Compress(h)

	req := func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/bundle.js", nil)
		r.Header.Set("Accept-Encoding", "gzip, deflate, br")
		return r
	}

	// Warm up: let the pool populate and any one-time setup costs (e.g. the
	// first pool.New allocation) happen outside the measured window.
	const warmup = 5
	for i := 0; i < warmup; i++ {
		rec := httptest.NewRecorder()
		wrapped.ServeHTTP(rec, req())
		_, _ = io.Copy(io.Discard, rec.Result().Body)
	}

	// GC is off for the measured window: a collection empties the sync.Pool, and
	// the next request then pays one fresh ~800KB compressor. That is GC timing,
	// not a per-request cost, and on Go 1.26 under a parallel `go test ./...` it
	// tipped the steady state over the bound. The regression this guards (a
	// writer per request) allocates on every request, GC or not, so it still
	// fails. 200 requests at the bound is ~26MB of uncollected heap.
	const iterations = 200
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for i := 0; i < iterations; i++ {
		rec := httptest.NewRecorder()
		wrapped.ServeHTTP(rec, req())
		res := rec.Result()
		if res.Header.Get("Content-Encoding") != "gzip" {
			t.Fatalf("iteration %d: expected a gzip response", i)
		}
		_, _ = io.Copy(io.Discard, res.Body)
	}
	runtime.ReadMemStats(&after)
	totalAlloc := after.TotalAlloc - before.TotalAlloc
	perRequest := totalAlloc / iterations
	const maxPerRequest = 128 * 1024 // 128KB, per #417's acceptance bound
	if perRequest > maxPerRequest {
		t.Fatalf("allocated %d bytes/request over %d iterations (total %d) — want <= %d",
			perRequest, iterations, totalAlloc, maxPerRequest)
	}
	t.Logf("allocated %d bytes/request over %d iterations", perRequest, iterations)
}
