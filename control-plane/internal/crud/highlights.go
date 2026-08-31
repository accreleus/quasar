package crud

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/auth"
	"github.com/accreleus/quasar/control-plane/internal/httpx"
)

// GET /v1/me/highlights: the home rail. Server owns the ranking (control-api.md
// §GET /v1/me/highlights) — order and `reason` come from here so a client never
// builds a second ranker off /v1/apps + /v1/sessions. RequireAuth only, no
// user_id param: subject is always the bearer identity, like favourites.go.
//
// Lives in internal/crud, not internal/session, so it can call the original
// entitledSQL (store.go) rather than add a fifth hand-copy (internal/session
// cannot import crud; scheduler.go and session/store.go each already carry one).

const (
	highlightLimit = 5 // contract's maxItems on HighlightList

	// window_days bounds (openapi.yaml: minimum 1, maximum 90, default 7).
	defaultHighlightWindowDays = 7
	minHighlightWindowDays     = 1
	maxHighlightWindowDays     = 90
)

// HighlightReason values (openapi.yaml HighlightReason), ranked in this order:
// live > most played > resume > recently added. One card per app.
const (
	reasonLive          = "live"
	reasonMostPlayed    = "most_played"
	reasonResume        = "resume"
	reasonRecentlyAdded = "recently_added"
)

// liveSessionStatesSQL must mirror web/src/pages/app/libraryGrid.ts LIVE_STATES
// exactly, since the same page renders this rail and the per-tile "Running"
// chip. Narrower than internal/session's activeStatesSQL (reservation concern)
// and devices/store.go's activeSessionStates (quota concern, includes
// 'stopping') — those answer different questions, do not substitute. 'stopping'
// is excluded: a session on its way out shouldn't be offered as "currently
// playing".
const liveSessionStatesSQL = `('pending','assigned','starting','running')`

// sessionClampInterval bounds play_seconds at 24h per session (mandatory, spec
// §2b): ended_at is NULL both for a live session and for one whose host
// vanished and never reconciled, and the latter is otherwise unbounded (one
// orphaned row could report thousands of hours). Applied uniformly, not only to
// NULL-ended rows, so a corrupt/skewed ended_at is bounded too — trading
// under-reporting a genuine >24h session for not trusting timestamps
// selectively. state='failed' rows are excluded before this: a launch that
// never ran is not play time.
const sessionClampInterval = `interval '24 hours'`

// highlight is one rail item (openapi.yaml Highlight). All fields required, so
// the four nullable ones serialize as null, never absent.
type highlight struct {
	AppID            string
	Reason           string
	SessionID        *string
	SessionStartedAt *time.Time
	PlaySeconds      int64
	LastPlayedAt     *time.Time
}

