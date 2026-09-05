package agentws

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/hostenroll"
)

var (
	ErrInvalidEnrollmentToken = errors.New("invalid enrollment token")
	// ErrHostAgentConnected refuses re-enrollment onto a host whose agent is live (#96).
	// Enrollment rotates node_secret, so allowing it against a connected host is identity
	// takeover: the incumbent's next reconnect fails and the scheduler keeps placing work
	// on the row the caller now authenticates as. A genuinely dead host is unaffected.
	ErrHostAgentConnected = errors.New("a live agent is already registered under this node name")
	ErrHostNotFound       = errors.New("host not found")
	ErrInvalidNodeSecret  = errors.New("invalid node secret")
)

type agentStore struct {
	pool *pgxpool.Pool
	// isAgentConnected reports whether a host id holds a live agent websocket ON THIS
	// PROCESS. A func rather than the Registry itself, so the store stays testable
	// without one. It is only half the liveness answer: another replica's connection is
	// invisible here, which is why enrollHost also reads the host row's own status (see
	// hostIsLiveSQL). Nil is treated as "no local connections".
	isAgentConnected func(hostID string) bool
	// redeemEnrollment consumes a minted token inside the caller's transaction. Injected
	// so agentws does not import hostenroll (and so tests can supply a stub). Nil means
	// minted tokens are unavailable: only the static token can enroll.
	redeemEnrollment func(ctx context.Context, db hostenroll.DBTX, plaintext, nodeName string) error
}

// Takes a host's GPU inventory out of scheduling before a capacity report
// re-upserts it. Must not touch VRAM telemetry: a capacity report is routine
// (hotplug, config_update, every session stop — every ~5 s on some hosts), and
// wiping the sample here erased it as fast as the heartbeat could write it.
// The genuine invalidation triggers are reconnect (the variant below) and an
// identity change at an index (the upsert's DO UPDATE).
const markGPUsStaleSQL = `UPDATE gpus SET reported = false WHERE host_id = $1`

// Reconnect/enrollment variant: also invalidates VRAM telemetry (#383 §3.3).
// On reconnect the two must move together — `gpus.index` is enumeration order,
// so a removed card shifts indices and a surviving sample would describe a
// different physical GPU; and a "full" reading taken just before a crash would
// otherwise block relaunch for the whole staleness window. NULL abstains in
// the veto, so this is the fail-open direction.
const markGPUsStaleAndClearVramSQL = `UPDATE gpus
	SET reported             = false,
	    vram_mb_used         = NULL,
	    vram_mb_free         = NULL,
	    vram_sampled_at      = NULL,
	    vram_sample_agent_ms = NULL
	WHERE host_id = $1`

type registerResult struct {
	HostID     string
	NodeSecret string // non-empty only on enrollment (including re-enrollment)
	// True when the reconnect classified as a genuine agent-process restart
	// rather than a WS blip (#429; see agentRestartMinGap). Always false from
	// enrollHost — enrollment is an identity event, not a counted restart.
	AgentRestarted bool
}

// Minimum offline gap for a reconnect to classify as a genuine agent-process
// restart rather than a WS blip (#429). agent-api.md carries no instance id to
// tell the two apart, so gap length is the only server-side signal: measured, a
// blip re-registers in ~0.6-2.4 s, a real restart in ~43-59 s; 15 s has ample
// margin both ways. A var so tests can retune it.
//
// The gap runs from agent_disconnected_at (stamped by markOffline) to now() —
// both DB-clock reads, immune to app/DB skew. agent_disconnected_at is consumed
// (cleared to NULL) by the reconnect that reads it; a control-plane restart
// never stamps it (the process dies before the deferred markOffline runs), so
// the next reconnect classifies as unknown rather than attributing the control
// plane's own downtime to the agent.
var agentRestartMinGap = 15 * time.Second

