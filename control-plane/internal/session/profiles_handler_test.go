package session

// AS10-02 HTTP-level tests for GET /v1/me/profiles. These exercise the real auth
// middleware (RequireAuth) and the real probe store, so they are DB-gated
// (go-test-db), mirroring metrics_http_test.go / probe_consumer_test.go.
//
// UI-P4 (BREAKING, amendment B1): the endpoint returns LAUNCH PROFILES. Each has
// a `nominal` block echoing its TOP rung's numbers (advertised, not resolved) and
// a `rungs[]` of full stream profiles with their own verdicts. There is no
// top-level width/height/fps any more, and no top-level `visibility` — a launch
// profile is only ever returned when it IS user-visible, so echoing the field
// said nothing.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// rungByID finds a rung by id anywhere across a profiles response, or nils.
func rungByID(body profilesBody, id string) *struct {
	ID          string `json:"id"`
	Position    int32  `json:"position"`
	Codec       string `json:"codec"`
	Width       int32  `json:"width"`
	Height      int32  `json:"height"`
	Visibility  string `json:"visibility"`
	Eligibility string `json:"eligibility"`
	Reasons     []struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"reasons"`
} {
	for _, p := range body.Profiles {
		for i, r := range p.Rungs {
			if r.ID == id {
				return &p.Rungs[i]
			}
		}
	}
	return nil
}

// launchProfileByID finds a launch profile entry by id, or nils.
func launchProfileByID(body profilesBody, id string) *struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Nominal     struct {
		Width       int32 `json:"width"`
		Height      int32 `json:"height"`
		FPS         int32 `json:"fps"`
		BitrateKbps int32 `json:"bitrate_kbps"`
	} `json:"nominal"`
	Eligibility string `json:"eligibility"`
	Reasons     []struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"reasons"`
	Rungs []struct {
		ID          string `json:"id"`
		Position    int32  `json:"position"`
		Codec       string `json:"codec"`
		Width       int32  `json:"width"`
		Height      int32  `json:"height"`
		Visibility  string `json:"visibility"`
		Eligibility string `json:"eligibility"`
		Reasons     []struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"reasons"`
	} `json:"rungs"`
} {
	for i, p := range body.Profiles {
		if p.ID == id {
			return &body.Profiles[i]
		}
	}
	return nil
}

// hasReasonCode reports whether any reason in rs carries the given code.
func hasReasonCode(rs []struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}, code string) bool {
	for _, r := range rs {
		if r.Code == code {
			return true
		}
	}
	return false
}

// profilesBody mirrors the GET /v1/me/profiles response for decoding in tests.
type profilesBody struct {
	RecommendedID string `json:"recommended_id"`
	Confidence    string `json:"confidence"`
	Notes         []struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"notes"`
	Profiles []struct {
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
		Nominal     struct {
			Width       int32 `json:"width"`
			Height      int32 `json:"height"`
			FPS         int32 `json:"fps"`
			BitrateKbps int32 `json:"bitrate_kbps"`
		} `json:"nominal"`
		Eligibility string `json:"eligibility"`
		Reasons     []struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"reasons"`
		Rungs []struct {
			ID          string `json:"id"`
			Position    int32  `json:"position"`
			Codec       string `json:"codec"`
			Width       int32  `json:"width"`
			Height      int32  `json:"height"`
			Visibility  string `json:"visibility"`
			Eligibility string `json:"eligibility"`
			Reasons     []struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"reasons"`
		} `json:"rungs"`
	} `json:"profiles"`
}

func getProfiles(t *testing.T, url, bearer string) (*http.Response, profilesBody) {
	t.Helper()
	resp := doJSON(t, "GET", url, bearer, nil)
	var body profilesBody
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode profiles body: %v", err)
		}
		_ = resp.Body.Close()
	}
	return resp, body
}

