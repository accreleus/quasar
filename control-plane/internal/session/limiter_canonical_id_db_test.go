package session

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

type limiterRoute struct {
	name, method, path string
	body               any
}

func limiterRoutes() []limiterRoute {
	return []limiterRoute{
		{"stats", http.MethodPost, "/stats", map[string]any{"samples": []map[string]any{
			{"ts_unix_ms": nowMs(), "metrics": map[string]any{"fps": 60}},
		}}},
		{"trace events", http.MethodPost, "/trace/events", map[string]any{"events": []map[string]any{
			{"ts_unix_ms": nowMs(), "type": "tab_hidden", "payload": map[string]any{}},
		}}},
		{"trace clock", http.MethodPost, "/trace/clock", map[string]any{"client_offset_ms": 1.0, "uncertainty_ms": 1.0}},
		{"verdict", http.MethodGet, "/verdict", nil},
	}
}

// #1100: isValidUUID accepts both hex cases and the store casts ::uuid, so one
// session is reachable under many spellings. Every per-session limiter must
// share one bucket across them. Each case drains the burst with the lower-case
// id, then expects the upper-case spelling to be limited too.
func TestSessionLimitersIgnoreIDCasing(t *testing.T) {
	for _, tc := range limiterRoutes() {
		t.Run(tc.name, func(t *testing.T) {
			sid, ownerTok, _, _, srvURL, _ := verdictSession(t)
			upper := strings.ToUpper(sid)
			if upper == sid {
				t.Fatalf("session id %s has no hex letters to recase", sid)
			}
			call := func(id string) int {
				resp := doJSON(t, tc.method, srvURL+"/v1/sessions/"+id+tc.path, ownerTok, tc.body)
				resp.Body.Close()
				return resp.StatusCode
			}
			for i := 0; i < statsRateBurst; i++ {
				if got := call(sid); got == http.StatusTooManyRequests {
					t.Fatalf("request %d of the burst was limited", i+1)
				}
			}
			if got := call(upper); got != http.StatusTooManyRequests {
				t.Errorf("other casing after the burst: %d, want 429", got)
			}
			if got := call(sid); got != http.StatusTooManyRequests {
				t.Errorf("same casing after the burst: %d, want 429", got)
			}
		})
	}
}

// #1100: the coordinator's in-memory per-session state is keyed on the row's id,
// not on the spelling the caller used, or a repeat stop under another casing
// would restart the grace and the heartbeat re-send would miss the record.
func TestStopRecordsCanonicalSessionID(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	s := seed(t, pool, 4)
	sess, err := store.ScheduleAndCreate(context.Background(), launchParams(s))
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	_, _ = store.Transition(context.Background(), sess.ID, StateStarting, nil, nil)
	_, _ = store.Transition(context.Background(), sess.ID, StateRunning, nil, nil)

	if _, err := coord.Stop(context.Background(), strings.ToUpper(sess.ID), "user_requested"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if !coord.stopInGrace(sess.ID) {
		t.Errorf("no stop record under the canonical id %s", sess.ID)
	}
}

// A spelling the store refuses (no hyphens) is a 404 that touches no bucket: the
// canonical spelling is still unlimited afterwards.
func TestSessionLimitersLeaveBucketAloneOnRefusedSpelling(t *testing.T) {
	for _, tc := range limiterRoutes() {
		t.Run(tc.name, func(t *testing.T) {
			sid, ownerTok, _, _, srvURL, _ := verdictSession(t)
			bare := strings.ReplaceAll(sid, "-", "")
			call := func(id string) int {
				resp := doJSON(t, tc.method, srvURL+"/v1/sessions/"+id+tc.path, ownerTok, tc.body)
				resp.Body.Close()
				return resp.StatusCode
			}
			for i := 0; i < statsRateBurst+2; i++ {
				if got := call(bare); got != http.StatusNotFound {
					t.Fatalf("request %d with no hyphens: %d, want 404", i+1, got)
				}
			}
			if got := call(sid); got == http.StatusTooManyRequests || got >= 400 {
				t.Errorf("canonical spelling after the refused ones: %d, want it served", got)
			}
		})
	}
}
