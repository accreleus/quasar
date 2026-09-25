package updater

// The case table behind testdata/recovery/trust-vectors (#356).
//
// Every case names its provenance in `source`: `go:<file>:<Test>[/<case>]` for
// a case lifted from this package's tests, `added:` for one that pins a rule
// those tests exercise only implicitly (check ordering, a boundary, a decoder
// quirk the port must reproduce). Expectations are never written by hand:
// they are Go's own output for the inputs, computed at generation time. Where
// a Go test asserted a reason, the generator re-asserts it, so a table typo
// cannot quietly pin the wrong behaviour.
//
// Regenerate after changing the table:
//
//	QUASAR_WRITE_TRUST_VECTORS=1 go test ./internal/updater -run TestTrustVectorsAreCurrent
//
// Messages are pinned exactly, except where Go embeds text from a library
// (encoding/json, net/url, a transport error): there only the part this
// package authors is pinned, as a prefix.

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	tvPlan      = "go:control-plane/internal/updater/plan_test.go:"
	tvSig       = "go:control-plane/internal/updater/signature_test.go:"
	tvSource    = "go:control-plane/internal/updater/signature_source_test.go:"
	tvSigServer = "go:control-plane/internal/updater/signature_server_test.go:"
	tvServer    = "go:control-plane/internal/updater/server_test.go:"
)

const tvBase = vectorOrigin + "/quasar/v{version}/"

func tvURL(version, asset string) string {
	return strings.ReplaceAll(tvBase, "{version}", version) + asset
}

var (
	tvSigURL      = tvURL("0.3.0", SignatureAssetName)
	tvManifestURL = tvURL("0.3.0", ManifestAssetName)
)

// ── signing helpers (the generator's twin of sign-platform-release-manifest.sh)

type tvSigner struct{ keyID, signer string }

func tvSignature(manifest []byte, signer string) string {
	return base64.StdEncoding.EncodeToString(ed25519.Sign(vectorPrivateKey(signer), manifest))
}

func tvSignDoc(t *testing.T, manifest []byte, signers ...tvSigner) []byte {
	t.Helper()
	doc := SignatureDocument{FormatVersion: SignatureDocumentFormatVersion}
	for _, s := range signers {
		doc.Signatures = append(doc.Signatures, SignatureEntry{
			Algorithm: SignatureAlgorithm, KeyID: s.keyID, Signature: tvSignature(manifest, s.signer),
		})
	}
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// tvNonCanonicalS re-encodes a signature with S+L in place of S: the same
// point equation, a non-canonical scalar. RFC 8032 verifiers refuse it.
func tvNonCanonicalS(manifest []byte, signer string) string {
	sig := ed25519.Sign(vectorPrivateKey(signer), manifest)
	l, _ := new(big.Int).SetString("7237005577332262213973186563042994240857116359379907606001950938285454250989", 10)
	le := func(b []byte) *big.Int {
		r := make([]byte, len(b))
		for i := range b {
			r[len(b)-1-i] = b[i]
		}
		return new(big.Int).SetBytes(r)
	}
	s := new(big.Int).Add(le(sig[32:]), l)
	be := s.FillBytes(make([]byte, 32))
	for i := 0; i < 32; i++ {
		sig[32+i] = be[31-i]
	}
	return base64.StdEncoding.EncodeToString(sig)
}

func tvKeys(pairs ...string) []vectorKey {
	out := []vectorKey{}
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, vectorKey{ID: pairs[i], PublicKeyOf: pairs[i+1]})
	}
	return out
}

func tvCfg(mode string, keys []vectorKey) admitConfig {
	return admitConfig{AllowedNamespaces: []string{"ghcr.io/accreleus/quasar"}, SignatureMode: mode, TrustedKeys: keys}
}

func tvOff() admitConfig { return tvCfg(SignatureModeOff, nil) }

func tvSigned(manifest, doc []byte) *evidenceSpec {
	return &evidenceSpec{State: "signed", Manifest: bytesOf(manifest), Signature: bytesOf(doc)}
}

func tvAbsent(why string) *evidenceSpec { return &evidenceSpec{State: "absent", Why: why} }

func tvFetchErr(e string) *evidenceSpec { return &evidenceSpec{State: "fetch_error", Error: e} }

func tvOK(body []byte) assetResponse { return assetResponse{Status: 200, Body: bytesOf(body)} }

func tvStatus(code int) assetResponse { return assetResponse{Status: code} }

func tvDrop() assetResponse { return assetResponse{TransportError: true} }

func with(r ApplyRequest, f func(*ApplyRequest)) ApplyRequest {
	r.Components = append([]Component(nil), r.Components...)
	f(&r)
	return r
}

// ── kind: admit ──────────────────────────────────────────────────────────────

type admitCase struct {
	name, source string
	agent        bool // caller = agent; default control_plane
	cfg          admitConfig
	req          ApplyRequest
	evidence     *evidenceSpec
	fetch        *fetchSpec
	// want is the reason the Go test asserted ("" = admitted); re-asserted.
	want string
	// prefix pins only the Go-authored start of the message.
	prefix string
	// agentExpect is what the Rust port returns for an agent-caller vector
	// when it differs from Go's caller-less answer: the one rule with no Go
	// counterpart in this package (architecture §5.2, the confused-deputy
	// guard the agent enforces today at node-agent/src/release/mod.rs).
	agentExpect *admitDecision
}

func agentGuard(component string) *admitDecision {
	return &admitDecision{
		Reason: ReasonInvalid,
		Message: `component "` + component + `" may not be named on the agent socket: ` +
			`a node agent asking to replace it is a confused deputy`,
		Warnings: []string{},
	}
}

