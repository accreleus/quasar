// Admin CRUD surface for runtime presets: shared container config (image, args,
// env, mounts, managed-home defaults) apps inherit. Never call this a "launch
// profile" — that's UI-P4's unrelated quality/encode chain object.
//
// Every route is under RequireAuth->RequireAdmin (CLAUDE.md invariant #6); the
// admin UI's disabled Delete button is UX only, the 409 below is enforcement.
//
// The preset->app merge is NOT here: it happens server-side at launch
// (internal/session/runtime_preset.go), so editing a preset changes the next
// launch of every app using it.
package crud

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/accreleus/quasar/control-plane/internal/audit"
	"github.com/accreleus/quasar/control-plane/internal/auth"
	"github.com/accreleus/quasar/control-plane/internal/httpx"
	"github.com/accreleus/quasar/control-plane/internal/mountpolicy"
	"github.com/accreleus/quasar/control-plane/internal/runtimeconfig"
)

// ErrPresetInUse: refuse-if-in-use with a 409, mirrors ErrAppHasActiveSessions.
// Migration 0035's ON DELETE RESTRICT is the backstop, not this gate.
var ErrPresetInUse = errors.New("runtime preset is in use")

var ErrPresetNameTaken = errors.New("runtime preset name already exists")

// ErrManagedPresetImageInvalid: a PATCH targets a managed preset's `image`
// (managed_image_id set, migration 0058) with a value the adoption pipeline
// couldn't itself have produced (#498). See validManagedPresetImage.
var ErrManagedPresetImageInvalid = errors.New("image value not permitted on a managed runtime preset")

