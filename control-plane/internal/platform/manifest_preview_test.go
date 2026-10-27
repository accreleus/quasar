package platform

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The preview manifest (amendment 48), read from the fixture the release tooling
// can validate too, so the producer and this reader agree on one document.
func previewFixture(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "release", "platform-release-manifest.preview.json"))
	if err != nil {
		t.Fatalf("read the shared preview fixture: %v", err)
	}
	return string(raw)
}

const fixtureCommits = `,
  "commits": [
    {
      "commit": "cccccccccccccccccccccccccccccccccccccccc",
      "subject": "#875 Classic layout marked deprecated"
    },
    {
      "commit": "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
      "subject": "docs: a direct commit on develop"
    }
  ]`

func TestParsePreviewManifest(t *testing.T) {
	raw := previewFixture(t)
	m, err := ParsePreviewManifest([]byte(raw))
	if err != nil {
		t.Fatalf("the documented preview manifest was rejected: %v", err)
	}
	if !m.Preview || m.Version != "" || len(m.Commits) != 2 || m.Commits[0].Subject != "#875 Classic layout marked deprecated" {
		t.Fatalf("parsed = %+v", m)
	}
	// Apply reads the stored row with ParseManifest: a preview row must stay applicable.
	if m2, err := ParseManifest([]byte(raw)); err != nil || len(ControlPlaneComponents(m2)) != 2 {
		t.Fatalf("apply's parse of a preview row: %+v, %v", m2, err)
	}
	// Without commits is valid.
	if _, err := ParsePreviewManifest([]byte(strings.Replace(raw, fixtureCommits, "", 1))); err != nil {
		t.Fatalf("a preview manifest without commits was rejected: %v", err)
	}
	// No version to compare the floor with, so a floor above any release is not refused.
	if _, err := ParsePreviewManifest([]byte(strings.ReplaceAll(raw, `"0.4.0-0"`, `"9.9.9"`))); err != nil {
		t.Fatalf("the floor was compared with a null version: %v", err)
	}
}

func TestPreviewAndReleaseManifestsAreNotInterchangeable(t *testing.T) {
	preview := previewFixture(t)
	for _, format := range []int{ManifestFormat1, ManifestFormat2} {
		if _, err := ParseManifestAsset([]byte(preview), format); err == nil {
			t.Errorf("a preview manifest was accepted as the format-%d release asset", format)
		}
	}
	if _, err := ParsePreviewManifest([]byte(v2Fixture(t))); err == nil {
		t.Error("a release asset was accepted as the preview manifest")
	}
	// Only a preview manifest may carry commits.
	withCommits := strings.Replace(v2Fixture(t), "\n  ]\n}", "\n  ]"+fixtureCommits+"\n}", 1)
	if withCommits == v2Fixture(t) {
		t.Fatal("fixture edit did not apply")
	}
	if _, err := ParseManifestAsset([]byte(withCommits), ManifestFormat2); err == nil ||
		!strings.Contains(err.Error(), "only the preview manifest") {
		t.Errorf("a release asset carrying commits: err = %v, want refused", err)
	}
}

func TestParsePreviewManifestRejections(t *testing.T) {
	long := strings.Repeat("é", maxManifestSubjectRunes+1)
	var many strings.Builder
	for i := 0; i <= maxManifestCommits; i++ {
		if i > 0 {
			many.WriteString(",")
		}
		fmt.Fprintf(&many, `{"commit":%q,"subject":"s"}`, strings.Repeat("c", 40))
	}
	commit := func(entry string) func(string) string {
		return func(s string) string {
			return strings.Replace(s, fixtureCommits, `,
  "commits": [`+entry+`]`, 1)
		}
	}
	tests := []struct {
		name    string
		mutate  func(string) string
		wantSub string
	}{
		{"prerelease", func(s string) string { return strings.Replace(s, `"prerelease": false`, `"prerelease": true`, 1) }, "prerelease"},
		{"format 1", func(s string) string { return strings.Replace(s, `"format_version": 2`, `"format_version": 1`, 1) }, "format_version 1"},
		{"no version key", func(s string) string { return strings.Replace(s, `"version": null,`, "", 1) }, "only the preview manifest"},
		{"an unknown key", func(s string) string {
			return strings.Replace(s, `"version": null,`, `"version": null, "channel": "preview",`, 1)
		}, "unknown field"},
		{"commit not full hex", commit(`{"commit":"abc1234","subject":"x"}`), "40 lowercase hex"},
		{"commit extra key", commit(`{"commit":"` + strings.Repeat("c", 40) + `","subject":"x","author":"a"}`), "unknown field"},
		{"no subject", commit(`{"commit":"` + strings.Repeat("c", 40) + `"}`), "no subject"},
		{"two-line subject", commit(`{"commit":"` + strings.Repeat("c", 40) + `","subject":"a\nb"}`), "one line"},
		{"NUL in a subject", commit(`{"commit":"` + strings.Repeat("c", 40) + `","subject":"a\u0000b"}`), "one line"},
		{"escape in a subject", commit(`{"commit":"` + strings.Repeat("c", 40) + `","subject":"a\u001b[31mb"}`), "one line"},
		{"C1 control in a subject", commit(`{"commit":"` + strings.Repeat("c", 40) + `","subject":"a\u0085b"}`), "one line"},
		{"commits null", func(s string) string {
			return strings.Replace(s, fixtureCommits, ",\n  \"commits\": null", 1)
		}, "commits is null"},
		{"subject over 200", commit(`{"commit":"` + strings.Repeat("c", 40) + `","subject":"` + long + `"}`), "one line"},
		{"501 commits", commit(many.String()), "at most 500"},
		{"no floor", func(s string) string {
			i := strings.Index(s, `,
  "floor"`)
			j := strings.Index(s, fixtureCommits)
			return s[:i] + s[j:]
		}, "has no floor"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw := tc.mutate(previewFixture(t))
			if raw == previewFixture(t) {
				t.Fatal("mutation did not apply")
			}
			_, err := ParsePreviewManifest([]byte(raw))
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("err = %v, want one containing %q", err, tc.wantSub)
			}
		})
	}
	// 200 multi-byte characters is the limit, not 200 bytes.
	ok := commit(`{"commit":"` + strings.Repeat("c", 40) + `","subject":"` + long[len("é"):] + `"}`)(previewFixture(t))
	if _, err := ParsePreviewManifest([]byte(ok)); err != nil {
		t.Errorf("a 200-character subject was refused: %v", err)
	}
}
