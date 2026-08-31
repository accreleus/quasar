package session

// stream_profile_codec_change_test.go — the OTHER door onto the H.264 floor rule.
//
// THE INCIDENT THIS FILE EXISTS FOR (2026-07-29, Tower). `stream_profiles.
// 1440p60-h264` had its codec changed h264→hevc through the admin stream-profile
// API. `1440p60` is the default launch profile for most apps and that rung was its
// ONLY rung. Tower's host encoder set is [h264, av1]. resolveRung then walked the
// chain, correctly rejected the rung (`h265!host_encoder`), correctly fired the
// unconditional floor... and dispatched HEVC anyway, because the TERMINAL rung
// bypasses every clamp BY DESIGN and that rung was the terminal rung. Every
// default-profile launch on Tower failed at dispatch.
//
// The floor rule was enforced when editing a launch profile's rung LIST
// (resolveRungWrite) and NOT when editing a rung's CODEC — same chains, other
// door, no error, no warning, nothing surfaced until a launch failed. These tests
// hold that door shut.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// streamProfileBody is the BARE StreamProfile the contract declares for a
// stream-profile write response, plus the warnings[] the codec-change guard adds
// (the same {code, message} shape the launch-profile surface already emits).
type streamProfileBody struct {
	ID       string `json:"id"`
	Codec    string `json:"codec"`
	Width    int32  `json:"width"`
	Warnings []struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"warnings"`
}

func decodeStreamProfile(t *testing.T, resp *http.Response) streamProfileBody {
	t.Helper()
	var body streamProfileBody
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode stream profile: %v", err)
	}
	_ = resp.Body.Close()
	return body
}

// decodeErrorMessage reads the uniform { "error": { code, message } } envelope.
func decodeErrorMessage(t *testing.T, resp *http.Response) (code, message string) {
	t.Helper()
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	_ = resp.Body.Close()
	return body.Error.Code, body.Error.Message
}

func hasStreamWarning(b streamProfileBody, code string) bool {
	for _, w := range b.Warnings {
		if w.Code == code {
			return true
		}
	}
	return false
}

// TestRungCodecChangeRefusedWhenItLeavesAChainWithNoH264 — the core gap. A rung
// that is the ONLY rung of a chain cannot stop being h264: doing so leaves that
// chain with zero launchable h264 rungs, and the terminal-rung floor then
// dispatches a codec the host may not have.
func TestRungCodecChangeRefusedWhenItLeavesAChainWithNoH264(t *testing.T) {
	pool := testDB(t)
	base, adminTok, _ := newProfileAdminServer(t, pool)
	seedChain(t, pool, "solo-chain", []chainRung{
		{id: "solo-h264", codec: "h264", w: 2560, h: 1440, minBW: 11000},
	})

	resp := doJSON(t, "PATCH", base+"/v1/admin/stream-profiles/solo-h264", adminTok,
		map[string]any{"codec": "hevc"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("codec change that strands the only h264 rung = %d, want 409", resp.StatusCode)
	}
	code, msg := decodeErrorMessage(t, resp)
	if code != "conflict" {
		t.Errorf("error code = %q, want conflict", code)
	}
	// NAMING THE CHAIN IS THE POINT. An operator editing a rung sees only the rung;
	// which launch profiles list it is exactly what they cannot know.
	if !strings.Contains(msg, "solo-chain") {
		t.Errorf("the 409 must name the affected launch profile; got %q", msg)
	}

	// And the write must NOT have landed.
	resp = doJSON(t, "GET", base+"/v1/admin/stream-profiles", adminTok, nil)
	var list struct {
		Items []streamProfileBody `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("decode stream profile list: %v", err)
	}
	_ = resp.Body.Close()
	for _, it := range list.Items {
		if it.ID == "solo-h264" && it.Codec != "h264" {
			t.Errorf("refused codec change was persisted anyway: codec = %q, want h264", it.Codec)
		}
	}
}

