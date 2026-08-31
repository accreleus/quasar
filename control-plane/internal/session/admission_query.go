// admission_query.go — the candidacy predicate and its bind parameters, in one
// place. Four queries decide or explain whether a GPU may host a launch:
//
//	candidateQuery  pick the best-ranked GPU that fits          (scheduler.go step 3)
//	recheckQuery    re-assert that pick under the GPU's lock    (step 5)
//	totalsQuery     "could the fleet EVER serve this?"          (classifyReject)
//	vetoDiagQuery   which GPUs the live-VRAM veto refused       (vetoedCandidates)
//
// They project different shapes and are not unified. What they share is the
// definition of a candidate: encoder/render-node binding, the optional host pin,
// managed-image readiness, free encode slots, and the live free-VRAM veto.
//
// Placeholder indices must never be worked out by hand at a call site. Two
// failures came from that arithmetic: a hard-coded `$3` that sent an int into
// `$3::uuid` and broke every locality launch, and the pick/re-check divergence
// that burns all 50 attempts and reports a spurious capacity error on an idle
// fleet (TestPickAndRecheckAgree). argset returns an index from the act of
// adding the parameter, so one cannot be written down wrongly.
//
// Parameter allocation ORDER at each site is preserved from the pre-refactor
// statements; TestAdmissionSQLMatchesPreRefactor replays 15 captured statements
// from testdata/admission_sql_pre_refactor.txt against it.
package session

import (
	"fmt"
)

// argset accumulates bind parameters and hands back each one's placeholder index
// as it is added, so no caller writes an index literal or derives one from a
// slice length. Indices are 1-based; the zero value is ready to use.
type argset struct {
	vals []any
}

// add appends v and returns the placeholder index it occupies.
func (a *argset) add(v any) int {
	a.vals = append(a.vals, v)
	return len(a.vals)
}

func (a *argset) args() []any { return a.vals }

// candidacy renders the gates deciding whether a GPU is a candidate. It carries
// the launch parameters and resolved veto tuning so the four queries cannot
// disagree about either.
type candidacy struct {
	p    CreateParams
	veto VramAdmission
}

// pinGate restricts candidates to one host (cert bench; a derived tile's hard
// pin). Empty when no pin is set.
func (c candidacy) pinGate(a *argset) string {
	if c.p.PinHostID == "" {
		return ""
	}
	return fmt.Sprintf(" AND h.id = $%d::uuid", a.add(c.p.PinHostID))
}

// imageGate excludes hosts that do not have the app's MANAGED image ready.
// Empty for an app with no image reference; inert for any image that is not an
// installed catalog entry — see imageReadySQL.
func (c candidacy) imageGate(a *argset, lead string) string {
	if c.p.AppImage == "" {
		return ""
	}
	return lead + imageReadySQL(a.add(c.p.AppImage))
}

// vetoGate is the live free-VRAM veto, empty when switched off so the kill
// switch is exactly slots-only and Postgres never gets a bind for an
// unreferenced parameter. staleIdx is passed in because the candidate query
// binds the freshness window once for both the veto and the spread ordering.
func (c candidacy) vetoGate(a *argset, staleIdx int, lead string) string {
	if !c.veto.enabled() {
		return ""
	}
	return lead + vramVetoSQL(staleIdx, a.add(c.veto.MinFreeMB), a.add(c.veto.InflightMB))
}

// candidateQuery picks the best-ranked GPU that can host this launch — an
// unlocked read over `online` hosts only, so draining/offline are never chosen.
//
// Parameter order is load-bearing: slots, freshness window, veto floor and
// debit, policy args, pin, image. The freshness window is always bound even with
// the veto off, because the spread ordering references it.
func (c candidacy) candidateQuery(policy PlacementPolicy) (string, []any) {
	a := &argset{}
	slotsIdx := a.add(c.p.NeedEncodeSlots)
	staleIdx := a.add(c.veto.StalenessSecs)

	vetoClause := c.vetoGate(a, staleIdx, "\n\t\t   AND ")
	policyOrder, policyArgs := policy.policyOrderSQL(c.p, staleIdx, len(a.args())+1)
	for _, v := range policyArgs {
		a.add(v)
	}
	pin := c.pinGate(a)
	image := c.imageGate(a, "\n\t\t  AND ")

	return `
		SELECT g.id::text, g.host_id::text, g.index
		FROM gpus g
		JOIN hosts h ON h.id = g.host_id
		LEFT JOIN sessions s ON s.gpu_id = g.id AND s.state IN ` + activeStatesSQL + `
		WHERE h.status = 'online' AND h.capacity_detection = 'ok' AND g.reported` + schedulableBindingSQL + pin + image + `
		GROUP BY g.id
		HAVING g.encode_slots_total - COALESCE(SUM(s.reserved_encode_slots), 0) >= $` + fmt.Sprint(slotsIdx) + vetoClause + `
		ORDER BY ` + policyOrder + `
		LIMIT 1
	`, a.args()
}

