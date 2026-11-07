package agentws

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
)

// The capacity.compat_tools grammar (agent-api.md §capacity, amendment 56); a tool outside it is
// dropped rather than failing the report.
var (
	compatNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	compatTagRe  = regexp.MustCompile(`^GE-Proton[0-9]{1,3}-[0-9]{1,4}$`)
	compatSHARe  = regexp.MustCompile(`^[0-9a-f]{128}$`)
)

type compatToolsReport struct {
	FreeBytes *int64 `json:"free_bytes"`
	Tools     []struct {
		Name   string `json:"name"`
		Tag    string `json:"tag"`
		SHA512 string `json:"sha512"`
		Bytes  int64  `json:"bytes"`
	} `json:"tools"`
}

// upsertHostCompatTools replaces the host's installed set wholesale and reports whether it
// changed. A report without the field keeps the stored set.
func (s *agentStore) upsertHostCompatTools(ctx context.Context, hostID string, raw json.RawMessage) (bool, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return false, nil
	}
	var rep compatToolsReport
	if err := json.Unmarshal(raw, &rep); err != nil {
		return false, fmt.Errorf("decode compat_tools: %w", err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	rows, err := tx.Query(ctx, `SELECT name || '/' || sha512 FROM host_compat_tools WHERE host_id = $1::uuid ORDER BY 1`, hostID)
	if err != nil {
		return false, err
	}
	var before []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			rows.Close()
			return false, err
		}
		before = append(before, k)
	}
	rows.Close()
	if _, err := tx.Exec(ctx, `DELETE FROM host_compat_tools WHERE host_id = $1::uuid`, hostID); err != nil {
		return false, err
	}
	var after []string
	for _, t := range rep.Tools {
		if !compatNameRe.MatchString(t.Name) || !compatTagRe.MatchString(t.Tag) || !compatSHARe.MatchString(t.SHA512) || t.Bytes < 0 {
			continue
		}
		if _, err := tx.Exec(ctx, `INSERT INTO host_compat_tools (host_id, name, tag, sha512, bytes)
			VALUES ($1::uuid, $2, $3, $4, $5) ON CONFLICT (host_id, name) DO NOTHING`,
			hostID, t.Name, t.Tag, t.SHA512, t.Bytes); err != nil {
			return false, err
		}
		after = append(after, t.Name+"/"+t.SHA512)
	}
	slices.Sort(after)
	if _, err := tx.Exec(ctx, `UPDATE hosts SET compat_tools_free_bytes = $2, compat_tools_reported_at = now()
		WHERE id = $1::uuid`, hostID, rep.FreeBytes); err != nil {
		return false, err
	}
	return !slices.Equal(before, slices.Compact(after)), tx.Commit(ctx)
}

// SupportsCompatTools reports compat_tools_v1 for the host's current connection; a
// disconnected host has none.
func (r *Registry) SupportsCompatTools(hostID string) bool {
	c, ok := r.get(hostID)
	return ok && c.compatToolsV1
}
