package hostenroll

// Requires Postgres: make test-db.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
)

const localToken = "local-enrollment-plaintext-for-tests"

func localRows(t *testing.T, ctx context.Context, s *Store) []Enrollment {
	t.Helper()
	list, err := s.List(ctx, false)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var out []Enrollment
	for _, e := range list {
		if e.Note != nil && *e.Note == LocalNote {
			out = append(out, e)
		}
	}
	return out
}

// schema.md amendment 14: inserted once, a restart neither duplicates it nor
// revives a spent one.
func TestEnsureLocalIsInsertedOnceAndNeverRevived(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	store := NewStore(pool)

	inserted, err := EnsureLocal(ctx, pool, localToken, "living-room-pc")
	if err != nil || !inserted {
		t.Fatalf("first EnsureLocal = %v, %v; want inserted", inserted, err)
	}
	inserted, err = EnsureLocal(ctx, pool, localToken, "living-room-pc")
	if err != nil || inserted {
		t.Fatalf("second EnsureLocal = %v, %v; want already present", inserted, err)
	}
	rows := localRows(t, ctx, store)
	if len(rows) != 1 {
		t.Fatalf("local rows = %d, want 1", len(rows))
	}
	e := rows[0]
	if e.MaxUses != 1 || e.UsedCount != 0 || e.ExpiresAt != nil || e.CreatedByUserID != nil ||
		e.NodeName == nil || *e.NodeName != "living-room-pc" {
		t.Fatalf("local row = %+v, want single-use, unexpiring, unminted, bound", e)
	}

	if err := Redeem(ctx, pool, localToken, "some-other-host"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("redeem by another node: %v, want ErrInvalidToken", err)
	}
	if err := Redeem(ctx, pool, localToken, "living-room-pc"); err != nil {
		t.Fatalf("redeem by the bound node: %v", err)
	}

	// The restart after the agent enrolled.
	if inserted, err := EnsureLocal(ctx, pool, localToken, "living-room-pc"); err != nil || inserted {
		t.Fatalf("EnsureLocal after use = %v, %v; want already present", inserted, err)
	}
	rows = localRows(t, ctx, store)
	if len(rows) != 1 || rows[0].UsedCount != 1 {
		t.Fatalf("after restart: %+v, want the one row with used_count 1", rows)
	}
	if err := Redeem(ctx, pool, localToken, "living-room-pc"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("second redeem: %v, want the spent token refused", err)
	}
}

// created_by is NULL, and the admin list still serves the row.
func TestTheAdminListServesTheLocalToken(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	if _, err := EnsureLocal(ctx, pool, localToken, "living-room-pc"); err != nil {
		t.Fatal(err)
	}
	do := adminClient(t, pool)
	resp := do(http.MethodGet, "/v1/admin/hosts/enrollments", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Enrollments []map[string]any `json:"enrollments"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range body.Enrollments {
		if e["note"] == LocalNote {
			found = true
			if e["created_by_user_id"] != nil || e["created_by_username"] != nil {
				t.Errorf("local token provenance = %v / %v, want null", e["created_by_user_id"], e["created_by_username"])
			}
		}
	}
	if !found {
		t.Fatalf("the local token is not in the admin list: %+v", body.Enrollments)
	}
}
