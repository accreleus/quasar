package platform

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The dedupe record against a real database (migration 0080). The claim's
// single-statement semantics are the whole guarantee, so they are exercised
// here rather than against a fake.

func seedNotifyRelease(t *testing.T, s *Store, version, commit string) string {
	t.Helper()
	v := version
	rel := Release{
		Channel: ChannelStable, Version: &v, SourceCommit: commit,
		BuiltAt: time.Now().UTC(), SchemaVersion: 79,
	}
	if _, err := s.UpsertRelease(context.Background(), rel); err != nil {
		t.Fatalf("seed release: %v", err)
	}
	rows, err := s.Releases(context.Background(), ChannelStable)
	if err != nil {
		t.Fatalf("read releases: %v", err)
	}
	for _, r := range rows {
		if r.SourceCommit == commit {
			return r.ID
		}
	}
	t.Fatalf("seeded release %q not found", commit)
	return ""
}

// expireClaimLease pushes a row's last_attempt_at back beyond notifyClaimLease,
// which is what a real later pass sees. Done in SQL rather than by sleeping —
// the lease is minutes long on purpose.
func expireClaimLease(t *testing.T, pool *pgxpool.Pool, releaseID string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		UPDATE platform_release_notifications
		SET last_attempt_at = now() - make_interval(secs => $2)
		WHERE release_id = $1::uuid
	`, releaseID, notifyClaimLease.Seconds()*2); err != nil {
		t.Fatalf("expire the claim lease: %v", err)
	}
}

// TestClaimIsGrantedOnceThenTerminalOnDelivery — the second pass over a
// delivered release sends nothing, which is the whole point of the table.
func TestClaimIsGrantedOnceThenTerminalOnDelivery(t *testing.T) {
	ctx := context.Background()
	pool := testDB(t)
	store := NewStore(pool)
	id := seedNotifyRelease(t, store, "0.2.4", "aaaaaaaaaaaaaaaa")

	attempts, claimed, err := store.ClaimNotification(ctx, id, MaxNotifyAttempts)
	if err != nil || !claimed || attempts != 1 {
		t.Fatalf("first claim = %d/%v/%v, want 1/true/nil", attempts, claimed, err)
	}
	code := 204
	if err := store.RecordDelivery(ctx, id, Delivery{OK: true, StatusCode: &code}, time.Now()); err != nil {
		t.Fatalf("record: %v", err)
	}

	// Past the lease, so the refusal below is about `delivered` being terminal
	// rather than about the claim still being held.
	expireClaimLease(t, pool, id)
	if _, claimed, err := store.ClaimNotification(ctx, id, MaxNotifyAttempts); err != nil || claimed {
		t.Fatalf("second claim on a delivered release = %v/%v, want false/nil", claimed, err)
	}
	rec, found, err := store.Notification(ctx, id)
	if err != nil || !found {
		t.Fatalf("read record: %v/%v", found, err)
	}
	if rec.Status != NotifyDelivered || rec.DeliveredAt == nil || rec.Attempts != 1 {
		t.Fatalf("record = %+v, want delivered with a timestamp after one attempt", rec)
	}
	if rec.Error != nil {
		t.Errorf("last_error = %v on a delivered row, want null", *rec.Error)
	}
}

// TestAFailedDeliveryIsRetriedUntilTheCap — and then left alone, so a
// permanently-broken URL costs a bounded amount of noise.
func TestAFailedDeliveryIsRetriedUntilTheCap(t *testing.T) {
	ctx := context.Background()
	pool := testDB(t)
	store := NewStore(pool)
	id := seedNotifyRelease(t, store, "0.2.5", "bbbbbbbbbbbbbbbb")

	code := 500
	for i := 1; i <= MaxNotifyAttempts; i++ {
		attempts, claimed, err := store.ClaimNotification(ctx, id, MaxNotifyAttempts)
		if err != nil || !claimed {
			t.Fatalf("claim %d = %v/%v, want granted", i, claimed, err)
		}
		if attempts != i {
			t.Fatalf("claim %d recorded attempts=%d", i, attempts)
		}
		if err := store.RecordDelivery(ctx, id,
			Delivery{StatusCode: &code, Error: "the webhook receiver answered 500"}, time.Now()); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
		// Each iteration stands in for a separate later pass.
		expireClaimLease(t, pool, id)
	}
	if _, claimed, err := store.ClaimNotification(ctx, id, MaxNotifyAttempts); err != nil || claimed {
		t.Fatalf("claim past the cap = %v/%v, want false/nil", claimed, err)
	}

	rec, _, err := store.Notification(ctx, id)
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	if rec.Status != NotifyFailed || rec.DeliveredAt != nil {
		t.Fatalf("record = %+v, want failed with no delivered_at", rec)
	}
	if rec.StatusCode == nil || *rec.StatusCode != 500 || rec.Error == nil {
		t.Errorf("record = %+v, want the receiver's status and prose recorded", rec)
	}
}

// TestAFailedReleaseCanStillBeDeliveredLater — a receiver that comes back is
// the normal case, and it must close the record out as terminal.
func TestAFailedReleaseCanStillBeDeliveredLater(t *testing.T) {
	ctx := context.Background()
	pool := testDB(t)
	store := NewStore(pool)
	id := seedNotifyRelease(t, store, "0.2.6", "cccccccccccccccc")

	if _, _, err := store.ClaimNotification(ctx, id, MaxNotifyAttempts); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := store.RecordDelivery(ctx, id, Delivery{Error: "could not reach hooks.example.com"}, time.Now()); err != nil {
		t.Fatalf("record failure: %v", err)
	}
	// "Later" is what the lease measures, so the row has to actually be older
	// than it before the next pass can have the row.
	expireClaimLease(t, pool, id)
	attempts, claimed, err := store.ClaimNotification(ctx, id, MaxNotifyAttempts)
	if err != nil || !claimed || attempts != 2 {
		t.Fatalf("retry claim = %d/%v/%v, want 2/true/nil", attempts, claimed, err)
	}
	if err := store.RecordDelivery(ctx, id, Delivery{OK: true}, time.Now()); err != nil {
		t.Fatalf("record success: %v", err)
	}
	rec, _, err := store.Notification(ctx, id)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if rec.Status != NotifyDelivered || rec.Error != nil {
		t.Fatalf("record = %+v, want delivered with the previous error cleared", rec)
	}
}

// TestASecondClaimInsideTheLeaseIsRefused — the dedupe guarantee itself. A
// send takes up to ~21 s and leaves the row at status='failed' the whole time,
// so without the lease an overlapping pass would read that as "retry me", claim
// it too, and the release would be POSTed twice.
func TestASecondClaimInsideTheLeaseIsRefused(t *testing.T) {
	ctx := context.Background()
	pool := testDB(t)
	store := NewStore(pool)
	id := seedNotifyRelease(t, store, "0.3.0", "1111111111111111")

	attempts, claimed, err := store.ClaimNotification(ctx, id, MaxNotifyAttempts)
	if err != nil || !claimed || attempts != 1 {
		t.Fatalf("first claim = %d/%v/%v, want 1/true/nil", attempts, claimed, err)
	}
	// No RecordDelivery yet: this is exactly the window the first send is in.
	if got, claimed, err := store.ClaimNotification(ctx, id, MaxNotifyAttempts); err != nil || claimed {
		t.Fatalf("a second claim inside the lease = %d/%v/%v, want 0/false/nil", got, claimed, err)
	}
	rec, found, err := store.Notification(ctx, id)
	if err != nil || !found {
		t.Fatalf("read record: %v/%v", found, err)
	}
	if rec.Attempts != 1 {
		t.Errorf("attempts = %d after a refused claim, want 1 — a refusal must not bump it", rec.Attempts)
	}

	// Once the lease has run out the row is claimable again, which is what
	// keeps a crashed pass from stranding the release.
	expireClaimLease(t, pool, id)
	if got, claimed, err := store.ClaimNotification(ctx, id, MaxNotifyAttempts); err != nil || !claimed || got != 2 {
		t.Fatalf("claim after the lease = %d/%v/%v, want 2/true/nil", got, claimed, err)
	}
}

// TestLastDeliveryReportsTheNewestAttempt — what the console's notification
// card reads.
func TestLastDeliveryReportsTheNewestAttempt(t *testing.T) {
	ctx := context.Background()
	pool := testDB(t)
	store := NewStore(pool)

	if last, err := store.LastDelivery(ctx); err != nil || last != nil {
		t.Fatalf("LastDelivery on a fresh instance = %v/%v, want nil/nil", last, err)
	}

	older := seedNotifyRelease(t, store, "0.2.7", "dddddddddddddddd")
	newer := seedNotifyRelease(t, store, "0.2.8", "eeeeeeeeeeeeeeee")
	for _, id := range []string{older, newer} {
		if _, _, err := store.ClaimNotification(ctx, id, MaxNotifyAttempts); err != nil {
			t.Fatalf("claim %s: %v", id, err)
		}
		if err := store.RecordDelivery(ctx, id, Delivery{OK: true}, time.Now()); err != nil {
			t.Fatalf("record %s: %v", id, err)
		}
	}
	// now() is per-statement, so two claims in one test can share a timestamp;
	// push the older one back to make the ordering unambiguous.
	if _, err := pool.Exec(ctx,
		`UPDATE platform_release_notifications SET last_attempt_at = now() - interval '1 hour' WHERE release_id = $1::uuid`,
		older); err != nil {
		t.Fatalf("age the older row: %v", err)
	}

	last, err := store.LastDelivery(ctx)
	if err != nil || last == nil {
		t.Fatalf("LastDelivery = %v/%v", last, err)
	}
	if last.ReleaseID != newer {
		t.Errorf("LastDelivery release = %s, want the newer attempt %s", last.ReleaseID, newer)
	}
	if last.ReleaseVersion == nil || *last.ReleaseVersion != "0.2.8" {
		t.Errorf("release_version = %v, want 0.2.8", last.ReleaseVersion)
	}
	if last.Status != NotifyDelivered || last.AttemptedAt == "" {
		t.Errorf("last delivery = %+v, want a delivered record with a timestamp", last)
	}
}

// TestDeletingAReleaseDropsItsNotificationRecord — the release cache is a
// cache; a record for a release nobody has suppresses nothing.
func TestDeletingAReleaseDropsItsNotificationRecord(t *testing.T) {
	ctx := context.Background()
	pool := testDB(t)
	store := NewStore(pool)
	id := seedNotifyRelease(t, store, "0.2.9", "ffffffffffffffff")

	if _, _, err := store.ClaimNotification(ctx, id, MaxNotifyAttempts); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM platform_releases WHERE id = $1::uuid`, id); err != nil {
		t.Fatalf("delete release: %v", err)
	}
	if _, found, err := store.Notification(ctx, id); err != nil || found {
		t.Fatalf("record after the release was deleted = %v/%v, want gone", found, err)
	}
}
