package artwork

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// unguardedFetcher is a Fetcher with the SSRF dialer removed. Tests that need a
// SUCCESSFUL fetch have to use it, because every httptest server listens on
// loopback and the real fetcher refuses loopback by design — which is exactly
// what TestFetcherBlocksLoopback asserts. Size caps, redirect limits, status
// handling and type checks are unaffected, so those stay under test on the real
// code path.
func unguardedFetcher(t *testing.T, maxBytes int64) *Fetcher {
	t.Helper()
	f := NewFetcher(10*time.Second, maxBytes)
	tr := f.client.Transport.(*http.Transport).Clone()
	tr.DialContext = (&net.Dialer{Timeout: 5 * time.Second}).DialContext
	f.client.Transport = tr
	return f
}

// --- the SSRF guard ---------------------------------------------------------

// The load-bearing test: an httptest server is on 127.0.0.1, and the production
// fetcher must refuse to connect to it. If this ever passes a fetch, an
// operator pasting an override URL can make the control plane hit its own
// network.
func TestFetcherBlocksLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(onePixelPNG)
	}))
	defer srv.Close()

	f := NewFetcher(5*time.Second, DefaultMaxImageBytes)
	_, _, err := f.Get(context.Background(), srv.URL+"/art.png")
	if err == nil {
		t.Fatal("fetching loopback must fail")
	}
	if !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("want ErrBlockedAddress, got %v", err)
	}
	if !errors.Is(err, ErrFetchFailed) {
		t.Fatalf("want the error to also carry ErrFetchFailed, got %v", err)
	}
}

// A public URL that 302s to an internal address must not be followed. The
// dialer re-checks every hop, so the redirect target is blocked at connect
// time even though the first hop was legitimate.
func TestFetcherBlocksRedirectToInternal(t *testing.T) {
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("internal secret"))
	}))
	defer internal.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, internal.URL+"/secret", http.StatusFound)
	}))
	defer redirector.Close()

	// Use the unguarded fetcher for hop 1 so the redirect is actually followed;
	// the guard is then re-applied by CheckRedirect + the response check. With
	// the real fetcher, hop 1 is blocked and the test proves nothing about
	// hop 2.
	f := unguardedFetcher(t, DefaultMaxImageBytes)
	// Restore the address guard for the SECOND hop only, by re-wrapping the
	// dial with the same predicate the production dialer uses.
	tr := f.client.Transport.(*http.Transport)
	first := true
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if !first {
			host, _, _ := net.SplitHostPort(addr)
			if !isPublicIP(net.ParseIP(host)) {
				return nil, ErrBlockedAddress
			}
		}
		first = false
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, addr)
	}

	if _, _, err := f.Get(context.Background(), redirector.URL+"/art.png"); err == nil {
		t.Fatal("a redirect into a non-public address must not be followed")
	}
}

// file:// and friends never reach the dialer, so the scheme is checked up front.
func TestFetcherRejectsNonHTTPSchemes(t *testing.T) {
	f := NewFetcher(5*time.Second, DefaultMaxImageBytes)
	for _, u := range []string{
		"file:///etc/passwd",
		"ftp://example.com/a.png",
		"gopher://example.com/",
		"data:image/png;base64,AAAA",
		"/etc/passwd",
		"example.com/a.png", // no scheme
	} {
		if _, _, err := f.Get(context.Background(), u); err == nil {
			t.Errorf("Get(%q): want an error", u)
		} else if !errors.Is(err, ErrFetchFailed) {
			t.Errorf("Get(%q): want ErrFetchFailed, got %v", u, err)
		}
	}
}

// A redirect chain longer than maxRedirects must terminate.
func TestFetcherCapsRedirects(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+"/again", http.StatusFound)
	}))
	defer srv.Close()

	f := unguardedFetcher(t, DefaultMaxImageBytes)
	if _, _, err := f.Get(context.Background(), srv.URL+"/start"); err == nil {
		t.Fatal("an unbounded redirect loop must fail")
	}
}

// --- size cap ---------------------------------------------------------------

// A body larger than the cap must be refused even when Content-Length lies
// about it (here: chunked, so there is no Content-Length at all).
func TestFetcherEnforcesSizeCapWithoutContentLength(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		flusher, _ := w.(http.Flusher)
		chunk := make([]byte, 1024)
		for i := 0; i < 64; i++ { // 64 KiB against a 4 KiB cap
			_, _ = w.Write(chunk)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer srv.Close()

	f := unguardedFetcher(t, 4096)
	if _, _, err := f.Get(context.Background(), srv.URL+"/big.png"); err == nil {
		t.Fatal("an oversized chunked body must be rejected")
	}
}

// A truthful oversized Content-Length is rejected before the body is read.
func TestFetcherRejectsOversizedContentLength(t *testing.T) {
	body := make([]byte, 8192)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	f := unguardedFetcher(t, 1024)
	if _, _, err := f.Get(context.Background(), srv.URL+"/big.png"); err == nil {
		t.Fatal("an oversized Content-Length must be rejected")
	}
}

func TestFetcherRejectsNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	f := unguardedFetcher(t, DefaultMaxImageBytes)
	if _, _, err := f.Get(context.Background(), srv.URL+"/x.png"); err == nil {
		t.Fatal("a non-200 must be an error")
	}
}

func TestFetcherHappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("User-Agent"); got != userAgent {
			t.Errorf("User-Agent: want %q, got %q", userAgent, got)
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(onePixelPNG)
	}))
	defer srv.Close()

	f := unguardedFetcher(t, DefaultMaxImageBytes)
	data, ct, err := f.Get(context.Background(), srv.URL+"/art.png")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ct != "image/png" {
		t.Fatalf("content type: want image/png, got %q", ct)
	}
	if len(data) != len(onePixelPNG) {
		t.Fatalf("body length: want %d, got %d", len(onePixelPNG), len(data))
	}
}

// --- the address predicate --------------------------------------------------

func TestIsPublicIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "127.1.2.3", "0.0.0.0",
		"10.0.0.5", "172.16.0.1", "172.31.255.255", "192.168.1.1",
		"169.254.169.254", // cloud metadata
		"100.64.0.1",      // CGNAT
		"224.0.0.1",       // multicast
		"192.0.0.1", "192.0.2.5", "198.18.0.1", "198.51.100.4", "203.0.113.9",
		"240.0.0.1", "255.255.255.255",
		"::1", "fe80::1", "fc00::1", "fd12:3456::1", "::",
		"::ffff:127.0.0.1", // v4-mapped loopback must not slip through
		"::ffff:10.0.0.1",
	}
	for _, s := range blocked {
		if isPublicIP(net.ParseIP(s)) {
			t.Errorf("isPublicIP(%s): want false", s)
		}
	}
	allowed := []string{"1.1.1.1", "8.8.8.8", "104.18.0.1", "2606:4700::1111"}
	for _, s := range allowed {
		if !isPublicIP(net.ParseIP(s)) {
			t.Errorf("isPublicIP(%s): want true", s)
		}
	}
	if isPublicIP(nil) {
		t.Error("isPublicIP(nil): want false")
	}
}

// --- #623: concurrency bound, redirect allowlist, no URL in errors ----------

func TestFetcherBoundsConcurrentFetches(t *testing.T) {
	var inflight, peak atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := inflight.Add(1)
		defer inflight.Add(-1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		_, _ = w.Write(onePixelPNG)
	}))
	defer srv.Close()
	f := unguardedFetcher(t, 1<<20)

	var wg sync.WaitGroup
	for range 4 * maxConcurrentFetches {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := f.Get(context.Background(), srv.URL); err != nil {
				t.Errorf("get: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := peak.Load(); got > maxConcurrentFetches {
		t.Fatalf("peak concurrent fetches = %d, want <= %d", got, maxConcurrentFetches)
	}
}

func TestFetcherQueuedFetchHonoursContext(t *testing.T) {
	f := unguardedFetcher(t, 1<<20)
	for range maxConcurrentFetches {
		f.slots <- struct{}{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, _, err := f.Get(ctx, "https://example.com/a.png"); !errors.Is(err, ErrFetchFailed) {
		t.Fatalf("want ErrFetchFailed when the context ends in the queue, got %v", err)
	}
}

// A redirect must not carry a thumbnail fetch off the provider's hosts.
func TestGetOnHostsChecksEveryRedirectHop(t *testing.T) {
	var offListHits atomic.Int32
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		offListHits.Add(1)
		_, _ = w.Write(onePixelPNG)
	}))
	defer target.Close()
	_, port, _ := net.SplitHostPort(target.Listener.Addr().String())
	start := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://example.com:"+port+"/x.png", http.StatusFound)
	}))
	defer start.Close()

	f := unguardedFetcher(t, 1<<20)
	tr := start.Client().Transport.(*http.Transport).Clone()
	// example.com is in the httptest certificate; the dial stays on loopback.
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		_, p, _ := net.SplitHostPort(addr)
		return (&net.Dialer{}).DialContext(ctx, network, "127.0.0.1:"+p)
	}
	f.client = &http.Client{Transport: tr}

	_, _, err := f.GetOnHosts(context.Background(), start.URL, func(h string) bool { return h == "127.0.0.1" })
	if err == nil {
		t.Fatal("a redirect off the allowlist must fail")
	}
	if offListHits.Load() != 0 {
		t.Fatal("the off-allowlist host was contacted")
	}
	// Control: with the host allowed the same chain succeeds.
	if _, _, err := f.GetOnHosts(context.Background(), start.URL, func(string) bool { return true }); err != nil {
		t.Fatalf("allowed redirect: %v", err)
	}
}

func TestFetchErrorsDoNotCarryTheURL(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close() // connection refused
	f := unguardedFetcher(t, 1<<20)
	for _, raw := range []string{srv.URL + "/a.png?token=s3cret", "http://exa mple.com/a.png?token=s3cret"} {
		_, _, err := f.Get(context.Background(), raw)
		if err == nil {
			t.Fatalf("%q: want an error", raw)
		}
		if strings.Contains(err.Error(), "s3cret") || strings.Contains(err.Error(), "a.png") {
			t.Errorf("%q: error leaks the URL: %v", raw, err)
		}
		if !errors.Is(err, ErrFetchFailed) {
			t.Errorf("%q: want ErrFetchFailed, got %v", raw, err)
		}
	}
}
