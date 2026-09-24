package session

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
)

func TestInitialSteamHomeSeedIsRecordedOnceFromAssignedHost(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	ctx := context.Background()
	res, err := coord.Launch(ctx, s.userID, s.appID, StreamOverride{})
	if err != nil {
		t.Fatal(err)
	}
	id := res.Session.ID
	coord.AgentState(ctx, s.hostID, agentws.SessionStateMsg{SessionID: id, State: "starting"})
	copySeed := json.RawMessage(`{"mode":"copy","reason":"seeded"}`)
	coord.AgentState(ctx, s.hostID, agentws.SessionStateMsg{SessionID: id, State: "starting", HomeSeed: copySeed})
	coord.AgentState(ctx, s.hostID, agentws.SessionStateMsg{SessionID: id, State: "starting", HomeSeed: copySeed})
	reflink := json.RawMessage(`{"mode":"reflink","reason":"seeded"}`)
	coord.AgentState(ctx, s.hostID, agentws.SessionStateMsg{SessionID: id, State: "starting", HomeSeed: reflink})
	got, err := store.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !sameHomeSeed(got.HomeSeed, copySeed) {
		t.Fatalf("first accepted seed must win: %s", got.HomeSeed)
	}
	if !sameHomeSeed(toSessionResp(got).HomeSeed, copySeed) {
		t.Fatal("operator read did not expose accepted seed")
	}
	coord.AgentState(ctx, s.hostID, agentws.SessionStateMsg{SessionID: id, State: "running"})
	coord.AgentState(ctx, s.hostID, agentws.SessionStateMsg{SessionID: id, State: "starting", HomeSeed: reflink})
	got, _ = store.Get(ctx, id)
	if !sameHomeSeed(got.HomeSeed, copySeed) {
		t.Fatal("late or swap-era seed rewrote the initial launch outcome")
	}
}

func sameHomeSeed(a, b json.RawMessage) bool {
	var left, right map[string]string
	return json.Unmarshal(a, &left) == nil && json.Unmarshal(b, &right) == nil &&
		left["mode"] == right["mode"] && left["reason"] == right["reason"]
}

func TestInvalidSeedCannotDropLifecycleOrLeakRawValue(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	ctx := context.Background()
	res, err := coord.Launch(ctx, s.userID, s.appID, StreamOverride{})
	if err != nil {
		t.Fatal(err)
	}
	id := res.Session.ID
	for _, raw := range []string{`"/private/home"`, `{"mode":"copy","reason":"/private/home"}`, `{"mode":"copy","reason":"seeded","path":"/private/home"}`} {
		coord.AgentState(ctx, s.hostID, agentws.SessionStateMsg{SessionID: id, State: "starting", HomeSeed: json.RawMessage(raw)})
	}
	got, _ := store.Get(ctx, id)
	if got.State != StateStarting || got.HomeSeed != nil {
		t.Fatalf("bad seed affected lifecycle or persisted: %+v", got)
	}
	coord.AgentState(ctx, s.hostID, agentws.SessionStateMsg{SessionID: id, State: "running"})
	got, _ = store.Get(ctx, id)
	if got.State != StateRunning || got.HomeSeed != nil {
		t.Fatalf("bad seed affected running transition: %+v", got)
	}
}

func TestOtherHostAndOlderAgentCannotInventSteamSeed(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	ctx := context.Background()
	res, err := coord.Launch(ctx, s.userID, s.appID, StreamOverride{})
	if err != nil {
		t.Fatal(err)
	}
	id := res.Session.ID
	coord.AgentState(ctx, "00000000-0000-0000-0000-000000000001", agentws.SessionStateMsg{SessionID: id, State: "starting", HomeSeed: json.RawMessage(`{"mode":"reflink","reason":"seeded"}`)})
	coord.AgentState(ctx, s.hostID, agentws.SessionStateMsg{SessionID: id, State: "starting"})
	coord.AgentState(ctx, s.hostID, agentws.SessionStateMsg{SessionID: id, State: "running"})
	got, _ := store.Get(ctx, id)
	if got.HomeSeed != nil || toSessionResp(got).HomeSeed != nil {
		t.Fatal("older agent or wrong host acquired a seed outcome")
	}
}
