package library

// denylist_test.go — Gate 4 item 3 (spec §13): the ladder over the REAL Tower
// manifest set, and the precedence of all four rungs.
//
// The manifest set below is not illustrative. It is the nine `appmanifest_*.acf`
// files observed installed on a live Steam install on a fleet host at implementation time,
// with the names and StateFlags they actually carried. Five are Valve tools and
// four are real games, which is the "5 of 9" split §8 rests its whole argument
// on — and `StateFlags` was 4 for all five tools AND for three of the four
// games, which is the live proof that no structural field distinguishes them.

import "testing"

// observedManifest is one observed manifest.
type observedManifest struct {
	appID string
	name  string
	// stateFlags is recorded and asserted on precisely to prove nothing branches
	// on it. If a future change starts filtering by StateFlags, the assertion in
	// TestStateFlagsDistinguishNothing fails and says why.
	stateFlags int
	isTool     bool
}

var observedManifests = []observedManifest{
	// --- the five Valve tools: MUST SUPPRESS ---
	{"1493710", "Proton Experimental", 4, true},
	{"2180100", "Proton Hotfix", 4, true},
	{"1628350", "Steam Linux Runtime 3.0 (sniper)", 4, true},
	{"4183110", "Steam Linux Runtime 4.0", 4, true},
	{"228980", "Steamworks Common Redistributables", 4, true},
	// --- the four real games: MUST PUBLISH ---
	{"2183900", "Warhammer 40,000: Space Marine 2", 6, false},
	{"2536520", "Diablo II: Resurrected – Infernal Edition", 4, false},
	{"3179810", "Tiny Dangerous Dungeons Remake", 4, false},
	{"517710", "Redout: Enhanced Edition", 4, false},
}

// TestDenylistOverObservedManifestSet is the headline assertion: with no operator
// rules at all, the shipped constant alone produces the 5-suppress / 4-publish
// split. This is the test that would have caught "the denylist is a convenience"
// becoming "the denylist is the correctness mechanism" without the constant
// actually being seeded from reality.
func TestDenylistOverObservedManifestSet(t *testing.T) {
	tools, games := 0, 0
	for _, m := range observedManifests {
		d := Decide("", m.appID, m.name)
		if d.Suppressed != m.isTool {
			t.Errorf("Decide(%q, %q).Suppressed = %v, want %v (layer %q)",
				m.appID, m.name, d.Suppressed, m.isTool, d.Layer)
		}
		if m.isTool {
			tools++
			if d.Layer != LayerBuiltinAppID && d.Layer != LayerBuiltinName {
				t.Errorf("%q suppressed by %q; want a built-in layer", m.name, d.Layer)
			}
		} else {
			games++
			if d.Layer != LayerDefault {
				t.Errorf("%q published by %q; want the default rung", m.name, d.Layer)
			}
		}
	}
	if tools != 5 || games != 4 {
		t.Fatalf("fixture drifted: %d tools / %d games, want 5 / 4 (§8's live split)", tools, games)
	}
}

// TestSteamVRIsSuppressed. SteamVR is deliberately NOT in observedManifests — it was
// not on that host, and that fixture must keep describing the live capture
// exactly, including its 5-of-9 split.
//
// It is here because it is a live instance of §8.4's residual, found by the
// Phase 4 reviewer: a Valve non-game tool that matches NEITHER the appid set as
// originally seeded NOR any name prefix. "SteamVR" is one word and shares no
// prefix with "Steam Linux Runtime" or "Steamworks Common Redistributables", and
// "steam" alone is far too broad to add — it would eat every game whose title
// begins with the word. So the only safe fix is the appid, which is what §8.4
// says the built-in list is for: absorbing these as they are found.
func TestSteamVRIsSuppressed(t *testing.T) {
	d := Decide("", "250820", "SteamVR")
	if !d.Suppressed || d.Layer != LayerBuiltinAppID {
		t.Fatalf("Decide(SteamVR) = %+v; want suppressed by the built-in appid set. "+
			"No name prefix can reach it safely, so the appid is the only mechanism.", d)
	}
	// ...and it stays recoverable, like every other built-in entry.
	if a := Decide(RuleAllow, "250820", "SteamVR"); a.Suppressed {
		t.Error("an allow rule could not un-suppress SteamVR")
	}
	// The prefix layer must NOT be what catches it — if someone later adds a
	// "steam" prefix, this fails and says why that is unsafe.
	if p := Decide("", "999999", "SteamVR"); p.Suppressed {
		t.Error("a name prefix now matches \"SteamVR\"; a prefix that broad also suppresses " +
			"every real game whose title begins with \"Steam\"")
	}
}

