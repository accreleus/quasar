package platform

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/actorsocket"
)

// A migrating control-plane step on an owned machine (#364, #352 decision 14):
// a Quasar-owned database is sent for its pre-update dump; an operator's own
// needs the operator's confirmation, which fails the step before anything
// moves when it is missing and reaches the recovery actor when it is given.

func ownMachineWith(db actorsocket.Database, free *int64) *fakeOwnMachine {
	node := "gpu-01"
	st := actorsocket.Status{
		Actor:         actorsocket.ActorIdentity{Version: "0.4.0", Commit: commitB},
		Role:          actorsocket.RoleCombined,
		NodeName:      &node,
		Database:      db,
		DumpFreeBytes: free,
	}
	return &fakeOwnMachine{m: OwnMachineFromStatus(st), ok: true}
}

// confirmingDrivers records the attempts whose external backup was confirmed.
type confirmingDrivers struct {
	*fakeDrivers
	mu        sync.Mutex
	confirmed []string
}

func (c *confirmingDrivers) ConfirmExternalBackup(id string) {
	c.mu.Lock()
	c.confirmed = append(c.confirmed, id)
	c.mu.Unlock()
}

func migratingFleet(t *testing.T, db actorsocket.Database) (*FleetRunner, *fakeFleetStore, *confirmingDrivers) {
	t.Helper()
	store := newFakeFleetStore(false)
	d := &fakeDrivers{store: store, outcome: map[string]string{}}
	f := ownedFleet(t, store, d, ownMachineWith(db, nil))
	c := &confirmingDrivers{fakeDrivers: d}
	f.self = c
	// testFleet's default: the fixture release migrates.
	return f, store, c
}

func migratingAttempt(t *testing.T, store *fakeFleetStore) Attempt {
	t.Helper()
	attempts, _ := store.RunAttempts(context.Background(), testRunID)
	for _, a := range attempts {
		if a.Target == TargetControlPlane {
			return a
		}
	}
	t.Fatalf("no control-plane attempt in %+v", attempts)
	return Attempt{}
}

func TestAMigratingStepOnAQuasarOwnedDatabaseIsSentForItsDump(t *testing.T) {
	f, store, d := migratingFleet(t, actorsocket.DatabaseOwned)
	run := runToEnd(t, f, store)
	if run.State != RunSucceeded {
		t.Fatalf("run = %q (%v), want succeeded", run.State, run.Error)
	}
	if steps := d.steps(); len(steps) == 0 || steps[0] != TargetControlPlane {
		t.Fatalf("steps = %v, want the control plane sent first", steps)
	}
	if len(d.confirmed) != 0 {
		t.Fatalf("confirmed = %v: nothing was confirmed", d.confirmed)
	}
}

func TestAMigratingStepOnAnOperatorsDatabaseWithoutConfirmationFailsBeforeAnythingMoves(t *testing.T) {
	f, store, d := migratingFleet(t, actorsocket.DatabaseExternal)
	run := runToEnd(t, f, store)
	if run.State != RunFailed {
		t.Fatalf("run = %q, want failed", run.State)
	}
	a := migratingAttempt(t, store)
	if a.State != AttemptFailed || a.Reason == nil || *a.Reason != ReasonBackupUnconfirmed {
		t.Fatalf("attempt = %+v, want failed backup_unconfirmed", a)
	}
	if steps := d.steps(); len(steps) != 0 {
		t.Fatalf("steps = %v, want nothing sent", steps)
	}
	if cordons, _ := store.CordonedHosts(context.Background(), testRunID); len(cordons) != 0 {
		t.Fatalf("cordons = %+v, want none: nothing was cordoned or drained", cordons)
	}
}

func TestAConfirmedBackupOfAnOperatorsDatabaseReachesTheStep(t *testing.T) {
	f, store, d := migratingFleet(t, actorsocket.DatabaseExternal)
	f.ConfirmExternalBackup(testRunID)
	run := runToEnd(t, f, store)
	if run.State != RunSucceeded {
		t.Fatalf("run = %q (%v), want succeeded", run.State, run.Error)
	}
	a := migratingAttempt(t, store)
	if len(d.confirmed) != 1 || d.confirmed[0] != a.ID {
		t.Fatalf("confirmed = %v, want the control-plane attempt %s", d.confirmed, a.ID)
	}
	// The confirmation dies with its run.
	if f.backupConfirmed(testRunID) {
		t.Fatal("the confirmation outlived its run")
	}
}