// TestRungCodecChangeAllowedWhenAnotherH264RungSurvives — the guard rejects a
// BROKEN FLOOR, not a codec change. A chain that still has an h264 rung is fine.
func TestRungCodecChangeAllowedWhenAnotherH264RungSurvives(t *testing.T) {
	pool := testDB(t)
	base, adminTok, _ := newProfileAdminServer(t, pool)
	seedChain(t, pool, "two-h264", []chainRung{
		{id: "two-h264-top", codec: "h264", w: 2560, h: 1440, minBW: 11000},
		{id: "two-h264-floor", codec: "h264", w: 1920, h: 1080, minBW: 8000},
	})

	resp := doJSON(t, "PATCH", base+"/v1/admin/stream-profiles/two-h264-top", adminTok,
		map[string]any{"codec": "av1"})
	if resp.StatusCode != http.StatusOK {
		_, msg := decodeErrorMessage(t, resp)
		t.Fatalf("codec change with a surviving h264 rung = %d (%s), want 200", resp.StatusCode, msg)
	}
	body := decodeStreamProfile(t, resp)
	if body.Codec != "av1" {
		t.Errorf("codec = %q, want av1", body.Codec)
	}
	// h264 is still LAST (the floor rung), so there is nothing to warn about.
	if hasStreamWarning(body, warnH264FloorNotLast) {
		t.Errorf("h264 is still last; got an %s warning anyway: %+v", warnH264FloorNotLast, body.Warnings)
	}
}

// TestRungCodecChangeWarnsWhenH264IsNoLongerLast — the WARN half of the two-part
// rule. "h264 must be last" can never be a rejection (rejecting would make a
// migrated chain whose stored codec order puts h264 first permanently uneditable),
// so a change that leaves h264 present but not last is ACCEPTED and warned about.
func TestRungCodecChangeWarnsWhenH264IsNoLongerLast(t *testing.T) {
	pool := testDB(t)
	base, adminTok, _ := newProfileAdminServer(t, pool)
	seedChain(t, pool, "warn-chain", []chainRung{
		{id: "warn-first-h264", codec: "h264", w: 2560, h: 1440, minBW: 11000},
		{id: "warn-last-h264", codec: "h264", w: 1920, h: 1080, minBW: 8000},
	})

	// Flipping the LAST rung to hevc leaves h264 present (the first rung) but no
	// longer last: everything after it is unreachable, because h264 passes every clamp.
	resp := doJSON(t, "PATCH", base+"/v1/admin/stream-profiles/warn-last-h264", adminTok,
		map[string]any{"codec": "hevc"})
	if resp.StatusCode != http.StatusOK {
		_, msg := decodeErrorMessage(t, resp)
		t.Fatalf("h264-no-longer-last codec change = %d (%s), want 200 — a warning is never a failure",
			resp.StatusCode, msg)
	}
	body := decodeStreamProfile(t, resp)
	if !hasStreamWarning(body, warnH264FloorNotLast) {
		t.Fatalf("expected an %s warning, got %+v", warnH264FloorNotLast, body.Warnings)
	}
	// The warning must name the chain too — same reason the 409 does.
	var found bool
	for _, wn := range body.Warnings {
		if wn.Code == warnH264FloorNotLast && strings.Contains(wn.Message, "warn-chain") {
			found = true
		}
	}
	if !found {
		t.Errorf("the warning must name the affected launch profile; got %+v", body.Warnings)
	}
}

