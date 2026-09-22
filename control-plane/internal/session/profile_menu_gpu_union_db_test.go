package session

// DB tests for #306 (#296 amendment 12): the profile menu's host-capability
// union moves from host codec sets to a union of GPU codec sets over the GPUs
// that pass the launch's candidacy without the free-slot term — a busy GPU
// still counts, the readiness gate applies, and a derived tile's union is
// limited to its home host. Require Postgres (make test-db).

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestProfileMenuUnionOverMixedGPUCodecSets: GPU 0 reports av1, GPU 1 on the
// same host does not. The union still offers av1 — a busy or otherwise
// non-preferred GPU is not dropped from the menu, only the free-slot term is.
func TestProfileMenuUnionOverMixedGPUCodecSets(t *testing.T) {
	pool := testDB(t)
	s := seed(t, pool, 4)
	addGPU(t, pool, s.hostID, 1, 4)
	srv, authSvc, _ := newMetricsServer(t, pool)
	ctx := context.Background()

	u, err := authSvc.Register(ctx, "menu-union@test.local", "menu-union", "quasar-fixture-pw-08")
	must(t, err)
	tok := loginTok(t, authSvc, "menu-union@test.local", "quasar-fixture-pw-08")
	enableChainCodecs(t, pool, "1440p60", "av1", "hevc", "h264")
	upsertCodecProbe(t, pool, u.ID, true, true)

	setGPUCodecsRaw(t, pool, s.hostID, 0, `["h264","h265","av1"]`)
	setGPUCodecsRaw(t, pool, s.hostID, 1, `["h264","h265"]`)

	// Reserve every one of gpu 0's slots with a running session: the union is a
	// totals question, like totalsQuery, not an availability one — a fully busy
	// GPU still counts, only the free-slot term is dropped.
	var filler string
	must(t, pool.QueryRow(ctx, `INSERT INTO users (email, username, password_hash)
		VALUES ('menu-union-filler@test.local','menu-union-filler','x') RETURNING id::text`).Scan(&filler))
	must(t, exec(t, pool, `INSERT INTO sessions
		(user_id, app_id, host_id, gpu_id, state, width, height, fps, bitrate_kbps,
		 h264_profile, reserved_vram_mb, reserved_encode_slots)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4::uuid, 'running', 1280, 720, 30, 2000,
		        'constrained-baseline', 0, 4)`, filler, s.appID, s.hostID, s.gpuID))

	url := srv.URL + "/v1/me/profiles?app_id=" + s.appID
	_, body := getProfiles(t, url, tok)
	if av1 := rungByID(body, "1440p60-av1"); av1 == nil || av1.Eligibility != "eligible" {
		t.Fatalf("av1 must be offered from the union (gpu 0 reports it, busy or not): %+v", av1)
	}
}

// TestProfileMenuReadinessBlockedGPUDropsItsCodec: the only GPU offering av1 is
// readiness-blocked, and the other GPU on the host does not report av1 — the
// union must then exclude av1 with host_encoder_not_supported.
func TestProfileMenuReadinessBlockedGPUDropsItsCodec(t *testing.T) {
	pool := testDB(t)
	s := seed(t, pool, 4)
	addGPU(t, pool, s.hostID, 1, 4)
	srv, authSvc, _ := newMetricsServer(t, pool)
	ctx := context.Background()

	u, err := authSvc.Register(ctx, "menu-blocked@test.local", "menu-blocked", "quasar-fixture-pw-08")
	must(t, err)
	tok := loginTok(t, authSvc, "menu-blocked@test.local", "quasar-fixture-pw-08")
	enableChainCodecs(t, pool, "1440p60", "av1", "hevc", "h264")
	upsertCodecProbe(t, pool, u.ID, true, true)

	setGPUCodecsRaw(t, pool, s.hostID, 0, `["h264","h265","av1"]`)
	setGPUCodecsRaw(t, pool, s.hostID, 1, `["h264","h265"]`)

	// Host-level flags stay false; only GPU 0 (the av1 reporter) is blocked. A
	// fresh readiness_reported_at is required — a stale/absent report abstains
	// the gate, same as at launch (readiness_admission_test.go).
	reportReadiness(t, pool, s.hostID, 5, false, false)
	blockGPU(t, pool, s.gpuID, true)

	url := srv.URL + "/v1/me/profiles?app_id=" + s.appID
	_, body := getProfiles(t, url, tok)
	av1 := rungByID(body, "1440p60-av1")
	if av1 == nil || av1.Eligibility != "ineligible" || !hasReasonCode(av1.Reasons, "host_encoder_not_supported") {
		t.Fatalf("av1 must drop once its only reporting GPU is readiness-blocked: %+v", av1)
	}
	if hevc := rungByID(body, "1440p60-hevc"); hevc == nil || hevc.Eligibility != "eligible" {
		t.Fatalf("hevc (offered by the unblocked gpu 1 too) must stay available: %+v", hevc)
	}
}

