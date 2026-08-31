package audit

import (
	"encoding/json"
	"reflect"
	"testing"
)

// patchReq mirrors the shape every admin PATCH payload in this repo uses:
// all-pointer, plus json.RawMessage for the JSONB columns.
type patchReq struct {
	Name        *string         `json:"name"`
	Description *string         `json:"description"`
	Image       *string         `json:"image"`
	Args        json.RawMessage `json:"args"`
	Env         json.RawMessage `json:"env"`
	ManagedHome *bool           `json:"managed_home"`
	Internal    string          `json:"-"`
	unexported  *string         //nolint:unused // present to prove it is skipped
}

func strPtr(s string) *string { return &s }

// An empty PATCH must produce an EMPTY key list, not a list of every field.
// A misleading key list is worse than no detail at all: it would tell an
// operator that fields changed which the request never carried.
func TestChangedKeys_UnchangedPatchReportsNothing(t *testing.T) {
	got := ChangedKeys(patchReq{})
	if len(got) != 0 {
		t.Fatalf("empty patch reported changed keys %v, want none", got)
	}
}

func TestChangedKeys_ReportsOnlyCarriedFields(t *testing.T) {
	yes := true
	req := patchReq{
		Name:        strPtr("bench"),
		Env:         json.RawMessage(`{"A":"1"}`),
		ManagedHome: &yes,
	}
	want := []string{"env", "managed_home", "name"} // sorted
	if got := ChangedKeys(req); !reflect.DeepEqual(got, want) {
		t.Fatalf("ChangedKeys = %v, want %v", got, want)
	}
}

// An explicit JSON null decodes to the 4 bytes "null" in a json.RawMessage, and
// the handlers reject that — but if one ever let it through it IS a carried
// field, so it must be reported.
func TestChangedKeys_NonEmptyRawMessageCounts(t *testing.T) {
	got := ChangedKeys(patchReq{Args: json.RawMessage(`null`)})
	if !reflect.DeepEqual(got, []string{"args"}) {
		t.Fatalf("ChangedKeys = %v, want [args]", got)
	}
	if got := ChangedKeys(patchReq{Args: json.RawMessage{}}); len(got) != 0 {
		t.Fatalf("empty RawMessage reported %v, want none", got)
	}
}

func TestChangedKeys_SkipsDashTaggedAndUnexported(t *testing.T) {
	got := ChangedKeys(patchReq{Internal: "x", Name: strPtr("n")})
	if !reflect.DeepEqual(got, []string{"name"}) {
		t.Fatalf("ChangedKeys = %v, want [name]", got)
	}
}

func TestChangedKeys_IgnoreList(t *testing.T) {
	type withID struct {
		ID   *string `json:"id"`
		Name *string `json:"name"`
	}
	req := withID{ID: strPtr("bench"), Name: strPtr("Bench")}
	if got := ChangedKeys(req, "id"); !reflect.DeepEqual(got, []string{"name"}) {
		t.Fatalf("ChangedKeys with ignore = %v, want [name]", got)
	}
}

func TestChangedKeys_AcceptsPointerToStruct(t *testing.T) {
	req := &patchReq{Image: strPtr("img")}
	if got := ChangedKeys(req); !reflect.DeepEqual(got, []string{"image"}) {
		t.Fatalf("ChangedKeys = %v, want [image]", got)
	}
	if got := ChangedKeys((*patchReq)(nil)); got != nil {
		t.Fatalf("nil pointer returned %v, want nil", got)
	}
	if got := ChangedKeys("not a struct"); got != nil {
		t.Fatalf("non-struct returned %v, want nil", got)
	}
}

// The whole point of recording field NAMES rather than values: a maximally
// changed payload must stay far under admin_activity's 4096-byte CHECK
// (migration 0028). streamProfileReq is the widest PATCH body in the repo at 20
// fields; this asserts the worst case with room to spare.
func TestChangedKeys_MaximalPayloadFitsTheCheck(t *testing.T) {
	// Every field of the widest admin PATCH body, carried.
	type widest struct {
		A *string `json:"id"`
		B *string `json:"display_name"`
		C *string `json:"codec"`
		D *int32  `json:"width"`
		E *int32  `json:"height"`
		F *int32  `json:"fps"`
		G *string `json:"h264_profile"`
		H *int32  `json:"nominal_bitrate_kbps"`
		I *int32  `json:"min_offer_bandwidth_kbps"`
		J *int32  `json:"recommended_offer_bandwidth_kbps"`
		K *string `json:"headroom_factor"`
		L *int32  `json:"abr_floor_kbps"`
		M *int32  `json:"max_startup_rtt_ms"`
		N *int32  `json:"min_decode_height"`
		O *string `json:"high_refresh_display"`
		P *bool   `json:"hardware_encoder_required"`
		Q *string `json:"browser_client"`
		R *int32  `json:"playout0_ms"`
		S *string `json:"visibility"`
		T *string `json:"home_container_path"`
	}
	v := reflect.New(reflect.TypeOf(widest{})).Elem()
	for i := 0; i < v.NumField(); i++ {
		v.Field(i).Set(reflect.New(v.Field(i).Type().Elem()))
	}
	keys := ChangedKeys(v.Interface())
	if len(keys) != v.NumField() {
		t.Fatalf("got %d keys, want %d", len(keys), v.NumField())
	}
	b, err := json.Marshal(map[string]any{"keys": keys})
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > 4096 {
		t.Fatalf("maximal key-list payload is %d bytes, over the 4096-byte CHECK", len(b))
	}
	t.Logf("maximal key-list payload: %d bytes", len(b))
}
