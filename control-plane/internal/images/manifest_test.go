package images

import (
	"encoding/json"
	"os"
	"testing"
)

func readFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/manifest-v1.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return data
}

// TestFixtureParsesAndValidates pins the real manifest fixture
// (testdata/manifest-v1.json) against Parse+Validate: the steam entry must
// come through with kind/version/registry_ref intact and the runtime
// superset object preserved as raw JSON.
func TestFixtureParsesAndValidates(t *testing.T) {
	m, err := ParseAndValidate(readFixture(t))
	if err != nil {
		t.Fatalf("parse+validate fixture: %v", err)
	}
	if m.ManifestVersion != 1 {
		t.Fatalf("manifest_version: got %d want 1", m.ManifestVersion)
	}
	if len(m.Images) != 1 {
		t.Fatalf("images: got %d want 1", len(m.Images))
	}
	img := m.Images[0]
	if img.ID != "steam" {
		t.Fatalf("id: got %q want steam", img.ID)
	}
	if img.Kind != KindPrebuilt {
		t.Fatalf("kind: got %q want prebuilt", img.Kind)
	}
	if img.Version != "sha-4afbf76" {
		t.Fatalf("version: got %q want sha-4afbf76", img.Version)
	}
	if img.RegistryRef != "ghcr.io/accreleus/quasar-steam:sha-4afbf76" {
		t.Fatalf("registry_ref: got %q", img.RegistryRef)
	}
	if img.LibraryProvider != "steam" {
		t.Fatalf("library_provider: got %q want steam", img.LibraryProvider)
	}
	if len(img.Runtime) == 0 {
		t.Fatal("runtime: expected raw JSON to be preserved, got empty")
	}
	// MAJOR 1 (adversarial review): Raw must carry the entry's exact original
	// bytes, including "notes" — a field ManifestImage does not declare.
	var decoded map[string]any
	if err := json.Unmarshal(img.Raw, &decoded); err != nil {
		t.Fatalf("decode img.Raw: %v", err)
	}
	if _, ok := decoded["notes"]; !ok {
		t.Fatalf("img.Raw lost the \"notes\" field (a field ManifestImage does not declare): %+v", decoded)
	}
}

// TestParseImagesFieldStrictness pins the data-loss guard: a v1 document with
// a MISSING, null, or non-array images field must be a parse error — never an
// empty manifest, because Store.upsert's deletion reconciliation would then
// wipe the entire cached catalog on what looked like a successful sync
// ({"manifest_version":1} is a plausible upstream publishing mistake). An
// explicit [] stays valid: an intentionally empty catalog may empty the cache.
func TestParseImagesFieldStrictness(t *testing.T) {
	refused := []struct {
		name string
		doc  string
	}{
		{"missing images", `{"manifest_version":1}`},
		{"null images", `{"manifest_version":1,"images":null}`},
		{"non-array images (object)", `{"manifest_version":1,"images":{}}`},
		{"non-array images (string)", `{"manifest_version":1,"images":"nope"}`},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse([]byte(tc.doc)); err == nil {
				t.Fatalf("%s: expected a parse error, got nil (this would let a sync empty the catalog)", tc.name)
			}
		})
	}

	t.Run("explicit empty array is valid", func(t *testing.T) {
		m, err := ParseAndValidate([]byte(`{"manifest_version":1,"images":[]}`))
		if err != nil {
			t.Fatalf("explicit []: %v", err)
		}
		if len(m.Images) != 0 {
			t.Fatalf("images: got %d want 0", len(m.Images))
		}
	})
}

// TestValidateRefusesUnknownManifestVersion — the spec's "refused rather
// than partially applied" acceptance criterion.
func TestValidateRefusesUnknownManifestVersion(t *testing.T) {
	m := &Manifest{
		ManifestVersion: 2,
		Images: []ManifestImage{
			{ID: "x", DisplayName: "X", Version: "1", Kind: KindPrebuilt, RegistryRef: "ghcr.io/x:1"},
		},
	}
	if err := Validate(m); err == nil {
		t.Fatal("expected an error for manifest_version=2, got nil")
	}
}

