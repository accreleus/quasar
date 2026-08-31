package session

// launch_profiles_store.go — the launch-profile store (UI-P4). A launch profile
// is an ordered chain of stream-profile rungs, best first; migration 0036 makes
// stream_profile_policy.global_default_profile_id, user_profile_preferences
// .default_profile_id, and apps.default_profile_id all foreign keys onto it.
//
// Order is preference: the write path takes an ordered id array and assigns
// `position` from it; a client never sends positions.
//
// Distinct from a runtime preset (UI-P3, internal/crud/presets.go), which says
// what container runs.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/accreleus/quasar/control-plane/internal/profile"
)

// ErrLaunchProfileUnknown is returned for an id that does not resolve.
var ErrLaunchProfileUnknown = errors.New("launch profile not found")

// ErrLaunchProfileInUse: refuse-if-referenced (app, global policy, or user
// preference), 409; the three foreign keys are the backstop, not the gate.
var ErrLaunchProfileInUse = errors.New("launch profile is in use")

// ErrLaunchProfileExists is returned when creating an id that is taken.
var ErrLaunchProfileExists = errors.New("launch profile id already exists")

// LaunchProfileUsedBy is everything referencing a launch profile. Non-empty in
// ANY dimension means DELETE is 409.
type LaunchProfileUsedBy struct {
	Apps            []AppRef `json:"apps"`
	GlobalDefault   bool     `json:"global_default"`
	UserPreferences int      `json:"user_preferences"`
}

// any reports whether anything at all references the launch profile.
func (u LaunchProfileUsedBy) any() bool {
	return len(u.Apps) > 0 || u.GlobalDefault || u.UserPreferences > 0
}

// AppRef is one app referencing a launch profile.
type AppRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ListLaunchProfiles returns launch profiles with their rungs resolved, in
// sort_order. userFacingOnly filters to visibility='user' (GET /v1/me/profiles
// and the launch path); debug/internal chains are never offered there.
func (s *Store) ListLaunchProfiles(ctx context.Context, userFacingOnly bool) ([]profile.LaunchProfile, error) {
	q := `SELECT id, display_name, description, visibility, sort_order FROM launch_profiles`
	if userFacingOnly {
		q += ` WHERE visibility = 'user'`
	}
	q += ` ORDER BY sort_order ASC, id ASC`

	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("query launch profiles: %w", err)
	}
	defer rows.Close()

	out := []profile.LaunchProfile{}
	ids := []string{}
	for rows.Next() {
		var lp profile.LaunchProfile
		var vis string
		if err := rows.Scan(&lp.ID, &lp.DisplayName, &lp.Description, &vis, &lp.SortOrder); err != nil {
			return nil, fmt.Errorf("scan launch profile: %w", err)
		}
		lp.Visibility = profile.Visibility(vis)
		out = append(out, lp)
		ids = append(ids, lp.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("launch profile rows: %w", err)
	}
	if len(out) == 0 {
		return out, nil
	}

	byID, err := s.rungsFor(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Rungs = byID[out[i].ID]
	}
	return out, nil
}