func admitCases(t *testing.T) []admitCase {
	t.Helper()
	m030 := testManifest(t, "0.3.0")
	good := tvSignDoc(t, m030, tvSigner{"release-2026", "release-2026"})
	keyed := func(mode string) admitConfig { return tvCfg(mode, tvKeys("release-2026", "release-2026")) }
	signedEv := tvSigned(m030, good)
	signedReq := signedRequest("0.3.0")
	nullVersion := with(signedReq, func(r *ApplyRequest) { r.Release.Version = nil })
	signedManifest := func(raw string) *evidenceSpec {
		return tvSigned([]byte(raw), tvSignDoc(t, []byte(raw), tvSigner{"release-2026", "release-2026"}))
	}
	na := func(image, digest string) Component {
		return Component{Name: "node-agent", Image: image, Digest: digest}
	}
	cp := func(image, digest string) Component {
		return Component{Name: "control-plane", Image: image, Digest: digest}
	}
	agentImg := "ghcr.io/accreleus/quasar/quasar-node-agent"
	controlImg := "ghcr.io/accreleus/quasar/quasar-control-plane"
	fetchBoth := &fetchSpec{BaseURL: tvBase, Responses: map[string]assetResponse{
		tvSigURL: tvOK(good), tvManifestURL: tvOK(m030),
	}}
	comps := func(cs ...Component) func(*ApplyRequest) {
		return func(r *ApplyRequest) { r.Components = append([]Component{}, cs...) }
	}

	return []admitCase{
		// ── plan.go request gates, lifted from plan_test.go ──
		{name: "an agent apply is admitted", source: tvPlan + "TestPlanAcceptsAnAgentApply", cfg: tvOff(), req: agentReq()},
		{name: "foreign namespace", source: tvPlan + "TestPlanRejections/foreign namespace", cfg: tvOff(),
			req: with(agentReq(), func(r *ApplyRequest) { r.Components[0].Image = "ghcr.io/someone-else/quasar/quasar-node-agent" }), want: ReasonNamespaceRejected},
		{name: "namespace prefix is not a segment boundary", source: tvPlan + "TestPlanRejections/namespace prefix is not a segment boundary", cfg: tvOff(),
			req: with(agentReq(), func(r *ApplyRequest) { r.Components[0].Image = "ghcr.io/accreleus/quasar-evil/quasar-node-agent" }), want: ReasonNamespaceRejected},
		{name: "malformed digest", source: tvPlan + "TestPlanRejections/malformed digest", cfg: tvOff(),
			req: with(agentReq(), func(r *ApplyRequest) { r.Components[0].Digest = "sha256:notahexdigest" }), want: ReasonDigestMalformed},
		{name: "uppercase digest", source: tvPlan + "TestPlanRejections/uppercase digest", cfg: tvOff(),
			req: with(agentReq(), func(r *ApplyRequest) { r.Components[0].Digest = strings.ToUpper(goodDigest[7:]) }), want: ReasonDigestMalformed},
		{name: "image carries a tag", source: tvPlan + "TestPlanRejections/image carries a tag", cfg: tvOff(),
			req: with(agentReq(), func(r *ApplyRequest) { r.Components[0].Image = agentImg + ":latest" }), want: ReasonInvalid},
		{name: "image carries a digest", source: tvPlan + "TestPlanRejections/image carries a digest", cfg: tvOff(),
			req: with(agentReq(), func(r *ApplyRequest) { r.Components[0].Image = agentImg + "@" + goodDigest }), want: ReasonInvalid},
		{name: "unknown component", source: tvPlan + "TestPlanRejections/unknown component", cfg: tvOff(),
			req: with(agentReq(), func(r *ApplyRequest) { r.Components[0].Name = "postgres" }), want: ReasonInvalid},
		{name: "the updater naming itself", source: tvPlan + "TestPlanRejections/the updater naming itself", cfg: tvOff(),
			req: with(agentReq(), func(r *ApplyRequest) { r.Components[0].Name = "updater" }), want: ReasonInvalid},
		{name: "the updater naming its service", source: tvPlan + "TestPlanRejections/the updater naming its service; " + tvServer + "TestServerRejectionStatuses", cfg: tvOff(),
			req: with(agentReq(), func(r *ApplyRequest) { r.Components[0].Name = "quasar-updater" }), want: ReasonInvalid},
		{name: "empty components", source: tvPlan + "TestPlanRejections/empty components (nil there; [] here, the same gate)", cfg: tvOff(),
			req: with(agentReq(), comps()), want: ReasonInvalid},
		{name: "duplicate component", source: tvPlan + "TestPlanRejections/duplicate component", cfg: tvOff(),
			req: with(agentReq(), func(r *ApplyRequest) { r.Components = append(r.Components, r.Components[0]) }), want: ReasonInvalid},
		{name: "request id is not a uuid", source: tvPlan + "TestPlanRejections/request id is not a uuid", cfg: tvOff(),
			req: with(agentReq(), func(r *ApplyRequest) { r.RequestID = "nope" }), want: ReasonInvalid},
		{name: "busy", source: tvPlan + "TestPlanRejections/busy; " + tvServer + "TestServerBusyRefusesNeverQueues",
			cfg: func() admitConfig { c := tvOff(); c.InFlightRequestID = otherID; return c }(), req: agentReq(), want: ReasonBusy},
		{name: "re-posting the in-flight id is not busy", source: tvPlan + "TestPlanAcceptsTheInFlightIDAgain",
			cfg: func() admitConfig { c := tvOff(); c.InFlightRequestID = reqID; return c }(), req: agentReq()},
		{name: "a control-plane apply is admitted", source: tvPlan + "TestPlanControlPlaneMapsToItsOwnVariable", cfg: tvOff(),
			req: with(agentReq(), comps(cp(controlImg, goodDigest)))},
		{name: "the org default allowlist admits an org image", source: tvPlan + "TestUnsetNamespaceKnobIsTheOrgDefault",
			cfg: admitConfig{AllowedNamespaces: DefaultAllowedNamespaces, SignatureMode: SignatureModeOff}, req: agentReq()},
		{name: "the org default allowlist refuses a foreign image", source: tvPlan + "TestUnsetNamespaceKnobIsTheOrgDefault",
			cfg: admitConfig{AllowedNamespaces: DefaultAllowedNamespaces, SignatureMode: SignatureModeOff},
			req: with(agentReq(), func(r *ApplyRequest) { r.Components[0].Image = "docker.io/library/busybox" }), want: ReasonNamespaceRejected},
		{name: "a two-character digest", source: tvServer + "TestServerRejectionStatuses", cfg: tvOff(),
			req: with(agentReq(), func(r *ApplyRequest) { r.RequestID = otherID; r.Components[0].Digest = "sha256:zz" }), want: ReasonDigestMalformed},

		// ── plan.go rules the Go tests exercise only implicitly ──
		{name: "an uppercase-hex request id is a uuid", source: "added: plan.go uuidRe accepts A-F", cfg: tvOff(),
			req: with(agentReq(), func(r *ApplyRequest) { r.RequestID = strings.ToUpper(reqID) })},
		{name: "a request id with a trailing newline is not a uuid", source: "added: plan.go uuidRe is anchored at the end of text", cfg: tvOff(),
			req: with(agentReq(), func(r *ApplyRequest) { r.RequestID = reqID + "\n" }), want: ReasonInvalid},
		{name: "a request id with braces is not a uuid", source: "added: plan.go uuidRe", cfg: tvOff(),
			req: with(agentReq(), func(r *ApplyRequest) { r.RequestID = "{" + reqID + "}" }), want: ReasonInvalid},
		{name: "a malformed request id beats busy", source: "added: plan.go checks request_id before single-flight",
			cfg: func() admitConfig { c := tvOff(); c.InFlightRequestID = otherID; return c }(),
			req: with(agentReq(), func(r *ApplyRequest) { r.RequestID = "nope" }), want: ReasonInvalid},
		{name: "busy beats empty components", source: "added: plan.go checks single-flight before the component rules",
			cfg: func() admitConfig { c := tvOff(); c.InFlightRequestID = otherID; return c }(),
			req: with(agentReq(), comps()), want: ReasonBusy},
		{name: "busy beats a foreign namespace", source: "added: plan.go checks single-flight before the component rules",
			cfg: func() admitConfig { c := tvOff(); c.InFlightRequestID = otherID; return c }(),
			req: with(agentReq(), func(r *ApplyRequest) { r.Components[0].Image = "docker.io/library/busybox" }), want: ReasonBusy},
		{name: "the first bad component decides: namespace before a later bad digest", source: "added: plan.go grades components in request order", cfg: tvOff(),
			req: with(agentReq(), comps(cp("docker.io/library/busybox", goodDigest), na(agentImg, "sha256:zz"))), want: ReasonNamespaceRejected},
		{name: "the first bad component decides: digest before a later foreign namespace", source: "added: plan.go grades components in request order", cfg: tvOff(),
			req: with(agentReq(), comps(na(agentImg, "sha256:zz"), cp("docker.io/library/busybox", goodDigest))), want: ReasonDigestMalformed},
		{name: "an unknown name beats every other fault in that component", source: "added: plan.go per-component check order", cfg: tvOff(),
			req: with(agentReq(), comps(Component{Name: "postgres", Image: "docker.io/library/postgres:16", Digest: "zz"})), want: ReasonInvalid},
		{name: "a tag beats a bad digest and a foreign namespace", source: "added: plan.go per-component check order", cfg: tvOff(),
			req: with(agentReq(), comps(na("docker.io/library/busybox:1", "zz"))), want: ReasonInvalid},
		{name: "a bad digest beats a foreign namespace", source: "added: plan.go per-component check order", cfg: tvOff(),
			req: with(agentReq(), comps(na("docker.io/library/busybox", "zz"))), want: ReasonDigestMalformed},
		{name: "a duplicate is caught before its own image is graded", source: "added: plan.go per-component check order", cfg: tvOff(),
			req: with(agentReq(), comps(na(agentImg, goodDigest), na("x y", "zz"))), want: ReasonInvalid},
		{name: "an empty image", source: "added: plan.go image rule", cfg: tvOff(),
			req: with(agentReq(), func(r *ApplyRequest) { r.Components[0].Image = "" }), want: ReasonInvalid},
		{name: "an image containing a space", source: "added: plan.go image rule", cfg: tvOff(),
			req: with(agentReq(), func(r *ApplyRequest) { r.Components[0].Image = agentImg + " x" }), want: ReasonInvalid},
		{name: "an image containing a tab", source: "added: plan.go image rule", cfg: tvOff(),
			req: with(agentReq(), func(r *ApplyRequest) { r.Components[0].Image = agentImg + "\tx" }), want: ReasonInvalid},
		{name: "an image containing a newline", source: "added: plan.go image rule", cfg: tvOff(),
			req: with(agentReq(), func(r *ApplyRequest) { r.Components[0].Image = agentImg + "\n" }), want: ReasonInvalid},
		{name: "an image containing a carriage return is admitted", source: "added: plan.go image rule rejects only space, tab and newline (pinned as found)", cfg: tvOff(),
			req: with(agentReq(), func(r *ApplyRequest) { r.Components[0].Image = agentImg + "\r" })},
		{name: "a registry port is not a tag", source: "added: plan.go imageHasTagOrDigest",
			cfg: admitConfig{AllowedNamespaces: []string{"registry.example.invalid:5000/quasar"}, SignatureMode: SignatureModeOff},
			req: with(agentReq(), func(r *ApplyRequest) {
				r.Components[0].Image = "registry.example.invalid:5000/quasar/quasar-node-agent"
			})},
		{name: "a colon in the last path segment is a tag", source: "added: plan.go imageHasTagOrDigest",
			cfg: admitConfig{AllowedNamespaces: []string{"registry.example.invalid:5000"}, SignatureMode: SignatureModeOff},
			req: with(agentReq(), func(r *ApplyRequest) { r.Components[0].Image = "registry.example.invalid:5000" }), want: ReasonInvalid},
		{name: "a single-segment image with a colon is a tag", source: "added: plan.go imageHasTagOrDigest with no slash", cfg: tvOff(),
			req: with(agentReq(), func(r *ApplyRequest) { r.Components[0].Image = "busybox:1" }), want: ReasonInvalid},
		{name: "an at sign anywhere is a digest", source: "added: plan.go imageHasTagOrDigest", cfg: tvOff(),
			req: with(agentReq(), func(r *ApplyRequest) { r.Components[0].Image = "ghcr.io/accreleus@quasar/quasar-node-agent" }), want: ReasonInvalid},
		{name: "a digest with 63 hex digits", source: "added: plan.go digestRe length", cfg: tvOff(),
			req: with(agentReq(), func(r *ApplyRequest) { r.Components[0].Digest = goodDigest[:len(goodDigest)-1] }), want: ReasonDigestMalformed},
		{name: "a digest with 65 hex digits", source: "added: plan.go digestRe length", cfg: tvOff(),
			req: with(agentReq(), func(r *ApplyRequest) { r.Components[0].Digest = goodDigest + "0" }), want: ReasonDigestMalformed},
		{name: "an uppercase algorithm prefix", source: "added: plan.go digestRe", cfg: tvOff(),
			req: with(agentReq(), func(r *ApplyRequest) { r.Components[0].Digest = "SHA256:" + goodDigest[7:] }), want: ReasonDigestMalformed},
		{name: "a sha512 digest", source: "added: plan.go digestRe", cfg: tvOff(),
			req: with(agentReq(), func(r *ApplyRequest) { r.Components[0].Digest = "sha512:" + goodDigest[7:] }), want: ReasonDigestMalformed},
		{name: "a digest with a trailing newline", source: "added: plan.go digestRe is anchored at the end of text", cfg: tvOff(),
			req: with(agentReq(), func(r *ApplyRequest) { r.Components[0].Digest = goodDigest + "\n" }), want: ReasonDigestMalformed},
		{name: "an image equal to the namespace", source: "added: plan.go namespaceAllowed needs a path below the namespace", cfg: tvOff(),
			req: with(agentReq(), func(r *ApplyRequest) { r.Components[0].Image = "ghcr.io/accreleus/quasar" }), want: ReasonNamespaceRejected},
		{name: "the namespace with only a trailing slash", source: "added: plan.go namespaceAllowed length rule", cfg: tvOff(),
			req: with(agentReq(), func(r *ApplyRequest) { r.Components[0].Image = "ghcr.io/accreleus/quasar/" }), want: ReasonNamespaceRejected},
		{name: "the namespace match is case-sensitive", source: "added: plan.go namespaceAllowed is a byte-exact prefix", cfg: tvOff(),
			req: with(agentReq(), func(r *ApplyRequest) { r.Components[0].Image = "GHCR.io/accreleus/quasar/quasar-node-agent" }), want: ReasonNamespaceRejected},
		{name: "a deeper path under the namespace", source: "added: plan.go namespaceAllowed", cfg: tvOff(),
			req: with(agentReq(), func(r *ApplyRequest) { r.Components[0].Image = "ghcr.io/accreleus/quasar/nested/quasar-node-agent" })},
		{name: "any listed namespace admits", source: "added: plan.go namespaceAllowed walks the whole list",
			cfg: admitConfig{AllowedNamespaces: []string{"registry.example.invalid/mirror", "ghcr.io/accreleus/quasar"}, SignatureMode: SignatureModeOff}, req: agentReq()},
		{name: "a parent namespace admits its children", source: "added: plan.go namespaceAllowed",
			cfg: admitConfig{AllowedNamespaces: []string{"ghcr.io/accreleus"}, SignatureMode: SignatureModeOff}, req: agentReq()},
		{name: "an empty allowlist admits nothing", source: "added: plan.go namespaceAllowed (ParseNamespaces never yields one)",
			cfg: admitConfig{AllowedNamespaces: []string{}, SignatureMode: SignatureModeOff}, req: agentReq(), want: ReasonNamespaceRejected},
		{name: "both components in one request", source: "added: plan.go accepts the closed table in any order", cfg: tvOff(),
			req: with(agentReq(), comps(na(agentImg, goodDigest), cp(controlImg, goodDigest)))},

		// ── the confused-deputy guard (Rust-only; Go answers caller-less) ──
		{name: "the agent socket may name the node agent", source: "added: architecture §5.2 caller guard; the agent's own ack-time rule today",
			agent: true, cfg: tvOff(), req: agentReq()},
		{name: "the agent socket may not name the control plane", source: "added: architecture §5.2 caller guard; agent-api.md release_apply `invalid` for control-plane",
			agent: true, cfg: tvOff(), req: with(agentReq(), comps(cp(controlImg, goodDigest))), agentExpect: agentGuard("control-plane")},
		{name: "the agent socket may not slip the control plane in second", source: "added: architecture §5.2 caller guard",
			agent: true, cfg: tvOff(), req: with(agentReq(), comps(na(agentImg, goodDigest), cp(controlImg, goodDigest))), agentExpect: agentGuard("control-plane")},
		{name: "the caller guard beats the control plane's own bad digest", source: "added: the guard sits beside the closed-table check",
			agent: true, cfg: tvOff(), req: with(agentReq(), comps(cp(controlImg, "zz"))), want: ReasonDigestMalformed, agentExpect: agentGuard("control-plane")},
		{name: "busy still beats the caller guard", source: "added: single-flight is checked before any component",
			agent: true, cfg: func() admitConfig { c := tvOff(); c.InFlightRequestID = otherID; return c }(),
			req: with(agentReq(), comps(cp(controlImg, goodDigest))), want: ReasonBusy},
		{name: "an unknown name on the agent socket is the closed-table refusal", source: "added: the closed table is checked before the guard",
			agent: true, cfg: tvOff(), req: with(agentReq(), func(r *ApplyRequest) { r.Components[0].Name = "quasar-updater" }), want: ReasonInvalid},

		// ── signature.go checkSignature, lifted from signature_test.go ──
		{name: "off ignores unverifiable evidence and no keys", source: tvSig + "TestCheckSignatureOffIgnoresEverything",
			cfg: tvOff(), req: signedReq, evidence: tvSigned([]byte(`{}`), []byte(`garbage`))},
		{name: "verify with no trusted keys refuses", source: tvSig + "TestCheckSignatureFailsClosedWithNoTrustedKeys",
			cfg: tvCfg(SignatureModeVerify, nil), req: signedReq, evidence: tvAbsent("none"), want: ReasonSignatureInvalid},
		{name: "require with no trusted keys refuses", source: tvSig + "TestCheckSignatureFailsClosedWithNoTrustedKeys",
			cfg: tvCfg(SignatureModeRequire, nil), req: signedReq, evidence: tvAbsent("none"), want: ReasonSignatureInvalid},
		{name: "verify accepts a definitive absence and warns", source: tvSig + "TestCheckSignatureAbsentSignature",
			cfg: keyed(SignatureModeVerify), req: signedReq, evidence: tvAbsent("release 0.3.0 publishes no signature asset")},
		{name: "require refuses a definitive absence", source: tvSig + "TestCheckSignatureAbsentSignature",
			cfg: keyed(SignatureModeRequire), req: signedReq, evidence: tvAbsent("release 0.3.0 publishes no signature asset"), want: ReasonSignatureMissing},
		{name: "verify refuses an undetermined signature", source: tvSig + "TestCheckSignatureUndeterminedIsNeverReadAsUnsigned",
			cfg: keyed(SignatureModeVerify), req: signedReq, evidence: tvFetchErr("dial tcp: no route to host"), want: ReasonSignatureInvalid},
		{name: "require refuses an undetermined signature", source: tvSig + "TestCheckSignatureUndeterminedIsNeverReadAsUnsigned",
			cfg: keyed(SignatureModeRequire), req: signedReq, evidence: tvFetchErr("dial tcp: no route to host"), want: ReasonSignatureInvalid},
		{name: "verify accepts a good signature", source: tvSig + "TestCheckSignatureGoodSignatureAccepted",
			cfg: keyed(SignatureModeVerify), req: signedReq, evidence: signedEv},
		{name: "require accepts a good signature", source: tvSig + "TestCheckSignatureGoodSignatureAccepted",
			cfg: keyed(SignatureModeRequire), req: signedReq, evidence: signedEv},
		{name: "binding: a digest the signed manifest does not name", source: tvSig + "TestCheckSignatureBindsTheManifestToTheRequest (swapped)",
			cfg: keyed(SignatureModeVerify), evidence: signedEv, want: ReasonSignatureInvalid,
			req: with(signedReq, func(r *ApplyRequest) { r.Components[0].Digest = "sha256:" + strings.Repeat("f", 64) })},
		{name: "binding: an image the signed manifest does not name", source: tvSig + "TestCheckSignatureBindsTheManifestToTheRequest (elsewhere)",
			cfg: keyed(SignatureModeVerify), evidence: signedEv, want: ReasonSignatureInvalid,
			req: with(signedReq, func(r *ApplyRequest) { r.Components[0].Image = "ghcr.io/accreleus/quasar/quasar-node-agent-evil" })},
		{name: "binding: a manifest for another version", source: tvSig + "TestCheckSignatureBindsTheManifestToTheRequest (other)",
			cfg: keyed(SignatureModeVerify), evidence: signedEv, req: signedRequest("9.9.9"), want: ReasonSignatureInvalid},
		{name: "binding: a digest that belongs to another component", source: tvSig + "TestCheckSignatureBindsTheManifestToTheRequest (unknown)",
			cfg: keyed(SignatureModeVerify), evidence: signedEv, want: ReasonSignatureInvalid,
			req: with(signedReq, comps(cp(testControlImage, testAgentDigest)))},
		{name: "a forward-compatible manifest still verifies", source: tvSig + "TestCheckSignatureToleratesAFutureManifestShape",
			cfg: keyed(SignatureModeRequire), req: signedReq,
			evidence: signedManifest(`{"format_version":2,"version":"0.3.0","something_new":{"a":1},` +
				`"components":[{"name":"node-agent","image":"` + testAgentImage + `","digest":"` + testAgentDigest + `"}]}`)},
		{name: "verify applies an unpublished version and says so", source: tvSig + "TestVerifyModeAppliesAnUnpublishedVersionAndSaysSoLoudly (adapted: through admit the request must pass the request gates, so it is a valid request naming 9.9.9 rather than an empty one)",
			cfg: keyed(SignatureModeVerify), req: signedRequest("9.9.9"), evidence: tvAbsent("no signature asset published for v9.9.9")},
		{name: "require refuses the same unpublished version", source: tvSig + "TestRequireModeRefusesTheSameUnpublishedVersion (adapted as above)",
			cfg: keyed(SignatureModeRequire), req: signedRequest("9.9.9"), evidence: tvAbsent("no signature asset published for v9.9.9"), want: ReasonSignatureMissing},

		// ── checkSignature rules the Go tests exercise only implicitly ──
		{name: "verify with no evidence gathered refuses", source: "added: signature.go checkSignature nil evidence",
			cfg: keyed(SignatureModeVerify), req: signedReq, want: ReasonSignatureInvalid},
		{name: "no trusted keys is reported before missing evidence", source: "added: signature.go checkSignature order",
			cfg: tvCfg(SignatureModeRequire, nil), req: signedReq, want: ReasonSignatureInvalid},
		{name: "the request gates run before the signature gate", source: "added: plan.go grades the signature last",
			cfg: keyed(SignatureModeRequire), evidence: signedEv, want: ReasonDigestMalformed,
			req: with(signedReq, func(r *ApplyRequest) { r.Components[0].Digest = "zz" })},
		{name: "a tampered manifest is refused", source: tvSigServer + "TestServerRefusesATamperedManifest",
			cfg: keyed(SignatureModeVerify), req: signedReq, want: ReasonSignatureInvalid,
			evidence: tvSigned(func() []byte { b := append([]byte(nil), m030...); b[len(b)-2] = ' '; return b }(), good)},
		{name: "binding: a padded manifest version is another version", source: "added: bindManifest compares the manifest version untrimmed",
			cfg: keyed(SignatureModeVerify), req: signedReq, want: ReasonSignatureInvalid,
			evidence: signedManifest(`{"version":" 0.3.0","components":[{"name":"node-agent","image":"` + testAgentImage + `","digest":"` + testAgentDigest + `"}]}`)},
		{name: "binding: the request version is trimmed", source: "added: bindManifest trims the requested version",
			cfg: keyed(SignatureModeVerify), evidence: signedEv, req: signedRequest(" 0.3.0 ")},
		{name: "binding: a request with no version is bound only by its components", source: "added: bindManifest skips the version when the request names none (unreachable through the fetcher, which reports that as absent)",
			cfg: keyed(SignatureModeRequire), evidence: signedEv, req: nullVersion},
		{name: "binding: every same-named manifest entry must match", source: "added: bindManifest walks every entry with the name",
			cfg: keyed(SignatureModeVerify), req: signedReq, want: ReasonSignatureInvalid,
			evidence: signedManifest(`{"version":"0.3.0","components":[{"name":"node-agent","image":"` + testAgentImage + `","digest":"` + testAgentDigest + `"},` +
				`{"name":"node-agent","image":"` + testAgentImage + `","digest":"` + testControlDigest + `"}]}`)},
		{name: "binding: extra manifest components are ignored", source: "added: bindManifest reads only what the request names",
			cfg: keyed(SignatureModeVerify), req: signedReq,
			evidence: signedManifest(`{"version":"0.3.0","components":[{"name":"recovery-actor","image":"x","digest":"y"},{"name":"node-agent","image":"` + testAgentImage + `","digest":"` + testAgentDigest + `"}]}`)},
		{name: "binding: a manifest naming no components", source: "added: bindManifest",
			cfg: keyed(SignatureModeVerify), req: signedReq, want: ReasonSignatureInvalid, evidence: signedManifest(`{"version":"0.3.0","components":null}`)},
		{name: "binding: a component the manifest does not name", source: "added: bindManifest",
			cfg: keyed(SignatureModeVerify), want: ReasonSignatureInvalid, evidence: signedManifest(`{"version":"0.3.0","components":[{"name":"control-plane","image":"` + testControlImage + `","digest":"` + testControlDigest + `"}]}`),
			req: signedReq},
		{name: "binding: both components named and bound", source: "added: bindManifest", cfg: keyed(SignatureModeRequire), evidence: signedEv,
			req: with(signedReq, comps(cp(testControlImage, testControlDigest), na(testAgentImage, testAgentDigest)))},
		{name: "binding: manifest field names match case-insensitively", source: "added: Go encoding/json matches field names case-insensitively",
			cfg: keyed(SignatureModeVerify), req: signedReq,
			evidence: signedManifest(`{"VERSION":"0.3.0","Components":[{"NAME":"node-agent","Image":"` + testAgentImage + `","dIgEsT":"` + testAgentDigest + `"}]}`)},
		{name: "binding: a repeated components key merges into the earlier entries", source: "added: Go encoding/json decodes a repeated array into the existing elements",
			cfg: keyed(SignatureModeVerify), req: signedReq,
			evidence: signedManifest(`{"version":"0.3.0","components":[{"name":"node-agent","image":"` + testAgentImage + `","digest":"` + testAgentDigest + `"}],"components":[{"name":"node-agent"}]}`)},
		{name: "binding: trailing content after the manifest", source: "added: bindManifest uses json.Unmarshal (one value, then only space)",
			cfg: keyed(SignatureModeVerify), req: signedReq, want: ReasonSignatureInvalid,
			prefix:   "the signed release manifest (key release-2026) does not describe this request: the signed manifest is not readable JSON: ",
			evidence: signedManifest(`{"version":"0.3.0","components":[{"name":"node-agent","image":"` + testAgentImage + `","digest":"` + testAgentDigest + `"}]} x`)},
		{name: "binding: a numeric manifest version", source: "added: bindManifest type error",
			cfg: keyed(SignatureModeVerify), req: signedReq, want: ReasonSignatureInvalid,
			prefix:   "the signed release manifest (key release-2026) does not describe this request: the signed manifest is not readable JSON: ",
			evidence: signedManifest(`{"version":3,"components":[]}`)},
		{name: "binding: nesting at Go's depth limit in an ignored field", source: "added: Go encoding/json allows 10000 levels",
			cfg: keyed(SignatureModeVerify), req: signedReq,
			evidence: signedManifest(`{"version":"0.3.0","components":[{"name":"node-agent","image":"` + testAgentImage + `","digest":"` + testAgentDigest + `"}],"x":` +
				strings.Repeat("[", 9999) + strings.Repeat("]", 9999) + `}`)},
		{name: "binding: nesting past Go's depth limit in an ignored field", source: "added: Go encoding/json refuses a 10001st level",
			cfg: keyed(SignatureModeVerify), req: signedReq, want: ReasonSignatureInvalid,
			prefix: "the signed release manifest (key release-2026) does not describe this request: the signed manifest is not readable JSON: ",
			evidence: signedManifest(`{"version":"0.3.0","components":[{"name":"node-agent","image":"` + testAgentImage + `","digest":"` + testAgentDigest + `"}],"x":` +
				strings.Repeat("[", 10000) + strings.Repeat("]", 10000) + `}`)},
		{name: "binding: a null manifest is an empty one", source: "added: json.Unmarshal of null leaves the zero value",
			cfg: keyed(SignatureModeVerify), req: signedReq, want: ReasonSignatureInvalid, evidence: signedManifest(`null`)},
		{name: "binding: a null version leaves the version empty", source: "added: Go encoding/json null handling",
			cfg: keyed(SignatureModeVerify), req: signedReq, want: ReasonSignatureInvalid,
			evidence: signedManifest(`{"version":null,"components":[{"name":"node-agent","image":"` + testAgentImage + `","digest":"` + testAgentDigest + `"}]}`)},

		// ── end to end: evidence gathered by the fetcher, as server.go does ──
		{name: "fetched: missing signature under require", source: tvSigServer + "TestServerRefusesAMissingSignatureUnderRequire",
			cfg: keyed(SignatureModeRequire), req: signedReq, want: ReasonSignatureMissing,
			fetch: &fetchSpec{BaseURL: tvBase, Responses: map[string]assetResponse{tvManifestURL: tvOK(m030)}}},
		{name: "fetched: missing signature under verify", source: tvSigServer + "TestServerAcceptsAMissingSignatureUnderVerify",
			cfg: keyed(SignatureModeVerify), req: signedReq,
			fetch: &fetchSpec{BaseURL: tvBase, Responses: map[string]assetResponse{}}},
		{name: "fetched: a good signature", source: tvSigServer + "TestServerAcceptsAGoodSignature",
			cfg: keyed(SignatureModeRequire), req: signedReq, fetch: fetchBoth},
		{name: "fetched: signing off fetches nothing", source: tvSigServer + "TestServerFetchesNothingWhenSigningIsOff",
			cfg: tvOff(), req: signedReq, fetch: &fetchSpec{BaseURL: tvBase, Responses: map[string]assetResponse{tvSigURL: tvStatus(503)}}},
		{name: "fetched: require refuses a request with no version, fetching nothing", source: "added: ADR 0003 (require refuses a release with no version)",
			cfg: keyed(SignatureModeRequire), req: nullVersion, want: ReasonSignatureMissing, fetch: fetchBoth},
		{name: "fetched: verify applies a request with no version and warns", source: "added: ADR 0003 (the documented verify bypass)",
			cfg: keyed(SignatureModeVerify), req: nullVersion, fetch: fetchBoth},
		{name: "fetched: a signature host outage is never unsigned", source: "added: ADR 0003 fail closed",
			cfg: keyed(SignatureModeVerify), req: signedReq, want: ReasonSignatureInvalid,
			fetch: &fetchSpec{BaseURL: tvBase, Responses: map[string]assetResponse{tvSigURL: tvStatus(503), tvManifestURL: tvOK(m030)}}},
		{name: "fetched: a dropped connection is never unsigned", source: "added: ADR 0003 fail closed",
			cfg: keyed(SignatureModeVerify), req: signedReq, want: ReasonSignatureInvalid,
			prefix: "the release signature could not be retrieved, so it could not be checked: fetching " + tvSigURL + ": ",
			fetch:  &fetchSpec{BaseURL: tvBase, Responses: map[string]assetResponse{tvSigURL: tvDrop()}}},
		{name: "fetched: a signature with no manifest is never unsigned", source: "added: signature_source.go (a broken publish)",
			cfg: keyed(SignatureModeVerify), req: signedReq, want: ReasonSignatureInvalid,
			fetch: &fetchSpec{BaseURL: tvBase, Responses: map[string]assetResponse{tvSigURL: tvOK(good)}}},
		{name: "fetched: a non-semver version is never unsigned", source: "added: signature_source.go versionRe",
			cfg: keyed(SignatureModeVerify), req: signedRequest("latest"), want: ReasonSignatureInvalid, fetch: fetchBoth},
		{name: "fetched: busy fetches nothing", source: "added: server.go signatureEvidence skips the fetch when busy",
			cfg: func() admitConfig { c := keyed(SignatureModeRequire); c.InFlightRequestID = reqID; return c }(),
			req: signedReq, want: ReasonBusy, fetch: fetchBoth},
		{name: "fetched: a signed release for another version is refused", source: "added: the fetch is by the request's version, the binding is by the manifest's",
			cfg: keyed(SignatureModeRequire), req: signedRequest("0.4.0"), want: ReasonSignatureInvalid,
			fetch: &fetchSpec{BaseURL: tvBase, Responses: map[string]assetResponse{
				tvURL("0.4.0", SignatureAssetName): tvOK(good), tvURL("0.4.0", ManifestAssetName): tvOK(m030)}}},
	}
}

