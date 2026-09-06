package session

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/accreleus/quasar/control-plane/internal/profile"
)

// profileHostCaps previews the codecs available for this app without reserving
// capacity. Busy hosts still count; offline/draining, unreported GPUs, incompatible
// bindings and hosts without the managed image do not. Placement remains the
// authoritative check at launch. Unknown legacy reports preserve advisory behavior.
func (s *Store) profileHostCaps(ctx context.Context, userID, appID string) (profile.HostCaps, error) {
	p := CreateParams{}
	if appID != "" {
		app, err := s.GetLaunchApp(ctx, appID)
		if err != nil {
			return profile.HostCaps{}, err
		}
		p.AppImage = app.Image()
		if app.IsDerived() {
			p.PinHostID, err = s.HomeHostForApp(ctx, userID, homeAppID(app))
			if err != nil || p.PinHostID == "" {
				return profile.HostCaps{}, err
			}
		}
	}
	a := &argset{}
	c := candidacy{p: p}
	pin := c.pinGate(a)
	image := c.imageGate(a, " AND ")
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT h.id, h.codecs
		FROM hosts h JOIN gpus g ON g.host_id = h.id
		WHERE h.status = 'online' AND h.capacity_detection = 'ok'
		AND g.reported AND g.encode_slots_total > 0`+schedulableBindingSQL+pin+image, a.args()...)
	if err != nil {
		return profile.HostCaps{}, fmt.Errorf("query profile host codecs: %w", err)
	}
	defer rows.Close()
	var reports [][]byte
	for rows.Next() {
		var id string
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			return profile.HostCaps{}, err
		}
		reports = append(reports, raw)
	}
	if err := rows.Err(); err != nil {
		return profile.HostCaps{}, err
	}
	return profile.HostCaps{Codecs: profileCodecUnion(reports)}, nil
}

// nil means the available host set is not fully reported. A non-nil map is a
// measured union: absence is then a real codec exclusion, not missing telemetry.
func profileCodecUnion(reports [][]byte) map[profile.Codec]bool {
	if len(reports) == 0 {
		return nil
	}
	union := map[profile.Codec]bool{}
	for _, raw := range reports {
		var codecs []string
		if len(raw) == 0 || json.Unmarshal(raw, &codecs) != nil || codecs == nil {
			return nil
		}
		// Reuse the wire/catalog bridge rather than growing a second h265 rename.
		set := codecSet(codecs)
		for _, codec := range []profile.Codec{profile.CodecH264, profile.CodecHEVC, profile.CodecAV1} {
			wire, _ := catalogToWire(codec)
			if set[wire] {
				union[codec] = true
			}
		}
	}
	return union
}