// The request the recovery actor receives: it migrates, names the schema it
// moves to and the version a restore returns to, and carries the confirmation.
func TestAMigratingControlPlaneRequestCarriesItsWayBack(t *testing.T) {
	req := ActorRequest(SelfRequest{
		RequestID: "7a1f6f1e-2c33-4a58-9a5e-0b6b0f7a1c22", Migrates: true, SchemaVersion: 81,
		ExternalBackupConfirmed: true, FromVersion: "0.3.0",
	})
	if !req.Migrates || req.SchemaVersion == nil || *req.SchemaVersion != 81 || !req.ExternalBackupConfirmed ||
		req.FromVersion == nil || *req.FromVersion != "0.3.0" {
		t.Fatalf("request = %+v", req)
	}
	// A request that does not migrate names no version: it has no restore.
	if plain := ActorRequest(SelfRequest{FromVersion: "0.3.0"}); plain.FromVersion != nil {
		t.Fatalf("a non-migrating request names from_version %q", *plain.FromVersion)
	}
}

// The failed migrating attempt's result names its dump; the self-applier keeps
// it on the attempt beside the output that ends with the restore command.
func TestAFailedMigratingResultRecordsItsDump(t *testing.T) {
	var dump actorsocket.Result
	if err := json.Unmarshal(fixtureBody(t, "result-failed-unhealthy-migrating-not-restored.json"), &dump); err != nil {
		t.Fatal(err)
	}
	res := resultOfActor(dump)
	if res.PreUpdateDump == nil || *res.PreUpdateDump != "20260925T100000Z-schema-80" || res.Restored {
		t.Fatalf("result = %+v, want the dump named and nothing restored", res)
	}
	last := res.Output[strings.LastIndex(res.Output, "\n")+1:]
	if !strings.HasPrefix(last, "docker exec quasar-recovery quasar-recovery restore --dump 20260925T100000Z-schema-80") {
		t.Fatalf("output ends %q, want the restore command", last)
	}
	store := &dumpStore{}
	s := &SelfApplier{store: store, log: testLogger()}
	if !s.record(context.Background(), "a1", res) {
		t.Fatal("a failed result did not resolve the attempt")
	}
	if store.dump != "20260925T100000Z-schema-80" || store.reason != string(actorsocket.ReasonUnhealthy) {
		t.Fatalf("recorded dump=%q reason=%q", store.dump, store.reason)
	}
}

// dumpStore is the sliver of selfStore record touches, plus the dump.
type dumpStore struct {
	selfStore
	dump, reason string
}

func (d *dumpStore) SetPreUpdateDump(_ context.Context, _, dump string) error {
	d.dump = dump
	return nil
}

func (d *dumpStore) FailAttempt(_ context.Context, _, reason, _ string) error {
	d.reason = reason
	return nil
}

func (d *dumpStore) SetPreviousDigests(context.Context, string, []PreviousDigest) error { return nil }

func TestBackupSpaceIsJudgedAgainstTheDumpTheDatabaseNeeds(t *testing.T) {
	owned := DatabaseModeOwned
	i64 := func(n int64) *int64 { return &n }
	yes, no := true, false
	for _, tc := range []struct {
		name     string
		migrates *bool
		free     *int64
		size     *int64
		status   string
		detail   string
	}{
		{"does not migrate", &no, nil, nil, CheckPass, "does not change the database"},
		{"nothing listed", nil, nil, nil, CheckPass, "does not change the database"},
		{"free space unreported", &yes, nil, i64(1_400_000_000), CheckUnknown, "has not been reported"},
		{"size unread", &yes, i64(212_000_000_000), nil, CheckUnknown, "size could not be read"},
		{"room", &yes, i64(212_000_000_000), i64(1_400_000_000), CheckPass, "about 1.4 GB; 212.0 GB is free"},
		{"no room", &yes, i64(600_000_000), i64(1_400_000_000), CheckFail, "needs about 1.5 GB and 600 MB is free"},
	} {
		f := PreflightFacts{
			OwnedActor:    &OwnedActorFact{Answered: true, DatabaseMode: &owned, DumpFreeBytes: tc.free},
			Migrates:      tc.migrates,
			DatabaseBytes: tc.size,
		}
		p := PlanPreflight(TargetControlPlane, f)
		c := p.Checks[len(p.Checks)-1]
		if c.ID != CheckBackupSpace || c.Status != tc.status || !strings.Contains(c.Detail, tc.detail) {
			t.Errorf("%s: %+v, want %s containing %q", tc.name, c, tc.status, tc.detail)
		}
		if (tc.status == CheckFail) != p.Blocked() {
			t.Errorf("%s: blocked=%v", tc.name, p.Blocked())
		}
	}
	// An operator's own database is never dumped, so it has no such check.
	external := DatabaseModeExternal
	p := PlanPreflight(TargetControlPlane, PreflightFacts{
		OwnedActor: &OwnedActorFact{Answered: true, DatabaseMode: &external}, Migrates: &yes,
	})
	for _, c := range p.Checks {
		if c.ID == CheckBackupSpace {
			t.Fatalf("external database carries %+v", c)
		}
	}
	if dumpSpaceNeeded(0) != 64<<20 || dumpSpaceNeeded(10_000_000_000) != 11_000_000_000 {
		t.Fatal("the space rule differs from the recovery actor's (quasar_recovery::dump::space_needed)")
	}
}
