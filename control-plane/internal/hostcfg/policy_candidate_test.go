package hostcfg

import (
	"strings"
	"testing"
)

// deploymentBaseline is a complete agent baseline in catalog JSON types.
func deploymentBaseline() map[string]any {
	baseline := map[string]any{}
	for _, knob := range Catalog() {
		baseline[knob.Key] = knob.Default
	}
	baseline["abr_floor_kbps"] = nil
	baseline["home_root"] = "/srv/homes"
	return baseline
}

func TestResolveGroupCandidateEveryNextSessionGroup(t *testing.T) {
	baseline := deploymentBaseline()
	for _, group := range NextSessionPolicyGroups() {
		t.Run(group, func(t *testing.T) {
			explicit := map[string]PolicyChoice{group: {Source: "explicit", Value: explicitSamples[group].valid}}
			got, reason, err := resolveGroupCandidate(group, 7, explicit, nil)
			if err != nil || reason != "" {
				t.Fatalf("explicit: %v %q", err, reason)
			}
			if got.Scope != "next_session" || got.Resolved[group] != explicitSamples[group].valid || len(got.Prerequisites) != 0 {
				t.Fatalf("explicit candidate = %+v", got)
			}

			// Deployment resolves only from the reported baseline and binds it
			// as exactly one deployment_baseline fact over this group's keys.
			deployment := map[string]PolicyChoice{group: {Source: "deployment"}}
			if _, reason, _ := resolveGroupCandidate(group, 7, deployment, nil); reason != "baseline_unavailable" {
				t.Fatalf("missing baseline reason = %q", reason)
			}
			partial := deploymentBaseline()
			delete(partial, group)
			if _, reason, _ := resolveGroupCandidate(group, 7, deployment, partial); reason != "baseline_unavailable" {
				t.Fatalf("indeterminate key reason = %q", reason)
			}
			got, reason, err = resolveGroupCandidate(group, 7, deployment, baseline)
			if err != nil || reason != "" {
				t.Fatalf("deployment: %v %q", err, reason)
			}
			fact, _ := digestJSON(map[string]any{group: baseline[group]})
			if len(got.Prerequisites) != 1 || got.Prerequisites[0].(map[string]any)["kind"] != "deployment_baseline" || got.Prerequisites[0].(map[string]any)["id"] != fact {
				t.Fatalf("deployment prerequisites = %+v", got.Prerequisites)
			}
			if _, present := got.Resolved[group]; !present || got.Resolved[group] != baseline[group] {
				t.Fatalf("deployment resolved = %+v", got.Resolved)
			}

			// The digest binds revision and resolution; an unchanged candidate is stable.
			again, _, _ := resolveGroupCandidate(group, 7, deployment, baseline)
			later, _, _ := resolveGroupCandidate(group, 8, deployment, baseline)
			if again.Digest != got.Digest || later.Digest == got.Digest || !validPolicyDigest(got.Digest) {
				t.Fatalf("digest stability: %s %s %s", got.Digest, again.Digest, later.Digest)
			}

			if _, reason, _ := resolveGroupCandidate(group, 7, map[string]PolicyChoice{group: {Source: "automatic"}}, baseline); reason != "unsupported_source" {
				t.Fatalf("automatic reason = %q", reason)
			}
		})
	}
}

func TestResolveGroupCandidateMatchesIdleWireDigest(t *testing.T) {
	// The #335 idle digest (policy_baseline.go idleCandidateForConnection) must
	// be unchanged by generalising, or already-verified hosts would re-apply.
	choice := PolicyChoice{Source: "explicit", Value: float64(900)}
	want, _ := digestJSON(map[string]any{
		"group": "idle_timeout_secs", "scope": "next_session", "revision": "3",
		"settings":          map[string]PolicyChoice{"idle_timeout_secs": choice},
		"resolved_settings": map[string]any{"idle_timeout_secs": float64(900)},
	})
	got, _, _ := resolveGroupCandidate("idle_timeout_secs", 3, map[string]PolicyChoice{"idle_timeout_secs": choice}, nil)
	if got.Digest != want {
		t.Fatalf("idle digest %s, want %s", got.Digest, want)
	}
}

func TestResolveGroupCandidateNullableBaselineAndUnknownGroup(t *testing.T) {
	got, reason, err := resolveGroupCandidate("abr_floor_kbps", 1, nil, deploymentBaseline())
	if err != nil || reason != "" || got.Resolved["abr_floor_kbps"] != nil {
		t.Fatalf("nullable deployment = %+v %q %v", got, reason, err)
	}
	baseline := deploymentBaseline()
	baseline["gop"] = nil
	if _, reason, _ := resolveGroupCandidate("gop", 1, nil, baseline); reason != "baseline_unavailable" {
		t.Fatalf("null for non-nullable key must be unavailable, got %q", reason)
	}
	if _, _, err := resolveGroupCandidate("hardware", 1, nil, baseline); err == nil {
		t.Fatal("restart-scope group resolved as next-session")
	}
	if _, _, err := resolveGroupCandidate("nope", 1, nil, baseline); err == nil {
		t.Fatal("unknown group resolved")
	}
}

func TestCanonicalJSONDoesNotEscapeHTML(t *testing.T) {
	b, err := canonicalJSON(map[string]any{"home_root": "/srv/a&b<c>", "z": 1e21, "a": 0.000001, "m": 1e-7})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b); got != `{"a":0.000001,"home_root":"/srv/a&b<c>","m":1e-7,"z":1e+21}` || strings.HasSuffix(got, "\n") {
		t.Fatalf("canonical = %s", got)
	}
}
