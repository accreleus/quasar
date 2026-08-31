// resolve_test.go — the admin-libraries amendment's shared resolver
// (resolve.go), tested as a pure unit against fakeSettings (see
// library_db_test.go). No TEST_DATABASE_URL needed: ResolverSettingsReader is
// an interface, and fakeSettings' implementation makes no database call.
package library

import (
	"context"
	"testing"
	"time"
)

// TestResolverScanIntervalEnvWinsWhenSet — the env var is an OVERRIDE: when
// set, it wins over the database column regardless of what the column says,
// including the 0 kill switch.
func TestResolverScanIntervalEnvWinsWhenSet(t *testing.T) {
	set := &fakeSettings{intervalMinutes: 720} // 12h in the database
	r := NewResolver(set, true, 5*time.Minute, false, false)

	got, overridden, err := r.ScanInterval(context.Background())
	if err != nil {
		t.Fatalf("ScanInterval: %v", err)
	}
	if !overridden {
		t.Error("overriddenByEnv = false, want true (env was set)")
	}
	if got != 5*time.Minute {
		t.Errorf("ScanInterval = %v, want the env value 5m (not the database's 12h)", got)
	}
}

// TestResolverScanIntervalFallsBackToDatabaseWhenEnvUnset — the other
// direction: with no env override, the database column decides, read
// through the interface (never a cached value).
func TestResolverScanIntervalFallsBackToDatabaseWhenEnvUnset(t *testing.T) {
	set := &fakeSettings{intervalMinutes: 45}
	r := NewResolver(set, false, 0, false, false)

	got, overridden, err := r.ScanInterval(context.Background())
	if err != nil {
		t.Fatalf("ScanInterval: %v", err)
	}
	if overridden {
		t.Error("overriddenByEnv = true, want false (env was unset)")
	}
	if got != 45*time.Minute {
		t.Errorf("ScanInterval = %v, want the database value 45m", got)
	}
}

// TestResolverScanIntervalReadsDatabasePerCall — an admin's edit must be
// visible on the VERY NEXT call, never cached at resolver construction.
func TestResolverScanIntervalReadsDatabasePerCall(t *testing.T) {
	set := &fakeSettings{intervalMinutes: 30}
	r := NewResolver(set, false, 0, false, false)

	got, _, err := r.ScanInterval(context.Background())
	if err != nil {
		t.Fatalf("ScanInterval: %v", err)
	}
	if got != 30*time.Minute {
		t.Fatalf("ScanInterval = %v, want 30m", got)
	}

	set.intervalMinutes = 999
	got, _, err = r.ScanInterval(context.Background())
	if err != nil {
		t.Fatalf("ScanInterval after edit: %v", err)
	}
	if got != 999*time.Minute {
		t.Errorf("ScanInterval after a database edit = %v, want 999m (read per call, not cached)", got)
	}
}

// TestResolverIntervalEnvKillSwitch — the ONE state that disables the
// janitor PERMANENTLY at Start: env explicitly set to <= 0. Neither "env
// unset" nor "env set positive" is a kill switch.
func TestResolverIntervalEnvKillSwitch(t *testing.T) {
	cases := []struct {
		name    string
		envSet  bool
		envVal  time.Duration
		wantOff bool
	}{
		{"unset", false, 0, false},
		{"set positive", true, 5 * time.Minute, false},
		{"set zero", true, 0, true},
		{"set negative (defensive)", true, -1, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := NewResolver(&fakeSettings{}, c.envSet, c.envVal, false, false)
			if got := r.IntervalEnvKillSwitch(); got != c.wantOff {
				t.Errorf("IntervalEnvKillSwitch() = %v, want %v", got, c.wantOff)
			}
		})
	}
}

// TestResolverAppDetailsEnabledEnvWinsWhenSet mirrors the interval case for
// the boolean switch — a privacy-hardened deployment can pin it off in the
// environment regardless of what an admin later sets in the database.
func TestResolverAppDetailsEnabledEnvWinsWhenSet(t *testing.T) {
	set := &fakeSettings{appDetailsEnabled: true} // database says on
	r := NewResolver(set, false, 0, true, false)  // env pins it OFF

	got, overridden, err := r.AppDetailsEnabled(context.Background())
	if err != nil {
		t.Fatalf("AppDetailsEnabled: %v", err)
	}
	if !overridden {
		t.Error("overriddenByEnv = false, want true")
	}
	if got {
		t.Error("AppDetailsEnabled = true, want the env override (false)")
	}
}

// TestResolverAppDetailsEnabledFallsBackToDatabaseWhenEnvUnset.
func TestResolverAppDetailsEnabledFallsBackToDatabaseWhenEnvUnset(t *testing.T) {
	set := &fakeSettings{appDetailsEnabled: true}
	r := NewResolver(set, false, 0, false, false)

	got, overridden, err := r.AppDetailsEnabled(context.Background())
	if err != nil {
		t.Fatalf("AppDetailsEnabled: %v", err)
	}
	if overridden {
		t.Error("overriddenByEnv = true, want false")
	}
	if !got {
		t.Error("AppDetailsEnabled = false, want the database value (true)")
	}
}
