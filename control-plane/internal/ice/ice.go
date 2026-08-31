// Package ice owns the operator-configured STUN/TURN server list (#509). Its
// own package so internal/config (reads it at startup) and internal/session
// (writes it onto signaling coordinates) can share the shape without importing
// each other — the internal/origins reasoning. Server is the W3C RTCIceServer
// dictionary verbatim (protocol/control-api.md, #509 amendment): no mapping
// layer that can disagree with either end. Validation refuses at boot — a bad
// entry otherwise breaks only when a peer connection quietly fails to gather
// candidates, the exact silent failure #509 exists to end.
package ice

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Server is one ICE server as the client receives it. JSON tags match the W3C
// RTCIceServer dictionary, which is also the operator-facing config format.
//
// Username and Credential are omitempty because a stun: entry legitimately has
// neither: STUN has no authentication, and emitting empty strings would invite a
// reader to think the credential went missing.
type Server struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

// maxServers bounds a configured list. Not a security property — the operator
// writes this value themselves — but a list this long is a mistake (a paste of
// the wrong thing), and the browser tries every server during gathering, so a
// runaway list slows every launch.
const maxServers = 32

// Parse reads the QUASAR_ICE_SERVERS value: a JSON array of Server objects.
// An empty or whitespace-only value yields a nil list, which is the default and
// reproduces the pre-#509 behaviour exactly (host candidates only).
//
// Every error names QUASAR_ICE_SERVERS and what a correct value looks like,
// because the operator reading it is holding a .env file, not this source.
func Parse(raw string) ([]Server, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}

	var servers []Server
	dec := json.NewDecoder(strings.NewReader(raw))
	// Reject unknown keys: a typo like "url" for "urls", or "password" for
	// "credential", would otherwise parse into an entry that silently does
	// nothing at all.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&servers); err != nil {
		return nil, malformed(err)
	}
	// Decode stops at the end of the first JSON value, so a value with anything
	// after the closing bracket would otherwise parse "successfully" while the
	// operator's real intent sits unread past it.
	if dec.More() {
		return nil, malformed(fmt.Errorf("unexpected content after the closing bracket"))
	}
	if len(servers) > maxServers {
		return nil, fmt.Errorf("QUASAR_ICE_SERVERS: %d servers configured, maximum is %d",
			len(servers), maxServers)
	}

	for i := range servers {
		// Normalize in place before validating: trimming only a loop copy once
		// let " turn:host:3478" pass validation and be served untrimmed, and
		// RTCPeerConnection throws SyntaxError on the leading space — a
		// client-side launch failure boot validation exists to catch.
		for j, u := range servers[i].URLs {
			servers[i].URLs[j] = strings.TrimSpace(u)
		}
		if err := validate(servers[i]); err != nil {
			return nil, fmt.Errorf("QUASAR_ICE_SERVERS entry %d: %w", i, err)
		}
	}
	if len(servers) == 0 {
		// An explicit "[]" means the same thing as unset. Normalize to nil so
		// callers have one representation of "nothing configured".
		return nil, nil
	}
	return servers, nil
}

// malformed wraps a decode failure with the knob's name and a correct example,
// because the operator who reads this is holding a .env file, not this source.
func malformed(err error) error {
	return fmt.Errorf(
		"QUASAR_ICE_SERVERS: must be a JSON array of ICE servers, e.g. "+
			`[{"urls":["stun:stun.example.net:3478"]}] — %w`, err)
}

// validate enforces the contract's rules on one entry (protocol/control-api.md,
// #509): at least one URL, every URL on an ICE scheme, and credentials present
// exactly when the entry is a TURN entry.
func validate(s Server) error {
	if len(s.URLs) == 0 {
		return fmt.Errorf(`"urls" must list at least one ICE URL`)
	}
	needsCredentials := false
	for _, u := range s.URLs {
		u = strings.TrimSpace(u)
		switch {
		case strings.HasPrefix(u, "stun:"), strings.HasPrefix(u, "stuns:"):
			// STUN has no authentication.
		case strings.HasPrefix(u, "turn:"), strings.HasPrefix(u, "turns:"):
			needsCredentials = true
		default:
			return fmt.Errorf(
				"url %q: scheme must be stun:, stuns:, turn: or turns: (an https:// or bare host is not an ICE URL)", u)
		}
	}
	// A TURN server reached without credentials fails authentication during
	// gathering and contributes nothing, so refuse the config instead of
	// shipping a relay that cannot relay.
	if needsCredentials && (s.Username == "" || s.Credential == "") {
		return fmt.Errorf(`a turn:/turns: entry needs both "username" and "credential"`)
	}
	if !needsCredentials && (s.Username != "" || s.Credential != "") {
		return fmt.Errorf(`a stun:/stuns: entry must not carry "username" or "credential" (STUN has no authentication)`)
	}
	return nil
}

// Redact returns a copy safe to log or to put in a diagnostic bundle: URLs and
// usernames survive (an operator needs them to tell which server is which),
// credentials do not.
func Redact(servers []Server) []Server {
	if len(servers) == 0 {
		return nil
	}
	out := make([]Server, len(servers))
	for i, s := range servers {
		out[i] = Server{URLs: s.URLs, Username: s.Username}
		if s.Credential != "" {
			out[i].Credential = "<redacted>"
		}
	}
	return out
}
