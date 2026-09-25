package updater

// The shared release-trust golden vectors (#356), run against THIS package.
//
// The recovery actor's Rust port (`node-agent/crates/quasar-recovery`, module
// `trust`) runs the very same files. The vectors are the contract between the
// two: every case here is Go's observed behaviour, written down, and a port
// that disagrees with one of them is wrong. Nothing in this file changes what
// the updater does; it only drives the updater's own functions.
//
// Layout and rules: testdata/recovery/trust-vectors/README.md. The files are
// generated from the case table in trustvectors_cases_test.go
// (QUASAR_WRITE_TRUST_VECTORS=1) and TestTrustVectorsAreCurrent fails when a
// committed file differs from what the table generates today.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

// trustVectorDir is the vector directory seen from this package.
var trustVectorDir = filepath.Join("..", "..", "..", "testdata", "recovery", "trust-vectors")

// The six vector kinds. A file of any other kind fails the run: the coverage
// guard is that both runners know every kind and run every vector in it.
const (
	kindAdmit           = "admit"
	kindVerifySignature = "verify_signature"
	kindEvidence        = "evidence"
	kindEvidenceGate    = "evidence_gate"
	kindConfig          = "config"
	kindRedirect        = "redirect"
)

// Every URL a vector names lives under this origin. The Go runner serves the
// vector's responses from a local test server and maps the origin onto it;
// messages are mapped back before they are compared.
const vectorOrigin = "https://releases.example.invalid"

// The seed of a vector key is derived from its label, so no key material is
// ever committed (signature_test.go's rule). Anyone can recompute these keys:
// they are public by construction and must never be trusted by a real host.
const vectorKeySeedPrefix = "quasar release-trust golden vector test key; public by construction, never trust it: "

func vectorPrivateKey(label string) ed25519.PrivateKey {
	seed := sha256.Sum256([]byte(vectorKeySeedPrefix + label))
	return ed25519.NewKeyFromSeed(seed[:])
}

func vectorPublicKey(label string) ed25519.PublicKey {
	return vectorPrivateKey(label).Public().(ed25519.PublicKey)
}

// ── file shapes ──────────────────────────────────────────────────────────────

type vectorFile struct {
	Kind    string            `json:"kind"`
	About   string            `json:"about"`
	Vectors []json.RawMessage `json:"vectors"`
}

// bytesSpec is an exact byte string: `text` (UTF-8), `base64` (anything), or
// `segments` (each text repeated `count` times, for large or deep inputs).
type bytesSpec struct {
	Text     *string        `json:"text,omitempty"`
	Base64   string         `json:"base64,omitempty"`
	Segments []bytesSegment `json:"segments,omitempty"`
}

type bytesSegment struct {
	Text  string `json:"text"`
	Count int    `json:"count"`
}

func (b *bytesSpec) bytes(t *testing.T) []byte {
	t.Helper()
	switch {
	case b == nil:
		return nil
	case b.Text != nil:
		return []byte(*b.Text)
	case b.Base64 != "":
		raw, err := base64.StdEncoding.DecodeString(b.Base64)
		if err != nil {
			t.Fatalf("bad base64 bytes spec: %v", err)
		}
		return raw
	default:
		var buf bytes.Buffer
		for _, s := range b.Segments {
			buf.WriteString(strings.Repeat(s.Text, s.Count))
		}
		return buf.Bytes()
	}
}

func bytesOf(raw []byte) *bytesSpec {
	if utf8.Valid(raw) {
		s := string(raw)
		return &bytesSpec{Text: &s}
	}
	return &bytesSpec{Base64: base64.StdEncoding.EncodeToString(raw)}
}

func textOf(s string) *bytesSpec { return &bytesSpec{Text: &s} }

type vectorKey struct {
	ID          string `json:"id"`
	PublicKeyOf string `json:"public_key_of"`
}

func trustedKeysOf(keys []vectorKey) []TrustedKey {
	out := make([]TrustedKey, 0, len(keys))
	for _, k := range keys {
		out = append(out, TrustedKey{ID: k.ID, Key: vectorPublicKey(k.PublicKeyOf)})
	}
	return out
}

