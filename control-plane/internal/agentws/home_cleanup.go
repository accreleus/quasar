package agentws

import (
	"context"
	"time"
)

// A lost terminal can become retryable after a later heartbeat synthetically
// reaps the row, so a registration-only scan cannot discharge every hold.
const homeCleanupRetryInterval = 30 * time.Second

type pendingHomeReconciler interface {
	ReconcilePendingHomes(context.Context, string, HomeCommandEpoch)
}

func (h *Handler) startHomeCleanupRetry(hostID string, c *conn) {
	if !c.terminalHomeCleanupV1 {
		return
	}
	reconciler, ok := h.events.(pendingHomeReconciler)
	if !ok {
		return
	}
	epoch := &homeCommandEpoch{registry: h.registry, conn: c}
	go func() {
		ticker := time.NewTicker(homeCleanupRetryInterval)
		defer ticker.Stop()
		for {
			ctx, cancel := context.WithTimeout(context.Background(), agentDBCallTimeout)
			reconciler.ReconcilePendingHomes(ctx, hostID, epoch)
			cancel()
			select {
			case <-c.done:
				return
			case <-ticker.C:
			}
		}
	}()
}
