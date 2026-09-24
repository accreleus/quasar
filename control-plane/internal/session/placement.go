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
//
// Key order is control-api.md "Admission control" (amendment 12): locality, then
// the codec preference, then spread. The preference renders and binds only when
// set, so a launch without one sends the statement it always did.
func (p PlacementPolicy) policyOrderSQL(cp CreateParams, staleIdx, firstExtraIdx int) (orderBy string, extraArgs []any) {
	next := firstExtraIdx
	switch p {
	case PolicyLocality:
		// last_used_at DESC: after a locality miss a (user, app) can hold homes on
		// two hosts, so pin to the most recently played one. Keyed on
		// cp.homeAppID(), never cp.AppID — a tile has no user_homes row, so the
		// tile id would silently degrade to spread. What actually lands a tile is
		// CreateParams.PinHostID, under both policies.
		orderBy = fmt.Sprintf(`CASE WHEN g.host_id = (
				SELECT host_id FROM user_homes
				WHERE user_id = $%d::uuid AND app_id = $%d::uuid AND gc_after IS NULL
				ORDER BY last_used_at DESC
				LIMIT 1
			) THEN 0 ELSE 1 END ASC,`, next, next+1)
		extraArgs = []any{cp.UserID, cp.homeAppID()}
		next += 2
	default: // spread, and any future policy, falls through to spread
	}
	if len(cp.CodecPreference) > 0 {
		orderBy += codecPreferenceOrderSQL(next)
		extraArgs = append(extraArgs, cp.CodecPreference)
	}
	return orderBy + spreadOrderBySQL(staleIdx), extraArgs
}

// codecPreferenceOrderSQL ranks a GPU by the preference position of the best
// codec in its set; a GPU with none of them ranks after every GPU with any. An
// ORDER BY key only, never a filter. prefIdx binds the preference as text[].
//
// It joins hosts itself because the candidate query groups by g.id, so the
// outer h.codecs is not referenceable here.
func codecPreferenceOrderSQL(prefIdx int) string {
	return fmt.Sprintf(`
			COALESCE((
				SELECT MIN(w.ord)
				FROM hosts ph, unnest($%[1]d::text[]) WITH ORDINALITY AS w(codec, ord)
				WHERE ph.id = g.host_id AND %[2]s ? w.codec
			), cardinality($%[1]d::text[]) + 1) ASC,`, prefIdx, gpuCodecSetSQL("g", "ph", true))
}