// TestValidateRefusesMissingRequiredField covers a handful of the required
// fields: a manifest missing any of them must be refused, not partially
// applied (no images upserted).
func TestValidateRefusesMissingRequiredField(t *testing.T) {
	cases := []struct {
		name string
		img  ManifestImage
	}{
		{"missing id", ManifestImage{DisplayName: "X", Version: "1", Kind: KindPrebuilt, RegistryRef: "ghcr.io/x:1"}},
		{"missing display_name", ManifestImage{ID: "x", Version: "1", Kind: KindPrebuilt, RegistryRef: "ghcr.io/x:1"}},
		{"missing version", ManifestImage{ID: "x", DisplayName: "X", Kind: KindPrebuilt, RegistryRef: "ghcr.io/x:1"}},
		{"prebuilt missing registry_ref", ManifestImage{ID: "x", DisplayName: "X", Version: "1", Kind: KindPrebuilt}},
		{"template missing dockerfile", ManifestImage{ID: "x", DisplayName: "X", Version: "1", Kind: KindTemplate}},
		{"unknown kind", ManifestImage{ID: "x", DisplayName: "X", Version: "1", Kind: "bogus"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &Manifest{ManifestVersion: 1, Images: []ManifestImage{tc.img}}
			if err := Validate(m); err == nil {
				t.Fatalf("%s: expected an error, got nil", tc.name)
			}
		})
	}
}

// TestValidateBuildArgsMustBeStringMap — P4 fix #3: build_args with a
// non-string value ({"PORT":8080}) is a hard validation error, never a silent
// drop to no-args. A silent drop would dispatch a build with the WRONG image
// (the Dockerfile's default for that arg instead of the intended value). An
// absent / empty / null / all-string build_args is accepted.
func TestValidateBuildArgsMustBeStringMap(t *testing.T) {
	tpl := func(buildArgs string) *Manifest {
		img := ManifestImage{ID: "x", DisplayName: "X", Version: "1", Kind: KindTemplate, Dockerfile: "Dockerfile"}
		if buildArgs != "" {
			img.BuildArgs = json.RawMessage(buildArgs)
		}
		return &Manifest{ManifestVersion: 1, Images: []ManifestImage{img}}
	}

	rejected := []struct{ name, buildArgs string }{
		{"int value", `{"BASE":"x","PORT":8080}`},
		{"bool value", `{"FLAG":true}`},
		{"nested object value", `{"CFG":{"a":1}}`},
		{"array value", `{"LIST":["a","b"]}`},
		{"null value", `{"K":null}`},
		{"non-object", `"nope"`},
	}
	for _, tc := range rejected {
		t.Run("reject/"+tc.name, func(t *testing.T) {
			if err := Validate(tpl(tc.buildArgs)); err == nil {
				t.Fatalf("build_args %s: expected a validation error, got nil (a bad arg must never dispatch argless)", tc.buildArgs)
			}
		})
	}

	accepted := []struct{ name, buildArgs string }{
		{"absent", ``},
		{"empty object", `{}`},
		{"literal null", `null`},
		{"all strings", `{"BASE":"ubuntu:24.04","PORT":"8080"}`},
	}
	for _, tc := range accepted {
		t.Run("accept/"+tc.name, func(t *testing.T) {
			if err := Validate(tpl(tc.buildArgs)); err != nil {
				t.Fatalf("build_args %q: expected no error, got %v", tc.buildArgs, err)
			}
		})
	}
}

// TestValidateRefusesDuplicateID — a manifest with two entries sharing an id
// must be refused: id is documented as "stable, never reused."
func TestValidateRefusesDuplicateID(t *testing.T) {
	m := &Manifest{
		ManifestVersion: 1,
		Images: []ManifestImage{
			{ID: "x", DisplayName: "X", Version: "1", Kind: KindPrebuilt, RegistryRef: "ghcr.io/x:1"},
			{ID: "x", DisplayName: "X2", Version: "2", Kind: KindPrebuilt, RegistryRef: "ghcr.io/x:2"},
		},
	}
	if err := Validate(m); err == nil {
		t.Fatal("expected an error for duplicate id, got nil")
	}
}
