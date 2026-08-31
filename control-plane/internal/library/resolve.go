// resolve.go is the shared env-override-else-database resolver (migration 0047) for
// QUASAR_LIBRARY_SCAN_INTERVAL / QUASAR_STEAM_APPDETAILS_LOOKUP, each now backed by a
// database column too (instance_settings.library_discovery_interval_minutes /
// _appdetails_enabled), env var as override. One resolver shared by the status endpoint,
// the janitor, and the appdetails worker so they can't disagree about which value won.
package library

import (
	"context"
	"time"
)

// ResolverSettingsReader is the database-side half of resolution, read per call and never
// cached, so an edited value takes effect on the next janitor pass or scan report.
type ResolverSettingsReader interface {
	LibraryDiscoveryIntervalMinutes(ctx context.Context) (int, error)
	LibraryDiscoveryAppDetailsEnabled(ctx context.Context) (bool, error)
}

// Resolver resolves the scan interval and appdetails switch: env override when set, else the
// database column. Env side is fixed at construction (plain fields, not an interface, since a
// process restart is required to change it); database side is read fresh per call.
type Resolver struct {
	settings ResolverSettingsReader

	intervalEnvSet   bool
	intervalEnvValue time.Duration

	appDetailsEnvSet   bool
	appDetailsEnvValue bool
}

// NewResolver builds the resolver from the parsed environment (internal/config.Config's
// LibraryScanInterval{,Set} / SteamAppDetailsLookup{,Set}) and the settings store.
func NewResolver(settings ResolverSettingsReader,
	intervalEnvSet bool, intervalEnvValue time.Duration,
	appDetailsEnvSet bool, appDetailsEnvValue bool) *Resolver {
	return &Resolver{
		settings:           settings,
		intervalEnvSet:     intervalEnvSet,
		intervalEnvValue:   intervalEnvValue,
		appDetailsEnvSet:   appDetailsEnvSet,
		appDetailsEnvValue: appDetailsEnvValue,
	}
}

// ScanInterval resolves the discovery interval. Env var wins when set, including its 0 =
// hard-kill-regardless-of-the-database-flag semantics; otherwise the database column, read
// per call.
func (r *Resolver) ScanInterval(ctx context.Context) (interval time.Duration, overriddenByEnv bool, err error) {
	if r.intervalEnvSet {
		return r.intervalEnvValue, true, nil
	}
	minutes, err := r.settings.LibraryDiscoveryIntervalMinutes(ctx)
	if err != nil {
		return 0, false, err
	}
	return time.Duration(minutes) * time.Minute, false, nil
}

// IntervalEnvKillSwitch reports whether QUASAR_LIBRARY_SCAN_INTERVAL was set to <= 0: the one
// state the database column (bounded 15..10080 by CHECK + PATCH validation) can never
// express, and so the only state allowed to disable the janitor loop permanently at Start.
func (r *Resolver) IntervalEnvKillSwitch() bool {
	return r.intervalEnvSet && r.intervalEnvValue <= 0
}

// AppDetailsEnabled resolves the same way: env var wins when set, else the database column,
// read per call so enabling it takes effect on the next scan report with no restart.
func (r *Resolver) AppDetailsEnabled(ctx context.Context) (enabled bool, overriddenByEnv bool, err error) {
	if r.appDetailsEnvSet {
		return r.appDetailsEnvValue, true, nil
	}
	enabled, err = r.settings.LibraryDiscoveryAppDetailsEnabled(ctx)
	if err != nil {
		return false, false, err
	}
	return enabled, false, nil
}