// TestProfilesEndpointRequiresAuth: no bearer token → 401.
func TestProfilesEndpointRequiresAuth(t *testing.T) {
	pool := testDB(t)
	srv, _, _ := newMetricsServer(t, pool)

	resp := doJSON(t, "GET", srv.URL+"/v1/me/profiles", "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated GET /v1/me/profiles = %d, want 401", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

// TestProfilesEndpointNoProbe: an authenticated user with no probe gets the
// conservative default (1080p60), low confidence, a probe_missing note, and one
// verdict per user-facing profile (7), none of them the debug 720p30.
func TestProfilesEndpointNoProbe(t *testing.T) {
	pool := testDB(t)
	srv, authSvc, _ := newMetricsServer(t, pool)
	ctx := context.Background()

	if _, err := authSvc.Register(ctx, "np@test.local", "npuser", "quasar-fixture-pw-08"); err != nil {
		t.Fatalf("register: %v", err)
	}
	tok := loginTok(t, authSvc, "np@test.local", "quasar-fixture-pw-08")

	resp, body := getProfiles(t, srv.URL+"/v1/me/profiles", tok)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/me/profiles = %d, want 200", resp.StatusCode)
	}
	if body.RecommendedID != "1080p60" {
		t.Errorf("recommended_id = %q, want 1080p60 (no-probe default)", body.RecommendedID)
	}
	if body.Confidence != "low" {
		t.Errorf("confidence = %q, want low", body.Confidence)
	}
	// testDB (lifecycle_test.go) rebuilds launch_profiles/rungs from the
	// LEGACY migration-0015 seed on every test's setup, for run-order
	// independence — migration 0046's 12-profile catalog never survives into
	// these DB tests. The legacy baseline is 7 user launch profiles + the
	// debug 720p30, each a single h264 rung.
	if len(body.Profiles) != 7 {
		t.Errorf("got %d profiles, want 7 user-facing (testDB's legacy-derived baseline)", len(body.Profiles))
	}
	var noteMissing bool
	for _, n := range body.Notes {
		if n.Code == "probe_missing" {
			noteMissing = true
		}
	}
	if !noteMissing {
		t.Errorf("expected probe_missing note, got %+v", body.Notes)
	}
	for _, p := range body.Profiles {
		if p.ID == "720p30" {
			t.Errorf("debug launch profile 720p30 leaked into response")
		}
		if p.Eligibility == "" {
			t.Errorf("launch profile %q missing eligibility", p.ID)
		}
		if len(p.Rungs) == 0 {
			t.Errorf("launch profile %q returned no rungs — a picker has nothing to explain with", p.ID)
			continue
		}
		top := p.Rungs[0]
		if top.Position != 1 {
			t.Errorf("launch profile %q first rung position = %d, want 1", p.ID, top.Position)
		}
		// `nominal` echoes the TOP rung, and is advertised rather than resolved.
		if p.Nominal.Width != top.Width || p.Nominal.Height != top.Height {
			t.Errorf("launch profile %q nominal %dx%d != top rung %dx%d",
				p.ID, p.Nominal.Width, p.Nominal.Height, top.Width, top.Height)
		}
		for _, r := range p.Rungs {
			if r.Visibility != "internal" {
				t.Errorf("rung %q visibility = %q, want internal (never offered standalone)", r.ID, r.Visibility)
			}
			if r.Codec == "" {
				t.Errorf("rung %q has no codec — a rung IS a codec", r.ID)
			}
			if r.Eligibility == "" {
				t.Errorf("rung %q missing its own eligibility verdict", r.ID)
			}
			// No probe row at all ⇒ handleListProfiles leaves Probe nil, so
			// Probe.Codecs is never even built (Workstream A, 2026-08-01
			// library-hero-band spec): codec_not_supported must be unreachable
			// here regardless of the rung's own codec.
			if hasReasonCode(r.Reasons, "codec_not_supported") {
				t.Errorf("rung %q got codec_not_supported with no probe at all — Codecs should stay nil (unknown → allow)", r.ID)
			}
		}
	}
}

// TestProfilesEndpointCodecAwareH264Only: an h264-only device probe
// (capabilities.codecs.hevc/av1 both false) makes every av1/hevc rung
// ineligible with codec_not_supported, while h264 rungs are unaffected, and a
// chain whose only launchable rung is the h264 floor rolls up to `risky`
// (launch.go's documented top-ineligible-lower-ok inversion) rather than
// `ineligible` — Workstream A, 2026-08-01 library-hero-band spec.
//
// testDB's baseline (see TestProfilesEndpointNoProbe) has one h264 rung per
// chain, so a real multi-codec chain has to be built explicitly: this uses
// enableChainCodecs (codec_db_test.go), the same helper the multi-codec
// launch-path tests use, to rebuild 1440p60 as av1 > hevc > h264, cloning the
// existing 1440p60-h264 rung's geometry so only the codec varies.
func TestProfilesEndpointCodecAwareH264Only(t *testing.T) {
	pool := testDB(t)
	srv, authSvc, _ := newMetricsServer(t, pool)
	ctx := context.Background()

	u, err := authSvc.Register(ctx, "h264only@test.local", "h264onlyuser", "quasar-fixture-pw-08")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	tok := loginTok(t, authSvc, "h264only@test.local", "quasar-fixture-pw-08")

	enableChainCodecs(t, pool, "1440p60", "av1", "hevc", "h264")
	// Excellent link (bw 50000, rtt 5, decode 2160), h264-only decode.
	upsertCodecProbe(t, pool, u.ID, false, false)

	resp, body := getProfiles(t, srv.URL+"/v1/me/profiles", tok)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/me/profiles = %d, want 200", resp.StatusCode)
	}

	// The rebuilt 1440p60 chain: av1 top rung and hevc second rung both
	// codec-gated ineligible; the h264 floor (position 3 in this chain)
	// unaffected.
	if r := rungByID(body, "1440p60-av1"); r == nil {
		t.Errorf("1440p60-av1 rung missing from response")
	} else {
		if r.Eligibility != "ineligible" {
			t.Errorf("1440p60-av1 eligibility = %q, want ineligible", r.Eligibility)
		}
		if !hasReasonCode(r.Reasons, "codec_not_supported") {
			t.Errorf("1440p60-av1 reasons = %+v, want codec_not_supported", r.Reasons)
		}
	}
	if r := rungByID(body, "1440p60-hevc"); r == nil {
		t.Errorf("1440p60-hevc rung missing from response")
	} else {
		if r.Eligibility != "ineligible" {
			t.Errorf("1440p60-hevc eligibility = %q, want ineligible", r.Eligibility)
		}
		if !hasReasonCode(r.Reasons, "codec_not_supported") {
			t.Errorf("1440p60-hevc reasons = %+v, want codec_not_supported", r.Reasons)
		}
	}
	if r := rungByID(body, "1440p60-h264"); r == nil {
		t.Errorf("1440p60-h264 rung missing from response")
	} else {
		if r.Eligibility != "eligible" {
			t.Errorf("1440p60-h264 eligibility = %q, want eligible (h264 is never codec-gated)", r.Eligibility)
		}
		if hasReasonCode(r.Reasons, "codec_not_supported") {
			t.Errorf("1440p60-h264 got codec_not_supported — h264 rungs must never be codec-gated")
		}
	}

	// Chain rollup: 1440p60's top rung (av1) is ineligible but its h264 floor
	// (position 3) is launchable, so the CHAIN is risky, not ineligible.
	if lp := launchProfileByID(body, "1440p60"); lp == nil {
		t.Errorf("1440p60 missing from response")
	} else if lp.Eligibility != "risky" {
		t.Errorf("1440p60 eligibility = %q, want risky (top rung codec-gated, h264 floor still launchable)", lp.Eligibility)
	}

	// Every OTHER chain in this baseline is a single native h264 rung, so
	// codec gating never touches them; 1440p60 (now risky) must not win the
	// recommendation over them. In sort_order, 4k120/4k60 stay risky
	// (browser_client=risky), 1440p120/1080p120 stay risky (high_refresh
	// required + unmeasured display), so the first fully-eligible chain —
	// and the recommendation — is 1080p60.
	if body.RecommendedID != "1080p60" {
		t.Errorf("recommended_id = %q, want 1080p60 (first single-h264-rung chain with no soft fail, since 1440p60 is now risky)", body.RecommendedID)
	}
	if lp := launchProfileByID(body, "1080p60"); lp == nil {
		t.Errorf("1080p60 missing from response")
	} else if lp.Eligibility != "eligible" {
		t.Errorf("1080p60 eligibility = %q, want eligible (h264-only chain, unaffected by codec gating)", lp.Eligibility)
	}
}

