// template_test.go — the P4 template context-sha resolver against a fake
// GitHub API (httptest). No DB and no network: these run in a plain
// `go test ./...`.
package images

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testSHA = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"

// fakeGitHub is a minimal stand-in for api.github.com's commit-sha endpoint.
type fakeGitHub struct {
	t *testing.T

	notFound    bool
	stall       time.Duration
	body        string // overrides the default sha response when set
	acceptSeen  string
	pathSeen    string
	rawPathSeen string // the raw, still-escaped request target (r.RequestURI)

	srv *httptest.Server
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{t: t}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/", func(w http.ResponseWriter, r *http.Request) {
		if f.stall > 0 {
			time.Sleep(f.stall)
		}
		f.acceptSeen = r.Header.Get("Accept")
		f.pathSeen = r.URL.Path
		f.rawPathSeen = r.RequestURI
		if f.notFound {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body := f.body
		if body == "" {
			body = testSHA
		}
		fmt.Fprint(w, body)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitHub) resolver() *GitHubContextResolver {
	return newTestContextResolver(&http.Client{Timeout: digestResolveTimeout}, f.srv.URL)
}

func TestGitHubContextResolverResolvesSHA(t *testing.T) {
	gh := newFakeGitHub(t)
	r := gh.resolver()

	sha, err := r.Resolve(context.Background(), "accreleus/quasar-images", "stable")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if sha != testSHA {
		t.Fatalf("sha: got %q want %q", sha, testSHA)
	}
	if gh.acceptSeen != "application/vnd.github.sha" {
		t.Fatalf("accept header: got %q", gh.acceptSeen)
	}
	if !strings.Contains(gh.pathSeen, "/repos/accreleus/quasar-images/commits/stable") {
		t.Fatalf("request path: got %q", gh.pathSeen)
	}
}

func TestGitHubContextResolverRejectsMalformedRepo(t *testing.T) {
	gh := newFakeGitHub(t)
	r := gh.resolver()
	if _, err := r.Resolve(context.Background(), "not-a-valid-repo", "stable"); err == nil {
		t.Fatal("resolve with a malformed repo: got nil error")
	}
}

func TestGitHubContextResolverRejectsEmptyRef(t *testing.T) {
	gh := newFakeGitHub(t)
	r := gh.resolver()
	if _, err := r.Resolve(context.Background(), "accreleus/quasar-images", ""); err == nil {
		t.Fatal("resolve with an empty ref: got nil error")
	}
}

func TestGitHubContextResolverRejectsMalformedResponse(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.body = "not-a-sha"
	r := gh.resolver()
	if _, err := r.Resolve(context.Background(), "accreleus/quasar-images", "stable"); err == nil {
		t.Fatal("resolve with a malformed API response: got nil error")
	}
}

func TestGitHubContextResolverPropagatesNotFound(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.notFound = true
	r := gh.resolver()
	if _, err := r.Resolve(context.Background(), "accreleus/quasar-images", "does-not-exist"); err == nil {
		t.Fatal("resolve against a 404 ref: got nil error")
	}
}

func TestGitHubContextResolverTimesOut(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.stall = 100 * time.Millisecond
	r := newTestContextResolver(&http.Client{Timeout: 10 * time.Millisecond}, gh.srv.URL)
	if _, err := r.Resolve(context.Background(), "accreleus/quasar-images", "stable"); err == nil {
		t.Fatal("resolve against a stalled server: got nil error")
	}
}

// TestGitHubContextResolverEscapesRefPath — P4 fix #4: a ref containing a '#'
// (or other URL-significant byte) must be percent-escaped into the commits URL
// path, not interpolated raw where it would truncate the URL (everything after
// '#' becomes a fragment and never reaches the server). The slash in a
// multi-segment ref stays a real path separator.
func TestGitHubContextResolverEscapesRefPath(t *testing.T) {
	gh := newFakeGitHub(t)
	r := gh.resolver()

	// A ref carrying a '#' and a space. Raw interpolation would send the server
	// only ".../commits/feature" (the rest lost to the fragment); escaped, the
	// whole thing arrives.
	const ref = "feature/#42 fix"
	if _, err := r.Resolve(context.Background(), "accreleus/quasar-images", ref); err != nil {
		t.Fatalf("resolve with a #-bearing ref: %v", err)
	}
	// The slash is preserved as a path separator; the '#' and space are escaped.
	// Assert against the RAW (still-escaped) request target — r.URL.Path is
	// already decoded, so it would show the '#' back and hide the escaping.
	if !strings.Contains(gh.rawPathSeen, "/commits/feature/") {
		t.Fatalf("request path did not preserve the ref's slash: %q", gh.rawPathSeen)
	}
	if !strings.Contains(gh.rawPathSeen, "%23") {
		t.Fatalf("request path did not escape the '#': %q (raw interpolation would have truncated at the '#')", gh.rawPathSeen)
	}
	if strings.Contains(gh.rawPathSeen, "#") {
		t.Fatalf("request path still contains a raw '#': %q", gh.rawPathSeen)
	}
}

func TestNoopContextResolverAlwaysErrors(t *testing.T) {
	if _, err := NoopContextResolver().Resolve(context.Background(), "a/b", "stable"); err == nil {
		t.Fatal("NoopContextResolver.Resolve: got nil error")
	}
}
