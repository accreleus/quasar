package session

// codec_decision_db_test.go — UI-P6 integration tests. Require a real Postgres
// (TEST_DATABASE_URL, provided by scripts/dev/dev.sh go-test-db) and migration 0038.
//
// codec_decision_test.go proves the RECORD is built correctly; these prove it
// SURVIVES — that a real launch persists it to sessions.codec_decision and that
// it comes back on the session object an operator actually reads, and that the
// browser-reported codec round-trips through the telemetry path onto the same
// object so the two can be compared.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// readDecision pulls a session's persisted decision back out and decodes it.
func readDecision(t *testing.T, store *Store, sessionID string) codecDecisionDoc {
	t.Helper()
	sess, err := store.Get(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if len(sess.CodecDecision) == 0 {
		t.Fatal("codec_decision is NULL; the launch resolved a rung and must have recorded why")
	}
	var doc codecDecisionDoc
	if err := json.Unmarshal(sess.CodecDecision, &doc); err != nil {
		t.Fatalf("decode codec_decision %s: %v", sess.CodecDecision, err)
	}
	return doc
}

func decisionRung(t *testing.T, doc codecDecisionDoc, rungID string) codecDecisionRung {
	t.Helper()
	for _, c := range doc.Considered {
		if c.RungID == rungID {
			return c
		}
	}
	t.Fatalf("rung %q missing from the persisted record: %+v", rungID, doc.Considered)
	return codecDecisionRung{}
}

// TestCodecDecisionPersistsTheRejectingClamp is the UI-P6 acceptance criterion
// end-to-end: a session that fell back records WHICH clamp rejected the rung it
// would have preferred, and that record survives to a fresh read.
//
// The setup is the same host-encoder clamp TestSessionCodecHostClamp exercises;
// what is asserted here is the diagnosis, not the outcome.
func TestCodecDecisionPersistsTheRejectingClamp(t *testing.T) {
	pool := testDB(t)
	userID, appID, _ := seed1080pApp(t, pool)
	store := NewStore(pool)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	ctx := context.Background()

	enableChainCodecs(t, pool, "1080p60", "hevc", "h264")
	// hosts.codecs left NULL ⇒ the host is h264-only, so the hevc rung dies at
	// clamp 1 even though the device could decode it.
	upsertCodecProbe(t, pool, userID, true, true)

	res, err := coord.LaunchByProfile(ctx, userID, LaunchParams{AppID: appID, ProfileID: "1080p60", IsAdmin: true})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	if res.Session.Codec != "h264" {
		t.Fatalf("session codec: got %q, want h264", res.Session.Codec)
	}

	doc := readDecision(t, store, res.Session.ID)
	if doc.ResultCodec != "h264" || doc.ResultRung != "1080p60-h264" {
		t.Errorf("persisted result = (%q,%q), want (1080p60-h264,h264)", doc.ResultRung, doc.ResultCodec)
	}
	hevc := decisionRung(t, doc, "1080p60-hevc")
	if hevc.RejectedBy == nil || *hevc.RejectedBy != rejectHostEncoder {
		t.Errorf("hevc rung rejected_by = %v, want %q — the operator cannot otherwise tell WHY they got H.264",
			hevc.RejectedBy, rejectHostEncoder)
	}
	sel := decisionRung(t, doc, "1080p60-h264")
	if !sel.Selected || sel.ClampsBypassed || sel.RejectedBy != nil {
		t.Errorf("selected rung recorded wrong: %+v (it won the walk on merit)", sel)
	}
	if doc.Floor {
		t.Error("floor = true; the h264 rung survived the walk, it is not the floor")
	}
	if doc.Override != nil {
		t.Errorf("override = %q, want null", *doc.Override)
	}
}

// TestCodecDecisionPersistsAnOverrideAsAnOverride: a forced codec must not read
// as a rung that won on merit, even though the persisted row's `codec` column is
// identical in both cases.
func TestCodecDecisionPersistsAnOverrideAsAnOverride(t *testing.T) {
	pool := testDB(t)
	userID, appID, hostID := seed1080pApp(t, pool)
	store := NewStore(pool)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	ctx := context.Background()

	setHostCodecs(t, pool, hostID, `["h264","h265"]`)
	enableChainCodecs(t, pool, "1080p60", "hevc", "h264")
	// The device does NOT advertise hevc decode — so without the override, clamp
	// 2/3 would have rejected this rung. That is exactly what makes recording the
	// bypass matter.
	upsertCodecProbe(t, pool, userID, false, false)

	h265 := "h265"
	res, err := coord.LaunchByProfile(ctx, userID, LaunchParams{
		AppID: appID, ProfileID: "1080p60", IsAdmin: true,
		Override: StreamOverride{Codec: &h265},
	})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	if res.Session.Codec != "h265" {
		t.Fatalf("session codec: got %q, want h265", res.Session.Codec)
	}

	doc := readDecision(t, store, res.Session.ID)
	if doc.Override == nil || *doc.Override != "h265" {
		t.Fatalf("persisted override = %v, want h265", doc.Override)
	}
	if doc.Floor {
		t.Error("floor = true; an override is not the floor")
	}
	sel := decisionRung(t, doc, "1080p60-hevc")
	if !sel.Selected || !sel.ClampsBypassed {
		t.Errorf("overridden rung = %+v; want selected AND clamps_bypassed (it skipped clamps 2/3, 4 and 5)", sel)
	}
	if sel.RejectedBy != nil {
		t.Errorf("overridden rung rejected_by = %q; clamp 0 evaluated no clamp on it", *sel.RejectedBy)
	}
}

// TestCodecDecisionIsOnTheSessionResponse: the record has to reach the operator,
// not merely the table. Asserts the serialized session body carries
// `codec_decision` with the walk in it, and `negotiated_codec` present-but-null
// before the client has reported one.
func TestCodecDecisionIsOnTheSessionResponse(t *testing.T) {
	pool := testDB(t)
	userID, appID, _ := seed1080pApp(t, pool)
	store := NewStore(pool)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	ctx := context.Background()

	enableChainCodecs(t, pool, "1080p60", "hevc", "h264")
	upsertCodecProbe(t, pool, userID, true, true)

	res, err := coord.LaunchByProfile(ctx, userID, LaunchParams{AppID: appID, ProfileID: "1080p60", IsAdmin: true})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	sess, err := store.Get(ctx, res.Session.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	body, err := json.Marshal(toSessionResp(sess))
	if err != nil {
		t.Fatalf("marshal session response: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	raw, ok := m["codec_decision"]
	if !ok {
		t.Fatalf("codec_decision missing from the session body: %s", body)
	}
	if string(raw) == "null" {
		t.Fatalf("codec_decision serialized as null despite a resolved rung: %s", body)
	}
	var doc codecDecisionDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode codec_decision: %v", err)
	}
	if len(doc.Considered) == 0 || doc.ResultRung == "" {
		t.Errorf("codec_decision has no walk: %+v", doc)
	}
	// Always serialized, present-but-null — a consumer must not have to
	// distinguish "absent" from "not reported yet".
	if nc, ok := m["negotiated_codec"]; !ok || string(nc) != "null" {
		t.Errorf("negotiated_codec = %s (present=%v), want present-but-null", nc, ok)
	}
}

// TestCodecDecisionNullWhenNoRungChainWasWalked: a session created WITHOUT the
// post-placement rung resolution — the console path (console_launch.go resolves
// no rung at all) and every pre-UI-P6 row — records nothing, rather than an
// empty-but-present document that would read as "we considered nothing and chose
// h264".
//
// Note for anyone extending this: Coordinator.Launch is NOT that case. It looks
// like the legacy path from its signature (no profile id), but it resolves the
// user's default launch profile and therefore DOES walk a chain and DOES record a
// decision. ScheduleAndCreate on its own is the reachable representative of
// "nothing was walked".
func TestCodecDecisionNullWhenNoRungChainWasWalked(t *testing.T) {
	pool := testDB(t)
	s := seed(t, pool, 4)
	store := NewStore(pool)
	ctx := context.Background()

	sess, err := store.ScheduleAndCreate(ctx, launchParams(s))
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	got, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.CodecDecision) != 0 && string(got.CodecDecision) != "null" {
		t.Errorf("codec_decision = %s with no rung walk, want NULL", got.CodecDecision)
	}
	// It still SERIALIZES, as null — present-but-null, so a consumer never has to
	// distinguish "absent" from "not recorded".
	body, _ := json.Marshal(toSessionResp(got))
	var m map[string]json.RawMessage
	_ = json.Unmarshal(body, &m)
	raw, ok := m["codec_decision"]
	if !ok || string(raw) != "null" {
		t.Errorf("session body codec_decision = %s (present=%v), want present-but-null", raw, ok)
	}
}

// TestCodecDecisionRecordedOnTheDefaultProfileLaunch pins the corollary of the
// note above: the no-profile-id entry point still resolves a chain, so it still
// explains itself. A regression here would silently blank the decision for the
// single most common launch shape there is.
func TestCodecDecisionRecordedOnTheDefaultProfileLaunch(t *testing.T) {
	pool := testDB(t)
	userID, appID, _ := seed1080pApp(t, pool)
	store := NewStore(pool)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	ctx := context.Background()

	res, err := coord.Launch(ctx, userID, appID, StreamOverride{})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	doc := readDecision(t, store, res.Session.ID)
	if doc.ResultRung == "" || len(doc.Considered) == 0 {
		t.Errorf("default-profile launch recorded an empty decision: %+v", doc)
	}
}

// TestNegotiatedCodecRoundTripsAndDisagrees is the second UI-P6 acceptance
// criterion: the browser's negotiated codec reaches the server over the existing
// telemetry path, lands on the session, and a DISAGREEMENT with the
// server-resolved codec is visible to an operator rather than only in the client.
func TestNegotiatedCodecRoundTripsAndDisagrees(t *testing.T) {
	pool := testDB(t)
	srv, authSvc, store := newMetricsServer(t, pool)
	ctx := context.Background()
	_ = seed(t, pool, 4)
	owner, err := authSvc.Register(ctx, "owner@test.local", "owner", "quasar-fixture-pw-03")
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}
	s := currentSeed(t, pool)
	sid := sessionForUser(t, store, s, owner.ID)
	ownerTok := loginTok(t, authSvc, "owner@test.local", "quasar-fixture-pw-03")

	// Before any report: null, not a guess.
	sess, err := store.Get(ctx, sid)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if sess.NegotiatedCodec != nil {
		t.Fatalf("negotiated_codec = %q before any telemetry, want nil", *sess.NegotiatedCodec)
	}
	resolved := sess.Codec // 'h264' for this seeded session

	// The browser reports it is decoding H.265 — DISAGREEING with what the server
	// resolved. That is the silent-fallback / mis-negotiated-m-line signal, and it
	// must survive ingest rather than being reconciled away.
	post := func(mime string) {
		t.Helper()
		body := map[string]any{"samples": []map[string]any{{
			"ts_unix_ms":      1735689600000,
			"metrics":         map[string]any{"fps": 59.6},
			"codec_mime_type": mime,
		}}}
		resp := doJSON(t, http.MethodPost, srv.URL+"/v1/sessions/"+sid+"/stats", ownerTok, body)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("stats POST (%s): got %d want 202", mime, resp.StatusCode)
		}
	}
	post("video/H265")

	sess, err = store.Get(ctx, sid)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if sess.NegotiatedCodec == nil {
		t.Fatal("negotiated_codec is still NULL; the browser's codec never reached the session")
	}
	if *sess.NegotiatedCodec != "h265" {
		t.Errorf("negotiated_codec = %q, want h265 (normalised from video/H265)", *sess.NegotiatedCodec)
	}
	if *sess.NegotiatedCodec == resolved {
		t.Errorf("no disagreement recorded: resolved %q == negotiated %q", resolved, *sess.NegotiatedCodec)
	}
	// It is on the session BODY, which is what the admin drill-down reads.
	body, _ := json.Marshal(toSessionResp(sess))
	var m map[string]json.RawMessage
	_ = json.Unmarshal(body, &m)
	if string(m["negotiated_codec"]) != `"h265"` {
		t.Errorf("session body negotiated_codec = %s, want \"h265\"", m["negotiated_codec"])
	}

	// A later, agreeing report replaces it (the value tracks the live stream, it
	// is not write-once).
	post("video/H264")
	sess, _ = store.Get(ctx, sid)
	if sess.NegotiatedCodec == nil || *sess.NegotiatedCodec != "h264" {
		t.Errorf("negotiated_codec = %v after a corrected report, want h264", sess.NegotiatedCodec)
	}

	// Junk is refused rather than stored: this is untrusted client input written
	// to a session row.
	post("video/<script>alert(1)</script>")
	sess, _ = store.Get(ctx, sid)
	if sess.NegotiatedCodec == nil || *sess.NegotiatedCodec != "h264" {
		t.Errorf("negotiated_codec = %v after a junk report, want the last good value h264", sess.NegotiatedCodec)
	}
}

