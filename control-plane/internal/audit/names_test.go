package audit

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"
)

func detailsOf(t *testing.T, v map[string]any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal details: %v", err)
	}
	return b
}

func ptr(s string) *string { return &s }

func sortRefs(refs []ref) []ref {
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].kind != refs[j].kind {
			return refs[i].kind < refs[j].kind
		}
		return refs[i].id < refs[j].id
	})
	return refs
}

const (
	appUUID     = "8b1116c8-fc33-4c01-9110-11601bab6be7"
	hostUUID    = "4daeaa27-8f6f-4dad-aaae-475f1d304916"
	userUUID    = "11111111-2222-3333-4444-555555555555"
	sessionUUID = "85d0b6a9-0000-4000-8000-000000000001"
	otherUUID   = "99999999-8888-4777-a666-555555555555"
)

func TestItemRefsResolvesTargetAndAllowlistedDetailIDs(t *testing.T) {
	// The row from the operator's screenshot: the target names the session, and
	// the two ids the Detail pane showed as bare uuids are both resolvable.
	got := itemRefs("session.launched", "session", ptr(sessionUUID), map[string]any{
		"app_id": appUUID, "host_id": hostUUID,
	})
	want := []ref{
		{kindApp, appUUID}, {kindHost, hostUUID}, {kindSession, sessionUUID},
	}
	if !reflect.DeepEqual(sortRefs(got), sortRefs(want)) {
		t.Fatalf("refs = %+v, want %+v", got, want)
	}
}

func TestItemRefsIgnoresIDKeysWithNoNameToShow(t *testing.T) {
	// entitlement_id/attempt_id/capture_id are uuids that name nothing; a
	// uuid-shape heuristic would query for all three.
	got := itemRefs("session.capture", "session", ptr(sessionUUID), map[string]any{
		"capture_id": otherUUID, "kind": "pipeline_dot",
	})
	want := []ref{{kindSession, sessionUUID}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("refs = %+v, want %+v", got, want)
	}
}

func TestItemRefsIgnoresUUIDsInsideFreeText(t *testing.T) {
	// session.failed writes operator-facing prose into reason/state_detail. A
	// heuristic that resolved anything uuid-shaped would query on it.
	got := itemRefs("session.failed", "session", ptr(sessionUUID), map[string]any{
		"reason":       "agent " + hostUUID + " went away",
		"state_detail": otherUUID,
		"host_id":      hostUUID,
	})
	want := []ref{{kindHost, hostUUID}, {kindSession, sessionUUID}}
	if !reflect.DeepEqual(sortRefs(got), sortRefs(want)) {
		t.Fatalf("refs = %+v, want %+v", got, want)
	}
}

func TestItemRefsSubjectIDOnlyWhenSubjectTypeIsUser(t *testing.T) {
	base := map[string]any{"subject_id": userUUID, "subject_type": "user"}
	if got := itemRefs("app.entitlement.grant", "app", ptr(appUUID), base); len(got) != 2 {
		t.Fatalf("subject_type=user should resolve the subject, got %+v", got)
	}
	notUser := map[string]any{"subject_id": userUUID, "subject_type": "all"}
	got := itemRefs("app.entitlement.grant", "app", ptr(appUUID), notUser)
	if !reflect.DeepEqual(got, []ref{{kindApp, appUUID}}) {
		t.Fatalf("subject_type=all must not resolve a user, got %+v", got)
	}
}

func TestItemRefsRunIDPicksItsTableFromTheAction(t *testing.T) {
	// The same key names two tables. platform.apply.cancel's run is a platform
	// apply run; job.run's is a job_runs row, which has no name of its own.
	got := itemRefs("platform.apply.cancel", "platform", ptr(otherUUID), map[string]any{"run_id": otherUUID})
	if !reflect.DeepEqual(got, []ref{{kindApplyRun, otherUUID}, {kindApplyRun, otherUUID}}) {
		t.Fatalf("cancel refs = %+v", got)
	}
	got = itemRefs("job.run", "job", ptr("platform.release_detect"), map[string]any{"run_id": otherUUID, "host_id": ""})
	if !reflect.DeepEqual(got, []ref{{kindJob, "platform.release_detect"}}) {
		t.Fatalf("job.run refs = %+v, want the job only", got)
	}
}

func TestItemRefsNeverSendsANonUUIDToAUUIDTable(t *testing.T) {
	// A text id reaching a `::uuid` cast errors the whole query rather than
	// missing, so the shape check is the guard, not an optimisation.
	if got := itemRefs("host.drain", "host", ptr("not-a-uuid"), nil); got != nil {
		t.Fatalf("refs = %+v, want none", got)
	}
	// A text-keyed table is unaffected by the shape check.
	got := itemRefs("stream_profile.update", "stream_profile", ptr("4k120"), nil)
	if !reflect.DeepEqual(got, []ref{{kindStreamProfile, "4k120"}}) {
		t.Fatalf("refs = %+v", got)
	}
}