// ── kind: verify_signature ───────────────────────────────────────────────────

type verifyCase struct {
	name, source string
	manifest     *bytesSpec
	doc          *bytesSpec
	keys         []vectorKey
	wantOK       bool
	prefix       string
}

func verifyCases(t *testing.T) []verifyCase {
	t.Helper()
	m := testManifest(t, "0.3.0")
	mb := bytesOf(m)
	sig := func(signer string) string { return tvSignature(m, signer) }
	doc := func(entries string) *bytesSpec { return textOf(`{"format_version":1,"signatures":[` + entries + `]}`) }
	entry := func(alg, id, s string) string {
		return `{"algorithm":"` + alg + `","key_id":"` + id + `","signature":"` + s + `"}`
	}
	k := tvKeys("k", "k")
	r26 := tvKeys("release-2026", "release-2026")
	good := bytesOf(tvSignDoc(t, m, tvSigner{"release-2026", "release-2026"}))
	dual := bytesOf(tvSignDoc(t, m, tvSigner{"release-2025", "release-2025"}, tvSigner{"release-2026", "release-2026"}))
	newOnly := bytesOf(tvSignDoc(t, m, tvSigner{"release-2026", "release-2026"}))
	tampered := []byte(strings.Replace(string(m), testAgentDigest, "sha256:"+strings.Repeat("e", 64), 1))
	jsonErr := "the signature document is not valid JSON in the documented shape: "
	deep := func(n int) *bytesSpec {
		return &bytesSpec{Segments: []bytesSegment{
			{Text: `{"format_version":1,"signatures":`, Count: 1}, {Text: "[", Count: n}, {Text: "]", Count: n}, {Text: "}", Count: 1},
		}}
	}
	withNewline := sig("k")[:40] + "\n" + sig("k")[40:]

	return []verifyCase{
		{name: "a genuine signature verifies", source: tvSig + "TestVerifyManifestSignatureAcceptsAGoodSignature", manifest: mb, doc: good, keys: r26, wantOK: true},
		{name: "a tampered manifest does not verify", source: tvSig + "TestVerifyManifestSignatureRejectsATamperedManifest", manifest: bytesOf(tampered), doc: good, keys: r26},
		{name: "an untrusted key wearing a trusted label", source: tvSig + "TestVerifyManifestSignatureRejectsTheWrongKey",
			manifest: mb, doc: bytesOf(tvSignDoc(t, m, tvSigner{"release-2026", "attacker"})), keys: r26},
		{name: "rotation: dual-signed, host trusts the old key only", source: tvSig + "TestVerifyManifestSignatureRotationOverlap/old only", manifest: mb, doc: dual, keys: tvKeys("release-2025", "release-2025"), wantOK: true},
		{name: "rotation: dual-signed, host trusts the new key only", source: tvSig + "TestVerifyManifestSignatureRotationOverlap/new only", manifest: mb, doc: dual, keys: r26, wantOK: true},
		{name: "rotation: dual-signed, host trusts both", source: tvSig + "TestVerifyManifestSignatureRotationOverlap/both", manifest: mb, doc: dual, keys: tvKeys("release-2025", "release-2025", "release-2026", "release-2026"), wantOK: true},
		{name: "rotation: dual-signed, host trusts both, new first", source: tvSig + "TestVerifyManifestSignatureRotationOverlap/new first", manifest: mb, doc: dual, keys: tvKeys("release-2026", "release-2026", "release-2025", "release-2025"), wantOK: true},
		{name: "rotation: signed by the new key only, host trusts the old", source: tvSig + "TestVerifyManifestSignatureRotationOverlap", manifest: mb, doc: newOnly, keys: tvKeys("release-2025", "release-2025")},
		{name: "malformed: not json", source: tvSig + "TestVerifyManifestSignatureRejectsMalformedDocuments/not json", manifest: mb, doc: textOf("{"), keys: k, prefix: jsonErr},
		{name: "malformed: unknown field", source: tvSig + "TestVerifyManifestSignatureRejectsMalformedDocuments/unknown field", manifest: mb, doc: textOf(`{"format_version":1,"signatures":[],"extra":1}`), keys: k, prefix: jsonErr},
		{name: "malformed: future format", source: tvSig + "TestVerifyManifestSignatureRejectsMalformedDocuments/future format", manifest: mb, doc: textOf(`{"format_version":2,"signatures":[]}`), keys: k},
		{name: "malformed: no signatures", source: tvSig + "TestVerifyManifestSignatureRejectsMalformedDocuments/no signatures", manifest: mb, doc: textOf(`{"format_version":1,"signatures":[]}`), keys: k},
		{name: "malformed: unknown algorithm only", source: tvSig + "TestVerifyManifestSignatureRejectsMalformedDocuments/unknown algorithm", manifest: mb, doc: doc(entry("rsa", "k", "AA==")), keys: k},
		{name: "malformed: truncated signature", source: tvSig + "TestVerifyManifestSignatureRejectsMalformedDocuments/truncated signature", manifest: mb, doc: doc(entry("ed25519", "k", "AA==")), keys: k},
		{name: "malformed: not base64", source: tvSig + "TestVerifyManifestSignatureRejectsMalformedDocuments/not base64", manifest: mb, doc: doc(entry("ed25519", "k", "!!!")), keys: k},
		{name: "malformed: trailing content", source: tvSig + "TestVerifyManifestSignatureRejectsMalformedDocuments/trailing content",
			manifest: mb, doc: textOf(string(tvSignDoc(t, m, tvSigner{"k", "k"})) + `{"format_version":1}`), keys: k},
		{name: "an unknown algorithm is skipped, not fatal", source: tvSig + "TestVerifyManifestSignatureSkipsPastAnUnknownAlgorithm",
			manifest: mb, doc: doc(entry("pq-whatever-2031", "future", "AAAA") + "," + entry("ed25519", "k", sig("k"))), keys: k, wantOK: true},

		{name: "no trusted keys", source: "added: VerifyManifestSignature with an empty key list", manifest: mb, doc: good, keys: tvKeys()},
		{name: "the label is only a hint: another trusted key verifies", source: "added: signature.go orderKeys tries every key",
			manifest: mb, doc: doc(entry("ed25519", "release-2025", sig("release-2026"))), keys: tvKeys("release-2025", "release-2025", "release-2026", "release-2026"), wantOK: true},
		{name: "a label matching no trusted key still tries them all", source: "added: signature.go orderKeys",
			manifest: mb, doc: doc(entry("ed25519", "nobody", sig("release-2026"))), keys: r26, wantOK: true},
		{name: "an empty label tries every key", source: "added: signature.go orderKeys", manifest: mb, doc: doc(entry("ed25519", "", sig("release-2026"))), keys: r26, wantOK: true},
		{name: "an unlabelled trusted key verifies with an empty id", source: "added: TrustedKey.ID is optional", manifest: mb, doc: good, keys: tvKeys("", "release-2026"), wantOK: true},
		{name: "an unlabelled trusted key is named in the refusal", source: "added: SignaturePolicy.KeyIDs", manifest: mb, doc: good, keys: tvKeys("", "attacker", "old", "release-2025")},
		{name: "a malformed entry before a good one", source: "added: signature.go skips an unreadable entry", manifest: mb, doc: doc(entry("ed25519", "k", "!!!") + "," + entry("ed25519", "k", sig("k"))), keys: k, wantOK: true},
		{name: "a signature with surrounding space", source: "added: signature.go trims the signature", manifest: mb, doc: doc(entry("ed25519", "k", " "+sig("k")+" ")), keys: k, wantOK: true},
		{name: "a signature with an embedded newline", source: "added: Go base64 ignores CR and LF", manifest: mb, doc: textOf(`{"format_version":1,"signatures":[{"algorithm":"ed25519","key_id":"k","signature":"` + strings.ReplaceAll(withNewline, "\n", `\n`) + `"}]}`), keys: k, wantOK: true},
		{name: "a signature without its padding", source: "added: StdEncoding requires padding", manifest: mb, doc: doc(entry("ed25519", "k", strings.TrimRight(sig("k"), "="))), keys: k},
		{name: "a signature with a non-canonical scalar", source: "added: RFC 8032 S < L", manifest: mb, doc: doc(entry("ed25519", "k", tvNonCanonicalS(m, "k"))), keys: k},
		{name: "a 63-byte signature", source: "added: ed25519.SignatureSize", manifest: mb, doc: doc(entry("ed25519", "k", base64.StdEncoding.EncodeToString(make([]byte, 63)))), keys: k},
		{name: "a 65-byte signature", source: "added: ed25519.SignatureSize", manifest: mb, doc: doc(entry("ed25519", "k", base64.StdEncoding.EncodeToString(make([]byte, 65)))), keys: k},
		{name: "the algorithm name is case-sensitive", source: "added: signature.go compares the algorithm exactly", manifest: mb, doc: doc(entry("Ed25519", "k", sig("k"))), keys: k},
		{name: "json: field names match case-insensitively", source: "added: Go encoding/json", manifest: mb,
			doc: textOf(`{"FORMAT_VERSION":1,"Signatures":[{"ALGORITHM":"ed25519","Key_Id":"k","SIGNATURE":"` + sig("k") + `"}]}`), keys: k, wantOK: true},
		{name: "json: the Kelvin sign and the long s fold to k and s", source: "added: Go encoding/json folds with unicode.SimpleFold", manifest: mb,
			doc: textOf("{\"format_version\":1,\"\u017fignatures\":[{\"algorithm\":\"ed25519\",\"\u212aey_id\":\"k\",\"\u017fignature\":\"" + sig("k") + "\"}]}"), keys: k, wantOK: true},
		{name: "json: a null key id is an empty one", source: "added: Go encoding/json null handling", manifest: mb,
			doc: textOf(`{"format_version":1,"signatures":[{"algorithm":"ed25519","key_id":null,"signature":"` + sig("k") + `"}]}`), keys: k, wantOK: true},
		{name: "json: null signatures are none", source: "added: Go encoding/json null handling", manifest: mb, doc: textOf(`{"format_version":1,"signatures":null}`), keys: k},
		{name: "json: a missing format version is zero", source: "added: Go encoding/json leaves absent fields zero", manifest: mb, doc: textOf(`{"signatures":[` + entry("ed25519", "k", sig("k")) + `]}`), keys: k},
		{name: "json: a null document is the zero document", source: "added: Go encoding/json null handling", manifest: mb, doc: textOf(`null`), keys: k},
		{name: "json: a null document may be followed by a close brace", source: "added: json.Decoder ends a scalar at its last byte", manifest: mb, doc: textOf(`null}`), keys: k},
		{name: "json: a null document followed by a word is trailing content", source: "added: json.Decoder.More", manifest: mb, doc: textOf(`nullx`), keys: k},
		{name: "json: a string document", source: "added: Go encoding/json into a struct", manifest: mb, doc: textOf(`"a"x`), keys: k, prefix: jsonErr},
		{name: "json: a repeated format version takes the last", source: "added: Go encoding/json", manifest: mb,
			doc: textOf(`{"format_version":2,"FORMAT_VERSION":1,"signatures":[` + entry("ed25519", "k", sig("k")) + `]}`), keys: k, wantOK: true},
		{name: "json: a negative-zero format version is zero", source: "added: strconv.ParseInt", manifest: mb, doc: textOf(`{"format_version":-0,"signatures":[]}`), keys: k},
		{name: "json: a fractional format version", source: "added: Go encoding/json into int", manifest: mb, doc: textOf(`{"format_version":1.0,"signatures":[]}`), keys: k, prefix: jsonErr},
		{name: "json: a quoted format version", source: "added: Go encoding/json into int", manifest: mb, doc: textOf(`{"format_version":"1","signatures":[]}`), keys: k, prefix: jsonErr},
		{name: "json: an array document", source: "added: Go encoding/json into a struct", manifest: mb, doc: textOf(`[]`), keys: k, prefix: jsonErr},
		{name: "json: an empty document", source: "added: Go encoding/json Decoder at EOF", manifest: mb, doc: textOf(``), keys: k, prefix: jsonErr},
		{name: "json: a byte-order mark", source: "added: Go encoding/json has no BOM rule", manifest: mb, doc: textOf("\ufeff" + string(tvSignDoc(t, m, tvSigner{"k", "k"}))), keys: k, prefix: jsonErr},
		{name: "json: leading space", source: "added: Go encoding/json", manifest: mb, doc: textOf(" \n\t" + string(tvSignDoc(t, m, tvSigner{"k", "k"}))), keys: k, wantOK: true},
		{name: "json: a raw tab inside a string", source: "added: Go encoding/json rejects control characters in strings", manifest: mb, doc: doc(entry("ed25519", "k\t", sig("k"))), keys: k, prefix: jsonErr},
		{name: "json: a repeated signatures key merges into the earlier entries", source: "added: Go encoding/json decodes a repeated array into the existing elements", manifest: mb,
			doc: textOf(`{"format_version":1,"signatures":[{"algorithm":"ed25519","signature":"` + sig("k") + `"}],"signatures":[{"key_id":"k"}]}`), keys: k, wantOK: true},
		{name: "json: an empty repeat discards the earlier entries", source: "added: Go encoding/json replaces a slice with a fresh one for []", manifest: mb,
			doc: textOf(`{"format_version":1,"signatures":[{"algorithm":"ed25519","signature":"` + sig("k") + `"}],"signatures":[],"signatures":[{"key_id":"k"}]}`), keys: k},
		{name: "json: a shorter repeat re-exposes a stale entry when it grows again", source: "added: Go encoding/json reuses slice capacity", manifest: mb,
			doc: textOf(`{"format_version":1,"signatures":[{"algorithm":"x"},{"algorithm":"ed25519","signature":"` + sig("k") + `"}],"signatures":[{}],"signatures":[{},{}]}`), keys: k, wantOK: true},
		{name: "json: a null repeat discards the earlier entries", source: "added: Go encoding/json sets a slice to nil for null", manifest: mb,
			doc: textOf(`{"format_version":1,"signatures":[{"algorithm":"x"},{"algorithm":"ed25519","signature":"` + sig("k") + `"}],"signatures":null,"signatures":[{},{}]}`), keys: k},
		{name: "json: a null entry keeps its slot", source: "added: Go encoding/json null into a struct element", manifest: mb,
			doc: textOf(`{"format_version":1,"signatures":[null,` + entry("ed25519", "k", sig("k")) + `]}`), keys: k, wantOK: true},
		{name: "json: a trailing close brace is not trailing content", source: "added: json.Decoder.More reports false before ] and }", manifest: mb, doc: textOf(string(tvSignDoc(t, m, tvSigner{"k", "k"})) + "}"), keys: k, wantOK: true},
		{name: "json: a trailing close bracket is not trailing content", source: "added: json.Decoder.More", manifest: mb, doc: textOf(string(tvSignDoc(t, m, tvSigner{"k", "k"})) + " ]]"), keys: k, wantOK: true},
		{name: "json: trailing space is not trailing content", source: "added: json.Decoder.More", manifest: mb, doc: textOf(string(tvSignDoc(t, m, tvSigner{"k", "k"})) + " \n"), keys: k, wantOK: true},
		{name: "json: a trailing word is trailing content", source: "added: json.Decoder.More", manifest: mb, doc: textOf(string(tvSignDoc(t, m, tvSigner{"k", "k"})) + " x"), keys: k},
		{name: "json: invalid UTF-8 in a label is replaced, not refused", source: "added: Go encoding/json coerces strings to UTF-8", manifest: mb,
			doc: bytesOf([]byte(`{"format_version":1,"signatures":[{"algorithm":"ed25519","key_id":"k` + "\xff\xe2\x82" + `","signature":"` + sig("k") + `"}]}`)), keys: k, wantOK: true},
		{name: "json: a lone surrogate in a label is replaced, not refused", source: "added: Go encoding/json", manifest: mb,
			doc: textOf(`{"format_version":1,"signatures":[{"algorithm":"ed25519","key_id":"\ud800","signature":"` + sig("k") + `"}]}`), keys: k, wantOK: true},
		{name: "json: an unknown field nested in an entry", source: "added: DisallowUnknownFields applies at every level", manifest: mb,
			doc: textOf(`{"format_version":1,"signatures":[{"algorithm":"ed25519","key_id":"k","signature":"` + sig("k") + `","x":1}]}`), keys: k, prefix: jsonErr},
		{name: "json: nesting past Go's depth limit", source: "added: Go encoding/json refuses more than 10000 levels", manifest: mb, doc: deep(10001), keys: k, prefix: jsonErr},
		{name: "json: nesting at Go's depth limit is a type error", source: "added: Go encoding/json", manifest: mb, doc: deep(9999), keys: k, prefix: jsonErr},
		{name: "json: a million open brackets", source: "added: the scan must not recurse on attacker-shaped input", manifest: mb,
			doc: &bytesSpec{Segments: []bytesSegment{{Text: `{"format_version":1,"signatures":`, Count: 1}, {Text: "[", Count: 1 << 20}}}, keys: k, prefix: jsonErr},
	}
}

