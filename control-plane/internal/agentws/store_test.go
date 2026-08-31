package agentws

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/migrate"
	"github.com/accreleus/quasar/control-plane/migrations"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	if err := migrate.Run(migrations.FS, dbURL); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	// The canonical DB verifier runs packages serially (-p 1); this package's
	// legacy fixtures use fixed host names and therefore reset their shared tables.
	if _, err := pool.Exec(ctx, `DELETE FROM sessions; DELETE FROM gpus; DELETE FROM hosts;
		DELETE FROM apps WHERE name='capacity-history'; DELETE FROM users WHERE email='capacity@test';`); err != nil {
		pool.Close()
		t.Fatalf("truncate: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func seedHost(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var id string
	err := pool.QueryRow(context.Background(),
		`INSERT INTO hosts (node_name, status) VALUES ('h1', 'online') RETURNING id::text`).Scan(&id)
	if err != nil {
		t.Fatalf("seed host: %v", err)
	}
	return id
}

func rawStorage(t *testing.T, pool *pgxpool.Pool, hostID string) []byte {
	t.Helper()
	var raw []byte
	if err := pool.QueryRow(context.Background(),
		`SELECT storage FROM hosts WHERE id::text = $1`, hostID).Scan(&raw); err != nil {
		t.Fatalf("query storage: %v", err)
	}
	return raw
}

func rawEffective(t *testing.T, pool *pgxpool.Pool, hostID string) []byte {
	t.Helper()
	var raw []byte
	if err := pool.QueryRow(context.Background(),
		`SELECT effective_settings FROM hosts WHERE id::text = $1`, hostID).Scan(&raw); err != nil {
		t.Fatalf("query effective_settings: %v", err)
	}
	return raw
}

func rawCPUModel(t *testing.T, pool *pgxpool.Pool, hostID string) *string {
	t.Helper()
	var model *string
	if err := pool.QueryRow(context.Background(),
		`SELECT cpu_model FROM hosts WHERE id::text = $1`, hostID).Scan(&model); err != nil {
		t.Fatalf("query cpu_model: %v", err)
	}
	return model
}

func rawCodecs(t *testing.T, pool *pgxpool.Pool, hostID string) []byte {
	t.Helper()
	var raw []byte
	if err := pool.QueryRow(context.Background(),
		`SELECT codecs FROM hosts WHERE id::text = $1`, hostID).Scan(&raw); err != nil {
		t.Fatalf("query codecs: %v", err)
	}
	return raw
}

func strPtr(s string) *string { return &s }

// TestUpsertHostCodecs verifies the multi-codec host codec set write and its
// keep-if-absent semantics (spec §3.1.2): a report with codecs writes it; a nil
// codecs slice leaves the prior value untouched.
func TestUpsertHostCodecs(t *testing.T) {
	pool := testPool(t)
	s := &agentStore{pool: pool}
	hostID := seedHost(t, pool)

	// Absent on a fresh host ⇒ NULL column.
	if raw := rawCodecs(t, pool, hostID); raw != nil {
		t.Fatalf("fresh host codecs: got %s, want NULL", raw)
	}

	// A report carrying codecs persists them.
	if err := s.upsertHostCodecs(context.Background(), hostID, []string{"h264", "h265"}); err != nil {
		t.Fatalf("upsert codecs: %v", err)
	}
	var got []string
	if err := json.Unmarshal(rawCodecs(t, pool, hostID), &got); err != nil {
		t.Fatalf("unmarshal codecs: %v", err)
	}
	if len(got) != 2 || got[0] != "h264" || got[1] != "h265" {
		t.Fatalf("stored codecs: got %v, want [h264 h265]", got)
	}

	// A subsequent report that omits codecs (nil) must not clobber the stored set.
	if err := s.upsertHostCodecs(context.Background(), hostID, nil); err != nil {
		t.Fatalf("upsert nil codecs: %v", err)
	}
	if err := json.Unmarshal(rawCodecs(t, pool, hostID), &got); err != nil {
		t.Fatalf("unmarshal codecs after nil: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("codecs after keep-if-absent: got %v, want [h264 h265]", got)
	}
}

// rawCodecPixelRates reads hosts.codec_pixel_rates as stored, with no projection —
// the point of the #506 test below is that the agent's object survives byte-shaped.
func rawCodecPixelRates(t *testing.T, pool *pgxpool.Pool, hostID string) []byte {
	t.Helper()
	var raw []byte
	if err := pool.QueryRow(context.Background(),
		`SELECT codec_pixel_rates FROM hosts WHERE id=$1`, hostID).Scan(&raw); err != nil {
		t.Fatalf("query codec_pixel_rates: %v", err)
	}
	return raw
}

// TestUpsertHostCodecPixelRates (#506) covers the three things the throughput hint
// has to get right at ingest: it is stored VERBATIM (so a newer agent's extra keys
// survive an older control plane), an omitted key keeps the prior value, and an
// explicit `{}` really does clear it.
func TestUpsertHostCodecPixelRates(t *testing.T) {
	pool := testPool(t)
	s := &agentStore{pool: pool}
	hostID := seedHost(t, pool)
	ctx := context.Background()

	if raw := rawCodecPixelRates(t, pool, hostID); raw != nil {
		t.Fatalf("fresh host codec_pixel_rates: got %s, want NULL", raw)
	}

	// Verbatim storage, INCLUDING a key this control plane does not know about —
	// that forward-compatibility is the reason the wire value is an object and the
	// reason the column holds raw JSON rather than a flattened codec→number map.
	report := json.RawMessage(
		`{"h265":{"max_pixel_rate_mpix_s":395,"measured_at":"2026-08-24T00:00:00Z"},` +
			`"h264":{"max_pixel_rate_mpix_s":1400}}`)
	if err := s.upsertHostCodecPixelRates(ctx, hostID, report); err != nil {
		t.Fatalf("upsert codec pixel rates: %v", err)
	}
	var stored map[string]map[string]any
	if err := json.Unmarshal(rawCodecPixelRates(t, pool, hostID), &stored); err != nil {
		t.Fatalf("unmarshal stored rates: %v", err)
	}
	if stored["h265"]["max_pixel_rate_mpix_s"] != float64(395) {
		t.Fatalf("stored h265 rate = %v, want 395", stored["h265"]["max_pixel_rate_mpix_s"])
	}
	if stored["h265"]["measured_at"] != "2026-08-24T00:00:00Z" {
		t.Errorf("the unknown per-codec key was dropped on ingest: %v — a newer agent's "+
			"fields must survive an older control plane", stored["h265"])
	}

	// Keep-if-absent: a later report that omits the key leaves the stored map alone.
	if err := s.upsertHostCodecPixelRates(ctx, hostID, nil); err != nil {
		t.Fatalf("upsert nil rates: %v", err)
	}
	if raw := rawCodecPixelRates(t, pool, hostID); len(raw) == 0 {
		t.Fatal("codec_pixel_rates cleared by an absent key, want keep-if-absent")
	}

	// An explicit {} IS an overwrite — the shape a host must be able to report after
	// a config_update moves it onto an encoder path with no measurements.
	if err := s.upsertHostCodecPixelRates(ctx, hostID, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("upsert empty rates: %v", err)
	}
	if got := string(rawCodecPixelRates(t, pool, hostID)); got != "{}" {
		t.Errorf("codec_pixel_rates after an explicit {} = %s, want {}", got)
	}

	// Malformed input is refused rather than stored, so the launch path never has to
	// tell "unparseable" apart from "unknown".
	if err := s.upsertHostCodecPixelRates(ctx, hostID, json.RawMessage(`["h265"]`)); err == nil {
		t.Error("upsert of a non-object payload returned nil, want an error")
	}
}

// TestUpsertCapacityStorageAbsentKeepsPriorValue verifies the host-observability
// keep-if-absent semantics (agent-api.md capacity §host.storage): a capacity
// report with no "storage" key must not clobber a previously stored value.
func TestUpsertCapacityStorageAbsentKeepsPriorValue(t *testing.T) {
	pool := testPool(t)
	s := &agentStore{pool: pool}
	hostID := seedHost(t, pool)

	first := HostCapacity{CPUCores: 8, MemMB: 16000, Storage: []StorageVolume{
		{Label: "agent-data", Path: "/var/lib/quasar-agent", TotalMB: 819200, AvailableMB: 512000},
	}}
	if err := s.upsertCapacity(context.Background(), hostID, first, nil, nil); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if got := rawStorage(t, pool, hostID); len(got) == 0 {
		t.Fatalf("storage not persisted: %s", got)
	}

	// Re-send with storage absent (nil) — must keep the prior value.
	second := HostCapacity{CPUCores: 8, MemMB: 16000}
	if err := s.upsertCapacity(context.Background(), hostID, second, nil, nil); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	got := rawStorage(t, pool, hostID)
	var vols []StorageVolume
	if err := json.Unmarshal(got, &vols); err != nil {
		t.Fatalf("decode storage: %v (%s)", err, got)
	}
	if len(vols) != 1 || vols[0].Label != "agent-data" {
		t.Fatalf("storage after absent re-report = %v, want the first report retained", vols)
	}
}

// TestUpsertCapacityStorageEmptyArrayOverwrites verifies an explicit empty array
// (as opposed to an absent key) is a real overwrite, not a no-op.
func TestUpsertCapacityStorageEmptyArrayOverwrites(t *testing.T) {
	pool := testPool(t)
	s := &agentStore{pool: pool}
	hostID := seedHost(t, pool)

	first := HostCapacity{Storage: []StorageVolume{{Label: "a", Path: "/a", TotalMB: 1, AvailableMB: 1}}}
	if err := s.upsertCapacity(context.Background(), hostID, first, nil, nil); err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	second := HostCapacity{Storage: []StorageVolume{}}
	if err := s.upsertCapacity(context.Background(), hostID, second, nil, nil); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	var vols []StorageVolume
	if err := json.Unmarshal(rawStorage(t, pool, hostID), &vols); err != nil {
		t.Fatalf("decode storage: %v", err)
	}
	if len(vols) != 0 {
		t.Fatalf("storage after explicit-empty report = %v, want overwritten to empty", vols)
	}
}

// TestUpsertCapacityEffectiveSettingsAbsentKeepsPriorValue mirrors the storage
// case for effective_settings (agent-api.md capacity §effective_settings).
func TestUpsertCapacityEffectiveSettingsAbsentKeepsPriorValue(t *testing.T) {
	pool := testPool(t)
	s := &agentStore{pool: pool}
	hostID := seedHost(t, pool)

	if err := s.upsertCapacity(context.Background(), hostID, HostCapacity{}, map[string]string{"encoder": "va"}, nil); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if err := s.upsertCapacity(context.Background(), hostID, HostCapacity{}, nil, nil); err != nil {
		t.Fatalf("second upsert (absent effective_settings): %v", err)
	}
	var out map[string]string
	if err := json.Unmarshal(rawEffective(t, pool, hostID), &out); err != nil {
		t.Fatalf("decode effective_settings: %v", err)
	}
	if out["encoder"] != "va" {
		t.Fatalf("effective_settings after absent re-report = %v, want the first report retained", out)
	}
}

// TestUpsertCapacityCPUModelAbsentKeepsPriorValue mirrors the storage/
// effective_settings keep-if-absent case for host.cpu_model
// (host-observability-2, agent-api.md capacity §host.cpu_model).
func TestUpsertCapacityCPUModelAbsentKeepsPriorValue(t *testing.T) {
	pool := testPool(t)
	s := &agentStore{pool: pool}
	hostID := seedHost(t, pool)

	first := HostCapacity{CPUModel: strPtr("AMD Ryzen 9 7950X 16-Core Processor")}
	if err := s.upsertCapacity(context.Background(), hostID, first, nil, nil); err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	second := HostCapacity{} // cpu_model absent
	if err := s.upsertCapacity(context.Background(), hostID, second, nil, nil); err != nil {
		t.Fatalf("second upsert (absent cpu_model): %v", err)
	}
	got := rawCPUModel(t, pool, hostID)
	if got == nil || *got != "AMD Ryzen 9 7950X 16-Core Processor" {
		t.Fatalf("cpu_model after absent re-report = %v, want the first report retained", got)
	}
}

// TestUpsertCapacityGPURenderNodePersists verifies gpus[].render_node persists
// through the wholesale-replace GPU upsert and round-trips as null when a GPU
// doesn't report one (host-observability-2, agent-api.md capacity
// §gpus[].render_node).
func TestUpsertCapacityGPURenderNodePersists(t *testing.T) {
	pool := testPool(t)
	s := &agentStore{pool: pool}
	hostID := seedHost(t, pool)

	gpus := []GPUCapacity{
		{Index: 0, Vendor: "amd", Model: "Radeon Pro V520", VRAMMBTotal: 16384, EncodeSlotsTotal: 2,
			RenderNode: strPtr("/dev/dri/by-path/pci-0000:04:00.0-render"), DevicePath: strPtr("/dev/dri/renderD128")},
		{Index: 1, Vendor: "nvidia", Model: "RTX 5090", VRAMMBTotal: 32768, EncodeSlotsTotal: 4},
	}
	if err := s.upsertCapacity(context.Background(), hostID, HostCapacity{}, nil, gpus); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	rows, err := pool.Query(context.Background(),
		`SELECT index, render_node, device_path FROM gpus WHERE host_id::text = $1 ORDER BY index`, hostID)
	if err != nil {
		t.Fatalf("query gpus: %v", err)
	}
	defer rows.Close()

	var got []struct {
		Index      int
		RenderNode *string
		DevicePath *string
	}
	for rows.Next() {
		var idx int
		var rn *string
		var devicePath *string
		if err := rows.Scan(&idx, &rn, &devicePath); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, struct {
			Index      int
			RenderNode *string
			DevicePath *string
		}{idx, rn, devicePath})
	}
	if len(got) != 2 {
		t.Fatalf("gpus rows = %d, want 2", len(got))
	}
	if got[0].RenderNode == nil || *got[0].RenderNode != "/dev/dri/by-path/pci-0000:04:00.0-render" {
		t.Errorf("gpu 0 render_node = %v, want the reported path", got[0].RenderNode)
	}
	if got[1].RenderNode != nil {
		t.Errorf("gpu 1 render_node = %v, want nil (not reported)", got[1].RenderNode)
	}
	if got[0].DevicePath == nil || *got[0].DevicePath != "/dev/dri/renderD128" {
		t.Errorf("gpu 0 device_path = %v, want reported kernel path", got[0].DevicePath)
	}
}

func TestFailedCapacityReportRetainsHistoryButUnschedulesGPU(t *testing.T) {
	pool := testPool(t)
	s := &agentStore{pool: pool}
	hostID := seedHost(t, pool)
	gpus := []GPUCapacity{{Index: 0, Vendor: "nvidia", Model: "RTX 5090", VRAMMBTotal: 32607, EncodeSlotsTotal: 3}}
	if err := s.upsertCapacityWithDetection(context.Background(), hostID, HostCapacity{}, nil, gpus, "ok", ""); err != nil {
		t.Fatalf("initial report: %v", err)
	}

	ctx := context.Background()
	var gpuID, userID, appID string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM gpus WHERE host_id::text=$1`, hostID).Scan(&gpuID); err != nil {
		t.Fatalf("gpu id: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO users(email,username,password_hash) VALUES('capacity@test','capacitytest','x') RETURNING id::text`).Scan(&userID); err != nil {
		t.Fatalf("user: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO apps(name) VALUES('capacity-history') RETURNING id::text`).Scan(&appID); err != nil {
		t.Fatalf("app: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sessions(user_id,app_id,host_id,gpu_id,state,width,height,fps,bitrate_kbps)
		VALUES($1,$2,$3,$4,'stopped',1280,720,60,6000)`, userID, appID, hostID, gpuID); err != nil {
		t.Fatalf("historical session: %v", err)
	}

	if err := s.upsertCapacityWithDetection(ctx, hostID, HostCapacity{}, nil, nil, "failed", "sysfs unavailable"); err != nil {
		t.Fatalf("failed re-report: %v", err)
	}
	var reported bool
	var detection string
	if err := pool.QueryRow(ctx, `SELECT reported FROM gpus WHERE id::text=$1`, gpuID).Scan(&reported); err != nil {
		t.Fatalf("historical gpu missing: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT capacity_detection FROM hosts WHERE id::text=$1`, hostID).Scan(&detection); err != nil {
		t.Fatalf("capacity status: %v", err)
	}
	if reported || detection != "failed" {
		t.Fatalf("reported=%v detection=%q, want false/failed", reported, detection)
	}
}

func rawPendingRestart(t *testing.T, pool *pgxpool.Pool, hostID string) bool {
	t.Helper()
	var pending bool
	if err := pool.QueryRow(context.Background(),
		`SELECT pending_restart FROM hosts WHERE id::text = $1`, hostID).Scan(&pending); err != nil {
		t.Fatalf("query pending_restart: %v", err)
	}
	return pending
}

// TestReconnectHostClearsPendingRestart verifies the host-observability-2
// self-healing mechanism: a host whose pending_restart was left true (a
// restart command was sent) gets it cleared as soon as its agent successfully
// reconnects (control-api.md: "pending_restart ... clears when the agent
// reconnects").
func TestReconnectHostClearsPendingRestart(t *testing.T) {
	pool := testPool(t)
	s := &agentStore{pool: pool}

	secret := "test-node-secret"
	h := sha256.Sum256([]byte(secret))
	secretHash := hex.EncodeToString(h[:])

	var hostID string
	err := pool.QueryRow(context.Background(), `
		INSERT INTO hosts (node_name, status, node_secret_hash, pending_restart)
		VALUES ('reconnect-host', 'offline', $1, true)
		RETURNING id::text
	`, secretHash).Scan(&hostID)
	if err != nil {
		t.Fatalf("seed host: %v", err)
	}
	if !rawPendingRestart(t, pool, hostID) {
		t.Fatalf("seed did not set pending_restart=true")
	}
	if _, err := pool.Exec(context.Background(), `INSERT INTO gpus(host_id,index,vram_mb_total,encode_slots_total,reported) VALUES($1,0,8192,1,true)`, hostID); err != nil {
		t.Fatalf("seed gpu: %v", err)
	}

	if _, err := s.reconnectHost(context.Background(), "reconnect-host", "0.2.0", secret); err != nil {
		t.Fatalf("reconnectHost: %v", err)
	}

	if rawPendingRestart(t, pool, hostID) {
		t.Error("pending_restart still true after a successful reconnect")
	}
	var reported bool
	if err := pool.QueryRow(context.Background(), `SELECT reported FROM gpus WHERE host_id=$1`, hostID).Scan(&reported); err != nil || reported {
		t.Errorf("reconnect inventory reported=%v err=%v, want false", reported, err)
	}
}

// TestEnrollHostClearsPendingRestart mirrors the reconnect case for
// re-enrollment (enrollHost's ON CONFLICT(node_name) DO UPDATE path — an
// already-known node_name re-enrolling, e.g. after a lost node_secret).
func TestEnrollHostClearsPendingRestart(t *testing.T) {
	pool := testPool(t)
	s := &agentStore{pool: pool}

	var hostID string
	err := pool.QueryRow(context.Background(), `
		INSERT INTO hosts (node_name, status, pending_restart)
		VALUES ('enroll-host', 'offline', true)
		RETURNING id::text
	`).Scan(&hostID)
	if err != nil {
		t.Fatalf("seed host: %v", err)
	}
	if !rawPendingRestart(t, pool, hostID) {
		t.Fatalf("seed did not set pending_restart=true")
	}
	if _, err := pool.Exec(context.Background(), `INSERT INTO gpus(host_id,index,vram_mb_total,encode_slots_total,reported) VALUES($1,0,8192,1,true)`, hostID); err != nil {
		t.Fatalf("seed gpu: %v", err)
	}

	const token = "shared-enrollment-token"
	if _, err := s.enrollHost(context.Background(), "enroll-host", "0.2.0", token, token); err != nil {
		t.Fatalf("enrollHost: %v", err)
	}

	if rawPendingRestart(t, pool, hostID) {
		t.Error("pending_restart still true after a successful re-enrollment")
	}
	var reported bool
	if err := pool.QueryRow(context.Background(), `SELECT reported FROM gpus WHERE host_id=$1`, hostID).Scan(&reported); err != nil || reported {
		t.Errorf("re-enrollment inventory reported=%v err=%v, want false", reported, err)
	}
}

// ── #429 follow-on: node-agent restart visibility ──────────────────────────
//
// These tests exercise reconnectHost's blip-vs-restart classification (see
// agentRestartMinGap's doc comment for the full rationale). They override the
// package-level threshold to keep the test fast and deterministic instead of
// sleeping for real seconds — the classification logic itself is identical
// regardless of the threshold's magnitude.

func seedHostWithSecret(t *testing.T, pool *pgxpool.Pool, nodeName, secret string) string {
	t.Helper()
	h := sha256.Sum256([]byte(secret))
	secretHash := hex.EncodeToString(h[:])
	var hostID string
	err := pool.QueryRow(context.Background(), `
		INSERT INTO hosts (node_name, status, node_secret_hash)
		VALUES ($1, 'offline', $2)
		RETURNING id::text
	`, nodeName, secretHash).Scan(&hostID)
	if err != nil {
		t.Fatalf("seed host with secret: %v", err)
	}
	return hostID
}

func setAgentDisconnectedAt(t *testing.T, pool *pgxpool.Pool, hostID string, at *time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE hosts SET agent_disconnected_at = $2 WHERE id::text = $1`, hostID, at); err != nil {
		t.Fatalf("set agent_disconnected_at: %v", err)
	}
}

func setAgentProcessStartedAt(t *testing.T, pool *pgxpool.Pool, hostID string, at *time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE hosts SET agent_process_started_at = $2 WHERE id::text = $1`, hostID, at); err != nil {
		t.Fatalf("set agent_process_started_at: %v", err)
	}
}

type restartFields struct {
	restartCount   int32
	lastRestart    *time.Time
	processStarted *time.Time
	disconnectedAt *time.Time
}

func readRestartFields(t *testing.T, pool *pgxpool.Pool, hostID string) restartFields {
	t.Helper()
	var f restartFields
	if err := pool.QueryRow(context.Background(), `
		SELECT agent_restart_count, agent_last_restart_at, agent_process_started_at, agent_disconnected_at
		FROM hosts WHERE id::text = $1
	`, hostID).Scan(&f.restartCount, &f.lastRestart, &f.processStarted, &f.disconnectedAt); err != nil {
		t.Fatalf("read restart fields: %v", err)
	}
	return f
}

// withRestartGap overrides agentRestartMinGap for the duration of one test,
// restoring the original on cleanup — tests must never leak this override
// into others (package var, no t.Parallel() in this file).
func withRestartGap(t *testing.T, gap time.Duration) {
	t.Helper()
	orig := agentRestartMinGap
	agentRestartMinGap = gap
	t.Cleanup(func() { agentRestartMinGap = orig })
}

// TestReconnectHostBlipDoesNotCountAsRestart: a disconnect-to-reconnect gap
// well under the threshold must NOT be counted as an agent restart, and must
// leave agent_process_started_at (process identity) untouched — it is read
// as continuous uptime across a mere WebSocket blip.
func TestReconnectHostBlipDoesNotCountAsRestart(t *testing.T) {
	pool := testPool(t)
	withRestartGap(t, 2*time.Second)
	s := &agentStore{pool: pool}

	hostID := seedHostWithSecret(t, pool, "blip-host", "secret-1")
	processStart := time.Now().Add(-time.Hour) // long-running process, well before this blip
	setAgentProcessStartedAt(t, pool, hostID, &processStart)
	disconnectedAt := time.Now().Add(-100 * time.Millisecond) // well under the 2s threshold
	setAgentDisconnectedAt(t, pool, hostID, &disconnectedAt)

	result, err := s.reconnectHost(context.Background(), "blip-host", "0.3.0", "secret-1")
	if err != nil {
		t.Fatalf("reconnectHost: %v", err)
	}
	if result.AgentRestarted {
		t.Error("AgentRestarted = true, want false for a sub-threshold gap (blip)")
	}

	f := readRestartFields(t, pool, hostID)
	if f.restartCount != 0 {
		t.Errorf("agent_restart_count = %d, want 0 after a blip", f.restartCount)
	}
	if f.lastRestart != nil {
		t.Errorf("agent_last_restart_at = %v, want nil after a blip", f.lastRestart)
	}
	if f.processStarted == nil || !f.processStarted.Equal(processStart) {
		t.Errorf("agent_process_started_at = %v, want unchanged (%v) after a blip", f.processStarted, processStart)
	}
	if f.disconnectedAt != nil {
		t.Errorf("agent_disconnected_at = %v, want nil (consumed) after reconnect", f.disconnectedAt)
	}
}

// TestReconnectHostGenuineRestartIsCounted: a disconnect-to-reconnect gap at
// or over the threshold IS a genuine restart — counted, timestamped, and the
// process-started marker resets to a fresh instance.
func TestReconnectHostGenuineRestartIsCounted(t *testing.T) {
	pool := testPool(t)
	withRestartGap(t, 200*time.Millisecond)
	s := &agentStore{pool: pool}

	hostID := seedHostWithSecret(t, pool, "restart-host", "secret-2")
	processStart := time.Now().Add(-time.Hour) // the OLD (pre-restart) process instance
	setAgentProcessStartedAt(t, pool, hostID, &processStart)
	disconnectedAt := time.Now().Add(-5 * time.Second) // well over the 200ms threshold
	setAgentDisconnectedAt(t, pool, hostID, &disconnectedAt)

	before := time.Now()
	result, err := s.reconnectHost(context.Background(), "restart-host", "0.3.0", "secret-2")
	if err != nil {
		t.Fatalf("reconnectHost: %v", err)
	}
	if !result.AgentRestarted {
		t.Error("AgentRestarted = false, want true for a gap over the threshold")
	}

	f := readRestartFields(t, pool, hostID)
	if f.restartCount != 1 {
		t.Errorf("agent_restart_count = %d, want 1 after a genuine restart", f.restartCount)
	}
	if f.lastRestart == nil || f.lastRestart.Before(before.Add(-time.Second)) {
		t.Errorf("agent_last_restart_at = %v, want ~now", f.lastRestart)
	}
	if f.processStarted == nil || f.processStarted.Equal(processStart) {
		t.Errorf("agent_process_started_at = %v, want reset away from the old value (%v)", f.processStarted, processStart)
	}
	if f.disconnectedAt != nil {
		t.Errorf("agent_disconnected_at = %v, want nil (consumed) after reconnect", f.disconnectedAt)
	}

	// A second genuine restart accumulates rather than resets the tally.
	disconnectedAt2 := time.Now().Add(-5 * time.Second)
	setAgentDisconnectedAt(t, pool, hostID, &disconnectedAt2)
	result2, err := s.reconnectHost(context.Background(), "restart-host", "0.3.0", "secret-2")
	if err != nil {
		t.Fatalf("second reconnectHost: %v", err)
	}
	if !result2.AgentRestarted {
		t.Error("second AgentRestarted = false, want true")
	}
	if f2 := readRestartFields(t, pool, hostID); f2.restartCount != 2 {
		t.Errorf("agent_restart_count after second restart = %d, want 2", f2.restartCount)
	}
}

// TestReconnectHostNullDisconnectedAtNeverCountsAsRestart covers the
// fail-toward-undercounting default: no recorded disconnect (the host never
// disconnected under this control plane's watch, OR — the case that matters
// most — a control-plane restart killed the process before markOffline ever
// ran) must never be misclassified as an agent restart, regardless of how
// long ago agent_process_started_at was.
func TestReconnectHostNullDisconnectedAtNeverCountsAsRestart(t *testing.T) {
	pool := testPool(t)
	withRestartGap(t, 200*time.Millisecond)
	s := &agentStore{pool: pool}

	hostID := seedHostWithSecret(t, pool, "cp-restart-host", "secret-3")
	processStart := time.Now().Add(-24 * time.Hour) // been running a day
	setAgentProcessStartedAt(t, pool, hostID, &processStart)
	// agent_disconnected_at left NULL — simulates a control-plane restart:
	// the old CP process died before its handleConn defer (markOffline)
	// could stamp anything, even though real downtime may have been minutes.

	result, err := s.reconnectHost(context.Background(), "cp-restart-host", "0.3.0", "secret-3")
	if err != nil {
		t.Fatalf("reconnectHost: %v", err)
	}
	if result.AgentRestarted {
		t.Error("AgentRestarted = true, want false when agent_disconnected_at was never recorded")
	}

	f := readRestartFields(t, pool, hostID)
	if f.restartCount != 0 {
		t.Errorf("agent_restart_count = %d, want 0 when the prior disconnect was never observed", f.restartCount)
	}
	if f.processStarted == nil || !f.processStarted.Equal(processStart) {
		t.Errorf("agent_process_started_at = %v, want unchanged (%v) — unknown is not evidence of a restart", f.processStarted, processStart)
	}
}

// TestReconnectHostSeedsProcessStartedAtWhenNull covers a legacy row (created
// before migration 0067, or any row where agent_process_started_at is
// unknown): the very first post-migration reconnect must populate it, even
// though — per the previous test — that reconnect is correctly NOT counted
// as a restart (no evidence either way).
func TestReconnectHostSeedsProcessStartedAtWhenNull(t *testing.T) {
	pool := testPool(t)
	withRestartGap(t, 200*time.Millisecond)
	s := &agentStore{pool: pool}

	hostID := seedHostWithSecret(t, pool, "legacy-host", "secret-4")
	// agent_process_started_at and agent_disconnected_at both left NULL
	// (fresh row / pre-migration backfill produced NULL for a never-registered host).

	result, err := s.reconnectHost(context.Background(), "legacy-host", "0.3.0", "secret-4")
	if err != nil {
		t.Fatalf("reconnectHost: %v", err)
	}
	if result.AgentRestarted {
		t.Error("AgentRestarted = true, want false for a never-before-seen process start")
	}
	f := readRestartFields(t, pool, hostID)
	if f.processStarted == nil {
		t.Error("agent_process_started_at still nil after reconnect, want it seeded to now()")
	}
	if f.restartCount != 0 {
		t.Errorf("agent_restart_count = %d, want 0", f.restartCount)
	}
}

// TestMarkOfflineStampsDisconnectedAt verifies markOffline records the precise
// disconnect instant reconnectHost's classification measures against.
func TestMarkOfflineStampsDisconnectedAt(t *testing.T) {
	pool := testPool(t)
	s := &agentStore{pool: pool}
	hostID := seedHost(t, pool)

	before := time.Now()
	if err := s.markOffline(context.Background(), hostID); err != nil {
		t.Fatalf("markOffline: %v", err)
	}

	f := readRestartFields(t, pool, hostID)
	if f.disconnectedAt == nil {
		t.Fatal("agent_disconnected_at nil after markOffline, want stamped")
	}
	if f.disconnectedAt.Before(before.Add(-time.Second)) {
		t.Errorf("agent_disconnected_at = %v, want ~now (after %v)", f.disconnectedAt, before)
	}
	var status string
	if err := pool.QueryRow(context.Background(), `SELECT status FROM hosts WHERE id::text=$1`, hostID).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "offline" {
		t.Errorf("status = %q, want offline", status)
	}
}

// TestEnrollHostResetsRestartState covers re-enrollment (an already-known
// node_name enrolling again, e.g. after losing its persisted node_secret): a
// fresh identity event resets the restart tally rather than inheriting the
// previous identity's history, and clears any stale pending disconnect so it
// can never leak into this identity's first reconnect classification.
func TestEnrollHostResetsRestartState(t *testing.T) {
	pool := testPool(t)
	s := &agentStore{pool: pool}

	const token = "shared-enrollment-token-2"
	result, err := s.enrollHost(context.Background(), "re-enroll-host", "0.3.0", token, token)
	if err != nil {
		t.Fatalf("initial enroll: %v", err)
	}
	hostID := result.HostID

	// Simulate restart history + a stale pending disconnect accrued under the
	// old identity.
	if _, err := pool.Exec(context.Background(), `
		UPDATE hosts SET agent_restart_count = 5, agent_last_restart_at = now(),
		agent_disconnected_at = now() - interval '1 hour' WHERE id::text = $1
	`, hostID); err != nil {
		t.Fatalf("seed restart history: %v", err)
	}

	if _, err := s.enrollHost(context.Background(), "re-enroll-host", "0.3.1", token, token); err != nil {
		t.Fatalf("re-enroll: %v", err)
	}

	f := readRestartFields(t, pool, hostID)
	if f.restartCount != 0 {
		t.Errorf("agent_restart_count after re-enrollment = %d, want reset to 0", f.restartCount)
	}
	if f.lastRestart != nil {
		t.Errorf("agent_last_restart_at after re-enrollment = %v, want nil", f.lastRestart)
	}
	if f.processStarted == nil {
		t.Error("agent_process_started_at after re-enrollment is nil, want stamped to now()")
	}
	if f.disconnectedAt != nil {
		t.Errorf("agent_disconnected_at after re-enrollment = %v, want nil", f.disconnectedAt)
	}
}
