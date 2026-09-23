package storage

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// HomeClaim is the admin diagnosis of canonical ownership. Recorded hosts are
// bookkeeping locations, not an assertion that files exist on those hosts.
type HomeClaim struct {
	UserID                    string     `json:"user_id"`
	Username                  *string    `json:"username"`
	CanonicalAppID            string     `json:"canonical_app_id"`
	AppName                   *string    `json:"app_name"`
	HostID                    *string    `json:"host_id"`
	HostName                  *string    `json:"host_name"`
	State                     string     `json:"state"`
	ConflictReason            *string    `json:"conflict_reason"`
	MaterializedAt            *time.Time `json:"materialized_at"`
	RecordedHostIDs           []string   `json:"recorded_host_ids"`
	PendingHomeOperation      bool       `json:"pending_home_operation"`
	HomeCleanupCapability     string     `json:"home_cleanup_capability"`
	LegacyUnprotectedDispatch bool       `json:"legacy_unprotected_dispatch"`
}

type ListHomeClaimsOpts struct {
	UserID string
	AppID  string
	HostID string
	State  string
	Limit  int
	Cursor string
}

var ErrInvalidHomeClaimFilter = errors.New("invalid home claim filter")
var ErrHomeConflict = errors.New("managed home location requires repair")

func sameHomeIdentity(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// canonicalManagedHome uses the same effective parent/preset managed-home
// policy as session.GetLaunchApp. A preset can enable managed home even when
// apps.managed_home is false; storage must still lock and tombstone its claim.
func canonicalManagedHome(ctx context.Context, tx pgx.Tx, appID string) (string, bool, error) {
	var canonical string
	var managed bool
	err := tx.QueryRow(ctx, `SELECT root.id::text,
		(root.managed_home OR COALESCE(rp.managed_home,false))
		FROM apps a JOIN apps root ON root.id=COALESCE(a.parent_app_id,a.id)
		LEFT JOIN runtime_presets rp ON rp.id=root.runtime_preset_id
		WHERE a.id=$1::uuid`, appID).Scan(&canonical, &managed)
	return canonical, managed, err
}

// lockClaimBeforeHome preserves the RH05 claim → user_homes lock order for
// tombstone and GC paths. Tombstoning a pre-RH05 row creates its conservative
// claim before taking the home row lock; GC never invents one.
func lockClaimBeforeHome(ctx context.Context, tx pgx.Tx, userID, appID string, hostID *string, create bool) error {
	canonical, managed, err := canonicalManagedHome(ctx, tx, appID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("resolve home claim app: %w", err)
	}
	if create && managed {
		reason := "gc_pending"
		if hostID == nil {
			reason = "legacy_location_uncertain"
		}
		_, err = tx.Exec(ctx, `INSERT INTO managed_home_claims
			(user_id,canonical_app_id,host_id,state,conflict_reason)
			VALUES ($1::uuid,$2::uuid,$3::uuid,'conflict',$4)
			ON CONFLICT (user_id,canonical_app_id) DO NOTHING`, userID, canonical, hostID, reason)
		if err != nil {
			return fmt.Errorf("record home claim before tombstone: %w", err)
		}
	}
	var id string
	var held bool
	err = tx.QueryRow(ctx, `SELECT canonical_app_id::text,pending_home_token IS NOT NULL FROM managed_home_claims
		WHERE user_id=$1::uuid AND canonical_app_id=$2::uuid FOR UPDATE`, userID, canonical).Scan(&id, &held)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("lock home claim: %w", err)
	}
	if create && held {
		return ErrHomeInUse
	}
	return nil
}

