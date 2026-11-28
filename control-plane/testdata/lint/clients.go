// Package lint is the planted fixture for the forbidigo bans in ../../.golangci.yml.
// scripts/verify/lint.d/go-lint.sh requires findings on exactly the "want" lines.
package lint

import "net/http"

// Naming the type is not a construction: none of these may be reported.
type holder struct {
	client *http.Client
}

func passThrough(c *http.Client, _ map[string]struct{}) *http.Client {
	var h holder
	h.client = c
	return h.client
}

func banned() {
	a := &http.Client{}                 // want
	b := http.Client{}                  // want
	c := new(http.Client)               // want
	var d *http.Client = &http.Client{} // want
	e := http.DefaultClient             // want
	_, _, _, _, _ = a, b.Timeout, c, d, e
}

var (
	_ = passThrough
	_ = banned
)