// RuntimePreset is the domain view of a runtime_presets row.
type RuntimePreset struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	// Mirror apps.runtime_spec's shape as first-class fields: the admin UI and
	// the launch-time merge both read them individually.
	Image  string          `json:"image"`
	Args   json.RawMessage `json:"args"`
	Env    json.RawMessage `json:"env"`
	Mounts json.RawMessage `json:"mounts"`
	// Storage defaults an app inherits and may override (session.mergeManagedHome).
	ManagedHome       bool   `json:"managed_home"`
	HomeContainerPath string `json:"home_container_path"`
	// S2 container-network requirement: "" (host default) | "none" | "bridge" |
	// "host". Validated here and by runtime_presets_network_ck (migration 0061).
	Network   string    `json:"network"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// UsedBy is resolved per read, never stored; feeds the admin "Used by" row.
	UsedBy []PresetUser `json:"used_by"`
}

// PresetUser is one app referencing a preset.
type PresetUser struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// --- store ---

// listRuntimePresets returns every preset, oldest-first by name, each with its
// used_by app list resolved in the same query.
func (s *store) listRuntimePresets(ctx context.Context) ([]RuntimePreset, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT rp.id::text, rp.name, rp.description, rp.image, rp.args, rp.env, rp.mounts,
		       rp.managed_home, rp.home_container_path, rp.network, rp.created_at, rp.updated_at,
		       COALESCE(
		           (SELECT json_agg(json_build_object('id', a.id::text, 'name', a.name) ORDER BY a.name)
		            FROM apps a WHERE a.runtime_preset_id = rp.id),
		           '[]'::json)
		FROM runtime_presets rp
		ORDER BY rp.name ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("query runtime presets: %w", err)
	}
	defer rows.Close()

	presets := []RuntimePreset{}
	for rows.Next() {
		p, err := scanRuntimePreset(rows)
		if err != nil {
			return nil, err
		}
		presets = append(presets, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return presets, nil
}

// rowScanner is satisfied by both pgx.Row and pgx.Rows.
type rowScanner interface{ Scan(dest ...any) error }

func scanRuntimePreset(r rowScanner) (RuntimePreset, error) {
	var p RuntimePreset
	var usedBy []byte
	if err := r.Scan(&p.ID, &p.Name, &p.Description, &p.Image, &p.Args, &p.Env, &p.Mounts,
		&p.ManagedHome, &p.HomeContainerPath, &p.Network, &p.CreatedAt, &p.UpdatedAt, &usedBy); err != nil {
		return RuntimePreset{}, fmt.Errorf("scan runtime preset: %w", err)
	}
	p.UsedBy = []PresetUser{}
	if len(usedBy) > 0 {
		if err := json.Unmarshal(usedBy, &p.UsedBy); err != nil {
			return RuntimePreset{}, fmt.Errorf("decode used_by: %w", err)
		}
	}
	return p, nil
}

const selectRuntimePresetSQL = `
	SELECT rp.id::text, rp.name, rp.description, rp.image, rp.args, rp.env, rp.mounts,
	       rp.managed_home, rp.home_container_path, rp.network, rp.created_at, rp.updated_at,
	       COALESCE(
	           (SELECT json_agg(json_build_object('id', a.id::text, 'name', a.name) ORDER BY a.name)
	            FROM apps a WHERE a.runtime_preset_id = rp.id),
	           '[]'::json)
	FROM runtime_presets rp
	WHERE rp.id::text = $1`

func (s *store) getRuntimePreset(ctx context.Context, id string) (RuntimePreset, error) {
	p, err := scanRuntimePreset(s.pool.QueryRow(ctx, selectRuntimePresetSQL, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return RuntimePreset{}, ErrNotFound
	}
	return p, err
}

// presetWrite is the create/patch payload after validation. Every field is a
// pointer: absent means "schema default on create, unchanged on patch", never
// a zero value.
type presetWrite struct {
	Name              *string
	Description       *string
	Image             *string
	Args              json.RawMessage
	Env               json.RawMessage
	Mounts            json.RawMessage
	ManagedHome       *bool
	HomeContainerPath *string
	Network           *string
}

func (s *store) createRuntimePreset(ctx context.Context, w presetWrite) (RuntimePreset, error) {
	cols := []string{"name"}
	args := []any{*w.Name}
	for _, f := range []struct {
		col string
		val any
	}{
		{"description", strOrNil(w.Description)},
		{"image", strOrNil(w.Image)},
		{"args", jsonOrNil(w.Args)},
		{"env", jsonOrNil(w.Env)},
		{"mounts", jsonOrNil(w.Mounts)},
		{"managed_home", boolOrNil(w.ManagedHome)},
		{"home_container_path", strOrNil(w.HomeContainerPath)},
		{"network", strOrNil(w.Network)},
	} {
		if f.val == nil {
			continue // omit the column so Postgres applies the schema DEFAULT
		}
		cols = append(cols, f.col)
		args = append(args, f.val)
	}
	placeholders := make([]string, len(args))
	for i := range args {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
	}
	var id string
	q := fmt.Sprintf(`INSERT INTO runtime_presets (%s) VALUES (%s) RETURNING id::text`,
		strings.Join(cols, ", "), strings.Join(placeholders, ", "))
	if err := s.pool.QueryRow(ctx, q, args...).Scan(&id); err != nil {
		if isUniqueViolation(err) {
			return RuntimePreset{}, ErrPresetNameTaken
		}
		return RuntimePreset{}, fmt.Errorf("insert runtime preset: %w", err)
	}
	return s.getRuntimePreset(ctx, id)
}

// presetSnapshot is the pre-write read updateRuntimePreset takes before a
// PATCH: needed to validate a managed row's new `image` (#498) and as the old
// side of the attribution log, both unavailable once the write has happened.
type presetSnapshot struct {
	Name              string
	Description       string
	Image             string
	Args              json.RawMessage
	Env               json.RawMessage
	Mounts            json.RawMessage
	ManagedHome       bool
	HomeContainerPath string
	Network           string
	// Non-nil exactly when image-managed (migration 0058). Nil means
	// admin-authored, unconstrained by validManagedPresetImage.
	ManagedImageID *string
}

func (s *store) getPresetSnapshot(ctx context.Context, id string) (presetSnapshot, error) {
	var p presetSnapshot
	err := s.pool.QueryRow(ctx, `
		SELECT name, description, image, args, env, mounts, managed_home, home_container_path, network, managed_image_id
		FROM runtime_presets WHERE id::text = $1`, id).Scan(
		&p.Name, &p.Description, &p.Image, &p.Args, &p.Env, &p.Mounts,
		&p.ManagedHome, &p.HomeContainerPath, &p.Network, &p.ManagedImageID)
	if errors.Is(err, pgx.ErrNoRows) {
		return presetSnapshot{}, ErrNotFound
	}
	if err != nil {
		return presetSnapshot{}, fmt.Errorf("snapshot runtime preset: %w", err)
	}
	return p, nil
}

// validManagedPresetImage reports whether image is a value the adoption
// pipeline (internal/images) could itself have produced for managedImageID
// (#498): a digest ref (contains "@sha256:") or exactly this image's
// deterministic template tag "quasar-local/<managed_image_id>:" prefix.
// Anything else, including another image's quasar-local/ tag, is refused —
// must trace back to this image's own adoption, not merely look plausible.
func validManagedPresetImage(image, managedImageID string) bool {
	if strings.Contains(image, "@sha256:") {
		return true
	}
	return strings.HasPrefix(image, "quasar-local/"+managedImageID+":")
}

func (s *store) updateRuntimePreset(ctx context.Context, id string, w presetWrite, actor auth.User) (RuntimePreset, error) {
	old, err := s.getPresetSnapshot(ctx, id)
	if err != nil {
		return RuntimePreset{}, err
	}

	// #498: only fires when this PATCH touches `image` on a managed row, so a
	// pre-existing value never retroactively breaks unrelated edits.
	if w.Image != nil && old.ManagedImageID != nil && !validManagedPresetImage(*w.Image, *old.ManagedImageID) {
		return RuntimePreset{}, ErrManagedPresetImageInvalid
	}

	var setClauses []string
	var args []any
	add := func(col string, val any) {
		args = append(args, val)
		setClauses = append(setClauses, fmt.Sprintf("%s = $%d", col, len(args)))
	}
	if w.Name != nil {
		add("name", *w.Name)
	}
	if w.Description != nil {
		add("description", *w.Description)
	}
	if w.Image != nil {
		add("image", *w.Image)
	}
	if len(w.Args) > 0 {
		add("args", w.Args)
	}
	if len(w.Env) > 0 {
		add("env", w.Env)
	}
	if len(w.Mounts) > 0 {
		add("mounts", w.Mounts)
	}
	if w.ManagedHome != nil {
		add("managed_home", *w.ManagedHome)
	}
	if w.HomeContainerPath != nil {
		add("home_container_path", *w.HomeContainerPath)
	}
	if w.Network != nil {
		add("network", *w.Network)
	}
	if len(setClauses) == 0 {
		return s.getRuntimePreset(ctx, id) // nothing to patch
	}
	args = append(args, id)
	q := fmt.Sprintf(`UPDATE runtime_presets SET %s WHERE id::text = $%d RETURNING id::text`,
		strings.Join(setClauses, ", "), len(args))
	var outID string
	if err := s.pool.QueryRow(ctx, q, args...).Scan(&outID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return RuntimePreset{}, ErrNotFound
		}
		if isUniqueViolation(err) {
			return RuntimePreset{}, ErrPresetNameTaken
		}
		return RuntimePreset{}, fmt.Errorf("update runtime preset: %w", err)
	}
	updated, err := s.getRuntimePreset(ctx, outID)
	if err != nil {
		return RuntimePreset{}, err
	}
	logPresetUpdateAttribution(actor, id, old, updated)
	return updated, nil
}

// logPresetUpdateAttribution emits the #498 attribution log line: actor,
// preset id, managed_image_id, and old->new for every changed column.
// reconcileRuntimeDrift (internal/images/preset.go) is the sync-path analogue
// for the same gap (#470), same old_X/new_X key shape.
func logPresetUpdateAttribution(actor auth.User, id string, old presetSnapshot, updated RuntimePreset) {
	attrs := []any{
		"actor_id", actor.ID, "actor_email", actor.Email,
		"preset_id", id, "managed_image_id", nilableString(old.ManagedImageID),
	}
	changed := false
	addIfChanged := func(col string, oldVal, newVal any, equal bool) {
		if equal {
			return
		}
		attrs = append(attrs, "old_"+col, oldVal, "new_"+col, newVal)
		changed = true
	}
	addIfChanged("name", old.Name, updated.Name, old.Name == updated.Name)
	addIfChanged("description", old.Description, updated.Description, old.Description == updated.Description)
	addIfChanged("image", old.Image, updated.Image, old.Image == updated.Image)
	addIfChanged("args", string(old.Args), string(updated.Args), jsonEqualTrim(old.Args, updated.Args))
	addIfChanged("env", string(old.Env), string(updated.Env), jsonEqualTrim(old.Env, updated.Env))
	addIfChanged("mounts", string(old.Mounts), string(updated.Mounts), jsonEqualTrim(old.Mounts, updated.Mounts))
	addIfChanged("managed_home", old.ManagedHome, updated.ManagedHome, old.ManagedHome == updated.ManagedHome)
	addIfChanged("home_container_path", old.HomeContainerPath, updated.HomeContainerPath, old.HomeContainerPath == updated.HomeContainerPath)
	addIfChanged("network", old.Network, updated.Network, old.Network == updated.Network)
	if !changed {
		return
	}
	slog.Info("admin PATCH: runtime preset updated", attrs...)
}

// jsonEqualTrim compares two JSONB columns read back from Postgres: both sides
// share the same canonical serialization, so a trimmed byte compare suffices
// (unlike runtimeBlocksEqual, which compares against a manifest literal).
func jsonEqualTrim(a, b json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(a), bytes.TrimSpace(b))
}

// nilableString flattens a *string for slog: the value, or nil.
func nilableString(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

// deleteRuntimePreset refuses while any app references it. Existence and
// in-use checks run in one transaction so a concurrent app edit can't slip a
// reference in between; mirrors deleteApp's pattern, returning the deleted
// name for the audit row.
func (s *store) deleteRuntimePreset(ctx context.Context, id string) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("begin delete runtime preset tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck — no-op after commit

	var name string
	err = tx.QueryRow(ctx, `SELECT name FROM runtime_presets WHERE id::text = $1`, id).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("check runtime preset exists: %w", err)
	}

	// Enforcement (the disabled Delete button in the UI is not). Counts every
	// referencing app including disabled ones — they still hold the reference.
	var inUse int
	if err := tx.QueryRow(ctx,
		`SELECT COUNT(*) FROM apps WHERE runtime_preset_id::text = $1`, id).Scan(&inUse); err != nil {
		return "", fmt.Errorf("count apps using runtime preset: %w", err)
	}
	if inUse > 0 {
		return "", ErrPresetInUse
	}

	tag, err := tx.Exec(ctx, `DELETE FROM runtime_presets WHERE id::text = $1`, id)
	if err != nil {
		return "", fmt.Errorf("delete runtime preset: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return "", ErrNotFound
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return name, nil
}

// validRuntimePresetID: nil/"" (clear the reference) is always valid.
func (s *store) validRuntimePresetID(ctx context.Context, id *string) (bool, error) {
	if id == nil || *id == "" {
		return true, nil
	}
	var ok bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM runtime_presets WHERE id::text = $1)`, *id).Scan(&ok); err != nil {
		return false, fmt.Errorf("validate runtime preset: %w", err)
	}
	return ok, nil
}

