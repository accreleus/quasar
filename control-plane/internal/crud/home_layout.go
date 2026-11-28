package crud

// Server-owned Home layout. semantics: control-api.md §Home layout (Amendment 101, #1701).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/accreleus/quasar/control-plane/internal/auth"
	"github.com/accreleus/quasar/control-plane/internal/httpx"
)

const (
	homeSourceUser    = "user"
	homeSourceAdmin   = "admin"
	homeSourceBuiltin = "builtin"

	homeMaxSections = 16
	homeMaxTitle    = 64
)

var (
	homeSectionID = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
	// Kinds accepted on write; a read serves whatever is stored.
	homeKnownKinds  = map[string]bool{"highlights": true, "jump_back_in": true, "favourites": true}
	homeSectionKeys = map[string]bool{"id": true, "kind": true, "title": true, "enabled": true, "params": true}
)

// exactSectionKeys refuses a key outside homeSectionKeys, matched exactly. It exists
// because encoding/json matches field names case-insensitively and keeps the last of
// a repeated key, so "ID" or a second "title" would otherwise pass.
func exactSectionKeys(raw json.RawMessage) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return errors.New("not an object")
	}
	seen := map[string]bool{}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return err
		}
		k, _ := t.(string)
		if !homeSectionKeys[k] || seen[k] {
			return fmt.Errorf("unknown or repeated key %q", k)
		}
		seen[k] = true
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return err
		}
	}
	return nil
}

type HomeSection struct {
	ID      string         `json:"id"`
	Kind    string         `json:"kind"`
	Title   string         `json:"title"`
	Enabled bool           `json:"enabled"`
	Params  map[string]any `json:"params"`
}

type homeLayout struct {
	Source   string        `json:"source"`
	Sections []HomeSection `json:"sections"`
}

// builtinHome is today's order. Clients carry the same list for a 404 (older server).
func builtinHome() []HomeSection {
	return []HomeSection{
		{ID: "highlights", Kind: "highlights", Title: "Highlights", Enabled: true, Params: map[string]any{}},
		{ID: "jump_back_in", Kind: "jump_back_in", Title: "Jump back in", Enabled: true, Params: map[string]any{}},
		{ID: "favourites", Kind: "favourites", Title: "Favourites", Enabled: true, Params: map[string]any{}},
	}
}

// parseHomeLayout validates a PUT body and returns its sections. Top-level keys
// other than sections are ignored, so a client may send back what it read.
func parseHomeLayout(body []byte) ([]HomeSection, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil || top == nil {
		return nil, errors.New("body must be a JSON object")
	}
	var raws []json.RawMessage
	if err := json.Unmarshal(top["sections"], &raws); err != nil || raws == nil {
		return nil, errors.New("sections must be an array")
	}
	if len(raws) > homeMaxSections {
		return nil, fmt.Errorf("at most %d sections", homeMaxSections)
	}
	out := make([]HomeSection, 0, len(raws))
	seen := map[string]bool{}
	for i, raw := range raws {
		var s struct {
			ID      *string         `json:"id"`
			Kind    *string         `json:"kind"`
			Title   *string         `json:"title"`
			Enabled *bool           `json:"enabled"`
			Params  json.RawMessage `json:"params"`
		}
		if err := exactSectionKeys(raw); err != nil {
			return nil, fmt.Errorf("sections[%d]: must be an object with id, kind, title, enabled and optional params: %v", i, err)
		}
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("sections[%d]: must be an object with id, kind, title, enabled and optional params", i)
		}
		if s.ID == nil || s.Kind == nil || s.Title == nil || s.Enabled == nil {
			return nil, fmt.Errorf("sections[%d]: id, kind, title and enabled are required", i)
		}
		if !homeSectionID.MatchString(*s.ID) {
			return nil, fmt.Errorf("sections[%d].id must match %s", i, homeSectionID)
		}
		if seen[*s.ID] {
			return nil, fmt.Errorf("sections[%d].id %q is not unique", i, *s.ID)
		}
		seen[*s.ID] = true
		if !homeKnownKinds[*s.Kind] {
			return nil, fmt.Errorf("sections[%d].kind %q is not a known kind", i, *s.Kind)
		}
		if n := utf8.RuneCountInString(*s.Title); n == 0 || n > homeMaxTitle || strings.TrimSpace(*s.Title) == "" {
			return nil, fmt.Errorf("sections[%d].title must be 1-%d characters and not only whitespace", i, homeMaxTitle)
		}
		// Postgres jsonb refuses U+0000, which would surface as a 500.
		if strings.IndexFunc(*s.Title, unicode.IsControl) >= 0 {
			return nil, fmt.Errorf("sections[%d].title must not hold control characters", i)
		}
		params := map[string]any{}
		if s.Params != nil {
			if err := json.Unmarshal(s.Params, &params); err != nil || params == nil {
				return nil, fmt.Errorf("sections[%d].params must be an object", i)
			}
			// No kind defines a parameter yet.
			if len(params) > 0 {
				return nil, fmt.Errorf("sections[%d].params: kind %q takes no parameters", i, *s.Kind)
			}
		}
		out = append(out, HomeSection{ID: *s.ID, Kind: *s.Kind, Title: *s.Title, Enabled: *s.Enabled, Params: params})
	}
	return out, nil
}