// recheckQuery re-derives ONE gpu's availability under its advisory lock, in a
// fresh statement so the READ COMMITTED snapshot is taken after the lock.
//
// The veto and image gates ride the projected boolean rather than the WHERE, so
// a mismatch reads as "does not fit" and retries. They are rendered by the same
// functions as the candidate query; that identity is what makes the retry loop
// terminate (TestPickAndRecheckAgree).
func (c candidacy) recheckQuery(gpuID string) (string, []any) {
	a := &argset{}
	gpuIdx := a.add(gpuID)
	slotsIdx := a.add(c.p.NeedEncodeSlots)

	vetoClause := ""
	if c.veto.enabled() {
		staleIdx := a.add(c.veto.StalenessSecs)
		vetoClause = c.vetoGate(a, staleIdx, "\n\t\t   AND ")
	}
	image := c.imageGate(a, "\n\t\t   AND ")

	return `
		SELECT h.status = 'online' AND h.capacity_detection = 'ok' AND g.reported
		   AND g.encode_slots_total
		         - COALESCE((SELECT SUM(x.reserved_encode_slots) FROM sessions x
		                     WHERE x.gpu_id = g.id AND x.state IN ` + activeStatesSQL + `), 0) >= $` + fmt.Sprint(slotsIdx) + vetoClause + image + `
		FROM gpus g
		JOIN hosts h ON h.id = g.host_id
		WHERE g.id = $` + fmt.Sprint(gpuIdx) + `::uuid` + schedulableBindingSQL + `
		FOR UPDATE OF h, g
	`, a.args()
}

// totalsQuery answers "could the fleet ever serve this?" — whether any online
// GPU's totals fit, independent of current reservations.
//
// Slots-only, no veto gate: §4.1 abstains whenever vram_mb_total <= floor, so
// such a GPU is servable, and gating here turned ordinary slot exhaustion into a
// non-retryable no_host_available on an APU host. See classifyReject. The image
// gate does belong here.
func (c candidacy) totalsQuery() (string, []any) {
	a := &argset{}
	slotsIdx := a.add(c.p.NeedEncodeSlots)
	image := c.imageGate(a, "\n\t\t\t  AND ")

	return `
		SELECT EXISTS (
			SELECT 1 FROM gpus g JOIN hosts h ON h.id = g.host_id
			WHERE h.status = 'online' AND h.capacity_detection = 'ok' AND g.reported` + schedulableBindingSQL + image + `
			  AND g.encode_slots_total >= $` + fmt.Sprint(slotsIdx) + `
		)
	`, a.args()
}

// vetoDiagQuery lists the GPUs that pass every admission gate except the veto.
// It binds only the parameters the statement references (Postgres rejects an
// unreferenced bind), so the caller applies the per-session debit in Go.
func (c candidacy) vetoDiagQuery() (string, []any) {
	a := &argset{}
	slotsIdx := a.add(c.p.NeedEncodeSlots)
	staleIdx := a.add(c.veto.StalenessSecs)
	pin := c.pinGate(a)
	image := c.imageGate(a, "\n\t\t  AND ")

	return `
		SELECT g.id::text, g.host_id::text, g.index, g.vram_mb_total, g.vram_mb_free,
		       g.vram_sampled_at,
		       (EXTRACT(EPOCH FROM (now() - g.vram_sampled_at)) * 1000)::bigint,
		       (SELECT COUNT(*) FROM sessions x
		         WHERE x.gpu_id = g.id
		           AND x.state IN ('assigned','starting','running','stopping')
		           AND (x.started_at IS NULL
		                OR x.started_at > g.vram_sampled_at - make_interval(secs => $` + fmt.Sprint(staleIdx) + `::int)))::int
		FROM gpus g
		JOIN hosts h ON h.id = g.host_id
		LEFT JOIN sessions s ON s.gpu_id = g.id AND s.state IN ` + activeStatesSQL + `
		WHERE h.status = 'online' AND h.capacity_detection = 'ok' AND g.reported` + schedulableBindingSQL + pin + image + `
		GROUP BY g.id
		HAVING g.encode_slots_total - COALESCE(SUM(s.reserved_encode_slots), 0) >= $` + fmt.Sprint(slotsIdx) + `
		ORDER BY g.id
		LIMIT 16
	`, a.args()
}
