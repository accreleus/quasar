package session

// P5-02 integration tests: home-mount injection at the two dispatch sites.
//
// Requires a real Postgres (TEST_DATABASE_URL). Verifies:
//   - managed app → exactly one injected mount on session_assign
//   - managed app → exactly one injected mount on session_swap_app
//   - non-managed app → dispatched spec byte-identical to runtime_spec
//   - user_homes row upserted with correct host pinning + provider
//   - managed app with no provider configured → launch fails, reservation freed

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
	"github.com/accreleus/quasar/control-plane/internal/storage"
)

// testHomeRoot is the fixed local-driver root used across this package's home
// fixtures (storage.NewLocal). The control plane never touches a host
// filesystem (invariant #1) — EnsureHome only synthesizes a string — so a
// literal, non-existent path is fine here; it just needs to be the SAME value
// everywhere a test computes an expected ref, since the local driver's ref is
// `{root}/{userSlug}/{appSlug}` (storage.go).
const testHomeRoot = "/data/test-homes"

// capturingDispatcher records the App payloads of assign/swap commands.
type capturingDispatcher struct {
	mu      sync.Mutex
	assigns []json.RawMessage
	swaps   []json.RawMessage
	saw     chan string
}

func newCapturingDispatcher() *capturingDispatcher {
	return &capturingDispatcher{saw: make(chan string, 16)}
}

func (f *capturingDispatcher) Send(string, any) error { return nil }

func (f *capturingDispatcher) SendWithAck(_ context.Context, _ string, _ string, v any) (agentws.AckResult, error) {
	f.mu.Lock()
	var kind string
	switch cmd := v.(type) {
	case agentws.SessionAssignCmd:
		kind = "assign"
		f.assigns = append(f.assigns, append(json.RawMessage(nil), cmd.App...))
	case agentws.SessionStartCmd:
		kind = "start"
	case agentws.SessionSwapAppCmd:
		kind = "swap"
		f.swaps = append(f.swaps, append(json.RawMessage(nil), cmd.App...))
	case agentws.SessionStopCmd:
		kind = "stop"
	}
	f.mu.Unlock()
	f.saw <- kind
	return agentws.AckResult{OK: true}, nil
}

func (f *capturingDispatcher) waitFor(t *testing.T, kind string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case k := <-f.saw:
			if k == kind {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s dispatch", kind)
		}
	}
}

// seedManagedApp adds a managed-home app reusing the standard seed's host/gpu.
func seedManagedApp(t *testing.T, pool *pgxpool.Pool, spec string) string {
	t.Helper()
	var id string
	must(t, pool.QueryRow(context.Background(), `INSERT INTO apps
		(name, default_vram_mb, default_encode_slots, default_width, default_height,
		 default_fps, default_bitrate_kbps, runtime_spec, managed_home, home_container_path)
		VALUES ('managed-app', 512, 1, 1280, 720, 30, 2000, $1, true, '/home/quasar')
		RETURNING id::text`, spec).Scan(&id))
	entitleAll(t, pool, id)
	return id
}

func mountsOf(t *testing.T, raw json.RawMessage) []string {
	t.Helper()
	var spec struct {
		Mounts []string `json:"mounts"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("parse dispatched spec: %v", err)
	}
	return spec.Mounts
}

func TestHomeMountInjectedOnAssign(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	managedApp := seedManagedApp(t, pool, `{"image":"img:1","mounts":["/pre:/existing:ro"]}`)
	disp := newCapturingDispatcher()
	coord := newTestCoordinator(t, store, disp, testLogger(),
		WithHomeProvider(storage.NewLocal(pool, testHomeRoot)))
	ctx := context.Background()

	res, err := coord.Launch(ctx, s.userID, managedApp, StreamOverride{})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	disp.waitFor(t, "assign")

	disp.mu.Lock()
	got := disp.assigns[0]
	disp.mu.Unlock()
	mounts := mountsOf(t, got)
	want := testHomeRoot + "/u/managed-app:/home/quasar:rw"
	if len(mounts) != 2 || mounts[0] != "/pre:/existing:ro" || mounts[1] != want {
		t.Errorf("mounts = %v, want [/pre:/existing:ro %s]", mounts, want)
	}
	// Other spec fields survive the re-marshal.
	var spec map[string]any
	_ = json.Unmarshal(got, &spec)
	if spec["image"] != "img:1" {
		t.Errorf("image = %v, want img:1", spec["image"])
	}

	// Bookkeeping row: pinned to the placement host, provider local.
	var provider, ref, hostID string
	must(t, pool.QueryRow(ctx, `SELECT provider, ref, host_id::text FROM user_homes
		WHERE user_id::text=$1 AND app_id::text=$2`, s.userID, managedApp).
		Scan(&provider, &ref, &hostID))
	if provider != "local" || !strings.HasPrefix(ref, testHomeRoot) {
		t.Errorf("row = (%s, %s), want (local, %s/…)", provider, ref, testHomeRoot)
	}
	if res.Session.HostID == nil || hostID != *res.Session.HostID {
		t.Errorf("home host %s != session host %v", hostID, res.Session.HostID)
	}
}

func TestNonManagedSpecUntouched(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	// Give the standard (non-managed) app a distinctive spec with odd spacing —
	// byte-identity proves the managed-home path never re-marshalled it.
	rawSpec := `{ "image":"plain:1",   "mounts": [],  "extra_unknown": {"k": 1} }`
	must2 := func(_ any, err error) {
		if err != nil {
			t.Fatalf("seed spec: %v", err)
		}
	}
	must2(pool.Exec(context.Background(),
		`UPDATE apps SET runtime_spec = $1 WHERE id::text = $2`, rawSpec, s.appID))

	disp := newCapturingDispatcher()
	coord := newTestCoordinator(t, store, disp, testLogger(),
		WithHomeProvider(storage.NewLocal(pool, testHomeRoot)))

	if _, err := coord.Launch(context.Background(), s.userID, s.appID, StreamOverride{}); err != nil {
		t.Fatalf("launch: %v", err)
	}
	disp.waitFor(t, "assign")

	// Postgres JSONB canonicalizes whitespace on storage, so compare against
	// what GetLaunchApp actually returns — the guarantee is the dispatch path
	// passes THOSE bytes through untouched (no re-marshal on the non-managed path).
	app, err := store.GetLaunchApp(context.Background(), s.appID)
	if err != nil {
		t.Fatalf("get app: %v", err)
	}
	disp.mu.Lock()
	got := string(disp.assigns[0])
	disp.mu.Unlock()
	if got != string(app.RuntimeSpec) {
		t.Errorf("non-managed spec was modified:\n got: %s\nwant: %s", got, string(app.RuntimeSpec))
	}
	// And the unknown field survived end-to-end.
	if !strings.Contains(got, "extra_unknown") {
		t.Errorf("unknown field dropped from dispatched spec: %s", got)
	}
	// And no bookkeeping row appeared.
	var n int
	must(t, pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM user_homes WHERE user_id::text=$1`, s.userID).Scan(&n))
	if n != 0 {
		t.Errorf("user_homes rows = %d, want 0 for non-managed app", n)
	}
}

