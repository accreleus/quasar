package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/accreleus/quasar/control-plane/internal/profile"
)

// SPT-06 — encoder certification store + verdict logic.
// host_encoder_certification holds the latest measured encode envelope per
// (host, gpu_index, encoder, stream_profile_id, bitrate_kbps): upsert-latest
// scheduling input for the launch path's cert cap (stream_plan.go). Keyed on
// the rung, not the launch profile (migration 0041), since encode cost is
// codec/resolution dependent and a chain can bundle multiple codecs under one
// launch-profile id — a verdict on the h264 rung must never apply to that
// chain's AV1/HEVC rung. ProfileID survives as context only.

// EncoderCertVerdict values (schema.md CHECK).
const (
	VerdictOK     = "ok"
	VerdictCapped = "capped"
	VerdictUnsafe = "unsafe"
)

// certThreshold: p95 encode_ms must be ≤ certThreshold × frame_budget_ms to
// pass as "ok" (shared by verdict computation and the scheduler cap).
const certThreshold = 0.70

// CertStaleness: max age of a cert row before the scheduler treats it as
// absent (no cap applied).
const CertStaleness = 7 * 24 * time.Hour

// EncoderCertRow is the domain view of a host_encoder_certification row.
type EncoderCertRow struct {
	ID       string
	HostID   string
	GPUIndex int
	Encoder  string
	// ProfileID is context; StreamProfileID is the key (migration 0041, see file doc).
	ProfileID       string
	StreamProfileID string
	Width           int
	Height          int
	FPS             int
	BitrateKbps     int
	Verdict         string
	EncodeP50       float64
	EncodeP95       float64
	EncodeMax       float64
	OutputFPS       float64
	DropRate        float64
	LiveWriteStable bool
	SampleWindowMs  int
	SampleCount     int
	AgentVersion    *string
	MeasuredAt      time.Time
	UpdatedAt       time.Time
}

// CertFilter optionally restricts a GetEncoderCerts query.
type CertFilter struct {
	GPUIndex *int
	Encoder  *string
	// ProfileID filters context; StreamProfileID filters the certified rung.
	ProfileID       *string
	StreamProfileID *string
	MaxAge          *time.Duration
}

// CertMetrics is the aggregated view of session_metrics rows for a cert bench session.
type CertMetrics struct {
	EncodeP50   float64
	EncodeP95   float64
	EncodeMax   float64
	OutputFPS   float64
	DropRate    float64
	SampleCount int
	WindowMs    int64
	// LiveWriteStable reads JSONB "live_write_stable" of the latest agent
	// sample; defaults true if absent.
	LiveWriteStable bool
}

// DeriveVerdict computes an EncoderCertVerdict from bench measurements:
// ok = p95 ≤ certThreshold×budget AND outputFPS ≥ 0.97×targetFPS AND dropRate ≤ 0.01;
// capped = p95 between threshold and budget, or low-drop fps shortfall;
// unsafe = p95 > budget, or fps materially short with drop_rate > 0.05.
// budgetMs is 1000/targetFPS.
func DeriveVerdict(p95Ms, budgetMs float64, outputFPS, targetFPS float64, dropRate float64) string {
	if p95Ms > budgetMs {
		return VerdictUnsafe
	}
	if outputFPS < 0.97*targetFPS && dropRate > 0.05 {
		return VerdictUnsafe
	}
	if dropRate > 0.05 {
		return VerdictUnsafe
	}
	if p95Ms <= certThreshold*budgetMs && outputFPS >= 0.97*targetFPS && dropRate <= 0.01 {
		return VerdictOK
	}
	return VerdictCapped
}