// TestProfileMenuGPUNullCodecsInheritsHost: a GPU that never reported its own
// codecs inherits the host's set for the purpose of the union.
func TestProfileMenuGPUNullCodecsInheritsHost(t *testing.T) {
	pool := testDB(t)
	s := seed(t, pool, 4)
	srv, authSvc, _ := newMetricsServer(t, pool)
	ctx := context.Background()

	u, err := authSvc.Register(ctx, "menu-inherit@test.local", "menu-inherit", "quasar-fixture-pw-08")
	must(t, err)
	tok := loginTok(t, authSvc, "menu-inherit@test.local", "quasar-fixture-pw-08")
	enableChainCodecs(t, pool, "1440p60", "av1", "hevc", "h264")
	upsertCodecProbe(t, pool, u.ID, true, true)

	// GPU never reported (NULL); the host has.
	setGPUCodecsRaw(t, pool, s.hostID, 0, "")
	setHostCodecs(t, pool, s.hostID, `["h264","h265","av1"]`)

	url := srv.URL + "/v1/me/profiles?app_id=" + s.appID
	_, body := getProfiles(t, url, tok)
	if av1 := rungByID(body, "1440p60-av1"); av1 == nil || av1.Eligibility != "eligible" {
		t.Fatalf("a NULL gpu report must inherit the host's av1: %+v", av1)
	}
}

// TestProfileMenuDerivedTileUnionLimitedToHomeHost: a derived tile's union
// comes only from its pinned home host, even when another online host in the
// fleet advertises a codec the home host does not.
func TestProfileMenuDerivedTileUnionLimitedToHomeHost(t *testing.T) {
	pool := testDB(t)
	s := seed(t, pool, 4)
	parent := seedSteamApp(t, pool, `{"image":"steam:1"}`)
	tile := seedTile(t, pool, parent, "Hades", "1145360")

	// A second, otherwise-eligible host that DOES advertise av1 — must not
	// leak into the tile's union, which is pinned to its home host.
	hostB, _ := addHost(t, pool, "host-2", 4)
	setGPUCodecsRaw(t, pool, hostB, 0, `["h264","h265","av1"]`)
	setGPUCodecsRaw(t, pool, s.hostID, 0, `["h264","h265"]`)

	srv, authSvc, _ := newMetricsServer(t, pool)
	ctx := context.Background()
	u, err := authSvc.Register(ctx, "menu-tile@test.local", "menu-tile", "quasar-fixture-pw-08")
	must(t, err)
	tok := loginTok(t, authSvc, "menu-tile@test.local", "quasar-fixture-pw-08")
	// The home is provisioned for the request's OWN user, not the seed()
	// fixture user — HomeHostForApp resolves the pin from the caller's home.
	provisionHome(t, pool, u.ID, parent, s.hostID)
	enableChainCodecs(t, pool, "1440p60", "av1", "hevc", "h264")
	upsertCodecProbe(t, pool, u.ID, true, true)

	url := srv.URL + "/v1/me/profiles?app_id=" + tile
	_, body := getProfiles(t, url, tok)
	av1 := rungByID(body, "1440p60-av1")
	if av1 == nil || av1.Eligibility != "ineligible" || !hasReasonCode(av1.Reasons, "host_encoder_not_supported") {
		t.Fatalf("the tile's union must stay pinned to its home host (no av1 there): %+v", av1)
	}
	if hevc := rungByID(body, "1440p60-hevc"); hevc == nil || hevc.Eligibility != "eligible" {
		t.Fatalf("hevc (offered by the home host) must stay available: %+v", hevc)
	}
}

