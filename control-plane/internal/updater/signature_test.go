package updater

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// EVERY key in this file is generated at run time. Nothing key-shaped is
// checked in, not even a "test" one: a committed private key is a committed
// private key however it is labelled, and a committed public key would quietly
// become a trust anchor the first time someone pasted it into a config.

func genKey(t *testing.T, id string) (TrustedKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return TrustedKey{ID: id, Key: pub}, priv
}

// signDoc is the test twin of scripts/release/sign-platform-release-manifest.sh:
// a detached signature document over the manifest's exact bytes.
func signDoc(t *testing.T, manifest []byte, signers map[string]ed25519.PrivateKey) []byte {
	t.Helper()
	doc := SignatureDocument{FormatVersion: SignatureDocumentFormatVersion}
	for id, priv := range signers {
		doc.Signatures = append(doc.Signatures, SignatureEntry{
			Algorithm: SignatureAlgorithm,
			KeyID:     id,
			Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(priv, manifest)),
		})
	}
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal signature document: %v", err)
	}
	return body
}

const testControlImage = "ghcr.io/accreleus/quasar/quasar-control-plane"
const testAgentImage = "ghcr.io/accreleus/quasar/quasar-node-agent"

// 64 lowercase hex, because plan.go's digest rule runs before this gate does.
var testControlDigest = "sha256:aa11" + strings.Repeat("0", 60)
var testAgentDigest = "sha256:bb22" + strings.Repeat("0", 60)

func testManifest(t *testing.T, version string) []byte {
	t.Helper()
	m := map[string]any{
		"format_version": 1,
		"version":        version,
		"prerelease":     false,
		"source_commit":  strings.Repeat("c", 40),
		"built_at":       "2026-09-08T00:00:00Z",
		"schema_version": 80,
		"components": []map[string]string{
			{"name": "control-plane", "image": testControlImage, "digest": testControlDigest},
			{"name": "node-agent", "image": testAgentImage, "digest": testAgentDigest},
		},
	}
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	return body
}

func signedRequest(version string) ApplyRequest {
	v := version
	return ApplyRequest{
		RequestID: "11111111-2222-3333-4444-555555555555",
		Components: []Component{
			{Name: "node-agent", Image: testAgentImage, Digest: testAgentDigest},
		},
		Release: Release{ID: "rel", Version: &v, SourceCommit: strings.Repeat("c", 40)},
	}
}

func TestVerifyManifestSignatureAcceptsAGoodSignature(t *testing.T) {
	key, priv := genKey(t, "release-2026")
	manifest := testManifest(t, "0.3.0")
	doc := signDoc(t, manifest, map[string]ed25519.PrivateKey{"release-2026": priv})

	id, err := VerifyManifestSignature(manifest, doc, []TrustedKey{key})
	if err != nil {
		t.Fatalf("a genuine signature must verify: %v", err)
	}
	if id != "release-2026" {
		t.Fatalf("verified by key %q, want release-2026", id)
	}
}

func TestVerifyManifestSignatureRejectsATamperedManifest(t *testing.T) {
	key, priv := genKey(t, "release-2026")
	manifest := testManifest(t, "0.3.0")
	doc := signDoc(t, manifest, map[string]ed25519.PrivateKey{"release-2026": priv})

	// One byte of the digest, the whole point of the document.
	tampered := []byte(strings.Replace(string(manifest), testAgentDigest,
		"sha256:"+strings.Repeat("e", 64), 1))
	if string(tampered) == string(manifest) {
		t.Fatal("the tamper did not change the manifest")
	}

	if _, err := VerifyManifestSignature(tampered, doc, []TrustedKey{key}); err == nil {
		t.Fatal("a tampered manifest must not verify")
	}
}

func TestVerifyManifestSignatureRejectsTheWrongKey(t *testing.T) {
	_, priv := genKey(t, "attacker")
	trusted, _ := genKey(t, "release-2026")
	manifest := testManifest(t, "0.3.0")
	// Signed by a well-formed key nobody trusts, and LABELLED as the trusted
	// one: the label must buy the attacker nothing.
	doc := signDoc(t, manifest, map[string]ed25519.PrivateKey{"release-2026": priv})

	_, err := VerifyManifestSignature(manifest, doc, []TrustedKey{trusted})
	if err == nil {
		t.Fatal("a signature by an untrusted key must not verify")
	}
	if !strings.Contains(err.Error(), "trusts") {
		t.Fatalf("the message must say the key is not trusted, got %v", err)
	}
}