// listHighlights ranks the caller's rail. windowDays bounds only the "most
// played" aggregate; resume is unbounded, recently_added is newest-first.
// Returns at most highlightLimit items, possibly none: an empty rail is a
// normal 200 (control-api.md "Best-effort by contract").
func (s *store) listHighlights(ctx context.Context, callerID string, windowDays int) ([]highlight, error) {
	rows, err := s.pool.Query(ctx, `
		WITH entitled AS (
			-- THE ENTITLEMENT FILTER, and it is a correctness requirement rather
			-- than politeness (control-api.md): a session row OUTLIVES the
			-- entitlement that authorised it, because revoking an entitlement does
			-- not delete history. Rank over sessions without this and a revoked
			-- title reappears on the user's home page with its play time attached.
			--
			-- Same enabled + entitled predicate as GET /v1/apps, using the SAME
			-- entitledSQL helper listApps uses (store.go) — not a hand-rolled second
			-- copy, and with no role arm: an admin here is a user reading their own
			-- library (§6.5).
			SELECT apps.id AS app_id, apps.created_at
			FROM apps
			WHERE apps.enabled = true
			  AND `+entitledSQL("$1")+`
		),
		-- Every session-derived CTE below reads ONLY from mine, which is already
		-- inner-joined to entitled. That join is the single point at which the
		-- filter is applied, so no later branch can forget it.
		mine AS (
			SELECT s.id, s.app_id, s.state, s.started_at, s.ended_at, s.created_at
			FROM sessions s
			JOIN entitled e ON e.app_id = s.app_id
			WHERE s.user_id = $1::uuid
		),
		live AS (
			-- One live session per app (DISTINCT ON), newest first. A user can hold
			-- several concurrent sessions (max_concurrent_sessions), so this is a
			-- real case, not a theoretical one.
			SELECT DISTINCT ON (app_id) app_id, id AS session_id, started_at, created_at
			FROM mine
			WHERE state IN `+liveSessionStatesSQL+`
			ORDER BY app_id, created_at DESC
		),
		played AS (
			-- Windowed play time. See sessionClampInterval for the clamp rule.
			-- The window filters on started_at: a session that began before the
			-- window contributes nothing, rather than contributing a part-session
			-- slice. Simpler, and the aggregate is a kicker, not a billing record.
			SELECT app_id,
			       FLOOR(SUM(GREATEST(
			           EXTRACT(EPOCH FROM (
			               LEAST(COALESCE(ended_at, now()), started_at + `+sessionClampInterval+`)
			               - started_at
			           )),
			           0
			       )))::bigint AS play_seconds
			FROM mine
			WHERE started_at IS NOT NULL
			  AND state <> 'failed'
			  AND started_at >= now() - make_interval(days => $2::int)
			GROUP BY app_id
		),
		last_played AS (
			-- MAX(ended_at) over non-failed sessions, UNBOUNDED by the window
			-- (openapi.yaml Highlight.last_played_at). NULL when the app has never
			-- been played to completion, which is the state of every app for a user
			-- whose only session is the currently-live one.
			SELECT app_id, MAX(ended_at) AS last_played_at
			FROM mine
			WHERE state <> 'failed'
			GROUP BY app_id
		),
		top_played AS (
			-- MOST PLAYED IS A SUPERLATIVE, SO IT CLAIMS EXACTLY ONE CARD: the app
			-- with the highest play time in the window, among those not already
			-- claimed by a live session. Every other played app falls through to
			-- 'resume'.
			--
			-- Labelling three cards "Most played this week" would be self-
			-- contradictory, and it would also starve 'resume' — inside the window
			-- almost every past session has a non-zero duration, so a "play_seconds
			-- > 0" test alone would leave the resume reason effectively dead. The
			-- mockup's rail is one live + one most-played + one resume + one newly
			-- added, which is what this produces.
			--
			-- app_id breaks a tie so the choice is deterministic rather than
			-- whatever order the planner returned.
			SELECT p.app_id, p.play_seconds
			FROM played p
			LEFT JOIN live lv ON lv.app_id = p.app_id
			WHERE lv.app_id IS NULL
			  AND p.play_seconds > 0
			ORDER BY p.play_seconds DESC, p.app_id
			LIMIT 1
		),
		resume AS (
			-- The most recent PAST session per app. Not-live and not-failed: a
			-- launch that never ran is nothing to pick up where you left off.
			-- COALESCE walks back through the timestamps a terminal row may be
			-- missing, which is the same ordering key the client's stopgap rail uses
			-- (web/src/pages/app/homeData.ts).
			SELECT DISTINCT ON (app_id) app_id,
			       COALESCE(ended_at, started_at, created_at) AS resumed_at
			FROM mine
			WHERE state NOT IN `+liveSessionStatesSQL+`
			  AND state <> 'failed'
			ORDER BY app_id, COALESCE(ended_at, started_at, created_at) DESC
		)
		SELECT e.app_id::text,
		       -- ONE CARD PER APP. The CASE is what guarantees it: an app claimed by
		       -- a higher-priority reason cannot fall through to a lower one, so the
		       -- app with both a live session and the week's highest play time
		       -- appears exactly once, as 'live'.
		       CASE
		           WHEN lv.app_id IS NOT NULL THEN '`+reasonLive+`'
		           WHEN tp.app_id IS NOT NULL THEN '`+reasonMostPlayed+`'
		           WHEN rs.app_id IS NOT NULL THEN '`+reasonResume+`'
		           ELSE '`+reasonRecentlyAdded+`'
		       END AS reason,
		       -- session_id / session_started_at come from the live CTE and from
		       -- nowhere else, so they are NULL for every other reason by
		       -- construction rather than by a nulling-out step.
		       lv.session_id::text,
		       lv.started_at,
		       -- play_seconds is reported for EVERY card that has it, not only the
		       -- most_played one: the contract says "0 when not applicable to the
		       -- reason", and it is applicable — a live card's elapsed play time and
		       -- a resume card's hours this week are both real and both renderable.
		       COALESCE(pl.play_seconds, 0),
		       lp.last_played_at
		FROM entitled e
		LEFT JOIN live lv        ON lv.app_id = e.app_id
		LEFT JOIN played pl      ON pl.app_id = e.app_id
		LEFT JOIN top_played tp  ON tp.app_id = e.app_id
		LEFT JOIN last_played lp ON lp.app_id = e.app_id
		LEFT JOIN resume rs      ON rs.app_id = e.app_id
		ORDER BY
		    CASE
		        WHEN lv.app_id IS NOT NULL THEN 0
		        WHEN tp.app_id IS NOT NULL THEN 1
		        WHEN rs.app_id IS NOT NULL THEN 2
		        ELSE 3
		    END,
		    -- Then the within-reason key. ORDER BY is lexicographic, so each key
		    -- below must be CONSTANT across the groups it does not order, or it
		    -- steals the tiebreak from the key that does. (The most_played group
		    -- holds at most one row, so it needs no key of its own.)
		    lv.created_at DESC NULLS LAST,
		    rs.resumed_at DESC NULLS LAST,
		    e.created_at DESC
		LIMIT $3
	`, callerID, windowDays, highlightLimit)
	if err != nil {
		return nil, fmt.Errorf("query highlights: %w", err)
	}
	defer rows.Close()

	var out []highlight
	for rows.Next() {
		var h highlight
		if err := rows.Scan(&h.AppID, &h.Reason, &h.SessionID, &h.SessionStartedAt,
			&h.PlaySeconds, &h.LastPlayedAt); err != nil {
			return nil, fmt.Errorf("scan highlight: %w", err)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return gateRecentlyAdded(out), nil
}

// gateRecentlyAdded drops recently_added items unless the rail already carries
// one derived from the caller's own history: the contract's empty-history rule
// ("items is empty for a user who has never launched anything", control-api.md
// "Best-effort by contract") — otherwise a brand-new user sees five fallback
// cards the contract says must not exist. Safe because recently_added is
// ranked last, so dropped items are always a suffix.
func gateRecentlyAdded(items []highlight) []highlight {
	for _, h := range items {
		if h.Reason != reasonRecentlyAdded {
			return items
		}
	}
	return nil
}

// --- handler ---

// highlightResp is the wire shape (openapi.yaml Highlight). No name/artwork/
// kind/favourite: the client already holds AppListItem and joins on app_id.
type highlightResp struct {
	AppID            string     `json:"app_id"`
	Reason           string     `json:"reason"`
	SessionID        *string    `json:"session_id"`
	SessionStartedAt *time.Time `json:"session_started_at"`
	PlaySeconds      int64      `json:"play_seconds"`
	LastPlayedAt     *time.Time `json:"last_played_at"`
}

// handleHighlights implements GET /v1/me/highlights.
func (h *Handler) handleHighlights(w http.ResponseWriter, r *http.Request) {
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "authentication required")
		return
	}

	// Out-of-range/non-integer window_days is a 400, not a silent clamp, same
	// posture as gpu_index/max_age_s. Absent and empty (`?window_days=`) both
	// take the default.
	windowDays := defaultHighlightWindowDays
	if v := r.URL.Query().Get("window_days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < minHighlightWindowDays || n > maxHighlightWindowDays {
			httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed,
				fmt.Sprintf("window_days must be an integer between %d and %d",
					minHighlightWindowDays, maxHighlightWindowDays))
			return
		}
		windowDays = n
	}

	items, err := h.store.listHighlights(r.Context(), user.ID, windowDays)
	if err != nil {
		slog.Warn("list highlights failed", "user_id", user.ID, "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not list highlights")
		return
	}

	// [] and never null: HighlightList.items is required.
	resp := []highlightResp{}
	for _, it := range items {
		resp = append(resp, highlightResp{
			AppID:            it.AppID,
			Reason:           it.Reason,
			SessionID:        it.SessionID,
			SessionStartedAt: it.SessionStartedAt,
			PlaySeconds:      it.PlaySeconds,
			LastPlayedAt:     it.LastPlayedAt,
		})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": resp})
}
