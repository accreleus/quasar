package session

import (
	"context"
	"fmt"
	"time"
)

// activeStatesSQL is exactly State.HoldsReservation()'s set (schema.md
// availability-sum filter); `stopping` releases on the terminal callback, not
// before (#489). Both the scheduler's availability check and GPUAvailability
// derive from this one fragment so they can never drift apart.
const activeStatesSQL = `('assigned','starting','running','stopping')`

// GPUAvailability is one GPU's agent-reported totals minus reservations held by
// active-state sessions. Derived from live sessions, never stored, so it cannot
// diverge from session truth (schema.md §gpus).
type GPUAvailability struct {
	HostID   string
	GPUID    string
	GPUIndex int32
	Vendor   string
	Model    string

	VramMBTotal int32
	// Declared accounting (sum of per-app default_vram_mb over active sessions).
	// #383 removed declared VRAM from admission, so new sessions leave Reserved 0;
	// retained since `vram_mb_reserved` is required in openapi.yaml and historical
	// sessions still carry values.
	VramMBReserved  int32
	VramMBAvailable int32

	// Live telemetry from the agent's periodic sampler (#383 §3); nil when
	// unsampled, implausible, or invalidated by a reconnect. Nil means unknown,
	// never 0 (0 reads as "this GPU is full").
	VramMBUsed    *int32
	VramMBFree    *int32
	VramSampledAt *time.Time

	SlotsTotal     int32
	SlotsReserved  int32
	SlotsAvailable int32

	ActiveSessions int32

	// Agent-reported stable by-path render-node device path (schema.md
	// gpus.render_node); null until reported.
	RenderNode *string
	DevicePath *string
}

// GPUAvailability returns the per-GPU resource view, ordered by host then GPU
// index. hostID "" reports every host's GPUs.
func (s *Store) GPUAvailability(ctx context.Context, hostID string) ([]GPUAvailability, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT g.host_id::text, g.id::text, g.index,
		       COALESCE(g.vendor, ''), COALESCE(g.model, ''),
		       g.vram_mb_total,
		       COALESCE(SUM(s.reserved_vram_mb), 0)::int,
		       g.encode_slots_total,
		       COALESCE(SUM(s.reserved_encode_slots), 0)::int,
		       COUNT(s.id)::int,
		       g.render_node, g.device_path,
		       g.vram_mb_used, g.vram_mb_free, g.vram_sampled_at
		FROM gpus g
		JOIN hosts h ON h.id = g.host_id
		LEFT JOIN sessions s
		    ON s.gpu_id = g.id AND s.state IN `+activeStatesSQL+`
		WHERE h.capacity_detection = 'ok' AND g.reported
		  AND ($1 = '' OR g.host_id::text = $1)
		GROUP BY g.id
		ORDER BY g.host_id, g.index
	`, hostID)
	if err != nil {
		return nil, fmt.Errorf("query gpu availability: %w", err)
	}
	defer rows.Close()

	var out []GPUAvailability
	for rows.Next() {
		var a GPUAvailability
		if err := rows.Scan(
			&a.HostID, &a.GPUID, &a.GPUIndex, &a.Vendor, &a.Model,
			&a.VramMBTotal, &a.VramMBReserved,
			&a.SlotsTotal, &a.SlotsReserved,
			&a.ActiveSessions,
			&a.RenderNode, &a.DevicePath,
			&a.VramMBUsed, &a.VramMBFree, &a.VramSampledAt,
		); err != nil {
			return nil, fmt.Errorf("scan gpu availability: %w", err)
		}
		a.VramMBAvailable = a.VramMBTotal - a.VramMBReserved
		a.SlotsAvailable = a.SlotsTotal - a.SlotsReserved
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate gpu availability: %w", err)
	}
	return out, nil
}
