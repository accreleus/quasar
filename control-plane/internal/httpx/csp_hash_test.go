package httpx

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// webIndexHTMLPath locates web/index.html relative to this test file
// (control-plane/internal/httpx -> ../../../web/index.html), the same
// runtime.Caller-based resolution TestOpenAPIDrift uses for
// protocol/openapi.yaml (cmd/quasar-control/openapi_drift_test.go).
func webIndexHTMLPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "web", "index.html")
}

// inlineScriptTag matches a <script ...>...</script> element and captures its
// opening-tag attributes (group 1) and body (group 2). Non-greedy body match
// so it stops at the first </script>.
var inlineScriptTag = regexp.MustCompile(`(?s)<script([^>]*)>(.*?)</script>`)

// inlineScriptBodies returns the text content of every <script> element in
// html that has no src attribute (i.e. is genuinely inline — a CSP hash only
// makes sense for those; a `<script type="module" src="...">` is covered by
// script-src 'self' already) and is non-blank.
func inlineScriptBodies(html string) []string {
	var out []string
	for _, m := range inlineScriptTag.FindAllStringSubmatch(html, -1) {
		attrs, body := m[1], m[2]
		if strings.Contains(attrs, "src=") {
			continue
		}
		if strings.TrimSpace(body) == "" {
			continue
		}
		out = append(out, body)
	}
	return out
}

func cspScriptHash(body string) string {
	sum := sha256.Sum256([]byte(body))
	return "sha256-" + base64.StdEncoding.EncodeToString(sum[:])
}

// TestCSPAllowsWebIndexInlineScripts is the anti-drift guard for the
// script-src hash allowlist in SecurityHeaders: every genuinely-inline
// <script> in web/index.html (today: the anti-FOUC theme + density guards)
// must have its exact sha256 hash present in the served Content-Security-
// Policy header, or the browser silently drops the script and (as a same-
// origin QUASAR_WEB_ROOT deploy showed — found by scripts/validate/'s UI
// journeys) spams a CSP violation into the console on every load.
//
// This is a content hash, not a route/shape check: it will fail the moment
// web/index.html's inline script bodies change even by one byte (reindent,
// added log line, ...) without security.go's script-src being updated to
// match — which is the point. The failure message below names the fix.
func TestCSPAllowsWebIndexInlineScripts(t *testing.T) {
	path := webIndexHTMLPath(t)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v (web/index.html is checked into every worktree of this repo — "+
			"if this fails with \"no such file or directory\" something is fundamentally wrong "+
			"with the checkout, not with this test; contrast protocol/openapi.yaml, which is an "+
			"uninitialized-submodule trap)", path, err)
	}

	bodies := inlineScriptBodies(string(data))
	if len(bodies) == 0 {
		t.Fatalf("found zero inline <script> elements in %s — either the anti-FOUC scripts were "+
			"removed (delete this test and the matching hashes in security.go) or the extraction "+
			"regex in this test needs updating to match how they're now written", path)
	}

	rr := httptest.NewRecorder()
	SecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	csp := rr.Header().Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("SecurityHeaders set no Content-Security-Policy header")
	}

	for i, body := range bodies {
		hash := cspScriptHash(body)
		if !strings.Contains(csp, "'"+hash+"'") {
			t.Errorf("web/index.html inline <script> #%d's sha256 hash %q is NOT in the served "+
				"script-src directive (%s). Fix: add 'sha256-...' to script-src in "+
				"control-plane/internal/httpx/security.go — recompute with:\n"+
				"  python3 -c \"import hashlib,base64,sys; print('sha256-' + "+
				"base64.b64encode(hashlib.sha256(open(sys.argv[1],'rb').read()).digest()).decode())\"\n"+
				"run against the exact script body (see web/index.html), or just read the hash "+
				"Chrome reports in its console CSP-violation message and paste it in.",
				i, hash, csp)
		}
	}
}
