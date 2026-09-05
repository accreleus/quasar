package images

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/outbound"
)

// GHCR serves a blob as a 307 to its blob store, so InspectConfig read no
// labels at all until the hop was taken (live: `status 307` in the detect job's
// last_error). The hop is outbound.GetOneRedirect's: https, allowlisted, one,
// and without the registry bearer token.

// blobStore stands in for pkg-containers.githubusercontent.com. TLS because the
// hop refuses anything that is not https.
type blobStore struct {
	srv      *httptest.Server
	body     string
	redirect bool // answer with another 307, to exercise the second-hop refusal

	requests int
	auth     []string
}

func newBlobStore(t *testing.T, body string) *blobStore {
	t.Helper()
	b := &blobStore{body: body}
	b.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.requests++
		b.auth = append(b.auth, r.Header.Get("Authorization"))
		if b.redirect {
			w.Header().Set("Location", b.srv.URL+"/again")
			w.WriteHeader(http.StatusTemporaryRedirect)
			return
		}
		fmt.Fprint(w, b.body)
	}))
	t.Cleanup(b.srv.Close)
	return b
}

// registryRedirectingBlobs is the registry half: manifests as usual, blobs 307.
func registryRedirectingBlobs(t *testing.T, labels, location string) (*configRegistry, string) {
	t.Helper()
	reg := newConfigRegistry(t, labels)
	reg.blobRedirect = location
	return reg, reg.configBody
}

func TestInspectConfigFollowsTheBlobRedirect(t *testing.T) {
	store := newBlobStore(t, "")
	reg, body := registryRedirectingBlobs(t, `{"org.quasar.schema.version":"76"}`, store.srv.URL+"/blobstore/x?sig=1")
	store.body = body

	client := store.srv.Client()
	client.CheckRedirect = outbound.NoRedirect
	r := newTestResolver(client, reg.srv.URL)

	cfg, err := r.InspectConfig(context.Background(), "ghcr.io/acme/app:develop")
	if err != nil {
		t.Fatalf("InspectConfig: %v", err)
	}
	if cfg.Label("org.quasar.schema.version") != "76" {
		t.Fatalf("labels = %v", cfg.Labels)
	}
	if store.requests != 1 {
		t.Fatalf("blob store saw %d requests, want 1", store.requests)
	}
	// A presigned URL must never be handed the registry bearer token.
	if store.auth[0] != "" {
		t.Fatalf("the hop carried %q", store.auth[0])
	}
}

func TestInspectConfigRefusesUnsafeBlobRedirects(t *testing.T) {
	tests := []struct {
		name     string
		location string
		allow    map[string]struct{}
		want     string
	}{
		{"host off the allowlist", "https://evil.example.com/x", map[string]struct{}{"ghcr.io": {}}, "not allowed"},
		{"plain http", "http://127.0.0.1:1/x", nil, "https"},
		{"relative", "/x", nil, "https"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reg, _ := registryRedirectingBlobs(t, `{}`, tc.location)
			client := &http.Client{CheckRedirect: outbound.NoRedirect}
			r := &RegistryResolver{client: client, allowHosts: tc.allow, baseURL: reg.srv.URL}

			_, err := r.InspectConfig(context.Background(), "ghcr.io/acme/app:develop")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one naming %q", err, tc.want)
			}
		})
	}
}

func TestInspectConfigRefusesASecondBlobRedirect(t *testing.T) {
	store := newBlobStore(t, "")
	store.redirect = true
	reg, _ := registryRedirectingBlobs(t, `{}`, store.srv.URL+"/blobstore/x")

	client := store.srv.Client()
	client.CheckRedirect = outbound.NoRedirect
	r := newTestResolver(client, reg.srv.URL)

	if _, err := r.InspectConfig(context.Background(), "ghcr.io/acme/app:develop"); err == nil ||
		!strings.Contains(err.Error(), "second redirect") {
		t.Fatalf("err = %v, want the second hop refused", err)
	}
}

// The manifest HEAD path is untouched by the hop: a redirect there is still an
// ordinary status, as it always was.
func TestResolveStillRefusesARedirectedManifestHead(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"token":"t"}`)
	})
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "https://elsewhere.example.com/v2/x")
		w.WriteHeader(http.StatusTemporaryRedirect)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	r := newTestResolver(&http.Client{Timeout: digestResolveTimeout}, srv.URL)
	if _, err := r.Resolve(context.Background(), "ghcr.io/acme/app:1.0"); err == nil ||
		!strings.Contains(err.Error(), "status 307") {
		t.Fatalf("err = %v, want the HEAD's 307 reported as a status", err)
	}
}

func TestGHCRImpliesItsBlobHost(t *testing.T) {
	t.Setenv("QUASAR_IMAGE_REGISTRY_HOSTS", "ghcr.io")
	if _, ok := allowedHostsFromEnv()[ghcrBlobHost]; !ok {
		t.Fatalf("ghcr.io must imply %s, else every blob GET is refused at the hop", ghcrBlobHost)
	}
	t.Setenv("QUASAR_IMAGE_REGISTRY_HOSTS", "registry.example.com")
	if _, ok := allowedHostsFromEnv()[ghcrBlobHost]; ok {
		t.Fatal("a non-GHCR allowlist must not gain the GHCR blob host")
	}
}