// TestProfileMenuNonAdminBodyCarriesOnlyReasons: a codec excluded by host
// capability never names a host or GPU in the response body a non-admin user
// receives — only the stable reason code/message.
func TestProfileMenuNonAdminBodyCarriesOnlyReasons(t *testing.T) {
	pool := testDB(t)
	s := seed(t, pool, 4)
	srv, authSvc, _ := newMetricsServer(t, pool)
	ctx := context.Background()

	u, err := authSvc.Register(ctx, "menu-noadmin@test.local", "menu-noadmin", "quasar-fixture-pw-08")
	must(t, err)
	tok := loginTok(t, authSvc, "menu-noadmin@test.local", "quasar-fixture-pw-08")
	enableChainCodecs(t, pool, "1440p60", "av1", "hevc", "h264")
	upsertCodecProbe(t, pool, u.ID, true, true)
	setGPUCodecsRaw(t, pool, s.hostID, 0, `["h264","h265"]`)

	url := srv.URL + "/v1/me/profiles?app_id=" + s.appID
	resp := doJSON(t, "GET", url, tok, nil)
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/me/profiles = %d, want 200: %s", resp.StatusCode, raw)
	}
	text := string(raw)
	if strings.Contains(text, s.hostID) || strings.Contains(text, s.gpuID) {
		t.Fatalf("non-admin body must never name a host or GPU: %s", text)
	}
	if !strings.Contains(text, "host_encoder_not_supported") {
		t.Fatalf("expected the av1 exclusion reason in the body: %s", text)
	}
}

// TestProfileMenuReadinessHomesTermAppliesOnlyToManagedHomeApps: the readiness
// gate's homes term (readinessGateSQL's `OR h.readiness_block_homes`) only
// engages when the resolved app is managed-home — CreateParams.ManagedHome,
// set from LaunchApp.ManagedHome the same way launcher.go does. A plain app
// must not be gated by a homes-only block.
func TestProfileMenuReadinessHomesTermAppliesOnlyToManagedHomeApps(t *testing.T) {
	pool := testDB(t)
	s := seed(t, pool, 4)
	managedApp := seedSteamApp(t, pool, `{"image":"steam:1"}`)
	setGPUCodecsRaw(t, pool, s.hostID, 0, `["h264","h265","av1"]`)

	// A second, non-av1 host: proves a homes-block excludes av1 specifically
	// rather than emptying the whole union into advisory-unknown.
	hostB, _ := addHost(t, pool, "host-2", 4)
	setGPUCodecsRaw(t, pool, hostB, 0, `["h264","h265"]`)

	// Fresh homes-only block, host-level flag stays false.
	reportReadiness(t, pool, s.hostID, 5, false, true)

	srv, authSvc, _ := newMetricsServer(t, pool)
	ctx := context.Background()
	u, err := authSvc.Register(ctx, "menu-homes@test.local", "menu-homes", "quasar-fixture-pw-08")
	must(t, err)
	tok := loginTok(t, authSvc, "menu-homes@test.local", "quasar-fixture-pw-08")
	enableChainCodecs(t, pool, "1440p60", "av1", "hevc", "h264")
	upsertCodecProbe(t, pool, u.ID, true, true)

	managedURL := srv.URL + "/v1/me/profiles?app_id=" + managedApp
	_, body := getProfiles(t, managedURL, tok)
	if av1 := rungByID(body, "1440p60-av1"); av1 == nil || av1.Eligibility != "ineligible" || !hasReasonCode(av1.Reasons, "host_encoder_not_supported") {
		t.Fatalf("a managed-home app must be gated by readiness_block_homes: %+v", av1)
	}

	plainURL := srv.URL + "/v1/me/profiles?app_id=" + s.appID
	_, body = getProfiles(t, plainURL, tok)
	if av1 := rungByID(body, "1440p60-av1"); av1 == nil || av1.Eligibility != "eligible" {
		t.Fatalf("a plain (non-managed-home) app must ignore readiness_block_homes: %+v", av1)
	}
}

