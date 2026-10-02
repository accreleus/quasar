package session

import (
	"regexp"
	"strings"
	"testing"
)

// The readiness gate's half of the admission-SQL proof (#262). The pre-refactor
// anchors in admission_query_test.go are rendered by a ZERO-VALUE candidacy, so
// they are still byte-exact; these pins are what covers the gated render, which
// no captured statement can.

// gatedCandidacy is what production builds: NewStore always defaults the
// window, so a gate-off candidacy is unreachable outside a test.
func gatedCandidacy(p CreateParams, veto VramAdmission) candidacy {
	return candidacy{p: p, veto: veto, readiness: ReadinessAdmission{StaleSecs: defaultReadinessStaleSecs}}
}

var placeholderRe = regexp.MustCompile(`\$\d+`)

// clauseOf returns the readiness predicate as it appears in sql, normalized for
// whitespace and placeholder numbering: the two call sites number it
// differently and must still be the same predicate.
func clauseOf(t *testing.T, sql string) string {
	t.Helper()
	i := strings.Index(sql, "h.readiness IS NULL")
	if i < 0 {
		return ""
	}
	j := strings.Index(sql[i:], ")")
	for j >= 0 && strings.Count(sql[i:i+j], "(") != strings.Count(sql[i:i+j], ")") {
		next := strings.Index(sql[i+j+1:], ")")
		if next < 0 {
			break
		}
		j += next + 1
	}
	return placeholderRe.ReplaceAllString(normSQL(sql[i:i+j+1]), "$?")
}

// TestReadinessClauseText pins the predicate itself: which columns it reads and
// which way round the fail-open disjuncts sit. Getting this inverted gates a
// fleet on evidence it does not have.
func TestReadinessClauseText(t *testing.T) {
	const want = "h.readiness IS NULL " +
		"OR h.readiness_reported_at IS NULL " +
		"OR h.readiness_reported_at < now() - make_interval(secs => $?::int) " +
		"OR NOT (h.readiness_block_host OR g.readiness_blocked)"
	got := placeholderRe.ReplaceAllString(normSQL(readinessGateSQL(7, false)), "$?")
	if got != "( "+want+" )" {
		t.Fatalf("clause = %s\nwant   = ( %s )", got, want)
	}
	withHomes := placeholderRe.ReplaceAllString(normSQL(readinessGateSQL(7, true)), "$?")
	if !strings.Contains(withHomes, "OR g.readiness_blocked OR h.readiness_block_homes)") {
		t.Fatalf("managed-home clause = %s", withHomes)
	}
	if strings.Contains(got, "readiness_block_homes") {
		t.Fatal("the homes term must not be rendered for a launch that mounts no home: " +
			"binding a parameter the statement does not reference is rejected by Postgres")
	}
	// make_interval with an integer parameter, never $n::interval: a NULL
	// interval makes the clause NULL, which WHERE treats as false — fail-closed.
	if strings.Contains(got, "::interval") {
		t.Fatal("an interval-typed bind would make the gate fail CLOSED on a NULL")
	}
}

// TestReadinessGateRendersIdenticallyInPickAndRecheck is the property that makes
// the retry loop terminate: a divergence here picks a GPU and then rejects it
// under its own lock, 50 times, for a spurious capacity error on an idle fleet.
func TestReadinessGateRendersIdenticallyInPickAndRecheck(t *testing.T) {
	for _, managedHome := range []bool{false, true} {
		p := CreateParams{NeedEncodeSlots: 1, ManagedHome: managedHome}
		c := gatedCandidacy(p, VramAdmission{MinFreeMB: 1024, InflightMB: 512, StalenessSecs: 20}.normalize())

		candSQL, _ := c.candidateQuery(PolicySpread)
		recheckSQL, _ := c.recheckQuery("11111111-1111-1111-1111-111111111111")
		diagSQL, _ := c.vetoDiagQuery()

		cand, recheck, diag := clauseOf(t, candSQL), clauseOf(t, recheckSQL), clauseOf(t, diagSQL)
		if cand == "" {
			t.Fatalf("managed_home=%v: the candidate query renders no readiness gate", managedHome)
		}
		if cand != recheck {
			t.Fatalf("managed_home=%v: pick and re-check disagree\n pick: %s\nrecheck: %s", managedHome, cand, recheck)
		}
		// The veto diagnostic carries it too, or a GPU the gate excluded is
		// reported as VRAM-vetoed (TestHostNotReadyBesideTheVramVeto).
		if cand != diag {
			t.Fatalf("managed_home=%v: the veto diagnostic's gate differs\n pick: %s\n diag: %s", managedHome, cand, diag)
		}
		if managedHome != strings.Contains(cand, "readiness_block_homes") {
			t.Fatalf("managed_home=%v but the homes term presence is %v", managedHome, !managedHome)
		}
	}
}

