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

// The admin JSON bodies stop reading at the 1 MiB cap (#622), and the ones
// that check for trailing input still refuse it. Every case is refused before
// the store is reached, so the zero Handler is enough.
func TestAdminBodiesAreBoundedAndStrict(t *testing.T) {
	const huge = 8 << 20
	big := `{"group":"` + strings.Repeat("a", huge) + `"}`
	h := &Handler{}
	routes := []struct {
		name     string
		fn       http.HandlerFunc
		base     string // a well-formed body the handler accepts as far as decoding
		trailing bool   // the handler refuses input after the first value
	}{
		{"idle-apply", h.handleIdleApply, `{"group":"x"}`, true},
		{"policy-retry", h.handleRetryPolicy, `{"group":"x"}`, true},
		{"policy-patch", h.handlePatchPolicy, `{"changes":{}}`, true},
		{"settings-patch", h.handlePatch, `{"overrides":{}}`, false},
		{"restart", h.handleRestart, `{"confirm":true}`, false},
	}
	for _, rt := range routes {
		t.Run(rt.name+" oversized", func(t *testing.T) {
			body := &countingBody{Reader: strings.NewReader(big)}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/", body)
			req.SetPathValue("id", "h1")
			rt.fn(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
			if body.n > 2<<20 {
				t.Fatalf("read %d bytes of an %d-byte body; want it capped near 1 MiB", body.n, huge)
			}
		})
		if !rt.trailing {
			continue
		}
		for _, tail := range []string{` {}`, ` [`, ` x`} {
			t.Run(rt.name+" trailing"+tail, func(t *testing.T) {
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(rt.base+tail))
				req.SetPathValue("id", "h1")
				rt.fn(rec, req)
				if rec.Code != http.StatusBadRequest {
					t.Fatalf("status = %d, want 400", rec.Code)
				}
			})
		}
	}
}
