// A migrating update of an owned control plane (#364) against a real Postgres:
// the fleet apply's external-backup confirmation through the admin route and
// its audit, and the attempt's pre-update dump reference (migration 0097).
package platform

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/actorsocket"
	"github.com/accreleus/quasar/control-plane/internal/buildinfo"
)

// confirmingSucceed resolves every attempt succeeded and records the attempts
// whose operator confirmed an external backup.
type confirmingSucceed struct {
	*succeedingDrivers
	mu        sync.Mutex
	confirmed []string
}

func (c *confirmingSucceed) ConfirmExternalBackup(id string) {
	c.mu.Lock()
	c.confirmed = append(c.confirmed, id)
	c.mu.Unlock()
}

func (c *confirmingSucceed) seen() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.confirmed...)
}

func applyAudit(t *testing.T, h *fleetHarness, runRelease string) []map[string]any {
	t.Helper()
	rows, err := h.pool.Query(context.Background(), `
		SELECT details FROM admin_activity
		 WHERE action = 'platform.apply.run' AND target_id = $1 ORDER BY created_at`, runRelease)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var d map[string]any
		if err := json.Unmarshal(raw, &d); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

func TestAMigratingFleetApplyOnAnOperatorsDatabaseNeedsItsBackupConfirmed(t *testing.T) {
	drivers := &confirmingSucceed{succeedingDrivers: &succeedingDrivers{}}
	h := newFleetHarness(t, commitA, drivers)
	drivers.store = h.store
	h.fleet.WithMachineShape(MachineShape{Role: MachineRoleControlOnly, NodeName: "attic-server"}).
		WithOwnMachine(ownMachineWith(actorsocket.DatabaseExternal, nil))
	migrating := seedRelease(t, h.store, commitC, buildinfo.Get().SchemaVersion+1)
	ctx := context.Background()

	finish := func(raw []byte) ApplyRun {
		run := decodeRun(t, raw)
		waitFor(t, "the run to finish", func() bool {
			r, err := h.store.Run(ctx, run.ID)
			return err == nil && TerminalRunState(r.State)
		})
		final, _ := h.store.Run(ctx, run.ID)
		final.Attempts, _ = h.store.RunAttempts(ctx, run.ID)
		return final
	}

	code, raw := h.do(t, http.MethodPost, "/v1/admin/platform/apply", h.admin, FleetApplyRequest{ReleaseID: migrating.ID})
	if code != http.StatusAccepted {
		t.Fatalf("POST apply = %d %s, want 202", code, raw)
	}
	unconfirmed := finish(raw)
	if unconfirmed.State != RunFailed || len(unconfirmed.Attempts) != 1 {
		t.Fatalf("run = %+v, want failed at its control-plane step", unconfirmed)
	}
	a := unconfirmed.Attempts[0]
	if a.State != AttemptFailed || a.Reason == nil || *a.Reason != ReasonBackupUnconfirmed ||
		!strings.Contains(a.Output, "Nothing was changed") {
		t.Fatalf("attempt = %+v, want failed backup_unconfirmed saying nothing changed", a)
	}
	var status string
	if err := h.pool.QueryRow(ctx, `SELECT status FROM hosts WHERE id = $1::uuid`, h.hostID).Scan(&status); err != nil || status != "online" {
		t.Fatalf("host status = %q (%v), want online: nothing was cordoned", status, err)
	}
	if len(drivers.seen()) != 0 {
		t.Fatalf("confirmed %v without a confirmation", drivers.seen())
	}

	code, raw = h.do(t, http.MethodPost, "/v1/admin/platform/apply", h.admin,
		map[string]any{"release_id": migrating.ID, "external_backup_confirmed": true})
	if code != http.StatusAccepted {
		t.Fatalf("POST apply = %d %s, want 202", code, raw)
	}
	confirmed := finish(raw)
	var cpAttempt string
	for _, at := range confirmed.Attempts {
		if at.Target == TargetControlPlane {
			cpAttempt = at.ID
		}
	}
	if got := drivers.seen(); len(got) != 1 || got[0] != cpAttempt {
		t.Fatalf("confirmed %v, want the control-plane attempt %s", got, cpAttempt)
	}
	audits := applyAudit(t, h, migrating.ID)
	if len(audits) != 2 || audits[0]["external_backup_confirmed"] != false || audits[1]["external_backup_confirmed"] != true {
		t.Fatalf("audit = %v, want the confirmation recorded beside force", audits)
	}
}

func TestThePreUpdateDumpIsKeptOnControlPlaneAttemptsOnly(t *testing.T) {
	h := newApplyHarness(t)
	ctx := context.Background()
	cpA, err := h.store.CreateControlPlaneAttempt(ctx, NewControlPlaneAttempt{
		ReleaseID: &h.release.ID,
		Requested: []ComponentDigest{cpComponent()},
		Previous:  unknownPrevious([]ComponentDigest{cpComponent()}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if cpA.PreUpdateDump != nil {
		t.Fatalf("a new attempt names dump %q", *cpA.PreUpdateDump)
	}
	if err := h.store.SetPreUpdateDump(ctx, cpA.ID, "20260925T140200Z-schema-88"); err != nil {
		t.Fatal(err)
	}
	// A name the column cannot hold is never written, so it cannot fail a write.
	if err := h.store.SetPreUpdateDump(ctx, cpA.ID, strings.Repeat("x", 256)); err != nil {
		t.Fatal(err)
	}
	if err := h.store.FailAttempt(ctx, cpA.ID, ReasonUnhealthy, "…\ndocker exec quasar-recovery quasar-recovery restore --dump 20260925T140200Z-schema-88 --to 0.5.2"); err != nil {
		t.Fatal(err)
	}
	got, err := h.store.Attempt(ctx, cpA.ID)
	if err != nil || got.PreUpdateDump == nil || *got.PreUpdateDump != "20260925T140200Z-schema-88" {
		t.Fatalf("attempt = %+v err=%v, want the dump kept through the failure", got, err)
	}
	raw, _ := json.Marshal(got)
	if !strings.Contains(string(raw), `"pre_update_dump":"20260925T140200Z-schema-88"`) {
		t.Fatalf("wire = %s", raw)
	}

	// The column's CHECK: a host attempt never names one.
	if _, err := h.pool.Exec(ctx, `
		INSERT INTO platform_apply_attempts (kind, target, host_id, requested_digests, previous_digests, state, pre_update_dump)
		VALUES ('apply', 'host', $1::uuid, '[]', '[]', 'failed', 'x')`, h.hostID); err == nil {
		t.Fatal("a host attempt stored a pre-update dump")
	}
	hostA, err := h.store.ListAttempts(ctx, h.hostID, 1)
	if err == nil && len(hostA) == 1 && hostA[0].PreUpdateDump != nil {
		t.Fatalf("host attempt = %+v", hostA[0])
	}
	n, err := h.store.DatabaseBytes(ctx)
	if err != nil || n <= 0 {
		t.Fatalf("database size = %d (%v)", n, err)
	}
}

// The developer apply's control-plane target across a schema change (#364): a
// Quasar-owned database is sent for its dump, an operator's own database needs
// the confirmation, and below the database is refused as before.
func TestADeveloperApplyAcrossAMigrationFollowsTheDatabaseRule(t *testing.T) {
	installed := buildinfo.Get().SchemaVersion
	for _, tc := range []struct {
		name      string
		schema    int
		db        actorsocket.Database
		confirmed bool
		wantCode  int
		wantState string
		wantStart bool
	}{
		{"below", installed - 1, actorsocket.DatabaseOwned, false, http.StatusUnprocessableEntity, "", false},
		{"owned", installed + 1, actorsocket.DatabaseOwned, false, http.StatusAccepted, AttemptQueued, true},
		{"external unconfirmed", installed + 1, actorsocket.DatabaseExternal, false, http.StatusAccepted, AttemptFailed, false},
		{"external confirmed", installed + 1, actorsocket.DatabaseExternal, true, http.StatusAccepted, AttemptQueued, true},
	} {
		conf := &confirmingSelfDev{recordingSelfDev: &recordingSelfDev{}}
		h := newControlDevHarnessWith(t, ownMachineWith(tc.db, nil), conf)
		rec := conf.recordingSelfDev
		h.images.schema = tc.schema
		body := controlBody(cpComponent())
		body["external_backup_confirmed"] = tc.confirmed
		code, out := h.post(t, devURL, h.adminToken, body)
		if code != tc.wantCode {
			t.Fatalf("%s: = %d %s, want %d", tc.name, code, out, tc.wantCode)
		}
		if code != http.StatusAccepted {
			continue
		}
		var env AttemptEnvelope
		if err := json.Unmarshal(out, &env); err != nil {
			t.Fatal(err)
		}
		if env.Attempt.State != tc.wantState {
			t.Fatalf("%s: attempt = %+v, want %s", tc.name, env.Attempt, tc.wantState)
		}
		if tc.wantState == AttemptFailed && (env.Attempt.Reason == nil || *env.Attempt.Reason != ReasonBackupUnconfirmed) {
			t.Fatalf("%s: attempt = %+v, want backup_unconfirmed", tc.name, env.Attempt)
		}
		if (rec.count() == 1) != tc.wantStart {
			t.Fatalf("%s: started %d, want %v", tc.name, rec.count(), tc.wantStart)
		}
		if tc.confirmed != (len(conf.confirmed) == 1) {
			t.Fatalf("%s: confirmed %v", tc.name, conf.confirmed)
		}
		if tc.wantStart && (len(conf.noted) != 1 || conf.noted[0] != tc.schema) {
			t.Fatalf("%s: noted schemas %v, want [%d] read once at admission", tc.name, conf.noted, tc.schema)
		}
	}
}

type confirmingSelfDev struct {
	*recordingSelfDev
	confirmed []string
	noted     []int
}

func (c *confirmingSelfDev) ConfirmExternalBackup(id string) { c.confirmed = append(c.confirmed, id) }

func (c *confirmingSelfDev) NoteDeveloperSchema(_ string, schema int, _ bool) {
	c.noted = append(c.noted, schema)
}

// newControlDevHarnessWith is newControlDevHarness with the machine's own
// recovery actor and the control-plane driver chosen by the test.
func newControlDevHarnessWith(t *testing.T, own OwnMachineSource, dev controlPlaneDeveloper) *devHarness {
	t.Helper()
	images := &fakeDevImages{commit: commitB}
	h := newApplyHarness(t, func(_ *applyHarness, handler *ApplyHandler) {
		handler.WithDeveloperApply(images, []string{"registry.example.invalid/dev"}).
			WithOwnMachine(own).
			WithMachineShape(MachineShape{Role: MachineRoleCombined, NodeName: "gpu-01"}).
			WithSelfDeveloper(dev)
	})
	mustExec(t, h.pool, `UPDATE hosts SET install_mode = 'owned' WHERE id = $1::uuid`, h.hostID)
	return &devHarness{applyHarness: h, images: images}
}
