package session

// stream_profiles_store.go — the stream-profile (encode rung) store. A stream
// profile is one codec at one resolution/fps/bitrate; not user-facing, a user
// picks a launch profile, which lists rungs in preference order
// (launch_profiles_store.go).
//
// The legacy `codecs` JSONB column is still SELECTed and merged into
// Profile.Codecs: migration 0036 keeps the column and legacy rows so a
// code-level revert still finds its data. Nothing on the launch path reads it
// (`codec` is authoritative), and the admin write path never writes it, so a
// legacy row's NULL stays NULL for a rolled-back binary.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/accreleus/quasar/control-plane/internal/profile"
)

// ErrProfileInUse: refuse-if-in-use, 409; the FK's ON DELETE RESTRICT
// (migration 0036) is the backstop, not this gate.
var ErrProfileInUse = errors.New("stream profile is in use")

// ErrProfileExists is returned when creating a stream profile whose id is taken.
var ErrProfileExists = errors.New("stream profile id already exists")

// streamProfileCols is the shared column list, kept in lockstep with
// scanStreamProfile's Scan order.
const streamProfileCols = `id, display_name, width, height, fps, h264_profile,
	       nominal_bitrate_kbps, min_offer_bandwidth_kbps, recommended_offer_bandwidth_kbps,
	       headroom_factor, abr_floor_kbps, max_startup_rtt_ms, min_decode_height,
	       high_refresh_display, hardware_encoder_required, browser_client, playout0_ms,
	       visibility, codecs, COALESCE(codec, '')`

func (s *Store) ListStreamProfiles(ctx context.Context, userFacingOnly bool) ([]profile.Profile, error) {
	q := `SELECT ` + streamProfileCols + ` FROM stream_profiles`
	if userFacingOnly {
		q += ` WHERE visibility = 'user'`
	}
	q += ` ORDER BY sort_order ASC, height DESC, fps DESC, id ASC`

	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("query stream profiles: %w", err)
	}
	defer rows.Close()

	var out []profile.Profile
	for rows.Next() {
		p, err := scanStreamProfile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("stream profile rows: %w", err)
	}
	return out, nil
}

// ListRungs returns every stream profile with codec IS NOT NULL, the set the
// admin surface edits. Legacy codec-less rows exist only for a code-level
// revert and are excluded here.
func (s *Store) ListRungs(ctx context.Context) ([]profile.Profile, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+streamProfileCols+`
		FROM stream_profiles
		WHERE codec IS NOT NULL
		ORDER BY sort_order ASC, height DESC, fps DESC, id ASC`)
	if err != nil {
		return nil, fmt.Errorf("query rungs: %w", err)
	}
	defer rows.Close()

	out := []profile.Profile{}
	for rows.Next() {
		p, err := scanStreamProfile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rung rows: %w", err)
	}
	return out, nil
}

func (s *Store) GetStreamProfile(ctx context.Context, id string) (profile.Profile, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+streamProfileCols+` FROM stream_profiles WHERE id = $1`, id)
	p, err := scanStreamProfile(row)
	if err != nil {
		return profile.Profile{}, ErrProfileUnknown
	}
	return p, nil
}

// RungABRFloor returns the ABR floor (kbps) of the rung a session actually
// resolved to, so an admin-tuned per-rung floor is honored. A missing row
// returns (0, nil) so the caller skips evaluation.
func (s *Store) RungABRFloor(ctx context.Context, rungID string) (int32, error) {
	var floor int32
	err := s.pool.QueryRow(ctx, `SELECT abr_floor_kbps FROM stream_profiles WHERE id = $1`, rungID).Scan(&floor)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read rung abr floor: %w", err)
	}
	return floor, nil
}