// evidenceSpec is SignatureEvidence as a vector input or expectation.
type evidenceSpec struct {
	State       string     `json:"state"` // signed | absent | fetch_error
	Manifest    *bytesSpec `json:"manifest,omitempty"`
	Signature   *bytesSpec `json:"signature,omitempty"`
	Why         string     `json:"why,omitempty"`
	Error       string     `json:"error,omitempty"`
	ErrorPrefix string     `json:"error_prefix,omitempty"` // expectations only
}

func (e *evidenceSpec) toGo(t *testing.T) SignatureEvidence {
	t.Helper()
	switch e.State {
	case "signed":
		return SignatureEvidence{Manifest: e.Manifest.bytes(t), Signature: e.Signature.bytes(t)}
	case "absent":
		return SignatureEvidence{Absent: true, Why: e.Why}
	case "fetch_error":
		return SignatureEvidence{FetchError: e.Error}
	}
	t.Fatalf("unknown evidence state %q", e.State)
	return SignatureEvidence{}
}

func evidenceFromGo(ev SignatureEvidence) evidenceSpec {
	switch {
	case ev.FetchError != "":
		return evidenceSpec{State: "fetch_error", Error: ev.FetchError}
	case ev.Absent:
		return evidenceSpec{State: "absent", Why: ev.Why}
	default:
		return evidenceSpec{State: "signed", Manifest: bytesOf(ev.Manifest), Signature: bytesOf(ev.Signature)}
	}
}

// fetchSpec is what the release host answers: by absolute URL, anything
// unlisted is a 404 (the Go source test's server did the same).
type fetchSpec struct {
	BaseURL   string                   `json:"base_url"`
	Responses map[string]assetResponse `json:"responses"`
}

type assetResponse struct {
	Status         int        `json:"status,omitempty"`
	Body           *bytesSpec `json:"body,omitempty"`
	TransportError bool       `json:"transport_error,omitempty"`
}

// ── kind: admit ──────────────────────────────────────────────────────────────

type admitConfig struct {
	AllowedNamespaces []string    `json:"allowed_namespaces"`
	InFlightRequestID string      `json:"in_flight_request_id"`
	SignatureMode     string      `json:"signature_mode"`
	TrustedKeys       []vectorKey `json:"trusted_keys"`
}

type admitDecision struct {
	Admitted      bool      `json:"admitted"`
	Reason        string    `json:"reason,omitempty"`
	Message       string    `json:"message,omitempty"`
	MessagePrefix string    `json:"message_prefix,omitempty"`
	Warnings      []string  `json:"warnings"`
	Fetched       *[]string `json:"fetched,omitempty"`
}

type admitVector struct {
	Name     string        `json:"name"`
	Source   string        `json:"source"`
	Caller   string        `json:"caller"` // control_plane | agent
	Config   admitConfig   `json:"config"`
	Request  ApplyRequest  `json:"request"`
	Evidence *evidenceSpec `json:"evidence,omitempty"`
	Fetch    *fetchSpec    `json:"fetch,omitempty"`
	Expect   admitDecision `json:"expect"`
	// Agent-caller vectors only: the Go updater has no notion of which socket
	// a request came in on, so Go is held to this, and the Rust port to both.
	ExpectWithoutCallerGuard *admitDecision `json:"expect_without_caller_guard,omitempty"`
}