// ── kind: evidence ───────────────────────────────────────────────────────────

type evidenceCase struct {
	name, source string
	version      *string
	responses    map[string]assetResponse
	prefix       string
}

func evidenceCases(t *testing.T) []evidenceCase {
	t.Helper()
	m := []byte(`{"version":"0.3.0"}`)
	sigDoc := []byte(`{"format_version":1,"signatures":[]}`)
	both := map[string]assetResponse{tvSigURL: tvOK(sigDoc), tvManifestURL: tvOK(m)}
	big := func(n int) assetResponse {
		return assetResponse{Status: 200, Body: &bytesSpec{Segments: []bytesSegment{{Text: "a", Count: n}}}}
	}
	cases := []evidenceCase{
		{name: "both assets present", source: tvSource + "TestEvidenceFetchesBothAssets", version: strptr("0.3.0"), responses: both},
		{name: "no signature asset is a definitive absence", source: tvSource + "TestEvidenceNoSignatureAssetIsDefinitiveAbsence", version: strptr("0.3.0"),
			responses: map[string]assetResponse{tvManifestURL: tvOK(m)}},
		{name: "a 503 is not an absence", source: tvSource + "TestEvidenceServerErrorIsNotAbsence", version: strptr("0.3.0"),
			responses: map[string]assetResponse{tvSigURL: tvStatus(503)}},
		{name: "a signature with no manifest is a failure", source: tvSource + "TestEvidenceSignatureWithoutManifestIsAFailure", version: strptr("0.3.0"),
			responses: map[string]assetResponse{tvSigURL: tvOK(sigDoc)}},
		{name: "an unreachable source is a failure", source: tvSource + "TestEvidenceUnreachableSourceIsAFailure (a dropped connection here)", version: strptr("0.3.0"),
			responses: map[string]assetResponse{tvSigURL: tvDrop()}, prefix: "fetching " + tvSigURL + ": "},
		{name: "no version is an absence", source: tvSource + "TestEvidenceNoVersionIsAbsence (nil)", version: nil, responses: both},
		{name: "an empty version is an absence", source: tvSource + "TestEvidenceNoVersionIsAbsence (\"\")", version: strptr(""), responses: both},
		{name: "a blank version is an absence", source: tvSource + "TestEvidenceNoVersionIsAbsence (\"  \")", version: strptr("  "), responses: both},
		{name: "a prerelease version is fetchable", source: tvSource + "TestEvidenceRefusesAVersionItWillNotPutInAURL (prerelease)", version: strptr("0.3.0-rc.1"),
			responses: map[string]assetResponse{tvURL("0.3.0-rc.1", SignatureAssetName): tvOK(sigDoc), tvURL("0.3.0-rc.1", ManifestAssetName): tvOK(m)}},

		{name: "a padded version is trimmed before the URL", source: "added: signature_source.go trims the version", version: strptr(" 0.3.0\n"), responses: both},
		{name: "a non-breaking space is trimmed too", source: "added: strings.TrimSpace is Unicode-aware", version: strptr("0.3.0\u00a0"), responses: both},
		{name: "only a 404 is an absence: 410", source: "added: signature_source.go", version: strptr("0.3.0"), responses: map[string]assetResponse{tvSigURL: tvStatus(410)}},
		{name: "only a 404 is an absence: a redirect with nowhere to go", source: "added: net/http returns a 3xx with no Location", version: strptr("0.3.0"), responses: map[string]assetResponse{tvSigURL: tvStatus(302)}},
		{name: "a manifest 404 after a signature is a failure", source: "added: signature_source.go", version: strptr("0.3.0"),
			responses: map[string]assetResponse{tvSigURL: tvOK(sigDoc), tvManifestURL: tvStatus(404)}},
		{name: "an absent signature is decided before the manifest is fetched", source: "added: signature_source.go fetches the signature first", version: strptr("0.3.0"),
			responses: map[string]assetResponse{tvSigURL: tvStatus(404), tvManifestURL: tvStatus(503)}},
		{name: "a manifest outage is a failure", source: "added: signature_source.go", version: strptr("0.3.0"),
			responses: map[string]assetResponse{tvSigURL: tvOK(sigDoc), tvManifestURL: tvStatus(503)}},
		{name: "a dropped manifest connection is a failure", source: "added: signature_source.go", version: strptr("0.3.0"),
			responses: map[string]assetResponse{tvSigURL: tvOK(sigDoc), tvManifestURL: tvDrop()}, prefix: "fetching " + tvManifestURL + ": "},
		{name: "an oversized signature is a failure", source: "added: signature_source.go MaxAssetBytes", version: strptr("0.3.0"),
			responses: map[string]assetResponse{tvSigURL: big(MaxAssetBytes + 1), tvManifestURL: tvOK(m)}},
		{name: "an oversized manifest is a failure", source: "added: signature_source.go MaxAssetBytes", version: strptr("0.3.0"),
			responses: map[string]assetResponse{tvSigURL: tvOK(sigDoc), tvManifestURL: big(MaxAssetBytes + 1)}},
		{name: "an asset of exactly the limit is read", source: "added: signature_source.go MaxAssetBytes", version: strptr("0.3.0"),
			responses: map[string]assetResponse{tvSigURL: big(MaxAssetBytes), tvManifestURL: tvOK(m)}},
		{name: "an empty signature asset is evidence", source: "added: signature_source.go (the gate, not the fetch, grades it)", version: strptr("0.3.0"),
			responses: map[string]assetResponse{tvSigURL: tvOK(nil), tvManifestURL: tvOK(m)}},
		{name: "a build suffix is not a release version", source: "added: signature_source.go versionRe", version: strptr("1.2.3+build"), responses: both},
		{name: "an empty prerelease is not a release version", source: "added: signature_source.go versionRe", version: strptr("1.2.3-"), responses: both},
		{name: "two components is not a release version", source: "added: signature_source.go versionRe", version: strptr("1.2"), responses: both},
		{name: "0.0.0 is a release version", source: "added: signature_source.go versionRe", version: strptr("0.0.0"),
			responses: map[string]assetResponse{tvURL("0.0.0", SignatureAssetName): tvOK(sigDoc), tvURL("0.0.0", ManifestAssetName): tvOK(m)}},
	}
	for _, v := range []string{"../../etc/passwd", "0.3.0/../..", "0.3.0?x=1", "latest", "v0.3.0", "0.3.0 0.4.0", "0.3.0%2f", "01.2.3"} {
		cases = append(cases, evidenceCase{name: "refused version " + v, source: tvSource + "TestEvidenceRefusesAVersionItWillNotPutInAURL", version: strptr(v), responses: both})
	}
	return cases
}

