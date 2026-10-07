package console

import (
	"encoding/json"
	"fmt"
)

// DefaultAppCheckID is the console readiness check the control plane evaluates
// (control-api.md §Console mode), because only it knows an app's runtime_spec.
const DefaultAppCheckID = "console_default_app"

// ReadinessCheck mirrors protocol/openapi.yaml ReadinessCheck for the checks
// this package evaluates. Blocks is always nil: a console check never affects
// admission or scheduling. Kept local because agentws (which has its own copy)
// imports this package.
type ReadinessCheck struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	Source      string `json:"source"`
	Summary     string `json:"summary"`
	Remediation string `json:"remediation"`
	Blocks      any    `json:"blocks,omitempty"`
}

// DefaultAppFacts is what the console needs to know about its default app:
// whether the row exists, its name, whether it (and a derived tile's parent)
// is enabled, the app's OWN kind and parent (never the effective/parent app's
// kind — a console default is about the picked row's identity, not what it
// borrows), and whether its effective runtime_spec declares direct_display.
type DefaultAppFacts struct {
	Found   bool
	Name    string
	Enabled bool
	// Kind is the app's own `kind` column ("game", "desktop" or "launcher").
	Kind string
	// ParentAppID is non-empty when the app is a derived tile (migration 0044).
	// A tile is never a console default regardless of its own Kind value —
	// belt-and-suspenders alongside the Kind check, since parent_app_id is the
	// authoritative "this row is a tile" signal and Kind is operator data.
	ParentAppID string
	Direct      bool
}

// KindAllowsConsoleDefault is the Go twin of console.Store.DirectApps' SQL
// filter (`a.kind IN ('desktop','launcher') AND a.parent_app_id IS NULL`):
// only a desktop or launcher app with no parent may be a console default —
// never a game, and never a derived tile (a Steam library tile is kind=game
// by convention, but parent_app_id is checked independently so a tile is
// excluded even if its kind were something else). Guarded alongside
// RuntimeSpecDirect by TestConsoleDirectAppsMatchLaunchSpec.
func KindAllowsConsoleDefault(kind string, hasParent bool) bool {
	return !hasParent && (kind == "desktop" || kind == "launcher")
}

// DefaultApp is one entry of the console page's default-app list.
type DefaultApp struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

const pickDirectApp = "Pick a default app from the console page's list; it offers only apps that declare direct_display in their runtime spec."

// DefaultAppCheck evaluates console_default_app. Pure: the store gathers facts.
// Anything but `pass` means the control plane does not auto-start a console
// session (no launch is attempted); this check is the explanation.
func DefaultAppCheck(defaultApp *string, facts DefaultAppFacts) ReadinessCheck {
	c := ReadinessCheck{ID: DefaultAppCheckID, Source: "operator"}
	switch {
	case defaultApp == nil:
		c.Status = "skip"
		c.Summary = "No default app is set, so console mode has nothing to run."
	case !facts.Found:
		c.Status = "fail"
		c.Summary = "The console's default app no longer exists, so console mode will not launch."
		c.Remediation = pickDirectApp
	case !facts.Enabled:
		c.Status = "fail"
		c.Summary = fmt.Sprintf("The console's default app %s is disabled, so console mode will not launch it.", facts.Name)
		c.Remediation = pickDirectApp
	case !KindAllowsConsoleDefault(facts.Kind, facts.ParentAppID != ""):
		c.Status = "fail"
		c.Summary = fmt.Sprintf("The console's default app %s is a %s, not a desktop or launcher, so console mode will not launch it.", facts.Name, consoleKindLabel(facts))
		c.Remediation = pickDirectApp
	case !facts.Direct:
		c.Status = "fail"
		c.Summary = fmt.Sprintf("The console's default app %s cannot run direct: its runtime spec does not declare direct_display, so console mode will not launch it.", facts.Name)
		c.Remediation = pickDirectApp
	default:
		c.Status = "pass"
		c.Summary = fmt.Sprintf("%s can run direct on this host's display.", facts.Name)
	}
	return c
}

// consoleKindLabel is the word the console_default_app fail message uses for
// facts.Kind: a derived tile is always described as "game" regardless of its
// own `kind` column (a tile's purpose is a Steam library entry, not an
// operator-chosen kind), and an empty Kind — the schema's own default — reads
// the same way.
func consoleKindLabel(facts DefaultAppFacts) string {
	if facts.ParentAppID != "" || facts.Kind == "" {
		return "game"
	}
	return facts.Kind
}

// RuntimeSpecDirect reports whether an app's runtime_spec declares
// `direct_display: true` (agent-api.md session_assign app.direct_display).
// Absent, false, a non-boolean or an unreadable spec all mean "cannot run
// direct".
func RuntimeSpecDirect(spec []byte) bool {
	var s struct {
		DirectDisplay any `json:"direct_display"`
	}
	if len(spec) == 0 || json.Unmarshal(spec, &s) != nil {
		return false
	}
	b, ok := s.DirectDisplay.(bool)
	return ok && b
}
