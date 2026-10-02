package images

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// InspectConfig against a fake OCI registry: index -> manifest -> config blob.
// No network and no DB; these run in a plain `go test ./...`.

type configRegistry struct {
	t   *testing.T
	srv *httptest.Server

	// index, when set, is served for the tag and points at manifestBody.
	index        string
	manifestBody string
	configBody   string

	requireToken bool
	blobStatus   int
	// blobRedirect, when set, answers the blob GET with a 307 to it — what
	// GHCR does.
	blobRedirect string
	seen         []string
}

func digestOf(body string) string {
	sum := sha256.Sum256([]byte(body))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func newConfigRegistry(t *testing.T, labels string) *configRegistry {
	t.Helper()
	f := &configRegistry{t: t}
	f.configBody = fmt.Sprintf(`{"architecture":"amd64","os":"linux","config":{"Labels":%s}}`, labels)
	f.manifestBody = fmt.Sprintf(
		`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json",`+
			`"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":%q,"size":%d},"layers":[]}`,
		digestOf(f.configBody), len(f.configBody))
	f.index = fmt.Sprintf(
		`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[`+
			`{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":%q,"platform":{"architecture":"amd64","os":"linux"}},`+
			`{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":%q,"platform":{"architecture":"unknown","os":"unknown"}}]}`,
		digestOf(f.manifestBody), digestOf("attestation"))

	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"token":"fake-anonymous-token"}`)
	})
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		f.seen = append(f.seen, r.Method+" "+r.URL.Path)
		if f.requireToken && r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate",
				`Bearer realm="https://auth.example.com/token",service="fake.registry",scope="repository:acme/app:pull"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case strings.Contains(r.URL.Path, "/manifests/"):
			ref := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			body := f.index
			if ref == digestOf(f.manifestBody) {
				body = f.manifestBody
			}
			w.Header().Set(dockerContentDigest, digestOf(body))
			fmt.Fprint(w, body)
		case strings.Contains(r.URL.Path, "/blobs/"):
			if f.blobRedirect != "" {
				w.Header().Set("Location", f.blobRedirect)
				w.WriteHeader(http.StatusTemporaryRedirect)
				return
			}
			if f.blobStatus != 0 {
				w.WriteHeader(f.blobStatus)
				return
			}
			fmt.Fprint(w, f.configBody)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *configRegistry) resolver() *RegistryResolver {
	return newTestResolver(&http.Client{Timeout: inspectTimeout}, f.srv.URL)
}

func TestInspectConfigIndexToLabels(t *testing.T) {
	reg := newConfigRegistry(t, `{"org.quasar.source.commit":"`+strings.Repeat("a", 40)+`","org.quasar.schema.version":"76"}`)

	cfg, err := reg.resolver().InspectConfig(context.Background(), "ghcr.io/acme/app:develop")
	if err != nil {
		t.Fatalf("InspectConfig: %v", err)
	}
	if cfg.Label("org.quasar.schema.version") != "76" {
		t.Fatalf("labels = %v", cfg.Labels)
	}
	// The digest is of the INDEX the tag pointed at, computed from the body.
	if cfg.ManifestDigest != digestOf(reg.index) {
		t.Fatalf("digest = %s, want %s", cfg.ManifestDigest, digestOf(reg.index))
	}
	if len(reg.seen) != 3 {
		t.Fatalf("requests = %v, want index, manifest, blob", reg.seen)
	}
}

// A registry that is not GHCR is discovered through its own bearer challenge,
// and the token is reused for the manifest and blob that follow.
func TestInspectConfigBearerChallenge(t *testing.T) {
	reg := newConfigRegistry(t, `{"org.quasar.schema.version":"76"}`)
	reg.requireToken = true

	cfg, err := reg.resolver().InspectConfig(context.Background(), "registry.example.com/acme/app:develop")
	if err != nil {
		t.Fatalf("InspectConfig: %v", err)
	}
	if cfg.Label("org.quasar.schema.version") != "76" {
		t.Fatalf("labels = %v", cfg.Labels)
	}
	// One refused index GET, then index, manifest, blob with the token.
	if len(reg.seen) != 4 {
		t.Fatalf("requests = %v, want a challenge then three authorized GETs", reg.seen)
	}
}

// A single-arch publish answers the tag with a plain manifest: no descent.
func TestInspectConfigPlainManifest(t *testing.T) {
	reg := newConfigRegistry(t, `{"org.quasar.built.at":"2026-09-04T12:00:00Z"}`)
	reg.index = reg.manifestBody

	cfg, err := reg.resolver().InspectConfig(context.Background(), "ghcr.io/acme/app:develop")
	if err != nil {
		t.Fatalf("InspectConfig: %v", err)
	}
	if cfg.Label("org.quasar.built.at") != "2026-09-04T12:00:00Z" {
		t.Fatalf("labels = %v", cfg.Labels)
	}
}