func TestHomeMountInjectedOnSwap(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	managedApp := seedManagedApp(t, pool, `{"image":"swap-target:1"}`)
	disp := newCapturingDispatcher()
	coord := newTestCoordinator(t, store, disp, testLogger(),
		WithHomeProvider(storage.NewLocal(pool, testHomeRoot)))
	ctx := context.Background()

	sess := runningSession(t, store, s)

	if _, err := coord.Swap(ctx, sess.ID, managedApp); err != nil {
		t.Fatalf("swap: %v", err)
	}
	disp.waitFor(t, "swap")

	disp.mu.Lock()
	got := disp.swaps[0]
	disp.mu.Unlock()
	mounts := mountsOf(t, got)
	want := testHomeRoot + "/u/managed-app:/home/quasar:rw"
	if len(mounts) != 1 || mounts[0] != want {
		t.Errorf("swap mounts = %v, want [%s]", mounts, want)
	}
}

// TestReportBytesUsedOnAgentMetrics verifies that a pre-terminal session_metrics
// sample carrying bytes_used updates user_homes.bytes_used (P5-03).
func TestReportBytesUsedOnAgentMetrics(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	managedApp := seedManagedApp(t, pool, `{"image":"img:1"}`)
	disp := newCapturingDispatcher()
	coord := newTestCoordinator(t, store, disp, testLogger(),
		WithHomeProvider(storage.NewLocal(pool, testHomeRoot)))
	ctx := context.Background()

	// Launch creates the user_homes row via EnsureHome in the assign dispatch.
	res, err := coord.Launch(ctx, s.userID, managedApp, StreamOverride{})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	disp.waitFor(t, "assign")

	// Transition to running so AgentMetrics does not drop the sample.
	if _, err = store.Transition(ctx, res.Session.ID, StateRunning, nil, nil); err != nil {
		t.Fatalf("→ running: %v", err)
	}

	hostID := ""
	if res.Session.HostID != nil {
		hostID = *res.Session.HostID
	}

	bu := int64(8192)
	coord.AgentMetrics(ctx, hostID, agentws.SessionMetricsMsg{
		SessionID: res.Session.ID,
		BytesUsed: &bu,
	})

	// ReportBytesUsed runs in a best-effort goroutine; give it time to land.
	time.Sleep(200 * time.Millisecond)

	var got int64
	must(t, pool.QueryRow(ctx, `SELECT bytes_used FROM user_homes
		WHERE user_id::text=$1 AND app_id::text=$2`, s.userID, managedApp).Scan(&got))
	if got != bu {
		t.Errorf("user_homes.bytes_used = %d, want %d", got, bu)
	}
}

func TestManagedHomeWithoutProviderFailsLaunch(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	managedApp := seedManagedApp(t, pool, `{"image":"img:1"}`)
	disp := newCapturingDispatcher()
	coord := newTestCoordinator(t, store, disp, testLogger()) // no provider

	_, err := coord.Launch(context.Background(), s.userID, managedApp, StreamOverride{})
	if err == nil {
		t.Fatal("expected launch to fail without a storage provider")
	}
	// The session must be failed (reservation released), not left assigned.
	var state string
	must(t, pool.QueryRow(context.Background(), `SELECT state FROM sessions
		WHERE user_id::text = $1 ORDER BY created_at DESC LIMIT 1`, s.userID).Scan(&state))
	if state != "failed" {
		t.Errorf("session state = %s, want failed", state)
	}
}