// goAdmit is Plan, with the evidence gathered the way server.go gathers it.
func goAdmit(t *testing.T, v admitVector) admitDecision {
	t.Helper()
	cfg := testCfg() // the compose facts Plan needs; not part of the port
	cfg.AllowedNamespaces = v.Config.AllowedNamespaces
	cfg.InFlightRequestID = v.Config.InFlightRequestID
	var warned []string
	cfg.Signature = SignaturePolicy{
		Mode: v.Config.SignatureMode,
		Keys: trustedKeysOf(v.Config.TrustedKeys),
		Warn: func(m string) { warned = append(warned, m) },
	}
	var fetched *[]string
	var unmap func(string) string = func(s string) string { return s }
	switch {
	case v.Evidence != nil && v.Fetch != nil:
		t.Fatalf("%s: a vector carries evidence or a fetch, never both", v.Name)
	case v.Evidence != nil:
		ev := v.Evidence.toGo(t)
		cfg.SignatureEvidence = &ev
	case v.Fetch != nil:
		src, urls, um := vectorSource(t, *v.Fetch)
		srv := &Server{Signatures: src}
		cfg.SignatureEvidence = srv.signatureEvidence(context.Background(), cfg, v.Request)
		got := urls()
		fetched = &got
		unmap = um
	}
	_, rej := Plan(v.Request, "", cfg)
	out := admitDecision{Admitted: rej == nil, Warnings: []string{}, Fetched: fetched}
	if rej != nil {
		out.Reason, out.Message = rej.Reason, unmap(rej.Message)
	}
	for _, w := range warned {
		out.Warnings = append(out.Warnings, unmap(w))
	}
	return out
}

func matchAdmit(t *testing.T, name string, want, got admitDecision) {
	t.Helper()
	if want.MessagePrefix != "" {
		if !strings.HasPrefix(got.Message, want.MessagePrefix) || got.Message == want.MessagePrefix {
			t.Errorf("%s: message %q does not extend the pinned prefix %q", name, got.Message, want.MessagePrefix)
		}
		got.Message, want.MessagePrefix = "", ""
	}
	if !jsonEqual(want, got) {
		t.Errorf("%s:\n want %s\n  got %s", name, mustJSON(want), mustJSON(got))
	}
}

// ── kind: verify_signature ───────────────────────────────────────────────────

type verifyVector struct {
	Name        string         `json:"name"`
	Source      string         `json:"source"`
	Manifest    bytesSpec      `json:"manifest"`
	Document    bytesSpec      `json:"document"`
	TrustedKeys []vectorKey    `json:"trusted_keys"`
	Expect      verifyDecision `json:"expect"`
}

type verifyDecision struct {
	Verified    bool    `json:"verified"`
	KeyID       *string `json:"key_id,omitempty"`
	Error       string  `json:"error,omitempty"`
	ErrorPrefix string  `json:"error_prefix,omitempty"`
}

func goVerify(t *testing.T, v verifyVector) verifyDecision {
	t.Helper()
	id, err := VerifyManifestSignature(v.Manifest.bytes(t), v.Document.bytes(t), trustedKeysOf(v.TrustedKeys))
	if err != nil {
		return verifyDecision{Error: err.Error()}
	}
	return verifyDecision{Verified: true, KeyID: &id}
}

// ── kind: evidence ───────────────────────────────────────────────────────────

type evidenceVector struct {
	Name    string    `json:"name"`
	Source  string    `json:"source"`
	Version *string   `json:"version"`
	Fetch   fetchSpec `json:"fetch"`
	Expect  struct {
		Evidence evidenceSpec `json:"evidence"`
		Fetched  []string     `json:"fetched"`
	} `json:"expect"`
}

type evidenceOutcome struct {
	Evidence evidenceSpec `json:"evidence"`
	Fetched  []string     `json:"fetched"`
}

func goEvidence(t *testing.T, v evidenceVector) evidenceOutcome {
	t.Helper()
	src, urls, unmap := vectorSource(t, v.Fetch)
	ev := src.Evidence(context.Background(), v.Version)
	out := evidenceOutcome{Evidence: evidenceFromGo(ev), Fetched: urls()}
	out.Evidence.Error = unmap(out.Evidence.Error)
	out.Evidence.Why = unmap(out.Evidence.Why)
	return out
}

