package library

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// appdetails is the opt-in fifth rung of §8.2's ladder (§8.3): QUASAR_STEAM_APPDETAILS_LOOKUP,
// default off (docs/configuration.md) because it discloses installed appids to Valve.
//
// It only narrows appids the first four rungs would already publish, and never overrides
// them: Decide's `allow`/`ignore` outcomes never reach it (see DecidedByRule, Store.Reconcile).
// A Valve outage or rate-limit fails open to "the denylist alone decided" — the default anyway.

// appDetailsEndpoint is Valve's undocumented, unversioned, rate-limited store endpoint.
const appDetailsEndpoint = "https://store.steampowered.com/api/appdetails"

// AppDetails answers "is this appid a game" plus short_description for a bounded set of appids.
type AppDetails struct {
	client   *http.Client
	endpoint string
	log      *slog.Logger
	enabled  bool
}

// Both fields come from one response; a second call per id to split them would double the
// rate-limited third-party traffic this type exists to bound.
type AppDetail struct {
	IsGame           bool
	ShortDescription string
}

// NewAppDetails: a disabled instance is not nil, it answers "not consulted" for everything,
// so no caller needs a nil check or risks an accidental request.
func NewAppDetails(enabled bool, log *slog.Logger) *AppDetails {
	return &AppDetails{
		client:   &http.Client{Timeout: 10 * time.Second},
		endpoint: appDetailsEndpoint,
		log:      log,
		enabled:  enabled,
	}
}

// Enabled reports whether the operator opted in.
func (a *AppDetails) Enabled() bool { return a != nil && a.enabled }

// maxAppDetailsLookups bounds one reconcile's third-party traffic; the excess is simply
// "not consulted", which the ladder already handles.
const maxAppDetailsLookups = 40

// Fetch does one HTTP call per id. An absent key means "not consulted", never "not a game" /
// "no description" — every failure path (disabled, network error, non-200, rate-limit,
// malformed body, per-sweep cap) omits the key rather than a zero value, so a Valve non-answer
// never gets treated as "not a game" and suppresses something real.
//
// Must be called before the reconcile transaction opens: a third-party call inside a DB
// transaction holds locks across a network timeout.
func (a *AppDetails) Fetch(ctx context.Context, ids []string) map[string]AppDetail {
	if !a.Enabled() || len(ids) == 0 {
		return nil
	}
	if len(ids) > maxAppDetailsLookups {
		ids = ids[:maxAppDetailsLookups]
	}
	out := make(map[string]AppDetail, len(ids))
	for _, id := range ids {
		if ctx.Err() != nil {
			return out
		}
		det, ok := a.fetchOne(ctx, id)
		if ok {
			out[id] = det
		}
	}
	return out
}

// Classify is Fetch narrowed to the IsGame bool, for callers that don't need ShortDescription.
func (a *AppDetails) Classify(ctx context.Context, ids []string) map[string]bool {
	details := a.Fetch(ctx, ids)
	if details == nil {
		return nil
	}
	out := make(map[string]bool, len(details))
	for id, d := range details {
		out[id] = d.IsGame
	}
	return out
}

func (a *AppDetails) fetchOne(ctx context.Context, id string) (AppDetail, bool) {
	// Revalidated here (not just at call sites) because this builds a URL from stored data.
	if !validAppID(id) {
		return AppDetail{}, false
	}
	url := fmt.Sprintf("%s?appids=%s&filters=basic", a.endpoint, id)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return AppDetail{}, false
	}
	resp, err := a.client.Do(req)
	if err != nil {
		a.log.Info("library: appdetails lookup failed", "appid", id, "err", err)
		return AppDetail{}, false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		a.log.Info("library: appdetails lookup non-200", "appid", id, "status", resp.StatusCode)
		return AppDetail{}, false
	}
	// wire shape: {"<appid>": {"success": bool, "data": {"type", "short_description"}}}
	var body map[string]struct {
		Success bool `json:"success"`
		Data    struct {
			Type             string `json:"type"`
			ShortDescription string `json:"short_description"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return AppDetail{}, false
	}
	entry, ok := body[id]
	if !ok || !entry.Success || entry.Data.Type == "" {
		// success=false covers many legitimate appids (delisted, playtests, tools too), not
		// just non-games. Treat as not consulted, never as "not a game" (§8.3).
		return AppDetail{}, false
	}
	return AppDetail{
		IsGame:           strings.EqualFold(entry.Data.Type, "game"),
		ShortDescription: strings.TrimSpace(entry.Data.ShortDescription),
	}, true
}
