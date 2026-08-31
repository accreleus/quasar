package invites

// invites_created_by_test.go — Invite.created_by_user_id / created_by_username
// and ?state=pending (UI v3 amendment §6). Requires Postgres: make test-db.
//
// NO MIGRATION: invites.created_by has been written since migration 0020 and was
// simply never served.

import (
	"context"
	"testing"
	"time"
)

func TestListServesMintingAdmin(t *testing.T) {
	pool := testDB(t)
	admin := seedAdmin(t, pool)
	s := NewStore(pool)
	ctx := context.Background()

	if _, _, err := s.Mint(ctx, MintParams{CreatedBy: admin}); err != nil {
		t.Fatalf("mint: %v", err)
	}

	list, err := s.List(ctx, StateAll)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("listed %d invites, want 1", len(list))
	}
	inv := list[0]
	if inv.CreatedByUserID == nil || *inv.CreatedByUserID != admin {
		t.Errorf("created_by_user_id = %v, want %s", inv.CreatedByUserID, admin)
	}
	if inv.CreatedByUsername == nil || *inv.CreatedByUsername != "admin" {
		t.Errorf("created_by_username = %v, want admin", inv.CreatedByUsername)
	}
}

// TestListSurvivesAMissingMinter is why the join is LEFT.
//
// Constructing the state takes a deliberate step, and that is worth recording:
// invites.created_by is NOT NULL ON DELETE CASCADE, so deleting an admin takes
// their invites with them — and that is a SECURITY property, not an oversight
// (the codes they minted stop redeeming). A dangling created_by is therefore not
// reachable through the API. The LEFT JOIN keeps the read path honest if a
// restore or a manual repair ever produces one: the invite must list as "minted
// by someone we can no longer name", never vanish.
//
// The orphan is built with `SET LOCAL session_replication_role = replica`, which
// suspends FK triggers for ONE transaction and reverts at commit. Deliberately
// not `ALTER TABLE ... DROP CONSTRAINT`: this database is shared by every
// package under -p 1, and a test killed between the DROP and its cleanup would
// leave the whole suite running without the constraint. SET LOCAL cannot outlive
// its transaction, so there is no window in which that is possible.
func TestListSurvivesAMissingMinter(t *testing.T) {
	pool := testDB(t)
	admin := seedAdmin(t, pool)
	s := NewStore(pool)
	ctx := context.Background()

	inv, _, err := s.Mint(ctx, MintParams{CreatedBy: admin})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck — no-op after commit
	if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
		t.Fatalf("suspend fk triggers: %v", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM users WHERE id::text = $1`, admin); err != nil {
		t.Fatalf("delete admin: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit orphan: %v", err)
	}
	// The row is committed and dangling; drop it so nothing downstream inherits
	// an invite whose FK can no longer be satisfied.
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(),
			`DELETE FROM invites WHERE id::text = $1`, inv.ID); err != nil {
			t.Errorf("cleanup: delete orphan invite: %v", err)
		}
	})

	list, err := s.List(ctx, StateAll)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("listed %d invites, want 1 — the invite must survive its minter", len(list))
	}
	if list[0].CreatedByUsername != nil {
		t.Errorf("created_by_username = %q, want nil for a deleted minter", *list[0].CreatedByUsername)
	}
	if list[0].CreatedByUserID == nil {
		t.Error("created_by_user_id was dropped; the id is the only handle left")
	}
}

// TestPendingFilterExcludesEveryDeadInvite: the three exclusions are one fact —
// "this code no longer works" — and the client cannot compute the used-up one
// for rows it has not loaded.
func TestPendingFilterExcludesEveryDeadInvite(t *testing.T) {
	pool := testDB(t)
	admin := seedAdmin(t, pool)
	s := NewStore(pool)
	ctx := context.Background()

	live, _, err := s.Mint(ctx, MintParams{CreatedBy: admin, MaxUses: 2})
	if err != nil {
		t.Fatalf("mint live: %v", err)
	}
	// Partly used but not exhausted: still pending.
	partlyUsed, code, err := s.Mint(ctx, MintParams{CreatedBy: admin, MaxUses: 2})
	if err != nil {
		t.Fatalf("mint partly used: %v", err)
	}
	if _, err := Redeem(ctx, pool, code); err != nil {
		t.Fatalf("redeem once: %v", err)
	}
	// Exhausted.
	usedUp, code, err := s.Mint(ctx, MintParams{CreatedBy: admin, MaxUses: 1})
	if err != nil {
		t.Fatalf("mint used-up: %v", err)
	}
	if _, err := Redeem(ctx, pool, code); err != nil {
		t.Fatalf("redeem used-up: %v", err)
	}
	// Expired.
	past := time.Now().Add(-time.Hour)
	expired, _, err := s.Mint(ctx, MintParams{CreatedBy: admin})
	if err != nil {
		t.Fatalf("mint expired: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE invites SET expires_at = $2 WHERE id::text = $1`, expired.ID, past); err != nil {
		t.Fatalf("expire: %v", err)
	}
	// Revoked.
	revoked, _, err := s.Mint(ctx, MintParams{CreatedBy: admin})
	if err != nil {
		t.Fatalf("mint revoked: %v", err)
	}
	if err := s.Revoke(ctx, revoked.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	all, err := s.List(ctx, StateAll)
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 5 {
		t.Fatalf("state=all listed %d, want 5 (the default filters nothing)", len(all))
	}

	pending, err := s.List(ctx, StatePending)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	want := map[string]bool{live.ID: true, partlyUsed.ID: true}
	if len(pending) != len(want) {
		t.Fatalf("state=pending listed %d, want %d", len(pending), len(want))
	}
	for _, inv := range pending {
		if !want[inv.ID] {
			t.Errorf("state=pending returned a dead invite %s", inv.ID)
		}
	}
	for _, dead := range []string{usedUp.ID, expired.ID, revoked.ID} {
		for _, inv := range pending {
			if inv.ID == dead {
				t.Errorf("state=pending returned %s, which no longer redeems", dead)
			}
		}
	}
}

func TestParseStateFilter(t *testing.T) {
	for _, v := range []string{"", "all", "pending"} {
		if _, ok := ParseStateFilter(v); !ok {
			t.Errorf("ParseStateFilter(%q) refused a valid value", v)
		}
	}
	for _, v := range []string{"expired", "revoked", "ALL", "0"} {
		if _, ok := ParseStateFilter(v); ok {
			t.Errorf("ParseStateFilter(%q) accepted an unknown value", v)
		}
	}
}