func strOrNil(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

func boolOrNil(b *bool) any {
	if b == nil {
		return nil
	}
	return *b
}

func jsonOrNil(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return raw
}

func isUniqueViolation(err error) bool {
	return strings.Contains(err.Error(), "SQLSTATE 23505")
}

// --- handlers ---

func (h *Handler) handleListRuntimePresets(w http.ResponseWriter, r *http.Request) {
	presets, err := h.store.listRuntimePresets(r.Context())
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not list runtime presets")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": presets})
}

func (h *Handler) handleGetRuntimePreset(w http.ResponseWriter, r *http.Request) {
	p, err := h.store.getRuntimePreset(r.Context(), r.PathValue("id"))
	if err != nil {
		writePresetErr(w, err, "could not get runtime preset")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"runtime_preset": p})
}

func (h *Handler) handleCreateRuntimePreset(w http.ResponseWriter, r *http.Request) {
	var req presetReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Name == nil || strings.TrimSpace(*req.Name) == "" {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "name is required")
		return
	}
	write, ok := req.validate(w)
	if !ok {
		return
	}
	p, err := h.store.createRuntimePreset(r.Context(), write)
	if err != nil {
		writePresetErr(w, err, "could not create runtime preset")
		return
	}
	h.recordActivity(r, "runtime_preset.create", "runtime_preset", p.ID, map[string]any{"name": p.Name})
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"runtime_preset": p})
}

