package session

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// CreateParams are the resolved inputs to a launch: the owner, the app, the
// (already defaulted) stream parameters, the resources to reserve, and the
// minted signaling-token hash.
type CreateParams struct {
	UserID      string
	AppID       string
	Width       int32
	Height      int32
	FPS         int32
	BitrateKbps int32
	H264Profile string
	// Wire vocabulary; empty is the h264 floor (the INSERT coalesces). The launch
	// path inserts the floor and lifts sessions.codec after placement, since the
	// host-codec clamp needs the placed host.
	Codec string
	// The GRANTED mic state, resolved by the caller before it reaches here.
	// Console/local_only launches never set it: no WebRTC pipeline.
	Mic bool
	// The selected launch-profile id, "" for a legacy/tier/override launch.
	// Persisted as NULL when empty.
	ProfileID  string
	Playout0Ms int32
	// The only per-GPU resource a launch reserves (#383): declared per-app VRAM
	// was never a cap and under-declaring silently oversubscribed. Live free VRAM
	// is an independent advisory veto on top — see vramVetoSQL.
	NeedEncodeSlots int32
	// SkipVramVeto omits the veto (#383 §4.4). Console auto-start only: a launch
	// failure feeds consoleBackoff, so after consoleBackoffMaxRetries a transient
	// veto would permanently kill the local console with only an error log. The
	// cert bench must NOT set it — a VRAM-pressured host is a real finding.
	SkipVramVeto bool
	TokenHash    string
	TokenExpires time.Time
	// The EFFECTIVE runtime app's opt-in to persistent home storage (P5-02); true
	// makes scheduleAttempt enforce the P5-04 single-writer guard. Must come from
	// LaunchApp.ManagedHome, never apps.managed_home: a tile's own column is false
	// by apps_derived_shape_ck, so copying it switches the guard off for exactly
	// the apps it protects.
	ManagedHome bool
	// The identity a launch's home is provisioned, locked and placed under: the
	// PARENT for a tile, the app itself otherwise; empty falls back to AppID. Kept
	// separate because AppID is what the session row records and what entitlement
	// authorizes, and collapsing them breaks either per-tile entitlement or the
	// single-writer guard.
	HomeAppID string
	// The effective runtime app's image ref, for the image placement filter.
	// Empty, or any image that is not an installed catalog entry, disables it.
	AppImage string
	// PinHostID restricts the scheduler to one host's GPUs. Placement policy and
	// capacity within it are unchanged.
	PinHostID string
}

// homeAppID is the storage key for this launch. Every storage-keyed site on this
// path goes through it rather than p.AppID.
func (p CreateParams) homeAppID() string {
	if p.HomeAppID != "" {
		return p.HomeAppID
	}
	return p.AppID
}

// maxPlacementAttempts bounds the retry loop. A retry happens only when the
// chosen GPU fills between the unlocked pick and its lock, so each retry makes
// progress and the loop ends once nothing fits (ErrCapacityExhausted). The bound
// is a backstop against pathological churn, far above any real fleet's GPU count.
const maxPlacementAttempts = 50

// schedulableBindingSQL mirrors the agent's binding resolver (bind_assignment):
// once effective settings are reported, hardware work goes only to the GPU whose
// render node and vendor can realize that host configuration. An empty or absent
// render_node (jsonb ->> yields NULL; a fresh deploy leaves QUASAR_RENDER_NODE
// unset) means unpinned — any vendor-compatible GPU — and the agent adopts the
// scheduled GPU's node; the two resolvers must not diverge. A non-empty value
// exact-matches, so 'software' stays unschedulable for hardware encoders.
const schedulableBindingSQL = ` AND (
	COALESCE(h.effective_settings->>'encoder', '') = ''
	OR h.effective_settings->>'encoder' = 'openh264'
	OR (h.effective_settings->>'encoder' = 'va'
		AND lower(g.vendor) IN ('amd','intel')
		AND (COALESCE(h.effective_settings->>'render_node', '') = ''
			OR g.render_node = h.effective_settings->>'render_node'
			OR g.device_path = h.effective_settings->>'render_node'))
	OR (h.effective_settings->>'encoder' = 'nvenc'
		AND lower(g.vendor) = 'nvidia'
		AND (COALESCE(h.effective_settings->>'render_node', '') = ''
			OR g.render_node = h.effective_settings->>'render_node'
			OR g.device_path = h.effective_settings->>'render_node'))
	OR (h.effective_settings->>'encoder' = 'vulkan'
		AND g.index = 0
		AND (COALESCE(h.effective_settings->>'render_node', '') = ''
			OR g.render_node = h.effective_settings->>'render_node'
			OR g.device_path = h.effective_settings->>'render_node'))
)`

