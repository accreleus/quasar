package crud

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// Both Host reads must explain every active owner without exposing internal
// identifiers or trusting the table's historical free-form reason text.
func TestHostReadsServeSafeOrderedAdmissionReasons(t *testing.T) {
	pool := testPool(t)
	s := &store{pool: pool}
	hostID := seedGateHost(t, pool, "admission-reasons-read", "", -1)
	ctx := context.Background()
	const ownerID = "00000000-0000-0000-0000-000000000123"
	if _, err := pool.Exec(ctx, `INSERT INTO host_admission_restrictions
		(host_id,owner_kind,owner_id,reason,created_at) VALUES
		($1::uuid,'platform',$2::uuid,'manual_drain','2026-09-23T12:00:00Z'),
		($1::uuid,'legacy','00000000-0000-0000-0000-000000000001'::uuid,
		 'legacy_drain','2026-09-23T12:01:00Z')`, hostID, ownerID); err != nil {
		t.Fatal(err)
	}
	check := func(label string, h Host) {
		t.Helper()
		raw, err := json.Marshal(hostToResp(h))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), ownerID) {
			t.Fatalf("%s exposed internal owner ID: %s", label, raw)
		}
		var body struct {
			AdmissionRestrictions []struct {
				OwnerKind string `json:"owner_kind"`
				Reason    string `json:"reason"`
			} `json:"admission_restrictions"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatal(err)
		}
		if len(body.AdmissionRestrictions) != 2 ||
			body.AdmissionRestrictions[0].OwnerKind != "legacy" || body.AdmissionRestrictions[0].Reason != "legacy_drain" ||
			body.AdmissionRestrictions[1].OwnerKind != "platform" || body.AdmissionRestrictions[1].Reason != "platform_apply" {
			t.Fatalf("%s admission reasons = %+v, want safe owner-kind ordering", label, body.AdmissionRestrictions)
		}
	}
	got, err := s.getHost(ctx, hostID)
	if err != nil {
		t.Fatal(err)
	}
	check("detail", got)
	hosts, _, err := s.listHosts(ctx, "", 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hosts {
		if h.ID == hostID {
			check("list", h)
			return
		}
	}
	t.Fatal("seeded host missing from list read")
}
