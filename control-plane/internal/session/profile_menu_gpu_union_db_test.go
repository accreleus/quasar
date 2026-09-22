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

	url := srv.URL + "/v1/me/profiles?app_id=" + s.appID
	_, body := getProfiles(t, url, tok)
	if av1 := rungByID(body, "1440p60-av1"); av1 == nil || av1.Eligibility != "eligible" {
		t.Fatalf("av1 must be offered from the union (gpu 0 reports it): %+v", av1)
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
