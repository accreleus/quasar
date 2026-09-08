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
// One statement, so a scheduled pass and a "Check now" racing on the same
// release cannot both win: the loser's ON CONFLICT UPDATE matches no row (the
// WHERE fails) and it returns claimed=false. It is refused for a delivered
// release and for one that has already cost maxAttempts passes.
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
		RETURNING attempts
	`, releaseID, maxAttempts).Scan(&attempts)
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