// TestProfilesEndpointCodecAwareHevcAv1Supported: a device probe reporting
// both hevc and av1 decode support sees the rebuilt av1>hevc>h264 1440p60
// chain fully eligible end to end — its av1 top rung is never codec-gated, so
// the chain is `eligible` (not the `risky` rollup) and wins the
// recommendation, exactly as it would have before Probe.Codecs was ever fed
// by handleListProfiles (Workstream A, 2026-08-01 library-hero-band spec).
func TestProfilesEndpointCodecAwareHevcAv1Supported(t *testing.T) {
	pool := testDB(t)
	srv, authSvc, _ := newMetricsServer(t, pool)
	ctx := context.Background()

	u, err := authSvc.Register(ctx, "allcodec@test.local", "allcodecuser", "quasar-fixture-pw-08")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	tok := loginTok(t, authSvc, "allcodec@test.local", "quasar-fixture-pw-08")

	enableChainCodecs(t, pool, "1440p60", "av1", "hevc", "h264")
	upsertCodecProbe(t, pool, u.ID, true, true)

	resp, body := getProfiles(t, srv.URL+"/v1/me/profiles", tok)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/me/profiles = %d, want 200", resp.StatusCode)
	}
	if body.RecommendedID != "1440p60" {
		t.Errorf("recommended_id = %q, want 1440p60 (av1 top rung fully eligible, hevc+av1 both decodable)", body.RecommendedID)
	}
	if body.Confidence != "high" {
		t.Errorf("confidence = %q, want high (fresh probe)", body.Confidence)
	}
	if r := rungByID(body, "1440p60-av1"); r == nil {
		t.Errorf("1440p60-av1 rung missing from response")
	} else if r.Eligibility != "eligible" {
		t.Errorf("1440p60-av1 eligibility = %q, want eligible (av1 decode reported)", r.Eligibility)
	}
	if lp := launchProfileByID(body, "1440p60"); lp == nil {
		t.Errorf("1440p60 missing from response")
	} else if lp.Eligibility != "eligible" {
		t.Errorf("1440p60 eligibility = %q, want eligible (top av1 rung fully eligible)", lp.Eligibility)
	}
	// No rung anywhere should be codec-gated when the device claims both
	// hevc and av1.
	for _, p := range body.Profiles {
		for _, r := range p.Rungs {
			if hasReasonCode(r.Reasons, "codec_not_supported") {
				t.Errorf("rung %q got codec_not_supported with hevc+av1 both reported true", r.ID)
			}
		}
	}
}

