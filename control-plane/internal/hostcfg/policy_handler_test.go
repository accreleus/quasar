package hostcfg

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/httpx"
)

type policyIdentityDispatcher struct {
	fakeDispatcher
	connection string
}

func TestInitialIdlePolicyEditorsRaceAtRevisionZero(t *testing.T) {
	pool := testPool(t)
	hostID := seedHost(t, pool)
	confirmPolicyGroups(t, pool, hostID, "idle_timeout_secs")
	h := NewHandler(NewStore(pool), &fakeDispatcher{}, nil)
	mux := http.NewServeMux()
	h.Register(mux, func(next http.Handler) http.Handler { return next }, func(next http.Handler) http.Handler { return next })
	url := "/v1/admin/hosts/" + hostID + "/policy"
	get := httptest.NewRecorder()
	mux.ServeHTTP(get, httptest.NewRequest(http.MethodGet, url, nil))
	if get.Code != http.StatusOK {
		t.Fatalf("initial read: %d %s", get.Code, get.Body.String())
	}
	var initial PolicyView
	if err := json.Unmarshal(get.Body.Bytes(), &initial); err != nil {
		t.Fatal(err)
	}
	group := initial.Groups["idle_timeout_secs"]
	if initial.Revision != "0" || group.Status != "pending" || group.DesiredDigest != nil {
		t.Fatalf("initial revisioned typed editor unavailable: %+v", initial)
	}
	start := make(chan struct{})
	results := make([]*httptest.ResponseRecorder, 2)
	var wg sync.WaitGroup
	for i, seconds := range []int{900, 1200} {
		wg.Add(1)
		go func(i, seconds int) {
			defer wg.Done()
			<-start
			body := fmt.Sprintf(`{"expected_revision":"0","changes":{"idle_timeout_secs":{"source":"explicit","value":%d}}}`, seconds)
			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPatch, url, strings.NewReader(body)))
			results[i] = rr
		}(i, seconds)
	}
	close(start)
	wg.Wait()
	var winners, stale int
	for _, rr := range results {
		switch rr.Code {
		case http.StatusOK:
			winners++
		case http.StatusConflict:
			stale++
			var conflict struct {
				Current PolicyView `json:"current"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &conflict); err != nil || conflict.Current.Revision != "1" {
				t.Fatalf("stale editor lacks current view: %s err=%v", rr.Body.String(), err)
			}
		default:
			t.Fatalf("unexpected race response: %d %s", rr.Code, rr.Body.String())
		}
	}
	if winners != 1 || stale != 1 {
		t.Fatalf("revision-zero race: winners=%d stale=%d", winners, stale)
	}
}

func TestInitialIdlePolicyReadDoesNotClaimMissingDeploymentBaseline(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	hostID := seedHost(t, pool)
	ctx := context.Background()
	connection := "00000000-0000-4000-8000-000000000161"
	if _, err := store.BeginPolicyConnection(ctx, hostID, connection, map[string]int{"typed_settings": 2, "deployment_baseline": 1}, []string{"idle_timeout_secs"}, true); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.ConfirmPolicyGroups(ctx, hostID, connection, []string{"idle_timeout_secs"}); err != nil || !ok {
		t.Fatalf("group echo: ok=%v err=%v", ok, err)
	}
	if err := store.ObserveDeploymentSettings(ctx, hostID, connection, json.RawMessage(`{"idle_timeout_secs":120}`)); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(store, &fakeDispatcher{}, nil)
	r := httptest.NewRequest(http.MethodGet, "/v1/admin/hosts/"+hostID+"/policy", nil)
	r.SetPathValue("id", hostID)
	w := httptest.NewRecorder()
	h.handleGetPolicy(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("policy read: %d %s", w.Code, w.Body.String())
	}
	var view PolicyView
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	group := view.Groups["idle_timeout_secs"]
	if view.Revision != "0" || group.Status != "pending" || group.DesiredDigest != nil || group.Remedy == nil || !strings.Contains(*group.Remedy, "No RH05 policy change has been saved") || strings.Contains(*group.Remedy, "baseline_unavailable") {
		t.Fatalf("initial policy read falsely requests baseline refresh: %+v", group)
	}
	var rowCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM host_setting_groups WHERE host_id=$1::uuid`, hostID).Scan(&rowCount); err != nil || rowCount != 0 {
		t.Fatalf("initial read persisted a group: count=%d err=%v", rowCount, err)
	}
}

func TestLegacyRestartIsAtomicWhileRH05JournalGateIsOpen(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	hostID := seedHost(t, pool)
	connection := "00000000-0000-4000-8000-000000000131"
	if _, err := store.BeginPolicyConnection(context.Background(), hostID, connection, map[string]int{"typed_settings": 2}, []string{"idle_timeout_secs"}, true); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(store, &fakeDispatcher{}, fakeCounter(0))
	r := httptest.NewRequest(http.MethodPatch, "/v1/admin/hosts/"+hostID+"/settings", strings.NewReader(`{"overrides":{"encoder":"va","gop":90},"restart_confirm":true}`))
	r.SetPathValue("id", hostID)
	w := httptest.NewRecorder()
	h.handlePatch(w, r)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "attempt_conflict") {
		t.Fatalf("legacy restart while gate open: %d %s", w.Code, w.Body.String())
	}
	overrides, err := store.Get(context.Background(), hostID)
	if err != nil || len(overrides) != 0 {
		t.Fatalf("mixed edit partially saved: %+v, err=%v", overrides, err)
	}
}