// enrollHost creates or re-enrolls a host using the enrollment token.
// If the host row already exists, the node_secret is rotated (idempotent re-enrollment).
func (s *agentStore) enrollHost(ctx context.Context, nodeName, agentVersion, token, configToken string) (registerResult, error) {
	// Two credentials are accepted, minted first (#12/#96).
	//
	// A minted token is per-host, hashed, single-use and expiring; the static configToken
	// is the fleet-wide value every deployment has today and must keep working across the
	// upgrade. Redemption is deferred into the transaction below so that consuming a
	// single-use token is atomic with the host row it creates: if the upsert fails, the
	// use is given back with the rollback.
	//
	// Constant-time on the static compare: the enrollment token gates rogue-node
	// enrollment, and /agent/ws is reachable pre-auth — don't leak a byte-by-byte timing
	// oracle. A minted token needs no such care: it is looked up by hash, not compared.
	staticOK := configToken != "" &&
		subtle.ConstantTimeCompare([]byte(token), []byte(configToken)) == 1

	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return registerResult{}, fmt.Errorf("generate node secret: %w", err)
	}
	secretHex := hex.EncodeToString(secret)
	h := sha256.Sum256([]byte(secretHex))
	secretHash := hex.EncodeToString(h[:])

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return registerResult{}, fmt.Errorf("begin enrollment: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// Consume the credential inside the transaction (see the note above), and BEFORE the
	// takeover guard below: /agent/ws is reachable pre-auth, and running the guard first
	// told an unauthenticated caller whether a node_name exists with a live agent.
	if !staticOK {
		if s.redeemEnrollment == nil {
			return registerResult{}, ErrInvalidEnrollmentToken // minted tokens unavailable
		}
		if err := s.redeemEnrollment(ctx, tx, token, nodeName); err != nil {
			// Only a genuinely unusable token is an auth failure. A DB outage reported as
			// "authentication failed" sends the operator to rotate a token that was fine.
			if errors.Is(err, hostenroll.ErrInvalidToken) {
				return registerResult{}, ErrInvalidEnrollmentToken
			}
			return registerResult{}, fmt.Errorf("redeem enrollment token: %w", err)
		}
	}

	// #96: enrollment onto an EXISTING node_name replaces its node_secret. That is correct
	// for re-enrolling a host you own and it is identity takeover for one you do not, so
	// refuse it while that host's agent is live. The refusal happens after the redeem, and
	// the deferred rollback gives the use back — a refused takeover never burns a token.
	var existingID string
	var dbLive bool
	err = tx.QueryRow(ctx,
		`SELECT id::text, `+hostIsLiveSQL+` FROM hosts WHERE node_name = $1 FOR UPDATE`,
		nodeName).Scan(&existingID, &dbLive)
	switch {
	case err == nil:
		if dbLive || (s.isAgentConnected != nil && s.isAgentConnected(existingID)) {
			return registerResult{}, ErrHostAgentConnected
		}
	case errors.Is(err, pgx.ErrNoRows):
		// A new node_name: nothing to take over, and nothing locked either — zero rows
		// lock nothing. Concurrent first-enrollments of the same name serialize on the
		// ON CONFLICT (node_name) below instead, which is the same outcome.
	default:
		return registerResult{}, fmt.Errorf("look up host for enrollment: %w", err)
	}

	// Enrollment is an identity event: the restart tally resets, and
	// agent_disconnected_at is cleared so a re-enrolling node never carries a
	// stale pending disconnect into its first reconnect.
	var hostID string
	err = tx.QueryRow(ctx, `
		INSERT INTO hosts (node_name, agent_version, node_secret_hash, status, last_registered_at, capacity_detection, agent_process_started_at)
		VALUES ($1, $2, $3, 'online', now(), 'unavailable', now())
		ON CONFLICT (node_name) DO UPDATE
		    SET agent_version      = EXCLUDED.agent_version,
		        node_secret_hash   = EXCLUDED.node_secret_hash,
		        status             = 'online',
		        last_registered_at = now(),
		        pending_restart    = false
		        ,capacity_detection = 'unavailable'
		        ,capacity_reason    = 'awaiting fresh capacity report'
		        ,agent_process_started_at = now()
		        ,agent_restart_count      = 0
		        ,agent_last_restart_at    = NULL
		        ,agent_disconnected_at    = NULL
		RETURNING id::text
	`, nodeName, agentVersion, secretHash).Scan(&hostID)
	if err != nil {
		return registerResult{}, fmt.Errorf("upsert host: %w", err)
	}
	if _, err := tx.Exec(ctx, markGPUsStaleAndClearVramSQL, hostID); err != nil {
		return registerResult{}, fmt.Errorf("mark enrollment inventory stale: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return registerResult{}, fmt.Errorf("commit enrollment: %w", err)
	}
	return registerResult{HostID: hostID, NodeSecret: secretHex}, nil
}

// hostIsLiveSQL is the DB half of the #96 takeover guard: the registry only sees
// connections on THIS process, so a multi-replica control plane would let a
// takeover through against a host connected to a sibling replica. markOffline
// stamps agent_disconnected_at on every path that ends the read loop, so a row
// that is online with none is one somebody believes they are still serving.
//
// Tradeoff: after a control-plane crash the deferred markOffline never runs, so a
// dead host reads as live until its agent connects and drops again — nothing
// sweeps stale rows. A genuine re-enrollment in that window uses a new node_name,
// or the admin deletes the host row first.
const hostIsLiveSQL = `(status = 'online' AND agent_disconnected_at IS NULL)`

// reconnectHost verifies the node_secret and marks the host online.
func (s *agentStore) reconnectHost(ctx context.Context, nodeName, agentVersion, nodeSecret string) (registerResult, error) {
	var hostID, storedHash string
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, node_secret_hash FROM hosts WHERE node_name = $1
	`, nodeName).Scan(&hostID, &storedHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return registerResult{}, ErrHostNotFound
	}
	if err != nil {
		return registerResult{}, fmt.Errorf("lookup host: %w", err)
	}

	h := sha256.Sum256([]byte(nodeSecret))
	if subtle.ConstantTimeCompare([]byte(hex.EncodeToString(h[:])), []byte(storedHash)) != 1 {
		return registerResult{}, ErrInvalidNodeSecret
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return registerResult{}, fmt.Errorf("begin reconnect: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var isRestart bool
	err = tx.QueryRow(ctx, reconnectHostSQL, hostID, agentVersion, agentRestartMinGap.Milliseconds()).Scan(&isRestart)
	if err != nil {
		return registerResult{}, fmt.Errorf("update host: %w", err)
	}
	if _, err := tx.Exec(ctx, markGPUsStaleAndClearVramSQL, hostID); err != nil {
		return registerResult{}, fmt.Errorf("mark reconnect inventory stale: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return registerResult{}, fmt.Errorf("commit reconnect: %w", err)
	}
	return registerResult{HostID: hostID, AgentRestarted: isRestart}, nil
}

// Reconnect UPDATE with #429 restart classification (rationale at
// agentRestartMinGap). The `old` CTE snapshots the pre-reconnect values under
// FOR UPDATE so the CASEs and RETURNING read them regardless of what this same
// statement writes — one atomic read-decide-write, no TOCTOU. NULL
// agent_disconnected_at ⇒ unknown, never counted (fail toward undercounting);
// gap < threshold ⇒ blip (process identity unchanged); gap >= threshold ⇒
// restart (bump count, reset agent_process_started_at). agent_disconnected_at
// is always cleared — consumed, so a stale value can never misclassify a later
// reconnect.
const reconnectHostSQL = `
	WITH old AS (
		SELECT agent_disconnected_at, agent_process_started_at
		FROM hosts WHERE id = $1
		FOR UPDATE
	)
	UPDATE hosts SET
		status              = 'online',
		agent_version       = $2,
		last_registered_at  = now(),
		pending_restart     = false,
		capacity_detection  = 'unavailable',
		capacity_reason     = 'awaiting fresh capacity report',
		agent_restart_count = CASE
			WHEN old.agent_disconnected_at IS NOT NULL
			     AND EXTRACT(EPOCH FROM (now() - old.agent_disconnected_at)) * 1000 >= $3::bigint
			THEN hosts.agent_restart_count + 1
			ELSE hosts.agent_restart_count
		END,
		agent_last_restart_at = CASE
			WHEN old.agent_disconnected_at IS NOT NULL
			     AND EXTRACT(EPOCH FROM (now() - old.agent_disconnected_at)) * 1000 >= $3::bigint
			THEN now()
			ELSE hosts.agent_last_restart_at
		END,
		agent_process_started_at = CASE
			WHEN old.agent_process_started_at IS NULL THEN now()
			WHEN old.agent_disconnected_at IS NOT NULL
			     AND EXTRACT(EPOCH FROM (now() - old.agent_disconnected_at)) * 1000 >= $3::bigint
			THEN now()
			ELSE old.agent_process_started_at
		END,
		agent_disconnected_at = NULL
	FROM old
	WHERE hosts.id = $1
	RETURNING (
		old.agent_disconnected_at IS NOT NULL
		AND EXTRACT(EPOCH FROM (now() - old.agent_disconnected_at)) * 1000 >= $3::bigint
	)
`

// upsertCapacity replaces the host's cpu/mem and full GPU set; storage and
// effectiveSettings are keep-if-absent (nil never clobbers the stored value —
// agent-api.md `capacity`). GPUs absent from the report are deleted.
func (s *agentStore) upsertCapacity(ctx context.Context, hostID string, host HostCapacity, effectiveSettings map[string]string, gpus []GPUCapacity) error {
	detection := "unavailable"
	if len(gpus) > 0 {
		detection = "ok"
	}
	return s.upsertCapacityWithDetection(ctx, hostID, host, effectiveSettings, gpus, detection, "")
}

func (s *agentStore) upsertCapacityWithDetection(ctx context.Context, hostID string, host HostCapacity, effectiveSettings map[string]string, gpus []GPUCapacity, detection, reason string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	_, err = tx.Exec(ctx, `
		UPDATE hosts SET cpu_cores=$2, mem_mb=$3, capacity_detection=$4,
		capacity_reason=NULLIF($5, '') WHERE id=$1
	`, hostID, host.CPUCores, host.MemMB, detection, reason)
	if err != nil {
		return fmt.Errorf("update host capacity: %w", err)
	}

	if host.Storage != nil {
		raw, err := json.Marshal(host.Storage)
		if err != nil {
			return fmt.Errorf("encode storage: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE hosts SET storage=$2 WHERE id=$1`, hostID, raw); err != nil {
			return fmt.Errorf("update host storage: %w", err)
		}
	}
	if effectiveSettings != nil {
		raw, err := json.Marshal(effectiveSettings)
		if err != nil {
			return fmt.Errorf("encode effective_settings: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE hosts SET effective_settings=$2 WHERE id=$1`, hostID, raw); err != nil {
			return fmt.Errorf("update host effective_settings: %w", err)
		}
	}
	if host.CPUModel != nil {
		if _, err := tx.Exec(ctx, `UPDATE hosts SET cpu_model=$2 WHERE id=$1`, hostID, *host.CPUModel); err != nil {
			return fmt.Errorf("update host cpu_model: %w", err)
		}
	}

	// Mark the previous inventory stale before upserting the current report.
	if _, err := tx.Exec(ctx, markGPUsStaleSQL, hostID); err != nil {
		return fmt.Errorf("mark stale gpus: %w", err)
	}

	// Collect reported indexes so unreferenced stale rows can be removed.
	reportedIndexes := make([]int, len(gpus))
	for i, g := range gpus {
		reportedIndexes[i] = g.Index
		_, err = tx.Exec(ctx, `
			INSERT INTO gpus (host_id, index, vendor, model, vram_mb_total, encode_slots_total, render_node, device_path, reported)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, true)
			ON CONFLICT (host_id, index) DO UPDATE
			    SET vendor             = EXCLUDED.vendor,
			        model              = EXCLUDED.model,
			        vram_mb_total      = EXCLUDED.vram_mb_total,
			        encode_slots_total = EXCLUDED.encode_slots_total,
			        render_node        = EXCLUDED.render_node,
			        device_path        = EXCLUDED.device_path,
			        reported           = true,
			        -- Identity change at this index ⇒ the stored sample describes a
			        -- DIFFERENT physical GPU (#383 §3.3, review finding #4). This is
			        -- the PRIMARY guard on the capacity path: markGPUsStaleSQL above
			        -- deliberately preserves telemetry, because a capacity report is
			        -- routine (console hotplug, config_update, every session stop —
			        -- hermes emits one every ~5 s) and wiping the sample on each one
			        -- erased it as fast as the heartbeat could write it. Only a real
			        -- identity change at an index invalidates here; reconnect is
			        -- handled by markGPUsStaleAndClearVramSQL.
			        vram_mb_used         = CASE WHEN gpus.vendor      IS DISTINCT FROM EXCLUDED.vendor
			                                      OR gpus.model       IS DISTINCT FROM EXCLUDED.model
			                                      OR gpus.render_node IS DISTINCT FROM EXCLUDED.render_node
			                                    THEN NULL ELSE gpus.vram_mb_used END,
			        vram_mb_free         = CASE WHEN gpus.vendor      IS DISTINCT FROM EXCLUDED.vendor
			                                      OR gpus.model       IS DISTINCT FROM EXCLUDED.model
			                                      OR gpus.render_node IS DISTINCT FROM EXCLUDED.render_node
			                                    THEN NULL ELSE gpus.vram_mb_free END,
			        vram_sampled_at      = CASE WHEN gpus.vendor      IS DISTINCT FROM EXCLUDED.vendor
			                                      OR gpus.model       IS DISTINCT FROM EXCLUDED.model
			                                      OR gpus.render_node IS DISTINCT FROM EXCLUDED.render_node
			                                    THEN NULL ELSE gpus.vram_sampled_at END,
			        vram_sample_agent_ms = CASE WHEN gpus.vendor      IS DISTINCT FROM EXCLUDED.vendor
			                                      OR gpus.model       IS DISTINCT FROM EXCLUDED.model
			                                      OR gpus.render_node IS DISTINCT FROM EXCLUDED.render_node
			                                    THEN NULL ELSE gpus.vram_sample_agent_ms END
		`, hostID, g.Index, g.Vendor, g.Model, g.VRAMMBTotal, g.EncodeSlotsTotal, g.RenderNode, g.DevicePath)
		if err != nil {
			return fmt.Errorf("upsert gpu %d: %w", g.Index, err)
		}
	}

	// Remove only unreferenced stale rows. Session history retains its GPU FK, but
	// reported=false makes every stale row ineligible for scheduling immediately.
	_, err = tx.Exec(ctx, `
		DELETE FROM gpus g WHERE g.host_id=$1 AND g.index != ALL($2)
		AND NOT EXISTS (SELECT 1 FROM sessions s WHERE s.gpu_id=g.id)
	`, hostID, reportedIndexes)
	if err != nil {
		return fmt.Errorf("remove stale gpus: %w", err)
	}

	return tx.Commit(ctx)
}