// ── kind: evidence_gate ──────────────────────────────────────────────────────

type gateCase struct {
	name, source, mode, inFlight, requestID string
}

func gateCases() []gateCase {
	return []gateCase{
		{"off fetches nothing", tvSigServer + "TestServerFetchesNothingWhenSigningIsOff", SignatureModeOff, "", reqID},
		{"verify fetches", tvSigServer + "TestServerAcceptsAMissingSignatureUnderVerify", SignatureModeVerify, "", reqID},
		{"require fetches", tvSigServer + "TestServerRefusesAMissingSignatureUnderRequire", SignatureModeRequire, "", reqID},
		{"busy fetches nothing", "added: server.go signatureEvidence answers busy without a network call", SignatureModeRequire, otherID, reqID},
		{"re-posting the in-flight id fetches", "added: server.go signatureEvidence", SignatureModeRequire, reqID, reqID},
		{"off and busy fetches nothing", "added: server.go signatureEvidence", SignatureModeOff, otherID, reqID},
	}
}

// ── kind: config ─────────────────────────────────────────────────────────────

type configCase struct {
	name, source, parse, raw string
	wantOK                   bool
	prefix                   string
}

func configCases() []configCase {
	mode := func(name, source, raw string, ok bool) configCase {
		return configCase{name: name, source: source, parse: "signature_mode", raw: raw, wantOK: ok}
	}
	keys := func(name, source, raw string, ok bool) configCase {
		return configCase{name: name, source: source, parse: "trusted_keys", raw: raw, wantOK: ok}
	}
	ns := func(name, source, raw string) configCase {
		return configCase{name: name, source: source, parse: "allowed_namespaces", raw: raw, wantOK: true}
	}
	base := func(name, source, raw string, ok bool, prefix string) configCase {
		return configCase{name: name, source: source, parse: "manifest_base_url", raw: raw, wantOK: ok, prefix: prefix}
	}
	short := base64.StdEncoding.EncodeToString([]byte("short"))
	long := base64.StdEncoding.EncodeToString(make([]byte, 33))
	var out []configCase
	for _, raw := range []string{"", "off", " OFF ", "verify", "require", "Require"} {
		out = append(out, mode("mode "+tvQuote(raw), tvSig+"TestParseSignatureMode", raw, true))
	}
	for _, raw := range []string{"requre", "on", "1", "true", "strict"} {
		out = append(out, mode("mode "+tvQuote(raw)+" is loud", tvSig+"TestParseSignatureMode", raw, false))
	}
	out = append(out,
		mode("mode with a tab and a newline", "added: strings.TrimSpace", "\tverify\n", true),
		mode("mode with a dotted capital I lowers to require", "added: strings.ToLower maps U+0130 to i", "REQU\u0130RE", true),
		mode("mode with a non-breaking space", "added: strings.TrimSpace is Unicode-aware", "\u00a0require\u00a0", true),

		keys("two labelled keys", tvSig+"TestParseTrustedKeys", "old:{public_key_of:release-2025}, new:{public_key_of:release-2026}", true),
		keys("an unlabelled key", tvSig+"TestParseTrustedKeys", "{public_key_of:release-2025}", true),
		keys("the same key twice is one trust decision", tvSig+"TestParseTrustedKeys", "a:{public_key_of:release-2025},b:{public_key_of:release-2025}", true),
		keys("blank is no keys", tvSig+"TestParseTrustedKeys", "  ", true),
		keys("not base64", tvSig+"TestParseTrustedKeys", "nope", false),
		keys("too short", tvSig+"TestParseTrustedKeys", "k:"+short, false),
		keys("one byte too long", tvSig+"TestParseTrustedKeys", "k:"+long, false),
		keys("empty entries are skipped", "added: ParseTrustedKeys", " , ,{public_key_of:release-2025},", true),
		keys("the label and key are trimmed", "added: ParseTrustedKeys", "  old key : {public_key_of:release-2025}  ", true),
		keys("an empty label", "added: ParseTrustedKeys", ":{public_key_of:release-2025}", true),
		keys("the first colon is the separator", "added: ParseTrustedKeys", "a:b:{public_key_of:release-2025}", false),
		keys("unpadded base64", "added: StdEncoding requires padding", "k:"+strings.TrimRight(base64.StdEncoding.EncodeToString(make([]byte, 32)), "="), false),
		keys("an unlabelled bad key is named by its first twelve characters", "added: ParseTrustedKeys labelOf", short+short+short, false),
		keys("one bad key fails the whole list", "added: ParseTrustedKeys", "{public_key_of:release-2025},k:"+short, false),

		ns("two namespaces, trimmed", tvPlan+"TestParseNamespaces", " ghcr.io/a/b/ , ghcr.io/c/d "),
		ns("unset is the org default", tvPlan+"TestUnsetNamespaceKnobIsTheOrgDefault", ""),
		ns("blank is the org default", tvPlan+"TestUnsetNamespaceKnobIsTheOrgDefault", "  "),
		ns("a lone comma is the org default", tvPlan+"TestUnsetNamespaceKnobIsTheOrgDefault", ","),
		ns("a spaced comma is the org default", tvPlan+"TestUnsetNamespaceKnobIsTheOrgDefault", " , "),
		ns("every trailing slash is trimmed", "added: ParseNamespaces", "registry.example.invalid/x//"),
		ns("a lone slash is nothing", "added: ParseNamespaces", "/"),
		ns("case is kept", "added: ParseNamespaces", "GHCR.io/Accreleus"),

		base("blank is the org's releases", tvSig+"TestParseManifestBaseURL", "", true, ""),
		base("a missing trailing slash is added", tvSig+"TestParseManifestBaseURL", "https://mirror.example/quasar/{version}", true, ""),
		base("http is refused", tvSig+"TestParseManifestBaseURL", "http://mirror.example/{version}/", false, ""),
		base("no placeholder is refused", tvSig+"TestParseManifestBaseURL", "https://mirror.example/releases/", false, ""),
		base("ftp is refused", tvSig+"TestParseManifestBaseURL", "ftp://mirror.example/{version}/", false, ""),
		base("a relative URL has no host", tvSig+"TestParseManifestBaseURL", "/relative/{version}/", false, ""),
		base("an uppercase scheme is https", "added: url.Parse lowercases the scheme", "HTTPS://mirror.example/{version}/", true, ""),
		base("surrounding space is trimmed", "added: ParseManifestBaseURL", "  https://mirror.example/{version}/ \n", true, ""),
		base("userinfo is allowed", "added: url.Parse", "https://user@mirror.example/{version}/", true, ""),
		base("a port is allowed", "added: url.Parse", "https://mirror.example:8443/{version}/", true, ""),
		base("an empty port is allowed", "added: url.Parse validOptionalPort", "https://mirror.example:/{version}/", true, ""),
		base("the placeholder may sit in the query", "added: ParseManifestBaseURL only needs the placeholder somewhere", "https://mirror.example/?v={version}", true, ""),
		base("an IPv6 literal", "added: url.Parse parseHost", "https://[2001:db8::1]/{version}/", true, ""),
		base("a scheme with no slashes has no host", "added: url.Parse opaque", "https:mirror.example/{version}/", false, ""),
		base("an empty authority has no host", "added: url.Parse", "https:///{version}/", false, ""),
		base("userinfo with no host has no host", "added: url.Parse", "https://user@/{version}/", false, ""),
		base("a non-numeric port", "added: url.Parse validOptionalPort", "https://mirror.example:abc/{version}/", false, ""),
		base("a space in the host", "added: url.Parse host characters", "https://mirror example/{version}/", false, ""),
		base("a pipe in the host", "added: url.Parse host characters", "https://mirror|x.example/{version}/", false, ""),
		base("a bad escape in the path", "added: url.Parse path escapes", "https://mirror.example/{version}/%zz", false, ""),
		base("an escaped ASCII byte in the host", "added: url.Parse host escapes", "https://mirror%2eexample/{version}/", false, ""),
		base("a control byte", "added: url.Parse control characters", "https://mirror.example/{version}/\x7f", false, ""),
		base("a bad escape in the fragment", "added: url.Parse fragment escapes", "https://mirror.example/{version}/#%zz", false, ""),
		base("a leading colon", "added: url.Parse missing scheme", "://mirror.example/{version}/", false, ""),
		base("an IPv4 address in brackets", "added: url.Parse parseHost", "https://[192.0.2.1]/{version}/", false, ""),
		base("a bracket inside the host", "added: url.Parse parseHost", "https://mirror[1]/{version}/", false, ""),
		base("an unclosed bracket", "added: url.Parse parseHost", "https://[2001:db8::1/{version}/", false, ""),
		base("brackets around a non-address", "added: url.Parse parseHost (net/netip text is not pinned)", "https://[mirror]/{version}/", false,
			`"https://[mirror]/{version}/" is not a URL: parse "https://[mirror]/0.0.0/": invalid host: `),
		base("a first segment with a colon and no scheme", "added: url.Parse", "1a:b/{version}/", false, ""),
	)
	return out
}