func markTombstonedClaim(ctx context.Context, tx pgx.Tx, userID, appID string, tombstoneHost *string) error {
	canonical, managed, err := canonicalManagedHome(ctx, tx, appID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("resolve tombstoned app: %w", err)
	}
	if !managed {
		var existing bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM managed_home_claims
			WHERE user_id=$1::uuid AND canonical_app_id=$2::uuid)`, userID, canonical).Scan(&existing); err != nil {
			return fmt.Errorf("check existing tombstone claim: %w", err)
		}
		if !existing {
			return nil
		}
	}
	var count int
	var unknown bool
	err = tx.QueryRow(ctx, `
		SELECT COUNT(DISTINCT uh.host_id),COALESCE(BOOL_OR(uh.host_id IS NULL),false)
		FROM user_homes uh JOIN apps a ON a.id=uh.app_id
		WHERE uh.user_id=$1::uuid AND COALESCE(a.parent_app_id,a.id)=$2::uuid
	`, userID, canonical).Scan(&count, &unknown)
	if err != nil {
		return fmt.Errorf("read tombstoned locations: %w", err)
	}
	initialReason := "gc_pending"
	if count != 1 || unknown || tombstoneHost == nil {
		initialReason = "legacy_location_uncertain"
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO managed_home_claims (user_id,canonical_app_id,host_id,state,conflict_reason)
		VALUES ($1::uuid,$2::uuid,$3::uuid,'conflict',$4)
		ON CONFLICT (user_id,canonical_app_id) DO NOTHING
	`, userID, canonical, tombstoneHost, initialReason)
	if err != nil {
		return fmt.Errorf("record tombstoned claim: %w", err)
	}
	var owner *string
	var previous *string
	err = tx.QueryRow(ctx, `SELECT host_id::text,conflict_reason FROM managed_home_claims
		WHERE user_id=$1::uuid AND canonical_app_id=$2::uuid FOR UPDATE`, userID, canonical).Scan(&owner, &previous)
	if err != nil {
		return fmt.Errorf("lock tombstoned claim: %w", err)
	}
	reason := initialReason
	if previous != nil {
		switch *previous {
		case "legacy_location_uncertain", "location_mismatch":
			reason = *previous
		case "claim_owner_missing":
			if reason == "gc_pending" {
				reason = *previous
			}
		}
	}
	if count > 1 && reason != "legacy_location_uncertain" {
		reason = "location_mismatch"
	}
	if owner == nil && reason == "gc_pending" {
		reason = "claim_owner_missing"
	}
	if owner != nil && tombstoneHost != nil && *owner != *tombstoneHost {
		reason = "location_mismatch"
	}
	_, err = tx.Exec(ctx, `UPDATE managed_home_claims SET state='conflict',conflict_reason=$3
		WHERE user_id=$1::uuid AND canonical_app_id=$2::uuid`, userID, canonical, reason)
	if err != nil {
		return fmt.Errorf("mark home claim gc pending: %w", err)
	}
	return nil
}

type homeClaimCursor struct {
	UserID    string `json:"u"`
	AppID     string `json:"a"`
	HostID    string `json:"h"`
	State     string `json:"s"`
	AfterUser string `json:"au"`
	AfterApp  string `json:"aa"`
}

func claimUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if r != '-' {
				return false
			}
		} else if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

func claimFilterUUID(s string) (string, error) {
	if s == "" {
		return "", nil
	}
	if !claimUUID(s) {
		return "", ErrInvalidHomeClaimFilter
	}
	return strings.ToLower(s), nil
}

