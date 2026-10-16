package crud

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/auth"
	"github.com/accreleus/quasar/control-plane/internal/httpx"
)

// GET /v1/me/play-history (control-api.md amendment 38): the caller's own play
// history per app. Same counted-session and clamp rules as highlights' played
// CTE, so the two never disagree about a session; same entitlement filter, since
// a session row outlives the entitlement that authorised it.

type playHistoryItem struct {
	AppID        string    `json:"app_id"`
	LastPlayedAt time.Time `json:"last_played_at"`
	TotalSeconds int64     `json:"total_seconds"`
	Sessions     int64     `json:"sessions"`
}

// listPlayHistory reads sessions through sessions_user_created_idx (user_id
// leads); the subject is always the bearer.
func (s *store) listPlayHistory(ctx context.Context, callerID string) ([]playHistoryItem, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT s.app_id::text,
		       MAX(s.started_at + d.dur),
		       FLOOR(SUM(EXTRACT(EPOCH FROM d.dur)))::bigint,
		       COUNT(*)
		FROM sessions s
		JOIN apps ON apps.id = s.app_id
		CROSS JOIN LATERAL (
			SELECT GREATEST(
			           LEAST(COALESCE(s.ended_at, now()), s.started_at + `+sessionClampInterval+`) - s.started_at,
			           interval '0') AS dur
		) d
		WHERE s.user_id = $1::uuid
		  AND s.started_at IS NOT NULL
		  AND s.state <> 'failed'
		  AND apps.enabled = true
		  AND `+entitledSQL("$1")+`
		GROUP BY s.app_id
		ORDER BY 2 DESC, 1
	`, callerID)
	if err != nil {
		return nil, fmt.Errorf("query play history: %w", err)
	}
	defer rows.Close()
	out := []playHistoryItem{}
	for rows.Next() {
		var it playHistoryItem
		if err := rows.Scan(&it.AppID, &it.LastPlayedAt, &it.TotalSeconds, &it.Sessions); err != nil {
			return nil, fmt.Errorf("scan play history: %w", err)
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

func (h *Handler) handlePlayHistory(w http.ResponseWriter, r *http.Request) {
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "authentication required")
		return
	}
	items, err := h.store.listPlayHistory(r.Context(), user.ID)
	if err != nil {
		slog.Warn("list play history failed", "user_id", user.ID, "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not list play history")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}
