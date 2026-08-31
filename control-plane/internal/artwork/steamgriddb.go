package artwork

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// SteamGridDBBaseURL is the public API root. Overridable on the client so tests
// can point at a stub server — no test in this package ever reaches the real
// service.
const SteamGridDBBaseURL = "https://www.steamgriddb.com/api/v2"

// SteamGridDB publishes no numeric rate limit (docs/configuration.md), so be
// conservative by construction rather than discover the limit by tripping it —
// resolution is a background sweep with no user waiting.
const sgdbMinInterval = 500 * time.Millisecond

// SteamGridDBClient is Provider implementation #1. Crop selection: tile ← a
// portrait `grid` at 600x900 / 660x930 / 342x482 (2:3 or close under
// `object-fit: cover`; wide and square grid dimensions are never requested —
// they would crop to ribbons in the 2:3 frame, web/src/styles/library.css);
// hero ← a `hero` at 1920x620 / 3840x1240 / 1600x650 (~3:1), a genuinely
// different source asset. The portrait tile is an operator-directed deviation
// from the 16:10 mockup (#385); design_handoff_quasar/ was updated to match.
type SteamGridDBClient struct {
	apiKey  string
	baseURL string
	http    *http.Client

	mu       sync.Mutex
	lastCall time.Time
}

// NewSteamGridDBClient builds the provider. httpClient is injected so tests
// supply a stub transport; pass nil for a sane default.
func NewSteamGridDBClient(apiKey, baseURL string, httpClient *http.Client) *SteamGridDBClient {
	if baseURL == "" {
		baseURL = SteamGridDBBaseURL
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 20 * time.Second}
	}
	return &SteamGridDBClient{
		apiKey:  apiKey,
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    httpClient,
	}
}

func (c *SteamGridDBClient) Name() string { return "steamgriddb" }

// sgdbEnvelope is SteamGridDB's uniform response wrapper.
type sgdbEnvelope[T any] struct {
	Success bool     `json:"success"`
	Data    T        `json:"data"`
	Errors  []string `json:"errors"`
}

type sgdbGame struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