// ListHomeClaims uses keyset pagination so a concurrent claim insert does not
// shift existing rows between pages. The opaque cursor binds every filter.
func (m *Manager) ListHomeClaims(ctx context.Context, opts ListHomeClaimsOpts) ([]HomeClaim, string, error) {
	user, err := claimFilterUUID(opts.UserID)
	if err != nil {
		return nil, "", err
	}
	app, err := claimFilterUUID(opts.AppID)
	if err != nil {
		return nil, "", err
	}
	host, err := claimFilterUUID(opts.HostID)
	if err != nil {
		return nil, "", err
	}
	if opts.State != "" && opts.State != "reserved" && opts.State != "materialized" && opts.State != "conflict" {
		return nil, "", ErrInvalidHomeClaimFilter
	}
	if opts.Limit == 0 {
		opts.Limit = 50
	}
	if opts.Limit < 1 || opts.Limit > 100 {
		return nil, "", ErrInvalidHomeClaimFilter
	}
	filter := homeClaimCursor{UserID: user, AppID: app, HostID: host, State: opts.State}
	var afterUser, afterApp any
	if opts.Cursor != "" {
		b, err := base64.RawURLEncoding.DecodeString(opts.Cursor)
		if err != nil || len(b) > 512 {
			return nil, "", ErrInvalidHomeClaimFilter
		}
		var cur homeClaimCursor
		if json.Unmarshal(b, &cur) != nil || !claimUUID(cur.AfterUser) || !claimUUID(cur.AfterApp) ||
			cur.UserID != filter.UserID || cur.AppID != filter.AppID || cur.HostID != filter.HostID || cur.State != filter.State {
			return nil, "", ErrInvalidHomeClaimFilter
		}
		afterUser, afterApp = cur.AfterUser, cur.AfterApp
	}
	rows, err := m.pool.Query(ctx, `
		SELECT c.user_id::text, u.username, c.canonical_app_id::text, a.name,
		       c.host_id::text, h.node_name, c.state, c.conflict_reason,
	       c.materialized_at, c.pending_home_token IS NOT NULL,
	       c.legacy_unprotected_dispatch,
		       COALESCE((SELECT array_agg(DISTINCT uh.host_id::text ORDER BY uh.host_id::text)
		                 FROM user_homes uh JOIN apps ha ON ha.id=uh.app_id
		                 WHERE uh.user_id=c.user_id AND COALESCE(ha.parent_app_id,ha.id)=c.canonical_app_id
		                   AND uh.host_id IS NOT NULL), ARRAY[]::text[])
		FROM managed_home_claims c
		LEFT JOIN users u ON u.id=c.user_id
		LEFT JOIN apps a ON a.id=c.canonical_app_id
		LEFT JOIN hosts h ON h.id=c.host_id
		WHERE ($1::uuid IS NULL OR c.user_id=$1::uuid)
		  AND ($2::uuid IS NULL OR c.canonical_app_id=(SELECT COALESCE(parent_app_id,id) FROM apps WHERE id=$2::uuid))
		  AND ($3::uuid IS NULL OR c.host_id=$3::uuid)
		  AND ($4::text IS NULL OR c.state=$4::text)
		  AND ($5::uuid IS NULL OR (c.user_id,c.canonical_app_id)>($5::uuid,$6::uuid))
		ORDER BY c.user_id,c.canonical_app_id
		LIMIT $7
	`, nilIfEmpty(user), nilIfEmpty(app), nilIfEmpty(host), nilIfEmpty(opts.State), afterUser, afterApp, opts.Limit+1)
	if err != nil {
		return nil, "", fmt.Errorf("list home claims: %w", err)
	}
	defer rows.Close()
	items := make([]HomeClaim, 0, opts.Limit)
	for rows.Next() {
		var item HomeClaim
		if err := rows.Scan(&item.UserID, &item.Username, &item.CanonicalAppID, &item.AppName,
			&item.HostID, &item.HostName, &item.State, &item.ConflictReason, &item.MaterializedAt,
			&item.PendingHomeOperation, &item.LegacyUnprotectedDispatch,
			&item.RecordedHostIDs); err != nil {
			return nil, "", fmt.Errorf("scan home claim: %w", err)
		}
		item.HomeCleanupCapability = "unknown"
		if item.HostID != nil && m.homeCleanupCapability != nil {
			capability := m.homeCleanupCapability(*item.HostID)
			switch capability {
			case "supported", "unsupported":
				item.HomeCleanupCapability = capability
			}
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("read home claims: %w", err)
	}
	var next string
	if len(items) > opts.Limit {
		items = items[:opts.Limit]
		last := items[len(items)-1]
		filter.AfterUser, filter.AfterApp = last.UserID, last.CanonicalAppID
		b, _ := json.Marshal(filter)
		next = base64.RawURLEncoding.EncodeToString(b)
	}
	return items, next, nil
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
