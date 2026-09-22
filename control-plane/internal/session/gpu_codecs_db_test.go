package session

// DB integration tests for the GPU codec-set read model (#296 amendment 12,
// migration 0086). Require Postgres (make test-db).

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestAutoLaunchOnNarrowerGPUResolvesHEVC (#303): a two-GPU host where only GPU 0
// encodes AV1. GPU 0 has no free slot, so an Auto launch lands on GPU 1 and must
// resolve its rung against GPU 1's set, not the host's union.
func TestAutoLaunchOnNarrowerGPUResolvesHEVC(t *testing.T) {
	pool := testDB(t)
	userID, appID, hostID := seed1080pApp(t, pool)
	store := NewStore(pool)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	ctx := context.Background()

	seedSecondGPU(t, pool, hostID, 16384, 4)
	setHostCodecsRaw(t, pool, hostID, `["h264","h265","av1"]`)
	setGPUCodecsRaw(t, pool, hostID, 0, `["h264","h265","av1"]`)
	setGPUCodecsRaw(t, pool, hostID, 1, `["h264","h265"]`)
	if _, err := pool.Exec(ctx, `UPDATE gpus SET encode_slots_total = 0 WHERE host_id::text = $1 AND index = 0`, hostID); err != nil {
		t.Fatalf("fill gpu 0: %v", err)
	}
	enableChainCodecs(t, pool, "1080p60", "av1", "hevc", "h264")
	upsertCodecProbe(t, pool, userID, true, true)

	res, err := coord.LaunchByProfile(ctx, userID, LaunchParams{AppID: appID, ProfileID: "1080p60", IsAdmin: true})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	if res.Session.GPUIndex == nil || *res.Session.GPUIndex != 1 {
		t.Fatalf("placed gpu index = %v, want 1", res.Session.GPUIndex)
	}
	if res.Session.Codec != "h265" {
		t.Errorf("session codec = %q, want h265 (GPU 1 has no av1)", res.Session.Codec)
	}
	got, err := store.Get(ctx, res.Session.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	gpu1, err := store.GPUCodecs(ctx, hostID, 1)
	if err != nil {
		t.Fatalf("GPUCodecs: %v", err)
	}
	if got.Codec != "h265" || !codecSet(gpu1)[got.Codec] {
		t.Errorf("stored codec = %q, want h265 and in GPU 1's set %v", got.Codec, gpu1)
	}
}

// seedSecondGPU adds a second GPU (index 1) on the same host as s, so a test
// can exercise a GPU with its own codecs distinct from index 0 / the host.
func seedSecondGPU(t *testing.T, pool *pgxpool.Pool, hostID string, vramMBTotal, encodeSlots int) (gpuID string) {
	t.Helper()
	must(t, pool.QueryRow(context.Background(), `
		INSERT INTO gpus (host_id, index, vram_mb_total, encode_slots_total)
		VALUES ($1, 1, $2, $3) RETURNING id::text
	`, hostID, vramMBTotal, encodeSlots).Scan(&gpuID))
	return gpuID
}

// setGPUCodecsRaw writes gpus.codecs for (hostID, index) directly. raw == ""
// writes SQL NULL (never reported); any other string is cast ::jsonb, so a
// test can also write the explicit-empty-report shape "[]". NOT the JSON
// literal `null` cast to jsonb — that is a non-NULL value COALESCE would
// return verbatim, breaking the inheritance this exists to test.
func setGPUCodecsRaw(t *testing.T, pool *pgxpool.Pool, hostID string, index int, raw string) {
	t.Helper()
	var arg any
	if raw != "" {
		arg = raw
	}
	if _, err := pool.Exec(context.Background(), `
		UPDATE gpus SET codecs = $3::jsonb WHERE host_id::text = $1 AND index = $2
	`, hostID, index, arg); err != nil {
		t.Fatalf("set gpu %d codecs: %v", index, err)
	}
}

// setHostCodecsRaw is setGPUCodecsRaw's host-level twin: raw == "" writes SQL
// NULL, anything else is cast ::jsonb.
func setHostCodecsRaw(t *testing.T, pool *pgxpool.Pool, hostID string, raw string) {
	t.Helper()
	var arg any
	if raw != "" {
		arg = raw
	}
	if _, err := pool.Exec(context.Background(), `
		UPDATE hosts SET codecs = $2::jsonb WHERE id::text = $1
	`, hostID, arg); err != nil {
		t.Fatalf("set host codecs: %v", err)
	}
}

// rawColumn reads one jsonb column's raw bytes (nil ⇒ SQL NULL) via an
// arbitrary single-row query, used to read the inputs the twin agreement test
// feeds independently into the SQL renderer and the Go twin.
func rawColumn(t *testing.T, pool *pgxpool.Pool, query string, args ...any) []byte {
	t.Helper()
	var raw []byte
	if err := pool.QueryRow(context.Background(), query, args...).Scan(&raw); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return raw
}

func mustParseCodecs(t *testing.T, raw []byte) []string {
	t.Helper()
	if raw == nil {
		return nil
	}
	var codecs []string
	if err := json.Unmarshal(raw, &codecs); err != nil {
		t.Fatalf("unmarshal codecs %s: %v", raw, err)
	}
	return codecs
}

// TestStoreGPUCodecsInheritance covers Store.GPUCodecs (the launch-side read,
// gpuCodecSetSQL with fallbackH264=true) across every inheritance case in the
// spec: a GPU's own set wins, NULL inherits the host's, and both NULL floors
// at h264 — never "unknown" on the placement path.
func TestStoreGPUCodecsInheritance(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	ctx := context.Background()

	cases := []struct {
		name       string
		gpuRaw     string
		hostRaw    string
		wantCodecs []string
	}{
		{"gpu set wins over host", `["h264","h265","av1"]`, `["h264"]`, []string{"h264", "h265", "av1"}},
		{"gpu never reported inherits host", "", `["h264","av1"]`, []string{"h264", "av1"}},
		{"neither ever reported falls back to h264", "", "", []string{"h264"}},
		{"gpu explicit empty does not inherit", "[]", `["h264","h265"]`, []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setGPUCodecsRaw(t, pool, s.hostID, 0, tc.gpuRaw)
			setHostCodecsRaw(t, pool, s.hostID, tc.hostRaw)

			got, err := store.GPUCodecs(ctx, s.hostID, 0)
			if err != nil {
				t.Fatalf("GPUCodecs: %v", err)
			}
			if !strSliceEqual(got, tc.wantCodecs) {
				t.Errorf("GPUCodecs = %v, want %v", got, tc.wantCodecs)
			}
		})
	}
}

// TestGPUCodecsUnknownGPUFallsBackToH264: no matching (host, index) row —
// Store.GPUCodecs degrades to h264, the same fail-safe HostCodecs uses for an
// unknown host, never an error that would abort a launch.
func TestGPUCodecsUnknownGPUFallsBackToH264(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	ctx := context.Background()

	got, err := store.GPUCodecs(ctx, s.hostID, 7)
	if err != nil {
		t.Fatalf("GPUCodecs: %v", err)
	}
	if !strSliceEqual(got, []string{"h264"}) {
		t.Errorf("GPUCodecs(unknown index) = %v, want [h264]", got)
	}

	got, err = store.GPUCodecs(ctx, "not-a-uuid", 0)
	if err != nil {
		t.Fatalf("GPUCodecs: %v", err)
	}
	if !strSliceEqual(got, []string{"h264"}) {
		t.Errorf("GPUCodecs(invalid host) = %v, want [h264]", got)
	}
}

// gpuCodecSetNullableSQL reads gpuCodecSetSQL(fallbackH264=false) — the admin
// renderer — directly, the way GPUAvailability's query embeds it. nil means
// the SQL rendered NULL (neither the GPU nor its host has ever reported).
func gpuCodecSetNullableSQL(t *testing.T, pool *pgxpool.Pool, hostID string, gpuIndex int) []string {
	t.Helper()
	raw := rawColumn(t, pool, `
		SELECT `+gpuCodecSetSQL("g", "h", false)+`
		FROM gpus g JOIN hosts h ON h.id = g.host_id
		WHERE g.host_id = $1::uuid AND g.index = $2
	`, hostID, gpuIndex)
	return mustParseCodecs(t, raw)
}

// TestGPUCodecSetMatchesSQL is the twin-agreement test (spec "Testing
// Decisions" item 4, in the manner of TestCertForRungMatchesPickCert):
// gpuCodecSetSQL and its pure Go twins — gpuCodecSet (fallbackH264=true, the
// launch-side read, via Store.GPUCodecs) and gpuCodecSetNullable
// (fallbackH264=false, the admin read) — must each agree with their own SQL
// rendering for every inheritance case, computed from the SAME raw column
// reads so neither side can cheat by sharing state.
func TestGPUCodecSetMatchesSQL(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	ctx := context.Background()
	seedSecondGPU(t, pool, s.hostID, 16384, 2)

	inputs := []struct {
		name    string
		gpuRaw  string // "" = NULL
		hostRaw string
	}{
		{"gpu set, host set", `["h264","h265"]`, `["h264"]`},
		{"gpu null, host set", "", `["h264","h265","av1"]`},
		{"gpu null, host null", "", ""},
		{"gpu empty, host set", "[]", `["h264"]`},
		{"gpu set, host null", `["av1"]`, ""},
	}

	for _, in := range inputs {
		t.Run(in.name, func(t *testing.T) {
			setGPUCodecsRaw(t, pool, s.hostID, 0, in.gpuRaw)
			setHostCodecsRaw(t, pool, s.hostID, in.hostRaw)

			gpuParsed := mustParseCodecs(t, rawColumn(t, pool,
				`SELECT codecs FROM gpus WHERE id::text = $1`, s.gpuID))
			hostParsed := mustParseCodecs(t, rawColumn(t, pool,
				`SELECT codecs FROM hosts WHERE id::text = $1`, s.hostID))

			t.Run("fallbackH264=true (launch)", func(t *testing.T) {
				sqlSide, err := store.GPUCodecs(ctx, s.hostID, 0)
				if err != nil {
					t.Fatalf("GPUCodecs: %v", err)
				}
				goSide := gpuCodecSet(gpuParsed, hostParsed)
				if !strSliceEqual(sqlSide, goSide) {
					t.Errorf("SQL chose %v, gpuCodecSet chose %v (gpu raw=%q host raw=%q)",
						sqlSide, goSide, in.gpuRaw, in.hostRaw)
				}
			})

			t.Run("fallbackH264=false (admin)", func(t *testing.T) {
				sqlSide := gpuCodecSetNullableSQL(t, pool, s.hostID, 0)
				goSide := gpuCodecSetNullable(gpuParsed, hostParsed)
				if !strSliceEqual(sqlSide, goSide) {
					t.Errorf("SQL chose %v, gpuCodecSetNullable chose %v (gpu raw=%q host raw=%q)",
						sqlSide, goSide, in.gpuRaw, in.hostRaw)
				}
				wantNil := in.gpuRaw == "" && in.hostRaw == ""
				if wantNil != (sqlSide == nil) {
					t.Errorf("SQL nullness = %v, want nil==%v (gpu raw=%q host raw=%q)",
						sqlSide, wantNil, in.gpuRaw, in.hostRaw)
				}
				if wantNil != (goSide == nil) {
					t.Errorf("gpuCodecSetNullable nullness = %v, want nil==%v (gpu raw=%q host raw=%q)",
						goSide, wantNil, in.gpuRaw, in.hostRaw)
				}
			})
		})
	}

	// A second GPU on the same host, left NULL throughout, always resolves to
	// the host set independently of GPU 0's — proves the renderer keys on the
	// right GPU row, not "any GPU on this host".
	setHostCodecs(t, pool, s.hostID, `["h264","h265"]`)
	setGPUCodecsRaw(t, pool, s.hostID, 0, `["av1"]`)
	got1, err := store.GPUCodecs(ctx, s.hostID, 1)
	if err != nil {
		t.Fatalf("GPUCodecs gpu 1: %v", err)
	}
	if !strSliceEqual(got1, []string{"h264", "h265"}) {
		t.Errorf("GPUCodecs gpu 1 = %v, want the host set [h264 h265], not gpu 0's", got1)
	}
}

// TestGPUAvailabilityCodecs: the admin read (GPUAvailability.Codecs,
// gpuCodecSetSQL fallbackH264=false) — a GPU's own set, its host's when it has
// none, and nil ONLY when neither has ever reported (never normalised to
// ["h264"], unlike the launch-side Store.GPUCodecs above).
func TestGPUAvailabilityCodecs(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)

	// Neither the GPU nor the host has reported: nil, not ["h264"].
	before := findGPU(t, mustAvail(t, store, ""), s.gpuID)
	if before.Codecs != nil {
		t.Fatalf("codecs before any report = %v, want nil", before.Codecs)
	}

	// Host reports, GPU does not: inherits the host set.
	setHostCodecs(t, pool, s.hostID, `["h264","h265"]`)
	afterHost := findGPU(t, mustAvail(t, store, ""), s.gpuID)
	if !strSliceEqual(afterHost.Codecs, []string{"h264", "h265"}) {
		t.Fatalf("codecs inheriting host = %v, want [h264 h265]", afterHost.Codecs)
	}

	// The GPU reports its own, narrower set: that wins over the host's.
	setGPUCodecsRaw(t, pool, s.hostID, 0, `["h264"]`)
	afterGPU := findGPU(t, mustAvail(t, store, ""), s.gpuID)
	if !strSliceEqual(afterGPU.Codecs, []string{"h264"}) {
		t.Fatalf("codecs with a GPU report = %v, want [h264]", afterGPU.Codecs)
	}
}