// replaceHostIdentity writes the four platform-release identity columns
// (schema.md `hosts`, migration 0074) from one `register` message.
//
// WHOLESALE REPLACE, not keep-if-absent, and that is the point: every column is
// written from this message and an absent field becomes NULL. The columns
// beside it (storage, codecs, readiness) are keep-if-absent because they
// describe hardware an older agent merely fails to re-report; these describe
// the binary connected right now, so an agent downgraded to a pre-amendment
// build must read as identity-unknown rather than carry a commit that
// describes nothing running.
//
// One statement, no branching on which fields are present — a partial UPDATE
// would be exactly the keep-if-absent behaviour the contract forbids.
func (s *agentStore) replaceHostIdentity(ctx context.Context, hostID string, id HostIdentity) error {
	if _, err := s.pool.Exec(ctx, `
		UPDATE hosts SET
			source_commit   = $2,
			built_at        = $3,
			install_mode    = $4,
			updater_present = $5
		WHERE id = $1
	`, hostID, id.SourceCommit, id.BuiltAt, id.InstallMode, id.UpdaterPresent); err != nil {
		return fmt.Errorf("update host identity: %w", err)
	}
	return nil
}

// upsertHostReadiness writes hosts.readiness. Keep-if-absent (nil leaves value
// and timestamp untouched; explicit [] is a real report and overwrites). The
// agent's raw bytes are stored verbatim, never re-encoded — re-encoding would
// drop per-check fields a newer agent sends (see RegisterMsg.Readiness).
// readiness_reported_at stamps every real report, not only changes, so a stale
// set cannot present as live. Advisory; nothing schedules on it.
func (s *agentStore) upsertHostReadiness(ctx context.Context, hostID string, raw json.RawMessage) error {
	if raw == nil {
		return nil // keep-if-absent
	}
	if _, ok := ValidReadiness(raw); !ok {
		return fmt.Errorf("malformed readiness payload (%d bytes); not stored", len(raw))
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE hosts SET readiness = $2, readiness_reported_at = now() WHERE id = $1`,
		hostID, []byte(raw)); err != nil {
		return fmt.Errorf("update host readiness: %w", err)
	}
	return nil
}

// upsertHostCodecs writes hosts.codecs (multi-codec spec §3.1.2).
// Keep-if-absent; a never-reporting host keeps a NULL column, read back as
// h264-only by Store.HostCodecs. Explicit [] is stored faithfully.
func (s *agentStore) upsertHostCodecs(ctx context.Context, hostID string, codecs []string) error {
	if codecs == nil {
		return nil // keep-if-absent
	}
	raw, err := json.Marshal(codecs)
	if err != nil {
		return fmt.Errorf("encode codecs: %w", err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE hosts SET codecs=$2 WHERE id=$1`, hostID, raw); err != nil {
		return fmt.Errorf("update host codecs: %w", err)
	}
	return nil
}