// rungsFor loads every launch profile's rungs in ONE query, in position order.
func (s *Store) rungsFor(ctx context.Context, ids []string) (map[string][]profile.Profile, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT r.launch_profile_id, `+streamProfileColsPrefixed+`
		FROM launch_profile_rungs r
		JOIN stream_profiles sp ON sp.id = r.stream_profile_id
		WHERE r.launch_profile_id = ANY($1)
		ORDER BY r.launch_profile_id, r.position ASC
	`, ids)
	if err != nil {
		return nil, fmt.Errorf("query launch profile rungs: %w", err)
	}
	defer rows.Close()

	out := make(map[string][]profile.Profile, len(ids))
	for rows.Next() {
		var lpID string
		p, err := scanRungRow(rows, &lpID)
		if err != nil {
			return nil, err
		}
		out[lpID] = append(out[lpID], p)
	}
	return out, rows.Err()
}

// streamProfileColsPrefixed is streamProfileCols qualified with the `sp` alias,
// for the rung join. Kept in lockstep with scanStreamProfile's Scan order.
const streamProfileColsPrefixed = `sp.id, sp.display_name, sp.width, sp.height, sp.fps, sp.h264_profile,
	       sp.nominal_bitrate_kbps, sp.min_offer_bandwidth_kbps, sp.recommended_offer_bandwidth_kbps,
	       sp.headroom_factor, sp.abr_floor_kbps, sp.max_startup_rtt_ms, sp.min_decode_height,
	       sp.high_refresh_display, sp.hardware_encoder_required, sp.browser_client, sp.playout0_ms,
	       sp.visibility, sp.codecs, COALESCE(sp.codec, '')`

// scanRungRow scans "<launch_profile_id>, <streamProfileColsPrefixed>".
func scanRungRow(rows pgx.Rows, lpID *string) (profile.Profile, error) {
	var p profile.Profile
	var highRefresh, browserClient, visibility, codec string
	var codecsRaw []byte
	if err := rows.Scan(
		lpID,
		&p.ID, &p.DisplayName, &p.Width, &p.Height, &p.FPS, &p.H264Profile,
		&p.NominalBitrateKbps, &p.MinOfferBandwidthKbps, &p.RecommendedOfferBandwidthKbps,
		&p.HeadroomFactor, &p.ABRFloorKbps, &p.MaxStartupRTTMs, &p.MinDecodeHeight,
		&highRefresh, &p.HardwareEncoderRequired, &browserClient, &p.Playout0Ms,
		&visibility, &codecsRaw, &codec,
	); err != nil {
		return profile.Profile{}, fmt.Errorf("scan rung: %w", err)
	}
	p.Codecs = mergeProfileCodecs(codecsRaw)
	p.Codec = profile.Codec(codec)
	p.HighRefreshDisplay = profile.DisplayReq(highRefresh)
	p.BrowserClient = profile.BrowserSupport(browserClient)
	p.Visibility = profile.Visibility(visibility)
	return p, nil
}

// GetLaunchProfile returns one launch profile with its rungs in position order.
func (s *Store) GetLaunchProfile(ctx context.Context, id string) (profile.LaunchProfile, error) {
	var lp profile.LaunchProfile
	var vis string
	err := s.pool.QueryRow(ctx, `
		SELECT id, display_name, description, visibility, sort_order
		FROM launch_profiles WHERE id = $1
	`, id).Scan(&lp.ID, &lp.DisplayName, &lp.Description, &vis, &lp.SortOrder)
	if errors.Is(err, pgx.ErrNoRows) {
		return profile.LaunchProfile{}, ErrLaunchProfileUnknown
	}
	if err != nil {
		return profile.LaunchProfile{}, fmt.Errorf("get launch profile: %w", err)
	}
	lp.Visibility = profile.Visibility(vis)

	byID, err := s.rungsFor(ctx, []string{id})
	if err != nil {
		return profile.LaunchProfile{}, err
	}
	lp.Rungs = byID[id]
	return lp, nil
}

// LaunchProfileExists reports whether the id names a user-visible launch
// profile; validates both the global-policy and user-preference writes.
func (s *Store) LaunchProfileExists(ctx context.Context, id string) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM launch_profiles WHERE id = $1 AND visibility = 'user')
	`, id).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("validate launch profile: %w", err)
	}
	return ok, nil
}

// LaunchProfileUsedByFor resolves everything referencing a launch profile.
func (s *Store) LaunchProfileUsedByFor(ctx context.Context, id string) (LaunchProfileUsedBy, error) {
	out := LaunchProfileUsedBy{Apps: []AppRef{}}

	rows, err := s.pool.Query(ctx,
		`SELECT id::text, name FROM apps WHERE default_profile_id = $1 ORDER BY name ASC`, id)
	if err != nil {
		return out, fmt.Errorf("query apps using launch profile: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var a AppRef
		if err := rows.Scan(&a.ID, &a.Name); err != nil {
			return out, fmt.Errorf("scan app ref: %w", err)
		}
		out.Apps = append(out.Apps, a)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}

	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM stream_profile_policy WHERE global_default_profile_id = $1)`,
		id).Scan(&out.GlobalDefault); err != nil {
		return out, fmt.Errorf("check global default: %w", err)
	}
	if err := s.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM user_profile_preferences WHERE default_profile_id = $1`,
		id).Scan(&out.UserPreferences); err != nil {
		return out, fmt.Errorf("count user preferences: %w", err)
	}
	return out, nil
}

