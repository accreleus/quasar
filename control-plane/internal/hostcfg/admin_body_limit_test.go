package hostcfg

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

// The admin JSON bodies that also check for trailing input stop reading at the
// 1 MiB cap (#622) and still refuse trailing input. Every case is refused
// before the store is reached, so the zero Handler is enough.
func TestAdminBodiesAreBoundedAndStrict(t *testing.T) {
	const huge = 8 << 20
	big := `{"group":"` + strings.Repeat("a", huge) + `"}`
	h := &Handler{}
	routes := map[string]http.HandlerFunc{
		"idle-apply":   h.handleIdleApply,
		"policy-retry": h.handleRetryPolicy,
		"policy-patch": h.handlePatchPolicy,
	}
	for name, fn := range routes {
		t.Run(name+" oversized", func(t *testing.T) {
			body := &countingBody{Reader: strings.NewReader(big)}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/", body)
			req.SetPathValue("id", "h1")
			fn(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
			if body.n > 2<<20 {
				t.Fatalf("read %d bytes of an %d-byte body; want it capped near 1 MiB", body.n, huge)
			}
		})
		for _, trailing := range []string{`{"group":"x"} {}`, `{"group":"x"} [`, `{"group":"x"} x`} {
			t.Run(name+" trailing "+trailing, func(t *testing.T) {
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(trailing))
				req.SetPathValue("id", "h1")
				fn(rec, req)
				if rec.Code != http.StatusBadRequest {
					t.Fatalf("status = %d, want 400", rec.Code)
				}
			})
		}
	}
}
