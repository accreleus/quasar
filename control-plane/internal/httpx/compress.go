package httpx

import (
	"bufio"
	"compress/gzip"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
)

// Compression is gzip-only and stdlib-only: zstd/brotli each need a third-party
// encoder in the image, for ~26% vs gzip's ~31% on the SPA bundle. It runs
// in-process rather than at the edge because only the hardened compose overlay
// wires Caddy, so a plain deployment would otherwise have no compressing edge
// at all (#386).
const (
	// Below this a gzip header plus CPU buys nothing for a body inside one TCP
	// segment. index.html (1.2 KB) sits under it on purpose.
	compressMinBytes = 1400

	gzipLevel = gzip.DefaultCompression

	// Ceiling on what compressBufPool accepts back: buffers grow with the body,
	// and a pathological outlier must not stay pinned in the pool forever.
	compressBufPoolCap = 1 << 20 // 1 MiB
)

// gzipWriterPool recycles *gzip.Writer across requests (#417). flate's compressor
// state is ~800KB, and Compress() wraps the whole mux, so allocating fresh per
// response dominated the PROF-01 heap profile.
var gzipWriterPool = sync.Pool{
	New: func() any {
		gz, _ := gzip.NewWriterLevel(io.Discard, gzipLevel)
		return gz
	},
}

func getGzipWriter(w io.Writer) *gzip.Writer {
	gz := gzipWriterPool.Get().(*gzip.Writer)
	gz.Reset(w)
	return gz
}

// putGzipWriter returns gz to the pool, re-targeted at io.Discard first so the
// pool never pins the finished response's ResponseWriter/connection.
func putGzipWriter(gz *gzip.Writer) {
	gz.Reset(io.Discard)
	gzipWriterPool.Put(gz)
}

var compressBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 0, compressMinBytes)
		return &b
	},
}

func getCompressBuf() *[]byte {
	return compressBufPool.Get().(*[]byte)
}

func putCompressBuf(p *[]byte) {
	if p == nil || cap(*p) > compressBufPoolCap {
		return
	}
	*p = (*p)[:0]
	compressBufPool.Put(p)
}

// compressibleTypes are the media types worth encoding, matched on the prefix
// before any ";charset=". Keep it an allowlist: a denylist silently
// double-compresses the first new binary type someone adds, with no error.
var compressibleTypes = []string{
	"text/",
	"application/json",
	"application/javascript",
	"text/javascript",
	"application/manifest+json",
	"application/wasm",
	"image/svg+xml",
	"application/xml",
	"application/x-ndjson",
}

func compressible(contentType string) bool {
	if i := strings.IndexByte(contentType, ';'); i >= 0 {
		contentType = contentType[:i]
	}
	contentType = strings.ToLower(strings.TrimSpace(contentType))
	for _, p := range compressibleTypes {
		if strings.HasPrefix(contentType, p) {
			return true
		}
	}
	return false
}

func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		// Ignore any q-value; a client that lists gzip at all can decode it,
		// and "gzip;q=0" is vanishingly rare in practice.
		if name, _, _ := strings.Cut(part, ";"); strings.EqualFold(strings.TrimSpace(name), "gzip") {
			return true
		}
	}
	return false
}

// isBandwidthProbe reports whether this is the client's downstream throughput
// probe (web/src/webrtc/capability.ts, probeBandwidthKbps).
//
// It must never be compressed: the probe discards samples under 50 KB, and gzip
// cuts the bundle sample from ~330 KB to ~102 KB, biasing the estimate low on a
// fast link and floor-tiering every user (#146). The `probe` query parameter is
// a contract with that client file — renaming it there means changing it here.
func isBandwidthProbe(r *http.Request) bool {
	return r.URL.Query().Has("probe")
}

// Compress wraps next so compressible responses are gzip-encoded for clients
// that advertise support.
//
// Vary: Accept-Encoding must be set on every response, compressed or not, or a
// shared cache serves a gzipped body to a client that never asked for one.
//
// WebSocket upgrades are handed to next unwrapped: gorilla/websocket
// type-asserts to http.Hijacker, and the agent WS and signaling relay ride on
// it. The wrapper forwards Hijack anyway, disabling compression if it fires.
func Compress(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Accept-Encoding")

		if isWebSocketUpgrade(r) || !acceptsGzip(r) || isBandwidthProbe(r) {
			next.ServeHTTP(w, r)
			return
		}

		cw := &compressWriter{ResponseWriter: w, status: http.StatusOK, bufPtr: getCompressBuf()}
		defer cw.finish()
		next.ServeHTTP(cw, r)
	})
}

