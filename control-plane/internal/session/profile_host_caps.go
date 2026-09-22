package session

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/accreleus/quasar/control-plane/internal/profile"
)

// profileHostCaps previews the codecs available for this app without reserving
// capacity: the union of GPU codec sets (#296 amendment 12, control-api.md
// "host_encoder_not_supported is computed over GPU codec sets") over the GPUs
// that pass the launch's candidacy without the free-slot term — a busy GPU
// still counts, but the readiness gate, the derived-tile host pin, the image
// gate and the app's own encode-slot totals all apply, same as at launch. No
// VRAM veto: like totalsQuery, that gate is a LOAD term (§4.1 abstains whenever
// vram_mb_total <= floor, so such a GPU is still servable) and this is a totals
// question, not an availability one. Placement remains the authoritative check.
func (s *Store) profileHostCaps(ctx context.Context, userID, appID string) (profile.HostCaps, error) {
	p := CreateParams{NeedEncodeSlots: 1}
	if appID != "" {
		app, err := s.GetLaunchApp(ctx, appID)
		if err != nil {
			return profile.HostCaps{}, err
		}
		p.AppImage = app.Image()
		// The PARENT's managed_home for a tile (LaunchApp.ManagedHome already
		// resolves it, see launcher.go), so the readiness gate's homes term
		// engages for a tile exactly as it does at launch.
		p.ManagedHome = app.ManagedHome
		p.NeedEncodeSlots = app.DefaultEncodeSlots
		if app.IsDerived() {
			p.PinHostID, err = s.HomeHostForApp(ctx, userID, homeAppID(app))
			if err != nil || p.PinHostID == "" {
				return profile.HostCaps{}, err
			}
		}
	}
	a := &argset{}
	c := candidacy{p: p, readiness: s.readiness}
	slotsIdx := a.add(p.NeedEncodeSlots)
	pin := c.pinGate(a)
	image := c.imageGate(a, " AND ")
	gate := c.readinessGate(a, " AND ")
	rows, err := s.pool.Query(ctx, `SELECT `+gpuCodecSetSQL("g", "h", true)+`
		FROM gpus g JOIN hosts h ON h.id = g.host_id
		WHERE h.status = 'online' AND h.capacity_detection = 'ok'
		AND g.reported AND g.encode_slots_total >= $`+fmt.Sprint(slotsIdx)+schedulableBindingSQL+pin+image+gate, a.args()...)
	if err != nil {
		return profile.HostCaps{}, fmt.Errorf("query profile host codecs: %w", err)
	}
	defer rows.Close()
	var reports [][]byte
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return profile.HostCaps{}, err
		}
		reports = append(reports, raw)
	}
	if err := rows.Err(); err != nil {
		return profile.HostCaps{}, err
	}
	return profile.HostCaps{Codecs: profileCodecUnion(reports)}, nil
}

// nil means no candidate GPU rows at all (every GPU blocked, or the fleet down)
// or a malformed report — both advisory-unknown. A row itself is never nil:
// gpuCodecSetSQL(fallbackH264=true) floors it at h264. A non-nil map is a
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