// TestStateFlagsDistinguishNothing pins the fact that motivates the whole
// denylist. If this ever fails, the fixture has been edited away from reality
// and §8's argument no longer has evidence behind it.
func TestStateFlagsDistinguishNothing(t *testing.T) {
	toolFlags := map[int]bool{}
	gameFlags := map[int]bool{}
	for _, m := range observedManifests {
		if m.isTool {
			toolFlags[m.stateFlags] = true
		} else {
			gameFlags[m.stateFlags] = true
		}
	}
	overlap := false
	for f := range toolFlags {
		if gameFlags[f] {
			overlap = true
		}
	}
	if !overlap {
		t.Fatal("no StateFlags value is shared between a tool and a game; §8's premise " +
			"(no structural field distinguishes them) is no longer supported by this fixture")
	}
}

// TestLadderPrecedence asserts §8.2's four rungs, each one shown WINNING over
// the rung below it. Four cases, one per rung, all against the real manifest set.
func TestLadderPrecedence(t *testing.T) {
	// A real game and a real tool from the live capture, so no rung is exercised
	// against a value that does not exist on disk.
	const (
		gameID   = "517710"
		gameName = "Redout: Enhanced Edition"
		toolID   = "1493710"
		toolName = "Proton Experimental"
		// A tool the built-in APPID set does not know but whose NAME matches a
		// prefix — the §8.4 mitigation, i.e. a future Proton release.
		futureProtonID   = "9999999"
		futureProtonName = "Proton 10.0"
	)

	cases := []struct {
		rung      string
		rule      string
		id, name  string
		suppress  bool
		wantLayer string
		why       string
	}{
		{
			rung: "1: allow BEATS the built-in denylist",
			rule: RuleAllow, id: toolID, name: toolName,
			suppress: false, wantLayer: LayerRuleAllow,
			why: "this is what makes a wrongly-denylisted game recoverable without a release",
		},
		{
			rung: "2: ignore BEATS the default publish",
			rule: RuleIgnore, id: gameID, name: gameName,
			suppress: true, wantLayer: LayerRuleIgnore,
			why: "this is what makes an operator's Ignore stick",
		},
		{
			rung: "3: the built-in denylist BEATS the default publish",
			rule: "", id: toolID, name: toolName,
			suppress: true, wantLayer: LayerBuiltinAppID,
			why: "the shipped constant is the correctness mechanism under auto-publish",
		},
		{
			rung: "4: the default is publish",
			rule: "", id: gameID, name: gameName,
			suppress: false, wantLayer: LayerDefault,
			why: "auto-publish is the behaviour, not an option",
		},
		{
			rung: "3 by NAME PREFIX: an unknown appid with a Valve title",
			rule: "", id: futureProtonID, name: futureProtonName,
			suppress: true, wantLayer: LayerBuiltinName,
			why: "§8.4's only real mitigation: a new Proton keeps the prefix and is caught with no release",
		},
		{
			rung: "1 by NAME PREFIX: allow beats even the prefix layer",
			rule: RuleAllow, id: futureProtonID, name: futureProtonName,
			suppress: false, wantLayer: LayerRuleAllow,
			why: "a game legitimately titled 'Proton…' must be recoverable",
		},
	}

	for _, c := range cases {
		t.Run(c.rung, func(t *testing.T) {
			got := Decide(c.rule, c.id, c.name)
			if got.Suppressed != c.suppress || got.Layer != c.wantLayer {
				t.Errorf("Decide(%q, %q, %q) = %+v, want {Suppressed:%v Layer:%q}\n  %s",
					c.rule, c.id, c.name, got, c.suppress, c.wantLayer, c.why)
			}
		})
	}
}