func TestInspectConfigNoLabelsIsEmptyNotAnError(t *testing.T) {
	reg := newConfigRegistry(t, `null`)
	cfg, err := reg.resolver().InspectConfig(context.Background(), "ghcr.io/acme/app:develop")
	if err != nil {
		t.Fatalf("InspectConfig: %v", err)
	}
	if len(cfg.Labels) != 0 || cfg.Label("org.quasar.source.commit") != "" {
		t.Fatalf("labels = %v", cfg.Labels)
	}
}

func TestInspectConfigBlobFailureIsAnError(t *testing.T) {
	reg := newConfigRegistry(t, `{}`)
	reg.blobStatus = http.StatusNotFound
	if _, err := reg.resolver().InspectConfig(context.Background(), "ghcr.io/acme/app:develop"); err == nil {
		t.Fatal("want an error when the config blob is missing")
	}
}

func TestInspectConfigHostOffAllowlist(t *testing.T) {
	r := &RegistryResolver{client: &http.Client{}, allowHosts: map[string]struct{}{"ghcr.io": {}}}
	if _, err := r.InspectConfig(context.Background(), "evil.example.com/acme/app:latest"); err == nil ||
		!strings.Contains(err.Error(), "allowlist") {
		t.Fatalf("err = %v, want an allowlist refusal", err)
	}
}

func TestRegistryEgressHostsAddsExtras(t *testing.T) {
	t.Setenv("QUASAR_IMAGE_REGISTRY_HOSTS", "ghcr.io")
	hosts := RegistryEgressHosts("Registry.Example.com", "")
	if _, ok := hosts["registry.example.com"]; !ok {
		t.Fatalf("hosts = %v, want the extra host lowercased", hosts)
	}
	if _, ok := hosts["ghcr.io"]; !ok {
		t.Fatalf("hosts = %v, want the env host kept", hosts)
	}
}

// The plain-HTTP reader reaches a named test registry by its host:port, over
// http, and nothing else.
func TestPlainHTTPResolverReadsOnlyItsNamedRegistries(t *testing.T) {
	reg := newConfigRegistry(t, `{"org.quasar.source.commit":"`+strings.Repeat("a", 40)+`"}`)
	host := strings.TrimPrefix(reg.srv.URL, "http://")
	r := NewPlainHTTPRegistryResolver(map[string]struct{}{host: {}}, inspectTimeout)

	cfg, err := r.InspectConfig(context.Background(), host+"/dev/quasar-node-agent@"+digestOf(reg.index))
	if err != nil {
		t.Fatalf("InspectConfig: %v", err)
	}
	if cfg.Label("org.quasar.source.commit") != strings.Repeat("a", 40) {
		t.Fatalf("labels = %v", cfg.Labels)
	}
	if _, err := r.InspectConfig(context.Background(), "ghcr.io/acme/app@"+digestOf(reg.index)); err == nil {
		t.Fatal("a registry the operator did not name was read")
	}
}

// Plain HTTP cannot forge the labels a digest carries: every document is checked
// against the digest that named it.
func TestAForgedRegistryAnswerIsRefusedOverPlainHTTP(t *testing.T) {
	commit := `{"org.quasar.source.commit":"` + strings.Repeat("a", 40) + `"}`
	forged := `{"architecture":"amd64","os":"linux","config":{"Labels":{"org.quasar.source.commit":"` + strings.Repeat("f", 40) + `"}}}`
	for name, forge := range map[string]func(*configRegistry) string{
		"config blob": func(reg *configRegistry) string {
			ref := digestOf(reg.index)
			reg.configBody = forged
			return ref
		},
		"child manifest": func(reg *configRegistry) string {
			ref := digestOf(reg.index)
			reg.manifestBody = strings.Replace(reg.manifestBody, `"layers":[]`, `"layers":[],"x":1`, 1)
			return ref
		},
		"top-level manifest": func(reg *configRegistry) string {
			return "sha256:" + strings.Repeat("0", 64)
		},
	} {
		reg := newConfigRegistry(t, commit)
		host := strings.TrimPrefix(reg.srv.URL, "http://")
		ref := host + "/dev/quasar-node-agent@" + forge(reg)
		r := NewPlainHTTPRegistryResolver(map[string]struct{}{host: {}}, inspectTimeout)
		_, err := r.InspectConfig(context.Background(), ref)
		if !errors.Is(err, ErrDigestMismatch) {
			t.Errorf("%s: err = %v, want ErrDigestMismatch", name, err)
		}
	}
}
