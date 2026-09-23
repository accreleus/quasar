package session

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// resolvedDispatchHome reads the same effective parent and preset policy used
// by GetLaunchApp. A preset can enable managed home or override the schema
// default target, so reading only apps.home_container_path would bind a
// different mount from the one actually dispatched.
func resolvedDispatchHome(ctx context.Context, tx pgx.Tx, appID string) (string, bool, string, error) {
	var canonical, appPath string
	var appManaged bool
	var presetManaged *bool
	var presetPath *string
	err := tx.QueryRow(ctx, `SELECT root.id::text,root.managed_home,root.home_container_path,
		rp.managed_home,rp.home_container_path
		FROM apps a JOIN apps root ON root.id=COALESCE(a.parent_app_id,a.id)
		LEFT JOIN runtime_presets rp ON rp.id=root.runtime_preset_id
		WHERE a.id=$1::uuid`, appID).
		Scan(&canonical, &appManaged, &appPath, &presetManaged, &presetPath)
	if err != nil {
		return "", false, "", err
	}
	if presetManaged == nil {
		return canonical, appManaged, appPath, nil
	}
	managed, target := mergeManagedHome(appManaged, appPath, runtimePreset{
		ManagedHome: *presetManaged, HomeContainerPath: deref(presetPath),
	})
	return canonical, managed, target, nil
}

// managedHomeDigest canonicalizes the one mount entry the control plane
// injects. Fixed lexicographic keys, string-only values and HTML escaping off
// match RFC 8785; U+2028/U+2029 remain literal as in JSON.stringify.
func managedHomeDigest(provider, ref, target string) (string, string, error) {
	if !utf8.ValidString(provider) || !utf8.ValidString(ref) || !utf8.ValidString(target) {
		return "", "", errors.New("invalid managed home mount text")
	}
	target = path.Clean(target)
	entry := struct {
		Mode     string `json:"mode"`
		Provider string `json:"provider"`
		Ref      string `json:"ref"`
		Target   string `json:"target"`
	}{Mode: "rw", Provider: provider, Ref: ref, Target: target}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(entry); err != nil {
		return "", "", err
	}
	canonical := bytes.TrimSuffix(buf.Bytes(), []byte{'\n'})
	canonical = bytes.ReplaceAll(canonical, []byte(`\u2028`), []byte("\u2028"))
	canonical = bytes.ReplaceAll(canonical, []byte(`\u2029`), []byte("\u2029"))
	sum := sha256.Sum256(canonical)
	return fmt.Sprintf("%x", sum), fmt.Sprintf("%s:%s:rw", ref, target), nil
}

func finalMount(spec []byte) (string, error) {
	var v struct {
		Mounts []json.RawMessage `json:"mounts"`
	}
	if err := json.Unmarshal(spec, &v); err != nil {
		return "", fmt.Errorf("decode dispatch mounts: %w", err)
	}
	if len(v.Mounts) == 0 {
		return "", errors.New("dispatch has no managed home mount")
	}
	var mount string
	if err := json.Unmarshal(v.Mounts[len(v.Mounts)-1], &mount); err != nil {
		return "", errors.New("final dispatch mount is not a string")
	}
	return mount, nil
}

// BindManagedHomeDispatch persists the exact home identity before assignment
// leaves the control plane. A retry may bind only the same immutable payload.
func (s *Store) BindManagedHomeDispatch(ctx context.Context, sessionID string, spec []byte) error {
	_, err := s.BindManagedHomeDispatchWithHold(ctx, sessionID, spec, false)
	return err
}

// BindManagedHomeDispatchWithHold binds the mount and, for a cleanup-capable
// command epoch, durably holds the original home before any socket handoff.
// The returned token is internal command correlation, never a wire field.
func (s *Store) BindManagedHomeDispatchWithHold(ctx context.Context, sessionID string, spec []byte, capable bool) (*HomeHoldDecision, error) {
	return s.bindManagedHomeDispatchWithHold(ctx, sessionID, spec, capable, nil)
}

// homeDispatchExpectation is captured before mount resolution. A later app or
// preset edit may not turn an already resolved managed mount into an unbound
// assignment.
type homeDispatchExpectation struct {
	canonical string
	managed   bool
	target    string
}

func expectedHomeDispatch(app LaunchApp) homeDispatchExpectation {
	return homeDispatchExpectation{canonical: homeAppID(app), managed: app.ManagedHome, target: app.HomeContainerPath}
}

