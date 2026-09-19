package crud

import (
	"context"
	"fmt"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/readiness"
	"github.com/accreleus/quasar/control-plane/internal/readinessgate"
)

// ReadinessGate is the served `readiness_gate` (openapi.yaml, control-api.md
// "Evidence-gated readiness"). Always serialized, and `blocking` is populated
// whatever the state: while `abstaining` the entries are facts for the console
// and nothing is excluded from admission.
type ReadinessGate struct {
	State    string               `json:"state"`
	Blocking []readiness.Blocking `json:"blocking"`
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

// attachReadinessGates fills every host's verdict AND its served overrides
// from one read (Gate.Overrides), so the gate's `overridden` flags and
// readiness_overrides can never disagree about which ids are stored.
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
	byHost, err := s.readinessGate().Overrides(ctx, ids)
	if err != nil {
		return fmt.Errorf("read readiness overrides: %w", err)
	}

	window := s.readinessWindow()
	for i := range hosts {
		overrides := byHost[hosts[i].ID]
		overrideIDs := make([]string, len(overrides))
		for j, o := range overrides {
			overrideIDs[j] = o.CheckID
		}
		v := readiness.Evaluate(hosts[i].Readiness, overrideIDs)
		hosts[i].ReadinessGate = ReadinessGate{
			State:    readiness.GateState(hosts[i].ReadinessReportedAt, dbNow, window),
			Blocking: v.Blocking,
		}
		// Inert from this host snapshot, the one `blocking` was judged on: the
		// override read is a later statement, and a report landing between the
		// two would otherwise serve an entry as both overriding and inert.
		inert := make(map[string]bool, len(v.Inert))
		for _, id := range v.Inert {
			inert[id] = true
		}
		served := make([]readinessgate.Override, len(overrides))
		for j, o := range overrides {
			o.Inert = inert[o.CheckID]
			served[j] = o
		}
		hosts[i].ReadinessOverrides = served
	}
	return nil
}
