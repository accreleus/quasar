package session

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/profile"
)

// allAdmissionQueries renders the six candidacy queries, keyed by shape.
func allAdmissionQueries(c candidacy) map[string]string {
	out := map[string]string{}
	for shape, f := range map[string]func() (string, []any){
		"candidate":       func() (string, []any) { return c.candidateQuery(PolicySpread) },
		"recheck":         func() (string, []any) { return c.recheckQuery("11111111-1111-1111-1111-111111111111") },
		"totals":          c.totalsQuery,
		"vetodiag":        c.vetoDiagQuery,
		"readinessdiag":   c.readinessDiagQuery,
		"readinesstotals": c.readinessTotalsQuery,
	} {
		sql, args := f()
		out[shape] = normSQL(sql) + fmt.Sprintf(" -- %d args", len(args))
	}
	return out
}

// TestCodecGateIsInEveryAdmissionQuery: the pick, the re-check, both totals
// probes and both diagnostics must see the same candidate set, or a constrained
// launch is picked and then rejected under its own lock, or refused with the
// wrong code.
func TestCodecGateIsInEveryAdmissionQuery(t *testing.T) {
	veto := VramAdmission{MinFreeMB: 1024, InflightMB: 512, StalenessSecs: 20}.normalize()
	ready := ReadinessAdmission{StaleSecs: 60}
	gate := `(COALESCE(g.codecs, h.codecs, '["h264"]'::jsonb) ? $`

	constrained := candidacy{p: CreateParams{NeedEncodeSlots: 1, RequireCodec: "av1"}, veto: veto, readiness: ready}
	for shape, sql := range allAdmissionQueries(constrained) {
		if strings.Count(sql, gate) != 1 {
			t.Errorf("%s: want the codec gate exactly once, got:\n%s", shape, sql)
		}
	}

	free := candidacy{p: CreateParams{NeedEncodeSlots: 1}, veto: veto, readiness: ready}
	for shape, sql := range allAdmissionQueries(free) {
		if strings.Contains(sql, "codecs") {
			t.Errorf("%s: an unconstrained launch must not render the codec gate:\n%s", shape, sql)
		}
	}
}

// TestGPUPinRendersOnlyBesideAHostPin: an index is per host, so a GPU pin with
// no host pin would match that index on every host.
func TestGPUPinRendersOnlyBesideAHostPin(t *testing.T) {
	one := int32(1)
	alone := candidacy{p: CreateParams{NeedEncodeSlots: 1, PinGPUIndex: &one}}
	if sql, _ := alone.candidateQuery(PolicySpread); strings.Contains(sql, "g.index =") {
		t.Errorf("a GPU pin without a host pin rendered:\n%s", sql)
	}

	pinned := candidacy{p: CreateParams{NeedEncodeSlots: 1, PinHostID: "22222222-2222-2222-2222-222222222222", PinGPUIndex: &one}}
	for _, f := range []func() (string, []any){
		func() (string, []any) { return pinned.candidateQuery(PolicySpread) },
		pinned.vetoDiagQuery,
	} {
		if sql, _ := f(); !strings.Contains(normSQL(sql), "AND h.id = $3::uuid AND g.index = $4::int") {
			t.Errorf("the GPU pin must follow the host pin:\n%s", normSQL(sql))
		}
	}
}

func TestCodecConstraint(t *testing.T) {
	chain := profile.LaunchProfile{ID: "hevc-first", Rungs: []profile.Profile{
		{ID: "r-hevc", Codec: profile.CodecHEVC},
		{ID: "r-h264", Codec: profile.CodecH264},
	}}
	str := func(s string) *string { return &s }

	cases := []struct {
		name    string
		chain   profile.LaunchProfile
		codec   *string
		want    string
		wantErr bool
	}{
		{"auto carries no constraint", chain, nil, "", false},
		{"a codec a rung offers is the constraint", chain, str("h265"), "h265", false},
		{"a codec no rung offers is the 400, before placement", chain, str("av1"), "", true},
		{"the legacy path has no chain, so only the gate applies", profile.LaunchProfile{}, str("av1"), "av1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := codecConstraint(tc.chain, StreamOverride{Codec: tc.codec})
			if tc.wantErr {
				if !errors.Is(err, ErrRungCodecNotAvailable) {
					t.Fatalf("err = %v, want ErrRungCodecNotAvailable", err)
				}
				// The handler writes this text verbatim; it must name both.
				if !strings.Contains(err.Error(), "av1") || !strings.Contains(err.Error(), `"hevc-first"`) {
					t.Errorf("message %q must name the codec and the launch profile", err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("codecConstraint = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

// TestWithCodecConstraintNamesOnlyTheTwoPlacementRefusals: no_host_available
// and capacity_exhausted name the codec; host_not_ready names nothing; the
// wrapped detail (fleet counts, veto numbers) stays reachable.
func TestWithCodecConstraintNamesOnlyTheTwoPlacementRefusals(t *testing.T) {
	constrained := CreateParams{RequireCodec: "av1"}

	for _, base := range []error{
		ErrNoHostAvailable,
		ErrCapacityExhausted,
		&NoHostRejection{err: ErrNoHostAvailable},
		&VramVetoRejection{err: ErrCapacityExhausted},
	} {
		err := withCodecConstraint(constrained, base)
		if constrainedCodec(err) != "av1" {
			t.Errorf("%v: constrainedCodec = %q, want av1", base, constrainedCodec(err))
		}
		if !errors.Is(err, base) {
			t.Errorf("%v: the wrap must unwrap to the original refusal", base)
		}
	}

	var nh *NoHostRejection
	if !errors.As(withCodecConstraint(constrained, &NoHostRejection{err: ErrNoHostAvailable}), &nh) {
		t.Error("NoHostRejection must stay reachable for the launcher's log")
	}

	for _, err := range []error{
		&HostNotReadyRejection{err: ErrHostNotReady},
		errors.New("classify rejection: boom"),
		nil,
	} {
		if got := withCodecConstraint(constrained, err); got != err || constrainedCodec(got) != "" {
			t.Errorf("%v must pass through unwrapped, got %v", err, got)
		}
	}

	if got := withCodecConstraint(CreateParams{}, ErrCapacityExhausted); got != ErrCapacityExhausted {
		t.Errorf("an unconstrained launch must not be wrapped, got %v", got)
	}
}

func TestRefusalMessage(t *testing.T) {
	wrapped := withCodecConstraint(CreateParams{RequireCodec: "av1"}, ErrCapacityExhausted)
	if got := refusalMessage(wrapped, "plain", "no free GPU can encode %s"); got != "no free GPU can encode av1" {
		t.Errorf("constrained: %q", got)
	}
	if got := refusalMessage(ErrCapacityExhausted, "plain", "no free GPU can encode %s"); got != "plain" {
		t.Errorf("unconstrained: %q", got)
	}
}
