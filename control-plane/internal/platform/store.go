package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Data access for the release surface: the detector's cache
// (`platform_releases`, migration 0074) and the identity projection of `hosts`.
// It holds no decision — PlanRelease takes these reads as input.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore constructs a Store from the shared pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

const releaseColumns = `id::text, channel, version, source_commit, built_at,
	schema_version, prerelease, notes, compare_url, manifest, discovered_at`

// Releases returns every row on one channel, unordered: the ADR 0002 ordering
// lives in PlanRelease with the rules that depend on it, not split across both.
func (s *Store) Releases(ctx context.Context, channel string) ([]Release, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+releaseColumns+` FROM platform_releases WHERE channel = $1`, channel)
	if err != nil {
		return nil, fmt.Errorf("query platform_releases: %w", err)
	}
	defer rows.Close()

	out := make([]Release, 0)
	for rows.Next() {
		var r Release
		var manifest []byte
		if err := rows.Scan(&r.ID, &r.Channel, &r.Version, &r.SourceCommit, &r.BuiltAt,
			&r.SchemaVersion, &r.Prerelease, &r.Notes, &r.CompareURL, &manifest,
			&r.DiscoveredAt); err != nil {
			return nil, fmt.Errorf("scan platform_release: %w", err)
		}
		// A SQL NULL stays nil, which is what len(Manifest) == 0 means to the plan.
		if len(manifest) > 0 {
			r.Manifest = json.RawMessage(manifest)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpsertRelease records one detected release on the idempotency key
// (channel, source_commit): re-running detection must not accumulate
// duplicates. It reports whether the row was NEW. `discovered_at` is never
// rewritten — it is when THIS instance first saw the release, not a publish time.
func (s *Store) UpsertRelease(ctx context.Context, r Release) (bool, error) {
	var inserted bool
	err := s.pool.QueryRow(ctx, `
		INSERT INTO platform_releases
		    (channel, version, source_commit, built_at, schema_version,
		     prerelease, notes, compare_url, manifest)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb)
		ON CONFLICT (channel, source_commit) DO UPDATE SET
		    version        = EXCLUDED.version,
		    built_at       = EXCLUDED.built_at,
		    schema_version = EXCLUDED.schema_version,
		    prerelease     = EXCLUDED.prerelease,
		    notes          = EXCLUDED.notes,
		    compare_url    = EXCLUDED.compare_url,
		    manifest       = EXCLUDED.manifest
		RETURNING (xmax = 0)
	`, r.Channel, r.Version, r.SourceCommit, r.BuiltAt, r.SchemaVersion,
		r.Prerelease, r.Notes, r.CompareURL, manifestArg(r.Manifest)).Scan(&inserted)
	if err != nil {
		return false, fmt.Errorf("upsert platform_release: %w", err)
	}
	return inserted, nil
}

// A nil manifest must be SQL NULL, not the four bytes "null" — that is a JSONB
// null and reads back as a manifest that exists.
func manifestArg(m json.RawMessage) any {
	if len(m) == 0 {
		return nil
	}
	return []byte(m)
}

// Hosts projects the host list's identity columns in that list's order
// (created_at DESC — SQL twin: crud.listHosts), because `installed.hosts` and
// `targets` must agree with GET /v1/hosts.
func (s *Store) Hosts(ctx context.Context) ([]HostIdentity, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, node_name, status, agent_version,
		       source_commit, built_at, install_mode, updater_present
		FROM hosts
		ORDER BY created_at DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("query hosts: %w", err)
	}
	defer rows.Close()

	out := make([]HostIdentity, 0)
	for rows.Next() {
		var h HostIdentity
		var builtAt *time.Time
		if err := rows.Scan(&h.HostID, &h.NodeName, &h.Status, &h.AgentVersion,
			&h.SourceCommit, &builtAt, &h.InstallMode, &h.UpdaterPresent); err != nil {
			return nil, fmt.Errorf("scan host identity: %w", err)
		}
		if builtAt != nil {
			s := builtAt.UTC().Format(time.RFC3339)
			h.BuiltAt = &s
		}
		h.IdentityKnown = h.Known()
		out = append(out, h)
	}
	return out, rows.Err()
}
