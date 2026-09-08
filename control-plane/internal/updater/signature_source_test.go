package updater

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// The source is exercised against a local server rather than the real release
// host: what is under test is the mapping from HTTP outcomes onto the three
// evidence states, which is the part that decides whether a release is
// "unsigned" or "unknown".
func assetServer(t *testing.T, assets map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := assets[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if code, ok := strings.CutPrefix(body, "STATUS:"); ok {
			n, err := strconv.Atoi(code)
			if err != nil {
				t.Errorf("bad STATUS directive %q", body)
				n = http.StatusInternalServerError
			}
			w.WriteHeader(n)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// source points at the test server. It bypasses ParseManifestBaseURL on
// purpose: that function's https rule is tested directly, and httptest cannot
// speak https without a certificate this test has no reason to own.
func source(base string) ReleaseAssetSource {
	return ReleaseAssetSource{BaseURL: base + "/v{version}/"}
}

func strptr(s string) *string { return &s }

func TestEvidenceFetchesBothAssets(t *testing.T) {
	srv := assetServer(t, map[string]string{
		"/v0.3.0/" + ManifestAssetName:  `{"version":"0.3.0"}`,
		"/v0.3.0/" + SignatureAssetName: `{"format_version":1,"signatures":[]}`,
	})
	ev := source(srv.URL).Evidence(context.Background(), strptr("0.3.0"))
	if ev.Absent || ev.FetchError != "" {
		t.Fatalf("both assets present must be evidence: %+v", ev)
	}
	if string(ev.Manifest) != `{"version":"0.3.0"}` {
		t.Fatalf("manifest = %q", ev.Manifest)
	}
}

func TestEvidenceNoSignatureAssetIsDefinitiveAbsence(t *testing.T) {
	srv := assetServer(t, map[string]string{
		"/v0.3.0/" + ManifestAssetName: `{"version":"0.3.0"}`,
	})
	ev := source(srv.URL).Evidence(context.Background(), strptr("0.3.0"))
	if !ev.Absent {
		t.Fatalf("a 404 on the signature asset is an unsigned release: %+v", ev)
	}
	if !strings.Contains(ev.Why, SignatureAssetName) {
		t.Fatalf("why = %q", ev.Why)
	}
}

func TestEvidenceServerErrorIsNotAbsence(t *testing.T) {
	// The distinction the whole three-state shape exists for: a proxy or an
	// outage must never read as "this release is unsigned".
	srv := assetServer(t, map[string]string{
		"/v0.3.0/" + SignatureAssetName: "STATUS:503",
	})
	ev := source(srv.URL).Evidence(context.Background(), strptr("0.3.0"))
	if ev.Absent || ev.FetchError == "" {
		t.Fatalf("a 503 must be an undetermined fetch, not an absence: %+v", ev)
	}
}

func TestEvidenceSignatureWithoutManifestIsAFailure(t *testing.T) {
	srv := assetServer(t, map[string]string{
		"/v0.3.0/" + SignatureAssetName: `{"format_version":1,"signatures":[]}`,
	})
	ev := source(srv.URL).Evidence(context.Background(), strptr("0.3.0"))
	if ev.Absent || ev.FetchError == "" {
		t.Fatalf("a signature with no manifest is a broken publish: %+v", ev)
	}
}

func TestEvidenceUnreachableSourceIsAFailure(t *testing.T) {
	srv := assetServer(t, nil)
	url := srv.URL
	srv.Close()
	ev := ReleaseAssetSource{BaseURL: url + "/v{version}/"}.Evidence(context.Background(), strptr("0.3.0"))
	if ev.Absent || ev.FetchError == "" {
		t.Fatalf("an unreachable source must be undetermined: %+v", ev)
	}
}

func TestEvidenceNoVersionIsAbsence(t *testing.T) {
	// An edge build, or a revert to one this instance can no longer name.
	// There is no published release, so there is nothing that could have
	// signed one — `require` refuses it, `verify` does not.
	for _, v := range []*string{nil, strptr(""), strptr("  ")} {
		ev := ReleaseAssetSource{BaseURL: "https://example.invalid/v{version}/"}.
			Evidence(context.Background(), v)
		if !ev.Absent {
			t.Fatalf("version %v must be an absence: %+v", v, ev)
		}
	}
}

func TestEvidenceRefusesAVersionItWillNotPutInAURL(t *testing.T) {
	// The version arrives over the wire. Nothing that is not a semver release
	// version is ever concatenated into a URL.
	for _, v := range []string{
		"../../etc/passwd", "0.3.0/../..", "0.3.0?x=1", "latest", "v0.3.0",
		"0.3.0 0.4.0", "0.3.0%2f", "01.2.3",
	} {
		ev := ReleaseAssetSource{BaseURL: "https://example.invalid/v{version}/"}.
			Evidence(context.Background(), strptr(v))
		if ev.FetchError == "" {
			t.Errorf("version %q must be refused, got %+v", v, ev)
		}
	}
	// ...and a real prerelease version must still be accepted.
	srv := assetServer(t, map[string]string{
		"/v0.3.0-rc.1/" + ManifestAssetName:  `{"version":"0.3.0-rc.1"}`,
		"/v0.3.0-rc.1/" + SignatureAssetName: `{"format_version":1,"signatures":[]}`,
	})
	if ev := source(srv.URL).Evidence(context.Background(), strptr("0.3.0-rc.1")); ev.FetchError != "" {
		t.Fatalf("a prerelease version must be fetchable: %+v", ev)
	}
}
