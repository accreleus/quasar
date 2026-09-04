package storage

// Agent-side backing-store reaping (#175).
//
// The control plane never touches a host filesystem or docker (invariant #1), so
// it cannot itself remove a tombstoned home's backing store (a docker named
// volume or a local directory). Instead the node-agent PULLS the list of homes
// pinned to its host that are past the 24h GC grace period, reaps the backing
// store host-side, and CONFIRMS the ids it reaped — at which point the row is
// hard-deleted here. This is a new additive HTTP surface; the agent WebSocket
// contract (protocol/agent-api.md) is byte-identical (control-api.md amendment).
//
// Auth mirrors the agentws reconnect trust model exactly: the agent presents its
// per-node node_secret as a bearer token plus its node_name; we look up the host
// row and verify the secret against node_secret_hash using the same
// hex(sha256(secret)) scheme agentStore.reconnectHost uses. The resolved host_id
// scopes every query, so an agent can only ever see / reap homes on its own host.

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// gcPendingLimit caps a single pull so a backlog drains over several passes
// rather than handing the agent an unbounded batch.
const gcPendingLimit = 100

// PendingHome is one reapable backing store, scoped to the calling agent's host.
type PendingHome struct {
	ID       string `json:"id"`
	Provider string `json:"provider"` // 'volume' | 'local'
	Ref      string `json:"ref"`      // docker volume name or absolute host path
}

// ErrAgentAuth is returned when the bearer node_secret / node_name pair does not
// match an enrolled host. Callers map it to 401.
var ErrAgentAuth = errors.New("agent authentication failed")

// AuthAgentHost resolves the host_id for an agent presenting (nodeName,
// nodeSecret). It verifies the secret against hosts.node_secret_hash with the
// same hex(sha256(secret)) scheme agentws uses for reconnect, in constant time.
// Any mismatch — unknown node, bad secret — returns ErrAgentAuth (do not leak
// which).
func (m *Manager) AuthAgentHost(ctx context.Context, nodeName, nodeSecret string) (string, error) {
	if nodeName == "" || nodeSecret == "" {
		return "", ErrAgentAuth
	}
	var hostID, storedHash string
	err := m.pool.QueryRow(ctx, `
		SELECT id::text, node_secret_hash FROM hosts WHERE node_name = $1
	`, nodeName).Scan(&hostID, &storedHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrAgentAuth
	}
	if err != nil {
		return "", fmt.Errorf("lookup host: %w", err)
	}
	h := sha256.Sum256([]byte(nodeSecret))
	if subtle.ConstantTimeCompare([]byte(hex.EncodeToString(h[:])), []byte(storedHash)) != 1 {
		return "", ErrAgentAuth
	}
	return hostID, nil
}

// gcReapable is the predicate for "this tombstone may be reaped now", shared by
// GCPending and GCConfirm so a row can never be offered and then refused.
//
// The 24h grace exists so a launch can revive a tombstoned home (EnsureHome
// clears gc_after). An ORPHANED home — user_id NULL, i.e. the owning users row
// is gone (ON DELETE SET NULL, migration 0009) — can never be revived, so the
// grace protects nothing and only costs disk: a harness minting an identity per
// login filled a host to 100% inside it (#92). Orphans are reapable at once.
const gcReapable = `gc_after IS NOT NULL
	  AND (user_id IS NULL OR gc_after + interval '24 hours' < now())`

// GCPending returns up to gcPendingLimit homes pinned to hostID that are ready
// for backing-store reaping. NULL-host rows are never returned here (no agent
// owns them — the janitor row-reaps them).
func (m *Manager) GCPending(ctx context.Context, hostID string) ([]PendingHome, error) {
	rows, err := m.pool.Query(ctx, `
		SELECT id::text, provider, ref
		FROM user_homes
		WHERE host_id = $1::uuid
		  AND `+gcReapable+`
		ORDER BY gc_after
		LIMIT $2
	`, hostID, gcPendingLimit)
	if err != nil {
		return nil, fmt.Errorf("gc pending: %w", err)
	}
	defer rows.Close()

	var out []PendingHome
	for rows.Next() {
		var p PendingHome
		if err := rows.Scan(&p.ID, &p.Provider, &p.Ref); err != nil {
			return nil, fmt.Errorf("scan pending home: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// GCConfirm hard-deletes the homes whose ids the agent reaped on hostID. The
// per-row guard (still reapable by the same gcReapable predicate GCPending
// offered, AND on this host) makes a
// confirm a no-op for any home that was revived (gc_after cleared by a launch)
// or relocated to another host between the pull and the confirm — the agent's
// reap of a now-stale backing store is harmless (idempotent at the agent), and
// the live row survives. Returns the count actually deleted.
func (m *Manager) GCConfirm(ctx context.Context, hostID string, homeIDs []string) (int, error) {
	if len(homeIDs) == 0 {
		return 0, nil
	}
	var deleted int
	for _, id := range homeIDs {
		tag, err := m.pool.Exec(ctx, `
			DELETE FROM user_homes
			WHERE id::text = $1
			  AND host_id = $2::uuid
			  AND `+gcReapable+`
		`, id, hostID)
		if err != nil {
			return deleted, fmt.Errorf("gc confirm %s: %w", id, err)
		}
		deleted += int(tag.RowsAffected())
	}
	return deleted, nil
}