// upsertHostCodecPixelRates writes hosts.codec_pixel_rates (#506) verbatim,
// keep-if-absent. Stored as received — agent-owned and forward-extensible (see
// CapacityMsg.CodecThroughput); validated only as "a JSON object", refusing
// malformed rather than storing it. Explicit `{}` IS stored: an encoder-path
// flip reports `{}`, and treating it as absent would leave the old encoder's
// numbers gating launches.
func (s *agentStore) upsertHostCodecPixelRates(ctx context.Context, hostID string, raw json.RawMessage) error {
	if raw == nil {
		return nil // keep-if-absent
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return fmt.Errorf("malformed codec_throughput payload (%d bytes); not stored: %w", len(raw), err)
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE hosts SET codec_pixel_rates=$2 WHERE id=$1`, hostID, []byte(raw)); err != nil {
		return fmt.Errorf("update host codec pixel rates: %w", err)
	}
	return nil
}

// applyVramSamples persists one heartbeat's per-GPU VRAM samples (#383 §3.3).
// Never called from the WS read loop — this UPDATE contends with the
// scheduler's `FOR UPDATE OF h, g` row lock; callers go through vramQueue.
//
// Three fail-open properties are load-bearing:
//  1. Clock source: vram_sampled_at is DB now(), never the agent's ts_unix_ms
//     — the in-flight debit compares against sessions.started_at (also DB
//     now()), and an agent clock ahead would make a dead agent's last sample
//     look permanently fresh.
//  2. Monotonicity: a displaced zombie read loop can keep writing heartbeats
//     until its read deadline expires, so the write requires agentMs strictly
//     increasing — a late zombie write cannot replace a fresher sample.
//  3. Plausibility: an implausible reading stores NULL (veto abstains), never
//     a number admission would treat as authoritative.
//
// Zero agentMs is rejected outright: it can never satisfy the monotonic guard
// and would pin the row at 0 forever.
func (s *agentStore) applyVramSamples(ctx context.Context, hostID string, agentMs int64, samples []GPUVramSample) error {
	if len(samples) == 0 || agentMs <= 0 {
		return nil
	}
	// Cap the array at the host's reported GPU count — an agent must not cause
	// thousands of UPDATEs by inflating one field.
	var reported int
	if err := s.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM gpus WHERE host_id = $1 AND reported`, hostID,
	).Scan(&reported); err != nil {
		return fmt.Errorf("count reported gpus: %w", err)
	}
	if reported == 0 {
		return nil
	}
	if len(samples) > reported {
		samples = samples[:reported]
	}

	for _, sample := range samples {
		if _, err := s.pool.Exec(ctx, vramSampleUpsertSQL,
			hostID, sample.Index, sample.UsedMB, sample.FreeMB, agentMs,
		); err != nil {
			return fmt.Errorf("apply vram sample (index %d): %w", sample.Index, err)
		}
	}
	return nil
}