// TestNamePrefixIsCaseInsensitive — the manifest is a third-party file and its
// casing is not ours to rely on.
func TestNamePrefixIsCaseInsensitive(t *testing.T) {
	for _, n := range []string{"PROTON 11", "proton 11", "  Proton 11", "sTeAm LiNuX rUnTiMe 5.0"} {
		if d := Decide("", "9999998", n); !d.Suppressed {
			t.Errorf("Decide(%q) published; want suppressed by the prefix layer", n)
		}
	}
}

// TestNamePrefixDoesNotOverMatch. A denylist that eats real games is worse than
// one that misses a tool: a miss is one Ignore click, an over-match is invisible.
func TestNamePrefixDoesNotOverMatch(t *testing.T) {
	for _, n := range []string{
		"Steam World Dig",   // starts with "Steam", not "Steam Linux Runtime"
		"Half-Life: Proton", // a Valve word that is not a PREFIX at all
	} {
		if d := Decide("", "9999997", n); d.Suppressed {
			t.Errorf("Decide(%q) suppressed by %q; the prefix list must not eat real games", n, d.Layer)
		}
	}
}

// TestNamePrefixOverMatchIsBidirectionalAndRecoverable records the OTHER half of
// the residual, which §8.4 states only in the under-match direction.
//
// A game whose title merely BEGINS with a denylisted prefix is suppressed. That
// includes a longer word ("Protonaut") and an exact title match ("Proton") —
// nothing distinguishes them, and nothing should try to, because the prefix
// layer is what makes a future `Proton 10.x` free with no release. The trade is
// deliberate and the recovery is symmetric with the under-match direction: one
// `allow` rule, which rung 1 places ABOVE the built-in list precisely so this is
// fixable without a release.
//
// Asserted rather than left implicit so the behaviour is a decision on record.
func TestNamePrefixOverMatchIsBidirectionalAndRecoverable(t *testing.T) {
	for _, n := range []string{
		"Protonaut", // "proton" as a prefix of a longer word
		"Proton",    // an exact-title collision, equally caught
	} {
		if d := Decide("", "9999997", n); !d.Suppressed || d.Layer != LayerBuiltinName {
			t.Errorf("Decide(%q) = %+v; the prefix layer is EXPECTED to over-match here — "+
				"if this changed, the mitigation for a future Proton release changed with it", n, d)
		}
		if allow := Decide(RuleAllow, "9999997", n); allow.Suppressed {
			t.Errorf("Decide(allow, %q) suppressed; an over-match must be recoverable "+
				"without a release, which is the whole reason rung 1 sits above rung 3", n)
		}
	}
}

// TestDecidedByRule guards the §8.3 containment: the opt-in appdetails rung must
// never reach a decision an operator wrote.
func TestDecidedByRule(t *testing.T) {
	if !DecidedByRule(Decide(RuleAllow, "517710", "Redout")) {
		t.Error("an allow rule must be reported as operator-decided, or appdetails could override it")
	}
	if !DecidedByRule(Decide(RuleIgnore, "517710", "Redout")) {
		t.Error("an ignore rule must be reported as operator-decided")
	}
	if DecidedByRule(Decide("", "517710", "Redout")) {
		t.Error("the default rung is not an operator decision")
	}
	if DecidedByRule(Decide("", "1493710", "Proton Experimental")) {
		t.Error("the built-in rung is not an operator decision")
	}
}

// TestValidAppID is §10's validation point 2 — the control-plane ingest.
func TestValidAppID(t *testing.T) {
	good := []string{"1", "517710", "4183110", "4294967295"}
	bad := []string{
		"", "0", "007", "-1", "+1", " 517710", "517710 ", "51 7710",
		"517710;rm -rf /", "--applaunch", "1e5", "0x1", "abc",
		"99999999999", // 11 digits: past the regex's cap
		"9999999999",  // 10 digits but >= 2^32: past the numeric bound
		"4294967296",  // exactly 2^32
	}
	for _, s := range good {
		if !validAppID(s) {
			t.Errorf("validAppID(%q) = false, want true", s)
		}
	}
	for _, s := range bad {
		if validAppID(s) {
			t.Errorf("validAppID(%q) = true, want false — this value can reach STEAM_STARTUP_FLAGS", s)
		}
	}
}
