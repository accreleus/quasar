package session

import (
	"fmt"
	"strings"
)

// PlacementPolicy only ORDERS candidate GPUs; admission and the per-GPU
// reservation race stay in ScheduleAndCreate.
type PlacementPolicy int

const (
	// Least-loaded: most free encode slots, then free VRAM, then fewest sessions.
	// The zero value, so a Store built without WithPlacementPolicy uses it.
	PolicySpread PlacementPolicy = iota
	// Prefers the host already holding the launching (user, app)'s home row,
	// falling back to spread when it is full, cordoned or offline.
	PolicyLocality
	// A binpack policy would be a const plus a case in policyOrderSQL and
	// ParsePlacementPolicy, with no caller changes. Not implemented.
)

// ParsePlacementPolicy maps QUASAR_PLACEMENT_POLICY. An unknown value errors, so
// a misconfiguration fails fast at startup rather than falling back.
func ParsePlacementPolicy(s string) (PlacementPolicy, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "spread", "least-loaded":
		return PolicySpread, nil
	case "locality":
		return PolicyLocality, nil
	default:
		return 0, fmt.Errorf("unknown placement policy %q (supported: spread, locality)", s)
	}
}

func (p PlacementPolicy) String() string {
	switch p {
	case PolicySpread:
		return "spread"
	case PolicyLocality:
		return "locality"
	default:
		return fmt.Sprintf("PlacementPolicy(%d)", int(p))
	}
}

// spreadOrderBySQL is shared with PolicyLocality's fallback tail. staleIdx is a
// placeholder index (freshness window, seconds).
//
// The VRAM key ranks only a FRESH sample; a stale one would promote a GPU that
// is actually full. Stale or unknown yields NULL, pushed last, and COUNT/id keep
// the order total.
func spreadOrderBySQL(staleIdx int) string {
	return fmt.Sprintf(`
			(g.encode_slots_total - COALESCE(SUM(s.reserved_encode_slots), 0)) DESC,
			CASE WHEN g.vram_sampled_at > now() - make_interval(secs => $%d::int)
			     THEN g.vram_mb_free END DESC NULLS LAST,
			COUNT(s.id) ASC,
			g.id ASC`, staleIdx)
}

// policyOrderSQL returns the ORDER BY and any extra args; the caller appends
// extraArgs in order. Both placeholder indices are passed in and must never be
// hard-coded: that couples this file to the scheduler's argument layout, and a
// layout change then sends the wrong type into `$n::uuid` on every launch.
func (p PlacementPolicy) policyOrderSQL(cp CreateParams, staleIdx, firstExtraIdx int) (orderBy string, extraArgs []any) {
	switch p {
	case PolicyLocality:
		// last_used_at DESC: after a locality miss a (user, app) can hold homes on
		// two hosts, so pin to the most recently played one. Keyed on
		// cp.homeAppID(), never cp.AppID — a tile has no user_homes row, so the
		// tile id would silently degrade to spread. What actually lands a tile is
		// CreateParams.PinHostID, under both policies.
		return fmt.Sprintf(`CASE WHEN g.host_id = (
				SELECT host_id FROM user_homes
				WHERE user_id = $%d::uuid AND app_id = $%d::uuid AND gc_after IS NULL
				ORDER BY last_used_at DESC
				LIMIT 1
			) THEN 0 ELSE 1 END ASC,`, firstExtraIdx, firstExtraIdx+1) + spreadOrderBySQL(staleIdx),
			[]any{cp.UserID, cp.homeAppID()}
	default: // spread, and any future policy, falls through to spread
		return spreadOrderBySQL(staleIdx), nil
	}
}

// imageReadySQL drops hosts where the app's managed image is not `ready`.
// refIdx carries the app's runtime_spec image reference.
//
// It engages only for a catalog-managed image: otherwise the inner SELECT
// matches nothing, NOT EXISTS is vacuously true, and no existing launch can
// become unplaceable.
//
// Match installed_images.registry_ref (or .local_tag for a template app;
// adoption populates exactly one), never image_catalog.registry_ref — that is
// the upstream offer and moves on every sync, which would make NOT EXISTS
// vacuously true and silently stop filtering. installed_images's ref is frozen
// at adoption (migration 0055 amendment).
//
// Version-aware, but an EMPTY host-row version still counts as ready:
// agent-api.md never requires image_state.version to be non-empty, so that is
// the fail-open floor for an honest agent. A mismatched version is excluded.
//
// One renderer for every candidacy site; see vramVetoSQL for what a pick /
// re-check divergence costs.
func imageReadySQL(refIdx int) string {
	return fmt.Sprintf(`NOT EXISTS (
		    SELECT 1
		      FROM installed_images ii
		     WHERE (ii.registry_ref = $%[1]d OR ii.local_tag = $%[1]d)
		       AND NOT EXISTS (
		           SELECT 1 FROM host_images hi
		            WHERE hi.host_id = g.host_id
		              AND hi.image_id = ii.image_id
		              AND hi.state = 'ready'
		              AND (hi.version = '' OR hi.version = ii.version)
		       )
		)`, refIdx)
}