// Advisory-lock namespaces: pg_advisory_xact_lock keys are scoped by the class,
// so a user-id and a gpu-id hash that collide as ints still take distinct locks.
// A launch must always take lockNamespaceUser before lockNamespaceGPU — that
// total class order is what makes the pair deadlock-free.
const (
	lockNamespaceUser = 1
	lockNamespaceGPU  = 2
)

// attemptObserver reports each attempt's 0-based index; test-only, nil in
// production. It makes a pick/re-check predicate divergence loud instead of
// silent — see TestPickAndRecheckAgree.
var attemptObserver func(attempt int)

// ScheduleAndCreate picks an online host+GPU whose derived availability
// satisfies the request, ranked by the Store's PlacementPolicy, reserves
// transactionally and inserts as `assigned`. On rejection it persists no row;
// gate order and error semantics are control-api.md §Admission control.
//
// Availability is derived from the sum of reservations held by active sessions
// (schema.md §gpus) — a launch reserves by INSERTing a sessions row, never by
// UPDATEing gpus — so availability itself cannot be locked. Two advisory locks,
// each held for one attempt's transaction, make it race-free: per-user, so a
// user's concurrent launches cannot race the quota COUNT, and per-GPU, so two
// launches on one GPU cannot both pass availability and overcommit it.
// Different users and different GPUs never contend.
//
// The GPU lock is taken after an unlocked pick, so the GPU can fill in between
// and the under-lock re-check retries. The user lock is always acquired first —
// a consistent class order, so the two cannot deadlock.
func (s *Store) ScheduleAndCreate(ctx context.Context, p CreateParams) (Session, error) {
	for attempt := 0; attempt < maxPlacementAttempts; attempt++ {
		if attemptObserver != nil {
			attemptObserver(attempt)
		}
		sess, retry, err := s.scheduleAttempt(ctx, p)
		if !retry {
			return sess, err
		}
	}
	// Retry budget exhausted under sustained same-GPU contention: report the
	// retryable "busy" code.
	return Session{}, ErrCapacityExhausted
}

