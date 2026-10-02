package platform

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
	"github.com/accreleus/quasar/control-plane/internal/semver"
)

// The release manifest assets a stable release carries: `platform-release-manifest.json`
// (format 1, control-api.md §"The release manifest asset") and, from RH06,
// `platform-release-manifest.v2.json` (format 2, amendment 14 §"Release manifest format
// 2"), which adds the recovery actor and the floor.
//
// Validation must stay as strict as the producer's validator
// (scripts/release/validate-platform-release-manifest.sh): what is being accepted is the
// digest set a fleet is about to be pinned to (ADR 0001). Every rejection is a
// manifest_invalid and the release is not listed.

// The two formats this build understands; any other value is invalid rather than
// best-effort parsed.
const (
	ManifestFormat1 = 1
	ManifestFormat2 = 2
)

var (
	// Full, not the 7-40 an agent may report: the workflow always has the sha.
	fullCommitRe = regexp.MustCompile(`^[0-9a-f]{40}$`)
	digestRe     = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// The NORMATIVE component sequences, validated positionally rather than by name: a
// reordered manifest is invalid, never quietly accepted.
var (
	manifestComponentsV1 = []string{"control-plane", "node-agent"}
	manifestComponentsV2 = []string{"control-plane", "node-agent", "recovery-actor"}
	// The floor's entries, in this order (amendment 14).
	manifestFloorComponents = []string{"node-agent", "recovery-actor"}
)

// ManifestComponent is one pinned component of a release.
type ManifestComponent struct {
	Name   string `json:"name"`
	Image  string `json:"image"`
	Digest string `json:"digest"`
}

// ManifestFloor is one entry of a format-2 manifest's `floor`: the oldest release of
// that component the release's control plane still manages.
type ManifestFloor struct {
	Name    string `json:"name"`
	Version string `json:"version"`
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
	// Floor is nil on a format-1 manifest and exactly two entries on a format-2 one.
	// Decoded separately (ParseManifest) so its presence is known.
	Floor []ManifestFloor `json:"-"`

	// builtAt is the parsed BuiltAt, filled by ParseManifest.
	builtAt time.Time
}

// BuiltAtTime is the parsed `built_at`, valid only on a Manifest ParseManifest
// returned without error.
func (m Manifest) BuiltAtTime() time.Time { return m.builtAt }

// FloorVersion is the floor this manifest declares for one component; false on a
// format-1 manifest, which declares none.
func (m Manifest) FloorVersion(component string) (string, bool) {
	for _, f := range m.Floor {
		if f.Name == component {
			return f.Version, true
		}
	}
	return "", false
}

// ParseManifestAsset parses one asset that must be in the given format: the v2 asset
// name carries format 2 and the original name format 1, so a document published under
// the other name is not the document that name promises, and is invalid.
func ParseManifestAsset(raw []byte, format int) (Manifest, error) {
	m, err := ParseManifest(raw)
	if err != nil {
		return Manifest{}, err
	}
	if m.FormatVersion != format {
		return Manifest{}, fmt.Errorf("the asset carries format_version %d, but its name is the format-%d asset's",
			m.FormatVersion, format)
	}
	return m, nil
}

// ParseManifest decodes and fully validates one manifest asset of either format.
// Unknown keys are invalid at every level: the producer refuses to publish one, so
// accepting it here would accept a document the workflow would not emit.
func ParseManifest(raw []byte) (Manifest, error) {
	// `floor` is decoded on its own so its PRESENCE is known: a format-1 manifest that
	// carries one has an unknown key, and a format-2 one without it is missing a key.
	var doc struct {
		Manifest
		Floor json.RawMessage `json:"floor"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return Manifest{}, fmt.Errorf("manifest is not valid JSON in the documented shape: %w", err)
	}
	// Not "the manifest plus noise" — a different document than the validated one.
	if dec.More() {
		return Manifest{}, fmt.Errorf("manifest carries trailing content after the object")
	}
	m := doc.Manifest

	var components []string
	switch m.FormatVersion {
	case ManifestFormat1:
		if doc.Floor != nil {
			return Manifest{}, fmt.Errorf("manifest format_version 1 carries a floor, which only format 2 has")
		}
		components = manifestComponentsV1
	case ManifestFormat2:
		if doc.Floor == nil {
			return Manifest{}, fmt.Errorf("manifest format_version 2 has no floor")
		}
		fdec := json.NewDecoder(bytes.NewReader(doc.Floor))
		fdec.DisallowUnknownFields()
		if err := fdec.Decode(&m.Floor); err != nil {
			return Manifest{}, fmt.Errorf("manifest floor is not in the documented shape: %w", err)
		}
		components = manifestComponentsV2
	default:
		return Manifest{}, fmt.Errorf("manifest format_version %d is not understood by this build (want %d or %d)",
			m.FormatVersion, ManifestFormat1, ManifestFormat2)
	}

	if strings.TrimSpace(m.Version) == "" {
		return Manifest{}, fmt.Errorf("manifest has no version")
	}
	if strings.HasPrefix(m.Version, "v") {
		return Manifest{}, fmt.Errorf("manifest version %q carries a leading v", m.Version)
	}
	// The version is an ORDERING key on the beta channel (#121), so an
	// unparseable one is a rejected manifest rather than a stored row the
	// comparator then has to invent an order for.
	version, ok := semver.ParseFull(m.Version)
	if !ok {
		return Manifest{}, fmt.Errorf("manifest version %q is not semver MAJOR.MINOR.PATCH[-prerelease]", m.Version)
	}
	// The flag and the version string are two statements of the same fact, and
	// two rules read DIFFERENT ones: stable hides a release by the flag, while
	// the switch-back rule protects an install by the version's prerelease part.
	// A manifest where they disagree would be hidden on stable and unprotected
	// once installed, so it is not a manifest this build accepts.
	if m.Prerelease != version.IsPrerelease() {
		has := "has no"
		if version.IsPrerelease() {
			has = "has"
		}
		return Manifest{}, fmt.Errorf("manifest prerelease=%v disagrees with version %q, which %s a prerelease part",
			m.Prerelease, m.Version, has)
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

	if len(m.Components) != len(components) {
		return Manifest{}, fmt.Errorf("manifest has %d components, want exactly %d",
			len(m.Components), len(components))
	}
	for i, c := range m.Components {
		if c.Name != components[i] {
			return Manifest{}, fmt.Errorf("manifest component %d is %q, want %q (the order is normative)",
				i, c.Name, components[i])
		}
		if err := validateImageRef(c.Image); err != nil {
			return Manifest{}, fmt.Errorf("manifest component %q: %w", c.Name, err)
		}
		if !digestRe.MatchString(c.Digest) {
			return Manifest{}, fmt.Errorf("manifest component %q digest %q is not sha256:<64 hex>",
				c.Name, c.Digest)
		}
	}
	if m.FormatVersion == ManifestFormat2 {
		if err := validateFloor(m.Floor, version); err != nil {
			return Manifest{}, err
		}
	}
	return m, nil
}

// validateFloor: exactly the two entries in order, each a version of the top-level
// grammar (no leading v, no build metadata) that does not order above the release.
func validateFloor(floor []ManifestFloor, release semver.Full) error {
	if len(floor) != len(manifestFloorComponents) {
		return fmt.Errorf("manifest floor has %d entries, want exactly %d", len(floor), len(manifestFloorComponents))
	}
	for i, f := range floor {
		if f.Name != manifestFloorComponents[i] {
			return fmt.Errorf("manifest floor entry %d is %q, want %q (the order is normative)",
				i, f.Name, manifestFloorComponents[i])
		}
		v, ok := strictVersion(f.Version)
		if !ok {
			return fmt.Errorf("manifest floor %q version %q is not semver MAJOR.MINOR.PATCH[-prerelease]", f.Name, f.Version)
		}
		if semver.ComparePrecedence(v, release) > 0 {
			return fmt.Errorf("manifest floor %q version %q orders above the release's own version", f.Name, f.Version)
		}
	}
	return nil
}

// strictVersion parses MAJOR.MINOR.PATCH[-prerelease] with no leading v and no build
// metadata — the grammar the floor and `register`'s recovery_actor_version share.
// ok=false for anything else, which the floor never judges (below_floor).
func strictVersion(v string) (semver.Full, bool) {
	if !agentws.ValidRecoveryActorVersion(v) {
		return semver.Full{}, false
	}
	return semver.ParseFull(v)
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
