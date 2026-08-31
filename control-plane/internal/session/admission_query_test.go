package session

import (
	"bufio"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The admission queries are pure string builders, so these tests need no
// database. That is the point of admission_query.go: before it, the only way to
// find out what SQL the scheduler sends was to run it against Postgres.

var wsRun = regexp.MustCompile(`\s+`)

// normSQL collapses whitespace so a comparison is about tokens and placeholder
// numbering, not indentation. Indentation is cosmetic here — it only affects how
// the statement reads in a log — while a changed `$N` is a behaviour change.
func normSQL(s string) string { return strings.TrimSpace(wsRun.ReplaceAllString(s, " ")) }

// admissionMatrix renders every admission query across the full configuration
// space, keyed by shape. Each entry is the normalized SQL.
func admissionMatrix() map[string][]string {
	const (
		gpuID = "11111111-1111-1111-1111-111111111111"
		host  = "22222222-2222-2222-2222-222222222222"
		user  = "33333333-3333-3333-3333-333333333333"
		app   = "44444444-4444-4444-4444-444444444444"
		image = "ghcr.io/example/app:1"
	)
	vetoOn := VramAdmission{MinFreeMB: 1024, InflightMB: 512, StalenessSecs: 20}.normalize()
	vetoOff := VramAdmission{}.normalize()

	out := map[string][]string{}
	add := func(shape, sql string) { out[shape] = append(out[shape], normSQL(sql)) }

	for _, veto := range []VramAdmission{vetoOff, vetoOn} {
		for _, pin := range []string{"", host} {
			for _, img := range []string{"", image} {
				p := CreateParams{
					UserID: user, AppID: app,
					NeedEncodeSlots: 1,
					PinHostID:       pin,
					AppImage:        img,
				}
				c := candidacy{p: p, veto: veto}
				for _, policy := range []PlacementPolicy{PolicySpread, PolicyLocality} {
					sql, _ := c.candidateQuery(policy)
					add("candidate", sql)
				}
				sql, _ := c.recheckQuery(gpuID)
				add("recheck", sql)
				sql, _ = c.totalsQuery()
				add("totals", sql)
				sql, _ = c.vetoDiagQuery()
				add("vetodiag", sql)
			}
		}
	}
	return out
}

// TestAdmissionSQLMatchesPreRefactor is the equivalence proof for the
// candidacy/argset extraction.
//
// testdata/admission_sql_pre_refactor.txt is NOT hand-written. It holds real
// statements captured from a Postgres `log_statement=all` run of this package's
// entire DB suite against the code as it stood BEFORE the extraction — 15
// distinct statements spanning veto on/off, host pin, managed-image readiness
// and both placement policies. Every one of them must still be producible by the
// new renderers, with identical placeholder numbering.
//
// If this fails, the extraction changed what the scheduler asks Postgres. That
// is the failure mode the whole exercise exists to prevent: the divergence class
// here is silent in production (50 burned retries and a spurious capacity error
// on an idle fleet), which is why it is pinned against captured reality rather
// than against a hand-written expectation that could be wrong in the same
// direction as the code.
//
// KNOW WHAT THIS GREEN MEANS. The anchors cover 15 of the 26 configurations in
// the veto × pin × image × policy space — the DB suite never exercised locality
// with a gate, nor veto+image together — and the assertion is one-directional
// (anchors ⊆ generated), so an un-anchored combination fails nothing here. Those
// gaps are closed by construction rather than by capture: gate composition order
// is fixed in the code and independent of configuration, the anchored combos pin
// that order, TestAdmissionArgCountsMatchPlaceholders runs the FULL matrix for
// placeholder density, and TestAdmissionArgValues pins which value lands at
// which index. This test alone is not the proof; those three together are.
func TestAdmissionSQLMatchesPreRefactor(t *testing.T) {
	f, err := os.Open("testdata/admission_sql_pre_refactor.txt")
	if err != nil {
		t.Fatalf("open anchor file: %v", err)
	}
	defer f.Close()

	generated := admissionMatrix()
	index := map[string]map[string]bool{}
	for shape, sqls := range generated {
		index[shape] = map[string]bool{}
		for _, s := range sqls {
			index[shape][s] = true
		}
	}

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	seen := map[string]int{}
	for line := 1; sc.Scan(); line++ {
		raw := sc.Text()
		if strings.TrimSpace(raw) == "" {
			continue
		}
		shape, sql, ok := strings.Cut(raw, "\t")
		if !ok {
			t.Fatalf("anchor line %d is not <shape>\\t<sql>", line)
		}
		seen[shape]++
		if index[shape] == nil {
			t.Errorf("anchor line %d names unknown shape %q", line, shape)
			continue
		}
		if !index[shape][normSQL(sql)] {
			t.Errorf("PRE-REFACTOR %s SQL is no longer producible (anchor line %d).\n"+
				"captured: %s\n\nthe renderer now produces, for this shape:\n  %s",
				shape, line, sql, strings.Join(sortedOf(generated[shape]), "\n  "))
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read anchor file: %v", err)
	}

	// Guard the guard: a truncated anchor file would otherwise make this test
	// pass while proving less than it claims. The counts are the capture's, and
	// they only ever grow — if you re-capture and get fewer, something stopped
	// being exercised by the DB suite and the proof got weaker without saying so.
	want := map[string]int{"candidate": 8, "recheck": 3, "totals": 2, "vetodiag": 2}
	for shape, n := range want {
		if seen[shape] != n {
			t.Errorf("anchor file carries %d %s statements, expected %d — "+
				"the proof changed size; re-read it before trusting a green run", seen[shape], shape, n)
		}
	}
}

// TestAdmissionArgValues pins WHICH VALUE sits at each placeholder.
//
// THE SQL COMPARISON ABOVE CANNOT SEE THIS. Swapping the two `a.add` calls
// inside vetoGate renders byte-identical SQL — the floor still lands at one
// index and the debit at the next — so every anchor still matches while the
// veto compares free VRAM against the debit and debits by the floor. Any pair
// of gates with the same arity has the same blind spot. This test is the half
// of the proof that watches the values.
func TestAdmissionArgValues(t *testing.T) {
	const (
		gpuID = "11111111-1111-1111-1111-111111111111"
		host  = "22222222-2222-2222-2222-222222222222"
		user  = "33333333-3333-3333-3333-333333333333"
		app   = "44444444-4444-4444-4444-444444444444"
		image = "ghcr.io/example/app:1"
	)
	veto := VramAdmission{MinFreeMB: 1024, InflightMB: 512, StalenessSecs: 20}.normalize()
	p := CreateParams{
		UserID: user, AppID: app, NeedEncodeSlots: 2,
		PinHostID: host, AppImage: image,
	}
	c := candidacy{p: p, veto: veto}

	// Every value is distinct, so a swapped pair cannot coincidentally match.
	cases := []struct {
		name string
		args []any
		want []any
	}{
		{
			// slots, stale, floor, debit, then pin, then image.
			name: "candidate/spread",
			args: argsOf(func() (string, []any) { return c.candidateQuery(PolicySpread) }),
			want: []any{int32(2), int32(20), int32(1024), int32(512), host, image},
		},
		{
			// The locality policy inserts its two args after the veto's and
			// before the pin: user id, then the home app id.
			name: "candidate/locality",
			args: argsOf(func() (string, []any) { return c.candidateQuery(PolicyLocality) }),
			want: []any{int32(2), int32(20), int32(1024), int32(512), user, app, host, image},
		},
		{
			// gpu id first here; no pin (the re-check is already keyed on one gpu).
			name: "recheck",
			args: argsOf(func() (string, []any) { return c.recheckQuery(gpuID) }),
			want: []any{gpuID, int32(2), int32(20), int32(1024), int32(512), image},
		},
		{
			// Slots-only plus the image gate: the veto is deliberately absent
			// from the totals check (see totalsQuery).
			name: "totals",
			args: argsOf(func() (string, []any) { return c.totalsQuery() }),
			want: []any{int32(2), image},
		},
		{
			// Binds only what the statement references — the debit estimate is
			// applied in Go, not bound here.
			name: "vetodiag",
			args: argsOf(func() (string, []any) { return c.vetoDiagQuery() }),
			want: []any{int32(2), int32(20), host, image},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if len(tc.args) != len(tc.want) {
				t.Fatalf("bound %d args, want %d\n got: %#v\nwant: %#v",
					len(tc.args), len(tc.want), tc.args, tc.want)
			}
			for i := range tc.want {
				if tc.args[i] != tc.want[i] {
					t.Errorf("$%d = %#v, want %#v", i+1, tc.args[i], tc.want[i])
				}
			}
		})
	}
}

func argsOf(f func() (string, []any)) []any {
	_, args := f()
	return args
}

func sortedOf(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// TestArgsetAllocatesSequentially pins the property the whole file rests on: an
// index is produced by the act of adding a parameter, so it cannot be
// transcribed wrongly.
func TestArgsetAllocatesSequentially(t *testing.T) {
	a := &argset{}
	if got := a.add("first"); got != 1 {
		t.Errorf("first index = %d, want 1 (Postgres placeholders are 1-based)", got)
	}
	if got := a.add(2); got != 2 {
		t.Errorf("second index = %d, want 2", got)
	}
	if got := a.add(nil); got != 3 {
		t.Errorf("third index = %d, want 3", got)
	}
	if got := a.args(); len(got) != 3 || got[0] != "first" || got[1] != 2 {
		t.Errorf("args() = %#v, want the three values in add order", got)
	}
	var zero argset
	if got := zero.add("x"); got != 1 {
		t.Errorf("zero value must be usable: first index = %d, want 1", got)
	}
}

// TestAdmissionArgCountsMatchPlaceholders is the invariant Postgres enforces at
// runtime and that used to be maintained by hand: the number of bound
// parameters must equal the highest placeholder the statement references, with
// no gaps. A bind carrying an unreferenced parameter is rejected outright, and a
// statement referencing an unbound one errors — both of which used to be
// reachable by miscounting.
func TestAdmissionArgCountsMatchPlaceholders(t *testing.T) {
	const (
		gpuID = "11111111-1111-1111-1111-111111111111"
		host  = "22222222-2222-2222-2222-222222222222"
		image = "ghcr.io/example/app:1"
	)
	placeholder := regexp.MustCompile(`\$(\d+)`)

	vetoOn := VramAdmission{MinFreeMB: 1024, InflightMB: 512, StalenessSecs: 20}.normalize()
	vetoOff := VramAdmission{}.normalize()

	for _, veto := range []VramAdmission{vetoOff, vetoOn} {
		for _, pin := range []string{"", host} {
			for _, img := range []string{"", image} {
				p := CreateParams{
					UserID: "u", AppID: "a", NeedEncodeSlots: 1,
					PinHostID: pin, AppImage: img,
				}
				c := candidacy{p: p, veto: veto}

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
				sql, args := c.recheckQuery(gpuID)
				qs = append(qs, q{"recheck", sql, args})
				sql, args = c.totalsQuery()
				qs = append(qs, q{"totals", sql, args})
				sql, args = c.vetoDiagQuery()
				qs = append(qs, q{"vetodiag", sql, args})

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
						" pin=" + boolStr(pin != "") + " image=" + boolStr(img != "")
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

func boolStr(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
