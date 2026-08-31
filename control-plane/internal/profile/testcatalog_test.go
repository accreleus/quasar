package profile

// testcatalog_test.go — the shipped catalog as TEST FIXTURES.
//
// UI-P4 deleted the in-code catalog (it was a live second source of truth read
// at runtime on two paths). The eligibility engine is now pure logic over
// supplied data, so its tests supply the data. This fixture mirrors migration
// 0015's seeded stream_profiles rows fanned out by migration 0036 exactly as the
// fan-out rule specifies: every row has `codecs` NULL, which resolves to the
// in-code default whose only LAUNCHABLE entry is h264, so every launch profile
// gets exactly ONE h264 rung inheriting its parent verbatim.
//
// Keeping the numbers identical to the seed is what lets these tests keep
// asserting the shipped behaviour rather than an invented ladder.

type catalogRow struct {
	id, display string
	w, h, fps   int32
	h264        string
	nominal     int32
	minOffer    int32
	recOffer    int32
	abrFloor    int32
	maxRTT      int32
	minDecode   int32
	highRefresh DisplayReq
	hwEncoder   bool
	browser     BrowserSupport
	playout0    int32
	visibility  Visibility
	sortOrder   int32
}

var seededCatalog = []catalogRow{
	{"4k120", "4K · 120 FPS", 3840, 2160, 120, "high", 75000, 90000, 112500, 20000, 40, 2160, DisplayRequired, true, BrowserRisky, 40, VisibilityUser, 10},
	{"4k60", "4K · 60 FPS", 3840, 2160, 60, "high", 40000, 48000, 60000, 12000, 0, 2160, DisplayNone, true, BrowserRisky, 50, VisibilityUser, 20},
	{"1440p120", "1440p · 120 FPS", 2560, 1440, 120, "high", 35000, 42000, 52500, 10000, 40, 1440, DisplayRequired, true, BrowserRisky, 40, VisibilityUser, 30},
	{"1440p60", "1440p · 60 FPS", 2560, 1440, 60, "high", 20000, 24000, 30000, 6000, 0, 1440, DisplayNone, true, BrowserSupported, 50, VisibilityUser, 40},
	{"1080p120", "1080p · 120 FPS", 1920, 1080, 120, "high", 20000, 24000, 30000, 6000, 40, 1080, DisplayRequired, true, BrowserSupported, 40, VisibilityUser, 50},
	{"1080p60", "1080p · 60 FPS", 1920, 1080, 60, "high", 12000, 14400, 18000, 4000, 0, 1080, DisplayNone, false, BrowserRecommended, 50, VisibilityUser, 60},
	{"720p60", "720p · 60 FPS", 1280, 720, 60, "high", 8000, 9600, 12000, 2500, 0, 720, DisplayNone, false, BrowserRecommended, 75, VisibilityUser, 70},
	{"720p30", "720p · 30 FPS (debug)", 1280, 720, 30, "constrained-baseline", 3000, 0, 4500, 1000, 0, 720, DisplayNone, false, BrowserSupported, 100, VisibilityDebug, 80},
}

// rung builds the single h264 rung migration 0036's fan-out would produce for
// this row: id "<parent>-h264", visibility internal, everything else verbatim.
func (c catalogRow) rung() Profile {
	return Profile{
		ID:                            c.id + "-h264",
		DisplayName:                   "H.264 · " + c.display,
		Codec:                         CodecH264,
		Width:                         c.w,
		Height:                        c.h,
		FPS:                           c.fps,
		H264Profile:                   c.h264,
		NominalBitrateKbps:            c.nominal,
		MinOfferBandwidthKbps:         c.minOffer,
		RecommendedOfferBandwidthKbps: c.recOffer,
		HeadroomFactor:                1.5,
		ABRFloorKbps:                  c.abrFloor,
		MaxStartupRTTMs:               c.maxRTT,
		MinDecodeHeight:               c.minDecode,
		HighRefreshDisplay:            c.highRefresh,
		HardwareEncoderRequired:       c.hwEncoder,
		BrowserClient:                 c.browser,
		Playout0Ms:                    c.playout0,
		Visibility:                    VisibilityInternal,
	}
}

func (c catalogRow) launchProfile() LaunchProfile {
	return LaunchProfile{
		ID:          c.id,
		DisplayName: c.display,
		Visibility:  c.visibility,
		SortOrder:   c.sortOrder,
		Rungs:       []Profile{c.rung()},
	}
}

// testCatalog returns the post-0036 launch-profile ladder, highest quality
// first (sort_order order, which is what the store returns).
func testCatalog() []LaunchProfile {
	out := make([]LaunchProfile, 0, len(seededCatalog))
	for _, c := range seededCatalog {
		out = append(out, c.launchProfile())
	}
	return out
}

// testUserFacing returns only the user-visible launch profiles.
func testUserFacing() []LaunchProfile {
	out := make([]LaunchProfile, 0, len(seededCatalog))
	for _, c := range seededCatalog {
		if c.visibility == VisibilityUser {
			out = append(out, c.launchProfile())
		}
	}
	return out
}

// testRungByID returns the single rung of the named launch profile.
func testRungByID(id string) (Profile, bool) {
	for _, c := range seededCatalog {
		if c.id == id {
			return c.rung(), true
		}
	}
	return Profile{}, false
}
