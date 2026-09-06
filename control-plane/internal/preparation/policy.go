// Package preparation owns the persisted Steam source policy. Image presence,
// discovery and user launch permissions remain separate decisions.
package preparation

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Image struct {
	ImageID     string `json:"image_id"`
	RegistryRef string `json:"registry_ref"`
	Version     string `json:"version"`
}
type Policy struct {
	Revision string  `json:"revision"`
	Enabled  bool    `json:"enabled"`
	Images   []Image `json:"images"`
}
type Policies struct {
	SteamPreparation Policy `json:"steam_preparation"`
}
type Template struct {
	RegistryRef string `json:"registry_ref"`
	Version     string `json:"version"`
}
type ImageReport struct {
	Image
	PreparationEnabled bool      `json:"preparation_enabled"`
	ConsumptionEnabled bool      `json:"consumption_enabled"`
	Reason             string    `json:"reason"`
	State              string    `json:"state"`
	Template           *Template `json:"template"`
	CloneMode          *string   `json:"clone_mode"`
	CloneReason        *string   `json:"clone_reason"`
	Detail             string    `json:"detail,omitempty"`
}
type Report struct {
	PolicyRevision string        `json:"policy_revision"`
	Images         []ImageReport `json:"images"`
}
type Reports struct {
	Steam Report `json:"steam"`
}
type Projection struct {
	Detail             string     `json:"detail"`
	Eligible           bool       `json:"eligible"`
	Supported          bool       `json:"supported"`
	DesiredEnabled     bool       `json:"desired_enabled"`
	DesiredRevision    string     `json:"desired_revision"`
	AppliedRevision    *string    `json:"applied_revision"`
	PolicyPending      bool       `json:"policy_pending"`
	PreparationEnabled *bool      `json:"preparation_enabled"`
	ConsumptionEnabled *bool      `json:"consumption_enabled"`
	State              string     `json:"state"`
	Reason             string     `json:"reason"`
	Template           *Template  `json:"template"`
	CloneMode          *string    `json:"clone_mode"`
	CloneReason        *string    `json:"clone_reason"`
	ReportedAt         *time.Time `json:"reported_at"`
}

// ConnectionContext binds reports to one authenticated websocket generation.
// The token is internal; agents cannot select it or impersonate a newer socket.
type connectionKey struct{}

func ConnectionContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, connectionKey{}, rand.Text())
}
func connectionID(ctx context.Context) string {
	id, _ := ctx.Value(connectionKey{}).(string)
	return id
}

type DB interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}
type Store struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

var officialRef = regexp.MustCompile(`^ghcr.io/accreleus/quasar-steam@sha256:[0-9a-f]{64}$`)

func ValidImage(i Image) bool {
	return i.ImageID == "steam" && officialRef.MatchString(i.RegistryRef) && len(i.Version) > 0 && len(i.Version) <= 256
}
func Current(ctx context.Context, db DB) (Policy, error) {
	p := Policy{Images: []Image{}}
	var raw []byte
	err := db.QueryRow(ctx, `SELECT steam_preparation_enabled,steam_preparation_revision::text,steam_preparation_image FROM instance_settings WHERE id=true`).Scan(&p.Enabled, &p.Revision, &raw)
	if err != nil {
		return p, err
	}
	if len(raw) > 0 && string(raw) != "null" {
		var i Image
		if err = json.Unmarshal(raw, &i); err != nil || !ValidImage(i) {
			return Policy{}, errors.New("invalid adopted Steam identity")
		}
		p.Images = append(p.Images, i)
	}
	return p, nil
}
func (s *Store) Current(ctx context.Context) (Policy, error) { return Current(ctx, s.pool) }