func TestVerifyManifestSignatureRotationOverlap(t *testing.T) {
	outgoing, outPriv := genKey(t, "release-2025")
	incoming, inPriv := genKey(t, "release-2026")
	manifest := testManifest(t, "0.3.0")
	both := signDoc(t, manifest, map[string]ed25519.PrivateKey{
		"release-2025": outPriv, "release-2026": inPriv,
	})

	// A host that has already rotated, a host that has not, and a host in the
	// overlap all accept the same release. That is the flag-day-free rotation.
	for name, keys := range map[string][]TrustedKey{
		"old only":  {outgoing},
		"new only":  {incoming},
		"both":      {outgoing, incoming},
		"new first": {incoming, outgoing},
	} {
		if _, err := VerifyManifestSignature(manifest, both, keys); err != nil {
			t.Errorf("%s: dual-signed release must verify: %v", name, err)
		}
	}

	// And a release signed only by the incoming key still fails on a host that
	// has not been given it — which is why the overlap exists.
	newOnly := signDoc(t, manifest, map[string]ed25519.PrivateKey{"release-2026": inPriv})
	if _, err := VerifyManifestSignature(manifest, newOnly, []TrustedKey{outgoing}); err == nil {
		t.Fatal("a host trusting only the outgoing key must reject a release signed only by the incoming one")
	}
}

func TestVerifyManifestSignatureRejectsMalformedDocuments(t *testing.T) {
	key, priv := genKey(t, "k")
	manifest := testManifest(t, "0.3.0")
	good := signDoc(t, manifest, map[string]ed25519.PrivateKey{"k": priv})

	cases := map[string]string{
		"not json":            "{",
		"unknown field":       `{"format_version":1,"signatures":[],"extra":1}`,
		"future format":       `{"format_version":2,"signatures":[]}`,
		"no signatures":       `{"format_version":1,"signatures":[]}`,
		"unknown algorithm":   `{"format_version":1,"signatures":[{"algorithm":"rsa","key_id":"k","signature":"AA=="}]}`,
		"truncated signature": `{"format_version":1,"signatures":[{"algorithm":"ed25519","key_id":"k","signature":"AA=="}]}`,
		"not base64":          `{"format_version":1,"signatures":[{"algorithm":"ed25519","key_id":"k","signature":"!!!"}]}`,
		"trailing content":    string(good) + `{"format_version":1}`,
	}
	for name, doc := range cases {
		if _, err := VerifyManifestSignature(manifest, []byte(doc), []TrustedKey{key}); err == nil {
			t.Errorf("%s: must not verify", name)
		}
	}
}

func TestVerifyManifestSignatureSkipsPastAnUnknownAlgorithm(t *testing.T) {
	key, priv := genKey(t, "k")
	manifest := testManifest(t, "0.3.0")
	doc := SignatureDocument{
		FormatVersion: SignatureDocumentFormatVersion,
		Signatures: []SignatureEntry{
			{Algorithm: "pq-whatever-2031", KeyID: "future", Signature: "AAAA"},
			{Algorithm: SignatureAlgorithm, KeyID: "k",
				Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(priv, manifest))},
		},
	}
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyManifestSignature(manifest, body, []TrustedKey{key}); err != nil {
		t.Fatalf("an entry in an unknown algorithm must be skipped, not fatal: %v", err)
	}
}

// ── the gate ─────────────────────────────────────────────────────────────────

func TestCheckSignatureOffIgnoresEverything(t *testing.T) {
	req := signedRequest("0.3.0")
	bad := &SignatureEvidence{Manifest: []byte(`{}`), Signature: []byte(`garbage`)}

	// Off, with a signature that could not possibly verify, and no keys: the
	// documented escape hatch has to actually be an escape hatch.
	for _, mode := range []string{"", SignatureModeOff} {
		if rej := checkSignature(req, SignaturePolicy{Mode: mode}, bad); rej != nil {
			t.Fatalf("mode %q must accept: %v", mode, rej)
		}
	}
}

func TestCheckSignatureFailsClosedWithNoTrustedKeys(t *testing.T) {
	req := signedRequest("0.3.0")
	for _, mode := range []string{SignatureModeVerify, SignatureModeRequire} {
		rej := checkSignature(req, SignaturePolicy{Mode: mode}, &SignatureEvidence{Absent: true, Why: "none"})
		if rej == nil {
			t.Fatalf("mode %q with no trusted keys must refuse", mode)
		}
		if rej.Reason != ReasonSignatureInvalid {
			t.Fatalf("mode %q: reason %q, want %q", mode, rej.Reason, ReasonSignatureInvalid)
		}
		if !strings.Contains(rej.Message, "QUASAR_UPDATER_TRUSTED_KEYS") {
			t.Fatalf("the refusal must name the variable to set, got %q", rej.Message)
		}
	}
}