func TestLegacyOwnedRestartSettingReturnsNoImmediateRestart(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	hostID := seedHost(t, pool)
	confirmPolicyGroups(t, pool, hostID, "hardware")
	dispatcher := &fakeDispatcher{}
	h := NewHandler(store, dispatcher, fakeCounter(2))
	r := httptest.NewRequest(http.MethodPatch, "/v1/admin/hosts/"+hostID+"/settings", strings.NewReader(`{"overrides":{"encoder":"va"}}`))
	r.SetPathValue("id", hostID)
	w := httptest.NewRecorder()
	h.handlePatch(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"restart_triggered":false`) {
		t.Fatalf("typed-owned restart response: %d %s", w.Code, w.Body.String())
	}
	for _, command := range dispatcher.sent {
		if _, restart := command.(restartCmd); restart {
			t.Fatal("typed-owned setting sent a legacy restart")
		}
	}
}

func (d *policyIdentityDispatcher) PolicyIdentity(string) (string, string, bool) {
	return "boot", d.connection, true
}

func TestPolicyReadbackFreshnessRequiresCurrentConnection(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	hostID := seedHost(t, pool)
	confirmPolicyGroups(t, pool, hostID, "idle_timeout_secs")
	view, err := store.SavePolicy(context.Background(), hostID, "0", map[string]PolicyChoice{"idle_timeout_secs": {Source: "explicit", Value: float64(900)}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	group := view.Groups["idle_timeout_secs"]
	if ok, err := store.ObservePolicyApplied(context.Background(), hostID, "idle_timeout_secs", group.DesiredRevision, *group.DesiredDigest, "next_session", "00000000-0000-4000-8000-000000000001"); err != nil || !ok {
		t.Fatalf("observed: %v %v", ok, err)
	}
	dispatcher := &policyIdentityDispatcher{connection: "00000000-0000-4000-8000-000000000002"}
	h := NewHandler(store, dispatcher, nil)
	read := func() PolicyView {
		r := httptest.NewRequest(http.MethodGet, "/v1/admin/hosts/"+hostID+"/policy", nil)
		r.SetPathValue("id", hostID)
		w := httptest.NewRecorder()
		h.handleGetPolicy(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
		var result PolicyView
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	if read().Groups["idle_timeout_secs"].Fresh {
		t.Fatal("prior connection counted as fresh evidence")
	}
	dispatcher.connection = "00000000-0000-4000-8000-000000000001"
	if !read().Groups["idle_timeout_secs"].Fresh {
		t.Fatal("current connection evidence was not fresh")
	}
}

func TestPolicyOperatorSaveRejectsUnauthorizedInvalidAndStale(t *testing.T) {
	pool := testPool(t)
	hostID := seedHost(t, pool)
	confirmPolicyGroups(t, pool, hostID, "idle_timeout_secs")
	h := NewHandler(NewStore(pool), &fakeDispatcher{}, nil)
	mux := http.NewServeMux()
	identity := func(next http.Handler) http.Handler { return next }
	admin := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Test-Role") != "admin" {
				httpx.WriteError(w, http.StatusForbidden, httpx.CodeForbidden, "admin required")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
	h.Register(mux, identity, admin)
	url := "/v1/admin/hosts/" + hostID + "/policy"
	call := func(role, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPatch, url, bytes.NewBufferString(body))
		req.Header.Set("X-Test-Role", role)
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		return rr
	}
	good := `{"expected_revision":"0","changes":{"idle_timeout_secs":{"source":"explicit","value":900}}}`
	if rr := call("user", good); rr.Code != http.StatusForbidden {
		t.Fatalf("non-admin code %d", rr.Code)
	}
	if rr := call("admin", `{"expected_revision":"0","changes":{"idle_timeout_secs":{"source":"automatic"}}}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid code %d: %s", rr.Code, rr.Body.String())
	}
	if rr := call("admin", good); rr.Code != http.StatusOK {
		t.Fatalf("valid code %d: %s", rr.Code, rr.Body.String())
	}
	stale := call("admin", good)
	if stale.Code != http.StatusConflict {
		t.Fatalf("stale code %d: %s", stale.Code, stale.Body.String())
	}
	var conflict struct {
		Current     PolicyView `json:"current"`
		ChangedKeys []string   `json:"changed_keys"`
	}
	if err := json.Unmarshal(stale.Body.Bytes(), &conflict); err != nil {
		t.Fatal(err)
	}
	if conflict.Current.Revision != "1" || len(conflict.ChangedKeys) != 1 || conflict.ChangedKeys[0] != "idle_timeout_secs" {
		t.Fatalf("stale view %+v", conflict)
	}
	view, err := NewStore(pool).GetPolicy(context.Background(), hostID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Revision != "1" {
		t.Fatalf("rejected edit wrote revision %s", view.Revision)
	}
}
