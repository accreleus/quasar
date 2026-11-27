package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/actorsocket"
)

// Release detection's signature check (#824): the actor grades, detection obeys.

type fakeVerifier struct {
	verdict   actorsocket.Verdict
	message   string // "" is "the actor's words"
	err       error
	calls     int
	manifest  []byte
	signature []byte
}

func (f *fakeVerifier) VerifyManifest(_ context.Context, manifest, signature []byte) (actorsocket.ManifestVerdict, error) {
	f.calls++
	f.manifest, f.signature = manifest, signature
	msg := f.message
	if msg == "" {
		msg = "the actor's words"
	}
	return actorsocket.ManifestVerdict{Verdict: f.verdict, Message: msg}, f.err
}

const sigURL = "u/0.3.0.sig"

// signedListing is v0.3.0 with its .sig published.
func signedListing() (*fakeSource, Listing) {
	src := twoReleaseSource()
	src.manifests[sigURL] = `{"format_version":1,"signatures":[]}`
	l := src.listings[1]
	l.SignatureURL = sigURL
	return src, l
}

func TestDetectStoresAReleaseTheActorVerifiedAndSendsItTheStoredBytes(t *testing.T) {
	src, l := signedListing()
	v := &fakeVerifier{verdict: actorsocket.VerdictVerified}
	rel, err := NewDetector(src, nil, nil).WithVerifier(v).resolve(context.Background(), l)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !bytes.Equal(v.manifest, rel.Manifest) || string(v.signature) != src.manifests[sigURL] {
		t.Fatalf("the actor must grade the stored bytes and the published .sig; got %q / %q", v.manifest, v.signature)
	}
}

func TestDetectRefusesAReleaseWhoseSignatureTheActorRefuses(t *testing.T) {
	src, l := signedListing()
	_, err := NewDetector(src, nil, nil).WithVerifier(&fakeVerifier{verdict: actorsocket.VerdictRefused}).
		resolve(context.Background(), l)
	if err == nil || !strings.Contains(err.Error(), "the actor's words") {
		t.Fatalf("err = %v, want the actor's refusal", err)
	}
}

func TestDetectSendsNoSignatureForAReleaseThatPublishesNone(t *testing.T) {
	src := twoReleaseSource()
	l := src.listings[1]
	v := &fakeVerifier{verdict: actorsocket.VerdictRefused}
	if _, err := NewDetector(src, nil, nil).WithVerifier(v).resolve(context.Background(), l); err == nil {
		t.Fatal("a missing signature the actor refuses must not be listed")
	}
	if v.calls != 1 || v.signature != nil {
		t.Fatalf("calls=%d signature=%q, want one call with a null signature", v.calls, v.signature)
	}
	v.verdict = actorsocket.VerdictUnchecked
	if _, err := NewDetector(src, nil, nil).WithVerifier(v).resolve(context.Background(), l); err != nil {
		t.Fatalf("mode off lists an unsigned release as before: %v", err)
	}
}

func TestDetectUnderModeOffIgnoresASignatureItCouldNotFetch(t *testing.T) {
	src, l := signedListing()
	delete(src.manifests, sigURL)
	v := &fakeVerifier{verdict: actorsocket.VerdictUnchecked}
	if _, err := NewDetector(src, nil, nil).WithVerifier(v).resolve(context.Background(), l); err != nil {
		t.Fatalf("mode off: %v", err)
	}
	v.verdict = actorsocket.VerdictRefused
	_, err := NewDetector(src, nil, nil).WithVerifier(v).resolve(context.Background(), l)
	if err == nil || !strings.Contains(err.Error(), SignatureAssetNameV2+" could not be fetched") {
		t.Fatalf("err = %v, want the failed fetch named", err)
	}
}

func TestDetectTreatsAVerdictItDoesNotKnowAsRefused(t *testing.T) {
	src, l := signedListing()
	_, err := NewDetector(src, nil, nil).WithVerifier(&fakeVerifier{verdict: "maybe"}).resolve(context.Background(), l)
	if err == nil {
		t.Fatal("an unknown verdict must not list the release")
	}
}

func TestDetectWithNoActorChecksNothing(t *testing.T) {
	src, l := signedListing()
	delete(src.manifests, sigURL) // fetching it would fail the release
	if _, err := NewDetector(src, nil, nil).resolve(context.Background(), l); err != nil {
		t.Fatalf("no recovery actor: listing unchanged, got %v", err)
	}
}

// Over the real client and a real unix socket.

func TestDetectFailsClosedWhenTheActorDoesNotAnswer(t *testing.T) {
	src, l := signedListing()
	down := NewActorClient(filepath.Join(t.TempDir(), "control.sock"))
	_, err := NewDetector(src, nil, nil).WithVerifier(down).resolve(context.Background(), l)
	if err == nil || !strings.Contains(err.Error(), "did not check the signature") {
		t.Fatalf("err = %v, want a refusal", err)
	}
}

func TestDetectListsAsBeforeBehindAnActorThatPredatesTheCheck(t *testing.T) {
	src, l := signedListing()
	old := NewActorClient(serveActor(t, &fakeActor{})) // no /v1/verify-manifest: 404
	if _, err := old.VerifyManifest(context.Background(), []byte("m"), nil); !errors.Is(err, ErrNoManifestCheck) {
		t.Fatalf("err = %v, want ErrNoManifestCheck", err)
	}
	if _, err := NewDetector(src, nil, nil).WithVerifier(old).resolve(context.Background(), l); err != nil {
		t.Fatalf("an older actor must not hide the release that moves it: %v", err)
	}
}

