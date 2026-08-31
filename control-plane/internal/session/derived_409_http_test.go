package session

// steam-library-discovery Phase 3 — the two 409s, asserted at the HTTP layer
// because their BODIES are the contract, not just their status codes.
//
//	home_in_use          §2.1 — carries the conflicting session id so the client
//	                     can offer "go to your running session" instead of a
//	                     generic launch failure for an app the user did not click.
//	home_not_provisioned §3/§5 — a derived tile launched by a user with no home
//	                     for its parent on any host, and NO home is created.
//
// The status-code mapping alone is covered by the store-level tests; what only an
// HTTP test can prove is that `session_id` is nested INSIDE the error object and
// is omitted (never emitted empty) when the guard could not name one.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/auth"
	"github.com/accreleus/quasar/control-plane/internal/storage"
)

// newDerived409Server wires the real handler over a coordinator that HAS a
// storage provider — without one, a managed-home launch fails for an unrelated
// reason and neither 409 is reachable.
func newDerived409Server(t *testing.T, pool *pgxpool.Pool) (*httptest.Server, *auth.Service) {
	t.Helper()
	authSvc, err := auth.NewService(pool, auth.DefaultParams(), time.Hour)
	if err != nil {
		t.Fatalf("auth service: %v", err)
	}
	store := NewStore(pool)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger(),
		WithHomeProvider(storage.NewLocal(pool, testHomeRoot)))

	mux := http.NewServeMux()
	authHandler := auth.NewHandler(authSvc)
	authHandler.Register(mux)
	NewHandler(coord, store).Register(mux, authHandler.RequireAuth, authHandler.RequireAdmin)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, authSvc
}

// errorBody is the parsed error envelope, with the Phase 3 extension.
type derived409Body struct {
	Error struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		SessionID string `json:"session_id"`
	} `json:"error"`
	// Raw keeps the error object as a map so a test can assert a field's ABSENCE,
	// which a typed struct cannot distinguish from an empty string.
	Raw map[string]any `json:"-"`
}

func launchHTTP(t *testing.T, srv, token, appID string) (*http.Response, derived409Body) {
	t.Helper()
	var buf bytes.Buffer
	_ = json.NewEncoder(&buf).Encode(map[string]any{"app_id": appID})
	req, _ := http.NewRequest(http.MethodPost, srv+"/v1/sessions", &buf)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/sessions: %v", err)
	}
	defer resp.Body.Close()

	var typed derived409Body
	var raw map[string]any
	dec := json.NewDecoder(resp.Body)
	var all json.RawMessage
	if err := dec.Decode(&all); err != nil {
		t.Fatalf("read body: %v", err)
	}
	_ = json.Unmarshal(all, &typed)
	_ = json.Unmarshal(all, &raw)
	if errObj, ok := raw["error"].(map[string]any); ok {
		typed.Raw = errObj
	}
	return resp, typed
}

// registerUser registers a user and returns a bearer token plus the user id.
func registerUser(t *testing.T, ctx context.Context, authSvc *auth.Service, email, username string) (string, string) {
	t.Helper()
	u, err := authSvc.Register(ctx, email, username, "password12345")
	if err != nil {
		t.Fatalf("register %s: %v", username, err)
	}
	tok, err := authSvc.Login(ctx, email, "password12345", "")
	if err != nil {
		t.Fatalf("login %s: %v", username, err)
	}
	return tok.Plaintext, u.ID
}