func TestCheckSignatureAbsentSignature(t *testing.T) {
	key, _ := genKey(t, "k")
	req := signedRequest("0.3.0")
	ev := &SignatureEvidence{Absent: true, Why: "release 0.3.0 publishes no signature asset"}

	if rej := checkSignature(req, SignaturePolicy{Mode: SignatureModeVerify, Keys: []TrustedKey{key}}, ev); rej != nil {
		t.Fatalf("verify must accept a release that definitively has no signature: %v", rej)
	}
	rej := checkSignature(req, SignaturePolicy{Mode: SignatureModeRequire, Keys: []TrustedKey{key}}, ev)
	if rej == nil || rej.Reason != ReasonSignatureMissing {
		t.Fatalf("require must refuse with %s, got %v", ReasonSignatureMissing, rej)
	}
	if !strings.Contains(rej.Message, ev.Why) {
		t.Fatalf("the refusal must carry why, got %q", rej.Message)
	}
}

func TestCheckSignatureUndeterminedIsNeverReadAsUnsigned(t *testing.T) {
	key, _ := genKey(t, "k")
	req := signedRequest("0.3.0")
	ev := &SignatureEvidence{FetchError: "dial tcp: no route to host"}

	for _, mode := range []string{SignatureModeVerify, SignatureModeRequire} {
		rej := checkSignature(req, SignaturePolicy{Mode: mode, Keys: []TrustedKey{key}}, ev)
		if rej == nil || rej.Reason != ReasonSignatureInvalid {
			t.Fatalf("mode %q must refuse an undetermined signature, got %v", mode, rej)
		}
	}
}

func TestCheckSignatureGoodSignatureAccepted(t *testing.T) {
	key, priv := genKey(t, "k")
	manifest := testManifest(t, "0.3.0")
	ev := &SignatureEvidence{
		Manifest:  manifest,
		Signature: signDoc(t, manifest, map[string]ed25519.PrivateKey{"k": priv}),
	}
	for _, mode := range []string{SignatureModeVerify, SignatureModeRequire} {
		if rej := checkSignature(signedRequest("0.3.0"),
			SignaturePolicy{Mode: mode, Keys: []TrustedKey{key}}, ev); rej != nil {
			t.Fatalf("mode %q must accept a good signature: %v", mode, rej)
		}
	}
}

func TestCheckSignatureBindsTheManifestToTheRequest(t *testing.T) {
	key, priv := genKey(t, "k")
	manifest := testManifest(t, "0.3.0")
	ev := &SignatureEvidence{
		Manifest:  manifest,
		Signature: signDoc(t, manifest, map[string]ed25519.PrivateKey{"k": priv}),
	}
	pol := SignaturePolicy{Mode: SignatureModeVerify, Keys: []TrustedKey{key}}

	// A PERFECTLY VALID signature over a real manifest, used to launder a
	// digest that manifest does not name. This is the attack the binding
	// exists for: without it, one genuine release would authorise any bytes.
	swapped := signedRequest("0.3.0")
	swapped.Components[0].Digest = "sha256:" + strings.Repeat("f", 64)
	rej := checkSignature(swapped, pol, ev)
	if rej == nil || rej.Reason != ReasonSignatureInvalid {
		t.Fatalf("a digest the signed manifest does not name must be refused, got %v", rej)
	}
	if !strings.Contains(rej.Message, "node-agent") {
		t.Fatalf("the refusal must name the component, got %q", rej.Message)
	}

	// Same for the repository...
	elsewhere := signedRequest("0.3.0")
	elsewhere.Components[0].Image = "ghcr.io/accreleus/quasar/quasar-node-agent-evil"
	if rej := checkSignature(elsewhere, pol, ev); rej == nil {
		t.Fatal("an image the signed manifest does not name must be refused")
	}

	// ...for the version the manifest claims...
	other := signedRequest("9.9.9")
	if rej := checkSignature(other, pol, ev); rej == nil {
		t.Fatal("a manifest for another version must be refused")
	}

	// ...and for a component the manifest says nothing about at all.
	unknown := signedRequest("0.3.0")
	unknown.Components[0].Name = "control-plane"
	unknown.Components[0].Image = testControlImage
	unknown.Components[0].Digest = testAgentDigest // control-plane's digest is not this one
	if rej := checkSignature(unknown, pol, ev); rej == nil {
		t.Fatal("a component whose digest belongs to another component must be refused")
	}
}