// TestReadinessGateIsNotInTheTotalsProbe: the totals probe stays "would anything
// have qualified on capacity alone". Gating it there would turn ordinary slot
// exhaustion into a non-retryable no_host_available and destroy the
// host_not_ready / capacity_exhausted distinction.
func TestReadinessGateIsNotInTheTotalsProbe(t *testing.T) {
	c := gatedCandidacy(CreateParams{NeedEncodeSlots: 1, ManagedHome: true}, VramAdmission{}.normalize())
	sql, args := c.totalsQuery()
	if strings.Contains(sql, "readiness") {
		t.Fatalf("the totals probe carries the readiness gate:\n%s", sql)
	}
	if len(args) != 2 || args[1] != "" {
		t.Fatalf("totals args = %#v, want slots plus canonical placement app", args)
	}
	// Its readiness-aware sibling, used only to classify a refusal, does carry it.
	sql, _ = c.readinessTotalsQuery()
	if clauseOf(t, sql) == "" {
		t.Fatalf("readinessTotalsQuery renders no gate:\n%s", sql)
	}
}

// TestReadinessGateOffRendersNothing guards the anchor proof: admissionMatrix
// builds a zero-value candidacy, and if that ever started rendering the gate the
// 15 captured statements would stop being reachable and the equivalence proof
// would be silently rewritten instead of extended.
func TestReadinessGateOffRendersNothing(t *testing.T) {
	c := candidacy{p: CreateParams{NeedEncodeSlots: 1, ManagedHome: true}, veto: VramAdmission{}.normalize()}
	for name, sql := range map[string]string{
		"candidate": first(c.candidateQuery(PolicySpread)),
		"recheck":   first(c.recheckQuery("11111111-1111-1111-1111-111111111111")),
		"totals":    first(c.totalsQuery()),
		"vetodiag":  first(c.vetoDiagQuery()),
	} {
		if strings.Contains(sql, "readiness") {
			t.Errorf("%s renders the gate from a zero-value candidacy:\n%s", name, sql)
		}
	}
	if c.readiness.enabled() {
		t.Error("the zero value must be gate-off; NewStore is what defaults the window")
	}
	if !NewStore(nil).readiness.enabled() {
		t.Error("NewStore with no option must gate with the contract default")
	}
	if got := NewStore(nil).readiness.StaleSecs; got != 60 {
		t.Errorf("default window = %d s, want 60", got)
	}
	if got := NewStore(nil, WithReadinessStaleSecs(0)).readiness.StaleSecs; got != 60 {
		t.Errorf("<= 0 window = %d s, want the default 60 — the gate has no off switch", got)
	}
	if got := NewStore(nil, WithReadinessStaleSecs(120)).readiness.StaleSecs; got != 120 {
		t.Errorf("configured window = %d s, want 120", got)
	}
}

func first(sql string, _ []any) string { return sql }