// vectorSource serves a fetchSpec. Keep-alives are off so a connection the
// handler drops (a transport error) is never retried on a fresh one.
func vectorSource(t *testing.T, f fetchSpec) (ReleaseAssetSource, func() []string, func(string) string) {
	t.Helper()
	if !strings.HasPrefix(f.BaseURL, vectorOrigin+"/") {
		t.Fatalf("base_url %q is not under %s", f.BaseURL, vectorOrigin)
	}
	var mu sync.Mutex
	fetched := []string{}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		url := vectorOrigin + r.URL.RequestURI()
		mu.Lock()
		fetched = append(fetched, url)
		mu.Unlock()
		resp, ok := f.Responses[url]
		switch {
		case !ok:
			http.NotFound(w, r)
		case resp.TransportError:
			hj, _ := w.(http.Hijacker)
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			conn.Close()
		case resp.Body != nil:
			if resp.Status != 0 && resp.Status != http.StatusOK {
				w.WriteHeader(resp.Status)
			}
			_, _ = w.Write(resp.Body.bytes(t))
		default:
			w.WriteHeader(resp.Status)
		}
	}))
	srv.Config.SetKeepAlivesEnabled(false)
	srv.Start()
	t.Cleanup(srv.Close)
	src := ReleaseAssetSource{BaseURL: srv.URL + strings.TrimPrefix(f.BaseURL, vectorOrigin)}
	urls := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string{}, fetched...)
	}
	unmap := func(s string) string { return strings.ReplaceAll(s, srv.URL, vectorOrigin) }
	return src, urls, unmap
}

// ── kind: evidence_gate ──────────────────────────────────────────────────────

type evidenceGateVector struct {
	Name              string `json:"name"`
	Source            string `json:"source"`
	SignatureMode     string `json:"signature_mode"`
	InFlightRequestID string `json:"in_flight_request_id"`
	RequestID         string `json:"request_id"`
	Expect            struct {
		Fetches bool `json:"fetches"`
	} `json:"expect"`
}

func goEvidenceGate(v evidenceGateVector) bool {
	src := &stubSource{ev: SignatureEvidence{Absent: true, Why: "stub"}}
	srv := &Server{Signatures: src}
	cfg := Config{InFlightRequestID: v.InFlightRequestID, Signature: SignaturePolicy{Mode: v.SignatureMode}}
	srv.signatureEvidence(context.Background(), cfg, ApplyRequest{RequestID: v.RequestID, Release: Release{Version: strptr("0.3.0")}})
	return src.called > 0
}

// ── kind: config ─────────────────────────────────────────────────────────────

type configVector struct {
	Name   string         `json:"name"`
	Source string         `json:"source"`
	Parse  string         `json:"parse"` // signature_mode | trusted_keys | allowed_namespaces | manifest_base_url
	Raw    string         `json:"raw"`
	Expect configDecision `json:"expect"`
}

type configDecision struct {
	OK          bool         `json:"ok"`
	Mode        string       `json:"mode,omitempty"`
	Keys        *[]vectorKey `json:"keys,omitempty"`
	Namespaces  []string     `json:"namespaces,omitempty"`
	BaseURL     string       `json:"base_url,omitempty"`
	Error       string       `json:"error,omitempty"`
	ErrorPrefix string       `json:"error_prefix,omitempty"`
}

// keyPlaceholder is how a trusted-key list names a vector key without
// committing it: `{public_key_of:LABEL}` becomes that key's standard base64.
const keyPlaceholderOpen = "{public_key_of:"

func expandKeyPlaceholders(raw string) (string, map[string]string) {
	labels := map[string]string{} // base64 → label
	var out strings.Builder
	for {
		i := strings.Index(raw, keyPlaceholderOpen)
		if i < 0 {
			out.WriteString(raw)
			return out.String(), labels
		}
		j := strings.Index(raw[i:], "}")
		label := raw[i+len(keyPlaceholderOpen) : i+j]
		b64 := base64.StdEncoding.EncodeToString(vectorPublicKey(label))
		labels[b64] = label
		out.WriteString(raw[:i])
		out.WriteString(b64)
		raw = raw[i+j+1:]
	}
}

