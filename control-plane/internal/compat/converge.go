package compat

// The compat_tools.converge job's control-plane half: desired state, the first difference, the
// failed hold and per-host install state. semantics: control-api.md §Compatibility tools,
// "Install job" and "Per-host install state".

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/audit"
	"github.com/accreleus/quasar/control-plane/internal/jobs"
)

// JobID is the code-owned job; its runner is node-agent/src/compat_tools/install.rs.
const JobID = "compat_tools.converge"

const gib = int64(1) << 30

// holdTimer releases a failed hold for a transient reason.
const holdTimer = time.Hour

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type geTool struct {
	ID, Tag, AssetName, SHA512 string
	SteamName                  *string
	Size                       int64
	Enabled, NameConflict      bool
	PinConflict                bool
	UpdatedAt                  time.Time
}

func (t geTool) desired() bool { return t.Enabled && !t.NameConflict && !t.PinConflict }

type installedRow struct {
	Name, Tag, SHA512 string
	Bytes             int64
}

type hostRun struct {
	ID, State, Trigger, Action, SHA512, Reason string
	Created, Scheduled                         time.Time
	Finished                                   *time.Time
}

// open is a run the host holds or may claim now; a deferred retry waiting for its time is not.
func (r hostRun) open(now time.Time) bool {
	return r.State == "running" || (r.State == "pending" && !r.Scheduled.After(now))
}

// fleet is everything the decisions below read, loaded once.
type fleet struct {
	tools     []geTool
	installed map[string][]installedRow
	runs      map[string][]hostRun
	reported  map[string]*time.Time // hosts.compat_tools_reported_at
	reserve   int64
}

