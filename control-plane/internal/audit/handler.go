package audit

import (
	"fmt"
	"net/http"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/httpx"
)

type Handler struct{ store *Store }

func NewHandler(store *Store) *Handler { return &Handler{store: store} }

func (h *Handler) Register(mux httpx.Router, admin func(http.Handler) http.Handler) {
	mux.Handle("GET /v1/admin/activity", admin(http.HandlerFunc(h.list)))
}

// maxFilterLen bounds a filter value (openapi.yaml maxLength). Over-length is a
// 400, never a silent truncation: a truncated substring search returns rows the
// caller did not ask for.
const maxFilterLen = 128

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	var cursor int64
	var limit = 50
	q := r.URL.Query()
	_, _ = fmt.Sscan(q.Get("cursor"), &cursor)
	_, _ = fmt.Sscan(q.Get("limit"), &limit)

	f := ListFilter{
		Action:      q.Get("action"),
		ActorUserID: q.Get("actor_user_id"),
		TargetType:  q.Get("target_type"),
		Q:           q.Get("q"),
	}
	// A SLICE, not a map: range order over a map is randomised, so two identical
	// over-long requests would name different parameters in the 400.
	for _, p := range []struct{ name, value string }{
		{"action", f.Action},
		{"target_type", f.TargetType},
		{"q", f.Q},
	} {
		if len(p.value) > maxFilterLen {
			httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed,
				p.name+" is too long")
			return
		}
	}
	// Validated here, not at the database: an unparseable uuid reaching the
	// `$4::uuid` cast is a 500 for what is a caller mistake.
	if f.ActorUserID != "" && !isUUID(f.ActorUserID) {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed,
			"actor_user_id must be a uuid")
		return
	}
	if v := q.Get("since"); v != "" {
		ts, err := time.Parse(time.RFC3339, v)
		if err != nil {
			httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed,
				"since must be an RFC 3339 timestamp")
			return
		}
		f.Since = &ts
	}

	items, next, err := h.store.List(r.Context(), cursor, limit, f)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not list activity")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": next})
}

// isUUID reports whether s is a canonical 8-4-4-4-12 hex uuid.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}