func (h *Handler) handleUpdateRuntimePreset(w http.ResponseWriter, r *http.Request) {
	var req presetReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Name != nil && strings.TrimSpace(*req.Name) == "" {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "name cannot be empty")
		return
	}
	write, ok := req.validate(w)
	if !ok {
		return
	}
	user, _ := auth.UserFromContext(r.Context())
	p, err := h.store.updateRuntimePreset(r.Context(), r.PathValue("id"), write, user)
	if err != nil {
		writePresetErr(w, err, "could not update runtime preset")
		return
	}
	// Changed field names only: env/args can hold credentials, so the audit log
	// records that they moved, never to what.
	h.recordActivity(r, "runtime_preset.update", "runtime_preset", p.ID,
		map[string]any{"keys": audit.ChangedKeys(req)})
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"runtime_preset": p})
}

func (h *Handler) handleDeleteRuntimePreset(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	name, err := h.store.deleteRuntimePreset(r.Context(), id)
	switch {
	case err == nil:
		h.recordActivity(r, "runtime_preset.delete", "runtime_preset", id, map[string]any{"name": name})
		w.WriteHeader(http.StatusNoContent)
	default:
		writePresetErr(w, err, "could not delete runtime preset")
	}
}

// presetReq is the wire shape for POST/PATCH. All-pointer, same reason as
// AppWrite: absent must fall through to the schema default on create and leave
// the column alone on patch, never decode to a zero value.
type presetReq struct {
	Name              *string         `json:"name"`
	Description       *string         `json:"description"`
	Image             *string         `json:"image"`
	Args              json.RawMessage `json:"args"`
	Env               json.RawMessage `json:"env"`
	Mounts            json.RawMessage `json:"mounts"`
	ManagedHome       *bool           `json:"managed_home"`
	HomeContainerPath *string         `json:"home_container_path"`
	Network           *string         `json:"network"`
}