// loadFleet reads the GE catalogue and, for hostID (every host when ""), the installed rows and
// this job's runs.
func loadFleet(ctx context.Context, q querier, hostID string) (*fleet, error) {
	f := &fleet{installed: map[string][]installedRow{}, runs: map[string][]hostRun{},
		reported: map[string]*time.Time{}, reserve: 10 * gib}
	var reserveGiB int64
	switch err := q.QueryRow(ctx, `SELECT compat_tools_disk_reserve_gib FROM instance_settings WHERE id = true`).Scan(&reserveGiB); {
	case err == nil:
		f.reserve = reserveGiB * gib
	case err != pgx.ErrNoRows:
		return nil, fmt.Errorf("read disk reserve: %w", err)
	}
	rows, err := q.Query(ctx, `
		SELECT id::text, tag, asset_name, asset_sha512, steam_name, asset_size_bytes, enabled,
		       name_conflict, pin_conflict, updated_at
		FROM compat_tools WHERE family = 'ge' ORDER BY created_at DESC, tag DESC`)
	if err != nil {
		return nil, fmt.Errorf("read GE tools: %w", err)
	}
	for rows.Next() {
		var t geTool
		if err := rows.Scan(&t.ID, &t.Tag, &t.AssetName, &t.SHA512, &t.SteamName, &t.Size, &t.Enabled,
			&t.NameConflict, &t.PinConflict, &t.UpdatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		f.tools = append(f.tools, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = q.Query(ctx, `SELECT id::text, compat_tools_reported_at FROM hosts
		WHERE $1 = '' OR id = NULLIF($1, '')::uuid`, hostID)
	if err != nil {
		return nil, fmt.Errorf("read host reports: %w", err)
	}
	for rows.Next() {
		var h string
		var at *time.Time
		if err := rows.Scan(&h, &at); err != nil {
			rows.Close()
			return nil, err
		}
		f.reported[h] = at
	}
	rows.Close()
	rows, err = q.Query(ctx, `SELECT host_id::text, name, tag, sha512, bytes FROM host_compat_tools
		WHERE $1 = '' OR host_id = NULLIF($1, '')::uuid ORDER BY name`, hostID)
	if err != nil {
		return nil, fmt.Errorf("read installed tools: %w", err)
	}
	for rows.Next() {
		var h string
		var r installedRow
		if err := rows.Scan(&h, &r.Name, &r.Tag, &r.SHA512, &r.Bytes); err != nil {
			rows.Close()
			return nil, err
		}
		f.installed[h] = append(f.installed[h], r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = q.Query(ctx, `
		SELECT host_id::text, id::text, state, trigger, COALESCE(params->>'action', ''),
		       COALESCE(params->>'sha512', ''), COALESCE(summary->>'reason', ''), created_at, scheduled_for, finished_at
		FROM job_runs WHERE job_id = $1 AND ($2 = '' OR host_id = NULLIF($2, '')::uuid)
		ORDER BY created_at, id`, JobID, hostID)
	if err != nil {
		return nil, fmt.Errorf("read converge runs: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var h string
		var r hostRun
		if err := rows.Scan(&h, &r.ID, &r.State, &r.Trigger, &r.Action, &r.SHA512, &r.Reason, &r.Created, &r.Scheduled, &r.Finished); err != nil {
			return nil, err
		}
		f.runs[h] = append(f.runs[h], r)
	}
	return f, rows.Err()
}

func (f *fleet) latest(host, action, sha string) (hostRun, bool) {
	var out hostRun
	found := false
	for _, r := range f.runs[host] {
		if r.Action == action && r.SHA512 == sha {
			out, found = r, true
		}
	}
	return out, found
}

func (f *fleet) open(host, action, sha string, now time.Time) *hostRun {
	for _, r := range f.runs[host] {
		if r.open(now) && r.Action == action && r.SHA512 == sha {
			return &r
		}
	}
	return nil
}

func transient(reason string) bool { return reason == "download_failed" || reason == "io_error" }

// hold is the failed hold on (host, action, sha): the latest such run failed and nothing has
// released it. releaseAt is when the timer releases it, nil when only an admin can.
func (f *fleet) hold(host, action, sha string, tool *geTool, now time.Time) (held bool, reason string, releaseAt *time.Time) {
	last, ok := f.latest(host, action, sha)
	if !ok || last.State != "failed" || last.Finished == nil {
		return false, "", nil
	}
	if tool != nil && tool.UpdatedAt.After(*last.Finished) {
		return false, "", nil // enabled again, or a new pin
	}
	for _, r := range f.runs[host] {
		if r.Trigger == "manual" && r.Created.After(*last.Finished) {
			return false, "", nil // Run now for this host
		}
	}
	if transient(last.Reason) {
		at := last.Finished.Add(holdTimer)
		if !now.Before(at) {
			return false, "", nil
		}
		return true, last.Reason, &at
	}
	return true, last.Reason, nil
}

// installedAs is the host's row for tool t: the SHA-512 under its bound name.
func (f *fleet) installedAs(host string, t geTool) *installedRow {
	for _, r := range f.installed[host] {
		if r.SHA512 == t.SHA512 && (t.SteamName == nil || r.Name == *t.SteamName) {
			return &r
		}
	}
	return nil
}

// awaitingReport: the latest install of t succeeded after the host's last capacity report, which
// the agent re-sends after every install, so another run would only find it installed.
func (f *fleet) awaitingReport(host string, t geTool) bool {
	last, ok := f.latest(host, "install", t.SHA512)
	rep := f.reported[host]
	return ok && last.State == "succeeded" && last.Finished != nil && (rep == nil || rep.Before(*last.Finished))
}

func (f *fleet) toolBySHA(sha string) *geTool {
	for i := range f.tools {
		if f.tools[i].SHA512 == sha {
			return &f.tools[i]
		}
	}
	return nil
}

func assetKind(name string) string {
	if strings.HasSuffix(name, "-x86_64.tar.gz") {
		return "x86_64"
	}
	return "plain"
}

func (f *fleet) installParams(t geTool) map[string]any {
	return map[string]any{
		"action": "install", "tool_id": t.ID, "tag": t.Tag, "asset": assetKind(t.AssetName),
		"size_bytes": t.Size, "sha512": t.SHA512, "expected_name": t.SteamName, "reserve_bytes": f.reserve,
	}
}

// firstDifference is the one action a run for host does next, or nil. releaseAt is the earliest
// time a failed hold's timer frees a difference, for the run to wait for.
func (f *fleet) firstDifference(host string, now time.Time) (params map[string]any, releaseAt *time.Time) {
	note := func(at *time.Time) {
		if at != nil && (releaseAt == nil || at.Before(*releaseAt)) {
			releaseAt = at
		}
	}
	var deferredRemovals []map[string]any
	for _, r := range f.installed[host] {
		owner := f.toolBySHA(r.SHA512)
		if owner != nil && owner.desired() && f.installedAs(host, *owner) != nil {
			continue
		}
		held, _, at := f.hold(host, "remove", r.SHA512, owner, now)
		if held {
			note(at)
			continue
		}
		var toolID any
		if owner != nil {
			toolID = owner.ID
		}
		p := map[string]any{"action": "remove", "tool_id": toolID, "name": r.Name, "sha512": r.SHA512}
		if last, ok := f.latest(host, "remove", r.SHA512); ok && last.State == "deferred" && last.Reason == "in_use" {
			deferredRemovals = append(deferredRemovals, p)
			continue
		}
		return p, nil
	}
	for _, t := range f.tools {
		if !t.desired() || f.installedAs(host, t) != nil || f.awaitingReport(host, t) {
			continue
		}
		if held, _, at := f.hold(host, "install", t.SHA512, &t, now); held {
			note(at)
			continue
		}
		return f.installParams(t), nil
	}
	if len(deferredRemovals) > 0 {
		return deferredRemovals[0], nil
	}
	return nil, releaseAt
}

// HostToolState is one host card entry (control-api.md GET /v1/admin/hosts/{id}/compat-tools).
type HostToolState struct {
	ToolID    *string `json:"tool_id"`
	SteamName *string `json:"steam_name"`
	Tag       string  `json:"tag"`
	State     string  `json:"state"`
	Bytes     *int64  `json:"bytes"`
	Reason    *string `json:"reason"`
	RunID     *string `json:"run_id"`
}

func strp(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// state is the first row of the per-host install state table that holds, "" when the tool has
// no entry on this host (disabled, not installed, no open run).
func (f *fleet) state(host string, t geTool, supported bool, now time.Time) HostToolState {
	out := HostToolState{ToolID: &t.ID, SteamName: t.SteamName, Tag: t.Tag}
	installed := f.installedAs(host, t)
	if installed == nil {
		for _, r := range f.installed[host] {
			if r.SHA512 == t.SHA512 {
				installed = &r
			}
		}
	}
	var runID string
	for _, r := range f.runs[host] {
		if r.SHA512 == t.SHA512 {
			runID = r.ID
		}
	}
	out.RunID = strp(runID)
	switch {
	case !supported && t.Enabled:
		out.State = "unsupported"
	case !supported:
	case f.open(host, "remove", t.SHA512, now) != nil:
		out.State = "removing"
	case f.open(host, "install", t.SHA512, now) != nil:
		out.State = "installing"
	case installed != nil:
		out.State = "installed"
		out.Bytes = &installed.Bytes
	case t.NameConflict:
		out.State, out.Reason = "failed", strp("name_conflict")
	default:
		if held, reason, _ := f.hold(host, "install", t.SHA512, &t, now); held && t.desired() {
			out.State, out.Reason = "failed", strp(reason)
		} else if t.Enabled {
			out.State = "waiting"
			for _, r := range f.runs[host] {
				if r.Action == "install" && r.SHA512 == t.SHA512 && r.State == "deferred" {
					out.Reason = strp(r.Reason)
				}
			}
		}
	}
	if out.State == "unsupported" || out.State == "" {
		out.Bytes = nil
	}
	return out
}

// hostCard is every entry for one host: each GE tool with a state there, then installed rows
// that match no catalogue row.
func (f *fleet) hostCard(host string, supported bool, now time.Time) []HostToolState {
	out := []HostToolState{}
	for _, t := range f.tools {
		if s := f.state(host, t, supported, now); s.State != "" {
			out = append(out, s)
		}
	}
	for _, r := range f.installed[host] {
		if f.toolBySHA(r.SHA512) == nil {
			b, name := r.Bytes, r.Name
			out = append(out, HostToolState{SteamName: &name, Tag: r.Tag, State: "installed", Bytes: &b})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ToolID != nil && out[j].ToolID == nil })
	return out
}

// Converger materializes compat_tools.converge runs, fills their params when a host claims one,
// and records each install or removal.
type Converger struct {
	pool interface {
		querier
		Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	}
	runs    *jobs.Store
	auditor audit.Recorder
	// Disp is set once the dispatcher exists; until then no run is materialized.
	Disp      *jobs.Dispatcher
	Supported func(hostID string) bool
	log       *slog.Logger
	now       func() time.Time
}

func NewConverger(pool *pgxpool.Pool, runs *jobs.Store, auditor audit.Recorder, log *slog.Logger) *Converger {
	return &Converger{pool: pool, runs: runs, auditor: auditor, log: log, now: time.Now}
}

func (c *Converger) supported(hostID string) bool { return c.Supported != nil && c.Supported(hostID) }

// Host materializes one run for the host's first difference when it has no open run. pull (a
// desired-state change) also pulls an open pending run forward, keeping its attempt.
func (c *Converger) Host(ctx context.Context, hostID string, pull bool) {
	if !c.supported(hostID) || c.Disp == nil {
		return
	}
	f, err := loadFleet(ctx, c.pool, hostID)
	if err != nil {
		c.log.Warn("compat tools: reading desired state failed", "host_id", hostID, "err", err)
		return
	}
	_, found, err := c.runs.OpenRun(ctx, JobID, hostID)
	if err != nil {
		return
	}
	params, releaseAt := f.firstDifference(hostID, c.now())
	switch {
	case found && pull:
		// Its params are filled at hand-out, so pulling it forward is enough.
		_, err = c.Disp.Enqueue(ctx, JobID, hostID, nil)
	case found, params == nil && releaseAt == nil:
		return
	case params == nil:
		_, _, err = c.runs.Materialize(ctx, jobs.MaterializeParams{
			JobID: JobID, HostID: hostID, Trigger: jobs.TriggerEvent, ScheduledFor: *releaseAt})
	default:
		_, err = c.Disp.Enqueue(ctx, JobID, hostID, params)
	}
	if err != nil && !errors.Is(err, jobs.ErrDisabled) {
		c.log.Warn("compat tools: materializing a converge run failed", "host_id", hostID, "err", err)
	}
}

// All runs Host for every host in scope.
func (c *Converger) All(ctx context.Context, pull bool) {
	for _, h := range c.hostIDs(ctx) {
		c.Host(ctx, h, pull)
	}
}

func (c *Converger) hostIDs(ctx context.Context) []string {
	rows, err := c.pool.Query(ctx, `SELECT id::text FROM hosts ORDER BY id`)
	if err != nil {
		c.log.Warn("compat tools: listing hosts failed", "err", err)
		return nil
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		c.log.Warn("compat tools: listing hosts failed", "err", err)
	}
	return ids
}

// Params is the job's hand-out hook: the host's current first difference, nil when there is
// none (the run is then closed skipped). A deferred retry never repeats its old params.
func (c *Converger) Params(ctx context.Context, hostID string) (any, error) {
	if !c.supported(hostID) {
		return nil, nil
	}
	f, err := loadFleet(ctx, c.pool, hostID)
	if err != nil {
		return nil, err
	}
	params, _ := f.firstDifference(hostID, c.now())
	if params == nil {
		return nil, nil
	}
	return params, nil
}

// Closed is the job's OnTerminal hook: audit a succeeded or failed install or removal, then
// look for the host's next difference.
func (c *Converger) Closed(ctx context.Context, run jobs.Run) {
	defer c.Host(ctx, run.HostID, false)
	if run.State != jobs.StateSucceeded && run.State != jobs.StateFailed {
		return
	}
	var p, s map[string]any
	_ = json.Unmarshal(run.Params, &p)
	_ = json.Unmarshal(run.Summary, &s)
	action, _ := p["action"].(string)
	if action != "install" && action != "remove" {
		return
	}
	sha, _ := p["sha512"].(string)
	toolID, _ := p["tool_id"].(string)
	name, _ := s["name"].(string)
	if name == "" {
		name, _ = p["name"].(string)
	}
	tag, _ := p["tag"].(string)
	if tag == "" {
		_ = c.pool.QueryRow(ctx, `SELECT COALESCE((SELECT tag FROM compat_tools WHERE asset_sha512 = $1),
			(SELECT tag FROM host_compat_tools WHERE host_id = $2::uuid AND sha512 = $1 LIMIT 1), '')`,
			sha, run.HostID).Scan(&tag)
	}
	actor := run.ActorUserID
	if actor == "" && toolID != "" {
		_ = c.pool.QueryRow(ctx, `SELECT COALESCE(actor_user_id::text, '') FROM admin_activity
			WHERE action IN ('compat_tool.enable', 'compat_tool.disable') AND target_id = $1
			ORDER BY created_at DESC LIMIT 1`, toolID).Scan(&actor)
	}
	if action == "install" && run.State == jobs.StateSucceeded && p["expected_name"] == nil && toolID != "" {
		c.bindName(ctx, run, toolID, tag, sha, name)
	}
	// name and reason are the agent's own words: cap them here so the common case stays readable,
	// and leave the guarantee to audit.TryRecord, which cuts the encoded row to fit rather than
	// dropping it (#1183, #1209). A reason that is not text is dropped and flagged, never written
	// as a structure.
	details := map[string]any{
		"tag": tag, "sha512": sha, "host_id": run.HostID, "state": string(run.State), "run_id": run.ID,
	}
	audit.CapText(details, "name", name)
	audit.CapText(details, "reason", s["reason"])
	audit.TryRecord(ctx, c.auditor, actor, "compat_tool."+action, "host", run.HostID, details)
}

// bindName binds a tag's Steam name at its first succeeded install, in the statement that checks
// it is still unbound (control-api.md §Install job, "A reported name that conflicts"). A row only
// ever binds its own asset stem (`<tag>-x86_64`, or `<tag>` for a plain asset): that is the name
// GE-Proton's compatibilitytool.vdf registers, as every entry of the shipped list in tools.go has
// (TestShippedNamesAreTheirAssetStems), so a tag row can never take another tag's name. Any other
// name, or one another row holds, marks the tool name_conflict, which takes it out of desired state
// with nothing retried.
//
// shortcut: a host whose succeeded install reports a name other than the bound one is not marked
// with a name_mismatch hold here; its first difference removes it by its own name, and the next
// install carries expected_name and fails name_mismatch at the agent. Identical bytes always carry
// the same vdf, so only a misbehaving agent reaches this.
func (c *Converger) bindName(ctx context.Context, run jobs.Run, toolID, tag, sha, name string) {
	bound, err := c.pool.Exec(ctx, `UPDATE compat_tools SET steam_name = $2, display_name = $2
		WHERE id = $1::uuid AND steam_name IS NULL AND $2 = left(asset_name, -length('.tar.gz'))
		  AND NOT EXISTS (SELECT 1 FROM compat_tools WHERE steam_name = $2)`, toolID, name)
	var pgErr *pgconn.PgError
	switch {
	case errors.As(err, &pgErr) && pgErr.Code == "23505":
		// Another row bound the name after our NOT EXISTS read it: held, so a conflict.
	case err != nil:
		c.log.Warn("compat tools: binding a Steam name failed", "tool_id", toolID, "err", err)
		return
	case bound.RowsAffected() == 1:
		return
	}
	ct, err := c.pool.Exec(ctx, `UPDATE compat_tools SET name_conflict = true
		WHERE id = $1::uuid AND steam_name IS NULL AND NOT name_conflict`, toolID)
	if err != nil {
		c.log.Warn("compat tools: marking a name conflict failed", "tool_id", toolID, "err", err)
		return
	}
	if ct.RowsAffected() == 1 {
		details := map[string]any{"tag": tag, "sha512": sha, "host_id": run.HostID, "run_id": run.ID}
		audit.CapText(details, "name", name)
		audit.TryRecord(ctx, c.auditor, "", "compat_tool.name_conflict", "compat_tool", toolID, details)
		c.All(ctx, true)
	}
}

// HostCard is GET /v1/admin/hosts/{id}/compat-tools.
func (c *Converger) HostCard(ctx context.Context, hostID string) (map[string]any, error) {
	var free *int64
	var reported *time.Time
	if err := c.pool.QueryRow(ctx, `SELECT compat_tools_free_bytes, compat_tools_reported_at FROM hosts WHERE id = $1::uuid`,
		hostID).Scan(&free, &reported); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	f, err := loadFleet(ctx, c.pool, hostID)
	if err != nil {
		return nil, err
	}
	supported := c.supported(hostID)
	if !supported {
		free = nil
	}
	return map[string]any{"supported": supported, "free_bytes": free, "reported_at": reported,
		"tools": f.hostCard(hostID, supported, c.now())}, nil
}

// counts fills Tool.Hosts for every GE tool, over every host in scope.
func (c *Converger) counts(ctx context.Context, items []Tool) error {
	f, err := loadFleet(ctx, c.pool, "")
	if err != nil {
		return err
	}
	hosts := c.hostIDs(ctx)
	now := c.now()
	for i := range items {
		if items[i].Hosts == nil {
			continue
		}
		var t *geTool
		for j := range f.tools {
			if f.tools[j].ID == items[i].ID {
				t = &f.tools[j]
			}
		}
		if t == nil {
			continue
		}
		n := HostCount{}
		for _, h := range hosts {
			switch f.state(h, *t, c.supported(h), now).State {
			case "installed":
				n.Installed++
			case "installing", "removing":
				n.Installing++
			case "waiting":
				n.Waiting++
			case "failed":
				n.Failed++
			case "unsupported":
				n.Unsupported++
			}
		}
		items[i].Hosts = &n
	}
	return nil
}
