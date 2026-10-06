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
// is enabled, and whether its effective runtime_spec declares direct_display.
type DefaultAppFacts struct {
	Found   bool
	Name    string
	Enabled bool
	Direct  bool
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
