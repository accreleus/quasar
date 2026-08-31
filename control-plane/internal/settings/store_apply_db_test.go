// store_apply_db_test.go — the testing-specialist finding on Store.Apply
// (fix/s6-followups item 4). Apply's doc comment claims it "writes every
// provided field IN ONE STATEMENT INSIDE ONE TRANSACTION ... must land or not
// land as one" — the entire reason the single-UPDATE refactor exists (it fixed
// a real partial-application defect where registration_mode committed and
// allowed_origins then failed, returning 500 half-applied). That guarantee was
// previously untested: existing tests cover successful updates and
// handler-level validation that rejects BEFORE any write. Nothing exercised a
// write that reaches the database and is rejected THERE.
//
// This test submits one field the database accepts alongside one the database
// itself rejects (image_update_policy's CHECK constraint), calling Store.Apply
// directly so the handler's own ValidImageUpdatePolicy pre-check is bypassed —
// the rejection must come from Postgres, not from Go validation, or the
// transaction boundary is not actually under test.
package settings

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/auth"
)

// TestApplyRollsBackEntirelyOnDatabaseRejection is the item-4 test. It asserts
// three things: Apply surfaces the database's CHECK-constraint rejection as an
// error, and NEITHER the field that would have been valid on its own
// (registration_mode) NOR the field that was rejected (image_update_policy)
// changed — proving the whole statement rolled back as one, not that the
// bad column alone was refused mid-write.
func TestApplyRollsBackEntirelyOnDatabaseRejection(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()

	store := NewStore(pool)
	must(t, store.Seed(ctx, RegistrationClosed))

	// Apply's updated_by is cast to ::uuid and instance_settings.updated_by
	// REFERENCES users(id), so a real user row is needed — an admin is not
	// required here, this goes straight through Store.Apply, not the HTTP
	// admin gate.
	authSvc, err := auth.NewService(pool, auth.DefaultParams(), time.Hour)
	must(t, err)
	user, err := authSvc.Register(ctx, "settings-apply@t.local", "settingsapply", "password12345")
	must(t, err)

	before, err := store.Get(ctx)
	must(t, err)
	if before.RegistrationMode != RegistrationClosed {
		t.Fatalf("precondition: registration_mode = %q, want %q", before.RegistrationMode, RegistrationClosed)
	}

	// registration_mode=open is, on its own, a perfectly valid write — the point
	// is that it must NOT land just because it rode along with a field the
	// database itself refuses. "not-a-real-policy" is never checked by Go (it
	// bypasses ValidImageUpdatePolicy entirely by calling Store.Apply directly)
	// and only fails instance_settings' image_update_policy CHECK constraint at
	// the database.
	openMode := RegistrationOpen
	badPolicy := "not-a-real-policy"
	_, _, err = store.Apply(ctx, Patch{
		RegistrationMode:  &openMode,
		ImageUpdatePolicy: &badPolicy,
	}, user.ID)
	if err == nil {
		t.Fatal("Apply with a CHECK-violating image_update_policy returned no error; " +
			"the database should have rejected the write")
	}
	if !strings.Contains(err.Error(), "instance_settings") {
		t.Fatalf("err = %v, want it to name the write that failed", err)
	}

	after, err := store.Get(ctx)
	must(t, err)
	if after.RegistrationMode != before.RegistrationMode {
		t.Errorf("registration_mode changed despite the rejected write: before=%q after=%q — "+
			"the valid field landed even though the transaction should have rolled back as one",
			before.RegistrationMode, after.RegistrationMode)
	}
	if after.ImageUpdatePolicy != before.ImageUpdatePolicy {
		t.Errorf("image_update_policy changed despite being the field the database rejected: before=%q after=%q",
			before.ImageUpdatePolicy, after.ImageUpdatePolicy)
	}
}
