package hostcfg

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

// ParseDeploymentSettings keeps the agent's pre-policy baseline separate from
// effective_settings. An invalid report is unusable as a whole; omissions of
// individual known keys remain indeterminate rather than becoming defaults.
func ParseDeploymentSettings(raw json.RawMessage) (map[string]any, error) {
	var values map[string]any
	if err := json.Unmarshal(raw, &values); err != nil || values == nil {
		return nil, errors.New("deployment_settings must be an object")
	}
	known := byKey()
	for key, value := range values {
		knob, ok := known[key]
		if !ok {
			continue // additive agent keys do not affect a known group
		}
		if value == nil {
			if !knob.Nullable {
				return nil, fmt.Errorf("%s may not be null", key)
			}
			continue
		}
		if err := validateValue(knob, value); err != nil {
			return nil, err
		}
	}
	return values, nil
}

// DeploymentSettingsForConnection never falls back to the previous socket's
// report. The caller supplies the live registry identity.
func (s *Store) DeploymentSettingsForConnection(ctx context.Context, hostID, connectionID string) (map[string]any, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT deployment_settings FROM hosts WHERE id=$1::uuid AND deployment_settings_connection=$2::uuid`, hostID, connectionID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return ParseDeploymentSettings(raw)
}

// ObserveDeploymentSettings accepts only a capacity report already bound by
// agentws to the current authenticated socket. A present malformed report
// invalidates earlier same-connection evidence; omission is handled by its
// caller and leaves this state alone.
func (s *Store) ObserveDeploymentSettings(ctx context.Context, hostID, connectionID string, raw json.RawMessage) error {
	values, parseErr := ParseDeploymentSettings(raw)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT id FROM hosts WHERE id=$1::uuid FOR UPDATE`, hostID); err != nil {
		return err
	}
	if parseErr != nil {
		_, err = tx.Exec(ctx, `UPDATE hosts SET deployment_settings=NULL,deployment_settings_connection=NULL,deployment_settings_reported_at=NULL WHERE id=$1::uuid`, hostID)
	} else {
		encoded, e := json.Marshal(values)
		if e != nil {
			return e
		}
		_, err = tx.Exec(ctx, `UPDATE hosts SET deployment_settings=$2::jsonb,deployment_settings_connection=$3::uuid,deployment_settings_reported_at=now() WHERE id=$1::uuid`, hostID, encoded, connectionID)
	}
	if err != nil {
		return err
	}
	if parseErr == nil {
		// A new current-connection baseline re-resolves every deployment-source
		// next-session group; a changed digest is a relevant condition change.
		if err := refreshDeploymentDigests(ctx, tx, hostID, connectionID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func digestJSON(value any) (string, error) {
	b, err := canonicalJSON(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func validPolicyDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, c := range value {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func sortPolicyFacts(facts []any) {
	sort.Slice(facts, func(i, j int) bool {
		a := facts[i].(map[string]any)
		b := facts[j].(map[string]any)
		if a["kind"] != b["kind"] {
			return a["kind"].(string) < b["kind"].(string)
		}
		return a["id"].(string) < b["id"].(string)
	})
}

func digestPolicyFacts(facts []any) (string, error) {
	var bytes []byte
	for _, item := range facts {
		fact, ok := item.(map[string]any)
		if !ok {
			return "", errors.New("invalid prerequisite fact")
		}
		kind, kindOK := fact["kind"].(string)
		id, idOK := fact["id"].(string)
		if !kindOK || !idOK || kind == "" || id == "" || strings.ContainsAny(kind, "\x00\n") || strings.ContainsAny(id, "\x00\n") {
			return "", errors.New("invalid prerequisite fact")
		}
		bytes = append(bytes, kind...)
		bytes = append(bytes, 0)
		bytes = append(bytes, id...)
		bytes = append(bytes, '\n')
	}
	sum := sha256.Sum256(bytes)
	return hex.EncodeToString(sum[:]), nil
}
