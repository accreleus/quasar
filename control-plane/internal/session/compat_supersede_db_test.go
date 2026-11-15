package session

// compat_supersede_db_test.go — a mapping or default change supersedes every parked session.
// Semantics: control-api.md §Compatibility tools, "Hot Loading".

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

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
	_, _, _, err := f.cs.SetDefault(ctx, &f.p10) // the fixture's default
	must(t, err)
	f.assertUntouched(t, "same default")
	_, _, _, err = f.cs.SetDefault(ctx, &f.hotfix)
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

// wiredCompat is a warm fixture whose compat store supersedes through the coordinator, as in
// quasar-control.
func wiredCompat(t *testing.T) (*warmFixture, *compat.Store, string) {
	t.Helper()
	f := compatFixture(t, true)
	cs := compat.NewStore(f.pool)
	cs.Superseder = f.coord.SupersedeParked
	var hotfix string
	must(t, f.pool.QueryRow(context.Background(), `SELECT id::text FROM compat_tools WHERE steam_name = 'proton_hotfix'`).Scan(&hotfix))
	return f, cs, hotfix
}

// No parked Steam may outlive an effective mapping change it predates: a play that started before
// the write and ends after it must not leave an adoptable park.
func TestAPlayThatStartedBeforeAMappingChangeLeavesNoParkWhenItEnds(t *testing.T) {
	f, cs, hotfix := wiredCompat(t)
	ctx := context.Background()
	play := f.runningTile(t)

	var admin string
	must(t, f.pool.QueryRow(ctx, `SELECT id::text FROM users LIMIT 1`).Scan(&admin))
	_, _, _, _, err := cs.SetMapping(ctx, "1145360", hotfix, false, admin)
	must(t, err)
	f.coord.AgentState(ctx, f.hostID, heldStop(play))

	if got := readRow(t, f.pool, play); got.State != "stopped" {
		t.Fatalf("play row %+v, want stopped", got)
	}
	if k := f.parkedID(t); k != "" {
		t.Errorf("parked session %s survived the mapping change: the next launch would adopt a stale Steam", k)
	}
	if f.disp.stopReason(play) == "" {
		t.Error("no session_stop for the ended id: the held runtime is never released")
	}
}

func TestAPlayThatStartedAfterAMappingChangeStillParks(t *testing.T) {
	f, cs, hotfix := wiredCompat(t)
	ctx := context.Background()
	var admin string
	must(t, f.pool.QueryRow(ctx, `SELECT id::text FROM users LIMIT 1`).Scan(&admin))
	_, _, _, _, err := cs.SetMapping(ctx, "1145360", hotfix, false, admin)
	must(t, err)
	play := f.runningTile(t)

	f.coord.AgentState(ctx, f.hostID, heldStop(play))
	if f.parkedID(t) == "" {
		t.Error("a play that started after the change did not park")
	}
}

// A superseder that fails takes the mapping write with it.
func TestAFailingSupersederRollsBackTheWrite(t *testing.T) {
	f, cs, hotfix := wiredCompat(t)
	ctx := context.Background()
	boom := errors.New("supersede failed")
	cs.Superseder = func(context.Context, pgx.Tx) (func(), error) { return nil, boom }
	var admin string
	must(t, f.pool.QueryRow(ctx, `SELECT id::text FROM users LIMIT 1`).Scan(&admin))

	if _, _, _, _, err := cs.SetMapping(ctx, "480", hotfix, false, admin); !errors.Is(err, boom) {
		t.Errorf("SetMapping = %v, want the superseder's error", err)
	}
	if _, _, err := cs.ClearMapping(ctx, "1145360"); !errors.Is(err, boom) {
		t.Errorf("ClearMapping = %v, want the superseder's error", err)
	}
	if _, _, _, err := cs.SetDefault(ctx, &hotfix); !errors.Is(err, boom) {
		t.Errorf("SetDefault = %v, want the superseder's error", err)
	}
	var n int
	var def string
	must(t, f.pool.QueryRow(ctx, `SELECT count(*) FROM compat_tool_mappings WHERE external_id = '480'`).Scan(&n))
	if n != 0 {
		t.Error("the mapping for 480 was stored although the supersede failed")
	}
	must(t, f.pool.QueryRow(ctx, `SELECT count(*) FROM compat_tool_mappings WHERE external_id = '1145360'`).Scan(&n))
	if n != 1 {
		t.Error("the mapping for 1145360 was cleared although the supersede failed")
	}
	must(t, f.pool.QueryRow(ctx, `SELECT steam_name FROM compat_tools WHERE id = (SELECT compat_tool_default_id FROM instance_settings)`).Scan(&def))
	if def != "proton_10" {
		t.Errorf("default = %q, want it left at proton_10", def)
	}
}

// Writers that stop overlapping parked sets lock them in one order, so none of them deadlocks.
func TestConcurrentMappingWritersDoNotDeadlock(t *testing.T) {
	f, cs, hotfix := wiredCompat(t)
	ctx := context.Background()
	var admin string
	must(t, f.pool.QueryRow(ctx, `SELECT id::text FROM users LIMIT 1`).Scan(&admin))
	for i := 0; i < 6; i++ {
		insertParked(t, f.pool, warmUser(t, f.pool, fmt.Sprintf("w%d", i)), f.steam, f.hostID, f.gpuID, 0, "10 minutes")
	}
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 15; i++ {
				var err error
				switch (w + i) % 3 {
				case 0:
					_, _, _, _, err = cs.SetMapping(ctx, fmt.Sprint(500+w), hotfix, i%2 == 0, admin)
				case 1:
					_, _, err = cs.ClearMapping(ctx, fmt.Sprint(500+w))
				default:
					_, _, _, err = cs.SetDefault(ctx, nil)
					if err == nil {
						_, _, _, err = cs.SetDefault(ctx, &hotfix)
					}
				}
				if err != nil {
					errs <- err
				}
				if _, err := f.coord.ExpireParked(ctx); err != nil {
					errs <- err
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent writer failed: %v", err)
	}
}

// A claim that commits after the first clear and before the parked stop leaves a new play on the
// old Steam; only the second clear catches it.
func TestAClaimCommittingDuringTheMappingWriteEndsWithParkOnEndClear(t *testing.T) {
	f, cs, hotfix := wiredCompat(t)
	ctx := context.Background()
	k := f.park(t)
	var admin string
	must(t, f.pool.QueryRow(ctx, `SELECT id::text FROM users LIMIT 1`).Scan(&admin))

	var claimed string
	betweenParkOnEndClears = func() {
		betweenParkOnEndClears = nil
		res, err := f.coord.LaunchByProfile(ctx, f.userID, LaunchParams{AppID: f.steam, DeviceID: f.deviceID, Mic: true})
		if err != nil {
			t.Errorf("claim inside the write: %v", err)
			return
		}
		claimed = res.Session.ID
	}
	t.Cleanup(func() { betweenParkOnEndClears = nil })

	_, _, _, _, err := cs.SetMapping(ctx, "480", hotfix, false, admin)
	must(t, err)
	if claimed == "" {
		t.Fatal("the claim did not run")
	}
	if a := f.disp.assignFor(t, claimed); a.AdoptSessionID != k {
		t.Fatalf("the play adopted %q, want the parked session %s", a.AdoptSessionID, k)
	}
	var parkOnEnd bool
	must(t, f.pool.QueryRow(ctx, `SELECT park_on_end FROM sessions WHERE id = $1::uuid`, claimed).Scan(&parkOnEnd))
	if parkOnEnd {
		t.Error("the play that claimed the old Steam during the write can still park it when its game exits")
	}
}
