package session

// DB tests for the codec constraint (#304): an explicit codec is a placement
// gate. Require Postgres (make test-db).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/profile"
	"github.com/jackc/pgx/v5/pgxpool"
)

// seedAV1OnGPU0 is the spec's two-GPU host: GPU 0 (one slot) encodes
// h264/h265/av1, GPU 1 (four slots) h264/h265. Spread alone prefers GPU 1.
func seedAV1OnGPU0(t *testing.T, pool *pgxpool.Pool) (s seedIDs, gpu1ID string) {
	t.Helper()
	s = seed(t, pool, 1)
	gpu1ID = seedSecondGPU(t, pool, s.hostID, 16384, 4)
	setGPUCodecsRaw(t, pool, s.hostID, 0, `["h264","h265","av1"]`)
	setGPUCodecsRaw(t, pool, s.hostID, 1, `["h264","h265"]`)
	setHostCodecsRaw(t, pool, s.hostID, `["h264","h265","av1"]`)
	setQuota(t, pool, s.userID, 20)
	return s, gpu1ID
}

func constrainedTo(s seedIDs, codec string) CreateParams {
	p := launchParams(s)
	p.RequireCodec = codec
	return p
}

func releaseSession(t *testing.T, pool *pgxpool.Pool, id string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE sessions SET state = 'stopped', ended_at = now() WHERE id::text = $1`, id); err != nil {
		t.Fatalf("release session: %v", err)
	}
}

func sessionsOnGPU(t *testing.T, pool *pgxpool.Pool, gpuID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM sessions WHERE gpu_id::text = $1`, gpuID).Scan(&n); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	return n
}

func TestExplicitAV1LandsOnTheOnlyAV1GPU(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s, gpu1ID := seedAV1OnGPU0(t, pool)
	ctx := context.Background()

	auto, err := store.ScheduleAndCreate(ctx, launchParams(s))
	if err != nil {
		t.Fatalf("unconstrained launch: %v", err)
	}
	if auto.GPUID == nil || *auto.GPUID != gpu1ID {
		t.Fatalf("fixture: spread should prefer GPU 1, got %v", auto.GPUID)
	}
	releaseSession(t, pool, auto.ID)

	av1, err := store.ScheduleAndCreate(ctx, constrainedTo(s, "av1"))
	if err != nil {
		t.Fatalf("explicit av1: %v", err)
	}
	if av1.GPUID == nil || *av1.GPUID != s.gpuID || *av1.GPUIndex != 0 {
		t.Fatalf("explicit av1 placed on %v (index %v), want GPU 0", av1.GPUID, av1.GPUIndex)
	}
}

func TestExplicitAV1WithItsGPUFullIsCapacityExhausted(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s, gpu1ID := seedAV1OnGPU0(t, pool)
	ctx := context.Background()

	first, err := store.ScheduleAndCreate(ctx, constrainedTo(s, "av1"))
	if err != nil {
		t.Fatalf("first av1: %v", err)
	}

	_, err = store.ScheduleAndCreate(ctx, constrainedTo(s, "av1"))
	if !errors.Is(err, ErrCapacityExhausted) {
		t.Fatalf("av1 with GPU 0 full: got %v, want ErrCapacityExhausted (GPU 1 is free but cannot encode av1)", err)
	}
	if got := constrainedCodec(err); got != "av1" {
		t.Errorf("refusal names codec %q, want av1", got)
	}
	if n := sessionsOnGPU(t, pool, gpu1ID); n != 0 {
		t.Fatalf("GPU 1 holds %d session(s); an av1 launch must never land on a GPU that cannot encode it", n)
	}

	// The same launch starts on GPU 0 once its slot frees.
	releaseSession(t, pool, first.ID)
	again, err := store.ScheduleAndCreate(ctx, constrainedTo(s, "av1"))
	if err != nil {
		t.Fatalf("av1 after the slot frees: %v", err)
	}
	if again.GPUID == nil || *again.GPUID != s.gpuID {
		t.Fatalf("av1 after the slot frees placed on %v, want GPU 0", again.GPUID)
	}
}