// validate type-checks the JSONB payloads and the home path, writing the 400
// itself on failure. args/mounts must be arrays of strings, env an object of
// strings, matching apps.runtime_spec's shape.
//
// Explicit null is rejected rather than silently accepted: json.RawMessage
// does not special-case null, so `{"args": null}` decodes to the 4 bytes
// "null", which unmarshals cleanly into a nil slice/map (isStringArray/
// isStringMap both return true) and would then pass update's `len(w.Args) > 0`
// guard and write a literal `'null'::jsonb`. control-api.md's runtime-preset
// section assigns no meaning to explicit null for args/env/mounts — only
// absent and real data — so this rejects with a 400 telling the caller to send
// `[]`/`{}` instead. Both create and update handlers route through this one
// validate().
func (p presetReq) validate(w http.ResponseWriter) (presetWrite, bool) {
	if isExplicitJSONNull(p.Args) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed,
			"args cannot be JSON null — send [] to clear it, or omit the field to leave it unchanged")
		return presetWrite{}, false
	}
	if len(p.Args) > 0 && !isStringArray(p.Args) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "args must be an array of strings")
		return presetWrite{}, false
	}
	if isExplicitJSONNull(p.Mounts) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed,
			"mounts cannot be JSON null — send [] to clear it, or omit the field to leave it unchanged")
		return presetWrite{}, false
	}
	if len(p.Mounts) > 0 && !isStringArray(p.Mounts) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "mounts must be an array of strings")
		return presetWrite{}, false
	}
	// Same deny list the catalog door applies (internal/mountpolicy) — the column is
	// one, so an admin write must not be the laxer way in. What a host actually
	// permits is the node agent's allowlist.
	if len(p.Mounts) > 0 {
		var mounts []string
		_ = json.Unmarshal(p.Mounts, &mounts)
		if err := mountpolicy.ValidateAll(mounts); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, err.Error())
			return presetWrite{}, false
		}
	}
	if isExplicitJSONNull(p.Env) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed,
			"env cannot be JSON null — send {} to clear it, or omit the field to leave it unchanged")
		return presetWrite{}, false
	}
	if len(p.Env) > 0 && !isStringMap(p.Env) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "env must be an object of string values")
		return presetWrite{}, false
	}
	if p.HomeContainerPath != nil && !strings.HasPrefix(*p.HomeContainerPath, "/") {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "home_container_path must be absolute")
		return presetWrite{}, false
	}
	// Accepted set lives in internal/runtimeconfig, not here, since the same
	// column is also written by internal/images/preset.go and two copies would
	// drift. `host` is refused: see runtimeconfig's package doc.
	if p.Network != nil && !runtimeconfig.ValidNetwork(*p.Network) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, runtimeconfig.NetworkError)
		return presetWrite{}, false
	}
	return presetWrite{
		Name:              p.Name,
		Description:       p.Description,
		Image:             p.Image,
		Args:              p.Args,
		Env:               p.Env,
		Mounts:            p.Mounts,
		ManagedHome:       p.ManagedHome,
		HomeContainerPath: p.HomeContainerPath,
		Network:           p.Network,
	}, true
}