// TestNegotiatedCodecIsOwnerGated: the telemetry write is not a back door. A
// non-owner cannot write another user's session's negotiated codec — the same
// ownership rule the rest of the stats POST enforces, asserted for the NEW
// side-effect specifically, because a 403 that still mutated the row would be
// worse than no gate at all.
func TestNegotiatedCodecIsOwnerGated(t *testing.T) {
	pool := testDB(t)
	srv, authSvc, store := newMetricsServer(t, pool)
	ctx := context.Background()
	_ = seed(t, pool, 4)
	owner, err := authSvc.Register(ctx, "owner@test.local", "owner", "quasar-fixture-pw-03")
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}
	if _, err := authSvc.Register(ctx, "other@test.local", "other", "quasar-fixture-pw-02"); err != nil {
		t.Fatalf("register other: %v", err)
	}
	s := currentSeed(t, pool)
	sid := sessionForUser(t, store, s, owner.ID)
	otherTok := loginTok(t, authSvc, "other@test.local", "quasar-fixture-pw-02")

	body := map[string]any{"samples": []map[string]any{{
		"ts_unix_ms":      1735689600000,
		"metrics":         map[string]any{"fps": 60},
		"codec_mime_type": "video/AV1",
	}}}
	resp := doJSON(t, http.MethodPost, srv.URL+"/v1/sessions/"+sid+"/stats", otherTok, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-owner stats POST: got %d want 403", resp.StatusCode)
	}
	sess, err := store.Get(ctx, sid)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if sess.NegotiatedCodec != nil {
		t.Errorf("negotiated_codec = %q written by a non-owner", *sess.NegotiatedCodec)
	}
}