func tvQuote(s string) string { b, _ := json.Marshal(s); return string(b) }

// ── kind: redirect ───────────────────────────────────────────────────────────

type redirectCase struct {
	name, source string
	via          []string
	next         string
}

func redirectCases() []redirectCase {
	from := tvSigURL
	storage := "https://storage.example.invalid/asset"
	tenHops := make([]string, 10)
	for i := range tenHops {
		tenHops[i] = from
	}
	return []redirectCase{
		{"https to https is followed", "added: signature_source.go client CheckRedirect", []string{from}, storage},
		{"https to http is refused", "added: signature_source.go client CheckRedirect (a plaintext hop could forge a 404)", []string{from}, "http://storage.example.invalid/asset"},
		{"https to an uppercase HTTP is refused", "added: the scheme is compared after url.Parse lowercases it", []string{from}, "HTTP://storage.example.invalid/asset"},
		{"a later hop off TLS is refused", "added: the rule is about the first request's scheme", []string{from, storage}, "http://storage.example.invalid/asset"},
		{"nine hops are followed", "added: the ten-redirect bound", tenHops[:9], storage},
		{"the tenth redirect is refused", "added: the ten-redirect bound", tenHops, storage},
		{"a plaintext origin may redirect to plaintext", "added: only an https origin is held to https (the base URL is always https)", []string{"http://mirror.example.invalid/a"}, "http://storage.example.invalid/asset"},
	}
}