// TestLaunchHomeInUse409CarriesTheSessionID is §2.1 and §2.2 at the wire.
//
// Under operator decision 2 the Steam app stays in the library as a Launcher tile
// beside the games derived from it, which makes the single-writer rule
// user-visible for the first time. The user clicks a game and is told a different
// app is in the way; without the session id in the body the client can only
// render a generic error toast, which reads as a bug.
func TestLaunchHomeInUse409CarriesTheSessionID(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newDerived409Server(t, pool)
	ctx := context.Background()

	s := seed(t, pool, 8)
	token, userID := registerUser(t, ctx, authSvc, "hiu@test.local", "hiuuser")
	parent := seedSteamApp(t, pool, `{"image":"steam:1"}`)
	tile := seedTile(t, pool, parent, "Hades", "1145360")
	provisionHome(t, pool, userID, parent, s.hostID)

	// The Launcher tile goes live.
	resp, _ := launchHTTP(t, srv.URL, token, parent)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("launch the parent: want 201, got %d", resp.StatusCode)
	}
	var liveID string
	if err := pool.QueryRow(ctx,
		`SELECT id::text FROM sessions WHERE user_id::text = $1 ORDER BY created_at DESC LIMIT 1`,
		userID).Scan(&liveID); err != nil {
		t.Fatalf("read the live session: %v", err)
	}

	// The game tile is refused, and the body names the live session.
	resp, body := launchHTTP(t, srv.URL, token, tile)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("launch the tile: want 409, got %d (%+v)", resp.StatusCode, body)
	}
	if body.Error.Code != "home_in_use" {
		t.Errorf("error.code = %q, want home_in_use", body.Error.Code)
	}
	if body.Error.SessionID != liveID {
		t.Errorf("error.session_id = %q, want the live session %q — the client links to it (§2.2)",
			body.Error.SessionID, liveID)
	}
	if _, nested := body.Raw["session_id"]; !nested {
		t.Error("session_id is not nested INSIDE the error object; control-api.md's envelope requires it there")
	}
}

// TestLaunchHomeInUse409OmitsAnUnknownSessionID pins the "omitted, never empty"
// rule: a client branches on the field's PRESENCE, and an empty string would make
// it render a link to nowhere. The unrelated-quota-style path that returns a bare
// ErrHomeInUse is exercised through the swap guard, which returns the sentinel
// when it has no id to give.
func TestLaunchHomeInUse409OmitsAnUnknownSessionID(t *testing.T) {
	w := httptest.NewRecorder()
	writeHomeInUse(w, ErrHomeInUse, "generic")

	var raw map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("parse body: %v", err)
	}
	errObj, ok := raw["error"].(map[string]any)
	if !ok {
		t.Fatalf("no error object: %s", w.Body.String())
	}
	if _, present := errObj["session_id"]; present {
		t.Errorf("session_id is present (%v) for an error that names no session; it must be OMITTED, "+
			"never emitted empty, so a client never renders a link to nowhere", errObj["session_id"])
	}
	if errObj["code"] != "home_in_use" {
		t.Errorf("code = %v, want home_in_use", errObj["code"])
	}
}

// TestLaunchHomeNotProvisioned409 is §5's refusal, with the assertion that
// matters most on the row count rather than the status: EnsureHome would have
// created a home here, mounted an empty directory, and let the session reach
// `running` looking perfectly healthy with the game absent.
func TestLaunchHomeNotProvisioned409(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newDerived409Server(t, pool)
	ctx := context.Background()

	seed(t, pool, 8)
	token, userID := registerUser(t, ctx, authSvc, "hnp@test.local", "hnpuser")
	parent := seedSteamApp(t, pool, `{"image":"steam:1"}`)
	tile := seedTile(t, pool, parent, "Hades", "1145360")
	// NO home: this user has never launched Steam.

	resp, body := launchHTTP(t, srv.URL, token, tile)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("launch a tile with no home: want 409, got %d (%+v)", resp.StatusCode, body)
	}
	if body.Error.Code != "home_not_provisioned" {
		t.Errorf("error.code = %q, want home_not_provisioned (its own code: the remedy is an action "+
			"on a DIFFERENT app, which a client cannot derive from a message string)", body.Error.Code)
	}

	var homes, sessions int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM user_homes`).Scan(&homes); err != nil {
		t.Fatalf("count homes: %v", err)
	}
	if homes != 0 {
		t.Errorf("user_homes rows = %d, want 0 — the refused launch must create no home", homes)
	}
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM sessions WHERE user_id::text = $1`, userID).Scan(&sessions); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessions != 0 {
		t.Errorf("sessions = %d, want 0 — the refusal is BEFORE placement, so nothing is reserved", sessions)
	}
}