// TestNegotiatedCodecAdminMetricsReadStaysAdminGated: UI-P6 adds no route, but it
// does add a field an operator reads through the admin surface. Re-assert the
// gate so a future refactor that moves the codec comparison onto the metrics read
// cannot quietly widen it.
func TestNegotiatedCodecAdminMetricsReadStaysAdminGated(t *testing.T) {
	pool := testDB(t)
	srv, authSvc, store := newMetricsServer(t, pool)
	ctx := context.Background()
	_ = seed(t, pool, 4)
	owner, err := authSvc.Register(ctx, "owner@test.local", "owner", "quasar-fixture-pw-03")
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}
	s := currentSeed(t, pool)
	sid := sessionForUser(t, store, s, owner.ID)
	ownerTok := loginTok(t, authSvc, "owner@test.local", "quasar-fixture-pw-03")

	// The session's OWNER is not an admin: the admin read is 403 for them.
	resp := doJSON(t, http.MethodGet, srv.URL+"/v1/admin/sessions/"+sid+"/metrics", ownerTok, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("owner GET admin metrics: got %d want 403", resp.StatusCode)
	}
}

// TestNegotiatedCodecNotWrittenOnStoppedSession pins store.go's
// UpdateSessionNegotiatedCodec terminal-state guard (`AND state NOT IN
// ('stopped','failed')`), which had no test before this: a late browser
// telemetry flush arriving after the session already ended must not rewrite the
// finished session's history. handlePostStats itself has no state check — "Accepting
// telemetry never affects state" — so without the store-level guard this write
// would go through.
//
// This is the real race the guard exists for: the browser posts its last stats
// batch, including a getStats()-derived codec, after the session has already
// transitioned to stopped server-side (agent teardown, host failure, admin
// stop, ...). The POST must still succeed (telemetry ingest is best-effort and
// decoupled from session lifecycle) but the codec it carries must not land on a
// row that is now the historical record of what happened.
func TestNegotiatedCodecNotWrittenOnStoppedSession(t *testing.T) {
	pool := testDB(t)
	srv, authSvc, store := newMetricsServer(t, pool)
	ctx := context.Background()
	_ = seed(t, pool, 4)
	owner, err := authSvc.Register(ctx, "owner@test.local", "owner", "quasar-fixture-pw-03")
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}
	s := currentSeed(t, pool)
	sid := sessionForUser(t, store, s, owner.ID)
	ownerTok := loginTok(t, authSvc, "owner@test.local", "quasar-fixture-pw-03")

	// The session ends before the browser's late telemetry arrives — mirrors
	// codec_db_test.go's identical raw-SQL pattern for forcing a session terminal
	// in test setup.
	if _, err := pool.Exec(ctx, `UPDATE sessions SET state = 'stopped' WHERE id::text = $1`, sid); err != nil {
		t.Fatalf("stop session: %v", err)
	}

	body := map[string]any{"samples": []map[string]any{{
		"ts_unix_ms":      1735689600000,
		"metrics":         map[string]any{"fps": 60},
		"codec_mime_type": "video/H265",
	}}}
	resp := doJSON(t, http.MethodPost, srv.URL+"/v1/sessions/"+sid+"/stats", ownerTok, body)
	defer resp.Body.Close()
	// The POST itself must still succeed: telemetry ingest never fails because
	// the session already ended.
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("stats POST on a stopped session: got %d want 202", resp.StatusCode)
	}

	sess, err := store.Get(ctx, sid)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if sess.NegotiatedCodec != nil {
		t.Errorf("negotiated_codec = %q after a stopped session received a differing report, want unchanged (nil) — the terminal-state guard should have no-opped the write",
			*sess.NegotiatedCodec)
	}
}