// ── the generator ────────────────────────────────────────────────────────────

func generateTrustVectors(t *testing.T) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	put := func(name, kind, about string, vectors []any) {
		raws := make([]json.RawMessage, 0, len(vectors))
		for _, v := range vectors {
			raws = append(raws, encodeCanonical(t, v))
		}
		files[name] = append(encodeCanonical(t, vectorFile{Kind: kind, About: about, Vectors: raws}), '\n')
	}

	// admit
	var admits []any
	for _, c := range admitCases(t) {
		caller := "control_plane"
		if c.agent {
			caller = "agent"
		}
		cfg := c.cfg
		if cfg.TrustedKeys == nil {
			cfg.TrustedKeys = []vectorKey{}
		}
		v := admitVector{Name: c.name, Source: c.source, Caller: caller, Config: cfg, Request: c.req, Evidence: c.evidence, Fetch: c.fetch}
		got := goAdmit(t, v)
		if got.Reason != c.want || got.Admitted != (c.want == "") {
			t.Fatalf("admit %q: Go answered %s, the case says %q", c.name, mustJSON(got), c.want)
		}
		if c.prefix != "" {
			if !strings.HasPrefix(got.Message, c.prefix) {
				t.Fatalf("admit %q: message %q does not start with %q", c.name, got.Message, c.prefix)
			}
			got.Message, got.MessagePrefix = "", c.prefix
		}
		if c.agent {
			withoutGuard := got
			v.ExpectWithoutCallerGuard = &withoutGuard
			v.Expect = got
			if c.agentExpect != nil {
				e := *c.agentExpect
				e.Fetched = got.Fetched
				v.Expect = e
			}
		} else {
			v.Expect = got
		}
		admits = append(admits, v)
	}
	put("admit.json", kindAdmit,
		"The request gates of plan.go Plan (uuid, single flight, the closed component table, image and digest rules, the namespace allowlist, in that order) followed by the ADR 0003 signature gate of signature.go checkSignature. `fetch` vectors gather evidence the way server.go signatureEvidence does.",
		admits)

	// verify_signature
	var verifies []any
	for _, c := range verifyCases(t) {
		keys := c.keys
		if keys == nil {
			keys = []vectorKey{}
		}
		v := verifyVector{Name: c.name, Source: c.source, Manifest: *c.manifest, Document: *c.doc, TrustedKeys: keys}
		got := goVerify(t, v)
		if got.Verified != c.wantOK {
			t.Fatalf("verify %q: Go answered %s, the case says verified=%v", c.name, mustJSON(got), c.wantOK)
		}
		if c.prefix != "" {
			if !strings.HasPrefix(got.Error, c.prefix) {
				t.Fatalf("verify %q: error %q does not start with %q", c.name, got.Error, c.prefix)
			}
			got.Error, got.ErrorPrefix = "", c.prefix
		}
		v.Expect = got
		verifies = append(verifies, v)
	}
	put("verify_signature.json", kindVerifySignature,
		"signature.go VerifyManifestSignature: the detached document parsed exactly as Go's encoding/json parses it, ed25519 over the manifest bytes, any entry under any trusted key.",
		verifies)

	// evidence
	var evs []any
	for _, c := range evidenceCases(t) {
		v := evidenceVector{Name: c.name, Source: c.source, Version: c.version, Fetch: fetchSpec{BaseURL: tvBase, Responses: c.responses}}
		got := goEvidence(t, v)
		// Keep a large body in its compact spelling.
		for _, r := range c.responses {
			if r.Body != nil && r.Body.Segments != nil {
				if got.Evidence.Signature != nil && bytes.Equal(got.Evidence.Signature.bytes(t), r.Body.bytes(t)) {
					got.Evidence.Signature = r.Body
				}
			}
		}
		if c.prefix != "" {
			if !strings.HasPrefix(got.Evidence.Error, c.prefix) {
				t.Fatalf("evidence %q: error %q does not start with %q", c.name, got.Evidence.Error, c.prefix)
			}
			got.Evidence.Error, got.Evidence.ErrorPrefix = "", c.prefix
		}
		v.Expect.Evidence, v.Expect.Fetched = got.Evidence, got.Fetched
		evs = append(evs, v)
	}
	put("evidence.json", kindEvidence,
		"signature_source.go ReleaseAssetSource.Evidence: how HTTP outcomes map onto signed / absent / fetch_error. Only a 404 on the signature asset is an absence; anything that could not be completed is a fetch_error, never an absence.",
		evs)

	// evidence_gate
	var gates []any
	for _, c := range gateCases() {
		v := evidenceGateVector{Name: c.name, Source: c.source, SignatureMode: c.mode, InFlightRequestID: c.inFlight, RequestID: c.requestID}
		v.Expect.Fetches = goEvidenceGate(v)
		gates = append(gates, v)
	}
	put("evidence_gate.json", kindEvidenceGate,
		"server.go signatureEvidence: whether a request's release assets are fetched at all (only when a mode is enabled and the host is not busy with another request).",
		gates)

	// config
	var cfgs []any
	for _, c := range configCases() {
		v := configVector{Name: c.name, Source: c.source, Parse: c.parse, Raw: c.raw}
		got := goConfig(t, v)
		if got.OK != c.wantOK {
			t.Fatalf("config %q: Go answered %s, the case says ok=%v", c.name, mustJSON(got), c.wantOK)
		}
		if c.prefix != "" {
			if !strings.HasPrefix(got.Error, c.prefix) {
				t.Fatalf("config %q: error %q does not start with %q", c.name, got.Error, c.prefix)
			}
			got.Error, got.ErrorPrefix = "", c.prefix
		}
		v.Expect = got
		cfgs = append(cfgs, v)
	}
	put("config.json", kindConfig,
		"The operator knobs: ParseSignatureMode (QUASAR_UPDATER_SIGNATURE_MODE), ParseTrustedKeys (QUASAR_UPDATER_TRUSTED_KEYS; `{public_key_of:LABEL}` stands for a derived vector key), ParseNamespaces (QUASAR_UPDATER_ALLOWED_NAMESPACES) and ParseManifestBaseURL (QUASAR_UPDATER_MANIFEST_BASE_URL).",
		cfgs)

	// redirect
	var reds []any
	for _, c := range redirectCases() {
		v := redirectVector{Name: c.name, Source: c.source, Via: c.via, Next: c.next}
		ok, msg := goRedirect(t, v)
		v.Expect.Allowed, v.Expect.Error = ok, msg
		reds = append(reds, v)
	}
	put("redirect.json", kindRedirect,
		"signature_source.go's redirect policy: follow at most ten redirects, and never from an https origin to anything but https.",
		reds)
	return files
}