// Every registration starts a new policy acknowledgement epoch, including a
// reconnect of the same binary. An old capacity report cannot authorize work.
func (s *Store) Register(ctx context.Context, host string, versions map[string]int) error {
	if connectionID(ctx) == "" {
		return errors.New("missing source policy connection epoch")
	}
	var value any
	if versions["steam_preparation"] == 1 {
		value = map[string]int{"steam_preparation": 1}
	}
	_, err := s.pool.Exec(ctx, `UPDATE hosts SET source_policy_versions=$2,source_preparation=NULL,source_preparation_reported_at=NULL,source_preparation_connection_id=$3 WHERE id=$1::uuid`, host, value, connectionID(ctx))
	return err
}
func (s *Store) Snapshot(ctx context.Context, host string) (*Policies, error) {
	var supported bool
	if err := s.pool.QueryRow(ctx, `SELECT COALESCE(source_policy_versions->>'steam_preparation'='1',false) FROM hosts WHERE id=$1::uuid`, host).Scan(&supported); err != nil {
		return nil, err
	}
	if !supported {
		return nil, nil
	}
	p, err := s.Current(ctx)
	if err != nil {
		return nil, err
	}
	return &Policies{p}, nil
}
func validReport(r Report, p Policy) error {
	n, err := strconv.ParseInt(r.PolicyRevision, 10, 64)
	if err != nil || n <= 0 || strconv.FormatInt(n, 10) != r.PolicyRevision {
		return errors.New("invalid policy revision")
	}
	desired, _ := strconv.ParseInt(p.Revision, 10, 64)
	if n > desired || len(r.Images) > 1 || r.Images == nil {
		return errors.New("invalid source preparation snapshot")
	}
	states := map[string]bool{"waiting_image": true, "queued": true, "preparing": true, "ready": true, "deferred": true, "failed": true, "disabled": true}
	reasons := map[string]bool{"none": true, "source_disabled": true, "host_warmup_disabled": true, "host_templates_disabled": true, "host_permissions_disabled": true, "host_setting_invalid": true, "image_not_ready": true, "host_busy": true, "storage_unavailable": true, "stale_policy": true, "preparation_failed": true}
	for _, i := range r.Images {
		if len(p.Images) != 1 || i.Image != p.Images[0] {
			return errors.New("unadopted Steam preparation identity")
		}
		if !states[i.State] || !reasons[i.Reason] || len(i.Detail) > 1024 || (i.CloneReason != nil && len(*i.CloneReason) > 1024) {
			return errors.New("invalid preparation status")
		}
		if i.CloneMode != nil && *i.CloneMode != "reflink" && *i.CloneMode != "copy" {
			return errors.New("invalid clone mode")
		}
		if i.State == "ready" && i.Template == nil {
			return errors.New("ready preparation requires a matching published template")
		}
		if i.Template != nil && (i.Template.RegistryRef != i.RegistryRef || i.Template.Version != i.Version) {
			return errors.New("template does not match adopted identity")
		}
		if r.PolicyRevision == p.Revision && !p.Enabled && (i.PreparationEnabled || i.ConsumptionEnabled) {
			return errors.New("disabled source reported enabled")
		}
	}
	return nil
}
func (s *Store) Report(ctx context.Context, host string, reports *Reports) error {
	_, err := s.AcceptReport(ctx, host, reports)
	return err
}

// AcceptReport returns whether admission-relevant state changed. Phase chatter
// must not pull a dispatcher-owned deferred run ahead of its retry time.
func (s *Store) AcceptReport(ctx context.Context, host string, reports *Reports) (bool, error) {
	if reports == nil {
		return false, nil
	}
	if connectionID(ctx) == "" {
		return false, errors.New("missing source policy connection epoch")
	}
	// Lock desired policy while validating and storing the acknowledgement. A
	// concurrent disable/adoption cannot turn a stale snapshot into a current one.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT id FROM instance_settings WHERE id=true FOR SHARE`); err != nil {
		return false, err
	}
	p, err := Current(ctx, tx)
	if err != nil {
		return false, err
	}
	if err = validReport(reports.Steam, p); err != nil {
		return false, err
	}
	var previousRaw []byte
	if err = tx.QueryRow(ctx, `SELECT source_preparation FROM hosts WHERE id=$1::uuid AND source_preparation_connection_id=$2 FOR UPDATE`, host, connectionID(ctx)).Scan(&previousRaw); err != nil {
		return false, err
	}
	var previous *Reports
	if len(previousRaw) > 0 {
		var value Reports
		if json.Unmarshal(previousRaw, &value) == nil {
			previous = &value
		}
	}
	reconcile := needsReconciliation(previous, reports)
	raw, err := json.Marshal(reports)
	if err != nil {
		return false, err
	}
	tag, err := tx.Exec(ctx, `UPDATE hosts SET source_preparation=$2,source_preparation_reported_at=now()
 WHERE id=$1::uuid AND source_policy_versions->>'steam_preparation'='1' AND source_preparation_connection_id=$4
 AND (source_preparation IS NULL OR (source_preparation->'steam'->>'policy_revision')::bigint <= $3::bigint)`, host, raw, reports.Steam.PolicyRevision, connectionID(ctx))
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, errors.New("stale or unsupported source preparation connection report")
	}
	if err = tx.Commit(ctx); err != nil {
		return false, err
	}
	return reconcile, nil
}

