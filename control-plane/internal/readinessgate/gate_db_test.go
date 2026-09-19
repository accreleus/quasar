package readinessgate

// The readiness override and the report share one transaction shape
// (control-api.md "Readiness override"): lock the host row, re-read the stored
// report, write, recompute the verdict. These tests drive both through the
// package's own seam, which is the only place a PUT can be raced against a report.
//
// Requires Postgres (TEST_DATABASE_URL).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/migrate"
	"github.com/accreleus/quasar/control-plane/migrations"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	if err := migrate.Run(migrations.FS, dbURL); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM sessions; DELETE FROM gpus; DELETE FROM hosts;
		DELETE FROM admin_activity WHERE action LIKE 'host.readiness_override.%';
		DELETE FROM users WHERE email LIKE '%@gate.test';`); err != nil {
		pool.Close()
		t.Fatalf("reset: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

type fixture struct {
	pool   *pgxpool.Pool
	gate   *Gate
	hostID string
	admin  string
}

func setup(t *testing.T) fixture {
	t.Helper()
	pool := testPool(t)
	ctx := context.Background()
	f := fixture{pool: pool, gate: New(pool)}
	if err := pool.QueryRow(ctx, `INSERT INTO hosts (node_name, status, capacity_detection)
		VALUES ('gate-host','online','ok') RETURNING id::text`).Scan(&f.hostID); err != nil {
		t.Fatalf("seed host: %v", err)
	}
	for _, idx := range []int{0, 1} {
		if _, err := pool.Exec(ctx, `INSERT INTO gpus (host_id, index, vram_mb_total, encode_slots_total)
			VALUES ($1, $2, 16384, 4)`, f.hostID, idx); err != nil {
			t.Fatalf("seed gpu %d: %v", idx, err)
		}
	}
	if err := pool.QueryRow(ctx, `INSERT INTO users (email, username, password_hash, role)
		VALUES ('alice@gate.test','alice','x','admin') RETURNING id::text`).Scan(&f.admin); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	return f
}

const (
	hostCP  = `{"scope":"host","enforced_by":"control_plane"}`
	hostAg  = `{"scope":"host","enforced_by":"agent"}`
	gpu1CP  = `{"scope":"gpu","gpu_index":1,"enforced_by":"control_plane"}`
	homesCP = `{"scope":"homes","enforced_by":"control_plane"}`
)

func check(id, status, blocks string) string {
	if blocks == "" {
		return fmt.Sprintf(`{"id":%q,"status":%q,"summary":"s","remediation":""}`, id, status)
	}
	return fmt.Sprintf(`{"id":%q,"status":%q,"summary":"s","remediation":"","blocks":%s}`, id, status, blocks)
}

func report(checks ...string) json.RawMessage {
	out := "["
	for i, c := range checks {
		if i > 0 {
			out += ","
		}
		out += c
	}
	return json.RawMessage(out + "]")
}

func (f fixture) store(t *testing.T, raw json.RawMessage) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := f.gate.StoreReport(ctx, f.hostID, raw); err != nil {
		t.Fatalf("store report: %v", err)
	}
}

func (f fixture) blocked(t *testing.T) (host, homes bool, gpus map[int]bool) {
	t.Helper()
	ctx := context.Background()
	if err := f.pool.QueryRow(ctx, `SELECT readiness_block_host, readiness_block_homes FROM hosts WHERE id::text=$1`,
		f.hostID).Scan(&host, &homes); err != nil {
		t.Fatalf("read host verdict: %v", err)
	}
	gpus = map[int]bool{}
	rows, err := f.pool.Query(ctx, `SELECT index FROM gpus WHERE host_id::text=$1 AND readiness_blocked`, f.hostID)
	if err != nil {
		t.Fatalf("read gpu verdict: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var i int
		if err := rows.Scan(&i); err != nil {
			t.Fatal(err)
		}
		gpus[i] = true
	}
	return host, homes, gpus
}

func (f fixture) overrideIDs(t *testing.T) []string {
	t.Helper()
	rows, err := f.pool.Query(context.Background(),
		`SELECT check_id FROM host_readiness_overrides WHERE host_id::text=$1 ORDER BY check_id`, f.hostID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

type auditRow struct {
	Action   string
	Actor    *string
	TargetID *string
	Details  map[string]any
}

func (f fixture) audit(t *testing.T) []auditRow {
	t.Helper()
	rows, err := f.pool.Query(context.Background(), `
		SELECT action, actor_user_id::text, target_id, details FROM admin_activity
		WHERE action LIKE 'host.readiness_override.%' ORDER BY id`)
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var r auditRow
		var raw []byte
		if err := rows.Scan(&r.Action, &r.Actor, &r.TargetID, &raw); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &r.Details); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func ctx10(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// TestOverrideCreateRepeatClear: the whole admin lifecycle, the verdict
// recomputed by each write, and exactly one audit row per real change.
func TestOverrideCreateRepeatClear(t *testing.T) {
	f := setup(t)
	f.store(t, report(check("audio_probe", "fail", hostCP), check("media_probe_gpu1", "fail", gpu1CP)))
	if host, _, gpus := f.blocked(t); !host || !gpus[1] || gpus[0] {
		t.Fatalf("precondition: host=%v gpus=%v", host, gpus)
	}

	o, created, err := f.gate.SetOverride(ctx10(t), f.hostID, "audio_probe", f.admin)
	if err != nil || !created {
		t.Fatalf("set: created=%v err=%v", created, err)
	}
	if o.CheckID != "audio_probe" || o.CreatedBy == nil || *o.CreatedBy != f.admin ||
		o.CreatedByUsername == nil || *o.CreatedByUsername != "alice" || o.Inert || o.CreatedAt.IsZero() {
		t.Fatalf("override = %+v", o)
	}
	if host, _, gpus := f.blocked(t); host || !gpus[1] {
		t.Fatalf("after overriding the host check: host=%v gpus=%v; only that scope may clear", host, gpus)
	}

	again, created, err := f.gate.SetOverride(ctx10(t), f.hostID, "audio_probe", f.admin)
	if err != nil || created {
		t.Fatalf("repeat: created=%v err=%v, want the existing override and created=false", created, err)
	}
	if !again.CreatedAt.Equal(o.CreatedAt) {
		t.Fatalf("repeat rewrote created_at: %v → %v", o.CreatedAt, again.CreatedAt)
	}

	removed, err := f.gate.ClearOverride(ctx10(t), f.hostID, "audio_probe", f.admin)
	if err != nil || !removed {
		t.Fatalf("clear: removed=%v err=%v", removed, err)
	}
	if host, _, _ := f.blocked(t); !host {
		t.Fatal("withdrawing the override must block the host again in the same call")
	}
	removed, err = f.gate.ClearOverride(ctx10(t), f.hostID, "audio_probe", f.admin)
	if err != nil || removed {
		t.Fatalf("second clear: removed=%v err=%v, want idempotent", removed, err)
	}

	rows := f.audit(t)
	if len(rows) != 2 {
		t.Fatalf("audit rows = %+v, want one .set and one .cleared; idempotent repeats write none", rows)
	}
	for i, want := range []string{"host.readiness_override.set", "host.readiness_override.cleared"} {
		r := rows[i]
		if r.Action != want || r.Actor == nil || *r.Actor != f.admin || r.TargetID == nil || *r.TargetID != f.hostID ||
			r.Details["node_name"] != "gate-host" || r.Details["check_id"] != "audio_probe" {
			t.Fatalf("audit[%d] = %+v, want %s by the admin on the host with node_name and check_id", i, r, want)
		}
	}
}

// TestOverrideRefusedWhenItWouldExcludeNothing: every 409 case. Nothing is
// stored, nothing is audited, and the verdict does not move.
func TestOverrideRefusedWhenItWouldExcludeNothing(t *testing.T) {
	cases := []struct {
		name    string
		report  json.RawMessage
		checkID string
	}{
		{"the report has no such check", report(check("audio_probe", "fail", hostCP)), "input_probe"},
		{"the check is a proxy (no blocks)", report(check("render_node", "fail", "")), "render_node"},
		{"the check passes", report(check("audio_probe", "pass", hostCP)), "audio_probe"},
		{"the check is unknown", report(check("audio_probe", "unknown", hostCP)), "audio_probe"},
		{"the check is a warning", report(check("homes_free_space", "warn", homesCP)), "homes_free_space"},
		{"the check is agent-enforced", report(check("startup_cleanup", "fail", hostAg)), "startup_cleanup"},
		{"an explicit empty report", report(), "audio_probe"},
		{"never reported", nil, "audio_probe"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t)
			if tc.report != nil {
				f.store(t, tc.report)
			}
			beforeHost, beforeHomes, beforeGPUs := f.blocked(t)
			_, created, err := f.gate.SetOverride(ctx10(t), f.hostID, tc.checkID, f.admin)
			var refused *NotOverridableError
			if !errors.As(err, &refused) || created {
				t.Fatalf("created=%v err=%v, want *NotOverridableError", created, err)
			}
			if refused.Reason == "" {
				t.Fatal("the refusal must say which precondition failed")
			}
			if ids := f.overrideIDs(t); len(ids) != 0 {
				t.Fatalf("a refused override was stored: %v", ids)
			}
			if rows := f.audit(t); len(rows) != 0 {
				t.Fatalf("a refused override was audited: %+v", rows)
			}
			h, ho, g := f.blocked(t)
			if h != beforeHost || ho != beforeHomes || len(g) != len(beforeGPUs) {
				t.Fatal("a refused override moved the verdict")
			}
		})
	}
}

// TestOverrideRepeatStillNeedsThePrecondition: idempotency is for a repeat
// that would succeed. The row is held either way; only a pass lapses it.
func TestOverrideRepeatStillNeedsThePrecondition(t *testing.T) {
	for name, next := range map[string]json.RawMessage{
		"the check became agent-enforced": report(check("audio_probe", "fail", hostAg)),
		"the check is now unknown":        report(check("audio_probe", "unknown", hostCP)),
		"the check id vanished":           report(check("input_probe", "fail", hostCP)),
	} {
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			f.store(t, report(check("audio_probe", "fail", hostCP)))
			if _, _, err := f.gate.SetOverride(ctx10(t), f.hostID, "audio_probe", f.admin); err != nil {
				t.Fatal(err)
			}
			f.store(t, next)
			_, created, err := f.gate.SetOverride(ctx10(t), f.hostID, "audio_probe", f.admin)
			var refused *NotOverridableError
			if !errors.As(err, &refused) || created {
				t.Fatalf("repeat: created=%v err=%v, want *NotOverridableError", created, err)
			}
			if ids := f.overrideIDs(t); len(ids) != 1 {
				t.Fatalf("override ids = %v: a refused repeat must not remove the stored decision", ids)
			}
			if n := len(f.audit(t)); n != 1 {
				t.Fatalf("%d audit rows, want only the original .set", n)
			}
		})
	}
}

func TestOverrideUnknownHost(t *testing.T) {
	f := setup(t)
	const nobody = "00000000-0000-4000-8000-000000000000"
	if _, _, err := f.gate.SetOverride(ctx10(t), nobody, "audio_probe", f.admin); !errors.Is(err, ErrHostNotFound) {
		t.Fatalf("set on an unknown host: %v, want ErrHostNotFound", err)
	}
	if _, err := f.gate.ClearOverride(ctx10(t), nobody, "audio_probe", f.admin); !errors.Is(err, ErrHostNotFound) {
		t.Fatalf("clear on an unknown host: %v, want ErrHostNotFound", err)
	}
}

// TestOverrideLapsesOnPassAndOnlyOnPass: lapse happens in the transaction that
// stores the passing report, is audited with a null actor, and cannot mask a
// later regression.
func TestOverrideLapsesOnPassAndOnlyOnPass(t *testing.T) {
	held := []struct {
		name string
		next json.RawMessage
	}{
		{"still failing", report(check("audio_probe", "fail", hostCP))},
		{"unknown", report(check("audio_probe", "unknown", hostCP))},
		{"warn", report(check("audio_probe", "warn", hostCP))},
		{"skip", report(check("audio_probe", "skip", hostCP))},
		{"explicit empty report", report()},
		{"the id vanished", report(check("input_probe", "pass", hostCP))},
	}
	for _, tc := range held {
		t.Run("held: "+tc.name, func(t *testing.T) {
			f := setup(t)
			f.store(t, report(check("audio_probe", "fail", hostCP)))
			if _, _, err := f.gate.SetOverride(ctx10(t), f.hostID, "audio_probe", f.admin); err != nil {
				t.Fatal(err)
			}
			f.store(t, tc.next)
			if ids := f.overrideIDs(t); len(ids) != 1 {
				t.Fatalf("override ids = %v, want it held", ids)
			}
			for _, r := range f.audit(t) {
				if r.Action == "host.readiness_override.lapsed" {
					t.Fatalf("a lapse was audited: %+v", r)
				}
			}
		})
	}

	t.Run("pass: lapses, audited by the system, and a later failure blocks again", func(t *testing.T) {
		f := setup(t)
		f.store(t, report(check("audio_probe", "fail", hostCP), check("media_probe_gpu1", "fail", gpu1CP)))
		for _, id := range []string{"audio_probe", "media_probe_gpu1"} {
			if _, _, err := f.gate.SetOverride(ctx10(t), f.hostID, id, f.admin); err != nil {
				t.Fatal(err)
			}
		}
		f.store(t, report(check("audio_probe", "pass", hostCP), check("media_probe_gpu1", "fail", gpu1CP)))
		if ids := f.overrideIDs(t); len(ids) != 1 || ids[0] != "media_probe_gpu1" {
			t.Fatalf("override ids = %v, want only the still-failing check's", ids)
		}
		var lapsed []auditRow
		for _, r := range f.audit(t) {
			if r.Action == "host.readiness_override.lapsed" {
				lapsed = append(lapsed, r)
			}
		}
		if len(lapsed) != 1 || lapsed[0].Actor != nil || lapsed[0].Details["check_id"] != "audio_probe" ||
			lapsed[0].Details["node_name"] != "gate-host" {
			t.Fatalf("lapse audit = %+v, want one row, actor null, naming the check and the host", lapsed)
		}

		f.store(t, report(check("audio_probe", "fail", hostCP), check("media_probe_gpu1", "fail", gpu1CP)))
		host, _, gpus := f.blocked(t)
		if !host {
			t.Fatal("the lapsed override still masks the regression")
		}
		if gpus[1] {
			t.Fatal("the other override was lost with the lapse")
		}
	})
}

// TestOverridesListedWithInertAndAuthor: what the host body serves.
func TestOverridesListedWithInertAndAuthor(t *testing.T) {
	f := setup(t)
	f.store(t, report(check("audio_probe", "fail", hostCP), check("input_probe", "fail", hostCP)))
	for _, id := range []string{"audio_probe", "input_probe"} {
		if _, _, err := f.gate.SetOverride(ctx10(t), f.hostID, id, f.admin); err != nil {
			t.Fatal(err)
		}
	}
	// The agent renames one check: that override is now inert, and the host is
	// blocked again by the renamed check until an admin decides again.
	f.store(t, report(check("audio_probe", "fail", hostCP), check("virtual_input_probe", "fail", hostCP)))
	if host, _, _ := f.blocked(t); !host {
		t.Fatal("a renamed failing check must block: the old override is inert")
	}

	byHost, err := f.gate.Overrides(ctx10(t), []string{f.hostID})
	if err != nil {
		t.Fatal(err)
	}
	got := byHost[f.hostID]
	if len(got) != 2 || got[0].CheckID != "audio_probe" || got[0].Inert || got[1].CheckID != "input_probe" || !got[1].Inert {
		t.Fatalf("overrides = %+v, want audio_probe live and input_probe inert, ordered by check id", got)
	}

	// An inert override can be withdrawn.
	if removed, err := f.gate.ClearOverride(ctx10(t), f.hostID, "input_probe", f.admin); err != nil || !removed {
		t.Fatalf("clear inert: removed=%v err=%v", removed, err)
	}

	// The author's account goes away: created_by and the username both read null.
	if _, err := f.pool.Exec(context.Background(), `DELETE FROM users WHERE id::text=$1`, f.admin); err != nil {
		t.Fatal(err)
	}
	byHost, err = f.gate.Overrides(ctx10(t), []string{f.hostID})
	if err != nil {
		t.Fatal(err)
	}
	if got := byHost[f.hostID]; len(got) != 1 || got[0].CreatedBy != nil || got[0].CreatedByUsername != nil {
		t.Fatalf("after the author is deleted: %+v, want created_by and created_by_username null", got)
	}
}

// TestOverrideRacingAReport: whichever order the two transactions take, the
// derived column ends describing the stored report and the stored overrides.
func TestOverrideRacingAReport(t *testing.T) {
	f := setup(t)
	failing := report(check("audio_probe", "fail", hostCP))
	passing := report(check("audio_probe", "pass", hostCP))

	for i := 0; i < 25; i++ {
		// From a failing, unoverridden state: an override races a passing report.
		f.store(t, failing)
		if _, err := f.gate.ClearOverride(ctx10(t), f.hostID, "audio_probe", f.admin); err != nil {
			t.Fatal(err)
		}
		last := passing
		if i%2 == 1 {
			// And the mirror: from passing, an override races a failing report.
			f.store(t, passing)
			last = failing
		}

		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		var wg sync.WaitGroup
		var setErr, storeErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _, setErr = f.gate.SetOverride(ctx, f.hostID, "audio_probe", f.admin)
		}()
		go func() {
			defer wg.Done()
			storeErr = f.gate.StoreReport(ctx, f.hostID, last)
		}()
		wg.Wait()
		cancel()

		if storeErr != nil {
			t.Fatalf("round %d: report: %v", i, storeErr)
		}
		var refused *NotOverridableError
		if setErr != nil && !errors.As(setErr, &refused) {
			t.Fatalf("round %d: override: %v (a deadlock or a timeout is a failure)", i, setErr)
		}

		overridden := len(f.overrideIDs(t)) == 1
		host, _, _ := f.blocked(t)
		if i%2 == 0 {
			// The stored report passes: no override may survive it, nothing blocks.
			if overridden || host {
				t.Fatalf("round %d (ends passing): overridden=%v blocked=%v, want neither", i, overridden, host)
			}
		} else if host == overridden {
			// The stored report fails: blocked exactly when not overridden.
			t.Fatalf("round %d (ends failing): overridden=%v blocked=%v, the column is stale", i, overridden, host)
		}
	}
}

// TestRecomputeInsideACallersTransaction: the capacity write keeps deriving
// the verdict for a GPU that appears after the report naming it.
func TestRecomputeInsideACallersTransaction(t *testing.T) {
	f := setup(t)
	f.store(t, report(check("media_probe_gpu2", "fail", `{"scope":"gpu","gpu_index":2,"enforced_by":"control_plane"}`)))
	if _, _, gpus := f.blocked(t); len(gpus) != 0 {
		t.Fatalf("gpu 2 does not exist yet, got %v", gpus)
	}
	ctx := ctx10(t)
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	// Host row first, as the capacity write does; gpus-then-host inverts against admission.
	if _, err := tx.Exec(ctx, `UPDATE hosts SET cpu_cores = 8 WHERE id::text = $1`, f.hostID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO gpus (host_id, index, vram_mb_total, encode_slots_total)
		VALUES ($1, 2, 16384, 4)`, f.hostID); err != nil {
		t.Fatal(err)
	}
	if err := f.gate.Recompute(ctx, tx, f.hostID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, gpus := f.blocked(t); !gpus[2] || len(gpus) != 1 {
		t.Fatalf("blocked gpus = %v, want exactly the new gpu 2", gpus)
	}
}