// decodeStoredHome reads a stored {"sections": [...]} value; nil means not set.
func decodeStoredHome(raw []byte) ([]HomeSection, bool, error) {
	if raw == nil {
		return nil, false, nil
	}
	var v struct {
		Sections []HomeSection `json:"sections"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, false, fmt.Errorf("decode stored home layout: %w", err)
	}
	if v.Sections == nil {
		v.Sections = []HomeSection{}
	}
	for i := range v.Sections {
		if v.Sections[i].Params == nil {
			v.Sections[i].Params = map[string]any{}
		}
	}
	return v.Sections, true, nil
}

// effectiveHome resolves user override, else admin default, else built-in.
// userID nil reads the admin layer alone (GET /v1/admin/home).
func (s *store) effectiveHome(ctx context.Context, userID any) (homeLayout, error) {
	var userRaw, adminRaw []byte
	err := s.pool.QueryRow(ctx, `
		SELECT (SELECT home_layout FROM user_ui_preferences WHERE user_id = $1::uuid),
		       (SELECT home_layout FROM instance_settings WHERE id)
	`, userID).Scan(&userRaw, &adminRaw)
	if err != nil {
		return homeLayout{}, fmt.Errorf("read home layout: %w", err)
	}
	for _, layer := range []struct {
		raw    []byte
		source string
	}{{userRaw, homeSourceUser}, {adminRaw, homeSourceAdmin}} {
		sections, ok, err := decodeStoredHome(layer.raw)
		if err != nil {
			return homeLayout{}, err
		}
		if ok {
			return homeLayout{Source: layer.source, Sections: sections}, nil
		}
	}
	return homeLayout{Source: homeSourceBuiltin, Sections: builtinHome()}, nil
}

// setUserHome stores (sections != nil) or clears (nil) the caller's override.
func (s *store) setUserHome(ctx context.Context, userID string, sections []HomeSection) error {
	if sections == nil {
		_, err := s.pool.Exec(ctx, `UPDATE user_ui_preferences SET home_layout = NULL WHERE user_id = $1::uuid`, userID)
		return err
	}
	buf, err := json.Marshal(map[string]any{"sections": sections})
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO user_ui_preferences (user_id, home_layout) VALUES ($1::uuid, $2::jsonb)
		ON CONFLICT (user_id) DO UPDATE SET home_layout = EXCLUDED.home_layout
	`, userID, buf)
	return err
}

// setAdminHome stores (sections != nil) or clears (nil) the instance default.
func (s *store) setAdminHome(ctx context.Context, adminID string, sections []HomeSection) error {
	if sections == nil {
		_, err := s.pool.Exec(ctx, `UPDATE instance_settings SET home_layout = NULL, updated_by = $1::uuid WHERE id`, adminID)
		return err
	}
	buf, err := json.Marshal(map[string]any{"sections": sections})
	if err != nil {
		return err
	}
	// The singleton is seeded at boot; the insert arm only covers a bare test database.
	_, err = s.pool.Exec(ctx, `
		INSERT INTO instance_settings (id, home_layout, updated_by) VALUES (true, $1::jsonb, $2::uuid)
		ON CONFLICT (id) DO UPDATE SET home_layout = EXCLUDED.home_layout, updated_by = EXCLUDED.updated_by
	`, buf, adminID)
	return err
}

func (h *Handler) writeHome(w http.ResponseWriter, r *http.Request, userID any) {
	layout, err := h.store.effectiveHome(r.Context(), userID)
	if err != nil {
		slog.Error("read home layout", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not read home layout")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, layout)
}

// readHomeBody answers the 400 itself and returns ok=false on any break.
func readHomeBody(w http.ResponseWriter, r *http.Request) ([]HomeSection, bool) {
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(http.MaxBytesReader(w, r.Body, 64<<10)); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "body too large or unreadable")
		return nil, false
	}
	sections, err := parseHomeLayout(buf.Bytes())
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, err.Error())
		return nil, false
	}
	return sections, true
}

func (h *Handler) handleGetMyHome(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFromContext(r.Context())
	h.writeHome(w, r, user.ID)
}

func (h *Handler) handlePutMyHome(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFromContext(r.Context())
	sections, ok := readHomeBody(w, r)
	if !ok {
		return
	}
	h.saveMyHome(w, r, user.ID, sections)
}

func (h *Handler) handleDeleteMyHome(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFromContext(r.Context())
	h.saveMyHome(w, r, user.ID, nil)
}

func (h *Handler) saveMyHome(w http.ResponseWriter, r *http.Request, userID string, sections []HomeSection) {
	if err := h.store.setUserHome(r.Context(), userID, sections); err != nil {
		slog.Error("save home layout", "user_id", userID, "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not save home layout")
		return
	}
	h.writeHome(w, r, userID)
}

func (h *Handler) handleGetAdminHome(w http.ResponseWriter, r *http.Request) {
	h.writeHome(w, r, nil)
}

func (h *Handler) handlePutAdminHome(w http.ResponseWriter, r *http.Request) {
	sections, ok := readHomeBody(w, r)
	if !ok {
		return
	}
	h.saveAdminHome(w, r, sections, "home_layout.set")
}

func (h *Handler) handleDeleteAdminHome(w http.ResponseWriter, r *http.Request) {
	h.saveAdminHome(w, r, nil, "home_layout.cleared")
}

func (h *Handler) saveAdminHome(w http.ResponseWriter, r *http.Request, sections []HomeSection, action string) {
	user, _ := auth.UserFromContext(r.Context())
	if err := h.store.setAdminHome(r.Context(), user.ID, sections); err != nil {
		slog.Error("save admin home layout", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not save home layout")
		return
	}
	h.recordActivity(r, action, "instance", "", nil)
	h.writeHome(w, r, nil)
}