// Monotonic, plausibility-validated sample write. The predicate lives in SQL
// because it needs gpus.vram_mb_total. On failure the numbers store as NULL
// but the timestamps still advance ("heard from the agent, nothing usable") —
// NULL free makes the veto abstain, so a glitching driver degrades to
// slots-only admission instead of failing the host closed. Implausible ⇔
// negative, free > total, used+free > total*1.05 (driver rounding tolerance),
// or both halves zero on a positive total (a faulted GPU's signature).
const vramSampleUpsertSQL = `
	UPDATE gpus g SET
	    vram_mb_used = CASE WHEN ` + vramSamplePlausibleSQL + ` THEN $3::int  ELSE NULL END,
	    vram_mb_free = CASE WHEN ` + vramSamplePlausibleSQL + ` THEN $4::int  ELSE NULL END,
	    vram_sampled_at      = now(),
	    vram_sample_agent_ms = $5::bigint
	WHERE g.host_id = $1 AND g.index = $2
	  AND (g.vram_sample_agent_ms IS NULL OR g.vram_sample_agent_ms < $5::bigint)`

const vramSamplePlausibleSQL = `(
	    COALESCE($3::int, 0) >= 0
	AND COALESCE($4::int, 0) >= 0
	AND COALESCE($4::int, 0) <= g.vram_mb_total
	AND COALESCE($3::int, 0) + COALESCE($4::int, 0) <= (g.vram_mb_total * 105) / 100
	-- used = 0 AND free = 0 on a card with a positive total is impossible: the
	-- memory has to be somewhere. It is what a faulted or falling-off-the-bus
	-- GPU prints, and it passed every other clause here. Stored as a reading it
	-- is CATASTROPHIC rather than merely wrong: free = 0 fails the veto for
	-- every launch, and because the driver keeps emitting it the sample never
	-- goes stale, so the GPU is refused forever with a retryable
	-- capacity_exhausted. Unknown (NULL) abstains; that is the fail-open answer.
	AND NOT (COALESCE($3::int, 0) = 0 AND COALESCE($4::int, 0) = 0 AND g.vram_mb_total > 0)
)`

// updateHeartbeat stamps last_heartbeat_at for a host.
func (s *agentStore) updateHeartbeat(ctx context.Context, hostID string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE hosts SET last_heartbeat_at=now() WHERE id=$1
	`, hostID)
	if err != nil {
		return fmt.Errorf("update heartbeat: %w", err)
	}
	return nil
}

// markOffline sets a host offline on WS disconnect (every path that ends the
// read loop). Also stamps agent_disconnected_at = now(), the anchor for
// reconnectHost's blip-vs-restart classification — see agentRestartMinGap for
// why a control-plane restart cannot misattribute its own downtime.
func (s *agentStore) markOffline(ctx context.Context, hostID string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE hosts SET status='offline', agent_disconnected_at=now() WHERE id=$1
	`, hostID)
	if err != nil {
		return fmt.Errorf("mark offline: %w", err)
	}
	return nil
}