// TestProfileMenuSlotsTermUsesTheAppsOwnEncodeSlots: the slots term is the
// app's own default_encode_slots (as totalsQuery/readinessTotalsQuery use),
// not a bare "> 0" — a GPU whose total sits below the app's ask is excluded
// even though it would satisfy a smaller app.
func TestProfileMenuSlotsTermUsesTheAppsOwnEncodeSlots(t *testing.T) {
	pool := testDB(t)
	s := seed(t, pool, 1) // gpu 0: 1 slot total, below the app's ask
	setGPUCodecsRaw(t, pool, s.hostID, 0, `["h264","h265","av1"]`)

	// A second, adequately-provisioned but non-av1 host: proves the small GPU's
	// exclusion drops av1 specifically rather than emptying the whole union.
	hostB, _ := addHost(t, pool, "host-2", 4)
	setGPUCodecsRaw(t, pool, hostB, 0, `["h264","h265"]`)

	twoSlotApp := insertApp(t, pool, "two-slot-app", 1024, 2)

	srv, authSvc, _ := newMetricsServer(t, pool)
	ctx := context.Background()
	u, err := authSvc.Register(ctx, "menu-slots@test.local", "menu-slots", "quasar-fixture-pw-08")
	must(t, err)
	tok := loginTok(t, authSvc, "menu-slots@test.local", "quasar-fixture-pw-08")
	enableChainCodecs(t, pool, "1440p60", "av1", "hevc", "h264")
	upsertCodecProbe(t, pool, u.ID, true, true)

	url := srv.URL + "/v1/me/profiles?app_id=" + twoSlotApp
	_, body := getProfiles(t, url, tok)
	if av1 := rungByID(body, "1440p60-av1"); av1 == nil || av1.Eligibility != "ineligible" || !hasReasonCode(av1.Reasons, "host_encoder_not_supported") {
		t.Fatalf("a 1-slot GPU must not satisfy a 2-slot app's totals: %+v", av1)
	}
}

// TestProfileMenuStaleReadinessBlockDoesNotHideCodec: past the staleness
// window the gate abstains, same as at launch — a block never turns into a
// standing menu exclusion once its evidence goes stale.
func TestProfileMenuStaleReadinessBlockDoesNotHideCodec(t *testing.T) {
	pool := testDB(t)
	s := seed(t, pool, 4)
	setGPUCodecsRaw(t, pool, s.hostID, 0, `["h264","h265","av1"]`)

	reportReadiness(t, pool, s.hostID, 65, false, false) // past the default 60s window
	blockGPU(t, pool, s.gpuID, true)

	srv, authSvc, _ := newMetricsServer(t, pool)
	ctx := context.Background()
	u, err := authSvc.Register(ctx, "menu-stale@test.local", "menu-stale", "quasar-fixture-pw-08")
	must(t, err)
	tok := loginTok(t, authSvc, "menu-stale@test.local", "quasar-fixture-pw-08")
	enableChainCodecs(t, pool, "1440p60", "av1", "hevc", "h264")
	upsertCodecProbe(t, pool, u.ID, true, true)

	url := srv.URL + "/v1/me/profiles?app_id=" + s.appID
	_, body := getProfiles(t, url, tok)
	if av1 := rungByID(body, "1440p60-av1"); av1 == nil || av1.Eligibility != "eligible" {
		t.Fatalf("a stale readiness block must abstain, not hide the codec: %+v", av1)
	}
}

// TestProfileMenuZeroSlotGPUContributesNothing: a zero-slot GPU fails the
// slots term outright, so it must not leak the host's wider set into the
// union through NULL-codec inheritance.
func TestProfileMenuZeroSlotGPUContributesNothing(t *testing.T) {
	pool := testDB(t)
	s := seed(t, pool, 4)
	setGPUCodecsRaw(t, pool, s.hostID, 0, `["h264","h265"]`)
	setHostCodecs(t, pool, s.hostID, `["h264","h265","av1"]`)
	addGPU(t, pool, s.hostID, 1, 0) // zero slots, codecs never reported (NULL)

	srv, authSvc, _ := newMetricsServer(t, pool)
	ctx := context.Background()
	u, err := authSvc.Register(ctx, "menu-zero-slot@test.local", "menu-zero-slot", "quasar-fixture-pw-08")
	must(t, err)
	tok := loginTok(t, authSvc, "menu-zero-slot@test.local", "quasar-fixture-pw-08")
	enableChainCodecs(t, pool, "1440p60", "av1", "hevc", "h264")
	upsertCodecProbe(t, pool, u.ID, true, true)

	url := srv.URL + "/v1/me/profiles?app_id=" + s.appID
	_, body := getProfiles(t, url, tok)
	if av1 := rungByID(body, "1440p60-av1"); av1 == nil || av1.Eligibility != "ineligible" || !hasReasonCode(av1.Reasons, "host_encoder_not_supported") {
		t.Fatalf("a zero-slot GPU must not contribute the host's av1 via inheritance: %+v", av1)
	}
}