// Buffers the response head so the gzip decision is made once, from the final
// headers and the real body size.
type compressWriter struct {
	http.ResponseWriter

	status      int
	wroteHeader bool // the handler called WriteHeader

	decided bool
	gz      *gzip.Writer // non-nil once we committed to compressing
	// bufPtr is pooled; buf is the working slice backed by *bufPtr's array, which
	// append may grow. Return the latest *bufPtr to the pool, never the one held
	// at construction. Both nil once decide() drained it or Hijack() ran.
	bufPtr *[]byte
	buf    []byte // plain bytes held while undecided
}

func (c *compressWriter) WriteHeader(status int) {
	if c.wroteHeader {
		return
	}
	c.status = status
	c.wroteHeader = true
	// Not forwarded yet: Content-Encoding and Content-Length must settle before
	// the status line goes out, and that needs the body size. decide() forwards.
}

func (c *compressWriter) Write(p []byte) (int, error) {
	if c.decided {
		if c.gz != nil {
			return c.gz.Write(p)
		}
		return c.ResponseWriter.Write(p)
	}

	if c.buf == nil && c.bufPtr != nil {
		c.buf = *c.bufPtr
	}
	c.buf = append(c.buf, p...)
	if c.bufPtr != nil {
		*c.bufPtr = c.buf
	}
	if len(c.buf) >= compressMinBytes {
		if err := c.decide(); err != nil {
			return 0, err
		}
	}
	// Everything was accepted, buffer or encoder.
	return len(p), nil
}

// Commits to gzip or passthrough, emits the status line, drains the buffer.
func (c *compressWriter) decide() error {
	if c.decided {
		return nil
	}
	c.decided = true

	h := c.Header()
	// Sniff from the plain bytes: after gzipping, net/http's sniff sees gzip magic
	// and labels it application/octet-stream, so the browser downloads the module
	// script instead of executing it.
	ct := h.Get("Content-Type")
	if ct == "" && len(c.buf) > 0 {
		ct = http.DetectContentType(c.buf)
		h.Set("Content-Type", ct)
	}

	if c.shouldCompress(ct, h) {
		// A Content-Length describing the PLAIN body truncates the response in
		// the browser. Drop it and let net/http chunk.
		h.Del("Content-Length")
		h.Set("Content-Encoding", "gzip")
		// Byte ranges are computed over the identity encoding; advertising
		// range support alongside a re-encoded body is a correctness trap.
		h.Del("Accept-Ranges")
		c.gz = getGzipWriter(c.ResponseWriter)
	}

	c.ResponseWriter.WriteHeader(c.status)

	if len(c.buf) == 0 {
		c.releaseBuf()
		return nil
	}
	var err error
	if c.gz != nil {
		_, err = c.gz.Write(c.buf)
	} else {
		_, err = c.ResponseWriter.Write(c.buf)
	}
	c.releaseBuf()
	return err
}

// releaseBuf returns the pooled buffer and clears buf and bufPtr, so a Write
// arriving after decide() never mistakes a stale bufPtr for live state.
func (c *compressWriter) releaseBuf() {
	putCompressBuf(c.bufPtr)
	c.bufPtr = nil
	c.buf = nil
}

func (c *compressWriter) shouldCompress(contentType string, h http.Header) bool {
	// A handler that encoded its own body owns the encoding.
	if h.Get("Content-Encoding") != "" {
		return false
	}
	// 1xx/204/304 carry no body; 206 is a byte range over the identity bytes.
	switch {
	case c.status < http.StatusOK,
		c.status == http.StatusNoContent,
		c.status == http.StatusNotModified,
		c.status == http.StatusPartialContent:
		return false
	}
	if len(c.buf) < compressMinBytes {
		return false
	}
	return compressible(contentType)
}

// Safe to call twice.
func (c *compressWriter) finish() {
	if !c.decided {
		// Short or empty body: decide() picks passthrough and emits the status.
		_ = c.decide()
	}
	if c.gz != nil {
		_ = c.gz.Close()
		putGzipWriter(c.gz)
		c.gz = nil
	}
}

// Undecided output must settle first, or a flush before the size threshold
// strands the body.
func (c *compressWriter) Flush() {
	if !c.decided {
		_ = c.decide()
	}
	if c.gz != nil {
		_ = c.gz.Flush()
	}
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack forwards to the underlying ResponseWriter, abandoning compression.
// Compress() already skips WebSocket upgrades, so this catches other hijackers.
func (c *compressWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := c.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("httpx: underlying ResponseWriter is not an http.Hijacker")
	}
	// Drop buffered bytes so the deferred finish() cannot write a status line
	// onto a connection the caller now owns. Both pooled objects must go back,
	// not just be nil-ed, or the pools starve.
	c.decided = true
	c.releaseBuf()
	if c.gz != nil {
		putGzipWriter(c.gz)
		c.gz = nil
	}
	return hj.Hijack()
}

func (c *compressWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }
