package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Display names for the ids on an audit row. Derived at read time and never
// stored, like ActorUsername and Severity: the log is append-only, so a rename
// must show the current name. A hard-deleted entity has nothing left to join
// against, so stampedName falls back to the name the emitter wrote into
// `details`. semantics: control-api.md "Audit-log names"
//
// itemRefs/collectRefs decide what to look up with no I/O; resolveNames does
// the I/O; attachNames is pure again.

// kind identifies which table resolves a reference's name.
type kind string

const (
	kindApp            kind = "app"
	kindHost           kind = "host"
	kindUser           kind = "user"
	kindSession        kind = "session"
	kindRuntimePreset  kind = "runtime_preset"
	kindStreamProfile  kind = "stream_profile"
	kindLaunchProfile  kind = "launch_profile"
	kindImage          kind = "image"
	kindJob            kind = "job"
	kindRelease        kind = "platform_release"
	kindApplyRun       kind = "platform_apply_run"
	kindHostEnrollment kind = "host_enrollment"
)

// ref is one thing to name.
type ref struct {
	kind kind
	id   string
}

// uuidKinds are the kinds keyed by uuid; the rest are text slugs (`4k120`, an
// image id). A slug must never reach a `::uuid` cast — that errors the query
// rather than missing.
var uuidKinds = map[kind]bool{
	kindApp: true, kindHost: true, kindUser: true, kindSession: true,
	kindRuntimePreset: true, kindRelease: true, kindApplyRun: true,
	kindHostEnrollment: true,
}

// targetKinds maps target_type to the table that names it. Absent means no name
// worth showing: an invite's code is never recorded, a secret's target_id
// already is its name, `instance` names the deployment, and `storage_home` is a
// (user, app, host) triple whose tombstone stamps username and app_name.
var targetKinds = map[string]kind{
	"app": kindApp,
	// library.scan.force writes target_type=library carrying an APP id
	// (library/handler.go). Rows with that value are already in the table, so
	// this maps rather than corrects it.
	"library":         kindApp,
	"host":            kindHost,
	"user":            kindUser,
	"session":         kindSession,
	"runtime_preset":  kindRuntimePreset,
	"stream_profile":  kindStreamProfile,
	"launch_profile":  kindLaunchProfile,
	"image":           kindImage,
	"job":             kindJob,
	"host_enrollment": kindHostEnrollment,
}

// detailKinds allowlists the details keys carrying a resolvable id. An
// allowlist, not "anything uuid-shaped": `run_id` names two tables depending on
// the action, `subject_id` is a user only when its sibling subject_type says so,
// three id keys name nothing, and session.failed writes free text into
// `reason`/`state_detail` that can contain a uuid. The two conditional keys are
// handled in itemRefs.
var detailKinds = map[string]kind{
	"app_id":     kindApp,
	"host_id":    kindHost,
	"user_id":    kindUser,
	"session_id": kindSession,
	"release_id": kindRelease,
}

// targetKind picks the table for a row's target. `platform` names two of them,
// so the action disambiguates: cancel targets the run, everything else the
// release it applies.
func targetKind(action, targetType string) (kind, bool) {
	if targetType == "platform" {
		if action == "platform.apply.cancel" {
			return kindApplyRun, true
		}
		return kindRelease, true
	}
	k, ok := targetKinds[targetType]
	return k, ok
}

// itemRefs lists every (kind, id) a row points at: its target, then the
// allowlisted ids inside its details. Pure.
func itemRefs(action, targetType string, targetID *string, details map[string]any) []ref {
	var refs []ref
	add := func(k kind, id string) {
		if id == "" {
			return
		}
		if uuidKinds[k] && !isUUID(id) {
			return
		}
		refs = append(refs, ref{kind: k, id: id})
	}
	if targetID != nil {
		if k, ok := targetKind(action, targetType); ok {
			add(k, *targetID)
		}
	}
	for key, raw := range details {
		v, ok := raw.(string)
		if !ok {
			continue
		}
		if k, ok := detailKinds[key]; ok {
			add(k, v)
			continue
		}
		switch key {
		case "subject_id":
			// A grant is scoped to a user or to everyone; only the first
			// carries a user id (crud/entitlements.go).
			if sub, _ := details["subject_type"].(string); sub == "user" {
				add(kindUser, v)
			}
		case "run_id":
			// job.run's run_id names a job_runs row, which has no name of its
			// own; the job it belongs to is already the target.
			if action == "platform.apply.cancel" {
				add(kindApplyRun, v)
			}
		}
	}
	return refs
}

// collectRefs is itemRefs over a page, deduplicated: two rows naming the same
// host must not produce two lookups.
func collectRefs(items []Item) []ref {
	seen := make(map[ref]struct{})
	var out []ref
	for i := range items {
		for _, r := range itemRefs(items[i].Action, items[i].TargetType, items[i].TargetID, decodeDetails(items[i].Details)) {
			if _, dup := seen[r]; dup {
				continue
			}
			seen[r] = struct{}{}
			out = append(out, r)
		}
	}
	return out
}

// decodeDetails reads a row's payload as a map; a payload that is not a JSON
// object yields nil, which callers treat as no details.
func decodeDetails(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	return m
}

// stampedNameKeys are the details keys emitters use to record a name at write
// time, most specific first. They differ per site by history; renaming them
// would not reach the rows already written.
var stampedNameKeys = []string{"name", "node_name", "username", "app_name"}

// stampedName is the only thing that can name a hard-deleted entity.
func stampedName(details map[string]any) string {
	for _, key := range stampedNameKeys {
		if v, ok := details[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// attachNames fills each item's Names with the refs it actually carries, then
// falls back to the stamped name for an unresolved target. Names is always
// non-nil so a consumer can index it without a guard.
func attachNames(items []Item, resolved map[ref]string) {
	for i := range items {
		details := decodeDetails(items[i].Details)
		names := make(map[string]string)
		for _, r := range itemRefs(items[i].Action, items[i].TargetType, items[i].TargetID, details) {
			if name, ok := resolved[r]; ok && name != "" {
				names[r.id] = name
			}
		}
		if id := items[i].TargetID; id != nil && *id != "" && names[*id] == "" {
			if name := stampedName(details); name != "" {
				names[*id] = name
			}
		}
		items[i].Names = names
	}
}

// nameQueries is one lookup per kind, each a primary-key scan over a page's
// ids: a 100-row page costs at most len(nameQueries) lookups, never one per row.
var nameQueries = map[kind]string{
	kindApp:           `SELECT id::text, name FROM apps WHERE id = ANY($1::uuid[])`,
	kindHost:          `SELECT id::text, node_name FROM hosts WHERE id = ANY($1::uuid[])`,
	kindUser:          `SELECT id::text, username FROM users WHERE id = ANY($1::uuid[])`,
	kindRuntimePreset: `SELECT id::text, name FROM runtime_presets WHERE id = ANY($1::uuid[])`,
	kindStreamProfile: `SELECT id, display_name FROM stream_profiles WHERE id = ANY($1::text[])`,
	kindLaunchProfile: `SELECT id, display_name FROM launch_profiles WHERE id = ANY($1::text[])`,
	kindImage:         `SELECT id, display_name FROM image_catalog WHERE id = ANY($1::text[])`,
	kindJob:           `SELECT id, name FROM jobs WHERE id = ANY($1::text[])`,
	// A session has no name column; which app and whose is what an operator
	// wants. Same three joins as session/store.go's ListAll.
	kindSession: `
		SELECT s.id::text,
		       trim(both ' ' from concat_ws(' · ', NULLIF(a.name, ''), NULLIF(u.username, '')))
		FROM sessions s
		LEFT JOIN apps a ON a.id = s.app_id
		LEFT JOIN users u ON u.id = s.user_id
		WHERE s.id = ANY($1::uuid[])`,
	// Version, or a short commit for an edge build with none — the rule
	// platform/plan.go's releaseLabel applies.
	kindRelease: `
		SELECT id::text, COALESCE(NULLIF(version, ''), left(source_commit, 12), '')
		FROM platform_releases WHERE id = ANY($1::uuid[])`,
	kindApplyRun: `
		SELECT r.id::text, COALESCE(NULLIF(rel.version, ''), left(rel.source_commit, 12), '')
		FROM platform_apply_runs r
		LEFT JOIN platform_releases rel ON rel.id = r.release_id
		WHERE r.id = ANY($1::uuid[])`,
	// The host it is for, else its operator note; an open-ended one has neither.
	kindHostEnrollment: `
		SELECT id::text, COALESCE(NULLIF(node_name, ''), NULLIF(note, ''), '')
		FROM host_enrollments WHERE id = ANY($1::uuid[])`,
}

// resolveNames looks up every reference, one query per kind present.
func (s *Store) resolveNames(ctx context.Context, refs []ref) (map[ref]string, error) {
	byKind := make(map[kind][]string)
	for _, r := range refs {
		byKind[r.kind] = append(byKind[r.kind], r.id)
	}
	out := make(map[ref]string, len(refs))
	for k, ids := range byKind {
		query, ok := nameQueries[k]
		if !ok {
			continue
		}
		if err := s.collectNames(ctx, out, k, query, ids); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// collectNames runs one kind's lookup into out. Split out so rows.Close fires
// per kind, not at the end of the whole loop.
func (s *Store) collectNames(ctx context.Context, out map[ref]string, k kind, query string, ids []string) error {
	rows, err := s.pool.Query(ctx, query, ids)
	if err != nil {
		return fmt.Errorf("resolve %s names: %w", k, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return fmt.Errorf("resolve %s names: %w", k, err)
		}
		if name = trimName(name); name != "" {
			out[ref{kind: k, id: id}] = name
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("resolve %s names: %w", k, err)
	}
	return nil
}

// trimName bounds a pathological name so one row cannot dominate a response.
func trimName(v string) string {
	const max = 120
	v = strings.TrimSpace(v)
	if len(v) > max {
		return v[:max]
	}
	return v
}
