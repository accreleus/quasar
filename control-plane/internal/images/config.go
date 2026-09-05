package images

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Reading an image's OCI config labels (#111), for the edge release channel:
// a branch build ships no manifest asset, so its identity is the org.quasar.*
// labels on the images. It lives beside the digest resolver because it is the
// same client — one place for the ref parsing, allowlist and token flow, so the
// SSRF containment is not almost-right in two.

const (
	// Bounds one image's whole inspection (up to four round trips); longer than
	// digestResolveTimeout because this runs on a weekly job, not in a request.
	inspectTimeout = 20 * time.Second

	// The architecture an index resolves to. A single-manifest index is taken
	// as-is, so this is never a guess for a single-arch publish.
	inspectOS   = "linux"
	inspectArch = "amd64"
)

// ImageConfig is what an inspection reads back.
type ImageConfig struct {
	// ManifestDigest is what the tag stood for at this instant.
	ManifestDigest string
	// Labels is the image config's label map, empty when the image carries none.
	Labels map[string]string
}

// Label returns one trimmed label value, "" when absent.
func (c ImageConfig) Label(name string) string { return strings.TrimSpace(c.Labels[name]) }

// ImageInspector is what a caller depends on; RegistryResolver implements it.
type ImageInspector interface {
	InspectConfig(ctx context.Context, ref string) (ImageConfig, error)
}

// descriptor is the subset of an OCI descriptor this consumer reads.
type descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Platform  *struct {
		Architecture string `json:"architecture"`
		OS           string `json:"os"`
	} `json:"platform"`
}

// manifestDoc covers both shapes one GET can answer with: the media type is not
// reliable across registries, the content distinguishes them.
type manifestDoc struct {
	Manifests []descriptor `json:"manifests"`
	Config    descriptor   `json:"config"`
}