// TestReadinessArgValues pins WHICH VALUE lands at which placeholder once the
// gate is on — the half of the proof the SQL comparison cannot see.
func TestReadinessArgValues(t *testing.T) {
	const (
		gpuID = "11111111-1111-1111-1111-111111111111"
		host  = "22222222-2222-2222-2222-222222222222"
		user  = "33333333-3333-3333-3333-333333333333"
		app   = "44444444-4444-4444-4444-444444444444"
		image = "ghcr.io/example/app:1"
	)
	veto := VramAdmission{MinFreeMB: 1024, InflightMB: 512, StalenessSecs: 20}.normalize()
	c := gatedCandidacy(CreateParams{
		UserID: user, AppID: app, NeedEncodeSlots: 2,
		PinHostID: host, AppImage: image,
	}, veto)
	// The window is a distinct value from the veto's, so a swap cannot pass.
	c.readiness = ReadinessAdmission{StaleSecs: 90}

	cases := []struct {
		name string
		args []any
		want []any
	}{
		// The gate's window is allocated LAST at each site, after the pin and
		// the image ref, so every pre-gate index is where it always was.
		{"candidate/spread", argsOf(func() (string, []any) { return c.candidateQuery(PolicySpread) }),
			[]any{int32(2), int32(20), int32(1024), int32(512), host, image, int32(90)}},
		{"candidate/locality", argsOf(func() (string, []any) { return c.candidateQuery(PolicyLocality) }),
			[]any{int32(2), int32(20), int32(1024), int32(512), user, app, host, image, int32(90)}},
		{"recheck", argsOf(func() (string, []any) { return c.recheckQuery(gpuID) }),
			[]any{gpuID, int32(2), int32(20), int32(1024), int32(512), image, int32(90)}},
		{"vetodiag", argsOf(func() (string, []any) { return c.vetoDiagQuery() }),
			[]any{int32(2), int32(20), host, image, int32(90)}},
		{"readinessdiag", argsOf(func() (string, []any) { return c.readinessDiagQuery() }),
			[]any{int32(2), int32(20), int32(1024), int32(512), host, image, int32(90)}},
		{"readinesstotals", argsOf(func() (string, []any) { return c.readinessTotalsQuery() }),
			[]any{int32(2), host, image, int32(90)}},
		{"totals", argsOf(func() (string, []any) { return c.totalsQuery() }),
			[]any{int32(2), host, image}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.want = append(tc.want, app) // RH05 canonical placement binds last.
			if len(tc.args) != len(tc.want) {
				t.Fatalf("bound %d args, want %d\n got: %#v\nwant: %#v", len(tc.args), len(tc.want), tc.args, tc.want)
			}
			for i := range tc.want {
				if tc.args[i] != tc.want[i] {
					t.Errorf("$%d = %#v, want %#v", i+1, tc.args[i], tc.want[i])
				}
			}
		})
	}
}

// TestReadinessArgCountsMatchPlaceholders extends the density invariant over the
// gate's own dimensions: Postgres rejects a bind carrying an unreferenced
// parameter, which is exactly what rendering the homes term unconditionally, or
// binding the veto's window where nothing references it, would produce.
func TestReadinessArgCountsMatchPlaceholders(t *testing.T) {
	const (
		gpuID = "11111111-1111-1111-1111-111111111111"
		host  = "22222222-2222-2222-2222-222222222222"
		image = "ghcr.io/example/app:1"
	)
	placeholder := regexp.MustCompile(`\$(\d+)`)
	gates := []ReadinessAdmission{{}, {StaleSecs: defaultReadinessStaleSecs}}

	for _, veto := range []VramAdmission{VramAdmission{}.normalize(),
		VramAdmission{MinFreeMB: 1024, InflightMB: 512, StalenessSecs: 20}.normalize()} {
		for _, gate := range gates {
			for _, pin := range []string{"", host} {
				for _, img := range []string{"", image} {
					for _, home := range []bool{false, true} {
						p := CreateParams{UserID: "u", AppID: "a", NeedEncodeSlots: 1,
							PinHostID: pin, AppImage: img, ManagedHome: home}
						c := candidacy{p: p, veto: veto, readiness: gate}

						type q struct {
							name string
							sql  string
							args []any
						}
						var qs []q
						for _, policy := range []PlacementPolicy{PolicySpread, PolicyLocality} {
							sql, args := c.candidateQuery(policy)
							qs = append(qs, q{"candidate/" + policy.String(), sql, args})
						}
						for name, f := range map[string]func() (string, []any){
							"recheck":         func() (string, []any) { return c.recheckQuery(gpuID) },
							"totals":          c.totalsQuery,
							"vetodiag":        c.vetoDiagQuery,
							"readinessdiag":   c.readinessDiagQuery,
							"readinesstotals": c.readinessTotalsQuery,
						} {
							sql, args := f()
							qs = append(qs, q{name, sql, args})
						}

						for _, tc := range qs {
							used := map[int]bool{}
							max := 0
							for _, m := range placeholder.FindAllStringSubmatch(tc.sql, -1) {
								n := 0
								for _, r := range m[1] {
									n = n*10 + int(r-'0')
								}
								used[n] = true
								if n > max {
									max = n
								}
							}
							desc := tc.name + " veto=" + boolStr(veto.enabled()) +
								" gate=" + boolStr(gate.enabled()) + " pin=" + boolStr(pin != "") +
								" image=" + boolStr(img != "") + " home=" + boolStr(home)
							if max != len(tc.args) {
								t.Errorf("%s: highest placeholder $%d but %d args bound", desc, max, len(tc.args))
							}
							for n := 1; n <= max; n++ {
								if !used[n] {
									t.Errorf("%s: $%d is never referenced — Postgres rejects a bind with a gap", desc, n)
								}
							}
						}
					}
				}
			}
		}
	}
}
