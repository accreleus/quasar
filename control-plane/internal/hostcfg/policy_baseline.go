package hostcfg

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
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
		candidate, err := idleCandidateForConnection(ctx, tx, hostID, connectionID)
		if err != nil {
			return err
		}
		if candidate != nil && candidate.Choice.Source == "deployment" {
			if _, err := tx.Exec(ctx, `UPDATE host_setting_groups SET desired_digest=$2,
				status=CASE WHEN status IN ('applied','failed','pending') AND desired_digest IS DISTINCT FROM $2 THEN 'pending' ELSE status END
				WHERE host_id=$1::uuid AND group_key='idle_timeout_secs'`, hostID, candidate.Digest); err != nil {
				return err
			}
		}
	}
	return tx.Commit(ctx)
}

func digestJSON(value any) (string, error) {
	b, err := json.Marshal(value)
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

type idleCandidate struct {
	Choice        PolicyChoice
	Value         any
	Digest        string
	Prerequisites []any
	PrereqDigest  string
}

// idleCandidateForConnection resolves only the idle group. A deployment
// choice requires a valid baseline from the same live connection as the offer.
func idleCandidateForConnection(ctx context.Context, tx pgx.Tx, hostID, connectionID string) (*idleCandidate, error) {
	var revision int64
	var source string
	var raw []byte
	var scope string
	err := tx.QueryRow(ctx, `SELECT g.desired_revision,g.scope,c.source,c.explicit_value FROM host_setting_groups g JOIN host_setting_choices c ON c.host_id=g.host_id AND c.key='idle_timeout_secs' WHERE g.host_id=$1::uuid AND g.group_key='idle_timeout_secs'`, hostID).Scan(&revision, &scope, &source, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if scope != "next_session" {
		return nil, nil
	}
	choice := PolicyChoice{Source: source}
	var value any
	prereqs := []any{}
	if source == "explicit" {
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, err
		}
		choice.Value = value
	} else if source == "deployment" {
		var baseline []byte
		err := tx.QueryRow(ctx, `SELECT deployment_settings FROM hosts WHERE id=$1::uuid AND deployment_settings_connection=$2::uuid`, hostID, connectionID).Scan(&baseline)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		values, err := ParseDeploymentSettings(baseline)
		if err != nil {
			return nil, nil
		}
		var ok bool
		value, ok = values["idle_timeout_secs"]
		if !ok || value == nil {
			return nil, nil
		}
		fact, err := digestJSON(map[string]any{"idle_timeout_secs": value})
		if err != nil {
			return nil, err
		}
		prereqs = append(prereqs, map[string]any{"kind": "deployment_baseline", "id": fact})
	} else {
		return nil, nil
	}
	content := map[string]any{
		"group": "idle_timeout_secs", "scope": scope,
		"revision":          strconv.FormatInt(revision, 10),
		"settings":          map[string]PolicyChoice{"idle_timeout_secs": choice},
		"resolved_settings": map[string]any{"idle_timeout_secs": value},
	}
	digest, err := digestJSON(content)
	if err != nil {
		return nil, err
	}
	prereqDigest, err := digestPolicyFacts(prereqs)
	if err != nil {
		return nil, err
	}
	return &idleCandidate{Choice: choice, Value: value, Digest: digest, Prerequisites: prereqs, PrereqDigest: prereqDigest}, nil
}
