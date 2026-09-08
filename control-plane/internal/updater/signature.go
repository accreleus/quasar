// Release-signature verification: the second gate beside plan.go's namespace
// allowlist. Rationale and rotation: docs/adr/0003-release-signatures.md.
//
// What is signed is the release MANIFEST, not each image. The manifest names
// every component digest, so a signature over its bytes covers the images.
//
// Pure: the bytes are fetched by signature_source.go and handed in as evidence.
package updater

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// QUASAR_UPDATER_SIGNATURE_MODE. Default off: an install predating signing must
// not break, and nothing is signed until an operator adds a key to the release
// pipeline.
const (
	// Verifies nothing and fetches nothing: ADR 0001 trust, digest + namespace.
	SignatureModeOff = "off"
	// Refuses a bad signature, accepts a release that definitively has none.
	SignatureModeVerify = "verify"
	// Also refuses a release with no signature.
	SignatureModeRequire = "require"
)

// Two more of plan.go's reasons. Not in agent-api.md's `ApplyFailureReason`
// enum: they are emitted only once an operator enables verification, and the
// contract's rule for an identifier it does not know is to render it verbatim.
// Adding them to the enum is an additive amendment, sign-off gated.
const (
	ReasonSignatureMissing = "signature_missing"
	ReasonSignatureInvalid = "signature_invalid"
)

// The only algorithm this build verifies. The document is a list so a second
// one can be published beside it without a flag day.
const SignatureAlgorithm = "ed25519"

// The version of the `.sig` envelope, not of the manifest or the release.
const SignatureDocumentFormatVersion = 1

// The org's own releases. `{version}` is the only substitution, and the scheme
// and host are host-local config, never anything the request supplies.
const DefaultManifestBaseURL = "https://github.com/accreleus/quasar/releases/download/v{version}/"

// The two asset names, fixed: the manifest and its detached signature.
const (
	ManifestAssetName  = "platform-release-manifest.json"
	SignatureAssetName = "platform-release-manifest.json.sig"
)

// TrustedKey is one public key this host trusts. ID is a label and is never
// what makes a signature good: an attacker chooses the key_id in the document
// they hand you, not which keys are in this list.
type TrustedKey struct {
	ID  string
	Key ed25519.PublicKey
}

// SignaturePolicy is the host's whole answer to "must a release be signed".
type SignaturePolicy struct {
	Mode string
	Keys []TrustedKey
}

// Enabled reports whether anything is fetched or graded at all.
func (p SignaturePolicy) Enabled() bool {
	return p.Mode == SignatureModeVerify || p.Mode == SignatureModeRequire
}

// KeyIDs is what `/v1/self` reports: labels, never key material.
func (p SignaturePolicy) KeyIDs() []string {
	ids := make([]string, 0, len(p.Keys))
	for _, k := range p.Keys {
		id := k.ID
		if id == "" {
			id = "(unlabelled)"
		}
		ids = append(ids, id)
	}
	return ids
}

// SignatureEvidence is what the source found: a manifest+signature pair, a
// definitive absence, or a failure to determine. "Could not tell" must never
// fold into "absent" — a proxy eating the request would read as unsigned.
type SignatureEvidence struct {
	Manifest  []byte
	Signature []byte

	// Absent: there is definitively no signature to check, and Why says so.
	Absent bool
	Why    string

	// FetchError: the question could not be answered. Refused in both enabled
	// modes.
	FetchError string
}

// SignatureDocument is `platform-release-manifest.json.sig`, a detached
// signature over the manifest asset's exact bytes. Not a field inside the
// manifest: that would be a `format_version` bump every deployed consumer is
// required to refuse, and would need a canonicalisation rule.
type SignatureDocument struct {
	FormatVersion int              `json:"format_version"`
	Signatures    []SignatureEntry `json:"signatures"`
}

// SignatureEntry is one signature by one key. More than one entry is what makes
// rotation a period rather than a flag day: a release signed by the outgoing and
// the incoming key verifies on hosts that trust either.
type SignatureEntry struct {
	Algorithm string `json:"algorithm"`
	KeyID     string `json:"key_id"`
	Signature string `json:"signature"` // standard base64 of the raw signature
}

// A lenient read: only the fields bindManifest needs, unknown keys tolerated.
// The strict validator is the producer's. A future `format_version 2` that still
// carries `components` must keep verifying, or every bump is a fleet outage.
type signedManifest struct {
	Version    string `json:"version"`
	Components []struct {
		Name   string `json:"name"`
		Image  string `json:"image"`
		Digest string `json:"digest"`
	} `json:"components"`
}

