package session

// compat_supersede_db_test.go — a mapping or default change supersedes every parked session.
// Semantics: control-api.md §Compatibility tools, "Hot Loading".

import (
	"context"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/compat"
)

// supersedeFixture wires the compat store to the coordinator as quasar-control does, with one
// parked Steam per user, one play session and one already-stopped parked row.
type supersedeFixture struct {
	*warmFixture
	cs                  *compat.Store
	admin, hotfix, p10  string
	parkedA, parkedB    string
	play, stoppedParked string
}

func newSupersedeFixture(t *testing.T) *supersedeFixture {
	t.Helper()
	f := &supersedeFixture{warmFixture: compatFixture(t, true)}
	ctx := context.Background()
	f.cs = compat.NewStore(f.pool)
	f.cs.Superseder = f.coord.SupersedeParked
	must(t, f.pool.QueryRow(ctx, `SELECT id::text FROM users LIMIT 1`).Scan(&f.admin))
	must(t, f.pool.QueryRow(ctx, `SELECT id::text FROM compat_tools WHERE steam_name = 'proton_hotfix'`).Scan(&f.hotfix))
	must(t, f.pool.QueryRow(ctx, `SELECT id::text FROM compat_tools WHERE steam_name = 'proton_10'`).Scan(&f.p10))
	f.parkedA = insertParked(t, f.pool, f.userID, f.steam, f.hostID, f.gpuID, 1, "10 minutes")
	f.parkedB = insertParked(t, f.pool, warmUser(t, f.pool, "u2"), f.steam, f.hostID, f.gpuID, 1, "10 minutes")
	play, err := f.store.ScheduleAndCreate(ctx, launchParams(f.seedIDs))
	must(t, err)
	f.play = play.ID
	f.running(t, f.play)
	f.stoppedParked = insertParked(t, f.pool, warmUser(t, f.pool, "u3"), f.steam, f.hostID, f.gpuID, 1, "10 minutes")
	must(t, exec(t, f.pool, `UPDATE sessions SET state = 'stopped', ended_at = now() WHERE id = $1::uuid`, f.stoppedParked))
	return f
}

func (f *supersedeFixture) assertSuperseded(t *testing.T, what string) {
	t.Helper()
	for _, id := range []string{f.parkedA, f.parkedB} {
		if got := readRow(t, f.pool, id); got.State != "stopping" || got.StopReason != StopReasonParkedSuperseded {
			t.Errorf("%s: parked %s = %+v, want stopping / parked_superseded", what, id, got)
		}
		if f.disp.waitStop(t, id) != StopReasonParkedSuperseded {
			t.Errorf("%s: parked %s was not sent a parked_superseded session_stop", what, id)
		}
	}
	if got := readRow(t, f.pool, f.play); got.State != "running" {
		t.Errorf("%s: play session = %+v, want left running", what, got)
	}
	if got := readRow(t, f.pool, f.stoppedParked); got.State != "stopped" || got.StopReason != "" {
		t.Errorf("%s: terminal parked row = %+v, want untouched", what, got)
	}
}

func (f *supersedeFixture) assertUntouched(t *testing.T, what string) {
	t.Helper()
	for _, id := range []string{f.parkedA, f.parkedB} {
		if got := readRow(t, f.pool, id); got.State != "running" || f.disp.stopReason(id) != "" {
			t.Errorf("%s: parked %s = %+v, want untouched", what, id, got)
		}
	}
}

func TestAMappingChangeSupersedesParkedSessionsOnly(t *testing.T) {
	f := newSupersedeFixture(t)
	ctx := context.Background()
	_, _, _, _, err := f.cs.SetMapping(ctx, "480", f.hotfix, false, f.admin)
	must(t, err)
	f.assertSuperseded(t, "new mapping")
}

func TestARepeatedMappingDoesNotSupersede(t *testing.T) {
	f := newSupersedeFixture(t)
	ctx := context.Background()
	_, _, _, _, err := f.cs.SetMapping(ctx, "1145360", f.hotfix, true, f.admin) // the fixture's own row
	must(t, err)
	f.assertUntouched(t, "identical mapping")
}

func TestForcingAnExistingMappingSupersedes(t *testing.T) {
	f := newSupersedeFixture(t)
	_, _, _, _, err := f.cs.SetMapping(context.Background(), "1145360", f.hotfix, false, f.admin)
	must(t, err)
	f.assertSuperseded(t, "forced flag changed")
}

func TestClearingAMappingSupersedesButClearingNothingDoesNot(t *testing.T) {
	f := newSupersedeFixture(t)
	ctx := context.Background()
	_, _, err := f.cs.ClearMapping(ctx, "480")
	must(t, err)
	f.assertUntouched(t, "clearing nothing")
	prev, _, err := f.cs.ClearMapping(ctx, "1145360")
	must(t, err)
	if prev == nil {
		t.Fatal("the fixture mapping was not cleared")
	}
	f.assertSuperseded(t, "cleared mapping")
}

func TestADefaultChangeSupersedesButTheSameDefaultDoesNot(t *testing.T) {
	f := newSupersedeFixture(t)
	ctx := context.Background()
	_, err := f.cs.SetDefault(ctx, &f.p10) // the fixture's default
	must(t, err)
	f.assertUntouched(t, "same default")
	_, err = f.cs.SetDefault(ctx, &f.hotfix)
	must(t, err)
	f.assertSuperseded(t, "new default")
}

func TestARefusedWriteSupersedesNothing(t *testing.T) {
	f := newSupersedeFixture(t)
	must(t, exec(t, f.pool, `UPDATE compat_tools SET enabled = false WHERE id = $1::uuid`, f.hotfix))
	_, _, _, _, err := f.cs.SetMapping(context.Background(), "480", f.hotfix, false, f.admin)
	if err != compat.ErrNotUsable {
		t.Fatalf("mapping to a disabled tool = %v, want ErrNotUsable", err)
	}
	f.assertUntouched(t, "refused write")
}
