package hostcfg

import (
	"context"
	"fmt"
	"strings"
)

// idleWaitRemedy is an operator observation. The executor in #339 must make
// its own atomic admission and agent-side idle decision before starting.
func (s *Store) idleWaitRemedy(ctx context.Context, hostID string) (string, error) {
	var gateComplete, currentHeartbeat bool
	err := s.pool.QueryRow(ctx, `SELECT
		COALESCE(j.state='complete' AND j.connection_incarnation IS NOT NULL AND h.status<>'offline',false),
		COALESCE(i.connection_incarnation=j.connection_incarnation AND
			i.reported_at >= h.last_registered_at AND i.reported_at >= now()-interval '30 seconds', false)
		FROM hosts h LEFT JOIN host_journal_reconciliation j ON j.host_id=h.id
		LEFT JOIN host_idle_inventory i ON i.host_id=h.id WHERE h.id=$1::uuid`, hostID).
		Scan(&gateComplete, &currentHeartbeat)
	if err != nil {
		return "", err
	}
	if !gateComplete || !currentHeartbeat {
		return "Waiting for a complete current-connection journal and fresh authenticated session inventory. Execution support is unavailable.", nil
	}
	var assigned, starting, running, stopping int
	err = s.pool.QueryRow(ctx, `SELECT
		COUNT(*) FILTER (WHERE state='assigned'),COUNT(*) FILTER (WHERE state='starting'),
		COUNT(*) FILTER (WHERE state='running'),COUNT(*) FILTER (WHERE state='stopping')
		FROM sessions WHERE host_id=$1::uuid`, hostID).Scan(&assigned, &starting, &running, &stopping)
	if err != nil {
		return "", err
	}
	var untracked int
	err = s.pool.QueryRow(ctx, `SELECT COUNT(DISTINCT sid.value) FROM host_idle_inventory i,
		jsonb_array_elements_text(i.running_sessions) AS sid
		WHERE i.host_id=$1::uuid AND NOT EXISTS (
			SELECT 1 FROM sessions s WHERE s.host_id=i.host_id AND s.id::text=sid.value
			AND s.state IN ('assigned','starting','running','stopping'))`, hostID).Scan(&untracked)
	if err != nil {
		return "", err
	}
	var preparing, runningJobs int
	var preparationCurrent bool
	err = s.pool.QueryRow(ctx, `SELECT
		(SELECT COUNT(*) FROM jsonb_array_elements(CASE
			WHEN jsonb_typeof(h.source_preparation->'steam'->'images')='array'
			THEN h.source_preparation->'steam'->'images' ELSE '[]'::jsonb END) AS image
			WHERE image->>'state' IN ('queued','preparing','waiting_image','deferred','failed')),
		(SELECT COUNT(*) FROM job_runs r WHERE r.host_id=h.id AND r.state IN ('pending','running')),
		COALESCE(jsonb_typeof(h.source_preparation->'steam'->'images')='array'
			AND h.source_preparation_reported_at >= h.last_registered_at
			AND h.source_preparation_reported_at >= now()-interval '30 seconds',false)
		FROM hosts h WHERE h.id=$1::uuid`, hostID).Scan(&preparing, &runningJobs, &preparationCurrent)
	if err != nil {
		return "", err
	}
	parts := []string{}
	if assigned > 0 {
		parts = append(parts, fmt.Sprintf("%d assigned", assigned))
	}
	if starting > 0 {
		parts = append(parts, fmt.Sprintf("%d starting", starting))
	}
	if running > 0 {
		parts = append(parts, fmt.Sprintf("%d running", running))
	}
	if stopping > 0 {
		parts = append(parts, fmt.Sprintf("%d stopping", stopping))
	}
	if untracked > 0 {
		parts = append(parts, fmt.Sprintf("%d agent-reported untracked", untracked))
	}
	if preparing+runningJobs > 0 {
		parts = append(parts, fmt.Sprintf("%d conflicting preparation or cleanup", preparing+runningJobs))
	}
	if !preparationCurrent {
		parts = append(parts, "preparation inventory is unknown")
	}
	if len(parts) > 0 {
		return "Waiting for " + strings.Join(parts, ", ") + ". No waiting deadline ends a session. Execution support is unavailable.", nil
	}
	return "Current inventory reports no active sessions or preparation. Execution support is unavailable until recovery support is installed.", nil
}
