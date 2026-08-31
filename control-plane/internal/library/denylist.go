// Package library implements Steam library discovery (spec §7, §8, §11): the
// agent pull channel, the reconciler, the two-layer denylist, and the janitor
// that schedules the whole thing.
//
// Invariant: the agent never learns a user. Neither pull-channel direction carries a user id,
// username, or user-derived field; the control plane holds scan_id -> (user_id, app_id, host_id)
// in library_scans and resolves it on receipt (§7.3, protocol/agent-api.md P2-01).
package library

import "strings"

// SourceSteam: the column is provider-shaped, not Steam-shaped (0044, apps.library_provider),
// so future providers (Heroic, RomM) slot in as new values with no re-model.
const SourceSteam = "steam"

// Layer 1, the built-in denylist. §8.2 forbids seeding these from memory: every entry was
// captured from a live scan of `appmanifest_*.acf` on a real Steam install, 2026-07-29, name
// in a comment. That scan is also the evidence for §8: StateFlags was 4 for all five tools and
// three of four real games, so no structural field distinguishes a tool from a game — filtering
// on appid/name is not weaker than a structural check that was passed over, there isn't one.
//
// Updating layer 1 is a code change and a release. Layer 2 (library_appid_rules, overlaid at
// runtime) exists because an operator can't wait for one: §8.4's residual (a new Valve runtime
// auto-publishing fleet-wide because every Steam user has Proton) recovers via one Ignore click.

// builtinDenyAppIDs: matching on appid catches a tool whose display name isn't Valve-shaped.
var builtinDenyAppIDs = map[string]string{
	"1493710": "Proton Experimental",                // Tower appmanifest_1493710.acf, StateFlags 4
	"2180100": "Proton Hotfix",                      // Tower appmanifest_2180100.acf, StateFlags 4
	"1628350": "Steam Linux Runtime 3.0 (sniper)",   // Tower appmanifest_1628350.acf, StateFlags 4
	"4183110": "Steam Linux Runtime 4.0",            // Tower appmanifest_4183110.acf, StateFlags 4
	"228980":  "Steamworks Common Redistributables", // Tower appmanifest_228980.acf,  StateFlags 4

	// SteamVR matches neither the prefix list (one-word title, no shared prefix) nor a
	// safer-length "steam" prefix (too broad, would eat real games); §8.4's accepted
	// residual (new appid + new name), added here once found rather than by prefix.
	"250820": "SteamVR",
}

// builtinDenyNamePrefixes matches case-insensitively against the manifest `name`; this is what
// actually mitigates §8.4, since a future `Proton 10.0` keeps the prefix under a new appid.
//
// Each prefix is the whole product-family name, never a shorter fragment ("steam" alone would
// suppress every game starting with the word) — an over-match is worse than a miss because a
// miss recovers with one Ignore click but an over-match is invisible. Both directions of the
// residual (an unmatched new Valve tool auto-publishing; a real game like "Protonaut" caught by
// the prefix) are accepted and asserted in TestNamePrefixDoesNotOverMatch.
var builtinDenyNamePrefixes = []string{
	"proton",                             // Proton Experimental, Proton Hotfix (Tower); covers future Proton N.N
	"steam linux runtime",                // Steam Linux Runtime 3.0 (sniper), 4.0 (Tower)
	"steamworks common redistributables", // Steamworks Common Redistributables (Tower)
}

// Layer names reported by Decide, for the admin "Seen, not published" read: a human decision
// vs the shipped constant, since only the latter is a candidate for `allow`.
const (
	LayerRuleAllow    = "rule_allow"     // an operator wrote rule='allow' (published)
	LayerRuleIgnore   = "rule_ignore"    // an operator wrote rule='ignore'
	LayerBuiltinAppID = "builtin_appid"  // the shipped appid set matched
	LayerBuiltinName  = "builtin_prefix" // a shipped name prefix matched
	LayerAppDetails   = "appdetails"     // the opt-in Steam type lookup said "not a game"
	LayerDefault      = ""               // nothing matched; published by rule 4
)

// Decision is the outcome of the ladder for one observed appid.
type Decision struct {
	// Suppressed is the only field the reconciler branches on. Steps 4 and 5 of
	// §7.7 MUST read the same Decision value, computed once — see Decide.
	Suppressed bool
	// Layer names which rung decided, for the admin read. Never load-bearing.
	Layer string
}

// Decide evaluates §8.2's ladder for one observed appid (`rule` is the layer-2 rule for this
// (parent, source, appid), or "" if none). First match wins: allow rule (above the built-in
// list, so a wrongly-denylisted game recovers without a release) > ignore rule > built-in
// appid/name-prefix match > default publish.
//
// Must be computed once per appid per scan and reused: §7.7 steps 4 and 5 both consume it, and
// step 4 revokes a suppressed tile's entitlements before step 5 filters on it — a re-derived or
// skipped filter would let an Ignore undo itself in the same transaction, forever.
func Decide(rule, externalID, name string) Decision {
	switch rule {
	case RuleAllow:
		return Decision{Suppressed: false, Layer: LayerRuleAllow}
	case RuleIgnore:
		return Decision{Suppressed: true, Layer: LayerRuleIgnore}
	}
	if _, ok := builtinDenyAppIDs[externalID]; ok {
		return Decision{Suppressed: true, Layer: LayerBuiltinAppID}
	}
	lower := strings.ToLower(strings.TrimSpace(name))
	for _, p := range builtinDenyNamePrefixes {
		if strings.HasPrefix(lower, p) {
			return Decision{Suppressed: true, Layer: LayerBuiltinName}
		}
	}
	return Decision{Suppressed: false, Layer: LayerDefault}
}

// DecidedByRule reports whether d was decided by an operator rule, not the shipped constant or
// default. Guards the opt-in appdetails step (§8.3): an `allow` rule must survive a
// `type != "game"` verdict, or enabling that third-party lookup would override the operator's
// own correction.
func DecidedByRule(d Decision) bool {
	return d.Layer == LayerRuleAllow || d.Layer == LayerRuleIgnore
}

// Rule values (library_appid_rules.rule CHECK).
const (
	RuleIgnore = "ignore"
	RuleAllow  = "allow"
)

// ValidRule reports whether r is one of the two accepted rule values. The DB
// CHECK is the durable guard; this is what turns a bad request into a 400 rather
// than a 500.
func ValidRule(r string) bool { return r == RuleIgnore || r == RuleAllow }