// Params is the single admission check used by manual/event enqueue and again
// by the authenticated agent pull endpoint immediately before dispatch.
func Params(ctx context.Context, db DB, host string) (map[string]any, error) {
	p, err := Current(ctx, db)
	if err != nil {
		return nil, err
	}
	if !p.Enabled || len(p.Images) != 1 {
		return nil, errors.New("Steam preparation is disabled or no eligible Steam image is adopted")
	}
	i := p.Images[0]
	var ready bool
	err = db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM hosts h JOIN host_images hi ON hi.host_id=h.id
 WHERE h.id=$1::uuid AND h.status='online' AND h.agent_disconnected_at IS NULL
 AND h.source_policy_versions->>'steam_preparation'='1'
 AND h.source_preparation->'steam'->>'policy_revision'=$2
 AND hi.image_id=$3 AND hi.version=$4 AND hi.state='ready'
 AND EXISTS (SELECT 1 FROM jsonb_array_elements(h.source_preparation->'steam'->'images') r
 WHERE r->>'image_id'=$3 AND r->>'registry_ref'=$5 AND r->>'version'=$4 AND r->>'preparation_enabled'='true'))`, host, p.Revision, i.ImageID, i.Version, i.RegistryRef).Scan(&ready)
	if err != nil {
		return nil, err
	}
	if !ready {
		return nil, errors.New("Steam preparation awaits a ready image and current host policy acknowledgement")
	}
	return map[string]any{"image_id": i.ImageID, "registry_ref": i.RegistryRef, "version": i.Version, "policy_revision": p.Revision}, nil
}
func (s *Store) Params(ctx context.Context, host string) (any, error) {
	return Params(ctx, s.pool, host)
}
func (s *Store) AllowJob(ctx context.Context, host string, raw json.RawMessage) error {
	desired, err := Params(ctx, s.pool, host)
	if err != nil {
		return err
	}
	var actual map[string]any
	if err = json.Unmarshal(raw, &actual); err != nil {
		return err
	}
	for k, v := range desired {
		if actual[k] != v {
			return fmt.Errorf("stale Steam preparation job: %s", k)
		}
	}
	return nil
}
func Project(p Policy, imageID string, versions []byte, raw []byte, at *time.Time, online bool) Projection {
	out := Projection{DesiredEnabled: p.Enabled, DesiredRevision: p.Revision, State: "unknown", Reason: "agent_upgrade_required", ReportedAt: at}
	var v map[string]int
	_ = json.Unmarshal(versions, &v)
	out.Supported = v["steam_preparation"] == 1
	out.Eligible = len(p.Images) == 1 && p.Images[0].ImageID == imageID
	if !out.Eligible {
		f := false
		out.PreparationEnabled = &f
		out.ConsumptionEnabled = &f
		out.State = "unsupported"
		out.Reason = "unsupported_image"
		return out
	}
	if !out.Supported {
		return out
	}
	out.PolicyPending = true
	out.State = "pending_policy"
	out.Reason = "policy_pending"
	var reports Reports
	if json.Unmarshal(raw, &reports) == nil && reports.Steam.PolicyRevision != "" {
		r := reports.Steam
		out.AppliedRevision = &r.PolicyRevision
		out.PolicyPending = r.PolicyRevision != p.Revision || !online
		for _, i := range r.Images {
			if i.Image != p.Images[0] {
				continue
			}
			out.PreparationEnabled = &i.PreparationEnabled
			out.ConsumptionEnabled = &i.ConsumptionEnabled
			out.Detail = i.Detail
			out.Template = i.Template
			out.CloneMode = i.CloneMode
			out.CloneReason = i.CloneReason
			if !out.PolicyPending {
				out.State = i.State
				out.Reason = i.Reason
			}
		}
	}
	if !online {
		out.Reason = "host_offline"
	}
	return out
}

// CancelStalePending prevents queued jobs surviving a disable/adoption change,
// including jobs belonging to legacy agents that never poll this CP again.
func (s *Store) CancelStalePending(ctx context.Context, host string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `UPDATE job_runs r SET state='skipped',finished_at=now(),error='Steam source policy changed',summary='{"reason":"source_policy_gate"}'::jsonb
 WHERE r.job_id='template.warmup' AND r.state='pending' AND ($1='' OR r.host_id::text=$1) AND NOT EXISTS (
 SELECT 1 FROM instance_settings s JOIN hosts h ON h.id=r.host_id
 WHERE s.id=true AND s.steam_preparation_enabled AND s.steam_preparation_image IS NOT NULL
 AND r.params->>'policy_revision'=s.steam_preparation_revision::text
 AND r.params->>'registry_ref'=s.steam_preparation_image->>'registry_ref'
 AND r.params->>'version'=s.steam_preparation_image->>'version'
 AND r.params->>'image_id'=s.steam_preparation_image->>'image_id'
 AND h.source_policy_versions->>'steam_preparation'='1') RETURNING COALESCE(r.host_id::text,'')`, host)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	hosts := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		if id != "" {
			hosts = append(hosts, id)
		}
	}
	return hosts, rows.Err()
}

// A queued websocket send is not an acknowledgement. Retry an unacknowledged
// snapshot so transient read/store/send failures heal without another restart.
func (s *Store) Acknowledged(ctx context.Context, host, revision string) bool {
	var yes bool
	err := s.pool.QueryRow(ctx, `SELECT COALESCE(source_preparation->'steam'->>'policy_revision'=$2,false) FROM hosts WHERE id=$1::uuid`, host, revision).Scan(&yes)
	return err == nil && yes
}

// Effective booleans are observations, so omission/null must never masquerade
// as an explicit disabled acknowledgement from a capable agent.
func (r *ImageReport) UnmarshalJSON(raw []byte) error {
	type plain ImageReport
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	for _, key := range []string{"preparation_enabled", "consumption_enabled"} {
		value, ok := fields[key]
		if !ok || string(value) == "null" {
			return fmt.Errorf("%s must be an explicit boolean", key)
		}
	}
	var value plain
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	*r = ImageReport(value)
	return nil
}

func needsReconciliation(previous, next *Reports) bool {
	if previous == nil || previous.Steam.PolicyRevision != next.Steam.PolicyRevision {
		return true
	}
	if len(previous.Steam.Images) != len(next.Steam.Images) {
		return true
	}
	if len(next.Steam.Images) == 0 {
		return false
	}
	old, current := previous.Steam.Images[0], next.Steam.Images[0]
	if old.Image != current.Image {
		return true
	}
	if !current.PreparationEnabled {
		return false
	}
	if !old.PreparationEnabled || (!old.ConsumptionEnabled && current.ConsumptionEnabled) {
		return true
	}
	if old.Template != nil && current.Template == nil {
		return true
	}
	if old.Reason == "storage_unavailable" && current.Reason != "storage_unavailable" {
		return true
	}
	return old.State == "waiting_image" && current.State != "waiting_image"
}

// ReconcileClosedJob changes generations only. A terminal or exhausted job at
// the SAME policy/identity must retain the framework's normal retry semantics.
func (s *Store) ReconcileClosedJob(ctx context.Context, host string, params json.RawMessage) (bool, error) {
	p, err := s.Current(ctx)
	if err != nil {
		return false, err
	}
	if !p.Enabled || len(p.Images) != 1 {
		return false, nil
	}
	var old map[string]any
	if err = json.Unmarshal(params, &old); err != nil {
		return false, err
	}
	image := p.Images[0]
	if old["policy_revision"] == p.Revision && old["image_id"] == image.ImageID && old["registry_ref"] == image.RegistryRef && old["version"] == image.Version {
		return false, nil
	}
	// A deferred report may just have materialized a retry with stale params.
	// Close it before the new-generation event can coalesce onto that row.
	if _, err = s.CancelStalePending(ctx, host); err != nil {
		return false, err
	}
	return true, nil
}
