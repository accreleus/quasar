package auth

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

// PATCH /v1/admin/users/{id} must stop reading an oversized body at the shared
// 1 MiB cap (#622), before the service is touched (so a nil service is safe).
func TestUpdateUserBodyIsBounded(t *testing.T) {
	const huge = 8 << 20
	body := &countingBody{Reader: strings.NewReader(`{"role":"` + strings.Repeat("a", huge) + `"}`)}
	req := httptest.NewRequest(http.MethodPatch, "/v1/admin/users/u1", body)
	req.SetPathValue("id", "u1")
	rec := httptest.NewRecorder()
	(&Handler{}).handleUpdateUser(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if body.n > 2<<20 {
		t.Fatalf("handler read %d bytes of an %d-byte body; want it capped near 1 MiB", body.n, huge)
	}
}