// StoreOption configures a Store at construction.
type StoreOption func(*Store)

// WithPlacementPolicy sets the multi-host placement policy (default PolicySpread).
func WithPlacementPolicy(p PlacementPolicy) StoreOption {
	return func(s *Store) { s.policy = p }
}

// VramAdmission tunes the live free-VRAM veto (#383 §4.3), from
// QUASAR_VRAM_{MIN_FREE_MB,INFLIGHT_ESTIMATE_MB,STALENESS_SECS}.
type VramAdmission struct {
	// The floor a GPU's live free VRAM must clear. <= 0 disables the veto by
	// OMITTING the clause rather than neutering it, so the kill switch leaves no
	// residual behaviour.
	MinFreeMB int32
	// Per-session debit for launches the latest sample cannot reflect yet.
	// <= 0 falls back to MinFreeMB.
	InflightMB int32
	// Freshness window and the debit's grace margin; <= 0 falls back below.
	StalenessSecs int32
}

// defaultVramStalenessSecs mirrors config's default (4x the 5 s heartbeat) so a
// Store built without WithVramAdmission still renders valid interval SQL. The
// floor is not defaulted: veto OFF is the fail-open zero value.
const defaultVramStalenessSecs = 20

func (v VramAdmission) normalize() VramAdmission {
	if v.StalenessSecs <= 0 {
		v.StalenessSecs = defaultVramStalenessSecs
	}
	if v.InflightMB <= 0 {
		v.InflightMB = v.MinFreeMB
	}
	return v
}

func (v VramAdmission) enabled() bool { return v.MinFreeMB > 0 }

// WithVramAdmission tunes the veto; unset leaves it disabled (slots-only).
func WithVramAdmission(v VramAdmission) StoreOption {
	return func(s *Store) { s.vram = v.normalize() }
}

// vramVetoSQL renders the live-VRAM veto (#383 §4.1). staleSecs/minFree/inflight
// are placeholder indices, not values.
//
// Exactly ONE renderer: ScheduleAndCreate terminates only if the candidate query
// and the under-lock re-check apply the same predicate, and a divergence picks
// then rejects a GPU under its own lock forever, burning all 50 attempts for a
// spurious capacity_exhausted on an idle fleet. Its single caller,
// candidacy.vetoGate, composes it into both queries.
//
// Every disjunct but the last is an abstain path: the veto is advisory, refusing
// a GPU already out of memory rather than allocating (slots are the
// reservation). Unmeasurable telemetry must fail OPEN — never sampled, sampled
// outside the freshness window (a dead sampler must not slowly strangle the
// host), or vram_mb_total <= floor, where the floor exceeds the whole pool and
// the pool is not the workload's budget (an AMD APU's UMA carve-out; abstaining
// structurally beats fragile APU detection).
//
// `make_interval(secs => $n::int)` with an integer parameter, never
// `$n::interval`: a NULL interval makes the clause NULL, which HAVING treats as
// false — fail-CLOSED, the exact inversion of the property above.
//
// The in-flight debit keys on started_at, not assigned_at: a session admitted at
// t0 has allocated nothing when the t0+3s sample is taken, and an assigned_at
// key would drop it while it is still invisible in the sample. One staleness
// window of grace covers running -> steady-state allocation; the debit
// self-corrects once the memory shows up in a later sample.
//
// `stopping` is in the state set although activeStatesSQL excludes it from
// reservations: migration 0029 records that a stopping pipeline still holds
// Vulkan image refs. Reservation and residency are different questions.
func vramVetoSQL(staleSecs, minFree, inflight int) string {
	return fmt.Sprintf(`(
	     g.vram_mb_free IS NULL
	  OR g.vram_sampled_at IS NULL
	  OR g.vram_sampled_at < now() - make_interval(secs => $%[1]d::int)
	  OR g.vram_mb_total <= $%[2]d
	  OR g.vram_mb_free - (
	        SELECT COUNT(*) * $%[3]d FROM sessions x
	         WHERE x.gpu_id = g.id
	           AND x.state IN ('assigned','starting','running','stopping')
	           AND (x.started_at IS NULL
	                OR x.started_at > g.vram_sampled_at - make_interval(secs => $%[1]d::int))
	     ) >= $%[2]d
	)`, staleSecs, minFree, inflight)
}