// UpsertEncoderCert inserts or replaces the certification row for a given
// (host_id, gpu_index, encoder, stream_profile_id, bitrate_kbps). Latest write
// wins. `profile_id` rides along and refreshes on conflict but is not part of
// the key: the same rung may be listed by more than one chain, and
// re-certifying it under a different chain must update, not fork, the row.
func (s *Store) UpsertEncoderCert(ctx context.Context, row EncoderCertRow) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO host_encoder_certification
		    (host_id, gpu_index, encoder, profile_id, stream_profile_id,
		     width, height, fps, bitrate_kbps,
		     verdict, encode_ms_p50, encode_ms_p95, encode_ms_max,
		     output_fps, drop_rate, live_write_stable,
		     sample_window_ms, sample_count, agent_version,
		     measured_at, updated_at)
		VALUES ($1::uuid, $2, $3, $4, $5,
		        $6, $7, $8, $9,
		        $10, $11, $12, $13,
		        $14, $15, $16,
		        $17, $18, $19,
		        $20, now())
		ON CONFLICT (host_id, gpu_index, encoder, stream_profile_id, bitrate_kbps) DO UPDATE
		    SET profile_id        = EXCLUDED.profile_id,
		        width             = EXCLUDED.width,
		        height            = EXCLUDED.height,
		        fps               = EXCLUDED.fps,
		        verdict           = EXCLUDED.verdict,
		        encode_ms_p50     = EXCLUDED.encode_ms_p50,
		        encode_ms_p95     = EXCLUDED.encode_ms_p95,
		        encode_ms_max     = EXCLUDED.encode_ms_max,
		        output_fps        = EXCLUDED.output_fps,
		        drop_rate         = EXCLUDED.drop_rate,
		        live_write_stable = EXCLUDED.live_write_stable,
		        sample_window_ms  = EXCLUDED.sample_window_ms,
		        sample_count      = EXCLUDED.sample_count,
		        agent_version     = EXCLUDED.agent_version,
		        measured_at       = EXCLUDED.measured_at,
		        updated_at        = now()
	`, row.HostID, row.GPUIndex, row.Encoder, row.ProfileID, row.StreamProfileID,
		row.Width, row.Height, row.FPS, row.BitrateKbps,
		row.Verdict, row.EncodeP50, row.EncodeP95, row.EncodeMax,
		row.OutputFPS, row.DropRate, row.LiveWriteStable,
		row.SampleWindowMs, row.SampleCount, row.AgentVersion,
		row.MeasuredAt)
	if err != nil {
		return fmt.Errorf("upsert encoder cert: %w", err)
	}
	return nil
}

// GetEncoderCerts returns certification rows for a host, optionally filtered,
// ordered by profile_id, stream_profile_id, gpu_index, encoder, bitrate_kbps.
func (s *Store) GetEncoderCerts(ctx context.Context, hostID string, filter CertFilter) ([]EncoderCertRow, error) {
	q := `
		SELECT id::text, host_id::text, gpu_index, encoder, profile_id, stream_profile_id,
		       width, height, fps, bitrate_kbps,
		       verdict, encode_ms_p50, encode_ms_p95, encode_ms_max,
		       output_fps, drop_rate, live_write_stable,
		       sample_window_ms, sample_count, agent_version,
		       measured_at, updated_at
		FROM host_encoder_certification
		WHERE host_id = $1::uuid
	`
	args := []any{hostID}
	i := 2

	if filter.GPUIndex != nil {
		q += fmt.Sprintf(" AND gpu_index = $%d", i)
		args = append(args, *filter.GPUIndex)
		i++
	}
	if filter.Encoder != nil {
		q += fmt.Sprintf(" AND encoder = $%d", i)
		args = append(args, *filter.Encoder)
		i++
	}
	if filter.ProfileID != nil {
		q += fmt.Sprintf(" AND profile_id = $%d", i)
		args = append(args, *filter.ProfileID)
		i++
	}
	if filter.StreamProfileID != nil {
		q += fmt.Sprintf(" AND stream_profile_id = $%d", i)
		args = append(args, *filter.StreamProfileID)
		i++
	}
	if filter.MaxAge != nil {
		cutoff := time.Now().Add(-*filter.MaxAge)
		q += fmt.Sprintf(" AND measured_at >= $%d", i)
		args = append(args, cutoff)
		i++
	}
	_ = i // declared-and-not-used if the loop body never fires

	q += " ORDER BY profile_id, stream_profile_id, gpu_index, encoder, bitrate_kbps"

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query encoder certs: %w", err)
	}
	defer rows.Close()

	var out []EncoderCertRow
	for rows.Next() {
		r, scanErr := scanEncoderCertRow(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate encoder certs: %w", err)
	}
	return out, nil
}

// CertForRung returns the most recently measured cert row for one rung at the
// closest bench bitrate to bitrateKbps. encoder "" means any encoder. Returns
// nil when no cert exists or all rows are stale (older than maxAge).
//
// Keyed on the rung (migration 0041), not the launch profile — a verdict on
// the h264 rung says nothing about the AV1 rung of the same chain. The caller
// must have resolved the rung first; see applyPostPlacement.
func (s *Store) CertForRung(ctx context.Context, hostID string, gpuIndex int, encoder, rungID string, bitrateKbps int32, maxAge time.Duration) (*EncoderCertRow, error) {
	cutoff := time.Now().Add(-maxAge)

	var row pgx.Row
	if encoder == "" {
		row = s.pool.QueryRow(ctx, `
			SELECT id::text, host_id::text, gpu_index, encoder, profile_id, stream_profile_id,
			       width, height, fps, bitrate_kbps,
			       verdict, encode_ms_p50, encode_ms_p95, encode_ms_max,
			       output_fps, drop_rate, live_write_stable,
			       sample_window_ms, sample_count, agent_version,
			       measured_at, updated_at
			FROM host_encoder_certification
			WHERE host_id = $1::uuid
			  AND gpu_index = $2
			  AND stream_profile_id = $3
			  AND measured_at >= $4
			ORDER BY ABS(bitrate_kbps - $5) ASC, measured_at DESC
			LIMIT 1
		`, hostID, gpuIndex, rungID, cutoff, bitrateKbps)
	} else {
		row = s.pool.QueryRow(ctx, `
			SELECT id::text, host_id::text, gpu_index, encoder, profile_id, stream_profile_id,
			       width, height, fps, bitrate_kbps,
			       verdict, encode_ms_p50, encode_ms_p95, encode_ms_max,
			       output_fps, drop_rate, live_write_stable,
			       sample_window_ms, sample_count, agent_version,
			       measured_at, updated_at
			FROM host_encoder_certification
			WHERE host_id = $1::uuid
			  AND gpu_index = $2
			  AND encoder = $3
			  AND stream_profile_id = $4
			  AND measured_at >= $5
			ORDER BY ABS(bitrate_kbps - $6) ASC, measured_at DESC
			LIMIT 1
		`, hostID, gpuIndex, encoder, rungID, cutoff, bitrateKbps)
	}

	cert, err := scanEncoderCertRow(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil // no cert / stale — caller proceeds uncapped
	}
	if err != nil {
		return nil, fmt.Errorf("cert for rung: %w", err)
	}
	return &cert, nil
}

// CertsForRungs returns every fresh cert row this host+GPU holds for any of
// rungIDs — the batch form of CertForRung, used by the launch path.
//
// Deliberately does no ranking: the launch path needs rows for rungs not yet
// chosen, so it reads the whole fresh set and ranks in Go via pickCert
// (stream_plan.go), which must stay unit-testable. pickCert's ordering
// restates this query's `ORDER BY ABS(bitrate_kbps - $n), measured_at DESC`;
// the two must not drift.
//
// `encoder` is not a filter (a cert is keyed on the rung, which implies a
// codec). Freshness is applied in SQL and again in pickCert.
func (s *Store) CertsForRungs(ctx context.Context, hostID string, gpuIndex int, rungIDs []string, maxAge time.Duration) ([]EncoderCertRow, error) {
	if len(rungIDs) == 0 {
		return nil, nil
	}
	cutoff := time.Now().Add(-maxAge)

	rows, err := s.pool.Query(ctx, `
		SELECT id::text, host_id::text, gpu_index, encoder, profile_id, stream_profile_id,
		       width, height, fps, bitrate_kbps,
		       verdict, encode_ms_p50, encode_ms_p95, encode_ms_max,
		       output_fps, drop_rate, live_write_stable,
		       sample_window_ms, sample_count, agent_version,
		       measured_at, updated_at
		FROM host_encoder_certification
		WHERE host_id = $1::uuid
		  AND gpu_index = $2
		  AND stream_profile_id = ANY($3)
		  AND measured_at >= $4
		ORDER BY measured_at DESC
	`, hostID, gpuIndex, rungIDs, cutoff)
	if err != nil {
		return nil, fmt.Errorf("certs for rungs: %w", err)
	}
	defer rows.Close()

	out := make([]EncoderCertRow, 0, len(rungIDs))
	for rows.Next() {
		cert, err := scanEncoderCertRow(rows)
		if err != nil {
			return nil, fmt.Errorf("certs for rungs: %w", err)
		}
		out = append(out, cert)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("certs for rungs: %w", err)
	}
	return out, nil
}

// ErrCertTargetNotARung: the named stream-profile id is not a rung (pre-0036
// legacy row, `codec` NULL) — certifying it is meaningless, no launch resolves to it.
var ErrCertTargetNotARung = errors.New("stream profile is not a rung (no codec)")

// ErrCertTargetOrphanRung: the named rung is listed by no launch profile, so
// nothing can ever launch at it or read the cert back.
var ErrCertTargetOrphanRung = errors.New("rung is not listed by any launch profile")

// CertTarget: the rung the bench streams/measures, plus launch-profile context.
type CertTarget struct {
	Rung            profile.Profile
	LaunchProfileID string
}

// ResolveCertTarget: rungID set ⇒ that rung exactly (must be real and listed
// by some launch profile). rungID empty ⇒ profileID is a launch profile (its
// first h264 rung) or else a rung id. The first-h264-rung fallback keeps
// `run-spt06-certify.sh` measuring what it always measured pre-0041, since the
// bench has always streamed h264.
func (s *Store) ResolveCertTarget(ctx context.Context, profileID, rungID string) (CertTarget, error) {
	if rungID != "" {
		return s.certTargetForRung(ctx, rungID, profileID)
	}
	if profileID == "" {
		return CertTarget{}, ErrProfileUnknown
	}
	if lp, err := s.GetLaunchProfile(ctx, profileID); err == nil {
		if len(lp.Rungs) == 0 {
			return CertTarget{}, ErrLaunchProfileEmpty
		}
		for _, r := range lp.Rungs {
			if r.Codec == profile.CodecH264 {
				return CertTarget{Rung: r, LaunchProfileID: lp.ID}, nil
			}
		}
		return CertTarget{Rung: lp.Rungs[0], LaunchProfileID: lp.ID}, nil
	}
	return s.certTargetForRung(ctx, profileID, "")
}

// certTargetForRung validates a rung id and resolves the launch profile to
// record it under: preferredLP if it lists the rung, else the lowest-sorted
// chain that does.
func (s *Store) certTargetForRung(ctx context.Context, rungID, preferredLP string) (CertTarget, error) {
	rung, err := s.GetStreamProfile(ctx, rungID)
	if err != nil {
		return CertTarget{}, err
	}
	if rung.Codec == "" {
		return CertTarget{}, fmt.Errorf("%w: %s", ErrCertTargetNotARung, rungID)
	}

	var lpID string
	err = s.pool.QueryRow(ctx, `
		SELECT r.launch_profile_id
		FROM launch_profile_rungs r
		JOIN launch_profiles lp ON lp.id = r.launch_profile_id
		WHERE r.stream_profile_id = $1
		ORDER BY (r.launch_profile_id = $2) DESC, lp.sort_order ASC, lp.id ASC
		LIMIT 1
	`, rungID, preferredLP).Scan(&lpID)
	if errors.Is(err, pgx.ErrNoRows) {
		return CertTarget{}, fmt.Errorf("%w: %s", ErrCertTargetOrphanRung, rungID)
	}
	if err != nil {
		return CertTarget{}, fmt.Errorf("resolve cert target launch profile: %w", err)
	}
	return CertTarget{Rung: rung, LaunchProfileID: lpID}, nil
}

// GetDiagnosticsAppID returns the enabled "Quasar Stream Diagnostics" app id, or "".
func (s *Store) GetDiagnosticsAppID(ctx context.Context) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `
		SELECT id::text FROM apps
		WHERE name = 'Quasar Stream Diagnostics' AND enabled = true
		LIMIT 1
	`).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get diagnostics app: %w", err)
	}
	return id, nil
}

// AggregateMetricsForSession computes p50/p95/max encode_ms, avg output_fps,
// drop_rate, and live_write_stable from agent session_metrics, for the
// certification bench to derive a verdict. Nil when no agent samples exist.
func (s *Store) AggregateMetricsForSession(ctx context.Context, sessionID string) (*CertMetrics, error) {
	return s.AggregateMetricsForSessionWindow(ctx, sessionID, 0)
}

// AggregateMetricsForSessionWindow is AggregateMetricsForSession with a leading
// warmup skip: samples within warmupMs of the session's first sample are
// dropped, since the session reaches `running` before the script's CFT peer
// connects and webrtcbin gates frames until then (~0 fps would falsely cap a
// 60fps-capable host). warmupMs <= 0 keeps all samples.
func (s *Store) AggregateMetricsForSessionWindow(ctx context.Context, sessionID string, warmupMs int64) (*CertMetrics, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT ts_unix_ms, metrics
		FROM session_metrics
		WHERE session_id = $1::uuid AND source = 'agent'
		ORDER BY ts_unix_ms ASC
	`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("query cert metrics: %w", err)
	}
	defer rows.Close()

	var firstTs int64
	haveFirst := false
	var acceptedFirstTs int64
	var acceptedLastTs int64

	var encodeMSVals []float64
	var fpsSumTotal float64
	var fpsCount int
	var dropTotal float64
	liveWriteStable := true // optimistic default when key absent

	for rows.Next() {
		var ts int64
		var raw []byte
		if err := rows.Scan(&ts, &raw); err != nil {
			return nil, fmt.Errorf("scan cert metric row: %w", err)
		}
		if !haveFirst {
			firstTs = ts
			haveFirst = true
		}
		if warmupMs > 0 && ts < firstTs+warmupMs {
			continue // drop the warmup prefix (pre-peer-connect ~0fps samples)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			continue
		}

		if v, ok := m["encode_ms"]; ok {
			var ms float64
			if err := json.Unmarshal(v, &ms); err == nil {
				if len(encodeMSVals) == 0 {
					acceptedFirstTs = ts
				}
				acceptedLastTs = ts
				encodeMSVals = append(encodeMSVals, ms)
			}
		}
		if v, ok := m["fps"]; ok {
			var fps float64
			if err := json.Unmarshal(v, &fps); err == nil {
				fpsSumTotal += fps
				fpsCount++
			}
		}
		if v, ok := m["frames_dropped"]; ok {
			var dropped float64
			if err := json.Unmarshal(v, &dropped); err == nil {
				dropTotal += dropped
			}
		}
		if v, ok := m["live_write_stable"]; ok {
			var lws bool
			if err := json.Unmarshal(v, &lws); err == nil && !lws {
				liveWriteStable = false
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate cert metrics: %w", err)
	}

	if len(encodeMSVals) == 0 {
		return nil, nil // no samples
	}

	sorted := make([]float64, len(encodeMSVals))
	copy(sorted, encodeMSVals)
	sortFloat64s(sorted)

	n := len(sorted)
	p50 := sorted[n*50/100]
	p95Idx := int(math.Ceil(float64(n)*0.95)) - 1
	if p95Idx >= n {
		p95Idx = n - 1
	}
	p95 := sorted[p95Idx]
	pMax := sorted[n-1]

	var avgFPS float64
	if fpsCount > 0 {
		avgFPS = fpsSumTotal / float64(fpsCount)
	}

	var dropRate float64
	if n > 0 {
		dropRate = dropTotal / float64(n)
	}

	return &CertMetrics{
		EncodeP50:       p50,
		EncodeP95:       p95,
		EncodeMax:       pMax,
		OutputFPS:       avgFPS,
		DropRate:        dropRate,
		SampleCount:     n,
		WindowMs:        max(0, acceptedLastTs-acceptedFirstTs),
		LiveWriteStable: liveWriteStable,
	}, nil
}

// EnsureBenchUser upserts the internal cert-bench system user
// ("system@quasar.internal") and returns its UUID: a scheduler anchor with an
// empty password_hash (cannot authenticate, never exposed via the API) and a
// high quota (100) so sequential bench cells never hit the per-user limit.
func (s *Store) EnsureBenchUser(ctx context.Context) (string, error) {
	// Bump rather than overwrite: some test seeds set the quota explicitly.
	var id string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO users (email, username, password_hash, max_concurrent_sessions)
		VALUES ('system@quasar.internal', '__cert_bench__', '', 100)
		ON CONFLICT (username) DO UPDATE
		    SET max_concurrent_sessions = GREATEST(users.max_concurrent_sessions, 100)
		RETURNING id::text
	`).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("ensure bench user: %w", err)
	}
	return id, nil
}

// SessionStreamUpdate is the post-schedule stream write-back; UI-P4 widened it
// to h264_profile/codec since a rung carries its own playout0_ms/h264_profile,
// and omitting them would leave the session row disagreeing with the agent.
// Blank/zero string fields leave their column unchanged (a caller may write a
// subset — the cert-cap path sets no rung).
type SessionStreamUpdate struct {
	Width           int32
	Height          int32
	FPS             int32
	BitrateKbps     int32
	H264Profile     string
	Codec           string
	ProfileID       string
	StreamProfileID string
	// Playout0Ms leaves its column unchanged when <= 0.
	Playout0Ms int32
	// CodecDecision (UI-P6) rides on this write, not a second UPDATE, so the two
	// can never disagree if one fails. Empty leaves the column unchanged.
	CodecDecision json.RawMessage
}

// UpdateSessionStream applies a SessionStreamUpdate, called once post-schedule
// by the launch path and by the SPT-06 cert-cap path.
func (s *Store) UpdateSessionStream(ctx context.Context, sessionID string, u SessionStreamUpdate) error {
	nilIfEmpty := func(v string) any {
		if v == "" {
			return nil
		}
		return v
	}
	var playoutArg any
	if u.Playout0Ms > 0 {
		playoutArg = u.Playout0Ms
	}
	var decisionArg any
	if len(u.CodecDecision) > 0 {
		decisionArg = []byte(u.CodecDecision)
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE sessions
		SET width             = $2,
		    height            = $3,
		    fps               = $4,
		    bitrate_kbps      = $5,
		    h264_profile      = COALESCE($6, h264_profile),
		    codec             = COALESCE($7, codec),
		    playout0_ms       = COALESCE($8, playout0_ms),
		    profile_id        = COALESCE($9, profile_id),
		    stream_profile_id = COALESCE($10, stream_profile_id),
		    codec_decision    = COALESCE($11, codec_decision)
		WHERE id::text = $1
	`, sessionID, u.Width, u.Height, u.FPS, u.BitrateKbps,
		nilIfEmpty(u.H264Profile), nilIfEmpty(u.Codec), playoutArg,
		nilIfEmpty(u.ProfileID), nilIfEmpty(u.StreamProfileID), decisionArg)
	if err != nil {
		return fmt.Errorf("update session stream: %w", err)
	}
	return nil
}

type certScanner interface {
	Scan(dest ...any) error
}

func scanEncoderCertRow(row certScanner) (EncoderCertRow, error) {
	var r EncoderCertRow
	err := row.Scan(
		&r.ID, &r.HostID, &r.GPUIndex, &r.Encoder, &r.ProfileID, &r.StreamProfileID,
		&r.Width, &r.Height, &r.FPS, &r.BitrateKbps,
		&r.Verdict, &r.EncodeP50, &r.EncodeP95, &r.EncodeMax,
		&r.OutputFPS, &r.DropRate, &r.LiveWriteStable,
		&r.SampleWindowMs, &r.SampleCount, &r.AgentVersion,
		&r.MeasuredAt, &r.UpdatedAt,
	)
	if err != nil {
		return EncoderCertRow{}, err
	}
	return r, nil
}

// sortFloat64s: insertion sort, fine for a bench window's few hundred samples.
func sortFloat64s(s []float64) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