func goConfig(t *testing.T, v configVector) configDecision {
	t.Helper()
	switch v.Parse {
	case "signature_mode":
		mode, err := ParseSignatureMode(v.Raw)
		if err != nil {
			return configDecision{Error: err.Error()}
		}
		return configDecision{OK: true, Mode: mode}
	case "trusted_keys":
		raw, labels := expandKeyPlaceholders(v.Raw)
		keys, err := ParseTrustedKeys(raw)
		if err != nil {
			return configDecision{Error: err.Error()}
		}
		got := []vectorKey{}
		for _, k := range keys {
			label, ok := labels[base64.StdEncoding.EncodeToString(k.Key)]
			if !ok {
				label = "(not a vector key)"
			}
			got = append(got, vectorKey{ID: k.ID, PublicKeyOf: label})
		}
		return configDecision{OK: true, Keys: &got}
	case "allowed_namespaces":
		return configDecision{OK: true, Namespaces: ParseNamespaces(v.Raw)}
	case "manifest_base_url":
		u, err := ParseManifestBaseURL(v.Raw)
		if err != nil {
			return configDecision{Error: err.Error()}
		}
		return configDecision{OK: true, BaseURL: u}
	}
	t.Fatalf("%s: unknown parse %q", v.Name, v.Parse)
	return configDecision{}
}

// ── kind: redirect ───────────────────────────────────────────────────────────

type redirectVector struct {
	Name   string   `json:"name"`
	Source string   `json:"source"`
	Via    []string `json:"via"`
	Next   string   `json:"next"`
	Expect struct {
		Allowed bool   `json:"allowed"`
		Error   string `json:"error,omitempty"`
	} `json:"expect"`
}

func goRedirect(t *testing.T, v redirectVector) (bool, string) {
	t.Helper()
	req := func(u string) *http.Request {
		r, err := http.NewRequest(http.MethodGet, u, nil)
		if err != nil {
			t.Fatalf("%s: %v", v.Name, err)
		}
		return r
	}
	via := make([]*http.Request, 0, len(v.Via))
	for _, u := range v.Via {
		via = append(via, req(u))
	}
	if err := (ReleaseAssetSource{}).client().CheckRedirect(req(v.Next), via); err != nil {
		return false, err.Error()
	}
	return true, ""
}

// ── the runner ───────────────────────────────────────────────────────────────

func readVectorFiles(t *testing.T) map[string]vectorFile {
	t.Helper()
	entries, err := os.ReadDir(trustVectorDir)
	if err != nil {
		t.Fatalf("read %s: %v", trustVectorDir, err)
	}
	files := map[string]vectorFile{}
	for _, e := range entries {
		if e.IsDir() || e.Name() == "README.md" {
			continue
		}
		if !strings.HasSuffix(e.Name(), ".json") {
			t.Fatalf("%s: only .json vector files and README.md belong in %s", e.Name(), trustVectorDir)
		}
		raw, err := os.ReadFile(filepath.Join(trustVectorDir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var f vectorFile
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&f); err != nil {
			t.Fatalf("%s: %v", e.Name(), err)
		}
		files[e.Name()] = f
	}
	return files
}

func strictDecode(t *testing.T, raw json.RawMessage, into any) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		t.Fatalf("vector %s: %v", raw, err)
	}
}

