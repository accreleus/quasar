package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

type Item struct {
	ID          int64           `json:"id"`
	ActorUserID *string         `json:"actor_user_id"`
	Action      string          `json:"action"`
	TargetType  string          `json:"target_type"`
	TargetID    *string         `json:"target_id"`
	Details     json.RawMessage `json:"details"`
	CreatedAt   time.Time       `json:"created_at"`
	// ActorUsername is joined at read time, never stored: this table is
	// append-only, so a rename must show the current name. Nil when the actor row
	// is gone (no FK, deliberately) or the event had no actor.
	ActorUsername *string `json:"actor_username"`
	// Severity is derived from Action (severity.go), never stored.
	Severity string `json:"severity"`
}

// Recorder is the write seam handlers take. Named form of the anonymous
// interface the older handlers declare inline; both are *Store.
type Recorder interface {
	Record(ctx context.Context, actorUserID, action, targetType, targetID string, details map[string]any) error
}

// TryRecord records one row through rec and SWALLOWS a failure, logging it: an
// audit write must never fail the operation it describes. A nil rec is a no-op,
// which is what makes the auditor optional at construction. Named for that
// swallow — a caller that needs the error calls Record directly.
func TryRecord(ctx context.Context, rec Recorder, actor, action, targetType, targetID string, details map[string]any) {
	if rec == nil {
		return
	}
	if err := rec.Record(ctx, actor, action, targetType, targetID, details); err != nil {
		slog.Warn("record admin activity failed", "action", action, "err", err)
	}
}

// ListFilter narrows GET /v1/admin/activity. Every field zero is the
// unfiltered feed. semantics: control-api.md §UI v3 console
type ListFilter struct {
	Action      string     // prefix match
	ActorUserID string     // exact
	TargetType  string     // exact
	Since       *time.Time // created_at >=
	Q           string     // case-insensitive substring over action/target_id/actor username
}

// escapeLike neutralizes LIKE wildcards in operator input, so a literal `_`
// matches a literal `_`. Paired with ESCAPE '\' in every predicate below.
func escapeLike(v string) string {
	r := strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`)
	return r.Replace(v)
}

// nilIfEmpty maps "" to a NULL parameter, which is how each predicate below
// turns itself off.
func nilIfEmpty(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

// maxDetailBytes mirrors migration 0028's
// CHECK (octet_length(details::text) <= 4096).
const maxDetailBytes = 4096

// renderedSize conservatively estimates what Postgres will measure. The CHECK
// counts `details::text` — jsonb's OWN rendering, not the bytes we send — and
// that rendering inserts a space after every `:` and every `,`, so a payload
// that fits here can still violate the constraint and turn an audit write into
// a 500 on the operation it was describing. Counting those two characters over
// the whole marshalled blob OVERCOUNTS (it also counts ones inside strings),
// which is the direction to be wrong in: we reject a little early rather than
// let the database reject at insert time.
func renderedSize(marshalled []byte) int {
	return len(marshalled) +
		bytes.Count(marshalled, []byte{':'}) +
		bytes.Count(marshalled, []byte{','})
}

// Record appends one bounded, non-secret administrative action. Callers pass
// allowlisted details, never request bodies or credentials.
func (s *Store) Record(ctx context.Context, actorUserID, action, targetType, targetID string, details map[string]any) error {
	if s == nil || s.pool == nil {
		return nil
	}
	if details == nil {
		details = map[string]any{}
	}
	b, err := json.Marshal(details)
	if err != nil {
		return fmt.Errorf("marshal audit details: %w", err)
	}
	if n := renderedSize(b); n > maxDetailBytes {
		return fmt.Errorf("audit details render to %d bytes, over the %d-byte limit", n, maxDetailBytes)
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO admin_activity (actor_user_id, action, target_type, target_id, details)
		VALUES (NULLIF($1, '')::uuid, $2, $3, NULLIF($4, ''), $5::jsonb)
	`, actorUserID, action, targetType, targetID, b)
	if err != nil {
		return fmt.Errorf("insert audit event: %w", err)
	}
	return nil
}

func (s *Store) List(ctx context.Context, cursor int64, limit int, f ListFilter) ([]Item, *int64, error) {
	if limit < 1 || limit > 100 {
		limit = 50
	}
	var action, q *string
	if f.Action != "" {
		v := escapeLike(f.Action) + "%"
		action = &v
	}
	if f.Q != "" {
		v := "%" + escapeLike(f.Q) + "%"
		q = &v
	}
	// The username join is also what ?q= searches, so it must be a LEFT JOIN:
	// an inner one would drop every row whose actor is gone or absent.
	rows, err := s.pool.Query(ctx, `
		SELECT a.id, a.actor_user_id::text, a.action, a.target_type, a.target_id,
		       a.details, a.created_at, u.username
		FROM admin_activity a
		LEFT JOIN users u ON u.id = a.actor_user_id
		WHERE ($1::bigint = 0 OR a.id < $1)
		  AND ($3::text IS NULL OR a.action LIKE $3 ESCAPE '\')
		  AND ($4::uuid IS NULL OR a.actor_user_id = $4::uuid)
		  AND ($5::text IS NULL OR a.target_type = $5)
		  AND ($6::timestamptz IS NULL OR a.created_at >= $6)
		  AND ($7::text IS NULL OR a.action ILIKE $7 ESCAPE '\'
		                        OR a.target_id ILIKE $7 ESCAPE '\'
		                        OR u.username ILIKE $7 ESCAPE '\')
		ORDER BY a.id DESC LIMIT $2
	`, cursor, limit+1, action, nilIfEmpty(f.ActorUserID), nilIfEmpty(f.TargetType), f.Since, q)
	if err != nil {
		return nil, nil, fmt.Errorf("list audit events: %w", err)
	}
	defer rows.Close()
	items := make([]Item, 0, limit)
	for rows.Next() {
		var item Item
		if err := rows.Scan(&item.ID, &item.ActorUserID, &item.Action, &item.TargetType,
			&item.TargetID, &item.Details, &item.CreatedAt, &item.ActorUsername); err != nil {
			return nil, nil, err
		}
		item.Severity = Severity(item.Action)
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	var next *int64
	if len(items) > limit {
		n := items[limit-1].ID
		next = &n
		items = items[:limit]
	}
	return items, next, nil
}
