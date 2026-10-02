package session

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
)

// canonicalHomeSeed rejects anything outside the frozen, path-free code pairs.
// Keep this independent of lifecycle decoding: bad optional evidence cannot
// suppress a legitimate agent state transition.
func canonicalHomeSeed(raw json.RawMessage) (json.RawMessage, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || len(fields) != 2 {
		return nil, false
	}
	var mode, reason string
	if err := json.Unmarshal(fields["mode"], &mode); err != nil || mode == "" {
		return nil, false
	}
	if err := json.Unmarshal(fields["reason"], &reason); err != nil || reason == "" {
		return nil, false
	}
	valid := false
	switch mode {
	case "reflink", "copy":
		valid = reason == "seeded"
	case "existing":
		valid = reason == "existing_home"
	case "cold":
		switch reason {
		case "template_unavailable", "source_disabled", "host_templates_disabled", "host_setting_invalid", "policy_unavailable", "storage_unavailable", "clone_failed", "policy_changed":
			valid = true
		}
	}
	if !valid {
		return nil, false
	}
	canonical, _ := json.Marshal(struct {
		Mode   string `json:"mode"`
		Reason string `json:"reason"`
	}{mode, reason})
	return canonical, true
}

// SetInitialHomeSeed accepts one authenticated initial-launch observation.
// The conditional update is atomic with concurrent lifecycle transitions and
// duplicate callbacks; a running session or a different host cannot write it.
func (s *Store) SetInitialHomeSeed(ctx context.Context, id, hostID string, seed json.RawMessage) error {
	if !isValidUUID(id) || !isValidUUID(hostID) {
		return nil
	}
	_, err := s.pool.Exec(ctx, `UPDATE sessions SET home_seed = $3::jsonb
		WHERE id = $1::uuid AND host_id = $2::uuid
		  AND state IN ('assigned', 'starting') AND home_seed IS NULL`, id, hostID, string(bytes.Clone(seed)))
	if err != nil {
		return fmt.Errorf("set initial home seed: %w", err)
	}
	return nil
}
