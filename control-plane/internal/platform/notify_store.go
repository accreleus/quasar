package platform

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Data access for `platform_release_notifications` (migration 0080): the record
// that makes an announced release stay announced.

// maxStoredErrorLen bounds what a receiver's failure can write into the row.
const maxStoredErrorLen = 500

// notifyClaimLease is how long a granted claim keeps the row to itself.
//
// Between ClaimNotification and RecordDelivery the row sits at
// status='failed', attempts=n — indistinguishable from "an earlier pass failed,
// retry me" — so without a lease two overlapping passes both claim and both
// POST. It MUST comfortably exceed one send's worst case: webhookHTTPAttempts
// (3) requests at webhookRequestTimeout (5 s) plus the linear backoff between
// them (2 s + 4 s) is ~21 s. Two minutes leaves that room; it is also the
// longest a genuinely-crashed pass delays the retry, which is nothing next to
// a weekly detection schedule.
const notifyClaimLease = 2 * time.Minute

// Notification is one platform_release_notifications row.
type Notification struct {
	ReleaseID     string     `json:"release_id"`
	Status        string     `json:"status"`
	Attempts      int        `json:"attempts"`
	LastAttemptAt time.Time  `json:"-"`
	StatusCode    *int       `json:"status_code"`
	Error         *string    `json:"error"`
	DeliveredAt   *time.Time `json:"-"`
}

// ClaimNotification takes the right to send this release's notification,
// returning the attempt number it just recorded.
//
// One statement holding a LEASE, which is what makes a scheduled pass and a
// "Check now" racing on the same release unable to both win. The single
// statement alone is not enough: a claim leaves the row at status='failed' for
// the whole send, which reads exactly like "an earlier pass failed, retry me",
// so an overlapping pass would claim it too and both would POST. Bumping
// last_attempt_at to now() and refusing any row touched inside
// notifyClaimLease closes that window — the loser matches no row and sends
// nothing. It is likewise refused for a delivered release and for one that has
// already cost maxAttempts passes.
//
// The lease is the guarantee this package owns, so it does not depend on
// internal/jobs single-flighting the detection job, and it still holds with a
// second control-plane replica.
func (s *Store) ClaimNotification(ctx context.Context, releaseID string, maxAttempts int) (attempts int, claimed bool, err error) {
	err = s.pool.QueryRow(ctx, `
		INSERT INTO platform_release_notifications
		    (release_id, status, attempts, last_attempt_at)
		VALUES ($1::uuid, 'failed', 1, now())
		ON CONFLICT (release_id) DO UPDATE SET
		    attempts        = platform_release_notifications.attempts + 1,
		    last_attempt_at = now()
		WHERE platform_release_notifications.status = 'failed'
		  AND platform_release_notifications.attempts < $2
		  AND platform_release_notifications.last_attempt_at < now() - make_interval(secs => $3)
		RETURNING attempts
	`, releaseID, maxAttempts, notifyClaimLease.Seconds()).Scan(&attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("claim platform release notification: %w", err)
	}
	return attempts, true, nil
}

// RecordDelivery closes out a claimed attempt. `delivered` is terminal: the row
// is never notified again. A failure leaves the row failed, which is what the
// next pass retries.
func (s *Store) RecordDelivery(ctx context.Context, releaseID string, d Delivery, now time.Time) error {
	var status string
	var deliveredAt any
	var errText any
	if d.OK {
		status, deliveredAt = NotifyDelivered, now.UTC()
	} else {
		status = NotifyFailed
		if msg := boundString(d.Error, maxStoredErrorLen); msg != "" {
			errText = msg
		}
	}
	var code any
	if d.StatusCode != nil {
		code = *d.StatusCode
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE platform_release_notifications SET
		    status           = $2,
		    delivered_at     = $3,
		    last_status_code = $4,
		    last_error       = $5
		WHERE release_id = $1::uuid
	`, releaseID, status, deliveredAt, code, errText)
	if err != nil {
		return fmt.Errorf("record platform release notification: %w", err)
	}
	return nil
}

// Notification reads one release's record.
func (s *Store) Notification(ctx context.Context, releaseID string) (Notification, bool, error) {
	var n Notification
	err := s.pool.QueryRow(ctx, `
		SELECT release_id::text, status, attempts, last_attempt_at,
		       last_status_code, last_error, delivered_at
		FROM platform_release_notifications WHERE release_id = $1::uuid
	`, releaseID).Scan(&n.ReleaseID, &n.Status, &n.Attempts, &n.LastAttemptAt,
		&n.StatusCode, &n.Error, &n.DeliveredAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Notification{}, false, nil
	}
	if err != nil {
		return Notification{}, false, fmt.Errorf("read platform release notification: %w", err)
	}
	return n, true, nil
}

// LastDelivery is the most recent attempt on the instance, for the console's
// notification card. Nil when nothing has ever been sent.
func (s *Store) LastDelivery(ctx context.Context) (*DeliveryRecord, error) {
	var d DeliveryRecord
	var attemptAt time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT n.release_id::text, r.version, n.status, n.attempts,
		       n.last_attempt_at, n.last_status_code, n.last_error
		FROM platform_release_notifications n
		JOIN platform_releases r ON r.id = n.release_id
		ORDER BY n.last_attempt_at DESC
		LIMIT 1
	`).Scan(&d.ReleaseID, &d.ReleaseVersion, &d.Status, &d.Attempts,
		&attemptAt, &d.StatusCode, &d.Error)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read last platform release notification: %w", err)
	}
	d.AttemptedAt = attemptAt.UTC().Format(time.RFC3339)
	return &d, nil
}

func boundString(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut] + "…"
}