func TestExplicitAV1OnAFleetWithNoAV1GPUIsNoHostAvailable(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s, _ := seedAV1OnGPU0(t, pool)
	ctx := context.Background()

	setGPUCodecsRaw(t, pool, s.hostID, 0, `["h264","h265"]`)
	setHostCodecsRaw(t, pool, s.hostID, `["h264","h265"]`)

	_, err := store.ScheduleAndCreate(ctx, constrainedTo(s, "av1"))
	if !errors.Is(err, ErrNoHostAvailable) {
		t.Fatalf("av1 with no av1 GPU: got %v, want ErrNoHostAvailable", err)
	}
	if got := constrainedCodec(err); got != "av1" {
		t.Errorf("refusal names codec %q, want av1", got)
	}
	var nh *NoHostRejection
	if !errors.As(err, &nh) {
		t.Errorf("the fleet counts for the launcher's log were lost: %v", err)
	}

	// A GPU that never reported inherits its host's set, so the host set decides.
	setGPUCodecsRaw(t, pool, s.hostID, 0, "")
	setGPUCodecsRaw(t, pool, s.hostID, 1, "")
	if _, err := store.ScheduleAndCreate(ctx, constrainedTo(s, "av1")); !errors.Is(err, ErrNoHostAvailable) {
		t.Fatalf("av1 on GPUs inheriting an av1-less host: got %v, want ErrNoHostAvailable", err)
	}

	// Neither the codec-less fleet nor the gate touches an unconstrained launch.
	if _, err := store.ScheduleAndCreate(ctx, launchParams(s)); err != nil {
		t.Fatalf("unconstrained launch: %v", err)
	}
	if _, err := store.ScheduleAndCreate(ctx, constrainedTo(s, "h265")); err != nil {
		t.Fatalf("h265, which every GPU encodes: %v", err)
	}
}

// TestCodecConstraintInTheDiagnostics: a diagnostic that ignored the gate would
// blame the veto or readiness for a GPU the launch could never have used.
func TestCodecConstraintInTheDiagnostics(t *testing.T) {
	t.Run("the veto diagnosis lists only capable GPUs", func(t *testing.T) {
		pool := testDB(t)
		store := vetoStore(pool)
		s, gpu1ID := seedAV1OnGPU0(t, pool)
		ctx := context.Background()
		noRetries := watchAttempts(t)

		// Both GPUs are out of memory, so both would be listed without the gate.
		sampleVram(t, pool, s.gpuID, 16000, 100, 0)
		sampleVram(t, pool, gpu1ID, 16000, 100, 0)

		_, err := store.ScheduleAndCreate(ctx, constrainedTo(s, "av1"))
		var vr *VramVetoRejection
		if !errors.As(err, &vr) || constrainedCodec(err) != "av1" {
			t.Fatalf("got %v, want a veto rejection naming av1", err)
		}
		if len(vr.Candidates) != 1 || vr.Candidates[0].GPUID != s.gpuID {
			t.Fatalf("veto diagnosis listed %+v, want only GPU 0", vr.Candidates)
		}
		noRetries("vetoed av1 launch")
	})

	t.Run("readiness is the sole reason among capable GPUs", func(t *testing.T) {
		pool := testDB(t)
		store := NewStore(pool)
		s, gpu1ID := seedAV1OnGPU0(t, pool)
		ctx := context.Background()
		noRetries := watchAttempts(t)

		// GPU 1 is ready and free but cannot encode av1, so it does not make
		// this a capacity problem.
		reportReadiness(t, pool, s.hostID, 5, false, false)
		blockGPU(t, pool, s.gpuID, true)

		_, err := store.ScheduleAndCreate(ctx, constrainedTo(s, "av1"))
		var nr *HostNotReadyRejection
		if !errors.As(err, &nr) {
			t.Fatalf("av1 with its only GPU blocked: got %v, want ErrHostNotReady", err)
		}
		if constrainedCodec(err) != "" {
			t.Error("a readiness refusal must name nothing, the codec included")
		}
		if len(nr.Candidates) != 1 || nr.Candidates[0].GPUID != s.gpuID {
			t.Fatalf("readiness diagnosis listed %+v, want only GPU 0", nr.Candidates)
		}
		if n := sessionsOnGPU(t, pool, gpu1ID); n != 0 {
			t.Fatalf("GPU 1 holds %d session(s)", n)
		}
		noRetries("readiness-blocked av1 launch")
	})
}

