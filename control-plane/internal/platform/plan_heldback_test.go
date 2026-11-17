package platform

import (
	"encoding/json"
	"testing"
)

// held_back (amendment 83): an empty `available` says why when ADR 0002's schema
// rule is the ONLY thing that emptied it. It gates nothing, so the cases that
// matter are the ones where it must stay null — a release the operator could not
// have had for some other reason is not an offer withheld from them.
func TestHeldBack(t *testing.T) {
	for _, tc := range []struct {
		name    string
		channel string
		rows    []Release
		want    *HeldBack
	}{
		{
			name:    "nothing detected",
			channel: ChannelStable,
			rows:    nil,
			want:    nil,
		},
		{
			name:    "available is non-empty",
			channel: ChannelStable,
			// A listed row outranks the explanation even when a lower-schema
			// row is stored beside it.
			rows: []Release{
				rel("stable-76", "0.7.6", commitB, 76, at(2)),
				rel("stable-75", "0.7.5", commitA, 75, at(1)),
			},
			want: nil,
		},
		{
			name:    "a release held back for its schema",
			channel: ChannelStable,
			rows:    []Release{rel("stable-75", "0.7.5", commitA, 75, at(1))},
			want:    &HeldBack{Channel: ChannelStable, SchemaVersion: 75, Reason: ReasonSchemaBelowInstalled},
		},
		{
			name:    "the newest held-back release is the one named",
			channel: ChannelStable,
			rows: []Release{
				rel("stable-74", "0.7.4", commitC, 74, at(5)),
				rel("stable-75", "0.7.5", commitA, 75, at(1)),
			},
			want: &HeldBack{Channel: ChannelStable, SchemaVersion: 75, Reason: ReasonSchemaBelowInstalled},
		},
		{
			name:    "refused for another reason too, so not held back",
			channel: ChannelStable,
			// ADR 0001: no manifest, so it could not be applied either way.
			rows: []Release{rel("stable-75", "0.7.5", commitA, 75, at(1), noManifest)},
			want: nil,
		},
		{
			name:    "a prerelease stable ignores, below the schema",
			channel: ChannelStable,
			rows:    []Release{rel("stable-75", "0.7.5-rc.1", commitA, 75, at(1), prerelease)},
			want:    nil,
		},
		{
			name:    "another channel's row",
			channel: ChannelStable,
			rows:    []Release{edgeRel("edge-75", commitA, 75, at(1))},
			want:    nil,
		},
		{
			name:    "an older version at the installed schema is not the schema rule",
			channel: ChannelStable,
			rows: []Release{
				// Same schema_version, so ADR 0002 does not refuse it; the
				// version precedence does.
				rel("stable-76", "0.7.5", commitA, 76, at(1)),
			},
			want: nil,
		},
		{
			name:    "beta names the held-back release's own channel",
			channel: ChannelBeta,
			rows:    []Release{rel("stable-75", "0.7.5", commitA, 75, at(1))},
			want:    &HeldBack{Channel: ChannelStable, SchemaVersion: 75, Reason: ReasonSchemaBelowInstalled},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view := PlanRelease(PlanInputs{
				Channel:      tc.channel,
				ControlPlane: cp(commitC, 76),
				Releases:     tc.rows,
			})
			if tc.want == nil {
				if view.HeldBack != nil {
					t.Fatalf("held_back = %+v, want null", *view.HeldBack)
				}
			} else if view.HeldBack == nil {
				t.Fatalf("held_back = null, want %+v", *tc.want)
			} else if *view.HeldBack != *tc.want {
				t.Fatalf("held_back = %+v, want %+v", *view.HeldBack, *tc.want)
			}

			// Always serialized by an amendment-aware server: null is an answer,
			// not an absent key.
			b, err := json.Marshal(view)
			if err != nil {
				t.Fatal(err)
			}
			var wire map[string]json.RawMessage
			if err := json.Unmarshal(b, &wire); err != nil {
				t.Fatal(err)
			}
			if _, ok := wire["held_back"]; !ok {
				t.Fatal("held_back must always be serialized, null when there is no answer")
			}
			if tc.want != nil && len(view.Available) != 0 {
				t.Fatalf("available = %v, want empty when a release is held back", ids(view.Available))
			}
		})
	}
}
