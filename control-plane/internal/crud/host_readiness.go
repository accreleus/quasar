package crud

import (
	"context"
	"fmt"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/readiness"
)

// ReadinessGate is the served `readiness_gate` (openapi.yaml, control-api.md
// "Evidence-gated readiness"). Always serialized, and `blocking` is populated
// whatever the state: while `abstaining` the entries are facts for the console
// and nothing is excluded from admission.
type ReadinessGate struct {
	State    string               `json:"state"`
	Blocking []readiness.Blocking `json:"blocking"`
}

// ReadinessOverride is the served `readiness_overrides` entry. #263 fills the
// array; #262 ships the shape so the host body is complete.
type ReadinessOverride struct {
	CheckID           string  `json:"check_id"`
	CreatedBy         *string `json:"created_by"`
	CreatedByUsername *string `json:"created_by_username"`
	CreatedAt         string  `json:"created_at"`
	Inert             bool    `json:"inert"`
}

// defaultReadinessStaleSecs mirrors the scheduler's default so a store built
// without SetReadinessStaleSecs judges freshness exactly as admission does.
const defaultReadinessStaleSecs = 60

func (s *store) readinessWindow() time.Duration {
	if s.readinessStaleSecs <= 0 {
		return defaultReadinessStaleSecs * time.Second
	}
	return time.Duration(s.readinessStaleSecs) * time.Second
}

// attachReadinessGates fills every host's verdict from its stored report and
// its overrides, by the same function that wrote the scheduling columns.
//
// dbNow is the database's clock, read in the same statement as the rows: the
// served state must be the one the admission SQL would reach, not one the
// control plane's own wall clock happens to agree with.
func (s *store) attachReadinessGates(ctx context.Context, hosts []Host, dbNow time.Time) error {
	if len(hosts) == 0 {
		return nil
	}
	ids := make([]string, len(hosts))
	for i, h := range hosts {
		ids[i] = h.ID
	}
	rows, err := s.pool.Query(ctx,
		`SELECT host_id::text, check_id FROM host_readiness_overrides WHERE host_id::text = ANY($1)`, ids)
	if err != nil {
		return fmt.Errorf("query readiness overrides: %w", err)
	}
	defer rows.Close()
	byHost := map[string][]string{}
	for rows.Next() {
		var hostID, checkID string
		if err := rows.Scan(&hostID, &checkID); err != nil {
			return fmt.Errorf("scan readiness override: %w", err)
		}
		byHost[hostID] = append(byHost[hostID], checkID)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate readiness overrides: %w", err)
	}

	window := s.readinessWindow()
	for i := range hosts {
		v := readiness.Evaluate(hosts[i].Readiness, byHost[hosts[i].ID])
		hosts[i].ReadinessGate = ReadinessGate{
			State:    readiness.GateState(hosts[i].ReadinessReportedAt, dbNow, window),
			Blocking: v.Blocking,
		}
	}
	return nil
}