// TestSwapIntoATileWithNoHomeIs409NotA500 is review finding 4.
//
// A swap is pinned to the LIVE session's host — there is no placement step to
// re-pin it — so "the user swaps into a tile whose library lives on another host,
// or does not exist yet" is an ordinary, user-correctable condition. Without the
// mapping it fell through to 500 internal and reported that as a server fault.
// The sentinel always survived swapper.Swap's %w wrap; only the case was missing.
func TestSwapIntoATileWithNoHomeIs409NotA500(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newDerived409Server(t, pool)
	ctx := context.Background()

	s := seed(t, pool, 8)
	token, userID := registerUser(t, ctx, authSvc, "swap409@test.local", "swap409")
	parent := seedSteamApp(t, pool, `{"image":"steam:1"}`)
	tile := seedTile(t, pool, parent, "Hades", "1145360")
	// NO home for this user anywhere.

	// A live, swappable session on an ordinary app, reserving enough slots that the
	// swap is not refused by ErrSwapExceedsReservation first.
	store := NewStore(pool)
	p := CreateParams{
		UserID: userID, AppID: s.appID,
		Width: 1280, Height: 720, FPS: 60, BitrateKbps: 6000,
		H264Profile: "constrained-baseline", NeedEncodeSlots: 2,
		TokenExpires: time.Now().Add(time.Minute),
	}
	sess, err := store.ScheduleAndCreate(ctx, p)
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	if _, err := store.Transition(ctx, sess.ID, StateRunning, nil, nil); err != nil {
		t.Fatalf("→ running: %v", err)
	}

	var buf bytes.Buffer
	_ = json.NewEncoder(&buf).Encode(map[string]any{"app_id": tile})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/sessions/"+sess.ID+"/swap", &buf)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("swap request: %v", err)
	}
	defer resp.Body.Close()
	var body derived409Body
	_ = json.NewDecoder(resp.Body).Decode(&body)

	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("swap into a tile with no home: want 409, got %d (%+v).\n"+
			"A user swapping into a game whose library is not on this host is a routine "+
			"mistake, not a server fault.", resp.StatusCode, body)
	}
	if body.Error.Code != "home_not_provisioned" {
		t.Errorf("error.code = %q, want home_not_provisioned", body.Error.Code)
	}
	// And no home was invented on the way past.
	var homes int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM user_homes`).Scan(&homes); err != nil {
		t.Fatalf("count homes: %v", err)
	}
	if homes != 0 {
		t.Errorf("user_homes rows = %d, want 0", homes)
	}
}

// TestLaunchingTheParentStillWorksWithNoTiles is the negative control for both
// 409s: an ordinary managed-home launch by a user with no home creates one and
// succeeds, exactly as it did before Phase 3. RequireHome's refusal must be
// reachable ONLY from a derived tile.
func TestLaunchingTheParentStillWorksWithNoTiles(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newDerived409Server(t, pool)
	ctx := context.Background()

	seed(t, pool, 8)
	token, userID := registerUser(t, ctx, authSvc, "ctrl@test.local", "ctrluser")
	parent := seedSteamApp(t, pool, `{"image":"steam:1"}`)

	resp, body := launchHTTP(t, srv.URL, token, parent)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("launch the parent with no home: want 201, got %d (%+v)", resp.StatusCode, body)
	}
	var homes int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM user_homes WHERE user_id::text = $1`, userID).Scan(&homes); err != nil {
		t.Fatalf("count homes: %v", err)
	}
	if homes != 1 {
		t.Errorf("user_homes rows = %d, want 1 — EnsureHome still provisions for an ORDINARY app", homes)
	}
}
