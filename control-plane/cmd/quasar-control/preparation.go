package main

import (
	"context"
	"github.com/accreleus/quasar/control-plane/internal/agentws"
	"github.com/accreleus/quasar/control-plane/internal/preparation"
	"github.com/accreleus/quasar/control-plane/internal/settings"
	"log/slog"
	"time"
)

// One coalescing worker pushes desired policy after settings saves and polls
// durable adoption revision changes. Failed sends retry; no per-host goroutines.
func startPreparationPolicyDelivery(ctx context.Context, store *preparation.Store, agents *agentws.Registry, handler *settings.Handler, reconcile func(string), log *slog.Logger) {
	nudge := make(chan struct{}, 1)
	handler.OnSteamPreparationChanged = func() {
		select {
		case nudge <- struct{}{}:
		default:
		}
	}
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		delivered := map[string]string{}
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			case <-nudge:
			}
			cleanupCtx, cleanupCancel := context.WithTimeout(ctx, 5*time.Second)
			staleHosts, err := store.CancelStalePending(cleanupCtx, "")
			if err != nil {
				log.Warn("Steam preparation stale queue cleanup failed", "err", err)
			}
			cleanupCancel()
			for _, host := range staleHosts {
				reconcile(host)
			}
			connected := map[string]bool{}
			for _, host := range agents.ConnectedHosts() {
				connected[host] = true
				callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				snapshot, err := store.Snapshot(callCtx, host)
				cancel()
				if err != nil {
					log.Warn("Steam source policy delivery lookup failed", "host_id", host, "err", err)
					continue
				}
				if snapshot == nil {
					continue
				}
				ackCtx, ackCancel := context.WithTimeout(ctx, 5*time.Second)
				acknowledged := store.Acknowledged(ackCtx, host, snapshot.SteamPreparation.Revision)
				ackCancel()
				if delivered[host] == snapshot.SteamPreparation.Revision && acknowledged {
					continue
				}
				if err = agents.Send(host, agentws.ConfigUpdateCmd{Type: "config_update", SourcePolicies: snapshot}); err != nil {
					continue
				}
				delivered[host] = snapshot.SteamPreparation.Revision
			}
			for host := range delivered {
				if !connected[host] {
					delete(delivered, host)
				}
			}
		}
	}()
}
