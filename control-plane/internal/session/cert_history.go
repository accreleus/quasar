package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/accreleus/quasar/control-plane/internal/profile"
)

// AS10-11 (#207) — per-(user, device, profile) client-performance
// certification history, behind eligibility's ReasonHistoricalClientPerfFailed
// (profile/eligibility.go). The browser classifies client health
// (clientHealth.ts); the control plane records a sustained outcome here, and
// on the next GET /v1/me/profiles a previously-failed profile goes ineligible.
//
// Latest-outcome-wins: one row per (user, device, profile, codec). A transient
// failure must never permanently ban a profile — a later sustained smooth run
// overwrites it with outcome='pass'; only outcome='fail' counts.
//
// Codec dimension (migration 0032): decode-side failures key on the wire
// codec actually streamed, so a broken h265 path never blanks h264. codec =
// '' is the profile-level verdict. Eligibility treats codec IN ('', 'h264')
// as profile-blocking (h264 is the guaranteed floor); non-h264 fails feed only
// clamp 4 (CodecFailures), skipping the codec instead of hiding the profile.
//
// Feeds only eligibility/diagnostics, never server ABR.

// profileOutcome is the stored certification outcome for a (user, device, profile).
const (
	outcomePass = "pass"
	outcomeFail = "fail"
)

// RecordProfileOutcome upserts the latest client-performance outcome for
// (userID, deviceKey, profileID). outcome must be "pass" or "fail";
// failureReason is nil for a pass. Latest write wins.
//
// codec scoping (0032): a fail with a non-empty codec lands in that codec's
// row only. A pass upserts the profile-level row AND deletes the fail row for
// the codec that ran — a sustained smooth h265 run clears the h265 suspect
// mark while leaving other codecs' fails standing.
func (s *Store) RecordProfileOutcome(ctx context.Context, userID, deviceKey, profileID, codec, outcome string, failureReason *string) error {
	if outcome != outcomePass && outcome != outcomeFail {
		return fmt.Errorf("invalid outcome %q", outcome)
	}
	if outcome == outcomeFail {
		_, err := s.pool.Exec(ctx, `
			INSERT INTO user_device_profile_history (user_id, device_key, profile_id, codec, outcome, failure_reason)
			VALUES ($1::uuid, $2, $3, $4, $5, $6)
			ON CONFLICT (user_id, device_key, profile_id, codec) DO UPDATE
			    SET outcome        = EXCLUDED.outcome,
			        failure_reason = EXCLUDED.failure_reason,
			        updated_at     = now()
		`, userID, deviceKey, profileID, codec, outcome, failureReason)
		if err != nil {
			return fmt.Errorf("record profile outcome: %w", err)
		}
		return nil
	}
	// Pass: clear the codec-scoped fail, then upsert the profile-level pass
	// row, in one statement so both commit together. UI-P4: must clear both
	// shapes — the legacy (launch profile, codec) row and the rung-level rows
	// for every rung of that profile using this codec — or a permanent ban
	// would stand on a device that just demonstrated it works.
	_, err := s.pool.Exec(ctx, `
		WITH cleared AS (
			DELETE FROM user_device_profile_history
			WHERE user_id = $1::uuid AND device_key = $2 AND codec = $4 AND codec <> ''
			  AND (
			      profile_id = $3
			      OR profile_id IN (
			          SELECT stream_profile_id FROM launch_profile_rungs
			          WHERE launch_profile_id = $3
			      )
			  )
		)
		INSERT INTO user_device_profile_history (user_id, device_key, profile_id, codec, outcome, failure_reason)
		VALUES ($1::uuid, $2, $3, '', 'pass', NULL)
		ON CONFLICT (user_id, device_key, profile_id, codec) DO UPDATE
		    SET outcome        = EXCLUDED.outcome,
		        failure_reason = EXCLUDED.failure_reason,
		        updated_at     = now()
	`, userID, deviceKey, profileID, codec)
	if err != nil {
		return fmt.Errorf("record profile outcome: %w", err)
	}
	return nil
}

