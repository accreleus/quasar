package session

// codec_preference_query_test.go — pure tests for where the codec preference
// (#305) renders in the admission SQL. No database.

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// TestCodecPreferenceOnlyOrdersTheCandidateQuery: the preference is an ORDER BY
// key in the pick and nothing else. An empty one, nil or not, renders and binds
// nothing, so an Auto launch without one sends the old statement.
func TestCodecPreferenceOnlyOrdersTheCandidateQuery(t *testing.T) {
	const host = "22222222-2222-2222-2222-222222222222"
	veto := VramAdmission{MinFreeMB: 1024, InflightMB: 512, StalenessSecs: 20}.normalize()
	ready := ReadinessAdmission{StaleSecs: 60}
	base := CreateParams{UserID: "u", AppID: "a", NeedEncodeSlots: 1, PinHostID: host}
	with := func(pref []string) candidacy {
		p := base
		p.CodecPreference = pref
		return candidacy{p: p, veto: veto, readiness: ready}
	}
	plain, preferred := with(nil), with([]string{"av1", "h265"})

	others := map[string]func(c candidacy) (string, []any){
		"recheck":         func(c candidacy) (string, []any) { return c.recheckQuery("g") },
		"totals":          candidacy.totalsQuery,
		"vetodiag":        candidacy.vetoDiagQuery,
		"readinessdiag":   candidacy.readinessDiagQuery,
		"readinesstotals": candidacy.readinessTotalsQuery,
	}
	for name, q := range others {
		ps, pa := q(plain)
		ws, wa := q(preferred)
		if ps != ws || !reflect.DeepEqual(pa, wa) {
			t.Errorf("%s changed with a codec preference; only the candidate query may see it", name)
		}
	}

	for _, policy := range []PlacementPolicy{PolicySpread, PolicyLocality} {
		ps, pa := plain.candidateQuery(policy)
		for _, empty := range [][]string{nil, {}} {
			es, ea := with(empty).candidateQuery(policy)
			if es != ps || !reflect.DeepEqual(ea, pa) {
				t.Errorf("%s: an empty preference (%#v) changed the candidate query", policy, empty)
			}
		}
		ws, _ := preferred.candidateQuery(policy)
		if !strings.Contains(ws, "WITH ORDINALITY") || strings.Contains(ps, "WITH ORDINALITY") {
			t.Errorf("%s: the preference key must render exactly when a preference is set", policy)
		}
	}
}

// TestCodecPreferenceKeyOrder pins amendment 12's ordering: locality, then the
// preference, then the spread keys.
func TestCodecPreferenceKeyOrder(t *testing.T) {
	c := candidacy{p: CreateParams{UserID: "u", AppID: "a", NeedEncodeSlots: 1,
		CodecPreference: []string{"av1", "h264"}}}
	for _, policy := range []PlacementPolicy{PolicySpread, PolicyLocality} {
		sql, _ := c.candidateQuery(policy)
		order := sql[strings.Index(sql, "ORDER BY"):]
		pref := strings.Index(order, "WITH ORDINALITY")
		spread := strings.Index(order, "(g.encode_slots_total - COALESCE(SUM(s.reserved_encode_slots), 0)) DESC")
		if pref < 0 || spread < 0 || pref > spread {
			t.Errorf("%s: the preference must rank before the spread keys:\n%s", policy, order)
		}
		if policy == PolicyLocality {
			if loc := strings.Index(order, "FROM user_homes"); loc < 0 || loc > pref {
				t.Errorf("locality must rank before the preference:\n%s", order)
			}
		}
	}
}

// TestCodecPreferenceArgValues: the preference binds right after the policy's
// own args, as one text[] value, before the pin and the image.
func TestCodecPreferenceArgValues(t *testing.T) {
	const (
		host  = "22222222-2222-2222-2222-222222222222"
		user  = "33333333-3333-3333-3333-333333333333"
		app   = "44444444-4444-4444-4444-444444444444"
		image = "ghcr.io/example/app:1"
	)
	pref := []string{"av1", "h265", "h264"}
	veto := VramAdmission{MinFreeMB: 1024, InflightMB: 512, StalenessSecs: 20}.normalize()
	c := candidacy{p: CreateParams{UserID: user, AppID: app, NeedEncodeSlots: 2,
		PinHostID: host, AppImage: image, CodecPreference: pref}, veto: veto}

	for _, tc := range []struct {
		policy PlacementPolicy
		idx    int
		want   []any
	}{
		{PolicySpread, 5, []any{int32(2), int32(20), int32(1024), int32(512), pref, host, image, app}},
		{PolicyLocality, 7, []any{int32(2), int32(20), int32(1024), int32(512), user, app, pref, host, image, app}},
	} {
		sql, args := c.candidateQuery(tc.policy)
		if !reflect.DeepEqual(args, tc.want) {
			t.Errorf("%s: args = %#v, want %#v", tc.policy, args, tc.want)
		}
		if want := fmt.Sprintf("unnest($%d::text[])", tc.idx); !strings.Contains(sql, want) {
			t.Errorf("%s: the key does not reference the preference at $%d:\n%s", tc.policy, tc.idx, sql)
		}
	}
}
