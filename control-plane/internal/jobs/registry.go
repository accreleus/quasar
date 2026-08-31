package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"sync"
)

// The registry is the code-owned half of the framework: which jobs EXIST. The
// database holds the admin-owned half: when each one may run. Keeping "which
// jobs exist" in code rather than in seed data is what stops adoption from
// meaning "edit an applied migration".

// idRe constrains a job id to the dotted, lowercase vocabulary the design fixes
// (`artwork.sweep`, `library.discovery`, `template.warmup`, `home.gc`). It is
// enforced at registration because the id is a URL path segment, a log field and
// a row key: an id with a slash or a space in it would break all three in ways
// that only show up much later.
var idRe = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._][a-z0-9]+)+$`)

// Registry holds the Definitions this build knows about. Registration order is
// preserved so the sync log and any listing are stable between boots.
type Registry struct {
	mu    sync.RWMutex
	defs  map[string]Definition
	order []string
}

// NewRegistry returns an empty registry — a valid, supported state: the
// dispatcher then materializes nothing and logs one sync line at boot.
func NewRegistry() *Registry {
	return &Registry{defs: make(map[string]Definition)}
}

// Register validates and adds a Definition.
//
// Every rule below exists to turn a class of silent misbehaviour into a boot
// failure, because a job that is registered wrong does not crash — it just
// quietly never runs, or runs without anything to execute it, and nobody
// notices for weeks.
func (r *Registry) Register(d Definition) error {
	if !idRe.MatchString(d.ID) {
		return fmt.Errorf("jobs: id %q must be lowercase dotted (e.g. artwork.sweep)", d.ID)
	}
	if d.Name == "" {
		return fmt.Errorf("jobs: %s has no name", d.ID)
	}
	switch d.Plane {
	case PlaneControl, PlaneAgent:
	default:
		return fmt.Errorf("jobs: %s has invalid plane %q", d.ID, d.Plane)
	}
	switch d.Scope {
	case ScopeInstance, ScopeHost:
	default:
		return fmt.Errorf("jobs: %s has invalid scope %q", d.ID, d.Scope)
	}
	// An agent-plane job is executed on the host, so an instance scope would give
	// it no host to run on. This is the single most plausible copy-paste error in
	// a Definition literal.
	if d.Plane == PlaneAgent && d.Scope != ScopeHost {
		return fmt.Errorf("jobs: %s is agent-plane and must be host-scoped", d.ID)
	}
	switch d.Default.Kind {
	case KindInterval:
		if d.Default.IntervalSecs < MinIntervalSecs {
			return fmt.Errorf("jobs: %s interval %ds is below the %ds floor",
				d.ID, d.Default.IntervalSecs, MinIntervalSecs)
		}
	case KindEvent, KindManual:
		if d.Default.IntervalSecs != 0 {
			return fmt.Errorf("jobs: %s is %s and must not set an interval", d.ID, d.Default.Kind)
		}
	default:
		return fmt.Errorf("jobs: %s has invalid schedule kind %q", d.ID, d.Default.Kind)
	}
	if (d.Default.WindowStart == nil) != (d.Default.WindowEnd == nil) {
		return fmt.Errorf("jobs: %s must set both window bounds or neither", d.ID)
	}
	for _, w := range d.Default.WindowDays {
		if w < 0 || w > 6 {
			return fmt.Errorf("jobs: %s window day %d out of range (0=Sunday..6=Saturday)", d.ID, w)
		}
	}
	if _, err := LoadLocation(d.Default.Timezone); err != nil {
		return fmt.Errorf("jobs: %s: %w", d.ID, err)
	}
	// A managed control-plane job with no body would be scheduled, claimed, and
	// then have nothing to execute — it would fail every pass forever.
	if d.Managed && d.Plane == PlaneControl && d.Run == nil {
		return fmt.Errorf("jobs: %s is a managed control-plane job and needs a Run func", d.ID)
	}
	// The mirror image: a body the framework will never call is a lie about where
	// the work happens.
	if (!d.Managed || d.Plane == PlaneAgent) && d.Run != nil {
		return fmt.Errorf("jobs: %s must not set a Run func (managed=%v plane=%s)", d.ID, d.Managed, d.Plane)
	}
	// A resolver on an unmanaged job would never be called: an unmanaged job is
	// never materialized at all. Same reasoning as the Run-func rule above — a
	// hook the framework will never invoke is a lie about where the work happens.
	if !d.Managed && d.ResolveParams != nil {
		return fmt.Errorf("jobs: %s is unmanaged and must not set a ResolveParams func", d.ID)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.defs[d.ID]; dup {
		return fmt.Errorf("jobs: duplicate registration of %q", d.ID)
	}
	r.defs[d.ID] = d
	r.order = append(r.order, d.ID)
	return nil
}

// MustRegister is Register for wiring code, where a bad Definition is a
// programming error and must not boot.
func (r *Registry) MustRegister(d Definition) {
	if err := r.Register(d); err != nil {
		panic(err)
	}
}

// Get returns the Definition for id.
func (r *Registry) Get(id string) (Definition, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.defs[id]
	return d, ok
}

// All returns every Definition in registration order.
func (r *Registry) All() []Definition {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Definition, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, r.defs[id])
	}
	return out
}

// IDs returns every registered id, sorted.
func (r *Registry) IDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.defs))
	for id := range r.defs {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// SyncResult is what one reconciliation did, for the boot log line.
type SyncResult struct {
	Added   int
	Updated int
	Removed int
}

// Sync reconciles the registry into the `jobs` table, once at boot, after
// migrations and before the dispatcher starts. The ownership split: an unknown
// id is inserted with its Default schedule; a known job has only its identity
// columns updated — the admin-owned columns (enabled, schedule_kind,
// interval_secs, window_*, timezone, history_limit) are never overwritten; an
// unregistered row is deleted, cascading its history. defaultTimezone
// (QUASAR_JOBS_TIMEZONE) and defaultHistoryLimit (QUASAR_JOBS_RUN_RETENTION)
// seed a new row only and are never re-applied to an existing one.
func (r *Registry) Sync(ctx context.Context, store *Store, defaultTimezone string, defaultHistoryLimit int, log *slog.Logger) (SyncResult, error) {
	res, err := store.SyncDefinitions(ctx, r.All(), defaultTimezone, defaultHistoryLimit)
	if err != nil {
		return res, err
	}
	if log != nil {
		log.Info("job: registry sync", "added", res.Added, "updated", res.Updated, "removed", res.Removed)
	}
	return res, nil
}