// TestNonCodecRungEditSkipsTheChainGuard — a non-codec field cannot change which
// rung is the H.264 floor, so it must be untouched by the guard. Asserted on a
// rung that is the SOLE rung of its chain, i.e. the exact row whose codec change
// is a 409: if the guard were keyed on "is this rung load-bearing" rather than "is
// the codec changing", this edit would wrongly 409 too.
func TestNonCodecRungEditSkipsTheChainGuard(t *testing.T) {
	pool := testDB(t)
	base, adminTok, _ := newProfileAdminServer(t, pool)
	seedChain(t, pool, "noncodec-chain", []chainRung{
		{id: "noncodec-h264", codec: "h264", w: 2560, h: 1440, minBW: 11000},
	})

	resp := doJSON(t, "PATCH", base+"/v1/admin/stream-profiles/noncodec-h264", adminTok,
		map[string]any{"display_name": "Renamed", "abr_floor_kbps": 5500})
	if resp.StatusCode != http.StatusOK {
		_, msg := decodeErrorMessage(t, resp)
		t.Fatalf("non-codec rung edit = %d (%s), want 200", resp.StatusCode, msg)
	}
	body := decodeStreamProfile(t, resp)
	if body.Codec != "h264" {
		t.Errorf("codec = %q, want h264 (unchanged)", body.Codec)
	}
	if len(body.Warnings) != 0 {
		t.Errorf("a non-codec edit must emit no chain warnings; got %+v", body.Warnings)
	}

	// A codec PATCH that RE-STATES the current codec is not a change either.
	resp = doJSON(t, "PATCH", base+"/v1/admin/stream-profiles/noncodec-h264", adminTok,
		map[string]any{"codec": "h264"})
	if resp.StatusCode != http.StatusOK {
		t.Errorf("re-stating the same codec = %d, want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

// TestRungCodecChangeNamesOnlyTheBrokenChain — a rung is SHARED. One 409 must
// list exactly the chains the change breaks, and not the ones it leaves healthy;
// naming a healthy chain sends the operator to fix the wrong thing.
func TestRungCodecChangeNamesOnlyTheBrokenChain(t *testing.T) {
	pool := testDB(t)
	base, adminTok, _ := newProfileAdminServer(t, pool)
	// The shared rung is the ONLY rung of `fragile-chain`, and one of two in
	// `resilient-chain` (which keeps its own h264 floor).
	seedChain(t, pool, "fragile-chain", []chainRung{
		{id: "shared-h264", codec: "h264", w: 2560, h: 1440, minBW: 11000},
	})
	seedChain(t, pool, "resilient-chain", []chainRung{
		{id: "shared-h264", codec: "h264", w: 2560, h: 1440, minBW: 11000},
		{id: "resilient-floor-h264", codec: "h264", w: 1920, h: 1080, minBW: 8000},
	})

	resp := doJSON(t, "PATCH", base+"/v1/admin/stream-profiles/shared-h264", adminTok,
		map[string]any{"codec": "av1"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("codec change breaking one of two chains = %d, want 409", resp.StatusCode)
	}
	_, msg := decodeErrorMessage(t, resp)
	if !strings.Contains(msg, "fragile-chain") {
		t.Errorf("the 409 must name the broken chain (fragile-chain); got %q", msg)
	}
	if strings.Contains(msg, "resilient-chain") {
		t.Errorf("the 409 must NOT name a chain that keeps its h264 floor; got %q", msg)
	}
}

// TestRegression1440p60RungFlippedToHEVC reproduces the EXACT production
// shape, against the SEEDED ladder rather than a synthetic chain: launch profile
// `1440p60`, whose sole rung is `1440p60-h264`, with that rung's codec flipped
// h264→hevc. This is the write that bricked Tower; it must now be refused.
func TestRegression1440p60RungFlippedToHEVC(t *testing.T) {
	pool := testDB(t)
	base, adminTok, _ := newProfileAdminServer(t, pool)
	store := NewStore(pool)
	ctx := context.Background()

	// Precondition, asserted rather than assumed: migration 0036's fan-out gives
	// 1440p60 exactly one rung, `1440p60-h264`. If that ever changes this test is
	// no longer reproducing the incident and must be rewritten, not deleted.
	lp, err := store.GetLaunchProfile(ctx, "1440p60")
	if err != nil {
		t.Fatalf("read the seeded 1440p60 launch profile: %v", err)
	}
	if len(lp.Rungs) != 1 || lp.Rungs[0].ID != "1440p60-h264" || lp.Rungs[0].Codec != "h264" {
		t.Fatalf("seeded 1440p60 is no longer the incident shape (single rung 1440p60-h264/h264): %+v", lp.Rungs)
	}

	resp := doJSON(t, "PATCH", base+"/v1/admin/stream-profiles/1440p60-h264", adminTok,
		map[string]any{"codec": "hevc"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("the Tower incident write (1440p60-h264 h264→hevc) = %d, want 409 — "+
			"this is the write that made every default-profile launch fail at dispatch", resp.StatusCode)
	}
	_, msg := decodeErrorMessage(t, resp)
	if !strings.Contains(msg, "1440p60") {
		t.Errorf("the 409 must name the 1440p60 launch profile; got %q", msg)
	}

	// The rung is unchanged, so the default launch profile still resolves to h264.
	after, err := store.GetLaunchProfile(ctx, "1440p60")
	if err != nil {
		t.Fatalf("re-read 1440p60: %v", err)
	}
	if after.Rungs[0].Codec != "h264" {
		t.Errorf("1440p60-h264 codec = %q after the refused write, want h264", after.Rungs[0].Codec)
	}
}