type sgdbImage struct {
	ID     int    `json:"id"`
	Score  int    `json:"score"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	NSFW   bool   `json:"nsfw"`
	Humor  bool   `json:"humor"`
	Mime   string `json:"mime"`
	URL    string `json:"url"`
	Thumb  string `json:"thumb"`
	Author struct {
		Name string `json:"name"`
	} `json:"author"`
}

// Search resolves a title to candidate games. An empty result is not an error.
func (c *SteamGridDBClient) Search(ctx context.Context, query string) ([]Candidate, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	var env sgdbEnvelope[[]sgdbGame]
	path := "/search/autocomplete/" + url.PathEscape(query)
	// A search 404 is "no such title" — the same answer as an empty list, so
	// ErrArtNotFound is absorbed; the sentinel exists for ArtByExternalRef.
	if err := c.do(ctx, path, &env); err != nil && !errors.Is(err, ErrArtNotFound) {
		return nil, err
	}
	out := make([]Candidate, 0, len(env.Data))
	for _, g := range env.Data {
		out = append(out, Candidate{
			Ref:         fmt.Sprintf("%d", g.ID),
			Name:        g.Name,
			Attribution: sgdbAttribution,
		})
	}
	return out, nil
}

// Art resolves the two crops for a SteamGridDB game id (a ref from Search).
// A double 404 reports an empty Candidate with a nil error — ApplyCandidate's
// "no usable artwork" 400 depends on that; only ArtByExternalRef propagates
// ErrArtNotFound, because only its caller has a different branch to take.
func (c *SteamGridDBClient) Art(ctx context.Context, ref string) (Candidate, error) {
	if strings.TrimSpace(ref) == "" {
		return Candidate{}, fmt.Errorf("artwork: empty provider ref")
	}
	art, err := c.crops(ctx, ref, "game/"+url.PathEscape(ref))
	if errors.Is(err, ErrArtNotFound) {
		return Candidate{Ref: ref, Attribution: sgdbAttribution}, nil
	}
	return art, err
}

// ArtByExternalRef resolves the two crops straight from a Steam appid
// (`/grids/steam/<appid>` + `/heroes/steam/<appid>`) — no Search, no title
// matching. Crop selection is the same code as the by-title path by
// construction, so the two cannot drift. The returned Candidate carries the
// appid as its Ref: the appid is what identified the art, and the SteamGridDB
// game id is a fact this call never learned.
func (c *SteamGridDBClient) ArtByExternalRef(ctx context.Context, source, id string) (Candidate, error) {
	if source != ExternalSourceSteam {
		// Never a silent miss — that would cache as "this app has no art".
		return Candidate{}, fmt.Errorf("%w: steamgriddb cannot resolve %q references", ErrUnsupportedExternalSource, source)
	}
	id = strings.TrimSpace(id)
	if !steamAppIDPattern.MatchString(id) {
		// Same grammar as apps_external_id_ck; re-checked because this is the
		// last point before the value joins an outbound URL path.
		return Candidate{}, fmt.Errorf("artwork: %q is not a steam appid", id)
	}
	return c.crops(ctx, id, "steam/"+url.PathEscape(id))
}

// steamAppIDPattern is the appid grammar shared with migration 0042's
// apps_external_id_ck and crud's write gate: a bare positive integer.
var steamAppIDPattern = regexp.MustCompile(`^[1-9][0-9]{0,9}$`)

// crops resolves the tile and hero assets for one selector ("game/<sgdb id>"
// or "steam/<appid>") — the shared body of Art and ArtByExternalRef. A missing
// crop is left empty rather than failing the resolution (half the art beats
// none); only when both endpoints 404 does it report ErrArtNotFound.
func (c *SteamGridDBClient) crops(ctx context.Context, ref, selector string) (Candidate, error) {
	out := Candidate{Ref: ref, Attribution: sgdbAttribution}

	// Portrait grid for the 2:3 tile, most-common dimension first.
	var grids sgdbEnvelope[[]sgdbImage]
	gridPath := "/grids/" + selector +
		"?dimensions=600x900,660x930,342x482&types=static&mimes=image/png,image/jpeg,image/webp&nsfw=false&humor=false"
	gridErr := c.do(ctx, gridPath, &grids)
	if gridErr != nil && !errors.Is(gridErr, ErrArtNotFound) {
		return Candidate{}, gridErr
	}
	if best, ok := pickBest(grids.Data, TileAspect); ok {
		out.TileURL = best.URL
		out.ThumbURL = best.Thumb
	}

	// Separate, much wider hero asset — NOT the grid rescaled.
	var heroes sgdbEnvelope[[]sgdbImage]
	heroPath := "/heroes/" + selector +
		"?dimensions=1920x620,3840x1240,1600x650&types=static&mimes=image/png,image/jpeg,image/webp&nsfw=false&humor=false"
	heroErr := c.do(ctx, heroPath, &heroes)
	if heroErr != nil && !errors.Is(heroErr, ErrArtNotFound) {
		return Candidate{}, heroErr
	}
	if best, ok := pickBest(heroes.Data, HeroAspect); ok {
		out.HeroURL = best.URL
		if out.ThumbURL == "" {
			out.ThumbURL = best.Thumb
		}
	}

	if errors.Is(gridErr, ErrArtNotFound) && errors.Is(heroErr, ErrArtNotFound) {
		return Candidate{}, ErrArtNotFound
	}
	return out, nil
}

// Target aspect ratios (width / height) for the two crops, as the CSS frames
// that render them define. TileAspect is the library tile's `aspect-ratio: 2/3`
// (600x900); HeroAspect is the hero/detail banner (1920x620 ≈ 3.10:1).
const (
	TileAspect = 600.0 / 900.0
	HeroAspect = 1920.0 / 620.0
)

// How far (as a fraction of the target) an asset's aspect may stray and still
// count as on target. 0.25 puts every dimension the two queries ask for in the
// same bucket while nothing with the wrong shape ever is (tile accepts
// 0.500–0.833, rejecting square and wide grids; hero accepts 2.323–3.871,
// holding all three requested hero dimensions so hero order is unchanged).
const aspectTolerance = 0.25

// onTargetAspect reports whether an asset's shape is close enough to target.
// An asset with no usable dimensions is NOT on target: unknown shape is not
// evidence of a good one, and it merely sorts below the assets we can vouch for.
func onTargetAspect(im sgdbImage, target float64) bool {
	if im.Width <= 0 || im.Height <= 0 || target <= 0 {
		return false
	}
	aspect := float64(im.Width) / float64(im.Height)
	return math.Abs(aspect-target)/target <= aspectTolerance
}

// pickBest chooses the highest-scored asset, ties broken on the larger image.
// NSFW/humor are filtered at the query AND here — a request parameter is not
// an assertion about what came back. Aspect is a preference, never a
// rejection: on-target shapes sort first (the query's `dimensions=` filter is
// unverified, and a dishonoured one would land landscape art in a 2:3 frame),
// but a hard reject could return no art for a title that has some, and a
// slightly-off crop under `object-fit: cover` beats the gradient.
func pickBest(images []sgdbImage, targetAspect float64) (sgdbImage, bool) {
	clean := make([]sgdbImage, 0, len(images))
	for _, im := range images {
		if im.NSFW || im.Humor || strings.TrimSpace(im.URL) == "" {
			continue
		}
		clean = append(clean, im)
	}
	if len(clean) == 0 {
		return sgdbImage{}, false
	}
	sort.SliceStable(clean, func(i, j int) bool {
		ai, aj := onTargetAspect(clean[i], targetAspect), onTargetAspect(clean[j], targetAspect)
		if ai != aj {
			return ai
		}
		if clean[i].Score != clean[j].Score {
			return clean[i].Score > clean[j].Score
		}
		return clean[i].Width*clean[i].Height > clean[j].Width*clean[j].Height
	})
	return clean[0], true
}

// do issues one throttled, authenticated GET and decodes the envelope. A 404
// is ErrArtNotFound, never swallowed — the by-appid resolver must tell "not in
// the database" from "has art but none fits the frame"; callers that do not
// care (Search, Art) absorb the sentinel explicitly.
func (c *SteamGridDBClient) do(ctx context.Context, path string, out any) error {
	c.throttle(ctx)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("artwork: steamgriddb request: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		// A normal miss; out stays zero-valued for callers that absorb it.
		return ErrArtNotFound
	case resp.StatusCode == http.StatusTooManyRequests:
		// Named so an operator sees a rate limit, not a generic outage.
		return fmt.Errorf("artwork: steamgriddb rate limited (429)")
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("artwork: steamgriddb rejected the API key (%d)", resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("artwork: steamgriddb status %d", resp.StatusCode)
	}
	// A provider response is untrusted input; an unbounded json.Decode on a
	// hostile body is a memory DoS.
	body, err := copyLimited(resp.Body, 4<<20)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("artwork: steamgriddb decode: %w", err)
	}
	return nil
}

// throttle spaces calls at least sgdbMinInterval apart.
func (c *SteamGridDBClient) throttle(ctx context.Context) {
	c.mu.Lock()
	wait := sgdbMinInterval - time.Since(c.lastCall)
	c.lastCall = time.Now().Add(max(wait, 0))
	c.mu.Unlock()
	if wait <= 0 {
		return
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

// sgdbAttribution is the credit line stored with provider-sourced art — not
// compliance (the terms require none), honesty about where art came from. The
// site terms restrict use to personal, non-commercial purposes, which is why
// the feature ships off and opt-in; docs/configuration.md has the terms an
// operator must read before setting a key.
const sgdbAttribution = "Artwork via SteamGridDB"
