package session

// HTTP-level coverage of GET /v1/hosts/{id}/gpus' codecs field (#296 amendment
// 12): the wire shape openapi.yaml GPUAvailability.codecs promises — always
// present, null only when neither the GPU nor its host has ever reported.

import (
	"encoding/json"
	"net/http"
	"testing"
)

type gpuAvailabilityBody struct {
	GPUID  string   `json:"gpu_id"`
	Codecs []string `json:"codecs"`
}

func decodeGPUsResponse(t *testing.T, resp *http.Response) []gpuAvailabilityBody {
	t.Helper()
	var body struct {
		Items []gpuAvailabilityBody `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode gpus response: %v", err)
	}
	_ = resp.Body.Close()
	return body.Items
}

func TestHostGPUsEndpointServesCodecs(t *testing.T) {
	pool := testDB(t)
	base, adminTok, _ := newProfileAdminServer(t, pool)
	s := seed(t, pool, 4)

	// Neither the GPU nor the host has reported: the key is present, null.
	resp := doJSON(t, "GET", base+"/v1/hosts/"+s.hostID+"/gpus", adminTok, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET gpus: want 200, got %d", resp.StatusCode)
	}
	// Decode into a raw map first to prove the key survives even when null —
	// decoding straight into []string would not distinguish "absent" from "null".
	rawBody, err := decodeGPUsRaw(t, resp)
	if err != nil {
		t.Fatalf("decode gpus response: %v", err)
	}
	if len(rawBody) == 0 {
		t.Fatalf("gpus response has no items")
	}
	if _, present := rawBody[0]["codecs"]; !present {
		t.Fatalf("codecs key missing from the response entirely, want present (null)")
	}
	if rawBody[0]["codecs"] != nil {
		t.Fatalf("codecs = %v before any report, want null", rawBody[0]["codecs"])
	}

	// The host reports; the GPU inherits.
	setHostCodecsRaw(t, pool, s.hostID, `["h264","h265"]`)
	items := decodeGPUsResponse(t, doJSON(t, "GET", base+"/v1/hosts/"+s.hostID+"/gpus", adminTok, nil))
	got := findGPUBody(t, items, s.gpuID)
	if !strSliceEqual(got.Codecs, []string{"h264", "h265"}) {
		t.Fatalf("codecs inheriting the host = %v, want [h264 h265]", got.Codecs)
	}

	// The GPU reports its own set; it wins over the host's.
	setGPUCodecsRaw(t, pool, s.hostID, 0, `["h264"]`)
	items = decodeGPUsResponse(t, doJSON(t, "GET", base+"/v1/hosts/"+s.hostID+"/gpus", adminTok, nil))
	got = findGPUBody(t, items, s.gpuID)
	if !strSliceEqual(got.Codecs, []string{"h264"}) {
		t.Fatalf("codecs with its own report = %v, want [h264]", got.Codecs)
	}
}

func findGPUBody(t *testing.T, items []gpuAvailabilityBody, gpuID string) gpuAvailabilityBody {
	t.Helper()
	for _, it := range items {
		if it.GPUID == gpuID {
			return it
		}
	}
	t.Fatalf("gpu %s not in response (%d items)", gpuID, len(items))
	return gpuAvailabilityBody{}
}

func decodeGPUsRaw(t *testing.T, resp *http.Response) ([]map[string]any, error) {
	t.Helper()
	var body struct {
		Items []map[string]any `json:"items"`
	}
	err := json.NewDecoder(resp.Body).Decode(&body)
	_ = resp.Body.Close()
	return body.Items, err
}
