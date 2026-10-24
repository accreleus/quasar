package crud

import (
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

// PATCH /v1/admin/apps/{id}/placement stops reading at 1 MiB (#655).
func TestAppPlacementBodyIsBounded(t *testing.T) {
	const huge = 8 << 20
	body := &countingBody{Reader: strings.NewReader(`{"mode":"` + strings.Repeat("a", huge) + `"}`)}
	req := httptest.NewRequest(http.MethodPatch, "/", body)
	req.SetPathValue("id", "00000000-0000-4000-8000-000000000001")
	rec := httptest.NewRecorder()
	(&Handler{}).handlePatchAppPlacement(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if body.n > 2<<20 {
		t.Fatalf("handler read %d bytes of an %d-byte body; want it capped near 1 MiB", body.n, huge)
	}
}