// LaunchProfilesContainingRung returns every launch profile listing the given
// rung, each with its full chain resolved in position order — the read behind
// the codec-change guard (rungCodecChangeImpact, profiles_handler.go), which
// needs the whole chain to check the h264 floor, unlike StreamProfileUsedBy
// (references only, for the admin used_by row). No visibility filter: a
// debug/internal chain losing its floor is just as broken.
func (s *Store) LaunchProfilesContainingRung(ctx context.Context, rungID string) ([]profile.LaunchProfile, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT lp.id, lp.display_name, lp.description, lp.visibility, lp.sort_order
		FROM launch_profiles lp
		WHERE EXISTS (
		    SELECT 1 FROM launch_profile_rungs r
		    WHERE r.launch_profile_id = lp.id AND r.stream_profile_id = $1
		)
		ORDER BY lp.sort_order ASC, lp.id ASC
	`, rungID)
	if err != nil {
		return nil, fmt.Errorf("query launch profiles containing rung: %w", err)
	}
	defer rows.Close()

	out := []profile.LaunchProfile{}
	ids := []string{}
	for rows.Next() {
		var lp profile.LaunchProfile
		var vis string
		if err := rows.Scan(&lp.ID, &lp.DisplayName, &lp.Description, &vis, &lp.SortOrder); err != nil {
			return nil, fmt.Errorf("scan launch profile containing rung: %w", err)
		}
		lp.Visibility = profile.Visibility(vis)
		out = append(out, lp)
		ids = append(ids, lp.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("launch profiles containing rung rows: %w", err)
	}
	if len(out) == 0 {
		return out, nil
	}

	byID, err := s.rungsFor(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Rungs = byID[out[i].ID]
	}
	return out, nil
}

// LaunchProfileWrite is the create/patch payload after handler validation.
// Rungs is the ORDERED id array; nil means "leave the chain alone" on patch.
type LaunchProfileWrite struct {
	ID          string
	DisplayName *string
	Description *string
	Visibility  *string
	SortOrder   *int32
	Rungs       []string
}

// CreateLaunchProfile inserts a launch profile and its ordered rungs in one
// transaction. The caller has already validated the h264-floor rule and that
// every rung id resolves.
func (s *Store) CreateLaunchProfile(ctx context.Context, w LaunchProfileWrite) (profile.LaunchProfile, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return profile.LaunchProfile{}, fmt.Errorf("begin create launch profile: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck — no-op after commit

	cols := []string{"id"}
	args := []any{w.ID}
	add := func(col string, v any) {
		cols = append(cols, col)
		args = append(args, v)
	}
	add("display_name", *w.DisplayName)
	if w.Description != nil {
		add("description", *w.Description)
	}
	if w.Visibility != nil {
		add("visibility", *w.Visibility)
	}
	if w.SortOrder != nil {
		add("sort_order", *w.SortOrder)
	}
	ph := make([]string, len(args))
	for i := range args {
		ph[i] = fmt.Sprintf("$%d", i+1)
	}
	q := fmt.Sprintf(`INSERT INTO launch_profiles (%s) VALUES (%s)`,
		strings.Join(cols, ", "), strings.Join(ph, ", "))
	if _, err := tx.Exec(ctx, q, args...); err != nil {
		if isUniqueViolationErr(err) {
			return profile.LaunchProfile{}, ErrLaunchProfileExists
		}
		return profile.LaunchProfile{}, fmt.Errorf("insert launch profile: %w", err)
	}
	if err := insertRungs(ctx, tx, w.ID, w.Rungs); err != nil {
		return profile.LaunchProfile{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return profile.LaunchProfile{}, fmt.Errorf("commit create launch profile: %w", err)
	}
	return s.GetLaunchProfile(ctx, w.ID)
}

// UpdateLaunchProfile edits a launch profile. When Rungs is non-nil the whole
// chain is REPLACED in position order — that is how a drag-reorder lands, and it
// is why the API takes an ordered id array rather than positions.
func (s *Store) UpdateLaunchProfile(ctx context.Context, id string, w LaunchProfileWrite) (profile.LaunchProfile, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return profile.LaunchProfile{}, fmt.Errorf("begin update launch profile: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck — no-op after commit

	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM launch_profiles WHERE id = $1)`, id).Scan(&exists); err != nil {
		return profile.LaunchProfile{}, fmt.Errorf("check launch profile exists: %w", err)
	}
	if !exists {
		return profile.LaunchProfile{}, ErrLaunchProfileUnknown
	}

	var set []string
	var args []any
	add := func(col string, v any) {
		args = append(args, v)
		set = append(set, fmt.Sprintf("%s = $%d", col, len(args)))
	}
	if w.DisplayName != nil {
		add("display_name", *w.DisplayName)
	}
	if w.Description != nil {
		add("description", *w.Description)
	}
	if w.Visibility != nil {
		add("visibility", *w.Visibility)
	}
	if w.SortOrder != nil {
		add("sort_order", *w.SortOrder)
	}
	if len(set) > 0 {
		args = append(args, id)
		q := fmt.Sprintf(`UPDATE launch_profiles SET %s WHERE id = $%d`, strings.Join(set, ", "), len(args))
		if _, err := tx.Exec(ctx, q, args...); err != nil {
			return profile.LaunchProfile{}, fmt.Errorf("update launch profile: %w", err)
		}
	}

	if w.Rungs != nil {
		if _, err := tx.Exec(ctx, `DELETE FROM launch_profile_rungs WHERE launch_profile_id = $1`, id); err != nil {
			return profile.LaunchProfile{}, fmt.Errorf("clear launch profile rungs: %w", err)
		}
		if err := insertRungs(ctx, tx, id, w.Rungs); err != nil {
			return profile.LaunchProfile{}, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return profile.LaunchProfile{}, fmt.Errorf("commit update launch profile: %w", err)
	}
	return s.GetLaunchProfile(ctx, id)
}

