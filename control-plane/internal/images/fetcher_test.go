package images

import (
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/outbound"
)

// The production catalog fetcher reads through the shared outbound client (#1107).
func TestNewHTTPFetcherUsesTheGuardedClient(t *testing.T) {
	if _, ok := NewHTTPFetcher().Client.(*outbound.Client); !ok {
		t.Fatalf("Client is %T, want *outbound.Client", NewHTTPFetcher().Client)
	}
}