// checkSignature is the gate; nil means the apply may proceed. The full
// mode/evidence matrix is docs/configuration.md §"Release signature
// verification".
func checkSignature(req ApplyRequest, pol SignaturePolicy, ev *SignatureEvidence) *Rejection {
	if !pol.Enabled() {
		return nil
	}
	// Fail closed: a host told to verify with nothing to verify against must
	// not accept, or the knob looks on while checking nothing.
	if len(pol.Keys) == 0 {
		return reject(ReasonSignatureInvalid,
			"signature mode is %q but this host trusts no release keys: set QUASAR_UPDATER_TRUSTED_KEYS (docs/upgrading.md)", pol.Mode)
	}
	if ev == nil {
		return reject(ReasonSignatureInvalid, "no signature evidence was gathered for this request")
	}
	if ev.FetchError != "" {
		return reject(ReasonSignatureInvalid,
			"the release signature could not be retrieved, so it could not be checked: %s", ev.FetchError)
	}
	if ev.Absent {
		if pol.Mode == SignatureModeRequire {
			return reject(ReasonSignatureMissing,
				"this host requires a signed release and this one carries no signature: %s", ev.Why)
		}
		return nil
	}

	keyID, err := VerifyManifestSignature(ev.Manifest, ev.Signature, pol.Keys)
	if err != nil {
		return reject(ReasonSignatureInvalid, "the release manifest signature did not verify: %v", err)
	}
	if err := bindManifest(ev.Manifest, req); err != nil {
		// Good signature over some manifest; not over this request.
		return reject(ReasonSignatureInvalid,
			"the signed release manifest (key %s) does not describe this request: %v", keyLabel(keyID), err)
	}
	return nil
}

// VerifyManifestSignature checks the detached document over the manifest bytes
// and returns the id of the trusted key that verified it. Any entry verifying
// under any trusted key is enough: the property comes from the private key, not
// the label. `key_id` only decides which key is tried first.
func VerifyManifestSignature(manifest, document []byte, keys []TrustedKey) (string, error) {
	if len(keys) == 0 {
		return "", fmt.Errorf("no trusted keys")
	}
	var doc SignatureDocument
	dec := json.NewDecoder(bytes.NewReader(document))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return "", fmt.Errorf("the signature document is not valid JSON in the documented shape: %w", err)
	}
	if dec.More() {
		return "", fmt.Errorf("the signature document carries trailing content after the object")
	}
	if doc.FormatVersion != SignatureDocumentFormatVersion {
		return "", fmt.Errorf("signature document format_version %d is not understood by this build (want %d)",
			doc.FormatVersion, SignatureDocumentFormatVersion)
	}
	if len(doc.Signatures) == 0 {
		return "", fmt.Errorf("the signature document carries no signatures")
	}

	usable := 0
	for _, entry := range doc.Signatures {
		// An unknown algorithm is skipped, not fatal, so a future one can be
		// published beside this one. `usable` catches "none were readable".
		if entry.Algorithm != SignatureAlgorithm {
			continue
		}
		sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(entry.Signature))
		if err != nil || len(sig) != ed25519.SignatureSize {
			continue
		}
		usable++
		for _, k := range orderKeys(keys, entry.KeyID) {
			if len(k.Key) == ed25519.PublicKeySize && ed25519.Verify(k.Key, manifest, sig) {
				return k.ID, nil
			}
		}
	}
	if usable == 0 {
		return "", fmt.Errorf("no %s signature in the document is well-formed", SignatureAlgorithm)
	}
	return "", fmt.Errorf("no signature was made by a key this host trusts (%s)",
		strings.Join(SignaturePolicy{Keys: keys}.KeyIDs(), ", "))
}

// orderKeys tries the label-matching key first. Every key is still tried.
func orderKeys(keys []TrustedKey, hint string) []TrustedKey {
	if hint == "" {
		return keys
	}
	out := make([]TrustedKey, 0, len(keys))
	for _, k := range keys {
		if k.ID == hint {
			out = append(out, k)
		}
	}
	if len(out) == 0 {
		return keys
	}
	for _, k := range keys {
		if k.ID != hint {
			out = append(out, k)
		}
	}
	return out
}

