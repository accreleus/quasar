package updater

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// The updater fetches the manifest and its signature itself, from a host-local
// base URL, rather than taking them from whoever posted the apply: a signature
// relayed along the same path as the digests would be supplied by the party it
// is meant to constrain. Matched against the request by bindManifest, a
// compromised control plane can ask for a digest set and this host still
// installs only what a signed release names.
//
// THAT SENTENCE IS TRUE ONLY UNDER `require`. It is worth being exact, because
// the opposite belief is the dangerous one:
//
// The version whose signature we go looking for comes from the request, so the
// party being constrained chooses it. A request naming no version at all, or a
// version that was never published, produces "no signature exists" — and under
// `verify` that applies. A compromised control plane therefore bypasses
// `verify` completely, without touching the network, by simply not naming a
// published version. A null version is not even anomalous: edge releases send
// one (platform/detect.go) and so does a revert with no release id.
//
// So `verify` buys exactly one thing: it catches a SIGNED release that has been
// tampered with. It is a migration rung — it lets a fleet turn signing on while
// unsigned releases are still in flight — not an enforcement boundary.
// `require` is the enforcement boundary. Anything that reads otherwise is a
// documentation bug and should be fixed here first.
//
// It needs no contract change: `release_apply` already carries
// `release.version` (agent-api.md), which is all the URL needs.

// The manifest is under a kilobyte and the signature document under 512 bytes.
const MaxAssetBytes = 1 << 20

// Bounds both fetches together. The agent's socket call gives up at 30 s, so a
// slow mirror must not turn into `updater_absent`.
const DefaultAssetTimeout = 15 * time.Second

// ReleaseAssetSource fetches a release's manifest and signature over HTTPS.
type ReleaseAssetSource struct {
	// BaseURL is the ParseManifestBaseURL output: an https URL ending in `/`
	// with a `{version}` placeholder.
	BaseURL string
	Client  *http.Client
	Timeout time.Duration
}

// Evidence answers "is this release signed, and with what". It never returns an
// error: every outcome is one of the three evidence states, and the gate — not
// the fetch — decides whether the apply proceeds.
func (s ReleaseAssetSource) Evidence(ctx context.Context, version *string) SignatureEvidence {
	v := strings.TrimSpace(derefString(version))
	if v == "" {
		// An edge build, or a revert to one this instance can no longer name:
		// no published release, so nothing could have signed one.
		return SignatureEvidence{Absent: true,
			Why: "the request names no release version, so there is no published release manifest to verify against"}
	}
	// Never concatenate an unvalidated wire value into a URL path.
	if !versionRe.MatchString(v) {
		return SignatureEvidence{FetchError: fmt.Sprintf(
			"release version %q is not a semver release version; refusing to compose an asset URL from it", v)}
	}

	timeout := s.Timeout
	if timeout <= 0 {
		timeout = DefaultAssetTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	base := strings.ReplaceAll(s.BaseURL, "{version}", v)

	// The signature decides absence and so is fetched first: a release with no
	// signature asset is unsigned, whatever its manifest says.
	sig, status, err := s.get(ctx, base+SignatureAssetName)
	switch {
	case err != nil:
		return SignatureEvidence{FetchError: fmt.Sprintf("fetching %s: %v", base+SignatureAssetName, err)}
	case status == http.StatusNotFound:
		return SignatureEvidence{Absent: true,
			Why: fmt.Sprintf("release %s publishes no %s asset", v, SignatureAssetName)}
	case status != http.StatusOK:
		return SignatureEvidence{FetchError: fmt.Sprintf("%s answered HTTP %d", base+SignatureAssetName, status)}
	}

	manifest, status, err := s.get(ctx, base+ManifestAssetName)
	switch {
	case err != nil:
		return SignatureEvidence{FetchError: fmt.Sprintf("fetching %s: %v", base+ManifestAssetName, err)}
	case status != http.StatusOK:
		// A signature with no manifest beside it is a broken publish, never an
		// unsigned release: absence was ruled out above.
		return SignatureEvidence{FetchError: fmt.Sprintf(
			"release %s publishes a signature but %s answered HTTP %d", v, base+ManifestAssetName, status)}
	}

	return SignatureEvidence{Manifest: manifest, Signature: sig}
}

// A release asset URL redirects to the storage host, so redirects must be
// followed — but never off TLS. A plaintext hop is a place to forge a 404, and
// a 404 is what `verify` reads as "this release is unsigned".
func (s ReleaseAssetSource) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return fmt.Errorf("stopped after 10 redirects")
		}
		if len(via) > 0 && via[0].URL.Scheme == "https" && req.URL.Scheme != "https" {
			return fmt.Errorf("refusing a redirect from https to %s", req.URL.Scheme)
		}
		return nil
	}}
}

func (s ReleaseAssetSource) get(ctx context.Context, rawURL string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/octet-stream")
	req.Header.Set("User-Agent", "quasar-updater")

	resp, err := s.client().Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, MaxAssetBytes))
		return nil, resp.StatusCode, nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxAssetBytes+1))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if len(body) > MaxAssetBytes {
		return nil, resp.StatusCode, fmt.Errorf("asset is larger than %d bytes", MaxAssetBytes)
	}
	return body, resp.StatusCode, nil
}