// StreamProfileUsedBy lists the launch profiles listing this rung and the
// number of sessions that resolved to it; DeleteStreamProfile's 409 is the
// actual rule. The session count is not decoration:
// sessions.stream_profile_id is a NO ACTION FK, so one historical session is a
// hard reference that fails the DELETE at the database with a 500 unless it's
// counted here too. Not ON DELETE SET NULL: that would lose which rung a
// historical session got.
func (s *Store) StreamProfileUsedBy(ctx context.Context, id string) ([]ProfileRef, int, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT lp.id, lp.display_name
		FROM launch_profile_rungs r
		JOIN launch_profiles lp ON lp.id = r.launch_profile_id
		WHERE r.stream_profile_id = $1
		ORDER BY lp.sort_order ASC, lp.id ASC
	`, id)
	if err != nil {
		return nil, 0, fmt.Errorf("query stream profile used_by: %w", err)
	}
	defer rows.Close()

	out := []ProfileRef{}
	for rows.Next() {
		var ref ProfileRef
		if err := rows.Scan(&ref.ID, &ref.DisplayName); err != nil {
			return nil, 0, fmt.Errorf("scan stream profile used_by: %w", err)
		}
		out = append(out, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate stream profile used_by: %w", err)
	}

	var sessions int
	if err := s.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM sessions WHERE stream_profile_id = $1`, id).Scan(&sessions); err != nil {
		return nil, 0, fmt.Errorf("count sessions using rung: %w", err)
	}
	return out, sessions, nil
}

// ProfileRef is a minimal reference to a profile object, used in "used by" lists.
type ProfileRef struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
}

// CreateStreamProfile inserts a new rung. `codec` is required and is what makes
// the row a rung rather than a legacy carcass.
func (s *Store) CreateStreamProfile(ctx context.Context, p profile.Profile) (profile.Profile, error) {
	row := s.pool.QueryRow(ctx, `
		INSERT INTO stream_profiles (
		    id, display_name, width, height, fps, h264_profile,
		    nominal_bitrate_kbps, min_offer_bandwidth_kbps, recommended_offer_bandwidth_kbps,
		    headroom_factor, abr_floor_kbps, max_startup_rtt_ms, min_decode_height,
		    high_refresh_display, hardware_encoder_required, browser_client, playout0_ms,
		    visibility, codec
		) VALUES (
		    $1, $2, $3, $4, $5, $6,
		    $7, $8, $9,
		    $10, $11, $12, $13,
		    $14, $15, $16, $17,
		    $18, $19
		)
		RETURNING `+streamProfileCols,
		p.ID, p.DisplayName, p.Width, p.Height, p.FPS, p.H264Profile,
		p.NominalBitrateKbps, p.MinOfferBandwidthKbps, p.RecommendedOfferBandwidthKbps,
		p.HeadroomFactor, p.ABRFloorKbps, p.MaxStartupRTTMs, p.MinDecodeHeight,
		string(p.HighRefreshDisplay), p.HardwareEncoderRequired, string(p.BrowserClient),
		p.Playout0Ms, string(p.Visibility), string(p.Codec))
	created, err := scanStreamProfile(row)
	if err != nil {
		if isUniqueViolationErr(err) {
			return profile.Profile{}, ErrProfileExists
		}
		return profile.Profile{}, fmt.Errorf("insert stream profile: %w", err)
	}
	return created, nil
}

