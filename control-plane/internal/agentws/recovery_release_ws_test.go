package agentws

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/hostcfg"
)

// A reconciled connection re-sends every requested release the agent has not
// reported, and only to an agent that advertised recovery_release (#656).
func TestAReconciledConnectionResendsPendingRecoveryReleases(t *testing.T) {
	pool := testPool(t)
	store := hostcfg.NewStore(pool)
	ctx := context.Background()
	hostID := seedHost(t, pool)
	boot, err := store.StartRH05Boot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	connection := "00000000-0000-4000-8000-000000000656"
	attempt := "00000000-0000-4000-8000-000000000657"
	if _, err := pool.Exec(ctx, `INSERT INTO host_journal_reconciliation(host_id,boot_incarnation,connection_incarnation,state,completed_at)
		VALUES($1::uuid,$2::uuid,$3::uuid,'complete',now())
		ON CONFLICT(host_id) DO UPDATE SET connection_incarnation=excluded.connection_incarnation,state='complete'`, hostID, boot, connection); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO host_config_attempts(id,host_id,group_key,approved_digest,approved_revision,scope,
		boot_incarnation,grant_connection,phase,journal_sequence,started_at,terminal_at,release_requested_at)
		VALUES($1::uuid,$2::uuid,'hardware',repeat('a',64),1,'restart',$3::uuid,$4::uuid,'uncertain',2,now(),now(),now())`,
		attempt, hostID, boot, connection); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := NewHandler(pool, log, NewRegistry(log), nil, nil, store, nil, boot)
	t.Cleanup(h.Close)
	for _, capable := range []bool{false, true} {
		c := newConn(hostID, nil)
		c.policyTyped, c.policyIdle, c.policyRecoveryRelease = true, true, capable
		c.bootIncarnation, c.connectionIncarnation = h.bootIncarnation, connection
		h.registry.add(c)
		if err := h.sendRecoveryReleases(ctx, c); err != nil {
			t.Fatal(err)
		}
		h.registry.remove(c)
		select {
		case raw := <-c.out:
			var cmd hostcfg.RecoveryReleaseCmd
			if !capable {
				t.Fatalf("sent to an agent without recovery_release: %s", raw)
			}
			if err := json.Unmarshal(raw, &cmd); err != nil || cmd != hostcfg.NewRecoveryReleaseCmd(attempt, h.bootIncarnation, connection) {
				t.Fatalf("release = %s (%v)", raw, err)
			}
		default:
			if capable {
				t.Fatal("a capable reconciled connection sent no release")
			}
		}
	}
}