// TestCertBenchLandsOnItsPinnedGPU: the cert row is keyed on gpu_index, so the
// bench session must run on that GPU even where spread would choose another.
func TestCertBenchLandsOnItsPinnedGPU(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s, gpu1ID := seedAV1OnGPU0(t, pool)
	ctx := context.Background()

	tok := signalingToken{Hash: "", ExpiresAt: time.Now().Add(time.Minute)}
	h264 := CertTarget{Rung: profile.Profile{ID: "r-h264", Codec: profile.CodecH264, Width: 1280, Height: 720, FPS: 60}}
	av1 := CertTarget{Rung: profile.Profile{ID: "r-av1", Codec: profile.CodecAV1, Width: 1280, Height: 720, FPS: 60}}

	// Spread picks GPU 1 (four free slots), so pinning GPU 0 is observable.
	sess, err := store.ScheduleAndCreate(ctx, certCellParams(s.userID, s.appID, s.hostID, 0, h264, "h264", 5000, tok))
	if err != nil {
		t.Fatalf("bench cell on GPU 0: %v", err)
	}
	if sess.GPUID == nil || *sess.GPUID != s.gpuID {
		t.Fatalf("bench cell pinned to GPU 0 ran on %v", sess.GPUID)
	}
	releaseSession(t, pool, sess.ID)

	// GPU 1 cannot encode av1: its cell is refused, never moved to GPU 0.
	_, err = store.ScheduleAndCreate(ctx, certCellParams(s.userID, s.appID, s.hostID, 1, av1, "av1", 5000, tok))
	if !errors.Is(err, ErrNoHostAvailable) && !errors.Is(err, ErrCapacityExhausted) {
		t.Fatalf("av1 bench cell pinned to GPU 1: got %v, want a placement refusal", err)
	}
	if n := sessionsOnGPU(t, pool, s.gpuID); n != 1 {
		t.Fatalf("GPU 0 holds %d session rows, want only the released h264 cell", n)
	}

	sess, err = store.ScheduleAndCreate(ctx, certCellParams(s.userID, s.appID, s.hostID, 1, h264, "h264", 5000, tok))
	if err != nil {
		t.Fatalf("bench cell on GPU 1: %v", err)
	}
	if sess.GPUID == nil || *sess.GPUID != gpu1ID {
		t.Fatalf("bench cell pinned to GPU 1 ran on %v", sess.GPUID)
	}
}

// TestPostSessionsExplicitCodecRefusalNamesTheCodec: over HTTP, a hand-picked
// av1 whose only capable GPU is full is 503 capacity_exhausted with Retry-After
// and a message naming av1; with no capable GPU at all, no_host_available.
func TestPostSessionsExplicitCodecRefusalNamesTheCodec(t *testing.T) {
	pool := testDB(t)
	srv, authSvc, store, _ := newStopServer(t, pool)
	ctx := context.Background()
	s, gpu1ID := seedAV1OnGPU0(t, pool)

	if _, err := authSvc.Register(ctx, "codec304@test.local", "codec304user", "quasar-fixture-pw-07"); err != nil {
		t.Fatalf("register: %v", err)
	}
	tok := loginTok(t, authSvc, "codec304@test.local", "quasar-fixture-pw-07")
	seedChain(t, pool, "av1-first", []chainRung{
		{id: "304-av1", codec: "av1", w: 1920, h: 1080, minBW: 10000},
		{id: "304-h264", codec: "h264", w: 1920, h: 1080, minBW: 10000},
	})
	allowLaunchProfiles(t, pool, s.appID, "av1-first")
	setAppProfilePolicy(t, pool, s.appID, "prefer", strPtr("av1-first"))

	launch := func() (*http.Response, map[string]any) {
		t.Helper()
		var buf bytes.Buffer
		_ = json.NewEncoder(&buf).Encode(map[string]any{
			"app_id": s.appID, "profile_id": "av1-first",
			"stream": map[string]any{"codec": "av1"},
		})
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/sessions", &buf)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST /v1/sessions: %v", err)
		}
		defer resp.Body.Close()
		var body map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return resp, body
	}
	errorOf := func(body map[string]any) (code, msg string) {
		e, _ := body["error"].(map[string]any)
		code, _ = e["code"].(string)
		msg, _ = e["message"].(string)
		return code, msg
	}

	// GPU 0, the only av1 GPU, is taken by someone else.
	if _, err := store.ScheduleAndCreate(ctx, constrainedTo(s, "av1")); err != nil {
		t.Fatalf("fill GPU 0: %v", err)
	}
	resp, body := launch()
	code, msg := errorOf(body)
	if resp.StatusCode != http.StatusServiceUnavailable || code != "capacity_exhausted" {
		t.Fatalf("got %d %q, want 503 capacity_exhausted (body %v)", resp.StatusCode, code, body)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("capacity_exhausted must carry Retry-After")
	}
	if !strings.Contains(msg, "av1") {
		t.Errorf("message %q must name the codec", msg)
	}
	if n := sessionsOnGPU(t, pool, gpu1ID); n != 0 {
		t.Fatalf("GPU 1 holds %d session(s) after an av1 refusal", n)
	}

	setGPUCodecsRaw(t, pool, s.hostID, 0, `["h264","h265"]`)
	resp, body = launch()
	code, msg = errorOf(body)
	if resp.StatusCode != http.StatusServiceUnavailable || code != "no_host_available" {
		t.Fatalf("got %d %q, want 503 no_host_available (body %v)", resp.StatusCode, code, body)
	}
	if !strings.Contains(msg, "av1") {
		t.Errorf("message %q must name the codec", msg)
	}
}