// imageReadySQL drops hosts where the app's managed image is not `ready`.
// refIdx carries the app's runtime_spec image reference.
//
// The adoption readiness check engages only for an installed managed image.
// A separate durable-attempt check keeps an exact ref unavailable after a
// catalog-pruned managed version was removed, until later verified readiness.
// A genuinely unmanaged ref remains launchable under its existing rules.
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
		              AND NOT EXISTS (
		                  SELECT 1 FROM host_image_operation_fences f
		                   WHERE f.host_id = hi.host_id
		                     AND f.image_id = hi.image_id
		                     AND f.state = 'removing'
		              )
		       )
		) %s AND NOT %s`, refIdx, imageCleanupIdentityFenceSQL(refIdx),
		removedManagedImageUnreadySQL("g.host_id", fmt.Sprintf("$%d", refIdx)))
}

// A successfully removed exact ref stays unavailable after catalog/adoption
// pruning. Only a later managed adoption plus a ready report for that same
// ref proves it was prepared again. The report must be newer than the
// terminal attempt; a ready row retained from before deletion is no proof.
func removedManagedImageUnreadySQL(hostExpr, refExpr string) string {
	return fmt.Sprintf(`EXISTS (
		SELECT 1 FROM host_image_cleanup_attempts a
		WHERE a.host_id=%[1]s AND a.image_ref=%[2]s AND a.state='removed'
		AND NOT EXISTS (
			SELECT 1 FROM installed_images ii JOIN host_images hi
			ON hi.image_id=ii.image_id AND hi.host_id=a.host_id
			WHERE (ii.registry_ref=%[2]s OR ii.local_tag=%[2]s)
			AND hi.state='ready' AND (hi.version='' OR hi.version=ii.version)
			AND hi.updated_at>a.updated_at
		)
	)`, hostExpr, refExpr)
}

// Active cleanup of a catalog-pruned/uninstalled managed version is still an
// image availability gate. Its exact ref survives in the durable attempt or
// successful-version history, even after installed_images disappears. This is
// rendered in every candidacy query, including the totals probe that decides
// no_host_available versus capacity_exhausted.
func imageCleanupIdentityFenceSQL(refIdx int) string {
	return fmt.Sprintf(`AND NOT EXISTS (
		SELECT 1 FROM host_image_operation_fences f
		WHERE f.host_id=g.host_id AND f.state='removing'
		AND (EXISTS(SELECT 1 FROM host_image_cleanup_attempts a
			WHERE a.host_id=f.host_id AND a.image_id=f.image_id AND a.image_ref=$%[1]d)
		OR EXISTS(SELECT 1 FROM host_image_success_history h
			WHERE h.host_id=f.host_id AND h.image_id=f.image_id
			AND (COALESCE(NULLIF(h.current_identity->>'registry_ref',''),NULLIF(h.current_identity->>'local_tag',''))=$%[1]d
				OR COALESCE(NULLIF(h.previous_identity->>'registry_ref',''),NULLIF(h.previous_identity->>'local_tag',''))=$%[1]d)))
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

// ReadinessAdmission tunes the evidence-gated readiness filter: the freshness
// window, from QUASAR_READINESS_STALE_SECS.
//
// The zero value renders no clause at all. That is not an operator-facing kill
// switch — the contract defines none, and NewStore always defaults the window —
// it is what keeps the pre-gate SQL anchors reachable from a test-built
// candidacy (TestAdmissionSQLMatchesPreRefactor).
type ReadinessAdmission struct {
	StaleSecs int32
}

// defaultReadinessStaleSecs is the contract's default: four missed 15 s reports.
const defaultReadinessStaleSecs = 60

func (r ReadinessAdmission) enabled() bool { return r.StaleSecs > 0 }

// WithReadinessStaleSecs sets the gate's freshness window in seconds; <= 0 is
// the default, never "off".
func WithReadinessStaleSecs(n int) StoreOption {
	return func(s *Store) {
		if n <= 0 {
			n = defaultReadinessStaleSecs
		}
		s.readiness = ReadinessAdmission{StaleSecs: int32(n)}
	}
}

// readinessGateSQL renders the evidence-gated readiness filter (control-api.md
// "Evidence-gated readiness"). staleIdx is a placeholder index, not a value.
//
// Exactly one renderer, for the same reason as vramVetoSQL: a pick / re-check
// divergence burns all 50 attempts and reports a spurious capacity error.
//
// Fails open on absent or stale evidence — a host that has gone quiet is
// already excluded by not being `online`, and stale evidence must not strand a
// fleet. `make_interval(secs => $n::int)` with an integer parameter, never
// `$n::interval`: a NULL interval would make the clause NULL, which WHERE and
// HAVING treat as false — fail-closed, the exact inversion.
//
// The homes term is rendered only for a launch that mounts a managed home, so
// no parameter is bound that the statement does not reference.
func readinessGateSQL(staleIdx int, managedHome bool) string {
	homes := ""
	if managedHome {
		homes = " OR h.readiness_block_homes"
	}
	return fmt.Sprintf(`(
	     h.readiness IS NULL
	  OR h.readiness_reported_at IS NULL
	  OR h.readiness_reported_at < now() - make_interval(secs => $%d::int)
	  OR NOT (h.readiness_block_host OR g.readiness_blocked%s)
	)`, staleIdx, homes)
}

// vramVetoSQL renders the live-VRAM veto (#383 §4.1). staleSecs/minFree/inflight
// are placeholder indices, not values.
//
// Exactly one renderer: ScheduleAndCreate terminates only if the candidate query
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
// false — fail-closed, the exact inversion of the property above.
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

// gpuCodecSetSQL renders the GPU codec set (#296 amendment 12, schema.md
// gpus.codecs / hosts.codecs): a GPU's own reported set, or its host's when
// the GPU reports none. g/h are the query's table aliases (gpus/hosts).
//
// Exactly one renderer, for the reason vramVetoSQL is: every read of a GPU's
// codec set — the launch-side candidacy gate and preference (#303), the
// profile menu union, the admin GPU list — must resolve the inheritance
// identically or two call sites can disagree about what a GPU can encode.
//
// fallbackH264 selects the renderer's only two meanings, per the contract:
// the launch-side read falls all the way to `["h264"]` when neither the GPU
// nor its host has ever reported (an unencodable codec is a dead session, so
// launch placement must never see "unknown" as "anything goes"); the admin
// read (openapi.yaml GPUAvailability.codecs) stays NULL in that case, because
// "never reported" and "reported h264 only" want different operator advice.
//
// COALESCE only treats SQL NULL as absent: a GPU that explicitly reports `[]`
// (a zero-slot GPU, agent-api.md) is not NULL and does not inherit — it reads
// back as codecs=[]. That is a placement no-op (zero slots is not a
// candidate); the admin list renders it as an explicit empty set.
func gpuCodecSetSQL(g, h string, fallbackH264 bool) string {
	if fallbackH264 {
		return fmt.Sprintf(`COALESCE(%s.codecs, %s.codecs, '["h264"]'::jsonb)`, g, h)
	}
	return fmt.Sprintf(`COALESCE(%s.codecs, %s.codecs)`, g, h)
}

// gpuCodecSet is gpuCodecSetSQL(fallbackH264=true)'s pure twin: the launch-side
// read. nil means the column stored SQL NULL (never reported); a non-nil empty
// slice is a real report of zero codecs and is returned as-is, never promoted
// to the fallback. Guarded against the SQL by TestGPUCodecSetMatchesSQL.
func gpuCodecSet(gpuCodecs, hostCodecs []string) []string {
	if gpuCodecs != nil {
		return gpuCodecs
	}
	if hostCodecs != nil {
		return hostCodecs
	}
	return []string{wireCodecH264}
}

// gpuCodecSetNullable is gpuCodecSetSQL(fallbackH264=false)'s twin: the admin
// read, nil only when neither this GPU nor its host has ever reported.
func gpuCodecSetNullable(gpuCodecs, hostCodecs []string) []string {
	if gpuCodecs != nil {
		return gpuCodecs
	}
	return hostCodecs
}