// configBlob is the image config document; only its labels are read.
type configBlob struct {
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"config"`
}

// InspectConfig resolves ref (tag or digest) to its config labels and the
// manifest digest. An error means the identity is unknown: skip, never guess.
func (r *RegistryResolver) InspectConfig(ctx context.Context, ref string) (ImageConfig, error) {
	if strings.Contains(ref, "://") {
		return ImageConfig{}, fmt.Errorf("image ref %q must not carry a URL scheme (https is implied)", ref)
	}
	p, err := parseRef(ref)
	if err != nil {
		return ImageConfig{}, err
	}
	// Same containment as Resolve: the allowlist is checked on the parsed ref
	// before any request is built.
	if !r.hostAllowed(p.Registry) {
		return ImageConfig{}, fmt.Errorf("registry host %q is not in the allowlist (QUASAR_IMAGE_REGISTRY_HOSTS)", p.Registry)
	}

	ctx, cancel := context.WithTimeout(ctx, inspectTimeout)
	defer cancel()

	token := ""
	if p.apiHost() == "ghcr.io" {
		token, _ = r.fetchToken(ctx, "https://ghcr.io/token", "ghcr.io", p.scope())
	}

	reference := p.Tag
	if p.Digest != "" {
		reference = p.Digest
	}
	body, digest, err := r.getManifest(ctx, p, reference, &token)
	if err != nil {
		return ImageConfig{}, err
	}
	var doc manifestDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return ImageConfig{}, fmt.Errorf("decode manifest of %s: %w", ref, err)
	}
	// Descend an index exactly once; a nested index is not followed.
	if len(doc.Manifests) > 0 {
		child, err := pickManifest(doc.Manifests)
		if err != nil {
			return ImageConfig{}, fmt.Errorf("%s: %w", ref, err)
		}
		body, _, err = r.getManifest(ctx, p, child, &token)
		if err != nil {
			return ImageConfig{}, err
		}
		doc = manifestDoc{}
		if err := json.Unmarshal(body, &doc); err != nil {
			return ImageConfig{}, fmt.Errorf("decode manifest %s of %s: %w", child, ref, err)
		}
		if len(doc.Manifests) > 0 {
			return ImageConfig{}, fmt.Errorf("%s: manifest index points at another index", ref)
		}
	}
	if !digestRe.MatchString(doc.Config.Digest) {
		return ImageConfig{}, fmt.Errorf("%s: manifest config digest %q is not sha256:<64 hex>", ref, doc.Config.Digest)
	}

	blob, err := r.getBlob(ctx, p, doc.Config.Digest, &token)
	if err != nil {
		return ImageConfig{}, err
	}
	var cfg configBlob
	if err := json.Unmarshal(blob, &cfg); err != nil {
		return ImageConfig{}, fmt.Errorf("decode image config of %s: %w", ref, err)
	}
	labels := cfg.Config.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	return ImageConfig{ManifestDigest: digest, Labels: labels}, nil
}

// pickManifest takes the linux/amd64 entry, or the only candidate. An
// attestation manifest carries an "unknown" platform and is skipped.
func pickManifest(all []descriptor) (string, error) {
	var candidates []descriptor
	for _, d := range all {
		if !digestRe.MatchString(d.Digest) {
			continue
		}
		if d.Platform != nil && (d.Platform.OS == "unknown" || d.Platform.Architecture == "unknown") {
			continue
		}
		candidates = append(candidates, d)
	}
	for _, d := range candidates {
		if d.Platform != nil && d.Platform.OS == inspectOS && d.Platform.Architecture == inspectArch {
			return d.Digest, nil
		}
	}
	if len(candidates) == 1 {
		return candidates[0].Digest, nil
	}
	return "", fmt.Errorf("manifest index carries no %s/%s manifest", inspectOS, inspectArch)
}

// getManifest returns the body and its digest, COMPUTED from the body: a
// registry must not decide the one value here that has to be right.
func (r *RegistryResolver) getManifest(ctx context.Context, p parsedRef, reference string, token *string) ([]byte, string, error) {
	url := fmt.Sprintf("%s/v2/%s/manifests/%s", r.base(p), p.Name, reference)
	body, err := r.registryGet(ctx, p, url, manifestAccept, token)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(body)
	return body, "sha256:" + hex.EncodeToString(sum[:]), nil
}

// getBlob GETs a config blob by digest.
func (r *RegistryResolver) getBlob(ctx context.Context, p parsedRef, digest string, token *string) ([]byte, error) {
	url := fmt.Sprintf("%s/v2/%s/blobs/%s", r.base(p), p.Name, digest)
	return r.registryGet(ctx, p, url, "application/octet-stream, application/json", token)
}

// registryGet does one GET, taking the bearer challenge once if asked.
func (r *RegistryResolver) registryGet(ctx context.Context, p parsedRef, url, accept string, token *string) ([]byte, error) {
	body, challenge, err := r.tryGet(ctx, url, accept, *token)
	if err != nil {
		return nil, err
	}
	if challenge == "" {
		return body, nil
	}
	realm, params := parseChallenge(challenge)
	if realm == "" {
		return nil, fmt.Errorf("registry %s: auth challenge without a realm", p.apiHost())
	}
	scope := params["scope"]
	if scope == "" {
		scope = p.scope()
	}
	fresh, err := r.fetchToken(ctx, realm, params["service"], scope)
	if err != nil {
		return nil, err
	}
	*token = fresh
	body, challenge, err = r.tryGet(ctx, url, accept, *token)
	if err != nil {
		return nil, err
	}
	if challenge != "" {
		return nil, fmt.Errorf("get %s: unauthorized after a token fetch", url)
	}
	return body, nil
}

// tryGet returns (body, "", nil) on success and (nil, challenge, nil) on a 401
// carrying one; every other status is an error, 404 included.
func (r *RegistryResolver) tryGet(ctx context.Context, url, accept, token string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", fmt.Errorf("build request %s: %w", url, err)
	}
	req.Header.Set("Accept", accept)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("get %s: %w", url, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode == http.StatusUnauthorized {
		if ch := resp.Header.Get("WWW-Authenticate"); ch != "" && token == "" {
			return nil, ch, nil
		}
		return nil, "", fmt.Errorf("get %s: unauthorized", url)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("get %s: status %d", url, resp.StatusCode)
	}
	// Repeated here because a caller-supplied *http.Client (tests) does not cap.
	body, err := io.ReadAll(io.LimitReader(resp.Body, registryMaxBodyBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("read %s: %w", url, err)
	}
	if int64(len(body)) > registryMaxBodyBytes {
		return nil, "", fmt.Errorf("get %s: response exceeds %d bytes", url, registryMaxBodyBytes)
	}
	return body, "", nil
}