// bindManifest is what makes a verified signature mean something: the signed
// document must name the digests this request asks for. Without it one genuine
// release would launder any digest set. Guarded by
// TestCheckSignatureBindsTheManifestToTheRequest.
func bindManifest(raw []byte, req ApplyRequest) error {
	var m signedManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("the signed manifest is not readable JSON: %w", err)
	}
	if want := strings.TrimSpace(derefString(req.Release.Version)); want != "" && m.Version != want {
		return fmt.Errorf("it names version %q, the request names %q", m.Version, want)
	}
	if len(m.Components) == 0 {
		return fmt.Errorf("it names no components")
	}
	for _, c := range req.Components {
		found := false
		for _, mc := range m.Components {
			if mc.Name != c.Name {
				continue
			}
			found = true
			if mc.Image != c.Image || mc.Digest != c.Digest {
				return fmt.Errorf("component %q is %s@%s in the request and %s@%s in the manifest",
					c.Name, c.Image, c.Digest, mc.Image, mc.Digest)
			}
		}
		if !found {
			return fmt.Errorf("component %q is not in the manifest", c.Name)
		}
	}
	return nil
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func keyLabel(id string) string {
	if id == "" {
		return "(unlabelled)"
	}
	return id
}

// ── Configuration parsing ─────────────────────────────────────────────────────

// ParseSignatureMode reads QUASAR_UPDATER_SIGNATURE_MODE. Blank is off; an
// unrecognised value is an error, never a fallback to off — a typo must not
// quietly disable the gate.
func ParseSignatureMode(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", SignatureModeOff:
		return SignatureModeOff, nil
	case SignatureModeVerify:
		return SignatureModeVerify, nil
	case SignatureModeRequire:
		return SignatureModeRequire, nil
	default:
		return "", fmt.Errorf("%q is not a signature mode (want off, verify or require)", raw)
	}
}

// ParseTrustedKeys reads QUASAR_UPDATER_TRUSTED_KEYS: comma-separated
// `key-id:base64key` (or a bare `base64key`), each the raw 32 bytes of an
// ed25519 public key. Base64 has no colon, so the first colon is the separator.
// Several entries at once is how a rotation avoids a flag day.
func ParseTrustedKeys(raw string) ([]TrustedKey, error) {
	out := make([]TrustedKey, 0, 2)
	seen := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, encoded := "", part
		if i := strings.Index(part, ":"); i >= 0 {
			id, encoded = strings.TrimSpace(part[:i]), strings.TrimSpace(part[i+1:])
		}
		key, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("trusted key %q is not standard base64: %w", labelOf(id, encoded), err)
		}
		if len(key) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("trusted key %q decodes to %d bytes, not the %d of an ed25519 public key",
				labelOf(id, encoded), len(key), ed25519.PublicKeySize)
		}
		if seen[string(key)] {
			continue // the same key twice is a paste, not a second trust decision
		}
		seen[string(key)] = true
		out = append(out, TrustedKey{ID: id, Key: ed25519.PublicKey(key)})
	}
	return out, nil
}

func labelOf(id, encoded string) string {
	if id != "" {
		return id
	}
	if len(encoded) > 12 {
		return encoded[:12] + "…"
	}
	return encoded
}

// The version arrives over the wire and is concatenated into a URL path
// segment, so it must match strict semver before it ever gets there.
var versionRe = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?$`)

// ParseManifestBaseURL reads QUASAR_UPDATER_MANIFEST_BASE_URL; blank is the
// org's own releases. HTTPS only: over plaintext a network attacker could turn
// "signed" into "no signature published", which in `verify` mode is an accept.
func ParseManifestBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = DefaultManifestBaseURL
	}
	if !strings.Contains(raw, "{version}") {
		return "", fmt.Errorf("%q contains no {version} placeholder, so it cannot name a release's assets", raw)
	}
	if !strings.HasSuffix(raw, "/") {
		raw += "/"
	}
	probe, err := url.Parse(strings.ReplaceAll(raw, "{version}", "0.0.0"))
	if err != nil {
		return "", fmt.Errorf("%q is not a URL: %w", raw, err)
	}
	if probe.Scheme != "https" {
		return "", fmt.Errorf("%q is not https; the release manifest must be fetched over TLS", raw)
	}
	if probe.Host == "" {
		return "", fmt.Errorf("%q names no host", raw)
	}
	return raw, nil
}