// encodeCanonical is two-space indented JSON with no HTML escaping, so the
// files read as the bytes they describe.
func encodeCanonical(t *testing.T, v any) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}

// TestTrustVectorsAreCurrent regenerates every file from the table and fails
// on any difference, so the files can never drift from Go's behaviour or from
// the table. QUASAR_WRITE_TRUST_VECTORS=1 rewrites them instead.
func TestTrustVectorsAreCurrent(t *testing.T) {
	files := generateTrustVectors(t)
	if os.Getenv("QUASAR_WRITE_TRUST_VECTORS") == "1" {
		if err := os.MkdirAll(trustVectorDir, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, body := range files {
			if err := os.WriteFile(filepath.Join(trustVectorDir, name), body, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		t.Logf("wrote %d vector files to %s", len(files), trustVectorDir)
	}
	onDisk := readVectorFiles(t)
	for name, want := range files {
		got, err := os.ReadFile(filepath.Join(trustVectorDir, name))
		if err != nil {
			t.Fatalf("%s: %v (regenerate with QUASAR_WRITE_TRUST_VECTORS=1)", name, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s is stale: regenerate with QUASAR_WRITE_TRUST_VECTORS=1 go test ./internal/updater -run TestTrustVectorsAreCurrent", name)
		}
	}
	for name := range onDisk {
		if _, ok := files[name]; !ok {
			t.Errorf("%s is not generated by the table: delete it or add its cases", name)
		}
	}
}