func (s *Store) bindManagedHomeDispatchWithHold(ctx context.Context, sessionID string, spec []byte, capable bool, expected *homeDispatchExpectation) (*HomeHoldDecision, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin home dispatch binding: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var userID, appID string
	var hostID *string
	var state State
	var oldHome *string
	var oldDigest *string
	err = tx.QueryRow(ctx, `SELECT user_id::text,app_id::text,host_id::text,state,
		managed_home_id::text,managed_home_mount_sha256 FROM sessions WHERE id=$1::uuid FOR UPDATE`, sessionID).
		Scan(&userID, &appID, &hostID, &state, &oldHome, &oldDigest)
	if err != nil {
		return nil, fmt.Errorf("lock session for home binding: %w", err)
	}
	canonical, managed, target, err := resolvedDispatchHome(ctx, tx, appID)
	if err != nil {
		return nil, fmt.Errorf("resolve dispatch home app: %w", err)
	}
	if expected != nil && (canonical != expected.canonical || managed != expected.managed || target != expected.target) {
		return nil, errors.New("managed home policy changed before dispatch")
	}
	if !managed {
		return nil, nil
	}
	if state != StateAssigned || hostID == nil {
		return nil, errors.New("session is not assigned to a host")
	}
	var claimHost *string
	var claimState string
	var pendingSession, pendingToken *string
	err = tx.QueryRow(ctx, `SELECT host_id::text,state,pending_home_session_id::text,pending_home_token::text
		FROM managed_home_claims
		WHERE user_id=$1::uuid AND canonical_app_id=$2::uuid FOR UPDATE`, userID, canonical).
		Scan(&claimHost, &claimState, &pendingSession, &pendingToken)
	if err != nil {
		return nil, fmt.Errorf("lock dispatch home claim: %w", err)
	}
	if claimState == "conflict" || claimHost == nil || *claimHost != *hostID ||
		(pendingSession != nil && *pendingSession != sessionID) {
		return nil, ErrHomeConflict
	}
	var homeID, provider, ref string
	err = tx.QueryRow(ctx, `SELECT id::text,provider,ref FROM user_homes
		WHERE user_id=$1::uuid AND app_id=$2::uuid AND host_id=$3::uuid AND gc_after IS NULL
		FOR SHARE`, userID, canonical, *hostID).Scan(&homeID, &provider, &ref)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrHomeConflict
	}
	if err != nil {
		return nil, fmt.Errorf("lock dispatch home row: %w", err)
	}
	digest, expectedMount, err := managedHomeDigest(provider, ref, target)
	if err != nil {
		return nil, err
	}
	injectedMount, err := finalMount(spec)
	if err != nil {
		return nil, err
	}
	if injectedMount != expectedMount {
		return nil, errors.New("resolved managed home mount changed before dispatch")
	}
	if oldHome != nil || oldDigest != nil {
		if oldHome == nil || oldDigest == nil || *oldHome != homeID || *oldDigest != digest {
			return nil, errors.New("managed home dispatch binding changed")
		}
	} else {
		_, err = tx.Exec(ctx, `UPDATE sessions SET managed_home_id=$2::uuid,
			managed_home_mount_sha256=$3 WHERE id=$1::uuid`, sessionID, homeID, digest)
		if err != nil {
			return nil, fmt.Errorf("persist home dispatch binding: %w", err)
		}
	}
	decision, err := setHomeDispatchHold(ctx, tx, userID, canonical, sessionID, capable, pendingToken)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return decision, nil
}

// materializeRunningHome is called only on the first accepted agent running
// transition, under that session row's lock. It never invents a claim or a
// physical absence fact; any mismatched evidence leaves the claim reserved.
func materializeRunningHome(ctx context.Context, tx pgx.Tx, userID, appID, hostID string, boundHome, boundDigest *string) error {
	if boundHome == nil || boundDigest == nil {
		return nil
	}
	canonical, managed, target, err := resolvedDispatchHome(ctx, tx, appID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("resolve running home app: %w", err)
	}
	if !managed {
		return nil
	}
	var claimHost *string
	var claimState string
	err = tx.QueryRow(ctx, `SELECT host_id::text,state FROM managed_home_claims
		WHERE user_id=$1::uuid AND canonical_app_id=$2::uuid FOR UPDATE`, userID, canonical).
		Scan(&claimHost, &claimState)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("lock running home claim: %w", err)
	}
	if claimState != "reserved" || claimHost == nil || *claimHost != hostID {
		return nil
	}
	var homeUser, homeApp, homeHost *string
	var provider, ref string
	err = tx.QueryRow(ctx, `SELECT user_id::text,app_id::text,host_id::text,provider,ref
		FROM user_homes WHERE id=$1::uuid AND gc_after IS NULL FOR SHARE`, *boundHome).
		Scan(&homeUser, &homeApp, &homeHost, &provider, &ref)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("lock running home row: %w", err)
	}
	if homeUser == nil || *homeUser != userID || homeApp == nil || *homeApp != canonical ||
		homeHost == nil || *homeHost != hostID {
		return nil
	}
	digest, _, err := managedHomeDigest(provider, ref, target)
	if err != nil {
		return err
	}
	if digest != *boundDigest {
		return nil
	}
	_, err = tx.Exec(ctx, `UPDATE managed_home_claims SET state='materialized',materialized_at=now()
		WHERE user_id=$1::uuid AND canonical_app_id=$2::uuid AND host_id=$3::uuid AND state='reserved'`,
		userID, canonical, hostID)
	if err != nil {
		return fmt.Errorf("materialize running home claim: %w", err)
	}
	return nil
}