func TestTargetKindDisambiguatesPlatformAndMapsLibrary(t *testing.T) {
	if k, _ := targetKind("platform.apply.cancel", "platform"); k != kindApplyRun {
		t.Errorf("cancel target = %s, want the apply run", k)
	}
	if k, _ := targetKind("platform.apply.run", "platform"); k != kindRelease {
		t.Errorf("run target = %s, want the release", k)
	}
	// library.scan.force records target_type=library carrying an app id.
	if k, _ := targetKind("library.scan.force", "library"); k != kindApp {
		t.Errorf("library target = %s, want the app", k)
	}
	for _, tt := range []string{"invite", "secret", "instance", "storage_home"} {
		if _, ok := targetKind("whatever", tt); ok {
			t.Errorf("%s has no name to resolve, but targetKind claims one", tt)
		}
	}
}

func TestCollectRefsDeduplicates(t *testing.T) {
	items := []Item{
		{Action: "session.launched", TargetType: "session", TargetID: ptr(sessionUUID),
			Details: detailsOf(t, map[string]any{"app_id": appUUID, "host_id": hostUUID})},
		{Action: "host.drain", TargetType: "host", TargetID: ptr(hostUUID),
			Details: detailsOf(t, map[string]any{"force": true})},
	}
	got := collectRefs(items)
	if len(got) != 3 {
		t.Fatalf("collectRefs = %+v, want 3 unique refs (the host appears twice)", got)
	}
}

func TestAttachNamesFallsBackToTheStampedNameForADeletedTarget(t *testing.T) {
	// app.delete stamps `name` precisely because the row is gone by read time.
	items := []Item{{
		Action: "app.delete", TargetType: "app", TargetID: ptr(appUUID),
		Details: detailsOf(t, map[string]any{"name": "Steam", "delete_derived": true}),
	}}
	attachNames(items, nil)
	if got := items[0].Names[appUUID]; got != "Steam" {
		t.Fatalf("names[%s] = %q, want the stamped name", appUUID, got)
	}
}

func TestAttachNamesPrefersTheCurrentNameOverTheStampedOne(t *testing.T) {
	// The log is append-only; a rename must show the current name.
	items := []Item{{
		Action: "host.uncordon", TargetType: "host", TargetID: ptr(hostUUID),
		Details: detailsOf(t, map[string]any{"node_name": "old-name"}),
	}}
	attachNames(items, map[ref]string{{kindHost, hostUUID}: "gpu-test"})
	if got := items[0].Names[hostUUID]; got != "gpu-test" {
		t.Fatalf("names[%s] = %q, want the resolved name", hostUUID, got)
	}
}

func TestAttachNamesIsAlwaysNonNil(t *testing.T) {
	items := []Item{{Action: "instance.settings.updated", TargetType: "instance"}}
	attachNames(items, nil)
	if items[0].Names == nil {
		t.Fatal("Names must be non-nil so a client can index it without a guard")
	}
	if len(items[0].Names) != 0 {
		t.Fatalf("Names = %v, want empty", items[0].Names)
	}
}

func TestAttachNamesDoesNotStampANameOntoADetailID(t *testing.T) {
	// The stamped-name fallback is for the TARGET only. storage.home.tombstone
	// stamps username AND app_name; neither names the home the row targets, and
	// neither may be attributed to some other id on the row.
	items := []Item{{
		Action: "storage.home.tombstone", TargetType: "storage_home", TargetID: ptr(otherUUID),
		Details: detailsOf(t, map[string]any{"username": "kenji", "app_name": "Steam"}),
	}}
	attachNames(items, nil)
	// storage_home has no resolvable kind, so the target still takes the
	// fallback - but nothing else on the row does.
	if len(items[0].Names) != 1 || items[0].Names[otherUUID] != "kenji" {
		t.Fatalf("Names = %v", items[0].Names)
	}
}

func TestEveryResolvableKindHasAQuery(t *testing.T) {
	kinds := map[kind]bool{}
	for _, k := range targetKinds {
		kinds[k] = true
	}
	for _, k := range detailKinds {
		kinds[k] = true
	}
	kinds[kindApplyRun] = true // reached via targetKind's platform branch
	for k := range kinds {
		if _, ok := nameQueries[k]; !ok {
			t.Errorf("kind %q can be referenced but has no lookup query", k)
		}
	}
}

func TestTrimNameBoundsAPathologicalName(t *testing.T) {
	long := ""
	for range 200 {
		long += "x"
	}
	if got := trimName(long); len(got) != 120 {
		t.Fatalf("len = %d, want 120", len(got))
	}
	if got := trimName("  spaced  "); got != "spaced" {
		t.Fatalf("trimName = %q", got)
	}
}

func TestTrimNameCutsOnARuneBoundary(t *testing.T) {
	// 3-byte runes: byte 120 lands mid-rune, and a byte slice there would
	// serialize as U+FFFD.
	long := strings.Repeat("日", 60)
	got := trimName(long)
	if !utf8.ValidString(got) {
		t.Fatalf("trimName produced invalid UTF-8: %q", got)
	}
	if len(got) > 120 {
		t.Fatalf("len = %d, want <= 120", len(got))
	}
}
