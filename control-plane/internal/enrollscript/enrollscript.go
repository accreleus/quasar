// Package enrollscript serves /enroll-host.sh: the web root's copy of
// deploy/enroll-host.sh with the two images this control plane's Add host command
// installs written into it. The console reads the same two lines back from the
// served script for its stack snippet, so the one-liner and the snippet cannot name
// different seeds. Rendered lines are pinned by testdata/enroll-host/pins.json,
// which the web tests read too.
package enrollscript

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// The placeholder lines in deploy/enroll-host.sh, replaced whole.
const (
	seedVar  = "PINNED_SEED_IMAGE"
	agentVar = "PINNED_AGENT_IMAGE"
)

// Pins are the images an Add host command installs, both repository@sha256:<digest>.
// Empty means unset: the script then refuses to install and says which variable.
type Pins struct {
	SeedImage  string // QUASAR_ENROLL_SEED_IMAGE
	AgentImage string // QUASAR_ENROLL_AGENT_IMAGE
}

// Or is p with each unset pin taken from d: the configured values are overrides of
// the installed release's images, field by field.
func (p Pins) Or(d Pins) Pins {
	if p.SeedImage == "" {
		p.SeedImage = d.SeedImage
	}
	if p.AgentImage == "" {
		p.AgentImage = d.AgentImage
	}
	return p
}

// PinSource answers the pins for one request.
type PinSource func(ctx context.Context) Pins

// The same rule as the recovery actor's ImageRef::parse, restricted to characters that
// are safe inside single quotes in a shell script.
var digestRef = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:-]*@sha256:[0-9a-f]{64}$`)

// ValidImage reports whether ref is repository@sha256:<64 lowercase hex>, never a tag.
func ValidImage(ref string) bool {
	if !digestRef.MatchString(ref) {
		return false
	}
	repo := ref[:strings.IndexByte(ref, '@')]
	return !strings.Contains(repo[strings.LastIndexByte(repo, '/')+1:], ":")
}

// Render writes pins into script. Each placeholder must be present exactly once as a
// whole line, so a script that lost one is refused instead of served without a pin.
func Render(script []byte, pins Pins) ([]byte, error) {
	lines := bytes.Split(script, []byte("\n"))
	for _, p := range []struct{ name, value string }{{seedVar, pins.SeedImage}, {agentVar, pins.AgentImage}} {
		if p.value != "" && !ValidImage(p.value) {
			return nil, fmt.Errorf("%s: %q is not repository@sha256:<digest>", p.name, p.value)
		}
		placeholder := []byte(p.name + "=''")
		at := -1
		for i, l := range lines {
			if bytes.Equal(l, placeholder) {
				if at >= 0 {
					return nil, fmt.Errorf("the script carries more than one %s placeholder line", p.name)
				}
				at = i
			}
		}
		if at < 0 {
			return nil, fmt.Errorf("the script carries no %s placeholder line", p.name)
		}
		lines[at] = []byte(p.name + "='" + p.value + "'")
	}
	return bytes.Join(lines, []byte("\n")), nil
}

// Handler serves webRoot/enroll-host.sh rendered with fixed pins.
func Handler(webRoot string, pins Pins, log *slog.Logger) http.Handler {
	return HandlerFrom(webRoot, func(context.Context) Pins { return pins }, log)
}

// HandlerFrom serves webRoot/enroll-host.sh rendered with the pins pinsFor answers
// for each request. The file is read on every request: the web root may be rebuilt under
// a running control plane.
func HandlerFrom(webRoot string, pinsFor PinSource, log *slog.Logger) http.Handler {
	path := filepath.Join(webRoot, "enroll-host.sh")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		src, err := os.ReadFile(path)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		body, err := Render(src, pinsFor(r.Context()))
		if err != nil {
			log.Error("enroll-host.sh cannot be served", "error", err)
			http.Error(w, "enroll-host.sh in the web root cannot be rendered: "+err.Error(), http.StatusInternalServerError)
			return
		}
		// Pins follow configuration, not the file: never cache.
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.Method == http.MethodHead {
			return
		}
		_, _ = w.Write(body)
	})
}