// UpdateStreamProfile edits a rung. Never writes the legacy `codecs` column:
// leaving it untouched keeps a legacy row's NULL a NULL for a rolled-back binary.
func (s *Store) UpdateStreamProfile(ctx context.Context, p profile.Profile) (profile.Profile, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE stream_profiles
		SET display_name = $2,
		    width = $3,
		    height = $4,
		    fps = $5,
		    h264_profile = $6,
		    nominal_bitrate_kbps = $7,
		    min_offer_bandwidth_kbps = $8,
		    recommended_offer_bandwidth_kbps = $9,
		    headroom_factor = $10,
		    abr_floor_kbps = $11,
		    max_startup_rtt_ms = $12,
		    min_decode_height = $13,
		    high_refresh_display = $14,
		    hardware_encoder_required = $15,
		    browser_client = $16,
		    playout0_ms = $17,
		    visibility = $18,
		    codec = NULLIF($19, '')
		WHERE id = $1
		RETURNING `+streamProfileCols,
		p.ID, p.DisplayName, p.Width, p.Height, p.FPS, p.H264Profile,
		p.NominalBitrateKbps, p.MinOfferBandwidthKbps, p.RecommendedOfferBandwidthKbps,
		p.HeadroomFactor, p.ABRFloorKbps, p.MaxStartupRTTMs, p.MinDecodeHeight,
		string(p.HighRefreshDisplay), p.HardwareEncoderRequired, string(p.BrowserClient),
		p.Playout0Ms, string(p.Visibility), string(p.Codec))
	updated, err := scanStreamProfile(row)
	if err != nil {
		return profile.Profile{}, ErrProfileUnknown
	}
	return updated, nil
}

// DeleteStreamProfile refuses while any launch profile lists it or any session
// resolved to it. Existence and in-use checks run in one transaction so a
// concurrent edit cannot slip in a reference. Both dimensions must be counted:
// sessions.stream_profile_id is a NO ACTION FK, so a historical session alone
// would otherwise turn this into a 500 instead of the actionable 409.
func (s *Store) DeleteStreamProfile(ctx context.Context, id string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin delete stream profile tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck — no-op after commit

	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM stream_profiles WHERE id = $1)`, id).Scan(&exists); err != nil {
		return fmt.Errorf("check stream profile exists: %w", err)
	}
	if !exists {
		return ErrProfileUnknown
	}

	var inUse int
	if err := tx.QueryRow(ctx,
		`SELECT (SELECT COUNT(*) FROM launch_profile_rungs WHERE stream_profile_id = $1)
		      + (SELECT COUNT(*) FROM sessions WHERE stream_profile_id = $1)`, id).Scan(&inUse); err != nil {
		return fmt.Errorf("count references to rung: %w", err)
	}
	if inUse > 0 {
		return ErrProfileInUse
	}

	if _, err := tx.Exec(ctx, `DELETE FROM stream_profiles WHERE id = $1`, id); err != nil {
		return fmt.Errorf("delete stream profile: %w", err)
	}
	return tx.Commit(ctx)
}

type profileScanner interface {
	Scan(dest ...any) error
}

func scanStreamProfile(row profileScanner) (profile.Profile, error) {
	var p profile.Profile
	var highRefresh, browserClient, visibility, codec string
	var codecsRaw []byte
	if err := row.Scan(
		&p.ID, &p.DisplayName, &p.Width, &p.Height, &p.FPS, &p.H264Profile,
		&p.NominalBitrateKbps, &p.MinOfferBandwidthKbps, &p.RecommendedOfferBandwidthKbps,
		&p.HeadroomFactor, &p.ABRFloorKbps, &p.MaxStartupRTTMs, &p.MinDecodeHeight,
		&highRefresh, &p.HardwareEncoderRequired, &browserClient, &p.Playout0Ms,
		&visibility, &codecsRaw, &codec,
	); err != nil {
		return profile.Profile{}, fmt.Errorf("scan stream profile: %w", err)
	}
	p.Codecs = mergeProfileCodecs(codecsRaw)
	p.Codec = profile.Codec(codec)
	p.HighRefreshDisplay = profile.DisplayReq(highRefresh)
	p.BrowserClient = profile.BrowserSupport(browserClient)
	p.Visibility = profile.Visibility(visibility)
	return p, nil
}

// defaultProfileCodecs is the in-code codec-preference list for a stream profile
// row with no persisted codecs (NULL column): single-sourced from
// profile.DefaultCodecs (ship-dark: h264 launchable, hevc/av1 future). Legacy,
// retained until the contract migration drops stream_profiles.codecs; migration
// 0036's fan-out encodes the same rule in SQL, which is why a NULL-codecs
// profile fans out to exactly one h264 rung.
func defaultProfileCodecs() []profile.CodecPref { return profile.DefaultCodecs() }

// mergeProfileCodecs resolves the stored stream_profiles.codecs JSONB against the
// in-code default: a NULL/empty column (existing rows, ship-dark) or an
// unparseable/empty value yields the default; a populated list is used verbatim.
func mergeProfileCodecs(raw []byte) []profile.CodecPref {
	if len(raw) == 0 {
		return defaultProfileCodecs()
	}
	var codecs []profile.CodecPref
	if err := json.Unmarshal(raw, &codecs); err != nil || len(codecs) == 0 {
		return defaultProfileCodecs()
	}
	return codecs
}
