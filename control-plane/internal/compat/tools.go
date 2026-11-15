// Package compat is the compatibility-tool catalogue, the fleet default and the effective
// mapping a Steam-family session is sent. Semantics: control-api.md §Compatibility tools.
package compat

// valveTool is a Valve Proton build: Steam downloads it into each home, Quasar only maps to it.
type valveTool struct{ SteamName, Display string }

// valveTools is code-owned and listed Experimental, Hotfix, then numbered newest first.
var valveTools = []valveTool{
	{"proton_experimental", "Proton Experimental"},
	{"proton_hotfix", "Proton Hotfix"},
	{"proton_11", "Proton 11"},
	{"proton_10", "Proton 10"},
	{"proton_9", "Proton 9"},
	{"proton_8", "Proton 8"},
	{"proton_7", "Proton 7"},
}

// recommendedTool is a GE-Proton release pinned in this Quasar release.
type recommendedTool struct {
	Tag, Asset, SteamName, SHA512 string
	SizeBytes                     int64
}

// recommended is the shipped list. Each entry passed the agent's install checks offline before it
// was listed (node-agent compat_tools::install::tests::a_release_passes_the_install_checks).
var recommended = []recommendedTool{
	{
		Tag: "GE-Proton11-12", Asset: "GE-Proton11-12-x86_64.tar.gz", SteamName: "GE-Proton11-12-x86_64",
		SizeBytes: 566859228,
		SHA512:    "2e37447902529c9fc39138dc0b3f24aed3513b59a190b825e6b4a668e49e2033bdf6b8a7998e8686e067bbb3f61f3a6633e2408f0cc96f6dad983bd18e2db31e",
	},
}
