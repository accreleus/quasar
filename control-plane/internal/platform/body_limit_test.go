package platform

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// countingBody wraps a reader and counts how many bytes the handler pulled.
type countingBody struct {
	io.Reader
	n int
}

func (c *countingBody) Read(p []byte) (int, error) {
	n, err := c.Reader.Read(p)
	c.n += n
	return n, err
}

// Every platform apply route stops reading at the shared 1 MiB cap (#655) and
// answers 400, before any store or runner is touched (the zero-value ones are
// only there to pass the "wired" checks).
func TestApplyBodiesAreBounded(t *testing.T) {
	const id = "00000000-0000-4000-8000-000000000001"
	const huge = 8 << 20
	h := &ApplyHandler{
		store:  &Store{},
		runner: &Runner{},
		fleet:  &FleetRunner{},
		view:   func(context.Context) (View, error) { return View{}, nil },
	}
	rm := &RemoveHandler{}
	for name, handle := range map[string]http.HandlerFunc{
		"fleet apply":     h.handleFleetApply,
		"host apply":      h.handleHostApply,
		"revert":          h.handleHostRevert,
		"developer apply": h.handleDeveloperApply,
		"remove host":     rm.handleRemove,
	} {
		t.Run(name, func(t *testing.T) {
			body := &countingBody{Reader: strings.NewReader(`{"release_id":"` + strings.Repeat("a", huge) + `"}`)}
			req := httptest.NewRequest(http.MethodPost, "/", body)
			req.SetPathValue("id", id)
			rec := httptest.NewRecorder()
			handle(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body)
			}
			if body.n > 2<<20 {
				t.Fatalf("handler read %d bytes of an %d-byte body; want it capped near 1 MiB", body.n, huge)
			}
		})
	}
}
