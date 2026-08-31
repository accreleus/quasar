package telemetry

import (
	"context"
	"log/slog"
	"time"
)

// The janitor: one periodic pass that applies the retention policy to every
// session, replacing four inline DELETEs on the ingest hot path.
//
// It owns no ticker. The control plane's job dispatcher is the trigger (see the
// telemetry.retain registration in cmd/quasar-control/app.go), which is the same
// arrangement every other janitor in this codebase has after WP2 — one tick
// loop, single-flight across instances, an admin-visible run history, and a
// "run now" button, none of which a bespoke time.Ticker would give.

// SlowRunThreshold is how long one pass may take before it is worth an
// operator's attention. Retain deletes in bounded batches specifically so a pass
// stays short; a slow one means either a large backlog or a table that has
// outgrown its plan.
const SlowRunThreshold = 30 * time.Second

// RunRetention performs one retention pass and emits the operator's line for it:
// exactly one INFO per run with the counts, plus a WARN when the pass was slow.
// The Report is returned so the caller can put the same numbers in a job
// summary.
func RunRetention(ctx context.Context, store Store, p Policy, log *slog.Logger) (Report, error) {
	rep, err := store.Retain(ctx, p)
	if err != nil {
		// The counts so far are still worth logging: a pass that failed on the
		// fourth statement did real work in the first three.
		log.Warn("telemetry retention failed", append(rep.LogArgs(), "err", err)...)
		return rep, err
	}
	log.Info("telemetry retention", append(rep.LogArgs(),
		"rolling", p.Rolling.String(), "post_mortem", p.PostMortem.String())...)
	if rep.Duration > SlowRunThreshold {
		log.Warn("telemetry retention pass was slow — check for a backlog or a missing index",
			"took_ms", rep.Duration.Milliseconds(),
			"threshold_ms", SlowRunThreshold.Milliseconds(),
			"total_deleted", rep.Total(),
			"truncated", rep.Truncated)
	}
	if rep.Truncated {
		log.Warn("telemetry retention did not finish its backlog this pass; the next pass continues",
			"deleted_this_pass", rep.Total())
	}
	return rep, nil
}