// TestTrustVectorsPassAgainstGo runs every vector in every file. The coverage
// guard is structural: an unknown kind fails, every vector of a known kind is
// run, and the count run must equal the count on disk.
func TestTrustVectorsPassAgainstGo(t *testing.T) {
	files := readVectorFiles(t)
	if len(files) == 0 {
		t.Fatal("no vector files: the guard must never pass vacuously")
	}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	total, ran := 0, 0
	seen := map[string]bool{}
	for _, file := range names {
		f := files[file]
		total += len(f.Vectors)
		for _, raw := range f.Vectors {
			var head struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal(raw, &head)
			key := f.Kind + "/" + head.Name
			if head.Name == "" || seen[key] {
				t.Fatalf("%s: vector names must be present and unique within a kind: %q", file, head.Name)
			}
			seen[key] = true
			switch f.Kind {
			case kindAdmit:
				var v admitVector
				strictDecode(t, raw, &v)
				got := goAdmit(t, v)
				switch v.Caller {
				case "control_plane":
					if v.ExpectWithoutCallerGuard != nil {
						t.Errorf("%s: expect_without_caller_guard is for agent-caller vectors only", v.Name)
					}
					matchAdmit(t, v.Name, v.Expect, got)
				case "agent":
					if v.ExpectWithoutCallerGuard == nil {
						t.Fatalf("%s: an agent-caller vector must say what the caller-less Go updater does", v.Name)
					}
					matchAdmit(t, v.Name, *v.ExpectWithoutCallerGuard, got)
				default:
					t.Fatalf("%s: unknown caller %q", v.Name, v.Caller)
				}
			case kindVerifySignature:
				var v verifyVector
				strictDecode(t, raw, &v)
				got := goVerify(t, v)
				want := v.Expect
				if want.ErrorPrefix != "" {
					if !strings.HasPrefix(got.Error, want.ErrorPrefix) {
						t.Errorf("%s: error %q does not start with %q", v.Name, got.Error, want.ErrorPrefix)
					}
					got.Error, want.ErrorPrefix = "", ""
				}
				if !jsonEqual(want, got) {
					t.Errorf("%s:\n want %s\n  got %s", v.Name, mustJSON(want), mustJSON(got))
				}
			case kindEvidence:
				var v evidenceVector
				strictDecode(t, raw, &v)
				got := goEvidence(t, v)
				want := evidenceOutcome{Evidence: v.Expect.Evidence, Fetched: v.Expect.Fetched}
				if p := want.Evidence.ErrorPrefix; p != "" {
					if !strings.HasPrefix(got.Evidence.Error, p) || got.Evidence.Error == p {
						t.Errorf("%s: fetch error %q does not extend %q", v.Name, got.Evidence.Error, p)
					}
					got.Evidence.Error, want.Evidence.ErrorPrefix = "", ""
				}
				if !evidenceEqual(t, want, got) {
					t.Errorf("%s:\n want %s\n  got %s", v.Name, mustJSON(want), mustJSON(got))
				}
			case kindEvidenceGate:
				var v evidenceGateVector
				strictDecode(t, raw, &v)
				if got := goEvidenceGate(v); got != v.Expect.Fetches {
					t.Errorf("%s: fetches = %v, want %v", v.Name, got, v.Expect.Fetches)
				}
			case kindConfig:
				var v configVector
				strictDecode(t, raw, &v)
				got := goConfig(t, v)
				want := v.Expect
				if want.ErrorPrefix != "" {
					if !strings.HasPrefix(got.Error, want.ErrorPrefix) {
						t.Errorf("%s: error %q does not start with %q", v.Name, got.Error, want.ErrorPrefix)
					}
					got.Error, want.ErrorPrefix = "", ""
				}
				if !jsonEqual(want, got) {
					t.Errorf("%s:\n want %s\n  got %s", v.Name, mustJSON(want), mustJSON(got))
				}
			case kindRedirect:
				var v redirectVector
				strictDecode(t, raw, &v)
				ok, msg := goRedirect(t, v)
				if ok != v.Expect.Allowed || msg != v.Expect.Error {
					t.Errorf("%s: allowed=%v error=%q, want allowed=%v error=%q", v.Name, ok, msg, v.Expect.Allowed, v.Expect.Error)
				}
			default:
				t.Fatalf("%s: unknown vector kind %q (both runners must know every kind)", file, f.Kind)
			}
			ran++
		}
	}
	if ran != total || ran == 0 {
		t.Fatalf("ran %d of %d vectors", ran, total)
	}
	t.Logf("ran %d trust vectors from %d files", ran, len(files))
}

// evidenceEqual compares evidence by bytes, so the text/base64/segments
// spelling of the same bytes never matters.
func evidenceEqual(t *testing.T, want, got evidenceOutcome) bool {
	t.Helper()
	if want.Evidence.State != got.Evidence.State || want.Evidence.Why != got.Evidence.Why ||
		want.Evidence.Error != got.Evidence.Error {
		return false
	}
	if !bytes.Equal(want.Evidence.Manifest.bytes(t), got.Evidence.Manifest.bytes(t)) ||
		!bytes.Equal(want.Evidence.Signature.bytes(t), got.Evidence.Signature.bytes(t)) {
		return false
	}
	return jsonEqual(want.Fetched, got.Fetched)
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("<%v>", err)
	}
	return string(b)
}

func jsonEqual(a, b any) bool { return mustJSON(a) == mustJSON(b) }
