package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestManagedHomeDispatchBindingMaterializesOnlyFirstVerifiedRunning(t *testing.T) {
	pool := testDB(t)
	s := seed(t, pool, 2)
	appID := seedManagedApp(t, pool, `{}`)
	seedHome(t, pool, s.userID, appID, s.hostID)
	store := NewStore(pool)
	ctx := context.Background()
	p := managedLaunchParams(s, appID)
	p.PinHostID = s.hostID
	sess, err := store.ScheduleAndCreate(ctx, p)
	must(t, err)
	var homeID, ref string
	must(t, pool.QueryRow(ctx, `SELECT id::text,ref FROM user_homes WHERE user_id=$1::uuid AND app_id=$2::uuid AND host_id=$3::uuid`, s.userID, appID, s.hostID).Scan(&homeID, &ref))
	spec := []byte(`{"mounts":["` + ref + `:/home/quasar:rw"]}`)
	must(t, store.BindManagedHomeDispatch(ctx, sess.ID, spec))
	var boundID, digest string
	must(t, pool.QueryRow(ctx, `SELECT managed_home_id::text,managed_home_mount_sha256 FROM sessions WHERE id=$1::uuid`, sess.ID).Scan(&boundID, &digest))
	if boundID != homeID || len(digest) != 64 {
		t.Fatalf("binding = (%s,%s), want home ID and digest", boundID, digest)
	}

	// An unassigned agent's callback is rejected before it can become evidence.
	otherHost, _ := seedSecondHost(t, pool, 16384, 2)
	if _, err := store.TransitionFromHost(ctx, sess.ID, otherHost, StateRunning, nil, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other host running report: %v, want not found", err)
	}
	assertClaimState(t, pool, s.userID, appID, "reserved")
	_, err = store.TransitionFromHost(ctx, sess.ID, s.hostID, StateRunning, nil, nil)
	must(t, err)
	assertClaimState(t, pool, s.userID, appID, "materialized")
	// Repeated running cannot make another home row materialized.
	_, err = store.TransitionFromHost(ctx, sess.ID, s.hostID, StateRunning, nil, nil)
	must(t, err)
}

func TestManagedHomeBindingRejectsChangedPayloadAndUnboundRunning(t *testing.T) {
	pool := testDB(t)
	s := seed(t, pool, 2)
	appID := seedManagedApp(t, pool, `{}`)
	seedHome(t, pool, s.userID, appID, s.hostID)
	store := NewStore(pool)
	ctx := context.Background()
	p := managedLaunchParams(s, appID)
	p.PinHostID = s.hostID
	sess, err := store.ScheduleAndCreate(ctx, p)
	must(t, err)
	// A session can report running after a crash before durable binding; it is
	// still running, but does not prove this managed home was mounted.
	_, err = store.TransitionFromHost(ctx, sess.ID, s.hostID, StateRunning, nil, nil)
	must(t, err)
	assertClaimState(t, pool, s.userID, appID, "reserved")

	second, err := store.ScheduleAndCreate(ctx, p)
	if err == nil {
		t.Fatalf("second session unexpectedly reserved: %s", second.ID)
	}
	// Check an independently reserved session after closing the first one.
	_, err = store.Transition(ctx, sess.ID, StateStopped, nil, nil)
	must(t, err)
	second, err = store.ScheduleAndCreate(ctx, p)
	must(t, err)
	if err := store.BindManagedHomeDispatch(ctx, second.ID, []byte(`{"mounts":["other:/home/quasar:rw"]}`)); err == nil {
		t.Fatal("mismatched final injected mount bound")
	}
	var ref string
	must(t, pool.QueryRow(ctx, `SELECT ref FROM user_homes WHERE user_id=$1::uuid AND app_id=$2::uuid AND host_id=$3::uuid`, s.userID, appID, s.hostID).Scan(&ref))
	spec := []byte(`{"mounts":["` + ref + `:/home/quasar:rw"]}`)
	must(t, store.BindManagedHomeDispatch(ctx, second.ID, spec))
	must(t, store.BindManagedHomeDispatch(ctx, second.ID, spec))
	if err := store.BindManagedHomeDispatch(ctx, second.ID, []byte(`{"mounts":["other:/home/quasar:rw"]}`)); err == nil {
		t.Fatal("binding retry accepted changed mount")
	}
	// A changed live home row cannot retroactively prove the old dispatch.
	_, err = pool.Exec(ctx, `UPDATE user_homes SET ref='replacement' WHERE user_id=$1::uuid AND app_id=$2::uuid AND host_id=$3::uuid`, s.userID, appID, s.hostID)
	must(t, err)
	_, err = store.TransitionFromHost(ctx, second.ID, s.hostID, StateRunning, nil, nil)
	must(t, err)
	assertClaimState(t, pool, s.userID, appID, "reserved")
}

func TestManagedHomeDigestCanonicalObject(t *testing.T) {
	digest, mount, err := managedHomeDigest("local", "/a<\u2028", "/home/quasar")
	must(t, err)
	canonical := []byte("{\"mode\":\"rw\",\"provider\":\"local\",\"ref\":\"/a<\u2028\",\"target\":\"/home/quasar\"}")
	sum := sha256.Sum256(canonical)
	if digest != hex.EncodeToString(sum[:]) || mount != "/a<\u2028:/home/quasar:rw" {
		t.Fatalf("canonical digest/mount = %s/%q", digest, mount)
	}
}

func TestManagedHomeBindingUsesEffectivePresetTarget(t *testing.T) {
	pool := testDB(t)
	s := seed(t, pool, 2)
	appID := seedManagedApp(t, pool, `{}`)
	presetID := insertPreset(t, pool, "binding-target", "image:test", `[]`, `{}`, `[]`, true, "/alternate/home")
	ctx := context.Background()
	_, err := pool.Exec(ctx, `UPDATE apps SET runtime_preset_id=$2::uuid WHERE id=$1::uuid`, appID, presetID)
	must(t, err)
	seedHome(t, pool, s.userID, appID, s.hostID)
	store := NewStore(pool)
	p := managedLaunchParams(s, appID)
	p.PinHostID = s.hostID
	sess, err := store.ScheduleAndCreate(ctx, p)
	must(t, err)
	var ref string
	must(t, pool.QueryRow(ctx, `SELECT ref FROM user_homes WHERE user_id=$1::uuid AND app_id=$2::uuid AND host_id=$3::uuid`, s.userID, appID, s.hostID).Scan(&ref))
	must(t, store.BindManagedHomeDispatch(ctx, sess.ID, []byte(`{"mounts":["`+ref+`:/alternate/home:rw"]}`)))
	_, err = store.TransitionFromHost(ctx, sess.ID, s.hostID, StateRunning, nil, nil)
	must(t, err)
	assertClaimState(t, pool, s.userID, appID, "materialized")
}

func assertClaimState(t *testing.T, pool *pgxpool.Pool, userID, appID, want string) {
	t.Helper()
	var state string
	if err := pool.QueryRow(context.Background(), `SELECT state FROM managed_home_claims WHERE user_id=$1::uuid AND canonical_app_id=$2::uuid`, userID, appID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != want {
		t.Fatalf("claim state = %s, want %s", state, want)
	}
}