func insertRungs(ctx context.Context, tx pgx.Tx, lpID string, rungs []string) error {
	for i, rid := range rungs {
		if _, err := tx.Exec(ctx, `
			INSERT INTO launch_profile_rungs (launch_profile_id, stream_profile_id, position)
			VALUES ($1, $2, $3)
		`, lpID, rid, i+1); err != nil {
			return fmt.Errorf("insert launch profile rung %q: %w", rid, err)
		}
	}
	return nil
}

// DeleteLaunchProfile removes a launch profile, refusing while any app, the
// global policy, or any user preference references it. The existence check and
// the reference checks run in ONE transaction so a concurrent app/policy edit
// cannot slip a reference in between them.
func (s *Store) DeleteLaunchProfile(ctx context.Context, id string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin delete launch profile: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck — no-op after commit

	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM launch_profiles WHERE id = $1)`, id).Scan(&exists); err != nil {
		return fmt.Errorf("check launch profile exists: %w", err)
	}
	if !exists {
		return ErrLaunchProfileUnknown
	}

	var refs int
	if err := tx.QueryRow(ctx, `
		SELECT (SELECT COUNT(*) FROM apps WHERE default_profile_id = $1)
		     + (SELECT COUNT(*) FROM stream_profile_policy WHERE global_default_profile_id = $1)
		     + (SELECT COUNT(*) FROM user_profile_preferences WHERE default_profile_id = $1)
	`, id).Scan(&refs); err != nil {
		return fmt.Errorf("count launch profile references: %w", err)
	}
	if refs > 0 {
		return ErrLaunchProfileInUse
	}

	if _, err := tx.Exec(ctx, `DELETE FROM launch_profiles WHERE id = $1`, id); err != nil {
		return fmt.Errorf("delete launch profile: %w", err)
	}
	return tx.Commit(ctx)
}

// isUniqueViolationErr reports a Postgres unique-constraint violation.
func isUniqueViolationErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "SQLSTATE 23505")
}