// isExplicitJSONNull distinguishes literal `null` from absent (len(raw) == 0)
// or real data. See validate() for why this matters.
func isExplicitJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func isStringArray(raw json.RawMessage) bool {
	var v []string
	return json.Unmarshal(raw, &v) == nil
}

func isStringMap(raw json.RawMessage) bool {
	var v map[string]string
	return json.Unmarshal(raw, &v) == nil
}

func writePresetErr(w http.ResponseWriter, err error, fallback string) {
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "runtime preset not found")
	case errors.Is(err, ErrPresetInUse):
		httpx.WriteError(w, http.StatusConflict, httpx.CodeConflict,
			"runtime preset is in use by one or more apps — point them elsewhere first")
	case errors.Is(err, ErrPresetNameTaken):
		httpx.WriteError(w, http.StatusConflict, httpx.CodeConflict, "a runtime preset with that name already exists")
	case errors.Is(err, ErrManagedPresetImageInvalid):
		httpx.WriteError(w, http.StatusUnprocessableEntity, httpx.CodeManagedPresetImageInvalid,
			"this preset is managed by a catalog image — its image value must be a digest ref or the image's own "+
				"quasar-local/<id>: build tag; to point a session at a local build for testing, create a separate "+
				"(non-managed) scratch preset instead of editing this one")
	default:
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, fallback)
	}
}
