// Package admission owns named, durable host scheduling restrictions. Its
// mutations serialize with session reservation on the host row.
package admission

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Kind string

const (
	Manual    Kind = "manual"
	Platform  Kind = "platform"
	IdleApply Kind = "idle_apply"
	Recovery  Kind = "recovery"
	Legacy    Kind = "legacy"
)

// These two owners have fixed IDs; run and attempt owners use their durable ID.
var (
	ManualOwner     = Owner{Kind: Manual, ID: "00000000-0000-0000-0000-000000000000"}
	LegacyOwner     = Owner{Kind: Legacy, ID: "00000000-0000-0000-0000-000000000001"}
	ErrHostNotFound = errors.New("host not found")
	ErrHostOffline  = errors.New("host offline")
)

type Owner struct {
	Kind Kind
	ID   string
}

type Restriction struct {
	OwnerKind Kind      `json:"owner_kind"`
	Reason    string    `json:"reason"`
	CreatedAt time.Time `json:"created_at"`
}

type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Acquire is idempotent for one owner. It keeps an offline host offline, but
// any connected/online host projects draining while at least one owner holds.
func (s *Store) Acquire(ctx context.Context, hostID string, owner Owner, reason string) (string, error) {
	return s.acquire(ctx, hostID, owner, reason, false)
}

// AcquireOnline is the operator drain: its offline refusal is checked under
// the host lock, so a concurrent disconnect cannot turn a 409 into a 200.
func (s *Store) AcquireOnline(ctx context.Context, hostID string, owner Owner, reason string) (string, error) {
	return s.acquire(ctx, hostID, owner, reason, true)
}

func (s *Store) acquire(ctx context.Context, hostID string, owner Owner, reason string, onlineOnly bool) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM hosts WHERE id=$1::uuid FOR UPDATE`, hostID).Scan(&status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrHostNotFound
		}
		return "", fmt.Errorf("lock host: %w", err)
	}
	if onlineOnly && status == "offline" {
		return "", ErrHostOffline
	}
	if _, err := tx.Exec(ctx, `INSERT INTO host_admission_restrictions (host_id,owner_kind,owner_id,reason)
		VALUES ($1::uuid,$2,$3::uuid,$4) ON CONFLICT DO NOTHING`, hostID, owner.Kind, owner.ID, reason); err != nil {
		return "", fmt.Errorf("acquire restriction: %w", err)
	}
	if status == "online" {
		if _, err := tx.Exec(ctx, `UPDATE hosts SET status='draining' WHERE id=$1::uuid`, hostID); err != nil {
			return "", err
		}
		status = "draining"
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return status, nil
}

// Release deletes only the named owner's row. connected is the live websocket
// observation at call time; a missing agent projects offline when the final
// restriction leaves, never a schedulable online host.
func (s *Store) Release(ctx context.Context, hostID string, owner Owner, connected bool) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM hosts WHERE id=$1::uuid FOR UPDATE`, hostID).Scan(&status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrHostNotFound
		}
		return "", fmt.Errorf("lock host: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM host_admission_restrictions
		WHERE host_id=$1::uuid AND owner_kind=$2 AND owner_id=$3::uuid`, hostID, owner.Kind, owner.ID); err != nil {
		return "", err
	}
	var held bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM host_admission_restrictions WHERE host_id=$1::uuid)`, hostID).Scan(&held); err != nil {
		return "", err
	}
	if held {
		if status == "online" {
			if _, err := tx.Exec(ctx, `UPDATE hosts SET status='draining' WHERE id=$1::uuid`, hostID); err != nil {
				return "", err
			}
			status = "draining"
		}
	} else if status == "draining" {
		next := "offline"
		if connected {
			next = "online"
		}
		if _, err := tx.Exec(ctx, `UPDATE hosts SET status=$2 WHERE id=$1::uuid`, hostID, next); err != nil {
			return "", err
		}
		status = next
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return status, nil
}

// ReleaseManual clears the two fixed operator/legacy owners in one host-locked
// transaction. It never touches a platform, idle-apply or recovery owner.
func (s *Store) ReleaseManual(ctx context.Context, hostID string, connected bool) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM hosts WHERE id=$1::uuid FOR UPDATE`, hostID).Scan(&status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrHostNotFound
		}
		return "", err
	}
	if status == "offline" {
		return "", ErrHostOffline
	}
	if _, err := tx.Exec(ctx, `DELETE FROM host_admission_restrictions WHERE host_id=$1::uuid
		AND ((owner_kind='manual' AND owner_id='00000000-0000-0000-0000-000000000000'::uuid)
		  OR (owner_kind='legacy' AND owner_id='00000000-0000-0000-0000-000000000001'::uuid))`, hostID); err != nil {
		return "", err
	}
	var held bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM host_admission_restrictions WHERE host_id=$1::uuid)`, hostID).Scan(&held); err != nil {
		return "", err
	}
	if held {
		if status == "online" {
			status = "draining"
		}
	} else if status == "draining" {
		status = "offline"
		if connected {
			status = "online"
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE hosts SET status=$2 WHERE id=$1::uuid`, hostID, status); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return status, nil
}

func (s *Store) List(ctx context.Context, hostID string) ([]Restriction, error) {
	rows, err := s.pool.Query(ctx, `SELECT owner_kind,reason,created_at FROM host_admission_restrictions
		WHERE host_id=$1::uuid ORDER BY created_at,owner_kind,owner_id`, hostID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Restriction, 0)
	for rows.Next() {
		var r Restriction
		if err := rows.Scan(&r.OwnerKind, &r.Reason, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
