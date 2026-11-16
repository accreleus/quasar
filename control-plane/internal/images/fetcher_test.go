package images

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/outbound"
)

// The production catalog fetcher reads through the shared outbound client (#1107).
func TestNewHTTPFetcherUsesTheGuardedClient(t *testing.T) {
	if _, ok := NewHTTPFetcher().Client.(*outbound.Client); !ok {
		t.Fatalf("Client is %T, want *outbound.Client", NewHTTPFetcher().Client)
	}
}

func TestFailedFetchMessageNamesHowTheRefMoves(t *testing.T) {
	f := &HTTPFetcher{Client: statusDoer(http.StatusNotFound), Repo: "accreleus/quasar-images", Path: "quasar-manifest.json"}
	_, err := f.Fetch(context.Background(), "nightly")
	if err == nil {
		t.Fatal("404 must fail the fetch")
	}
	// No API route or console control sets image_catalog_ref, so the message
	// must not send an admin looking for one (#999).
	if strings.Contains(err.Error(), "admin-editable") {
		t.Fatalf("message still calls the ref admin-editable: %v", err)
	}
	for _, want := range []string{
		`fetched ref "nightly"`,
		"QUASAR_IMAGE_CATALOG_REPO/_PATH",
		"UPDATE instance_settings SET image_catalog_ref = '<ref>' WHERE id = true",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("message does not name %q: %v", want, err)
		}
	}
}

// statusDoer answers every request with one status and an empty body.
type statusDoer int

func (d statusDoer) Do(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: int(d), Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
}
