// own_machine_wiring_db_test.go — the own-machine guard on Forget host (#1288)
// and Remove host reaches its handler only through one injection each in
// NewServices (crud.Handler.SetOwnMachine and RemoveDeps.SharesOwnMachine).
// Nothing else sets them, so deleting either line left every test green while
// the refusal silently disappeared (fails open, #1395). This drives the real
// NewServices wiring rather than a hand-built handler.
//
// TEST_DATABASE_URL-gated like every other DB test in this package; `make
// test-db` provisions the database that makes it run.
package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/auth"
	"github.com/accreleus/quasar/control-plane/internal/config"
	"github.com/accreleus/quasar/control-plane/internal/platform"
)

// ownMachineNode is the node name the configured machine shape names below, so
// a host row with it is the agent on the control plane's own machine.
const ownMachineNode = "own-machine"

func TestNewServicesWiresTheOwnMachineGuardOnForgetHost(t *testing.T) {
	// The shape is what makes a host "ours", and NewServices reads it from the
	// config: config.Load parses these two together.
	t.Setenv("QUASAR_MACHINE_ROLE", "combined")
	t.Setenv("QUASAR_MACHINE_NODE_NAME", ownMachineNode)
	// No jobs ticker: nothing on this path reads it, and a background run would
	// only race the rows this test reads.
	t.Setenv("QUASAR_JOBS", "0")

	pool := jobsTestDB(t)
	cfg := jobsTestConfig(t)

	svc, err := NewServices(cfg, pool, discardSlogLogger(), nil)
	if err != nil {
		t.Fatalf("NewServices: %v", err)
	}
	defer svc.Stop()

	ctx := context.Background()
	var hostID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO hosts (node_name, status) VALUES ($1, 'offline') RETURNING id::text`,
		ownMachineNode).Scan(&hostID); err != nil {
		t.Fatalf("seed the own-machine host: %v", err)
	}

	tok, err := svc.authSvc.MintEphemeral(ctx, auth.RoleAdmin, time.Minute)
	if err != nil {
		t.Fatalf("mint admin identity: %v", err)
	}
	// The real routes and the real auth gate, as RegisterRoutes wires them.
	mux := http.NewServeMux()
	svc.crudHandler.Register(mux, svc.authHandler.RequireAuth, svc.authHandler.RequireAdmin)

	req := httptest.NewRequest(http.MethodDelete, "/v1/hosts/"+hostID, nil)
	req.Header.Set("Authorization", "Bearer "+tok.Plaintext)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"host_not_removable"`) {
		t.Fatalf("forgetting this machine's own host: got %d %s, want 409 host_not_removable", rec.Code, rec.Body)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM hosts WHERE id::text = $1`, hostID).Scan(&rows); err != nil {
		t.Fatalf("count the host row: %v", err)
	}
	if rows != 1 {
		t.Fatal("the control plane's own host row was dropped")
	}

	// Remove host gets the same decision through RemoveDeps; nil there skips the
	// check, and its route cannot reach the check without a connected owned host,
	// so the wiring itself is what this asserts.
	if refuses, wired := svc.platformRemove.OwnMachineRefuses(ownMachineNode); !wired || !refuses {
		t.Fatal("NewServices did not give Remove host its own-machine guard, or it let this machine's host through")
	}
}

// TestNewServicesOwnMachineGuardRefusesWhatItCannotTellApart: the guard's answer
// comes from the configured machine shape, and a shape the configuration cannot
// produce — or no shape at all on an install that has its own recovery actor —
// cannot tell any host apart from the control plane's own machine. Both Forget
// and Remove then refuse, because dropping the wrong row is worse (#1395 part
// 2). A correctly shaped control plane still forgets a host on another machine:
// failing closed must not become refusing every removal.
func TestNewServicesOwnMachineGuardRefusesWhatItCannotTellApart(t *testing.T) {
	const otherHost = "another-machine"
	for _, c := range []struct {
		name  string
		shape func(*config.Config)
		want  int
	}{
		{"a role without its node name", func(cfg *config.Config) {
			cfg.MachineRole = platform.MachineRoleCombined
			cfg.MachineNodeName = ""
		}, http.StatusConflict},
		{"an owned install with no machine shape", func(cfg *config.Config) {
			cfg.RecoveryControlSocket = "/run/quasar/no-such-recovery.sock"
		}, http.StatusConflict},
		{"a shape naming another machine", func(cfg *config.Config) {
			cfg.MachineRole = platform.MachineRoleCombined
			cfg.MachineNodeName = ownMachineNode
		}, http.StatusNoContent},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("QUASAR_JOBS", "0")
			pool := jobsTestDB(t)
			cfg := jobsTestConfig(t)
			cfg.MachineRole, cfg.MachineNodeName = "", "" // only this case's shape
			c.shape(cfg)

			svc, err := NewServices(cfg, pool, discardSlogLogger(), nil)
			if err != nil {
				t.Fatalf("NewServices: %v", err)
			}
			defer svc.Stop()

			ctx := context.Background()
			var hostID string
			if err := pool.QueryRow(ctx,
				`INSERT INTO hosts (node_name, status) VALUES ($1, 'offline') RETURNING id::text`,
				otherHost).Scan(&hostID); err != nil {
				t.Fatalf("seed the other host: %v", err)
			}
			tok, err := svc.authSvc.MintEphemeral(ctx, auth.RoleAdmin, time.Minute)
			if err != nil {
				t.Fatalf("mint admin identity: %v", err)
			}
			mux := http.NewServeMux()
			svc.crudHandler.Register(mux, svc.authHandler.RequireAuth, svc.authHandler.RequireAdmin)

			req := httptest.NewRequest(http.MethodDelete, "/v1/hosts/"+hostID, nil)
			req.Header.Set("Authorization", "Bearer "+tok.Plaintext)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != c.want {
				t.Fatalf("forget %s = %d %s, want %d", otherHost, rec.Code, rec.Body, c.want)
			}
			var rows int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM hosts WHERE id::text = $1`, hostID).Scan(&rows); err != nil {
				t.Fatalf("count the host row: %v", err)
			}
			if rows != 1 && c.want == http.StatusConflict {
				t.Fatalf("%s: refused but dropped the row", c.name)
			}
			if rows != 0 && c.want == http.StatusNoContent {
				t.Fatalf("%s: forgot nothing", c.name)
			}
			if c.want == http.StatusConflict && !strings.Contains(rec.Body.String(), `"host_not_removable"`) {
				t.Errorf("refusal = %s, want the host_not_removable code", rec.Body)
			}

			// The same guard on Remove host, which asks the function NewServices
			// injected: an identity it cannot trust refuses this other host too.
			if refuses, wired := svc.platformRemove.OwnMachineRefuses(otherHost); !wired {
				t.Fatal("NewServices did not give Remove host its own-machine guard")
			} else if refuses != (c.want == http.StatusConflict) {
				t.Errorf("Remove host guard(%q) = %v, want refuses=%v", otherHost, refuses, c.want == http.StatusConflict)
			}
		})
	}
}
