package outbound

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// The one validated hop. Every refusal here is a containment boundary: the
// Location is remote-supplied, and the hop must not carry the origin's token.

// redirectingTransport answers the first request with a 3xx to location and
// every later one with 200 + body, recording what was asked and with what auth.
type redirectingTransport struct {
	location string
	body     string
	// alwaysRedirect keeps answering 3xx, to exercise the second-hop refusal.
	alwaysRedirect bool
	status         int

	asked []string
	auth  []string
}

func (t *redirectingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.asked = append(t.asked, r.URL.String())
	t.auth = append(t.auth, r.Header.Get("Authorization"))
	status := t.status
	if status == 0 {
		status = http.StatusTemporaryRedirect
	}
	if len(t.asked) == 1 || t.alwaysRedirect {
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Location": []string{t.location}},
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    r,
		}, nil
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(t.body)),
		Request:    r,
	}, nil
}

func TestGetFollowingOneRedirectTakesTheHopWithoutTheToken(t *testing.T) {
	tr := &redirectingTransport{location: "https://blobs.example.com/x?sig=1", body: "config"}
	cfg := testConfig("registry.example.com", "blobs.example.com")
	cfg.transport = tr
	c := mustNew(t, cfg)

	resp, err := c.GetFollowingOneRedirectWithHeader(context.Background(),
		"https://registry.example.com/v2/x/blobs/sha256:abc",
		http.Header{"Authorization": []string{"Bearer secret"}, "Accept": []string{"application/json"}},
		[]string{"blobs.example.com"})
	if err != nil {
		t.Fatalf("GetFollowingOneRedirect: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "config" {
		t.Fatalf("body = %q", body)
	}
	if len(tr.asked) != 2 || tr.asked[1] != "https://blobs.example.com/x?sig=1" {
		t.Fatalf("requests = %v", tr.asked)
	}
	// The presigned target must never see the origin's bearer token.
	if tr.auth[0] != "Bearer secret" || tr.auth[1] != "" {
		t.Fatalf("authorization headers = %v, want the token on the first request only", tr.auth)
	}
}

func TestGetFollowingOneRedirectRefusals(t *testing.T) {
	tests := []struct {
		name     string
		location string
		extra    []string
		want     error
	}{
		{"host off the allowlist", "https://evil.example.com/x", nil, ErrRedirectHost},
		{"host off the caller's narrower list", "https://blobs.example.com/x", []string{"other.example.com"}, ErrRedirectHost},
		{"plain http", "http://blobs.example.com/x", nil, ErrRedirectScheme},
		{"relative", "/x", nil, ErrRedirectScheme},
		{"userinfo", "https://user:pw@blobs.example.com/x", nil, ErrRedirectUser},
		{"no location", "", nil, ErrNoLocation},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tr := &redirectingTransport{location: tc.location, body: "config"}
			cfg := testConfig("registry.example.com", "blobs.example.com")
			cfg.transport = tr
			c := mustNew(t, cfg)

			_, err := c.GetFollowingOneRedirect(context.Background(),
				"https://registry.example.com/v2/x/blobs/sha256:abc", tc.extra)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if len(tr.asked) != 1 {
				t.Fatalf("requests = %v — a refused hop must never be contacted", tr.asked)
			}
		})
	}
}

func TestGetFollowingOneRedirectRefusesASecondHop(t *testing.T) {
	tr := &redirectingTransport{location: "https://blobs.example.com/x", alwaysRedirect: true}
	cfg := testConfig("registry.example.com", "blobs.example.com")
	cfg.transport = tr
	c := mustNew(t, cfg)

	_, err := c.GetFollowingOneRedirect(context.Background(),
		"https://registry.example.com/v2/x/blobs/sha256:abc", nil)
	if !errors.Is(err, ErrRedirectSecond) {
		t.Fatalf("err = %v, want ErrRedirectSecond", err)
	}
	if len(tr.asked) != 2 {
		t.Fatalf("requests = %v, want the origin and exactly one hop", tr.asked)
	}
}

// A non-3xx answer is returned untouched, so this is a drop-in for Do.
func TestGetFollowingOneRedirectPassesANonRedirectThrough(t *testing.T) {
	tr := &redirectingTransport{status: http.StatusOK, body: "config"}
	cfg := testConfig("registry.example.com")
	cfg.transport = tr
	c := mustNew(t, cfg)

	resp, err := c.GetFollowingOneRedirect(context.Background(), "https://registry.example.com/v2/x", nil)
	if err != nil {
		t.Fatalf("GetFollowingOneRedirect: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if len(tr.asked) != 1 {
		t.Fatalf("requests = %v, want one", tr.asked)
	}
}

func TestIsRedirect(t *testing.T) {
	for _, s := range []int{301, 302, 303, 307, 308} {
		if !IsRedirect(s) {
			t.Errorf("IsRedirect(%d) = false", s)
		}
	}
	for _, s := range []int{200, 304, 401, 404, 500} {
		if IsRedirect(s) {
			t.Errorf("IsRedirect(%d) = true", s)
		}
	}
}
