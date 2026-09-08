package updater

import (
	"context"
	"crypto/ed25519"
	"net/http"
	"testing"
)

// End to end over the real socket: the refusal an operator eventually reads in
// Fleet ▸ Releases starts as this response body, so the identifier and the
// status code are what the test pins.

// stubSource is the fetched evidence, made deterministic.
type stubSource struct {
	ev     SignatureEvidence
	asked  []string
	called int
}

func (s *stubSource) Evidence(_ context.Context, version *string) SignatureEvidence {
	s.called++
	s.asked = append(s.asked, derefString(version))
	return s.ev
}

func signingEnv(t *testing.T) (*fakeEnv, []byte, ed25519.PrivateKey, TrustedKey) {
	t.Helper()
	f := newFakeEnv(t, "QUASAR_AGENT_IMAGE=ghcr.io/accreleus/quasar/quasar-node-agent@"+prevDigest+"\n")
	f.canned("ps.out", psJSON("quasar-node-agent", "na1", "running", ""))
	key, priv := genKey(t, "release-2026")
	return f, testManifest(t, "0.3.0"), priv, key
}

func withPolicy(pol SignaturePolicy, src *stubSource) func(*Server) {
	return func(s *Server) {
		s.Cfg.Signature = pol
		s.Signatures = src
		s.ManifestBaseURL = "https://example.invalid/v{version}/"
	}
}

func applyBody(version string) map[string]any {
	return map[string]any{
		"request_id": "11111111-2222-3333-4444-555555555555",
		"components": []map[string]string{
			{"name": "node-agent", "image": testAgentImage, "digest": testAgentDigest},
		},
		"release": map[string]any{"id": "rel", "version": version, "source_commit": "c"},
	}
}

func TestServerRefusesATamperedManifest(t *testing.T) {
	f, manifest, priv, key := signingEnv(t)
	doc := signDoc(t, manifest, map[string]ed25519.PrivateKey{"release-2026": priv})
	// The manifest the source returns is not the one that was signed.
	tampered := append([]byte(nil), manifest...)
	tampered[len(tampered)-2] = ' '

	src := &stubSource{ev: SignatureEvidence{Manifest: tampered, Signature: doc}}
	c := serveOnSocket(t, f, withPolicy(
		SignaturePolicy{Mode: SignatureModeVerify, Keys: []TrustedKey{key}}, src))

	status, body := post(t, c, "/v1/apply", applyBody("0.3.0"))
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", status)
	}
	if body["reason"] != ReasonSignatureInvalid {
		t.Fatalf("reason = %v, want %s", body["reason"], ReasonSignatureInvalid)
	}
	if msg, _ := body["message"].(string); msg == "" {
		t.Fatal("the refusal must carry a message for the operator")
	}
	// Refused before anything was pulled or recreated.
	if argv := f.argv(); argv != "" {
		t.Fatalf("a refused apply must run no docker commands, ran: %s", argv)
	}
}

func TestServerRefusesAMissingSignatureUnderRequire(t *testing.T) {
	f, _, _, key := signingEnv(t)
	src := &stubSource{ev: SignatureEvidence{Absent: true, Why: "release 0.3.0 publishes no signature asset"}}
	c := serveOnSocket(t, f, withPolicy(
		SignaturePolicy{Mode: SignatureModeRequire, Keys: []TrustedKey{key}}, src))

	status, body := post(t, c, "/v1/apply", applyBody("0.3.0"))
	if status != http.StatusUnprocessableEntity || body["reason"] != ReasonSignatureMissing {
		t.Fatalf("status %d reason %v, want 422 %s", status, body["reason"], ReasonSignatureMissing)
	}
	if src.called != 1 || src.asked[0] != "0.3.0" {
		t.Fatalf("the source must be asked once, for the request's version: %+v", src.asked)
	}
}

func TestServerAcceptsAMissingSignatureUnderVerify(t *testing.T) {
	f, _, _, key := signingEnv(t)
	src := &stubSource{ev: SignatureEvidence{Absent: true, Why: "no signature asset"}}
	c := serveOnSocket(t, f, withPolicy(
		SignaturePolicy{Mode: SignatureModeVerify, Keys: []TrustedKey{key}}, src))

	if status, body := post(t, c, "/v1/apply", applyBody("0.3.0")); status != http.StatusAccepted {
		t.Fatalf("status %d body %v, want 202: verify must not break an unsigned release", status, body)
	}
}

func TestServerAcceptsAGoodSignature(t *testing.T) {
	f, manifest, priv, key := signingEnv(t)
	src := &stubSource{ev: SignatureEvidence{
		Manifest:  manifest,
		Signature: signDoc(t, manifest, map[string]ed25519.PrivateKey{"release-2026": priv}),
	}}
	c := serveOnSocket(t, f, withPolicy(
		SignaturePolicy{Mode: SignatureModeRequire, Keys: []TrustedKey{key}}, src))

	if status, body := post(t, c, "/v1/apply", applyBody("0.3.0")); status != http.StatusAccepted {
		t.Fatalf("status %d body %v, want 202", status, body)
	}
}

func TestServerFetchesNothingWhenSigningIsOff(t *testing.T) {
	f, _, _, _ := signingEnv(t)
	src := &stubSource{ev: SignatureEvidence{FetchError: "this must never be consulted"}}
	c := serveOnSocket(t, f, withPolicy(SignaturePolicy{Mode: SignatureModeOff}, src))

	if status, _ := post(t, c, "/v1/apply", applyBody("0.3.0")); status != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", status)
	}
	if src.called != 0 {
		t.Fatalf("mode off must fetch nothing, fetched %d times", src.called)
	}
}

func TestSelfReportsTheSignaturePolicy(t *testing.T) {
	f, _, _, key := signingEnv(t)
	c := serveOnSocket(t, f, withPolicy(
		SignaturePolicy{Mode: SignatureModeRequire, Keys: []TrustedKey{key}}, &stubSource{}))

	status, body := get(t, c, "/v1/self")
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if body["signature_mode"] != SignatureModeRequire {
		t.Fatalf("signature_mode = %v", body["signature_mode"])
	}
	ids, _ := body["trusted_key_ids"].([]any)
	if len(ids) != 1 || ids[0] != "release-2026" {
		t.Fatalf("trusted_key_ids = %v", body["trusted_key_ids"])
	}
	// The LABELS, never the key material: /v1/self is read by anything on this
	// host that can reach the socket.
	if body["manifest_source"] != "https://example.invalid/v{version}/" {
		t.Fatalf("manifest_source = %v", body["manifest_source"])
	}
}

func TestSelfSaysOffWhenNothingIsConfigured(t *testing.T) {
	f, _, _, _ := signingEnv(t)
	c := serveOnSocket(t, f)
	_, body := get(t, c, "/v1/self")
	if body["signature_mode"] != SignatureModeOff {
		t.Fatalf("an unconfigured updater must report %q, got %v", SignatureModeOff, body["signature_mode"])
	}
}
