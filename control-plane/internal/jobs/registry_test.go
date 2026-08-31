// registry_test.go — Definition validation. Pure unit tests, no database.
//
// Every rule tested here turns a class of SILENT misbehaviour into a boot
// failure. A job registered wrong does not crash: it quietly never runs, or is
// scheduled with nothing to execute it, and nobody notices for weeks. That is
// precisely the invisibility the framework exists to end, so it must not be
// reintroduced by the framework's own configuration surface.
package jobs

import (
	"context"
	"strings"
	"testing"
)

func okDef() Definition {
	return Definition{
		ID:      "artwork.sweep",
		Name:    "Artwork grabber",
		Plane:   PlaneControl,
		Scope:   ScopeInstance,
		Managed: true,
		Default: Schedule{Kind: KindInterval, IntervalSecs: 900, Timezone: "UTC"},
		Run: func(context.Context, RunContext) (Outcome, error) {
			return Succeeded(nil), nil
		},
	}
}

func TestRegisterAcceptsAWellFormedDefinition(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(okDef()); err != nil {
		t.Fatalf("register: %v", err)
	}
	if got := r.IDs(); len(got) != 1 || got[0] != "artwork.sweep" {
		t.Fatalf("ids: %v", got)
	}
	if _, ok := r.Get("artwork.sweep"); !ok {
		t.Fatal("registered job not retrievable")
	}
}

func TestRegisterRejections(t *testing.T) {
	cases := []struct {
		name string
		want string
		fn   func(d *Definition)
	}{
		{"empty id", "lowercase dotted", func(d *Definition) { d.ID = "" }},
		{"undotted id", "lowercase dotted", func(d *Definition) { d.ID = "sweep" }},
		{"id with a slash", "lowercase dotted", func(d *Definition) { d.ID = "artwork/sweep" }},
		{"uppercase id", "lowercase dotted", func(d *Definition) { d.ID = "Artwork.Sweep" }},
		{"no name", "has no name", func(d *Definition) { d.Name = "" }},
		{"bad plane", "invalid plane", func(d *Definition) { d.Plane = "both" }},
		{"bad scope", "invalid scope", func(d *Definition) { d.Scope = "cluster" }},
		{"bad kind", "invalid schedule kind", func(d *Definition) { d.Default.Kind = "cron" }},
		{"interval below the floor", "below the", func(d *Definition) { d.Default.IntervalSecs = 30 }},
		{"unknown timezone", "unknown IANA zone", func(d *Definition) { d.Default.Timezone = "Europe/Londn" }},
		{"window day out of range", "out of range", func(d *Definition) { d.Default.WindowDays = []int{7} }},
		{"half a window", "both window bounds", func(d *Definition) { d.Default.WindowStart = tod("02:00") }},
		{
			// The single most plausible copy-paste error in a Definition literal:
			// an agent-plane job with no host to run on.
			"agent plane with instance scope", "must be host-scoped",
			func(d *Definition) { d.Plane, d.Run = PlaneAgent, nil },
		},
		{
			"managed control-plane job with no body", "needs a Run func",
			func(d *Definition) { d.Run = nil },
		},
		{
			"unmanaged job with a body", "must not set a Run func",
			func(d *Definition) { d.Managed = false },
		},
		{
			"event schedule carrying an interval", "must not set an interval",
			func(d *Definition) { d.Default.Kind, d.Default.IntervalSecs = KindEvent, 900 },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := okDef()
			tc.fn(&d)
			err := NewRegistry().Register(d)
			if err == nil {
				t.Fatalf("accepted a definition it should have rejected")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestRegisterRejectsDuplicates(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(okDef()); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(okDef()); err == nil {
		t.Fatal("a duplicate id must be refused: two Definitions for one id means one of them silently never runs")
	}
}

func TestRegisterAcceptsAnAgentPlaneHostScopedJobWithNoBody(t *testing.T) {
	d := Definition{
		ID:      "template.warmup",
		Name:    "Golden-home template warm-up",
		Plane:   PlaneAgent,
		Scope:   ScopeHost,
		Managed: true,
		Default: Schedule{Kind: KindEvent, Timezone: "Europe/London",
			WindowStart: tod("02:00"), WindowEnd: tod("06:00")},
	}
	if err := NewRegistry().Register(d); err != nil {
		t.Fatalf("register: %v", err)
	}
}

func TestRegisterAcceptsAnUnmanagedRow(t *testing.T) {
	// The §3.7 shape: listed so an operator can see a goroutine they cannot
	// otherwise observe, never scheduled, no body.
	d := Definition{
		ID:          "console.selfheal",
		Name:        "Console self-heal backoff",
		Description: "Runs on a hard-coded backoff in internal/agentws/handler.go.",
		Plane:       PlaneControl,
		Scope:       ScopeHost,
		Managed:     false,
		Default:     Schedule{Kind: KindEvent, Timezone: "UTC"},
	}
	if err := NewRegistry().Register(d); err != nil {
		t.Fatalf("register: %v", err)
	}
}

func TestEmptyRegistryIsValid(t *testing.T) {
	// WP1's shipping state, and the reason merging the framework changes zero
	// behaviour.
	r := NewRegistry()
	if len(r.All()) != 0 || len(r.IDs()) != 0 {
		t.Fatal("a fresh registry is not empty")
	}
}
