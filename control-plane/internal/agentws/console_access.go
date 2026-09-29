package agentws

import (
	"context"

	"github.com/accreleus/quasar/control-plane/internal/audit"
	"github.com/accreleus/quasar/control-plane/internal/console"
)

// applyConsoleAccessReport is amendment 18's control-plane reaction to one
// capacity.console_capabilities.access report (agent-api.md, control-api.md
// §Console mode): the once-per-request_id restored reset ("Restored attempt")
// and lifting the PATCH-triggered placement hold once the report settles it
// ("Placement" — admission_query.go consoleAccessGate reads the same stored
// marker).
func (h *Handler) applyConsoleAccessReport(ctx context.Context, hostID string, access console.Access) {
	applied, resolved, err := h.consoleStore.ResetEnabledOnRestoredAccess(ctx, hostID, access)
	if err != nil {
		h.log.Warn("console access restored-reset failed", "host_id", hostID, "err", err)
		return
	}
	if applied {
		_ = h.registry.Send(hostID, ConfigUpdateCmd{Type: "config_update", ConsoleConfig: resolved})
		details := map[string]any{"target": *access.Target}
		if access.RequestID != nil {
			details["request_id"] = *access.RequestID
		}
		if access.Reason != nil {
			details["reason"] = *access.Reason
		}
		audit.TryRecord(ctx, h.auditor, "", "console.access.restored", "host", hostID, details)
	}

	// The report settles the PATCH-pending hold when it is `applying`
	// (nothing to hold once the state itself carries that), `unsupported`
	// (never replaced), or already agrees with the stored `enabled` — which,
	// after a just-applied reset, it does by construction.
	settled := access.State == "applying" || access.State == "unsupported" || access.HasAccess() == resolved.Enabled
	if settled {
		if err := h.consoleStore.SetPlacementHoldPending(ctx, hostID, false); err != nil {
			h.log.Warn("console placement hold clear failed", "host_id", hostID, "err", err)
		}
	}
}
