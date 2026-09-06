package preparation

import (
	"encoding/json"
	"strings"
	"testing"
)

func testPolicy() Policy {
	return Policy{Revision: "2", Enabled: true, Images: []Image{{"steam", "ghcr.io/accreleus/quasar-steam@sha256:" + strings.Repeat("a", 64), "2026.09.02"}}}
}
func TestOfficialIdentity(t *testing.T) {
	p := testPolicy()
	if !ValidImage(p.Images[0]) {
		t.Fatal("official image rejected")
	}
	for _, ref := range []string{"ghcr.io/accreleus/quasar-steam:latest", "ghcr.io/other/quasar-steam@sha256:" + strings.Repeat("a", 64), "ghcr.io/accreleus/quasar-steam-extra@sha256:" + strings.Repeat("a", 64)} {
		i := p.Images[0]
		i.RegistryRef = ref
		if ValidImage(i) {
			t.Fatalf("accepted %s", ref)
		}
	}
}
func TestReportValidation(t *testing.T) {
	p := testPolicy()
	valid := Report{PolicyRevision: p.Revision, Images: []ImageReport{{Image: p.Images[0], PreparationEnabled: true, ConsumptionEnabled: true, State: "waiting_image", Reason: "image_not_ready"}}}
	if err := validReport(valid, p); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(*Report)
	}{
		{"future", func(r *Report) { r.PolicyRevision = "3" }},
		{"noncanonical", func(r *Report) { r.PolicyRevision = "02" }},
		{"negative", func(r *Report) { r.PolicyRevision = "-1" }},
		{"unadopted", func(r *Report) { r.Images[0].RegistryRef += "other" }},
		{"duplicate", func(r *Report) { r.Images = append(r.Images, r.Images[0]) }},
		{"unknownstate", func(r *Report) { r.Images[0].State = "success" }},
		{"oversize", func(r *Report) { r.Images[0].Detail = strings.Repeat("x", 1025) }},
		{"readywithouttemplate", func(r *Report) { r.Images[0].State = "ready" }},
		{"wrongtemplate", func(r *Report) { r.Images[0].Template = &Template{"other", "x"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := json.Marshal(valid)
			var r Report
			_ = json.Unmarshal(b, &r)
			tc.change(&r)
			if validReport(r, p) == nil {
				t.Fatal("accepted invalid report")
			}
		})
	}
	p.Enabled = false
	if validReport(valid, p) == nil {
		t.Fatal("accepted enabled report after disable")
	}
}
func TestDesiredAndObservedAreDistinct(t *testing.T) {
	p := testPolicy()
	r := Reports{Steam: Report{PolicyRevision: "1", Images: []ImageReport{{Image: p.Images[0], PreparationEnabled: true, ConsumptionEnabled: true, State: "ready", Reason: "none"}}}}
	raw, _ := json.Marshal(r)
	legacy := Project(p, "steam", nil, raw, nil, true)
	if legacy.Supported || legacy.PreparationEnabled != nil || legacy.Reason != "agent_upgrade_required" {
		t.Fatalf("legacy falsely acknowledged: %+v", legacy)
	}
	p.Enabled = false
	out := Project(p, "steam", []byte(`{"steam_preparation":1}`), raw, nil, true)
	if !out.PolicyPending || out.DesiredEnabled || out.PreparationEnabled == nil || !*out.PreparationEnabled || out.State != "pending_policy" {
		t.Fatalf("desired confused with effective: %+v", out)
	}
	r.Steam.PolicyRevision = p.Revision
	r.Steam.Images[0].PreparationEnabled = false
	r.Steam.Images[0].ConsumptionEnabled = false
	r.Steam.Images[0].State = "disabled"
	raw, _ = json.Marshal(r)
	out = Project(p, "steam", []byte(`{"steam_preparation":1}`), raw, nil, false)
	if !out.PolicyPending || out.Reason != "host_offline" {
		t.Fatal("offline status presented current")
	}
}

func TestEffectivePermissionsRequireExplicitBooleans(t *testing.T) {
	for _, raw := range []string{`{}`, `{"preparation_enabled":null,"consumption_enabled":true}`, `{"preparation_enabled":true}`, `{"preparation_enabled":true,"consumption_enabled":"false"}`} {
		var report ImageReport
		if json.Unmarshal([]byte(raw), &report) == nil {
			t.Fatalf("accepted ambiguous permissions: %s", raw)
		}
	}
	var report ImageReport
	if err := json.Unmarshal([]byte(`{"preparation_enabled":false,"consumption_enabled":false}`), &report); err != nil {
		t.Fatal(err)
	}
}

func TestReconciliationPreservesDispatcherBackoff(t *testing.T) {
	p := testPolicy()
	base := Reports{Steam: Report{PolicyRevision: p.Revision, Images: []ImageReport{{Image: p.Images[0], PreparationEnabled: true, ConsumptionEnabled: true, State: "deferred", Reason: "host_busy"}}}}
	copyReport := func() Reports {
		raw, _ := json.Marshal(base)
		var out Reports
		_ = json.Unmarshal(raw, &out)
		return out
	}
	if !needsReconciliation(nil, &base) {
		t.Fatal("first acknowledgement must reconcile")
	}
	for _, state := range []string{"deferred", "preparing", "failed"} {
		next := copyReport()
		next.Steam.Images[0].State = state
		next.Steam.Images[0].Detail = "new phase detail"
		if needsReconciliation(&base, &next) {
			t.Fatalf("%s phase report pulls forward dispatcher-owned retry", state)
		}
	}
	next := copyReport()
	next.Steam.PolicyRevision = "3"
	if !needsReconciliation(&base, &next) {
		t.Fatal("new policy must reconcile")
	}
	old := copyReport()
	old.Steam.Images[0].PreparationEnabled = false
	if !needsReconciliation(&old, &base) {
		t.Fatal("restored host permission must reconcile")
	}
	old = copyReport()
	old.Steam.Images[0].Reason = "storage_unavailable"
	if !needsReconciliation(&old, &base) {
		t.Fatal("restored storage must reconcile")
	}
	old = copyReport()
	old.Steam.Images[0].State = "ready"
	old.Steam.Images[0].Template = &Template{p.Images[0].RegistryRef, p.Images[0].Version}
	if !needsReconciliation(&old, &base) {
		t.Fatal("lost template must reconcile")
	}
	old = copyReport()
	old.Steam.Images[0].State = "waiting_image"
	if !needsReconciliation(&old, &base) {
		t.Fatal("image readiness must reconcile")
	}
}
