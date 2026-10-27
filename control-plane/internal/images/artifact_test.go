package images

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// FetchArtifact reads the preview release manifest: an OCI image manifest with
// exactly one layer whose bytes are the document (amendment 48).

func artifactManifest(layers ...string) string {
	out := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json",` +
		`"config":{"mediaType":"application/vnd.oci.empty.v1+json","digest":"sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a","size":2},"layers":[`
	for i, l := range layers {
		if i > 0 {
			out += ","
		}
		out += fmt.Sprintf(`{"mediaType":"application/json","digest":%q,"size":1}`, l)
	}
	return out + "]}"
}

func TestFetchArtifact(t *testing.T) {
	const doc = `{"format_version":2,"version":null}`
	ctx := context.Background()
	ref := "ghcr.io/acme/app/quasar-release-manifest:preview"

	reg := newConfigRegistry(t, `{}`)
	reg.configBody = doc
	reg.index = artifactManifest(digestOf(doc))
	got, err := reg.resolver().FetchArtifact(ctx, ref)
	if err != nil || string(got) != doc {
		t.Fatalf("FetchArtifact = %q, %v; want the layer's bytes", got, err)
	}

	for name, index := range map[string]string{
		"two layers":   artifactManifest(digestOf(doc), digestOf(doc)),
		"no layer":     artifactManifest(),
		"an index":     newConfigRegistry(t, `{}`).index,
		"a bad digest": artifactManifest("sha256:nope"),
	} {
		reg.index = index
		if _, err := reg.resolver().FetchArtifact(ctx, ref); !errors.Is(err, ErrArtifactShape) {
			t.Errorf("%s: err = %v, want ErrArtifactShape", name, err)
		}
	}

	reg.index = artifactManifest(digestOf("something else"))
	if _, err := reg.resolver().FetchArtifact(ctx, ref); !errors.Is(err, ErrDigestMismatch) {
		t.Errorf("a layer that does not hash to its digest: err = %v", err)
	}

	// A missing layer is a broken publish, never "not published".
	reg.index = artifactManifest(digestOf(doc))
	reg.blobStatus = http.StatusNotFound
	if _, err := reg.resolver().FetchArtifact(ctx, ref); err == nil || errors.Is(err, ErrRegistryNotFound) {
		t.Errorf("a missing layer: err = %v, want a failure that is not ErrRegistryNotFound", err)
	}

	// The tag itself absent is.
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	if _, err := newTestResolver(&http.Client{}, srv.URL).FetchArtifact(ctx, ref); !errors.Is(err, ErrRegistryNotFound) {
		t.Errorf("an unpublished tag: err = %v, want ErrRegistryNotFound", err)
	}
}
