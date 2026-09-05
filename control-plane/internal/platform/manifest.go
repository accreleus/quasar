package platform

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// `platform-release-manifest.json`, the asset a stable release carries.
// semantics: control-api.md §"The release manifest asset".
//
// Validation must stay as strict as #108's producer validator: what is being
// accepted is the digest set a fleet is about to be pinned to (ADR 0001). Every
// rejection is a manifest_invalid and the release is not listed.

// ManifestFormatVersion is the only format this build understands; any other
// value is invalid rather than best-effort parsed.
const ManifestFormatVersion = 1

var (
	// Full, not the 7-40 an agent may report: the workflow always has the sha.
	fullCommitRe = regexp.MustCompile(`^[0-9a-f]{40}$`)
	digestRe     = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// The NORMATIVE component sequence, validated positionally rather than by
// name: a reordered manifest is invalid, never quietly accepted.
var manifestComponents = [2]string{"control-plane", "node-agent"}

// ManifestComponent is one pinned component of a release.
type ManifestComponent struct {
	Name   string `json:"name"`
	Image  string `json:"image"`
	Digest string `json:"digest"`
}

// Manifest is the asset decoded to validate. The raw bytes, not a
// re-marshalling of this struct, are what is served back.
type Manifest struct {
	FormatVersion int                 `json:"format_version"`
	Version       string              `json:"version"`
	Prerelease    bool                `json:"prerelease"`
	SourceCommit  string              `json:"source_commit"`
	BuiltAt       string              `json:"built_at"`
	SchemaVersion int                 `json:"schema_version"`
	Components    []ManifestComponent `json:"components"`

	// builtAt is the parsed BuiltAt, filled by ParseManifest.
	builtAt time.Time
}

// BuiltAtTime is the parsed `built_at`, valid only on a Manifest ParseManifest
// returned without error.
func (m Manifest) BuiltAtTime() time.Time { return m.builtAt }

// ParseManifest decodes and fully validates one manifest asset. Unknown keys
// are invalid at both levels: the producer refuses to publish one, so accepting
// it here would accept a document the workflow would not emit.
func ParseManifest(raw []byte) (Manifest, error) {
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("manifest is not valid JSON in the documented shape: %w", err)
	}
	// Not "the manifest plus noise" — a different document than the validated one.
	if dec.More() {
		return Manifest{}, fmt.Errorf("manifest carries trailing content after the object")
	}

	if m.FormatVersion != ManifestFormatVersion {
		return Manifest{}, fmt.Errorf("manifest format_version %d is not understood by this build (want %d)",
			m.FormatVersion, ManifestFormatVersion)
	}
	if strings.TrimSpace(m.Version) == "" {
		return Manifest{}, fmt.Errorf("manifest has no version")
	}
	if strings.HasPrefix(m.Version, "v") {
		return Manifest{}, fmt.Errorf("manifest version %q carries a leading v", m.Version)
	}
	if !fullCommitRe.MatchString(m.SourceCommit) {
		return Manifest{}, fmt.Errorf("manifest source_commit %q is not 40 lowercase hex", m.SourceCommit)
	}
	// The ADR 0002 ordering key: a non-positive one orders below every real
	// release and offers a downgrade.
	if m.SchemaVersion <= 0 {
		return Manifest{}, fmt.Errorf("manifest schema_version %d is not positive", m.SchemaVersion)
	}
	built, err := time.Parse(time.RFC3339, m.BuiltAt)
	if err != nil {
		return Manifest{}, fmt.Errorf("manifest built_at %q is not RFC3339: %w", m.BuiltAt, err)
	}
	m.builtAt = built.UTC()

	if len(m.Components) != len(manifestComponents) {
		return Manifest{}, fmt.Errorf("manifest has %d components, want exactly %d",
			len(m.Components), len(manifestComponents))
	}
	for i, c := range m.Components {
		if c.Name != manifestComponents[i] {
			return Manifest{}, fmt.Errorf("manifest component %d is %q, want %q (the order is normative)",
				i, c.Name, manifestComponents[i])
		}
		if err := validateImageRef(c.Image); err != nil {
			return Manifest{}, fmt.Errorf("manifest component %q: %w", c.Name, err)
		}
		if !digestRe.MatchString(c.Digest) {
			return Manifest{}, fmt.Errorf("manifest component %q digest %q is not sha256:<64 hex>",
				c.Name, c.Digest)
		}
	}
	return m, nil
}

// validateImageRef enforces "repository name alone": no tag, no digest. A tag
// is never an identity (ADR 0001); the digest field beside it is authoritative.
func validateImageRef(image string) error {
	if strings.TrimSpace(image) == "" {
		return fmt.Errorf("image is empty")
	}
	if image != strings.TrimSpace(image) {
		return fmt.Errorf("image %q has surrounding whitespace", image)
	}
	if strings.Contains(image, "@") {
		return fmt.Errorf("image %q carries a digest; the digest field is the only place one belongs", image)
	}
	// Only past the final path separator, so a host:port is not read as a tag.
	last := image[strings.LastIndex(image, "/")+1:]
	if strings.Contains(last, ":") {
		return fmt.Errorf("image %q carries a tag; a tag is never an identity", image)
	}
	return nil
}