// scheduleAttempt runs one placement transaction. It returns retry=true (with a
// nil error) when the chosen GPU filled between pick and lock and the caller
// should try again from scratch; otherwise it returns the created session or a
// terminal error.
func (s *Store) scheduleAttempt(ctx context.Context, p CreateParams) (_ Session, retry bool, _ error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Session{}, false, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// (1) Per-user lock: makes the quota check below race-free. Released with the tx.
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock($1, hashtext($2::text))`, lockNamespaceUser, p.UserID,
	); err != nil {
		return Session{}, false, fmt.Errorf("lock user: %w", err)
	}

	// (1b) Entitlement: the authorization boundary. A hand-copy of entitledSQL
	// (internal/crud/store.go, the definition of record) plus FOR SHARE, copied
	// because session cannot import crud. LaunchByProfile's pre-check is only for
	// error ORDERING; this is the enforcement and must never be removed because
	// the handler already checked.
	//
	// It lives here because scheduleAttempt is the single chokepoint every created
	// session passes through, so a future fourth caller inherits it. Guarding
	// LaunchByProfile instead leaves the operator-triggered paths open — the shape
	// of the cb97bfb admission-bypass incident.
	//
	// FOR SHARE, never a plain EXISTS: under READ COMMITTED each statement takes a
	// fresh snapshot, so check and INSERT could straddle a revoke's commit. The
	// share lock leaves only two orderings, both correct, and shared locks do not
	// conflict, so only a revoke ever waits.
	//
	// Before the quota check and before placement: authorization precedes resource
	// accounting, and nothing should be reserved for a launch that will be
	// refused. No role arm, ever (§6.5) — an admin grants themselves the
	// entitlement via POST /v1/admin/apps/{id}/entitlements, leaving an audit row.
	//
	// The params are cast, not the columns, so entitlements_all_uk /
	// entitlements_user_uk stay usable.
	var one int
	err = tx.QueryRow(ctx, `
		SELECT 1 FROM entitlements e
		WHERE e.app_id = $1::uuid
		  AND (e.subject_type = 'all'
		       OR (e.subject_type = 'user' AND e.subject_id = $2::uuid))
		LIMIT 1
		FOR SHARE`, p.AppID, p.UserID).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, false, ErrNotEntitled
	}
	if err != nil {
		return Session{}, false, fmt.Errorf("check entitlement: %w", err)
	}

	// (2) Per-user concurrent-session quota (contract gate #1, before capacity).
	// "active" includes pending on top of the reservation set, so a user cannot
	// evade the cap by launching faster than the scheduler places.
	var userLimit, activeCount int32
	if err := tx.QueryRow(ctx,
		`SELECT max_concurrent_sessions FROM users WHERE id = $1::uuid`, p.UserID,
	).Scan(&userLimit); err != nil {
		return Session{}, false, fmt.Errorf("read user quota: %w", err)
	}
	if err := tx.QueryRow(ctx, `
		SELECT COUNT(*) FROM sessions
		WHERE user_id = $1::uuid
		  AND state IN ('pending','assigned','starting','running')
	`, p.UserID).Scan(&activeCount); err != nil {
		return Session{}, false, fmt.Errorf("count active sessions: %w", err)
	}
	if activeCount >= userLimit {
		return Session{}, false, ErrSessionQuotaExceeded
	}

	// (2b) Per-(user, home) single-writer guard for managed-home apps (P5-04),
	// serialized by the per-user lock above. Both halves are load-bearing: the
	// gate reads the EFFECTIVE app's managed_home (see CreateParams.ManagedHome),
	// and the key is p.homeAppID() compared over the whole family
	// (liveHomeSessionSQL) so parent-vs-tile and tile-vs-sibling-tile both
	// collide. Two Steam clients on one steamapps tree is the corruption at stake.
	//
	// excludeID is "": a launch excludes nothing, unlike the swap path.
	if p.ManagedHome {
		var conflictID string
		err := tx.QueryRow(ctx, liveHomeSessionSQL, p.UserID, p.homeAppID(), "").Scan(&conflictID)
		switch {
		case err == nil:
			// The 409 body must carry the live session's id so the client can link
			// to it (§2.1).
			return Session{}, false, &HomeInUseError{SessionID: conflictID}
		case errors.Is(err, pgx.ErrNoRows):
		default:
			return Session{}, false, fmt.Errorf("count same-home sessions: %w", err)
		}
	}

	// (3) Pick the best candidate GPU by policy (contract gate #2) — an unlocked
	// read of derived availability across `online` hosts only, so draining and
	// offline hosts are never chosen. The gates, parameters and placeholder
	// indices come from `candidacy` (admission_query.go), the same object the
	// under-lock re-check and the rejection classifier use.
	veto := s.vramVeto(p)
	cand := candidacy{p: p, veto: veto}

	candidateSQL, candidateArgs := cand.candidateQuery(s.policy)
	var gpuID, hostID string
	var gpuIndex int32
	err = tx.QueryRow(ctx, candidateSQL, candidateArgs...).Scan(&gpuID, &hostID, &gpuIndex)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, false, classifyReject(ctx, tx, cand)
	}
	if err != nil {
		return Session{}, false, fmt.Errorf("pick gpu: %w", err)
	}

	// (4) Per-GPU lock: serializes launches on THIS gpu only.
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock($1, hashtext($2::text))`, lockNamespaceGPU, gpuID,
	); err != nil {
		return Session{}, false, fmt.Errorf("lock gpu: %w", err)
	}

	// (5) Re-derive THIS gpu's availability under the lock, in a fresh statement so
	// the READ COMMITTED snapshot is taken after it. A launch that reserved here
	// and released its lock first is now visible; if it took the room, retry.
	//
	// The gates come from the SAME candidacy as the pick above, only numbered
	// differently. That identity is what makes the retry loop terminate — see
	// TestPickAndRecheckAgree.
	recheckSQL, recheckArgs := cand.recheckQuery(gpuID)
	var fits bool
	err = tx.QueryRow(ctx, recheckSQL, recheckArgs...).Scan(&fits)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, true, nil // inventory changed after candidate selection
	}
	if err != nil {
		return Session{}, false, fmt.Errorf("recheck gpu: %w", err)
	}
	if !fits {
		return Session{}, true, nil // raced; retry from scratch
	}

	// (6) Insert placed + reserved, as `assigned`: the reservation exists by this
	// row's active state being counted in the availability sums above, and the
	// per-GPU lock keeps anyone else from reserving here until we commit.
	//
	// reserved_vram_mb is absent from the column list, taking its DEFAULT 0: #383
	// removed declared VRAM from admission. The column stays for historical rows.
	sess, err := scanSession(tx.QueryRow(ctx, `
		INSERT INTO sessions (
		    user_id, app_id, host_id, gpu_id, state,
		    width, height, fps, bitrate_kbps, h264_profile,
		    codec,
		    profile_id,
		    playout0_ms,
		    reserved_encode_slots,
		    signaling_token_hash, signaling_token_expires_at,
		    mic,
		    assigned_at
		) VALUES (
		    $1, $2, $3, $4, 'assigned',
		    $5, $6, $7, $8, $9,
		    COALESCE(NULLIF($15, ''), 'h264'),
		    NULLIF($14, ''),
		    $10,
		    $11,
		    NULLIF($12, ''), $13,
		    $16,
		    now()
		)
		RETURNING `+sessionCols,
		p.UserID, p.AppID, hostID, gpuID,
		p.Width, p.Height, p.FPS, p.BitrateKbps, p.H264Profile,
		p.Playout0Ms,
		p.NeedEncodeSlots,
		p.TokenHash, p.TokenExpires,
		p.ProfileID,
		p.Codec,
		p.Mic,
	))
	if err != nil {
		return Session{}, false, fmt.Errorf("insert session: %w", err)
	}
	// Internal launches omit browser signaling; only persist a minted token.
	if p.TokenHash != "" {
		if _, err := tx.Exec(ctx, `
			INSERT INTO session_tokens (session_id, token_hash, expires_at)
			VALUES ($1, $2, $3)
		`, sess.ID, p.TokenHash, p.TokenExpires); err != nil {
			return Session{}, false, fmt.Errorf("insert signaling token: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return Session{}, false, fmt.Errorf("commit: %w", err)
	}

	sess.GPUIndex = &gpuIndex
	return sess, false, nil
}

// vramVeto resolves one launch's veto config: the Store's normalized tuning, or
// the veto off when the caller bypasses it. Returning a value rather than
// branching per use site keeps pick, re-check and classifyReject on one set of
// numbers.
func (s *Store) vramVeto(p CreateParams) VramAdmission {
	if p.SkipVramVeto {
		return VramAdmission{}.normalize()
	}
	return s.vram
}

// classifyReject splits the contract's two 503s: no online GPU whose TOTALS fit
// ⇒ ErrNoHostAvailable, vs totals fit but availability does not ⇒
// ErrCapacityExhausted (retryable).
//
// The totals check must stay slots-only. The VRAM floor here contradicts §4.1's
// structural abstain (a GPU with vram_mb_total <= floor is never vetoed, so it
// is servable) and was proved wrong live on an APU host reporting a 512 MB UMA
// carve-out against the 1024 MB default floor: ordinary slot exhaustion came
// back as a non-retryable no_host_available.
func classifyReject(ctx context.Context, tx pgx.Tx, cand candidacy) error {
	// The image gate DOES belong here, unlike the VRAM floor: a fleet where no
	// host has the app's managed image ready genuinely cannot serve the launch.
	// totalsQuery encodes both that inclusion and the veto's exclusion.
	totalsSQL, classifyArgs := cand.totalsQuery()
	var totalsFit bool
	if err := tx.QueryRow(ctx, totalsSQL, classifyArgs...).Scan(&totalsFit); err != nil {
		return fmt.Errorf("classify rejection: %w", err)
	}
	if !totalsFit {
		return ErrNoHostAvailable
	}
	// Totals fit, so something transient refused it. If the veto did, attach the
	// numbers: otherwise a misconfigured floor is indistinguishable from plain
	// slot exhaustion and becomes an unexplainable 503.
	if cand.veto.enabled() {
		if cands, err := vetoedCandidates(ctx, tx, cand); err == nil && len(cands) > 0 {
			return &VramVetoRejection{err: ErrCapacityExhausted, Veto: cand.veto, Candidates: cands}
		}
	}
	return ErrCapacityExhausted
}

// VramVetoCandidate is one GPU that had free encode slots and was refused by the
// veto: the numbers that tell a misconfigured floor from a really-full GPU.
type VramVetoCandidate struct {
	GPUID       string
	HostID      string
	GPUIndex    int32
	VramMBTotal int32
	VramMBFree  *int32     // nil ⇒ unknown (then the veto abstained, not vetoed)
	SampledAt   *time.Time // nil ⇒ never sampled
	SampleAgeMs *int64     // nil ⇒ never sampled
	InflightN   int32      // sessions inside the debit's grace window
	DebitMB     int32      // InflightN × the per-session estimate
}

// VramVetoRejection carries the per-GPU evidence for a veto-caused rejection. It
// unwraps to ErrCapacityExhausted so errors.Is checks and the HTTP status mapping
// are unchanged; the detail is for the launcher's structured log.
type VramVetoRejection struct {
	err        error
	Veto       VramAdmission
	Candidates []VramVetoCandidate
}

func (e *VramVetoRejection) Error() string {
	return fmt.Sprintf("%v (live free-VRAM veto refused %d candidate GPU(s), floor %d MB)",
		e.err, len(e.Candidates), e.Veto.MinFreeMB)
}

func (e *VramVetoRejection) Unwrap() error { return e.err }

// vetoedCandidates lists the GPUs that pass every admission gate except the
// veto. It runs only after a failed pick, so a non-empty result is conclusive:
// slots were available, so the veto refused.
func vetoedCandidates(ctx context.Context, tx pgx.Tx, cand candidacy) ([]VramVetoCandidate, error) {
	// The pin and image gates apply here too, or a GPU excluded by image
	// readiness is reported as VRAM-vetoed and sends an operator hunting a memory
	// problem that does not exist. vetoDiagQuery binds only what its statement
	// references (Postgres rejects an unreferenced bind), so the debit is applied
	// in Go below.
	diagSQL, args := cand.vetoDiagQuery()
	rows, err := tx.Query(ctx, diagSQL, args...)
	if err != nil {
		return nil, fmt.Errorf("veto diagnosis: %w", err)
	}
	defer rows.Close()

	var out []VramVetoCandidate
	for rows.Next() {
		var c VramVetoCandidate
		if err := rows.Scan(&c.GPUID, &c.HostID, &c.GPUIndex, &c.VramMBTotal,
			&c.VramMBFree, &c.SampledAt, &c.SampleAgeMs, &c.InflightN); err != nil {
			return nil, fmt.Errorf("scan veto diagnosis: %w", err)
		}
		c.DebitMB = c.InflightN * cand.veto.InflightMB
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate veto diagnosis: %w", err)
	}
	return out, nil
}