func TestCheckSignatureToleratesAFutureManifestShape(t *testing.T) {
	key, priv := genKey(t, "k")
	// format_version 2 with an extra key: a manifest bump must not brick a
	// fleet's signature gate, as long as it still names its components.
	manifest := []byte(`{"format_version":2,"version":"0.3.0","something_new":{"a":1},` +
		`"components":[{"name":"node-agent","image":"` + testAgentImage + `","digest":"` + testAgentDigest + `"}]}`)
	ev := &SignatureEvidence{
		Manifest:  manifest,
		Signature: signDoc(t, manifest, map[string]ed25519.PrivateKey{"k": priv}),
	}
	if rej := checkSignature(signedRequest("0.3.0"),
		SignaturePolicy{Mode: SignatureModeRequire, Keys: []TrustedKey{key}}, ev); rej != nil {
		t.Fatalf("a forward-compatible manifest must still verify: %v", rej)
	}
}

// ── configuration ────────────────────────────────────────────────────────────

func TestParseSignatureMode(t *testing.T) {
	for raw, want := range map[string]string{
		"": SignatureModeOff, "off": SignatureModeOff, " OFF ": SignatureModeOff,
		"verify": SignatureModeVerify, "require": SignatureModeRequire, "Require": SignatureModeRequire,
	} {
		got, err := ParseSignatureMode(raw)
		if err != nil || got != want {
			t.Errorf("ParseSignatureMode(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	// A typo must be loud. Reading `requre` as `off` is the failure mode that
	// makes a security knob decorative.
	for _, raw := range []string{"requre", "on", "1", "true", "strict"} {
		if _, err := ParseSignatureMode(raw); err == nil {
			t.Errorf("ParseSignatureMode(%q) must be an error, not a silent off", raw)
		}
	}
}

func TestParseTrustedKeys(t *testing.T) {
	pub1, _, _ := ed25519.GenerateKey(nil)
	pub2, _, _ := ed25519.GenerateKey(nil)
	b1 := base64.StdEncoding.EncodeToString(pub1)
	b2 := base64.StdEncoding.EncodeToString(pub2)

	keys, err := ParseTrustedKeys("old:" + b1 + ", new:" + b2)
	if err != nil {
		t.Fatalf("two labelled keys must parse: %v", err)
	}
	if len(keys) != 2 || keys[0].ID != "old" || keys[1].ID != "new" {
		t.Fatalf("got %+v", keys)
	}
	if !keys[0].Key.Equal(pub1) || !keys[1].Key.Equal(pub2) {
		t.Fatal("key material did not round-trip")
	}

	bare, err := ParseTrustedKeys(b1)
	if err != nil || len(bare) != 1 || bare[0].ID != "" {
		t.Fatalf("an unlabelled key must parse: %+v %v", bare, err)
	}

	dup, err := ParseTrustedKeys("a:" + b1 + ",b:" + b1)
	if err != nil || len(dup) != 1 {
		t.Fatalf("the same key twice is one trust decision: %+v %v", dup, err)
	}

	if empty, err := ParseTrustedKeys("  "); err != nil || len(empty) != 0 {
		t.Fatalf("blank must be no keys, not an error: %+v %v", empty, err)
	}

	for _, raw := range []string{
		"nope", // not base64
		"k:" + base64.StdEncoding.EncodeToString([]byte("short")), // wrong length
		"k:" + base64.StdEncoding.EncodeToString(append([]byte(pub1), 0)),
	} {
		if _, err := ParseTrustedKeys(raw); err == nil {
			t.Errorf("ParseTrustedKeys(%q) must be an error", raw)
		}
	}
}

func TestParseManifestBaseURL(t *testing.T) {
	got, err := ParseManifestBaseURL("")
	if err != nil || got != DefaultManifestBaseURL {
		t.Fatalf("blank must be the default: %q %v", got, err)
	}
	got, err = ParseManifestBaseURL("https://mirror.example/quasar/{version}")
	if err != nil || got != "https://mirror.example/quasar/{version}/" {
		t.Fatalf("a missing trailing slash must be added: %q %v", got, err)
	}
	for _, raw := range []string{
		"http://mirror.example/{version}/", // not TLS
		"https://mirror.example/releases/", // no placeholder
		"ftp://mirror.example/{version}/",  // not TLS
		"/relative/{version}/",             // no host
	} {
		if _, err := ParseManifestBaseURL(raw); err == nil {
			t.Errorf("ParseManifestBaseURL(%q) must be an error", raw)
		}
	}
}