// ProfileFailures returns profile ids that most recently failed client
// performance certification for (user, device) — input to eligibility's
// HistoricalFailures map. deviceKey "" is the coarse per-user default.
//
// Only profile-blocking rows count: empty codec, or codec = 'h264' (the
// guaranteed floor — a failure there means no codec saves the profile). A
// non-h264 fail does not blank the profile; RungFailures skips that rung.
//
// UI-P4 addition: decode fails are now keyed by rung id (e.g.
// "1080p60-h264"), but eligibility looks up launch-profile ids; without the
// second arm below an h264 decode failure would stop blocking anything, since
// resolveRung's unconditional floor bypasses clamp 4 and keeps handing back
// the rung that just failed. Restricted to the floor rung (the last h264 rung
// by position): unrestricted, a failure on `4k60-h264` would kill
// [4k60-h264, 1080p60-h264] outright even though clamp 4 correctly lands on
// the working 1080p rung. Neutral on migrated data — 0036 gives every chain
// exactly one h264 rung, trivially the last.
func (s *Store) ProfileFailures(ctx context.Context, userID, deviceKey string) (map[string]bool, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT profile_id
		FROM user_device_profile_history
		WHERE user_id = $1::uuid AND device_key = $2 AND outcome = 'fail'
		  AND codec IN ('', 'h264')
		UNION
		SELECT r.launch_profile_id
		FROM user_device_profile_history h
		JOIN launch_profile_rungs r ON r.stream_profile_id = h.profile_id
		JOIN stream_profiles sp ON sp.id = r.stream_profile_id
		WHERE h.user_id = $1::uuid AND h.device_key = $2 AND h.outcome = 'fail'
		  AND h.codec = 'h264'
		  AND sp.codec = 'h264'
		  AND r.position = (
		      SELECT MAX(r2.position)
		      FROM launch_profile_rungs r2
		      JOIN stream_profiles sp2 ON sp2.id = r2.stream_profile_id
		      WHERE r2.launch_profile_id = r.launch_profile_id
		        AND sp2.codec = 'h264'
		  )
	`, userID, deviceKey)
	if err != nil {
		return nil, fmt.Errorf("query profile failures: %w", err)
	}
	defer rows.Close()

	out := make(map[string]bool)
	for rows.Next() {
		var pid string
		if err := rows.Scan(&pid); err != nil {
			return nil, fmt.Errorf("scan profile failure: %w", err)
		}
		out[pid] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate profile failures: %w", err)
	}
	return out, nil
}

// RungFailures returns the rung ids this (user, device) may not be given, for
// one launch profile — UI-P4 clamp 4. Grain changed because
// user_device_profile_history keys (user_id, device_key, profile_id, codec)
// (migration 0032) is wrong-grained under launch profiles: two rungs may
// share a codec at different resolutions, and decode failure is resolution-
// dependent (why MinDecodeHeight exists) — a failure on an AV1 4K rung must
// not clamp the AV1 1080p rung of the same profile.
//
// Union of rung-level rows (profile_id = rung id, bans that rung) and legacy
// rows (profile_id = launch profile id + non-empty codec, bans every rung of
// that profile using that codec). Presentation-side fails/passes write the
// launch-profile-level row (codec = ”), feeding ProfileFailures separately.
func (s *Store) RungFailures(ctx context.Context, userID, deviceKey string, lp profile.LaunchProfile) (map[string]bool, error) {
	if len(lp.Rungs) == 0 {
		return nil, nil
	}
	rungIDs := make([]string, 0, len(lp.Rungs))
	for _, r := range lp.Rungs {
		rungIDs = append(rungIDs, r.ID)
	}

	rows, err := s.pool.Query(ctx, `
		SELECT profile_id, codec
		FROM user_device_profile_history
		WHERE user_id = $1::uuid AND device_key = $2
		  AND outcome = 'fail' AND codec <> ''
		  AND (profile_id = $3 OR profile_id = ANY($4))
	`, userID, deviceKey, lp.ID, rungIDs)
	if err != nil {
		return nil, fmt.Errorf("query rung failures: %w", err)
	}
	defer rows.Close()

	bannedCodecs := make(map[string]bool)
	out := make(map[string]bool)
	for rows.Next() {
		var pid, codec string
		if err := rows.Scan(&pid, &codec); err != nil {
			return nil, fmt.Errorf("scan rung failure: %w", err)
		}
		if pid == lp.ID {
			bannedCodecs[codec] = true
			continue
		}
		out[pid] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate rung failures: %w", err)
	}
	if len(bannedCodecs) > 0 {
		for _, r := range lp.Rungs {
			if wire, ok := catalogToWire(r.Codec); ok && bannedCodecs[wire] {
				out[r.ID] = true
			}
		}
	}
	return out, nil
}

// LatestDeviceKey returns the user's most-recently-seen device_key (P4-08), or
// "" when absent. GET /v1/me/profiles has no device_key in context, so it keys
// historical-failure lookup on the same latest device it loads for eligibility.
func (s *Store) LatestDeviceKey(ctx context.Context, userID string) (string, error) {
	var key string
	err := s.pool.QueryRow(ctx, `
		SELECT device_key
		FROM user_devices
		WHERE user_id = $1::uuid
		ORDER BY last_seen_at DESC
		LIMIT 1
	`, userID).Scan(&key)
	if err != nil {
		// No device row is expected (never posted a probe); degrade silently.
		// Any other error is unexpected — still degrade, but log it.
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Default().Warn("latest device key lookup failed", "user_id", userID, "err", err)
		}
		return "", nil
	}
	return key, nil
}