// TestProfilesEndpointWithProbe: a fresh excellent probe recommends the highest
// fully-eligible profile (1440p60 — the browser-client cap, since 4K is
// browser-risky and 120 fps needs an unconfirmable high-refresh display) with
// high confidence, emits no probe note, and surfaces 4k120 as a risky option.
//
// testDB's baseline (see TestProfilesEndpointNoProbe) gives every chain here
// exactly one h264 rung, so an h264-only probe (upsertProbe never sets
// capabilities.codecs.{hevc,av1}, which parses as false/false) never actually
// exercises codec gating in this fixture — h264 is never codec-gated. The
// codec_not_supported path (Workstream A, 2026-08-01 library-hero-band spec)
// is covered by TestProfilesEndpointCodecAware*, which build a real
// multi-codec chain via enableChainCodecs before asserting on it.
func TestProfilesEndpointWithProbe(t *testing.T) {
	pool := testDB(t)
	srv, authSvc, _ := newMetricsServer(t, pool)
	ctx := context.Background()

	u, err := authSvc.Register(ctx, "wp@test.local", "wpuser", "quasar-fixture-pw-08")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	tok := loginTok(t, authSvc, "wp@test.local", "quasar-fixture-pw-08")

	// Excellent LAN probe: bw 120000 ≥ 4k120 recommended (112500), rtt 15 ≤ 40,
	// decode 2160 ≥ 2160.
	upsertProbe(t, pool, u.ID, 120000, 15, 2160, time.Now())

	resp, body := getProfiles(t, srv.URL+"/v1/me/profiles", tok)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/me/profiles = %d, want 200", resp.StatusCode)
	}
	if body.RecommendedID != "1440p60" {
		t.Errorf("recommended_id = %q, want 1440p60 (browser-client cap on an excellent link)", body.RecommendedID)
	}
	if body.Confidence != "high" {
		t.Errorf("confidence = %q, want high (fresh probe)", body.Confidence)
	}
	for _, n := range body.Notes {
		if n.Code == "probe_missing" || n.Code == "probe_stale" {
			t.Errorf("unexpected note %q with a fresh probe", n.Code)
		}
	}
	// 4k120 is launchable on this link but surfaced as risky (browser), not recommended.
	var saw4k120 bool
	for _, p := range body.Profiles {
		if p.ID == "4k120" {
			saw4k120 = true
			if p.Eligibility != "risky" {
				t.Errorf("4k120 = %q on an excellent link, want risky (browser)", p.Eligibility)
			}
		}
	}
	if !saw4k120 {
		t.Errorf("4k120 missing from response")
	}
}

// TestProfilesEndpointPoorNetwork: a poor probe makes every profile ineligible
// with bandwidth_too_low; the recommendation falls back to 720p60, low confidence.
func TestProfilesEndpointPoorNetwork(t *testing.T) {
	pool := testDB(t)
	srv, authSvc, _ := newMetricsServer(t, pool)
	ctx := context.Background()

	u, err := authSvc.Register(ctx, "poor@test.local", "pooruser", "quasar-fixture-pw-08")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	tok := loginTok(t, authSvc, "poor@test.local", "quasar-fixture-pw-08")

	// 5000 kbps is below every user-facing profile minimum (720p60 min = 9600).
	upsertProbe(t, pool, u.ID, 5000, 60, 1080, time.Now())

	resp, body := getProfiles(t, srv.URL+"/v1/me/profiles", tok)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/me/profiles = %d, want 200", resp.StatusCode)
	}
	if body.RecommendedID != "720p60" {
		t.Errorf("recommended_id = %q, want 720p60 (best-effort floor)", body.RecommendedID)
	}
	if body.Confidence != "low" {
		t.Errorf("confidence = %q, want low (nothing fully eligible)", body.Confidence)
	}
	for _, p := range body.Profiles {
		if p.Eligibility != "ineligible" {
			t.Errorf("profile %q = %q on a 5000 kbps link, want ineligible", p.ID, p.Eligibility)
		}
	}
}