func TestActorClientSendsTheSharedManifestCheckShape(t *testing.T) {
	var got actorsocket.ManifestCheck
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/verify-manifest", func(w http.ResponseWriter, r *http.Request) {
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&got); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		key := "release-2026"
		_ = json.NewEncoder(w).Encode(actorsocket.ManifestVerdict{Verdict: actorsocket.VerdictVerified, KeyID: &key})
	})
	sock := filepath.Join(t.TempDir(), "control.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	v, err := NewActorClient(sock).VerifyManifest(context.Background(), []byte("{\"m\":1}\n"), []byte("sig"))
	if err != nil || v.Verdict != actorsocket.VerdictVerified || v.KeyID == nil {
		t.Fatalf("verdict = %+v, err = %v", v, err)
	}
	if string(got.Manifest) != "{\"m\":1}\n" || string(got.Signature) != "sig" {
		t.Fatalf("the actor must receive the exact bytes, got %+v", got)
	}
}

func TestDetectTreatsAnOversizedSignatureAsOneItCouldNotFetch(t *testing.T) {
	src, l := signedListing()
	src.manifests[sigURL] = strings.Repeat("x", signatureMaxBytes+1)
	v := &fakeVerifier{verdict: actorsocket.VerdictUnchecked}
	if _, err := NewDetector(src, nil, nil).WithVerifier(v).resolve(context.Background(), l); err != nil {
		t.Fatalf("mode off lists it: %v", err)
	}
	if v.signature != nil {
		t.Fatalf("an over-cap .sig must not reach the actor, got %d bytes", len(v.signature))
	}
	v.verdict = actorsocket.VerdictRefused
	_, err := NewDetector(src, nil, nil).WithVerifier(v).resolve(context.Background(), l)
	if err == nil || !strings.Contains(err.Error(), "over the 8192-byte cap") {
		t.Fatalf("err = %v, want the cap named", err)
	}
}

// serveEarlyAnswer replays the Rust server's order for a request it answers
// unread (an older actor's 404 for an unknown route): read the head byte by
// byte, answer status with Connection: close, close with the body unread.
func serveEarlyAnswer(t *testing.T, status int) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "control.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			var head []byte
			b := make([]byte, 1)
			for !bytes.HasSuffix(head, []byte("\r\n\r\n")) {
				if n, err := c.Read(b); n == 0 || err != nil {
					break
				}
				head = append(head, b[0])
			}
			body := `{"error":"x"}`
			_, _ = fmt.Fprintf(c, "HTTP/1.1 %d %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
				status, http.StatusText(status), len(body), body)
			_ = c.Close()
		}
	}()
	return sock
}

func TestActorClientReadsAnOlderActors404BeforeItsUnreadBodyIsReset(t *testing.T) {
	client := NewActorClient(serveEarlyAnswer(t, http.StatusNotFound))
	for _, n := range []int{1 << 10, 64 << 10, 256 << 10, int(manifestMaxBytes)} {
		_, err := client.VerifyManifest(context.Background(), bytes.Repeat([]byte("m"), n), bytes.Repeat([]byte("s"), signatureMaxBytes))
		if !errors.Is(err, ErrNoManifestCheck) {
			t.Errorf("%d-byte manifest: err = %v, want ErrNoManifestCheck", n, err)
		}
	}
}

func TestActorClientFailsClosedWhenAnEarlyAnswerIsNotA404(t *testing.T) {
	client := NewActorClient(serveEarlyAnswer(t, http.StatusRequestEntityTooLarge))
	_, err := client.VerifyManifest(context.Background(), bytes.Repeat([]byte("m"), int(manifestMaxBytes)), nil)
	if err == nil || errors.Is(err, ErrNoManifestCheck) {
		t.Fatalf("err = %v, want a refusal", err)
	}
}

// A malformed .sig whose diagnostic quotes a long field name still fits the
// Jobs summary beside the notify and auto-apply keys.
func TestTheDetectionSummaryFitsWithLongSignatureDiagnostics(t *testing.T) {
	long := "signature refused by the recovery actor: the release manifest signature did not verify: " +
		"the signature document is not valid JSON in the documented shape: unknown field `" +
		strings.Repeat("\"\x01é", 3000) + "`"
	rep := Report{Seen: 7, ManifestInvalid: 7}
	for i := 0; i < 7; i++ {
		rep.Errors = append(rep.Errors, fmt.Sprintf("v0.6.%d: %s", i, long))
	}
	s := rep.Summary()
	if s["manifest_invalid"] != 7 {
		t.Fatalf("the count must survive: %v", s["manifest_invalid"])
	}
	for _, e := range s["manifest_errors"].([]string) {
		if !strings.HasPrefix(e, "v0.6.") || !strings.HasSuffix(e, "…") {
			t.Fatalf("entry %q must keep its tag and show the cut", e)
		}
	}
	notify := NotifyOutcome{Status: "failed", Reason: strings.Repeat("r", 200), ReleaseID: "00000000-0000-4000-8000-000000000000"}
	for k, v := range notify.Summary() {
		s[k] = v
	}
	s["auto_apply"], s["auto_apply_detail"] = "held_back", strings.Repeat("d", 200)
	if b, _ := json.Marshal(s); len(b) > 4096 {
		t.Fatalf("summary is %d bytes, over the jobs ceiling", len(b))
	}
}
